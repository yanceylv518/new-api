package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
)

var (
	taskErrorBearerPattern = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[^\s"',;<>]+`)
	taskErrorAPIKeyPattern = regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]+`)
	taskErrorSecretPattern = regexp.MustCompile(`(?i)\b(?:api[_-]?key|(?:secret[_-]?)?access[_-]?key(?:[_-]?id)?|access[_-]?token|refresh[_-]?token|session[_-]?token|client[_-]?secret|private[_-]?key|token|secret|password|authorization|cookie|credentials?)["']?\s*[:=]\s*["']?[^\s"',;<>]+`)
)

const (
	taskUpstreamErrorHeadersKey = "task_upstream_error_headers"
	taskUpstreamErrorModelKey   = "task_upstream_error_model"
	taskUpstreamRetryErrorKey   = "task_upstream_retry_error"
)

type taskUpstreamErrorSnapshot struct {
	status      int
	contentType string
	body        string
}

// 保留 Seedance 适配器非原生入口的上游提交错误，结果不确定时公开本站任务 ID。
func respondSeedanceAdapterSubmissionError(c *gin.Context, taskErr *dto.TaskError) bool {
	if taskErr == nil || taskErr.LocalError || taskErr.Code != "fail_to_fetch_task" || taskErr.Error == nil ||
		taskErr.StatusCode < http.StatusBadRequest || taskErr.StatusCode > 599 {
		return false
	}
	if action := c.GetString("task_action"); action != "text_to_video" && action != "image_to_video" {
		return false
	}
	pinned, ok := common.GetContextKeyType[pluginruntime.PinnedPlugin](c, pluginruntime.ContextKeyPinnedPlugin)
	if !ok || pinned.Plugin == nil || (pinned.Plugin.Meta.Key != "originocean" && pinned.Plugin.Meta.Key != "zzengine") {
		return false
	}
	route, native := common.GetContextKeyType[pluginruntime.PinnedRoute](c, pluginruntime.ContextKeyPinnedRoute)
	if native && route.Route.Protocol == "seedance-v3" {
		return false
	}
	raw := taskErr.Error.Error()
	if strings.TrimSpace(raw) == "" {
		return false
	}
	if data, ok := taskErr.Data.(map[string]any); ok {
		if taskID, ok := data["task_id"].(string); ok && taskID != "" {
			c.Header("X-Task-ID", taskID)
			c.Writer.Header().Add("Access-Control-Expose-Headers", "X-Task-ID")
		}
	}
	return writeTaskUpstreamError(c, taskErr.StatusCode, raw, taskUpstreamErrorContext{})
}

func writeTaskUpstreamError(c *gin.Context, status int, raw string, extra taskUpstreamErrorContext) bool {
	sanitized, ok := sanitizeTaskUpstreamMessage(c, raw, extra)
	if !ok {
		return false
	}
	if !json.Valid([]byte(sanitized)) {
		c.Data(status, "text/plain; charset=utf-8", []byte(sanitized))
	} else {
		c.Data(status, "application/json; charset=utf-8", []byte(sanitized))
	}
	return true
}

func sanitizeTaskUpstreamMessage(c *gin.Context, raw string, extra taskUpstreamErrorContext) (string, bool) {
	redactor := newTaskUpstreamErrorRedactor(c, extra)
	pinned, _ := common.GetContextKeyType[pluginruntime.PinnedPlugin](c, pluginruntime.ContextKeyPinnedPlugin)
	if route, ok := common.GetContextKeyType[pluginruntime.PinnedRoute](c, pluginruntime.ContextKeyPinnedRoute); ok && route.Plugin != nil {
		pinned.Plugin = route.Plugin
	}
	zzengine := pinned.Plugin != nil && pinned.Plugin.Meta.Key == "zzengine"
	if !json.Valid([]byte(raw)) {
		message := redactor.text(raw, "")
		if zzengine {
			return marshalZZEngineAPIError(message)
		}
		return message, true
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var body any
	if decoder.Decode(&body) != nil {
		return "", false
	}
	sanitized := redactor.value(body, "", 0)
	if zzengine {
		return marshalZZEngineAPIError(sanitized)
	}
	if reflect.DeepEqual(body, sanitized) {
		return raw, true
	}
	encoded, err := common.Marshal(sanitized)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// 上游诊断字段不进入方舟错误体；只保留错误详情和官方可选的 type、param。
func marshalZZEngineAPIError(value any) (string, bool) {
	problem := map[string]any{"code": "upstream_error", "message": "Upstream request failed"}
	switch body := value.(type) {
	case string:
		if strings.TrimSpace(body) != "" {
			problem["message"] = body
		}
	case map[string]any:
		if message, ok := body["message"].(string); ok && strings.TrimSpace(message) != "" {
			problem["message"] = message
		}
		source := body
		if failure, ok := body["error"].(map[string]any); ok {
			source = failure
		}
		for _, field := range []string{"code", "message", "type", "param"} {
			value, exists := source[field]
			if !exists {
				continue
			}
			if text, ok := value.(string); ok && (field == "param" || strings.TrimSpace(text) != "") {
				problem[field] = text
			} else if code, ok := value.(json.Number); ok && field == "code" {
				problem[field] = string(code)
			} else if field == "param" && value == nil {
				problem[field] = nil
			}
		}
		if message, ok := body["error"].(string); ok && strings.TrimSpace(message) != "" {
			problem["message"] = message
		}
	}
	encoded, err := common.Marshal(map[string]any{"error": problem})
	return string(encoded), err == nil
}

// 提交响应已被受理但解析失败时，仅展示明确的业务错误，不推断任务或退款状态。
func seedanceSubmitBusinessError(c *gin.Context) (string, bool) {
	saved, ok := common.GetContextKeyType[json.RawMessage](c, "task_submission_raw_response")
	if !ok || len(saved) == 0 {
		return "", false
	}
	var body map[string]json.RawMessage
	if common.Unmarshal(saved, &body) != nil {
		return "", false
	}
	objects := []map[string]json.RawMessage{body}
	for index := 0; index < len(objects); index++ {
		if index >= 64 {
			return "", false
		}
		for field, raw := range objects[index] {
			name := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(field), "_", ""), "-", "")
			if name == "id" || name == "taskid" {
				if string(bytes.TrimSpace(raw)) != "null" {
					var id string
					if common.Unmarshal(raw, &id) != nil || strings.TrimSpace(id) != "" {
						return "", false
					}
				}
				continue
			}
			if name != "data" && name != "task" && name != "result" && name != "payload" && name != "response" && name != "error" {
				continue
			}
			switch common.GetJsonType(raw) {
			case "unknown", "null":
				continue
			case "array":
				var items []json.RawMessage
				if common.Unmarshal(raw, &items) != nil || len(objects)+len(items) > 64 {
					return "", false
				}
				for _, item := range items {
					var nested map[string]json.RawMessage
					if common.Unmarshal(item, &nested) != nil || nested == nil {
						return "", false
					}
					objects = append(objects, nested)
				}
			case "object":
				var nested map[string]json.RawMessage
				if common.Unmarshal(raw, &nested) != nil {
					return "", false
				}
				objects = append(objects, nested)
			default:
				return "", false
			}
		}
	}
	var response struct {
		Error   json.RawMessage `json:"error"`
		Success *bool           `json:"success"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if common.Unmarshal(saved, &response) != nil {
		return "", false
	}
	var problem struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if common.Unmarshal(response.Error, &problem) == nil && seedanceBusinessErrorHasDetail(problem.Code, problem.Message) {
		return string(saved), true
	}
	if response.Success == nil || *response.Success ||
		!seedanceBusinessErrorHasDetail(response.Code, response.Message) {
		return "", false
	}
	return string(saved), true
}

func seedanceBusinessErrorHasDetail(rawCode json.RawMessage, message string) bool {
	if strings.TrimSpace(message) != "" {
		return true
	}
	code := ""
	switch common.GetJsonType(rawCode) {
	case "string":
		if common.Unmarshal(rawCode, &code) != nil {
			return false
		}
	case "number":
		code = string(rawCode)
	}
	return strings.TrimSpace(code) != "" && strings.TrimSpace(code) != "0"
}

type taskUpstreamErrorRedactor struct {
	secrets       []string
	headerValues  []taskUpstreamErrorHeader
	privateNames  []*regexp.Regexp
	publicModel   string
	privateModels []string
	modelPattern  *regexp.Regexp
	upstreamTask  string
	publicTask    string
}

type taskUpstreamErrorHeader struct {
	name      string
	value     string
	sensitive bool
}

type taskUpstreamErrorContext struct {
	baseURL        string
	channelName    string
	key            string
	upstreamTask   string
	publicTask     string
	publicModel    string
	privateModel   string
	headerOverride map[string]any
}

func newTaskUpstreamErrorRedactor(c *gin.Context, extra taskUpstreamErrorContext) taskUpstreamErrorRedactor {
	redactor := taskUpstreamErrorRedactor{
		secrets: []string{
			common.GetContextKeyString(c, constant.ContextKeyChannelKey),
			common.GetContextKeyString(c, constant.ContextKeyTokenKey),
			c.GetHeader("Authorization"),
			extra.key,
		},
		publicModel:  extra.publicModel,
		upstreamTask: extra.upstreamTask,
		publicTask:   extra.publicTask,
	}
	if redactor.publicModel == "" {
		redactor.publicModel = c.GetString("resolved_task_model")
	}
	if redactor.publicModel == "" {
		redactor.publicModel = common.GetContextKeyString(c, constant.ContextKeyOriginalModel)
	}
	redactor.privateModels = append(redactor.privateModels, extra.privateModel, c.GetString(taskUpstreamErrorModelKey))
	for _, headers := range []map[string]any{common.GetContextKeyStringMap(c, constant.ContextKeyChannelHeaderOverride), c.GetStringMap(taskUpstreamErrorHeadersKey), extra.headerOverride} {
		for name, rawValue := range headers {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" || name == "*" || strings.HasPrefix(name, "re:") || strings.HasPrefix(name, "regex:") ||
				name == "content-type" || name == "accept" || name == "accept-encoding" || name == "user-agent" {
				continue
			}
			value, ok := rawValue.(string)
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "{client_header:") && strings.HasSuffix(value, "}") {
				value = c.GetHeader(strings.TrimSuffix(strings.TrimPrefix(value, "{client_header:"), "}"))
			} else {
				key := extra.key
				if key == "" {
					key = common.GetContextKeyString(c, constant.ContextKeyChannelKey)
				}
				value = strings.ReplaceAll(value, "{api_key}", key)
			}
			if value != "" {
				sensitive := strings.Contains(name, "auth") || strings.Contains(name, "key") || strings.Contains(name, "token") ||
					strings.Contains(name, "secret") || strings.Contains(name, "password") || strings.Contains(name, "cookie") ||
					strings.Contains(name, "credential") || strings.Contains(name, "signature") || strings.Contains(name, "private") ||
					strings.Contains(name, "session") || strings.HasSuffix(name, "-sign")
				redactor.headerValues = append(redactor.headerValues, taskUpstreamErrorHeader{name: name, value: value, sensitive: sensitive})
				if fields := strings.Fields(value); len(fields) == 2 && (strings.EqualFold(fields[0], "bearer") || strings.EqualFold(fields[0], "basic")) {
					redactor.headerValues = append(redactor.headerValues, taskUpstreamErrorHeader{name: name, value: fields[1], sensitive: true})
				}
			}
		}
	}
	sort.SliceStable(redactor.secrets, func(i, j int) bool { return len(redactor.secrets[i]) > len(redactor.secrets[j]) })
	sort.SliceStable(redactor.headerValues, func(i, j int) bool { return len(redactor.headerValues[i].value) > len(redactor.headerValues[j].value) })
	var mapping map[string]string
	if common.UnmarshalJsonStr(common.GetContextKeyString(c, constant.ContextKeyChannelModelMapping), &mapping) == nil {
		seen := map[string]bool{redactor.publicModel: true}
		for model := mapping[redactor.publicModel]; model != "" && !seen[model]; model = mapping[model] {
			seen[model] = true
			redactor.privateModels = append(redactor.privateModels, model)
		}
	}
	if pinned, ok := common.GetContextKeyType[pluginruntime.PinnedRoute](c, pluginruntime.ContextKeyPinnedRoute); ok {
		redactor.privateModels = append(redactor.privateModels, pinned.DeclaredModel)
	}
	if pinned, ok := common.GetContextKeyType[pluginruntime.PinnedEndpoint](c, pluginruntime.ContextKeyPinnedEndpoint); ok {
		redactor.privateModels = append(redactor.privateModels, pinned.MappedModel)
	}
	if redactor.publicModel != "" {
		models := append([]string{redactor.publicModel}, redactor.privateModels...)
		sort.SliceStable(models, func(i, j int) bool { return len(models[i]) > len(models[j]) })
		patterns := make([]string, 0, len(models))
		for _, model := range models {
			if model != "" {
				patterns = append(patterns, regexp.QuoteMeta(model))
			}
		}
		redactor.modelPattern = regexp.MustCompile("(?i)(?:" + strings.Join(patterns, "|") + ")")
	}

	names := []string{"originocean"}
	var pluginKey string
	if pinned, ok := common.GetContextKeyType[pluginruntime.PinnedRoute](c, pluginruntime.ContextKeyPinnedRoute); ok && pinned.Plugin != nil {
		pluginKey = pinned.Plugin.Meta.Key
	} else if pinned, ok := common.GetContextKeyType[pluginruntime.PinnedPlugin](c, pluginruntime.ContextKeyPinnedPlugin); ok && pinned.Plugin != nil {
		pluginKey = pinned.Plugin.Meta.Key
	}
	if pluginKey != "" {
		name := pluginKey
		if name != "doubao" && len(name) >= 5 && !strings.Contains(strings.ToLower(redactor.publicModel), strings.ToLower(name)) {
			names = append(names, name)
		}
	}
	for _, rawAddress := range []string{common.GetContextKeyString(c, constant.ContextKeyChannelBaseUrl), extra.baseURL} {
		if address, err := url.Parse(rawAddress); err == nil {
			host := address.Hostname()
			if host != "" {
				names = append(names, host)
				for _, part := range strings.Split(host, ".") {
					if len(part) >= 4 && part != "www" && part != "openapi" && part != "cloud" && part != "video" {
						names = append(names, part)
						break
					}
				}
			}
		}
	}
	for _, name := range []string{common.GetContextKeyString(c, constant.ContextKeyChannelName), extra.channelName} {
		if len(name) >= 5 && name != redactor.publicModel {
			isModel := false
			for _, model := range redactor.privateModels {
				isModel = isModel || name == model
			}
			if !isModel {
				names = append(names, name)
			}
		}
	}
	for _, name := range names {
		if name != "" && !taskErrorProtocolField(name) && !strings.Contains(strings.ToLower(redactor.publicModel), strings.ToLower(name)) {
			redactor.privateNames = append(redactor.privateNames, regexp.MustCompile("(?i)"+regexp.QuoteMeta(name)))
		}
	}
	return redactor
}

func (r taskUpstreamErrorRedactor) value(value any, field string, depth int) any {
	if depth > 100 {
		return "[redacted]"
	}
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		var suffixes map[string]int
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			left, right := taskErrorProtocolField(keys[i]), taskErrorProtocolField(keys[j])
			if left != right {
				return left
			}
			return keys[i] < keys[j]
		})
		for _, key := range keys {
			item := value[key]
			name := key
			if !taskErrorProtocolField(key) {
				for _, secret := range r.secrets {
					if secret != "" {
						name = strings.ReplaceAll(name, secret, "[redacted]")
					}
				}
				for _, header := range r.headerValues {
					name = redactTaskHeaderValue(name, header)
				}
				if r.modelPattern != nil {
					name = r.modelPattern.ReplaceAllStringFunc(name, func(string) string { return r.publicModel })
				}
				for _, privateName := range r.privateNames {
					name = privateName.ReplaceAllString(name, "[redacted]")
				}
				name = taskErrorAPIKeyPattern.ReplaceAllString(name, "[redacted]")
			}
			if _, exists := result[name]; exists {
				base := name
				if suffixes == nil {
					suffixes = make(map[string]int)
				}
				suffix := max(2, suffixes[base])
				for {
					name = base + "_" + strconv.Itoa(suffix)
					suffix++
					if _, exists = result[name]; !exists {
						suffixes[base] = suffix
						break
					}
				}
			}
			if taskUpstreamSensitiveErrorField(key) {
				if r.publicTask != "" && strings.EqualFold(strings.ReplaceAll(key, "_", ""), "taskid") {
					if id, ok := item.(string); ok && (id == r.publicTask || r.upstreamTask != "" && id == r.upstreamTask) {
						result[name] = r.publicTask
						continue
					}
				}
				if strings.EqualFold(key, "token") {
					if _, ok := item.(json.Number); ok {
						result[name] = item
						continue
					}
				}
				result[name] = "[redacted]"
			} else {
				result[name] = r.value(item, key, depth+1)
			}
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, item := range value {
			result[i] = r.value(item, field, depth+1)
		}
		return result
	case string:
		trimmed := strings.TrimSpace(value)
		if (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) && json.Valid([]byte(trimmed)) {
			decoder := json.NewDecoder(bytes.NewBufferString(trimmed))
			decoder.UseNumber()
			var nested any
			if decoder.Decode(&nested) == nil {
				sanitized := r.value(nested, field, depth+1)
				if reflect.DeepEqual(nested, sanitized) {
					return value
				}
				if encoded, err := common.Marshal(sanitized); err == nil {
					return string(encoded)
				}
			}
		}
		return r.text(value, field)
	default:
		return value
	}
}

func (r taskUpstreamErrorRedactor) text(value, field string) string {
	if r.upstreamTask != "" && r.publicTask != "" {
		value = strings.ReplaceAll(value, r.upstreamTask, r.publicTask)
	}
	for _, secret := range r.secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	for _, header := range r.headerValues {
		value = redactTaskHeaderValue(value, header)
	}
	if r.modelPattern != nil {
		value = r.modelPattern.ReplaceAllStringFunc(value, func(string) string { return r.publicModel })
	}
	for _, name := range r.privateNames {
		value = name.ReplaceAllString(value, "[redacted]")
	}
	value = taskErrorBearerPattern.ReplaceAllString(value, "[redacted]")
	value = taskErrorAPIKeyPattern.ReplaceAllString(value, "[redacted]")
	value = taskErrorSecretPattern.ReplaceAllString(value, "[redacted]")
	if !strings.EqualFold(field, "code") && !strings.EqualFold(field, "type") && field != "key" {
		value = common.MaskSensitiveInfo(value)
	}
	return value
}

// 普通请求头短值仅匹配完整词，避免替换普通单词中的字符。
func redactTaskHeaderValue(value string, header taskUpstreamErrorHeader) string {
	if header.value == "" {
		return value
	}
	if len(header.value) >= 8 {
		return strings.ReplaceAll(value, header.value, "[redacted]")
	}
	if !header.sensitive && value != header.value && !strings.Contains(strings.ToLower(value), header.name) {
		return value
	}
	if !strings.Contains(value, header.value) {
		return value
	}
	var result strings.Builder
	start := 0
	for {
		index := strings.Index(value[start:], header.value)
		if index < 0 {
			result.WriteString(value[start:])
			return result.String()
		}
		begin, end := start+index, start+index+len(header.value)
		left, right := begin == 0, end == len(value)
		if !left {
			r, _ := utf8.DecodeLastRuneInString(value[:begin])
			left = !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
		}
		if !right {
			r, _ := utf8.DecodeRuneInString(value[end:])
			right = !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
		}
		result.WriteString(value[start:begin])
		if left && right {
			result.WriteString("[redacted]")
		} else {
			result.WriteString(value[begin:end])
		}
		start = end
	}
}

func taskErrorProtocolField(field string) bool {
	name := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, field)
	switch name {
	case "error", "errors", "code", "message", "type", "data", "success", "result", "payload", "response", "output", "object", "status", "statuscode", "statusmessage", "statusmsg", "httpstatus", "id", "model", "reason", "failurereason", "detail", "details", "description", "field", "param", "parameter", "metadata", "usage", "timestamp", "errorcode", "errormessage", "errortype", "errordescription", "taskid", "requestid", "traceid":
		return true
	default:
		return false
	}
}

func taskUpstreamSensitiveErrorField(field string) bool {
	name := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, field)
	switch name {
	case "apikey", "xapikey", "accesskey", "accesskeyid", "secretkey", "secretaccesskey", "accesstoken", "refreshtoken", "sessiontoken", "clientsecret", "privatekey", "token", "secret", "password", "authorization", "credential", "credentials", "headers", "cookie", "cookies", "channel", "channelid", "channelname", "provider", "providerid", "providername", "internalprovider", "vendor", "vendorid", "vendorname", "baseurl", "hostname", "upstreamid", "upstreamname", "upstreamprovider", "upstreamurl", "upstreambaseurl", "upstreamtaskid", "taskid":
		return true
	default:
		return false
	}
}
