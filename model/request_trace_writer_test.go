package model

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/requesttracetest"
	"github.com/go-sql-driver/mysql"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func requestTraceWriterMySQL(t *testing.T) (*gorm.DB, *requestTraceWriter) {
	t.Helper()
	dsn := requesttracetest.MySQLDSN(t)
	previousDB, previousLog := DB, LOG_DB
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousConfig := common.GetRequestTraceConfig()
	t.Setenv("REQUEST_TRACE_CASE_DSN", dsn)
	db, dbType, err := chooseDB("REQUEST_TRACE_CASE_DSN", true)
	require.NoError(t, err)
	require.Equal(t, common.DatabaseTypeMySQL, dbType)
	db.Logger = logger.Default.LogMode(logger.Silent)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(16)
	DB, LOG_DB = db, db
	common.SetDatabaseTypes(dbType, dbType)
	initCol()
	require.NoError(t, db.AutoMigrate(&RequestTraceMetadata{}, &SystemTask{}))
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all","compression_level":"fast"}`))
	t.Cleanup(func() {
		assert.NoError(t, sqlDB.Close())
		DB, LOG_DB = previousDB, previousLog
		common.SetDatabaseTypes(previousMainType, previousLogType)
		initCol()
		assert.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previousConfig)))
	})
	requestTraceWriterState.Lock()
	previousWriter := requestTraceWriterState.writer
	requestTraceWriterState.writer = nil
	requestTraceWriterState.Unlock()
	StartRequestTraceWriter()
	w := requestTraceWriterState.writer
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, StopRequestTraceWriter(ctx))
		select {
		case <-w.done:
		case <-ctx.Done():
			t.Error("trace workers did not exit before database cleanup")
		}
		requestTraceWriterState.Lock()
		requestTraceWriterState.writer = previousWriter
		requestTraceWriterState.Unlock()
	})
	return db, w
}

func traceWriterRows(prefix string, start, count int, data string) []RequestTraceMetadata {
	rows := make([]RequestTraceMetadata, count)
	for index := range rows {
		rows[index] = RequestTraceMetadata{RequestId: fmt.Sprintf("%s-%d", prefix, start+index), UserId: 11, CreatedAt: time.Now().Unix(), Data: data}
	}
	return rows
}

func blockTraceWriterCreates(t *testing.T, db *gorm.DB) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{}, 16)
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("trace-writer-test-gate", func(tx *gorm.DB) {
		if tx.Statement.Table != (RequestTraceMetadata{}).TableName() {
			return
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-released:
		case <-tx.Statement.Context.Done():
			tx.AddError(tx.Statement.Context.Err())
		}
	}))
	t.Cleanup(release)
	return entered, release
}

func awaitTraceWriterCreate(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("trace write did not reach the controlled database boundary")
	}
}

func stopTraceWriter(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, StopRequestTraceWriter(ctx))
}

func TestRequestTraceWriterMySQL(t *testing.T) {
	data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Milliseconds: 123, Status: 200}})
	t.Run("ownership authorization deduplication and graceful drain", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		entered, release := blockTraceWriterCreates(t, db)
		rows := traceWriterRows("owned", 0, 1, data)
		require.NoError(t, SubmitRequestTraces(rows))
		awaitTraceWriterCreate(t, entered)
		assert.True(t, HasPendingRequestTrace(rows[0].RequestId, 11, false))
		assert.False(t, HasPendingRequestTrace(rows[0].RequestId, 12, false))
		assert.True(t, HasPendingRequestTrace(rows[0].RequestId, 0, true))
		rows[0].Data = `{"trace":{"ms":999,"status":500}}`
		require.NoError(t, SubmitRequestTraces(traceWriterRows("owned", 0, 1, data)))
		release()
		stopTraceWriter(t)
		views, err := GetRequestTraces(t.Context(), "owned-0", 11, false)
		require.NoError(t, err)
		require.Len(t, views, 1)
		assert.EqualValues(t, 123, views[0].Trace.Milliseconds)
		assert.False(t, HasPendingRequestTrace("owned-0", 11, false))
		assert.Zero(t, GetRequestTraceWriterStats().QueuedRows)
		assert.Zero(t, GetRequestTraceWriterStats().InFlightRows)
		assert.Error(t, SubmitRequestTraces(traceWriterRows("closed", 0, 1, data)))
	})
	t.Run("row budget drops immediately while database is blocked", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		entered, release := blockTraceWriterCreates(t, db)
		for start := 0; start < requestTraceQueueRows; start += requestTraceBatchRows {
			require.NoError(t, SubmitRequestTracesWithPriority(traceWriterRows("row-budget", start, requestTraceBatchRows, data), true))
		}
		awaitTraceWriterCreate(t, entered)
		awaitTraceWriterCreate(t, entered)
		assert.Equal(t, requestTraceQueueRows, GetRequestTraceWriterStats().QueuedRows)
		require.ErrorIs(t, SubmitRequestTraces(traceWriterRows("row-overflow", 0, 1, data)), ErrRequestTraceDropped)
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().DroppedRows-before.DroppedRows)
		assert.Zero(t, GetRequestTraceWriterStats().FallbackRows)
		assert.False(t, HasPendingRequestTrace("row-overflow-0", 11, false))
		release()
		stopTraceWriter(t)
		var count int64
		require.NoError(t, db.Model(&RequestTraceMetadata{}).Count(&count).Error)
		assert.EqualValues(t, requestTraceQueueRows, count)
		after := GetRequestTraceWriterStats()
		assert.EqualValues(t, requestTraceQueueRows, after.PersistedRows-before.PersistedRows)
		assert.Equal(t, before.FailedRows, after.FailedRows)
		assert.Zero(t, after.QueuedRows)
		assert.Zero(t, after.QueuedBytes)
	})
	t.Run("byte budget drops without waiting and releases accounting", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		entered, release := blockTraceWriterCreates(t, db)
		large := `{"padding":"` + strings.Repeat("x", 512<<10) + `"}`
		accepted := requestTraceQueueBytes / len(large)
		for index := range accepted {
			require.NoError(t, SubmitRequestTracesWithPriority(traceWriterRows("byte-budget", index, 1, large), true))
		}
		awaitTraceWriterCreate(t, entered)
		awaitTraceWriterCreate(t, entered)
		assert.Equal(t, accepted*len(large), GetRequestTraceWriterStats().QueuedBytes)
		require.ErrorIs(t, SubmitRequestTraces(traceWriterRows("byte-overflow", 0, 1, large)), ErrRequestTraceDropped)
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().DroppedRows-before.DroppedRows)
		assert.Zero(t, GetRequestTraceWriterStats().FallbackRows)
		release()
		stopTraceWriter(t)
		var count int64
		require.NoError(t, db.Model(&RequestTraceMetadata{}).Count(&count).Error)
		assert.EqualValues(t, accepted, count)
		assert.Zero(t, GetRequestTraceWriterStats().QueuedBytes)
	})
	t.Run("oversized request group is dropped atomically", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		rows := traceWriterRows("large-group", 0, requestTraceBatchRows+1, data)
		require.ErrorIs(t, SubmitRequestTraces(rows), ErrRequestTraceDropped)
		var count int64
		require.NoError(t, db.Model(&RequestTraceMetadata{}).Count(&count).Error)
		assert.Zero(t, count)
		assert.EqualValues(t, len(rows), GetRequestTraceWriterStats().DroppedRows-before.DroppedRows)
	})
	t.Run("injected MySQL deadlock retries once without duplicate rows", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		var attempts atomic.Int64
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("trace-deadlock-once", func(tx *gorm.DB) {
			if tx.Statement.Table == (RequestTraceMetadata{}).TableName() && attempts.Add(1) == 1 {
				tx.AddError(&mysql.MySQLError{Number: 1213, Message: "injected trace deadlock"})
			}
		}))
		require.NoError(t, SubmitRequestTraces(traceWriterRows("retry", 0, 1, data)))
		stopTraceWriter(t)
		assert.EqualValues(t, 2, attempts.Load())
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().Retries-before.Retries)
		var count int64
		require.NoError(t, db.Model(&RequestTraceMetadata{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
	t.Run("actual MySQL permanent error is counted without retry", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		sqlDB, err := db.DB()
		require.NoError(t, err)
		_, err = sqlDB.ExecContext(t.Context(), "CREATE TRIGGER trace_reject BEFORE INSERT ON request_trace_metadata FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'intentional trace rejection'")
		require.NoError(t, err)
		require.NoError(t, SubmitRequestTraces(traceWriterRows("reject", 0, 1, data)))
		stopTraceWriter(t)
		after := GetRequestTraceWriterStats()
		assert.EqualValues(t, 1, after.FailedRows-before.FailedRows)
		assert.Equal(t, before.Retries, after.Retries)
		assert.Equal(t, before.PersistedRows, after.PersistedRows)
		assert.Zero(t, after.QueuedRows)
	})
	t.Run("actual MySQL table lock times out then recovers on retry", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		sqlDB, err := db.DB()
		require.NoError(t, err)
		connection, err := sqlDB.Conn(t.Context())
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, connection.Close()) })
		_, err = connection.ExecContext(t.Context(), "LOCK TABLES request_trace_metadata WRITE")
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = connection.ExecContext(context.Background(), "UNLOCK TABLES") })
		require.NoError(t, SubmitRequestTraces(traceWriterRows("lock-recovery", 0, 1, data)))
		require.Eventually(t, func() bool { return GetRequestTraceWriterStats().Retries > before.Retries }, 5*time.Second, 5*time.Millisecond)
		_, err = connection.ExecContext(t.Context(), "UNLOCK TABLES")
		require.NoError(t, err)
		stopTraceWriter(t)
		after := GetRequestTraceWriterStats()
		assert.EqualValues(t, 1, after.PersistedRows-before.PersistedRows)
		assert.Equal(t, before.FailedRows, after.FailedRows)
		assert.Zero(t, after.QueuedRows)
	})
	t.Run("expired shutdown cancels writers and accounts for pending rows", func(t *testing.T) {
		db, w := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		entered, _ := blockTraceWriterCreates(t, db)
		require.NoError(t, SubmitRequestTraces(traceWriterRows("cancel", 0, 20, data)))
		awaitTraceWriterCreate(t, entered)
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 1))
		defer cancel()
		assert.ErrorIs(t, StopRequestTraceWriter(ctx), context.DeadlineExceeded)
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled trace writer did not release pending work")
		}
		after := GetRequestTraceWriterStats()
		assert.EqualValues(t, 20, after.FailedRows-before.FailedRows)
		assert.Equal(t, before.PersistedRows, after.PersistedRows)
		assert.Zero(t, after.QueuedRows)
		assert.Zero(t, after.QueuedBytes)
		assert.Zero(t, after.InFlightRows)
		assert.False(t, HasPendingRequestTrace("cancel-0", 11, false))
	})
	t.Run("oversized metadata is dropped before enqueue", func(t *testing.T) {
		_, _ = requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		err := SubmitRequestTraces(traceWriterRows("invalid-size", 0, 1, strings.Repeat("x", common.TraceMetadataMaxBytes+1)))
		require.ErrorIs(t, err, ErrRequestTraceDropped)
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().DroppedRows-before.DroppedRows)
		assert.Equal(t, before.FailedRows, GetRequestTraceWriterStats().FailedRows)
		assert.False(t, HasPendingRequestTrace("invalid-size-0", 11, false))
	})
	t.Run("writer can restart after a completed drain", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		stopTraceWriter(t)
		before := GetRequestTraceWriterStats()
		require.ErrorIs(t, SubmitRequestTraces(traceWriterRows("stopped", 0, 1, data)), ErrRequestTraceDropped)
		StartRequestTraceWriter()
		require.NoError(t, SubmitRequestTraces(traceWriterRows("restarted", 0, 1, data)))
		stopTraceWriter(t)
		var count int64
		require.NoError(t, db.Model(&RequestTraceMetadata{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().DroppedRows-before.DroppedRows)
	})
}

func TestRequestTracePriorityReserveMySQL(t *testing.T) {
	for _, tc := range []struct {
		name, data     string
		regular, total int
	}{
		{name: "row reserve", data: `{"trace":{"status":200}}`, regular: requestTraceQueueRows - requestTraceReservedRows, total: requestTraceQueueRows},
		{name: "byte reserve", data: `{"padding":"` + strings.Repeat("x", 512<<10) + `"}`, regular: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := requestTraceWriterMySQL(t)
			entered, release := blockTraceWriterCreates(t, db)
			before := GetRequestTraceWriterStats()
			regular, total := tc.regular, tc.total
			if regular < 0 {
				regular = (requestTraceQueueBytes - requestTraceReservedBytes) / len(tc.data)
				total = requestTraceQueueBytes / len(tc.data)
			}
			for index := range regular {
				require.NoError(t, SubmitRequestTraces(traceWriterRows("regular", index, 1, tc.data)))
			}
			awaitTraceWriterCreate(t, entered)
			require.ErrorIs(t, SubmitRequestTraces(traceWriterRows("regular-overflow", 0, 1, tc.data)), ErrRequestTraceDropped)
			for index := range total - regular {
				require.NoError(t, SubmitRequestTracesWithPriority(traceWriterRows("priority", index, 1, tc.data), true))
			}
			require.ErrorIs(t, SubmitRequestTracesWithPriority(traceWriterRows("priority-overflow", 0, 1, tc.data), true), ErrRequestTraceDropped)
			assert.False(t, HasPendingRequestTrace("regular-overflow-0", 11, false))
			assert.False(t, HasPendingRequestTrace("priority-overflow-0", 11, false))
			release()
			stopTraceWriter(t)
			after := GetRequestTraceWriterStats()
			assert.EqualValues(t, 1, after.DroppedRegularRows-before.DroppedRegularRows)
			assert.EqualValues(t, 1, after.DroppedPriorityRows-before.DroppedPriorityRows)
			assert.EqualValues(t, 2, after.DroppedRows-before.DroppedRows)
			assert.EqualValues(t, total, after.PersistedRows-before.PersistedRows)
			assert.Zero(t, after.QueuedRows)
			assert.Zero(t, after.QueuedBytes)
			var count int64
			require.NoError(t, db.Model(&RequestTraceMetadata{}).Count(&count).Error)
			assert.EqualValues(t, total, count)
		})
	}
}

func TestRequestTraceMainAndSeparateLogMigration(t *testing.T) {
	mainDB, _ := requestTraceWriterMySQL(t)
	data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}})
	require.NoError(t, RecordRequestTraces(t.Context(), []RequestTraceMetadata{{RequestId: "main-migration", UserId: 11, CreatedAt: 100, Data: data}}))
	require.NoError(t, mainDB.Migrator().RenameTable(&RequestTraceMetadata{}, "request_traces"))
	for range 2 {
		require.NoError(t, migrateDB())
	}
	var mainRow RequestTraceMetadata
	require.NoError(t, mainDB.Where("request_id = ?", "main-migration").First(&mainRow).Error)
	assert.False(t, mainDB.Migrator().HasTable("request_traces"))
	assert.True(t, mainDB.Migrator().HasTable("request_trace_metadata"))

	logDSN := requesttracetest.MySQLDSN(t)
	t.Setenv("REQUEST_TRACE_SEPARATE_LOG_DSN", logDSN)
	logDB, dbType, err := chooseDB("REQUEST_TRACE_SEPARATE_LOG_DSN", true)
	require.NoError(t, err)
	logDB.Logger = logger.Default.LogMode(logger.Silent)
	logSQL, err := logDB.DB()
	require.NoError(t, err)
	t.Cleanup(func() { LOG_DB = mainDB; assert.NoError(t, logSQL.Close()) })
	LOG_DB = logDB
	common.SetLogDatabaseType(dbType)
	require.NoError(t, logDB.AutoMigrate(&RequestTraceMetadata{}))
	require.NoError(t, RecordRequestTraces(t.Context(), []RequestTraceMetadata{{RequestId: "separate-log-migration", UserId: 11, CreatedAt: 200, Data: data}}))
	require.NoError(t, logDB.Migrator().RenameTable(&RequestTraceMetadata{}, "request_traces"))
	for range 2 {
		require.NoError(t, migrateLOGDB())
	}
	views, err := GetRequestTraces(t.Context(), "separate-log-migration", 11, false)
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, 200, views[0].Trace.Status)
	assert.False(t, logDB.Migrator().HasTable("request_traces"))
	var unchanged RequestTraceMetadata
	require.NoError(t, mainDB.Where("request_id = ?", "main-migration").First(&unchanged).Error)
	assert.Equal(t, mainRow, unchanged)
}

func TestRequestTraceTimelineLegacyMigrationMySQL(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("separate-log=%t", separate), func(t *testing.T) {
			mainDB, _ := requestTraceWriterMySQL(t)
			db := mainDB
			if separate {
				t.Setenv("REQUEST_TRACE_TIMELINE_LOG_DSN", requesttracetest.MySQLDSN(t))
				var dbType common.DatabaseType
				var err error
				db, dbType, err = chooseDB("REQUEST_TRACE_TIMELINE_LOG_DSN", true)
				require.NoError(t, err)
				db.Logger = logger.Default.LogMode(logger.Silent)
				sqlDB, err := db.DB()
				require.NoError(t, err)
				t.Cleanup(func() { LOG_DB = mainDB; assert.NoError(t, sqlDB.Close()) })
				LOG_DB = db
				common.SetLogDatabaseType(dbType)
				require.NoError(t, db.AutoMigrate(&RequestTraceMetadata{}))
			}
			require.NoError(t, MigrateRequestTraces())
			metadata := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}})
			require.NoError(t, RecordRequestTraces(t.Context(), []RequestTraceMetadata{{RequestId: "preserved-metadata", UserId: 11, Data: metadata}}))
			row := RequestTrace{RequestId: "legacy-timeline", RequestKind: "chat", Outcome: "success", StartedAtUS: 100, FinishedAtUS: 150, DurationUS: 50, CaptureStatus: "complete", NodeId: "migration-test", SchemaVersion: 1, TraceData: `{"measurement_version":3}`}
			require.NoError(t, db.Create(&row).Error)
			require.NoError(t, db.Migrator().RenameTable(&RequestTrace{}, "request_traces"))
			for range 2 {
				if !separate {
					require.NoError(t, migrateDB())
				}
				require.NoError(t, migrateLOGDB())
			}
			assert.False(t, db.Migrator().HasTable("request_traces"))
			columns, err := db.Migrator().ColumnTypes(&RequestTrace{})
			require.NoError(t, err)
			assert.Len(t, columns, 21)
			columns, err = db.Migrator().ColumnTypes(&RequestTraceMetadata{})
			require.NoError(t, err)
			assert.Len(t, columns, 6)
			stored, err := FindRequestTrace(t.Context(), row.RequestId, 0)
			require.NoError(t, err)
			assert.Equal(t, row, *stored)
			views, err := GetRequestTraces(t.Context(), "preserved-metadata", 11, false)
			require.NoError(t, err)
			require.Len(t, views, 1)
			assert.Equal(t, 200, views[0].Trace.Status)
		})
	}
	t.Run("unknown legacy layout preserves the source table", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		legacy := struct {
			Id      int64 `gorm:"primaryKey"`
			Payload string
		}{Id: 1, Payload: "preserve"}
		require.NoError(t, db.Table("request_traces").Migrator().CreateTable(&legacy))
		require.NoError(t, db.Table("request_traces").Create(&legacy).Error)
		require.ErrorContains(t, migrateRequestTraceTableName(db), "unknown legacy request_traces layout")
		var stored struct {
			Id      int64
			Payload string
		}
		require.NoError(t, db.Table("request_traces").First(&stored).Error)
		assert.Equal(t, "preserve", stored.Payload)
		assert.False(t, db.Migrator().HasTable(&RequestTrace{}))
	})
	t.Run("existing destination preserves both metadata tables", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		require.NoError(t, db.Table("request_traces").Migrator().CreateTable(&RequestTraceMetadata{}))
		legacy := RequestTraceMetadata{RequestId: "legacy-preserved", Data: "legacy"}
		current := RequestTraceMetadata{RequestId: "current-preserved", Data: "current"}
		require.NoError(t, db.Table("request_traces").Create(&legacy).Error)
		require.NoError(t, db.Create(&current).Error)
		require.ErrorContains(t, migrateRequestTraceTableName(db), "both request_traces and request_trace_metadata exist")
		var legacyStored, currentStored RequestTraceMetadata
		require.NoError(t, db.Table("request_traces").First(&legacyStored).Error)
		require.NoError(t, db.First(&currentStored).Error)
		assert.Equal(t, legacy, legacyStored)
		assert.Equal(t, current, currentStored)
	})
}

func TestRequestTraceStorageContractMySQL(t *testing.T) {
	t.Run("live compression changes and legacy frames remain readable", func(t *testing.T) {
		_, _ = requestTraceWriterMySQL(t)
		value := `{"trace":{"status":200},"padding":"` + strings.Repeat("metadata-", 200) + `"}`
		var compressor common.RequestTraceCompressor
		t.Cleanup(func() { assert.NoError(t, compressor.Close()) })
		for _, level := range []string{"fast", "standard", "fast"} {
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", fmt.Sprintf(`{"level":"all","compression_level":%q}`, level)))
			encoded, err := compressor.Encode(value)
			require.NoError(t, err)
			decoded, err := common.DecodeRequestTraceStorage(encoded)
			require.NoError(t, err)
			assert.Equal(t, value, decoded)
		}
		require.NoError(t, compressor.Close())
		encoded, err := compressor.Encode(`{"trace":{"status":201}}`)
		require.NoError(t, err)
		assert.Equal(t, `{"trace":{"status":201}}`, encoded)
		for _, level := range []zstd.EncoderLevel{zstd.SpeedBetterCompression, zstd.SpeedBestCompression} {
			encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1))
			require.NoError(t, err)
			frame := encoder.EncodeAll([]byte(value), nil)
			require.NoError(t, encoder.Close())
			decoded, err := common.DecodeRequestTraceStorage("zstd-base64-v1:" + base64.StdEncoding.EncodeToString(frame))
			require.NoError(t, err)
			assert.Equal(t, value, decoded)
		}
	})
	for _, level := range []string{"fast", "standard", "high", "best"} {
		t.Run(level, func(t *testing.T) {
			_, _ = requestTraceWriterMySQL(t)
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(common.RequestTraceConfig{Level: "all", CompressionLevel: level, SuccessSampleRate: 100})))
			data := common.RequestTraceData{Version: 1, Trace: common.RequestTraceSummary{Status: 200, Milliseconds: 19}, AdminInfo: &common.RequestTraceAdmin{ChannelID: 7, Client: common.RequestTraceClient{UserAgent: strings.Repeat("compressible-metadata", 100)}}}
			encoded := common.GetJsonString(data)
			rows := traceWriterRows("codec-"+level, 0, 1, encoded)
			require.NoError(t, SubmitRequestTraces(rows))
			stopTraceWriter(t)
			views, err := GetRequestTraces(t.Context(), rows[0].RequestId, 0, true)
			require.NoError(t, err)
			require.Len(t, views, 1)
			assert.Equal(t, data, views[0].RequestTraceData)
			var stored RequestTraceMetadata
			require.NoError(t, LOG_DB.Where("request_id = ?", rows[0].RequestId).First(&stored).Error)
			assert.True(t, strings.HasPrefix(stored.Data, "zstd-base64-v1:"))
			assert.Less(t, len(stored.Data), len(encoded))
		})
	}
	t.Run("legacy JSON and corrupt compressed records", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		legacy := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 201}})
		require.NoError(t, db.Create(&RequestTraceMetadata{RequestId: "legacy-json", UserId: 11, Data: legacy}).Error)
		views, err := GetRequestTraces(t.Context(), "legacy-json", 11, false)
		require.NoError(t, err)
		require.Len(t, views, 1)
		assert.Equal(t, 201, views[0].Trace.Status)
		for index, data := range []string{"zstd-base64-v1:not base64", "zstd-base64-v1:AAAA", "{not-json"} {
			id := fmt.Sprintf("corrupt-%d", index)
			require.NoError(t, db.Create(&RequestTraceMetadata{RequestId: id, UserId: 11, Data: data}).Error)
			_, err := GetRequestTraces(t.Context(), id, 11, false)
			assert.Error(t, err)
		}
	})
	t.Run("decoded size limit rejects oversized frames", func(t *testing.T) {
		_, _ = requestTraceWriterMySQL(t)
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(common.TraceMetadataMaxBytes))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, encoder.Close()) })
		frame := encoder.EncodeAll([]byte(strings.Repeat("x", 2*common.TraceMetadataMaxBytes)), nil)
		_, err = common.DecodeRequestTraceStorage("zstd-base64-v1:" + base64.StdEncoding.EncodeToString(frame))
		assert.Error(t, err)
	})
}
