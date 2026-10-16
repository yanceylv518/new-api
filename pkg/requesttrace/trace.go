package requesttrace

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const SchemaVersion = 1

const (
	maxPhases            = 96
	maxAttempts          = 64
	maxExchanges         = 16
	maxEvents            = 64
	initialPhaseCapacity = 8
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
	BytesRead   int64   `json:"bytes_read"`
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
	events         int
	lastWriteUS    int64
	firstPayloadUS int64
	record         Record
}

type Span struct {
	collector *Collector
	index     int
}

func New(requestID, nodeID, method string, start time.Time) *Collector {
	return &Collector{start: start, current: -1, record: Record{
		RequestID: requestID, NodeID: nodeID, StartedAtUS: start.UnixMicro(), SchemaVersion: SchemaVersion,
		Data: Data{MeasurementVersion: 2, Method: method, Phases: []Phase{}, Attempts: []Attempt{}, Errors: []Failure{}, Capture: Coverage{Missing: []string{}}},
	}}
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

func (c *Collector) Begin(name string) *Span {
	if c == nil {
		return nil
	}
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if len(c.record.Data.Phases) >= maxPhases {
		c.record.Data.Capture.Truncated = true
		return nil
	}
	if cap(c.record.Data.Phases) == 0 {
		c.record.Data.Phases = make([]Phase, 0, initialPhaseCapacity)
	}
	index := len(c.record.Data.Phases)
	attemptIndex := 0
	if c.current >= 0 && c.record.Data.Attempts[c.current].EndUS == nil {
		attemptIndex = c.record.AttemptCount
	}
	c.record.Data.Phases = append(c.record.Data.Phases, Phase{Name: name, Attempt: attemptIndex, StartUS: now})
	return &Span{collector: c, index: index}
}

func (s *Span) End() {
	if s == nil {
		return
	}
	c := s.collector
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && c.record.Data.Phases[s.index].EndUS == nil {
		c.record.Data.Phases[s.index].EndUS = &now
	}
}

func (c *Collector) BeginAttempt(channelID int) {
	if c == nil {
		return
	}
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.record.AttemptCount++
	id := int64(channelID)
	c.record.FinalChannelID = &id
	c.current = -1
	if len(c.record.Data.Attempts) >= maxAttempts {
		c.record.Data.Capture.Truncated = true
		return
	}
	c.current = len(c.record.Data.Attempts)
	c.record.Data.Attempts = append(c.record.Data.Attempts, Attempt{
		Index: c.record.AttemptCount, ChannelID: id, StartUS: now, Exchanges: []Exchange{},
	})
}

func (c *Collector) EndAttempt(outcome string) {
	if c == nil {
		return
	}
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.current < 0 {
		return
	}
	attempt := &c.record.Data.Attempts[c.current]
	if attempt.EndUS == nil {
		if outcome == "success" && attempt.Stream != nil && attempt.Stream.Failed {
			outcome = "failed"
		}
		if outcome == "failed" {
			if len(attempt.Exchanges) > 0 {
				switch attempt.Exchanges[len(attempt.Exchanges)-1].EndReason {
				case "request_timeout", "read_timeout":
					outcome = "timeout"
				case "request_canceled", "read_canceled":
					outcome = "canceled"
				}
			}
			if attempt.Stream != nil && attempt.Stream.Failed {
				switch attempt.Stream.End {
				case "timeout":
					outcome = "timeout"
				case "client_gone":
					outcome = "canceled"
				}
			}
		}
		attempt.EndUS = &now
		attempt.Outcome = outcome
	}
}

func (c *Collector) SetStream(stream Stream) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if c.current >= 0 {
		c.record.Data.Attempts[c.current].Stream = &stream
	}
}

func (c *Collector) Fail(outcome, stage, code string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.record.Outcome = outcome
	if len(c.record.Data.Errors) < 32 {
		c.record.Data.Errors = append(c.record.Data.Errors, Failure{Stage: stage, Code: code, OffsetUS: c.OffsetUS()})
	} else {
		c.record.Data.Capture.Truncated = true
	}
}

func (c *Collector) Missing(reason string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.missing(reason)
	}
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

func (c *Collector) Write(n int, err error, payload bool) {
	if c == nil {
		return
	}
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if n > 0 {
		c.record.Data.Response.BytesWritten += int64(n)
		if c.record.Data.Response.LastWriteUS == nil || now > c.lastWriteUS {
			c.lastWriteUS = now
			c.record.Data.Response.LastWriteUS = &c.lastWriteUS
		}
		if payload && (c.record.FirstPayloadWriteOffsetUS == nil || now < c.firstPayloadUS) {
			c.firstPayloadUS = now
			c.record.FirstPayloadWriteOffsetUS = &c.firstPayloadUS
		}
	}
	if err != nil {
		c.record.Data.Response.WriteFailed = true
		if c.record.Outcome == "" || c.record.Outcome == "success" {
			c.record.Outcome = "failed"
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

func (c *Collector) ObserveCancel(err error) {
	if c == nil || err == nil {
		return
	}
	now := c.OffsetUS()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && (c.record.Data.Response.CancelUS == nil || now < *c.record.Data.Response.CancelUS) {
		c.record.Data.Response.CancelUS = &now
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
	duration := c.OffsetUS()
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
	if ctxErr != nil {
		if c.record.Data.Response.CancelUS == nil {
			c.missing("cancel_time_not_observed")
		}
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
			c.missing("unfinished_attempt")
			if attempt.Outcome == "" {
				attempt.Outcome = c.record.Outcome
			}
		}
		if len(attempt.Exchanges) == 0 {
			c.missing("upstream_transport")
		}
		for _, exchange := range attempt.Exchanges {
			if exchange.EndUS == nil {
				c.missing("upstream_body_not_closed")
			}
		}
		for _, exchange := range attempt.Exchanges {
			if exchange.EventStream && exchange.Status >= 200 && exchange.Status < 300 && attempt.Stream == nil {
				c.missing("stream_events")
			}
		}
		if attempt.Index == c.record.AttemptCount {
			// 每次尝试单独判定，避免已恢复的重试留下全局失败状态。
			if c.record.Outcome == "success" || c.record.Outcome == "failed" {
				switch attempt.Outcome {
				case "failed", "timeout", "canceled":
					c.record.Outcome = attempt.Outcome
				}
			}
			for _, exchange := range attempt.Exchanges {
				if exchange.FirstByteUS != nil && (c.record.FinalAttemptFirstByteOffsetUS == nil || *exchange.FirstByteUS < *c.record.FinalAttemptFirstByteOffsetUS) {
					c.record.FinalAttemptFirstByteOffsetUS = exchange.FirstByteUS
				}
			}
		}
	}
	for _, phase := range c.record.Data.Phases {
		if phase.EndUS == nil {
			c.missing("unfinished_phase")
		}
	}
	c.record.CaptureStatus = "complete"
	if len(c.record.Data.Capture.Missing) > 0 || c.record.Data.Capture.Truncated {
		c.record.CaptureStatus = "partial"
	}
	if c.record.FirstPayloadWriteOffsetUS != nil {
		// 摘要会独立排队，内部字段指针会使整份采集器随队列记录一起滞留。
		offset := *c.record.FirstPayloadWriteOffsetUS
		c.record.FirstPayloadWriteOffsetUS = &offset
	}
	return &c.record
}
