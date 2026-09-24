package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

var ErrSeedanceAssetMappingPending = errors.New("private asset is syncing to the selected upstream account")

const (
	seedanceAssetMappingWorkerSize    = 4
	seedanceAssetMappingMaxConcurrent = 64
)

var seedanceAssetMappingSlots = make(chan struct{}, seedanceAssetMappingMaxConcurrent)

type seedanceAssetOSSStorageCache struct {
	mu       sync.Mutex
	storages map[model.SeedanceAssetStorage]*PrivateAssetOSSStorage
}

func (cache *seedanceAssetOSSStorageCache) upstreamURL(ctx context.Context, asset *model.SeedanceAsset) (string, error) {
	cache.mu.Lock()
	storage, exists := cache.storages[asset.Storage]
	if !exists {
		var err error
		storage, err = privateAssetOSSStorageFactory(asset.Storage)
		if err != nil {
			cache.mu.Unlock()
			return "", err
		}
		cache.storages[asset.Storage] = storage
	}
	cache.mu.Unlock()
	return storage.PrivateAssetUpstreamURL(ctx, asset.ObjectKey)
}

type seedanceAssetMappingResult struct {
	upstreamID string
	needsPoll  bool
	err        error
}

// MapSeedanceAssetsToAccount 确保当前用户引用的素材在选中账号中都有可用的上游 ID。
func MapSeedanceAssetsToAccount(ctx context.Context, userID int, client *SeedanceAssetClient, assetIDs []string) (map[string]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if client == nil || client.AccountFingerprint == "" {
		return nil, errors.New("selected Seedance account is unavailable")
	}
	assets, err := model.FindSeedanceAssetsForUser(ctx, userID, assetIDs)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]string, 0, len(assets))
	seenGroups := make(map[string]struct{}, len(assets))
	for index := range assets {
		if _, exists := seenGroups[assets[index].GroupID]; !exists {
			groupIDs = append(groupIDs, assets[index].GroupID)
			seenGroups[assets[index].GroupID] = struct{}{}
		}
	}
	var groups []model.SeedanceAssetGroup
	if err := model.DB.WithContext(ctx).
		Where("user_id = ? AND group_id IN ? AND status <> ?", userID, groupIDs, "Deleting").
		Find(&groups).Error; err != nil {
		return nil, err
	}
	if len(groups) != len(groupIDs) {
		return nil, model.ErrSeedanceAssetUnavailable
	}
	localGroupIDs := make([]uint, 0, len(groups))
	groupsByUpstreamID := make(map[string]*model.SeedanceAssetGroup, len(groups))
	for index := range groups {
		localGroupIDs = append(localGroupIDs, groups[index].ID)
		groupsByUpstreamID[groups[index].GroupID] = &groups[index]
	}
	groupReplicas, err := model.FindSeedanceAssetGroupReplicasForAccount(ctx, userID, localGroupIDs, client.AccountFingerprint)
	if err != nil {
		return nil, err
	}
	localAssetIDs := make([]uint, 0, len(assets))
	for index := range assets {
		localAssetIDs = append(localAssetIDs, assets[index].ID)
	}
	assetReplicas, err := model.FindSeedanceAssetReplicasForAccount(ctx, userID, localAssetIDs, client.AccountFingerprint)
	if err != nil {
		return nil, err
	}
	ossStorages := &seedanceAssetOSSStorageCache{storages: make(map[model.SeedanceAssetStorage]*PrivateAssetOSSStorage)}
	pollNeeded := false
	defer func() {
		if !pollNeeded {
			return
		}
		if _, _, err := EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetPoll, nil); err != nil {
			common.SysError("failed to enqueue Seedance replica polling: " + err.Error())
		}
	}()
	upstreamIDs := make(map[string]string, len(assets))
	upstreamGroups := make(map[string]string, len(groups))
	for index := range assets {
		asset := &assets[index]
		if _, exists := upstreamGroups[asset.GroupID]; exists {
			continue
		}
		group := groupsByUpstreamID[asset.GroupID]
		select {
		case seedanceAssetMappingSlots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		upstreamGroupID, ensureErr := ensureSeedanceAssetGroupReplica(ctx, client, group, groupReplicas[group.ID])
		<-seedanceAssetMappingSlots
		if ensureErr != nil {
			return nil, ensureErr
		}
		upstreamGroups[asset.GroupID] = upstreamGroupID
	}
	results := make([]seedanceAssetMappingResult, len(assets))
	if len(assets) == 1 {
		select {
		case seedanceAssetMappingSlots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		asset := &assets[0]
		group := groupsByUpstreamID[asset.GroupID]
		upstreamID, needsPoll, ensureErr := ensureSeedanceAssetReplica(ctx, client, group, asset, upstreamGroups[asset.GroupID], assetReplicas[asset.ID], ossStorages)
		<-seedanceAssetMappingSlots
		results[0] = seedanceAssetMappingResult{upstreamID: upstreamID, needsPoll: needsPoll, err: ensureErr}
	} else {
		jobs := make(chan int, len(assets))
		for index := range assets {
			jobs <- index
		}
		close(jobs)
		workerCount := min(seedanceAssetMappingWorkerSize, len(assets))
		var workers sync.WaitGroup
		for range workerCount {
			workers.Go(func() {
				for index := range jobs {
					select {
					case seedanceAssetMappingSlots <- struct{}{}:
					case <-ctx.Done():
						results[index].err = ctx.Err()
						continue
					}
					asset := &assets[index]
					group := groupsByUpstreamID[asset.GroupID]
					upstreamID, needsPoll, ensureErr := ensureSeedanceAssetReplica(ctx, client, group, asset, upstreamGroups[asset.GroupID], assetReplicas[asset.ID], ossStorages)
					<-seedanceAssetMappingSlots
					results[index] = seedanceAssetMappingResult{upstreamID: upstreamID, needsPoll: needsPoll, err: ensureErr}
				}
			})
		}
		workers.Wait()
	}
	for _, result := range results {
		pollNeeded = pollNeeded || result.needsPoll
	}
	for index, result := range results {
		if result.err != nil {
			return nil, result.err
		}
		upstreamIDs[assets[index].AssetID] = result.upstreamID
	}
	return upstreamIDs, nil
}

func ensureSeedanceAssetGroupReplica(ctx context.Context, client *SeedanceAssetClient, group *model.SeedanceAssetGroup, snapshot *model.SeedanceAssetGroupReplica) (string, error) {
	originFingerprint := group.AccountFingerprint
	if originFingerprint == "" {
		originChannel, err := model.GetChannelById(group.ChannelID, true)
		if err == nil {
			originFingerprint = SeedanceAssetAccountFingerprint(originChannel, group.KeyFingerprint)
		}
		if originFingerprint == "" && group.ChannelID == client.ChannelID && group.KeyFingerprint == client.KeyFingerprint {
			originFingerprint = client.AccountFingerprint
		}
	}
	if originFingerprint != "" && originFingerprint == client.AccountFingerprint ||
		originFingerprint == "" && group.ChannelID == client.ChannelID && group.KeyFingerprint == client.KeyFingerprint {
		if group.ChannelID != client.ChannelID || group.KeyFingerprint != client.KeyFingerprint || group.AccountFingerprint != client.AccountFingerprint {
			result := model.DB.WithContext(ctx).Model(&model.SeedanceAssetGroup{}).
				Where("id = ? AND user_id = ? AND status <> ?", group.ID, group.UserID, "Deleting").
				Updates(map[string]any{
					"channel_id":          client.ChannelID,
					"key_fingerprint":     client.KeyFingerprint,
					"account_fingerprint": client.AccountFingerprint,
				})
			if result.Error != nil {
				return "", result.Error
			}
			if result.RowsAffected == 0 {
				return "", model.ErrSeedanceAssetUnavailable
			}
			group.ChannelID = client.ChannelID
			group.KeyFingerprint = client.KeyFingerprint
			group.AccountFingerprint = client.AccountFingerprint
		}
		return group.GroupID, nil
	}

	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetPollLease.Seconds())
	replica, claimed, err := model.ClaimSeedanceAssetGroupReplicaWithSnapshot(ctx, group.UserID, group.ID, client.ChannelID, client.AccountFingerprint, client.KeyFingerprint, group.Name, now, leaseUntil, snapshot)
	if err != nil {
		return "", err
	}
	if !claimed {
		if replica == nil {
			return "", ErrSeedanceAssetMappingPending
		}
		if replica.Status == model.SeedanceAssetReplicaReady && replica.UpstreamGroupID != "" {
			if replica.GroupName != group.Name {
				if err := client.UpdateSeedanceAssetGroup(ctx, replica.UpstreamGroupID, group.Name, &struct{}{}); err != nil {
					return "", fmt.Errorf("failed to update the selected account's asset group: %w", err)
				}
			}
			if replica.GroupName != group.Name || replica.ChannelID != client.ChannelID || replica.KeyFingerprint != client.KeyFingerprint {
				result := model.DB.WithContext(ctx).Model(replica).
					Where("id = ? AND status = ?", replica.ID, model.SeedanceAssetReplicaReady).
					Updates(map[string]any{
						"group_name":      group.Name,
						"channel_id":      client.ChannelID,
						"key_fingerprint": client.KeyFingerprint,
					})
				if result.Error != nil {
					return "", result.Error
				}
				if result.RowsAffected == 0 {
					return "", ErrSeedanceAssetMappingPending
				}
				replica.GroupName = group.Name
				replica.ChannelID = client.ChannelID
				replica.KeyFingerprint = client.KeyFingerprint
			}
			return replica.UpstreamGroupID, nil
		}
		return "", ErrSeedanceAssetMappingPending
	}

	var response seedanceAssetResponse
	if err := client.CreateSeedanceAssetGroup(ctx, group.Name, group.GroupType, &response); err != nil {
		_ = model.FailSeedanceAssetGroupReplica(replica.ID, leaseUntil, err.Error())
		return "", err
	}
	if response.Result.ID == "" {
		_ = model.FailSeedanceAssetGroupReplica(replica.ID, leaseUntil, "upstream returned an empty group ID")
		return "", errors.New("upstream returned an empty asset group ID")
	}
	completed, err := model.CompleteSeedanceAssetGroupReplica(ctx, replica.ID, leaseUntil, response.Result.ID, group.Name)
	if err != nil || !completed {
		if rollbackErr := RollbackSeedanceAssetGroup(ctx, client, response.Result.ID); rollbackErr != nil {
			common.SysError("failed to roll back an unrecorded Seedance asset group: " + rollbackErr.Error())
		}
		if err != nil {
			return "", err
		}
		return "", errors.New("asset group was removed while its upstream copy was being created")
	}
	return response.Result.ID, nil
}

func ensureSeedanceAssetReplica(ctx context.Context, client *SeedanceAssetClient, group *model.SeedanceAssetGroup, asset *model.SeedanceAsset, upstreamGroupID string, snapshot *model.SeedanceAssetReplica, ossStorages *seedanceAssetOSSStorageCache) (string, bool, error) {
	originFingerprint := asset.AccountFingerprint
	if originFingerprint == "" {
		originFingerprint = group.AccountFingerprint
	}
	if originFingerprint == "" {
		originChannel, err := model.GetChannelById(asset.ChannelID, true)
		if err == nil {
			originFingerprint = SeedanceAssetAccountFingerprint(originChannel, asset.KeyFingerprint)
		}
		if originFingerprint == "" && asset.ChannelID == client.ChannelID && asset.KeyFingerprint == client.KeyFingerprint {
			originFingerprint = client.AccountFingerprint
		}
	}
	if originFingerprint != "" && originFingerprint == client.AccountFingerprint ||
		originFingerprint == "" && asset.ChannelID == client.ChannelID && asset.KeyFingerprint == client.KeyFingerprint {
		if asset.ChannelID != client.ChannelID || asset.KeyFingerprint != client.KeyFingerprint || asset.AccountFingerprint != client.AccountFingerprint {
			result := model.DB.WithContext(ctx).Model(&model.SeedanceAsset{}).
				Where("id = ? AND user_id = ? AND status <> ?", asset.ID, asset.UserID, "Deleting").
				Updates(map[string]any{
					"channel_id":          client.ChannelID,
					"key_fingerprint":     client.KeyFingerprint,
					"account_fingerprint": client.AccountFingerprint,
				})
			if result.Error != nil {
				return "", false, result.Error
			}
			if result.RowsAffected == 0 {
				return "", false, model.ErrSeedanceAssetUnavailable
			}
			asset.ChannelID = client.ChannelID
			asset.KeyFingerprint = client.KeyFingerprint
			asset.AccountFingerprint = client.AccountFingerprint
		}
		status := normalizeSeedanceAssetStatus(asset.Status)
		if status != "active" {
			status, err := refreshSeedancePrimaryAsset(ctx, client, asset)
			if err != nil {
				return "", true, fmt.Errorf("%w: %v", ErrSeedanceAssetMappingPending, err)
			}
			if status != "active" {
				if status == "failed" {
					return "", false, errors.New("private asset was rejected by the selected upstream account")
				}
				return "", true, ErrSeedanceAssetMappingPending
			}
		}
		return asset.AssetID, false, nil
	}

	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetPollLease.Seconds())
	replica, claimed, err := model.ClaimSeedanceAssetReplicaWithSnapshot(ctx, asset, group.ID, client.ChannelID, client.AccountFingerprint, client.KeyFingerprint, now, leaseUntil, snapshot)
	if err != nil {
		return "", false, err
	}
	if !claimed {
		if replica == nil {
			return "", true, ErrSeedanceAssetMappingPending
		}
		if replica.ProvisionStatus != model.SeedanceAssetReplicaReady || replica.UpstreamAssetID == "" {
			return "", true, ErrSeedanceAssetMappingPending
		}
		status := normalizeSeedanceAssetStatus(replica.Status)
		if status != "active" {
			refreshed, refreshErr := refreshSeedanceAssetReplica(ctx, client, replica)
			if refreshErr != nil {
				return "", true, fmt.Errorf("%w: %v", ErrSeedanceAssetMappingPending, refreshErr)
			}
			status = normalizeSeedanceAssetStatus(refreshed.Status)
		}
		if status == "failed" {
			return "", false, errors.New("private asset was rejected by the selected upstream account")
		}
		if status != "active" {
			return "", true, ErrSeedanceAssetMappingPending
		}
		return replica.UpstreamAssetID, false, nil
	}

	sourceURL := strings.TrimSpace(asset.SourceURL)
	if strings.TrimSpace(asset.ObjectKey) != "" {
		sourceURL, err = ossStorages.upstreamURL(ctx, asset)
		if err != nil {
			_ = model.FailSeedanceAssetReplica(replica.ID, leaseUntil, err.Error())
			return "", false, err
		}
	}
	if err := ValidateSeedanceAssetSourceURL(sourceURL); err != nil {
		_ = model.FailSeedanceAssetReplica(replica.ID, leaseUntil, err.Error())
		return "", false, errors.New("private asset source is unavailable for account synchronization")
	}
	upstreamAssetID, err := client.CreateSeedanceAsset(ctx, upstreamGroupID, sourceURL, asset.AssetType, asset.Name)
	if err != nil {
		_ = model.FailSeedanceAssetReplica(replica.ID, leaseUntil, err.Error())
		return "", false, err
	}
	completed, err := model.CompleteSeedanceAssetReplica(ctx, replica.ID, leaseUntil, upstreamGroupID, upstreamAssetID, common.GetTimestamp())
	if err != nil || !completed {
		if rollbackErr := RollbackSeedanceAsset(ctx, client, upstreamAssetID, ""); rollbackErr != nil {
			common.SysError("failed to roll back an unrecorded Seedance asset replica: " + rollbackErr.Error())
		}
		if err != nil {
			return "", false, err
		}
		return "", false, errors.New("asset was removed while its upstream copy was being created")
	}
	var saved model.SeedanceAssetReplica
	if err := model.DB.WithContext(ctx).First(&saved, replica.ID).Error; err != nil {
		return "", true, err
	}
	refreshed, refreshErr := refreshSeedanceAssetReplica(ctx, client, &saved)
	if refreshErr != nil {
		return "", true, fmt.Errorf("%w: %v", ErrSeedanceAssetMappingPending, refreshErr)
	}
	if normalizeSeedanceAssetStatus(refreshed.Status) == "failed" {
		return "", false, errors.New("private asset was rejected by the selected upstream account")
	}
	if normalizeSeedanceAssetStatus(refreshed.Status) != "active" {
		return "", true, ErrSeedanceAssetMappingPending
	}
	return refreshed.UpstreamAssetID, false, nil
}

func refreshSeedancePrimaryAsset(ctx context.Context, client *SeedanceAssetClient, asset *model.SeedanceAsset) (string, error) {
	var current model.SeedanceAsset
	if err := model.DB.WithContext(ctx).First(&current, asset.ID).Error; err != nil {
		return "", err
	}
	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetPollLease.Seconds())
	claimed, err := model.ClaimSeedanceAssetForPolling(current.ID, current.UpdatedAt, now, leaseUntil)
	if err != nil {
		return "", err
	}
	if !claimed {
		if err := model.DB.WithContext(ctx).First(&current, asset.ID).Error; err != nil {
			return "", err
		}
		return normalizeSeedanceAssetStatus(current.Status), nil
	}
	status, previewURL, err := client.GetSeedanceAsset(ctx, current.AssetID)
	if err != nil {
		_, scheduleErr := model.UpdateSeedanceAssetPollState(current.ID, leaseUntil, map[string]any{"poll_lease_until": int64(0)})
		return "", errors.Join(err, scheduleErr)
	}
	attempts, nextPollAt := SeedanceAssetPollSchedule(status, current.PollAttempts, common.GetTimestamp())
	updates := map[string]any{"status": status, "poll_attempts": attempts, "next_poll_at": nextPollAt, "poll_lease_until": int64(0)}
	if previewURL != "" {
		updates["preview_url"] = previewURL
	}
	if _, err := model.UpdateSeedanceAssetPollState(current.ID, leaseUntil, updates); err != nil {
		return "", err
	}
	if err := model.DB.WithContext(ctx).First(&current, asset.ID).Error; err != nil {
		return "", err
	}
	return normalizeSeedanceAssetStatus(current.Status), nil
}

func refreshSeedanceAssetReplica(ctx context.Context, client *SeedanceAssetClient, replica *model.SeedanceAssetReplica) (*model.SeedanceAssetReplica, error) {
	var current model.SeedanceAssetReplica
	if err := model.DB.WithContext(ctx).First(&current, replica.ID).Error; err != nil {
		return nil, err
	}
	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetPollLease.Seconds())
	claimed, err := model.ClaimSeedanceAssetReplicaForPolling(current.ID, current.UpdatedAt, now, leaseUntil)
	if err != nil {
		return nil, err
	}
	if !claimed {
		if err := model.DB.WithContext(ctx).First(&current, replica.ID).Error; err != nil {
			return nil, err
		}
		return &current, nil
	}
	status, previewURL, err := client.GetSeedanceAsset(ctx, current.UpstreamAssetID)
	if err != nil {
		attempts, nextPollAt := SeedanceAssetPollSchedule(current.Status, current.PollAttempts, common.GetTimestamp())
		_, scheduleErr := model.UpdateSeedanceAssetReplicaPollState(current.ID, leaseUntil, map[string]any{
			"poll_attempts": attempts, "next_poll_at": nextPollAt, "poll_lease_until": int64(0), "last_error": truncateSeedanceAssetMappingError(err.Error()),
		})
		if scheduleErr != nil {
			return nil, errors.Join(err, scheduleErr)
		}
		return nil, err
	}
	attempts, nextPollAt := SeedanceAssetPollSchedule(status, current.PollAttempts, common.GetTimestamp())
	updates := map[string]any{
		"status": status, "poll_attempts": attempts, "next_poll_at": nextPollAt,
		"poll_lease_until": int64(0), "last_error": "",
	}
	if previewURL != "" {
		updates["preview_url"] = previewURL
	}
	if _, err := model.UpdateSeedanceAssetReplicaPollState(current.ID, leaseUntil, updates); err != nil {
		return nil, err
	}
	if err := model.DB.WithContext(ctx).First(&current, replica.ID).Error; err != nil {
		return nil, err
	}
	return &current, nil
}

func truncateSeedanceAssetMappingError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		return message[:512]
	}
	return message
}
