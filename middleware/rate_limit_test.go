package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-contrib/static"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setManagementRateLimitTestSettings(t *testing.T, settings common.ManagementRateLimits) {
	t.Helper()
	previous, err := common.Marshal(common.GetManagementRateLimits())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, common.LoadManagementRateLimits(string(previous))) })
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, common.LoadManagementRateLimits(string(encoded)))
}

func managementRateLimitTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(GlobalAPIRateLimit())
	authenticated := func(c *gin.Context) {
		id, _ := strconv.Atoi(c.GetHeader("X-Test-User"))
		c.Set("id", id)
	}
	finish := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	router.GET("/ping", finish)
	router.GET("/login", CriticalRateLimit("login"), finish)
	router.GET("/logout", CriticalRateLimit("logout"), finish)
	router.GET("/sensitive", CriticalRateLimit(), finish)
	router.GET("/usage", authenticated, TokenQueryRateLimit(), finish)
	router.GET("/logs", authenticated, TokenQueryRateLimit(), finish)
	return router
}

func managementRateLimitTestRequest(router http.Handler, path, address string, userID int) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = address
	request.Header.Set("X-Test-User", strconv.Itoa(userID))
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestManagementRateLimitScopesAndHotUpdates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, backend := range []string{"redis", "memory"} {
		t.Run(backend, func(t *testing.T) {
			if backend == "redis" {
				useRateLimitMiniRedis(t)
			} else {
				previous := common.RedisEnabled
				common.RedisEnabled = false
				t.Cleanup(func() { common.RedisEnabled = previous })
			}
			settings := common.DefaultManagementRateLimits()
			settings.GlobalAPI = common.ManagementRateLimit{Enabled: true, Requests: 100, WindowSeconds: 60}
			settings.LoginIP = common.ManagementRateLimit{Enabled: true, Requests: 2, WindowSeconds: 60}
			settings.Sensitive = common.ManagementRateLimit{Enabled: true, Requests: 2, WindowSeconds: 60}
			settings.TokenQuery = common.ManagementRateLimit{Enabled: true, Requests: 2, WindowSeconds: 60}
			setManagementRateLimitTestSettings(t, settings)
			router := managementRateLimitTestRouter(t)
			request := func(path, address string, id int) *httptest.ResponseRecorder {
				return managementRateLimitTestRequest(router, path, address, id)
			}
			const address = "192.0.2.110:1000"
			assert.Equal(t, 204, request("/usage", address, 41).Code)
			assert.Equal(t, 204, request("/logs", address, 41).Code)
			limited := request("/usage", "192.0.2.111:1000", 41)
			assert.Equal(t, 429, limited.Code)
			assert.Equal(t, "60", limited.Header().Get("Retry-After"))
			assert.Equal(t, 204, request("/usage", address, 42).Code)
			assert.Equal(t, 401, request("/usage", address, 0).Code)
			assert.Equal(t, 204, request("/login", address, 0).Code)
			assert.Equal(t, 204, request("/login", address, 0).Code)
			assert.Equal(t, 429, request("/login", address, 0).Code)
			assert.Equal(t, 204, request("/logout", address, 0).Code)
			assert.Equal(t, 204, request("/sensitive", address, 0).Code)

			settings.LoginIP.Requests = 10
			setManagementRateLimitTestSettings(t, settings)
			assert.Equal(t, 204, request("/login", address, 0).Code)
			settings.LoginIP.Requests, settings.LoginIP.WindowSeconds = 1, 90
			setManagementRateLimitTestSettings(t, settings)
			assert.Equal(t, 204, request("/login", address, 0).Code)
			limited = request("/login", address, 0)
			assert.Equal(t, 429, limited.Code)
			assert.Equal(t, "90", limited.Header().Get("Retry-After"))
			settings.LoginIP.Enabled = false
			setManagementRateLimitTestSettings(t, settings)
			assert.Equal(t, 204, request("/login", address, 0).Code)
			assert.Equal(t, 429, request("/logs", address, 41).Code)

			settings.GlobalAPI.Requests, settings.GlobalAPI.WindowSeconds = 2, 30
			setManagementRateLimitTestSettings(t, settings)
			assert.Equal(t, 204, request("/ping", address, 0).Code)
			assert.Equal(t, 204, request("/ping", address, 0).Code)
			assert.Equal(t, 429, request("/ping", address, 0).Code)
		})
	}
}

func TestManagementRateLimitConcurrentAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if address := os.Getenv("TEST_RATE_LIMIT_REDIS_ADDR"); address != "" {
		previousEnabled, previousClient := common.RedisEnabled, common.RDB
		client := redis.NewClient(&redis.Options{Addr: address})
		require.NoError(t, client.Ping(t.Context()).Err())
		common.RedisEnabled, common.RDB = true, client
		t.Cleanup(func() {
			common.RedisEnabled, common.RDB = previousEnabled, previousClient
			keys := []string{redisIPRateLimitKey("GA:w61", "192.0.2.200")}
			for userID := range 200 {
				keys = append(keys, redisUserRateLimitKey("TQ:w61", 10_000+userID))
			}
			require.NoError(t, client.Del(context.Background(), keys...).Err())
			require.NoError(t, client.Close())
		})
	} else {
		useRateLimitMiniRedis(t)
	}
	settings := common.DefaultManagementRateLimits()
	settings.GlobalAPI = common.ManagementRateLimit{Enabled: true, Requests: 1000, WindowSeconds: 61}
	settings.TokenQuery = common.ManagementRateLimit{Enabled: true, Requests: 2, WindowSeconds: 61}
	setManagementRateLimitTestSettings(t, settings)
	router := managementRateLimitTestRouter(t)
	jobs := make(chan int)
	failures := make(chan string, 600)
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			for userID := range jobs {
				for _, expected := range []int{204, 204, 429} {
					response := managementRateLimitTestRequest(router, "/usage", "192.0.2.200:1000", userID)
					if response.Code != expected {
						failures <- fmt.Sprintf("user %d: got %d, want %d", userID, response.Code, expected)
					}
				}
			}
		})
	}
	for userID := range 200 {
		jobs <- 10_000 + userID
	}
	close(jobs)
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

func useRateLimitMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()

	previousRedisEnabled := common.RedisEnabled
	previousRedisClient := common.RDB
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	require.NoError(t, redisClient.Ping(context.Background()).Err())

	common.RedisEnabled = true
	common.RDB = redisClient
	t.Cleanup(func() {
		_ = redisClient.Close()
		common.RedisEnabled = previousRedisEnabled
		common.RDB = previousRedisClient
	})

	return redisServer, redisClient
}

func performRateLimitRequest(router http.Handler, path string, remoteAddr string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.RemoteAddr = remoteAddr
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestRedisIPRateLimiterThresholdTTLAndNamespace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	redisServer, _ := useRateLimitMiniRedis(t)

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.GET("/limited", rateLimitFactory(2, 37, "TEST"), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	remoteAddr := "192.0.2.10:12345"
	legacyKey := "rateLimit:TEST192.0.2.10"
	_, err := redisServer.Push(legacyKey, "legacy-list-entry")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/limited", remoteAddr).Code)
	assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/limited", remoteAddr).Code)
	limitedResponse := performRateLimitRequest(router, "/limited", remoteAddr)
	assert.Equal(t, http.StatusTooManyRequests, limitedResponse.Code)
	assert.Equal(t, "37", limitedResponse.Header().Get("Retry-After"))

	key := redisIPRateLimitKey("TEST", "192.0.2.10")
	count, err := redisServer.Get(key)
	require.NoError(t, err)
	assert.Equal(t, "3", count)
	assert.Equal(t, 37*time.Second, redisServer.TTL(key))
	assert.True(t, redisServer.Exists(legacyKey), "the v2 counter must not touch an old list key")
}

func TestRedisUserRateLimiterUsesSharedFixedWindow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	redisServer, _ := useRateLimitMiniRedis(t)

	router := gin.New()
	router.GET(
		"/limited",
		func(c *gin.Context) { c.Set("id", 42) },
		userRateLimitFactory(1, 23, "USER"),
		func(c *gin.Context) { c.Status(http.StatusNoContent) },
	)

	assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/limited", "192.0.2.20:12345").Code)
	assert.Equal(t, http.StatusTooManyRequests, performRateLimitRequest(router, "/limited", "198.51.100.20:12345").Code)

	key := redisUserRateLimitKey("USER", 42)
	assert.True(t, redisServer.Exists(key))
	assert.Equal(t, 23*time.Second, redisServer.TTL(key))
}

func TestRedisEmailVerificationRateLimiterPreservesResponseAndTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	redisServer, _ := useRateLimitMiniRedis(t)

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.GET("/verify", EmailVerificationRateLimit(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	remoteAddr := "192.0.2.30:12345"
	assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/verify", remoteAddr).Code)
	assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/verify", remoteAddr).Code)
	response := performRateLimitRequest(router, "/verify", remoteAddr)
	assert.Equal(t, http.StatusTooManyRequests, response.Code)
	assert.JSONEq(t, `{"success":false,"message":"发送过于频繁，请等待 30 秒后再试"}`, response.Body.String())

	key := redisIPRateLimitKey(EmailVerificationRateLimitMark, "192.0.2.30")
	assert.True(t, redisServer.Exists(key))
	assert.Equal(t, time.Duration(EmailVerificationDuration)*time.Second, redisServer.TTL(key))
}

func TestRedisFixedWindowIsAtomicUnderConcurrency(t *testing.T) {
	redisServer, _ := useRateLimitMiniRedis(t)
	const (
		requestCount = 20
		maximumCount = 7
		duration     = int64(41)
	)
	key := redisIPRateLimitKey("CONCURRENT", "192.0.2.40")

	var allowedCount atomic.Int64
	errorsFound := make(chan error, requestCount)
	var waitGroup sync.WaitGroup
	waitGroup.Add(requestCount)
	for range requestCount {
		go func() {
			defer waitGroup.Done()
			allowed, _, _, err := redisFixedWindowTake(context.Background(), key, maximumCount, duration)
			if err != nil {
				errorsFound <- err
				return
			}
			if allowed {
				allowedCount.Add(1)
			}
		}()
	}
	waitGroup.Wait()
	close(errorsFound)
	for err := range errorsFound {
		require.NoError(t, err)
	}

	assert.Equal(t, int64(maximumCount), allowedCount.Load())
	count, err := redisServer.Get(key)
	require.NoError(t, err)
	assert.Equal(t, "20", count)
	assert.Equal(t, time.Duration(duration)*time.Second, redisServer.TTL(key))
}

func TestRedisFixedWindowResetsAtBoundary(t *testing.T) {
	redisServer, _ := useRateLimitMiniRedis(t)
	const duration = int64(10)
	key := redisIPRateLimitKey("BOUNDARY", "192.0.2.50")

	for range 2 {
		allowed, _, _, err := redisFixedWindowTake(context.Background(), key, 2, duration)
		require.NoError(t, err)
		assert.True(t, allowed)
	}
	allowed, _, _, err := redisFixedWindowTake(context.Background(), key, 2, duration)
	require.NoError(t, err)
	assert.False(t, allowed)

	// This reset is intentional fixed-window behavior. A client can consume one
	// full allowance immediately before and another immediately after a boundary.
	redisServer.FastForward(time.Duration(duration) * time.Second)
	for range 2 {
		allowed, _, _, err = redisFixedWindowTake(context.Background(), key, 2, duration)
		require.NoError(t, err)
		assert.True(t, allowed)
	}
}

func TestRedisFixedWindowRepairsCounterWithoutTTL(t *testing.T) {
	redisServer, _ := useRateLimitMiniRedis(t)
	const duration = int64(29)
	key := redisIPRateLimitKey("MISSING-TTL", "192.0.2.51")
	redisServer.Set(key, "5")

	allowed, count, ttl, err := redisFixedWindowTake(context.Background(), key, 3, duration)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, int64(6), count)
	assert.Equal(t, duration, ttl)
	assert.Equal(t, time.Duration(duration)*time.Second, redisServer.TTL(key))

	redisServer.FastForward(time.Duration(duration) * time.Second)
	assert.False(t, redisServer.Exists(key), "a recovered counter must not remain permanently rate-limited")
}

func TestRedisFailurePolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, redisClient := useRateLimitMiniRedis(t)
	require.NoError(t, redisClient.Close())

	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.GET("/ip", rateLimitFactory(1, 30, "FAIL-IP"), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	router.GET(
		"/user",
		func(c *gin.Context) { c.Set("id", 7) },
		userRateLimitFactory(1, 30, "FAIL-USER"),
		func(c *gin.Context) { c.Status(http.StatusNoContent) },
	)
	router.GET("/email", EmailVerificationRateLimit(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	ipResponse := performRateLimitRequest(router, "/ip", "192.0.2.60:12345")
	assert.Equal(t, http.StatusInternalServerError, ipResponse.Code)
	assert.Empty(t, ipResponse.Body.String())
	userResponse := performRateLimitRequest(router, "/user", "192.0.2.61:12345")
	assert.Equal(t, http.StatusInternalServerError, userResponse.Code)
	assert.Empty(t, userResponse.Body.String())
	assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/email", "192.0.2.62:12345").Code)
}

func TestGlobalWebRateLimitCountsFrontendFilesInStaticBucket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousWebEnable, previousWebNum, previousWebDuration := common.GlobalWebRateLimitEnable, common.GlobalWebRateLimitNum, common.GlobalWebRateLimitDuration
	previousStaticEnable, previousStaticNum, previousStaticDuration := common.GlobalStaticRateLimitEnable, common.GlobalStaticRateLimitNum, common.GlobalStaticRateLimitDuration
	t.Cleanup(func() {
		common.GlobalWebRateLimitEnable, common.GlobalWebRateLimitNum, common.GlobalWebRateLimitDuration = previousWebEnable, previousWebNum, previousWebDuration
		common.GlobalStaticRateLimitEnable, common.GlobalStaticRateLimitNum, common.GlobalStaticRateLimitDuration = previousStaticEnable, previousStaticNum, previousStaticDuration
	})
	common.GlobalWebRateLimitEnable, common.GlobalWebRateLimitNum, common.GlobalWebRateLimitDuration = true, 2, 41
	common.GlobalStaticRateLimitNum, common.GlobalStaticRateLimitDuration = 2, 43

	frontendDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(frontendDir, "static", "js"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(frontendDir, "static", "js", "app.js"), []byte("app"), 0o644))
	frontendFS := static.LocalFile(frontendDir, false)

	const clientIP = "192.0.2.70"
	const remoteAddr = clientIP + ":12345"

	t.Run("static limiter disabled by default", func(t *testing.T) {
		redisServer, _ := useRateLimitMiniRedis(t)
		common.GlobalStaticRateLimitEnable = false
		router := gin.New()
		require.NoError(t, router.SetTrustedProxies(nil))
		router.NoRoute(GlobalWebRateLimit(frontendFS), func(c *gin.Context) { c.Status(http.StatusNoContent) })

		for range 5 {
			assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/static/js/app.js", remoteAddr).Code)
		}
		assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/channels", remoteAddr).Code)
		assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/static/js/missing.js", remoteAddr).Code)
		pageResponse := performRateLimitRequest(router, "/channels", remoteAddr)
		assert.Equal(t, http.StatusTooManyRequests, pageResponse.Code)
		assert.Equal(t, "41", pageResponse.Header().Get("Retry-After"))

		webCount, err := redisServer.Get(redisIPRateLimitKey("GW", clientIP))
		require.NoError(t, err)
		assert.Equal(t, "3", webCount)
		assert.False(t, redisServer.Exists(redisIPRateLimitKey("GS", clientIP)))
	})

	t.Run("static limiter enabled uses its own bucket", func(t *testing.T) {
		redisServer, _ := useRateLimitMiniRedis(t)
		common.GlobalStaticRateLimitEnable = true
		router := gin.New()
		require.NoError(t, router.SetTrustedProxies(nil))
		router.NoRoute(GlobalWebRateLimit(frontendFS), func(c *gin.Context) { c.Status(http.StatusNoContent) })

		assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/static/js/app.js", remoteAddr).Code)
		assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/static/js/app.js", remoteAddr).Code)
		staticResponse := performRateLimitRequest(router, "/static/js/app.js", remoteAddr)
		assert.Equal(t, http.StatusTooManyRequests, staticResponse.Code)
		assert.Equal(t, "43", staticResponse.Header().Get("Retry-After"))
		assert.Equal(t, http.StatusNoContent, performRateLimitRequest(router, "/channels", remoteAddr).Code)

		staticCount, err := redisServer.Get(redisIPRateLimitKey("GS", clientIP))
		require.NoError(t, err)
		assert.Equal(t, "3", staticCount)
		webCount, err := redisServer.Get(redisIPRateLimitKey("GW", clientIP))
		require.NoError(t, err)
		assert.Equal(t, "1", webCount)
	})
}
