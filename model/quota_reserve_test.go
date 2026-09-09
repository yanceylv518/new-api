package model

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 任务事务只能同步本次差额，不能抹掉普通请求和任务预扣尚未刷盘的缓存增量。
func TestTaskSettlementPreservesPendingBatchQuota(t *testing.T) {
	for _, target := range []int{0, 50, 150} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			truncateTables(t)
			resetBatchUpdateTestState(t)
			useUserCacheMiniRedis(t)
			common.BatchUpdateEnabled = true
			user := createReserveTestUser(t, 1000)
			token := createReserveTestToken(t, 1000)
			require.NoError(t, DB.Model(&token).Update("user_id", user.Id).Error)
			channel := Channel{Name: "task-pending", Key: "test-only"}
			require.NoError(t, DB.Create(&channel).Error)
			task := Task{TaskID: "pending-task", UserId: user.Id, ChannelId: channel.Id, Quota: 100, PrivateData: TaskPrivateData{TokenId: token.Id, DiscountAmounts: types.NewDiscountAmounts(200, 100)}}
			require.NoError(t, DB.Create(&task).Error)
			ok, err := TryReserveUserQuota(user.Id, 200)
			require.NoError(t, err)
			require.True(t, ok)
			ok, err = TryReserveTokenQuota(token.Id, token.Key, 200, false)
			require.NoError(t, err)
			require.True(t, ok)
			for range 2 {
				UpdateUserUsedQuotaAndRequestCount(user.Id, 100)
				UpdateChannelUsedQuota(channel.Id, 200)
			}
			// 先注入任务快照写入失败，资金事务回滚时不能提前更新任何缓存。
			const hook = "test:reject-task-quota"
			injected := errors.New("task snapshot unavailable")
			require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
				// 跳过 SQLite 的起始写锁，在资金已经变更后的最终快照写入处制造故障。
				if values, ok := tx.Statement.Dest.(map[string]any); ok && tx.Statement.Table == "tasks" {
					if _, writesSnapshot := values["private_data"]; writesSnapshot {
						tx.AddError(injected)
					}
				}
			}))
			_, err = CommitTaskSettlement(t.Context(), &task, target, types.NewDiscountAmounts(target*2, target))
			require.NoError(t, DB.Callback().Update().Remove(hook))
			require.ErrorIs(t, err, injected)
			cached, err := GetUserQuota(user.Id, false)
			require.NoError(t, err)
			assert.Equal(t, 800, cached)
			assert.Equal(t, 1000, getUserQuotaFromDB(t, user.Id))
			// 补扣、部分退款和全退均保留另一笔 100 的待刷盘预扣，同一目标重试不得再调整缓存。
			for attempt := range 2 {
				delta, err := CommitTaskSettlement(t.Context(), &task, target, types.NewDiscountAmounts(target*2, target))
				require.NoError(t, err)
				if attempt == 0 {
					assert.Equal(t, target-100, delta)
				} else {
					assert.Zero(t, delta)
				}
				cached, err = GetUserQuota(user.Id, false)
				require.NoError(t, err)
				assert.Equal(t, 900-target, cached)
				cachedToken, err := GetTokenByKey(token.Key, false)
				require.NoError(t, err)
				assert.Equal(t, 900-target, cachedToken.RemainQuota)
				assert.Equal(t, 100+target, cachedToken.UsedQuota)
			}
			for range 2 {
				batchUpdate()
			}
			var actual User
			require.NoError(t, DB.First(&actual, user.Id).Error)
			assert.Equal(t, 900-target, actual.Quota)
			assert.Equal(t, 100+target, actual.UsedQuota)
			assert.Equal(t, 2, actual.RequestCount)
			assert.Equal(t, 900-target, getTokenFromDB(t, token.Id).RemainQuota)
			require.NoError(t, DB.First(&channel, channel.Id).Error)
			assert.EqualValues(t, 200+2*target, channel.UsedQuota)
		})
	}
}

// 混合折扣结算和失败退款在同一用户、令牌、渠道上竞争；刷盘不能丢增量或重复扣款。
func TestDiscountConcurrentBatchAccounting(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true
	user := createReserveTestUser(t, 100000)
	token := createReserveTestToken(t, 100000)
	require.NoError(t, DB.Model(&token).Update("user_id", user.Id).Error)
	channel := Channel{Name: "discount-batch", Key: "test-only", Status: common.ChannelStatusEnabled, UsedQuota: 777}
	require.NoError(t, DB.Create(&channel).Error)
	// 预热完整缓存，随后并发读必须看到增量余额，不能用尚未刷盘的数据库覆盖。
	_, err := GetUserCache(user.Id)
	require.NoError(t, err)
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	var workers sync.WaitGroup
	results := make(chan error, 32)
	for index := range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			reserved, err := TryReserveTokenQuota(token.Id, token.Key, 1000, false)
			if err != nil || !reserved {
				results <- fmt.Errorf("token reservation: reserved=%v err=%v", reserved, err)
				return
			}
			reserved, err = TryReserveUserQuota(user.Id, 1000)
			if err != nil || !reserved {
				results <- fmt.Errorf("wallet reservation: reserved=%v err=%v", reserved, err)
				return
			}
			// 一半请求失败全退，一半按原价 1000 的 40% 结算并退还预扣差额。
			refund := 1000
			if index%2 == 0 {
				refund = 600
				UpdateUserUsedQuotaAndRequestCount(user.Id, 400)
				UpdateChannelUsedQuota(channel.Id, 1000)
			}
			if err := IncreaseUserQuota(user.Id, refund, false); err != nil {
				results <- err
				return
			}
			results <- IncreaseTokenQuota(token.Id, token.Key, refund)
		}()
	}
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.EventuallyWithT(t, func(check *assert.CollectT) {
		cachedUser, err := GetUserCache(user.Id)
		if assert.NoError(check, err) {
			assert.Equal(check, 93600, cachedUser.Quota)
		}
		cachedToken, err := GetTokenByKey(token.Key, false)
		if assert.NoError(check, err) {
			assert.Equal(check, 93600, cachedToken.RemainQuota)
			assert.Equal(check, 6400, cachedToken.UsedQuota)
		}
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 100000, getUserQuotaFromDB(t, user.Id), "batch deltas remain pending until flush")
	// 连续两次刷盘验证首次提交全部增量、再次执行不重复计费。
	for range 2 {
		batchUpdate()
		var actual User
		require.NoError(t, DB.First(&actual, user.Id).Error)
		assert.Equal(t, 93600, actual.Quota)
		assert.Equal(t, 6400, actual.UsedQuota)
		assert.Equal(t, 16, actual.RequestCount)
		actualToken := getTokenFromDB(t, token.Id)
		assert.Equal(t, 93600, actualToken.RemainQuota)
		assert.Equal(t, 6400, actualToken.UsedQuota)
		var actualChannel Channel
		require.NoError(t, DB.First(&actualChannel, channel.Id).Error)
		assert.EqualValues(t, 16777, actualChannel.UsedQuota)
	}
}

func createReserveTestUser(t *testing.T, quota int) User {
	t.Helper()
	user := User{
		Username:    "reserve-user-" + common.GetRandomString(6),
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
		Quota:       quota,
		AffCode:     "reserve-aff-" + common.GetRandomString(8),
	}
	require.NoError(t, DB.Create(&user).Error)
	return user
}

func createReserveTestToken(t *testing.T, remainQuota int) Token {
	t.Helper()
	token := Token{
		UserId:      1,
		Key:         "reserve-token-" + common.GetRandomString(8),
		Name:        "reserve-test",
		Status:      common.TokenStatusEnabled,
		ExpiredTime: -1,
		RemainQuota: remainQuota,
	}
	require.NoError(t, token.Insert())
	return token
}

func getUserQuotaFromDB(t *testing.T, id int) int {
	t.Helper()
	var user User
	require.NoError(t, DB.Select("quota").First(&user, id).Error)
	return user.Quota
}

func getTokenFromDB(t *testing.T, id int) Token {
	t.Helper()
	var token Token
	require.NoError(t, DB.First(&token, id).Error)
	return token
}

func resetBatchUpdateTestState(t *testing.T) {
	t.Helper()
	accountingBatchMu.Lock()
	pendingAccountingBatch = nil
	accountingBatchMu.Unlock()
	oldBatchEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateLocks[i].Lock()
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
	}
	t.Cleanup(func() {
		accountingBatchMu.Lock()
		pendingAccountingBatch = nil
		accountingBatchMu.Unlock()
		common.BatchUpdateEnabled = oldBatchEnabled
		for i := 0; i < BatchUpdateTypeCount; i++ {
			batchUpdateLocks[i].Lock()
			batchUpdateStores[i] = make(map[int]int)
			batchUpdateLocks[i].Unlock()
		}
	})
}

func TestTryReserveQuotaWithoutRedis(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)

	user := createReserveTestUser(t, 100)
	reserved, err := TryReserveUserQuota(user.Id, 60)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, 40, getUserQuotaFromDB(t, user.Id))

	reserved, err = TryReserveUserQuota(user.Id, 41)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 40, getUserQuotaFromDB(t, user.Id))

	token := createReserveTestToken(t, 80)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 25, false)
	require.NoError(t, err)
	assert.True(t, reserved)
	reloaded := getTokenFromDB(t, token.Id)
	assert.Equal(t, 55, reloaded.RemainQuota)
	assert.Equal(t, 25, reloaded.UsedQuota)

	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 56, false)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 55, getTokenFromDB(t, token.Id).RemainQuota)
}

func TestRedisBatchReserveNeverFallsBackToStaleDatabaseBalance(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true

	user := createReserveTestUser(t, 10)
	reserved, err := TryReserveUserQuota(user.Id, 8)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, 10, getUserQuotaFromDB(t, user.Id), "batch delta is not flushed yet")

	reserved, err = TryReserveUserQuota(user.Id, 3)
	require.NoError(t, err)
	assert.False(t, reserved, "stale DB balance must not authorize a second spend")
	cachedUser, err := GetUserCache(user.Id)
	require.NoError(t, err)
	assert.Equal(t, 2, cachedUser.Quota)

	token := createReserveTestToken(t, 9)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 7, false)
	require.NoError(t, err)
	assert.True(t, reserved)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 3, false)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 9, getTokenFromDB(t, token.Id).RemainQuota)

	batchUpdate()
	assert.Equal(t, 2, getUserQuotaFromDB(t, user.Id))
	reloadedToken := getTokenFromDB(t, token.Id)
	assert.Equal(t, 2, reloadedToken.RemainQuota)
	assert.Equal(t, 7, reloadedToken.UsedQuota)
}

func TestBatchUpdateAccumulatesTwoMaximumRequestCharges(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)
	common.BatchUpdateEnabled = true

	user := createReserveTestUser(t, common.MaxQuota*2+100)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, DecreaseUserQuota(user.Id, common.MaxQuota, false))
	require.NoError(t, DecreaseUserQuota(user.Id, common.MaxQuota, false))
	// 等待本用例的两次异步缓存增量完成，避免下一用例切换 Redis 配置时仍有读者。
	require.EventuallyWithT(t, func(check *assert.CollectT) {
		cached, err := GetUserCache(user.Id)
		if assert.NoError(check, err) {
			assert.Equal(check, 100, cached.Quota)
		}
	}, 5*time.Second, 10*time.Millisecond)

	batchUpdate()
	assert.Equal(t, 100, getUserQuotaFromDB(t, user.Id))
}

func TestBatchUpdateAccumulatorSaturatesOverflow(t *testing.T) {
	resetBatchUpdateTestState(t)

	addNewRecord(BatchUpdateTypeUserQuota, 1, math.MaxInt)
	addNewRecord(BatchUpdateTypeUserQuota, 1, 1)
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	assert.Equal(t, math.MaxInt, batchUpdateStores[BatchUpdateTypeUserQuota][1])
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()

	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	batchUpdateStores[BatchUpdateTypeUserQuota] = make(map[int]int)
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
	addNewRecord(BatchUpdateTypeUserQuota, 1, math.MinInt)
	addNewRecord(BatchUpdateTypeUserQuota, 1, -1)
	batchUpdateLocks[BatchUpdateTypeUserQuota].Lock()
	assert.Equal(t, math.MinInt, batchUpdateStores[BatchUpdateTypeUserQuota][1])
	batchUpdateLocks[BatchUpdateTypeUserQuota].Unlock()
}

func TestReserveFallsBackToDatabaseWhenRedisIsUnavailable(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 20)
	require.NoError(t, populateUserCache(user))
	server.Close()

	// Redis 故障时降级为数据库条件更新：服务保持可用且不会超扣。
	reserved, err := TryReserveUserQuota(user.Id, 5)
	require.NoError(t, err)
	assert.True(t, reserved)
	assert.Equal(t, 15, getUserQuotaFromDB(t, user.Id))

	reserved, err = TryReserveUserQuota(user.Id, 16)
	require.NoError(t, err)
	assert.False(t, reserved)
	assert.Equal(t, 15, getUserQuotaFromDB(t, user.Id))
}

func TestSynchronousReserveCompensatesCacheWhenPersistenceFails(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	useUserCacheMiniRedis(t)

	user := createReserveTestUser(t, 10)
	require.NoError(t, populateUserCache(user))
	require.NoError(t, DB.Delete(&user).Error)

	reserved, err := TryReserveUserQuota(user.Id, 6)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	cached, cacheErr := cacheGetUserBase(user.Id)
	require.NoError(t, cacheErr)
	assert.Equal(t, 10, cached.Quota)

	token := createReserveTestToken(t, 12)
	_, err = GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	require.NoError(t, DB.Delete(&token).Error)
	reserved, err = TryReserveTokenQuota(token.Id, token.Key, 7, false)
	assert.False(t, reserved)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	cachedToken, cacheErr := cacheGetTokenByKey(token.Key)
	require.NoError(t, cacheErr)
	assert.Equal(t, 12, cachedToken.RemainQuota)
	assert.Zero(t, cachedToken.UsedQuota)
}

func TestTokenCacheInitPreservesLiveQuotaAndFenceBlocksStaleSnapshot(t *testing.T) {
	truncateTables(t)
	resetBatchUpdateTestState(t)
	server := useUserCacheMiniRedis(t)

	token := createReserveTestToken(t, 100)
	loaded, err := GetTokenByKey(token.Key, true)
	require.NoError(t, err)
	stale := *loaded

	result, err := cacheApplyTokenQuotaDelta(token.Id, token.Key, -70)
	require.NoError(t, err)
	require.Equal(t, cacheQuotaOK, result)

	// 已存在的哈希只刷新 TTL：数据库快照不得覆盖已被原子预扣的余额。
	code, err := cacheInitToken(stale)
	require.NoError(t, err)
	assert.Equal(t, 2, code)
	cached, err := cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 30, cached.RemainQuota)

	// 变更期间：fence 删除缓存并拦截并发读者手中的过期快照。
	require.NoError(t, invalidateTokenCacheForMutation(token.Key))
	code, err = cacheInitToken(stale)
	require.NoError(t, err)
	assert.Zero(t, code, "the pre-mutation snapshot must not be published while fenced")
	_, err = cacheGetTokenByKey(token.Key)
	assert.Error(t, err)

	// fence 过期后可重新从数据库水合。
	server.FastForward(time.Duration(tokenCacheFenceSeconds+1) * time.Second)
	fresh, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 100, fresh.RemainQuota)
	cached, err = cacheGetTokenByKey(token.Key)
	require.NoError(t, err)
	assert.Equal(t, 100, cached.RemainQuota)
}
