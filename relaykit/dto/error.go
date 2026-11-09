package dto

import (
	"encoding/json"
	"strings"

	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/tidwall/gjson"
)

//type OpenAIError struct {
//	Message string `json:"message"`
//	Type    string `json:"type"`
//	Param   string `json:"param"`
//	Code    any    `json:"code"`
//}

type OpenAIErrorWithStatusCode struct {
	Error      types.OpenAIError `json:"error"`
	StatusCode int               `json:"status_code"`
	LocalError bool
}

type GeneralErrorResponse struct {
	Error    json.RawMessage `json:"error"`
	Message  string          `json:"message"`
	Msg      string          `json:"msg"`
	Err      string          `json:"err"`
	ErrorMsg string          `json:"error_msg"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Detail   string          `json:"detail,omitempty"`
	Header   struct {
		Message string `json:"message"`
	} `json:"header"`
	Response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

// ExtractJSONError 优先提取精确 error 字段，兼容大小写别名；忽略 null、false 和空字符串。
// 调用方负责校验 JSON；后续空标记不会清除已经出现的业务错误。
func ExtractJSONError(body []byte) json.RawMessage {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil
	}
	var exact, alias json.RawMessage
	root.ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.Null || value.Type == gjson.False ||
			(value.Type == gjson.String && strings.TrimSpace(value.Str) == "") {
			return true
		}
		name := key.String()
		if name == "error" {
			exact = json.RawMessage(value.Raw)
		} else if strings.EqualFold(name, "error") {
			alias = json.RawMessage(value.Raw)
		}
		return true
	})
	if len(exact) > 0 {
		return exact
	}
	return alias
}

func (e GeneralErrorResponse) TryToOpenAIError() *types.OpenAIError {
	var openAIError types.OpenAIError
	if len(e.Error) > 0 {
		err := kitutil.Unmarshal(e.Error, &openAIError)
		if err == nil && openAIError.Message != "" {
			return &openAIError
		}
	}
	return nil
}

func (e GeneralErrorResponse) ToMessage() string {
	if len(e.Error) > 0 {
		switch kitutil.GetJsonType(e.Error) {
		case "object":
			var openAIError types.OpenAIError
			err := kitutil.Unmarshal(e.Error, &openAIError)
			if err == nil && openAIError.Message != "" {
				return openAIError.Message
			}
		case "string":
			var msg string
			err := kitutil.Unmarshal(e.Error, &msg)
			if err == nil && msg != "" {
				return msg
			}
		default:
			return string(e.Error)
		}
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != "" {
		return e.Err
	}
	if e.ErrorMsg != "" {
		return e.ErrorMsg
	}
	if e.Detail != "" {
		return e.Detail
	}
	if e.Header.Message != "" {
		return e.Header.Message
	}
	if e.Response.Error.Message != "" {
		return e.Response.Error.Message
	}
	return ""
}
