package model

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/clickhouse"
	"gorm.io/gorm"
)

// 使用真实ClickHouse和生产日志表建表语句，验证200用户64并发写入不会丢字段或串用户。
func TestVideoClickHouseBillingLogLoad(t *testing.T) {
	db := openVideoDeepDatabase(t, "video_clickhouse_")
	require.NoError(t, db.AutoMigrate(&VideoTaskPendingLog{}))
	t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(&VideoTaskPendingLog{})) })
	logDB, err := gorm.Open(clickhouse.Open("clickhouse://127.0.0.1:29000/default"), &gorm.Config{})
	require.NoError(t, err)
	oldLog, oldConsume := LOG_DB, common.LogConsumeEnabled
	oldType := common.LogDatabaseType()
	LOG_DB, common.LogConsumeEnabled = logDB, true
	common.SetLogDatabaseType(common.DatabaseTypeClickHouse)
	initCol()
	require.NoError(t, migrateClickHouseLogDB())
	t.Cleanup(func() {
		assert.NoError(t, logDB.Migrator().DropTable(&Log{}))
		connection, err := logDB.DB()
		assert.NoError(t, err)
		assert.NoError(t, connection.Close())
		LOG_DB, common.LogConsumeEnabled = oldLog, oldConsume
		common.SetLogDatabaseType(oldType)
		initCol()
	})
	for user := range 200 {
		id := user + 1
		require.NoError(t, db.Create(&User{Id: id, Username: fmt.Sprintf("video-user-%d", id), AffCode: fmt.Sprint(id), Quota: 8000, UsedQuota: 2000}).Error)
	}
	require.NoError(t, db.Create(&Channel{Id: 1, UsedQuota: 400000}).Error)
	tasks := make([]Task, 4000)
	for index := range tasks {
		tasks[index] = Task{TaskID: fmt.Sprintf("audit-%d", index), UserId: index%200 + 1, ChannelId: 1, Quota: 100, Status: TaskStatusQueued}
	}
	require.NoError(t, db.CreateInBatches(tasks, 100).Error)
	// 首条日志写入后主库确认失败一次，恢复必须识别已写日志而不是重复插入。
	ackFailure := errors.New("injected video log acknowledgement failure")
	var interrupted atomic.Bool
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("video-ch-ack", func(tx *gorm.DB) {
		if pending, ok := tx.Statement.Dest.(*VideoTaskPendingLog); ok && pending.TaskID == tasks[0].ID && interrupted.CompareAndSwap(false, true) {
			tx.AddError(ackFailure)
		}
	}))
	t.Cleanup(func() { assert.NoError(t, db.Callback().Delete().Remove("video-ch-ack")) })
	jobs := make(chan int, 128)
	results := make(chan error, len(tasks))
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			for index := range jobs {
				other := NewLogOther()
				other.SetPublic("task_id", fmt.Sprintf("audit-%d", index))
				task := tasks[index]
				task.Status = TaskStatusSuccess
				log := &Log{UserId: task.UserId, Username: fmt.Sprintf("video-user-%d", task.UserId), CreatedAt: common.GetTimestamp(), RequestId: fmt.Sprintf("video-audit-%d", index), Type: LogTypeRefund, Content: "isolated video refund", ModelName: "MiniMax-H3", Quota: 40, ChannelId: 1, Group: "default", Other: other.JSONString()}
				won, err := FinalizeVideoTask(t.Context(), &task, TaskStatusQueued, 60, log)
				if err == nil && !won {
					err = fmt.Errorf("task %d did not settle", index)
				}
				if err == nil && index%10 != 9 {
					err = DeliverVideoTaskLog(t.Context(), task.ID)
				}
				if errors.Is(err, ackFailure) {
					err = nil
				}
				results <- err
			}
		})
	}
	for index := range 4000 {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.True(t, interrupted.Load())
	// 模拟确认失败进程的租约到期，之后由新一轮补写接管。
	require.NoError(t, db.Model(&VideoTaskPendingLog{}).Where("task_id = ?", tasks[0].ID).Update("claim_until", 0).Error)
	// 每十条刻意跳过即时投递，验证进程重启后的有界补写入口。
	for range 40 {
		if !HasPendingVideoTaskLogs() {
			break
		}
		require.NoError(t, RetryVideoTaskLogs(t.Context()))
	}
	require.False(t, HasPendingVideoTaskLogs())
	var logs []Log
	require.NoError(t, logDB.Find(&logs).Error)
	// 失败只报告数量，避免将数千条完整业务日志淹没诊断输出。
	require.Equal(t, 4000, len(logs), "persisted log count")
	counts := make(map[int]int)
	seen := make(map[string]bool)
	for _, entry := range logs {
		assert.Equal(t, 40, entry.Quota)
		assert.Equal(t, fmt.Sprintf("video-user-%d", entry.UserId), entry.Username)
		assert.False(t, seen[entry.Other])
		seen[entry.Other] = true
		counts[entry.UserId]++
	}
	for user := range 200 {
		assert.Equal(t, 20, counts[user+1])
		var owner User
		require.NoError(t, db.First(&owner, user+1).Error)
		assert.Equal(t, 8800, owner.Quota)
		assert.Equal(t, 1200, owner.UsedQuota)
	}
	t.Log("CLICKHOUSE users=200 workers=64 logs=4000 unique=4000 fields=exact")
}
