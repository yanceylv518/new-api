package model

import (
	"context"
	"errors"
	"fmt"
	"math"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrInvalidTokenResourcePolicy = errors.New("invalid token resource policy")

// TokenResourcePolicy stores administrator-only overrides separately from Token
// so existing token APIs and cache formats do not expose or overwrite them.
// A nil limit inherits the default; zero disables only this per-token limit.
type TokenResourcePolicy struct {
	TokenID               int    `json:"token_id" gorm:"primaryKey;autoIncrement:false"`
	MaxRequestBodyBytes   *int64 `json:"max_request_body_bytes" gorm:"type:bigint"`
	MaxConcurrentRequests *int   `json:"max_concurrent_requests" gorm:"type:int"`
}

// TokenResourceLimits contains resolved limits. Zero means no per-token limit;
// the existing system-wide request body ceiling still applies.
type TokenResourceLimits struct {
	MaxRequestBodyBytes   int64
	MaxConcurrentRequests int
}

func (policy TokenResourcePolicy) Validate() error {
	if policy.TokenID <= 0 {
		return fmt.Errorf("%w: token_id must be positive", ErrInvalidTokenResourcePolicy)
	}
	if policy.MaxRequestBodyBytes != nil && *policy.MaxRequestBodyBytes < 0 {
		return fmt.Errorf("%w: max_request_body_bytes must be non-negative", ErrInvalidTokenResourcePolicy)
	}
	if policy.MaxConcurrentRequests != nil && (*policy.MaxConcurrentRequests < 0 || int64(*policy.MaxConcurrentRequests) > math.MaxInt32) {
		return fmt.Errorf("%w: max_concurrent_requests must be between 0 and %d", ErrInvalidTokenResourcePolicy, math.MaxInt32)
	}
	return nil
}

// Resolve applies each override independently. A missing policy inherits both
// defaults. Callers apply the feature switch and system ceiling separately.
func (policy *TokenResourcePolicy) Resolve(defaults TokenResourceLimits) TokenResourceLimits {
	if policy == nil {
		return defaults
	}
	if policy.MaxRequestBodyBytes != nil {
		defaults.MaxRequestBodyBytes = *policy.MaxRequestBodyBytes
	}
	if policy.MaxConcurrentRequests != nil {
		defaults.MaxConcurrentRequests = *policy.MaxConcurrentRequests
	}
	return defaults
}

// GetTokenResourcePolicy returns nil only when no override exists. Database
// failures, including an unprepared schema, must not silently disable limits.
func GetTokenResourcePolicy(ctx context.Context, tokenID int) (*TokenResourcePolicy, error) {
	if tokenID <= 0 {
		return nil, fmt.Errorf("%w: token_id must be positive", ErrInvalidTokenResourcePolicy)
	}
	var policy TokenResourcePolicy
	err := DB.WithContext(ctx).Where("token_id = ?", tokenID).Take(&policy).Error
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

// SetTokenResourcePolicy replaces both overrides, including explicit zero and
// nil values. Passing two nil limits removes the override. Authorization is the
// caller's responsibility; writes require an existing, non-deleted token.
func SetTokenResourcePolicy(ctx context.Context, policy TokenResourcePolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).Select("id").First(&token, policy.TokenID).Error; err != nil {
			return err
		}
		if policy.MaxRequestBodyBytes == nil && policy.MaxConcurrentRequests == nil {
			return tx.Where("token_id = ?", policy.TokenID).Delete(&TokenResourcePolicy{}).Error
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "token_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"max_request_body_bytes", "max_concurrent_requests"}),
		}).Create(&policy).Error
	})
}

// TokenResourcePolicyItem deliberately excludes API credentials and quota data.
type TokenResourcePolicyItem struct {
	TokenResourcePolicy
	Name string `json:"name"`
}

func GetUserTokenResourcePolicies(ctx context.Context, userID, offset, limit int) ([]TokenResourcePolicyItem, error) {
	var tokens []Token
	if err := DB.WithContext(ctx).Select("id", "name").Where("user_id = ?", userID).Order("id DESC").Offset(offset).Limit(limit).Find(&tokens).Error; err != nil {
		return nil, err
	}
	items := make([]TokenResourcePolicyItem, 0, len(tokens))
	ids := make([]int, 0, len(tokens))
	for _, token := range tokens {
		ids = append(ids, token.Id)
	}
	var policies []TokenResourcePolicy
	if len(ids) > 0 {
		if err := DB.WithContext(ctx).Where("token_id IN ?", ids).Find(&policies).Error; err != nil {
			return nil, err
		}
	}
	byID := make(map[int]TokenResourcePolicy, len(policies))
	for _, policy := range policies {
		if err := policy.Validate(); err != nil {
			return nil, err
		}
		byID[policy.TokenID] = policy
	}
	for _, token := range tokens {
		policy := byID[token.Id]
		policy.TokenID = token.Id
		items = append(items, TokenResourcePolicyItem{TokenResourcePolicy: policy, Name: token.Name})
	}
	return items, nil
}
