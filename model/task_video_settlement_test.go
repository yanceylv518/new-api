package model

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

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
					task := &Task{TaskID: fmt.Sprint(id), UserId: id, ChannelId: id, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: id}}
					require.NoError(t, db.Create(task).Error)
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
				tasks[index] = Task{TaskID: fmt.Sprintf("load-%d", index), UserId: 1000 + owner, ChannelId: 1000 + owner, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: 1000 + owner}}
			}
			for owner := range userCount {
				id := 1000 + owner
				require.NoError(t, db.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: initialQuota - reserved[owner], UsedQuota: reserved[owner]}).Error)
				require.NoError(t, db.Create(&Token{Id: id, UserId: id, Key: fmt.Sprintf("audit-load-token-%d", id), RemainQuota: initialQuota - reserved[owner], UsedQuota: reserved[owner]}).Error)
				require.NoError(t, db.Create(&Channel{Id: id, UsedQuota: int64(reserved[owner])}).Error)
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
				assert.EqualValues(t, expected[owner], channel.UsedQuota)
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
