package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/requesttimeline"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

var activeTimelineWriter atomic.Pointer[RequestTimelineWriter]
var timelineStorageReady atomic.Bool

// InitRequestTimeline runs after LOG_DB is initialized. This optional feature
// cannot turn a healthy relay into a failed startup when its own table fails.
func InitRequestTimeline() *RequestTimelineWriter {
	timelineStorageReady.Store(false)
	if !model.RequestTimelineStorageSupported() {
		return nil
	}
	// Prepare storage and one idle writer so the system option can change
	// without restarting or disrupting requests already being observed.
	if common.IsMasterNode {
		if err := model.MigrateRequestTimelines(); err != nil {
			common.SysError("request timeline disabled: " + err.Error())
			return nil
		}
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ready := model.LOG_DB.WithContext(checkCtx).Migrator().HasTable(&model.RequestTimeline{})
	cancel()
	timelineStorageReady.Store(ready)
	if !ready {
		common.SysError("request timeline disabled: storage is not ready")
		return nil
	}
	w := NewRequestTimelineWriter(256, model.InsertRequestTimelines)
	activeTimelineWriter.Store(w)
	return w
}

func RequestTimelineEnabled() bool {
	if !model.RequestTimelineCaptureEnabled.Load() {
		return false
	}
	w := activeTimelineWriter.Load()
	return w != nil && !w.stopped.Load()
}
func RequestTimelineStorageReady() bool { return timelineStorageReady.Load() }
func EnqueueRequestTimeline(record *requesttimeline.Record) {
	if w := activeTimelineWriter.Load(); w != nil {
		w.Enqueue(record)
	}
}

// BeginRequestTimeline freezes the client's mode before upstream adapters can
// change IsStream. Channel tests never enter controller.Relay, and this guard
// also protects callers which already have an explicit test identity.
func BeginRequestTimeline(c *gin.Context, info *relaycommon.RelayInfo) *requesttimeline.Trace {
	if !RequestTimelineEnabled() || info == nil || !info.IsStream || info.IsChannelTest {
		return nil
	}
	if info.RelayFormat != types.RelayFormatOpenAI && info.RelayFormat != types.RelayFormatOpenAIResponses {
		return nil
	}
	if info.RelayMode != relayconstant.RelayModeChatCompletions && info.RelayMode != relayconstant.RelayModeResponses {
		return nil
	}
	apiType, _ := common.ChannelType2APIType(common.GetContextKeyInt(c, constant.ContextKeyChannelType))
	switch apiType {
	case constant.APITypeOpenAI, constant.APITypeOpenRouter, constant.APITypeXinference, constant.APITypeCodex, constant.APITypeNewAPI, constant.APITypeSub2API:
		return requesttimeline.New(info.StartTime, c.GetString(common.RequestIdKey), info.UserId, info.OriginModelName)
	default:
		return nil
	}
}

type RequestTimelineWriter struct {
	mu                       sync.RWMutex
	stopped                  atomic.Bool
	queue                    chan *requesttimeline.Record
	done                     chan struct{}
	cancel                   context.CancelFunc
	insert                   func(context.Context, []*model.RequestTimeline) error
	dropped, failed, written atomic.Int64
	reportedDrops            int64 // Owned exclusively by run.
}

// A record contains only bounded metadata, never a request context or body.
// The explicit capacity and insert boundary support deterministic fault tests.
func NewRequestTimelineWriter(capacity int, insert func(context.Context, []*model.RequestTimeline) error) *RequestTimelineWriter {
	ctx, cancel := context.WithCancel(context.Background())
	w := &RequestTimelineWriter{queue: make(chan *requesttimeline.Record, max(1, capacity)), done: make(chan struct{}), cancel: cancel, insert: insert}
	go w.run(ctx)
	return w
}

func (w *RequestTimelineWriter) Enqueue(record *requesttimeline.Record) bool {
	if record == nil {
		return false
	}
	w.mu.RLock()
	accepted := false
	if !w.stopped.Load() {
		select {
		case w.queue <- record:
			accepted = true
		default:
		}
	}
	w.mu.RUnlock()
	if !accepted {
		w.dropped.Add(1)
	}
	return accepted
}

type RequestTimelineWriterStats struct {
	Pending                  int
	Dropped, Failed, Written int64
}

func (w *RequestTimelineWriter) Stats() RequestTimelineWriterStats {
	return RequestTimelineWriterStats{Pending: len(w.queue), Dropped: w.dropped.Load(), Failed: w.failed.Load(), Written: w.written.Load()}
}

// Only the existing worker reports drops; the relay path never writes logs.
func (w *RequestTimelineWriter) reportDrops() {
	dropped := w.dropped.Load()
	if dropped != w.reportedDrops {
		common.SysError(fmt.Sprintf("request timeline diagnostic records dropped: total=%d, new=%d", dropped, dropped-w.reportedDrops))
		w.reportedDrops = dropped
	}
}

func (w *RequestTimelineWriter) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]*model.RequestTimeline, 0, 32)
	lastDropReport := time.Now()
	for {
		select {
		case record, ok := <-w.queue:
			if !ok {
				w.flush(ctx, batch)
				w.reportDrops()
				return
			}
			raw, err := common.Marshal(record.Data)
			if err != nil || len(raw) > requesttimeline.MaxDataBytes {
				if w.dropped.Add(1)%100 == 1 {
					common.SysError("request timeline rejected: invalid or oversized diagnostic payload")
				}
				continue
			}
			batch = append(batch, &model.RequestTimeline{RequestID: record.RequestID, UserID: record.UserID, CreatedAt: record.CreatedAt, UpstreamStatus: record.UpstreamStatus,
				Result: record.Result, ErrorKind: record.ErrorKind, UpstreamRequestBodyBytes: record.RequestBodyBytes, Data: string(raw)})
			if len(batch) == cap(batch) {
				w.flush(ctx, batch)
				clear(batch)
				batch = batch[:0]
			}
		case now := <-ticker.C:
			if now.Sub(lastDropReport) >= 10*time.Second {
				w.reportDrops()
				lastDropReport = now
			}
			w.flush(ctx, batch)
			clear(batch)
			batch = batch[:0]
		case <-ctx.Done():
			w.dropped.Add(int64(len(batch) + len(w.queue)))
			common.SysError("request timeline shutdown deadline reached; pending records discarded")
			return
		}
	}
}

func (w *RequestTimelineWriter) flush(ctx context.Context, batch []*model.RequestTimeline) {
	if len(batch) == 0 {
		return
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = w.insert(writeCtx, batch)
		cancel()
		if err == nil {
			w.written.Add(int64(len(batch)))
			return
		}
		if ctx.Err() != nil {
			break
		}
	}
	w.failed.Add(int64(len(batch)))
	common.SysError(fmt.Sprintf("request timeline write failed (%d records): %v", len(batch), err))
}

func (w *RequestTimelineWriter) Shutdown(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if !w.stopped.Swap(true) {
		close(w.queue)
	}
	w.mu.Unlock()
	defer activeTimelineWriter.CompareAndSwap(w, nil)
	select {
	case <-w.done:
		w.cancel()
		return nil
	case <-ctx.Done():
		w.cancel()
		return ctx.Err()
	}
}
