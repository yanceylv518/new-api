package controller

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func UploadSeedanceCompatAsset(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.SeedanceCompatMaxMultipartBodySize())
	if err := c.Request.ParseMultipartForm(2 << 20); err != nil {
		seedanceCompatError(c, http.StatusBadRequest, "invalid or oversized asset upload")
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	group, err := seedanceCompatGroup(c, c.PostForm("group_id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	fileHeader, err := c.FormFile("file")
	limits := common.GetManagementRateLimits().AssetLimits
	maxImageMiB := limits.CompatImageMiB
	if err != nil || fileHeader == nil || fileHeader.Size <= 0 || fileHeader.Size > maxImageMiB<<20 {
		seedanceCompatError(c, http.StatusBadRequest, fmt.Sprintf("a non-empty image of at most %d MB is required", maxImageMiB))
		return
	}
	extension := strings.ToLower(filepath.Ext(fileHeader.Filename))
	switch extension {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
	default:
		seedanceCompatError(c, http.StatusBadRequest, "unsupported image format")
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		name = filepath.Base(fileHeader.Filename)
	}
	if utf8.RuneCountInString(name) > 64 {
		seedanceCompatError(c, http.StatusBadRequest, "asset name is too long")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(c.PostForm("mode")))
	if mode != "" && mode != "link" && mode != "library" && mode != "real" {
		seedanceCompatError(c, http.StatusBadRequest, "mode must be link, library, or real")
		return
	}
	if mode == "" {
		mode = "link"
		if strings.EqualFold(group.GroupType, "LivenessFace") {
			mode = "real"
		}
	}
	file, err := fileHeader.Open()
	if err != nil {
		seedanceCompatError(c, http.StatusBadRequest, "unable to read image")
		return
	}
	defer file.Close()
	if err := service.ValidateSeedanceCompatImage(c.Request.Context(), file); err != nil {
		seedanceCompatError(c, http.StatusBadRequest, "invalid image")
		return
	}
	if mode == "real" && !strings.EqualFold(group.GroupType, "LivenessFace") {
		seedanceCompatError(c, http.StatusBadRequest, "real mode requires a verified real-person group")
		return
	}
	if mode == "link" && strings.EqualFold(group.GroupType, "LivenessFace") {
		seedanceCompatError(c, http.StatusBadRequest, "verified real-person groups require real mode")
		return
	}
	if mode != "link" {
		c.Request.MultipartForm.Value["group_id"] = []string{group.GroupID}
		c.Set("seedance_compat_asset_api", true)
		result, status, err := seedanceCompatCall(c, UploadSeedanceAsset)
		if seedanceCompatResultError(c, result, status, err) {
			return
		}
		var asset model.SeedanceAsset
		if err := common.Unmarshal(result.Data, &asset); err != nil {
			seedanceCompatError(c, http.StatusInternalServerError, "asset response is invalid")
			return
		}
		c.JSON(http.StatusOK, seedanceCompatDataResponse{Data: seedanceCompatAssetGroupView(asset)})
		return
	}
	if err := model.CheckSeedanceAssetCapacity(c.Request.Context(), c.GetInt("id"), fileHeader.Size); err != nil {
		seedanceCompatError(c, http.StatusBadRequest, err.Error())
		return
	}
	upload, err := service.StoreSeedanceCompatImageUpload(c.Request.Context(), file, fileHeader.Filename, fileHeader.Header.Get("Content-Type"), fileHeader.Size, c.GetInt("id"))
	if err != nil {
		seedanceCompatError(c, http.StatusBadGateway, "asset upload failed")
		return
	}
	localOnly := true
	asset := &model.SeedanceAsset{
		UserID: c.GetInt("id"), UploadBytes: upload.Size, ChannelID: group.ChannelID,
		GroupID: group.GroupID, Name: name, AssetType: "Image", LocalOnly: &localOnly,
		ObjectKey: upload.ObjectKey, Storage: upload.Storage, KeyFingerprint: group.KeyFingerprint,
		AccountFingerprint: group.AccountFingerprint, Status: "Active",
	}
	for attempt := 0; attempt < 5; attempt++ {
		var suffix string
		suffix, err = common.GenerateRandomCharsKey(5)
		if err != nil {
			break
		}
		asset.AssetID = fmt.Sprintf("asset-%s-%s", time.Now().In(seedanceCompatTimeZone).Format("20060102150405"), strings.ToLower(suffix))
		asset.ID = 0
		err = model.CreateSeedanceAssetInGroup(c.Request.Context(), asset)
		if err == nil {
			break
		}
		var count int64
		if checkErr := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).Where("asset_id = ?", asset.AssetID).Count(&count).Error; checkErr != nil || count == 0 {
			break
		}
	}
	if err != nil {
		cleanupSeedanceAssetAfterFailure(c.Request.Context(), nil, "", upload.ObjectKey, upload)
		seedanceCompatError(c, http.StatusBadGateway, "asset upload failed")
		return
	}
	c.JSON(http.StatusOK, seedanceCompatDataResponse{Data: seedanceCompatAssetGroupView(*asset)})
}
