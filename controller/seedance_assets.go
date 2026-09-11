package controller

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type seedanceAssetGroupRequest struct {
	Name string `json:"name" binding:"required,max=64"`
}

const seedanceAssetGroupCleanupWorkerSize = 4

// cleanupSeedanceAssetGroupAssets 用固定并发清理组内 OSS 对象，减少大素材组删除的总耗时。
// 每条素材仍独立返回错误，已完成的条目不会因为同批其他条目失败而重复处理。
func cleanupSeedanceAssetGroupAssets(ctx context.Context, assets []model.SeedanceAsset) error {
	if len(assets) == 0 {
		return nil
	}
	jobs := make(chan *model.SeedanceAsset, len(assets))
	results := make(chan error, len(assets))
	for index := range assets {
		jobs <- &assets[index]
	}
	close(jobs)
	workerSize := min(seedanceAssetGroupCleanupWorkerSize, len(assets))
	var workers sync.WaitGroup
	workers.Add(workerSize)
	for worker := 0; worker < workerSize; worker++ {
		go func() {
			defer workers.Done()
			for asset := range jobs {
				if err := cleanupSeedanceAssetUpload(ctx, asset); err != nil {
					results <- err
					continue
				}
				results <- model.DB.WithContext(ctx).Delete(asset).Error
			}
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			return err
		}
	}
	return nil
}

// UpdateSeedanceAssetGroup 更新当前用户自己的素材组名称。
func UpdateSeedanceAssetGroup(c *gin.Context) {
	var req seedanceAssetGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	var group model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&group).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if group.Status == "Deleting" {
		common.ApiErrorMsg(c, "asset group is being deleted")
		return
	}
	duplicated, err := model.IsSeedanceAssetGroupNameDuplicated(group.ID, c.GetInt("id"), req.Name)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if duplicated {
		common.ApiErrorI18n(c, i18n.MsgGroupNameExists)
		return
	}
	channel, err := model.GetChannelById(group.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	client, err := service.NewSeedanceAssetClient(channel, group.KeyFingerprint)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	var response struct{}
	if err := client.UpdateSeedanceAssetGroup(c.Request.Context(), group.GroupID, strings.TrimSpace(req.Name), &response); err != nil {
		common.ApiError(c, err)
		return
	}
	previousName := group.Name
	group.Name = strings.TrimSpace(req.Name)
	if err := model.DB.Model(&group).Updates(map[string]any{
		"name":     group.Name,
		"name_key": model.NormalizeSeedanceAssetGroupName(group.Name),
	}).Error; err != nil {
		if rollbackErr := service.RollbackSeedanceAssetGroupName(c.Request.Context(), client, group.GroupID, previousName); rollbackErr != nil {
			common.SysError("failed to roll back Seedance asset group rename: " + rollbackErr.Error())
		}
		if duplicate, duplicateErr := model.IsSeedanceAssetGroupNameDuplicated(group.ID, c.GetInt("id"), group.Name); duplicateErr == nil && duplicate {
			common.ApiErrorI18n(c, i18n.MsgGroupNameExists)
			return
		}
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, group)
}

// DeleteSeedanceAssetGroup 删除当前用户自己的素材组、OSS 对象及本地授权映射。
func DeleteSeedanceAssetGroup(c *gin.Context) {
	var group model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&group).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	// 先持久化删除意图，最终入库与此更新争用同一分组锁，阻止清理期间新增映射。
	if err := model.DB.WithContext(c.Request.Context()).Model(&group).Update("status", "Deleting").Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).
		Where("user_id = ? AND group_id = ?", group.UserID, group.GroupID).
		Updates(map[string]any{"status": "Deleting", "poll_lease_until": 0}).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if group.UpstreamDeletedAt == 0 {
		channel, err := model.GetChannelById(group.ChannelID, true)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		client, err := service.NewSeedanceAssetClient(channel, group.KeyFingerprint)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		if err := client.DeleteSeedanceAssetGroup(c.Request.Context(), group.GroupID); err != nil {
			common.ApiError(c, err)
			return
		}
		if err := model.DB.WithContext(c.Request.Context()).Model(&group).Update("upstream_deleted_at", common.GetTimestamp()).Error; err != nil {
			common.ApiError(c, err)
			return
		}
	}
	// 上游确认后按批清理；每个已完成对象单独移除映射，重试只处理剩余部分。
	for {
		var assets []model.SeedanceAsset
		if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND group_id = ?", group.UserID, group.GroupID).Limit(100).Find(&assets).Error; err != nil {
			common.ApiError(c, err)
			return
		}
		if len(assets) == 0 {
			break
		}
		if err := cleanupSeedanceAssetGroupAssets(c.Request.Context(), assets); err != nil {
			common.ApiError(c, err)
			return
		}
	}
	if err := model.DB.WithContext(c.Request.Context()).Delete(&group).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

type seedanceAssetRequest struct {
	GroupID   string `json:"group_id" binding:"required"`
	SourceURL string `json:"source_url" binding:"required,url"`
	AssetType string `json:"asset_type" binding:"required,oneof=Image Video Audio"`
	Name      string `json:"name" binding:"max=64"`
}

// cleanupSeedanceAssetUpload 只回收用户主动删除的 OSS 对象。
func cleanupSeedanceAssetUpload(ctx context.Context, asset *model.SeedanceAsset) error {
	return service.RemoveSeedanceAssetObject(ctx, asset.ObjectKey, asset.Storage)
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

// ListSeedanceAssetGroups 只返回当前用户自己的素材组。
func ListSeedanceAssetGroups(c *gin.Context) {
	var groups []model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ?", c.GetInt("id")).Order("id desc").Find(&groups).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, groups)
}

// CreateSeedanceAssetGroup 创建上游素材组并保存本地授权映射。
func CreateSeedanceAssetGroup(c *gin.Context) {
	var req seedanceAssetGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	duplicated, err := model.IsSeedanceAssetGroupNameDuplicated(0, c.GetInt("id"), req.Name)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if duplicated {
		common.ApiErrorI18n(c, i18n.MsgGroupNameExists)
		return
	}
	channel, err := service.FindSeedanceAssetChannel()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	client, err := service.NewSeedanceAssetClient(channel)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	var result struct {
		Result struct {
			ID string `json:"Id"`
		} `json:"Result"`
	}
	if err := client.CreateSeedanceAssetGroup(c.Request.Context(), req.Name, &result); err != nil {
		common.ApiError(c, err)
		return
	}
	group := &model.SeedanceAssetGroup{UserID: c.GetInt("id"), ChannelID: channel.Id, GroupID: result.Result.ID, Name: strings.TrimSpace(req.Name), KeyFingerprint: client.KeyFingerprint}
	if group.GroupID == "" {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	if err := model.DB.Create(group).Error; err != nil {
		cleanupSeedanceAssetGroupAfterFailure(c.Request.Context(), client, group.GroupID)
		if duplicate, duplicateErr := model.IsSeedanceAssetGroupNameDuplicated(0, c.GetInt("id"), group.Name); duplicateErr == nil && duplicate {
			common.ApiErrorI18n(c, i18n.MsgGroupNameExists)
			return
		}
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, group)
}

// ListSeedanceAssets 返回当前用户素材，GroupID 为空时返回全部素材。
func ListSeedanceAssets(c *gin.Context) {
	query := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).Where("user_id = ?", c.GetInt("id"))
	if groupID := strings.TrimSpace(c.Query("group_id")); groupID != "" {
		query = query.Where("group_id = ?", groupID)
	}
	// 当前页即使只显示终态，也要在同组仍有审核任务时更新筛选结果。
	var pendingMarker struct {
		ID uint
	}
	if err := query.Session(&gorm.Session{}).Where("LOWER(status) IN ?", []string{"processing", "pending", "deleting"}).Select("id").Limit(1).Find(&pendingMarker).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	// 搜索和筛选在分页前执行，旧素材不会因落在其他页而无法检索。
	if keyword := strings.TrimSpace(c.Query("search")); keyword != "" {
		if utf8.RuneCountInString(keyword) > 128 {
			common.ApiErrorI18n(c, "invalid_params")
			return
		}
		keyword = "%" + strings.NewReplacer("#", "##", "%", "#%", "_", "#_").Replace(strings.ToLower(keyword)) + "%"
		query = query.Where("(LOWER(name) LIKE ? ESCAPE '#' OR LOWER(asset_id) LIKE ? ESCAPE '#')", keyword, keyword)
	}
	if assetType := strings.ToLower(c.Query("asset_type")); assetType != "" && assetType != "all" {
		if assetType != "image" && assetType != "video" && assetType != "audio" {
			common.ApiErrorI18n(c, "invalid_params")
			return
		}
		query = query.Where("LOWER(asset_type) = ?", assetType)
	}
	switch strings.ToLower(c.Query("status")) {
	case "", "all":
	case "processing":
		query = query.Where("LOWER(status) IN ?", []string{"processing", "pending"})
	case "deleting":
		query = query.Where("LOWER(status) = ?", "deleting")
	case "active":
		query = query.Where("LOWER(status) IN ?", []string{"active", "success", "succeeded"})
	case "failed":
		query = query.Where("LOWER(status) = ?", "failed")
	default:
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	page := common.GetPageQuery(c)
	page.PageSize = max(1, min(100, page.PageSize))
	page.Page = max(1, min(page.Page, int((total+int64(page.PageSize)-1)/int64(page.PageSize))))
	var assets []model.SeedanceAsset
	if err := query.Order("id desc").Offset(page.GetStartIdx()).Limit(page.PageSize).Find(&assets).Error; err != nil {
		common.ApiError(c, err)
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
	c.JSON(http.StatusOK, gin.H{"success": true, "data": assets, "total": total, "page": page.Page, "page_size": page.PageSize, "has_pending": pendingMarker.ID > 0})
}

// RefreshSeedanceAsset 同步查询上游审核状态并更新本地记录。
func RefreshSeedanceAsset(c *gin.Context) {
	var asset model.SeedanceAsset
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&asset).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if asset.Status == "Deleting" {
		common.ApiErrorMsg(c, "asset is being deleted")
		return
	}
	channel, err := model.GetChannelById(asset.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	loadedUpdatedAt := asset.UpdatedAt
	client, err := service.NewSeedanceAssetClient(channel, asset.KeyFingerprint)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	status, previewURL, err := client.GetSeedanceAsset(c.Request.Context(), asset.AssetID)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	attempts, nextPollAt := service.SeedanceAssetPollSchedule(status, asset.PollAttempts, common.GetTimestamp())
	updates := map[string]any{"status": status}
	updates["poll_attempts"] = attempts
	updates["next_poll_at"] = nextPollAt
	updates["poll_lease_until"] = int64(0)
	if previewURL != "" {
		asset.PreviewURL = previewURL
		updates["preview_url"] = previewURL
	}
	// 审核状态不会触发 OSS 回收，对象只在用户主动删除素材时移除。
	_, err = model.UpdateSeedanceAssetIfUnchanged(asset.ID, loadedUpdatedAt, updates)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	// 成功更新和 CAS 冲突都返回数据库中的完整当前记录，禁止把旧状态重新写回前端缓存。
	if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND id = ?", c.GetInt("id"), asset.ID).First(&asset).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	asset.PreviewURL, err = seedanceAssetPreviewURL(c.Request.Context(), &asset)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, asset)
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

// deleteSeedanceAssetRecord 删除单条素材的本地映射、OSS 对象和上游引用。
// 删除过程允许在上游或本地清理失败后重试，已确认的上游删除不会重复调用。
func deleteSeedanceAssetRecord(ctx context.Context, asset *model.SeedanceAsset) error {
	// 删除意图使后台轮询停止；上游确认之前保留 OSS 文件与授权映射。
	if err := model.DB.WithContext(ctx).Model(asset).Updates(map[string]any{"status": "Deleting", "poll_lease_until": 0}).Error; err != nil {
		return err
	}
	if asset.UpstreamDeletedAt == 0 {
		channel, err := model.GetChannelById(asset.ChannelID, true)
		if err != nil {
			return err
		}
		client, err := service.NewSeedanceAssetClient(channel, asset.KeyFingerprint)
		if err != nil {
			return err
		}
		if err := client.DeleteSeedanceAsset(ctx, asset.AssetID); err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Model(asset).Update("upstream_deleted_at", common.GetTimestamp()).Error; err != nil {
			return err
		}
	}
	if err := cleanupSeedanceAssetUpload(ctx, asset); err != nil {
		return err
	}
	if err := model.DB.WithContext(ctx).Delete(asset).Error; err != nil {
		return err
	}
	return nil
}

// DeleteSeedanceAsset 删除用户自己的素材及上游引用。
func DeleteSeedanceAsset(c *gin.Context) {
	var asset model.SeedanceAsset
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&asset).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if strings.EqualFold(asset.Status, "deleting") {
		// 失败后的删除必须保留人工重试入口，并与批量删除共享同一幂等任务。
		job := service.NewSeedanceAssetDeleteCleanupJob(&asset)
		if err := model.QueueSeedanceAssetDeletion(c.Request.Context(), []model.SeedanceAssetCleanupJob{job}); err != nil {
			common.ApiError(c, err)
			return
		}
		service.EnqueueSeedanceAssetCleanup()
		c.JSON(http.StatusAccepted, gin.H{"success": true, "data": nil})
		return
	}
	if err := deleteSeedanceAssetRecord(c.Request.Context(), &asset); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// DeleteSeedanceAssetBatch 按当前用户归属创建后台删除任务，避免批量上游请求阻塞单次 HTTP 请求。
// 本地状态与任务入队使用同一事务，后台任务完成外部清理后才删除素材映射。
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
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.SeedanceAssetMaxMultipartBodySize())
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
	channel, err := model.GetChannelById(group.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	client, err := service.NewSeedanceAssetClient(channel, group.KeyFingerprint)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		common.ApiError(c, err)
		return
	}
	defer file.Close()

	upload, err := service.StoreSeedanceAssetUpload(
		c.Request.Context(),
		file,
		fileHeader.Filename,
		fileHeader.Header.Get("Content-Type"),
		strings.TrimSpace(c.PostForm("asset_type")),
		fileHeader.Size,
		c.GetInt("id"),
	)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	assetID, err := client.CreateSeedanceAsset(c.Request.Context(), group.GroupID, upload.URL, upload.AssetType, name)
	if err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, "", upload.ObjectKey, upload)
		common.ApiError(c, err)
		return
	}
	asset := &model.SeedanceAsset{
		UserID:         c.GetInt("id"),
		ChannelID:      group.ChannelID,
		GroupID:        group.GroupID,
		AssetID:        assetID,
		Name:           name,
		AssetType:      upload.AssetType,
		ObjectKey:      upload.ObjectKey,
		Storage:        upload.Storage,
		KeyFingerprint: group.KeyFingerprint,
		Status:         "Processing",
		NextPollAt:     common.GetTimestamp(),
	}
	if err := model.CreateSeedanceAssetInGroup(c.Request.Context(), asset); err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey, upload)
		common.ApiError(c, err)
		return
	}
	asset.PreviewURL, err = seedanceAssetPreviewURL(c.Request.Context(), asset)
	if err != nil {
		if deleteErr := model.DB.Delete(asset).Error; deleteErr != nil {
			common.SysError("failed to remove Seedance asset after preview signing failure: " + deleteErr.Error())
			common.ApiError(c, err)
			return
		}
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey, upload)
		common.ApiError(c, err)
		return
	}
	enqueueSeedanceAssetPolling()
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": asset})
}

// CreateSeedanceAsset 提交公网资源并保存异步审核任务。
func CreateSeedanceAsset(c *gin.Context) {
	var req seedanceAssetRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorI18n(c, "invalid_params")
		return
	}
	// 用户提供的地址交给上游抓取前必须执行现有 SSRF 策略，禁止任意协议和私网目标。
	if err := service.ValidateSeedanceAssetSourceURL(strings.TrimSpace(req.SourceURL)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid or disallowed asset source URL"})
		return
	}
	var group model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ? AND group_id = ?", c.GetInt("id"), req.GroupID).First(&group).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	if group.Status == "Deleting" {
		common.ApiErrorMsg(c, "asset group is being deleted")
		return
	}
	channel, err := model.GetChannelById(group.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	client, err := service.NewSeedanceAssetClient(channel, group.KeyFingerprint)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	assetID, err := client.CreateSeedanceAsset(c.Request.Context(), group.GroupID, strings.TrimSpace(req.SourceURL), req.AssetType, strings.TrimSpace(req.Name))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	asset := &model.SeedanceAsset{UserID: c.GetInt("id"), ChannelID: group.ChannelID, GroupID: group.GroupID, AssetID: assetID, Name: strings.TrimSpace(req.Name), AssetType: req.AssetType, SourceURL: strings.TrimSpace(req.SourceURL), Status: "Processing", NextPollAt: common.GetTimestamp()}
	asset.KeyFingerprint = group.KeyFingerprint
	if err := model.CreateSeedanceAssetInGroup(c.Request.Context(), asset); err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, "")
		common.ApiError(c, err)
		return
	}
	asset.PreviewURL, err = seedanceAssetPreviewURL(c.Request.Context(), asset)
	if err != nil {
		if deleteErr := model.DB.Delete(asset).Error; deleteErr != nil {
			common.SysError("failed to remove Seedance asset after preview signing failure: " + deleteErr.Error())
			common.ApiError(c, err)
			return
		}
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, "")
		common.ApiError(c, err)
		return
	}
	enqueueSeedanceAssetPolling()
	c.JSON(http.StatusAccepted, gin.H{"success": true, "data": asset})
}
