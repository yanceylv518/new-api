package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

// ErrSeedanceAssetScopeUnavailable 表示范围无效或其中没有可用的绑定账号。
var ErrSeedanceAssetScopeUnavailable = errors.New("bound Seedance account is unavailable within the allowed scope")

// SeedanceAssetScopeRequiresReauthorization 仅识别版本 1，不使用旧范围推导权限。
func SeedanceAssetScopeRequiresReauthorization(scope string) bool {
	var stored seedanceAssetSyncScope
	return scope != "" && common.UnmarshalJsonStr(scope, &stored) == nil && stored.Version == 1
}

// EnsureSeedanceAssetPrimaryAuthorization 用本次已验证范围恢复在途主素材，不复制旧范围中的权限。
func EnsureSeedanceAssetPrimaryAuthorization(ctx context.Context, userID int, assets []model.SeedanceAsset, scope string) (bool, error) {
	type binding struct {
		channelID    int
		key, account string
		modelScoped  bool
	}
	authorized := make(map[binding]bool)
	pending, updatedAny := false, false
	for index := range assets {
		asset := &assets[index]
		status := strings.ToLower(strings.TrimSpace(asset.Status))
		if asset.UserID != userID || asset.UpstreamDeletedAt != 0 || asset.UpstreamAssetID() == "" ||
			!SeedanceAssetScopeRequiresReauthorization(asset.PrimarySyncScope) ||
			status != "pending" && status != "processing" && !(status == "failed" && asset.FailureCode == model.SeedanceAssetReviewTimeoutCode) {
			continue
		}
		modelScoped := asset.LocalOnly != nil && !*asset.LocalOnly && asset.PrimaryUpstreamID != ""
		key := binding{asset.ChannelID, asset.KeyFingerprint, asset.AccountFingerprint, modelScoped}
		allowed, checked := authorized[key]
		if !checked {
			_, err := ResolveSeedanceAssetClientWithinScope(ctx, userID, asset.ChannelID, asset.KeyFingerprint, asset.AccountFingerprint, scope, modelScoped)
			if err != nil && !errors.Is(err, ErrSeedanceAssetScopeUnavailable) {
				return pending, err
			}
			allowed = err == nil
			authorized[key] = allowed
		}
		if !allowed {
			continue
		}
		updated, err := model.AuthorizeSeedanceAssetPrimaryScope(ctx, asset, scope, common.GetTimestamp())
		if err != nil {
			return pending, err
		}
		pending = pending || !updated
		updatedAny = updatedAny || updated
	}
	if updatedAny {
		if _, _, err := EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetPoll, nil); err != nil {
			common.SysError("failed to wake Seedance asset polling: " + err.Error())
		}
	}
	return pending, nil
}

// ResolveSeedanceAssetClientWithinScope 在原始凭证范围和用户当前权限内恢复素材账号。
// scope 必须由 EncodeSeedanceAssetSyncScope 编码；nil 分组使用当前用户可用分组，nil 模型表示不限制模型，空切片表示无权限。
// modelScoped 为 true 时匹配 AIGC 模型指纹，否则匹配真人认证账号指纹；权限不足返回 ErrSeedanceAssetScopeUnavailable。
func ResolveSeedanceAssetClientWithinScope(ctx context.Context, userID, channelID int, keyFingerprint, accountFingerprint, scope string, modelScoped bool) (*SeedanceAssetClient, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if model.DB == nil {
		return nil, errors.New("database is not initialized")
	}
	var allowed seedanceAssetSyncScope
	if userID <= 0 || channelID <= 0 || scope == "" || common.UnmarshalJsonStr(scope, &allowed) != nil || allowed.Version != seedanceAssetSyncScopeVersion || allowed.PinnedChannel < 0 {
		return nil, ErrSeedanceAssetScopeUnavailable
	}
	canonical, err := EncodeSeedanceAssetSyncScope(allowed.Groups, allowed.Models, allowed.PinnedChannel)
	if err != nil || canonical != scope {
		return nil, ErrSeedanceAssetScopeUnavailable
	}
	if accountFingerprint == "" && !modelScoped {
		original, err := model.GetChannelById(channelID, true)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrSeedanceAssetScopeUnavailable
			}
			return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
		}
		accountFingerprint = model.SeedanceAssetAccountFingerprint(original, keyFingerprint)
	}
	if strings.TrimSpace(accountFingerprint) == "" {
		return nil, ErrSeedanceAssetScopeUnavailable
	}
	channels, err := FindSeedanceAssetChannels()
	if err != nil {
		if errors.Is(err, ErrSeedanceAssetChannelUnavailable) {
			return nil, ErrSeedanceAssetScopeUnavailable
		}
		return nil, errors.Join(ErrSeedanceAssetServiceTemporary, err)
	}
	if allowed.PinnedChannel > 0 {
		channels = slices.DeleteFunc(channels, func(channel *model.Channel) bool { return channel.Id != allowed.PinnedChannel })
	}
	if index := slices.IndexFunc(channels, func(channel *model.Channel) bool { return channel.Id == channelID }); index > 0 {
		original := channels[index]
		copy(channels[1:index+1], channels[:index])
		channels[0] = original
	}
	clients, err := seedanceAssetSyncClientsForUser(ctx, userID, channels, allowed.Groups, allowed.Models, !modelScoped)
	if err != nil {
		if errors.Is(err, errSeedanceAssetSyncOwnerUnavailable) {
			return nil, ErrSeedanceAssetScopeUnavailable
		}
		return nil, err
	}
	if modelScoped {
		client := clients[accountFingerprint]
		if client == nil {
			return nil, ErrSeedanceAssetScopeUnavailable
		}
		if client.ChannelID == channelID && client.KeyFingerprint != keyFingerprint && keyFingerprint != "" {
			original, err := NewSeedanceAssetClient(channels[0], keyFingerprint)
			if err == nil && original.matchesAccountFingerprint(accountFingerprint) {
				return original, nil
			}
		}
		return client, nil
	}
	clientsByChannel := make(map[int][]*SeedanceAssetClient, len(channels))
	for _, client := range clients {
		clientsByChannel[client.ChannelID] = append(clientsByChannel[client.ChannelID], client)
	}
	for _, channel := range channels {
		var selected *SeedanceAssetClient
		for _, scoped := range clientsByChannel[channel.Id] {
			client, err := NewSeedanceAssetClient(channel, scoped.KeyFingerprint)
			if err != nil || !client.matchesAccountFingerprint(accountFingerprint) {
				continue
			}
			if client.KeyFingerprint == keyFingerprint {
				return client, nil
			}
			selected = client
		}
		if selected != nil {
			if channel.Id == channelID && keyFingerprint != "" {
				original, err := NewSeedanceAssetClient(channel, keyFingerprint)
				if err == nil && original.matchesAccountFingerprint(accountFingerprint) {
					return original, nil
				}
			}
			return selected, nil
		}
	}
	return nil, ErrSeedanceAssetScopeUnavailable
}
