package model

import (
	"context"
	"encoding/base64"
	"errors"
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
	require.NoError(t, db.AutoMigrate(&RequestTrace{}, &SystemTask{}))
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

func traceWriterRows(prefix string, start, count int, data string) []RequestTrace {
	rows := make([]RequestTrace, count)
	for index := range rows {
		rows[index] = RequestTrace{RequestId: fmt.Sprintf("%s-%d", prefix, start+index), UserId: 11, CreatedAt: time.Now().Unix(), Data: data}
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
		if tx.Statement.Table != (RequestTrace{}).TableName() {
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
	t.Run("row budget uses synchronous fallback without dropping data", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		entered, release := blockTraceWriterCreates(t, db)
		for start := 0; start < requestTraceQueueRows; start += requestTraceBatchRows {
			require.NoError(t, SubmitRequestTraces(traceWriterRows("row-budget", start, requestTraceBatchRows, data)))
		}
		awaitTraceWriterCreate(t, entered)
		awaitTraceWriterCreate(t, entered)
		assert.Equal(t, requestTraceQueueRows, GetRequestTraceWriterStats().QueuedRows)
		fallback := make(chan error, 1)
		go func() { fallback <- SubmitRequestTraces(traceWriterRows("row-overflow", 0, 1, data)) }()
		awaitTraceWriterCreate(t, entered)
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().FallbackRows-before.FallbackRows)
		release()
		require.NoError(t, <-fallback)
		stopTraceWriter(t)
		var count int64
		require.NoError(t, db.Model(&RequestTrace{}).Count(&count).Error)
		assert.EqualValues(t, requestTraceQueueRows+1, count)
		after := GetRequestTraceWriterStats()
		assert.EqualValues(t, requestTraceQueueRows+1, after.PersistedRows-before.PersistedRows)
		assert.Equal(t, before.FailedRows, after.FailedRows)
		assert.Zero(t, after.QueuedRows)
		assert.Zero(t, after.QueuedBytes)
	})
	t.Run("byte budget uses synchronous fallback and releases accounting", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		entered, release := blockTraceWriterCreates(t, db)
		large := `{"padding":"` + strings.Repeat("x", 512<<10) + `"}`
		accepted := requestTraceQueueBytes / len(large)
		for index := range accepted {
			require.NoError(t, SubmitRequestTraces(traceWriterRows("byte-budget", index, 1, large)))
		}
		awaitTraceWriterCreate(t, entered)
		awaitTraceWriterCreate(t, entered)
		assert.Equal(t, accepted*len(large), GetRequestTraceWriterStats().QueuedBytes)
		fallback := make(chan error, 1)
		go func() { fallback <- SubmitRequestTraces(traceWriterRows("byte-overflow", 0, 1, large)) }()
		awaitTraceWriterCreate(t, entered)
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().FallbackRows-before.FallbackRows)
		release()
		require.NoError(t, <-fallback)
		stopTraceWriter(t)
		var count int64
		require.NoError(t, db.Model(&RequestTrace{}).Count(&count).Error)
		assert.EqualValues(t, accepted+1, count)
		assert.Zero(t, GetRequestTraceWriterStats().QueuedBytes)
	})
	t.Run("oversized request group stays atomic in synchronous fallback", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		rows := traceWriterRows("large-group", 0, requestTraceBatchRows+1, data)
		require.NoError(t, SubmitRequestTraces(rows))
		var count int64
		require.NoError(t, db.Model(&RequestTrace{}).Count(&count).Error)
		assert.EqualValues(t, len(rows), count)
		assert.EqualValues(t, len(rows), GetRequestTraceWriterStats().FallbackRows-before.FallbackRows)
	})
	t.Run("injected MySQL deadlock retries once without duplicate rows", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		var attempts atomic.Int64
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("trace-deadlock-once", func(tx *gorm.DB) {
			if tx.Statement.Table == (RequestTrace{}).TableName() && attempts.Add(1) == 1 {
				tx.AddError(&mysql.MySQLError{Number: 1213, Message: "injected trace deadlock"})
			}
		}))
		require.NoError(t, SubmitRequestTraces(traceWriterRows("retry", 0, 1, data)))
		stopTraceWriter(t)
		assert.EqualValues(t, 2, attempts.Load())
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().Retries-before.Retries)
		var count int64
		require.NoError(t, db.Model(&RequestTrace{}).Count(&count).Error)
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
	t.Run("unencodable metadata fails visibly and releases pending index", func(t *testing.T) {
		_, _ = requestTraceWriterMySQL(t)
		before := GetRequestTraceWriterStats()
		err := SubmitRequestTraces(traceWriterRows("invalid-size", 0, 1, strings.Repeat("x", common.TraceMetadataMaxBytes+1)))
		require.Error(t, err)
		assert.False(t, errors.Is(err, context.Canceled))
		assert.EqualValues(t, 1, GetRequestTraceWriterStats().FailedRows-before.FailedRows)
		assert.False(t, HasPendingRequestTrace("invalid-size-0", 11, false))
	})
}

func TestRequestTraceMainAndSeparateLogMigration(t *testing.T) {
	mainDB, _ := requestTraceWriterMySQL(t)
	data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}})
	require.NoError(t, RecordRequestTraces(t.Context(), []RequestTrace{{RequestId: "main-migration", UserId: 11, CreatedAt: 100, Data: data}}))
	require.NoError(t, mainDB.Migrator().RenameTable(&RequestTrace{}, "request_traces"))
	for range 2 {
		require.NoError(t, migrateDB())
	}
	var mainRow RequestTrace
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
	require.NoError(t, logDB.AutoMigrate(&RequestTrace{}))
	require.NoError(t, RecordRequestTraces(t.Context(), []RequestTrace{{RequestId: "separate-log-migration", UserId: 11, CreatedAt: 200, Data: data}}))
	require.NoError(t, logDB.Migrator().RenameTable(&RequestTrace{}, "request_traces"))
	for range 2 {
		require.NoError(t, migrateLOGDB())
	}
	views, err := GetRequestTraces(t.Context(), "separate-log-migration", 11, false)
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, 200, views[0].Trace.Status)
	assert.False(t, logDB.Migrator().HasTable("request_traces"))
	var unchanged RequestTrace
	require.NoError(t, mainDB.Where("request_id = ?", "main-migration").First(&unchanged).Error)
	assert.Equal(t, mainRow, unchanged)
}

func TestRequestTraceStorageContractMySQL(t *testing.T) {
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
			var stored RequestTrace
			require.NoError(t, LOG_DB.Where("request_id = ?", rows[0].RequestId).First(&stored).Error)
			assert.True(t, strings.HasPrefix(stored.Data, "zstd-base64-v1:"))
			assert.Less(t, len(stored.Data), len(encoded))
		})
	}
	t.Run("legacy JSON and corrupt compressed records", func(t *testing.T) {
		db, _ := requestTraceWriterMySQL(t)
		legacy := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 201}})
		require.NoError(t, db.Create(&RequestTrace{RequestId: "legacy-json", UserId: 11, Data: legacy}).Error)
		views, err := GetRequestTraces(t.Context(), "legacy-json", 11, false)
		require.NoError(t, err)
		require.Len(t, views, 1)
		assert.Equal(t, 201, views[0].Trace.Status)
		for index, data := range []string{"zstd-base64-v1:not base64", "zstd-base64-v1:AAAA", "{not-json"} {
			id := fmt.Sprintf("corrupt-%d", index)
			require.NoError(t, db.Create(&RequestTrace{RequestId: id, UserId: 11, Data: data}).Error)
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
