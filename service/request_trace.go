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
	maxRequestTracePendingBytes      = 16 << 20
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
	mu             sync.RWMutex
	stopped        bool
	queue          chan *pendingRequestTrace
	done           chan struct{}
	cleanupDone    chan struct{}
	cancel         context.CancelFunc
	written        atomic.Int64
	dropped        atomic.Int64
	sampledOut     atomic.Int64
	failed         atomic.Int64
	cleanupDeleted atomic.Int64
	cleanupFailed  atomic.Int64
	cleanupPending atomic.Bool
	// mu 保护运行状态；单写入协程按入队顺序完成批次，时间环只保存仍待完成记录的时刻。
	nodeID              string
	startedAtUS         int64
	accepted            int64
	pendingSince        [requestTraceQueueCapacity + requestTraceBatchSize + 1]time.Time
	pendingSizes        [requestTraceQueueCapacity + requestTraceBatchSize + 1]int
	pendingBytes        int
	pendingHead         int
	pendingCount        int
	buffered            int
	writing             int
	retries             int64
	droppedEncoding     int64
	droppedQueueFull    int64
	droppedByteLimit    int64
	droppedStopped      int64
	droppedShutdown     int64
	lastWrittenAtUS     int64
	lastFailedAtUS      int64
	lastBatchSize       int
	lastBatchDurationUS int64
}

var activeRequestTraceWriter *RequestTraceWriter

func StartRequestTraceWriter() *RequestTraceWriter {
	ctx, cancel := context.WithCancel(context.Background())
	w := &RequestTraceWriter{
		queue: make(chan *pendingRequestTrace, requestTraceQueueCapacity), done: make(chan struct{}),
		cleanupDone: make(chan struct{}), cancel: cancel,
		nodeID: common.GetNodeIdentity().Name, startedAtUS: time.Now().UnixMicro(),
	}
	activeRequestTraceWriter = w
	go w.run(ctx)
	go w.cleanup(ctx)
	return w
}

func (w *RequestTraceWriter) Enabled() bool { return system_setting.IsRequestTraceEnabled() }

func (w *RequestTraceWriter) Enqueue(record *requesttrace.Record) {
	// 开关只在请求入口采样，关闭后仍接收此前已开始采集的请求。
	if record == nil {
		return
	}
	if !record.SamplingPolicy.Keep(record) {
		w.sampledOut.Add(1)
		return
	}
	// 只提前拒绝已确定无法接收的状态；编码期间状态会变，入队时仍须精确复核。
	w.mu.Lock()
	rejected, logQueueFull := true, false
	switch {
	case w.stopped:
		w.droppedStopped++
	case w.pendingBytes >= maxRequestTracePendingBytes:
		w.droppedByteLimit++
	case len(w.queue) == cap(w.queue):
		w.droppedQueueFull++
		logQueueFull = w.dropped.Load()%100 == 0
	default:
		rejected = false
	}
	if rejected {
		w.dropped.Add(1)
	}
	w.mu.Unlock()
	if rejected {
		if logQueueFull {
			common.SysError("request trace queue full; records dropped")
		}
		return
	}
	stored := newStoredRequestTrace(record.Data)
	encoded, err := common.Marshal(stored)
	if err == nil && len(encoded) > maxRequestTraceBytes {
		record.Data.Capture.Truncated, record.CaptureStatus = true, "partial"
		stored.Capture.Truncated = true
		for i := range record.Data.Attempts {
			for j := range record.Data.Attempts[i].Exchanges {
				record.Data.Attempts[i].Exchanges[j].Events = nil
				stored.Attempts[i].Exchanges[j].Events = nil
			}
		}
		encoded, err = common.Marshal(stored)
		if err == nil && len(encoded) > maxRequestTraceBytes {
			for i := range record.Data.Attempts {
				record.Data.Attempts[i].Exchanges = nil
				stored.Attempts[i].Exchanges = nil
			}
			encoded, err = common.Marshal(stored)
		}
	}
	if err != nil || len(encoded) > maxRequestTraceBytes {
		w.mu.Lock()
		w.dropped.Add(1)
		w.droppedEncoding++
		w.mu.Unlock()
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
	w.mu.Lock()
	if w.stopped {
		w.dropped.Add(1)
		w.droppedStopped++
		w.mu.Unlock()
		return
	}
	// 预算覆盖排队、编码和 SQL 重试中的记录，直到批次结束才释放。
	if len(encoded) > maxRequestTracePendingBytes-w.pendingBytes {
		w.dropped.Add(1)
		w.droppedByteLimit++
		w.mu.Unlock()
		return
	}
	logQueueFull = false
	select {
	case w.queue <- entry:
		slot := (w.pendingHead + w.pendingCount) % len(w.pendingSince)
		w.pendingSince[slot] = time.Now()
		w.pendingSizes[slot] = len(encoded)
		w.pendingBytes += len(encoded)
		w.pendingCount++
		w.accepted++
	default:
		w.dropped.Add(1)
		w.droppedQueueFull++
		if w.dropped.Load()%100 == 1 {
			logQueueFull = true
		}
	}
	w.mu.Unlock()
	if logQueueFull {
		common.SysError("request trace queue full; records dropped")
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
			w.mu.Lock()
			w.buffered = len(batch)
			w.mu.Unlock()
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
			w.mu.Lock()
			w.dropped.Add(int64(w.pendingCount))
			w.droppedShutdown += int64(w.pendingCount)
			clear(w.pendingSince[:])
			clear(w.pendingSizes[:])
			w.pendingBytes = 0
			w.pendingHead, w.pendingCount = 0, 0
			w.buffered, w.writing = 0, 0
			w.mu.Unlock()
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
	started := time.Now()
	w.mu.Lock()
	w.buffered = 0
	w.writing = len(batch)
	w.mu.Unlock()
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		for _, entry := range batch {
			entry.Id = 0
		}
		if attempt > 0 {
			w.mu.Lock()
			w.retries++
			w.mu.Unlock()
		}
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = model.InsertRequestTraces(writeCtx, batch)
		cancel()
		if lastErr == nil {
			w.finishBatch(len(batch), started, true)
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
	w.finishBatch(len(batch), started, false)
	common.SysError(fmt.Sprintf("request trace batch write failed (count=%d, first_request_id=%s, last_request_id=%s): %v", len(batch), batch[0].RequestId, batch[len(batch)-1].RequestId, lastErr))
}

func (w *RequestTraceWriter) finishBatch(count int, started time.Time, success bool) {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writing = 0
	w.lastBatchSize = count
	w.lastBatchDurationUS = now.Sub(started).Microseconds()
	if success {
		w.written.Add(int64(count))
		w.lastWrittenAtUS = now.UnixMicro()
	} else {
		w.failed.Add(int64(count))
		w.lastFailedAtUS = now.UnixMicro()
	}
	for i := 0; i < count; i++ {
		w.pendingBytes -= w.pendingSizes[w.pendingHead]
		w.pendingSizes[w.pendingHead] = 0
		w.pendingSince[w.pendingHead] = time.Time{}
		w.pendingHead = (w.pendingHead + 1) % len(w.pendingSince)
		w.pendingCount--
	}
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
	Initialized            bool   `json:"initialized"`
	Enabled                bool   `json:"enabled"`
	Pending                int    `json:"pending"`
	Written                int64  `json:"written"`
	Dropped                int64  `json:"dropped"`
	SampledOut             int64  `json:"sampled_out"`
	PendingPayloadBytes    int    `json:"pending_payload_bytes"`
	PayloadBudgetBytes     int    `json:"payload_budget_bytes"`
	Failed                 int64  `json:"failed"`
	RetentionDays          int    `json:"retention_days"`
	CleanupDeleted         int64  `json:"cleanup_deleted"`
	CleanupFailed          int64  `json:"cleanup_failed"`
	CleanupPending         bool   `json:"cleanup_pending"`
	Accepting              bool   `json:"accepting"`
	NodeID                 string `json:"node_id"`
	StartedAtUS            int64  `json:"started_at_us"`
	QueueCapacity          int    `json:"queue_capacity"`
	Accepted               int64  `json:"accepted"`
	Processing             int    `json:"processing"`
	Buffered               int    `json:"buffered"`
	Writing                int    `json:"writing"`
	Outstanding            int    `json:"outstanding"`
	OldestOutstandingAgeUS *int64 `json:"oldest_outstanding_age_us"`
	Retries                int64  `json:"retries"`
	DroppedEncoding        int64  `json:"dropped_encoding"`
	DroppedQueueFull       int64  `json:"dropped_queue_full"`
	DroppedByteLimit       int64  `json:"dropped_byte_limit"`
	DroppedStopped         int64  `json:"dropped_stopped"`
	DroppedShutdown        int64  `json:"dropped_shutdown"`
	LastWrittenAtUS        *int64 `json:"last_written_at_us"`
	LastFailedAtUS         *int64 `json:"last_failed_at_us"`
	LastBatchSize          int    `json:"last_batch_size"`
	LastBatchDurationUS    *int64 `json:"last_batch_duration_us"`
}

func GetRequestTraceWriterStatus() RequestTraceWriterStatus {
	w := activeRequestTraceWriter
	if w == nil {
		return RequestTraceWriterStatus{Enabled: system_setting.IsRequestTraceEnabled(), QueueCapacity: requestTraceQueueCapacity, PayloadBudgetBytes: maxRequestTracePendingBytes, RetentionDays: model.RequestTraceRetentionDays()}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	pending := min(len(w.queue), w.pendingCount)
	enabled := w.Enabled()
	status := RequestTraceWriterStatus{
		Initialized: true, Enabled: enabled, Accepting: enabled && !w.stopped,
		NodeID: w.nodeID, StartedAtUS: w.startedAtUS, QueueCapacity: requestTraceQueueCapacity,
		Accepted: w.accepted, Processing: w.pendingCount - pending - w.buffered - w.writing,
		Buffered: w.buffered, Writing: w.writing, Outstanding: w.pendingCount, Pending: pending,
		Written: w.written.Load(), Dropped: w.dropped.Load(), Failed: w.failed.Load(), RetentionDays: model.RequestTraceRetentionDays(),
		SampledOut: w.sampledOut.Load(), PendingPayloadBytes: w.pendingBytes, PayloadBudgetBytes: maxRequestTracePendingBytes,
		CleanupDeleted: w.cleanupDeleted.Load(), CleanupFailed: w.cleanupFailed.Load(), CleanupPending: w.cleanupPending.Load(),
		Retries: w.retries, DroppedEncoding: w.droppedEncoding, DroppedQueueFull: w.droppedQueueFull,
		DroppedByteLimit: w.droppedByteLimit,
		DroppedStopped:   w.droppedStopped, DroppedShutdown: w.droppedShutdown, LastBatchSize: w.lastBatchSize,
	}
	if w.pendingCount > 0 {
		age := time.Since(w.pendingSince[w.pendingHead]).Microseconds()
		status.OldestOutstandingAgeUS = &age
	}
	if w.lastWrittenAtUS != 0 {
		value := w.lastWrittenAtUS
		status.LastWrittenAtUS = &value
	}
	if w.lastFailedAtUS != 0 {
		value := w.lastFailedAtUS
		status.LastFailedAtUS = &value
	}
	if w.lastBatchSize > 0 {
		value := w.lastBatchDurationUS
		status.LastBatchDurationUS = &value
	}
	return status
}
