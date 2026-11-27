package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
)

// BuildTaskPluginView converts a persisted task into the deliberately narrow
// public shape permitted at JavaScript plugin boundaries.
func BuildTaskPluginView(task *model.Task) (dto.TaskView, error) {
	return BuildTaskPluginViewWithMediaPolicies(task, nil)
}

func BuildTaskPluginViewWithMediaPolicies(task *model.Task, policies TaskMediaProxyPolicies) (dto.TaskView, error) {
	createdAt := task.CreatedAt
	if createdAt == 0 {
		createdAt = task.SubmitTime
	}
	view := dto.TaskView{
		TaskID:     task.TaskID,
		Platform:   string(model.PublicTaskPlatform(task.Platform)),
		Action:     task.Action,
		Status:     string(task.Status),
		Progress:   task.Progress,
		FailReason: PublicTaskModelText(task.FailReason, task.Properties.OriginModelName, task.Properties.UpstreamModelName),
		CreatedAt:  createdAt,
		UpdatedAt:  task.UpdatedAt,
		FinishedAt: task.FinishTime,
	}
	if len(task.Data) > 0 {
		data, err := PublicTaskDataWithMediaPolicies(task, policies)
		if err != nil {
			return dto.TaskView{}, err
		}
		if err := common.Unmarshal(data, &view.Data); err != nil {
			return dto.TaskView{}, err
		}
		view.Data = replacePrivateTaskID(view.Data, task.PrivateData.UpstreamTaskID, task.TaskID)
	}
	return view, nil
}

// PublicTaskModelText 只替换已知映射名，保留错误内容供用户排查。
func PublicTaskModelText(value, publicModel, upstreamModel string) string {
	if publicModel == "" || upstreamModel == "" || publicModel == upstreamModel {
		return value
	}
	return strings.NewReplacer(publicModel, publicModel, upstreamModel, publicModel).Replace(value)
}

// PublicTaskData 仅修改对外副本，不改变轮询与计费使用的任务快照。
func PublicTaskData(task *model.Task) (json.RawMessage, error) {
	return PublicTaskDataWithMediaPolicies(task, nil)
}

func PublicTaskDataWithMediaPolicies(task *model.Task, policies TaskMediaProxyPolicies) (json.RawMessage, error) {
	data, err := taskDataWithoutMediaProxy(task)
	if err != nil {
		return nil, err
	}
	return ProjectTaskMediaDataWithPolicies(task, data, policies)
}

func taskDataWithoutMediaProxy(task *model.Task) (json.RawMessage, error) {
	if task == nil {
		return nil, nil
	}
	data := task.Data
	if len(data) == 0 {
		return data, nil
	}
	if (task.Platform == "doubao" || task.Platform == "originocean" || task.Platform == "qstart" || task.Platform == "zzengine") && common.GetJsonType(data) == "object" {
		var fields map[string]json.RawMessage
		if err := common.Unmarshal(data, &fields); err != nil {
			return nil, err
		}
		changed := false
		if task.Platform == "zzengine" {
			// 回调不经过插件轮询，统一从提交状态回显，保留用户显式传入的空字符串。
			var state struct {
				SafetyIdentifier *string `json:"safety_identifier"`
			}
			if len(task.PrivateData.PluginState) > 0 {
				if err := common.Unmarshal(task.PrivateData.PluginState, &state); err != nil {
					return nil, err
				}
			}
			delete(fields, "safety_identifier")
			if state.SafetyIdentifier != nil {
				encoded, err := common.Marshal(*state.SafetyIdentifier)
				if err != nil {
					return nil, err
				}
				fields["safety_identifier"] = encoded
			}
			changed = true
		}
		if marker, exists := fields["_seedance_inferred_service_tier"]; exists {
			var inferred bool
			if err := common.Unmarshal(marker, &inferred); err != nil {
				return nil, err
			}
			delete(fields, "_seedance_inferred_service_tier")
			changed = true
			if inferred {
				delete(fields, "service_tier")
			}
		}
		if _, exists := fields["model"]; exists && task.Platform == "originocean" && task.Properties.OriginModelName != "" {
			encoded, err := common.Marshal(task.Properties.OriginModelName)
			if err != nil {
				return nil, err
			}
			fields["model"] = encoded
			changed = true
		}
		if changed {
			var err error
			data, err = common.Marshal(fields)
			if err != nil {
				return nil, err
			}
		}
	}
	if task.Platform == "originocean" || task.Platform == "zzengine" || task.Properties.OriginModelName != "" && task.Properties.UpstreamModelName != "" && task.Properties.OriginModelName != task.Properties.UpstreamModelName {
		var err error
		data, err = publicTaskJSON(data, task.Properties.OriginModelName, task.Properties.UpstreamModelName, 0, task.Platform == "originocean" || task.Platform == "zzengine")
		if err != nil {
			return nil, err
		}
	}
	if task.Platform == "originocean" && (strings.EqualFold(task.Action, "text_to_video") || strings.EqualFold(task.Action, "image_to_video")) && common.GetJsonType(data) == "object" {
		var err error
		data, err = originOceanVideoPublicData(data)
		if err != nil {
			return nil, err
		}
	}
	if task.Platform == "zzengine" {
		return zzengineVideoPublicData(data)
	}
	if task.Platform != "anyllm" || task.Status != model.TaskStatusSuccess {
		return data, nil
	}
	// 原生结果直接交付上游签名媒体地址，已有快照中的本站路径从私有媒体记录还原。
	var state struct {
		Media struct {
			Video     string `json:"video"`
			LastFrame string `json:"last_frame"`
		} `json:"media"`
	}
	if len(task.PrivateData.PluginState) == 0 {
		return data, nil
	}
	if err := common.Unmarshal(task.PrivateData.PluginState, &state); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if len(fields["content"]) == 0 {
		return data, nil
	}
	var content map[string]json.RawMessage
	if err := common.Unmarshal(fields["content"], &content); err != nil {
		return nil, err
	}
	changed := false
	for _, field := range []struct{ name, value, path string }{
		{"video_url", state.Media.Video, "/v1/videos/" + task.TaskID + "/content"},
		{"last_frame_url", state.Media.LastFrame, "/v1/tasks/" + task.TaskID + "/artifacts/last-frame/content"},
	} {
		var value string
		if len(content[field.name]) == 0 {
			continue
		}
		if err := common.Unmarshal(content[field.name], &value); err != nil {
			return nil, err
		}
		if value != field.path || field.value == "" {
			continue
		}
		if (!strings.HasPrefix(field.value, "https://") && !strings.HasPrefix(field.value, "http://")) ||
			(task.PrivateData.Key != "" && strings.Contains(field.value, task.PrivateData.Key)) {
			return nil, fmt.Errorf("invalid public task media URL")
		}
		encoded, err := common.Marshal(field.value)
		if err != nil {
			return nil, err
		}
		content[field.name] = encoded
		changed = true
	}
	if !changed {
		return data, nil
	}
	var err error
	fields["content"], err = common.Marshal(content)
	if err != nil {
		return nil, err
	}
	return common.Marshal(fields)
}

// 原生查询和回调共用官方字段投影，任务快照仍保留结算所需的上游数据。
func originOceanVideoPublicData(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if nested := fields["upstream_response"]; common.GetJsonType(nested) == "object" {
		var upstream map[string]json.RawMessage
		if err := common.Unmarshal(nested, &upstream); err != nil {
			return nil, err
		}
		for field, value := range upstream {
			switch field {
			// 历史快照的顶层错误已经脱敏，不能用未经同样处理的内层错误覆盖。
			case "id", "task_id", "model", "status", "upstream_response", "error", "failure_reason":
				continue
			}
			fields[field] = value
		}
	}
	for field := range fields {
		switch field {
		case "id", "model", "status", "error", "content", "usage", "created_at", "updated_at",
			"seed", "resolution", "ratio", "duration", "frames", "framespersecond", "service_tier",
			"execution_expires_after", "generate_audio", "draft", "draft_task_id", "priority",
			"output_format", "safety_identifier", "tools", "reasoning_effort", "revised_prompt", "failure_reason",
			"subdivisionlevel", "fileformat":
		default:
			delete(fields, field)
		}
	}
	if rawContent, exists := fields["content"]; exists && common.GetJsonType(rawContent) == "object" {
		var content map[string]json.RawMessage
		if err := common.Unmarshal(rawContent, &content); err != nil {
			return nil, err
		}
		for field := range content {
			if field != "video_url" && field != "last_frame_url" && field != "file_url" {
				delete(content, field)
			}
		}
		if value, exists := content["last_frame_url"]; exists && strings.TrimSpace(string(value)) == "null" {
			delete(content, "last_frame_url")
		}
		encoded, err := common.Marshal(content)
		if err != nil {
			return nil, err
		}
		fields["content"] = encoded
	}
	return common.Marshal(fields)
}

// 查询、列表和回调只公开方舟视频字段，原始快照仍用于轮询和结算。
func zzengineVideoPublicData(raw json.RawMessage) (json.RawMessage, error) {
	var response struct {
		ID      *string         `json:"id,omitempty"`
		Model   *string         `json:"model,omitempty"`
		Status  string          `json:"status,omitempty"`
		Error   json.RawMessage `json:"error,omitempty"`
		Content *struct {
			VideoURL     *string `json:"video_url,omitempty"`
			LastFrameURL *string `json:"last_frame_url,omitempty"`
		} `json:"content,omitempty"`
		Usage *struct {
			CompletionTokens *int64 `json:"completion_tokens,omitempty"`
			TotalTokens      *int64 `json:"total_tokens,omitempty"`
			ToolUsage        *struct {
				WebSearch *int64 `json:"web_search,omitempty"`
			} `json:"tool_usage,omitempty"`
		} `json:"usage,omitempty"`
		CreatedAt             *int64  `json:"created_at,omitempty"`
		UpdatedAt             *int64  `json:"updated_at,omitempty"`
		Seed                  *int64  `json:"seed,omitempty"`
		Resolution            *string `json:"resolution,omitempty"`
		Ratio                 *string `json:"ratio,omitempty"`
		Duration              *int64  `json:"duration,omitempty"`
		Frames                *int64  `json:"frames,omitempty"`
		FramesPerSecond       *int64  `json:"framespersecond,omitempty"`
		ServiceTier           *string `json:"service_tier,omitempty"`
		ExecutionExpiresAfter *int64  `json:"execution_expires_after,omitempty"`
		GenerateAudio         *bool   `json:"generate_audio,omitempty"`
		Draft                 *bool   `json:"draft,omitempty"`
		DraftTaskID           *string `json:"draft_task_id,omitempty"`
		Priority              *int64  `json:"priority,omitempty"`
		OutputFormat          *string `json:"output_format,omitempty"`
		SafetyIdentifier      *string `json:"safety_identifier,omitempty"`
		Tools                 *[]struct {
			Type string `json:"type"`
		} `json:"tools,omitempty"`
	}
	if common.GetJsonType(raw) != "object" {
		return nil, fmt.Errorf("invalid ZZEngine task response")
	}
	if err := common.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("invalid ZZEngine task response: %w", err)
	}
	switch response.Status {
	case "pending":
		response.Status = "queued"
	case "processing":
		response.Status = "running"
	case "", "queued", "running", "succeeded", "failed", "cancelled", "expired":
	default:
		return nil, fmt.Errorf("invalid ZZEngine task status")
	}
	if response.Tools != nil {
		for _, tool := range *response.Tools {
			if tool.Type != "web_search" {
				return nil, fmt.Errorf("invalid ZZEngine video tool")
			}
		}
	}
	var problem struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	switch common.GetJsonType(response.Error) {
	case "object":
		if err := common.Unmarshal(response.Error, &problem); err != nil {
			return nil, fmt.Errorf("invalid ZZEngine task error: %w", err)
		}
	case "string":
		if err := common.Unmarshal(response.Error, &problem.Message); err != nil {
			return nil, err
		}
	case "null", "unknown":
	default:
		return nil, fmt.Errorf("invalid ZZEngine task error")
	}
	if problem.Message == "" && (response.Status == "failed" || response.Status == "expired") {
		var failure struct {
			FailureReason json.RawMessage `json:"failure_reason"`
			Reason        json.RawMessage `json:"reason"`
			Message       json.RawMessage `json:"message"`
		}
		if err := common.Unmarshal(raw, &failure); err != nil {
			return nil, fmt.Errorf("invalid ZZEngine failure reason: %w", err)
		}
		for _, rawMessage := range []json.RawMessage{failure.FailureReason, failure.Reason, failure.Message} {
			if common.GetJsonType(rawMessage) != "string" {
				continue
			}
			var message string
			if err := common.Unmarshal(rawMessage, &message); err != nil {
				return nil, err
			}
			if message != "" {
				problem.Message = message
				break
			}
		}
	}
	if response.Status == "succeeded" {
		response.Error = json.RawMessage("null")
	} else if problem.Code != "" || problem.Message != "" {
		encoded, err := common.Marshal(problem)
		if err != nil {
			return nil, err
		}
		response.Error = encoded
	} else {
		response.Error = nil
	}
	return common.Marshal(response)
}

func publicTaskJSON(raw json.RawMessage, publicModel, upstreamModel string, depth int, redactSecrets bool) (json.RawMessage, error) {
	if depth > 100 {
		return nil, fmt.Errorf("task response nesting exceeds limit")
	}
	switch common.GetJsonType(raw) {
	case "object":
		var fields map[string]json.RawMessage
		if err := common.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		for key, value := range fields {
			if redactSecrets {
				switch strings.ToLower(key) {
				case "encrypted_key", "api_key", "secret_key", "access_token", "authorization":
					delete(fields, key)
					continue
				}
			}
			if common.GetJsonType(value) == "string" {
				var text string
				if err := common.Unmarshal(value, &text); err != nil {
					return nil, err
				}
				switch key {
				case "model", "model_name", "modelName", "origin_model_name", "upstream_model_name", "actual_model":
					if publicModel != "" && upstreamModel != "" && strings.EqualFold(text, upstreamModel) {
						text = publicModel
					}
				case "message", "reason", "fail_reason", "error":
					text = PublicTaskModelText(text, publicModel, upstreamModel)
				default:
					continue
				}
				encoded, err := common.Marshal(text)
				if err != nil {
					return nil, err
				}
				fields[key] = encoded
				continue
			}
			projected, err := publicTaskJSON(value, publicModel, upstreamModel, depth+1, redactSecrets)
			if err != nil {
				return nil, err
			}
			fields[key] = projected
		}
		return common.Marshal(fields)
	case "array":
		var items []json.RawMessage
		if err := common.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		for i, item := range items {
			projected, err := publicTaskJSON(item, publicModel, upstreamModel, depth+1, redactSecrets)
			if err != nil {
				return nil, err
			}
			items[i] = projected
		}
		return common.Marshal(items)
	default:
		return raw, nil
	}
}

// replacePrivateTaskID rewrites exact private IDs only in known task-ID fields.
// Map keys and opaque strings, including URLs containing the ID, are preserved.
func replacePrivateTaskID(value any, privateTaskID, publicTaskID string) any {
	if privateTaskID == "" || privateTaskID == publicTaskID {
		return value
	}
	switch typed := value.(type) {
	case []any:
		replaced := make([]any, len(typed))
		for index, item := range typed {
			replaced[index] = replacePrivateTaskID(item, privateTaskID, publicTaskID)
		}
		return replaced
	case map[string]any:
		replaced := make(map[string]any, len(typed))
		for key, item := range typed {
			if (key == "id" || key == "task_id" || key == "taskId") && item == privateTaskID {
				replaced[key] = publicTaskID
				continue
			}
			replaced[key] = replacePrivateTaskID(item, privateTaskID, publicTaskID)
		}
		return replaced
	default:
		return value
	}
}
