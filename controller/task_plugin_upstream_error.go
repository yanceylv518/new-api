package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
)

var (
	taskErrorBearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[^\s"',;<>]+`)
	taskErrorAPIKeyPattern = regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]+`)
	taskErrorSecretPattern = regexp.MustCompile(`(?i)\b(?:api[_-]?key|(?:secret[_-]?)?access[_-]?key(?:[_-]?id)?|access[_-]?token|refresh[_-]?token|session[_-]?token|client[_-]?secret|private[_-]?key|secret|password|authorization)\s*[:=]\s*["']?[^\s"',;<>]+`)
)

// 仅处理上游已明确拒绝的视频提交，保留原始错误结构及状态码。
func respondOriginOceanSubmissionError(c *gin.Context, taskErr *dto.TaskError) bool {
	if taskErr == nil || taskErr.LocalError || taskErr.Code != "fail_to_fetch_task" || taskErr.Error == nil ||
		!c.GetBool("task_submission_rejected") || taskErr.StatusCode < http.StatusBadRequest || taskErr.StatusCode > 599 {
		return false
	}
	if action := c.GetString("task_action"); action != "text_to_video" && action != "image_to_video" {
		return false
	}
	pinned, ok := common.GetContextKeyType[pluginruntime.PinnedPlugin](c, pluginruntime.ContextKeyPinnedPlugin)
	if !ok || pinned.Plugin == nil || pinned.Plugin.Meta.Key != "originocean" {
		return false
	}
	raw := taskErr.Error.Error()
	if strings.TrimSpace(raw) == "" {
		return false
	}

	redactor := newOriginOceanErrorRedactor(c)
	if !json.Valid([]byte(raw)) {
		c.Data(taskErr.StatusCode, "text/plain; charset=utf-8", []byte(redactor.text(raw, "")))
		return true
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var body any
	if decoder.Decode(&body) != nil {
		return false
	}
	sanitized := redactor.value(body, "", 0)
	if reflect.DeepEqual(body, sanitized) {
		c.Data(taskErr.StatusCode, "application/json; charset=utf-8", []byte(raw))
		return true
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return false
	}
	c.Data(taskErr.StatusCode, "application/json; charset=utf-8", encoded)
	return true
}

type originOceanErrorRedactor struct {
	secrets       []string
	privateNames  []*regexp.Regexp
	publicModel   string
	privateModels []string
	modelPattern  *regexp.Regexp
}

func newOriginOceanErrorRedactor(c *gin.Context) originOceanErrorRedactor {
	redactor := originOceanErrorRedactor{
		secrets: []string{
			common.GetContextKeyString(c, constant.ContextKeyChannelKey),
			common.GetContextKeyString(c, constant.ContextKeyTokenKey),
			c.GetHeader("Authorization"),
		},
		publicModel: c.GetString("resolved_task_model"),
	}
	if redactor.publicModel == "" {
		redactor.publicModel = common.GetContextKeyString(c, constant.ContextKeyOriginalModel)
	}
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
	if address, err := url.Parse(common.GetContextKeyString(c, constant.ContextKeyChannelBaseUrl)); err == nil {
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
	if name := common.GetContextKeyString(c, constant.ContextKeyChannelName); len(name) >= 5 && name != redactor.publicModel {
		isModel := false
		for _, model := range redactor.privateModels {
			isModel = isModel || name == model
		}
		if !isModel {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if name != "" {
			redactor.privateNames = append(redactor.privateNames, regexp.MustCompile("(?i)"+regexp.QuoteMeta(name)))
		}
	}
	return redactor
}

func (r originOceanErrorRedactor) value(value any, field string, depth int) any {
	if depth > 100 {
		return "[redacted]"
	}
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, item := range value {
			name := r.text(key, "key")
			if originOceanSensitiveErrorField(key) {
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
				if encoded, err := json.Marshal(sanitized); err == nil {
					return string(encoded)
				}
			}
		}
		return r.text(value, field)
	default:
		return value
	}
}

func (r originOceanErrorRedactor) text(value, field string) string {
	for _, secret := range r.secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	for _, name := range r.privateNames {
		value = name.ReplaceAllString(value, "[redacted]")
	}
	if r.modelPattern != nil {
		value = r.modelPattern.ReplaceAllStringFunc(value, func(string) string { return r.publicModel })
	}
	value = taskErrorBearerPattern.ReplaceAllString(value, "[redacted]")
	value = taskErrorAPIKeyPattern.ReplaceAllString(value, "[redacted]")
	value = taskErrorSecretPattern.ReplaceAllString(value, "[redacted]")
	if !strings.EqualFold(field, "code") && !strings.EqualFold(field, "type") && field != "key" {
		value = common.MaskSensitiveInfo(value)
	}
	return value
}

func originOceanSensitiveErrorField(field string) bool {
	name := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, field)
	switch name {
	case "apikey", "accesskey", "accesskeyid", "secretkey", "secretaccesskey", "accesstoken", "refreshtoken", "sessiontoken", "clientsecret", "privatekey", "token", "secret", "password", "authorization", "credential", "credentials", "headers", "cookie", "cookies", "channel", "channelid", "channelname", "provider", "providername", "upstreamurl", "upstreambaseurl", "upstreamtaskid", "taskid":
		return true
	default:
		return false
	}
}
