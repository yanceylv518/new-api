package setting

import (
	"fmt"
	"strconv"

	"github.com/QuantumNous/new-api/common"
)

const (
	TokenResourceLimitsEnabledKey                = "TokenResourceLimitsEnabled"
	TokenResourceDefaultMaxRequestBodyBytesKey   = "TokenResourceDefaultMaxRequestBodyBytes"
	TokenResourceDefaultMaxConcurrentRequestsKey = "TokenResourceDefaultMaxConcurrentRequests"
)

// These options use the existing root-only configuration API and option sync.
// They must not be added to public status responses or user token DTOs.
type TokenResourceLimitSettings struct {
	Enabled                      bool  `json:"enabled"`
	DefaultMaxRequestBodyBytes   int64 `json:"max_request_body_bytes"`
	DefaultMaxConcurrentRequests int   `json:"max_concurrent_requests"`
}

func ValidateTokenResourceLimitOption(key, value string) error {
	switch key {
	case TokenResourceLimitsEnabledKey:
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("%s must be a boolean", key)
		}
	case TokenResourceDefaultMaxRequestBodyBytesKey:
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit < 0 {
			return fmt.Errorf("%s must be a non-negative integer number of bytes", key)
		}
	case TokenResourceDefaultMaxConcurrentRequestsKey:
		limit, err := strconv.ParseInt(value, 10, 32)
		if err != nil || limit < 0 {
			return fmt.Errorf("%s must be a non-negative 32-bit integer", key)
		}
	}
	return nil
}

// GetTokenResourceLimitSettings reads a single synchronized snapshot. Absent
// options keep the feature disabled during upgrades and mixed-version runs.
func GetTokenResourceLimitSettings() (TokenResourceLimitSettings, error) {
	return getTokenResourceLimitSettings(false)
}

// GetConfiguredTokenResourceLimitSettings includes saved defaults even while
// enforcement is disabled, for administrator-only configuration displays.
func GetConfiguredTokenResourceLimitSettings() (TokenResourceLimitSettings, error) {
	return getTokenResourceLimitSettings(true)
}

func getTokenResourceLimitSettings(includeDisabledDefaults bool) (TokenResourceLimitSettings, error) {
	common.OptionMapRWMutex.RLock()
	enabledValue, hasEnabled := common.OptionMap[TokenResourceLimitsEnabledKey]
	bodyValue, hasBody := common.OptionMap[TokenResourceDefaultMaxRequestBodyBytesKey]
	concurrentValue, hasConcurrent := common.OptionMap[TokenResourceDefaultMaxConcurrentRequestsKey]
	common.OptionMapRWMutex.RUnlock()
	if !hasEnabled {
		enabledValue = "false"
	}
	if !hasBody {
		bodyValue = "0"
	}
	if !hasConcurrent {
		concurrentValue = "0"
	}
	var settings TokenResourceLimitSettings
	if err := ValidateTokenResourceLimitOption(TokenResourceLimitsEnabledKey, enabledValue); err != nil {
		return settings, err
	}
	settings.Enabled, _ = strconv.ParseBool(enabledValue)
	if !settings.Enabled && !includeDisabledDefaults {
		return settings, nil
	}
	if err := ValidateTokenResourceLimitOption(TokenResourceDefaultMaxRequestBodyBytesKey, bodyValue); err != nil {
		return settings, err
	}
	settings.DefaultMaxRequestBodyBytes, _ = strconv.ParseInt(bodyValue, 10, 64)
	if err := ValidateTokenResourceLimitOption(TokenResourceDefaultMaxConcurrentRequestsKey, concurrentValue); err != nil {
		return settings, err
	}
	concurrent, _ := strconv.ParseInt(concurrentValue, 10, 32)
	settings.DefaultMaxConcurrentRequests = int(concurrent)
	return settings, nil
}
