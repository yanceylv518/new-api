package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTaskPollingIsolatesUnreadableCredentials(t *testing.T) {
	truncateTables(t)
	previous := common.CryptoSecret
	common.CryptoSecret = "fixture-persistent-polling-secret"
	t.Cleanup(func() { common.CryptoSecret = previous })
	before := &Task{TaskID: "readable-before", Status: TaskStatusQueued, Progress: "0%", Quota: 31, PrivateData: TaskPrivateData{Key: "fixture-before-key"}}
	broken := &Task{TaskID: "unreadable", Status: TaskStatusQueued, Progress: "0%", Quota: 41}
	after := &Task{TaskID: "readable-after", Status: TaskStatusInProgress, Progress: "10%", Quota: 51, PrivateData: TaskPrivateData{Key: "fixture-after-key"}}
	for _, task := range []*Task{before, broken, after} {
		require.NoError(t, DB.Create(task).Error)
	}
	badJSON := `{"key":"task-sealed:v1:invalid","token_id":7}`
	require.NoError(t, DB.Model(broken).UpdateColumn("private_data", badJSON).Error)
	tasks, err := GetUnfinishedSyncTasksAfter(3, 0)
	require.NoError(t, err)
	require.Len(t, tasks, 3)
	assert.Equal(t, "fixture-before-key", tasks[0].PrivateData.Key)
	assert.NoError(t, tasks[0].PrivateDataReadError)
	assert.Equal(t, broken.ID, tasks[1].ID)
	assert.Error(t, tasks[1].PrivateDataReadError)
	assert.Empty(t, tasks[1].PrivateData.Key)
	assert.Zero(t, tasks[1].PrivateData.TokenId)
	assert.Equal(t, "fixture-after-key", tasks[2].PrivateData.Key)
	assert.NoError(t, tasks[2].PrivateDataReadError)
	wrapped, err := GetUnfinishedSyncTasksAfter(2, broken.ID)
	require.NoError(t, err)
	require.Len(t, wrapped, 2)
	assert.Equal(t, []int64{after.ID, before.ID}, []int64{wrapped[0].ID, wrapped[1].ID})
	var stored string
	require.NoError(t, DB.Model(&Task{}).Select("private_data").Where("id = ?", broken.ID).Scan(&stored).Error)
	assert.JSONEq(t, badJSON, stored)
	var quota int
	require.NoError(t, DB.Model(&Task{}).Select("quota").Where("id = ?", broken.ID).Scan(&quota).Error)
	assert.Equal(t, 41, quota)
	assert.False(t, DB.Migrator().HasColumn(&Task{}, "private_data_read_error"))
}

func TestTaskLegacyCredentialsEncryptOnWriteback(t *testing.T) {
	truncateTables(t)
	previous := common.CryptoSecret
	common.CryptoSecret = "fixture-persistent-writeback-secret"
	t.Cleanup(func() { common.CryptoSecret = previous })
	task := &Task{TaskID: "legacy-plaintext", Status: TaskStatusQueued, Progress: "0%"}
	require.NoError(t, DB.Create(task).Error)
	require.NoError(t, DB.Model(task).UpdateColumn("private_data", `{"key":"fixture-legacy-key","token_id":8}`).Error)
	require.NoError(t, DB.First(task, task.ID).Error)
	assert.Equal(t, "fixture-legacy-key", task.PrivateData.Key)
	require.NoError(t, DB.Model(task).Update("private_data", task.PrivateData).Error)
	var stored string
	require.NoError(t, DB.Model(task).Select("private_data").Scan(&stored).Error)
	assert.Contains(t, stored, "task-sealed:v1:")
	assert.NotContains(t, stored, "fixture-legacy-key")
	require.NoError(t, DB.First(task, task.ID).Error)
	assert.Equal(t, "fixture-legacy-key", task.PrivateData.Key)
	assert.Equal(t, 8, task.PrivateData.TokenId)
}

// 状态索引优化不得遗漏可重试任务，也不能重新领取终态或进度已完成的任务。
func TestVideoPollingQueryStatusBoundaries(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	old := DB
	DB = db
	t.Cleanup(func() { DB = old; assert.NoError(t, connection.Close()) })
	require.NoError(t, db.AutoMigrate(&Task{}))
	states := []TaskStatus{TaskStatusNotStart, TaskStatusSubmitted, TaskStatusQueued, TaskStatusInProgress, TaskStatusUnknown, "", TaskStatusSuccess, TaskStatusFailure}
	var pending, expired []int64
	for i, status := range states {
		for _, progress := range []string{"0%", "100%"} {
			task := Task{TaskID: fmt.Sprintf("%d-%s", i, progress), Status: status, Progress: progress, SubmitTime: 100}
			require.NoError(t, db.Create(&task).Error)
			if i < 6 && progress == "0%" {
				pending = append(pending, task.ID)
				expired = append(expired, task.ID)
			}
		}
	}
	boundary := Task{TaskID: "cutoff-boundary", Status: TaskStatusQueued, Progress: "0%", SubmitTime: 101}
	require.NoError(t, db.Create(&boundary).Error)
	pending = append(pending, boundary.ID)
	assert.True(t, HasUnfinishedSyncTasks())
	var ids []int64
	for _, task := range GetAllUnFinishSyncTasks(100) {
		ids = append(ids, task.ID)
	}
	assert.Equal(t, pending, ids)
	ids = nil
	for _, task := range GetTimedOutUnfinishedTasks(101, 100) {
		ids = append(ids, task.ID)
	}
	assert.ElementsMatch(t, expired, ids)
	assert.Len(t, GetAllUnFinishSyncTasks(2), 2)
	require.NoError(t, db.Model(&Task{}).Where("id IN ?", pending).Update("status", TaskStatusSuccess).Error)
	assert.False(t, HasUnfinishedSyncTasks())
	assert.Empty(t, GetAllUnFinishSyncTasks(100))
}

// 用同一历史数据同时记录旧查询与索引查询的真实执行计划，不以机器相关时间作断言。
func TestVideoPollingMillionHistoryPlan(t *testing.T) {
	db := openVideoDeepDatabase(t, "video_poll_plan_")
	require.NoError(t, db.Exec("INSERT INTO video_poll_plan_tasks (task_id,status,progress,submit_time) SELECT 'history-' || n, 'SUCCESS', '100%', 1 FROM generate_series(1,2400000) n").Error)
	require.NoError(t, db.Exec("ANALYZE video_poll_plan_tasks").Error)
	for _, where := range []string{"status NOT IN ('SUCCESS','FAILURE')", "status IN ('NOT_START','SUBMITTED','QUEUED','IN_PROGRESS','UNKNOWN','')"} {
		rows, err := db.Raw("EXPLAIN (ANALYZE,BUFFERS) SELECT * FROM video_poll_plan_tasks WHERE progress != '100%' AND " + where + " ORDER BY id LIMIT 100").Rows()
		require.NoError(t, err)
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			t.Log(line)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	assert.False(t, HasUnfinishedSyncTasks())
	assert.Empty(t, GetAllUnFinishSyncTasks(100))
}
