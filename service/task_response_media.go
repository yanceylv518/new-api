package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const TaskResponseMediaKeyPrefix = "response-media-"

var taskResponseMediaFields = map[string]string{
	"video_url":       "response-media-video",
	"first_frame_url": "response-media-first-frame",
	"last_frame_url":  "response-media-last-frame",
	"file_url":        "response-media-file",
}

var taskResponseTopLevelMediaFields = map[string]string{
	"video_url":       "response-media-top-video",
	"download_url":    "response-media-top-download",
	"first_frame_url": "response-media-top-first-frame",
	"last_frame_url":  "response-media-top-last-frame",
	"file_url":        "response-media-top-file",
}

type taskMediaProxyPolicy struct {
	enabled bool
	err     error
}

// TaskMediaProxyPolicies 是单次响应读取的渠道设置快照。
type TaskMediaProxyPolicies map[int]taskMediaProxyPolicy

// LoadTaskMediaProxyPolicies 从数据库批量读取最新设置，避免跨节点缓存延迟和逐条查询。
func LoadTaskMediaProxyPolicies(tasks []*model.Task) (TaskMediaProxyPolicies, error) {
	policies := make(TaskMediaProxyPolicies)
	ids := make([]int, 0, len(tasks))
	for _, task := range tasks {
		if task == nil || task.ChannelId <= 0 {
			continue
		}
		if _, exists := policies[task.ChannelId]; !exists {
			// 已删除渠道没有可确认的设置，默认保护历史媒体地址。
			policies[task.ChannelId] = taskMediaProxyPolicy{enabled: true}
			ids = append(ids, task.ChannelId)
		}
	}
	if len(ids) == 0 {
		return policies, nil
	}
	var channels []struct {
		Id      int
		Setting *string
	}
	if err := model.DB.Model(&model.Channel{}).Select("id", "setting").Where("id IN ?", ids).Find(&channels).Error; err != nil {
		return nil, fmt.Errorf("task channel configuration unavailable: %w", err)
	}
	for _, channel := range channels {
		var setting struct {
			Enabled bool `json:"task_media_proxy_enabled"`
		}
		if channel.Setting != nil && strings.TrimSpace(*channel.Setting) != "" {
			if err := common.UnmarshalJsonStr(*channel.Setting, &setting); err != nil {
				policies[channel.Id] = taskMediaProxyPolicy{err: fmt.Errorf("invalid task channel configuration: %w", err)}
				continue
			}
		}
		policies[channel.Id] = taskMediaProxyPolicy{enabled: setting.Enabled}
	}
	return policies, nil
}

func taskMediaProxyEnabled(task *model.Task, policies TaskMediaProxyPolicies) (bool, error) {
	if task == nil || task.ChannelId <= 0 {
		return false, nil
	}
	if policies != nil {
		if policy, exists := policies[task.ChannelId]; exists {
			return policy.enabled, policy.err
		}
	}
	loaded, err := LoadTaskMediaProxyPolicies([]*model.Task{task})
	if err != nil {
		return false, err
	}
	policy := loaded[task.ChannelId]
	return policy.enabled, policy.err
}

func taskResponseMediaKey(path []any) (string, error) {
	name, _ := path[len(path)-1].(string)
	if len(path) == 1 {
		if key := taskResponseTopLevelMediaFields[name]; key != "" {
			return key, nil
		}
	}
	if len(path) == 2 && path[0] == "content" {
		if key := taskResponseMediaFields[name]; key != "" {
			return key, nil
		}
	}
	encoded, err := common.Marshal(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	prefix := TaskResponseMediaKeyPrefix + "path-"
	if name == "first_frame_url" || name == "last_frame_url" {
		prefix = TaskResponseMediaKeyPrefix + "frame-"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func isTaskResponseMedia(task *model.Task, path []any, source string, parent map[string]json.RawMessage) bool {
	name, _ := path[len(path)-1].(string)
	switch name {
	case "video_url", "first_frame_url", "last_frame_url", "file_url", "download_url", "watermarked_url":
		return true
	case "url", "uri":
		if task != nil && source == strings.TrimSpace(task.GetResultURL()) && source != "" {
			return true
		}
		for _, part := range path[:len(path)-1] {
			switch part {
			case "video", "videos", "creations":
				return true
			case "outputs":
				var outputType string
				if common.GetJsonType(parent["type"]) == "string" && common.Unmarshal(parent["type"], &outputType) == nil && strings.EqualFold(outputType, "video") {
					return true
				}
			}
		}
		return task != nil && task.Platform == "hailuo" && len(path) > 1 && path[len(path)-2] == "content"
	}
	return false
}

// walkTaskResponseMedia 保留未修改的原始 JSON 值，只处理结果媒体字段。
func walkTaskResponseMedia(task *model.Task, data json.RawMessage, path []any, parent map[string]json.RawMessage, visit func(string, string) (string, error)) (json.RawMessage, bool, error) {
	if len(path) > 100 {
		return nil, false, fmt.Errorf("task response nesting exceeds limit")
	}
	switch common.GetJsonType(data) {
	case "object":
		var fields map[string]json.RawMessage
		if err := common.Unmarshal(data, &fields); err != nil {
			return nil, false, err
		}
		changed := false
		for name, value := range fields {
			next := append(append([]any(nil), path...), name)
			projected, updated, err := walkTaskResponseMedia(task, value, next, fields, visit)
			if err != nil {
				return nil, false, err
			}
			if updated {
				fields[name], changed = projected, true
			}
		}
		if !changed {
			return data, false, nil
		}
		encoded, err := common.Marshal(fields)
		return encoded, true, err
	case "array":
		var items []json.RawMessage
		if err := common.Unmarshal(data, &items); err != nil {
			return nil, false, err
		}
		changed := false
		for i, value := range items {
			next := append(append([]any(nil), path...), i)
			projected, updated, err := walkTaskResponseMedia(task, value, next, nil, visit)
			if err != nil {
				return nil, false, err
			}
			if updated {
				items[i], changed = projected, true
			}
		}
		if !changed {
			return data, false, nil
		}
		encoded, err := common.Marshal(items)
		return encoded, true, err
	case "string":
		if len(path) == 0 {
			return data, false, nil
		}
		var source string
		if err := common.Unmarshal(data, &source); err != nil {
			return nil, false, err
		}
		source = strings.TrimSpace(source)
		if source == "" || !isTaskResponseMedia(task, path, source, parent) {
			return data, false, nil
		}
		key, err := taskResponseMediaKey(path)
		if err != nil {
			return nil, false, err
		}
		result, err := visit(key, source)
		if err != nil || result == source {
			return data, false, err
		}
		encoded, err := common.Marshal(result)
		return encoded, true, err
	default:
		return data, false, nil
	}
}

// ProjectTaskMediaData 只修改响应副本；原始快照仍供产物代理读取。
func ProjectTaskMediaData(task *model.Task, data json.RawMessage) (json.RawMessage, error) {
	return ProjectTaskMediaDataWithPolicies(task, data, nil)
}

func ProjectTaskMediaDataWithPolicies(task *model.Task, data json.RawMessage, policies TaskMediaProxyPolicies) (json.RawMessage, error) {
	kind := common.GetJsonType(data)
	if task == nil || task.Action == "text_to_image" || task.Action == "image_to_image" || kind != "object" && kind != "array" {
		return data, nil
	}
	var enabled *bool
	projected, _, err := walkTaskResponseMedia(task, data, nil, nil, func(key, source string) (string, error) {
		if strings.HasPrefix(source, "data:") {
			return source, nil
		}
		if enabled == nil {
			value, err := taskMediaProxyEnabled(task, policies)
			if err != nil {
				return "", err
			}
			enabled = &value
		}
		if !*enabled {
			return source, nil
		}
		return BuildTaskResponseMediaURL(task.TaskID, key)
	})
	return projected, err
}

// TaskResponseMediaSource 从未投影的任务快照中寻找对应媒体。
func TaskResponseMediaSource(task *model.Task, artifactKey string) (string, bool, error) {
	if artifactKey == "response-media-result" {
		return strings.TrimSpace(task.GetResultURL()), true, nil
	}
	if !strings.HasPrefix(artifactKey, TaskResponseMediaKeyPrefix) {
		return "", false, nil
	}
	data := task.Data
	var err error
	if task.Platform == "anyllm" {
		data, err = taskDataWithoutMediaProxy(task)
		if err != nil {
			return "", true, err
		}
	}
	var source string
	_, _, err = walkTaskResponseMedia(task, data, nil, nil, func(key, value string) (string, error) {
		if key == artifactKey {
			source = value
		}
		return value, nil
	})
	if source == "" && (artifactKey == "response-media-video" || artifactKey == "response-media-top-video") {
		source = task.GetResultURL()
	}
	return strings.TrimSpace(source), true, err
}

// PublicTaskResultURL 对通用任务响应中的结果地址使用同一渠道设置。
func PublicTaskResultURL(task *model.Task) (string, error) {
	return publicTaskResultURL(task, nil)
}

func PublicTaskResultURLWithMediaPolicies(task *model.Task, policies TaskMediaProxyPolicies) (string, error) {
	return publicTaskResultURL(task, policies)
}

func publicTaskResultURL(task *model.Task, policies TaskMediaProxyPolicies) (string, error) {
	if task == nil {
		return "", nil
	}
	source := task.GetResultURL()
	if task.Action == "text_to_image" || task.Action == "image_to_image" || task.Status != model.TaskStatusSuccess ||
		(!strings.HasPrefix(source, "https://") && !strings.HasPrefix(source, "http://")) {
		return source, nil
	}
	enabled, err := taskMediaProxyEnabled(task, policies)
	if err != nil {
		return "", err
	}
	if !enabled {
		return source, nil
	}
	return BuildTaskResponseMediaURL(task.TaskID, "response-media-result")
}
