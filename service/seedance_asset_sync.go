package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

type seedanceAssetSyncJob struct {
	replica *model.SeedanceAssetReplica
}

var seedanceAssetSyncAccountGates = struct {
	sync.Mutex
	items map[string]struct{}
}{items: make(map[string]struct{})}

// acquireSeedanceAssetSyncAccount 避免同账号并发导入，忙碌账号不占用其他账号的工作槽。
func acquireSeedanceAssetSyncAccount(fingerprint string) func() {
	seedanceAssetSyncAccountGates.Lock()
	if _, busy := seedanceAssetSyncAccountGates.items[fingerprint]; busy {
		seedanceAssetSyncAccountGates.Unlock()
		return nil
	}
	seedanceAssetSyncAccountGates.items[fingerprint] = struct{}{}
	seedanceAssetSyncAccountGates.Unlock()
	return func() {
		seedanceAssetSyncAccountGates.Lock()
		delete(seedanceAssetSyncAccountGates.items, fingerprint)
		seedanceAssetSyncAccountGates.Unlock()
	}
}

var errSeedanceAssetSyncOwnerUnavailable = errors.New("asset owner is unavailable")
var errSeedanceAssetSyncTargetUnavailable = errors.New("asset synchronization target is not currently available to this user")

type seedanceAssetSyncScope struct {
	Version       int      `json:"version"`
	Groups        []string `json:"groups"`
	Models        []string `json:"models"`
	PinnedChannel int      `json:"pinned_channel"`
}

// EncodeSeedanceAssetSyncScope 固定创建任务时的令牌范围，后台仍需重新校验用户权限。
func EncodeSeedanceAssetSyncScope(groups, models []string, pinnedChannel int) (string, error) {
	groups = slices.Clone(groups)
	models = slices.Clone(models)
	slices.Sort(groups)
	slices.Sort(models)
	groups = slices.Compact(groups)
	models = slices.Compact(models)
	value, err := common.Marshal(seedanceAssetSyncScope{Version: 1, Groups: groups, Models: models, PinnedChannel: pinnedChannel})
	return string(value), err
}

const seedanceAssetSyncRetryJitterSeconds = 5
const seedanceAssetSyncRecoveryDelay = 15 * time.Minute
const seedanceAssetSyncDiscoveryInterval = 2 * time.Second
const seedanceAssetSyncBlockedDelay = 15 * time.Second

// CreateSeedanceAssetWithSynchronization 将上传记录和去重后的同步目标一并提交。
func CreateSeedanceAssetWithSynchronization(ctx context.Context, asset *model.SeedanceAsset, requestedGroups, allowedModels []string, pinnedChannels ...int) error {
	var group model.SeedanceAssetGroup
	if err := model.DB.WithContext(ctx).Where("user_id = ? AND group_id = ?", asset.UserID, asset.GroupID).First(&group).Error; err != nil {
		return err
	}
	if strings.EqualFold(group.GroupType, "LivenessFace") || asset.LocalOnly != nil && *asset.LocalOnly {
		return model.CreateSeedanceAssetInGroup(ctx, asset)
	}
	channels, err := FindSeedanceAssetChannels()
	if err != nil {
		return err
	}
	pinnedChannel := 0
	if len(pinnedChannels) > 0 {
		pinnedChannel = pinnedChannels[0]
	}
	if pinnedChannel > 0 {
		channels = slices.DeleteFunc(channels, func(channel *model.Channel) bool { return channel.Id != pinnedChannel })
	}
	clients, err := seedanceAssetSyncClientsForUser(ctx, asset.UserID, channels, requestedGroups, allowedModels)
	if err != nil {
		return err
	}
	scope, err := EncodeSeedanceAssetSyncScope(requestedGroups, allowedModels, pinnedChannel)
	if err != nil {
		return err
	}
	targets := make([]model.SeedanceAssetSyncTarget, 0, len(clients))
	priorities := make(map[int]int64, len(channels))
	for _, channel := range channels {
		priorities[channel.Id] = channel.GetPriority()
	}
	for _, client := range clients {
		if !asset.IsReplicaManaged() && (client.matchesAccountFingerprint(asset.AccountFingerprint) ||
			client.ChannelID == asset.ChannelID && client.KeyFingerprint == asset.KeyFingerprint) {
			continue
		}
		targets = append(targets, model.SeedanceAssetSyncTarget{
			ChannelID: client.ChannelID, AccountFingerprint: client.AccountFingerprint, KeyFingerprint: client.KeyFingerprint, SyncScope: scope,
		})
	}
	slices.SortFunc(targets, func(a, b model.SeedanceAssetSyncTarget) int {
		if priorities[a.ChannelID] != priorities[b.ChannelID] {
			if priorities[a.ChannelID] > priorities[b.ChannelID] {
				return -1
			}
			return 1
		}
		if a.ChannelID != b.ChannelID {
			return a.ChannelID - b.ChannelID
		}
		return strings.Compare(a.KeyFingerprint, b.KeyFingerprint)
	})
	if asset.IsReplicaManaged() && len(targets) == 0 {
		return ErrSeedanceAssetChannelUnavailable
	}
	return model.CreateSeedanceAssetInGroup(ctx, asset, targets...)
}

func seedanceAssetSyncGroupsForUser(ctx context.Context, userID int, requestedGroups []string) ([]string, error) {
	var user model.User
	if err := model.DB.WithContext(ctx).Select([]string{"id", "group", "status"}).Where("id = ?", userID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errSeedanceAssetSyncOwnerUnavailable
		}
		return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
	}
	if user.Status != common.UserStatusEnabled {
		return nil, errSeedanceAssetSyncOwnerUnavailable
	}
	groups := make([]string, 0)
	usable := GetUserUsableGroups(user.Group)
	if requestedGroups == nil {
		for group := range usable {
			if group != "" && group != "auto" && ratio_setting.ContainsGroupRatio(group) {
				groups = append(groups, group)
			}
		}
	} else {
		for _, group := range requestedGroups {
			if _, allowed := usable[group]; allowed && group != "" && group != "auto" && ratio_setting.ContainsGroupRatio(group) && !slices.Contains(groups, group) {
				groups = append(groups, group)
			}
		}
	}
	return groups, nil
}

func seedanceAssetModelAllowed(name string, allowedModels []string) bool {
	return allowedModels == nil || slices.Contains(allowedModels, name) ||
		slices.Contains(allowedModels, ratio_setting.FormatMatchingModelName(name)) ||
		slices.Contains(allowedModels, ratio_setting.RoutingMatchModelName(name))
}

func seedanceAssetModelName(channel *model.Channel, name string) bool {
	isSeedance := func(value string) bool {
		value = strings.ToLower(value)
		return strings.HasPrefix(value, "doubao-seedance-") || strings.HasPrefix(value, "maas-doubao-seedance-") || strings.HasPrefix(value, "seedance-")
	}
	if isSeedance(name) {
		return true
	}
	mapping := make(map[string]string)
	if raw := channel.GetModelMapping(); raw == "" || common.UnmarshalJsonStr(raw, &mapping) != nil {
		return false
	}
	seen := make(map[string]bool)
	for !seen[name] {
		seen[name] = true
		next := mapping[name]
		if next == "" || next == name {
			return false
		}
		if isSeedance(next) {
			return true
		}
		name = next
	}
	return false
}

// seedanceAssetSyncClientsForUser 按模型、渠道和密钥创建独立同步目标。
func seedanceAssetSyncClientsForUser(ctx context.Context, userID int, channels []*model.Channel, requestedGroups, allowedModels []string, includeOtherModels ...bool) (map[string]*SeedanceAssetClient, error) {
	groups, err := seedanceAssetSyncGroupsForUser(ctx, userID, requestedGroups)
	if err != nil {
		return nil, err
	}
	clients := make(map[string]*SeedanceAssetClient)
	if len(groups) == 0 || len(channels) == 0 {
		return clients, nil
	}
	ids := make([]int, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.Id)
	}
	var abilities []model.Ability
	abilityQuery := model.DB.WithContext(ctx).Model(&model.Ability{}).
		Where(map[string]any{"group": groups, "enabled": true}).Where("channel_id IN ?", ids)
	if err := abilityQuery.Find(&abilities).Error; err != nil {
		return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
	}
	byID := make(map[int]*model.Channel, len(channels))
	order := make(map[int]int, len(channels))
	for index, channel := range channels {
		byID[channel.Id] = channel
		order[channel.Id] = index
	}
	slices.SortStableFunc(abilities, func(a, b model.Ability) int {
		if order[a.ChannelId] != order[b.ChannelId] {
			return order[a.ChannelId] - order[b.ChannelId]
		}
		return strings.Compare(a.Model, b.Model)
	})
	for _, ability := range abilities {
		channel := byID[ability.ChannelId]
		if channel == nil || !seedanceAssetModelAllowed(ability.Model, allowedModels) ||
			!seedanceAssetModelName(channel, ability.Model) && (len(includeOtherModels) == 0 || !includeOtherModels[0]) {
			continue
		}
		keys := []string{channel.Key}
		if channel.ChannelInfo.IsMultiKey {
			keys = channel.GetKeys()
		}
		for index, key := range keys {
			if strings.TrimSpace(key) == "" {
				continue
			}
			if status, exists := channel.ChannelInfo.MultiKeyStatusList[index]; channel.ChannelInfo.IsMultiKey && exists && status != common.ChannelStatusEnabled {
				continue
			}
			fingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte(key)))
			account := model.SeedanceAssetModelScopeFingerprint(channel, ability.Model, fingerprint)
			if account == "" {
				continue
			}
			if _, exists := clients[account]; exists {
				continue
			}
			client, err := NewSeedanceAssetClient(channel, fingerprint)
			if err != nil {
				continue
			}
			clients[account] = client.ForSeedanceModel(channel, ability.Model)
		}
	}
	return clients, nil
}

// SeedanceAssetModelScopes 返回当前凭证可用模型及其独立同步目标。
func SeedanceAssetModelScopes(ctx context.Context, userID int, requestedGroups, allowedModels []string, pinnedChannel int) (map[string][]string, error) {
	groups, err := seedanceAssetSyncGroupsForUser(ctx, userID, requestedGroups)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]string)
	if len(groups) == 0 {
		return result, nil
	}
	var abilities []model.Ability
	query := model.DB.WithContext(ctx).Where(map[string]any{"group": groups, "enabled": true})
	if pinnedChannel > 0 {
		query = query.Where("channel_id = ?", pinnedChannel)
	}
	if err := query.Find(&abilities).Error; err != nil {
		return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
	}
	ids := make([]int, 0, len(abilities))
	for _, ability := range abilities {
		if !slices.Contains(ids, ability.ChannelId) {
			ids = append(ids, ability.ChannelId)
		}
	}
	if len(ids) == 0 {
		return result, nil
	}
	var channels []*model.Channel
	if err := model.DB.WithContext(ctx).Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
	}
	byID := make(map[int]*model.Channel, len(channels))
	for _, channel := range channels {
		byID[channel.Id] = channel
	}
	for _, ability := range abilities {
		channel := byID[ability.ChannelId]
		if channel != nil && seedanceAssetModelAllowed(ability.Model, allowedModels) && seedanceAssetModelName(channel, ability.Model) {
			if _, exists := result[ability.Model]; !exists {
				result[ability.Model] = nil
			}
		}
	}
	assetChannels, err := FindSeedanceAssetChannels()
	if errors.Is(err, ErrSeedanceAssetChannelUnavailable) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if pinnedChannel > 0 {
		assetChannels = slices.DeleteFunc(assetChannels, func(channel *model.Channel) bool { return channel.Id != pinnedChannel })
	}
	available := make(map[int]*model.Channel, len(assetChannels))
	for _, channel := range assetChannels {
		available[channel.Id] = channel
	}
	for _, ability := range abilities {
		channel := available[ability.ChannelId]
		if channel == nil || !seedanceAssetModelAllowed(ability.Model, allowedModels) || !seedanceAssetModelName(channel, ability.Model) {
			continue
		}
		for index, key := range channel.GetKeys() {
			if strings.TrimSpace(key) == "" {
				continue
			}
			if status, exists := channel.ChannelInfo.MultiKeyStatusList[index]; channel.ChannelInfo.IsMultiKey && exists && status != common.ChannelStatusEnabled {
				continue
			}
			keyFingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte(key)))
			if _, err := NewSeedanceAssetClient(channel, keyFingerprint); err != nil {
				continue
			}
			fingerprint := model.SeedanceAssetModelScopeFingerprint(channel, ability.Model, keyFingerprint)
			if fingerprint != "" && !slices.Contains(result[ability.Model], fingerprint) {
				result[ability.Model] = append(result[ability.Model], fingerprint)
			}
		}
	}
	for name := range result {
		slices.Sort(result[name])
	}
	return result, nil
}

// EnsureSeedanceAssetSynchronization 补排权限或渠道变化后新增的模型目标。
func EnsureSeedanceAssetSynchronization(ctx context.Context, userID int, assets []model.SeedanceAsset, requestedGroups, allowedModels []string, pinnedChannel int) error {
	if len(assets) == 0 {
		return nil
	}
	channels, err := FindSeedanceAssetChannels()
	if errors.Is(err, ErrSeedanceAssetChannelUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	if pinnedChannel > 0 {
		channels = slices.DeleteFunc(channels, func(channel *model.Channel) bool { return channel.Id != pinnedChannel })
	}
	clients, err := seedanceAssetSyncClientsForUser(ctx, userID, channels, requestedGroups, allowedModels)
	if err != nil || len(clients) == 0 {
		return err
	}
	scope, err := EncodeSeedanceAssetSyncScope(requestedGroups, allowedModels, pinnedChannel)
	if err != nil {
		return err
	}
	groupIDs, assetIDs := make([]string, 0, len(assets)), make([]uint, 0, len(assets))
	for _, asset := range assets {
		if asset.UserID == userID && asset.Status != "Deleting" && (asset.LocalOnly == nil || !*asset.LocalOnly) {
			groupIDs = append(groupIDs, asset.GroupID)
			assetIDs = append(assetIDs, asset.ID)
		}
	}
	if len(assetIDs) == 0 {
		return nil
	}
	var groups []model.SeedanceAssetGroup
	if err := model.DB.WithContext(ctx).Where("user_id = ? AND group_id IN ? AND status <> ?", userID, groupIDs, "Deleting").Find(&groups).Error; err != nil {
		return err
	}
	groupsByID := make(map[string]model.SeedanceAssetGroup, len(groups))
	for _, group := range groups {
		groupsByID[group.GroupID] = group
	}
	var replicas []model.SeedanceAssetReplica
	if err := model.DB.WithContext(ctx).Where("user_id = ? AND local_asset_id IN ?", userID, assetIDs).Find(&replicas).Error; err != nil {
		return err
	}
	existing := make(map[uint]map[string]*model.SeedanceAssetReplica, len(assetIDs))
	for index := range replicas {
		replica := &replicas[index]
		if existing[replica.LocalAssetID] == nil {
			existing[replica.LocalAssetID] = make(map[string]*model.SeedanceAssetReplica)
		}
		existing[replica.LocalAssetID][replica.AccountFingerprint] = replica
	}
	var targets []model.SeedanceAssetReplica
	updatedScope := false
	for _, asset := range assets {
		group, exists := groupsByID[asset.GroupID]
		if !exists || asset.UserID != userID || asset.Status == "Deleting" || strings.EqualFold(group.GroupType, "LivenessFace") || asset.LocalOnly != nil && *asset.LocalOnly {
			continue
		}
		for _, client := range clients {
			if replica := existing[asset.ID][client.AccountFingerprint]; replica != nil {
				if replica.SyncScope != scope {
					restart := replica.LastError == errSeedanceAssetSyncTargetUnavailable.Error()
					updated, err := model.AuthorizeSeedanceAssetSyncScope(ctx, replica, scope, common.GetTimestamp(), restart)
					if err != nil {
						return err
					}
					updatedScope = updatedScope || updated
				}
				continue
			}
			if asset.UpstreamAssetID() != "" && client.matchesAccountFingerprint(asset.AccountFingerprint) {
				continue
			}
			targets = append(targets, model.SeedanceAssetReplica{
				UserID: userID, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: client.ChannelID,
				AccountFingerprint: client.AccountFingerprint, KeyFingerprint: client.KeyFingerprint, SyncScope: scope,
				ProvisionStatus: model.SeedanceAssetReplicaCreating, Status: "Pending", NextPollAt: common.GetTimestamp(),
			})
		}
	}
	if len(targets) == 0 && !updatedScope {
		return nil
	}
	if err := model.ScheduleSeedanceAssetSyncTargets(ctx, userID, targets); err != nil {
		return err
	}
	if _, _, err := EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetSync, nil); err != nil {
		common.SysError("failed to wake Seedance asset synchronization: " + err.Error())
	}
	return nil
}

// SeedanceAssetStatusAccounts 返回当前凭证范围内的账号摘要，用于状态查询和重试。
func SeedanceAssetStatusAccounts(ctx context.Context, userID int, requestedGroups, allowedModels []string, pinnedChannels ...int) ([]string, error) {
	channels, err := FindSeedanceAssetChannels()
	if err != nil {
		return nil, err
	}
	if len(pinnedChannels) > 0 && pinnedChannels[0] > 0 {
		channels = slices.DeleteFunc(channels, func(channel *model.Channel) bool { return channel.Id != pinnedChannels[0] })
	}
	clients, err := seedanceAssetSyncClientsForUser(ctx, userID, channels, requestedGroups, allowedModels, true)
	if err != nil {
		return nil, err
	}
	fingerprints := make([]string, 0, len(clients))
	channelsByID := make(map[int]*model.Channel, len(channels))
	for _, channel := range channels {
		channelsByID[channel.Id] = channel
	}
	for _, client := range clients {
		accounts := append([]string{client.AccountFingerprint}, model.SeedanceAssetAccountFingerprints(channelsByID[client.ChannelID], client.KeyFingerprint)...)
		for _, fingerprint := range accounts {
			if !slices.Contains(fingerprints, fingerprint) {
				fingerprints = append(fingerprints, fingerprint)
			}
		}
	}
	return fingerprints, nil
}

// RetrySeedanceAssetSynchronization 按当前凭证可用分组重新安排失败的副本。
func RetrySeedanceAssetSynchronization(ctx context.Context, userID int, assetID uint, requestedGroups, allowedModels []string, pinnedChannels ...int) error {
	fingerprints, err := SeedanceAssetStatusAccounts(ctx, userID, requestedGroups, allowedModels, pinnedChannels...)
	if err != nil {
		return err
	}
	return model.RetrySeedanceAssetSyncTargets(ctx, userID, assetID, fingerprints, common.GetTimestamp())
}

// RunSeedanceAssetSynchronizationOnce 与审核轮询分开调度，慢导入不阻塞已有素材状态更新。
func RunSeedanceAssetSynchronizationOnce(ctx context.Context, report func(processed, total int)) (SeedanceAssetPollSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SeedanceAssetPollSummary{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type completedSync struct {
		account string
		result  seedanceAssetPollResult
	}
	summary := SeedanceAssetPollSummary{}
	active := make(map[string]struct{}, seedanceAssetPollWorkerSize)
	blocked := make(map[string]time.Time)
	results := make(chan completedSync, seedanceAssetPollWorkerSize)
	ticker := time.NewTicker(seedanceAssetSyncDiscoveryInterval)
	defer ticker.Stop()
	processed := 0
	var runErr error
	for {
		started := false
		if runErr == nil && len(active) < seedanceAssetPollWorkerSize {
			excluded := make([]string, 0, len(active)+len(blocked))
			for account := range active {
				excluded = append(excluded, account)
			}
			for account, retryAt := range blocked {
				if !time.Now().Before(retryAt) {
					delete(blocked, account)
					continue
				}
				excluded = append(excluded, account)
			}
			replicas, err := model.ListDueSeedanceAssetSyncExceptAccounts(ctx, common.GetTimestamp(), seedanceAssetPollBatchSize, excluded)
			if err != nil {
				runErr = err
				cancel()
			} else {
				for _, replica := range replicas {
					if len(active) == seedanceAssetPollWorkerSize {
						break
					}
					if _, exists := active[replica.AccountFingerprint]; exists {
						continue
					}
					active[replica.AccountFingerprint] = struct{}{}
					summary.Candidates++
					started = true
					go func(replica *model.SeedanceAssetReplica) {
						result := synchronizeSeedanceAssetReplica(ctx, &seedanceAssetSyncJob{replica: replica})
						results <- completedSync{account: replica.AccountFingerprint, result: result}
					}(replica)
				}
			}
		}
		if len(active) == 0 {
			return summary, errors.Join(runErr, ctx.Err())
		}
		if started && report != nil {
			report(processed, summary.Candidates)
		}
		if started && len(active) < seedanceAssetPollWorkerSize {
			continue
		}
		select {
		case completed := <-results:
			delete(active, completed.account)
			processed++
			result := completed.result
			if result.claimed {
				summary.Claimed++
			}
			if result.updated {
				summary.Updated++
			}
			if result.retryScheduled {
				summary.RetryScheduled++
			}
			if result.skipped {
				summary.Skipped++
			}
			// 未推进时暂缓该账号，避免数据库故障导致原地重试。
			if result.skipped || result.err != nil && !result.claimed {
				blocked[completed.account] = time.Now().Add(seedanceAssetSyncBlockedDelay)
			}
			if result.err != nil {
				summary.Errors++
				logger.LogWarn(ctx, fmt.Sprintf("Seedance asset synchronization failed: %v", result.err))
			}
			if report != nil {
				report(processed, summary.Candidates)
			}
		case <-ticker.C:
		case <-ctx.Done():
			runErr = errors.Join(runErr, ctx.Err())
			for len(active) > 0 {
				completed := <-results
				delete(active, completed.account)
			}
			return summary, runErr
		}
	}
}

// resolveSeedanceAssetSyncClient 每项任务执行前校验权限；原渠道失效时仅在同一共享范围内恢复。
func resolveSeedanceAssetSyncClient(ctx context.Context, replica *model.SeedanceAssetReplica) (*SeedanceAssetClient, error) {
	var scope seedanceAssetSyncScope
	if replica.SyncScope == "" || common.UnmarshalJsonStr(replica.SyncScope, &scope) != nil || scope.Version != 1 || scope.PinnedChannel < 0 {
		return nil, errSeedanceAssetSyncTargetUnavailable
	}
	canonical, err := EncodeSeedanceAssetSyncScope(scope.Groups, scope.Models, scope.PinnedChannel)
	if err != nil || canonical != replica.SyncScope {
		return nil, errSeedanceAssetSyncTargetUnavailable
	}
	channel, err := model.GetChannelById(replica.ChannelID, true)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
	}
	if channel != nil && channel.Status == common.ChannelStatusEnabled && isSeedanceAssetChannel(channel) && (scope.PinnedChannel == 0 || scope.PinnedChannel == channel.Id) {
		clients, err := seedanceAssetSyncClientsForUser(ctx, replica.UserID, []*model.Channel{channel}, scope.Groups, scope.Models)
		if err != nil {
			return nil, err
		}
		if client := clients[replica.AccountFingerprint]; client != nil {
			return client, nil
		}
	}
	channels, err := FindSeedanceAssetChannels()
	if err != nil {
		return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
	}
	if scope.PinnedChannel > 0 {
		channels = slices.DeleteFunc(channels, func(channel *model.Channel) bool { return channel.Id != scope.PinnedChannel })
	}
	clients, err := seedanceAssetSyncClientsForUser(ctx, replica.UserID, channels, scope.Groups, scope.Models)
	if err != nil {
		return nil, err
	}
	if client := clients[replica.AccountFingerprint]; client != nil {
		return client, nil
	}
	return nil, errSeedanceAssetSyncTargetUnavailable
}

func synchronizeSeedanceAssetReplica(ctx context.Context, job *seedanceAssetSyncJob) (result seedanceAssetPollResult) {
	client, err := resolveSeedanceAssetSyncClient(ctx, job.replica)
	if err == nil {
		release := acquireSeedanceAssetSyncAccount(client.syncAccount)
		if release == nil {
			return seedanceAssetPollResult{skipped: true}
		}
		defer release()
	}
	defer func() {
		if ctx.Err() == nil {
			result.err = errors.Join(result.err, model.RefreshReplicaManagedSeedanceAsset(ctx, job.replica.UserID, job.replica.LocalAssetID))
		}
	}()
	if err == nil {
		var asset model.SeedanceAsset
		err = model.DB.WithContext(ctx).Where("id = ? AND user_id = ? AND status <> ?", job.replica.LocalAssetID, job.replica.UserID, "Deleting").First(&asset).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return seedanceAssetPollResult{skipped: true}
		}
		if err == nil {
			operationCtx, cancel := context.WithTimeout(ctx, seedanceAssetJSONImportTimeout+3*seedanceAssetRequestTimeout)
			_, err = mapSeedanceAssetsToAccount(operationCtx, asset.UserID, client, []string{asset.AssetID}, true)
			cancel()
		}
	}
	if ctx.Err() != nil {
		return seedanceAssetPollResult{err: ctx.Err()}
	}
	var current model.SeedanceAssetReplica
	if readErr := model.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.replica.ID, job.replica.UserID).First(&current).Error; readErr != nil {
		if errors.Is(readErr, gorm.ErrRecordNotFound) {
			return seedanceAssetPollResult{skipped: true}
		}
		return seedanceAssetPollResult{err: readErr}
	}
	if current.ProvisionStatus == model.SeedanceAssetReplicaReady {
		return seedanceAssetPollResult{claimed: true, updated: true}
	}
	now := common.GetTimestamp()
	if current.PollLeaseUntil > now {
		return seedanceAssetPollResult{skipped: true}
	}
	if err == nil {
		err = ErrSeedanceAssetMappingPending
	}
	attempts, nextPollAt := SeedanceAssetPollSchedule("Processing", current.PollAttempts, now)
	paused := errors.Is(err, errSeedanceAssetSyncOwnerUnavailable) || errors.Is(err, errSeedanceAssetSyncTargetUnavailable)
	retryable := errors.Is(err, ErrSeedanceAssetMappingPending) || isSeedanceAssetServiceTemporary(err)
	if paused {
		attempts = seedanceAssetPollMaxAttempts
	}
	if paused || retryable && attempts >= seedanceAssetPollMaxAttempts {
		// 停止密集重试，保留低频恢复；状态汇总不再将其视为处理中。
		nextPollAt = now + int64(seedanceAssetSyncRecoveryDelay/time.Second)
	} else if !retryable {
		nextPollAt = 0
	}
	if nextPollAt > 0 {
		nextPollAt += int64(current.ID % seedanceAssetSyncRetryJitterSeconds)
	}
	updated, updateErr := model.ScheduleSeedanceAssetSyncRetry(ctx, &current, now, attempts, nextPollAt, err.Error())
	result.err = errors.Join(err, updateErr)
	result.claimed = updated
	result.retryScheduled = updated && nextPollAt > 0
	return result
}
