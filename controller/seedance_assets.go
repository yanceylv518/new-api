package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type seedanceAssetGroupCreateRequest struct {
	Name        string  `json:"name" binding:"required,max=64"`
	Description *string `json:"description" binding:"omitempty,max=1024"`
	// 仅在建组时选择上游，已有素材的归属仍由认证用户和组绑定决定。
	Model string `json:"model" binding:"max=191"`
	// 兼容识别旧客户端的覆盖请求，普通建组固定为 AIGC。
	GroupType *string `json:"GroupType"`
}

// GroupType 只能由对应的上游流程创建，普通组编辑仅更新名称和本地备注。
type seedanceAssetGroupUpdateRequest struct {
	Name        string  `json:"name" binding:"required,max=64"`
	Description *string `json:"description" binding:"omitempty,max=1024"`
	Tags        *string `json:"tags" binding:"omitempty,max=512"`
}

// UpdateSeedanceAssetGroup 更新当前用户自己的素材组名称。
func UpdateSeedanceAssetGroup(c *gin.Context) {
	var req seedanceAssetGroupUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		seedanceAssetAPIErrorI18n(c, "invalid_params")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		seedanceAssetAPIErrorI18n(c, "invalid_params")
		return
	}
	var group model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&group).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if group.Status == "Deleting" {
		seedanceAssetAPIErrorMsg(c, "asset group is being deleted")
		return
	}
	client, err := service.ResolveSeedanceAssetClient(c.Request.Context(), group.ChannelID, group.KeyFingerprint, group.AccountFingerprint)
	if err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	remoteRename := client.SupportsGroupRename()
	if remoteRename {
		var response struct{}
		if err := client.UpdateSeedanceAssetGroup(c.Request.Context(), group.GroupID, strings.TrimSpace(req.Name), &response); err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
	}
	previousName := group.Name
	group.Name = strings.TrimSpace(req.Name)
	updates := map[string]any{"name": group.Name}
	if req.Description != nil {
		group.Description = strings.TrimSpace(*req.Description)
		updates["description"] = group.Description
	}
	if req.Tags != nil {
		group.Tags = strings.TrimSpace(*req.Tags)
		updates["tags"] = group.Tags
	}
	if err := model.DB.Model(&group).Updates(updates).Error; err != nil {
		if remoteRename {
			if rollbackErr := service.RollbackSeedanceAssetGroupName(c.Request.Context(), client, group.GroupID, previousName); rollbackErr != nil {
				common.SysError("failed to roll back Seedance asset group rename: " + rollbackErr.Error())
			}
		}
		seedanceAssetAPIError(c, err)
		return
	}
	seedanceAssetAPISuccess(c, group)
}

// DeleteSeedanceAssetGroup 将当前用户的素材组删除请求持久化并交给后台清理。
func DeleteSeedanceAssetGroup(c *gin.Context) {
	var group model.SeedanceAssetGroup
	if err := model.DB.WithContext(c.Request.Context()).
		Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&group).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	job := service.NewSeedanceAssetGroupDeleteCleanupJob(&group)
	if err := model.QueueSeedanceAssetGroupDeletion(c.Request.Context(), job); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	service.EnqueueSeedanceAssetCleanup()
	seedanceAssetAPIResponse(c, http.StatusAccepted, gin.H{
		"success": true,
		"message": "asset group deletion queued",
		"data":    nil,
	})
}

type seedanceAssetRequest struct {
	GroupID   string `json:"group_id" binding:"required"`
	SourceURL string `json:"source_url" binding:"required,url"`
	AssetType string `json:"asset_type" binding:"required,oneof=Image Video Audio"`
	Name      string `json:"name" binding:"max=64"`
}

// cleanupSeedanceAssetAfterFailure 在请求取消后仍回收上游素材和 OSS 对象。
func cleanupSeedanceAssetAfterFailure(ctx context.Context, client *service.SeedanceAssetClient, assetID, objectKey string, upload ...*service.SeedanceAssetUpload) {
	if err := service.RollbackSeedanceAsset(ctx, client, assetID, objectKey, upload...); err != nil {
		common.SysError("failed to roll back Seedance asset resources: " + err.Error())
	}
}

// cleanupSeedanceAssetGroupAfterFailure 在本地授权映射创建失败时回收上游素材组。
func cleanupSeedanceAssetGroupAfterFailure(ctx context.Context, client *service.SeedanceAssetClient, groupID string) {
	if err := service.RollbackSeedanceAssetGroup(ctx, client, groupID); err != nil {
		common.SysError("failed to roll back Seedance asset group: " + err.Error())
	}
}

// enqueueSeedanceAssetPolling 唤醒去重的后台任务，让新素材尽快开始状态同步。
func enqueueSeedanceAssetPolling() {
	if _, _, err := service.EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetPoll, nil); err != nil {
		common.SysError("failed to enqueue Seedance asset polling: " + err.Error())
	}
	if !model.HasDueSeedanceAssetSync(common.GetTimestamp()) {
		return
	}
	if _, _, err := service.EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetSync, nil); err != nil {
		common.SysError("failed to enqueue Seedance asset synchronization: " + err.Error())
	}
}

func seedanceAssetSynchronizationGroups(c *gin.Context) []string {
	if c.GetInt("token_id") == 0 {
		return nil
	}
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if group == "auto" {
		return service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	return []string{group}
}

func seedanceAssetSynchronizationModels(c *gin.Context) []string {
	if !c.GetBool("token_model_limit_enabled") {
		return nil
	}
	limits, _ := c.Get("token_model_limit")
	allowed, _ := limits.(map[string]bool)
	models := make([]string, 0, len(allowed))
	for name, enabled := range allowed {
		if enabled {
			models = append(models, name)
		}
	}
	return models
}

func seedanceAssetSynchronizationChannel(c *gin.Context) int {
	if pin, pinned, _ := service.GetChannelConstraints(c).ResolvedPin(); pinned {
		return pin.ChannelId
	}
	return 0
}

func seedanceAssetClientForRequest(c *gin.Context, channelID int, keyFingerprint, accountFingerprint string, modelScoped bool) (*service.SeedanceAssetClient, string, error) {
	scope, err := service.EncodeSeedanceAssetSyncScope(seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c))
	if err != nil {
		return nil, "", err
	}
	client, err := service.ResolveSeedanceAssetClientWithinScope(c.Request.Context(), c.GetInt("id"), channelID, keyFingerprint, accountFingerprint, scope, modelScoped)
	return client, scope, err
}

func seedanceAssetRequestScopeError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrSeedanceAssetScopeUnavailable) {
		seedanceAssetAPIResponse(c, http.StatusForbidden, gin.H{"success": false, "message": "asset account is unavailable for this credential"})
		return
	}
	seedanceAssetAPIError(c, err)
}

// createStoredSeedanceAsset 将 OSS 素材和账号同步目标一起持久化。
func createStoredSeedanceAsset(c *gin.Context, group *model.SeedanceAssetGroup, upload *service.SeedanceAssetUpload, name, sourceURL string) (*model.SeedanceAsset, error) {
	suffix, err := common.GenerateRandomCharsKey(16)
	if err != nil {
		return nil, err
	}
	localOnly := false
	asset := &model.SeedanceAsset{
		UserID: c.GetInt("id"), UploadBytes: upload.Size, ChannelID: group.ChannelID, GroupID: group.GroupID,
		AssetID: fmt.Sprintf("asset-%s-%s", time.Now().In(seedanceCompatTimeZone).Format("20060102150405"), strings.ToLower(suffix)),
		Name:    name, AssetType: upload.AssetType, SourceURL: sourceURL, ObjectKey: upload.ObjectKey, Storage: upload.Storage,
		KeyFingerprint: group.KeyFingerprint, AccountFingerprint: group.AccountFingerprint,
		LocalOnly: &localOnly, Status: "Processing",
	}
	previewURL, err := seedanceAssetPreviewURL(c.Request.Context(), asset)
	if err != nil {
		return nil, err
	}
	if err := service.CreateSeedanceAssetWithSynchronization(c.Request.Context(), asset, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
		return nil, err
	}
	asset.PreviewURL = previewURL
	enqueueSeedanceAssetPolling()
	return asset, nil
}

// seedanceAssetPreviewURL 为 OSS 对象按需签名，外部 URL 素材继续使用原始地址。
func seedanceAssetPreviewURL(ctx context.Context, asset *model.SeedanceAsset) (string, error) {
	return seedanceAssetPreviewURLWithStorage(ctx, asset, nil)
}

// seedanceAssetPreviewURLWithStorage 允许列表请求复用同一位置的 OSS 客户端。
func seedanceAssetPreviewURLWithStorage(ctx context.Context, asset *model.SeedanceAsset, storage *service.PrivateAssetOSSStorage) (string, error) {
	if strings.TrimSpace(asset.ObjectKey) != "" {
		if storage == nil {
			return service.SeedanceAssetOSSPreviewURL(ctx, asset.ObjectKey, asset.Storage)
		}
		return storage.PrivateAssetPreviewURL(ctx, asset.ObjectKey)
	}
	if previewURL := strings.TrimSpace(asset.PreviewURL); previewURL != "" {
		return previewURL, nil
	}
	if sourceURL := strings.TrimSpace(asset.SourceURL); sourceURL != "" {
		return sourceURL, nil
	}
	return "", nil
}

// GetSeedanceAssetUploadLimits 向素材库页面返回当前文件大小限制。
func GetSeedanceAssetUploadLimits(c *gin.Context) {
	limits := common.GetManagementRateLimits().AssetLimits
	common.ApiSuccess(c, gin.H{"image_mib": limits.ImageMiB, "video_mib": limits.VideoMiB, "audio_mib": limits.AudioMiB})
}

// ListSeedanceAssetGroups 只返回当前用户自己的素材组。
func ListSeedanceAssetGroups(c *gin.Context) {
	options, err := parseSeedanceLibraryQuery(c, true)
	if err != nil {
		seedanceAssetAPIResponse(c, http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	query := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetGroup{}).Where("user_id = ?", c.GetInt("id"))
	if c.GetBool("seedance_compat_ark_action") {
		query = query.Where("status <> ?", "Deleting")
	}
	query = options.filter(options.scope(query), true)
	var total int64
	// 无分页参数时保留旧页面全量数组行为，显式分页才计算总数。
	if options.paginated {
		if err := query.Count(&total).Error; err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
		if c.GetString(seedanceAssetActionContextKey) == "" {
			options.page.Page = max(1, min(options.page.Page, int((total+int64(options.page.PageSize)-1)/int64(options.page.PageSize))))
		}
		offset := options.page.GetStartIdx()
		if c.GetBool("seedance_compat_ark_token_page") {
			offset = c.GetInt("seedance_compat_ark_offset")
		}
		query = query.Offset(offset).Limit(options.page.PageSize)
	}
	var groups []model.SeedanceAssetGroup
	if err := query.Order(options.order).Find(&groups).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if err := model.LoadSeedanceAssetGroupDeletionStates(c.Request.Context(), c.GetInt("id"), groups); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if options.paginated {
		seedanceAssetAPIResponse(c, http.StatusOK, gin.H{"success": true, "message": "", "data": groups, "total": total, "page": options.page.Page, "page_size": options.page.PageSize})
		return
	}
	seedanceAssetAPISuccess(c, groups)
}

// CreateSeedanceAssetGroup 创建上游素材组并保存本地授权映射。
func CreateSeedanceAssetGroup(c *gin.Context) {
	var req seedanceAssetGroupCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		seedanceAssetAPIErrorI18n(c, "invalid_params")
		return
	}
	if req.GroupType != nil {
		seedanceAssetAPIErrorI18n(c, "invalid_params")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		seedanceAssetAPIErrorI18n(c, "invalid_params")
		return
	}
	var channel *model.Channel
	var err error
	if c.GetBool("seedance_compat_asset_api") {
		value, _ := c.Get("seedance_compat_channel")
		channel, _ = value.(*model.Channel)
		if channel == nil {
			seedanceAssetAPIError(c, errors.New("selected asset channel is unavailable"))
			return
		}
	} else {
		channel, err = selectSeedanceAssetGroupChannel(c, strings.TrimSpace(req.Model))
		if err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
	}
	client, err := service.NewSeedanceAssetClient(channel)
	if err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	var result struct {
		Result struct {
			ID string `json:"Id"`
		} `json:"Result"`
	}
	if err := client.CreateSeedanceAssetGroup(c.Request.Context(), req.Name, "AIGC", &result); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	group := &model.SeedanceAssetGroup{
		UserID: c.GetInt("id"), ChannelID: channel.Id, GroupID: result.Result.ID,
		Name: strings.TrimSpace(req.Name), GroupType: "AIGC",
		KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint,
		AssetCount: new(int64),
	}
	if c.GetBool("seedance_compat_asset_api") {
		group.BindModel = c.GetString("seedance_compat_bind_model")
		if group.BindModel == "" {
			group.BindModel = strings.TrimSpace(req.Model)
		}
		group.CompatType = c.GetString("seedance_compat_group_type")
	}
	if req.Description != nil {
		group.Description = strings.TrimSpace(*req.Description)
	}
	if group.GroupID == "" {
		seedanceAssetAPIErrorI18n(c, "invalid_params")
		return
	}
	if err := model.DB.Create(group).Error; err != nil {
		cleanupSeedanceAssetGroupAfterFailure(c.Request.Context(), client, group.GroupID)
		seedanceAssetAPIError(c, err)
		return
	}
	seedanceAssetAPISuccess(c, group)
}

// ListSeedanceAssets 返回当前用户素材，GroupID 为空时返回全部素材。
func ListSeedanceAssets(c *gin.Context) {
	options, err := parseSeedanceLibraryQuery(c, false)
	if err != nil {
		seedanceAssetAPIResponse(c, http.StatusBadRequest, gin.H{"success": false, "message": err.Error()})
		return
	}
	query := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).Where("user_id = ?", c.GetInt("id"))
	if c.GetBool("seedance_compat_ark_action") {
		groups := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetGroup{}).
			Select("group_id").Where("user_id = ? AND status <> ?", c.GetInt("id"), "Deleting")
		query = query.Where("status <> ? AND group_id IN (?)", "Deleting", groups)
	}
	query = options.scope(query)
	options.statusExpression, options.statusArguments, err = seedanceAssetStatusExpression(c, model.DB.NamingStrategy.TableName("SeedanceAsset"), false)
	if err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if err := scheduleMissingSeedanceAssetTargets(c); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	// 当前页即使只显示终态，也要在同组仍有审核任务时更新筛选结果。
	var pendingMarker struct {
		ID uint
	}
	pendingQuery := query.Session(&gorm.Session{})
	if options.statusExpression != "" {
		args := append(append([]any{}, options.statusArguments...), []string{"processing", "pending"})
		pendingQuery = pendingQuery.Where("("+options.statusExpression+") IN ?", args...)
	} else {
		pendingQuery = pendingQuery.Where("pending_status = ?", true)
	}
	if err := pendingQuery.Select("id").Limit(1).Find(&pendingMarker).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	// 全部筛选先于计数和分页，has_pending 保持组范围内的轮询语义。
	query = options.filter(query, false)
	var total int64
	usesGroupCount := !c.GetBool("seedance_compat_ark_action") && options.groupType == "" && len(options.groups) == 1 && len(options.ids) == 0 && len(options.assetIDs) == 0 &&
		len(options.statuses) == 0 && len(options.assetTypes) == 0 && options.search == "" &&
		options.after == nil && options.before == nil
	if usesGroupCount {
		var group model.SeedanceAssetGroup
		groupLookup := model.DB.WithContext(c.Request.Context()).Select("asset_count").
			Where("user_id = ? AND group_id = ?", c.GetInt("id"), options.groups[0]).Limit(1).Find(&group)
		if groupLookup.Error != nil {
			seedanceAssetAPIError(c, groupLookup.Error)
			return
		}
		if groupLookup.RowsAffected > 0 && group.AssetCount != nil {
			total = *group.AssetCount
		} else if err := query.Count(&total).Error; err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
	} else if err := query.Count(&total).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	page := options.page
	page.PageSize = max(1, min(100, page.PageSize))
	if c.GetString(seedanceAssetActionContextKey) == "" {
		page.Page = max(1, min(page.Page, int((total+int64(page.PageSize)-1)/int64(page.PageSize))))
	}
	var assets []model.SeedanceAsset
	offset := page.GetStartIdx()
	if c.GetBool("seedance_compat_ark_token_page") {
		offset = c.GetInt("seedance_compat_ark_offset")
	}
	if err := query.Order(options.order).Offset(offset).Limit(page.PageSize).Find(&assets).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if err := projectSeedanceAssetStatuses(c, assets, false); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	// 同一页通常共用一个 OSS 位置，复用客户端避免每个素材重复加载和校验配置。
	previewStorages := make(map[model.SeedanceAssetStorage]*service.PrivateAssetOSSStorage)
	previewStorageErrors := make(map[model.SeedanceAssetStorage]error)
	previewSignFailures := 0
	var firstPreviewSignFailure error
	var firstPreviewFailureAssetID uint
	for index := range assets {
		var storage *service.PrivateAssetOSSStorage
		var err error
		if strings.TrimSpace(assets[index].ObjectKey) != "" {
			location := assets[index].Storage
			if cached, ok := previewStorages[location]; ok {
				storage = cached
			} else if cachedErr, ok := previewStorageErrors[location]; ok {
				err = cachedErr
			} else {
				storage, err = service.NewPrivateAssetOSSStorage(location)
				if err != nil {
					previewStorageErrors[location] = err
				} else {
					previewStorages[location] = storage
				}
			}
		}
		var previewURL string
		if err == nil {
			previewURL, err = seedanceAssetPreviewURLWithStorage(c.Request.Context(), &assets[index], storage)
		}
		if err != nil {
			assets[index].PreviewURL = ""
			previewSignFailures++
			if firstPreviewSignFailure == nil {
				firstPreviewSignFailure = err
				firstPreviewFailureAssetID = assets[index].ID
			}
			continue
		}
		assets[index].PreviewURL = previewURL
	}
	if firstPreviewSignFailure != nil {
		common.SysError(fmt.Sprintf("failed to sign %d Seedance asset previews; first_asset_id=%d: %v", previewSignFailures, firstPreviewFailureAssetID, firstPreviewSignFailure))
	}
	seedanceAssetAPIResponse(c, http.StatusOK, gin.H{"success": true, "data": assets, "total": total, "page": page.Page, "page_size": page.PageSize, "has_pending": pendingMarker.ID > 0})
}

func refreshSeedanceAssetReplicas(c *gin.Context, asset model.SeedanceAsset, primaryErr error) {
	if err := service.RetrySeedanceAssetSynchronization(c.Request.Context(), asset.UserID, asset.ID, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if err := model.DB.WithContext(c.Request.Context()).Where("id = ? AND user_id = ? AND status <> ?", asset.ID, asset.UserID, "Deleting").First(&asset).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	assets := []model.SeedanceAsset{asset}
	if err := projectSeedanceAssetStatuses(c, assets, false); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if primaryErr != nil {
		accounts, err := service.SeedanceAssetStatusAccounts(c.Request.Context(), asset.UserID, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c))
		if err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
		var count int64
		if len(accounts) > 0 {
			if err := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetReplica{}).
				Where("user_id = ? AND local_asset_id = ? AND account_fingerprint IN ? AND upstream_deleted_at = 0", asset.UserID, asset.ID, accounts).Count(&count).Error; err != nil {
				seedanceAssetAPIError(c, err)
				return
			}
		}
		if count == 0 {
			seedanceAssetAPIError(c, primaryErr)
			return
		}
	}
	asset = assets[0]
	previewURL, err := seedanceAssetPreviewURL(c.Request.Context(), &asset)
	if err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	asset.PreviewURL = previewURL
	enqueueSeedanceAssetPolling()
	seedanceAssetAPISuccess(c, asset)
}

func retrySeedanceAssetAfterPrimaryError(c *gin.Context, asset model.SeedanceAsset, primaryErr error) bool {
	if asset.LocalOnly == nil || *asset.LocalOnly {
		return false
	}
	var group model.SeedanceAssetGroup
	if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND group_id = ? AND status <> ?", asset.UserID, asset.GroupID, "Deleting").First(&group).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return true
	}
	if strings.EqualFold(group.GroupType, "LivenessFace") {
		return false
	}
	refreshSeedanceAssetReplicas(c, asset, primaryErr)
	return true
}

// RefreshSeedanceAsset 同步查询上游审核状态并更新本地记录。
func RefreshSeedanceAsset(c *gin.Context) {
	var asset model.SeedanceAsset
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&asset).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if asset.Status == "Deleting" {
		seedanceAssetAPIErrorMsg(c, "asset is being deleted")
		return
	}
	if asset.IsReplicaManaged() {
		refreshSeedanceAssetReplicas(c, asset, nil)
		return
	}
	if asset.LocalOnly != nil && *asset.LocalOnly {
		previewURL, err := seedanceAssetPreviewURL(c.Request.Context(), &asset)
		if err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
		asset.PreviewURL = previewURL
		seedanceAssetAPISuccess(c, asset)
		return
	}
	loadedUpdatedAt := asset.UpdatedAt
	modelScoped := asset.LocalOnly != nil && !*asset.LocalOnly && asset.PrimaryUpstreamID != ""
	client, requestScope, err := seedanceAssetClientForRequest(c, asset.ChannelID, asset.KeyFingerprint, asset.AccountFingerprint, modelScoped)
	if err != nil {
		if retrySeedanceAssetAfterPrimaryError(c, asset, err) {
			return
		}
		seedanceAssetRequestScopeError(c, err)
		return
	}
	var details service.SeedanceAssetDetails
	if asset.LocalOnly != nil && !*asset.LocalOnly {
		details, err = client.GetCheckedSeedanceAssetDetails(c.Request.Context(), asset.UpstreamAssetID())
	} else {
		details, err = client.GetSeedanceAssetDetails(c.Request.Context(), asset.UpstreamAssetID())
	}
	if err != nil {
		if retrySeedanceAssetAfterPrimaryError(c, asset, err) {
			return
		}
		seedanceAssetAPIError(c, err)
		return
	}
	reviewAsset := asset
	if reviewAsset.FailureCode == model.SeedanceAssetReviewTimeoutCode {
		reviewAsset.PollAttempts = 0
	}
	updates := service.SeedanceAssetPrimaryReviewUpdates(&reviewAsset, details, common.GetTimestamp())
	if asset.PrimarySyncScope == "" {
		updates["primary_sync_scope"] = requestScope
	}
	// 审核状态不会触发 OSS 回收，对象只在用户主动删除素材时移除。
	_, err = model.UpdateSeedanceAssetIfUnchanged(asset.ID, loadedUpdatedAt, updates)
	if err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	// 成功更新和 CAS 冲突都返回数据库中的完整当前记录，禁止把旧状态重新写回前端缓存。
	if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND id = ?", c.GetInt("id"), asset.ID).First(&asset).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if asset.LocalOnly != nil && !*asset.LocalOnly {
		if err := service.RetrySeedanceAssetSynchronization(c.Request.Context(), asset.UserID, asset.ID, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
		enqueueSeedanceAssetPolling()
	}
	assets := []model.SeedanceAsset{asset}
	if err := projectSeedanceAssetStatuses(c, assets, false); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	asset = assets[0]
	asset.PreviewURL, err = seedanceAssetPreviewURL(c.Request.Context(), &asset)
	if err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	seedanceAssetAPISuccess(c, asset)
}

const seedanceAssetBatchDeleteLimit = 100

type seedanceAssetBatchDeleteRequest struct {
	IDs []uint `json:"ids"`
}

type seedanceAssetBatchDeleteResult struct {
	DeletedIDs []uint `json:"deleted_ids"`
	FailedIDs  []uint `json:"failed_ids"`
	PendingIDs []uint `json:"pending_ids"`
}

// DeleteSeedanceAsset 删除用户自己的素材；上游删除失败不阻塞本站删除。
func DeleteSeedanceAsset(c *gin.Context) {
	var asset model.SeedanceAsset
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&asset).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	jobs := []model.SeedanceAssetCleanupJob{service.NewSeedanceAssetDeleteCleanupJob(&asset)}
	if err := model.QueueSeedanceAssetDeletion(c.Request.Context(), jobs); err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	service.EnqueueSeedanceAssetCleanup()
	seedanceAssetAPIResponse(c, http.StatusAccepted, gin.H{"success": true, "data": nil})
}

// DeleteSeedanceAssetBatch 按当前用户归属创建后台删除任务，避免批量上游请求阻塞单次 HTTP 请求。
// 本地状态与任务入队使用同一事务；上游失败仍清理本站素材和 OSS。
func DeleteSeedanceAssetBatch(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req seedanceAssetBatchDeleteRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.IDs) == 0 || len(req.IDs) > seedanceAssetBatchDeleteLimit {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}

	ids := make([]uint, 0, len(req.IDs))
	seen := make(map[uint]struct{}, len(req.IDs))
	for _, id := range req.IDs {
		if id == 0 {
			common.ApiErrorI18n(c, "invalid_params")
			return
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	var assets []model.SeedanceAsset
	if err := model.DB.WithContext(c.Request.Context()).
		Where("user_id = ? AND id IN ?", c.GetInt("id"), ids).
		Find(&assets).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	assetsByID := make(map[uint]*model.SeedanceAsset, len(assets))
	for index := range assets {
		assetsByID[assets[index].ID] = &assets[index]
	}
	jobs := make([]model.SeedanceAssetCleanupJob, 0, len(assets))
	result := seedanceAssetBatchDeleteResult{
		DeletedIDs: make([]uint, 0),
		FailedIDs:  make([]uint, 0),
		PendingIDs: make([]uint, 0, len(assets)),
	}
	for _, id := range ids {
		asset, exists := assetsByID[id]
		if !exists {
			// 不返回不属于当前用户的 ID，避免批量接口泄露资源是否存在。
			continue
		}
		jobs = append(jobs, service.NewSeedanceAssetDeleteCleanupJob(asset))
		result.PendingIDs = append(result.PendingIDs, id)
	}
	if len(jobs) == 0 {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	if err := model.QueueSeedanceAssetDeletion(c.Request.Context(), jobs); err != nil {
		common.ApiError(c, err)
		return
	}
	service.EnqueueSeedanceAssetCleanup()
	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": fmt.Sprintf("%d assets queued for deletion", len(result.PendingIDs)),
		"data":    result,
	})
}

// UploadSeedanceAsset 接收用户拖拽的本地文件，转为上游可访问的临时 URL 后提交审核。
func UploadSeedanceAsset(c *gin.Context) {
	maxBodySize := service.SeedanceAssetMaxMultipartBodySize()
	if c.GetBool("seedance_compat_asset_api") {
		maxBodySize = service.SeedanceCompatMaxMultipartBodySize()
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize)
	if err := c.Request.ParseMultipartForm(2 << 20); err != nil {
		if common.IsRequestBodyTooLargeError(err) {
			common.ApiErrorMsg(c, "seedance asset upload is too large")
			return
		}
		common.ApiErrorMsg(c, "invalid seedance asset upload")
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}

	groupID := strings.TrimSpace(c.PostForm("group_id"))
	if groupID == "" {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	fileHeader, err := c.FormFile("file")
	if err != nil || fileHeader == nil {
		common.ApiErrorMsg(c, "seedance asset file is required")
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		name = filepath.Base(strings.TrimSpace(fileHeader.Filename))
	}
	if utf8.RuneCountInString(name) > 64 {
		common.ApiErrorMsg(c, "seedance asset name is too long")
		return
	}

	var group model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ? AND group_id = ?", c.GetInt("id"), groupID).First(&group).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if group.Status == "Deleting" {
		common.ApiErrorMsg(c, "asset group is being deleted")
		return
	}
	if err := model.CheckSeedanceAssetCapacity(c.Request.Context(), c.GetInt("id"), fileHeader.Size); err != nil {
		common.ApiError(c, err)
		return
	}
	var client *service.SeedanceAssetClient
	var primaryScope string
	if strings.EqualFold(group.GroupType, "LivenessFace") {
		client, primaryScope, err = seedanceAssetClientForRequest(c, group.ChannelID, group.KeyFingerprint, group.AccountFingerprint, false)
		if err != nil {
			seedanceAssetRequestScopeError(c, err)
			return
		}
		if err := service.EnsureSeedanceAssetGroupReady(c.Request.Context(), client, &group); err != nil {
			if errors.Is(err, service.ErrSeedanceAssetServiceTemporary) {
				seedanceAssetAPIError(c, err)
				return
			}
			seedanceAssetAPIResponse(c, http.StatusConflict, gin.H{"success": false, "message": err.Error()})
			return
		}
	}
	file, err := fileHeader.Open()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	defer file.Close()

	var upload *service.SeedanceAssetUpload
	if c.GetBool("seedance_compat_asset_api") {
		upload, err = service.StoreSeedanceCompatImageUpload(c.Request.Context(), file, fileHeader.Filename,
			fileHeader.Header.Get("Content-Type"), fileHeader.Size, c.GetInt("id"))
	} else {
		upload, err = service.StoreSeedanceAssetUpload(c.Request.Context(), file, fileHeader.Filename,
			fileHeader.Header.Get("Content-Type"), strings.TrimSpace(c.PostForm("asset_type")), fileHeader.Size, c.GetInt("id"))
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if client == nil {
		asset, err := createStoredSeedanceAsset(c, &group, upload, name, "")
		if err != nil {
			cleanupSeedanceAssetAfterFailure(c.Request.Context(), nil, "", upload.ObjectKey, upload)
			common.ApiError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"success": true, "data": asset})
		return
	}
	assetID, err := client.CreateSeedanceAsset(c.Request.Context(), group.GroupID, upload.URL, upload.AssetType, name)
	if err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, "", upload.ObjectKey, upload)
		common.ApiError(c, err)
		return
	}
	asset := &model.SeedanceAsset{
		UserID:             c.GetInt("id"),
		UploadBytes:        upload.Size,
		ChannelID:          group.ChannelID,
		GroupID:            group.GroupID,
		AssetID:            assetID,
		Name:               name,
		AssetType:          upload.AssetType,
		ObjectKey:          upload.ObjectKey,
		Storage:            upload.Storage,
		KeyFingerprint:     group.KeyFingerprint,
		AccountFingerprint: group.AccountFingerprint,
		PrimarySyncScope:   primaryScope,
		Status:             "Processing",
		NextPollAt:         common.GetTimestamp(),
	}
	previewURL, err := seedanceAssetPreviewURL(c.Request.Context(), asset)
	if err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey, upload)
		common.ApiError(c, err)
		return
	}
	if err := service.CreateSeedanceAssetWithSynchronization(c.Request.Context(), asset, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey, upload)
		common.ApiError(c, err)
		return
	}
	asset.PreviewURL = previewURL
	enqueueSeedanceAssetPolling()
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": asset})
}

// CreateSeedanceAsset 提交公网资源并保存异步审核任务。
func CreateSeedanceAsset(c *gin.Context) {
	var req seedanceAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		seedanceAssetAPIErrorI18n(c, "invalid_params")
		return
	}
	// 用户提供的地址交给上游抓取前必须执行现有 SSRF 策略，禁止任意协议和私网目标。
	if err := service.ValidateSeedanceAssetSourceURL(strings.TrimSpace(req.SourceURL)); err != nil {
		seedanceAssetAPIResponse(c, http.StatusBadRequest, gin.H{"success": false, "message": "invalid or disallowed asset source URL"})
		return
	}
	var group model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ? AND group_id = ?", c.GetInt("id"), req.GroupID).First(&group).Error; err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if group.Status == "Deleting" {
		seedanceAssetAPIErrorMsg(c, "asset group is being deleted")
		return
	}
	var client *service.SeedanceAssetClient
	var err error
	var primaryScope string
	if strings.EqualFold(group.GroupType, "LivenessFace") {
		client, primaryScope, err = seedanceAssetClientForRequest(c, group.ChannelID, group.KeyFingerprint, group.AccountFingerprint, false)
		if err != nil {
			seedanceAssetRequestScopeError(c, err)
			return
		}
		if err := service.EnsureSeedanceAssetGroupReady(c.Request.Context(), client, &group); err != nil {
			if errors.Is(err, service.ErrSeedanceAssetServiceTemporary) {
				seedanceAssetAPIError(c, err)
				return
			}
			seedanceAssetAPIResponse(c, http.StatusConflict, gin.H{"success": false, "message": err.Error()})
			return
		}
	}
	upload, err := service.StoreSeedanceAssetURL(c.Request.Context(), strings.TrimSpace(req.SourceURL), req.AssetType, c.GetInt("id"))
	if err != nil {
		seedanceAssetAPIError(c, err)
		return
	}
	if client == nil {
		asset, err := createStoredSeedanceAsset(c, &group, upload, strings.TrimSpace(req.Name), strings.TrimSpace(req.SourceURL))
		if err != nil {
			cleanupSeedanceAssetAfterFailure(c.Request.Context(), nil, "", upload.ObjectKey, upload)
			seedanceAssetAPIError(c, err)
			return
		}
		seedanceAssetAPIResponse(c, http.StatusAccepted, gin.H{"success": true, "data": asset})
		return
	}
	assetID, err := client.CreateSeedanceAsset(c.Request.Context(), group.GroupID, upload.URL, upload.AssetType, strings.TrimSpace(req.Name))
	if err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), nil, "", upload.ObjectKey, upload)
		seedanceAssetAPIError(c, err)
		return
	}
	asset := &model.SeedanceAsset{
		UserID: c.GetInt("id"), UploadBytes: upload.Size, ChannelID: group.ChannelID, GroupID: group.GroupID, AssetID: assetID,
		Name: strings.TrimSpace(req.Name), AssetType: req.AssetType, SourceURL: strings.TrimSpace(req.SourceURL), ObjectKey: upload.ObjectKey, Storage: upload.Storage,
		AccountFingerprint: group.AccountFingerprint, PrimarySyncScope: primaryScope, Status: "Processing", NextPollAt: common.GetTimestamp(),
	}
	asset.KeyFingerprint = group.KeyFingerprint
	previewURL, err := seedanceAssetPreviewURL(c.Request.Context(), asset)
	if err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey, upload)
		seedanceAssetAPIError(c, err)
		return
	}
	if err := service.CreateSeedanceAssetWithSynchronization(c.Request.Context(), asset, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey, upload)
		seedanceAssetAPIError(c, err)
		return
	}
	asset.PreviewURL = previewURL
	enqueueSeedanceAssetPolling()
	seedanceAssetAPIResponse(c, http.StatusAccepted, gin.H{"success": true, "data": asset})
}
