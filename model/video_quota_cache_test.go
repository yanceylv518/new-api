package model

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 恢复必须保留尚未批量落库的预扣；同时覆盖普通退款与结算影子的合并。
func TestVideoCacheRecoveryPreservesPendingBatch(t *testing.T) {
	for _, actual := range []int{60, 125} {
		t.Run(fmt.Sprint(actual), func(t *testing.T) {
			db := openVideoDeepDatabase(t, "video_batch_recovery_")
			resetBatchUpdateTestState(t)
			common.BatchUpdateEnabled = true
			client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379", DB: 4})
			old := common.RDB
			common.RDB, common.RedisEnabled = client, true
			t.Cleanup(func() { common.RDB, common.RedisEnabled = old, false; assert.NoError(t, client.Close()) })
			require.NoError(t, client.FlushDB(t.Context()).Err())
			require.NoError(t, db.Create(&User{Id: 1, Username: "batch-owner", AffCode: "batch", Quota: 900, UsedQuota: 100}).Error)
			token := Token{Id: 1, UserId: 1, Key: "batch-cache-token", RemainQuota: 900, UsedQuota: 100}
			require.NoError(t, db.Create(&token).Error)
			require.NoError(t, db.Create(&Channel{Id: 1, UsedQuota: 100}).Error)
			task := Task{TaskID: "batch-task", UserId: 1, ChannelId: 1, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: 1}}
			require.NoError(t, db.Create(&task).Error)
			reserved, err := TryReserveUserQuota(1, 50)
			require.NoError(t, err)
			require.True(t, reserved)
			reserved, err = TryReserveTokenQuota(1, token.Key, 50, false)
			require.NoError(t, err)
			require.True(t, reserved)
			injected := false
			client.AddHook(videoAuditRedisHook{before: func(ctx context.Context, cmd redis.Cmder) error {
				if injected || !isVideoFinishCommand(cmd) {
					return nil
				}
				injected = true
				// 提交之后发生普通退款并丢失活缓存，只能从结算影子恢复。
				// 精确排列现有退款路径的缓存增量和批量入队，不依赖后台协程调度时序。
				if _, err := cacheApplyUserQuotaDelta(1, 10); err != nil {
					return err
				}
				if _, err := cacheApplyTokenQuotaDelta(1, token.Key, 10); err != nil {
					return err
				}
				if err := persistUserQuotaDelta(1, 10); err != nil {
					return err
				}
				if err := persistTokenQuotaDelta(1, 10); err != nil {
					return err
				}
				if err := client.Del(ctx, getUserCacheKey(1), getTokenCacheKey(token.Key)).Err(); err != nil {
					return err
				}
				return fmt.Errorf("simulated post-commit interruption")
			}})
			task.Status = TaskStatusSuccess
			won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, actual)
			require.NoError(t, err)
			require.True(t, won)
			user, err := GetUserCache(1)
			require.NoError(t, err)
			cachedToken, err := GetTokenByKey(token.Key, false)
			require.NoError(t, err)
			expected := 1000 - actual - 40
			assert.Equal(t, expected, user.Quota)
			assert.Equal(t, expected, cachedToken.RemainQuota)
			assert.Equal(t, actual+40, cachedToken.UsedQuota)
			// 批量落库后与恢复值必须完全一致，不能重复应用结算差额。
			batchUpdate()
			assert.Equal(t, expected, getUserQuotaFromDB(t, 1))
			assert.Equal(t, expected, getTokenFromDB(t, 1).RemainQuota)
		})
	}
}

// SQLite WAL允许并发读旧快照，恢复必须等待结算提交后才能消费标记。
func TestVideoSQLiteRecoveryWaitsForCommit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "quota.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(4)
	old := DB
	DB = db
	t.Cleanup(func() { DB = old; assert.NoError(t, connection.Close()) })
	useUserCacheMiniRedis(t)
	require.NoError(t, db.AutoMigrate(&User{}, &Channel{}, &Task{}))
	require.NoError(t, db.Create(&User{Id: 1, Username: "sqlite-owner", AffCode: "sqlite", Quota: 900, UsedQuota: 100}).Error)
	require.NoError(t, db.Create(&Channel{Id: 1, UsedQuota: 100}).Error)
	task := Task{TaskID: "sqlite-pending", UserId: 1, ChannelId: 1, Quota: 100, Status: TaskStatusQueued}
	require.NoError(t, db.Create(&task).Error)
	_, err = GetUserCache(1)
	require.NoError(t, err)
	prepared, release, recovering := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("video-sqlite-commit-barrier", func(tx *gorm.DB) {
		if tx.Statement.Table == "tasks" {
			close(prepared)
			<-release
		}
		if tx.Statement.Table == "users" {
			if fields, ok := tx.Statement.Dest.(map[string]interface{}); ok && fields["id"] != nil {
				recovering <- struct{}{}
			}
		}
	}))
	settled := make(chan error, 1)
	go func() {
		task.Status = TaskStatusSuccess
		_, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, 125)
		settled <- err
	}()
	select {
	case <-prepared:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "settlement did not prepare")
	}
	read := make(chan error, 1)
	go func() {
		user, err := GetUserCache(1)
		if err == nil && user.Quota != 875 {
			err = fmt.Errorf("stale quota: %d", user.Quota)
		}
		read <- err
	}()
	select {
	case <-recovering:
	case err := <-read:
		require.FailNow(t, "recovery returned before commit", "%v", err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "recovery did not lock")
	}
	once.Do(func() { close(release) })
	require.NoError(t, <-settled)
	require.NoError(t, <-read)
}

// 旧任务提交后的Lua若晚于同用户下一任务，必须通过任务ID拒绝重复应用差额。
func TestVideoCacheLateFinishDoesNotOverwriteNextSettlement(t *testing.T) {
	db := openVideoDeepDatabase(t, "video_late_finish_")
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:26379", DB: 4})
	old := common.RDB
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() { common.RDB, common.RedisEnabled = old, false; assert.NoError(t, client.Close()) })
	require.NoError(t, client.FlushDB(t.Context()).Err())
	require.NoError(t, db.Create(&User{Id: 1, Username: "late-owner", AffCode: "late", Quota: 800, UsedQuota: 200}).Error)
	token := Token{Id: 1, UserId: 1, Key: "late-token", RemainQuota: 800, UsedQuota: 200}
	require.NoError(t, db.Create(&token).Error)
	require.NoError(t, db.Create(&Channel{Id: 1, UsedQuota: 200}).Error)
	first := Task{TaskID: "first", UserId: 1, ChannelId: 1, Quota: 100, Status: TaskStatusQueued, PrivateData: TaskPrivateData{TokenId: 1}}
	second := first
	second.TaskID = "second"
	require.NoError(t, db.Create(&first).Error)
	require.NoError(t, db.Create(&second).Error)
	_, err := GetUserCache(1)
	require.NoError(t, err)
	_, err = GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	injected := false
	client.AddHook(videoAuditRedisHook{before: func(ctx context.Context, cmd redis.Cmder) error {
		if injected || !isVideoFinishCommand(cmd) {
			return nil
		}
		injected = true
		second.Status = TaskStatusSuccess
		won, err := FinalizeVideoTask(ctx, &second, TaskStatusQueued, 60)
		if err == nil && !won {
			return fmt.Errorf("second settlement did not win")
		}
		return err
	}})
	first.Status = TaskStatusSuccess
	won, err := FinalizeVideoTask(t.Context(), &first, TaskStatusQueued, 125)
	require.NoError(t, err)
	require.True(t, won)
	require.True(t, injected)
	user, err := GetUserCache(1)
	require.NoError(t, err)
	cached, err := GetTokenByKey(token.Key, false)
	require.NoError(t, err)
	assert.Equal(t, 815, user.Quota)
	assert.Equal(t, 815, cached.RemainQuota)
	assert.Equal(t, 185, cached.UsedQuota)
	assert.Equal(t, 815, getUserQuotaFromDB(t, 1))
}
