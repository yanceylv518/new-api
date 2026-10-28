package common

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const traceUsageMaxBytes = 64 << 10
const traceUsageMaxParts = 32

// StartSDKRequestTrace 让 SDK 返回的原始协议数据复用同一采集入口，不伪造 HTTP 时点。
func StartSDKRequestTrace(c *gin.Context, attempt, channelID int, requestBytes int64, stream bool) {
	trace := GetRequestTrace(c)
	if trace == nil {
		return
	}
	entry := trace.StartAttempt(attempt, channelID, &http.Request{ContentLength: requestBytes})
	entry.Observer = trace.ClientObserver.NewRelatedObserverWithMode(trace.ResponseMetadataLevel(channelID, 0))
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	entry.Observer.Prepare(contentType, -1)
	entry.Observer.decoded = true
	c.Set(decodedRequestTraceKey, entry.Observer)
}

func (o *TraceBodyObserver) captureUsage(data []byte) {
	if !o.upstreamUsage || !bytes.Contains(data, []byte(`"usage"`)) && !bytes.Contains(data, []byte(`"usageMetadata"`)) {
		return
	}
	// GetBytes 只复制命中的小字段，避免为完整返回正文再分配一份字符串。
	event := gjson.GetBytes(data, "type").String()
	values := gjson.GetManyBytes(data, "usage", "usageMetadata", "message.usage", "response.usage")
	choices := gjson.GetBytes(data, "choices.#")
	object := gjson.GetBytes(data, "object").String()
	format := "unknown"
	switch {
	case values[1].Exists():
		format = "gemini"
	case strings.HasPrefix(event, "message_"), strings.HasPrefix(event, "content_block_"), event == "message":
		format = "claude"
	case strings.HasPrefix(event, "response."), object == "response", object == "response.compaction":
		format = "openai_responses"
	case choices.Exists(), strings.HasPrefix(object, "chat.completion"), object == "text_completion":
		format = "openai"
	}
	if format == "unknown" && o.usageFormat != "" {
		format = o.usageFormat
	}
	for index, path := range []string{"usage", "usageMetadata", "message.usage", "response.usage"} {
		if value := values[index]; value.Exists() {
			o.captureUsagePart(format, event, path, value)
		}
	}
	rootUsageCount := 0
	if values[0].Exists() {
		rootUsageCount = 1
	}
	if choices.Exists() && bytes.Count(data, []byte(`"usage"`)) > rootUsageCount {
		gjson.GetBytes(data, "choices").ForEach(func(index, choice gjson.Result) bool {
			if value := choice.Get("usage"); value.Exists() {
				o.captureUsagePart(format, event, "choices."+index.String()+".usage", value)
			}
			return o.upstream.Usage == nil || !o.upstream.Usage.Incomplete
		})
	}
}

func (o *TraceBodyObserver) captureUsagePart(format, event, path string, value gjson.Result) {
	if len(value.Raw) > traceUsageMaxBytes {
		o.usageIncomplete()
		return
	}
	if !utf8.ValidString(value.Raw) || !gjson.Valid(value.Raw) {
		if o.upstream.Usage == nil {
			o.upstream.Usage = &RequestTraceUsage{Format: format, Parts: []RequestTraceUsagePart{}}
		}
		o.upstream.Usage.Incomplete = true
		o.markIncomplete("usage_invalid_json")
		return
	}
	if o.upstream.Usage == nil {
		o.upstream.Usage = &RequestTraceUsage{Format: format, Parts: []RequestTraceUsagePart{}}
	}
	usage := o.upstream.Usage
	if format == "unknown" && usage.Format != "" {
		format = usage.Format
	}
	if usage.Format == "" || usage.Format == "unknown" {
		usage.Format = format
	}
	replace := -1
	for index := len(usage.Parts) - 1; index >= 0; index-- {
		part := &usage.Parts[index]
		if part.Path != path || part.Event != event {
			continue
		}
		if string(part.Value) == value.Raw {
			return
		}
		if replace < 0 {
			replace = index
		}
	}
	if replace >= 0 {
		part := &usage.Parts[replace]
		// 只有字段覆盖关系明确的累计快照才能替代旧值；增补片段保留各自来源。
		if format != "unknown" && traceUsageCovers(gjson.ParseBytes(part.Value), value, 0) {
			size := o.usageBytes - len(part.Value) + len(value.Raw)
			if size > traceUsageMaxBytes {
				o.usageIncomplete()
				return
			}
			part.Value = json.RawMessage([]byte(value.Raw))
			o.usageBytes = size
			return
		}
	}
	if len(usage.Parts) >= traceUsageMaxParts || o.usageBytes+len(value.Raw) > traceUsageMaxBytes {
		o.usageIncomplete()
		return
	}
	usage.Parts = append(usage.Parts, RequestTraceUsagePart{Event: strings.Clone(TraceMetadataString(event, 128)), Path: path, Value: json.RawMessage([]byte(value.Raw))})
	o.usageBytes += len(value.Raw)
}

func traceUsageCovers(previous, next gjson.Result, depth int) bool {
	if depth >= 32 || !previous.IsObject() || !next.IsObject() {
		return previous.Type == next.Type && !previous.IsArray() && !previous.IsObject()
	}
	covered := true
	previous.ForEach(func(key, value gjson.Result) bool {
		// 逐键比较，厂商扩展字段名中的点号不能被解释为查询路径。
		var replacement gjson.Result
		next.ForEach(func(candidate, child gjson.Result) bool {
			if candidate.Str == key.Str {
				replacement = child
				return false
			}
			return true
		})
		covered = replacement.Exists() && traceUsageCovers(value, replacement, depth+1)
		return covered
	})
	return covered
}

func (o *TraceBodyObserver) usageIncomplete() {
	if o.upstream.Usage == nil {
		o.upstream.Usage = &RequestTraceUsage{Parts: []RequestTraceUsagePart{}}
	}
	o.upstream.Usage.Incomplete = true
	o.markIncomplete("usage_size_limit")
}

func (o *TraceBodyObserver) usageSnapshot() *RequestTraceUsage {
	if !o.upstreamUsage {
		return nil
	}
	usage := RequestTraceUsage{Parts: []RequestTraceUsagePart{}}
	if o.upstream.Usage != nil {
		usage = *o.upstream.Usage
		usage.Parts = append([]RequestTraceUsagePart{}, usage.Parts...)
	}
	if !o.observed {
		usage.Incomplete = true
	}
	if o.stream && o.response.EndMarkerPolicy == "required" && !o.response.Done && !o.pendingDone {
		usage.Incomplete = true
	}
	for _, reason := range o.response.IncompleteReasons {
		switch reason {
		case "invalid_json", "size_limit", "upstream_read_error":
			usage.Incomplete = true
		}
	}
	return &usage
}

// SDK 也按真实请求结束尝试，耗时不代替未观测到的首字节或响应头时点。
func FinishSDKRequestTrace(c *gin.Context, attempt int) {
	if trace := GetRequestTrace(c); trace != nil {
		trace.FinishAttempt(attempt)
	}
}
