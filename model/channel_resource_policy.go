package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var ErrInvalidChannelResourcePolicy = errors.New("invalid channel resource policy")

// ChannelResourcePolicy is independent of Channel so ordinary channel edits
// and copies do not write or inherit its limit. Missing policies and zero mean
// no channel-specific limit; system and token limits remain independent.
type ChannelResourcePolicy struct {
	ChannelID           int   `json:"channel_id" gorm:"primaryKey;autoIncrement:false"`
	MaxRequestBodyBytes int64 `json:"max_request_body_bytes" gorm:"type:bigint;not null"`
}

func (policy ChannelResourcePolicy) Validate() error {
	if policy.ChannelID <= 0 {
		return fmt.Errorf("%w: channel_id must be positive", ErrInvalidChannelResourcePolicy)
	}
	if policy.MaxRequestBodyBytes < 0 {
		return fmt.Errorf("%w: max_request_body_bytes must be non-negative", ErrInvalidChannelResourcePolicy)
	}
	return nil
}

// GetChannelResourcePolicy distinguishes an absent policy from a read failure.
// The caller decides whether to retain a cached policy or bypass size filtering
// on failure; database errors must not masquerade as successful configuration.
func GetChannelResourcePolicy(ctx context.Context, channelID int) (*ChannelResourcePolicy, error) {
	if channelID <= 0 {
		return nil, fmt.Errorf("%w: channel_id must be positive", ErrInvalidChannelResourcePolicy)
	}
	var policy ChannelResourcePolicy
	err := DB.WithContext(ctx).Where("channel_id = ?", channelID).Take(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &policy, nil
}

// GetChannelResourcePolicies loads only the requested candidates, using bounded
// batches instead of one query per channel. It never returns partial results
// after an error. An empty ID list does not load policies for other channels.
func GetChannelResourcePolicies(ctx context.Context, channelIDs []int) (map[int]ChannelResourcePolicy, error) {
	for _, id := range channelIDs {
		if id <= 0 {
			return nil, fmt.Errorf("%w: channel_id must be positive", ErrInvalidChannelResourcePolicy)
		}
	}
	policies := make(map[int]ChannelResourcePolicy, len(channelIDs))
	for start := 0; start < len(channelIDs); start += 200 {
		end := min(start+200, len(channelIDs))
		var batch []ChannelResourcePolicy
		// The relay cache layer reports these errors with rate limiting. Avoid
		// GORM also logging each failed request independently.
		query := DB.WithContext(ctx).Session(&gorm.Session{Logger: DB.Logger.LogMode(logger.Silent)})
		if err := query.Where("channel_id IN ?", channelIDs[start:end]).Find(&batch).Error; err != nil {
			return nil, err
		}
		for _, policy := range batch {
			if err := policy.Validate(); err != nil {
				return nil, err
			}
			policies[policy.ChannelID] = policy
		}
	}
	return policies, nil
}

// SetChannelResourcePolicy requires an existing channel and replaces its size
// limit. Saving zero removes the policy. Authorization belongs to the caller.
func SetChannelResourcePolicy(ctx context.Context, policy ChannelResourcePolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	channelResourcePolicyUpdateMu.Lock()
	defer channelResourcePolicyUpdateMu.Unlock()
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var channel Channel
		if err := lockForUpdate(tx).Select("id").First(&channel, policy.ChannelID).Error; err != nil {
			return err
		}
		if policy.MaxRequestBodyBytes == 0 {
			return tx.Where("channel_id = ?", policy.ChannelID).Delete(&ChannelResourcePolicy{}).Error
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "channel_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"max_request_body_bytes"}),
		}).Create(&policy).Error
	})
	if err != nil {
		return err
	}
	if common.MemoryCacheEnabled {
		channelResourcePolicyCache.Lock()
		if channelResourcePolicyCache.policies == nil {
			channelResourcePolicyCache.policies = make(map[int]ChannelResourcePolicy)
		}
		if policy.MaxRequestBodyBytes == 0 {
			delete(channelResourcePolicyCache.policies, policy.ChannelID)
		} else {
			channelResourcePolicyCache.policies[policy.ChannelID] = policy
		}
		channelResourcePolicyCache.Unlock()
	}
	return nil
}
