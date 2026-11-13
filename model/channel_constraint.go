package model

import (
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
)

var filterEvalOrder = []dto.ChannelFilterKind{
	dto.FilterRequestPath,
	dto.FilterTaskPluginIdentity,
	dto.FilterVideoRouting,
	dto.FilterResponsesWebSocket,
	dto.FilterSeedanceAssetAccount,
	dto.FilterSeedanceAssetReady,
}

// ChannelSatisfiesFilters reports whether ch passes every filter.
// 失败时按固定顺序返回首个未满足的约束，供调用方区分协议、身份和素材账号错误。
func ChannelSatisfiesFilters(ch *Channel, modelName string, filters []dto.ChannelFilter) (bool, dto.ChannelFilterKind) {
	return channelSatisfiesFiltersForAbility(ch, modelName, "", "", filters)
}

func ChannelSatisfiesFiltersForGroup(ch *Channel, modelName, group string, filters []dto.ChannelFilter) (bool, dto.ChannelFilterKind) {
	return channelSatisfiesFiltersForAbility(ch, modelName, "", group, filters)
}

func channelSatisfiesFiltersForAbility(ch *Channel, modelName, abilityModel, group string, filters []dto.ChannelFilter) (bool, dto.ChannelFilterKind) {
	if ch == nil {
		return false, ""
	}
	for _, kind := range filterEvalOrder {
		for _, filter := range filters {
			if filter.Kind != kind {
				continue
			}
			if !channelMatchesFilter(ch, modelName, abilityModel, group, filter) {
				return false, kind
			}
		}
	}
	return true, ""
}

// filterCandidateIDs applies filters to a cached candidate id list.
// Caller must hold channelSyncLock (read lock). The input slice is never mutated.
// A missing id in channelsIDM is kept for request_path (downstream consistency
// error) and dropped for task_plugin_identity, matching the previous filters.
func filterCandidateIDs(ids []int, modelName string, filters []dto.ChannelFilter) (kept []int, emptiedBy dto.ChannelFilterKind) {
	return filterCandidateIDsForAbility(ids, modelName, "", "", filters)
}

func filterCandidateIDsForAbility(ids []int, modelName, abilityModel, group string, filters []dto.ChannelFilter) (kept []int, emptiedBy dto.ChannelFilterKind) {
	if len(ids) == 0 {
		return ids, ""
	}
	kept = ids
	for _, kind := range filterEvalOrder {
		kindFilters := filtersByKind(filters, kind)
		if len(kindFilters) == 0 {
			continue
		}
		next := make([]int, 0, len(kept))
		for _, id := range kept {
			channel, exists := channelsIDM[id]
			if candidatePassesKindFilters(channel, exists, modelName, abilityModel, group, kind, kindFilters) {
				next = append(next, id)
			}
		}
		if len(kept) > 0 && len(next) == 0 {
			return next, kind
		}
		kept = next
	}
	return kept, ""
}

func filtersByKind(filters []dto.ChannelFilter, kind dto.ChannelFilterKind) []dto.ChannelFilter {
	var matched []dto.ChannelFilter
	for _, filter := range filters {
		if filter.Kind == kind {
			matched = append(matched, filter)
		}
	}
	return matched
}

func candidatePassesKindFilters(ch *Channel, exists bool, modelName, abilityModel, group string, kind dto.ChannelFilterKind, filters []dto.ChannelFilter) bool {
	if kind == dto.FilterRequestPath && !exists {
		return true
	}
	if !exists || ch == nil {
		return false
	}
	for _, filter := range filters {
		if !channelMatchesFilter(ch, modelName, abilityModel, group, filter) {
			return false
		}
	}
	return true
}

func channelMatchesFilter(ch *Channel, modelName, abilityModel, group string, filter dto.ChannelFilter) bool {
	switch filter.Kind {
	case dto.FilterVideoRouting:
		return ch.matchesVideoRouting(modelName, filter)
	case dto.FilterRequestPath:
		if filter.RequestPath == "" {
			return true
		}
		if !constant.IsAdvancedCustomChannel(ch.Type) {
			return true
		}
		config := ch.GetOtherSettings().AdvancedCustom
		return config != nil && config.SupportsPathForModel(filter.RequestPath, modelName)
	case dto.FilterTaskPluginIdentity:
		if len(filter.TaskPluginChannelIDs) > 0 {
			if _, allowed := filter.TaskPluginChannelIDs[ch.Id]; !allowed {
				return false
			}
		}
		if filter.TaskPluginKey == "" {
			return ch.Type != constant.ChannelTypeTaskPlugin
		}
		if ch.Type == constant.ChannelTypeTaskPlugin || ch.Type == constant.ChannelTypeNewAPI {
			// A New API channel serves every plugin it is extended with; the
			// pinned plugin or any shared-model candidate may execute there.
			setting := ch.GetSetting()
			return setting.BindsTaskPlugin(filter.TaskPluginKey) || slices.ContainsFunc(filter.TaskPluginKeys, setting.BindsTaskPlugin)
		}
		return slices.Contains(filter.TaskPluginChannelTypes, ch.Type)
	case dto.FilterResponsesWebSocket:
		if !ch.GetSetting().ResponsesWebSocketEnabled {
			return false
		}
		switch ch.Type {
		case constant.ChannelTypeOpenAI, constant.ChannelTypeCodex, constant.ChannelTypeSub2API, constant.ChannelTypeNewAPI:
			return true
		case constant.ChannelTypeAdvancedCustom:
			// The session forwards native Responses events without protocol
			// conversion, so only a converter-free /v1/responses route qualifies.
			route, ok := ch.GetOtherSettings().AdvancedCustom.MatchPathForModel("/v1/responses", modelName)
			return ok && route.IsNative()
		default:
			return false
		}
	case dto.FilterSeedanceAssetAccount:
		if ch.Status != common.ChannelStatusEnabled || filter.SeedanceAssetAccountFingerprint == "" || filter.SeedanceAssetKeyFingerprint == "" {
			return false
		}
		if SeedanceAssetSharedAccountMatches(ch, filter.SeedanceAssetAccountFingerprint) {
			if !ch.ChannelInfo.IsMultiKey {
				return strings.TrimSpace(ch.Key) != ""
			}
			for index, key := range ch.GetKeys() {
				if strings.TrimSpace(key) == "" {
					continue
				}
				if status, exists := ch.ChannelInfo.MultiKeyStatusList[index]; !exists || status == common.ChannelStatusEnabled {
					return true
				}
			}
			return false
		}
		if !ch.ChannelInfo.IsMultiKey {
			return ch.Key != "" && fmt.Sprintf("%x", common.Sha256Raw([]byte(ch.Key))) == filter.SeedanceAssetKeyFingerprint &&
				SeedanceAssetAccountFingerprintMatches(ch, filter.SeedanceAssetKeyFingerprint, filter.SeedanceAssetAccountFingerprint)
		}
		for index, key := range ch.GetKeys() {
			if strings.TrimSpace(key) == "" || fmt.Sprintf("%x", common.Sha256Raw([]byte(key))) != filter.SeedanceAssetKeyFingerprint {
				continue
			}
			if status, exists := ch.ChannelInfo.MultiKeyStatusList[index]; exists && status != common.ChannelStatusEnabled {
				continue
			}
			return SeedanceAssetAccountFingerprintMatches(ch, filter.SeedanceAssetKeyFingerprint, filter.SeedanceAssetAccountFingerprint)
		}
		return false
	case dto.FilterSeedanceAssetReady:
		if ch.Status != common.ChannelStatusEnabled {
			return false
		}
		for candidate, allowed := range filter.SeedanceAssetReadyKeys[ch.Id] {
			if abilityModel != "" && candidate != abilityModel {
				continue
			}
			if group != "" {
				if _, exists := filter.SeedanceAssetReadyGroups[ch.Id][candidate][group]; !exists {
					continue
				}
			}
			for index, key := range ch.GetKeys() {
				if strings.TrimSpace(key) == "" {
					continue
				}
				if status, exists := ch.ChannelInfo.MultiKeyStatusList[index]; ch.ChannelInfo.IsMultiKey && exists && status != common.ChannelStatusEnabled {
					continue
				}
				fingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte(key)))
				if _, ready := allowed[fingerprint]; ready {
					return true
				}
			}
		}
		return false
	default:
		return true
	}
}
