package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/requesttrace"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/go-sql-driver/mysql"
)

const (
	maxRequestTraceBytes             = 64 << 10
	requestTraceQueueCapacity        = 512
	requestTraceBatchSize            = 64
	maxRequestTraceBatchBytes        = 1 << 20
	requestTraceFlushInterval        = 250 * time.Millisecond
	requestTraceCleanupIdleInterval  = 10 * time.Minute
	requestTraceCleanupBatchInterval = 100 * time.Millisecond
	requestTraceCleanupRetryInterval = 30 * time.Second
	requestTraceCleanupTimeout       = 3 * time.Second
)

type pendingRequestTrace struct {
	entry model.RequestTrace
	raw   []byte
}

type RequestTraceWriter struct {
	enabled        bool
	mu             sync.RWMutex
	stopped        bool
	queue          chan *pendingRequestTrace
	done           chan struct{}
	cleanupDone    chan struct{}
	cancel         context.CancelFunc
	written        atomic.Int64
	dropped        atomic.Int64
	failed         atomic.Int64
	cleanupDeleted atomic.Int64
	cleanupFailed  atomic.Int64
	cleanupPending atomic.Bool
}

var activeRequestTraceWriter *RequestTraceWriter

func StartRequestTraceWriter() *RequestTraceWriter {
	ctx, cancel := context.WithCancel(context.Background())
	w := &RequestTraceWriter{enabled: common.GetEnvOrDefaultBool("REQUEST_TRACE_ENABLED", true), queue: make(chan *pendingRequestTrace, requestTraceQueueCapacity), done: make(chan struct{}), cleanupDone: make(chan struct{}), cancel: cancel}
	activeRequestTraceWriter = w
	go w.run(ctx)
	go w.cleanup(ctx)
	return w
}

func (w *RequestTraceWriter) Enabled() bool { return w.enabled }

func (w *RequestTraceWriter) Enqueue(record *requesttrace.Record) {
	if record == nil || !w.enabled {
		return
	}
	encoded, err := common.Marshal(record.Data)
	if err == nil && len(encoded) > maxRequestTraceBytes {
		record.Data.Capture.Truncated, record.CaptureStatus = true, "partial"
		for i := range record.Data.Attempts {
			for j := range record.Data.Attempts[i].Exchanges {
				record.Data.Attempts[i].Exchanges[j].Events = nil
			}
		}
		encoded, err = common.Marshal(record.Data)
		if err == nil && len(encoded) > maxRequestTraceBytes {
			for i := range record.Data.Attempts {
				record.Data.Attempts[i].Exchanges = nil
			}
			encoded, err = common.Marshal(record.Data)
		}
	}
	if err != nil || len(encoded) > maxRequestTraceBytes {
		w.dropped.Add(1)
		common.SysError("request trace encoding failed or exceeded size limit (request_id=" + record.RequestID + ")")
		return
	}
	entry := &pendingRequestTrace{raw: encoded, entry: model.RequestTrace{
		RequestId: record.RequestID, UserId: record.UserID, TokenId: record.TokenID, ModelName: record.ModelName,
		GroupName: record.GroupName, FinalChannelId: record.FinalChannelID, RequestKind: record.RequestKind,
		IsStream: record.IsStream, HTTPStatus: record.HTTPStatus, Outcome: record.Outcome,
		StartedAtUS: record.StartedAtUS, FinishedAtUS: record.FinishedAtUS, DurationUS: record.DurationUS,
		FinalAttemptFirstByteOffsetUS: record.FinalAttemptFirstByteOffsetUS, FirstPayloadWriteOffsetUS: record.FirstPayloadWriteOffsetUS,
		AttemptCount: record.AttemptCount, CaptureStatus: record.CaptureStatus, NodeId: record.NodeID,
		SchemaVersion: record.SchemaVersion,
	}}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.stopped {
		w.dropped.Add(1)
		return
	}
	select {
	case w.queue <- entry:
	default:
		w.dropped.Add(1)
		if w.dropped.Load()%100 == 1 {
			common.SysError("request trace queue full; records dropped")
		}
	}
}

func (w *RequestTraceWriter) run(ctx context.Context) {
	defer close(w.done)
	compressInApp := !common.UsingLogDatabase(common.DatabaseTypeClickHouse)
	var compressor requestTraceCompressor
	defer func() {
		if err := compressor.Close(); err != nil {
			common.SysError("request trace encoder close failed: " + err.Error())
		}
	}()
	flushTicker := time.NewTicker(requestTraceFlushInterval)
	defer flushTicker.Stop()
	batch := make([]*model.RequestTrace, 0, requestTraceBatchSize)
	batchBytes := 0
	for {
		select {
		case pending, ok := <-w.queue:
			if !ok {
				w.flush(ctx, batch)
				return
			}
			entry := &pending.entry
			if compressInApp {
				encoded, err := compressor.Encode(pending.raw, system_setting.GetRequestTraceCompressionLevel())
				if err != nil {
					entry.TraceData = string(pending.raw)
					common.SysError("request trace compression failed; storing original JSON (request_id=" + entry.RequestId + "): " + err.Error())
				} else {
					entry.TraceData = encoded
				}
			} else {
				entry.TraceData = string(pending.raw)
			}
			// 批次持有 entry 时也会保留整个 pending，提前释放原始 JSON 引用。
			pending.raw = nil
			if batchBytes+len(entry.TraceData) > maxRequestTraceBatchBytes {
				w.flush(ctx, batch)
				clear(batch)
				batch, batchBytes = batch[:0], 0
			}
			batch = append(batch, entry)
			batchBytes += len(entry.TraceData)
			if len(batch) == cap(batch) {
				w.flush(ctx, batch)
				clear(batch)
				batch, batchBytes = batch[:0], 0
			}
		case <-flushTicker.C:
			// 慢写入积压的 tick 不应把队列中已有的数据拆成小批次。
			if len(w.queue) > 0 {
				continue
			}
			w.flush(ctx, batch)
			clear(batch)
			batch, batchBytes = batch[:0], 0
		case <-ctx.Done():
			w.dropped.Add(int64(len(batch) + len(w.queue)))
			return
		}
	}
}

func (w *RequestTraceWriter) cleanup(ctx context.Context) {
	defer close(w.cleanupDone)
	if !common.IsMasterNode || common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			cleanupCtx, cancel := context.WithTimeout(ctx, requestTraceCleanupTimeout)
			deleted, more, err := model.CleanupRequestTraceBatch(cleanupCtx)
			cancel()
			delay := requestTraceCleanupIdleInterval
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				w.cleanupFailed.Add(1)
				common.SysError("request trace cleanup failed: " + err.Error())
				delay = requestTraceCleanupRetryInterval
			} else {
				w.cleanupDeleted.Add(deleted)
				w.cleanupPending.Store(more)
				if more {
					delay = requestTraceCleanupBatchInterval
				}
			}
			timer.Reset(delay)
		}
	}
}

func (w *RequestTraceWriter) flush(ctx context.Context, batch []*model.RequestTrace) {
	if len(batch) == 0 {
		return
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		for _, entry := range batch {
			entry.Id = 0
		}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = model.InsertRequestTraces(writeCtx, batch)
		cancel()
		if lastErr == nil {
			w.written.Add(int64(len(batch)))
			return
		}
		if ctx.Err() != nil || !requestTraceRetryable(lastErr) || attempt == 2 {
			break
		}
		delay := time.NewTimer(time.Duration(100*(1<<attempt)+rand.IntN(100)) * time.Millisecond)
		select {
		case <-ctx.Done():
			delay.Stop()
		case <-delay.C:
		}
	}
	w.failed.Add(int64(len(batch)))
	common.SysError(fmt.Sprintf("request trace batch write failed (count=%d, first_request_id=%s, last_request_id=%s): %v", len(batch), batch[0].RequestId, batch[len(batch)-1].RequestId, lastErr))
}

func requestTraceRetryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) {
		switch mysqlError.Number {
		case 1040, 1205, 1213, 2006, 2013:
			return true
		}
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		code := state.SQLState()
		return strings.HasPrefix(code, "08") || code == "40001" || code == "40P01" || code == "53300" || code == "57P01" || code == "57P02" || code == "57P03"
	}
	var sqliteError interface{ Code() int }
	if errors.As(err, &sqliteError) {
		code := sqliteError.Code() & 0xff
		return code == 5 || code == 6
	}
	return false
}

func (w *RequestTraceWriter) Shutdown(ctx context.Context) error {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		w.cancel()
		select {
		case <-w.cleanupDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		w.cancel()
		return ctx.Err()
	}
}

type RequestTraceWriterStatus struct {
	Enabled        bool  `json:"enabled"`
	Pending        int   `json:"pending"`
	Written        int64 `json:"written"`
	Dropped        int64 `json:"dropped"`
	Failed         int64 `json:"failed"`
	RetentionDays  int   `json:"retention_days"`
	CleanupDeleted int64 `json:"cleanup_deleted"`
	CleanupFailed  int64 `json:"cleanup_failed"`
	CleanupPending bool  `json:"cleanup_pending"`
}

func GetRequestTraceWriterStatus() RequestTraceWriterStatus {
	w := activeRequestTraceWriter
	if w == nil {
		return RequestTraceWriterStatus{}
	}
	return RequestTraceWriterStatus{
		Enabled: w.enabled, Pending: len(w.queue), Written: w.written.Load(), Dropped: w.dropped.Load(), Failed: w.failed.Load(), RetentionDays: model.RequestTraceRetentionDays(),
		CleanupDeleted: w.cleanupDeleted.Load(), CleanupFailed: w.cleanupFailed.Load(), CleanupPending: w.cleanupPending.Load(),
	}
}
