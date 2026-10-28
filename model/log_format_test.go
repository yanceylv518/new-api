package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/requesttracetest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/logger"
)

func TestRequestTraceDatabaseMatrix(t *testing.T) {
	cases := []struct{ name, dsn string }{{"sqlite", "local"}}
	if os.Getenv("REQUEST_TRACE_TEST_DSN") != "" {
		cases = []struct{ name, dsn string }{{"mysql", requesttracetest.MySQLDSN(t)}}
	} else if os.Getenv("REQUEST_TRACE_EXTERNAL_TESTS") == "1" {
		cases = append(cases, struct{ name, dsn string }{"mysql", "root@tcp(127.0.0.1:13327)/request_trace_review?charset=utf8mb4&parseTime=true&timeout=5s"}, struct{ name, dsn string }{"postgres", "postgres://postgres@127.0.0.1:15428/request_trace_review?sslmode=disable&connect_timeout=5"}, struct{ name, dsn string }{"clickhouse", "clickhouse://default@127.0.0.1:19000/request_trace_review?dial_timeout=5s"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			previousDB, previousLog := DB, LOG_DB
			previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
			previousPath := common.SQLitePath
			common.SQLitePath = filepath.Join(t.TempDir(), "matrix.sqlite")
			t.Setenv("REQUEST_TRACE_MATRIX_DSN", tc.dsn)
			db, dbType, err := chooseDB("REQUEST_TRACE_MATRIX_DSN", true)
			require.NoError(t, err)
			db.Logger = logger.Default.LogMode(logger.Silent)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			DB, LOG_DB = db, db
			common.SetDatabaseTypes(dbType, dbType)
			initCol()
			require.False(t, db.Migrator().HasTable(&RequestTraceMetadata{}), "isolated target must be empty")
			t.Cleanup(func() {
				if dbType != common.DatabaseTypeClickHouse {
					assert.NoError(t, db.Migrator().DropTable(&RequestTraceMetadata{}, &Log{}, &AuditLog{}))
				} else {
					for _, table := range []string{"request_trace_metadata", "logs", "audit_logs"} {
						assert.NoError(t, db.Exec("DROP TABLE IF EXISTS "+table).Error)
					}
				}
				assert.NoError(t, sqlDB.Close())
				DB, LOG_DB = previousDB, previousLog
				common.SetDatabaseTypes(previousMainType, previousLogType)
				common.SQLitePath = previousPath
				initCol()
			})
			var version string
			query := "SELECT version()"
			if dbType == common.DatabaseTypeSQLite {
				query = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(query).Scan(&version).Error)
			t.Logf("engine=%s version=%s", tc.name, version)
			// 先安装新表两次，再验证保留旧 logs.other 的升级路径。
			for range 2 {
				require.NoError(t, migrateRequestTraces())
			}
			if dbType == common.DatabaseTypeMySQL {
				metadata := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 201}, AdminInfo: &common.RequestTraceAdmin{Client: common.RequestTraceClient{UserAgent: strings.Repeat("migration-metadata", 64)}}})
				require.NoError(t, RecordRequestTraces(t.Context(), []RequestTraceMetadata{{RequestId: "rename-preserved", UserId: 11, CreatedAt: 100, Data: metadata}}))
				var before RequestTraceMetadata
				require.NoError(t, db.Where("request_id = ?", "rename-preserved").First(&before).Error)
				require.NoError(t, db.Migrator().RenameTable(&RequestTraceMetadata{}, "request_traces"))
				for range 2 {
					require.NoError(t, migrateRequestTraces())
				}
				var after RequestTraceMetadata
				require.NoError(t, db.Where("request_id = ?", "rename-preserved").First(&after).Error)
				assert.Equal(t, before, after)
				assert.False(t, db.Migrator().HasTable("request_traces"))
				require.NoError(t, db.Exec("CREATE TABLE request_traces LIKE request_trace_metadata").Error)
				assert.Error(t, migrateRequestTraces())
				require.NoError(t, db.Where("request_id = ?", "rename-preserved").First(&after).Error)
				assert.Equal(t, before, after)
				require.NoError(t, db.Migrator().DropTable("request_traces"))
			}
			if dbType != common.DatabaseTypeClickHouse {
				require.True(t, db.Migrator().HasIndex(&RequestTraceMetadata{}, "idx_request_trace_attempt"))
				require.True(t, db.Migrator().HasIndex(&RequestTraceMetadata{}, "idx_request_traces_created_at"))
				require.NoError(t, db.Migrator().DropTable(&RequestTraceMetadata{}))
				require.NoError(t, db.AutoMigrate(&Log{}))
			} else {
				require.NoError(t, db.Exec("DROP TABLE request_trace_metadata").Error)
				require.NoError(t, migrateClickHouseLogDB())
			}
			legacyOther := `{"model_price":0.01,"admin_info":{"use_channel":["7"]}}`
			require.NoError(t, createLog(&Log{UserId: 11, Type: LogTypeConsume, CreatedAt: 100, RequestId: "matrix-legacy", Other: legacyOther}))
			for range 2 {
				require.NoError(t, migrateLOGDB())
			}
			var legacy Log
			require.NoError(t, db.Where("request_id = ?", "matrix-legacy").First(&legacy).Error)
			assert.Equal(t, legacyOther, legacy.Other)
			data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}, AdminInfo: &common.RequestTraceAdmin{ChannelID: 7}})
			rows := []RequestTraceMetadata{{RequestId: "matrix-owned", Attempt: 0, UserId: 11, CreatedAt: 100, Data: data}, {RequestId: "matrix-owned", Attempt: 1, UserId: 11, CreatedAt: 200, Data: data}}
			for range 2 {
				require.NoError(t, RecordRequestTraces(t.Context(), rows))
			}
			admin, err := GetRequestTraces(t.Context(), "matrix-owned", 0, true)
			require.NoError(t, err)
			require.Len(t, admin, 2)
			assert.Equal(t, 0, admin[0].Attempt)
			assert.NotNil(t, admin[0].AdminInfo)
			user, err := GetRequestTraces(t.Context(), "matrix-owned", 11, false)
			require.NoError(t, err)
			require.Len(t, user, 1)
			assert.Equal(t, 1, user[0].Attempt)
			assert.Nil(t, user[0].AdminInfo)
			other, err := GetRequestTraces(t.Context(), "matrix-owned", 12, false)
			require.NoError(t, err)
			assert.Empty(t, other)
			single := []RequestTraceMetadata{{RequestId: "matrix-single", Attempt: 0, UserId: 11, CreatedAt: 200, Data: data}}
			for range 2 {
				require.NoError(t, RecordRequestTraces(t.Context(), single))
			}
			unique, err := GetRequestTraces(t.Context(), "matrix-single", 11, false)
			require.NoError(t, err)
			require.Len(t, unique, 1)
			assert.Equal(t, 200, int(unique[0].CreatedAt))
			for index := range 37 {
				require.NoError(t, RecordRequestTraces(t.Context(), []RequestTraceMetadata{{RequestId: fmt.Sprintf("matrix-old-%d", index), Attempt: 0, UserId: 11, CreatedAt: 100, Data: data}}))
			}
			var deleted int64
			for {
				n, err := DeleteOldRequestTraces(t.Context(), 150, 7)
				require.NoError(t, err)
				deleted += n
				if n == 0 {
					break
				}
			}
			assert.EqualValues(t, 38, deleted)
			admin, err = GetRequestTraces(t.Context(), "matrix-owned", 0, true)
			require.NoError(t, err)
			require.Len(t, admin, 1)
			assert.Equal(t, 1, admin[0].Attempt)
			if dbType == common.DatabaseTypeClickHouse {
				t.Setenv("LOG_SQL_CLICKHOUSE_TTL_DAYS", "7")
				require.NoError(t, migrateRequestTraces())
				var definition string
				require.NoError(t, db.Raw("SHOW CREATE TABLE request_trace_metadata").Scan(&definition).Error)
				assert.Contains(t, definition, "TTL")
				t.Setenv("LOG_SQL_CLICKHOUSE_TTL_DAYS", "0")
				require.NoError(t, migrateRequestTraces())
				require.NoError(t, db.Raw("SHOW CREATE TABLE request_trace_metadata").Scan(&definition).Error)
				assert.NotContains(t, definition, "TTL ")
			}
		})
	}
}

// TestFormatUserLogsStripsQuotaSaturation verifies the admin-only quota
// saturation marker (nested under other.admin_info) is removed for non-admin
// log views, since formatUserLogs strips the whole admin_info object.
func TestFormatUserLogsStripsQuotaSaturation(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"model_price": 0.004,
		"admin_info": map[string]interface{}{
			"quota_saturation": map[string]interface{}{
				"op":      "QuotaFromDecimal",
				"kind":    "overflow",
				"clamped": common.MaxQuota,
			},
		},
	})
	logs := []*Log{{Other: other}}

	formatUserLogs(logs, 0)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	_, hasAdminInfo := parsed["admin_info"]
	require.False(t, hasAdminInfo, "admin_info (and nested quota_saturation) must be stripped for non-admin views")
	// Non-admin billing fields remain visible.
	require.Contains(t, parsed, "model_price")
}

func TestTaskPluginLogVisibilityIsRoleSeparated(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"model_price": 1.25,
		"admin_info": map[string]interface{}{
			"task_plugin": map[string]interface{}{
				"key":     "document-parser",
				"name":    "Document Parser",
				"version": "1.2.3",
			},
		},
		"root_info": map[string]interface{}{
			"upstream_task_id": "upstream-private",
			"task_plugin": map[string]interface{}{
				"generation": 42,
			},
		},
	})

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		formatUserLogs(logs, 0)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.NotContains(t, parsed, "admin_info")
		assert.NotContains(t, parsed, "root_info")
		assert.Equal(t, 1.25, parsed["model_price"])
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		FormatAdminLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Contains(t, parsed, "admin_info")
		assert.NotContains(t, parsed, "root_info")
	})

	t.Run("root", func(t *testing.T) {
		logs := []*Log{{Other: other}}
		FormatRootLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Contains(t, parsed, "admin_info")
		assert.Contains(t, parsed, "root_info")
	})
}

func TestLegacyLogOtherVisibilityIsRoleSeparated(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"request_path":  "/v1/chat/completions",
		"channel_id":    202,
		"channel_name":  "legacy-secret-channel",
		"channel_type":  1,
		"reject_reason": "legacy-policy-rejection",
		"admin_info": map[string]interface{}{
			"existing_admin_field": "preserved",
		},
		"root_info": map[string]interface{}{
			"upstream_request_id": "upstream-private",
		},
		"audit_info": map[string]interface{}{
			"method": "POST",
		},
	})

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{
			Id:          99,
			ChannelId:   77,
			ChannelName: "resolved-secret-channel",
			Other:       other,
		}}

		formatUserLogs(logs, 10)

		assert.Equal(t, 11, logs[0].Id)
		assert.Equal(t, 77, logs[0].ChannelId)
		assert.Empty(t, logs[0].ChannelName)
		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Equal(t, "/v1/chat/completions", parsed["request_path"])
		for _, key := range []string{
			"channel_id",
			"channel_name",
			"channel_type",
			"reject_reason",
			"admin_info",
			"root_info",
			"audit_info",
		} {
			assert.NotContains(t, parsed, key)
		}
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatAdminLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Equal(t, "legacy-secret-channel", parsed["channel_name"])
		assert.NotContains(t, parsed, "reject_reason")
		assert.NotContains(t, parsed, "root_info")
		assert.Contains(t, parsed, "audit_info")
		adminInfo, ok := parsed["admin_info"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "preserved", adminInfo["existing_admin_field"])
		assert.Equal(t, "legacy-policy-rejection", adminInfo["reject_reason"])
	})

	t.Run("root", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatRootLogs(logs)

		parsed, err := common.StrToMap(logs[0].Other)
		require.NoError(t, err)
		assert.Equal(t, "legacy-secret-channel", parsed["channel_name"])
		assert.NotContains(t, parsed, "reject_reason")
		assert.Contains(t, parsed, "root_info")
		assert.Contains(t, parsed, "audit_info")
		adminInfo, ok := parsed["admin_info"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, "preserved", adminInfo["existing_admin_field"])
		assert.Equal(t, "legacy-policy-rejection", adminInfo["reject_reason"])
	})
}

func TestLegacyRejectReasonDoesNotOverrideScopedValue(t *testing.T) {
	other := common.MapToJsonStr(map[string]interface{}{
		"reject_reason": "legacy-value",
		"admin_info": map[string]interface{}{
			"reject_reason": "scoped-value",
		},
	})
	logs := []*Log{{Other: other}}

	FormatRootLogs(logs)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	assert.NotContains(t, parsed, "reject_reason")
	adminInfo, ok := parsed["admin_info"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "scoped-value", adminInfo["reject_reason"])
}

func TestLegacyRejectReasonHandlesNullAdminInfo(t *testing.T) {
	logs := []*Log{{Other: `{"reject_reason":"legacy-value","admin_info":null}`}}

	FormatAdminLogs(logs)

	parsed, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	assert.NotContains(t, parsed, "reject_reason")
	adminInfo, ok := parsed["admin_info"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "legacy-value", adminInfo["reject_reason"])
}

func TestLogFormattingPreservesLargeIntegerLexemes(t *testing.T) {
	const other = `{"public_id":9007199254740993,"admin_info":{"admin_id":9007199254740995},"root_info":{"generation":18446744073709551615}}`

	t.Run("user", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		formatUserLogs(logs, 0)

		assert.Contains(t, logs[0].Other, `"public_id":9007199254740993`)
		assert.NotContains(t, logs[0].Other, "admin_id")
		assert.NotContains(t, logs[0].Other, "generation")
	})

	t.Run("admin", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatAdminLogs(logs)

		assert.Contains(t, logs[0].Other, `"public_id":9007199254740993`)
		assert.Contains(t, logs[0].Other, `"admin_id":9007199254740995`)
		assert.NotContains(t, logs[0].Other, "generation")
	})

	t.Run("root", func(t *testing.T) {
		logs := []*Log{{Other: other}}

		FormatRootLogs(logs)

		assert.Equal(t, other, logs[0].Other)
	})

	t.Run("unprivileged", func(t *testing.T) {
		const unprivileged = `{"public_id":9007199254740993,"model_price":0.004}`

		userLogs := []*Log{{Other: unprivileged}}
		formatUserLogs(userLogs, 0)
		assert.Equal(t, unprivileged, userLogs[0].Other)

		adminLogs := []*Log{{Other: unprivileged}}
		FormatAdminLogs(adminLogs)
		assert.Equal(t, unprivileged, adminLogs[0].Other)
	})
}
