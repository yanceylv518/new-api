package service

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/requesttrace"
)

// 存储版本与计时口径独立；读取时恢复公开 Data，避免影响旧前端和空值语义。
type storedRequestTrace struct {
	Version            int                  `json:"v"`
	MeasurementVersion int                  `json:"m"`
	Method             string               `json:"h"`
	Route              string               `json:"r"`
	Phases             []storedTracePhase   `json:"p,omitempty"`
	Attempts           []storedTraceAttempt `json:"a,omitempty"`
	Response           storedTraceResponse  `json:"w"`
	Capture            storedTraceCoverage  `json:"c"`
	Errors             []storedTraceFailure `json:"e,omitempty"`
}

type storedTracePhase struct {
	Name    string `json:"n"`
	Attempt int    `json:"a,omitempty"`
	StartUS int64  `json:"s"`
	EndUS   *int64 `json:"e,omitempty"`
}

type storedTraceEvent struct {
	Sequence int    `json:"q"`
	Name     string `json:"n"`
	OffsetUS int64  `json:"t"`
	Failed   bool   `json:"f,omitempty"`
}

type storedTraceExchange struct {
	Index       int                `json:"i"`
	Kind        string             `json:"k"`
	StartUS     int64              `json:"s"`
	HeadersUS   *int64             `json:"h,omitempty"`
	FirstByteUS *int64             `json:"b,omitempty"`
	EndUS       *int64             `json:"e,omitempty"`
	Status      int                `json:"c,omitempty"`
	Protocol    string             `json:"p,omitempty"`
	EventStream bool               `json:"t,omitempty"`
	Proxy       bool               `json:"x,omitempty"`
	Reused      *bool              `json:"r,omitempty"`
	BytesRead   int64              `json:"n"`
	EndReason   string             `json:"z,omitempty"`
	Events      []storedTraceEvent `json:"v"`
}

type storedTraceStream struct {
	FirstUS  *int64 `json:"f,omitempty"`
	LastUS   *int64 `json:"l,omitempty"`
	Count    int64  `json:"n"`
	MaxGapUS int64  `json:"g"`
	End      string `json:"e"`
	Failed   bool   `json:"x,omitempty"`
}

type storedTraceAttempt struct {
	Index     int                   `json:"i"`
	ChannelID int64                 `json:"c"`
	StartUS   int64                 `json:"s"`
	EndUS     *int64                `json:"e,omitempty"`
	Outcome   string                `json:"o"`
	Exchanges []storedTraceExchange `json:"x"`
	Stream    *storedTraceStream    `json:"t,omitempty"`
}

type storedTraceResponse struct {
	BytesWritten int64  `json:"n"`
	WriteFailed  bool   `json:"f,omitempty"`
	LastWriteUS  *int64 `json:"l,omitempty"`
	CancelUS     *int64 `json:"c,omitempty"`
	Hijacked     bool   `json:"h,omitempty"`
}

type storedTraceCoverage struct {
	Truncated bool     `json:"t,omitempty"`
	Missing   []string `json:"m,omitempty"`
}

type storedTraceFailure struct {
	Stage    string `json:"s"`
	Code     string `json:"c"`
	OffsetUS int64  `json:"t"`
}

type requestTraceEncodeBuffer struct {
	data      storedRequestTrace
	phases    []storedTracePhase
	attempts  []storedTraceAttempt
	exchanges []storedTraceExchange
	events    []storedTraceEvent
	failures  []storedTraceFailure
}

const (
	maxCachedTracePhases    = 32
	maxCachedTraceAttempts  = 4
	maxCachedTraceExchanges = 8
	maxCachedTraceEvents    = 64
	maxCachedTraceFailures  = 8
)

var requestTraceEncodeBuffers = sync.Pool{New: func() any {
	return &requestTraceEncodeBuffer{
		exchanges: []storedTraceExchange{}, events: []storedTraceEvent{},
	}
}}

func encodeRequestTraceJSON(data requesttrace.Data) ([]byte, error) {
	buffer := requestTraceEncodeBuffers.Get().(*requestTraceEncodeBuffer)
	defer buffer.release()
	exchangeCount, eventCount := 0, 0
	for _, attempt := range data.Attempts {
		exchangeCount += len(attempt.Exchanges)
		for _, exchange := range attempt.Exchanges {
			eventCount += len(exchange.Events)
		}
	}
	buffer.phases = slices.Grow(buffer.phases[:0], len(data.Phases))[:len(data.Phases)]
	buffer.attempts = slices.Grow(buffer.attempts[:0], len(data.Attempts))[:len(data.Attempts)]
	buffer.exchanges = slices.Grow(buffer.exchanges[:0], exchangeCount)[:exchangeCount]
	buffer.events = slices.Grow(buffer.events[:0], eventCount)[:eventCount]
	buffer.failures = slices.Grow(buffer.failures[:0], len(data.Errors))[:len(data.Errors)]
	buffer.data = storedRequestTrace{
		Version: 1, MeasurementVersion: data.MeasurementVersion, Method: data.Method, Route: data.Route,
		Response: storedTraceResponse(data.Response), Capture: storedTraceCoverage(data.Capture),
		Phases: buffer.phases, Attempts: buffer.attempts, Errors: buffer.failures,
	}
	for i, phase := range data.Phases {
		buffer.phases[i] = storedTracePhase(phase)
	}
	for i, failure := range data.Errors {
		buffer.failures[i] = storedTraceFailure(failure)
	}
	exchangeOffset, eventOffset := 0, 0
	for i, attempt := range data.Attempts {
		entry := &buffer.attempts[i]
		*entry = storedTraceAttempt{Index: attempt.Index, ChannelID: attempt.ChannelID, StartUS: attempt.StartUS,
			EndUS: attempt.EndUS, Outcome: attempt.Outcome, Stream: (*storedTraceStream)(attempt.Stream)}
		if attempt.Exchanges != nil {
			end := exchangeOffset + len(attempt.Exchanges)
			entry.Exchanges = buffer.exchanges[exchangeOffset:end:end]
			exchangeOffset = end
		}
		for j, exchange := range attempt.Exchanges {
			wire := &entry.Exchanges[j]
			*wire = storedTraceExchange{Index: exchange.Index, Kind: exchange.Kind, StartUS: exchange.StartUS,
				HeadersUS: exchange.HeadersUS, FirstByteUS: exchange.FirstByteUS, EndUS: exchange.EndUS,
				Status: exchange.Status, Protocol: exchange.Protocol, EventStream: exchange.EventStream,
				Proxy: exchange.Proxy, Reused: exchange.Reused, BytesRead: exchange.BytesRead, EndReason: exchange.EndReason}
			if exchange.Events != nil {
				end := eventOffset + len(exchange.Events)
				wire.Events = buffer.events[eventOffset:end:end]
				eventOffset = end
			}
			for k, event := range exchange.Events {
				wire.Events[k] = storedTraceEvent(event)
			}
		}
	}
	// Marshal 返回独立字节；工作区只在本次同步编码期间持有冻结记录的引用。
	return common.Marshal(&buffer.data)
}

func (b *requestTraceEncodeBuffer) release() {
	// 超大记录仍完整编码，只限制工作区复用，避免偶发重试长期撑大池内对象。
	if cap(b.phases) > maxCachedTracePhases || cap(b.attempts) > maxCachedTraceAttempts || cap(b.exchanges) > maxCachedTraceExchanges || cap(b.events) > maxCachedTraceEvents || cap(b.failures) > maxCachedTraceFailures {
		return
	}
	b.data = storedRequestTrace{}
	clear(b.phases)
	clear(b.attempts)
	clear(b.exchanges)
	clear(b.events)
	clear(b.failures)
	requestTraceEncodeBuffers.Put(b)
}

// 读取侧用指针区分必填标量的缺失与真实零值，不改变写入侧的分配成本。
type readTracePhase struct {
	storedTracePhase
	Name    *string `json:"n"`
	StartUS *int64  `json:"s"`
}

type readTraceEvent struct {
	storedTraceEvent
	Sequence *int    `json:"q"`
	Name     *string `json:"n"`
	OffsetUS *int64  `json:"t"`
}

type readTraceExchange struct {
	storedTraceExchange
	Index     *int             `json:"i"`
	Kind      *string          `json:"k"`
	StartUS   *int64           `json:"s"`
	BytesRead *int64           `json:"n"`
	Events    []readTraceEvent `json:"v"`
}

type readTraceStream struct {
	storedTraceStream
	Count    *int64  `json:"n"`
	MaxGapUS *int64  `json:"g"`
	End      *string `json:"e"`
}

type readTraceAttempt struct {
	storedTraceAttempt
	Index     *int                `json:"i"`
	ChannelID *int64              `json:"c"`
	StartUS   *int64              `json:"s"`
	Outcome   *string             `json:"o"`
	Exchanges []readTraceExchange `json:"x"`
	Stream    *readTraceStream    `json:"t"`
}

type readTraceResponse struct {
	storedTraceResponse
	BytesWritten *int64 `json:"n"`
}

type readTraceFailure struct {
	Stage    *string `json:"s"`
	Code     *string `json:"c"`
	OffsetUS *int64  `json:"t"`
}

func decodeRequestTraceJSON(raw []byte) (*requesttrace.Data, bool, error) {
	var header struct {
		Version json.RawMessage `json:"v"`
	}
	if err := common.Unmarshal(raw, &header); err != nil {
		return nil, false, err
	}
	if header.Version == nil {
		var data requesttrace.Data
		if err := common.Unmarshal(raw, &data); err != nil {
			return nil, false, err
		}
		if data.Phases == nil || data.Attempts == nil || data.Errors == nil || data.Capture.Missing == nil {
			return nil, false, errors.New("invalid request trace data")
		}
		return &data, false, nil
	}
	var version int
	if err := common.Unmarshal(header.Version, &version); err != nil || version != 1 {
		return nil, true, errors.New("unsupported request trace storage version")
	}
	var stored struct {
		MeasurementVersion int                  `json:"m"`
		Method             string               `json:"h"`
		Route              *string              `json:"r"`
		Phases             []readTracePhase     `json:"p"`
		Attempts           []readTraceAttempt   `json:"a"`
		Response           *readTraceResponse   `json:"w"`
		Capture            *storedTraceCoverage `json:"c"`
		Errors             []readTraceFailure   `json:"e"`
	}
	if err := common.Unmarshal(raw, &stored); err != nil {
		return nil, true, err
	}
	invalid := errors.New("invalid compact request trace data")
	if stored.Method == "" || stored.MeasurementVersion < 1 || stored.Route == nil || stored.Response == nil || stored.Response.BytesWritten == nil || stored.Capture == nil {
		return nil, true, invalid
	}
	data := requesttrace.Data{MeasurementVersion: stored.MeasurementVersion, Method: stored.Method, Route: *stored.Route,
		Response: requesttrace.Response(stored.Response.storedTraceResponse), Capture: requesttrace.Coverage(*stored.Capture),
		Phases: make([]requesttrace.Phase, len(stored.Phases)), Attempts: make([]requesttrace.Attempt, len(stored.Attempts)),
		Errors: make([]requesttrace.Failure, len(stored.Errors))}
	data.Response.BytesWritten = *stored.Response.BytesWritten
	if data.Capture.Missing == nil {
		data.Capture.Missing = []string{}
	}
	for i, phase := range stored.Phases {
		if phase.Name == nil || phase.StartUS == nil {
			return nil, true, invalid
		}
		data.Phases[i] = requesttrace.Phase{Name: *phase.Name, Attempt: phase.Attempt, StartUS: *phase.StartUS, EndUS: phase.EndUS}
	}
	for i, failure := range stored.Errors {
		if failure.Stage == nil || failure.Code == nil || failure.OffsetUS == nil {
			return nil, true, invalid
		}
		data.Errors[i] = requesttrace.Failure{Stage: *failure.Stage, Code: *failure.Code, OffsetUS: *failure.OffsetUS}
	}
	for i, attempt := range stored.Attempts {
		if attempt.Index == nil || attempt.ChannelID == nil || attempt.StartUS == nil || attempt.Outcome == nil {
			return nil, true, invalid
		}
		entry := &data.Attempts[i]
		*entry = requesttrace.Attempt{Index: *attempt.Index, ChannelID: *attempt.ChannelID, StartUS: *attempt.StartUS,
			EndUS: attempt.EndUS, Outcome: *attempt.Outcome}
		if stream := attempt.Stream; stream != nil {
			if stream.Count == nil || stream.MaxGapUS == nil || stream.End == nil {
				return nil, true, invalid
			}
			entry.Stream = &requesttrace.Stream{FirstUS: stream.FirstUS, LastUS: stream.LastUS, Count: *stream.Count,
				MaxGapUS: *stream.MaxGapUS, End: *stream.End, Failed: stream.Failed}
		}
		if attempt.Exchanges != nil {
			entry.Exchanges = make([]requesttrace.Exchange, len(attempt.Exchanges))
		}
		for j, exchange := range attempt.Exchanges {
			if exchange.Index == nil || exchange.Kind == nil || exchange.StartUS == nil || exchange.BytesRead == nil {
				return nil, true, invalid
			}
			wire := &entry.Exchanges[j]
			*wire = requesttrace.Exchange{Index: *exchange.Index, Kind: *exchange.Kind, StartUS: *exchange.StartUS,
				HeadersUS: exchange.HeadersUS, FirstByteUS: exchange.FirstByteUS, EndUS: exchange.EndUS,
				Status: exchange.Status, Protocol: exchange.Protocol, EventStream: exchange.EventStream,
				Proxy: exchange.Proxy, Reused: exchange.Reused, BytesRead: *exchange.BytesRead, EndReason: exchange.EndReason}
			if exchange.Events != nil {
				wire.Events = make([]requesttrace.Event, len(exchange.Events))
			}
			for k, event := range exchange.Events {
				if event.Sequence == nil || event.Name == nil || event.OffsetUS == nil {
					return nil, true, invalid
				}
				wire.Events[k] = requesttrace.Event{Sequence: *event.Sequence, Name: *event.Name, OffsetUS: *event.OffsetUS, Failed: event.Failed}
			}
		}
	}
	return &data, true, nil
}
