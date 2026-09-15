package model

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// 插件任务列表必须同时隔离用户、插件平台、模型和状态，并保持分页总数一致。
func TestTaskPluginUserTaskListFiltersAndCounts(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Create([]*Task{
		{TaskID: "h3-success", UserId: 7, Platform: "hailuo", Action: "text_to_video", Status: TaskStatusSuccess, Properties: Properties{OriginModelName: "MiniMax-H3"}},
		{TaskID: "h3-queued", UserId: 7, Platform: "35", Action: "image_to_video", Status: TaskStatusQueued, Properties: Properties{OriginModelName: "MiniMax-H3"}},
		{TaskID: "h3-regeneration", UserId: 7, Platform: "hailuo", Action: "regeneration", Status: TaskStatusSuccess, Properties: Properties{OriginModelName: "MiniMax-H3"}},
		{TaskID: "other-model", UserId: 7, Platform: "hailuo", Action: "text_to_video", Status: TaskStatusSuccess, Properties: Properties{OriginModelName: "MiniMax-Hailuo-2.3"}},
		{TaskID: "other-user", UserId: 8, Platform: "hailuo", Action: "text_to_video", Status: TaskStatusSuccess, Properties: Properties{OriginModelName: "MiniMax-H3"}},
		{TaskID: "other-plugin", UserId: 7, Platform: "54", Action: "text_to_video", Status: TaskStatusSuccess, Properties: Properties{OriginModelName: "MiniMax-H3"}},
	}).Error)

	filter := TaskPluginTaskFilter{
		Platforms: []constant.TaskPlatform{"hailuo", "35"},
		Model:     "MiniMax-H3",
		Statuses:  []TaskStatus{TaskStatusSuccess},
		Actions:   []string{"text_to_video", "image_to_video"},
	}
	total, err := CountTaskPluginUserTasks(7, filter)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	tasks, err := ListTaskPluginUserTasks(7, filter, 0, 10)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "h3-success", tasks[0].TaskID)

	filter.Statuses = []TaskStatus{TaskStatusQueued}
	filter.Actions = nil
	tasks, err = ListTaskPluginUserTasks(7, filter, 0, 10)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "h3-queued", tasks[0].TaskID)
}

// 使用隔离表验证三种实际数据库的 JSON 服务等级、别名、时间边界和分页总数。
func TestTaskPluginListDatabaseDialects(t *testing.T) {
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
			db, err := gorm.Open(dialect, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "doubao_native_filter_test_"}})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(1)
			versionSQL := "SELECT version()"
			if engine == "sqlite" {
				versionSQL = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
			t.Logf("Database version: %s", version)
			previousDB := DB
			DB = db
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(&Task{}))
				DB = previousDB
				assert.NoError(t, connection.Close())
			})
			require.NoError(t, db.AutoMigrate(&Task{}))
			const now int64 = 1700000000
			fixtures := []struct {
				id, data, platform string
				userID             int
				created            int64
			}{
				{"default", `{"service_tier":"default"}`, "doubao", 7, now - 10},
				{"flex", "{\n  \"service_tier\": \"flex\"\n}", "54", 7, now - 10},
				{"missing", `{"id":"upstream"}`, "45", 7, now - 10},
				{"null", `{"service_tier":null}`, "doubao", 7, now - 10},
				{"empty", "", "doubao", 7, now - 10},
				{"boundary", `{}`, "doubao", 7, now - 604800},
				{"old", `{}`, "doubao", 7, now - 604801},
				{"future", `{}`, "doubao", 7, now},
				{"other-user", `{}`, "doubao", 8, now - 10},
				{"other-plugin", `{}`, "hailuo", 7, now - 10},
			}
			for _, row := range fixtures {
				task := &Task{TaskID: row.id, UserId: row.userID, Platform: constant.TaskPlatform(row.platform), CreatedAt: row.created,
					Properties: Properties{OriginModelName: "doubao-seedance-2-0-260128", UpstreamModelName: "ep-test"}}
				if row.data != "" {
					task.Data = json.RawMessage(row.data)
				}
				require.NoError(t, db.Create(task).Error)
			}
			filter := TaskPluginTaskFilter{Platforms: []constant.TaskPlatform{"doubao", "54", "45"}, ServiceTier: "default", CreatedAfter: now - 604800, CreatedBefore: now}
			total, err := CountTaskPluginUserTasks(7, filter)
			require.NoError(t, err)
			assert.EqualValues(t, 5, total)
			page, err := ListTaskPluginUserTasks(7, filter, 0, 2)
			require.NoError(t, err)
			require.Len(t, page, 2)
			assert.Equal(t, "boundary", page[0].TaskID)
			assert.Equal(t, "empty", page[1].TaskID)
			filter.ServiceTier, filter.Model = "flex", "ep-test"
			total, err = CountTaskPluginUserTasks(7, filter)
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			page, err = ListTaskPluginUserTasks(7, filter, 0, 500)
			require.NoError(t, err)
			require.Len(t, page, 1)
			assert.Equal(t, "flex", page[0].TaskID)
			// 过期和失败在本地共用 FAILURE，原生失败筛选仍需保留官方状态差异。
			require.NoError(t, db.Create(&Task{TaskID: "expired", UserId: 7, Platform: "doubao", CreatedAt: now - 10, Status: TaskStatusFailure, Data: json.RawMessage(`{"status":"expired"}`)}).Error)
			filter.Model, filter.ServiceTier, filter.ExcludeExpired = "", "default", true
			total, err = CountTaskPluginUserTasks(7, filter)
			require.NoError(t, err)
			assert.EqualValues(t, 5, total)
		})
	}
}

// LIKE 条件必须转义通配符，避免声明模型名中的字符改变过滤范围。
func TestTaskPluginUserTaskListEscapesModelPattern(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Create(&Task{
		TaskID: "literal-model", UserId: 7, Platform: "hailuo", Action: "text_to_video", Status: TaskStatusSuccess,
		Properties: Properties{OriginModelName: "model%_literal"},
	}).Error)
	filter := TaskPluginTaskFilter{Platforms: []constant.TaskPlatform{"hailuo"}, Model: "model%_literal"}
	tasks, err := ListTaskPluginUserTasks(7, filter, 0, 10)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "literal-model", tasks[0].TaskID)
}

// 模型映射任务只有 upstream_model_name 时也必须能被插件列表检索到。
func TestTaskPluginUserTaskListMatchesUpstreamModel(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.Create(&Task{
		TaskID: "mapped-model", UserId: 7, Platform: "hailuo", Action: "text_to_video", Status: TaskStatusSuccess,
		Properties: Properties{UpstreamModelName: "MiniMax-H3"},
	}).Error)

	tasks, err := ListTaskPluginUserTasks(7, TaskPluginTaskFilter{
		Platforms: []constant.TaskPlatform{"hailuo"},
		Model:     "MiniMax-H3",
	}, 0, 10)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "mapped-model", tasks[0].TaskID)
}
