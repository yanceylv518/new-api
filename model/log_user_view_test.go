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

func TestGetUserLogsKeepsOnlyFinalFallbackOutcomeWithCorrectPagination(t *testing.T) {
	db := setupUserLogViewTestDB(t)

	logs := []Log{
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-success", Content: "retryable error"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeConsume, RequestId: "eventual-success", Content: "final success"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-failure", Content: "first failure"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-failure", Content: "final failure"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "", Content: "legacy empty request one"},
		{UserId: 4, CreatedAt: 100, Type: LogTypeError, RequestId: "", Content: "legacy empty request two"},
		// A later row belonging to another user must not suppress user 4's result.
		{UserId: 5, CreatedAt: 100, Type: LogTypeError, RequestId: "eventual-failure", Content: "other user"},
	}
	require.NoError(t, db.Create(&logs).Error)

	firstPage, total, err := GetUserLogs(4, LogTypeUnknown, 0, 0, "", "", 0, 2, "", "", "")
	require.NoError(t, err)
	assert.EqualValues(t, 4, total)
	require.Len(t, firstPage, 2)
	assert.Equal(t, []string{"legacy empty request two", "legacy empty request one"}, []string{firstPage[0].Content, firstPage[1].Content})

	secondPage, secondTotal, err := GetUserLogs(4, LogTypeUnknown, 0, 0, "", "", 2, 2, "", "", "")
	require.NoError(t, err)
	assert.Equal(t, total, secondTotal)
	require.Len(t, secondPage, 2)
	assert.Equal(t, []string{"final failure", "final success"}, []string{secondPage[0].Content, secondPage[1].Content})
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

func TestSelectUserLogColumnsAddsSafeMySQLOptimizerHint(t *testing.T) {
	db := setupUserLogViewTestDB(t)
	common.SetLogDatabaseType(common.DatabaseTypeMySQL)

	var logs []*Log
	query := selectUserLogColumns(db.Session(&gorm.Session{DryRun: true}).Table("logs"))
	statement := query.Find(&logs).Statement

	assert.Contains(t, statement.SQL.String(), "SELECT /*+ INDEX(logs idx_logs_user_created_type) */ logs.* FROM `logs`")
}

func TestSelectUserLogColumnsLeavesOtherDatabasesUnchanged(t *testing.T) {
	db := setupUserLogViewTestDB(t)

	var logs []*Log
	query := selectUserLogColumns(db.Session(&gorm.Session{DryRun: true}).Table("logs"))
	statement := query.Find(&logs).Statement

	assert.NotContains(t, statement.SQL.String(), "INDEX(logs idx_logs_user_created_type)")
}
