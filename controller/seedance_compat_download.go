package controller

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/hex"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
)

const seedanceCompatPublicTTL = 2 * time.Hour

func SeedanceCompatAssetPublicURL(c *gin.Context) {
	asset, err := seedanceCompatAsset(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	base := strings.TrimSpace(system_setting.TaskPublicAddress)
	if base == "" {
		base = strings.TrimSpace(system_setting.ServerAddress)
	}
	if err := service.ValidateTaskArtifactBaseURL(base); err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "public address is not configured")
		return
	}
	parsed, err := url.Parse(base)
	if err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "public address is not configured")
		return
	}
	expires := time.Now().Add(seedanceCompatPublicTTL).Unix()
	access, err := service.IssueTaskArtifactAccess(asset.AssetID, "asset-public:"+strconv.FormatInt(expires, 10))
	if err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "public link is unavailable")
		return
	}
	digest, err := base64.RawURLEncoding.Strict().DecodeString(access)
	if err != nil || len(digest) != 32 {
		seedanceCompatError(c, http.StatusServiceUnavailable, "public link is unavailable")
		return
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/doubao/api/a/" + asset.AssetID
	parsed.RawPath = ""
	parsed.RawQuery = url.Values{"e": {strconv.FormatInt(expires, 10)}, "s": {hex.EncodeToString(digest[:16])}}.Encode()
	c.JSON(http.StatusOK, seedanceCompatSuccessResponse{Success: true, Data: struct {
		URL       string `json:"url"`
		ExpiresIn int    `json:"expires_in"`
	}{URL: parsed.String(), ExpiresIn: int(seedanceCompatPublicTTL.Seconds())}})
}

func DownloadSeedanceCompatAsset(c *gin.Context) {
	asset, err := seedanceCompatAsset(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	streamSeedanceCompatAsset(c, asset)
}

func DownloadSeedanceCompatPublicAsset(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	expires, err := strconv.ParseInt(c.Query("e"), 10, 64)
	valid := false
	if err == nil && expires > time.Now().Unix() && expires <= time.Now().Add(seedanceCompatPublicTTL).Unix() {
		purpose := "asset-public:" + strconv.FormatInt(expires, 10)
		if c.FullPath() == "/doubao/api/a/:id" {
			values := c.Request.URL.Query()
			provided, decodeErr := hex.DecodeString(c.Query("s"))
			access, accessErr := service.IssueTaskArtifactAccess(id, purpose)
			expected, expectedErr := base64.RawURLEncoding.Strict().DecodeString(access)
			valid = len(values["e"]) == 1 && len(values["s"]) == 1 && decodeErr == nil && accessErr == nil &&
				expectedErr == nil && len(provided) == 16 && len(expected) == 32 && hmac.Equal(provided, expected[:16])
		} else {
			valid = service.VerifyTaskArtifactAccess(c.Query("access"), id, purpose)
		}
	}
	if !valid {
		seedanceCompatError(c, http.StatusNotFound, "asset resource not found")
		return
	}
	var asset model.SeedanceAsset
	if err := model.DB.WithContext(c.Request.Context()).Where("asset_id = ? AND status <> ?", id, "Deleting").First(&asset).Error; err != nil {
		seedanceCompatError(c, http.StatusNotFound, "asset resource not found")
		return
	}
	var group model.SeedanceAssetGroup
	if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND group_id = ? AND status <> ?", asset.UserID, asset.GroupID, "Deleting").First(&group).Error; err != nil {
		seedanceCompatError(c, http.StatusNotFound, "asset resource not found")
		return
	}
	streamSeedanceCompatAsset(c, &asset)
}

func seedanceCompatDownloadLimit(ctx context.Context, asset *model.SeedanceAsset, contentLength int64) (int64, error) {
	limits := common.GetManagementRateLimits().AssetLimits
	const previousLimit int64 = 200 << 20
	limit := previousLimit
	switch strings.ToLower(asset.AssetType) {
	case "image":
		limit = max(limit, limits.ImageMiB<<20, limits.CompatImageMiB<<20)
	case "video":
		limit = max(limit, limits.VideoMiB<<20)
	case "audio":
		limit = max(limit, limits.AudioMiB<<20)
	default:
		limit = max(limit, limits.ImageMiB<<20, limits.CompatImageMiB<<20, limits.VideoMiB<<20, limits.AudioMiB<<20)
	}
	if asset.ObjectKey == "" || asset.ID == 0 || contentLength >= 0 && contentLength <= limit {
		return limit, nil
	}
	var stored model.SeedanceAssetSize
	result := model.DB.WithContext(ctx).Where("asset_id = ? AND user_id = ?", asset.ID, asset.UserID).Limit(1).Find(&stored)
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected > 0 && stored.Bytes > 0 {
		limit = max(limit, min(stored.Bytes, int64(math.MaxInt64-1)))
	}
	return limit, nil
}

func streamSeedanceCompatAsset(c *gin.Context, asset *model.SeedanceAsset) {
	var source string
	var err error
	if asset.ObjectKey != "" {
		source, err = service.SeedanceAssetOSSPreviewURL(c.Request.Context(), asset.ObjectKey, asset.Storage)
	} else if asset.SourceURL != "" {
		source = asset.SourceURL
	} else {
		source = asset.PreviewURL
	}
	if err != nil || strings.TrimSpace(source) == "" {
		seedanceCompatError(c, http.StatusNotFound, "asset download is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		seedanceCompatError(c, http.StatusBadGateway, "asset download failed")
		return
	}
	response, err := service.GetSSRFProtectedHTTPClient().Do(request)
	if err != nil {
		seedanceCompatError(c, http.StatusBadGateway, "asset download failed")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		seedanceCompatError(c, http.StatusBadGateway, "asset download failed")
		return
	}
	maxDownloadSize, err := seedanceCompatDownloadLimit(c.Request.Context(), asset, response.ContentLength)
	if err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "asset download is temporarily unavailable")
		return
	}
	if response.ContentLength > maxDownloadSize {
		seedanceCompatError(c, http.StatusRequestEntityTooLarge, "asset download exceeds the size limit")
		return
	}
	var body io.Reader = response.Body
	contentLength := response.ContentLength
	if contentLength < 0 {
		file, err := os.CreateTemp("", "seedance-compat-download-*")
		if err != nil {
			seedanceCompatError(c, http.StatusBadGateway, "asset download failed")
			return
		}
		defer func() {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}()
		contentLength, err = io.Copy(file, io.LimitReader(response.Body, maxDownloadSize+1))
		if err != nil {
			seedanceCompatError(c, http.StatusBadGateway, "asset download failed")
			return
		}
		if contentLength > maxDownloadSize {
			seedanceCompatError(c, http.StatusRequestEntityTooLarge, "asset download exceeds the size limit")
			return
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			seedanceCompatError(c, http.StatusBadGateway, "asset download failed")
			return
		}
		body = file
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		c.Header("Content-Type", contentType)
	}
	if contentLength >= 0 {
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": asset.Name}); disposition != "" {
		c.Header("Content-Disposition", disposition)
	}
	c.Status(http.StatusOK)
	if _, err := io.Copy(c.Writer, body); err != nil {
		common.SysError("seedance asset download interrupted: " + err.Error())
	}
}
