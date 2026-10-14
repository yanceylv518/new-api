package model

import (
	"context"
	"errors"
	"slices"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
)

var ErrChannelRequestBodyTooLarge = errors.New("request body exceeds the limit of available channels")

var filterEvalOrder = []dto.ChannelFilterKind{
	dto.FilterRequestPath,
	dto.FilterTaskPluginIdentity,
	dto.FilterRequestBodySize,
}

// ChannelSatisfiesFilters reports whether ch passes every filter.
// On false, it returns the first violated kind: path, plugin identity, then body size.
func ChannelSatisfiesFilters(ch *Channel, modelName string, filters []dto.ChannelFilter) (bool, dto.ChannelFilterKind) {
	if ch == nil {
		return false, ""
	}
	filters = prepareChannelBodyLimits([]int{ch.Id}, filters)
	for _, kind := range filterEvalOrder {
		for _, filter := range filters {
			if filter.Kind != kind {
				continue
			}
			if !channelMatchesFilter(ch, modelName, filter) {
				return false, kind
			}
		}
	}
	return true, ""
}

// filterCandidateIDs applies filters to a cached candidate id list.
// Caller must hold channelSyncLock (read lock). The input slice is never mutated.
// A missing id is kept for path/body filters (downstream consistency error),
// and dropped for task_plugin_identity, matching its existing behavior.
func filterCandidateIDs(ids []int, modelName string, filters []dto.ChannelFilter) (kept []int, emptiedBy dto.ChannelFilterKind) {
	if len(ids) == 0 {
		return ids, ""
	}
	kept = ids
	filters = prepareChannelBodyLimits(ids, filters)
	for _, kind := range filterEvalOrder {
		kindFilters := filtersByKind(filters, kind)
		if len(kindFilters) == 0 {
			continue
		}
		next := make([]int, 0, len(kept))
		for _, id := range kept {
			channel, exists := channelsIDM[id]
			if candidatePassesKindFilters(channel, exists, modelName, kind, kindFilters) {
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

func candidatePassesKindFilters(ch *Channel, exists bool, modelName string, kind dto.ChannelFilterKind, filters []dto.ChannelFilter) bool {
	if (kind == dto.FilterRequestPath || kind == dto.FilterRequestBodySize) && !exists {
		return true
	}
	if !exists || ch == nil {
		return false
	}
	for _, filter := range filters {
		if !channelMatchesFilter(ch, modelName, filter) {
			return false
		}
	}
	return true
}

func channelMatchesFilter(ch *Channel, modelName string, filter dto.ChannelFilter) bool {
	switch filter.Kind {
	case dto.FilterRequestBodySize:
		limit := filter.ChannelBodyLimits[ch.Id]
		return limit <= 0 || filter.RequestBodyBytes <= limit
	case dto.FilterRequestPath:
		if filter.RequestPath == "" {
			return true
		}
		if ch.Type != constant.ChannelTypeAdvancedCustom {
			return true
		}
		config := ch.GetOtherSettings().AdvancedCustom
		return config != nil && config.SupportsPathForModel(filter.RequestPath, modelName)
	case dto.FilterTaskPluginIdentity:
		if ch.Type == constant.ChannelTypeTaskPlugin {
			return filter.TaskPluginKey != "" && ch.GetSetting().TaskPluginKey == filter.TaskPluginKey
		}
		return filter.TaskPluginKey == "" || slices.Contains(filter.TaskPluginChannelTypes, ch.Type)
	default:
		return true
	}
}

// Load policies once per candidate set, including when memory caching is off.
// A failed optional policy lookup leaves an empty map and preserves routing.
func prepareChannelBodyLimits(ids []int, filters []dto.ChannelFilter) []dto.ChannelFilter {
	var prepared []dto.ChannelFilter
	for i, filter := range filters {
		if filter.Kind != dto.FilterRequestBodySize || filter.ChannelBodyLimits != nil {
			continue
		}
		if prepared == nil {
			prepared = slices.Clone(filters)
		}
		limits := make(map[int]int64)
		for id, policy := range CacheGetChannelResourcePolicies(context.Background(), ids) {
			limits[id] = policy.MaxRequestBodyBytes
		}
		prepared[i].ChannelBodyLimits = limits
	}
	if prepared != nil {
		return prepared
	}
	return filters
}
