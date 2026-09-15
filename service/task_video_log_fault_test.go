package service

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 独立日志库慢写时主库账务已经完整提交；日志失败后的重试是否能补齐由明确断言检查。
func TestVideoIndependentLogFailureAndSlowWrite(t *testing.T) {
	if os.Getenv("VIDEO_DEEP_AUDIT") != "1" {
		t.Skip("explicit isolated fault audit only")
	}
	for _, mode := range []string{"slow", "failure", "ack-failure"} {
		t.Run(mode, func(t *testing.T) {
			// 独立磁盘主库允许在投递持有一条连接时，从另一连接观察已提交资金。
			mainDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "main.db")), &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, mainDB.Exec("PRAGMA journal_mode=WAL").Error)
			require.NoError(t, mainDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Task{}, &model.VideoTaskPendingLog{}))
			oldMain := model.DB
			model.DB = mainDB
			t.Cleanup(func() {
				model.DB = oldMain
				connection, err := mainDB.DB()
				require.NoError(t, err)
				assert.NoError(t, connection.Close())
			})
			seedUser(t, 970, 900)
			seedChannel(t, 970)
			seedChargedAccounting(t, 970, 970, 0, 100, 1)
			logPath := filepath.Join(t.TempDir(), "separate-log.db")
			logDB, err := gorm.Open(sqlite.Open(logPath), &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, logDB.AutoMigrate(&model.Log{}))
			previous := model.LOG_DB
			model.LOG_DB = logDB
			t.Cleanup(func() {
				model.LOG_DB = previous
				connection, err := logDB.DB()
				assert.NoError(t, err)
				assert.NoError(t, connection.Close())
			})
			task := model.Task{TaskID: "separate-log-" + mode, UserId: 970, ChannelId: 970, Quota: 100, Status: model.TaskStatusQueued}
			require.NoError(t, model.DB.Create(&task).Error)
			entered, release := make(chan struct{}), make(chan struct{})
			require.NoError(t, logDB.Callback().Create().Before("gorm:create").Register("video-log-fault", func(tx *gorm.DB) {
				if mode != "slow" {
					return
				}
				close(entered)
				select {
				case <-release:
				case <-t.Context().Done():
					tx.AddError(t.Context().Err())
				}
			}))
			// 关闭真实日志连接模拟库不可用，恢复时重开同一磁盘文件。
			if mode == "failure" {
				connection, err := logDB.DB()
				require.NoError(t, err)
				require.NoError(t, connection.Close())
			}
			if mode == "ack-failure" {
				require.NoError(t, mainDB.Callback().Delete().Before("gorm:delete").Register("video-log-ack", func(tx *gorm.DB) { tx.AddError(fmt.Errorf("injected acknowledgement failure")) }))
			}
			original := task
			task.Status = model.TaskStatusSuccess
			done := make(chan error, 1)
			go func() {
				_, err := FinalizeVideoTaskBilling(t.Context(), &task, model.TaskStatusQueued, 60, "audit", nil)
				done <- err
			}()
			if mode == "slow" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "log writer did not reach barrier")
				}
				assert.Equal(t, 940, getUserQuota(t, 970))
				// 另一个投递者不得与尚在慢写的事件争抢，也不应阻塞主库读取。
				require.NoError(t, model.DeliverVideoTaskLog(t.Context(), task.ID))
				var stored model.Task
				require.NoError(t, model.DB.First(&stored, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
				close(release)
			}
			require.NoError(t, <-done)
			if mode == "ack-failure" {
				require.NoError(t, mainDB.Callback().Delete().Remove("video-log-ack"))
			}
			require.NoError(t, logDB.Callback().Create().Remove("video-log-fault"))
			if mode == "failure" {
				logDB, err = gorm.Open(sqlite.Open(logPath), &gorm.Config{})
				require.NoError(t, err)
				model.LOG_DB = logDB
			}
			if mode != "slow" {
				assert.False(t, model.HasUnfinishedSyncTasks())
				assert.True(t, model.HasPendingVideoTaskLogs())
				// 显式推进租约到期，不依赖30秒真实等待；确认失败保留原事件身份。
				require.NoError(t, mainDB.Model(&model.VideoTaskPendingLog{}).Where("task_id = ?", task.ID).Update("claim_until", 0).Error)
				// 所有任务已终态时也能独立补写，确认失败后重试不得重复日志。
				require.NoError(t, model.RetryVideoTaskLogs(t.Context()))
				assert.False(t, model.HasPendingVideoTaskLogs())
			}
			original.Status = model.TaskStatusSuccess
			won, err := FinalizeVideoTaskBilling(t.Context(), &original, model.TaskStatusQueued, 60, "audit", nil)
			require.NoError(t, err)
			assert.False(t, won)
			assert.Equal(t, 940, getUserQuota(t, 970))
			var count int64
			require.NoError(t, logDB.Model(&model.Log{}).Count(&count).Error)
			assert.EqualValues(t, 1, count, "independent log recovery must retain the adjustment record")
		})
	}
}
