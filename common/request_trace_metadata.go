package common

import (
	"bytes"
	"errors"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const TraceMetadataMaxBytes = 1 << 20
const traceSummaryMaxBytes = 64 << 10
const traceMaxToolCalls = 256
const traceMaxToolNameBytes = 128
const traceMaxCachedStrings = 256

type TraceMetadataLevel string

const (
	TraceMetadataSummary TraceMetadataLevel = "summary"
	TraceMetadataFull    TraceMetadataLevel = "full"
)

var errTraceRequestTooLarge = errors.New("request trace shape exceeds capture limit")
var errTraceRequestInvalidJSON = errors.New("request trace shape is not valid JSON")

type traceUpstreamFields uint16

const (
	traceFieldResponseID traceUpstreamFields = 1 << iota
	traceFieldModel
	traceFieldObject
	traceFieldFingerprint
	traceFieldServiceTier
	traceFieldErrorType
	traceFieldErrorCode
	traceFieldReasoning
)

type traceToolCall struct {
	key         string
	name        string
	nameDelta   bool
	invalidName bool
}

type tracePayloadResponse struct {
	ReasoningTokens       *int64
	ReasoningOutputTokens *int64
	EndMarker             string
	Error                 bool
	Refused               bool
	Done                  bool
	MetadataIncomplete    bool
	IncompleteReasons     []string
	EndMarkerPolicy       string
}

type tracePayloadUpstream struct {
	ResponseID  string
	Model       string
	Object      string
	Created     *int64
	Fingerprint string
	ServiceTier string
	ErrorType   string
	ErrorCode   string
}

type tracePayloadMetadata struct {
	response      tracePayloadResponse
	upstream      tracePayloadUpstream
	finishReason  *string
	fields        traceUpstreamFields
	errorStatus   *string
	errorEvent    bool
	messageObject bool
	tools         []traceToolCall
}

type tracePayloadParser struct {
	tracePayloadMetadata
	stream  bool
	summary bool
	parsing *traceParseWorkspace
}

type traceParseWorkspace struct {
	sync.Mutex
	strings map[traceStringKey]string
	parser  tracePayloadParser
}

type traceStringKey struct {
	value string
	limit int
}

func (workspace *traceParseWorkspace) metadataString(value string, limit int) string {
	if value == "" {
		return ""
	}
	key := traceStringKey{value: value, limit: limit}
	if cleaned, found := workspace.strings[key]; found {
		return cleaned
	}
	cleaned := strings.Clone(TraceMetadataString(value, limit))
	if len(value) <= 256 && len(workspace.strings) < traceMaxCachedStrings {
		if workspace.strings == nil {
			workspace.strings = make(map[traceStringKey]string)
		}
		key.value = strings.Clone(value)
		workspace.strings[key] = cleaned
	}
	return cleaned
}

func (workspace *traceParseWorkspace) parse(data []byte, stream, summary bool) tracePayloadMetadata {
	workspace.Lock()
	defer workspace.Unlock()
	parser := &workspace.parser
	*parser = tracePayloadParser{stream: stream, summary: summary, parsing: workspace}
	parser.parsePayload(data)
	metadata := parser.tracePayloadMetadata
	*parser = tracePayloadParser{}
	return metadata
}

func readRequestTraceShape(reader io.Reader) (*RequestTraceShape, error) {
	data, err := io.ReadAll(io.LimitReader(reader, TraceMetadataMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > TraceMetadataMaxBytes {
		return nil, errTraceRequestTooLarge
	}
	if !gjson.ValidBytes(data) {
		return nil, errTraceRequestInvalidJSON
	}
	r := gjson.ParseBytes(data)
	shape := &RequestTraceShape{}
	for _, key := range []string{"messages", "contents", "input", "prompt"} {
		value := r.Get(key)
		if value.IsArray() {
			if key == "prompt" && value.Get("0").Type == gjson.Number {
				shape.Messages = 1
			} else {
				value.ForEach(func(_, _ gjson.Result) bool {
					shape.Messages++
					return true
				})
			}
			break
		}
	}
	tools := r.Get("tools")
	if !tools.IsArray() {
		tools = r.Get("functions")
	}
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			declarations := tool.Get("functionDeclarations")
			if !declarations.IsArray() {
				declarations = tool.Get("function_declarations")
			}
			if declarations.IsArray() {
				declarations.ForEach(func(_, declaration gjson.Result) bool {
					if declaration.IsObject() {
						shape.Tools++
					}
					return true
				})
			} else if tool.IsObject() {
				shape.Tools++
			}
			return true
		})
	}
	if r.Get("input").Type == gjson.String || r.Get("prompt").Type == gjson.String {
		shape.Messages = 1
	}
	for _, key := range []string{"system", "systemInstruction", "system_instruction", "instructions"} {
		if value := r.Get(key); value.Exists() && value.Type != gjson.Null {
			shape.System = true
			break
		}
	}
	choice := r.Get("tool_choice")
	if !choice.Exists() {
		choice = r.Get("function_call")
		if choice.IsObject() {
			shape.ToolChoice = "function"
		}
	}
	if choice.IsObject() {
		choice = choice.Get("type")
	}
	if !choice.Exists() {
		choice = r.Get("toolConfig.functionCallingConfig.mode")
	}
	if !choice.Exists() {
		choice = r.Get("tool_config.function_calling_config.mode")
	}
	if shape.ToolChoice == "" {
		shape.ToolChoice = strings.ToLower(TraceMetadataString(choice.String(), 64))
	}
	switch shape.ToolChoice {
	case "", "auto", "none", "required", "any", "tool", "function":
	default:
		shape.ToolChoice = "unknown"
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "generationConfig.maxOutputTokens", "generation_config.max_output_tokens"} {
		if value := r.Get(key); value.Type == gjson.Number {
			n := value.Int()
			shape.MaxTokens = &n
			break
		}
	}
	for _, key := range []string{"temperature", "generationConfig.temperature", "generation_config.temperature"} {
		if value := r.Get(key); value.Type == gjson.Number {
			n := value.Float()
			if !math.IsNaN(n) && !math.IsInf(n, 0) {
				shape.Temperature = &n
			}
			break
		}
	}
	shape.IncludeUsage = r.Get("stream_options.include_usage").Bool()
	for _, key := range []string{"messages", "contents", "input", "system", "systemInstruction", "system_instruction", "instructions", "prompt"} {
		traceCountRequestContent(r.Get(key), shape, 0)
	}
	return shape, nil
}

func traceCountRequestContent(value gjson.Result, shape *RequestTraceShape, depth int) {
	if depth > 16 {
		return
	}
	if value.Type == gjson.String {
		shape.TextCharacters += int64(utf8.RuneCountInString(value.String()))
		return
	}
	if value.IsArray() {
		value.ForEach(func(_, item gjson.Result) bool { traceCountRequestContent(item, shape, depth+1); return true })
		return
	}
	if !value.IsObject() {
		return
	}
	if role := value.Get("role").String(); role == "system" || role == "developer" {
		shape.System = true
	}
	media := value.Get("type").String()
	mime := value.Get("inlineData.mimeType").String()
	if mime == "" {
		mime = value.Get("fileData.mimeType").String()
	}
	switch {
	case strings.Contains(media, "image") || strings.HasPrefix(mime, "image/") || value.Get("image_url").Exists():
		shape.Images++
		return
	case strings.Contains(media, "video") || strings.HasPrefix(mime, "video/"):
		shape.Videos++
		return
	case strings.Contains(media, "audio") || strings.HasPrefix(mime, "audio/"):
		shape.Audios++
		return
	case media == "file" || media == "input_file" || media == "document" || mime != "":
		shape.Files++
		return
	case media == "tool_use" || media == "function_call":
		return
	case media == "function_call_output":
		traceCountRequestContent(value.Get("output"), shape, depth+1)
		return
	}
	for _, key := range []string{"content", "parts", "text"} {
		if item := value.Get(key); item.Exists() {
			traceCountRequestContent(item, shape, depth+1)
		}
	}
}

// 解析缓冲只在请求处理中存在；超限时丢弃缓冲并标记统计不完整，落库结构没有正文字段。
type TraceBodyObserver struct {
	sync.Mutex
	buffer        []byte
	event         []byte
	stream        bool
	finished      bool
	finishedAt    time.Time
	skippingLine  bool
	observed      bool
	response      RequestTraceResponse
	upstream      RequestTraceUpstream
	finishReason  *string
	toolCalls     map[string]traceToolCall
	workspace     *traceParseWorkspace
	decoded       bool
	pendingDone   bool
	contentLength int64
	level         TraceMetadataLevel
	summaryEvents int64
	source        *TraceBodyObserver
}

func NewTraceBodyObserver(started time.Time) *TraceBodyObserver {
	return NewTraceBodyObserverWithMode(started, TraceMetadataFull)
}

// 保留时间参数以兼容既有调用，观察器不再统计分片时间。
func NewTraceBodyObserverWithMode(_ time.Time, level TraceMetadataLevel) *TraceBodyObserver {
	return &TraceBodyObserver{level: level}
}

func (o *TraceBodyObserver) NewRelatedObserver() *TraceBodyObserver {
	o.Lock()
	defer o.Unlock()
	return o.relatedObserver(o.level)
}

func (o *TraceBodyObserver) NewRelatedObserverWithMode(level TraceMetadataLevel) *TraceBodyObserver {
	o.Lock()
	defer o.Unlock()
	return o.relatedObserver(level)
}

func (o *TraceBodyObserver) relatedObserver(level TraceMetadataLevel) *TraceBodyObserver {
	if o.workspace == nil {
		o.workspace = &traceParseWorkspace{}
	}
	observer := NewTraceBodyObserverWithMode(time.Time{}, level)
	observer.workspace = o.workspace
	return observer
}

// IsEventStreamContentType 匹配媒体类型，忽略大小写及后面的参数。
func IsEventStreamContentType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream")
}

func (o *TraceBodyObserver) Prepare(contentType string, contentLength int64) {
	o.Lock()
	defer o.Unlock()
	if contentType != "" {
		o.response.ContentType = TraceMetadataString(contentType, 128)
		o.stream = IsEventStreamContentType(contentType)
	}
	// 延迟到第一次实际观察字节时分配，协议处理器接管采集后无需第二份正文缓冲。
	o.contentLength = contentLength
}

func (o *TraceBodyObserver) MetadataLevel() TraceMetadataLevel {
	o.Lock()
	source, level := o.source, o.level
	o.Unlock()
	if source != nil {
		return source.MetadataLevel()
	}
	return level
}

func (o *TraceBodyObserver) PromoteToFull() {
	o.Lock()
	defer o.Unlock()
	o.promoteToFull()
}

func (o *TraceBodyObserver) promoteToFull() {
	if o.level == TraceMetadataFull {
		return
	}
	if o.summaryEvents > 0 {
		// 升级前没有保存正文，无法补齐此前略过的工具调用。
		o.markIncomplete("capture_upgraded")
	}
	o.level = TraceMetadataFull
}

func (o *TraceBodyObserver) ReuseUpstream(source *TraceBodyObserver) {
	if source == nil || source == o {
		return
	}
	o.Lock()
	defer o.Unlock()
	if !o.finished && !o.observed && len(o.buffer) == 0 && len(o.event) == 0 {
		o.source = source
	}
}

func (o *TraceBodyObserver) DeliveryFailed() {
	o.Lock()
	defer o.Unlock()
	o.response.Error = true
	o.markIncomplete("client_delivery_incomplete")
}

func (o *TraceBodyObserver) ObserveClientError() {
	o.Lock()
	defer o.Unlock()
	if o.source != nil {
		o.source = nil
		if o.observed {
			o.markIncomplete("capture_upgraded")
		}
	}
	o.promoteToFull()
	o.response.Error = true
}

func (o *TraceBodyObserver) readFailed() {
	o.Lock()
	defer o.Unlock()
	o.promoteToFull()
	o.response.Error = true
	o.markIncomplete("upstream_read_error")
}

func (o *TraceBodyObserver) bufferLimit() int {
	if !o.stream && o.level == TraceMetadataSummary {
		return traceSummaryMaxBytes
	}
	return TraceMetadataMaxBytes
}

func (o *TraceBodyObserver) appendBuffer(data []byte) {
	if capacity := len(o.buffer) + len(data); capacity > cap(o.buffer) {
		if cap(o.buffer) == 0 && !o.stream && o.contentLength > 0 {
			capacity = max(capacity, int(min(o.contentLength, int64(o.bufferLimit()))))
		}
		capacity = min(o.bufferLimit(), max(capacity, max(4096, cap(o.buffer)*2)))
		grown := make([]byte, len(o.buffer), capacity)
		copy(grown, o.buffer)
		o.buffer = grown
	}
	o.buffer = append(o.buffer, data...)
}

func (o *TraceBodyObserver) Observe(data []byte, contentType string) {
	o.Lock()
	defer o.Unlock()
	if o.finished {
		return
	}
	o.observed = o.observed || len(data) > 0
	o.observe(data, contentType)
}

func (o *TraceBodyObserver) ObserveString(data string, contentType string) {
	o.Lock()
	defer o.Unlock()
	if o.finished {
		return
	}
	o.observed = o.observed || len(data) > 0
	if o.source != nil || o.decoded {
		return
	}
	// 仅在本次调用内读取；缓冲区与保留的元数据均取得自己的拷贝。
	o.observe(StringToByteSlice(data), contentType)
}

func (o *TraceBodyObserver) observe(data []byte, contentType string) {
	if o.source != nil || o.decoded {
		return
	}
	if o.response.ContentType == "" && contentType != "" {
		o.response.ContentType = TraceMetadataString(contentType, 128)
		o.stream = IsEventStreamContentType(contentType)
	}
	if !o.stream {
		if o.response.MetadataIncomplete {
			return
		}
		if len(o.buffer)+len(data) > o.bufferLimit() {
			o.buffer = nil
			o.markIncomplete("size_limit")
			return
		}
		o.appendBuffer(data)
		return
	}
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		part := data
		if newline >= 0 {
			part = data[:newline]
		}
		if !o.skippingLine {
			if len(o.buffer)+len(part) > TraceMetadataMaxBytes {
				o.buffer = nil
				o.event = nil
				o.skippingLine = true
				o.markIncomplete("size_limit")
			} else if newline >= 0 && len(o.buffer) == 0 {
				remaining := data[newline+1:]
				separator := 0
				if bytes.HasPrefix(remaining, []byte{'\n'}) {
					separator = 1
				} else if bytes.HasPrefix(remaining, []byte{'\r', '\n'}) {
					separator = 2
				}
				// 完整单行事件在本次调用内消费，不保留上游可复用的字节切片。
				if len(o.event) == 0 && separator > 0 && bytes.HasPrefix(part, []byte("data:")) {
					value := bytes.TrimSpace(part[5:])
					if len(value) > 0 {
						o.consumePayload(value)
					}
					data = remaining[separator:]
					continue
				}
				o.consumeLine(part)
			} else {
				o.appendBuffer(part)
				if newline >= 0 {
					o.consumeLine(o.buffer)
				}
			}
		}
		if newline < 0 {
			break
		}
		o.buffer = o.buffer[:0]
		o.skippingLine = false
		data = data[newline+1:]
	}
}

func (o *TraceBodyObserver) consumeLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) == 0 {
		if len(o.event) > 0 {
			o.consumePayload(bytes.TrimSpace(o.event))
			o.event = o.event[:0]
		}
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	value := bytes.TrimSpace(line[5:])
	if len(o.event)+len(value)+1 > TraceMetadataMaxBytes {
		o.event = nil
		o.markIncomplete("size_limit")
		return
	}
	if len(o.event) > 0 {
		o.event = append(o.event, '\n')
	}
	o.event = append(o.event, value...)
}

func (o *TraceBodyObserver) Finish() {
	o.Lock()
	defer o.Unlock()
	if o.finished {
		return
	}
	if o.stream {
		if !o.skippingLine && len(o.buffer) > 0 {
			o.consumeLine(o.buffer)
		}
		if len(o.event) > 0 {
			o.consumePayload(bytes.TrimSpace(o.event))
		}
	} else if !o.response.MetadataIncomplete && len(o.buffer) > 0 {
		o.consumePayload(o.buffer)
	}
	if o.pendingDone {
		o.response.Done, o.response.EndMarker, o.response.EndMarkerPolicy = true, "[DONE]", "required"
	}
	o.buffer = nil
	o.event = nil
	o.finished = true
	if o.finishedAt.IsZero() {
		o.finishedAt = time.Now()
	}
}

func (o *TraceBodyObserver) FinishedAt() time.Time {
	o.Lock()
	defer o.Unlock()
	return o.finishedAt
}

func traceIncompleteReason(reasons []string, reason string) []string {
	if len(reasons) < 8 && !slices.Contains(reasons, reason) {
		return append(reasons, reason)
	}
	return reasons
}

func (o *TraceBodyObserver) markIncomplete(reason string) {
	o.response.MetadataIncomplete = true
	o.response.IncompleteReasons = traceIncompleteReason(o.response.IncompleteReasons, reason)
}

func (o *tracePayloadParser) markIncomplete(reason string) {
	o.response.MetadataIncomplete = true
	o.response.IncompleteReasons = traceIncompleteReason(o.response.IncompleteReasons, reason)
}

func (o *TraceBodyObserver) Snapshot() (RequestTraceResponse, RequestTraceUpstream, *string) {
	o.Lock()
	if o.source != nil {
		source, local := o.source, o.response
		o.Unlock()
		response, upstream, reason := source.Snapshot()
		if local.ContentType != "" {
			response.ContentType = local.ContentType
		}
		if local.Error {
			response.Error, response.Done, response.EndMarker = true, false, ""
		}
		response.MetadataIncomplete = response.MetadataIncomplete || local.MetadataIncomplete
		if len(local.IncompleteReasons) > 0 {
			response.IncompleteReasons = append([]string(nil), response.IncompleteReasons...)
		}
		for _, item := range local.IncompleteReasons {
			response.IncompleteReasons = traceIncompleteReason(response.IncompleteReasons, item)
		}
		return response, upstream, reason
	}
	defer o.Unlock()
	response := o.response
	if o.pendingDone {
		response.Done, response.EndMarker, response.EndMarkerPolicy = true, "[DONE]", "required"
	}
	for _, tool := range o.toolCalls {
		if tool.invalidName {
			continue
		}
		name := TraceMetadataString(strings.TrimSpace(tool.name), traceMaxToolNameBytes)
		if name == "" {
			continue
		}
		if response.ToolCallsByName == nil {
			response.ToolCallsByName = make(map[string]int64)
		}
		response.ToolCallsByName[name]++
	}
	return response, o.upstream, o.finishReason
}

// HasError 读取已解析的错误状态；收尾时先 Finish，避免遗漏最后一个事件。
func (o *TraceBodyObserver) HasError() bool {
	o.Lock()
	source, failed := o.source, o.response.Error
	o.Unlock()
	return failed || (source != nil && source.HasError())
}

func (o *TraceBodyObserver) HasIncompleteMetadata() bool {
	o.Lock()
	source, incomplete := o.source, o.response.MetadataIncomplete
	o.Unlock()
	return incomplete || (source != nil && source.HasIncompleteMetadata())
}

func (o *tracePayloadParser) captureTool(key string, name gjson.Result, nameDelta bool) {
	if len(o.tools) >= traceMaxToolCalls {
		o.markIncomplete("tool_limit")
		return
	}
	tool := traceToolCall{key: key, nameDelta: nameDelta}
	// 流式参数片段可能用 null 占位，此时保留此前收到的工具名。
	if name.Exists() && (!nameDelta || name.Type != gjson.Null) {
		tool.invalidName = name.Type != gjson.String || len(name.Str) > traceMaxToolNameBytes
		if tool.invalidName {
			o.markIncomplete("invalid_tool_name")
		}
		if !tool.invalidName {
			tool.name = strings.Clone(name.Str)
		}
	}
	o.tools = append(o.tools, tool)
}

func (o *tracePayloadParser) toolIndex(value gjson.Result) string {
	if value.Type == gjson.Number {
		return value.String()
	}
	if value.Exists() {
		o.markIncomplete("invalid_tool_index")
	}
	return ""
}

func traceSummaryNeedsParsing(data []byte) bool {
	if bytes.Equal(data, []byte("[DONE]")) {
		return true
	}
	for index, value := range data {
		if value != '"' {
			continue
		}
		rest := data[index:]
		var keyLength int
		errorWord := false
		switch {
		case bytes.HasPrefix(rest, []byte(`"error"`)):
			keyLength = len(`"error"`)
			errorWord = true
		case bytes.HasPrefix(rest, []byte(`"finish_reason"`)):
			keyLength = len(`"finish_reason"`)
		case bytes.HasPrefix(rest, []byte(`"finishReason"`)):
			keyLength = len(`"finishReason"`)
		case bytes.HasPrefix(rest, []byte(`"stop_reason"`)):
			keyLength = len(`"stop_reason"`)
		case bytes.HasPrefix(rest, []byte(`"blockReason"`)):
			keyLength = len(`"blockReason"`)
		case bytes.HasPrefix(rest, []byte(`"message_stop"`)),
			bytes.HasPrefix(rest, []byte(`"response.completed"`)),
			bytes.HasPrefix(rest, []byte(`"response.done"`)),
			bytes.HasPrefix(rest, []byte(`"response.failed"`)),
			bytes.HasPrefix(rest, []byte(`"response.incomplete"`)):
			return true
		}
		if keyLength == 0 {
			continue
		}
		rest = bytes.TrimSpace(rest[keyLength:])
		if len(rest) == 0 || rest[0] != ':' {
			if errorWord {
				return true
			}
			continue
		}
		rest = bytes.TrimSpace(rest[1:])
		// 常见 Chat 流事件总带 finish_reason:null，不能因此逐事件解析。
		if len(rest) > 0 && !bytes.HasPrefix(rest, []byte("null")) {
			return true
		}
	}
	return false
}

func (o *TraceBodyObserver) consumePayload(data []byte) {
	summary := o.level == TraceMetadataSummary
	if summary && o.stream && !traceSummaryNeedsParsing(data) {
		o.summaryEvents++
		return
	}
	if o.workspace == nil {
		o.workspace = &traceParseWorkspace{}
	}
	metadata := o.workspace.parse(data, o.stream, summary)
	if summary && (metadata.response.Error || metadata.response.EndMarker == "response.incomplete") {
		o.promoteToFull()
		metadata = o.workspace.parse(data, o.stream, false)
	} else if summary {
		o.summaryEvents++
	}
	o.mergeMetadata(metadata)
}

func (o *TraceBodyObserver) mergeMetadata(metadata tracePayloadMetadata) {
	response := metadata.response
	o.response.Error = o.response.Error || response.Error
	if response.ReasoningTokens != nil {
		if o.response.ReasoningTokens == nil || *o.response.ReasoningTokens != *response.ReasoningTokens {
			tokens := *response.ReasoningTokens
			o.response.ReasoningTokens = &tokens
		}
		if response.ReasoningOutputTokens == nil {
			o.response.ReasoningOutputTokens = nil
		} else if o.response.ReasoningOutputTokens == nil || *o.response.ReasoningOutputTokens != *response.ReasoningOutputTokens {
			outputTokens := *response.ReasoningOutputTokens
			o.response.ReasoningOutputTokens = &outputTokens
		}
	}
	o.response.Refused = o.response.Refused || response.Refused
	o.response.Done = o.response.Done || response.Done
	if response.EndMarker != "" {
		o.response.EndMarker = response.EndMarker
	}
	o.response.MetadataIncomplete = o.response.MetadataIncomplete || response.MetadataIncomplete
	for _, reason := range response.IncompleteReasons {
		o.response.IncompleteReasons = traceIncompleteReason(o.response.IncompleteReasons, reason)
	}
	if response.EndMarkerPolicy != "" {
		o.response.EndMarkerPolicy = response.EndMarkerPolicy
	}
	for _, tool := range metadata.tools {
		if o.stream && strings.HasPrefix(tool.key, "gemini:") {
			// 不同 Gemini 事件可在相同 parts 下标返回不同完整工具调用。
			tool.key += ":" + strconv.FormatInt(o.response.ToolCalls, 10)
		}
		current, exists := o.toolCalls[tool.key]
		if !exists {
			if len(o.toolCalls) >= traceMaxToolCalls {
				o.markIncomplete("tool_limit")
				continue
			}
			if o.toolCalls == nil {
				o.toolCalls = make(map[string]traceToolCall)
			}
			o.response.ToolCalls++
		}
		if !tool.nameDelta && (tool.name != "" || tool.invalidName) {
			current.name, current.invalidName = tool.name, tool.invalidName
		} else if tool.nameDelta {
			// OpenAI 的函数名可能跨多个 delta；完整名称合并后再脱敏，避免跨片绕过。
			if current.invalidName || tool.invalidName || len(current.name)+len(tool.name) > traceMaxToolNameBytes {
				current.name, current.invalidName = "", true
				o.markIncomplete("invalid_tool_name")
			} else {
				current.name += tool.name
			}
		}
		o.toolCalls[tool.key] = current
	}
	for _, field := range []struct {
		flag   traceUpstreamFields
		value  string
		target *string
	}{
		{traceFieldResponseID, metadata.upstream.ResponseID, &o.upstream.ResponseID},
		{traceFieldModel, metadata.upstream.Model, &o.upstream.Model},
		{traceFieldObject, metadata.upstream.Object, &o.upstream.Object},
		{traceFieldFingerprint, metadata.upstream.Fingerprint, &o.upstream.Fingerprint},
		{traceFieldServiceTier, metadata.upstream.ServiceTier, &o.upstream.ServiceTier},
		{traceFieldErrorType, metadata.upstream.ErrorType, &o.upstream.ErrorType},
		{traceFieldErrorCode, metadata.upstream.ErrorCode, &o.upstream.ErrorCode},
	} {
		if metadata.fields&field.flag != 0 {
			*field.target = field.value
		}
	}
	if metadata.upstream.Created != nil && (o.upstream.Created == nil || *o.upstream.Created != *metadata.upstream.Created) {
		created := *metadata.upstream.Created
		o.upstream.Created = &created
	}
	if o.upstream.Object == "" && metadata.messageObject {
		o.upstream.Object = "message"
	}
	if o.upstream.ErrorType == "" && metadata.errorStatus != nil {
		o.upstream.ErrorType = *metadata.errorStatus
	}
	if o.upstream.ErrorType == "" && metadata.errorEvent {
		o.upstream.ErrorType = "error"
	}
	if metadata.finishReason != nil && (o.finishReason == nil || *o.finishReason != *metadata.finishReason) {
		reason := *metadata.finishReason
		o.finishReason = &reason
	}
}

func (o *tracePayloadParser) parsePayload(data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) {
		o.response.Done = true
		o.response.EndMarker = "[DONE]"
		o.response.EndMarkerPolicy = "required"
		return
	}
	if !gjson.ValidBytes(data) {
		o.markIncomplete("invalid_json")
		return
	}
	r := gjson.ParseBytes(data)
	if o.summary {
		o.parseSummary(r)
		return
	}
	choices := r.Get("choices")
	candidates := r.Get("candidates")
	kind := r.Get("type").String()
	gemini := candidates.IsArray() || (kind == "" && !choices.IsArray() && (r.Get("usageMetadata").IsObject() || r.Get("promptFeedback").IsObject()))
	if o.stream {
		switch {
		case gemini:
			o.response.EndMarkerPolicy = "not_applicable"
		case choices.IsArray(), strings.HasPrefix(kind, "response."), strings.HasPrefix(kind, "message_"), strings.HasPrefix(kind, "content_block_"):
			o.response.EndMarkerPolicy = "required"
		}
	}
	if kind == "" && !choices.IsArray() && !gemini && !r.Get("output").IsArray() && !r.Get("error").IsObject() && r.Get("error").Type != gjson.String {
		o.markIncomplete("unsupported_format")
	}
	root := r
	if kind == "message_start" {
		if message := r.Get("message"); message.IsObject() {
			root = message
		}
	}
	if response := r.Get("response"); response.IsObject() {
		root = response
	}
	for _, field := range []struct {
		key    string
		target *string
		flag   traceUpstreamFields
	}{
		{"id", &o.upstream.ResponseID, traceFieldResponseID}, {"model", &o.upstream.Model, traceFieldModel}, {"object", &o.upstream.Object, traceFieldObject},
		{"system_fingerprint", &o.upstream.Fingerprint, traceFieldFingerprint}, {"service_tier", &o.upstream.ServiceTier, traceFieldServiceTier},
		{"responseId", &o.upstream.ResponseID, traceFieldResponseID}, {"modelVersion", &o.upstream.Model, traceFieldModel},
	} {
		if value := root.Get(field.key); value.Type == gjson.String {
			*field.target = o.parsing.metadataString(value.String(), 128)
			o.fields |= field.flag
		}
	}
	for _, key := range []string{"created", "created_at"} {
		if value := root.Get(key); value.Type == gjson.Number {
			n := value.Int()
			o.upstream.Created = &n
			break
		}
	}
	if o.upstream.Object == "" && root.Get("type").String() == "message" {
		o.upstream.Object = "message"
		o.messageObject = true
	}
	if err := root.Get("error"); err.IsObject() {
		o.response.Error = true
		if value := err.Get("type"); value.Type == gjson.String {
			o.upstream.ErrorType = o.parsing.metadataString(value.String(), 128)
			o.fields |= traceFieldErrorType
		}
		if value := err.Get("status"); value.Type == gjson.String {
			status := o.parsing.metadataString(value.String(), 128)
			o.errorStatus = &status
			if o.upstream.ErrorType == "" {
				o.upstream.ErrorType = status
			}
		}
		if value := err.Get("code"); value.Type == gjson.String || value.Type == gjson.Number {
			o.upstream.ErrorCode = o.parsing.metadataString(value.String(), 128)
			o.fields |= traceFieldErrorCode
		}
	} else if err.Type == gjson.String {
		o.response.Error = true
	}
	if kind == "error" {
		o.errorEvent = true
		o.response.Error = true
		if o.upstream.ErrorType == "" {
			o.upstream.ErrorType = "error"
		}
		if code := r.Get("code"); code.Type == gjson.String || code.Type == gjson.Number {
			o.upstream.ErrorCode = o.parsing.metadataString(code.String(), 128)
			o.fields |= traceFieldErrorCode
		}
	}
	o.captureReasoningTokens(root.Get("usage"), root.Get("usageMetadata"))
	if choices.IsArray() {
		choices.ForEach(func(_, choice gjson.Result) bool {
			o.setFinishReason(choice.Get("finish_reason"))
			message := choice.Get("message")
			delta := choice.Get("delta")
			if delta.IsObject() {
				message = delta
			}
			if refusal := message.Get("refusal"); refusal.Type == gjson.String && refusal.String() != "" {
				o.response.Refused = true
			}
			choicePosition := o.toolIndex(choice.Get("index"))
			// gjson.ForEach 对显式 null 也会回调一次，必须先确认是工具调用数组。
			if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() {
				toolCalls.ForEach(func(index, tool gjson.Result) bool {
					if !tool.IsObject() {
						return true
					}
					position := index.String()
					if n := tool.Get("index"); n.Type == gjson.Number {
						position = n.String()
					} else if n.Exists() {
						o.markIncomplete("invalid_tool_index")
					}
					o.captureTool("openai:"+choicePosition+":"+position, tool.Get("function.name"), o.stream && delta.IsObject())
					return true
				})
			}
			functionCall := message.Get("function_call")
			functionName, functionArguments := functionCall.Get("name"), functionCall.Get("arguments")
			// 兼容网关会附带 name 和 arguments 均为空的占位对象，它不代表实际调用。
			if functionCall.IsObject() && ((functionName.Type == gjson.String && strings.TrimSpace(functionName.Str) != "") || (functionArguments.Type == gjson.String && strings.TrimSpace(functionArguments.Str) != "")) {
				o.captureTool("function_call:"+choicePosition, functionName, o.stream && delta.IsObject())
			}
			return true
		})
	}
	for _, source := range []gjson.Result{root.Get("stop_reason"), r.Get("delta.stop_reason")} {
		o.setFinishReason(source)
	}
	if kind == "message" || kind == "message_start" || kind == "message_delta" || kind == "content_block_start" || kind == "content_block_delta" {
		if kind == "content_block_start" {
			o.countContentBlock(r.Get("content_block"), "claude:"+o.toolIndex(r.Get("index")))
		}
		if kind == "message" {
			root.Get("content").ForEach(func(index, block gjson.Result) bool {
				o.countContentBlock(block, "claude:"+index.String())
				return true
			})
		}
	}
	if kind == "message_stop" || kind == "response.completed" || kind == "response.done" {
		o.response.Done = true
		o.response.EndMarker = strings.Clone(kind)
	}
	if kind == "response.failed" || kind == "response.incomplete" {
		o.response.Done = true
		o.response.EndMarker = strings.Clone(kind)
		if kind == "response.failed" {
			o.response.Error = true
		}
		reason := strings.Clone(strings.TrimPrefix(kind, "response."))
		o.finishReason = &reason
	}
	if candidates.IsArray() {
		candidates.ForEach(func(index, candidate gjson.Result) bool {
			o.setFinishReason(candidate.Get("finishReason"))
			candidate.Get("content.parts").ForEach(func(partIndex, part gjson.Result) bool {
				if part.Get("functionCall").IsObject() {
					key := "gemini:" + index.String() + ":" + partIndex.String()
					o.captureTool(key, part.Get("functionCall.name"), false)
				}
				return true
			})
			return true
		})
	}
	if gemini {
		o.captureGeminiBlockReason(r)
	}
	if strings.HasPrefix(kind, "response.") {
		if kind == "response.refusal.delta" {
			o.response.Refused = true
		}
		if kind == "response.output_item.added" || kind == "response.output_item.done" {
			o.captureResponsesTool("responses:"+o.toolIndex(r.Get("output_index")), r.Get("item"))
		}
	}
	if status := root.Get("status").String(); status == "completed" || status == "incomplete" || status == "failed" {
		if status == "failed" {
			o.response.Error = true
		}
		reason := strings.Clone(status)
		if detail := root.Get("incomplete_details.reason"); detail.Type == gjson.String && detail.String() != "" {
			reason += ":" + o.parsing.metadataString(detail.String(), 128)
			o.response.Refused = o.response.Refused || detail.Str == "content_filter"
		}
		o.finishReason = &reason
	}
	if !o.stream || kind == "response.completed" || kind == "response.done" || kind == "response.incomplete" || kind == "response.failed" {
		root.Get("output").ForEach(func(index, output gjson.Result) bool {
			o.captureResponsesTool("responses:"+index.String(), output)
			if o.stream {
				return true
			}
			output.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "refusal" {
					o.response.Refused = true
				}
				return true
			})
			return true
		})
		o.response.Done = true
	}
}

func (o *tracePayloadParser) captureReasoningTokens(usage, gemini gjson.Result) {
	for _, field := range []struct {
		source            gjson.Result
		reasoning, output string
	}{
		{usage, "completion_tokens_details.reasoning_tokens", "completion_tokens"},
		{usage, "output_tokens_details.reasoning_tokens", "output_tokens"},
		{gemini, "thoughtsTokenCount", ""},
		{usage, "reasoning_tokens", ""},
	} {
		value := field.source.Get(field.reasoning)
		if value.Type != gjson.Number {
			continue
		}
		n, err := strconv.ParseInt(value.Raw, 10, 64)
		if err != nil || n < 0 {
			continue
		}
		o.response.ReasoningTokens = &n
		// 仅将同一 usage 中标准 details 对应的输出总量用于“其中推理”展示。
		if field.output != "" {
			if output := field.source.Get(field.output); output.Type == gjson.Number && output.Int() >= n {
				total := output.Int()
				o.response.ReasoningOutputTokens = &total
			}
		}
		break
	}
}

func (o *tracePayloadParser) countContentBlock(block gjson.Result, key string) {
	switch block.Get("type").String() {
	case "tool_use", "server_tool_use":
		o.captureTool(key, block.Get("name"), false)
	}
}

func (o *tracePayloadParser) captureResponsesTool(key string, output gjson.Result) {
	kind := output.Get("type").String()
	name := output.Get("name")
	switch kind {
	case "function_call", "custom_tool_call", "mcp_call":
	case "web_search_call", "file_search_call", "image_generation_call", "computer_call", "code_interpreter_call", "local_shell_call":
		name = gjson.Result{Type: gjson.String, Str: kind}
	default:
		return
	}
	o.captureTool(key, name, false)
}

func (o *tracePayloadParser) setFinishReason(reason gjson.Result) {
	if reason.Type != gjson.String {
		return
	}
	value := o.parsing.metadataString(reason.String(), 128)
	o.finishReason = &value
	switch reason.Str {
	case "content_filter", "refusal", "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "RECITATION", "IMAGE_SAFETY":
		o.response.Refused = true
	}
}

func (o *tracePayloadParser) captureGeminiBlockReason(value gjson.Result) {
	blocked := value.Get("promptFeedback.blockReason")
	if blocked.Type == gjson.String && blocked.Str != "" && blocked.Str != "BLOCK_REASON_UNSPECIFIED" {
		o.response.Refused = true
		if o.finishReason == nil {
			o.setFinishReason(blocked)
		}
	}
}

func (o *tracePayloadParser) parseSummary(value gjson.Result) {
	root := value
	if response := value.Get("response"); response.IsObject() {
		root = response
	} else if message := value.Get("message"); message.IsObject() {
		root = message
	}
	kind := value.Get("type").String()
	if o.stream {
		if strings.HasPrefix(kind, "response.") || strings.HasPrefix(kind, "message_") || value.Get("choices").IsArray() {
			o.response.EndMarkerPolicy = "required"
		}
	}
	if problem := root.Get("error"); problem.IsObject() {
		o.response.Error = true
		o.upstream.ErrorType = o.parsing.metadataString(problem.Get("type").String(), 128)
		if o.upstream.ErrorType == "" {
			o.upstream.ErrorType = o.parsing.metadataString(problem.Get("status").String(), 128)
		}
		o.upstream.ErrorCode = o.parsing.metadataString(problem.Get("code").String(), 128)
		o.fields |= traceFieldErrorType | traceFieldErrorCode
	} else if problem.Type == gjson.String {
		o.response.Error = true
	}
	if kind == "error" || kind == "response.failed" || root.Get("status").String() == "failed" {
		o.response.Error = true
	}
	value.Get("choices").ForEach(func(_, choice gjson.Result) bool {
		o.setFinishReason(choice.Get("finish_reason"))
		return true
	})
	o.setFinishReason(root.Get("stop_reason"))
	o.setFinishReason(value.Get("delta.stop_reason"))
	if candidates := value.Get("candidates"); candidates.IsArray() {
		o.response.EndMarkerPolicy = "not_applicable"
		candidates.ForEach(func(_, candidate gjson.Result) bool {
			o.setFinishReason(candidate.Get("finishReason"))
			return true
		})
	}
	if value.Get("promptFeedback").IsObject() {
		o.response.EndMarkerPolicy = "not_applicable"
		o.captureGeminiBlockReason(value)
	}
	switch kind {
	case "message_stop", "response.completed", "response.done", "response.failed", "response.incomplete":
		o.response.Done = true
		o.response.EndMarker = strings.Clone(kind)
	}
	if status := root.Get("status"); status.Type == gjson.String {
		switch status.Str {
		case "completed", "failed", "incomplete":
			o.setFinishReason(status)
			if detail := root.Get("incomplete_details.reason"); detail.Type == gjson.String && detail.Str != "" {
				reason := status.Str + ":" + o.parsing.metadataString(detail.Str, 128)
				o.finishReason = &reason
				o.response.Refused = o.response.Refused || detail.Str == "content_filter"
			}
		}
	}
	if o.finishReason == nil && (kind == "response.failed" || kind == "response.incomplete") {
		reason := strings.Clone(strings.TrimPrefix(kind, "response."))
		o.finishReason = &reason
	}
	if !o.stream {
		o.response.Done = true
	}
}

type TraceResponseBody struct {
	io.ReadCloser
	Observer    *TraceBodyObserver
	ContentType string
}

func (body *TraceResponseBody) Read(data []byte) (int, error) {
	n, err := body.ReadCloser.Read(data)
	if n > 0 {
		body.Observer.Observe(data[:n], body.ContentType)
	}
	if err != nil {
		if !errors.Is(err, io.EOF) {
			body.Observer.readFailed()
		}
		body.Observer.finishRead()
	}
	return n, err
}

func (body *TraceResponseBody) Close() error {
	err := body.ReadCloser.Close()
	body.Observer.finishRead()
	return err
}
