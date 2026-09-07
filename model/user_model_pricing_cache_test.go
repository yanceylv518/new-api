package model

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 每个用例独立初始化数据库、Redis 和本地快照，避免相同用户 ID 继承其他用例的缓存。
func setupUserModelPricingCacheTest(t *testing.T) User {
	t.Helper()
	setupUserUpdateTestState(t)
	userModelPricingCache.Lock()
	clear(userModelPricingCache.entries)
	userModelPricingCache.Unlock()
	user := User{Username: "pricing-cache-user", Password: "password", ModelPricingVersion: 1}
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
	stale := userModelPricingCache.entries[key]
	userModelPricingCache.Unlock()
	for index, discounts := range []map[string]int{{"gpt-4o": 6000}, {}} {
		_, err := ReplaceUserModelPricing(user.Id, discounts, int64(index+2))
		require.NoError(t, err)
		userModelPricingCache.Lock()
		userModelPricingCache.entries[key] = stale
		userModelPricingCache.Unlock()
		got, err := GetUserModelDiscountBPS(user.Id)
		require.NoError(t, err)
		assert.Equal(t, discounts, got)
	}
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

// 模拟旧的大小写不敏感索引，迁移后不同大小写可分别计价，同一规范名仍不可重复。
func TestUserModelPricingCaseSensitiveIndexMigration(t *testing.T) {
	oldDB := DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	DB = db
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { DB = oldDB; _ = sqlDB.Close() })
	require.NoError(t, db.Exec("CREATE TABLE `user_model_pricings` (`id` integer PRIMARY KEY, `user_id` integer NOT NULL, `model_name` varchar(128) NOT NULL, `discount_bps` integer NOT NULL)").Error)
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_user_model_pricing_user_model ON user_model_pricings(user_id, model_name COLLATE NOCASE)").Error)
	require.NoError(t, db.Exec("INSERT INTO user_model_pricings(user_id, model_name, discount_bps) VALUES (?, ?, ?)", 1, "Model-A", 8000).Error)
	require.NoError(t, db.AutoMigrate(&UserModelPricing{}))
	require.NoError(t, InitializeUserModelPricingKeys())
	require.NoError(t, InitializeUserModelPricingKeys())
	require.NoError(t, db.Create(&UserModelPricing{UserId: 1, ModelName: "model-a", DiscountBPS: 6000}).Error)
	require.Error(t, db.Create(&UserModelPricing{UserId: 1, ModelName: "Model-A", DiscountBPS: 5000}).Error)
	got, err := readUserModelPricing(db, 1)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"Model-A": 8000, "model-a": 6000}, got)
}
