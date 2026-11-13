/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

package model

import (
	"context"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	SeedanceAssetValidationStatusCreating  = "Creating"
	SeedanceAssetValidationStatusPending   = "Pending"
	SeedanceAssetValidationStatusSucceeded = "Succeeded"
	SeedanceAssetValidationStatusFailed    = "Failed"
	SeedanceAssetValidationStatusExpired   = "Expired"
	SeedanceAssetValidationStatusCancelled = "Cancelled"
)

var (
	ErrSeedanceAssetValidationSessionNotFound = errors.New("Seedance validation session not found")
	ErrSeedanceAssetValidationSessionClaimed  = errors.New("Seedance validation session is being refreshed")
)

// SeedanceAssetValidationSession 保存真人认证会话的本地状态。
// BytedToken 只保存加密密文；哈希用于 API 协议查询时定位会话，不可反推出令牌。
// CompatCallbackURL 也用于标准接口的客户回调密文。
type SeedanceAssetValidationSession struct {
	ID                   uint      `gorm:"primaryKey" json:"id"`
	UserID               int       `gorm:"index;not null" json:"-"`
	ChannelID            int       `gorm:"index;not null" json:"-"`
	KeyFingerprint       string    `gorm:"size:64;not null" json:"-"`
	AccountFingerprint   string    `gorm:"size:64;not null;index" json:"-"`
	Name                 string    `gorm:"size:64;not null" json:"-"`
	Description          string    `gorm:"type:text" json:"-"`
	Tags                 string    `gorm:"size:512" json:"-"`
	ProjectName          string    `gorm:"size:191" json:"-"`
	CompatCallbackURL    string    `gorm:"type:text" json:"-"`
	CompatModel          string    `gorm:"size:191" json:"-"`
	CallbackStateHash    string    `gorm:"size:64;uniqueIndex" json:"-"`
	BytedTokenHash       *string   `gorm:"size:64;uniqueIndex" json:"-"`
	BytedTokenCiphertext string    `gorm:"type:text" json:"-"`
	LaunchURL            string    `gorm:"size:4096" json:"-"`
	UpstreamGroupID      string    `gorm:"size:128;index" json:"-"`
	LocalGroupID         uint      `gorm:"index" json:"local_group_id,omitempty"`
	Status               string    `gorm:"size:16;not null;index;index:idx_seedance_validation_poll_due,priority:1" json:"status"`
	LastError            string    `gorm:"size:512" json:"last_error,omitempty"`
	PollAttempts         int       `gorm:"not null;default:0" json:"-"`
	NextPollAt           int64     `gorm:"not null;default:0;index:idx_seedance_validation_poll_due,priority:2" json:"-"`
	PollLeaseUntil       int64     `gorm:"not null;default:0;index;index:idx_seedance_validation_poll_due,priority:3" json:"-"`
	ExpiresAt            int64     `gorm:"not null;index" json:"expires_at"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func dueSeedanceAssetValidationSessions(db *gorm.DB, now int64) *gorm.DB {
	return db.Model(&SeedanceAssetValidationSession{}).
		Where("status = ? AND (next_poll_at = 0 OR next_poll_at <= ?)", SeedanceAssetValidationStatusPending, now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now)
}

// HasDueSeedanceAssetValidationSessions 供系统任务调度器判断是否需要同步认证会话。
func HasDueSeedanceAssetValidationSessions(now int64) bool {
	if DB == nil {
		return false
	}
	var marker struct{ ID uint }
	result := dueSeedanceAssetValidationSessions(DB, now).Select("id").Limit(1).Find(&marker)
	return result.Error == nil && result.RowsAffected > 0
}

// ListDueSeedanceAssetValidationSessions 优先处理最早到期的会话，避免积压时新会话长期等待。
func ListDueSeedanceAssetValidationSessions(ctx context.Context, now int64, limit int) ([]*SeedanceAssetValidationSession, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if limit <= 0 {
		limit = 1
	}
	var sessions []*SeedanceAssetValidationSession
	err := dueSeedanceAssetValidationSessions(DB.WithContext(ctx), now).
		Order("next_poll_at asc").Order("id asc").Limit(limit).Find(&sessions).Error
	return sessions, err
}

func GetSeedanceAssetValidationSession(ctx context.Context, userID int, id uint) (*SeedanceAssetValidationSession, error) {
	if DB == nil || userID <= 0 || id == 0 {
		return nil, ErrSeedanceAssetValidationSessionNotFound
	}
	var session SeedanceAssetValidationSession
	if err := DB.WithContext(ctx).Where("user_id = ? AND id = ?", userID, id).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSeedanceAssetValidationSessionNotFound
		}
		return nil, err
	}
	return &session, nil
}

func FindSeedanceAssetValidationSessionByTokenHash(ctx context.Context, userID int, tokenHash string) (*SeedanceAssetValidationSession, error) {
	if DB == nil || userID <= 0 || strings.TrimSpace(tokenHash) == "" {
		return nil, ErrSeedanceAssetValidationSessionNotFound
	}
	var session SeedanceAssetValidationSession
	if err := DB.WithContext(ctx).Where("user_id = ? AND byted_token_hash = ?", userID, tokenHash).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSeedanceAssetValidationSessionNotFound
		}
		return nil, err
	}
	return &session, nil
}

func FindSeedanceAssetValidationSessionByCallbackState(ctx context.Context, id uint, stateHash string) (*SeedanceAssetValidationSession, error) {
	if DB == nil || id == 0 || strings.TrimSpace(stateHash) == "" {
		return nil, ErrSeedanceAssetValidationSessionNotFound
	}
	var session SeedanceAssetValidationSession
	if err := DB.WithContext(ctx).Where("id = ? AND callback_state_hash = ?", id, stateHash).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSeedanceAssetValidationSessionNotFound
		}
		return nil, err
	}
	return &session, nil
}

// ClaimSeedanceAssetValidationSession 用短租约合并并发轮询，避免同一令牌重复查询并重复落组。
func ClaimSeedanceAssetValidationSession(id uint, userID int, now, leaseUntil int64, expectedUpdatedAt ...time.Time) (bool, error) {
	if DB == nil || id == 0 || userID <= 0 || leaseUntil <= now {
		return false, ErrSeedanceAssetValidationSessionNotFound
	}
	query := DB.Model(&SeedanceAssetValidationSession{}).
		Where("id = ? AND user_id = ? AND status = ?", id, userID, SeedanceAssetValidationStatusPending).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now)
	if len(expectedUpdatedAt) > 0 {
		query = query.Where("updated_at = ?", expectedUpdatedAt[0])
	}
	result := query.Updates(map[string]any{"poll_lease_until": leaseUntil})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func UpdateSeedanceAssetValidationSession(id uint, userID int, leaseUntil int64, updates map[string]any) (bool, error) {
	if DB == nil || id == 0 || userID <= 0 || leaseUntil <= 0 || len(updates) == 0 {
		return false, nil
	}
	values := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		values[key] = value
	}
	values["poll_lease_until"] = int64(0)
	result := DB.Model(&SeedanceAssetValidationSession{}).
		Where("id = ? AND user_id = ? AND status = ? AND poll_lease_until = ?", id, userID, SeedanceAssetValidationStatusPending, leaseUntil).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompleteSeedanceAssetValidationSession 在一个事务内发布真人素材组和会话终态。
func CompleteSeedanceAssetValidationSession(ctx context.Context, id uint, userID int, leaseUntil, now int64, upstreamGroupID string) (*SeedanceAssetGroup, error) {
	if DB == nil || id == 0 || userID <= 0 || leaseUntil <= 0 || strings.TrimSpace(upstreamGroupID) == "" {
		return nil, ErrSeedanceAssetValidationSessionNotFound
	}
	// 先保存上游结果，发布事务回滚后仍可跳过一次性认证令牌查询。
	if err := DB.WithContext(ctx).Model(&SeedanceAssetValidationSession{}).
		Where("id = ? AND user_id = ? AND status = ? AND poll_lease_until = ?", id, userID, SeedanceAssetValidationStatusPending, leaseUntil).
		Updates(map[string]any{"upstream_group_id": upstreamGroupID}).Error; err != nil {
		return nil, err
	}
	var group SeedanceAssetGroup
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var session SeedanceAssetValidationSession
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", id, userID).First(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSeedanceAssetValidationSessionNotFound
			}
			return err
		}
		if session.Status == SeedanceAssetValidationStatusSucceeded && session.LocalGroupID != 0 {
			return tx.Where("id = ? AND user_id = ?", session.LocalGroupID, userID).First(&group).Error
		}
		if session.Status != SeedanceAssetValidationStatusPending || session.PollLeaseUntil != leaseUntil {
			return ErrSeedanceAssetValidationSessionClaimed
		}

		group = SeedanceAssetGroup{
			UserID:             userID,
			ChannelID:          session.ChannelID,
			GroupID:            upstreamGroupID,
			Name:               session.Name,
			Description:        session.Description,
			Tags:               session.Tags,
			GroupType:          "LivenessFace",
			BindModel:          session.CompatModel,
			KeyFingerprint:     session.KeyFingerprint,
			AccountFingerprint: session.AccountFingerprint,
			AssetCount:         new(int64),
			Status:             "Active",
		}
		if err := tx.Create(&group).Error; err != nil {
			return err
		}
		result := tx.Model(&SeedanceAssetValidationSession{}).
			Where("id = ? AND user_id = ? AND status = ? AND poll_lease_until = ?", id, userID, SeedanceAssetValidationStatusPending, leaseUntil).
			Updates(map[string]any{
				"upstream_group_id":      upstreamGroupID,
				"local_group_id":         group.ID,
				"status":                 SeedanceAssetValidationStatusSucceeded,
				"last_error":             "",
				"byted_token_ciphertext": "",
				"next_poll_at":           int64(0),
				"poll_lease_until":       int64(0),
				"updated_at":             time.Now(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrSeedanceAssetValidationSessionClaimed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &group, nil
}

func ExpireSeedanceAssetValidationSession(ctx context.Context, id uint, userID int, now int64) (bool, error) {
	if DB == nil || id == 0 || userID <= 0 || now <= 0 {
		return false, nil
	}
	result := DB.WithContext(ctx).Model(&SeedanceAssetValidationSession{}).
		Where("id = ? AND user_id = ? AND status IN ? AND expires_at > 0 AND expires_at <= ?", id, userID,
			[]string{SeedanceAssetValidationStatusCreating, SeedanceAssetValidationStatusPending}, now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Updates(map[string]any{
			"status":                 SeedanceAssetValidationStatusExpired,
			"byted_token_ciphertext": "",
			"next_poll_at":           int64(0),
			"poll_lease_until":       int64(0),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}
