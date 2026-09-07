package controller

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
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
	client := service.NewSeedanceAssetClient(channel)
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
	var assets []model.SeedanceAsset
	if err := model.DB.Where("user_id = ? AND group_id = ?", c.GetInt("id"), group.GroupID).Find(&assets).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	// 用户已确认删除素材组，先回收持久对象；失败时保留数据库记录以便重试。
	for index := range assets {
		if err := cleanupSeedanceAssetUpload(c.Request.Context(), &assets[index]); err != nil {
			common.ApiError(c, err)
			return
		}
	}
	channel, err := model.GetChannelById(group.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err := service.NewSeedanceAssetClient(channel).DeleteSeedanceAssetGroup(c.Request.Context(), group.GroupID); err != nil {
		common.ApiError(c, err)
		return
	}
	// 远端删除成功后，本地素材和素材组必须原子移除，避免留下半删除映射。
	if err := model.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND group_id = ?", c.GetInt("id"), group.GroupID).Delete(&model.SeedanceAsset{}).Error; err != nil {
			return err
		}
		return tx.Delete(&group).Error
	}); err != nil {
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
	return service.RemoveSeedanceAssetObject(ctx, asset.ObjectKey)
}

// cleanupSeedanceAssetAfterFailure 在请求取消后仍回收上游素材和 OSS 对象。
func cleanupSeedanceAssetAfterFailure(ctx context.Context, client *service.SeedanceAssetClient, assetID, objectKey string) {
	if err := service.RollbackSeedanceAsset(ctx, client, assetID, objectKey); err != nil {
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
	if strings.TrimSpace(asset.ObjectKey) != "" {
		return service.SeedanceAssetOSSPreviewURL(ctx, asset.ObjectKey)
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
	client := service.NewSeedanceAssetClient(channel)
	var result struct {
		Result struct {
			ID string `json:"Id"`
		} `json:"Result"`
	}
	if err := client.CreateSeedanceAssetGroup(c.Request.Context(), req.Name, &result); err != nil {
		common.ApiError(c, err)
		return
	}
	group := &model.SeedanceAssetGroup{UserID: c.GetInt("id"), ChannelID: channel.Id, GroupID: result.Result.ID, Name: strings.TrimSpace(req.Name)}
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
	query := model.DB.Where("user_id = ?", c.GetInt("id"))
	if groupID := strings.TrimSpace(c.Query("group_id")); groupID != "" {
		query = query.Where("group_id = ?", groupID)
	}
	var assets []model.SeedanceAsset
	if err := query.Order("id desc").Limit(200).Find(&assets).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	previewSignFailures := 0
	var firstPreviewSignFailure error
	var firstPreviewFailureAssetID uint
	for index := range assets {
		previewURL, err := seedanceAssetPreviewURL(c.Request.Context(), &assets[index])
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
	common.ApiSuccess(c, assets)
}

// RefreshSeedanceAsset 同步查询上游审核状态并更新本地记录。
func RefreshSeedanceAsset(c *gin.Context) {
	var asset model.SeedanceAsset
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&asset).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	channel, err := model.GetChannelById(asset.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	loadedUpdatedAt := asset.UpdatedAt
	status, previewURL, err := service.NewSeedanceAssetClient(channel).GetSeedanceAsset(c.Request.Context(), asset.AssetID)
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
	updated, err := model.UpdateSeedanceAssetIfUnchanged(asset.ID, loadedUpdatedAt, updates)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !updated {
		if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), asset.ID).First(&asset).Error; err != nil {
			common.ApiError(c, err)
			return
		}
	}
	asset.PreviewURL, err = seedanceAssetPreviewURL(c.Request.Context(), &asset)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, asset)
}

// DeleteSeedanceAsset 删除用户自己的素材及上游引用。
func DeleteSeedanceAsset(c *gin.Context) {
	var asset model.SeedanceAsset
	if err := model.DB.Where("user_id = ? AND id = ?", c.GetInt("id"), c.Param("id")).First(&asset).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	// 删除失败时保留本地映射，用户可再次触发清理。
	if err := cleanupSeedanceAssetUpload(c.Request.Context(), &asset); err != nil {
		common.ApiError(c, err)
		return
	}
	channel, err := model.GetChannelById(asset.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if err := service.NewSeedanceAssetClient(channel).DeleteSeedanceAsset(c.Request.Context(), asset.AssetID); err != nil {
		common.ApiError(c, err)
		return
	}
	if err := model.DB.Delete(&asset).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
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
	channel, err := model.GetChannelById(group.ChannelID, true)
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
	client := service.NewSeedanceAssetClient(channel)
	assetID, err := client.CreateSeedanceAsset(c.Request.Context(), group.GroupID, upload.URL, upload.AssetType, name)
	if err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, "", upload.ObjectKey)
		common.ApiError(c, err)
		return
	}
	asset := &model.SeedanceAsset{
		UserID:     c.GetInt("id"),
		ChannelID:  group.ChannelID,
		GroupID:    group.GroupID,
		AssetID:    assetID,
		Name:       name,
		AssetType:  upload.AssetType,
		ObjectKey:  upload.ObjectKey,
		Status:     "Processing",
		NextPollAt: common.GetTimestamp(),
	}
	if err := model.DB.Create(asset).Error; err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey)
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
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), client, assetID, upload.ObjectKey)
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
	var group model.SeedanceAssetGroup
	if err := model.DB.Where("user_id = ? AND group_id = ?", c.GetInt("id"), req.GroupID).First(&group).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	channel, err := model.GetChannelById(group.ChannelID, true)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	client := service.NewSeedanceAssetClient(channel)
	assetID, err := client.CreateSeedanceAsset(c.Request.Context(), group.GroupID, strings.TrimSpace(req.SourceURL), req.AssetType, strings.TrimSpace(req.Name))
	if err != nil {
		common.ApiError(c, err)
		return
	}
	asset := &model.SeedanceAsset{UserID: c.GetInt("id"), ChannelID: group.ChannelID, GroupID: group.GroupID, AssetID: assetID, Name: strings.TrimSpace(req.Name), AssetType: req.AssetType, SourceURL: strings.TrimSpace(req.SourceURL), Status: "Processing", NextPollAt: common.GetTimestamp()}
	if err := model.DB.Create(asset).Error; err != nil {
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
