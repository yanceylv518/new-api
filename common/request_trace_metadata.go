package common

import (
	"bytes"
	"crypto/sha256"
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
const traceMaxKeys = 32
const traceMaxToolCalls = 256
const traceMaxToolNameBytes = 128
const traceMaxCachedPayloads = 64
const traceMaxCachedStrings = 256

var errTraceRequestTooLarge = errors.New("request trace shape exceeds capture limit")
var errTraceRequestInvalidJSON = errors.New("request trace shape is not valid JSON")

type traceKeyLayer uint8

const (
	traceKeysRoot traceKeyLayer = iota
	traceKeysError
	traceKeysUsage
	traceKeysChoices
	traceKeysMessage
	traceKeysCandidates
	traceKeyLayerCount
)

var traceKeyLayerNames = [traceKeyLayerCount]string{"root", "error", "usage", "choices", "message", "candidates"}

type traceUpstreamFields uint16

const (
	traceFieldResponseID traceUpstreamFields = 1 << iota
	traceFieldModel
	traceFieldObject
	traceFieldFingerprint
	traceFieldServiceTier
	traceFieldErrorType
	traceFieldErrorCode
)

type tracePayloadKey struct {
	digest [sha256.Size]byte
	stream bool
}

type traceToolCall struct {
	key         string
	name        string
	nameDelta   bool
	invalidName bool
}

type tracePayloadResponse struct {
	ContentCharacters     int64
	ReasoningCharacters   int64
	ReasoningTokens       *int64
	ReasoningOutputTokens *int64
	Chunks                int64
	EndMarker             string
	Error                 bool
	Refused               bool
	Done                  bool
	UsageChunk            bool
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

type tracePayloadKeys struct {
	names []string
}

type tracePayloadMetadata struct {
	response      tracePayloadResponse
	upstream      tracePayloadUpstream
	keys          [traceKeyLayerCount]*tracePayloadKeys
	finishReason  *string
	fields        traceUpstreamFields
	errorStatus   *string
	errorEvent    bool
	messageObject bool
	tools         []traceToolCall
	bytes         int
}

type tracePayloadParser struct {
	tracePayloadMetadata
	stream  bool
	parsing *tracePayloadCache
}

type traceStringKey struct {
	value string
	limit int
}

type tracePayloadCache struct {
	sync.Mutex
	payloads map[tracePayloadKey]*tracePayloadMetadata
	order    []tracePayloadKey
	bytes    int
	strings  map[traceStringKey]string
	lastKeys [traceKeyLayerCount]*tracePayloadKeys
	parser   tracePayloadParser
}

func (cache *tracePayloadCache) metadataString(value string, limit int) string {
	if value == "" {
		return ""
	}
	key := traceStringKey{value: value, limit: limit}
	if cached, exists := cache.strings[key]; exists {
		return cached
	}
	cleaned := strings.Clone(TraceMetadataString(value, limit))
	if len(value) <= 256 && len(cache.strings) < traceMaxCachedStrings {
		if cache.strings == nil {
			cache.strings = make(map[traceStringKey]string)
		}
		key.value = strings.Clone(value)
		cache.strings[key] = cleaned
	}
	return cleaned
}

func (cache *tracePayloadCache) parse(data []byte, stream, reuseOnly bool) tracePayloadMetadata {
	key := tracePayloadKey{digest: sha256.Sum256(data), stream: stream}
	cache.Lock()
	defer cache.Unlock()
	if metadata, exists := cache.payloads[key]; exists {
		return *metadata
	}
	// 工作区由请求内缓存锁串行使用；事件结果不引用下一次解析会改写的状态。
	parser := &cache.parser
	*parser = tracePayloadParser{stream: stream, parsing: cache}
	parser.parsePayload(data)
	metadata := parser.tracePayloadMetadata
	metadata.bytes = len(data)
	if !reuseOnly && len(data) <= TraceMetadataMaxBytes {
		if cache.payloads == nil {
			cache.payloads = make(map[tracePayloadKey]*tracePayloadMetadata)
		}
		for len(cache.order) > 0 && (len(cache.order) >= traceMaxCachedPayloads || cache.bytes+len(data) > TraceMetadataMaxBytes) {
			oldest := cache.order[0]
			cache.bytes -= cache.payloads[oldest].bytes
			delete(cache.payloads, oldest)
			cache.order = cache.order[1:]
		}
		stored := new(tracePayloadMetadata)
		*stored = metadata
		cache.payloads[key] = stored
		cache.order = append(cache.order, key)
		cache.bytes += len(data)
	}
	return metadata
}

func traceFieldNames(value gjson.Result) []string {
	keys := make([]string, 0)
	value.ForEach(func(key, _ gjson.Result) bool {
		keys = append(keys, TraceMetadataString(key.String(), 64))
		return len(keys) < traceMaxKeys
	})
	return keys
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
	shape := &RequestTraceShape{Keys: traceFieldNames(r)}
	for _, key := range []string{"messages", "contents", "input"} {
		value := r.Get(key)
		if value.IsArray() {
			value.ForEach(func(_, _ gjson.Result) bool {
				shape.Messages++
				return true
			})
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
	if r.Get("input").Type == gjson.String {
		shape.Messages = 1
	}
	shape.System = r.Get("system").Exists() || r.Get("systemInstruction").Exists() || r.Get("system_instruction").Exists()
	choice := r.Get("tool_choice")
	if choice.IsObject() {
		choice = choice.Get("type")
	}
	shape.ToolChoice = TraceMetadataString(choice.String(), 64)
	switch shape.ToolChoice {
	case "", "auto", "none", "required", "any", "tool", "function":
	default:
		shape.ToolChoice = "unknown"
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "generationConfig.maxOutputTokens"} {
		if value := r.Get(key); value.Type == gjson.Number {
			n := value.Int()
			shape.MaxTokens = &n
			break
		}
	}
	for _, key := range []string{"temperature", "generationConfig.temperature"} {
		if value := r.Get(key); value.Type == gjson.Number {
			n := value.Float()
			if !math.IsNaN(n) && !math.IsInf(n, 0) {
				shape.Temperature = &n
			}
			break
		}
	}
	shape.IncludeUsage = r.Get("stream_options.include_usage").Bool()
	for _, key := range []string{"messages", "contents", "input", "system", "systemInstruction", "system_instruction", "prompt"} {
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
	case media == "tool_use" || media == "tool_result" || media == "function_call" || media == "function_call_output":
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
	started      time.Time
	buffer       []byte
	event        []byte
	stream       bool
	finished     bool
	finishedAt   time.Time
	skippingLine bool
	bytes        int64
	response     RequestTraceResponse
	upstream     RequestTraceUpstream
	finishReason *string
	toolCalls    map[string]traceToolCall
	cache        *tracePayloadCache
	reuseOnly    bool
}

func NewTraceBodyObserver(started time.Time) *TraceBodyObserver {
	return &TraceBodyObserver{started: started, cache: &tracePayloadCache{}}
}

// 同一请求的采集器共享有界事件摘要，分别累计转换前后及重试统计；缓存不保留正文。
func (o *TraceBodyObserver) NewRelatedObserver() *TraceBodyObserver {
	o.Lock()
	defer o.Unlock()
	o.reuseOnly = true
	return &TraceBodyObserver{started: o.started, cache: o.cache}
}

func (o *TraceBodyObserver) Observe(data []byte, contentType string) {
	o.Lock()
	defer o.Unlock()
	if o.finished {
		return
	}
	o.bytes += int64(len(data))
	if o.response.ContentType != contentType {
		o.response.ContentType = TraceMetadataString(contentType, 128)
		o.stream = strings.Contains(contentType, "text/event-stream")
	}
	if !o.stream {
		if o.response.MetadataIncomplete {
			return
		}
		if len(o.buffer)+len(data) > TraceMetadataMaxBytes {
			o.buffer = nil
			o.markIncomplete("size_limit")
			return
		}
		o.buffer = append(o.buffer, data...)
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
				o.buffer = append(o.buffer, part...)
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
	o.buffer = nil
	o.event = nil
	o.finished = true
	o.finishedAt = time.Now()
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
	defer o.Unlock()
	response := o.response
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

func (o *TraceBodyObserver) Size() int64 {
	o.Lock()
	defer o.Unlock()
	return o.bytes
}

// HasError 读取已解析的错误状态；收尾时先 Finish，避免遗漏最后一个事件。
func (o *TraceBodyObserver) HasError() bool {
	o.Lock()
	defer o.Unlock()
	return o.response.Error
}

func (o *TraceBodyObserver) HasIncompleteMetadata() bool {
	o.Lock()
	defer o.Unlock()
	return o.response.MetadataIncomplete
}

func (o *tracePayloadParser) fieldNames(layer traceKeyLayer, value gjson.Result) {
	if !value.IsObject() {
		return
	}
	var keys []string
	if current := o.keys[layer]; current != nil {
		keys = current.names
	}
	if len(keys) >= traceMaxKeys {
		return
	}
	var names [traceMaxKeys]string
	count := copy(names[:], keys)
	visited := 0
	value.ForEach(func(keyValue, _ gjson.Result) bool {
		key := o.parsing.metadataString(keyValue.String(), 64)
		visited++
		found := false
		for _, existing := range names[:count] {
			if existing == key {
				found = true
				break
			}
		}
		if !found && count < traceMaxKeys {
			names[count] = key
			count++
		}
		return visited < traceMaxKeys && count < traceMaxKeys
	})
	if o.keys[layer] != nil && slices.Equal(keys, names[:count]) {
		return
	}
	// 复用同层最近一次的相同字段列表，缓存事件与工作区都不修改该列表。
	if last := o.parsing.lastKeys[layer]; last != nil && slices.Equal(last.names, names[:count]) {
		o.keys[layer] = last
		return
	}
	keys = make([]string, count)
	copy(keys, names[:count])
	o.keys[layer] = &tracePayloadKeys{names: keys}
	o.parsing.lastKeys[layer] = o.keys[layer]
}

func (o *tracePayloadParser) captureTool(key string, name gjson.Result, nameDelta bool) {
	if len(o.tools) >= traceMaxToolCalls {
		o.markIncomplete("tool_limit")
		return
	}
	tool := traceToolCall{key: key, nameDelta: nameDelta}
	if name.Exists() {
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

func (o *TraceBodyObserver) consumePayload(data []byte) {
	metadata := o.cache.parse(data, o.stream, o.reuseOnly)
	response := metadata.response
	o.response.Error = o.response.Error || response.Error
	o.response.ContentCharacters += response.ContentCharacters
	o.response.ReasoningCharacters += response.ReasoningCharacters
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
	o.response.UsageChunk = o.response.UsageChunk || response.UsageChunk
	o.response.MetadataIncomplete = o.response.MetadataIncomplete || response.MetadataIncomplete
	for _, reason := range response.IncompleteReasons {
		o.response.IncompleteReasons = traceIncompleteReason(o.response.IncompleteReasons, reason)
	}
	if response.EndMarkerPolicy != "" {
		o.response.EndMarkerPolicy = response.EndMarkerPolicy
	}
	if response.Chunks > 0 {
		o.response.Chunks++
		o.response.LastChunkMilliseconds = time.Since(o.started).Milliseconds()
	}
	for index, fields := range metadata.keys {
		if fields == nil {
			continue
		}
		layer, keys := traceKeyLayerNames[index], fields.names
		if o.response.Keys == nil {
			o.response.Keys = make(map[string][]string)
		}
		current := o.response.Keys[layer]
		if len(current) > 0 && slices.Equal(current, keys) {
			continue
		}
		for _, key := range keys {
			if len(current) >= traceMaxKeys {
				break
			}
			found := false
			for _, existing := range current {
				if existing == key {
					found = true
					break
				}
			}
			if !found {
				current = append(current, key)
			}
		}
		o.response.Keys[layer] = current
	}
	for _, tool := range metadata.tools {
		if o.stream && strings.HasPrefix(tool.key, "gemini:") {
			// 不同 Gemini 事件可在相同 parts 下标返回不同完整工具调用。
			tool.key += ":" + strconv.FormatInt(o.response.Chunks, 10)
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
	choices := r.Get("choices")
	candidates := r.Get("candidates")
	if o.stream {
		o.response.Chunks = 1
	}
	o.fieldNames(traceKeysRoot, r)
	kind := r.Get("type").String()
	if o.stream {
		switch {
		case candidates.IsArray():
			o.response.EndMarkerPolicy = "not_applicable"
		case choices.IsArray(), strings.HasPrefix(kind, "response."), strings.HasPrefix(kind, "message_"), strings.HasPrefix(kind, "content_block_"):
			o.response.EndMarkerPolicy = "required"
		}
	}
	if kind == "" && !choices.IsArray() && !candidates.IsArray() && !r.Get("output").IsArray() && !r.Get("error").IsObject() {
		o.markIncomplete("unsupported_format")
	}
	root := r
	nestedRoot := false
	if kind == "message_start" {
		if message := r.Get("message"); message.IsObject() {
			root = message
			nestedRoot = true
		}
	}
	if response := r.Get("response"); response.IsObject() {
		root = response
		nestedRoot = true
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
		o.fieldNames(traceKeysError, err)
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
	usage := root.Get("usage")
	hasUsage := usage.IsObject()
	if !hasUsage && nestedRoot {
		hasUsage = r.Get("usage").IsObject()
	}
	if hasUsage || r.Get("usageMetadata").IsObject() {
		o.response.UsageChunk = true
		o.fieldNames(traceKeysUsage, usage)
	}
	for _, fields := range []struct{ reasoning, output string }{
		{"usage.completion_tokens_details.reasoning_tokens", "usage.completion_tokens"},
		{"usage.output_tokens_details.reasoning_tokens", "usage.output_tokens"},
		{"usageMetadata.thoughtsTokenCount", ""},
	} {
		if value := root.Get(fields.reasoning); value.Type == gjson.Number && value.Int() >= 0 {
			n := value.Int()
			o.response.ReasoningTokens = &n
			// 仅将同一 usage 中标准 details 对应的输出总量用于“其中推理”展示。
			if fields.output != "" {
				if output := root.Get(fields.output); output.Type == gjson.Number && output.Int() >= n {
					total := output.Int()
					o.response.ReasoningOutputTokens = &total
				}
			}
			break
		}
	}
	if choices.IsArray() {
		choices.ForEach(func(_, choice gjson.Result) bool {
			o.fieldNames(traceKeysChoices, choice)
			if text := choice.Get("text"); text.Type == gjson.String {
				o.response.ContentCharacters += int64(utf8.RuneCountInString(text.String()))
			}
			if reason := choice.Get("finish_reason"); reason.Type == gjson.String {
				s := o.parsing.metadataString(reason.String(), 128)
				o.finishReason = &s
			}
			message := choice.Get("message")
			delta := choice.Get("delta")
			if delta.IsObject() {
				message = delta
			}
			o.fieldNames(traceKeysMessage, message)
			if content := message.Get("content"); content.Type == gjson.String {
				o.response.ContentCharacters += int64(utf8.RuneCountInString(content.String()))
			} else if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if text := part.Get("text"); text.Type == gjson.String {
						o.response.ContentCharacters += int64(utf8.RuneCountInString(text.String()))
					}
					return true
				})
			}
			for _, key := range []string{"reasoning_content", "reasoning"} {
				if text := message.Get(key); text.Type == gjson.String {
					o.response.ReasoningCharacters += int64(utf8.RuneCountInString(text.String()))
				}
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
		if source.Type == gjson.String {
			s := o.parsing.metadataString(source.String(), 128)
			o.finishReason = &s
		}
	}
	if kind == "message" || kind == "message_start" || kind == "message_delta" || kind == "content_block_start" || kind == "content_block_delta" {
		if kind == "content_block_delta" {
			o.response.ContentCharacters += int64(utf8.RuneCountInString(r.Get("delta.text").String()))
			o.response.ReasoningCharacters += int64(utf8.RuneCountInString(r.Get("delta.thinking").String()))
		}
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
	if kind == "message_stop" || kind == "response.completed" {
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
			o.fieldNames(traceKeysCandidates, candidate)
			if reason := candidate.Get("finishReason"); reason.Type == gjson.String {
				s := o.parsing.metadataString(reason.String(), 128)
				o.finishReason = &s
			}
			candidate.Get("content.parts").ForEach(func(partIndex, part gjson.Result) bool {
				if text := part.Get("text"); text.Type == gjson.String {
					n := int64(utf8.RuneCountInString(text.String()))
					if part.Get("thought").Bool() {
						o.response.ReasoningCharacters += n
					} else {
						o.response.ContentCharacters += n
					}
				}
				if part.Get("functionCall").IsObject() {
					key := "gemini:" + index.String() + ":" + partIndex.String()
					o.captureTool(key, part.Get("functionCall.name"), false)
				}
				return true
			})
			return true
		})
	}
	if strings.HasPrefix(kind, "response.") {
		if kind == "response.output_text.delta" {
			o.response.ContentCharacters += int64(utf8.RuneCountInString(r.Get("delta").String()))
		}
		if kind == "response.reasoning_text.delta" || kind == "response.reasoning_summary_text.delta" {
			o.response.ReasoningCharacters += int64(utf8.RuneCountInString(r.Get("delta").String()))
		}
		if kind == "response.refusal.delta" {
			o.response.Refused = true
		}
		if (kind == "response.output_item.added" || kind == "response.output_item.done") && r.Get("item.type").String() == "function_call" {
			o.captureTool("responses:"+o.toolIndex(r.Get("output_index")), r.Get("item.name"), false)
		}
	}
	if status := root.Get("status").String(); status == "completed" || status == "incomplete" || status == "failed" {
		if status == "failed" {
			o.response.Error = true
		}
		reason := strings.Clone(status)
		if detail := root.Get("incomplete_details.reason"); detail.Type == gjson.String && detail.String() != "" {
			reason += ":" + o.parsing.metadataString(detail.String(), 128)
		}
		o.finishReason = &reason
	}
	if !o.stream || kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" {
		root.Get("output").ForEach(func(index, output gjson.Result) bool {
			if output.Get("type").String() == "function_call" {
				o.captureTool("responses:"+index.String(), output.Get("name"), false)
			}
			if o.stream {
				return true
			}
			output.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					o.response.ContentCharacters += int64(utf8.RuneCountInString(part.Get("text").String()))
				}
				if part.Get("type").String() == "refusal" {
					o.response.Refused = true
				}
				return true
			})
			output.Get("summary").ForEach(func(_, part gjson.Result) bool {
				o.response.ReasoningCharacters += int64(utf8.RuneCountInString(part.Get("text").String()))
				return true
			})
			return true
		})
		o.response.Done = true
	}
}

func (o *tracePayloadParser) countContentBlock(block gjson.Result, key string) {
	switch block.Get("type").String() {
	case "text":
		o.response.ContentCharacters += int64(utf8.RuneCountInString(block.Get("text").String()))
	case "thinking":
		o.response.ReasoningCharacters += int64(utf8.RuneCountInString(block.Get("thinking").String()))
	case "tool_use", "server_tool_use":
		o.captureTool(key, block.Get("name"), false)
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
		body.Observer.Finish()
	}
	return n, err
}

func (body *TraceResponseBody) Close() error {
	err := body.ReadCloser.Close()
	body.Observer.Finish()
	return err
}
