package model

import (
	"context"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Serialize policy refreshes and committed writes so a slow refresh cannot
// overwrite a newer local save/delete. Readers use only the short cache lock;
// no database I/O is performed while holding it or channelSyncLock.
var channelResourcePolicyUpdateMu sync.Mutex
var channelResourcePolicyCache = struct {
	sync.RWMutex
	policies map[int]ChannelResourcePolicy
}{}
var channelResourcePolicyLogMu sync.Mutex
var channelResourcePolicyLastErrorLog time.Time

// RefreshChannelResourcePolicyCache publishes only a complete, valid snapshot.
// On failure, existing entries remain available. With no cached entry, relay
// callers deliberately bypass this optional channel-size constraint.
func RefreshChannelResourcePolicyCache(ctx context.Context) error {
	channelResourcePolicyUpdateMu.Lock()
	defer channelResourcePolicyUpdateMu.Unlock()

	var rows []ChannelResourcePolicy
	query := DB.WithContext(ctx).Session(&gorm.Session{Logger: DB.Logger.LogMode(logger.Silent)})
	if err := query.Find(&rows).Error; err != nil {
		logChannelResourcePolicyError(err)
		return err
	}
	policies := make(map[int]ChannelResourcePolicy, len(rows))
	for _, policy := range rows {
		if err := policy.Validate(); err != nil {
			logChannelResourcePolicyError(err)
			return err
		}
		policies[policy.ChannelID] = policy
	}
	channelResourcePolicyCache.Lock()
	channelResourcePolicyCache.policies = policies
	channelResourcePolicyCache.Unlock()
	return nil
}

// CacheGetChannelResourcePolicies is for relay selection, not admin reads.
// Direct database failures fail open when the memory cache is disabled. Admin
// handlers must use GetChannelResourcePolicy so read errors remain visible.
func CacheGetChannelResourcePolicies(ctx context.Context, channelIDs []int) map[int]ChannelResourcePolicy {
	if !common.MemoryCacheEnabled {
		policies, err := GetChannelResourcePolicies(ctx, channelIDs)
		if err != nil {
			logChannelResourcePolicyError(err)
			return nil
		}
		return policies
	}
	channelResourcePolicyCache.RLock()
	defer channelResourcePolicyCache.RUnlock()
	policies := make(map[int]ChannelResourcePolicy, len(channelIDs))
	for _, id := range channelIDs {
		if policy, ok := channelResourcePolicyCache.policies[id]; ok {
			policies[id] = policy
		}
	}
	return policies
}

// Called after a successful commit while channelResourcePolicyUpdateMu is held.
func removeChannelResourcePoliciesFromCache(channelIDs []int) {
	channelResourcePolicyCache.Lock()
	defer channelResourcePolicyCache.Unlock()
	for _, id := range channelIDs {
		delete(channelResourcePolicyCache.policies, id)
	}
}

func logChannelResourcePolicyError(err error) {
	channelResourcePolicyLogMu.Lock()
	now := time.Now()
	shouldLog := now.Sub(channelResourcePolicyLastErrorLog) >= time.Minute
	if shouldLog {
		channelResourcePolicyLastErrorLog = now
	}
	channelResourcePolicyLogMu.Unlock()
	if shouldLog {
		common.SysError("channel resource policy read failed; retaining cached limits where available, otherwise bypassing channel size filtering: " + err.Error())
	}
}
