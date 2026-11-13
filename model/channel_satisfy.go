package model

import (
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// MatchingChannelAbilityForGroup 返回与就绪副本一致的实际模型能力。
func MatchingChannelAbilityForGroup(channel *Channel, group, modelName string, filters []dto.ChannelFilter) string {
	if channel == nil || group == "" || modelName == "" {
		return ""
	}
	models := []string{modelName}
	if normalized := ratio_setting.RoutingMatchModelName(modelName); normalized != "" && normalized != modelName {
		models = append(models, normalized)
	}
	for _, candidate := range models {
		var enabled bool
		if common.MemoryCacheEnabled {
			channelSyncLock.RLock()
			enabled = isChannelIDInList(group2model2channels[group][candidate], channel.Id)
			channelSyncLock.RUnlock()
		} else {
			var count int64
			err := DB.Model(&Ability{}).Where(commonGroupCol+" = ? and model = ? and channel_id = ? and enabled = ?", group, candidate, channel.Id, true).Count(&count).Error
			enabled = err == nil && count > 0
		}
		if enabled {
			if ok, _ := channelSatisfiesFiltersForAbility(channel, modelName, candidate, group, filters); ok {
				return candidate
			}
		}
	}
	return ""
}

func IsChannelEnabledForGroupModel(group string, modelName string, channelID int) bool {
	if group == "" || modelName == "" || channelID <= 0 {
		return false
	}
	if !common.MemoryCacheEnabled {
		return isChannelEnabledForGroupModelDB(group, modelName, channelID)
	}

	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	if group2model2channels == nil {
		return false
	}

	if isChannelIDInList(group2model2channels[group][modelName], channelID) {
		return true
	}
	normalized := ratio_setting.RoutingMatchModelName(modelName)
	if normalized != "" && normalized != modelName {
		return isChannelIDInList(group2model2channels[group][normalized], channelID)
	}
	return false
}

func IsChannelEnabledForAnyGroupModel(groups []string, modelName string, channelID int) bool {
	if len(groups) == 0 {
		return false
	}
	for _, g := range groups {
		if IsChannelEnabledForGroupModel(g, modelName, channelID) {
			return true
		}
	}
	return false
}

func isChannelEnabledForGroupModelDB(group string, modelName string, channelID int) bool {
	var count int64
	err := DB.Model(&Ability{}).
		Where(commonGroupCol+" = ? and model = ? and channel_id = ? and enabled = ?", group, modelName, channelID, true).
		Count(&count).Error
	if err == nil && count > 0 {
		return true
	}
	normalized := ratio_setting.RoutingMatchModelName(modelName)
	if normalized == "" || normalized == modelName {
		return false
	}
	count = 0
	err = DB.Model(&Ability{}).
		Where(commonGroupCol+" = ? and model = ? and channel_id = ? and enabled = ?", group, normalized, channelID, true).
		Count(&count).Error
	return err == nil && count > 0
}

func isChannelIDInList(list []int, channelID int) bool {
	return slices.Contains(list, channelID)
}
