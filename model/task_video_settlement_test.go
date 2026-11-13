package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestVideoQuotaCacheLateCompletionKeepsNewOperation(t *testing.T) {
	address := os.Getenv("TASK_VIDEO_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("TASK_VIDEO_TEST_REDIS_ADDR is not configured")
	}
	host, _, err := net.SplitHostPort(address)
	require.NoError(t, err)
	require.True(t, net.ParseIP(host).IsLoopback())
	require.True(t, common.UsingMainDatabase(common.DatabaseTypeMySQL))

	const userID = 974301
	const tokenKey = "video-operation-fixture"
	client := redis.NewClient(&redis.Options{Addr: address})
	previousClient, previousEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	keys := []string{getUserCacheKey(userID), getTokenCacheKey(tokenKey), videoUserQuotaKey(userID), videoTokenQuotaKey(tokenKey)}
	t.Cleanup(func() {
		assert.NoError(t, client.Del(context.Background(), keys...).Err())
		common.RDB, common.RedisEnabled = previousClient, previousEnabled
		assert.NoError(t, client.Close())
		assert.NoError(t, DB.Where("user_id = ?", userID).Delete(&Task{}).Error)
		assert.NoError(t, DB.Unscoped().Where("user_id = ? AND `key` = ?", userID, tokenKey).Delete(&Token{}).Error)
		assert.NoError(t, DB.Unscoped().Where("id = ?", userID).Delete(&User{}).Error)
	})
	require.NoError(t, client.Del(t.Context(), keys...).Err())
	require.NoError(t, DB.Unscoped().Where("user_id = ? AND `key` = ?", userID, tokenKey).Delete(&Token{}).Error)
	require.NoError(t, DB.Create(&User{Id: userID, Username: "video-operation-fixture", AffCode: "video-operation-fixture", Quota: 920}).Error)
	require.NoError(t, DB.Create(&Token{UserId: userID, Key: tokenKey, RemainQuota: 920, UsedQuota: 80}).Error)
	task := &Task{TaskID: "video-operation-fixture", UserId: userID, Quota: 80, Status: TaskStatusSubmitted, PrivateData: TaskPrivateData{SubmissionFundsCommitted: true}}
	require.NoError(t, DB.Create(task).Error)
	require.NoError(t, client.HSet(t.Context(), keys[0], "Quota", 900).Err())
	require.NoError(t, client.HSet(t.Context(), keys[1], "RemainQuota", 900, "UsedQuota", 100).Err())

	first := *task
	first.Quota = 100
	firstKind, err := prepareVideoQuotaCache(t.Context(), DB, &first, 80, true, tokenKey)
	require.NoError(t, err)
	require.NoError(t, finishVideoQuotaCache(t.Context(), DB, userID))
	require.Equal(t, "920", client.HGet(t.Context(), keys[0], "Quota").Val())

	secondKind, err := prepareVideoQuotaCache(t.Context(), DB, task, 100, true, tokenKey)
	require.NoError(t, err)
	require.NotEqual(t, firstKind, secondKind)
	require.NoError(t, DB.Model(task).Updates(map[string]any{"quota": 100, "status": TaskStatusSuccess}).Error)
	late, err := finishVideoQuota.Run(t.Context(), client, keys, task.ID, 20, "1", keys[1], firstKind).Int()
	require.NoError(t, err)
	assert.Zero(t, late)
	require.NoError(t, applyVideoQuotaCache(t.Context(), userID, task.ID, -20, "1", keys[1], secondKind))
	assert.Equal(t, "900", client.HGet(t.Context(), keys[0], "Quota").Val())
	assert.Equal(t, "900", client.HGet(t.Context(), keys[1], "RemainQuota").Val())
	assert.Equal(t, "100", client.HGet(t.Context(), keys[1], "UsedQuota").Val())
}

func TestVideoSettlementUsesKnownChannelBaselineWithoutDiscountSnapshot(t *testing.T) {
	require.True(t, common.UsingMainDatabase(common.DatabaseTypeMySQL))
	const id = 974303
	t.Cleanup(func() {
		assert.NoError(t, DB.Where("user_id = ?", id).Delete(&Task{}).Error)
		assert.NoError(t, DB.Unscoped().Where("id = ?", id).Delete(&User{}).Error)
		assert.NoError(t, DB.Where("id = ?", id).Delete(&Channel{}).Error)
	})
	require.NoError(t, DB.Create(&User{Id: id, Username: "video-channel-baseline", AffCode: "video-channel-baseline", Quota: 900, UsedQuota: 100}).Error)
	require.NoError(t, DB.Create(&Channel{Id: id, UsedQuota: 100}).Error)
	task := &Task{TaskID: "video-channel-baseline", UserId: id, ChannelId: id, Quota: 100, Status: TaskStatusQueued}
	require.NoError(t, DB.Create(task).Error)
	task.Status = TaskStatusSuccess
	task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(200, 100)
	won, err := FinalizeVideoTask(t.Context(), task, TaskStatusQueued, 100)
	require.NoError(t, err)
	require.True(t, won)
	var channel Channel
	require.NoError(t, DB.First(&channel, id).Error)
	assert.EqualValues(t, 200, channel.UsedQuota)
}

func TestVideoSettlementUsesFullSubscriptionReservation(t *testing.T) {
	require.True(t, common.UsingMainDatabase(common.DatabaseTypeMySQL))
	const id = 974304
	t.Cleanup(func() {
		assert.NoError(t, DB.Where("user_id = ?", id).Delete(&Task{}).Error)
		assert.NoError(t, DB.Unscoped().Where("user_id = ?", id).Delete(&Token{}).Error)
		assert.NoError(t, DB.Unscoped().Where("id = ?", id).Delete(&User{}).Error)
		assert.NoError(t, DB.Where("id = ?", id).Delete(&Channel{}).Error)
		assert.NoError(t, DB.Where("id = ?", id).Delete(&UserSubscription{}).Error)
	})
	require.NoError(t, DB.Create(&User{Id: id, Username: "video-subscription-reserve", AffCode: "video-subscription-reserve", Quota: 1000, UsedQuota: 100}).Error)
	require.NoError(t, DB.Create(&Token{Id: id, UserId: id, Key: "video-subscription-reserve", RemainQuota: 850, UsedQuota: 150}).Error)
	require.NoError(t, DB.Create(&Channel{Id: id, UsedQuota: 200}).Error)
	require.NoError(t, DB.Create(&UserSubscription{Id: id, UserId: id, AmountTotal: 1000, AmountUsed: 150, Status: "active"}).Error)
	task := &Task{TaskID: "video-subscription-reserve", UserId: id, ChannelId: id, Quota: 100, Status: TaskStatusUnknown, PrivateData: TaskPrivateData{TokenId: id, BillingSource: "subscription", SubscriptionId: id, SubmissionFunding: &TaskSubmissionFunding{ReservedQuota: 150, UpdateToken: true}, DiscountAmounts: types.NewDiscountAmounts(200, 100)}}
	require.NoError(t, DB.Create(task).Error)
	task.Status = TaskStatusSuccess
	task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(100, 50)
	won, err := FinalizeVideoTask(t.Context(), task, TaskStatusUnknown, 50)
	require.NoError(t, err)
	require.True(t, won)
	won, err = FinalizeVideoTask(t.Context(), task, TaskStatusUnknown, 50)
	require.NoError(t, err)
	assert.False(t, won)
	var user User
	var token Token
	var channel Channel
	var subscription UserSubscription
	require.NoError(t, DB.First(&user, id).Error)
	require.NoError(t, DB.First(&token, id).Error)
	require.NoError(t, DB.First(&channel, id).Error)
	require.NoError(t, DB.First(&subscription, id).Error)
	assert.Equal(t, 1000, user.Quota)
	assert.Equal(t, 50, user.UsedQuota)
	assert.Equal(t, 950, token.RemainQuota)
	assert.Equal(t, 50, token.UsedQuota)
	assert.EqualValues(t, 100, channel.UsedQuota)
	assert.EqualValues(t, 50, subscription.AmountUsed)
}

func TestRetiredVideoAccountingMarkersAreClearedWithoutLosingLogs(t *testing.T) {
	truncateTables(t)
	user := User{Username: "legacy-accounting-owner", AffCode: "legacy-accounting-owner", Quota: 900, UsedQuota: 100}
	require.NoError(t, DB.Create(&user).Error)
	task := Task{TaskID: "legacy-accounting-task", UserId: user.Id, Status: TaskStatusSuccess, Quota: 100}
	require.NoError(t, DB.Create(&task).Error)
	original := Log{UserId: user.Id, RequestId: "legacy-existing-log", CreatedAt: 1234, Quota: 100, Type: LogTypeConsume}
	require.NoError(t, DB.Create(&original).Error)
	delivery := Log{UserId: user.Id, RequestId: "legacy-delivery-log", CreatedAt: 1235, Quota: 50, Type: LogTypeConsume}
	payload, err := common.Marshal(delivery)
	require.NoError(t, err)
	const retiredPayload = "legacy-accounting-unverified"
	pending := []VideoTaskPendingLog{
		{TaskID: -1, Kind: "", Payload: ""},
		{TaskID: -2, Kind: "", Payload: string(payload)},
		{TaskID: -3, Kind: "manual", Payload: retiredPayload},
		{TaskID: -4, Kind: "delivery", Payload: retiredPayload},
		{TaskID: -5, Kind: "manual", Payload: ""},
		{TaskID: 6, Kind: "delivery", Payload: "invalid-json"},
	}
	for i := range videoLogRetryBatch + 1 {
		pending = append(pending, VideoTaskPendingLog{TaskID: -1000 - int64(i), Payload: retiredPayload})
	}
	require.NoError(t, DB.Session(&gorm.Session{SkipHooks: true}).CreateInBatches(&pending, 64).Error)
	require.NoError(t, migrateVideoTaskLogKinds(DB))
	require.NoError(t, migrateVideoTaskLogKinds(DB))
	var markers int64
	require.NoError(t, DB.Model(&VideoTaskPendingLog{}).Where("payload = ?", retiredPayload).Count(&markers).Error)
	assert.EqualValues(t, 1, markers)
	for _, id := range []int64{-1, -5} {
		var accounting VideoTaskPendingLog
		require.NoError(t, DB.First(&accounting, "task_id = ?", id).Error)
		assert.Equal(t, videoLogKindAccounting, accounting.Kind)
		assert.Empty(t, accounting.Payload)
	}
	var queued VideoTaskPendingLog
	require.NoError(t, DB.First(&queued, "task_id = ?", -2).Error)
	assert.Equal(t, videoLogKindDelivery, queued.Kind)
	require.NoError(t, DeliverVideoTaskLog(t.Context(), -4))
	require.NoError(t, DeliverVideoTaskLog(t.Context(), -2))
	require.NoError(t, DeliverVideoTaskLog(t.Context(), -2))
	require.NoError(t, DB.Model(&VideoTaskPendingLog{}).Where("task_id IN ?", []int64{-2, -3, -4}).Count(&markers).Error)
	assert.Zero(t, markers)
	require.ErrorContains(t, DeliverVideoTaskLog(t.Context(), 6), "decode video billing log")
	var invalid VideoTaskPendingLog
	require.NoError(t, DB.First(&invalid, "task_id = ?", 6).Error)
	assert.Equal(t, "invalid-json", invalid.Payload)
	var logs []Log
	require.NoError(t, DB.Order("id").Find(&logs).Error)
	require.Len(t, logs, 2)
	assert.Equal(t, original.RequestId, logs[0].RequestId)
	assert.Equal(t, delivery.RequestId, logs[1].RequestId)
	assert.Equal(t, delivery.Quota, logs[1].Quota)
	var stored User
	var storedTask Task
	require.NoError(t, DB.First(&stored, user.Id).Error)
	require.NoError(t, DB.First(&storedTask, task.ID).Error)
	assert.Equal(t, 900, stored.Quota)
	assert.Equal(t, 100, stored.UsedQuota)
	assert.Equal(t, 100, storedTask.Quota)
	assert.Equal(t, TaskStatus(TaskStatusSuccess), storedTask.Status)
}

// 真实数据库执行终态事务，覆盖中途故障回滚、重试和成功/取消争抢同一任务。
func TestVideoTaskAtomicSettlementDatabaseMatrix(t *testing.T) {
	// 日常回归保持轻量；压测参数有界，保证每个用户至少持有一个任务。
	load := map[string]int{"VIDEO_AUDIT_TASKS": 64, "VIDEO_AUDIT_USERS": 16, "VIDEO_AUDIT_WORKERS": 32}
	limits := map[string]int{"VIDEO_AUDIT_TASKS": 65536, "VIDEO_AUDIT_USERS": 1000, "VIDEO_AUDIT_WORKERS": 128}
	for name := range load {
		if configured := os.Getenv(name); configured != "" {
			value, err := strconv.Atoi(configured)
			require.NoError(t, err, name)
			require.True(t, value > 0 && value <= limits[name], "%s outside supported range", name)
			load[name] = value
		}
	}
	taskCount, userCount, workerCount := load["VIDEO_AUDIT_TASKS"], load["VIDEO_AUDIT_USERS"], load["VIDEO_AUDIT_WORKERS"]
	require.GreaterOrEqual(t, taskCount, userCount)
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialect gorm.Dialector
			switch engine {
			case "mysql":
				dsn := os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TASK_PLUGIN_TEST_MYSQL_DSN is unset")
				}
				dialect = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TASK_PLUGIN_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TASK_PLUGIN_TEST_POSTGRES_DSN is unset")
				}
				dialect = postgres.Open(dsn)
			default:
				dialect = sqlite.Open(":memory:")
			}
			db, err := gorm.Open(dialect, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "video_atomic_audit_"}})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			if engine == "sqlite" {
				connection.SetMaxOpenConns(1)
			} else {
				// 数据库连接池随压测并发提升，避免64个 worker 实际被16个连接限流。
				connection.SetMaxOpenConns(workerCount)
				connection.SetMaxIdleConns(workerCount)
			}
			previousDB, previousRedis := DB, common.RedisEnabled
			previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
			DB, common.RedisEnabled = db, false
			common.SetDatabaseTypes(common.DatabaseType(engine), common.DatabaseType(engine))
			initCol()
			models := []any{&User{}, &Token{}, &Channel{}, &Task{}, &UserSubscription{}}
			require.NoError(t, db.AutoMigrate(models...))
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(models...))
				DB, common.RedisEnabled = previousDB, previousRedis
				common.SetDatabaseTypes(previousMain, previousLog)
				initCol()
				assert.NoError(t, connection.Close())
			})
			for index, failTable := range []string{"users", "tokens", "channels", "tasks", ""} {
				t.Run("rollback_"+failTable, func(t *testing.T) {
					id := 100 + index
					require.NoError(t, db.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: 900, UsedQuota: 100}).Error)
					require.NoError(t, db.Create(&Token{Id: id, UserId: id, Key: fmt.Sprintf("audit-token-%d", id), RemainQuota: 900, UsedQuota: 100}).Error)
					require.NoError(t, db.Create(&Channel{Id: id, UsedQuota: 100}).Error)
					task := &Task{TaskID: fmt.Sprint(id), UserId: id, ChannelId: id, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: id, Key: "fixture-upstream-secret"}}
					require.NoError(t, db.Create(task).Error)
					var encrypted string
					require.NoError(t, db.Model(&Task{}).Where("id = ?", task.ID).Select("private_data").Scan(&encrypted).Error)
					assert.NotContains(t, encrypted, "fixture-upstream-secret")
					var loaded Task
					require.NoError(t, db.First(&loaded, task.ID).Error)
					assert.Equal(t, task.PrivateData.Key, loaded.PrivateData.Key)
					task.Status, task.Progress = TaskStatusSuccess, "100%"
					if failTable != "" {
						require.NoError(t, db.Callback().Update().Before("gorm:update").Register("audit_reject_settlement", func(tx *gorm.DB) {
							if tx.Statement.Table == "video_atomic_audit_"+failTable {
								tx.AddError(errors.New("injected storage failure"))
							}
						}))
						won, err := FinalizeVideoTask(t.Context(), task, TaskStatusQueued, 60)
						require.Error(t, err)
						assert.False(t, won)
						require.NoError(t, db.Callback().Update().Remove("audit_reject_settlement"))
						var stored Task
						require.NoError(t, db.First(&stored, task.ID).Error)
						assert.Equal(t, TaskStatus(TaskStatusQueued), stored.Status)
						assert.Equal(t, 100, stored.Quota)
						var user User
						require.NoError(t, db.First(&user, id).Error)
						assert.EqualValues(t, 900, user.Quota)
					}
					won, err := FinalizeVideoTask(t.Context(), task, TaskStatusQueued, 60)
					require.NoError(t, err)
					assert.True(t, won)
					var user User
					var token Token
					var channel Channel
					require.NoError(t, db.First(&user, id).Error)
					require.NoError(t, db.First(&token, id).Error)
					require.NoError(t, db.First(&channel, id).Error)
					assert.EqualValues(t, 940, user.Quota)
					assert.EqualValues(t, 60, user.UsedQuota)
					assert.Equal(t, 940, token.RemainQuota)
					assert.Equal(t, 60, token.UsedQuota)
					assert.Equal(t, int64(60), channel.UsedQuota)
				})
			}
			// 订阅不足时连终态一起回滚；随后有效结算只调整订阅，不动钱包余额。
			require.NoError(t, db.Create(&User{Id: 300, Username: "subscriber", AffCode: "subscriber", Quota: 900, UsedQuota: 100}).Error)
			require.NoError(t, db.Create(&Channel{Id: 300, UsedQuota: 100}).Error)
			require.NoError(t, db.Create(&UserSubscription{Id: 300, UserId: 300, AmountTotal: 200, AmountUsed: 100}).Error)
			subTask := &Task{TaskID: "subscription-task", UserId: 300, ChannelId: 300, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: 300}}
			require.NoError(t, db.Create(subTask).Error)
			subTask.Status = TaskStatusSuccess
			subWon, subErr := FinalizeVideoTask(t.Context(), subTask, TaskStatusQueued, 250)
			require.Error(t, subErr)
			assert.False(t, subWon)
			subWon, subErr = FinalizeVideoTask(t.Context(), subTask, TaskStatusQueued, 50)
			require.NoError(t, subErr)
			assert.True(t, subWon)
			var sub UserSubscription
			var subscriber User
			require.NoError(t, db.First(&sub, 300).Error)
			require.NoError(t, db.First(&subscriber, 300).Error)
			assert.EqualValues(t, 50, sub.AmountUsed)
			assert.EqualValues(t, 900, subscriber.Quota)
			assert.EqualValues(t, 50, subscriber.UsedQuota)
			// 同一原快照的成功、取消请求并发抵达，只允许一笔账务变更。
			require.NoError(t, db.Create(&User{Id: 200, Username: "parallel-owner", AffCode: "parallel", Quota: 900, UsedQuota: 100}).Error)
			require.NoError(t, db.Create(&Channel{Id: 200, UsedQuota: 100}).Error)
			task := Task{TaskID: "parallel-task", UserId: 200, ChannelId: 200, Quota: 100, Status: TaskStatusQueued}
			require.NoError(t, db.Create(&task).Error)
			type outcome struct {
				won   bool
				err   error
				quota int
			}
			results := make(chan outcome, 16)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for index := range 16 {
				workers.Go(func() {
					copy := task
					copy.Status, copy.Progress = TaskStatusSuccess, "100%"
					quota := 60
					if index%2 == 0 {
						copy.Status = TaskStatusFailure
						quota = 0
					}
					<-start
					won, err := FinalizeVideoTask(t.Context(), &copy, TaskStatusQueued, quota)
					results <- outcome{won, err, quota}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			wins, actual := 0, 0
			for result := range results {
				require.NoError(t, result.err)
				if result.won {
					wins++
					actual = result.quota
				}
			}
			assert.Equal(t, 1, wins)
			var owner User
			require.NoError(t, db.First(&owner, 200).Error)
			assert.EqualValues(t, 1000-actual, owner.Quota)
			assert.EqualValues(t, actual, owner.UsedQuota)
			// 多用户各自有独立令牌/渠道，每个任务重复投递两次；逐户验证资金和统计守恒。
			const initialQuota = 100000000
			expected := make([]int, userCount)
			reserved := make([]int, userCount)
			tasks := make([]Task, taskCount)
			for index := range tasks {
				owner := index % userCount
				quota := []int{0, 50, 125}[index%3]
				expected[owner] += quota
				reserved[owner] += 100
				tasks[index] = Task{TaskID: fmt.Sprintf("load-%d", index), UserId: 1000 + owner, ChannelId: 1000 + owner, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: 1000 + owner, DiscountAmounts: types.NewDiscountAmounts(200, 100)}}
			}
			for owner := range userCount {
				id := 1000 + owner
				require.NoError(t, db.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: initialQuota - reserved[owner], UsedQuota: reserved[owner]}).Error)
				require.NoError(t, db.Create(&Token{Id: id, UserId: id, Key: fmt.Sprintf("audit-load-token-%d", id), RemainQuota: initialQuota - reserved[owner], UsedQuota: reserved[owner]}).Error)
				require.NoError(t, db.Create(&Channel{Id: id, UsedQuota: int64(reserved[owner] * 2)}).Error)
			}
			require.NoError(t, db.CreateInBatches(tasks, 64).Error)
			// 可选真实 Redis 只连接本次隔离环境，验证事务后增量与历史缓存快照不会互相覆盖。
			if address := os.Getenv("TASK_PLUGIN_TEST_REDIS_ADDR"); address != "" {
				require.Equal(t, "127.0.0.1:26379", address)
				client := redis.NewClient(&redis.Options{Addr: address})
				require.NoError(t, client.FlushDB(t.Context()).Err())
				oldRDB := common.RDB
				common.RDB, common.RedisEnabled = client, true
				t.Cleanup(func() { common.RedisEnabled = false; common.RDB = oldRDB; assert.NoError(t, client.Close()) })
				for owner := range userCount {
					_, err := GetUserCache(1000 + owner)
					require.NoError(t, err)
					var token Token
					require.NoError(t, db.First(&token, 1000+owner).Error)
					_, err = cacheInitToken(token)
					require.NoError(t, err)
				}
			}
			type timedOutcome struct {
				index   int
				won     bool
				err     error
				elapsed time.Duration
			}
			jobs := make(chan int, workerCount*2)
			completed := make(chan timedOutcome, taskCount*2)
			started := time.Now()
			for range workerCount {
				workers.Go(func() {
					for index := range jobs {
						copy := tasks[index]
						quota := []int{0, 50, 125}[index%3]
						copy.Status, copy.Progress = TaskStatusSuccess, "100%"
						if quota == 0 {
							copy.Status = TaskStatusFailure
						}
						begin := time.Now()
						copy.PrivateData.DiscountAmounts = types.NewDiscountAmounts(quota*2, quota)
						won, err := FinalizeVideoTask(t.Context(), &copy, TaskStatusQueued, quota)
						completed <- timedOutcome{index, won, err, time.Since(begin)}
					}
				})
			}
			for index := range tasks {
				jobs <- index
				jobs <- index
			}
			close(jobs)
			workers.Wait()
			close(completed)
			elapsed := time.Since(started)
			wins = 0
			// 逐任务核对获胜次数，不能让某任务重复结算和另一任务漏结算在总数中抵消。
			taskWins := make([]int, taskCount)
			latencies := make([]time.Duration, 0, taskCount*2)
			for result := range completed {
				require.NoError(t, result.err)
				if result.won {
					wins++
					taskWins[result.index]++
				}
				latencies = append(latencies, result.elapsed)
			}
			assert.Equal(t, taskCount, wins)
			var storedTasks []Task
			require.NoError(t, db.Where("user_id >= ?", 1000).Order("id").Find(&storedTasks).Error)
			require.Len(t, storedTasks, taskCount)
			for index, stored := range storedTasks {
				assert.Equal(t, 1, taskWins[index], "task %s", stored.TaskID)
				assert.Equal(t, tasks[index].ID, stored.ID)
				assert.Equal(t, tasks[index].UserId, stored.UserId)
				actual := []int{0, 50, 125}[index%3]
				assert.Equal(t, actual, stored.Quota)
				status := TaskStatus(TaskStatusSuccess)
				if actual == 0 {
					status = TaskStatusFailure
				}
				assert.Equal(t, status, stored.Status)
			}
			for owner := range userCount {
				var user User
				var token Token
				var channel Channel
				require.NoError(t, db.First(&user, 1000+owner).Error)
				require.NoError(t, db.First(&token, 1000+owner).Error)
				require.NoError(t, db.First(&channel, 1000+owner).Error)
				assert.EqualValues(t, initialQuota-expected[owner], user.Quota)
				assert.EqualValues(t, expected[owner], user.UsedQuota)
				assert.Equal(t, initialQuota-expected[owner], token.RemainQuota)
				assert.Equal(t, expected[owner], token.UsedQuota)
				assert.EqualValues(t, expected[owner]*2, channel.UsedQuota)
				if common.RedisEnabled {
					cached, err := GetUserCache(user.Id)
					require.NoError(t, err)
					assert.Equal(t, user.Quota, cached.Quota)
					// race插桩可能超过缓存TTL，走真实读取入口验证过期后的数据库回填。
					cachedToken, err := GetTokenByKey(token.Key, false)
					require.NoError(t, err)
					assert.Equal(t, token.RemainQuota, cachedToken.RemainQuota)
					assert.Equal(t, token.UsedQuota, cachedToken.UsedQuota)
					// 用户资料旧快照仅更新元数据，不得恢复已结算前的旧余额。
					user.Quota = initialQuota - reserved[owner]
					require.NoError(t, updateUserCache(user))
					cached, err = GetUserCache(user.Id)
					require.NoError(t, err)
					assert.EqualValues(t, initialQuota-expected[owner], cached.Quota)
				}
			}
			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			t.Logf("LOAD engine=%s tasks=%d attempts=%d users=%d workers=%d db_connections=%d elapsed=%s attempts_per_second=%.1f p50=%s p95=%s p99=%s accounting=exact", engine, taskCount, len(latencies), userCount, workerCount, connection.Stats().MaxOpenConnections, elapsed, float64(len(latencies))/elapsed.Seconds(), latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)*99/100])
		})
	}
}

func TestTaskAccountingSurvivesStaleTaskUpdates(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal_%t", terminal), func(t *testing.T) {
			truncateTables(t)
			require.NoError(t, DB.Create(&User{Id: 79, Username: "stale-update-owner", AffCode: "stale-update-owner", Quota: 900}).Error)
			require.NoError(t, DB.Create(&Channel{Id: 79}).Error)
			task := Task{TaskID: "stale-accounting-update", UserId: 79, ChannelId: 79, Quota: 100, Status: TaskStatusQueued, Progress: "10%", PrivateData: TaskPrivateData{InitialAccountingPending: true}}
			require.NoError(t, DB.Create(&task).Error)
			stale := task
			accounting := task
			won, err := CommitTaskInitialAccounting(t.Context(), &accounting, &Log{UserId: 79, Quota: 100, RequestId: "stale-accounting-update"})
			require.NoError(t, err)
			require.True(t, won)
			require.NoError(t, DB.Model(&Task{}).Where("id = ?", task.ID).Update("progress", "60%").Error)
			require.NoError(t, stale.MarkReconciliation(t.Context(), TaskStatusQueued, "overdue"))
			require.NoError(t, stale.SetResponsesBackground(t.Context()))
			var stored Task
			require.NoError(t, DB.First(&stored, task.ID).Error)
			assert.Equal(t, "60%", stored.Progress)
			assert.True(t, stored.PrivateData.ReconciliationRequired)
			assert.False(t, stored.PrivateData.InitialAccountingPending)
			assert.True(t, stored.PrivateData.ResponsesBackground)
			if terminal {
				stale.Status, stale.Progress = TaskStatusSuccess, "100%"
				won, err = FinalizeVideoTask(t.Context(), &stale, TaskStatusQueued, 50)
			} else {
				stale.Progress = "70%"
				won, err = stale.UpdateWithStatus(TaskStatusQueued)
			}
			require.NoError(t, err)
			require.True(t, won)
			require.NoError(t, DB.First(&stored, task.ID).Error)
			assert.False(t, stored.PrivateData.InitialAccountingPending)
			assert.True(t, stored.PrivateData.ResponsesBackground)
			won, err = CommitTaskInitialAccounting(t.Context(), &task)
			require.NoError(t, err)
			assert.False(t, won)
			var user User
			var channel Channel
			var logs int64
			require.NoError(t, DB.First(&user, 79).Error)
			require.NoError(t, DB.First(&channel, 79).Error)
			require.NoError(t, DB.Model(&Log{}).Where("user_id = ?", 79).Count(&logs).Error)
			if terminal {
				assert.Equal(t, 950, user.Quota)
				assert.Equal(t, 50, user.UsedQuota)
				assert.EqualValues(t, 50, channel.UsedQuota)
			} else {
				assert.Equal(t, 900, user.Quota)
				assert.Equal(t, 100, user.UsedQuota)
				assert.EqualValues(t, 100, channel.UsedQuota)
			}
			assert.Equal(t, 1, user.RequestCount)
			assert.EqualValues(t, 1, logs)
		})
	}
	t.Run("submission evidence", func(t *testing.T) {
		truncateTables(t)
		task := Task{TaskID: "stale-submission-evidence", Status: TaskStatusUnknown, PrivateData: TaskPrivateData{SubmissionPending: true, InitialAccountingPending: true}}
		require.NoError(t, DB.Create(&task).Error)
		stale := task
		current := task
		current.PrivateData.SubmissionResponse = []byte(`{"parsed":{"taskId":"upstream"}}`)
		won, err := PersistTaskSubmission(t.Context(), &current, false)
		require.NoError(t, err)
		require.True(t, won)
		stale.PrivateData.PollFailures++
		won, err = stale.UpdateWithStatus(TaskStatusUnknown)
		require.NoError(t, err)
		assert.False(t, won)
		var stored Task
		require.NoError(t, DB.First(&stored, task.ID).Error)
		assert.JSONEq(t, string(current.PrivateData.SubmissionResponse), string(stored.PrivateData.SubmissionResponse))
	})
	t.Run("changed reservation", func(t *testing.T) {
		truncateTables(t)
		require.NoError(t, DB.Create(&User{Id: 78, Username: "changed-owner", AffCode: "changed-owner", Quota: 900}).Error)
		require.NoError(t, DB.Create(&Channel{Id: 78}).Error)
		task := Task{TaskID: "changed-accounting-reservation", UserId: 78, ChannelId: 78, Quota: 100, Status: TaskStatusUnknown, PrivateData: TaskPrivateData{SubmissionPending: true, InitialAccountingPending: true}}
		require.NoError(t, DB.Create(&task).Error)
		stale := task
		final := task
		final.Status, final.Progress, final.Quota = TaskStatusSuccess, "100%", 50
		final.PrivateData.SubmissionPending = false
		won, err := PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: 100})
		require.NoError(t, err)
		require.True(t, won)
		won, err = CommitTaskInitialAccounting(t.Context(), &stale, &Log{UserId: 78, Quota: 100, RequestId: task.TaskID})
		require.ErrorContains(t, err, "snapshot changed")
		assert.False(t, won)
		var stored Task
		var user User
		require.NoError(t, DB.First(&stored, task.ID).Error)
		require.NoError(t, DB.First(&user, 78).Error)
		assert.True(t, stored.PrivateData.InitialAccountingPending)
		assert.Equal(t, 950, user.Quota)
		assert.Zero(t, user.UsedQuota)
		won, err = CommitTaskInitialAccounting(t.Context(), &stored, &Log{UserId: 78, Quota: 50, RequestId: task.TaskID})
		require.NoError(t, err)
		require.True(t, won)
		require.NoError(t, DB.First(&user, 78).Error)
		assert.Equal(t, 50, user.UsedQuota)
		assert.Equal(t, 1, user.RequestCount)
		var logs []Log
		require.NoError(t, DB.Where("user_id = ?", 78).Find(&logs).Error)
		require.Len(t, logs, 1)
		assert.Equal(t, 50, logs[0].Quota)
	})
	t.Run("concurrent updates", func(t *testing.T) {
		truncateTables(t)
		connection, err := DB.DB()
		require.NoError(t, err)
		connection.SetMaxOpenConns(32)
		t.Cleanup(func() { connection.SetMaxOpenConns(1) })
		require.NoError(t, DB.Create(&User{Id: 80, Username: "concurrent-owner", AffCode: "concurrent-owner", Quota: 900}).Error)
		require.NoError(t, DB.Create(&Channel{Id: 80}).Error)
		task := Task{TaskID: "concurrent-accounting-update", UserId: 80, ChannelId: 80, Quota: 100, Status: TaskStatusQueued, Progress: "10%", PrivateData: TaskPrivateData{InitialAccountingPending: true}}
		require.NoError(t, DB.Create(&task).Error)
		start := make(chan struct{})
		results := make(chan error, 64)
		var workers sync.WaitGroup
		for index := range 64 {
			workers.Go(func() {
				<-start
				copy := task
				var err error
				switch index % 4 {
				case 0:
					_, err = CommitTaskInitialAccounting(t.Context(), &copy, &Log{UserId: 80, Quota: 100, RequestId: task.TaskID})
				case 1:
					err = copy.MarkReconciliation(t.Context(), TaskStatusQueued, "overdue")
				case 2:
					err = copy.SetResponsesBackground(t.Context())
				default:
					copy.Progress = "20%"
					_, err = copy.UpdateWithStatus(TaskStatusQueued)
				}
				results <- err
			})
		}
		close(start)
		workers.Wait()
		close(results)
		for err := range results {
			require.NoError(t, err)
		}
		var stored Task
		var user User
		var channel Channel
		var logs int64
		require.NoError(t, DB.First(&stored, task.ID).Error)
		require.NoError(t, DB.First(&user, 80).Error)
		require.NoError(t, DB.First(&channel, 80).Error)
		require.NoError(t, DB.Model(&Log{}).Where("user_id = ?", 80).Count(&logs).Error)
		assert.False(t, stored.PrivateData.InitialAccountingPending)
		assert.True(t, stored.PrivateData.ResponsesBackground)
		assert.Equal(t, 900, user.Quota)
		assert.Equal(t, 100, user.UsedQuota)
		assert.Equal(t, 1, user.RequestCount)
		assert.EqualValues(t, 100, channel.UsedQuota)
		assert.EqualValues(t, 1, logs)
	})
}

func TestImmediateTaskFundingDatabase(t *testing.T) {
	for _, tc := range []struct {
		name, funding, failTable string
		actual, reserved         int
		updateToken, async       bool
	}{
		{"wallet partial refund", "wallet", "", 50, 100, true, false},
		{"wallet full refund", "wallet", "", 0, 100, true, false},
		{"wallet same amount", "wallet", "", 100, 100, true, false},
		{"playground skips token", "wallet", "", 50, 100, false, false},
		{"subscription refund", "subscription", "", 50, 100, true, false},
		{"wallet failure rolls back", "wallet", "users", 50, 100, true, false},
		{"token failure rolls back", "wallet", "tokens", 50, 100, true, false},
		{"subscription failure rolls back", "subscription", "user_subscriptions", 50, 100, true, false},
		{"async wallet refund", "wallet", "", 50, 100, true, true},
		{"async wallet same amount", "wallet", "", 100, 100, true, true},
		{"async playground refund", "wallet", "", 50, 100, false, true},
		{"async subscription refund", "subscription", "", 50, 100, true, true},
		{"async subscription failure rolls back", "subscription", "user_subscriptions", 50, 100, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			userQuota := 900
			if tc.funding == "subscription" {
				userQuota = 1000
			}
			require.NoError(t, DB.Create(&User{Id: 81, Username: "immediate-owner", AffCode: "immediate-owner", Quota: userQuota}).Error)
			require.NoError(t, DB.Create(&Token{Id: 81, UserId: 81, Key: "immediate-local-token", RemainQuota: 900, UsedQuota: 100}).Error)
			require.NoError(t, DB.Create(&Channel{Id: 81}).Error)
			if tc.funding == "subscription" {
				require.NoError(t, DB.Create(&UserSubscription{Id: 81, UserId: 81, AmountTotal: 1000, AmountUsed: 100}).Error)
			}
			staged := Task{TaskID: "immediate-funding", UserId: 81, ChannelId: 81, Quota: 100, Status: TaskStatusUnknown, PrivateData: TaskPrivateData{SubmissionPending: true, InitialAccountingPending: true, BillingSource: tc.funding, TokenId: 81}}
			if tc.funding == "subscription" {
				staged.PrivateData.SubscriptionId = 81
			}
			require.NoError(t, DB.Create(&staged).Error)
			final := staged
			final.Status, final.Progress, final.Quota = TaskStatusSuccess, "100%", tc.actual
			if tc.actual == 0 {
				final.Status = TaskStatusFailure
			}
			if tc.async {
				final.Status, final.Progress = TaskStatusSubmitted, "10%"
			}
			final.PrivateData.SubmissionPending = false
			if tc.failTable != "" {
				require.NoError(t, DB.Callback().Update().Before("gorm:update").Register("immediate-funding-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == tc.failTable {
						tx.AddError(errors.New("injected funding failure"))
					}
				}))
				won, err := PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: tc.reserved, UpdateToken: tc.updateToken})
				require.Error(t, err)
				assert.False(t, won)
				require.NoError(t, DB.Callback().Update().Remove("immediate-funding-failure"))
				var retained Task
				var retainedUser User
				var retainedToken Token
				require.NoError(t, DB.First(&retained, staged.ID).Error)
				require.NoError(t, DB.First(&retainedUser, 81).Error)
				require.NoError(t, DB.First(&retainedToken, 81).Error)
				assert.Equal(t, TaskStatus(TaskStatusUnknown), retained.Status)
				assert.True(t, retained.PrivateData.SubmissionPending)
				assert.Equal(t, 100, retained.Quota)
				assert.Equal(t, userQuota, retainedUser.Quota)
				assert.Equal(t, 900, retainedToken.RemainQuota)
			}
			won, err := PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: tc.reserved, UpdateToken: tc.updateToken})
			require.NoError(t, err)
			require.True(t, won)
			var stored Task
			var user User
			var token Token
			var marker VideoTaskPendingLog
			require.NoError(t, DB.First(&stored, staged.ID).Error)
			require.NoError(t, DB.First(&user, 81).Error)
			require.NoError(t, DB.First(&token, 81).Error)
			if !tc.async {
				require.NoError(t, DB.First(&marker, "task_id = ?", -staged.ID).Error)
				assert.Empty(t, marker.Payload)
			}
			assert.Equal(t, final.Status, stored.Status)
			assert.Equal(t, tc.actual, stored.Quota)
			assert.True(t, stored.PrivateData.SubmissionFundsCommitted)
			assert.True(t, stored.PrivateData.InitialAccountingPending)
			assert.Zero(t, user.UsedQuota)
			if tc.funding == "wallet" {
				assert.Equal(t, userQuota+tc.reserved-tc.actual, user.Quota)
			} else {
				var subscription UserSubscription
				require.NoError(t, DB.First(&subscription, 81).Error)
				assert.EqualValues(t, tc.actual, subscription.AmountUsed)
				assert.Equal(t, userQuota, user.Quota)
			}
			if tc.updateToken {
				assert.Equal(t, 1000-tc.actual, token.RemainQuota)
				assert.Equal(t, tc.actual, token.UsedQuota)
			} else {
				assert.Equal(t, 900, token.RemainQuota)
				assert.Equal(t, 100, token.UsedQuota)
			}
			won, err = PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: tc.reserved, UpdateToken: tc.updateToken})
			require.NoError(t, err)
			assert.False(t, won)
			require.NoError(t, DB.First(&user, 81).Error)
			if tc.funding == "wallet" {
				assert.Equal(t, userQuota+tc.reserved-tc.actual, user.Quota)
			} else {
				assert.Equal(t, userQuota, user.Quota)
			}
		})
	}
	t.Run("insufficient reservation leaves intent unchanged", func(t *testing.T) {
		truncateTables(t)
		staged := Task{TaskID: "insufficient-reservation", UserId: 82, ChannelId: 82, Quota: 100, Status: TaskStatusUnknown, PrivateData: TaskPrivateData{SubmissionPending: true, InitialAccountingPending: true}}
		require.NoError(t, DB.Create(&staged).Error)
		final := staged
		final.Status, final.Quota = TaskStatusSuccess, 101
		final.PrivateData.SubmissionPending = false
		won, err := PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: 100})
		require.Error(t, err)
		assert.False(t, won)
		var stored Task
		require.NoError(t, DB.First(&stored, staged.ID).Error)
		assert.Equal(t, TaskStatus(TaskStatusUnknown), stored.Status)
		assert.True(t, stored.PrivateData.SubmissionPending)
		assert.False(t, stored.PrivateData.SubmissionFundsCommitted)
	})
}

func TestImmediateTaskFundingConcurrentLoad(t *testing.T) {
	truncateTables(t)
	const users, workers, initialQuota = 200, 64, 10000000
	count := 4096
	if value := os.Getenv("VIDEO_IMMEDIATE_AUDIT_TASKS"); value != "" {
		var err error
		count, err = strconv.Atoi(value)
		require.NoError(t, err)
		require.GreaterOrEqual(t, count, users)
		require.LessOrEqual(t, count, 65536)
	}
	connection, err := DB.DB()
	require.NoError(t, err)
	if DB.Dialector.Name() != "sqlite" {
		connection.SetMaxOpenConns(workers)
		connection.SetMaxIdleConns(workers)
		t.Cleanup(func() { connection.SetMaxOpenConns(1); connection.SetMaxIdleConns(1) })
	}
	expected, reserved, requests := make([]int, users), make([]int, users), make([]int, users)
	tasks := make([]Task, count)
	for index := range tasks {
		owner, actual := index%users, []int{0, 50, 100}[index%3]
		reserved[owner] += 100
		expected[owner] += actual
		requests[owner]++
		tasks[index] = Task{TaskID: fmt.Sprintf("immediate-load-%d", index), UserId: 1000 + owner, ChannelId: 1000 + owner, Quota: 100, Status: TaskStatusUnknown, PrivateData: TaskPrivateData{TokenId: 1000 + owner, SubmissionPending: true, InitialAccountingPending: true}}
	}
	for owner := range users {
		id := 1000 + owner
		require.NoError(t, DB.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: initialQuota - reserved[owner]}).Error)
		require.NoError(t, DB.Create(&Token{Id: id, UserId: id, Key: fmt.Sprintf("immediate-load-token-%d", id), RemainQuota: initialQuota - reserved[owner], UsedQuota: reserved[owner]}).Error)
		require.NoError(t, DB.Create(&Channel{Id: id}).Error)
	}
	require.NoError(t, DB.CreateInBatches(tasks, 64).Error)
	type outcome struct {
		index int
		won   bool
		err   error
	}
	jobs := make(chan int, workers*2)
	results := make(chan outcome, count*2)
	var group sync.WaitGroup
	started := time.Now()
	for range workers {
		group.Go(func() {
			for index := range jobs {
				final := tasks[index]
				final.Quota = []int{0, 50, 100}[index%3]
				final.Status, final.Progress = TaskStatusSuccess, "100%"
				if final.Quota == 0 {
					final.Status = TaskStatusFailure
				}
				if index%2 == 1 {
					final.Status, final.Progress = TaskStatusSubmitted, "10%"
				}
				final.PrivateData.SubmissionPending = false
				won, err := PersistTaskSubmission(t.Context(), &final, true, TaskSubmissionFunding{ReservedQuota: 100, UpdateToken: true})
				if err == nil && won && index%2 == 1 {
					log := &Log{UserId: final.UserId, Type: LogTypeConsume, ModelName: "submission-load", Quota: final.Quota, RequestId: final.TaskID}
					_, err = CommitTaskInitialAccounting(t.Context(), &final, log)
				}
				results <- outcome{index: index, won: won, err: err}
			}
		})
	}
	for index := range tasks {
		jobs <- index
		jobs <- index
	}
	close(jobs)
	group.Wait()
	close(results)
	wins := make([]int, count)
	for result := range results {
		require.NoError(t, result.err)
		if result.won {
			wins[result.index]++
		}
	}
	for _, winsForTask := range wins {
		assert.Equal(t, 1, winsForTask)
	}
	for pass := 0; pass < (count+videoLogRetryBatch-1)/videoLogRetryBatch+1 && HasPendingVideoTaskLogs(); pass++ {
		require.NoError(t, RetryTaskInitialAccounting(t.Context(), func(ctx context.Context, task *Task) error {
			log := &Log{UserId: task.UserId, Type: LogTypeConsume, ModelName: "immediate-load", Quota: task.Quota, RequestId: task.TaskID}
			_, err := CommitTaskInitialAccounting(ctx, task, log)
			return err
		}))
	}
	assert.False(t, HasPendingVideoTaskLogs())
	for owner := range users {
		var user User
		var token Token
		var channel Channel
		require.NoError(t, DB.First(&user, 1000+owner).Error)
		require.NoError(t, DB.First(&token, 1000+owner).Error)
		require.NoError(t, DB.First(&channel, 1000+owner).Error)
		assert.Equal(t, initialQuota-expected[owner], user.Quota)
		assert.Equal(t, expected[owner], user.UsedQuota)
		assert.Equal(t, requests[owner], user.RequestCount)
		assert.Equal(t, user.Quota, token.RemainQuota)
		assert.Equal(t, user.UsedQuota, token.UsedQuota)
		assert.EqualValues(t, expected[owner], channel.UsedQuota)
	}
	var logs, pending int64
	require.NoError(t, DB.Model(&Log{}).Count(&logs).Error)
	require.NoError(t, DB.Model(&Task{}).Where("status NOT IN ?", []TaskStatus{TaskStatusSubmitted, TaskStatusSuccess, TaskStatusFailure}).Count(&pending).Error)
	assert.EqualValues(t, count, logs)
	assert.Zero(t, pending)
	t.Logf("SUBMISSION_LOAD engine=%s tasks=%d attempts=%d users=%d workers=%d elapsed=%s accounting=exact", DB.Dialector.Name(), count, count*2, users, workers, time.Since(started))
}
