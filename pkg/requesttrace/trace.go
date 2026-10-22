package requesttrace

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const SchemaVersion = 1

const (
	maxAttempts  = 64
	maxExchanges = 16
)

type contextKey struct{}

type Phase struct {
	Name    string `json:"name"`
	Attempt int    `json:"attempt,omitempty"`
	StartUS int64  `json:"start_us"`
	EndUS   *int64 `json:"end_us"`
}

type Event struct {
	Sequence int    `json:"sequence"`
	Name     string `json:"name"`
	OffsetUS int64  `json:"offset_us"`
	Failed   bool   `json:"failed,omitempty"`
}

type Exchange struct {
	Index       int     `json:"index"`
	Kind        string  `json:"kind"`
	StartUS     int64   `json:"start_us"`
	HeadersUS   *int64  `json:"headers_us"`
	FirstByteUS *int64  `json:"first_byte_us"`
	EndUS       *int64  `json:"end_us"`
	Status      int     `json:"status,omitempty"`
	Protocol    string  `json:"protocol,omitempty"`
	EventStream bool    `json:"event_stream"`
	Proxy       bool    `json:"proxy"`
	Reused      *bool   `json:"reused"`
	BytesRead   *int64  `json:"bytes_read"`
	EndReason   string  `json:"end_reason,omitempty"`
	Events      []Event `json:"events"`
}

type Stream struct {
	FirstUS  *int64 `json:"first_us"`
	LastUS   *int64 `json:"last_us"`
	Count    int64  `json:"count"`
	MaxGapUS int64  `json:"max_gap_us"`
	End      string `json:"end"`
	Failed   bool   `json:"failed"`
}

type Attempt struct {
	Index     int        `json:"index"`
	ChannelID int64      `json:"channel_id"`
	StartUS   int64      `json:"start_us"`
	EndUS     *int64     `json:"end_us"`
	Outcome   string     `json:"outcome"`
	Exchanges []Exchange `json:"exchanges"`
	Stream    *Stream    `json:"stream,omitempty"`
}

type Coverage struct {
	Truncated bool     `json:"truncated"`
	Missing   []string `json:"missing"`
}

type Failure struct {
	Stage    string `json:"stage"`
	Code     string `json:"code"`
	OffsetUS int64  `json:"offset_us"`
}

type Response struct {
	BytesWritten int64  `json:"bytes_written"`
	WriteFailed  bool   `json:"write_failed"`
	LastWriteUS  *int64 `json:"last_write_us"`
	CancelUS     *int64 `json:"cancel_observed_us"`
	Hijacked     bool   `json:"hijacked"`
}

type Data struct {
	MeasurementVersion int       `json:"measurement_version,omitempty"`
	Method             string    `json:"method"`
	Route              string    `json:"route"`
	Phases             []Phase   `json:"phases"`
	Attempts           []Attempt `json:"attempts"`
	Response           Response  `json:"response"`
	Capture            Coverage  `json:"capture"`
	Errors             []Failure `json:"errors"`
}

type Record struct {
	SamplingPolicy                SamplingPolicy
	RequestID                     string
	UserID                        *int64
	TokenID                       *int64
	ModelName                     *string
	GroupName                     *string
	FinalChannelID                *int64
	RequestKind                   string
	IsStream                      *bool
	HTTPStatus                    *int
	Outcome                       string
	StartedAtUS                   int64
	FinishedAtUS                  int64
	DurationUS                    int64
	FinalAttemptFirstByteOffsetUS *int64
	FirstPayloadWriteOffsetUS     *int64
	AttemptCount                  int
	CaptureStatus                 string
	NodeID                        string
	SchemaVersion                 int
	Data                          Data
}

type Collector struct {
	mu             sync.Mutex
	start          time.Time
	closed         bool
	enabled        bool
	current        int
	attemptOutcome string
	forwarding     atomic.Bool
	forwardedUS    *int64
	forwardedErr   error
	record         Record
}

func New(requestID, nodeID, method string, start time.Time) *Collector {
	return &Collector{start: start, current: -1, record: Record{
		SamplingPolicy: SamplingPolicy{SuccessRate: 100},
		RequestID:      requestID, NodeID: nodeID, StartedAtUS: start.UnixMicro(), SchemaVersion: SchemaVersion,
		Data: Data{MeasurementVersion: 3, Method: method, Phases: []Phase{}, Attempts: []Attempt{}, Errors: []Failure{}, Capture: Coverage{Missing: []string{}}},
	}}
}

func (c *Collector) SetSamplingPolicy(policy SamplingPolicy) {
	c.mu.Lock()
	c.record.SamplingPolicy = policy
	c.mu.Unlock()
}

func WithCollector(ctx context.Context, collector *Collector) context.Context {
	return context.WithValue(ctx, contextKey{}, collector)
}

func FromContext(ctx context.Context) *Collector {
	collector, _ := ctx.Value(contextKey{}).(*Collector)
	return collector
}

func (c *Collector) RequestID() string {
	if c == nil {
		return ""
	}
	return c.record.RequestID
}

func (c *Collector) Status(status int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		if c.record.Data.Response.Hijacked && c.record.HTTPStatus != nil && *c.record.HTTPStatus == 101 {
			return
		}
		if c.record.HTTPStatus == nil || *c.record.HTTPStatus != status {
			value := status
			c.record.HTTPStatus = &value
		}
	}
}

func (c *Collector) EnsureAttempt(channelID int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	needed := !c.closed && c.current < 0 && c.record.AttemptCount == 0
	c.mu.Unlock()
	if needed {
		c.BeginAttempt(channelID)
	}
}

func (c *Collector) OffsetUS() int64 {
	if c == nil {
		return 0
	}
	return time.Since(c.start).Microseconds()
}

func (c *Collector) Enable(route, kind string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.enabled = true
		c.record.Data.Route = route
		c.record.RequestKind = kind
	}
}

func (c *Collector) Identify(userID, tokenID int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if userID > 0 && (c.record.UserID == nil || *c.record.UserID != int64(userID)) {
		value := int64(userID)
		c.record.UserID = &value
	}
	if tokenID > 0 && (c.record.TokenID == nil || *c.record.TokenID != int64(tokenID)) {
		value := int64(tokenID)
		c.record.TokenID = &value
	}
}

func (c *Collector) Describe(modelName, groupName string, stream *bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if modelName != "" {
		if len(modelName) > 4096 {
			modelName = strings.Clone(modelName[:4096])
			for !utf8.ValidString(modelName) {
				modelName = modelName[:len(modelName)-1]
			}
			c.missing("metadata_truncated")
		}
		if c.record.ModelName == nil || *c.record.ModelName != modelName {
			value := modelName
			c.record.ModelName = &value
		}
	}
	if groupName != "" {
		if len(groupName) > 4096 {
			groupName = strings.Clone(groupName[:4096])
			for !utf8.ValidString(groupName) {
				groupName = groupName[:len(groupName)-1]
			}
			c.missing("metadata_truncated")
		}
		if c.record.GroupName == nil || *c.record.GroupName != groupName {
			value := groupName
			c.record.GroupName = &value
		}
	}
	if stream != nil && (c.record.IsStream == nil || *c.record.IsStream != *stream) {
		value := *stream
		c.record.IsStream = &value
	}
}

func (c *Collector) BeginAttempt(channelID int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.record.AttemptCount++
	id := int64(channelID)
	c.record.FinalChannelID = &id
	c.current = -1
	c.attemptOutcome = ""
	c.forwardedUS = nil
	c.forwardedErr = nil
}

func (c *Collector) EndAttempt(outcome string) {
	if c == nil {
		return
	}
	c.forwarding.Store(false)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.current < 0 {
		if c.attemptOutcome == "" {
			c.attemptOutcome = outcome
		}
		return
	}
	attempt := &c.record.Data.Attempts[c.current]
	if attempt.Outcome == "" || attempt.Outcome == "success" {
		if outcome == "failed" && len(attempt.Exchanges) > 0 {
			switch attempt.Exchanges[len(attempt.Exchanges)-1].EndReason {
			case "request_timeout":
				outcome = "timeout"
			case "request_canceled":
				outcome = "canceled"
			}
		}
		attempt.Outcome = outcome
	}
	if outcome != "success" && attempt.EndUS == nil {
		now := c.OffsetUS()
		attempt.EndUS = &now
	}
	c.attemptOutcome = attempt.Outcome
}

func (c *Collector) SetStreamResult(end string, failed bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if failed {
		outcome := "failed"
		switch end {
		case "timeout":
			outcome = "timeout"
		case "client_gone":
			outcome = "canceled"
		}
		c.attemptOutcome = outcome
		if c.current >= 0 {
			c.record.Data.Attempts[c.current].Outcome = outcome
		}
	}
}

func (c *Collector) Fail(outcome string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.record.Outcome = outcome
}

func (c *Collector) missing(reason string) {
	for _, existing := range c.record.Data.Capture.Missing {
		if existing == reason {
			return
		}
	}
	if len(c.record.Data.Capture.Missing) < 16 {
		c.record.Data.Capture.Missing = append(c.record.Data.Capture.Missing, reason)
	}
}

func (c *Collector) Write(n int, err error, ctx context.Context) {
	if c == nil {
		return
	}
	// 常规转发由响应处理器统一结束，逐块写出不再取时或加锁。
	if err == nil {
		if n > 0 && !c.forwarding.Load() {
			c.ForwardingDone(ctx.Err())
		}
		return
	}
	if !c.forwarding.Load() {
		c.ForwardingDone(ctx.Err())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.record.Data.Response.WriteFailed = true
	if c.record.Outcome == "" || c.record.Outcome == "success" {
		c.record.Outcome = "failed"
	}
}

func (c *Collector) BeginForwarding() {
	if c != nil {
		c.forwarding.Store(true)
	}
}

// ForwardingDone 必须在响应处理结束、结算和日志之前调用；不表示客户端已收到数据。
func (c *Collector) ForwardingDone(ctxErr error) {
	if c == nil {
		return
	}
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.forwardedUS = &now
		c.forwardedErr = ctxErr
		if c.current >= 0 {
			c.record.Data.Attempts[c.current].EndUS = &now
		}
	}
	c.forwarding.Store(false)
}

func (c *Collector) FinishResponse(size int, ctxErr error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		if size >= 0 {
			c.record.Data.Response.BytesWritten = int64(size)
		}
		// 没有响应正文时，以处理器交还响应控制权为终点，不覆盖已完成的转发。
		if c.forwardedUS == nil && size <= 0 {
			now := c.OffsetUS()
			c.forwardedUS = &now
			c.forwardedErr = ctxErr
		}
	}
}

func (c *Collector) Hijack() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.record.Data.Response.Hijacked = true
		c.missing("hijacked_payload_writes")
	}
}

func (c *Collector) Freeze(status int, ctxErr error) *Record {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if !c.enabled {
		return nil
	}
	duration := int64(0)
	if c.forwardedUS != nil {
		duration = *c.forwardedUS
	} else {
		c.missing("forwarding_end_not_observed")
		for _, attempt := range c.record.Data.Attempts {
			duration = max(duration, attempt.StartUS)
			if attempt.EndUS != nil {
				duration = max(duration, *attempt.EndUS)
			}
			for _, exchange := range attempt.Exchanges {
				duration = max(duration, exchange.StartUS)
				if exchange.FirstByteUS != nil {
					duration = max(duration, *exchange.FirstByteUS)
				}
				if exchange.EndUS != nil {
					duration = max(duration, *exchange.EndUS)
				}
			}
		}
	}
	// 墙钟终点由同一次单调耗时推导，避免系统校时使起止差与耗时矛盾。
	c.record.DurationUS = duration
	c.record.FinishedAtUS = c.record.StartedAtUS + duration
	if status == 0 && c.record.HTTPStatus != nil {
		status = *c.record.HTTPStatus
	}
	if status >= 100 && status <= 599 && (c.record.HTTPStatus == nil || *c.record.HTTPStatus != status) {
		value := status
		c.record.HTTPStatus = &value
	}
	if c.forwardedUS != nil {
		ctxErr = c.forwardedErr
	}
	if ctxErr != nil {
		if c.record.Outcome != "panic" {
			c.record.Outcome = "canceled"
			if ctxErr == context.DeadlineExceeded {
				c.record.Outcome = "timeout"
			}
		}
	}
	if c.record.Outcome == "" {
		c.record.Outcome = "success"
		if status >= 400 {
			c.record.Outcome = "failed"
		}
	}
	for i := range c.record.Data.Attempts {
		attempt := &c.record.Data.Attempts[i]
		if attempt.EndUS == nil {
			if attempt.Outcome == "" {
				attempt.Outcome = c.record.Outcome
			}
		}
		if len(attempt.Exchanges) == 0 {
			c.missing("upstream_transport")
		}
		if attempt.Index == c.record.AttemptCount {
			if !c.record.Data.Capture.Truncated && len(attempt.Exchanges) > 0 {
				exchange := attempt.Exchanges[len(attempt.Exchanges)-1]
				c.record.FinalAttemptFirstByteOffsetUS = exchange.FirstByteUS
				if exchange.Status > 0 && exchange.FirstByteUS == nil && exchange.HeadersUS == nil {
					c.missing("upstream_first_byte_not_observed")
				}
			}
		}
	}
	if attempts := c.record.Data.Attempts; c.record.AttemptCount > 0 && (len(attempts) == 0 || attempts[len(attempts)-1].Index != c.record.AttemptCount) {
		c.missing("upstream_transport")
	}
	// 即使调用明细达到上限，最终业务失败也必须保留。
	if c.record.Outcome == "success" || c.record.Outcome == "failed" {
		switch c.attemptOutcome {
		case "failed", "timeout", "canceled":
			c.record.Outcome = c.attemptOutcome
		}
	}
	c.record.CaptureStatus = "complete"
	if len(c.record.Data.Capture.Missing) > 0 || c.record.Data.Capture.Truncated {
		c.record.CaptureStatus = "partial"
	}
	return &c.record
}
