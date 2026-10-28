package model

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-sql-driver/mysql"
)

const (
	requestTraceQueueRows     = 8192
	requestTraceQueueBytes    = 16 << 20
	requestTraceReservedRows  = requestTraceQueueRows / 8
	requestTraceReservedBytes = requestTraceQueueBytes / 8
	requestTraceBatchRows     = 128
	requestTraceBatchBytes    = 1 << 20
	requestTraceWorkers       = 2
	requestTraceWriteTimeout  = 3 * time.Second
)

var ErrRequestTraceDropped = errors.New("request trace queue is unavailable or full")

type requestTracePending struct {
	userID int
	rows   int
}

type requestTraceWriter struct {
	mu        sync.Mutex
	queue     chan []RequestTraceMetadata
	pending   map[string]requestTracePending
	rows      int
	bytes     int
	accepting bool
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	wg        sync.WaitGroup
}

var requestTraceWriterState struct {
	sync.RWMutex
	writer *requestTraceWriter
}

var requestTraceWriteCounters struct {
	enqueued             atomic.Int64
	persisted            atomic.Int64
	failed               atomic.Int64
	dropped              atomic.Int64
	droppedPriority      atomic.Int64
	droppedRegular       atomic.Int64
	retries              atomic.Int64
	inFlight             atomic.Int64
	collected            atomic.Int64
	sampledOut           atomic.Int64
	partial              atomic.Int64
	batches              atomic.Int64
	writeNanoseconds     atomic.Int64
	lastWriteNanoseconds atomic.Int64
	lastSuccess          atomic.Int64
	lastFailure          atomic.Int64
}

type RequestTraceWriterStats struct {
	Node           string `json:"node"`
	Running        bool   `json:"running"`
	QueuedRows     int    `json:"queued_rows"`
	QueuedBytes    int    `json:"queued_bytes"`
	QueueRowLimit  int    `json:"queue_row_limit"`
	QueueByteLimit int    `json:"queue_byte_limit"`
	InFlightRows   int64  `json:"in_flight_rows"`
	EnqueuedRows   int64  `json:"enqueued_rows"`
	PersistedRows  int64  `json:"persisted_rows"`
	FailedRows     int64  `json:"failed_rows"`
	// 兼容旧状态 API；当前不再执行同步回退，该计数固定为零。
	FallbackRows             int64   `json:"fallback_rows"`
	DroppedRows              int64   `json:"dropped_rows"`
	DroppedPriorityRows      int64   `json:"dropped_priority_rows"`
	DroppedRegularRows       int64   `json:"dropped_regular_rows"`
	Retries                  int64   `json:"retries"`
	CollectedRequests        int64   `json:"collected_requests"`
	SampledOutRequests       int64   `json:"sampled_out_requests"`
	PartialRequests          int64   `json:"partial_requests"`
	WriteBatches             int64   `json:"write_batches"`
	AverageWriteMilliseconds float64 `json:"average_write_ms"`
	LastWriteMilliseconds    float64 `json:"last_write_ms"`
	LastSuccessAt            int64   `json:"last_success_at"`
	LastFailureAt            int64   `json:"last_failure_at"`
}

func StartRequestTraceWriter() {
	requestTraceWriterState.Lock()
	defer requestTraceWriterState.Unlock()
	if requestTraceWriterState.writer != nil {
		select {
		case <-requestTraceWriterState.writer.done:
		default:
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &requestTraceWriter{
		queue:     make(chan []RequestTraceMetadata, requestTraceQueueRows),
		pending:   make(map[string]requestTracePending),
		accepting: true, ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	requestTraceWriterState.writer = w
	w.wg.Add(requestTraceWorkers)
	for range requestTraceWorkers {
		go func() {
			defer w.wg.Done()
			w.run()
		}()
	}
	go func() {
		w.wg.Wait()
		close(w.done)
	}()
}

func StopRequestTraceWriter(ctx context.Context) error {
	requestTraceWriterState.RLock()
	w := requestTraceWriterState.writer
	requestTraceWriterState.RUnlock()
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if w.accepting {
		w.accepting = false
		close(w.queue)
	}
	w.mu.Unlock()
	select {
	case <-w.done:
		w.cancel()
		return nil
	case <-ctx.Done():
		w.cancel()
		return fmt.Errorf("drain request trace writer: %w", ctx.Err())
	}
}

func RecordRequestTraceCollection(sampledOut, partial bool) {
	requestTraceWriteCounters.collected.Add(1)
	if sampledOut {
		requestTraceWriteCounters.sampledOut.Add(1)
	}
	if partial {
		requestTraceWriteCounters.partial.Add(1)
	}
}

func SubmitRequestTraces(rows []RequestTraceMetadata) error {
	return SubmitRequestTracesWithPriority(rows, false)
}

// 优先请求可使用预留容量；总行数/字节预算和整组准入规则保持一致。
func SubmitRequestTracesWithPriority(rows []RequestTraceMetadata, priority bool) error {
	if len(rows) == 0 {
		return nil
	}
	requestTraceWriterState.RLock()
	w := requestTraceWriterState.writer
	requestTraceWriterState.RUnlock()
	if w != nil {
		bytes := 0
		for _, row := range rows {
			bytes += len(row.Data)
		}
		rowLimit, byteLimit := requestTraceQueueRows, requestTraceQueueBytes
		if !priority {
			rowLimit -= requestTraceReservedRows
			byteLimit -= requestTraceReservedBytes
		}
		w.mu.Lock()
		if w.accepting && len(rows) <= requestTraceBatchRows && bytes <= requestTraceBatchBytes && w.rows+len(rows) <= rowLimit && w.bytes+bytes <= byteLimit {
			// 队列取得元数据行所有权；不保留上下文、正文或采集器。
			owned := append([]RequestTraceMetadata(nil), rows...)
			select {
			case w.queue <- owned:
				w.rows += len(owned)
				w.bytes += bytes
				for _, row := range owned {
					pending := w.pending[row.RequestId]
					pending.userID = row.UserId
					pending.rows++
					w.pending[row.RequestId] = pending
				}
				requestTraceWriteCounters.enqueued.Add(int64(len(owned)))
				w.mu.Unlock()
				return nil
			default:
			}
		}
		w.mu.Unlock()
	}
	// 追踪尽力写入；拥塞时丢弃并计数，数据库变慢不再阻塞请求协程。
	requestTraceWriteCounters.dropped.Add(int64(len(rows)))
	if priority {
		requestTraceWriteCounters.droppedPriority.Add(int64(len(rows)))
	} else {
		requestTraceWriteCounters.droppedRegular.Add(int64(len(rows)))
	}
	return ErrRequestTraceDropped
}

func HasPendingRequestTrace(requestID string, userID int, admin bool) bool {
	requestTraceWriterState.RLock()
	w := requestTraceWriterState.writer
	requestTraceWriterState.RUnlock()
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	pending, exists := w.pending[requestID]
	return exists && (admin || pending.userID == userID)
}

func GetRequestTraceWriterStats() RequestTraceWriterStats {
	c := &requestTraceWriteCounters
	stats := RequestTraceWriterStats{
		Node: common.NodeName, QueueRowLimit: requestTraceQueueRows, QueueByteLimit: requestTraceQueueBytes,
		InFlightRows: c.inFlight.Load(), EnqueuedRows: c.enqueued.Load(), PersistedRows: c.persisted.Load(), FailedRows: c.failed.Load(),
		Retries: c.retries.Load(), CollectedRequests: c.collected.Load(),
		DroppedRows:         c.dropped.Load(),
		DroppedPriorityRows: c.droppedPriority.Load(), DroppedRegularRows: c.droppedRegular.Load(),
		SampledOutRequests: c.sampledOut.Load(), PartialRequests: c.partial.Load(), WriteBatches: c.batches.Load(),
		LastWriteMilliseconds: float64(c.lastWriteNanoseconds.Load()) / float64(time.Millisecond),
		LastSuccessAt:         c.lastSuccess.Load(), LastFailureAt: c.lastFailure.Load(),
	}
	if stats.WriteBatches > 0 {
		stats.AverageWriteMilliseconds = float64(c.writeNanoseconds.Load()) / float64(time.Millisecond) / float64(stats.WriteBatches)
	}
	requestTraceWriterState.RLock()
	w := requestTraceWriterState.writer
	requestTraceWriterState.RUnlock()
	if w != nil {
		w.mu.Lock()
		stats.Running, stats.QueuedRows, stats.QueuedBytes = w.accepting, w.rows, w.bytes
		w.mu.Unlock()
	}
	return stats
}

func (w *requestTraceWriter) complete(rows []RequestTraceMetadata) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, row := range rows {
		w.rows--
		w.bytes -= len(row.Data)
		pending := w.pending[row.RequestId]
		pending.rows--
		if pending.rows == 0 {
			delete(w.pending, row.RequestId)
		} else {
			w.pending[row.RequestId] = pending
		}
	}
}

func (w *requestTraceWriter) run() {
	var compressor common.RequestTraceCompressor
	defer func() {
		if err := compressor.Close(); err != nil {
			common.SysError("close request trace compressor: " + err.Error())
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]RequestTraceMetadata, 0, requestTraceBatchRows)
	bytes := 0
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := writeRequestTraceBatch(w.ctx, batch, &compressor); err != nil {
			common.SysError(fmt.Sprintf("request trace batch write failed: rows=%d request_id=%s: %v", len(batch), batch[0].RequestId, err))
		}
		w.complete(batch)
		clear(batch)
		batch, bytes = batch[:0], 0
	}
	for {
		select {
		case rows, open := <-w.queue:
			if !open {
				flush()
				return
			}
			rowBytes := 0
			for _, row := range rows {
				rowBytes += len(row.Data)
			}
			if len(batch)+len(rows) > requestTraceBatchRows || bytes+rowBytes > requestTraceBatchBytes {
				flush()
			}
			batch = append(batch, rows...)
			bytes += rowBytes
			if len(batch) >= requestTraceBatchRows || bytes >= requestTraceBatchBytes {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.ctx.Done():
			flush()
			for rows := range w.queue {
				requestTraceWriteCounters.failed.Add(int64(len(rows)))
				w.complete(rows)
			}
			return
		}
	}
}

func writeRequestTraceBatch(parent context.Context, rows []RequestTraceMetadata, compressor *common.RequestTraceCompressor) error {
	counters := &requestTraceWriteCounters
	counters.inFlight.Add(int64(len(rows)))
	defer counters.inFlight.Add(-int64(len(rows)))
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		for index := range rows {
			rows[index].Id = 0
		}
		started := time.Now()
		ctx, cancel := context.WithTimeout(parent, requestTraceWriteTimeout)
		err = recordRequestTraces(ctx, rows, compressor)
		cancel()
		elapsed := time.Since(started).Nanoseconds()
		counters.batches.Add(1)
		counters.writeNanoseconds.Add(elapsed)
		counters.lastWriteNanoseconds.Store(elapsed)
		if err == nil || parent.Err() != nil || attempt != 0 || !transientRequestTraceWriteError(err) {
			break
		}
		counters.retries.Add(1)
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-parent.Done():
			timer.Stop()
			err = parent.Err()
		}
	}
	if err != nil {
		counters.failed.Add(int64(len(rows)))
		counters.lastFailure.Store(time.Now().Unix())
		return err
	}
	counters.persisted.Add(int64(len(rows)))
	counters.lastSuccess.Store(time.Now().Unix())
	return nil
}

func transientRequestTraceWriteError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	var mysqlError *mysql.MySQLError
	if errors.As(err, &mysqlError) {
		return mysqlError.Number == 1205 || mysqlError.Number == 1213 || mysqlError.Number == 1040
	}
	var state interface{ SQLState() string }
	return errors.As(err, &state) && (state.SQLState() == "40001" || state.SQLState() == "40P01")
}
