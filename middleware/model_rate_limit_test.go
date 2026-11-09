package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var memoryLimitTestUser atomic.Int64

func TestMemorySuccessLimitSharedBetweenSearchAndChat(t *testing.T) {
	const search, chat = "/v1/web_search", "/v1/chat/completions"
	for _, order := range [][2]string{{search, chat}, {chat, search}} {
		engine := gin.New()
		userID := -int(memoryLimitTestUser.Add(1))
		engine.Use(func(c *gin.Context) { c.Set("id", userID) }, memoryRateLimitHandler(60, 0, 1))
		for _, path := range order {
			engine.POST(path, func(c *gin.Context) { c.Status(http.StatusOK) })
		}
		for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
			out := httptest.NewRecorder()
			engine.ServeHTTP(out, httptest.NewRequest(http.MethodPost, order[i], nil))
			require.Equal(t, want, out.Code, "order %v", order)
		}
	}
}

func TestMemorySuccessLimitReleasesFailedRequests(t *testing.T) {
	router := gin.New()
	userID := -int(memoryLimitTestUser.Add(1))
	router.Use(func(c *gin.Context) { c.Set("id", userID) }, memoryRateLimitHandler(60, 0, 1))
	entered, release := make(chan struct{}), make(chan struct{})
	router.GET("/search", func(c *gin.Context) {
		if c.Query("result") == "error" {
			close(entered)
			<-release
			common.SetContextKey(c, constant.ContextKeyZhipuSearchBusinessError, true)
		}
		c.Status(http.StatusOK)
	})
	request := func(path string) int {
		result := httptest.NewRecorder()
		router.ServeHTTP(result, httptest.NewRequest(http.MethodGet, path, nil))
		return result.Code
	}
	done := make(chan int, 1)
	go func() { done <- request("/search?result=error") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("first request did not reach the handler")
	}
	assert.Equal(t, http.StatusTooManyRequests, request("/search"))
	close(release)
	require.Equal(t, http.StatusOK, <-done)
	require.Equal(t, http.StatusOK, request("/search"))
	require.Equal(t, http.StatusTooManyRequests, request("/search"))
}

func TestModelRedisRateLimitUsesUTCRegardlessOfLocalTimezone(t *testing.T) {
	redisServer, redisClient := useRateLimitMiniRedis(t)
	previousLocation := time.Local
	time.Local = time.FixedZone("test-utc-plus-eight", 8*60*60)
	t.Cleanup(func() { time.Local = previousLocation })

	ctx := context.Background()
	recordKey := "rateLimit:model-utc-record"
	recordRedisRequest(ctx, redisClient, recordKey, 2)
	recorded, err := redisClient.LIndex(ctx, recordKey, 0).Result()
	require.NoError(t, err)
	recordedAt, err := time.Parse(modelRateLimitTimeFormat, recorded)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC(), recordedAt, 2*time.Second)

	checkKey := "rateLimit:model-utc-check"
	withinWindow := time.Now().UTC().Add(-30 * time.Second).Format(modelRateLimitTimeFormat)
	_, err = redisServer.Push(checkKey, withinWindow, withinWindow)
	require.NoError(t, err)
	allowed, err := checkRedisRateLimit(ctx, redisClient, checkKey, 2, 60)
	require.NoError(t, err)
	assert.False(t, allowed, "an existing UTC timestamp inside the window must remain limited on a non-UTC host")
}
