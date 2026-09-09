package model

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 每个用例独立初始化数据库、Redis 和本地快照，避免相同用户 ID 继承其他用例的缓存。
func setupUserModelPricingCacheTest(t *testing.T) User {
	t.Helper()
	setupUserUpdateTestState(t)
	require.NoError(t, DB.AutoMigrate(&UserModelPricing{}, &UserModelPricingRevision{}))
	previousCache := userModelPricingCache
	userModelPricingCache = &pricingSnapshotCache{}
	t.Cleanup(func() { userModelPricingCache = previousCache })
	user := User{Username: "pricing-cache-user", Password: "password"}
	require.NoError(t, DB.Create(&user).Error)
	return user
}

// 命中缓存必须省去规则查询，返回副本必须隔离请求之间的可变状态，空规则也要缓存。
func TestUserModelPricingCacheHitAndEmptyRules(t *testing.T) {
	for _, discounts := range []map[string]int{{"gpt-4o": 8000}, {}} {
		t.Run(fmt.Sprint(len(discounts)), func(t *testing.T) {
			user := setupUserModelPricingCacheTest(t)
			useUserCacheMiniRedis(t)
			_, err := ReplaceUserModelPricing(user.Id, discounts, 1)
			require.NoError(t, err)
			var queries atomic.Int32
			const callback = "test:pricing-query-count"
			require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				queries.Add(1)
			}))
			t.Cleanup(func() { _ = DB.Callback().Query().Remove(callback) })
			first, err := GetUserModelDiscountBPSContext(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Equal(t, discounts, first)
			first["gpt-4o"] = 1
			queries.Store(0)
			second, err := GetUserModelDiscountBPSContext(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Equal(t, discounts, second)
			assert.Zero(t, queries.Load(), "cache hit must not execute SQL queries")
		})
	}
}

// 改价和清空都递增版本；模拟迟到的旧快照后仍必须按新版本读取。
func TestUserModelPricingCacheRejectsStaleSnapshotAfterReplace(t *testing.T) {
	user := setupUserModelPricingCacheTest(t)
	useUserCacheMiniRedis(t)
	_, err := ReplaceUserModelPricing(user.Id, map[string]int{"gpt-4o": 8000}, 1)
	require.NoError(t, err)
	_, err = GetUserModelDiscountBPS(user.Id)
	require.NoError(t, err)
	key := fmt.Sprintf("%p:%d", DB, user.Id)
	userModelPricingCache.Lock()
	stale, exists := userModelPricingCache.entries.Peek(key)
	userModelPricingCache.Unlock()
	require.True(t, exists)
	for index, discounts := range []map[string]int{{"gpt-4o": 6000}, {}} {
		_, err := ReplaceUserModelPricing(user.Id, discounts, int64(index+2))
		require.NoError(t, err)
		userModelPricingCache.Lock()
		userModelPricingCache.entries.Set(key, stale)
		userModelPricingCache.usedBytes = stale.discounts.EstimatedBytes()
		userModelPricingCache.Unlock()
		got, err := GetUserModelDiscountBPS(user.Id)
		require.NoError(t, err)
		assert.Equal(t, discounts, got)
	}
}

// 旧请求持有的快照在改价、清空和缓存淘汰后仍保持原价，新请求必须看到新版本。
func TestUserModelPricingImmutableRequestSnapshot(t *testing.T) {
	user := setupUserModelPricingCacheTest(t)
	useUserCacheMiniRedis(t)
	_, err := ReplaceUserModelPricing(user.Id, map[string]int{"model": 8000}, 1)
	require.NoError(t, err)
	first, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	mutable := first.Copy()
	mutable["model"] = 1
	_, err = ReplaceUserModelPricing(user.Id, map[string]int{"model": 6000}, 2)
	require.NoError(t, err)
	second, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	_, err = ReplaceUserModelPricing(user.Id, map[string]int{}, 3)
	require.NoError(t, err)
	cleared, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, 8000, first.DiscountBPS("model"))
	assert.Equal(t, 6000, second.DiscountBPS("model"))
	assert.Zero(t, cleared.DiscountBPS("model"))
}

// 合并读取只能省去往返，不能绕过禁用屏障、使用错误用户 Hash 或冻结旧折扣版本。
func TestUserModelPricingPipelinePreservesAuthAndPricing(t *testing.T) {
	user := setupUserModelPricingCacheTest(t)
	server := useUserCacheMiniRedis(t)
	user.Status, user.AuthVersion = common.UserStatusEnabled, 1
	require.NoError(t, DB.Model(&user).Updates(map[string]interface{}{"status": user.Status, "auth_version": user.AuthVersion}).Error)
	_, err := ReplaceUserModelPricing(user.Id, map[string]int{"model": 8000}, 1)
	require.NoError(t, err)
	_, first, err := GetUserCacheWithModelDiscounts(t.Context(), user.Id)
	require.NoError(t, err)
	commands := server.CommandCount()
	cached, second, err := GetUserCacheWithModelDiscounts(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, user.Id, cached.Id)
	assert.Equal(t, 8000, second.DiscountBPS("model"))
	assert.Equal(t, 2, server.CommandCount()-commands, "warm read uses HGETALL and a combined MGET")
	_, err = ReplaceUserModelPricing(user.Id, map[string]int{"model": 6000}, 2)
	require.NoError(t, err)
	_, changed, err := GetUserCacheWithModelDiscounts(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, 6000, changed.DiscountBPS("model"))
	assert.Equal(t, 8000, first.DiscountBPS("model"))
	server.HSet(getUserCacheKey(user.Id), "Id", "99999")
	cached, _, err = GetUserCacheWithModelDiscounts(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, user.Id, cached.Id)
	require.NoError(t, SetUserAuthVersionFence(user.Id, 2))
	_, _, err = GetUserCacheWithModelDiscounts(t.Context(), user.Id)
	require.ErrorIs(t, err, ErrUserAuthCachePending)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	commands = server.CommandCount()
	_, _, err = GetUserCacheWithModelDiscounts(ctx, user.Id)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, commands, server.CommandCount())
}

// 用户数和估算字节预算均驱逐最久未访问的快照，过期和超大快照不能继续占用预算。
func TestUserModelPricingCacheLRUAndBudget(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		t.Run(fmt.Sprint(byBytes), func(t *testing.T) {
			t.Setenv("USER_MODEL_PRICING_CACHE_USERS", "2")
			if byBytes {
				t.Setenv("USER_MODEL_PRICING_CACHE_USERS", "10")
			}
			t.Setenv("USER_MODEL_PRICING_CACHE_MB", "1")
			cache := &pricingSnapshotCache{}
			cache.initLocked()
			snapshot := userModelPricingSnapshot{discounts: hosttypes.NewUserModelDiscountSnapshot(map[string]int{"model": 8000}), revision: 1, expiresAt: time.Now().Add(time.Hour)}
			if byBytes {
				cache.maxBytes = 2 * snapshot.discounts.EstimatedBytes()
			}
			cache.put("a", snapshot)
			cache.put("b", snapshot)
			_, ok := cache.get("a", 1)
			require.True(t, ok)
			cache.put("c", snapshot)
			_, ok = cache.get("b", 1)
			assert.False(t, ok, "least recently used entry must be evicted")
			_, ok = cache.get("a", 1)
			assert.True(t, ok)
			assert.LessOrEqual(t, cache.usedBytes, cache.maxBytes)
			// 不能被迟到回源覆盖的新版本，即使是原价空快照也有预算成本。
			newer := userModelPricingSnapshot{discounts: hosttypes.NewUserModelDiscountSnapshot(map[string]int{}), revision: 2, expiresAt: snapshot.expiresAt}
			cache.put("a", newer)
			cache.put("a", snapshot)
			got, ok := cache.get("a", 2)
			require.True(t, ok)
			assert.Zero(t, got.discounts.DiscountBPS("model"))
			expired := snapshot
			expired.expiresAt = time.Now().Add(-time.Second)
			cache.put("expired", expired)
			_, ok = cache.get("expired", 1)
			assert.False(t, ok)
			cache.maxBytes = 1
			cache.put("oversized", snapshot)
			_, ok = cache.get("oversized", 1)
			assert.False(t, ok)
		})
	}
}

// 两个配置开关都可关闭缓存；非法输入按已记录的默认配置继续提供缓存。
func TestUserModelPricingCacheConfiguration(t *testing.T) {
	for _, testCase := range []struct {
		name, users, megabytes string
		cached                 bool
	}{
		{"users disabled", "0", "64", false},
		{"memory disabled", "4096", "0", false},
		{"invalid negative", "-1", "-1", true},
		{"invalid excessive", "65537", "1025", true},
		{"invalid text", "invalid", "invalid", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("USER_MODEL_PRICING_CACHE_USERS", testCase.users)
			t.Setenv("USER_MODEL_PRICING_CACHE_MB", testCase.megabytes)
			cache := &pricingSnapshotCache{}
			cache.put("user", userModelPricingSnapshot{
				discounts: hosttypes.NewUserModelDiscountSnapshot(map[string]int{"model": 8000}),
				revision:  1, expiresAt: time.Now().Add(time.Hour),
			})
			_, found := cache.get("user", 1)
			assert.Equal(t, testCase.cached, found)
		})
	}
}

// 显式关闭本地缓存时仍读取权威价格，不能因为不缓存而回退成原价。
func TestUserModelPricingCacheCanBeDisabled(t *testing.T) {
	t.Setenv("USER_MODEL_PRICING_CACHE_USERS", "0")
	user := setupUserModelPricingCacheTest(t)
	useUserCacheMiniRedis(t)
	_, err := ReplaceUserModelPricing(user.Id, map[string]int{"model": 8000}, 1)
	require.NoError(t, err)
	var queries atomic.Int32
	const callback = "test:disabled-pricing-cache"
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "user_model_pricings" {
			queries.Add(1)
		}
	}))
	t.Cleanup(func() { _ = DB.Callback().Query().Remove(callback) })
	for range 2 {
		snapshot, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
		require.NoError(t, err)
		assert.Equal(t, 8000, snapshot.DiscountBPS("model"))
	}
	assert.EqualValues(t, 2, queries.Load())
}

// Redis 故障不能放行旧价；无法建立写入屏障时必须回滚，数据库也故障时返回错误。
func TestUserModelPricingRedisFailureFallsBackAndRejectsUnsafeWrite(t *testing.T) {
	user := setupUserModelPricingCacheTest(t)
	server := useUserCacheMiniRedis(t)
	_, err := ReplaceUserModelPricing(user.Id, map[string]int{"gpt-4o": 8000}, 1)
	require.NoError(t, err)
	_, err = GetUserModelDiscountBPS(user.Id)
	require.NoError(t, err)
	server.SetError("ERR unavailable")
	_, err = ReplaceUserModelPricing(user.Id, map[string]int{"gpt-4o": 6000}, 2)
	require.Error(t, err)
	got, revision, err := GetUserModelPricing(user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 2, revision)
	assert.Equal(t, map[string]int{"gpt-4o": 8000}, got)
	got, err = GetUserModelDiscountBPS(user.Id)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"gpt-4o": 8000}, got)
	const callback = "test:pricing-db-failure"
	failure := errors.New("database unavailable")
	require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) { tx.AddError(failure) }))
	t.Cleanup(func() { _ = DB.Callback().Query().Remove(callback) })
	got, err = GetUserModelDiscountBPS(user.Id)
	require.ErrorIs(t, err, failure)
	assert.Nil(t, got)
}

// 屏障发布后的事务失败不能留下新价格，回滚屏障到期后可恢复正常命中。
func TestUserModelPricingRollbackFenceAndRedisReset(t *testing.T) {
	user := setupUserModelPricingCacheTest(t)
	server := useUserCacheMiniRedis(t)
	_, err := GetUserModelDiscountBPS(user.Id)
	require.NoError(t, err)
	const callback = "test:pricing-insert-failure"
	failure := errors.New("insert failed")
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "user_model_pricings" {
			tx.AddError(failure)
		}
	}))
	_, err = ReplaceUserModelPricing(user.Id, map[string]int{"gpt-4o": 6000}, 1)
	require.NoError(t, DB.Callback().Create().Remove(callback))
	require.ErrorIs(t, err, failure)
	got, err := GetUserModelDiscountBPS(user.Id)
	require.NoError(t, err)
	assert.Empty(t, got)
	server.FastForward(userModelPricingFenceTTL)
	server.FlushAll()
	_, err = ReplaceUserModelPricing(user.Id, map[string]int{"gpt-4o": 5000}, 1)
	require.NoError(t, err)
	server.FlushAll()
	got, err = GetUserModelDiscountBPS(user.Id)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"gpt-4o": 5000}, got)
}

// 即使命中缓存也尊重取消；数据库连接池耗尽时必须在调用方截止时间内结束等待。
func TestUserModelPricingContextCancellation(t *testing.T) {
	user := setupUserModelPricingCacheTest(t)
	useUserCacheMiniRedis(t)
	_, err := GetUserModelDiscountBPS(user.Id)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = GetUserModelDiscountBPSContext(ctx, user.Id)
	require.ErrorIs(t, err, context.Canceled)
	sqlDB, err := DB.DB()
	require.NoError(t, err)
	connection, err := sqlDB.Conn(t.Context())
	require.NoError(t, err)
	defer connection.Close()
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, _, err = GetUserModelPricingContext(ctx, user.Id)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = ReplaceUserModelPricingContext(ctx, user.Id, map[string]int{}, 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// 共享回源的取消错误只允许仍有预算的请求重试一次，持续取消不能形成无限重试。
func TestUserModelPricingRetriesSharedCancellationOnce(t *testing.T) {
	for _, failures := range []int32{1, 2} {
		t.Run(fmt.Sprint(failures), func(t *testing.T) {
			user := setupUserModelPricingCacheTest(t)
			var queries atomic.Int32
			const callback = "test:pricing-load-cancellation"
			require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if queries.Add(1) <= failures {
					tx.AddError(context.Canceled)
				}
			}))
			t.Cleanup(func() { _ = DB.Callback().Query().Remove(callback) })
			got, err := GetUserModelDiscountBPSContext(t.Context(), user.Id)
			if failures == 1 {
				require.NoError(t, err)
				assert.Empty(t, got)
			} else {
				require.ErrorIs(t, err, context.Canceled)
				assert.Nil(t, got)
				assert.EqualValues(t, 2, queries.Load())
			}
		})
	}
}
