package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
)

var ErrChannelConfigurationConflict = errors.New("channel settings changed; reload the channel before saving")

func decodeChannelConfiguration(raw string, target any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	if !strings.HasPrefix(raw, "{") {
		return errors.New("channel settings must be a JSON object")
	}
	return common.UnmarshalJsonStr(raw, target)
}

// 运行时只检查配置格式，保留代理等设置已有的兼容处理。
func (channel *Channel) ValidateConfigurationEncoding() error {
	if channel.Setting != nil {
		if err := decodeChannelConfiguration(*channel.Setting, &dto.ChannelSettings{}); err != nil {
			return err
		}
	}
	return decodeChannelConfiguration(channel.OtherSettings, &dto.ChannelOtherSettings{})
}

// 配置补丁只修改明确提交的字段；null 表示删除，未提交的字段保持原样。
type ChannelConfigurationPatch struct {
	SettingPatch  map[string]json.RawMessage `json:"setting_patch,omitempty"`
	SettingsPatch map[string]json.RawMessage `json:"settings_patch,omitempty"`
	SettingBase   *string                    `json:"setting_base,omitempty"`
	SettingsBase  *string                    `json:"settings_base,omitempty"`
}

func (p ChannelConfigurationPatch) CheckBase(channel *Channel) error {
	if p.SettingPatch != nil {
		if p.SettingBase == nil {
			return errors.New("setting_base is required with setting_patch")
		}
		current := ""
		if channel.Setting != nil {
			current = *channel.Setting
		}
		if current != *p.SettingBase {
			return ErrChannelConfigurationConflict
		}
	}
	if p.SettingsPatch != nil {
		if p.SettingsBase == nil {
			return errors.New("settings_base is required with settings_patch")
		}
		if channel.OtherSettings != *p.SettingsBase {
			return ErrChannelConfigurationConflict
		}
	}
	return nil
}

func (p ChannelConfigurationPatch) Apply(channel, original *Channel) error {
	if err := p.CheckBase(original); err != nil {
		return err
	}
	if p.SettingPatch != nil {
		updated, err := MergeChannelConfiguration(*p.SettingBase, p.SettingPatch)
		if err != nil {
			return fmt.Errorf("invalid setting patch: %w", err)
		}
		channel.Setting = &updated
	}
	if p.SettingsPatch != nil {
		updated, err := MergeChannelConfiguration(*p.SettingsBase, p.SettingsPatch)
		if err != nil {
			return fmt.Errorf("invalid settings patch: %w", err)
		}
		channel.OtherSettings = updated
	}
	return nil
}

func channelConfigurationObject(raw string) (map[string]json.RawMessage, error) {
	if value := strings.TrimSpace(raw); value == "" || value == "null" {
		return make(map[string]json.RawMessage), nil
	}
	var value map[string]json.RawMessage
	if err := common.UnmarshalJsonStr(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("channel settings must be a JSON object")
	}
	return value, nil
}

// MergeChannelConfiguration 保留未知字段及其原始数值精度，不经浮点数重建配置。
func MergeChannelConfiguration(raw string, patch map[string]json.RawMessage) (string, error) {
	value, err := channelConfigurationObject(raw)
	if err != nil {
		return "", err
	}
	if len(patch) == 0 {
		return raw, nil
	}
	if err := mergeChannelConfigurationObject(value, patch, 0); err != nil {
		return "", err
	}
	encoded, err := common.Marshal(value)
	return string(encoded), err
}

func mergeChannelConfigurationObject(value, patch map[string]json.RawMessage, depth int) error {
	if depth > 32 {
		return errors.New("channel settings exceed the nesting limit")
	}
	for key, next := range patch {
		if bytes.Equal(bytes.TrimSpace(next), []byte("null")) {
			delete(value, key)
			continue
		}
		if bytes.HasPrefix(bytes.TrimSpace(next), []byte("{")) {
			var childPatch map[string]json.RawMessage
			if err := common.Unmarshal(next, &childPatch); err != nil {
				return err
			}
			child := make(map[string]json.RawMessage)
			if bytes.HasPrefix(bytes.TrimSpace(value[key]), []byte("{")) {
				if err := common.Unmarshal(value[key], &child); err != nil {
					return err
				}
			}
			if err := mergeChannelConfigurationObject(child, childPatch, depth+1); err != nil {
				return err
			}
			encoded, err := common.Marshal(child)
			if err != nil {
				return err
			}
			value[key] = encoded
		} else {
			value[key] = next
		}
	}
	return nil
}

func updateTypedChannelConfiguration(raw string, updated any) (string, error) {
	if _, err := channelConfigurationObject(raw); err != nil {
		return "", err
	}
	previous := reflect.New(reflect.TypeOf(updated)).Interface()
	if strings.TrimSpace(raw) != "" {
		if err := common.UnmarshalJsonStr(raw, previous); err != nil {
			return "", err
		}
	}
	before, err := common.Marshal(previous)
	if err != nil {
		return "", err
	}
	after, err := common.Marshal(updated)
	if err != nil {
		return "", err
	}
	patch, err := channelConfigurationDelta(before, after)
	if err != nil {
		return "", err
	}
	return MergeChannelConfiguration(raw, patch)
}

func channelConfigurationDelta(before, after []byte) (map[string]json.RawMessage, error) {
	var oldValue, newValue map[string]json.RawMessage
	if err := common.Unmarshal(before, &oldValue); err != nil {
		return nil, err
	}
	if err := common.Unmarshal(after, &newValue); err != nil {
		return nil, err
	}
	patch := make(map[string]json.RawMessage)
	for key, old := range oldValue {
		next, exists := newValue[key]
		if !exists {
			patch[key] = json.RawMessage("null")
		} else if !bytes.Equal(old, next) {
			if bytes.HasPrefix(old, []byte("{")) && bytes.HasPrefix(next, []byte("{")) {
				child, err := channelConfigurationDelta(old, next)
				if err != nil {
					return nil, err
				}
				encoded, err := common.Marshal(child)
				if err != nil {
					return nil, err
				}
				patch[key] = encoded
			} else {
				patch[key] = next
			}
		}
	}
	for key, next := range newValue {
		if _, exists := oldValue[key]; !exists {
			patch[key] = next
		}
	}
	return patch, nil
}

func (channel *Channel) UpdateWithConfigurationPatch(patch ChannelConfigurationPatch) error {
	if patch.SettingPatch == nil && patch.SettingsPatch == nil {
		return channel.Update()
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var current Channel
		if err := lockForUpdate(tx).Select("id", "setting", "settings").First(&current, channel.Id).Error; err != nil {
			return err
		}
		if err := patch.CheckBase(&current); err != nil {
			return err
		}
		return channel.update(tx)
	})
}
