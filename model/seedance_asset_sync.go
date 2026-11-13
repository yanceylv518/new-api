package model

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ScheduleSeedanceAssetSyncTargets 与删除流程共用组和素材锁，避免删除后重建同步记录。
func ScheduleSeedanceAssetSyncTargets(ctx context.Context, userID int, targets []SeedanceAssetReplica) error {
	if len(targets) == 0 {
		return nil
	}
	groupIDs := make([]uint, 0, len(targets))
	assetIDs := make([]uint, 0, len(targets))
	seenGroups := make(map[uint]struct{}, len(targets))
	seenAssets := make(map[uint]struct{}, len(targets))
	for _, target := range targets {
		if target.UserID != userID || target.LocalGroupID == 0 || target.LocalAssetID == 0 || target.ChannelID <= 0 || target.AccountFingerprint == "" || target.KeyFingerprint == "" {
			return errors.New("invalid asset synchronization target")
		}
		if _, exists := seenGroups[target.LocalGroupID]; !exists {
			seenGroups[target.LocalGroupID] = struct{}{}
			groupIDs = append(groupIDs, target.LocalGroupID)
		}
		if _, exists := seenAssets[target.LocalAssetID]; !exists {
			seenAssets[target.LocalAssetID] = struct{}{}
			assetIDs = append(assetIDs, target.LocalAssetID)
		}
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var groups []SeedanceAssetGroup
		if err := lockForUpdate(tx).Where("user_id = ? AND id IN ? AND status <> ?", userID, groupIDs, "Deleting").Order("id").Find(&groups).Error; err != nil {
			return err
		}
		if len(groups) != len(groupIDs) {
			return ErrSeedanceAssetUnavailable
		}
		groupsByID := make(map[uint]SeedanceAssetGroup, len(groups))
		for _, group := range groups {
			groupsByID[group.ID] = group
		}
		var assets []SeedanceAsset
		if err := lockForUpdate(tx).Where("user_id = ? AND id IN ? AND status <> ?", userID, assetIDs, "Deleting").Order("id").Find(&assets).Error; err != nil {
			return err
		}
		if len(assets) != len(assetIDs) {
			return ErrSeedanceAssetUnavailable
		}
		assetsByID := make(map[uint]SeedanceAsset, len(assets))
		for _, asset := range assets {
			assetsByID[asset.ID] = asset
		}
		for _, target := range targets {
			group, asset := groupsByID[target.LocalGroupID], assetsByID[target.LocalAssetID]
			if asset.GroupID != group.GroupID || strings.EqualFold(group.GroupType, "LivenessFace") || asset.LocalOnly != nil && *asset.LocalOnly {
				return ErrSeedanceAssetUnavailable
			}
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "local_asset_id"}, {Name: "account_fingerprint"}}, DoNothing: true,
		}).CreateInBatches(&targets, 100).Error; err != nil {
			return err
		}
		for index := range assets {
			if err := refreshReplicaManagedAsset(tx, &assets[index]); err != nil {
				return err
			}
		}
		return nil
	})
}

// AuthorizeSeedanceAssetSyncScope 仅在无活动租约时更新已授权副本的后台范围。
func AuthorizeSeedanceAssetSyncScope(ctx context.Context, replica *SeedanceAssetReplica, scope string, now int64, restart bool) (bool, error) {
	values := map[string]any{"sync_scope": scope, "updated_at": maxSeedanceAssetUpdateTime(replica.UpdatedAt)}
	if restart {
		values["poll_attempts"] = 0
		values["next_poll_at"] = now
		values["last_error"] = ""
	}
	result := DB.WithContext(ctx).Model(&SeedanceAssetReplica{}).
		Where("id = ? AND user_id = ? AND updated_at = ? AND poll_lease_until <= ?", replica.ID, replica.UserID, replica.UpdatedAt, now).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}

// RefreshReplicaManagedSeedanceAsset 将账号副本的就绪结果汇总为素材状态。
func RefreshReplicaManagedSeedanceAsset(ctx context.Context, userID int, assetID uint) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var asset SeedanceAsset
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", assetID, userID, "Deleting").First(&asset).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		return refreshReplicaManagedAsset(tx, &asset)
	})
}

// RetrySeedanceAssetSyncTargets 只重试暂时调用失败的副本，保留上游明确拒绝的审核结果。
func RetrySeedanceAssetSyncTargets(ctx context.Context, userID int, assetID uint, fingerprints []string, now int64) error {
	if len(fingerprints) == 0 {
		return nil
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current SeedanceAsset
		if err := tx.Where("id = ? AND user_id = ?", assetID, userID).First(&current).Error; err != nil {
			return err
		}
		var group SeedanceAssetGroup
		if err := lockForUpdate(tx).Where("user_id = ? AND group_id = ? AND status <> ?", userID, current.GroupID, "Deleting").First(&group).Error; err != nil {
			return err
		}
		var asset SeedanceAsset
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", assetID, userID, "Deleting").First(&asset).Error; err != nil {
			return err
		}
		if asset.LocalOnly == nil || *asset.LocalOnly || strings.EqualFold(group.GroupType, "LivenessFace") {
			return nil
		}
		query := tx.Model(&SeedanceAssetReplica{}).
			Where("user_id = ? AND local_asset_id = ? AND account_fingerprint IN ? AND last_error <> ? AND poll_lease_until <= ?", userID, assetID, fingerprints, "", now)
		if err := query.Where("provision_status = ?", SeedanceAssetReplicaFailed).
			Updates(map[string]any{"status": "Pending", "poll_attempts": 0, "next_poll_at": now, "last_error": ""}).Error; err != nil {
			return err
		}
		if err := tx.Model(&SeedanceAssetReplica{}).
			Where("user_id = ? AND local_asset_id = ? AND account_fingerprint IN ? AND last_error <> ? AND poll_lease_until <= ?", userID, assetID, fingerprints, "", now).
			Where("provision_status = ? AND LOWER(status) IN ?", SeedanceAssetReplicaReady, []string{"failed", "pending", "processing"}).
			Updates(map[string]any{"status": "Processing", "poll_attempts": 0, "next_poll_at": now, "last_error": ""}).Error; err != nil {
			return err
		}
		return refreshReplicaManagedAsset(tx, &asset)
	})
}

func refreshReplicaManagedAsset(tx *gorm.DB, asset *SeedanceAsset) error {
	if !asset.IsReplicaManaged() {
		return nil
	}
	var replicas []SeedanceAssetReplica
	if err := tx.Where("user_id = ? AND local_asset_id = ?", asset.UserID, asset.ID).Find(&replicas).Error; err != nil {
		return err
	}
	status, message := "Failed", "Asset synchronization failed"
	reviewTimedOut := false
	for _, replica := range replicas {
		if replica.UpstreamDeletedAt != 0 {
			continue
		}
		statusKey := strings.ToLower(strings.TrimSpace(replica.Status))
		if replica.LastError == SeedanceAssetReviewTimeoutCode && replica.PollAttempts >= SeedanceAssetSyncMaxAttempts {
			reviewTimedOut = true
		}
		if replica.ProvisionStatus == SeedanceAssetReplicaReady && replica.UpstreamAssetID != "" && (statusKey == "active" || statusKey == "success" || statusKey == "succeeded") {
			status, message = "Active", ""
			break
		}
		if replica.ProvisionStatus == SeedanceAssetReplicaCreating || replica.ProvisionStatus == SeedanceAssetReplicaFailed && replica.NextPollAt > 0 && replica.PollAttempts < SeedanceAssetSyncMaxAttempts ||
			replica.ProvisionStatus == SeedanceAssetReplicaReady && (strings.EqualFold(replica.Status, "Pending") || strings.EqualFold(replica.Status, "Processing")) && (replica.LastError == "" || replica.PollAttempts < SeedanceAssetSyncMaxAttempts) {
			status, message = "Processing", ""
		}
	}
	if status == "Failed" && reviewTimedOut {
		message = SeedanceAssetReviewTimeoutCode
	}
	if asset.Status == status && asset.FailureCode == message {
		return nil
	}
	values := seedanceAssetStatusUpdateValues(map[string]any{"status": status, "failure_code": message})
	values["updated_at"] = maxSeedanceAssetUpdateTime(asset.UpdatedAt)
	return tx.Model(asset).Where("id = ? AND user_id = ? AND status <> ?", asset.ID, asset.UserID, "Deleting").Updates(values).Error
}

// seedanceAssetSyncQuery 按待导入状态和到期时间读取主动同步队列。
func seedanceAssetSyncQuery(now int64) *gorm.DB {
	return DB.Model(&SeedanceAssetReplica{}).
		Where("status = ? AND provision_status IN ?", "Pending", []string{SeedanceAssetReplicaCreating, SeedanceAssetReplicaFailed}).
		Where("next_poll_at > 0 AND next_poll_at <= ? AND poll_lease_until <= ?", now, now).
		Where("local_asset_id IN (?)", DB.Model(&SeedanceAsset{}).Select("id").Where("status <> ?", "Deleting")).
		Where("local_group_id IN (?)", DB.Model(&SeedanceAssetGroup{}).Select("id").Where("status <> ?", "Deleting"))
}

func ListDueSeedanceAssetSync(ctx context.Context, now int64, limit int) ([]*SeedanceAssetReplica, error) {
	return ListDueSeedanceAssetSyncExceptAccounts(ctx, now, limit, nil)
}

// ListDueSeedanceAssetSyncExceptAccounts 防止忙碌账号的积压挡住其他账号。
func ListDueSeedanceAssetSyncExceptAccounts(ctx context.Context, now int64, limit int, excluded []string) ([]*SeedanceAssetReplica, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	var replicas []*SeedanceAssetReplica
	query := seedanceAssetSyncQuery(now).WithContext(ctx)
	if len(excluded) > 0 {
		query = query.Where("account_fingerprint NOT IN ?", excluded)
	}
	err := query.
		Order("next_poll_at ASC").Order("id ASC").Limit(max(1, limit)).Find(&replicas).Error
	return replicas, err
}

func HasDueSeedanceAssetSync(now int64) bool {
	if DB == nil {
		return false
	}
	var marker struct{ ID uint }
	result := seedanceAssetSyncQuery(now).Select("id").Limit(1).Find(&marker)
	return result.Error == nil && result.RowsAffected > 0
}

// ScheduleSeedanceAssetSyncRetry 不覆盖已受理的副本或其他请求正在持有的导入租约。
func ScheduleSeedanceAssetSyncRetry(ctx context.Context, replica *SeedanceAssetReplica, now int64, attempts int, nextPollAt int64, message string) (bool, error) {
	result := DB.WithContext(ctx).Model(&SeedanceAssetReplica{}).
		Where("id = ? AND user_id = ? AND updated_at = ?", replica.ID, replica.UserID, replica.UpdatedAt).
		Where("status = ? AND provision_status IN ?", "Pending", []string{SeedanceAssetReplicaCreating, SeedanceAssetReplicaFailed}).
		Where("poll_lease_until <= ? AND upstream_asset_id = ?", now, "").
		Updates(map[string]any{
			"provision_status": SeedanceAssetReplicaFailed, "poll_attempts": attempts,
			"next_poll_at": nextPollAt, "poll_lease_until": int64(0),
			"last_error": truncateSeedanceAssetMappingError(message),
			"updated_at": maxSeedanceAssetUpdateTime(replica.UpdatedAt),
		})
	return result.RowsAffected == 1, result.Error
}
