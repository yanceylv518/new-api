package system_setting

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/QuantumNous/new-api/common"
)

const (
	PrivateAssetOSSOptionPrefix          = "private_asset_oss."
	PrivateAssetOSSRegionKey             = PrivateAssetOSSOptionPrefix + "region"
	PrivateAssetOSSEndpointKey           = PrivateAssetOSSOptionPrefix + "endpoint"
	PrivateAssetOSSBucketKey             = PrivateAssetOSSOptionPrefix + "bucket"
	PrivateAssetOSSPrefixKey             = PrivateAssetOSSOptionPrefix + "prefix"
	PrivateAssetOSSAccessKeyIDKey        = PrivateAssetOSSOptionPrefix + "access_key_id"
	PrivateAssetOSSAccessKeySecretKey    = PrivateAssetOSSOptionPrefix + "access_key_secret"
	PrivateAssetOSSSecretConfiguredKey   = PrivateAssetOSSOptionPrefix + "secret_configured"
	privateAssetOSSDefaultObjectPrefix   = "private-assets/"
	privateAssetOSSMaxObjectPrefixLength = 512
)

var (
	privateAssetOSSRegionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	privateAssetOSSBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// PrivateAssetOSSSettings 描述私域素材库使用的 OSS 连接信息。
// AccessKey 只保存在服务端配置中，不得通过普通用户接口返回。
type PrivateAssetOSSSettings struct {
	Region          string
	Endpoint        string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	AccessKeySecret string
}

// DefaultPrivateAssetOSSSettings 从环境变量读取启动默认值，数据库配置会在启动后覆盖这些值。
func DefaultPrivateAssetOSSSettings() PrivateAssetOSSSettings {
	return normalizePrivateAssetOSSSettings(PrivateAssetOSSSettings{
		Region:          strings.TrimSpace(os.Getenv("PRIVATE_ASSET_OSS_REGION")),
		Endpoint:        strings.TrimSpace(os.Getenv("PRIVATE_ASSET_OSS_ENDPOINT")),
		Bucket:          strings.TrimSpace(os.Getenv("PRIVATE_ASSET_OSS_BUCKET")),
		Prefix:          strings.TrimSpace(os.Getenv("PRIVATE_ASSET_OSS_PREFIX")),
		AccessKeyID:     strings.TrimSpace(os.Getenv("PRIVATE_ASSET_OSS_ACCESS_KEY_ID")),
		AccessKeySecret: strings.TrimSpace(os.Getenv("PRIVATE_ASSET_OSS_ACCESS_KEY_SECRET")),
	})
}

// PrivateAssetOSSOptionValues 将配置转换为系统 Option 使用的扁平键值。
func PrivateAssetOSSOptionValues(settings PrivateAssetOSSSettings) map[string]string {
	return map[string]string{
		PrivateAssetOSSRegionKey:          settings.Region,
		PrivateAssetOSSEndpointKey:        settings.Endpoint,
		PrivateAssetOSSBucketKey:          settings.Bucket,
		PrivateAssetOSSPrefixKey:          settings.Prefix,
		PrivateAssetOSSAccessKeyIDKey:     settings.AccessKeyID,
		PrivateAssetOSSAccessKeySecretKey: settings.AccessKeySecret,
	}
}

// GetPrivateAssetOSSSettings 获取当前生效配置的快照，避免上传期间读取到一组混合值。
func GetPrivateAssetOSSSettings() PrivateAssetOSSSettings {
	settings := DefaultPrivateAssetOSSSettings()
	common.OptionMapRWMutex.RLock()
	defer common.OptionMapRWMutex.RUnlock()
	if common.OptionMap == nil {
		return normalizePrivateAssetOSSSettings(settings)
	}

	values := map[string]*string{
		PrivateAssetOSSRegionKey:          &settings.Region,
		PrivateAssetOSSEndpointKey:        &settings.Endpoint,
		PrivateAssetOSSBucketKey:          &settings.Bucket,
		PrivateAssetOSSPrefixKey:          &settings.Prefix,
		PrivateAssetOSSAccessKeyIDKey:     &settings.AccessKeyID,
		PrivateAssetOSSAccessKeySecretKey: &settings.AccessKeySecret,
	}
	for key, target := range values {
		if value, ok := common.OptionMap[key]; ok {
			*target = value
		}
	}
	return normalizePrivateAssetOSSSettings(settings)
}

// NormalizeAndValidatePrivateAssetOSSSettings 统一处理页面和环境变量配置的格式及边界。
func NormalizeAndValidatePrivateAssetOSSSettings(settings PrivateAssetOSSSettings) (PrivateAssetOSSSettings, error) {
	settings = normalizePrivateAssetOSSSettings(settings)
	if !privateAssetOSSRegionPattern.MatchString(settings.Region) {
		return settings, fmt.Errorf("invalid OSS Region")
	}
	if settings.Endpoint != "" {
		parsed, err := url.Parse(settings.Endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return settings, fmt.Errorf("OSS Endpoint must be an HTTPS origin without a path, query, or credentials")
		}
	}
	if !privateAssetOSSBucketPattern.MatchString(settings.Bucket) {
		return settings, fmt.Errorf("invalid OSS Bucket name")
	}
	if settings.AccessKeyID == "" {
		return settings, fmt.Errorf("OSS AccessKey ID is required")
	}
	if settings.AccessKeySecret == "" {
		return settings, fmt.Errorf("OSS AccessKey Secret is required")
	}
	if len(settings.AccessKeyID) > 128 || len(settings.AccessKeySecret) > 256 {
		return settings, fmt.Errorf("OSS AccessKey is too long")
	}
	if err := validatePrivateAssetOSSPrefix(settings.Prefix); err != nil {
		return settings, err
	}
	return settings, nil
}

func normalizePrivateAssetOSSSettings(settings PrivateAssetOSSSettings) PrivateAssetOSSSettings {
	settings.Region = strings.TrimSpace(settings.Region)
	settings.Endpoint = strings.TrimRight(strings.TrimSpace(settings.Endpoint), "/")
	settings.Bucket = strings.TrimSpace(settings.Bucket)
	settings.AccessKeyID = strings.TrimSpace(settings.AccessKeyID)
	settings.AccessKeySecret = strings.TrimSpace(settings.AccessKeySecret)
	settings.Prefix = strings.Trim(strings.TrimSpace(settings.Prefix), "/")
	if settings.Prefix == "" {
		settings.Prefix = privateAssetOSSDefaultObjectPrefix
	} else {
		settings.Prefix += "/"
	}
	return settings
}

func validatePrivateAssetOSSPrefix(prefix string) error {
	if len(prefix) > privateAssetOSSMaxObjectPrefixLength {
		return fmt.Errorf("OSS object prefix is too long")
	}
	if strings.Contains(prefix, `\`) {
		return fmt.Errorf("OSS object prefix must use forward slashes")
	}
	for _, segment := range strings.Split(strings.TrimSuffix(prefix, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("OSS object prefix contains invalid path segments")
		}
	}
	for _, char := range prefix {
		if unicode.IsControl(char) {
			return fmt.Errorf("OSS object prefix must not contain control characters")
		}
	}
	return nil
}
