package model

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// SeedanceAssetGroup 保存用户在火山方舟 Seedance 素材库中的本地授权映射。
type SeedanceAssetGroup struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    int       `gorm:"index;not null;uniqueIndex:idx_seedance_asset_group_user_name_key,priority:1" json:"user_id"`
	ChannelID int       `gorm:"index;not null" json:"channel_id"`
	GroupID   string    `gorm:"size:128;not null" json:"group_id"`
	Name      string    `gorm:"size:64;not null" json:"name"`
	NameKey   *string   `gorm:"size:64;uniqueIndex:idx_seedance_asset_group_user_name_key,priority:2" json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NormalizeSeedanceAssetGroupName 生成跨数据库一致的素材组名称键。
func NormalizeSeedanceAssetGroupName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// BeforeSave 在数据库边界统一维护名称键，使并发写入也受同一用户唯一约束保护。
func (group *SeedanceAssetGroup) BeforeSave(_ *gorm.DB) error {
	group.Name = strings.TrimSpace(group.Name)
	nameKey := NormalizeSeedanceAssetGroupName(group.Name)
	group.NameKey = &nameKey
	return nil
}

// IsSeedanceAssetGroupNameDuplicated 检查同一用户下是否已有相同名称的素材组，排除指定记录本身。
func IsSeedanceAssetGroupNameDuplicated(id uint, userID int, name string) (bool, error) {
	if DB == nil {
		return false, errors.New("database is not initialized")
	}
	trimmedName := strings.TrimSpace(name)
	if trimmedName == "" {
		return false, nil
	}
	nameKey := NormalizeSeedanceAssetGroupName(trimmedName)
	query := DB.Model(&SeedanceAssetGroup{}).
		Where("user_id = ? AND (name_key = ? OR LOWER(name) = LOWER(?))", userID, nameKey, trimmedName)
	if id > 0 {
		query = query.Where("id <> ?", id)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// SeedanceAsset 保存上游素材状态及其所属用户，所有读取必须带 UserID 条件。
type SeedanceAsset struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	UserID         int       `gorm:"index;not null" json:"user_id"`
	ChannelID      int       `gorm:"index;not null" json:"channel_id"`
	GroupID        string    `gorm:"index;size:128;not null" json:"group_id"`
	AssetID        string    `gorm:"uniqueIndex;size:128;not null" json:"asset_id"`
	Name           string    `gorm:"size:64;not null" json:"name"`
	AssetType      string    `gorm:"size:16;not null" json:"asset_type"`
	SourceURL      string    `gorm:"size:2048;not null" json:"-"`
	ObjectKey      string    `gorm:"index;size:640" json:"-"`
	PreviewURL     string    `gorm:"size:4096" json:"preview_url,omitempty"`
	Status         string    `gorm:"index;size:32;not null" json:"status"`
	FailureCode    string    `gorm:"size:128" json:"failure_code,omitempty"`
	PollAttempts   int       `gorm:"not null;default:0" json:"-"`
	NextPollAt     int64     `gorm:"index;not null;default:0" json:"-"`
	PollLeaseUntil int64     `gorm:"index;not null;default:0" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ListDueSeedanceAssets 返回到达轮询时间且没有有效租约的处理中素材。
func ListDueSeedanceAssets(now int64, limit int) ([]*SeedanceAsset, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if limit <= 0 {
		limit = 1
	}
	var assets []*SeedanceAsset
	err := DB.Where("LOWER(status) IN ?", []string{"processing", "pending"}).
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Order("id asc").Limit(limit).Find(&assets).Error
	return assets, err
}

// HasDueSeedanceAssets 判断是否存在需要后台同步的素材，供系统任务调度器决定是否创建任务。
func HasDueSeedanceAssets(now int64) bool {
	if DB == nil {
		return false
	}
	var count int64
	err := DB.Model(&SeedanceAsset{}).
		Where("LOWER(status) IN ?", []string{"processing", "pending"}).
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Count(&count).Error
	return err == nil && count > 0
}

// ClaimSeedanceAssetForPolling 使用读取时的更新时间抢占素材，避免过期列表结果重复发起上游请求。
func ClaimSeedanceAssetForPolling(id uint, expectedUpdatedAt time.Time, now, leaseUntil int64) (bool, error) {
	if leaseUntil <= now {
		return false, nil
	}
	result := DB.Model(&SeedanceAsset{}).
		Where("id = ? AND updated_at = ?", id, expectedUpdatedAt).
		Where("LOWER(status) IN ?", []string{"processing", "pending"}).
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Updates(map[string]any{
			"poll_lease_until": leaseUntil,
			"updated_at":       time.Now(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// UpdateSeedanceAssetIfUnchanged 仅在素材仍保持读取版本时写入，防止手动刷新覆盖后台任务的新状态。
func UpdateSeedanceAssetIfUnchanged(id uint, expectedUpdatedAt time.Time, updates map[string]any) (bool, error) {
	if len(updates) == 0 {
		return false, nil
	}
	values := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		values[key] = value
	}
	values["updated_at"] = time.Now()
	result := DB.Model(&SeedanceAsset{}).
		Where("id = ? AND updated_at = ?", id, expectedUpdatedAt).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// UpdateSeedanceAssetPollState 只允许当前轮询租约持有者提交状态，旧请求即使晚返回也不能回写。
func UpdateSeedanceAssetPollState(id uint, leaseUntil int64, updates map[string]any) (bool, error) {
	if leaseUntil <= 0 || len(updates) == 0 {
		return false, nil
	}
	values := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		values[key] = value
	}
	values["updated_at"] = time.Now()
	result := DB.Model(&SeedanceAsset{}).
		Where("id = ? AND poll_lease_until = ?", id, leaseUntil).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}
