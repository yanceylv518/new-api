package model

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// PromoteLocalSeedanceAsset 在持有组锁时将通用素材切换为真实的上游审核任务。
func PromoteLocalSeedanceAsset(ctx context.Context, userID int, assetID uint, groupID string, expectedUpdatedAt time.Time,
	channelID int, keyFingerprint, accountFingerprint, upstreamID, syncScope string) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var group SeedanceAssetGroup
		if err := lockForUpdate(tx).Where("user_id = ? AND group_id = ? AND status <> ?", userID, groupID, "Deleting").First(&group).Error; err != nil {
			return err
		}
		var asset SeedanceAsset
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND group_id = ?", assetID, userID, groupID).First(&asset).Error; err != nil {
			return err
		}
		if asset.LocalOnly == nil || !*asset.LocalOnly || asset.Status == "Deleting" || !asset.UpdatedAt.Equal(expectedUpdatedAt) {
			return errors.New("asset changed while optimization was starting")
		}
		return tx.Model(&asset).Updates(map[string]any{
			"local_only": false, "primary_upstream_id": upstreamID, "primary_sync_scope": syncScope,
			"channel_id": channelID, "key_fingerprint": keyFingerprint, "account_fingerprint": accountFingerprint,
			"status": "Processing", "status_key": SeedanceAssetStatusIndexKey("Processing"), "pending_status": true,
			"poll_attempts": 0, "poll_lease_until": 0, "next_poll_at": common.GetTimestamp(), "compat_optimize_replica_id": 0,
		}).Error
	})
}
