package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	commonRelay "github.com/QuantumNous/new-api/relay/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func withTaskRequestSnapshotSettings(t *testing.T, enabled bool, maxBytes, maxDepth, maxItems int) {
	t.Helper()
	previous := [4]any{
		constant.TaskRequestSnapshotEnabled,
		constant.TaskRequestSnapshotMaxBytes,
		constant.TaskRequestSnapshotMaxDepth,
		constant.TaskRequestSnapshotMaxItems,
	}
	constant.TaskRequestSnapshotEnabled = enabled
	constant.TaskRequestSnapshotMaxBytes = maxBytes
	constant.TaskRequestSnapshotMaxDepth = maxDepth
	constant.TaskRequestSnapshotMaxItems = maxItems
	t.Cleanup(func() {
		constant.TaskRequestSnapshotEnabled = previous[0].(bool)
		constant.TaskRequestSnapshotMaxBytes = previous[1].(int)
		constant.TaskRequestSnapshotMaxDepth = previous[2].(int)
		constant.TaskRequestSnapshotMaxItems = previous[3].(int)
	})
}

func TestNewTaskRequestSnapshotRedactsMediaAndCredentials(t *testing.T) {
	withTaskRequestSnapshotSettings(t, true, 256*1024, 16, 1000)
	task := &Task{
		TaskID:     "task_snapshot_redaction",
		UserId:     7,
		Platform:   "doubao",
		Action:     constant.TaskActionImageToVideo,
		Properties: Properties{OriginModelName: "doubao-seedance-2-0-fast-260128"},
	}
	body := map[string]any{
		"model":  task.Properties.OriginModelName,
		"prompt": "让人物自然转身",
		"content": []any{
			map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:image/jpeg;base64,SECRET_BASE64_CONTENT",
				},
			},
		},
		"image_base64": "RAW_BASE64_CONTENT",
		"access_token": "must-not-persist",
		"authToken":    "must-not-persist-camel-case",
		"clientSecret": "must-not-persist-camel-case-secret",
		"asset":        "asset://asset-example",
		"token_count":  12,
		"signed_url":   "https://user:URL_PASSWORD@assets.example/video.mp4?X-Tos-Signature=URL_SECRET&expires=3600",
		"binary":       []byte("raw binary content"),
	}

	snapshot, err := NewTaskRequestSnapshot(task, body)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.True(t, snapshot.Base64Omitted)
	assert.False(t, snapshot.Truncated)
	assert.Equal(t, task.Properties.OriginModelName, snapshot.Model)
	assert.NotContains(t, string(snapshot.Body), "SECRET_BASE64_CONTENT")
	assert.NotContains(t, string(snapshot.Body), "RAW_BASE64_CONTENT")
	assert.NotContains(t, string(snapshot.Body), "must-not-persist")
	assert.NotContains(t, string(snapshot.Body), "must-not-persist-camel-case")
	assert.NotContains(t, string(snapshot.Body), "must-not-persist-camel-case-secret")
	assert.NotContains(t, string(snapshot.Body), "URL_SECRET")
	assert.NotContains(t, string(snapshot.Body), "URL_PASSWORD")
	assert.Contains(t, string(snapshot.Body), "assets.example/video.mp4")
	assert.NotContains(t, string(snapshot.Body), "raw binary content")
	assert.Contains(t, string(snapshot.Body), taskRequestSnapshotBase64Marker)
	assert.Contains(t, string(snapshot.Body), taskRequestSnapshotRedactedMarker)
	assert.Contains(t, string(snapshot.Body), "asset://asset-example")
	assert.Contains(t, string(snapshot.Body), "token_count")
}

func TestNewTaskRequestSnapshotRedactsRawBase64Media(t *testing.T) {
	withTaskRequestSnapshotSettings(t, true, 256*1024, 16, 1000)
	task := &Task{
		TaskID:   "task_snapshot_raw_base64",
		UserId:   7,
		Platform: "vertex-ai",
		Action:   constant.TaskActionImageToVideo,
	}
	rawBase64 := "iVBORw0KGgo" + strings.Repeat("A", 128)
	snapshot, err := NewTaskRequestSnapshot(task, map[string]any{
		"prompt": "animate this image",
		"images": []any{rawBase64},
	})
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.True(t, snapshot.Base64Omitted)
	assert.NotContains(t, string(snapshot.Body), rawBase64)
	assert.Contains(t, string(snapshot.Body), taskRequestSnapshotBase64Marker)
}

func TestNewTaskRequestSnapshotRedactsTypedTaskRequestMedia(t *testing.T) {
	withTaskRequestSnapshotSettings(t, true, 256*1024, 16, 1000)
	task := &Task{TaskID: "task_snapshot_typed_request", UserId: 7, Platform: "hailuo", Action: constant.TaskActionImageToVideo}
	rawBase64 := "iVBORw0KGgo" + strings.Repeat("A", 128)
	snapshot, err := NewTaskRequestSnapshot(task, commonRelay.TaskSubmitReq{
		Prompt: "animate this image",
		Images: []string{rawBase64},
	})
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.True(t, snapshot.Base64Omitted)
	assert.NotContains(t, string(snapshot.Body), rawBase64)
	assert.Contains(t, string(snapshot.Body), taskRequestSnapshotBase64Marker)
}

func TestNewTaskRequestSnapshotBoundsDepthItemsAndBytes(t *testing.T) {
	withTaskRequestSnapshotSettings(t, true, 128, 3, 2)
	task := &Task{
		TaskID:   "task_snapshot_bounds",
		UserId:   7,
		Platform: "doubao",
		Action:   constant.TaskActionTextToVideo,
	}
	body := map[string]any{
		"prompt": strings.Repeat("large prompt ", 100),
		"items":  []any{1, 2, 3},
		"nested": map[string]any{"level": map[string]any{"too_deep": true}},
	}

	snapshot, err := NewTaskRequestSnapshot(task, body)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.True(t, snapshot.Truncated)
	assert.LessOrEqual(t, snapshot.BodyBytes, 128)
	var decoded any
	require.NoError(t, common.UnmarshalJsonStr(string(snapshot.Body), &decoded))
}

func TestNewTaskRequestSnapshotSkipsDisabledAndNonVideoTasks(t *testing.T) {
	task := &Task{
		TaskID:   "task_snapshot_skip",
		UserId:   7,
		Platform: "doubao",
		Action:   "chat",
	}
	withTaskRequestSnapshotSettings(t, true, 256*1024, 16, 1000)
	snapshot, err := NewTaskRequestSnapshot(task, map[string]any{"prompt": "text"})
	require.NoError(t, err)
	assert.Nil(t, snapshot)

	withTaskRequestSnapshotSettings(t, false, 256*1024, 16, 1000)
	task.Action = constant.TaskActionTextToVideo
	snapshot, err = NewTaskRequestSnapshot(task, map[string]any{"prompt": "video"})
	require.NoError(t, err)
	assert.Nil(t, snapshot)
}

func TestTaskRequestSnapshotInsertAndLoad(t *testing.T) {
	previousDB := DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	DB = db
	t.Cleanup(func() { DB = previousDB })
	require.NoError(t, db.AutoMigrate(&Task{}, &TaskRequestSnapshot{}))
	withTaskRequestSnapshotSettings(t, true, 256*1024, 16, 1000)

	task := &Task{
		TaskID:     "task_snapshot_persist",
		UserId:     11,
		Platform:   "doubao",
		Action:     constant.TaskActionTextToVideo,
		Properties: Properties{OriginModelName: "video-model"},
	}
	task.RequestSnapshot, err = NewTaskRequestSnapshot(task, map[string]any{"prompt": "hello"})
	require.NoError(t, err)
	require.NotNil(t, task.RequestSnapshot)
	require.NoError(t, task.InsertWithContext(t.Context()))
	require.NoError(t, SaveTaskRequestSnapshot(t.Context(), task.ID, task.RequestSnapshot))
	require.NotZero(t, task.ID)

	stored, err := GetTaskRequestSnapshot(t.Context(), task.ID)
	require.NoError(t, err)
	assert.Equal(t, task.TaskID, stored.PublicTaskID)
	assert.Equal(t, task.ID, stored.TaskID)
	assert.JSONEq(t, `{"prompt":"hello"}`, string(stored.Body))
}

func TestSaveTaskRequestSnapshotIgnoresCallerCancellation(t *testing.T) {
	previousDB := DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&TaskRequestSnapshot{}))

	snapshot := &TaskRequestSnapshot{
		PublicTaskID: "task_snapshot_cancelled_client",
		UserID:       12,
		Platform:     "doubao",
		Model:        "video-model",
		Body:         LongText(`{"prompt":"hello"}`),
		BodyBytes:    len(`{"prompt":"hello"}`),
		CreatedAt:    common.GetTimestamp(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, SaveTaskRequestSnapshot(ctx, 99, snapshot))

	var stored TaskRequestSnapshot
	require.NoError(t, db.Where("task_id = ?", 99).First(&stored).Error)
	assert.Equal(t, snapshot.PublicTaskID, stored.PublicTaskID)
}

func TestTaskRequestSnapshotDatabaseMatrix(t *testing.T) {
	engines := []struct {
		name string
		dsn  string
		open func(string) gorm.Dialector
	}{
		{name: "mysql", dsn: os.Getenv("TEST_MYSQL_DSN"), open: mysql.Open},
		{name: "postgres", dsn: os.Getenv("TEST_POSTGRES_DSN"), open: postgres.Open},
	}
	previousDB := DB
	t.Cleanup(func() { DB = previousDB })
	for _, engine := range engines {
		engine := engine
		t.Run(engine.name, func(t *testing.T) {
			if engine.dsn == "" {
				t.Skip("database DSN is not configured")
			}
			db, err := gorm.Open(engine.open(engine.dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			DB = db
			require.NoError(t, db.AutoMigrate(&Task{}, &TaskRequestSnapshot{}))
			require.NoError(t, db.AutoMigrate(&Task{}, &TaskRequestSnapshot{}))
			withTaskRequestSnapshotSettings(t, true, 256*1024, 16, 1000)

			task := &Task{
				TaskID:   "task_snapshot_matrix_" + engine.name,
				UserId:   991,
				Platform: "doubao",
				Action:   constant.TaskActionTextToVideo,
			}
			task.RequestSnapshot, err = NewTaskRequestSnapshot(task, map[string]any{"prompt": "matrix"})
			require.NoError(t, err)
			require.NoError(t, task.InsertWithContext(t.Context()))
			require.NoError(t, SaveTaskRequestSnapshot(t.Context(), task.ID, task.RequestSnapshot))
			stored, err := GetTaskRequestSnapshot(t.Context(), task.ID)
			require.NoError(t, err)
			assert.Equal(t, task.TaskID, stored.PublicTaskID)
			assert.JSONEq(t, `{"prompt":"matrix"}`, string(stored.Body))
			require.NoError(t, db.Where("task_id = ?", task.ID).Delete(&TaskRequestSnapshot{}).Error)
			require.NoError(t, db.Delete(task).Error)
			sqlDB, closeErr := db.DB()
			if closeErr == nil {
				_ = sqlDB.Close()
			}
		})
	}
}

func TestTaskRequestSnapshotMockLoad(t *testing.T) {
	if os.Getenv("TASK_REQUEST_SNAPSHOT_LOAD_TEST") != "1" {
		t.Skip("set TASK_REQUEST_SNAPSHOT_LOAD_TEST=1 to run the 200-user mock load test")
	}
	withTaskRequestSnapshotSettings(t, true, 256*1024, 16, 1000)
	const users = 200
	const workers = 64
	body := map[string]any{
		"model":  "doubao-seedance-2-0-fast-260128",
		"prompt": "mock video request",
		"content": []any{map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:image/jpeg;base64," + strings.Repeat("A", 32*1024),
			},
		}},
	}
	started := time.Now()
	jobs := make(chan int)
	var succeeded atomic.Int64
	var failed atomic.Int64
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for userID := range jobs {
				task := &Task{
					TaskID:   fmt.Sprintf("task_load_%d", userID),
					UserId:   userID + 1,
					Platform: "doubao",
					Action:   constant.TaskActionImageToVideo,
				}
				if snapshot, err := NewTaskRequestSnapshot(task, body); err != nil || snapshot == nil {
					failed.Add(1)
				} else {
					succeeded.Add(1)
				}
			}
		})
	}
	for userID := range users {
		jobs <- userID
	}
	close(jobs)
	group.Wait()
	elapsed := time.Since(started)
	t.Logf("mock load users=%d workers=%d succeeded=%d failed=%d elapsed=%s throughput=%.2f/s",
		users, workers, succeeded.Load(), failed.Load(), elapsed, float64(succeeded.Load())/elapsed.Seconds())
	assert.Equal(t, int64(users), succeeded.Load())
	assert.Zero(t, failed.Load())
	runTaskRequestSnapshotPersistenceLoad(t)
}

func runTaskRequestSnapshotPersistenceLoad(t *testing.T) {
	t.Helper()
	previousWriteTimeout := constant.TaskRequestSnapshotWriteTimeoutMilliseconds
	constant.TaskRequestSnapshotWriteTimeoutMilliseconds = 5000
	t.Cleanup(func() { constant.TaskRequestSnapshotWriteTimeoutMilliseconds = previousWriteTimeout })
	previousDB := DB
	databasePath := filepath.Join(t.TempDir(), "task-request-snapshot-load.db")
	db, err := gorm.Open(sqlite.Open(databasePath+"?_pragma=busy_timeout(10000)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// SQLite 写入是串行的；单连接池让 -race 下的持久化夹具保持确定性，MySQL/PostgreSQL 由上面的矩阵覆盖。
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		_ = sqlDB.Close()
	})
	require.NoError(t, db.AutoMigrate(&Task{}, &TaskRequestSnapshot{}))

	const users = 200
	const workers = 64
	body := map[string]any{
		"model":  "doubao-seedance-2-0-fast-260128",
		"prompt": "mock persisted video request",
		"content": []any{map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:image/jpeg;base64," + strings.Repeat("A", 16*1024),
			},
		}},
	}
	jobs := make(chan int)
	var succeeded atomic.Int64
	var failed atomic.Int64
	errors := make(chan error, 1)
	var group sync.WaitGroup
	started := time.Now()
	for range workers {
		group.Go(func() {
			for userID := range jobs {
				task := &Task{
					TaskID:   fmt.Sprintf("task_persist_load_%d", userID),
					UserId:   userID + 1,
					Platform: "doubao",
					Action:   constant.TaskActionImageToVideo,
				}
				snapshot, buildErr := NewTaskRequestSnapshot(task, body)
				if buildErr != nil || snapshot == nil {
					select {
					case errors <- buildErr:
					default:
					}
					failed.Add(1)
					continue
				}
				task.RequestSnapshot = snapshot
				if insertErr := task.InsertWithContext(t.Context()); insertErr != nil {
					select {
					case errors <- insertErr:
					default:
					}
					failed.Add(1)
					continue
				}
				if saveErr := SaveTaskRequestSnapshot(t.Context(), task.ID, task.RequestSnapshot); saveErr != nil {
					select {
					case errors <- saveErr:
					default:
					}
					failed.Add(1)
					continue
				}
				succeeded.Add(1)
			}
		})
	}
	for userID := range users {
		jobs <- userID
	}
	close(jobs)
	group.Wait()
	select {
	case firstErr := <-errors:
		t.Logf("first persistence load error: %v", firstErr)
	default:
	}
	var count int64
	require.NoError(t, db.Model(&TaskRequestSnapshot{}).Count(&count).Error)
	elapsed := time.Since(started)
	t.Logf("mock persistence load users=%d workers=%d succeeded=%d failed=%d snapshots=%d elapsed=%s throughput=%.2f/s",
		users, workers, succeeded.Load(), failed.Load(), count, elapsed, float64(succeeded.Load())/elapsed.Seconds())
	assert.Equal(t, int64(users), succeeded.Load())
	assert.Zero(t, failed.Load())
	assert.Equal(t, int64(users), count)
}

func BenchmarkNewTaskRequestSnapshot(b *testing.B) {
	constant.TaskRequestSnapshotEnabled = true
	constant.TaskRequestSnapshotMaxBytes = 256 * 1024
	constant.TaskRequestSnapshotMaxDepth = 16
	constant.TaskRequestSnapshotMaxItems = 1000
	task := &Task{TaskID: "task_benchmark", UserId: 1, Platform: "doubao", Action: constant.TaskActionImageToVideo}
	body := map[string]any{
		"model":  "doubao-seedance-2-0-fast-260128",
		"prompt": "benchmark video request",
		"content": []any{map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/jpeg;base64," + strings.Repeat("A", 8*1024)},
		}},
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = NewTaskRequestSnapshot(task, body)
		}
	})
}

func BenchmarkNewTaskRequestSnapshotDisabled(b *testing.B) {
	constant.TaskRequestSnapshotEnabled = false
	task := &Task{TaskID: "task_benchmark_disabled", UserId: 1, Platform: "doubao", Action: constant.TaskActionImageToVideo}
	body := map[string]any{
		"model":  "doubao-seedance-2-0-fast-260128",
		"prompt": "benchmark video request",
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = NewTaskRequestSnapshot(task, body)
		}
	})
}
