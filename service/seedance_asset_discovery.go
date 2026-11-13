package service

import (
	"context"
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

var errSeedanceAssetDiscoveryScope = errors.New("invalid asset discovery scope")
var errSeedanceAssetDiscoveryReauthorization = errors.New("asset discovery requires reauthorization")

func QueueSeedanceAssetDiscovery(ctx context.Context, userID int, groups, models []string, pinnedChannel int) error {
	scope, err := EncodeSeedanceAssetSyncScope(groups, models, pinnedChannel)
	if err != nil {
		return err
	}
	if err := model.QueueSeedanceAssetDiscovery(ctx, userID, scope); err != nil {
		return err
	}
	_, _, err = EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetDiscovery, nil)
	return err
}

// RunSeedanceAssetDiscoveryBatch 重新校验范围后补排一批，成功后才推进游标。
func RunSeedanceAssetDiscoveryBatch(ctx context.Context) (processed bool, err error) {
	job, err := model.NextSeedanceAssetDiscovery(ctx, common.GetTimestamp())
	if err != nil || job == nil {
		return false, err
	}
	defer func() {
		if err != nil && ctx.Err() == nil {
			if errors.Is(err, errSeedanceAssetDiscoveryReauthorization) {
				err = errors.Join(err, model.RejectSeedanceAssetDiscovery(ctx, job, "scope requires reauthorization"))
			} else if errors.Is(err, errSeedanceAssetDiscoveryScope) {
				err = errors.Join(err, model.RejectSeedanceAssetDiscovery(ctx, job))
			} else {
				err = errors.Join(err, model.DeferSeedanceAssetDiscovery(ctx, job, common.GetTimestamp()))
			}
		}
	}()
	var scope seedanceAssetSyncScope
	if err = common.UnmarshalJsonStr(job.Scope, &scope); err != nil {
		return true, errors.Join(errSeedanceAssetDiscoveryScope, err)
	}
	if scope.Version == 1 {
		return true, errSeedanceAssetDiscoveryReauthorization
	}
	canonical, err := EncodeSeedanceAssetSyncScope(scope.Groups, scope.Models, scope.PinnedChannel)
	if err != nil {
		return true, err
	}
	// 只有编码器生成的完整范围才可用于后台任务，避免缺失字段被解释为无限制。
	if job.UserID <= 0 || scope.Version != seedanceAssetSyncScopeVersion || scope.PinnedChannel < 0 || canonical != job.Scope || job.ScopeKey != model.SeedanceAssetDiscoveryScopeKey(job.UserID, job.Scope) {
		return true, errSeedanceAssetDiscoveryScope
	}
	modelScopes, err := SeedanceAssetModelScopes(ctx, job.UserID, scope.Groups, scope.Models, scope.PinnedChannel)
	if errors.Is(err, errSeedanceAssetSyncOwnerUnavailable) {
		return true, model.AdvanceSeedanceAssetDiscovery(ctx, job, 0, true)
	}
	if err != nil {
		return true, err
	}
	scopes := make([]string, 0)
	seen := make(map[string]struct{})
	for _, fingerprints := range modelScopes {
		for _, fingerprint := range fingerprints {
			if _, exists := seen[fingerprint]; !exists {
				seen[fingerprint] = struct{}{}
				scopes = append(scopes, fingerprint)
			}
		}
	}
	accounts, err := SeedanceAssetStatusAccounts(ctx, job.UserID, scope.Groups, scope.Models, scope.PinnedChannel)
	if err != nil && !errors.Is(err, ErrSeedanceAssetChannelUnavailable) {
		return true, err
	}
	if len(scopes) == 0 && len(accounts) == 0 {
		return true, model.AdvanceSeedanceAssetDiscovery(ctx, job, 0, true)
	}
	batch, err := model.ListSeedanceAssetDiscoveryBatch(ctx, job.UserID, job.Cursor)
	if err != nil {
		return true, err
	}
	if len(batch) == 0 {
		return true, model.AdvanceSeedanceAssetDiscovery(ctx, job, 0, true)
	}
	ids := make([]uint, 0, len(batch))
	for _, asset := range batch {
		ids = append(ids, asset.ID)
	}
	assets, err := model.ListMissingSeedanceAssetSyncTargets(ctx, job.UserID, scopes, job.Cursor, ids, accounts)
	if err != nil {
		return true, err
	}
	if err = EnsureSeedanceAssetSynchronization(ctx, job.UserID, assets, scope.Groups, scope.Models, scope.PinnedChannel); err != nil {
		return true, err
	}
	pending, err := EnsureSeedanceAssetPrimaryAuthorization(ctx, job.UserID, assets, job.Scope)
	if err != nil {
		return true, err
	}
	remaining, err := model.ListMissingSeedanceAssetSyncTargets(ctx, job.UserID, scopes, job.Cursor, ids)
	if err != nil {
		return true, err
	}
	if pending || len(remaining) > 0 {
		return true, ErrSeedanceAssetServiceTemporary
	}
	return true, model.AdvanceSeedanceAssetDiscovery(ctx, job, batch[len(batch)-1].ID, len(batch) < model.SeedanceAssetDiscoveryBatchSize)
}
