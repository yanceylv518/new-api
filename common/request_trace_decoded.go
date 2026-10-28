package common

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const decodedRequestTraceKey = "decoded_request_trace_observer"

// UseDecodedRequestTrace 必须在读取响应前调用；只有接入解码上报的处理器可以接管字节观察器。
func UseDecodedRequestTrace(c *gin.Context, response *http.Response) {
	if c == nil || GetRequestTrace(c) == nil {
		return
	}
	c.Set(decodedRequestTraceKey, nil)
	if response == nil {
		return
	}
	body, ok := response.Body.(*TraceResponseBody)
	if !ok {
		return
	}
	o := body.Observer
	contentType := body.ContentType
	if contentType == "" {
		contentType = response.Header.Get("Content-Type")
	}
	o.Prepare(contentType, response.ContentLength)
	o.Lock()
	defer o.Unlock()
	if o.observed || o.finished {
		return
	}
	o.decoded = true
	o.upstreamUsage = true
	o.buffer, o.event = nil, nil
	c.Set(decodedRequestTraceKey, o)
}

// UseDecodedJSONRequestTrace 只用于非流式 JSON 处理器，防止上游误标 SSE 响应头。
func UseDecodedJSONRequestTrace(c *gin.Context, response *http.Response) {
	UseDecodedRequestTrace(c, response)
	if c == nil {
		return
	}
	trace := GetRequestTrace(c)
	if trace == nil {
		return
	}
	entry, exists := c.Get(decodedRequestTraceKey)
	observer, ok := entry.(*TraceBodyObserver)
	if !exists || !ok {
		return
	}
	observer.UseJSON()
	if trace.ClientObserver != nil {
		trace.ClientObserver.UseJSON()
	}
}

// UnmarshalRequestTraceResponse 复用业务解码结果，解码失败仍按原始字段记录诊断，不改变调用方错误处理。
func UnmarshalRequestTraceResponse(c *gin.Context, data []byte, value any) error {
	err := Unmarshal(data, value)
	if c != nil {
		if entry, exists := c.Get(decodedRequestTraceKey); exists {
			if observer, ok := entry.(*TraceBodyObserver); ok {
				if err != nil {
					observer.ObserveDecoded(data, nil)
				} else {
					if projected, ok := value.(interface{ TraceResponseValue() any }); ok {
						value = projected.TraceResponseValue()
					}
					observer.ObserveDecoded(data, value)
				}
			}
		}
	}
	return err
}

func (o *TraceBodyObserver) finishRead() {
	o.Lock()
	if o.decoded {
		// EOF/Close 可能先于消费协程处理完最后一帧，只结束传输计时。
		if o.finishedAt.IsZero() {
			o.finishedAt = time.Now()
		}
		o.Unlock()
		return
	}
	o.Unlock()
	o.Finish()
}

// RecordRequestTraceDone 保留扫描器截获的原始结束标记，收尾时在所有已解码事件之后合入。
func RecordRequestTraceDone(response *http.Response) {
	if response == nil {
		return
	}
	if body, ok := response.Body.(*TraceResponseBody); ok {
		body.Observer.Lock()
		body.Observer.pendingDone = body.Observer.decoded
		body.Observer.Unlock()
	}
}

type traceDecodedHeader struct {
	id, model, object string
	created           int64
	fingerprint       *string
	protocol          string
	choices           int
	stopReason        *string
}

// 只提取 DTO 已经解码的字段；兼容扩展、null 占位和不明确的下标仍交给原始字段兜底。
func (o *tracePayloadParser) parseDecoded(data []byte, value any, event string) bool {
	var header traceDecodedHeader
	switch v := value.(type) {
	case dto.ChatCompletionsStreamResponse:
		return o.parseDecoded(data, &v, event)
	case dto.ClaudeResponse:
		return o.parseDecoded(data, &v, event)
	case dto.ResponsesStreamResponse:
		return o.parseDecoded(data, &v, event)
	case dto.GeminiChatResponse:
		return o.parseDecoded(data, &v, event)
	case dto.OpenAITextResponse:
		return o.parseDecoded(data, &v, event)
	case dto.OpenAIResponsesResponse:
		return o.parseDecoded(data, &v, event)
	case *dto.ChatCompletionsStreamResponse:
		if v == nil || v.Choices == nil {
			return false
		}
		header = traceDecodedHeader{id: v.Id, model: v.Model, object: v.Object, created: v.Created, fingerprint: v.SystemFingerprint, protocol: "chat", choices: len(v.Choices)}
		for _, choice := range v.Choices {
			if choice.FinishReason != nil {
				o.setFinishReason(gjson.Result{Type: gjson.String, Str: *choice.FinishReason})
			}
			for index, tool := range choice.Delta.ToolCalls {
				if tool.Index == nil && tool.ID == "" && tool.Function.Name == "" && tool.Function.Arguments == "" {
					return false
				}
				if tool.Index != nil {
					index = *tool.Index
				}
				o.captureTool("openai:"+strconv.Itoa(choice.Index)+":"+strconv.Itoa(index), gjson.Result{Type: gjson.String, Str: tool.Function.Name}, o.stream)
			}
		}
	case *dto.OpenAITextResponse:
		if v == nil || v.Choices == nil || v.Error != nil {
			return false
		}
		header = traceDecodedHeader{id: v.Id, model: v.Model, object: v.Object, protocol: "chat", choices: len(v.Choices)}
		if v.Created != nil {
			created, ok := v.Created.(float64)
			if !ok || math.Abs(created) > 1<<53 {
				return false
			}
			header.created = int64(created)
		}
		for _, choice := range v.Choices {
			// 非流式 DTO 的空标量无法区分缺失与空字符串。
			if choice.FinishReason == "" {
				return false
			}
			o.setFinishReason(gjson.Result{Type: gjson.String, Str: choice.FinishReason})
			if len(choice.ToolCalls) > 0 {
				tools := gjson.Parse(string(choice.ToolCalls))
				if tools.IsArray() {
					tools.ForEach(func(index, tool gjson.Result) bool {
						if !tool.IsObject() {
							return true
						}
						position := index.String()
						if n := tool.Get("index"); n.Type == gjson.Number {
							position = n.String()
						} else if n.Exists() {
							o.markIncomplete("invalid_tool_index")
						}
						o.captureTool("openai:"+strconv.Itoa(choice.Index)+":"+position, tool.Get("function.name"), false)
						return true
					})
				}
			}
		}
	case *dto.ClaudeResponse:
		if v == nil || v.Error != nil || v.Type == "" || v.Type == "error" {
			return false
		}
		switch v.Type {
		case "message", "message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop":
		default:
			return false
		}
		header = traceDecodedHeader{id: v.Id, model: v.Model, protocol: "claude", stopReason: &v.StopReason}
		if v.Type == "message_start" && v.Message != nil {
			message := gjson.GetBytes(data, "message")
			if !message.IsObject() {
				return false
			}
			data = StringToByteSlice(message.Raw)
			header.id, header.model, header.stopReason = v.Message.Id, v.Message.Model, v.Message.StopReason
			o.messageObject = v.Message.Type == "message"
		}
		if v.Delta != nil && v.Delta.StopReason != nil {
			o.setFinishReason(gjson.Result{Type: gjson.String, Str: *v.Delta.StopReason})
		}
		if v.Type == "content_block_start" && v.ContentBlock != nil {
			position := ""
			if v.Index != nil {
				position = strconv.Itoa(*v.Index)
			}
			if v.ContentBlock.Type == "tool_use" || v.ContentBlock.Type == "server_tool_use" {
				o.captureTool("claude:"+position, gjson.Result{Type: gjson.String, Str: v.ContentBlock.Name}, false)
			}
		}
		if v.Type == "message" {
			o.messageObject = true
			for index, block := range v.Content {
				if block.Type == "tool_use" || block.Type == "server_tool_use" {
					o.captureTool("claude:"+strconv.Itoa(index), gjson.Result{Type: gjson.String, Str: block.Name}, false)
				}
			}
		}
		if v.Type == "message_stop" {
			o.response.Done, o.response.EndMarker = true, v.Type
		}
	case *dto.ResponsesStreamResponse:
		if v == nil || !strings.HasPrefix(v.Type, "response.") {
			return false
		}
		if v.Response != nil {
			response := gjson.GetBytes(data, "response")
			return response.IsObject() && o.parseDecoded(StringToByteSlice(response.Raw), v.Response, v.Type)
		}
		header.protocol = "responses"
		if v.Type == "response.output_item.added" || v.Type == "response.output_item.done" {
			if v.Item != nil {
				position := ""
				if v.OutputIndex != nil {
					position = strconv.Itoa(*v.OutputIndex)
				}
				o.captureDecodedResponsesTool("responses:"+position, v.Item)
			}
		}
		switch v.Type {
		case "response.refusal.delta":
			o.response.Refused = true
		case "response.completed", "response.done", "response.failed", "response.incomplete":
			o.response.Done, o.response.EndMarker = true, v.Type
			if v.Type == "response.failed" || v.Type == "response.incomplete" {
				o.response.Error = v.Type == "response.failed"
				reason := strings.TrimPrefix(v.Type, "response.")
				o.finishReason = &reason
			}
		}
	case *dto.OpenAIResponsesResponse:
		if v == nil || v.Error != nil || v.Output == nil {
			return false
		}
		header = traceDecodedHeader{id: v.ID, model: v.Model, object: v.Object, created: int64(v.CreatedAt), protocol: "responses"}
		status := gjson.Parse(string(v.Status))
		if status.Type == gjson.String && (status.Str == "completed" || status.Str == "incomplete" || status.Str == "failed") {
			reason := status.Str
			o.response.Error = reason == "failed"
			if v.IncompleteDetails != nil && v.IncompleteDetails.Reason != "" {
				reason += ":" + o.parsing.metadataString(v.IncompleteDetails.Reason, 128)
				o.response.Refused = v.IncompleteDetails.Reason == "content_filter"
			}
			o.finishReason = &reason
		}
		if !o.stream || event == "response.completed" || event == "response.done" || event == "response.failed" || event == "response.incomplete" {
			for index := range v.Output {
				output := &v.Output[index]
				o.captureDecodedResponsesTool("responses:"+strconv.Itoa(index), output)
				if !o.stream {
					for _, part := range output.Content {
						o.response.Refused = o.response.Refused || part.Type == "refusal"
					}
				}
			}
		}
	case *dto.GeminiChatResponse:
		if v == nil || v.Candidates == nil {
			return false
		}
		header = traceDecodedHeader{id: v.ResponseID, model: v.ModelVersion, protocol: "gemini"}
		for index, candidate := range v.Candidates {
			if candidate.FinishReason != nil {
				o.setFinishReason(gjson.Result{Type: gjson.String, Str: *candidate.FinishReason})
			}
			for position, part := range candidate.Content.Parts {
				if part.FunctionCall != nil {
					o.captureTool("gemini:"+strconv.Itoa(index)+":"+strconv.Itoa(position), gjson.Result{Type: gjson.String, Str: part.FunctionCall.FunctionName}, false)
				}
			}
		}
		if v.PromptFeedback != nil && v.PromptFeedback.BlockReason != nil {
			reason := *v.PromptFeedback.BlockReason
			if reason != "" && reason != "BLOCK_REASON_UNSPECIFIED" {
				o.response.Refused = true
				if o.finishReason == nil {
					o.setFinishReason(gjson.Result{Type: gjson.String, Str: reason})
				}
			}
		}
	default:
		return false
	}
	fields, created, stopReason, choices, extra := traceDecodedFields(data, header.protocol, o.stream, event != "")
	if extra || header.protocol == "chat" && header.choices != choices || fields&traceFieldFingerprint != 0 && header.fingerprint == nil {
		return false
	}
	for _, field := range []struct {
		flag   traceUpstreamFields
		value  string
		target *string
	}{
		{traceFieldResponseID, header.id, &o.upstream.ResponseID},
		{traceFieldModel, header.model, &o.upstream.Model},
		{traceFieldObject, header.object, &o.upstream.Object},
	} {
		if fields&field.flag != 0 {
			*field.target = o.parsing.metadataString(field.value, 128)
		}
	}
	if fields&traceFieldFingerprint != 0 && header.fingerprint != nil {
		o.upstream.Fingerprint = o.parsing.metadataString(*header.fingerprint, 128)
	}
	if fields&traceFieldServiceTier != 0 {
		service := gjson.GetBytes(data, "service_tier")
		o.upstream.ServiceTier = o.parsing.metadataString(service.Str, 128)
	}
	if fields&traceFieldReasoning != 0 {
		// DTO 的 int 默认值无法区分缺失、null 与显式 0，只读取小型 usage 子对象。
		o.captureReasoningTokens(gjson.GetBytes(data, "usage"), gjson.GetBytes(data, "usageMetadata"))
	}
	o.fields = fields
	if created {
		n := header.created
		o.upstream.Created = &n
	}
	if stopReason && header.stopReason != nil {
		// root.stop_reason 优先于 delta.stop_reason，后者最终覆盖前者。
		reason := o.finishReason
		o.setFinishReason(gjson.Result{Type: gjson.String, Str: *header.stopReason})
		if reason != nil {
			o.finishReason = reason
		}
	}
	if !o.stream {
		o.response.Done = true
	} else if header.protocol == "gemini" {
		o.response.EndMarkerPolicy = "not_applicable"
	} else {
		o.response.EndMarkerPolicy = "required"
	}
	switch event {
	case "response.completed", "response.done", "response.failed", "response.incomplete":
		o.response.Done, o.response.EndMarker = true, event
		if event == "response.failed" || event == "response.incomplete" {
			o.response.Error = o.response.Error || event == "response.failed"
			if o.finishReason == nil {
				reason := strings.TrimPrefix(event, "response.")
				o.finishReason = &reason
			}
		}
	case "response.refusal.delta":
		o.response.Refused = true
	}
	return true
}

func (o *tracePayloadParser) captureDecodedResponsesTool(key string, output *dto.ResponsesOutput) {
	name := output.Name
	switch output.Type {
	case "function_call", "custom_tool_call", "mcp_call":
	case "web_search_call", "file_search_call", "image_generation_call", "computer_call", "code_interpreter_call", "local_shell_call":
		name = output.Type
	default:
		return
	}
	o.captureTool(key, gjson.Result{Type: gjson.String, Str: name}, false)
}

// 业务 DTO 不一定声明兼容扩展字段。只扫描 JSON 键，避免正文内的关键词触发完整解析。
func traceDecodedFields(data []byte, protocol string, stream, responseObject bool) (traceUpstreamFields, bool, bool, int, bool) {
	var fields traceUpstreamFields
	var created, stopReason bool
	var choices int
	var depth int
	for index := 0; index < len(data); {
		start := bytes.IndexByte(data[index:], '"')
		if start < 0 {
			break
		}
		start += index + 1
		for _, value := range data[index : start-1] {
			switch value {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		end := start
		for {
			quote := bytes.IndexByte(data[end:], '"')
			if quote < 0 {
				return fields, created, stopReason, choices, true
			}
			end += quote
			backslashes := 0
			for position := end - 1; position >= start && data[position] == '\\'; position-- {
				backslashes++
			}
			if backslashes%2 == 0 {
				break
			}
			end++
		}
		escaped := bytes.IndexByte(data[start:end], '\\') >= 0
		index = end + 1
		for index < len(data) && (data[index] == ' ' || data[index] == '\t' || data[index] == '\r' || data[index] == '\n') {
			index++
		}
		if index >= len(data) || data[index] != ':' {
			continue
		}
		if escaped {
			return fields, created, stopReason, choices, true
		}
		key := string(data[start:end])
		value := bytes.TrimSpace(data[index+1:])
		if bytes.HasPrefix(value, []byte("null")) {
			if key == "index" || key == "output_index" || key == "name" && (protocol != "chat" || !stream) {
				return fields, created, stopReason, choices, true
			}
			continue
		}
		switch key {
		case "id", "responseId", "model", "modelVersion":
			if depth != 1 {
				continue
			}
			if (key == "id" || key == "model") && protocol != "chat" && protocol != "claude" && (protocol != "responses" || stream && !responseObject) || (key == "responseId" || key == "modelVersion") && protocol != "gemini" {
				return fields, created, stopReason, choices, true
			}
			if len(value) > 0 && value[0] == '"' {
				if key == "id" || key == "responseId" {
					fields |= traceFieldResponseID
				} else {
					fields |= traceFieldModel
				}
			}
		case "object":
			if depth != 1 {
				continue
			}
			if protocol != "chat" && (protocol != "responses" || stream && !responseObject) {
				return fields, created, stopReason, choices, true
			}
			if len(value) > 0 && value[0] == '"' {
				fields |= traceFieldObject
			}
		case "created", "created_at":
			if depth != 1 {
				continue
			}
			if key == "created" && protocol != "chat" || key == "created_at" && protocol != "responses" {
				return fields, created, stopReason, choices, true
			}
			created = len(value) > 0 && (value[0] == '-' || value[0] >= '0' && value[0] <= '9')
		case "system_fingerprint":
			if depth != 1 {
				continue
			}
			if protocol != "chat" {
				return fields, created, stopReason, choices, true
			}
			fields |= traceFieldFingerprint
		case "finishReason":
			if protocol != "gemini" {
				return fields, created, stopReason, choices, true
			}
		case "refusal":
			if !bytes.HasPrefix(value, []byte(`""`)) {
				return fields, created, stopReason, choices, true
			}
		case "index":
			if protocol == "chat" && depth == 3 {
				if len(value) == 0 || !(value[0] == '-' || value[0] >= '0' && value[0] <= '9') {
					return fields, created, stopReason, choices, true
				}
				choices++
			}
		case "message":
			if protocol != "chat" || stream {
				return fields, created, stopReason, choices, true
			}
		case "delta":
			if protocol == "chat" && !stream {
				return fields, created, stopReason, choices, true
			}
		case "stop_reason":
			if protocol != "claude" {
				return fields, created, stopReason, choices, true
			}
			stopReason = stopReason || depth == 1
		case "tool_calls":
			if protocol != "chat" {
				return fields, created, stopReason, choices, true
			}
		case "content_block":
			if protocol != "claude" {
				return fields, created, stopReason, choices, true
			}
		case "output", "item":
			if protocol != "responses" {
				return fields, created, stopReason, choices, true
			}
		case "status", "incomplete_details":
			if protocol != "responses" || depth == 1 && stream && !responseObject {
				return fields, created, stopReason, choices, true
			}
		case "blockReason":
			if protocol != "gemini" {
				return fields, created, stopReason, choices, true
			}
		case "reasoning_tokens", "thoughtsTokenCount":
			fields |= traceFieldReasoning
		case "service_tier":
			if depth == 1 && len(value) > 0 && value[0] == '"' {
				fields |= traceFieldServiceTier
			}
		case "error", "response", "function_call":
			return fields, created, stopReason, choices, true
		case "type":
			if bytes.HasPrefix(value, []byte(`"error"`)) {
				return fields, created, stopReason, choices, true
			}
		}
	}
	return fields, created, stopReason, choices, false
}

func (o *TraceBodyObserver) ObserveDecoded(data []byte, value any) {
	if o == nil {
		return
	}
	o.Lock()
	defer o.Unlock()
	if o.finished || o.source != nil || len(data) == 0 {
		return
	}
	if o.upstreamUsage && o.usageFormat == "" {
		// 错误事件可能没有协议标识，使用实际业务解码类型补充来源，不依据模型名猜测。
		switch value.(type) {
		case dto.ClaudeResponse, *dto.ClaudeResponse:
			o.usageFormat = "claude"
		case dto.ChatCompletionsStreamResponse, *dto.ChatCompletionsStreamResponse, *dto.OpenAITextResponse:
			o.usageFormat = "openai"
		case *dto.OpenAIResponsesResponse, *dto.ResponsesStreamResponse:
			o.usageFormat = "openai_responses"
		case *dto.GeminiChatResponse:
			o.usageFormat = "gemini"
		}
	}
	o.observed = o.observed || len(data) > 0
	if len(data) > o.bufferLimit() {
		o.captureUsage(data)
		o.markIncomplete("size_limit")
		return
	}
	if o.level == TraceMetadataSummary {
		o.consumePayload(bytes.TrimSpace(data))
		return
	}
	if o.workspace == nil {
		o.workspace = &traceParseWorkspace{}
	}
	o.workspace.Lock()
	parser := &o.workspace.parser
	*parser = tracePayloadParser{stream: o.stream, parsing: o.workspace}
	reused := parser.parseDecoded(data, value, "")
	metadata := parser.tracePayloadMetadata
	*parser = tracePayloadParser{}
	o.workspace.Unlock()
	if reused {
		o.captureUsage(data)
		o.mergeMetadata(metadata)
		return
	}
	// 原始扩展和业务解码失败继续兜底，不重复编码 DTO。
	o.consumePayload(bytes.TrimSpace(data))
}

func (o *TraceBodyObserver) ObserveDecodedEvent(data string, value any) {
	data = strings.ReplaceAll(data, "\r", "\\r")
	o.ObserveDecoded(StringToByteSlice(strings.TrimSpace(strings.TrimPrefix(data, "data:"))), value)
}

func (o *TraceBodyObserver) NeedsDecodedEvent() bool {
	o.Lock()
	defer o.Unlock()
	return !o.finished && o.source == nil && o.level == TraceMetadataFull
}
