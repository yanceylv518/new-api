package common

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagementRateLimitDefaultsAreValid(t *testing.T) {
	encoded, err := Marshal(DefaultManagementRateLimits())
	require.NoError(t, err)
	_, err = ParseManagementRateLimits(string(encoded))
	require.NoError(t, err)
}

func TestManagementRateLimitRequiresExplicitBoolean(t *testing.T) {
	encoded, err := Marshal(DefaultManagementRateLimits())
	require.NoError(t, err)
	for _, test := range []struct {
		name  string
		omit  bool
		value any
	}{
		{"missing", true, nil}, {"null", false, nil}, {"string", false, "true"}, {"number", false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value map[string]map[string]any
			require.NoError(t, Unmarshal(encoded, &value))
			if test.omit {
				delete(value["global_api"], "enabled")
			} else {
				value["global_api"]["enabled"] = test.value
			}
			invalid, err := Marshal(value)
			require.NoError(t, err)
			_, err = ParseManagementRateLimits(string(invalid))
			require.Error(t, err)
		})
	}
}

func TestManagementRateLimitPreservesLongEnvironmentDefaults(t *testing.T) {
	oldAPIRequests, oldAPIWindow := GlobalApiRateLimitNum, GlobalApiRateLimitDuration
	oldCriticalRequests, oldCriticalWindow, oldCriticalEnabled := CriticalRateLimitNum, CriticalRateLimitDuration, CriticalRateLimitEnable
	t.Cleanup(func() {
		GlobalApiRateLimitNum, GlobalApiRateLimitDuration = oldAPIRequests, oldAPIWindow
		CriticalRateLimitNum, CriticalRateLimitDuration, CriticalRateLimitEnable = oldCriticalRequests, oldCriticalWindow, oldCriticalEnabled
	})
	GlobalApiRateLimitNum, GlobalApiRateLimitDuration = 2_000_000, 3600
	CriticalRateLimitNum, CriticalRateLimitDuration, CriticalRateLimitEnable = 0, 86400, false
	settings := DefaultManagementRateLimits()
	assert.Equal(t, 2_000_000, settings.GlobalAPI.Requests)
	assert.EqualValues(t, 3600, settings.GlobalAPI.WindowSeconds)
	assert.EqualValues(t, 86400, settings.LoginIP.WindowSeconds)
	assert.False(t, settings.LoginIP.Enabled)
	assert.Positive(t, settings.LoginIP.Requests)
	encoded, err := Marshal(settings)
	require.NoError(t, err)
	_, err = ParseManagementRateLimits(string(encoded))
	require.NoError(t, err)
}

func TestInMemoryRateLimiterKeepsLongWindowsDuringIdleCleanup(t *testing.T) {
	var limiter InMemoryRateLimiter
	limiter.Init(0)
	limiter.expirationDuration = 20 * time.Minute
	now := time.Now()
	require.True(t, limiter.Request("long-window", 1, 3600))
	long := limiter.store["long-window"]
	long.lastActive = now.Add(-30 * time.Minute)
	long.requests.head.timestamp = long.lastActive.Unix()
	require.True(t, limiter.Request("short-window", 1, 60))
	short := limiter.store["short-window"]
	short.lastActive = now.Add(-25 * time.Minute)
	short.requests.head.timestamp = short.lastActive.Unix()
	limiter.deleteExpiredEntries(now)
	assert.NotContains(t, limiter.store, "short-window")
	assert.False(t, limiter.Request("long-window", 1, 3600))
	long.lastActive = now.Add(-2 * time.Hour)
	long.requests.head.timestamp = long.lastActive.Unix()
	limiter.deleteExpiredEntries(now)
	assert.NotContains(t, limiter.store, "long-window")
}

func TestInMemoryRateLimiterReservationRetainsItsWindowAfterEviction(t *testing.T) {
	var limiter InMemoryRateLimiter
	limiter.Init(0)
	limiter.expirationDuration = 20 * time.Minute
	reservation := limiter.Reserve("reserved", 1, 3600)
	require.NotNil(t, reservation)
	limiter.store["reserved"].lastActive = time.Now().Add(-2 * time.Hour)
	limiter.deleteExpiredEntries(time.Now())
	assert.NotContains(t, limiter.store, "reserved")
	reservation.Complete(true)
	entry := limiter.store["reserved"]
	entry.lastActive = time.Now().Add(-30 * time.Minute)
	entry.requests.head.timestamp = entry.lastActive.Unix()
	limiter.deleteExpiredEntries(time.Now())
	assert.False(t, limiter.Request("reserved", 1, 3600))
}

func TestManagementRateLimitRejectsInvalidSettingsWithoutReplacingSnapshot(t *testing.T) {
	previous := managementRateLimits.Load()
	t.Cleanup(func() { managementRateLimits.Store(previous) })
	settings := DefaultManagementRateLimits()
	settings.TokenQuery = ManagementRateLimit{Enabled: true, Requests: 3, WindowSeconds: 60}
	encoded, err := Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, LoadManagementRateLimits(string(encoded)))
	for _, raw := range []string{"invalid", "null", "{}"} {
		require.Error(t, LoadManagementRateLimits(raw))
		assert.Equal(t, settings, GetManagementRateLimits())
	}
	for _, mutate := range []func(*ManagementRateLimits){
		func(value *ManagementRateLimits) { value.LoginAccount.Requests = 0 },
		func(value *ManagementRateLimits) { value.TokenQuery.Requests = MaxManagementRateLimitRequests + 1 },
		func(value *ManagementRateLimits) { value.SessionRefresh.WindowSeconds = -1 },
		func(value *ManagementRateLimits) {
			value.GlobalAPI.WindowSeconds = MaxManagementRateLimitWindowSeconds + 1
		},
	} {
		invalid := settings
		mutate(&invalid)
		encoded, err := Marshal(invalid)
		require.NoError(t, err)
		require.Error(t, LoadManagementRateLimits(string(encoded)))
		assert.Equal(t, settings, GetManagementRateLimits())
	}
}

func TestScopedMemoryRateLimitReturnsRetryAfterAndIsolatesKeys(t *testing.T) {
	previous := RedisEnabled
	RedisEnabled = false
	t.Cleanup(func() { RedisEnabled = previous })
	key := t.Name()
	require.NoError(t, CheckRateLimit(context.Background(), key, 1, 60))
	var limited *RateLimitError
	require.ErrorAs(t, CheckRateLimit(context.Background(), key, 1, 60), &limited)
	assert.EqualValues(t, 60, limited.RetryAfterSeconds)
	require.NoError(t, CheckRateLimit(context.Background(), key+":another-account", 1, 60))
}

func TestInMemoryRateLimiterAllocatesTimestampsOnDemand(t *testing.T) {
	var limiter InMemoryRateLimiter
	limiter.Init(0)

	require.True(t, limiter.Request("client", 1_000_000, 60))

	entry := limiter.store["client"]
	require.NotNil(t, entry)
	assert.Equal(t, 1, entry.requests.length)
	assert.Same(t, entry.requests.head, entry.requests.tail)
	assert.Nil(t, entry.requests.head.next)

	limiter.deleteExpiredEntries(time.Now().Add(time.Hour))
	assert.Contains(t, limiter.store, "client")
}

func TestInMemoryRateLimiterEnforcesWindowBoundary(t *testing.T) {
	var queue rateLimitQueue
	queue.append(100)
	queue.append(100)

	queue.removeExpired(159, 60)
	assert.Equal(t, 2, queue.length)

	queue.removeExpired(160, 60)
	assert.Zero(t, queue.length)
	assert.Nil(t, queue.head)
	assert.Nil(t, queue.tail)
}

func TestInMemoryRateLimiterRemovesAllExpiredRequestsPerCall(t *testing.T) {
	var queue rateLimitQueue
	for range 5 {
		queue.append(100)
	}
	queue.append(110)

	queue.removeExpired(110, 10)
	assert.Equal(t, 1, queue.length)
	require.NotNil(t, queue.head)
	assert.EqualValues(t, 110, queue.head.timestamp)
	assert.Same(t, queue.head, queue.tail)
}

func TestInMemoryRateLimiterKeepsNonExpiredRequests(t *testing.T) {
	var queue rateLimitQueue
	for range 3 {
		queue.append(100)
	}

	queue.removeExpired(109, 10)
	assert.Equal(t, 3, queue.length)
	assert.NotNil(t, queue.head)
	assert.NotNil(t, queue.tail)
}

func TestInMemoryRateLimiterEvictsOnlyExpiredLRUTail(t *testing.T) {
	var limiter InMemoryRateLimiter
	limiter.Init(0)
	limiter.expirationDuration = 10 * time.Second

	require.True(t, limiter.Request("active", 1, 10))
	require.True(t, limiter.Request("idle", 1, 10))
	assert.False(t, limiter.Request("active", 1, 10))

	assert.Equal(t, "active", limiter.lru.Front().Value.(*rateLimitEntry).key)
	assert.Equal(t, "idle", limiter.lru.Back().Value.(*rateLimitEntry).key)

	idleEntry := limiter.store["idle"]
	now := time.Now()
	idleEntry.lastActive = now.Add(-10 * time.Second)
	limiter.store["active"].lastActive = now
	limiter.deleteExpiredEntries(now)

	assert.NotContains(t, limiter.store, "idle")
	assert.Contains(t, limiter.store, "active")
	assert.Equal(t, 1, limiter.lru.Len())
	assert.Nil(t, idleEntry.element)
	assert.Nil(t, idleEntry.requests.head)
	assert.Nil(t, idleEntry.requests.tail)
	assert.Zero(t, idleEntry.requests.length)
}

func TestInMemoryRateLimiterCleanupTicksEvictExpiredKeys(t *testing.T) {
	var limiter InMemoryRateLimiter
	limiter.Init(0)
	limiter.expirationDuration = 10 * time.Second
	require.True(t, limiter.Request("idle", 1, 10))

	now := time.Now()
	limiter.store["idle"].lastActive = now.Add(-10 * time.Second)
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		limiter.clearExpiredItems(ticks)
		close(done)
	}()

	ticks <- now
	close(ticks)
	<-done

	assert.Empty(t, limiter.store)
	assert.Zero(t, limiter.lru.Len())
}

func TestInMemoryRateLimiterInitKeepsFirstConfiguration(t *testing.T) {
	var limiter InMemoryRateLimiter
	limiter.Init(time.Hour)
	limiter.Init(0)

	assert.Equal(t, time.Hour, limiter.expirationDuration)
	require.True(t, limiter.Request("client", 1, 60))
	limiter.deleteExpiredEntries(time.Now().Add(time.Hour))
	assert.NotContains(t, limiter.store, "client")
}

func TestInMemoryRateLimiterConcurrentInitializationAndRequests(t *testing.T) {
	var limiter InMemoryRateLimiter
	var allowed atomic.Int64
	var waitGroup sync.WaitGroup

	for range 100 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			limiter.Init(0)
			if limiter.Request("client", 10, 60) {
				allowed.Add(1)
			}
		}()
	}

	waitGroup.Wait()
	assert.EqualValues(t, 10, allowed.Load())
	assert.Len(t, limiter.store, 1)
	assert.Equal(t, 10, limiter.store["client"].requests.length)
}
