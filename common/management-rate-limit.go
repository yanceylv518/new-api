package common

import (
	"fmt"
	"sync/atomic"
)

const (
	ManagementRateLimitsOptionKey       = "ManagementRateLimits"
	MaxManagementRateLimitRequests      = 2_147_483_647
	MaxManagementRateLimitWindowSeconds = 2_147_483_647
)

type ManagementRateLimit struct {
	Enabled       bool  `json:"enabled"`
	Requests      int   `json:"requests"`
	WindowSeconds int64 `json:"window_seconds"`
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
	var settings ManagementRateLimits
	if err := UnmarshalJsonStr(raw, &settings); err != nil {
		return settings, fmt.Errorf("invalid management rate limits: %w", err)
	}
	for _, limit := range []ManagementRateLimit{settings.GlobalAPI, settings.LoginIP, settings.LoginAccount, settings.SessionRefresh, settings.TokenQuery, settings.Sensitive} {
		if limit.Requests < 1 || limit.Requests > MaxManagementRateLimitRequests {
			return settings, fmt.Errorf("rate limit requests must be between 1 and %d", MaxManagementRateLimitRequests)
		}
		if limit.WindowSeconds < 1 || limit.WindowSeconds > MaxManagementRateLimitWindowSeconds {
			return settings, fmt.Errorf("rate limit window must be between 1 and %d seconds", MaxManagementRateLimitWindowSeconds)
		}
	}
	return settings, nil
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
