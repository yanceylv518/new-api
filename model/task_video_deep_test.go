package model

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// 隔离的真实磁盘测试库只在显式启用时访问；前缀与其他审查测试分离。
func openVideoDeepDatabase(t *testing.T, prefix string) *gorm.DB {
	t.Helper()
	if os.Getenv("VIDEO_DEEP_AUDIT") != "1" {
		t.Skip("set VIDEO_DEEP_AUDIT=1 to use the isolated audit database")
	}
	db, err := gorm.Open(postgres.Open("host=127.0.0.1 port=25446 user=postgres dbname=doubao_native_test sslmode=disable"), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: prefix}, Logger: logger.Default.LogMode(logger.Error),
	})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(64)
	connection.SetMaxIdleConns(64)
	if prefix != "video_soak_" {
		connection.SetMaxOpenConns(8)
		connection.SetMaxIdleConns(8)
	}
	previousDB, previousRedis := DB, common.RedisEnabled
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	DB, common.RedisEnabled = db, false
	common.SetDatabaseTypes(common.DatabaseTypePostgreSQL, common.DatabaseTypePostgreSQL)
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
	return db
}

// 在途任务的用户/令牌被管理员变更后，余额只能写回原所有者；已删除用户必须回滚。
func TestVideoInFlightOwnerAndTokenMutations(t *testing.T) {
	db := openVideoDeepDatabase(t, "video_mutation_")
	for index := range 4 {
		require.NoError(t, db.Create(&Channel{Id: index + 1, UsedQuota: 5000}).Error)
	}
	tasks := make([]Task, 200)
	for index := range tasks {
		id := index + 1
		require.NoError(t, db.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: 900, UsedQuota: 100, Status: common.UserStatusEnabled}).Error)
		require.NoError(t, db.Create(&Token{Id: id, UserId: id, Key: fmt.Sprintf("mutation-local-%d", id), RemainQuota: 900, UsedQuota: 100, Status: common.TokenStatusEnabled}).Error)
		tasks[index] = Task{TaskID: fmt.Sprintf("mutation-%d", id), UserId: id, ChannelId: index%4 + 1, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: id}}
		require.NoError(t, db.Create(&tasks[index]).Error)
		switch index % 4 {
		case 0:
			require.NoError(t, db.Model(&User{}).Where("id = ?", id).Update("status", common.UserStatusDisabled).Error)
		case 1:
			require.NoError(t, db.Model(&Token{}).Where("id = ?", id).Update("status", common.TokenStatusDisabled).Error)
		case 2:
			require.NoError(t, db.Delete(&Token{}, id).Error)
		case 3:
			require.NoError(t, db.Delete(&User{}, id).Error)
		}
	}
	type result struct {
		index int
		won   bool
		err   error
	}
	jobs, results := make(chan int, 128), make(chan result, 200)
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			for index := range jobs {
				copy := tasks[index]
				copy.Status = TaskStatusSuccess
				won, err := FinalizeVideoTask(t.Context(), &copy, TaskStatusQueued, 60)
				results <- result{index, won, err}
			}
		})
	}
	for index := range tasks {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(results)
	for outcome := range results {
		var user User
		var task Task
		require.NoError(t, db.Unscoped().First(&user, outcome.index+1).Error)
		require.NoError(t, db.First(&task, tasks[outcome.index].ID).Error)
		if outcome.index%4 == 3 {
			assert.Error(t, outcome.err)
			assert.False(t, outcome.won)
			assert.EqualValues(t, 900, user.Quota)
			assert.Equal(t, TaskStatus(TaskStatusQueued), task.Status)
		} else {
			assert.NoError(t, outcome.err)
			assert.True(t, outcome.won)
			assert.EqualValues(t, 940, user.Quota)
			assert.EqualValues(t, 60, user.UsedQuota)
			assert.Equal(t, 60, task.Quota)
		}
	}
	t.Log("OWNER_MUTATIONS users=200 workers=64 disabled_users=50 disabled_tokens=50 deleted_tokens=50 deleted_users=50 isolation=exact")
}

// 单订阅、单用户的多令牌集中争用同一行，64worker重复结算不得超退或串令牌。
func TestVideoSingleSubscriptionHotRow(t *testing.T) {
	db := openVideoDeepDatabase(t, "video_subscription_hot_")
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(16)
	require.NoError(t, db.Create(&User{Id: 1, Username: "subscription-hot", AffCode: "hot", Quota: 1000000, UsedQuota: 40000}).Error)
	require.NoError(t, db.Create(&UserSubscription{Id: 1, UserId: 1, AmountTotal: 100000, AmountUsed: 40000}).Error)
	require.NoError(t, db.Create(&Channel{Id: 1, UsedQuota: 40000}).Error)
	for index := range 2 {
		require.NoError(t, db.Create(&Token{Id: index + 1, UserId: 1, Key: fmt.Sprintf("subscription-hot-local-%d", index), RemainQuota: 980000, UsedQuota: 20000}).Error)
	}
	tasks := make([]Task, 400)
	for index := range tasks {
		tasks[index] = Task{TaskID: fmt.Sprintf("hot-%d", index), UserId: 1, ChannelId: 1, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{BillingSource: "subscription", SubscriptionId: 1, TokenId: index%2 + 1}}
	}
	require.NoError(t, db.CreateInBatches(tasks, 100).Error)
	jobs, results := make(chan int, 128), make(chan error, 800)
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			for index := range jobs {
				copy := tasks[index]
				copy.Status = TaskStatusSuccess
				_, err := FinalizeVideoTask(t.Context(), &copy, TaskStatusQueued, 60)
				results <- err
			}
		})
	}
	for index := range tasks {
		jobs <- index
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	var user User
	var subscription UserSubscription
	var channel Channel
	var tokens []Token
	require.NoError(t, db.First(&user, 1).Error)
	require.NoError(t, db.First(&subscription, 1).Error)
	require.NoError(t, db.First(&channel, 1).Error)
	require.NoError(t, db.Find(&tokens).Error)
	assert.EqualValues(t, 1000000, user.Quota)
	assert.EqualValues(t, 24000, user.UsedQuota)
	assert.EqualValues(t, 24000, subscription.AmountUsed)
	assert.EqualValues(t, 24000, channel.UsedQuota)
	require.Len(t, tokens, 2)
	for _, token := range tokens {
		assert.Equal(t, 988000, token.RemainQuota)
		assert.Equal(t, 12000, token.UsedQuota)
	}
	t.Log("HOT_SUBSCRIPTION workers=64 tasks=400 attempts=800 wallet_unchanged=true subscription_and_tokens=exact")
}

// 百万条历史记录保留在真实磁盘上，200用户共享4渠道、每人2令牌，一半用户走订阅。
// 每轮经过真实预扣数据准备和生产结算事务，逐户对账；持续时间显式配置且至少一小时。
func TestVideoMillionHistorySoak(t *testing.T) {
	if os.Getenv("VIDEO_SOAK_DURATION") == "" {
		t.Skip("set VIDEO_SOAK_DURATION=1h for the sustained disk-backed audit")
	}
	duration, err := time.ParseDuration(os.Getenv("VIDEO_SOAK_DURATION"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, duration, time.Hour)
	require.LessOrEqual(t, duration, 2*time.Hour)
	db := openVideoDeepDatabase(t, "video_soak_")
	// 分批造数限制内存，历史记录均为终态，不与本轮待结算任务混淆。
	for offset := 0; offset < 1000000; offset += 1000 {
		batch := make([]Task, 1000)
		for index := range batch {
			batch[index] = Task{TaskID: fmt.Sprintf("history-%d", offset+index), UserId: 1, Status: TaskStatusSuccess, SubmitTime: 1}
		}
		require.NoError(t, db.CreateInBatches(batch, 250).Error)
	}
	t.Log("SOAK history_ready=1000000 users=200 workers=64 channels=4 tokens=400 subscription_users=100")
	const initial = 1000000000
	for user := range 200 {
		id := 1000 + user
		require.NoError(t, db.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: initial}).Error)
		for token := range 2 {
			tokenID := 1000 + user*2 + token
			require.NoError(t, db.Create(&Token{Id: tokenID, UserId: id, Key: fmt.Sprintf("soak-local-%d", tokenID), RemainQuota: initial}).Error)
		}
		if user%2 == 1 {
			require.NoError(t, db.Create(&UserSubscription{Id: id, UserId: id, AmountTotal: initial}).Error)
		}
	}
	for channel := range 4 {
		require.NoError(t, db.Create(&Channel{Id: 1000 + channel}).Error)
	}
	started, lastSample := time.Now(), time.Now()
	rounds, attempts := 0, 0
	expected := make([]int, 200)
	expectedTokens := make([]int, 400)
	expectedChannels := make([]int, 4)
	for time.Since(started) < duration {
		// 预扣在独立事务准备；这里只测持久化结算稳定性，不冒充HTTP提交入口。
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&User{}).Where("id >= ?", 1000).Update("used_quota", gorm.Expr("used_quota + 200")).Error; err != nil {
				return err
			}
			if err := tx.Model(&User{}).Where("id >= ? AND id % 2 = 0", 1000).Update("quota", gorm.Expr("quota - 200")).Error; err != nil {
				return err
			}
			if err := tx.Model(&UserSubscription{}).Where("id >= ?", 1000).Update("amount_used", gorm.Expr("amount_used + 200")).Error; err != nil {
				return err
			}
			if err := tx.Model(&Token{}).Where("id >= ?", 1000).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - 100"), "used_quota": gorm.Expr("used_quota + 100")}).Error; err != nil {
				return err
			}
			return tx.Model(&Channel{}).Where("id >= ?", 1000).Update("used_quota", gorm.Expr("used_quota + 10000")).Error
		}))
		tasks := make([]Task, 400)
		for index := range tasks {
			owner := index / 2
			private := TaskPrivateData{TokenId: 1000 + index}
			if owner%2 == 1 {
				private.BillingSource, private.SubscriptionId = "subscription", 1000+owner
			}
			tasks[index] = Task{TaskID: fmt.Sprintf("soak-%d-%d", rounds, index), UserId: 1000 + owner, ChannelId: 1000 + owner%4, Quota: 100, Status: TaskStatusQueued, PrivateData: private}
		}
		require.NoError(t, db.CreateInBatches(tasks, 100).Error)
		jobs, failures := make(chan int, 128), make(chan error, 800)
		var workers sync.WaitGroup
		for range 64 {
			workers.Go(func() {
				for index := range jobs {
					copy := tasks[index]
					copy.Status = TaskStatusSuccess
					_, err := FinalizeVideoTask(t.Context(), &copy, TaskStatusQueued, []int{0, 50, 125}[index%3])
					failures <- err
				}
			})
		}
		for index := range tasks {
			jobs <- index
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		close(failures)
		for err := range failures {
			require.NoError(t, err)
		}
		for index := range tasks {
			amount := []int{0, 50, 125}[index%3]
			expected[index/2] += amount
			expectedTokens[index] += amount
			expectedChannels[index/2%4] += amount
		}
		// 批量读取后逐条核验，不依赖总和抵消；保留所有本轮任务作为累积历史。
		var users []User
		var tokens []Token
		var channels []Channel
		var subscriptions []UserSubscription
		require.NoError(t, db.Where("id >= ?", 1000).Find(&users).Error)
		require.NoError(t, db.Where("id >= ?", 1000).Find(&tokens).Error)
		require.NoError(t, db.Where("id >= ?", 1000).Find(&channels).Error)
		require.NoError(t, db.Where("id >= ?", 1000).Find(&subscriptions).Error)
		for _, user := range users {
			index := user.Id - 1000
			require.EqualValues(t, expected[index], user.UsedQuota)
			balance := initial
			if index%2 == 0 {
				balance -= expected[index]
			}
			require.EqualValues(t, balance, user.Quota)
		}
		for _, token := range tokens {
			require.Equal(t, expectedTokens[token.Id-1000], token.UsedQuota)
			require.Equal(t, initial-expectedTokens[token.Id-1000], token.RemainQuota)
		}
		for _, channel := range channels {
			require.EqualValues(t, expectedChannels[channel.Id-1000], channel.UsedQuota)
		}
		for _, subscription := range subscriptions {
			require.EqualValues(t, expected[subscription.UserId-1000], subscription.AmountUsed)
		}
		rounds++
		attempts += 800
		if time.Since(lastSample) >= time.Minute {
			runtime.GC()
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			connection, err := db.DB()
			require.NoError(t, err)
			stats := connection.Stats()
			t.Logf("SOAK elapsed=%s rounds=%d tasks=%d attempts=%d heap_bytes=%d goroutines=%d connections=%d in_use=%d wait_count=%d accounting=exact", time.Since(started).Round(time.Second), rounds, rounds*400, attempts, memory.HeapAlloc, runtime.NumGoroutine(), stats.OpenConnections, stats.InUse, stats.WaitCount)
			lastSample = time.Now()
		}
	}
	t.Logf("SOAK COMPLETE duration=%s rounds=%d tasks=%d attempts=%d accounting=exact", time.Since(started), rounds, rounds*400, attempts)
}
