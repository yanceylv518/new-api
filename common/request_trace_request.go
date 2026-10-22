package common

import (
	"bytes"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 只在已确定完整采集时提取；摘要请求不额外遍历内容，失败后仍由收尾按原始请求兜底。
func captureDecodedRequestTrace(c *gin.Context, storage BodyStorage, data []byte, value any) {
	trace := GetRequestTrace(c)
	if trace == nil || trace.Shape != nil || trace.Config.Level != "all" || !trace.SampledSuccess || storage.Size() > TraceMetadataMaxBytes {
		return
	}
	var shape RequestTraceShape
	var maxTokens *uint
	var toolChoice any
	var tools any
	claude := false
	if _, ok := value.(*dto.ClaudeRequest); ok {
		claude = true
	} else if _, ok := value.(*dto.GeneralOpenAIRequest); !ok {
		return
	}
	supported, hasToolChoice := traceDecodedRequestFields(data, claude)
	if !supported {
		return
	}
	switch request := value.(type) {
	case *dto.GeneralOpenAIRequest:
		if request == nil {
			return
		}
		shape.Messages = int64(len(request.Messages))
		if request.Messages == nil {
			if input, ok := request.Input.([]any); ok {
				shape.Messages = int64(len(input))
			} else if prompt, ok := request.Prompt.([]any); ok {
				shape.Messages = int64(len(prompt))
				if len(prompt) > 0 {
					if _, tokens := prompt[0].(float64); tokens {
						shape.Messages = 1
					}
				}
			}
		}
		_, inputString := request.Input.(string)
		_, promptString := request.Prompt.(string)
		if inputString || promptString {
			shape.Messages = 1
		}
		for _, message := range request.Messages {
			shape.System = shape.System || message.Role == "system" || message.Role == "developer"
			traceCountDecodedRequestContent(message.Content, &shape, 2)
		}
		traceCountDecodedRequestContent(request.Input, &shape, 0)
		traceCountDecodedRequestContent(request.Prompt, &shape, 0)
		if request.Tools != nil {
			for _, tool := range request.Tools {
				if tool.Type == "" && tool.ID == "" && tool.Function.Name == "" {
					// null 与空对象解码结果相同，仍以原始结构判定。
					return
				}
				shape.Tools++
			}
		} else if len(request.Functions) > 0 {
			declarations := gjson.Parse(string(request.Functions))
			if declarations.IsArray() {
				declarations.ForEach(func(_, item gjson.Result) bool {
					if item.IsObject() {
						shape.Tools++
					}
					return true
				})
			}
		}
		toolChoice = request.ToolChoice
		if toolChoice == nil && len(request.FunctionCall) > 0 {
			if hasToolChoice {
				return
			}
			choice := gjson.Parse(string(request.FunctionCall))
			if choice.IsObject() {
				shape.ToolChoice = "function"
			} else if choice.Type == gjson.String {
				toolChoice = choice.Str
			}
		}
		maxTokens = request.MaxTokens
		if maxTokens == nil {
			maxTokens = request.MaxCompletionTokens
		}
		shape.Temperature = request.Temperature
		shape.IncludeUsage = request.StreamOptions != nil && request.StreamOptions.IncludeUsage
	case *dto.ClaudeRequest:
		if request == nil {
			return
		}
		shape.Messages = int64(len(request.Messages))
		shape.System = request.System != nil
		traceCountDecodedRequestContent(request.System, &shape, 0)
		traceCountDecodedRequestContent(request.Prompt, &shape, 0)
		for _, message := range request.Messages {
			shape.System = shape.System || message.Role == "system" || message.Role == "developer"
			traceCountDecodedRequestContent(message.Content, &shape, 2)
		}
		tools, toolChoice = request.Tools, request.ToolChoice
		maxTokens, shape.Temperature = request.MaxTokens, request.Temperature
	default:
		// Responses/Gemini 的关键请求字段仍为 RawMessage 或有别名，继续使用原始形状兜底。
		return
	}
	if declarations, ok := tools.([]any); ok {
		for _, item := range declarations {
			tool, object := item.(map[string]any)
			if !object {
				continue
			}
			functions, array := tool["functionDeclarations"].([]any)
			if !array {
				functions, array = tool["function_declarations"].([]any)
			}
			if !array {
				shape.Tools++
				continue
			}
			for _, function := range functions {
				if _, ok := function.(map[string]any); ok {
					shape.Tools++
				}
			}
		}
	}
	if choice, ok := toolChoice.(map[string]any); ok {
		toolChoice = choice["type"]
	}
	if choice, ok := toolChoice.(string); ok {
		shape.ToolChoice = strings.ToLower(TraceMetadataString(choice, 64))
	} else if toolChoice != nil {
		return
	}
	switch shape.ToolChoice {
	case "", "auto", "none", "required", "any", "tool", "function":
	default:
		shape.ToolChoice = "unknown"
	}
	if maxTokens != nil {
		if uint64(*maxTokens) > math.MaxInt64 {
			return
		}
		n := int64(*maxTokens)
		shape.MaxTokens = &n
	}
	if shape.Temperature != nil {
		temperature := *shape.Temperature
		shape.Temperature = &temperature
	}
	trace.Shape = &shape
}

// 业务解码已经校验 JSON；这里只检查形状字段是否都被 DTO 保留，不解析正文值。
func traceDecodedRequestFields(data []byte, claude bool) (bool, bool) {
	var depth int
	var seen uint32
	for index := 0; index < len(data); index++ {
		switch data[index] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case '"':
			start := index + 1
			for index++; index < len(data); index++ {
				if data[index] == '\\' {
					index++
				} else if data[index] == '"' {
					break
				}
			}
			if index >= len(data) {
				continue
			}
			rest := bytes.TrimSpace(data[index+1:])
			if len(rest) == 0 || rest[0] != ':' {
				continue
			}
			key := data[start:index]
			if depth != 1 {
				if !claude && (bytes.Equal(key, []byte("functionDeclarations")) || bytes.Equal(key, []byte("function_declarations"))) {
					return false, false
				}
				continue
			}
			for _, letter := range key {
				if letter == '\\' || letter >= 'A' && letter <= 'Z' {
					return false, false
				}
			}
			var field uint32
			switch string(key) {
			case "messages":
				field = 1
			case "tools":
				field = 2
			case "tool_choice":
				field = 4
			case "max_tokens":
				field = 8
			case "temperature":
				field = 16
			case "system":
				if !claude {
					return false, false
				}
				field = 32
			case "input", "prompt", "functions", "function_call", "max_completion_tokens", "stream_options":
				if claude {
					return false, false
				}
				switch string(key) {
				case "input":
					field = 64
				case "prompt":
					field = 128
				case "functions":
					field = 256
				case "function_call":
					field = 512
				case "max_completion_tokens":
					field = 1024
				case "stream_options":
					field = 2048
				}
			case "contents", "instructions", "system_instruction", "generation_config", "max_output_tokens", "tool_config":
				return false, false
			}
			if field != 0 && seen&field != 0 {
				return false, false
			}
			seen |= field
		}
	}
	return true, seen&4 != 0
}

func traceCountDecodedRequestContent(value any, shape *RequestTraceShape, depth int) {
	if depth > 16 {
		return
	}
	switch content := value.(type) {
	case string:
		shape.TextCharacters += int64(utf8.RuneCountInString(content))
	case []any:
		for _, item := range content {
			traceCountDecodedRequestContent(item, shape, depth+1)
		}
	case map[string]any:
		role, _ := content["role"].(string)
		shape.System = shape.System || role == "system" || role == "developer"
		media, _ := content["type"].(string)
		var mime string
		for _, key := range []string{"inlineData", "fileData"} {
			if source, ok := content[key].(map[string]any); ok {
				mime, _ = source["mimeType"].(string)
				if mime != "" {
					break
				}
			}
		}
		_, image := content["image_url"]
		switch {
		case strings.Contains(media, "image") || strings.HasPrefix(mime, "image/") || image:
			shape.Images++
		case strings.Contains(media, "video") || strings.HasPrefix(mime, "video/"):
			shape.Videos++
		case strings.Contains(media, "audio") || strings.HasPrefix(mime, "audio/"):
			shape.Audios++
		case media == "file" || media == "input_file" || media == "document" || mime != "":
			shape.Files++
		case media == "tool_use" || media == "function_call":
		case media == "function_call_output":
			traceCountDecodedRequestContent(content["output"], shape, depth+1)
		default:
			for _, key := range []string{"content", "parts", "text"} {
				traceCountDecodedRequestContent(content[key], shape, depth+1)
			}
		}
	}
}
