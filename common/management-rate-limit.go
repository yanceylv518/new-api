package common

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/QuantumNous/new-api/constant"
)

const (
	ManagementRateLimitsOptionKey       = "ManagementRateLimits"
	MaxManagementRateLimitRequests      = 2_147_483_647
	MaxManagementRateLimitWindowSeconds = 2_147_483_647
	MaxPrivateAssetSizeMiB              = (math.MaxInt64 >> 20) - 1
	MaxPrivateAssetStorageMiB           = math.MaxInt64 >> 20
	MaxPrivateAssetCount                = 9_007_199_254_740_991
)

type ManagementRateLimit struct {
	Enabled       bool  `json:"enabled"`
	Requests      int   `json:"requests"`
	WindowSeconds int64 `json:"window_seconds"`
}

type PrivateAssetLimits struct {
	ImageMiB       int64 `json:"image_mib"`
	VideoMiB       int64 `json:"video_mib"`
	AudioMiB       int64 `json:"audio_mib"`
	CompatImageMiB int64 `json:"compat_image_mib"`
	UserMaxCount   int64 `json:"user_max_count"`
	UserStorageMiB int64 `json:"user_storage_mib"`
}

func (limits *PrivateAssetLimits) UnmarshalJSON(data []byte) error {
	var value struct {
		ImageMiB       *int64 `json:"image_mib"`
		VideoMiB       *int64 `json:"video_mib"`
		AudioMiB       *int64 `json:"audio_mib"`
		CompatImageMiB *int64 `json:"compat_image_mib"`
		UserMaxCount   *int64 `json:"user_max_count"`
		UserStorageMiB *int64 `json:"user_storage_mib"`
	}
	if err := Unmarshal(data, &value); err != nil {
		return err
	}
	if value.ImageMiB == nil || value.VideoMiB == nil || value.AudioMiB == nil || value.CompatImageMiB == nil || value.UserMaxCount == nil || value.UserStorageMiB == nil {
		return fmt.Errorf("all private asset limits must be explicit integers")
	}
	*limits = PrivateAssetLimits{*value.ImageMiB, *value.VideoMiB, *value.AudioMiB, *value.CompatImageMiB, *value.UserMaxCount, *value.UserStorageMiB}
	return nil
}

func (limit *ManagementRateLimit) UnmarshalJSON(data []byte) error {
	var value struct {
		Enabled       *bool `json:"enabled"`
		Requests      int   `json:"requests"`
		WindowSeconds int64 `json:"window_seconds"`
	}
	if err := Unmarshal(data, &value); err != nil {
		return err
	}
	if value.Enabled == nil {
		return fmt.Errorf("rate limit enabled must be an explicit boolean")
	}
	*limit = ManagementRateLimit{Enabled: *value.Enabled, Requests: value.Requests, WindowSeconds: value.WindowSeconds}
	return nil
}

type ManagementRateLimits struct {
	GlobalAPI      ManagementRateLimit `json:"global_api"`
	LoginIP        ManagementRateLimit `json:"login_ip"`
	LoginAccount   ManagementRateLimit `json:"login_account"`
	SessionRefresh ManagementRateLimit `json:"session_refresh"`
	TokenQuery     ManagementRateLimit `json:"token_query"`
	Sensitive      ManagementRateLimit `json:"sensitive"`
	SeedanceAsset  ManagementRateLimit `json:"seedance_asset"`
	AssetUpload    ManagementRateLimit `json:"asset_upload"`
	AssetDownload  ManagementRateLimit `json:"asset_download"`
	AssetLimits    PrivateAssetLimits  `json:"asset_limits"`
}

var managementRateLimits atomic.Pointer[ManagementRateLimits]

func DefaultManagementRateLimits() ManagementRateLimits {
	settings := ManagementRateLimits{
		GlobalAPI:      ManagementRateLimit{GlobalApiRateLimitEnable, GlobalApiRateLimitNum, GlobalApiRateLimitDuration},
		LoginIP:        ManagementRateLimit{CriticalRateLimitEnable, CriticalRateLimitNum, CriticalRateLimitDuration},
		LoginAccount:   ManagementRateLimit{CriticalRateLimitEnable, CriticalRateLimitNum, CriticalRateLimitDuration},
		SessionRefresh: ManagementRateLimit{CriticalRateLimitEnable, AuthRefreshRateLimitNum, AuthRefreshRateLimitDuration},
		TokenQuery:     ManagementRateLimit{CriticalRateLimitEnable, TokenQueryRateLimitNum, TokenQueryRateLimitDuration},
		Sensitive:      ManagementRateLimit{CriticalRateLimitEnable, CriticalRateLimitNum, CriticalRateLimitDuration},
		SeedanceAsset:  ManagementRateLimit{true, 60, 60},
		AssetUpload:    ManagementRateLimit{true, UploadRateLimitNum, UploadRateLimitDuration},
		AssetDownload:  ManagementRateLimit{true, DownloadRateLimitNum, DownloadRateLimitDuration},
		AssetLimits: PrivateAssetLimits{
			ImageMiB: 30, VideoMiB: 50, AudioMiB: 15, CompatImageMiB: 50,
			UserMaxCount: int64(constant.PrivateAssetUserMaxCount), UserStorageMiB: constant.PrivateAssetUserMaxBytes >> 20,
		},
	}
	for _, item := range []struct {
		name     string
		limit    *ManagementRateLimit
		requests int
		window   int64
	}{
		{"global_api", &settings.GlobalAPI, 360, 180},
		{"login_ip", &settings.LoginIP, 20, 1200},
		{"login_account", &settings.LoginAccount, 20, 1200},
		{"session_refresh", &settings.SessionRefresh, 60, 60},
		{"token_query", &settings.TokenQuery, 60, 60},
		{"sensitive", &settings.Sensitive, 20, 1200},
	} {
		if item.limit.Requests < 1 || item.limit.Requests > MaxManagementRateLimitRequests {
			SysError(fmt.Sprintf("invalid %s rate limit count; using default %d", item.name, item.requests))
			item.limit.Requests = item.requests
		}
		if item.limit.WindowSeconds < 1 || item.limit.WindowSeconds > MaxManagementRateLimitWindowSeconds {
			SysError(fmt.Sprintf("invalid %s rate limit window; using default %d seconds", item.name, item.window))
			item.limit.WindowSeconds = item.window
		}
	}
	return settings
}

func GetManagementRateLimits() ManagementRateLimits {
	if settings := managementRateLimits.Load(); settings != nil {
		return *settings
	}
	return DefaultManagementRateLimits()
}

func ParseManagementRateLimits(raw string) (ManagementRateLimits, error) {
	defaults := DefaultManagementRateLimits()
	// 旧配置缺少素材限流字段时保留原有默认值，其他字段仍需显式提供。
	settings := ManagementRateLimits{
		SeedanceAsset: defaults.SeedanceAsset,
		AssetUpload:   defaults.AssetUpload,
		AssetDownload: defaults.AssetDownload,
		AssetLimits:   defaults.AssetLimits,
	}
	if err := UnmarshalJsonStr(raw, &settings); err != nil {
		return settings, fmt.Errorf("invalid management rate limits: %w", err)
	}
	for _, limit := range []ManagementRateLimit{settings.GlobalAPI, settings.LoginIP, settings.LoginAccount, settings.SessionRefresh, settings.TokenQuery, settings.Sensitive, settings.SeedanceAsset, settings.AssetUpload, settings.AssetDownload} {
		if limit.Requests < 1 || limit.Requests > MaxManagementRateLimitRequests {
			return settings, fmt.Errorf("rate limit requests must be between 1 and %d", MaxManagementRateLimitRequests)
		}
		if limit.WindowSeconds < 1 || limit.WindowSeconds > MaxManagementRateLimitWindowSeconds {
			return settings, fmt.Errorf("rate limit window must be between 1 and %d seconds", MaxManagementRateLimitWindowSeconds)
		}
	}
	for _, size := range []int64{settings.AssetLimits.ImageMiB, settings.AssetLimits.VideoMiB, settings.AssetLimits.AudioMiB, settings.AssetLimits.CompatImageMiB} {
		if size < 1 || size > MaxPrivateAssetSizeMiB {
			return settings, fmt.Errorf("private asset file size must be between 1 and %d MiB", MaxPrivateAssetSizeMiB)
		}
	}
	if settings.AssetLimits.UserMaxCount < 0 || settings.AssetLimits.UserMaxCount > MaxPrivateAssetCount {
		return settings, fmt.Errorf("private asset count must be between 0 and %d", MaxPrivateAssetCount)
	}
	if settings.AssetLimits.UserStorageMiB < 0 || settings.AssetLimits.UserStorageMiB > MaxPrivateAssetStorageMiB {
		return settings, fmt.Errorf("private asset storage must be between 0 and %d MiB", MaxPrivateAssetStorageMiB)
	}
	return settings, nil
}

func PrivateAssetMultipartBodyLimit(compat bool) int64 {
	limits := GetManagementRateLimits().AssetLimits
	if compat {
		return (limits.CompatImageMiB + 1) << 20
	}
	return (max(limits.ImageMiB, limits.VideoMiB, limits.AudioMiB) + 1) << 20
}

// 素材上传使用独立的管理员配置，其他接口保留全局请求体上限。
func IsPrivateAssetUploadPath(method, path string) bool {
	return method == "POST" && (path == "/v1/seedance/assets/upload" || path == "/doubao/api/assets/upload")
}

func RequestBodyLimitForPath(method, path string, fallback int64) int64 {
	if method == "POST" {
		switch path {
		case "/v1/seedance/assets/upload":
			return PrivateAssetMultipartBodyLimit(false)
		case "/doubao/api/assets/upload":
			return PrivateAssetMultipartBodyLimit(true)
		}
	}
	return fallback
}

// LoadManagementRateLimits 原子替换完整配置，请求不会读到一半新一半旧的参数。
func LoadManagementRateLimits(raw string) error {
	settings, err := ParseManagementRateLimits(raw)
	if err != nil {
		return err
	}
	managementRateLimits.Store(&settings)
	return nil
}
