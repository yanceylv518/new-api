package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
)

var seedanceNativeParameterNames = map[string]struct{}{
	"model": {}, "prompt": {}, "content": {},
	"resolution": {}, "ratio": {}, "duration": {}, "frames": {},
	"generate_audio": {}, "watermark": {}, "output_format": {}, "seed": {},
	"camera_fixed": {}, "return_last_frame": {}, "draft": {},
	"omni_reference_task_type": {}, "service_tier": {}, "callback_url": {},
	"execution_expires_after": {}, "priority": {}, "safety_identifier": {}, "tools": {}, "moderation_options": {},
}

var seedanceCompatTaskFieldOrder = [...]string{
	"id", "model", "status", "error", "content", "usage", "created_at", "updated_at",
	"seed", "resolution", "ratio", "duration", "frames", "framespersecond",
	"service_tier", "execution_expires_after", "generate_audio", "draft", "draft_task_id", "priority",
}

// 未返回的字段不补默认值，其他上游字段保留在官方字段之后。
func marshalSeedanceCompatTaskResponse(fields map[string]any) ([]byte, error) {
	return marshalSeedanceCompatFields(fields, seedanceCompatTaskFieldOrder[:], true)
}

func marshalSeedanceCompatFields(fields map[string]any, order []string, task bool) ([]byte, error) {
	keys := make([]string, 0, len(fields))
	for _, key := range order {
		if _, exists := fields[key]; exists {
			keys = append(keys, key)
		}
	}
	var extra []string
	for key := range fields {
		if !slices.Contains(order, key) {
			extra = append(extra, key)
		}
	}
	slices.Sort(extra)
	keys = append(keys, extra...)

	var response bytes.Buffer
	response.WriteByte('{')
	for index, key := range keys {
		name, err := common.Marshal(key)
		if err != nil {
			return nil, err
		}
		var value []byte
		if nested, ok := fields[key].(map[string]any); task && ok && (key == "content" || key == "usage") {
			nestedOrder := []string{"video_url", "last_frame_url"}
			if key == "usage" {
				nestedOrder = []string{"completion_tokens", "total_tokens"}
			}
			value, err = marshalSeedanceCompatFields(nested, nestedOrder, false)
		} else {
			value, err = common.Marshal(fields[key])
		}
		if err != nil {
			return nil, err
		}
		if index > 0 {
			response.WriteByte(',')
		}
		response.Write(name)
		response.WriteByte(':')
		response.Write(value)
	}
	response.WriteByte('}')
	return response.Bytes(), nil
}

// 兼容参数先合并进实际请求，确保校验、预扣和表达式读取同一组值。
func normalizeSeedanceNativeCompatibility(c *gin.Context, request *pluginruntime.RouteRequestContext) error {
	container, ok := request.Body.(map[string]any)
	if !ok || container["kind"] != string(pluginruntime.BodyJSON) {
		return nil
	}
	input, ok := container["value"].(map[string]any)
	if !ok {
		return nil
	}
	_, hasParameters := input["parameters"]
	_, hasPrompt := input["prompt"]
	if !hasParameters && !hasPrompt {
		return nil
	}
	var body map[string]json.RawMessage
	if err := common.Unmarshal(request.BodyText, &body); err != nil || body == nil {
		return fmt.Errorf("request body must be a JSON object")
	}
	changed := false
	if raw, exists := body["parameters"]; exists {
		var parameters map[string]json.RawMessage
		if err := common.Unmarshal(raw, &parameters); err != nil || parameters == nil {
			return fmt.Errorf("parameters must be an object")
		}
		for key, value := range parameters {
			if _, allowed := seedanceNativeParameterNames[key]; !allowed {
				return fmt.Errorf("unsupported parameters field: %s", key)
			}
			body[key] = value
		}
		delete(body, "parameters")
		changed = true
	}
	if prompt, exists := body["prompt"]; exists {
		var text string
		if err := common.Unmarshal(prompt, &text); err != nil {
			if _, hasContent := body["content"]; !hasContent {
				return fmt.Errorf("prompt must be a string")
			}
		} else if strings.TrimSpace(text) != "" {
			content := make([]json.RawMessage, 0)
			if raw, present := body["content"]; present {
				if err := common.Unmarshal(raw, &content); err != nil || content == nil {
					return fmt.Errorf("content must be an array")
				}
			}
			hasText := false
			for _, entry := range content {
				var item struct {
					Type string `json:"type"`
				}
				if common.Unmarshal(entry, &item) == nil && item.Type == "text" {
					hasText = true
					break
				}
			}
			if !hasText {
				entry, err := common.Marshal(map[string]any{"type": "text", "text": text})
				if err != nil {
					return err
				}
				content = append(content, entry)
				encoded, err := common.Marshal(content)
				if err != nil {
					return err
				}
				body["content"] = encoded
				delete(body, "prompt")
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	encoded, err := common.Marshal(body)
	if err != nil {
		return err
	}
	storage, err := common.CreateBodyStorage(encoded)
	if err != nil {
		return err
	}
	if err := replaceSeedanceRequestBody(c, storage, c.ContentType()); err != nil {
		return err
	}
	var normalized any
	if err := common.Unmarshal(encoded, &normalized); err != nil {
		return err
	}
	container["value"] = normalized
	request.BodyText = encoded
	return nil
}
