package model

import (
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 在真实方言上验证最终回退结果、筛选和分页；外部连接只允许本项目专用测试端口。
func TestUserLogFinalOutcomeDatabaseMatrix(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			if engine != "sqlite" && os.Getenv("PRICING_EXTERNAL_TESTS") != "1" {
				t.Skip("requires isolated pricing test containers")
			}
			var dialect gorm.Dialector = sqlite.Open(":memory:")
			logType := common.DatabaseTypeSQLite
			if engine == "mysql" {
				dialect = mysql.Open("root@tcp(127.0.0.1:13326)/pricing_review?charset=utf8mb4&parseTime=true&timeout=5s")
				logType = common.DatabaseTypeMySQL
			}
			if engine == "postgres" {
				dialect = postgres.Open("postgres://postgres@127.0.0.1:15427/pricing_review?sslmode=disable&connect_timeout=5")
				logType = common.DatabaseTypePostgreSQL
			}
			db, err := gorm.Open(dialect, &gorm.Config{})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, connection.Close()) })
			connection.SetMaxOpenConns(1)
			// 拒绝接管已有日志表，避免清理其他测试的数据。
			require.False(t, db.Migrator().HasTable(&Log{}))
			require.NoError(t, db.AutoMigrate(&Log{}))
			t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(&Log{})) })
			previousDB, previousType := LOG_DB, common.LogDatabaseType()
			LOG_DB = db
			common.SetLogDatabaseType(logType)
			initCol()
			t.Cleanup(func() { LOG_DB = previousDB; common.SetLogDatabaseType(previousType); initCol() })
			query := "SELECT VERSION()"
			if engine == "sqlite" {
				query = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("%s %s", engine, version)
			logs := []Log{
				{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "success", ModelName: "before", Content: "hidden retry"},
				{UserId: 4, CreatedAt: 100, Type: LogTypeConsume, RequestId: "success", ModelName: "final", Content: "final success"},
				{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "failure", Content: "hidden failure"},
				{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "failure", Content: "final failure"},
				{UserId: 4, CreatedAt: 100, Type: LogTypeError, Content: "legacy one"},
				{UserId: 4, CreatedAt: 100, Type: LogTypeError, Content: "legacy two"},
				{UserId: 5, CreatedAt: 100, Type: LogTypeError, RequestId: "failure", Content: "other user"},
				{UserId: 4, CreatedAt: 100, Type: LogTypeError, Content: "legacy null"},
			}
			require.NoError(t, db.Create(&logs).Error)
			require.NoError(t, db.Model(&logs[7]).UpdateColumn("request_id", nil).Error)
			var contents []string
			for _, offset := range []int{0, 2, 4} {
				page, total, err := GetUserLogs(4, LogTypeUnknown, 0, 0, "", "", offset, 2, "", "", "")
				require.NoError(t, err)
				assert.EqualValues(t, 5, total)
				for _, row := range page {
					contents = append(contents, row.Content)
				}
			}
			assert.Equal(t, []string{"legacy null", "legacy two", "legacy one", "final failure", "final success"}, contents)
			failures, total, err := GetUserLogs(4, LogTypeError, 0, 0, "", "", 0, 20, "", "success", "")
			require.NoError(t, err)
			assert.Zero(t, total)
			assert.Empty(t, failures)
			_, total, err = GetUserLogs(4, LogTypeUnknown, 0, 0, "before", "", 0, 20, "", "", "")
			require.NoError(t, err)
			assert.Zero(t, total, "模型筛选不得复活中间失败")
			admin, total, err := GetAllLogs(LogTypeUnknown, 0, 0, "", "", "", 0, 20, 0, "", "", "")
			require.NoError(t, err)
			assert.EqualValues(t, 8, total)
			assert.Len(t, admin, 8)
		})
	}
}
