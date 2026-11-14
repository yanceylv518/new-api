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
	jobs, err := prepareSeedanceAssetPublication(ctx, []SeedanceAssetCleanupJob{{
		Kind: SeedanceAssetCleanupKindUpstreamAsset, UserID: userID, ChannelID: channelID,
		KeyFingerprint: keyFingerprint, AccountFingerprint: accountFingerprint, UpstreamID: upstreamID,
	}})
	if err != nil {
		return err
	}
	promote := func(attemptCtx context.Context, afterCommitError bool) (bool, error) {
		ready := false
		err := DB.WithContext(attemptCtx).Transaction(func(tx *gorm.DB) error {
			var group SeedanceAssetGroup
			if err := lockForUpdate(tx).Where("user_id = ? AND group_id = ? AND status <> ?", userID, groupID, "Deleting").First(&group).Error; err != nil {
				return err
			}
			var asset SeedanceAsset
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND group_id = ?", assetID, userID, groupID).First(&asset).Error; err != nil {
				return err
			}
			if asset.LocalOnly != nil && !*asset.LocalOnly && asset.Status != "Deleting" && asset.PrimaryUpstreamID == upstreamID && asset.ChannelID == channelID && asset.KeyFingerprint == keyFingerprint && asset.AccountFingerprint == accountFingerprint && asset.PrimarySyncScope == syncScope {
				if err := completeSeedanceAssetPublication(tx, jobs, afterCommitError); err != nil {
					return err
				}
				ready = true
				return nil
			}
			if asset.LocalOnly == nil || !*asset.LocalOnly || asset.Status == "Deleting" || !asset.UpdatedAt.Equal(expectedUpdatedAt) {
				return errors.New("asset changed while optimization was starting")
			}
			if err := tx.Model(&asset).Updates(map[string]any{
				"local_only": false, "primary_upstream_id": upstreamID, "primary_sync_scope": syncScope,
				"channel_id": channelID, "key_fingerprint": keyFingerprint, "account_fingerprint": accountFingerprint,
				"status": "Processing", "status_key": SeedanceAssetStatusIndexKey("Processing"), "pending_status": true,
				"poll_attempts": 0, "poll_lease_until": 0, "next_poll_at": common.GetTimestamp(), "compat_optimize_replica_id": 0,
			}).Error; err != nil {
				return err
			}
			if err := completeSeedanceAssetPublication(tx, jobs, false); err != nil {
				return err
			}
			ready = true
			return nil
		})
		return ready, err
	}
	ready, err := promote(ctx, false)
	if err == nil {
		return nil
	}
	if !ready {
		return seedanceAssetCleanupScheduledError{cause: err}
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if recovered, retryErr := promote(checkCtx, true); retryErr == nil && recovered {
		return nil
	}
	return ErrSeedanceAssetCommitUncertain
}
