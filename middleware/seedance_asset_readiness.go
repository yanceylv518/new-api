package middleware

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

const seedanceReadyScopeKey = "seedance_asset_ready_scope"

var errSeedanceAssetChannelOutsideGroup = errors.New("selected channel is unavailable for this model and group")
var errSeedanceAssetSynchronizationFailed = errors.New("private asset synchronization failed; retry after the issue is resolved")

type seedanceReadyScope struct {
	keys    map[int]map[string]struct{}
	models  map[int]string
	pending bool
	failed  bool
}

// prepareSeedanceReadyChannels 只在本次请求允许的分组和模型内选取素材已就绪的账号。
func prepareSeedanceReadyChannels(c *gin.Context, modelName string, assetIDs []string) error {
	ctx, userID := c.Request.Context(), c.GetInt("id")
	assets, err := model.FindSeedanceAssetsForUser(ctx, userID, assetIDs)
	if err != nil {
		return err
	}
	groupIDs := make([]string, 0, len(assets))
	seenGroups := make(map[string]struct{}, len(assets))
	localIDs := make([]uint, 0, len(assets))
	for _, asset := range assets {
		localIDs = append(localIDs, asset.ID)
		if _, exists := seenGroups[asset.GroupID]; !exists {
			seenGroups[asset.GroupID] = struct{}{}
			groupIDs = append(groupIDs, asset.GroupID)
		}
	}
	var groups []model.SeedanceAssetGroup
	if err := model.DB.WithContext(ctx).Where("user_id = ? AND group_id IN ? AND status <> ?", userID, groupIDs, "Deleting").Find(&groups).Error; err != nil {
		return err
	}
	if len(groups) != len(groupIDs) {
		return model.ErrSeedanceAssetUnavailable
	}
	groupsByID := make(map[string]model.SeedanceAssetGroup, len(groups))
	for _, group := range groups {
		groupsByID[group.GroupID] = group
	}
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	requestGroups := []string{usingGroup}
	if usingGroup == "auto" {
		requestGroups = service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	models := []string{modelName}
	if normalized := ratio_setting.RoutingMatchModelName(modelName); normalized != modelName {
		models = append(models, normalized)
	}
	var abilities []model.Ability
	if err := model.DB.WithContext(ctx).Where(map[string]any{"group": requestGroups, "model": models, "enabled": true}).Find(&abilities).Error; err != nil {
		return err
	}
	channelModels := make(map[int]string, len(abilities))
	channelIDs := make([]int, 0, len(abilities))
	for _, ability := range abilities {
		if _, exists := channelModels[ability.ChannelId]; !exists {
			channelIDs = append(channelIDs, ability.ChannelId)
		}
		if channelModels[ability.ChannelId] == "" || ability.Model == modelName {
			channelModels[ability.ChannelId] = ability.Model
		}
	}
	if len(channelIDs) == 0 {
		if _, pinned, _ := service.GetChannelConstraints(c).ResolvedPin(); pinned {
			return errSeedanceAssetChannelOutsideGroup
		}
		return nil
	}
	var candidates []model.Channel
	if err := model.DB.WithContext(ctx).Where("id IN ?", channelIDs).Find(&candidates).Error; err != nil {
		return err
	}
	constraints := service.GetChannelConstraints(c)
	pin, pinned, _ := constraints.ResolvedPin()
	pinnedChannel := 0
	if pinned {
		pinnedChannel = pin.ChannelId
	}
	syncScope, err := service.EncodeSeedanceAssetSyncScope(requestGroups, models, pinnedChannel)
	if err != nil {
		return err
	}
	if pinned && !slices.ContainsFunc(candidates, func(channel model.Channel) bool { return channel.Id == pin.ChannelId }) {
		return errSeedanceAssetChannelOutsideGroup
	}
	if len(candidates) == 0 {
		return nil
	}
	channelsByID := make(map[int]*model.Channel, len(candidates))
	for index := range candidates {
		channelsByID[candidates[index].Id] = &candidates[index]
	}
	originIDs := make([]int, 0)
	for _, asset := range assets {
		if asset.AccountFingerprint == "" && groupsByID[asset.GroupID].AccountFingerprint == "" && channelsByID[asset.ChannelID] == nil {
			originIDs = append(originIDs, asset.ChannelID)
		}
	}
	for _, group := range groups {
		if strings.EqualFold(group.GroupType, "LivenessFace") && group.AccountFingerprint == "" && channelsByID[group.ChannelID] == nil {
			originIDs = append(originIDs, group.ChannelID)
		}
	}
	if len(originIDs) > 0 {
		var origins []model.Channel
		if err := model.DB.WithContext(ctx).Where("id IN ?", originIDs).Find(&origins).Error; err != nil {
			return err
		}
		for index := range origins {
			channelsByID[origins[index].Id] = &origins[index]
		}
	}
	var replicas []model.SeedanceAssetReplica
	if err := model.DB.WithContext(ctx).Where("user_id = ? AND local_asset_id IN ?", userID, localIDs).Find(&replicas).Error; err != nil {
		return err
	}
	replicasByAsset := make(map[uint]map[string]*model.SeedanceAssetReplica, len(assets))
	for index := range replicas {
		replica := &replicas[index]
		if replicasByAsset[replica.LocalAssetID] == nil {
			replicasByAsset[replica.LocalAssetID] = make(map[string]*model.SeedanceAssetReplica)
		}
		replicasByAsset[replica.LocalAssetID][replica.AccountFingerprint] = replica
	}
	scope := &seedanceReadyScope{keys: make(map[int]map[string]struct{}), models: channelModels}
	targets := make([]model.SeedanceAssetReplica, 0)
	seenTargets := make(map[string]struct{})
	candidateAccounts := 0
	needsAuthorization := false
	for index := range candidates {
		channel := &candidates[index]
		channelID := channel.Id
		if channel.Status != common.ChannelStatusEnabled || !service.SupportsSeedanceAssets(channel) || pinned && pin.ChannelId != channelID {
			continue
		}
		if ok, _ := model.ChannelSatisfiesFilters(channel, modelName, constraints.Filters); !ok {
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
			fingerprints := model.SeedanceAssetAccountFingerprints(channel, keyFingerprint)
			if len(fingerprints) == 0 {
				continue
			}
			candidateAccounts++
			ready, pending, failed := true, false, false
			for _, asset := range assets {
				group := groupsByID[asset.GroupID]
				assetFingerprints := fingerprints
				if !strings.EqualFold(group.GroupType, "LivenessFace") {
					assetFingerprints = []string{model.SeedanceAssetModelScopeFingerprint(channel, channelModels[channelID], keyFingerprint)}
				}
				if strings.EqualFold(group.GroupType, "LivenessFace") {
					groupAccount := group.AccountFingerprint
					if groupAccount == "" {
						groupAccount = model.SeedanceAssetAccountFingerprint(channelsByID[group.ChannelID], group.KeyFingerprint)
					}
					if groupAccount != "" && !slices.Contains(fingerprints, groupAccount) ||
						groupAccount == "" && (group.ChannelID != channelID || group.KeyFingerprint != keyFingerprint) {
						ready, failed = false, true
						continue
					}
				}
				if asset.LocalOnly != nil && *asset.LocalOnly {
					if !strings.EqualFold(asset.Status, "Active") {
						ready, failed = false, true
					}
					continue
				}
				origin := asset.AccountFingerprint
				if origin == "" {
					origin = group.AccountFingerprint
				}
				if origin == "" {
					origin = model.SeedanceAssetAccountFingerprint(channelsByID[asset.ChannelID], asset.KeyFingerprint)
				}
				originMatches := origin != "" && slices.Contains(assetFingerprints, origin) ||
					origin == "" && asset.ChannelID == channelID && asset.KeyFingerprint == keyFingerprint &&
						(strings.EqualFold(group.GroupType, "LivenessFace") || channel.GetSetting().SeedanceAssetShareGroup != "")
				if originMatches && asset.UpstreamAssetID() != "" {
					if asset.UpstreamDeletedAt != 0 {
						ready, failed = false, true
						continue
					}
					switch strings.ToLower(strings.TrimSpace(asset.Status)) {
					case "active", "success", "succeeded":
					case "failed":
						ready, failed = false, true
					default:
						ready, pending = false, true
					}
					continue
				}
				var replica *model.SeedanceAssetReplica
				for _, fingerprint := range assetFingerprints {
					if replica = replicasByAsset[asset.ID][fingerprint]; replica != nil {
						break
					}
				}
				if replica != nil && replica.ProvisionStatus == model.SeedanceAssetReplicaReady && replica.UpstreamAssetID != "" && replica.UpstreamDeletedAt == 0 {
					switch strings.ToLower(strings.TrimSpace(replica.Status)) {
					case "active", "success", "succeeded":
						continue
					}
				}
				ready = false
				if replica != nil {
					if replica.SyncScope != syncScope {
						needsAuthorization = true
					}
					if replica.UpstreamDeletedAt != 0 || strings.EqualFold(replica.Status, "Failed") || replica.ProvisionStatus == model.SeedanceAssetReplicaFailed && (replica.NextPollAt == 0 || replica.PollAttempts >= model.SeedanceAssetSyncMaxAttempts) ||
						replica.ProvisionStatus == model.SeedanceAssetReplicaReady && replica.LastError != "" && replica.PollAttempts >= model.SeedanceAssetSyncMaxAttempts {
						failed = true
					} else {
						pending = true
					}
					continue
				}
				if strings.EqualFold(group.GroupType, "LivenessFace") {
					failed = true
					continue
				}
				pending = true
				identity := fmt.Sprintf("%d:%s", asset.ID, assetFingerprints[0])
				if _, exists := seenTargets[identity]; !exists {
					seenTargets[identity] = struct{}{}
					targets = append(targets, model.SeedanceAssetReplica{
						UserID: userID, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channelID,
						AccountFingerprint: assetFingerprints[0], KeyFingerprint: keyFingerprint, SyncScope: syncScope,
						ProvisionStatus: model.SeedanceAssetReplicaCreating, Status: "Pending", NextPollAt: common.GetTimestamp(),
					})
				}
			}
			if ready {
				if scope.keys[channelID] == nil {
					scope.keys[channelID] = make(map[string]struct{})
				}
				scope.keys[channelID][keyFingerprint] = struct{}{}
			} else if pending && !failed {
				scope.pending = true
			} else {
				scope.failed = true
			}
		}
	}
	if candidateAccounts == 0 {
		return nil
	}
	if needsAuthorization {
		if err := service.EnsureSeedanceAssetSynchronization(ctx, userID, assets, requestGroups, models, pinnedChannel); err != nil {
			return err
		}
	}
	if err := model.ScheduleSeedanceAssetSyncTargets(ctx, userID, targets); err != nil {
		return err
	}
	if len(targets) > 0 || len(scope.keys) == 0 && scope.pending {
		if _, _, err := service.EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetSync, nil); err != nil {
			common.SysError("failed to wake Seedance asset synchronization: " + err.Error())
		}
	}
	constraints.AddFilter(dto.ChannelFilter{Kind: dto.FilterSeedanceAssetReady, SeedanceAssetReadyKeys: scope.keys})
	c.Set(seedanceReadyScopeKey, scope)
	return nil
}
