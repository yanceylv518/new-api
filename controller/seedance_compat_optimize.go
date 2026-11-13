package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func OptimizeSeedanceCompatAsset(c *gin.Context) {
	asset, err := seedanceCompatAsset(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	group, err := seedanceCompatGroup(c, asset.GroupID)
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	if strings.EqualFold(group.GroupType, "LivenessFace") {
		seedanceCompatError(c, http.StatusBadRequest, "real-person assets require verification")
		return
	}
	var request struct {
		ModelFamily string `json:"model_family"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.ModelFamily) == "" {
		seedanceCompatError(c, http.StatusBadRequest, "model_family is required")
		return
	}
	choices, err := selectSeedanceAssetValidationChoices(c, strings.TrimSpace(request.ModelFamily), true)
	if err != nil {
		seedanceCompatChannelError(c, err)
		return
	}
	channel := choices[0].channel
	client, err := service.NewSeedanceAssetClient(channel)
	if err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "asset service is unavailable")
		return
	}
	scopedClient := client.ForSeedanceModel(channel, choices[0].model)
	if asset.LocalOnly != nil && *asset.LocalOnly {
		scope, err := service.EncodeSeedanceAssetSyncScope(seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c))
		if err != nil {
			seedanceCompatError(c, http.StatusServiceUnavailable, "asset optimization is temporarily unavailable")
			return
		}
		upstreamGroupID, err := service.EnsureSeedanceAssetGroupForAccount(c.Request.Context(), c.GetInt("id"), asset.GroupID, scopedClient)
		if err != nil {
			seedanceCompatError(c, http.StatusServiceUnavailable, "asset optimization is temporarily unavailable")
			return
		}
		source := strings.TrimSpace(asset.SourceURL)
		if asset.ObjectKey != "" {
			source, err = service.SeedanceAssetOSSUpstreamURL(c.Request.Context(), asset.ObjectKey, asset.Storage)
		}
		if err != nil || service.ValidateSeedanceAssetSourceURL(source) != nil {
			seedanceCompatError(c, http.StatusBadGateway, "asset source is unavailable")
			return
		}
		upstreamID, err := client.CreateSeedanceAsset(c.Request.Context(), upstreamGroupID, source, asset.AssetType, asset.Name)
		if err != nil {
			seedanceCompatError(c, http.StatusBadGateway, "asset optimization failed")
			return
		}
		err = model.PromoteLocalSeedanceAsset(c.Request.Context(), c.GetInt("id"), asset.ID, asset.GroupID, asset.UpdatedAt,
			scopedClient.ChannelID, scopedClient.KeyFingerprint, scopedClient.AccountFingerprint, upstreamID, scope)
		if err != nil {
			if rollbackErr := service.RollbackSeedanceAsset(c.Request.Context(), client, upstreamID, ""); rollbackErr != nil {
				common.SysError("seedance asset optimization rollback failed: " + rollbackErr.Error())
			}
			seedanceCompatError(c, http.StatusConflict, "asset changed while optimization was starting")
			return
		}
		localOnly := false
		asset.LocalOnly = &localOnly
		asset.PrimaryUpstreamID = upstreamID
		asset.PrimarySyncScope = scope
		asset.ChannelID, asset.KeyFingerprint, asset.AccountFingerprint = scopedClient.ChannelID, scopedClient.KeyFingerprint, scopedClient.AccountFingerprint
		asset.Status = "Processing"
		enqueueSeedanceAssetPolling()
		if err := service.EnsureSeedanceAssetSynchronization(c.Request.Context(), c.GetInt("id"), []model.SeedanceAsset{*asset}, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
			seedanceCompatLookupError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": seedanceCompatAssetGroupView(*asset), "status": "processing"})
		return
	}
	if asset.UpstreamAssetID() != "" && asset.KeyFingerprint == client.KeyFingerprint &&
		(asset.AccountFingerprint == scopedClient.AccountFingerprint ||
			asset.AccountFingerprint == "" && asset.ChannelID == channel.Id) {
		status := "already_optimized"
		if strings.EqualFold(asset.Status, "Failed") {
			if asset.FailureCode == model.SeedanceAssetReviewTimeoutCode {
				seedanceCompatError(c, http.StatusConflict, "asset review timed out; please retry later")
				return
			}
			seedanceCompatError(c, http.StatusConflict, "asset processing failed")
			return
		}
		if strings.EqualFold(asset.Status, "Processing") || strings.EqualFold(asset.Status, "Pending") {
			status = "processing"
		}
		assets := []model.SeedanceAsset{*asset}
		if err := projectSeedanceAssetStatuses(c, assets, true); err != nil {
			seedanceCompatLookupError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": seedanceCompatAssetGroupView(assets[0]), "status": status})
		return
	}
	if err := service.EnsureSeedanceAssetSynchronization(c.Request.Context(), c.GetInt("id"), []model.SeedanceAsset{*asset}, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	if _, err := model.FindSeedanceAssetReplica(c.Request.Context(), c.GetInt("id"), asset.ID, scopedClient.AccountFingerprint); err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "asset optimization is temporarily unavailable")
		return
	}
	_, mapErr := service.MapSeedanceAssetsToModelAccount(c.Request.Context(), c.GetInt("id"), client, channel, choices[0].model, []string{asset.AssetID})
	if mapErr != nil && !errors.Is(mapErr, service.ErrSeedanceAssetMappingPending) {
		seedanceCompatError(c, http.StatusBadGateway, "asset optimization failed")
		return
	}
	replica, err := model.FindSeedanceAssetReplica(c.Request.Context(), c.GetInt("id"), asset.ID, scopedClient.AccountFingerprint)
	if err != nil || replica == nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "asset optimization is temporarily unavailable")
		return
	}
	result := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).
		Where("id = ? AND user_id = ? AND group_id = ? AND status <> ?", asset.ID, c.GetInt("id"), asset.GroupID, "Deleting").
		Update("compat_optimize_replica_id", replica.ID)
	if result.Error != nil {
		seedanceCompatError(c, http.StatusConflict, "asset changed while optimization was starting")
		return
	}
	if result.RowsAffected == 0 {
		var current model.SeedanceAsset
		if err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ? AND group_id = ? AND status <> ?", asset.ID, c.GetInt("id"), asset.GroupID, "Deleting").First(&current).Error; err != nil || current.CompatOptimizeReplicaID != replica.ID {
			seedanceCompatError(c, http.StatusConflict, "asset changed while optimization was starting")
			return
		}
	}
	asset.CompatOptimizeReplicaID = replica.ID
	status := "already_optimized"
	if mapErr != nil {
		status = "processing"
	}
	assets := []model.SeedanceAsset{*asset}
	if err := projectSeedanceAssetStatuses(c, assets, true); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": seedanceCompatAssetGroupView(assets[0]), "status": status})
}
