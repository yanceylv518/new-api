package model

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// Redis钩子只存在于测试进程，用于精确定位主库提交和缓存增量之间的故障窗口。
type videoAuditRedisHook struct {
	before func(context.Context, redis.Cmder) error
}

// 同时覆盖首次脚本加载和后续EVALSHA执行的同一个提交后故障窗口。
func isVideoFinishCommand(cmd redis.Cmder) bool {
	return (cmd.Name() == "eval" && cmd.Args()[1] == finishVideoQuotaScript) || (cmd.Name() == "evalsha" && cmd.Args()[1] == finishVideoQuota.Hash())
}

func (h videoAuditRedisHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	return ctx, h.before(ctx, cmd)
}
func (videoAuditRedisHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (videoAuditRedisHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (videoAuditRedisHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestVideoSettlementConcurrentTokenQuotaEdit(t *testing.T) {
	address := os.Getenv("TASK_PLUGIN_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("TASK_PLUGIN_TEST_REDIS_ADDR is unset")
	}
	require.Equal(t, "127.0.0.1:26379", address)
	for _, order := range []string{"edit_before_settlement", "edit_before_cache_finish", "edit_after_cold_finish", "edit_with_pending_batch", "edit_before_settlement_with_pending", "edit_with_large_usage", "edit_cache_failure"} {
		t.Run(order, func(t *testing.T) {
			truncateTables(t)
			client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1})
			oldRDB, oldRedis := common.RDB, common.RedisEnabled
			common.RDB, common.RedisEnabled = client, true
			t.Cleanup(func() {
				common.RDB, common.RedisEnabled = oldRDB, oldRedis
				assert.NoError(t, client.Close())
			})
			require.NoError(t, client.FlushDB(t.Context()).Err())
			require.NoError(t, DB.Create(&User{Id: 91, Username: "token-edit", AffCode: "token-edit", Quota: 900, UsedQuota: 100}).Error)
			require.NoError(t, DB.Create(&Channel{Id: 91, UsedQuota: 100}).Error)
			token := Token{Id: 91, UserId: 91, Key: "video-token-edit-test", RemainQuota: 900, UsedQuota: 100}
			if order == "edit_with_large_usage" {
				token.UsedQuota = 9_007_199_254_740_993
			}
			require.NoError(t, DB.Create(&token).Error)
			task := Task{TaskID: "token-edit-task", UserId: 91, ChannelId: 91, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: 91}}
			require.NoError(t, DB.Create(&task).Error)
			_, err := GetTokenByKey(token.Key, false)
			require.NoError(t, err)
			if order == "edit_with_pending_batch" || order == "edit_before_settlement_with_pending" {
				resetBatchUpdateTestState(t)
				common.BatchUpdateEnabled = true
				accepted, err := TryReserveTokenQuota(token.Id, token.Key, 70, false)
				require.NoError(t, err)
				require.True(t, accepted)
			}
			token.RemainQuota = 500
			if order == "edit_before_settlement" || order == "edit_before_settlement_with_pending" {
				require.NoError(t, token.Update())
			}
			if order == "edit_cache_failure" {
				injected := errors.New("injected token cache failure")
				failed := false
				client.AddHook(videoAuditRedisHook{before: func(_ context.Context, cmd redis.Cmder) error {
					if cmd.Name() == "eval" && !failed {
						failed = true
						return injected
					}
					return nil
				}})
				require.ErrorIs(t, token.Update(), injected)
				require.True(t, failed)
				var unchanged Token
				require.NoError(t, DB.First(&unchanged, token.Id).Error)
				assert.Equal(t, 900, unchanged.RemainQuota)
				require.NoError(t, token.Update())
			}
			injected := false
			client.AddHook(videoAuditRedisHook{before: func(ctx context.Context, cmd redis.Cmder) error {
				if !isVideoFinishCommand(cmd) || injected {
					return nil
				}
				injected = true
				if order == "edit_before_cache_finish" || order == "edit_with_large_usage" {
					return token.Update()
				}
				if order == "edit_after_cold_finish" || order == "edit_with_pending_batch" {
					return client.Del(ctx, getTokenCacheKey(token.Key)).Err()
				}
				return nil
			}})
			task.Status = TaskStatusSuccess
			won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, 60)
			require.NoError(t, err)
			require.True(t, won)
			require.True(t, injected)
			if order == "edit_after_cold_finish" || order == "edit_with_pending_batch" {
				require.NoError(t, token.Update())
			}
			// fence 到期后走真实冷填充，不能让旧额度影子重新授权请求。
			require.NoError(t, client.PExpireAt(t.Context(), getTokenCacheFenceKey(token.Key), time.Unix(1, 0)).Err())
			var stored Token
			require.NoError(t, DB.First(&stored, token.Id).Error)
			cached, err := GetTokenByKey(token.Key, false)
			require.NoError(t, err)
			pending := 0
			if order == "edit_with_pending_batch" || order == "edit_before_settlement_with_pending" {
				pending = 70
			}
			assert.Equal(t, stored.RemainQuota-pending, cached.RemainQuota)
			assert.Equal(t, stored.UsedQuota+pending, cached.UsedQuota)
			amount := 600
			if pending > 0 {
				amount = 500
			}
			accepted, err := TryReserveTokenQuota(token.Id, token.Key, amount, false)
			require.NoError(t, err)
			assert.False(t, accepted, "old quota must not authorize a new request")
		})
	}
}

func TestVideoSettlementAndTokenEditConcurrently(t *testing.T) {
	address := os.Getenv("TASK_PLUGIN_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("TASK_PLUGIN_TEST_REDIS_ADDR is unset")
	}
	require.Equal(t, "127.0.0.1:26379", address)
	truncateTables(t)
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1})
	oldRDB, oldRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = oldRDB, oldRedis
		assert.NoError(t, client.Close())
	})
	require.NoError(t, client.FlushDB(t.Context()).Err())
	connection, err := DB.DB()
	require.NoError(t, err)
	// SQLite :memory: 按连接隔离，保留单连接仍可验证并发调用与 Redis 交错。
	if !common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		connection.SetMaxOpenConns(32)
	}
	t.Cleanup(func() { connection.SetMaxOpenConns(1) })
	const count = 64
	tasks := make([]Task, count)
	tokens := make([]Token, count)
	for i := range count {
		id := 3000 + i
		require.NoError(t, DB.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: 900, UsedQuota: 100}).Error)
		require.NoError(t, DB.Create(&Channel{Id: id, UsedQuota: 100}).Error)
		tokens[i] = Token{Id: id, UserId: id, Key: fmt.Sprintf("concurrent-token-%d", id), RemainQuota: 900, UsedQuota: 100}
		require.NoError(t, DB.Create(&tokens[i]).Error)
		tasks[i] = Task{TaskID: fmt.Sprintf("concurrent-edit-%d", id), UserId: id, ChannelId: id, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: id}}
		require.NoError(t, DB.Create(&tasks[i]).Error)
		_, err := GetTokenByKey(tokens[i].Key, false)
		require.NoError(t, err)
	}
	start := make(chan struct{})
	results := make(chan error, count*2)
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			<-start
			task := tasks[i]
			task.Status = TaskStatusSuccess
			won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, 60)
			if err == nil && !won {
				err = errors.New("settlement did not commit")
			}
			results <- err
		})
		workers.Go(func() {
			<-start
			token := tokens[i]
			token.RemainQuota = 500
			results <- token.Update()
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	for i := range count {
		require.NoError(t, client.PExpireAt(t.Context(), getTokenCacheFenceKey(tokens[i].Key), time.Unix(1, 0)).Err())
		var stored Token
		require.NoError(t, DB.First(&stored, tokens[i].Id).Error)
		cached, err := GetTokenByKey(tokens[i].Key, false)
		require.NoError(t, err)
		assert.Equal(t, stored.RemainQuota, cached.RemainQuota)
		assert.Equal(t, stored.UsedQuota, cached.UsedQuota)
		assert.Contains(t, []int{500, 540}, stored.RemainQuota)
		accepted, err := TryReserveTokenQuota(stored.Id, stored.Key, 600, false)
		require.NoError(t, err)
		assert.False(t, accepted)
	}
}

func TestAsyncSubmissionCacheRecoversAfterCommit(t *testing.T) {
	address := os.Getenv("TASK_PLUGIN_TEST_REDIS_ADDR")
	if address == "" {
		t.Skip("TASK_PLUGIN_TEST_REDIS_ADDR is unset")
	}
	require.Equal(t, "127.0.0.1:26379", address)
	truncateTables(t)
	client := redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1})
	oldRDB, oldRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = oldRDB, oldRedis
		assert.NoError(t, client.Close())
	})
	require.NoError(t, client.FlushDB(t.Context()).Err())
	require.NoError(t, DB.Create(&User{Id: 92, Username: "async-cache", AffCode: "async-cache", Quota: 900}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 92}).Error)
	token := Token{Id: 92, UserId: 92, Key: "async-cache-test", RemainQuota: 900, UsedQuota: 100}
	require.NoError(t, DB.Create(&token).Error)
	task := Task{TaskID: "async-cache", UserId: 92, ChannelId: 92, Quota: 100, Status: TaskStatusUnknown, PrivateData: TaskPrivateData{TokenId: 92, SubmissionPending: true, InitialAccountingPending: true}}
	require.NoError(t, DB.Create(&task).Error)
	_, err := GetUserCache(92)
	require.NoError(t, err)
	_, err = GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	injected := false
	client.AddHook(videoAuditRedisHook{before: func(_ context.Context, cmd redis.Cmder) error {
		if isVideoFinishCommand(cmd) && !injected {
			injected = true
			return errors.New("injected submission cache failure")
		}
		return nil
	}})
	final := task
	final.Status, final.Quota = TaskStatusSubmitted, 60
	final.PrivateData.SubmissionPending = false
	won, err := PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: 100, UpdateToken: true})
	require.NoError(t, err)
	require.True(t, won)
	require.True(t, injected)
	var stored User
	require.NoError(t, DB.First(&stored, 92).Error)
	assert.Equal(t, 940, stored.Quota)
	cached, err := GetUserCache(92)
	require.NoError(t, err)
	assert.Equal(t, 940, cached.Quota)
	cachedToken, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 940, cachedToken.RemainQuota)
	assert.Equal(t, 60, cachedToken.UsedQuota)
	won, err = PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: 100, UpdateToken: true})
	require.NoError(t, err)
	assert.False(t, won)
}

// 子进程复用测试二进制，不依赖shell、不携带生产凭证；只访问固定的隔离库和测试Redis DB4。
func TestVideoProcessWorker(t *testing.T) {
	mode := os.Getenv("VIDEO_PROCESS_MODE")
	if mode == "" {
		t.Skip("audit child process only")
	}
	dsn, prefix := "host=127.0.0.1 port=25446 user=postgres dbname=doubao_native_test sslmode=disable", "video_process_"
	if mode == "lease-crash" {
		dsn = "host=127.0.0.1 port=25447 user=postgres dbname=doubao_native_test sslmode=disable search_path=video_audit_lease"
		prefix = ""
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: prefix}, Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	DB = db
	common.SetDatabaseTypes(common.DatabaseTypePostgreSQL, common.DatabaseTypePostgreSQL)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(8)
	defer connection.Close()
	if mode == "lease-crash" {
		_, won, err := ClaimSystemTask(1, SystemTaskTypeAsyncTaskPoll, "crashed-node", common.GetTimestamp()+60)
		require.NoError(t, err)
		require.True(t, won)
		os.Exit(87)
	}
	var tasks []Task
	require.NoError(t, db.Order("id").Find(&tasks).Error)
	if mode == "crash" || mode == "rollback-crash" || mode == "prepared-rollback-crash" {
		common.CryptoSecret = "video-audit-shared-cache-secret"
		common.RedisEnabled = true
		common.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379", DB: 4})
		if mode == "rollback-crash" {
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register("crash_before_commit", func(*gorm.DB) { os.Exit(85) }))
		}
		if mode == "prepared-rollback-crash" {
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register("crash_after_prepare", func(tx *gorm.DB) {
				if tx.Statement.Table == "video_process_tasks" {
					os.Exit(85)
				}
			}))
		}
		common.RDB.AddHook(videoAuditRedisHook{before: func(_ context.Context, cmd redis.Cmder) error {
			if isVideoFinishCommand(cmd) {
				os.Exit(86)
			}
			return nil
		}})
		tasks[0].Status = TaskStatusSuccess
		_, err = FinalizeVideoTask(t.Context(), &tasks[0], TaskStatusQueued, 125)
		require.NoError(t, err)
		t.Fatal("crash injection was not reached")
	}
	common.RedisEnabled = false
	fmt.Println("AUDIT_READY")
	_, err = bufio.NewReader(os.Stdin).ReadString('\n')
	require.NoError(t, err)
	jobs, results := make(chan int, 64), make(chan error, len(tasks))
	var workers sync.WaitGroup
	for range 32 {
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
	}
	close(jobs)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
}

// 独立进程领取轮询租约后退出，另一进程在租约过期后接管；旧执行者不能继续写状态。
func TestVideoPollingLeaseCrashTakeover(t *testing.T) {
	if os.Getenv("VIDEO_DEEP_AUDIT") != "1" {
		t.Skip("isolated PostgreSQL audit only")
	}
	dsn := "host=127.0.0.1 port=25447 user=postgres dbname=doubao_native_test sslmode=disable"
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminConnection, err := admin.DB()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, adminConnection.Close()) })
	// 固定测试schema只在创建成功后拥有，绝不覆盖已有schema。
	require.NoError(t, admin.Exec("CREATE SCHEMA video_audit_lease").Error)
	t.Cleanup(func() { assert.NoError(t, admin.Exec("DROP SCHEMA video_audit_lease CASCADE").Error) })
	db, err := gorm.Open(postgres.Open(dsn+" search_path=video_audit_lease"), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(4)
	previous := DB
	DB = db
	t.Cleanup(func() { DB = previous; assert.NoError(t, connection.Close()) })
	require.NoError(t, db.AutoMigrate(&SystemTask{}, &SystemTaskLock{}))
	first, err := CreateSystemTask(SystemTaskTypeAsyncTaskPoll, nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, first.ID)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVideoProcessWorker$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), "VIDEO_PROCESS_MODE=lease-crash")
	err = cmd.Run()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 87, exit.ExitCode())
	require.NoError(t, db.Model(&SystemTaskLock{}).Where("task_id = ?", first.TaskID).Update("locked_until", common.GetTimestamp()-1).Error)
	require.NoError(t, ExpireStaleSystemTaskLocks(common.GetTimestamp()))
	stale, err := GetSystemTaskByTaskID(first.TaskID)
	require.NoError(t, err)
	assert.Equal(t, SystemTaskStatusFailed, stale.Status)
	next, err := CreateSystemTask(SystemTaskTypeAsyncTaskPoll, nil, nil)
	require.NoError(t, err)
	_, won, err := ClaimSystemTask(next.ID, SystemTaskTypeAsyncTaskPoll, "takeover-node", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, won)
	assert.ErrorIs(t, UpdateSystemTaskState(first.TaskID, "crashed-node", map[string]any{"processed": 1}), ErrSystemTaskLockLost)
	require.NoError(t, UpdateSystemTaskState(next.TaskID, "takeover-node", map[string]any{"processed": 1}))
	t.Log("LEASE crashed_process_released=true expired_run_failed=true takeover_succeeded=true stale_writer_rejected=true")
}

// 两个独立进程先各自读到同一待处理快照，再同时用32线程结算；验证数据库锁跨进程有效。
func TestVideoTwoProcessSettlement(t *testing.T) {
	db := openVideoDeepDatabase(t, "video_process_")
	require.NoError(t, db.Create(&Channel{Id: 1, UsedQuota: 40000}).Error)
	for user := range 200 {
		id := user + 1
		require.NoError(t, db.Create(&User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: 9800, UsedQuota: 200}).Error)
		for index := range 2 {
			require.NoError(t, db.Create(&Task{TaskID: fmt.Sprintf("process-%d-%d", user, index), UserId: id, ChannelId: 1, Quota: 100, Status: TaskStatusQueued}).Error)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var commands []*exec.Cmd
	var starts []func()
	for range 2 {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVideoProcessWorker$", "-test.timeout=90s")
		cmd.Env = append(os.Environ(), "VIDEO_PROCESS_MODE=compete")
		stdout, err := cmd.StdoutPipe()
		require.NoError(t, err)
		stdin, err := cmd.StdinPipe()
		require.NoError(t, err)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		scanner := bufio.NewScanner(stdout)
		require.True(t, scanner.Scan())
		require.Equal(t, "AUDIT_READY", scanner.Text())
		commands = append(commands, cmd)
		starts = append(starts, func() {
			_, err := fmt.Fprintln(stdin, "start")
			require.NoError(t, err)
			require.NoError(t, stdin.Close())
		})
	}
	for _, start := range starts {
		start()
	}
	for _, cmd := range commands {
		require.NoError(t, cmd.Wait())
	}
	var users []User
	require.NoError(t, db.Find(&users).Error)
	for _, user := range users {
		assert.EqualValues(t, 9880, user.Quota)
		assert.EqualValues(t, 120, user.UsedQuota)
	}
	var channel Channel
	require.NoError(t, db.First(&channel, 1).Error)
	assert.EqualValues(t, 24000, channel.UsedQuota)
	var unfinished int64
	require.NoError(t, db.Model(&Task{}).Where("status <> ? OR quota <> ?", TaskStatusSuccess, 60).Count(&unfinished).Error)
	assert.Zero(t, unfinished)
	t.Log("MULTIPROCESS users=200 processes=2 workers=64 tasks=400 attempts=800 accounting=exact")
}

// 精确复现提交后崩溃、Redis写失败以及冷缓存恰在提交后重建的时序。
// 断言要求立即一致；失败保留为真实审查发现，不能将不一致改写成成功断言。
func TestVideoSettlementCacheFaultWindows(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "video-audit-shared-cache-secret"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })
	for _, mode := range []string{"crash", "rollback-crash", "prepared-rollback-crash", "redis-write-failure", "redis-network-recovery", "cold-rebuild", "cold-refund", "expired-before-settlement", "evicted-before-settlement"} {
		t.Run(mode, func(t *testing.T) {
			actualQuota := 125
			if mode == "cold-refund" {
				actualQuota = 60
			}
			expectedBalance := 1000 - actualQuota
			db := openVideoDeepDatabase(t, "video_process_")
			require.NoError(t, db.Create(&User{Id: 1, Username: "fault-owner", AffCode: "fault", Quota: 900, UsedQuota: 100}).Error)
			require.NoError(t, db.Create(&Token{Id: 1, UserId: 1, Key: "fault-local-token", RemainQuota: 900, UsedQuota: 100}).Error)
			require.NoError(t, db.Create(&Channel{Id: 1, UsedQuota: 100}).Error)
			task := Task{TaskID: "fault-task", UserId: 1, ChannelId: 1, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: 1}}
			require.NoError(t, db.Create(&task).Error)
			client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379", DB: 4, MaxRetries: -1})
			oldRDB := common.RDB
			common.RDB, common.RedisEnabled = client, true
			t.Cleanup(func() { common.RDB, common.RedisEnabled = oldRDB, false; assert.NoError(t, client.Close()) })
			require.NoError(t, client.FlushDB(t.Context()).Err())
			_, err := GetUserCache(1)
			require.NoError(t, err)
			var token Token
			require.NoError(t, db.First(&token, 1).Error)
			_, err = cacheInitToken(token)
			require.NoError(t, err)
			if mode == "crash" || mode == "rollback-crash" || mode == "prepared-rollback-crash" {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestVideoProcessWorker$", "-test.timeout=25s")
				cmd.Env = append(os.Environ(), "VIDEO_PROCESS_MODE="+mode)
				err := cmd.Run()
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				if mode != "crash" {
					require.Equal(t, 85, exit.ExitCode())
					var rolledBack User
					require.NoError(t, db.First(&rolledBack, 1).Error)
					require.EqualValues(t, 900, rolledBack.Quota)
					cached, err := GetUserCache(1)
					require.NoError(t, err)
					require.EqualValues(t, 900, cached.Quota)
					task.Status = TaskStatusSuccess
					won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, actualQuota)
					require.NoError(t, err)
					require.True(t, won)
				} else {
					require.Equal(t, 86, exit.ExitCode())
				}
			} else if mode == "redis-network-recovery" || mode == "expired-before-settlement" || mode == "evicted-before-settlement" {
				var offline *redis.Client
				if mode == "redis-network-recovery" {
					// 实际TCP连接失败后切回原Redis，验证恢复服务不会继续使用丢失增量的旧值。
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					require.NoError(t, err)
					address := listener.Addr().String()
					require.NoError(t, listener.Close())
					offline = redis.NewClient(&redis.Options{Addr: address, MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
					common.RDB = offline
				} else {
					for _, key := range []string{getUserCacheKey(1), getTokenCacheKey(token.Key)} {
						if mode == "expired-before-settlement" {
							require.NoError(t, client.PExpireAt(t.Context(), key, time.Unix(1, 0)).Err())
						} else {
							require.NoError(t, client.Del(t.Context(), key).Err())
						}
					}
				}
				task.Status = TaskStatusSuccess
				won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, actualQuota)
				common.RDB = client
				if offline != nil {
					require.NoError(t, offline.Close())
					// 无法建立缓存恢复标记时主库必须回滚，网络恢复后才允许结算。
					require.Error(t, err)
					require.False(t, won)
					var unchanged User
					require.NoError(t, db.First(&unchanged, 1).Error)
					require.EqualValues(t, 900, unchanged.Quota)
					won, err = FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, actualQuota)
				}
				require.NoError(t, err)
				require.True(t, won)
			} else {
				// 独立客户端绕过故障钩子，模拟另一个请求在DB提交后读到最新值并水合冷缓存。
				reader := redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379", DB: 4})
				defer reader.Close()
				injected := false
				client.AddHook(videoAuditRedisHook{before: func(ctx context.Context, cmd redis.Cmder) error {
					if !isVideoFinishCommand(cmd) || injected {
						return nil
					}
					injected = true
					if mode == "redis-write-failure" {
						return fmt.Errorf("injected Redis connection failure")
					}
					if err := reader.Del(ctx, getUserCacheKey(1), getTokenCacheKey(token.Key)).Err(); err != nil {
						return err
					}
					common.RDB = reader
					defer func() { common.RDB = client }()
					if _, err := GetUserCache(1); err != nil {
						return err
					}
					_, err := GetTokenByKey(token.Key, false)
					return err
				}})
				task.Status = TaskStatusSuccess
				won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, actualQuota)
				require.NoError(t, err)
				require.True(t, won)
			}
			var stored User
			require.NoError(t, db.First(&stored, 1).Error)
			require.EqualValues(t, expectedBalance, stored.Quota)
			cached, err := GetUserCache(1)
			require.NoError(t, err)
			cachedToken, err := GetTokenByKey("fault-local-token", false)
			require.NoError(t, err)
			assert.EqualValues(t, expectedBalance, cached.Quota, "committed wallet must be visible after %s", mode)
			assert.Equal(t, expectedBalance, cachedToken.RemainQuota, "committed token must be visible after %s", mode)
			if mode == "redis-network-recovery" {
				// 不只检查展示值：恢复后旧缓存不能放行超出主库真实余额的新请求。
				reserved, err := TryReserveUserQuota(1, 890)
				require.NoError(t, err)
				assert.False(t, reserved, "stale cache must not authorize overspending")
				tokenReserved, err := TryReserveTokenQuota(1, token.Key, 890, false)
				require.NoError(t, err)
				assert.False(t, tokenReserved)
				require.NoError(t, db.First(&stored, 1).Error)
				require.NoError(t, db.First(&token, 1).Error)
				assert.GreaterOrEqual(t, stored.Quota, 0)
				assert.GreaterOrEqual(t, token.RemainQuota, 0)
				t.Logf("STALE_RESERVE wallet_accepted=%t token_accepted=%t wallet_db=%d token_db=%d", reserved, tokenReserved, stored.Quota, token.RemainQuota)
			}
			// 故障客户端退出后显式淘汰，确认重新水合可恢复主库事实，且终态不能二次扣费。
			require.NoError(t, client.Del(t.Context(), getUserCacheKey(1), getTokenCacheKey("fault-local-token")).Err())
			cached, err = GetUserCache(1)
			require.NoError(t, err)
			assert.EqualValues(t, stored.Quota, cached.Quota)
			task.Status = TaskStatusSuccess
			won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, actualQuota)
			require.NoError(t, err)
			assert.False(t, won)
			t.Logf("FAULT mode=%s db_quota=%d recovery_after_eviction=%d", strings.TrimSpace(mode), stored.Quota, cached.Quota)
		})
	}
}

func TestImmediateTaskCacheRecoversAfterCommit(t *testing.T) {
	db := openVideoDeepDatabase(t, "video_submission_")
	require.NoError(t, db.AutoMigrate(&VideoTaskPendingLog{}))
	t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(&VideoTaskPendingLog{})) })
	require.NoError(t, db.Create(&User{Id: 1, Username: "submission-owner", AffCode: "submission-owner", Quota: 900}).Error)
	require.NoError(t, db.Create(&Token{Id: 1, UserId: 1, Key: "submission-local-token", RemainQuota: 900, UsedQuota: 100}).Error)
	require.NoError(t, db.Create(&Channel{Id: 1}).Error)
	task := Task{TaskID: "submission-cache", UserId: 1, ChannelId: 1, Quota: 100, Status: TaskStatusUnknown, PrivateData: TaskPrivateData{TokenId: 1, SubmissionPending: true, InitialAccountingPending: true}}
	require.NoError(t, db.Create(&task).Error)
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379", DB: 4, MaxRetries: -1})
	oldRDB, oldRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() { common.RDB, common.RedisEnabled = oldRDB, oldRedis; assert.NoError(t, client.Close()) })
	require.NoError(t, client.FlushDB(t.Context()).Err())
	_, err := GetUserCache(1)
	require.NoError(t, err)
	var token Token
	require.NoError(t, db.First(&token, 1).Error)
	_, err = cacheInitToken(token)
	require.NoError(t, err)
	injected := false
	client.AddHook(videoAuditRedisHook{before: func(_ context.Context, cmd redis.Cmder) error {
		if isVideoFinishCommand(cmd) && !injected {
			injected = true
			return fmt.Errorf("injected submission cache failure")
		}
		return nil
	}})
	final := task
	final.Status, final.Quota = TaskStatusSuccess, 60
	final.PrivateData.SubmissionPending = false
	won, err := PersistTaskSubmission(t.Context(), &final, true, ImmediateTaskFunding{ReservedQuota: 100, UpdateToken: true})
	require.NoError(t, err)
	require.True(t, won)
	require.True(t, injected)
	var user User
	require.NoError(t, db.First(&user, 1).Error)
	assert.Equal(t, 940, user.Quota)
	cached, err := GetUserCache(1)
	require.NoError(t, err)
	assert.Equal(t, 940, cached.Quota)
	cachedToken, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 940, cachedToken.RemainQuota)
	assert.Equal(t, 60, cachedToken.UsedQuota)
}
