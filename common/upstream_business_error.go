package common

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const upstreamBusinessErrorKey = "upstream_business_error"

type UpstreamBusinessError struct {
	HTTPStatus         int                  `json:"http_status"`
	Event              string               `json:"event"`
	ReportedStatusCode *int                 `json:"reported_status_code,omitempty"`
	Error              UpstreamErrorDetails `json:"error"`
	Note               string               `json:"note,omitempty"`
	Truncated          bool                 `json:"truncated,omitempty"`
}

type UpstreamErrorDetails struct {
	Type    string          `json:"type,omitempty"`
	Code    json.RawMessage `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Param   string          `json:"param,omitempty"`
	Status  string          `json:"status,omitempty"`
}

// 只记录协议处理器已确认的业务失败，HTTP 状态不根据错误文字推断。
func CaptureUpstreamBusinessError(c *gin.Context, httpStatus int, event string, payload string) {
	if c == nil || httpStatus != 200 || GetUpstreamBusinessError(c) != nil {
		return
	}
	document := gjson.Parse(payload)
	root := document
	if response := root.Get("response"); response.IsObject() {
		root = response
	}
	problem := root.Get("error")
	flatError := false
	hasError := problem.Type == gjson.String && strings.TrimSpace(problem.Str) != ""
	if problem.IsObject() {
		problem.ForEach(func(_, _ gjson.Result) bool {
			hasError = true
			return false
		})
	}
	if !hasError {
		if event != "response.failed" && event != "response.error" && document.Get("type").String() != "error" && root.Get("status").String() != "failed" {
			return
		}
		flatError = document.Get("type").String() == "error" || document.Get("type").String() == "response.error"
		problem = gjson.Result{}
		if flatError {
			problem = document
		}
	}
	record := &UpstreamBusinessError{HTTPStatus: httpStatus, Event: strings.Clone(TraceMetadataString(event, 64))}
	knownKeys := []string{c.GetString("key"), GetContextKeyString(c, constant.ContextKeyChannelKey)}
	clean := func(value gjson.Result, limit int) string {
		if value.Type != gjson.String {
			return ""
		}
		text := SanitizeErrorLogContent(value.Str, knownKeys...)
		record.Truncated = record.Truncated || len(text) > limit
		text = TraceMetadataString(text, limit)
		return strings.Clone(text)
	}
	if !flatError {
		record.Error.Type = clean(problem.Get("type"), 128)
	}
	record.Error.Message = clean(problem.Get("message"), 2048)
	record.Error.Param = clean(problem.Get("param"), 128)
	record.Error.Status = clean(problem.Get("status"), 128)
	if problem.Type == gjson.String {
		record.Error.Message = clean(problem, 2048)
	}
	code := problem.Get("code")
	if code.Type == gjson.String {
		encoded, err := Marshal(clean(code, 128))
		if err == nil {
			record.Error.Code = encoded
		}
	} else if code.Type == gjson.Number && len(code.Raw) <= 32 {
		record.Error.Code = json.RawMessage(strings.Clone(code.Raw))
	}
	for _, source := range []gjson.Result{problem, root, document} {
		for _, key := range []string{"status_code", "http_status"} {
			value := source.Get(key)
			if value.Type != gjson.Number {
				continue
			}
			status, err := strconv.Atoi(value.Raw)
			if err == nil && status >= 100 && status <= 599 {
				record.ReportedStatusCode = &status
				break
			}
		}
		if record.ReportedStatusCode != nil {
			break
		}
	}
	c.Set(upstreamBusinessErrorKey, record)
}

func GetUpstreamBusinessError(c *gin.Context) *UpstreamBusinessError {
	if c == nil {
		return nil
	}
	value, _ := c.Get(upstreamBusinessErrorKey)
	record, _ := value.(*UpstreamBusinessError)
	return record
}

func ClearUpstreamBusinessError(c *gin.Context) {
	if GetUpstreamBusinessError(c) != nil {
		c.Set(upstreamBusinessErrorKey, nil)
	}
}

func (record *UpstreamBusinessError) LogContent(note string, knownKeys ...string) string {
	snapshot := *record
	snapshot.Note = SanitizeErrorLogContent(note, knownKeys...)
	data, err := Marshal(snapshot)
	if err != nil {
		SysError("failed to encode upstream business error: " + err.Error())
		return strings.TrimSpace("上游业务失败（HTTP 200） " + snapshot.Note)
	}
	return string(data)
}
