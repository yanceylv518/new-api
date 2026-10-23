package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
)

const (
	redisRateLimitNamespace         = "rateLimit:v2"
	seedanceAssetRateLimitRequests  = 60
	seedanceAssetRateLimitWindowSec = 60
)

var inMemoryRateLimiter common.InMemoryRateLimiter

var defNext = func(c *gin.Context) {
	c.Next()
}

func redisIPRateLimitKey(mark string, clientIP string) string {
	return fmt.Sprintf("%s:ip:%s:%s", redisRateLimitNamespace, mark, clientIP)
}

func redisUserRateLimitKey(mark string, userID int) string {
	return fmt.Sprintf("%s:user:%s:%d", redisRateLimitNamespace, mark, userID)
}

func redisFixedWindowTake(ctx context.Context, key string, maxRequestNum int, duration int64) (bool, int64, int64, error) {
	return common.RedisFixedWindowTake(ctx, key, maxRequestNum, duration)
}

func redisRateLimiter(c *gin.Context, maxRequestNum int, duration int64, mark string) {
	allowed, _, ttlSeconds, err := redisFixedWindowTake(
		c.Request.Context(),
		redisIPRateLimitKey(mark, c.ClientIP()),
		maxRequestNum,
		duration,
	)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("rate limit check failed (mark=%s): %v", mark, err))
		c.Status(http.StatusInternalServerError)
		c.Abort()
		return
	}
	if !allowed {
		writeRateLimited(c, ttlSeconds)
	}
}

func memoryRateLimiter(c *gin.Context, maxRequestNum int, duration int64, mark string) {
	key := mark + c.ClientIP()
	if !inMemoryRateLimiter.Request(key, maxRequestNum, duration) {
		writeRateLimited(c, duration)
		return
	}
}

// writeRateLimited rejects the request with 429 and a Retry-After hint so
// clients can back off instead of treating the rejection as a fatal error.
// The in-memory limiter cannot report the remaining window, so callers
// without a TTL pass the full window duration as a conservative upper bound.
func writeRateLimited(c *gin.Context, retryAfterSeconds int64) {
	if retryAfterSeconds > 0 {
		c.Header("Retry-After", strconv.FormatInt(retryAfterSeconds, 10))
	}
	c.Status(http.StatusTooManyRequests)
	c.Abort()
}

func rateLimitFactory(maxRequestNum int, duration int64, mark string) func(c *gin.Context) {
	if common.RedisEnabled {
		return func(c *gin.Context) {
			redisRateLimiter(c, maxRequestNum, duration, mark)
		}
	}
	// It's safe to call multi times.
	inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
	return func(c *gin.Context) {
		memoryRateLimiter(c, maxRequestNum, duration, mark)
	}
}

// GlobalWebRateLimit limits dashboard pages and other fallback paths per IP.
// Files that exist in the frontend build count against a separate static
// limiter instead, so lazy-loaded chunks and icons do not use up the page
// budget. The static limiter is disabled by default.
func GlobalWebRateLimit(frontendFS static.ServeFileSystem) func(c *gin.Context) {
	webLimit := defNext
	if common.GlobalWebRateLimitEnable {
		webLimit = rateLimitFactory(common.GlobalWebRateLimitNum, common.GlobalWebRateLimitDuration, "GW")
	}
	staticLimit := defNext
	if common.GlobalStaticRateLimitEnable {
		staticLimit = rateLimitFactory(common.GlobalStaticRateLimitNum, common.GlobalStaticRateLimitDuration, "GS")
	}
	return func(c *gin.Context) {
		if frontendFS.Exists("/", c.Request.URL.Path) {
			staticLimit(c)
			return
		}
		webLimit(c)
	}
}

func GlobalAPIRateLimit() func(c *gin.Context) {
	return managementRateLimitFactory("GA", false, func(settings common.ManagementRateLimits) common.ManagementRateLimit { return settings.GlobalAPI })
}

func CriticalRateLimit(scope ...string) func(c *gin.Context) {
	mark := "CT"
	if len(scope) > 0 && scope[0] != "" {
		mark += ":" + scope[0]
	}
	return managementRateLimitFactory(mark, false, func(settings common.ManagementRateLimits) common.ManagementRateLimit {
		if len(scope) > 0 && scope[0] == "login" {
			return settings.LoginIP
		}
		return settings.Sensitive
	})
}

func UserCriticalRateLimit(scope string) func(c *gin.Context) {
	return managementRateLimitFactory("UC:"+scope, true, func(settings common.ManagementRateLimits) common.ManagementRateLimit { return settings.Sensitive })
}

// TokenQueryRateLimit 按已认证的用户合并统计各 API Key 的用量和日志查询。
func TokenQueryRateLimit() func(c *gin.Context) {
	return managementRateLimitFactory("TQ", true, func(settings common.ManagementRateLimits) common.ManagementRateLimit { return settings.TokenQuery })
}

func managementRateLimitFactory(mark string, byUser bool, policy func(common.ManagementRateLimits) common.ManagementRateLimit) func(*gin.Context) {
	return func(c *gin.Context) {
		limit := policy(common.GetManagementRateLimits())
		if !limit.Enabled {
			return
		}
		// 修改窗口后立即使用新计数，不继承旧窗口的剩余 TTL。
		scope := fmt.Sprintf("%s:w%d", mark, limit.WindowSeconds)
		key := redisIPRateLimitKey(scope, c.ClientIP())
		if byUser {
			userID := c.GetInt("id")
			if userID <= 0 {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			key = redisUserRateLimitKey(scope, userID)
		}
		if err := common.CheckRateLimit(c.Request.Context(), key, limit.Requests, limit.WindowSeconds); err != nil {
			var limited *common.RateLimitError
			if errors.As(err, &limited) {
				writeRateLimited(c, limited.RetryAfterSeconds)
				return
			}
			logger.LogError(c.Request.Context(), fmt.Sprintf("rate limit check failed (mark=%s): %v", mark, err))
			c.AbortWithStatus(http.StatusInternalServerError)
		}
	}
}

func DownloadRateLimit() func(c *gin.Context) {
	return rateLimitFactory(common.DownloadRateLimitNum, common.DownloadRateLimitDuration, "DW")
}

func UploadRateLimit() func(c *gin.Context) {
	return rateLimitFactory(common.UploadRateLimitNum, common.UploadRateLimitDuration, "UP")
}

// SeedanceAssetRateLimit 为素材库所有读写接口提供统一的用户维度限流。
func SeedanceAssetRateLimit() func(c *gin.Context) {
	return userRateLimitFactory(seedanceAssetRateLimitRequests, seedanceAssetRateLimitWindowSec, "SA")
}

// userRateLimitFactory creates a rate limiter keyed by authenticated user ID
// instead of client IP, making it resistant to proxy rotation attacks.
// Must be used AFTER authentication middleware (UserAuth).
func userRateLimitFactory(maxRequestNum int, duration int64, mark string) func(c *gin.Context) {
	if common.RedisEnabled {
		return func(c *gin.Context) {
			userID := c.GetInt("id")
			if userID == 0 {
				c.Status(http.StatusUnauthorized)
				c.Abort()
				return
			}
			userRedisRateLimiter(c, maxRequestNum, duration, redisUserRateLimitKey(mark, userID))
		}
	}
	// It's safe to call multi times.
	inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
	return func(c *gin.Context) {
		userID := c.GetInt("id")
		if userID == 0 {
			c.Status(http.StatusUnauthorized)
			c.Abort()
			return
		}
		key := fmt.Sprintf("%s:user:%d", mark, userID)
		if !inMemoryRateLimiter.Request(key, maxRequestNum, duration) {
			writeRateLimited(c, duration)
			return
		}
	}
}

// userRedisRateLimiter is like redisRateLimiter but accepts a pre-built key
// (to support user-ID-based keys).
func userRedisRateLimiter(c *gin.Context, maxRequestNum int, duration int64, key string) {
	allowed, _, ttlSeconds, err := redisFixedWindowTake(c.Request.Context(), key, maxRequestNum, duration)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("rate limit check failed (key=%s): %v", key, err))
		c.Status(http.StatusInternalServerError)
		c.Abort()
		return
	}
	if !allowed {
		writeRateLimited(c, ttlSeconds)
	}
}

// SearchRateLimit returns a per-user rate limiter for search endpoints.
// Configurable via SEARCH_RATE_LIMIT_ENABLE / SEARCH_RATE_LIMIT / SEARCH_RATE_LIMIT_DURATION.
func SearchRateLimit() func(c *gin.Context) {
	if !common.SearchRateLimitEnable {
		return defNext
	}
	return userRateLimitFactory(common.SearchRateLimitNum, common.SearchRateLimitDuration, "SR")
}
