package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

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

func taskMediaProxyEnabled(task *model.Task) (bool, error) {
	if task == nil || task.ChannelId <= 0 {
		return false, nil
	}
	channel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		return false, fmt.Errorf("task channel configuration unavailable: %w", err)
	}
	if channel.Setting == nil || strings.TrimSpace(*channel.Setting) == "" {
		return false, nil
	}
	var setting struct {
		Enabled bool `json:"task_media_proxy_enabled"`
	}
	if err := common.UnmarshalJsonStr(*channel.Setting, &setting); err != nil {
		return false, fmt.Errorf("invalid task channel configuration: %w", err)
	}
	return setting.Enabled, nil
}

// ProjectTaskMediaData 只修改响应副本；原始快照仍供产物代理读取。
func ProjectTaskMediaData(task *model.Task, data json.RawMessage) (json.RawMessage, error) {
	if task == nil || common.GetJsonType(data) != "object" {
		return data, nil
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	var content map[string]json.RawMessage
	if common.GetJsonType(fields["content"]) == "object" {
		if err := common.Unmarshal(fields["content"], &content); err != nil {
			return nil, err
		}
	}
	type mediaField struct {
		fields map[string]json.RawMessage
		name   string
		key    string
	}
	var available []mediaField
	for _, group := range []struct {
		fields map[string]json.RawMessage
		keys   map[string]string
	}{{fields, taskResponseTopLevelMediaFields}, {content, taskResponseMediaFields}} {
		for name, key := range group.keys {
			if common.GetJsonType(group.fields[name]) != "string" {
				continue
			}
			var source string
			if err := common.Unmarshal(group.fields[name], &source); err != nil {
				return nil, err
			}
			if strings.TrimSpace(source) != "" {
				available = append(available, mediaField{group.fields, name, key})
			}
		}
	}
	if len(available) == 0 {
		return data, nil
	}
	enabled, err := taskMediaProxyEnabled(task)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return data, nil
	}
	for _, field := range available {
		proxyURL, err := BuildTaskArtifactContentURL(task.TaskID, field.key)
		if err != nil {
			return nil, err
		}
		field.fields[field.name], err = common.Marshal(proxyURL)
		if err != nil {
			return nil, err
		}
	}
	if content != nil {
		fields["content"], err = common.Marshal(content)
		if err != nil {
			return nil, err
		}
	}
	return common.Marshal(fields)
}

// TaskResponseMediaSource 从原始快照读取响应媒体，不能使用已经投影的地址。
func TaskResponseMediaSource(task *model.Task, artifactKey string) (string, bool, error) {
	if artifactKey == "response-media-result" {
		return strings.TrimSpace(task.GetResultURL()), true, nil
	}
	field, topLevel := "", false
	for name, key := range taskResponseMediaFields {
		if key == artifactKey {
			field = name
			break
		}
	}
	if field == "" {
		for name, key := range taskResponseTopLevelMediaFields {
			if key == artifactKey {
				field, topLevel = name, true
				break
			}
		}
	}
	if field == "" {
		return "", false, nil
	}
	data := task.Data
	if task.Platform == "anyllm" {
		var err error
		data, err = taskDataWithoutMediaProxy(task)
		if err != nil {
			return "", true, err
		}
	}
	var fields map[string]json.RawMessage
	if len(data) > 0 {
		if err := common.Unmarshal(data, &fields); err != nil {
			return "", true, err
		}
	}
	values := fields
	if !topLevel && common.GetJsonType(fields["content"]) == "object" {
		values = nil
		if err := common.Unmarshal(fields["content"], &values); err != nil {
			return "", true, err
		}
	} else if !topLevel {
		values = nil
	}
	var source string
	if common.GetJsonType(values[field]) == "string" {
		if err := common.Unmarshal(values[field], &source); err != nil {
			return "", true, err
		}
	}
	if source == "" && field == "video_url" {
		source = task.GetResultURL()
	}
	return strings.TrimSpace(source), true, nil
}

// PublicTaskResultURL 对通用任务响应中的结果地址使用同一渠道设置。
func PublicTaskResultURL(task *model.Task) (string, error) {
	if task == nil {
		return "", nil
	}
	source := task.GetResultURL()
	if task.Status != model.TaskStatusSuccess ||
		(!strings.HasPrefix(source, "https://") && !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "data:")) {
		return source, nil
	}
	enabled, err := taskMediaProxyEnabled(task)
	if err != nil {
		return "", err
	}
	if !enabled {
		return source, nil
	}
	return BuildTaskArtifactContentURL(task.TaskID, "response-media-result")
}
