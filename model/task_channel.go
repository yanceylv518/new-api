package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
)

// GetTransportHeaderOverride 与地址和密钥共用提交快照，空快照也不能继承渠道的新凭据。
func (task *Task) GetTransportHeaderOverride(channel *Channel) (map[string]any, error) {
	if !task.PrivateData.TransportSaved {
		return channel.GetHeaderOverride(), nil
	}
	var headers map[string]any
	if len(task.PrivateData.HeaderOverride) > 0 {
		if err := common.Unmarshal(task.PrivateData.HeaderOverride, &headers); err != nil {
			return nil, err
		}
	}
	return headers, nil
}

// GetResourceTaskChannel 只使用仍启用的原凭据，避免多密钥渠道随机切换资源所属账号。
func GetResourceTaskChannel(task *Task, current *Channel) (*Channel, error) {
	if task.ChannelId != current.Id || current.Status != common.ChannelStatusEnabled || task.PrivateData.Key == "" {
		return nil, errors.New("resource account binding is unavailable")
	}
	enabled := false
	for index, key := range current.GetKeys() {
		status, explicit := current.ChannelInfo.MultiKeyStatusList[index]
		if key == task.PrivateData.Key && (!current.ChannelInfo.IsMultiKey || !explicit || status == common.ChannelStatusEnabled) {
			enabled = true
			break
		}
	}
	if !enabled {
		return nil, errors.New("resource credential is disabled or unavailable")
	}
	channel := *current
	channel.Key = task.PrivateData.Key
	channel.ChannelInfo.IsMultiKey = false
	if task.PrivateData.TransportSaved {
		if task.PrivateData.BaseURL == "" {
			return nil, errors.New("resource transport snapshot is unavailable")
		}
		channel.BaseURL = &task.PrivateData.BaseURL
		channel.Type = task.PrivateData.ChannelType
		setting := channel.GetSetting()
		setting.Proxy = task.PrivateData.Proxy
		channel.SetSetting(setting)
		headers, err := task.GetTransportHeaderOverride(current)
		if err != nil {
			return nil, err
		}
		encoded, err := common.Marshal(headers)
		if err != nil {
			return nil, err
		}
		raw := string(encoded)
		channel.HeaderOverride = &raw
	}
	return &channel, nil
}

// GetTaskChannel 仅在渠道已删除时使用提交快照，数据库故障仍向上传递。
func GetTaskChannel(task *Task) (*Channel, error) {
	channel, err := CacheGetChannel(task.ChannelId)
	if err == nil || !errors.Is(err, gorm.ErrRecordNotFound) {
		return channel, err
	}
	return TaskChannelFromSnapshot(task)
}

func TaskChannelFromSnapshot(task *Task) (*Channel, error) {
	if task == nil || !task.PrivateData.TransportSaved || task.PrivateData.Key == "" || task.PrivateData.BaseURL == "" {
		return nil, gorm.ErrRecordNotFound
	}
	baseURL := task.PrivateData.BaseURL
	encoded, err := common.Marshal(dto.ChannelSettings{Proxy: task.PrivateData.Proxy})
	if err != nil {
		return nil, err
	}
	setting := string(encoded)
	channel := &Channel{Id: task.ChannelId, Type: task.PrivateData.ChannelType, BaseURL: &baseURL, Key: task.PrivateData.Key, Setting: &setting}
	if len(task.PrivateData.HeaderOverride) > 0 {
		headerOverride := string(task.PrivateData.HeaderOverride)
		channel.HeaderOverride = &headerOverride
	}
	return channel, nil
}
