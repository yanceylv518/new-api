package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupUserLogViewTestDB 为日志查询测试创建独立的内存数据库，并恢复全局日志连接。
func setupUserLogViewTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	previousLogDB := LOG_DB
	previousLogDatabaseType := common.LogDatabaseType()
	common.SetLogDatabaseType(common.DatabaseTypeSQLite)

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Log{}))
	LOG_DB = db

	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		LOG_DB = previousLogDB
		common.SetLogDatabaseType(previousLogDatabaseType)
		_ = sqlDB.Close()
	})

	return db
}

func TestGetUserLogsKeepsFinalFallbackAndIndependentBillingEvents(t *testing.T) {
	db := setupUserLogViewTestDB(t)

	logs := []Log{
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-success", Content: "retryable error"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeConsume, RequestId: "eventual-success", Content: "final success"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-failure", Content: "first failure"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-failure", Content: "final failure"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "", Content: "legacy empty request one"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "", Content: "legacy empty request two"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeRefund, RequestId: "eventual-success", Content: "refund"},
		// 相同请求号属于其他用户时，不得影响当前用户的最终结果。
		{UserId: 5, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-failure", Content: "other user"},
	}
	require.NoError(t, db.Create(&logs).Error)

	result, total, err := GetUserLogs(4, LogTypeUnknown, 0, 0, "", "", 0, 20, "", "", "")
	require.NoError(t, err)
	assert.EqualValues(t, 5, total)
	require.Len(t, result, 5)
	assert.Equal(t, []string{
		"refund",
		"legacy empty request two",
		"legacy empty request one",
		"final failure",
		"final success",
	}, []string{result[0].Content, result[1].Content, result[2].Content, result[3].Content, result[4].Content})
}

func TestGetUserLogsAppliesTypeFilterToFinalOutcome(t *testing.T) {
	db := setupUserLogViewTestDB(t)

	logs := []Log{
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "success", Content: "hidden error"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeConsume, RequestId: "success", Content: "success"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "failure", Content: "visible failure"},
	}
	require.NoError(t, db.Create(&logs).Error)

	result, total, err := GetUserLogs(4, LogTypeError, 0, 0, "", "", 0, 20, "", "", "")
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, result, 1)
	assert.Equal(t, "visible failure", result[0].Content)
}

func TestSelectUserLogColumnsAddsSafeMySQLIndexHint(t *testing.T) {
	db := setupUserLogViewTestDB(t)
	common.SetLogDatabaseType(common.DatabaseTypeMySQL)

	var logs []*Log
	query := selectUserLogColumns(db.Session(&gorm.Session{DryRun: true}).Table("logs"))
	statement := query.Find(&logs).Statement

	assert.Contains(t, statement.SQL.String(), "SELECT /*+ INDEX(logs idx_user_id_id) */ logs.* FROM `logs`")
}

func TestSelectUserLogColumnsLeavesOtherDatabasesUnchanged(t *testing.T) {
	db := setupUserLogViewTestDB(t)

	var logs []*Log
	query := selectUserLogColumns(db.Session(&gorm.Session{DryRun: true}).Table("logs"))
	statement := query.Find(&logs).Statement

	assert.NotContains(t, statement.SQL.String(), "INDEX(logs idx_user_id_id)")
}
