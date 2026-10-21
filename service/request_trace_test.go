package service

import (
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/requesttrace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestTraceStorageRejectsMissingRequiredFields(t *testing.T) {
	for _, raw := range []string{
		`{"v":1,"m":2,"h":"POST"}`,
		`{"v":null,"m":2,"h":"POST","r":"","w":{"n":0},"c":{}}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":null,"c":{}}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{},"c":{}}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0}}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":null}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":{},"p":[{"n":"auth"}]}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":{},"a":[{"i":1,"c":1,"s":0,"o":"success","x":[{"i":1,"k":"http","s":0,"n":0,"v":[{"n":"get_conn","t":0}]}]}]}`,
		`{"v":1,"m":2,"h":"POST","r":null,"w":{"n":0},"c":{}}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":null},"c":{}}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":{},"a":[{"i":1,"c":0,"o":"success","x":null}]}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":{},"a":[{"i":1,"c":0,"s":0,"o":"success","x":[{"i":1,"k":"http","s":0,"v":null}]}]}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":{},"a":[{"i":1,"c":0,"s":0,"o":"success","x":null,"t":{"n":0,"e":"done"}}]}`,
		`{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":{},"e":[{"s":"request","c":"timeout"}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := DecodeRequestTraceData(raw)
			require.Error(t, err)
			_, err = DecodeRequestTrace(raw)
			require.Error(t, err)
		})
	}
	valid := `{"v":1,"m":2,"h":"POST","r":"","w":{"n":0},"c":{},"p":[{"n":"auth","s":0}]}`
	raw, err := DecodeRequestTraceData(valid)
	require.NoError(t, err)
	var restored requesttrace.Data
	require.NoError(t, common.Unmarshal(raw, &restored))
	assert.Equal(t, int64(0), restored.Phases[0].StartUS)
	assert.Nil(t, restored.Phases[0].EndUS)
	assert.Empty(t, restored.Route)
	assert.Equal(t, int64(0), restored.Response.BytesWritten)
}

func TestRequestTraceWriterRejectsBeforeEncoding(t *testing.T) {
	for _, reason := range []string{"stopped", "full", "byte_limit"} {
		t.Run(reason, func(t *testing.T) {
			writer := &RequestTraceWriter{stopped: reason == "stopped", queue: make(chan *pendingRequestTrace, 1), nodeID: "reject-fixture", startedAtUS: time.Now().UnixMicro()}
			if reason == "full" {
				writer.queue <- &pendingRequestTrace{}
			}
			if reason == "byte_limit" {
				writer.pendingBytes = maxRequestTracePendingBytes
			}
			previous := activeRequestTraceWriter
			activeRequestTraceWriter = writer
			t.Cleanup(func() { activeRequestTraceWriter = previous })
			record := &requesttrace.Record{RequestID: "reject-fixture", SamplingPolicy: requesttrace.SamplingPolicy{SuccessRate: 100},
				Outcome: "success", Data: requesttrace.Data{Method: "POST", Route: strings.Repeat("x", 70<<10)}}
			writer.Enqueue(record)
			status := GetRequestTraceWriterStatus()
			assert.Equal(t, int64(1), status.Dropped)
			assert.Zero(t, status.DroppedEncoding)
			switch reason {
			case "stopped":
				assert.Equal(t, int64(1), status.DroppedStopped)
			case "full":
				assert.Equal(t, int64(1), status.DroppedQueueFull)
			case "byte_limit":
				assert.Equal(t, int64(1), status.DroppedByteLimit)
			}
		})
	}
}

func requestTraceStorageObservations() requesttrace.Data {
	zero, last, notReused := int64(0), int64(900), false
	return requesttrace.Data{
		MeasurementVersion: 2, Method: "POST", Route: "/v1/chat/completions",
		Phases: []requesttrace.Phase{{Name: "auth", StartUS: 0, EndUS: &zero}, {Name: "unfinished", StartUS: 12}},
		Attempts: []requesttrace.Attempt{{Index: 1, ChannelID: 42, StartUS: 0, EndUS: &last, Outcome: "success",
			Exchanges: []requesttrace.Exchange{{Index: 1, Kind: "http", StartUS: 0, HeadersUS: &zero, FirstByteUS: &zero, EndUS: &last,
				Status: 200, Protocol: "HTTP/2.0", EventStream: true, Proxy: true, Reused: &notReused, BytesRead: 1234, EndReason: "eof",
				Events: []requesttrace.Event{{Sequence: 7, Name: "get_conn", OffsetUS: 0}, {Sequence: 11, Name: "connect_done", OffsetUS: 7, Failed: true}}},
				{Index: 2, Kind: "http", StartUS: 5, Reused: nil, BytesRead: 0, Events: []requesttrace.Event{}}},
			Stream: &requesttrace.Stream{FirstUS: &zero, LastUS: &last, Count: 9, MaxGapUS: 31, End: "done", Failed: true}}},
		Response: requesttrace.Response{BytesWritten: 456, LastWriteUS: &zero, CancelUS: &last, WriteFailed: true, Hijacked: true},
		Capture:  requesttrace.Coverage{Truncated: true, Missing: []string{"unfinished_phase"}},
		Errors:   []requesttrace.Failure{{Stage: "channel", Code: "timeout", OffsetUS: 8}},
	}
}

func TestRequestTraceStoragePreservesObservations(t *testing.T) {
	data := requestTraceStorageObservations()
	for _, route := range []string{data.Route, "", "/v1/模型/\"escaped\\path"} {
		data.Route = route
		for _, level := range []string{"fastest", "default", "better", "best"} {
			t.Run(route+"/"+level, func(t *testing.T) {
				wire, err := encodeRequestTraceJSON(data)
				require.NoError(t, err)
				var compressor requestTraceCompressor
				t.Cleanup(func() { assert.NoError(t, compressor.Close()) })
				stored, err := compressor.Encode(wire, level)
				require.NoError(t, err)
				raw, err := DecodeRequestTraceData(stored)
				require.NoError(t, err)
				var restored requesttrace.Data
				require.NoError(t, common.Unmarshal(raw, &restored))
				assert.Equal(t, data, restored)
				typed, err := DecodeRequestTrace(stored)
				require.NoError(t, err)
				assert.Equal(t, data, *typed)
			})
		}
	}
	legacy, err := common.Marshal(data)
	require.NoError(t, err)
	restored, err := DecodeRequestTraceData(string(legacy))
	require.NoError(t, err)
	assert.Equal(t, legacy, restored)
	typed, err := DecodeRequestTrace(string(legacy))
	require.NoError(t, err)
	assert.Equal(t, data, *typed)
	padded := " \n" + string(legacy[:len(legacy)-1]) + `,"padding":"unknown field"} `
	for _, compressed := range []bool{false, true} {
		stored := padded
		if compressed {
			var compressor requestTraceCompressor
			t.Cleanup(func() { assert.NoError(t, compressor.Close()) })
			stored, err = compressor.Encode([]byte(padded), "fastest")
			require.NoError(t, err)
		}
		restored, err := DecodeRequestTraceData(stored)
		require.NoError(t, err)
		assert.Equal(t, padded, string(restored))
	}
	_, err = DecodeRequestTraceData(`{"v":2,"m":2,"h":"POST"}`)
	require.ErrorContains(t, err, "unsupported")
}

func BenchmarkRequestTraceJSON(b *testing.B) {
	for _, workload := range []string{"json", "sse64", "retry"} {
		b.Run(workload, func(b *testing.B) {
			data := requestTraceStorageObservations()
			end := int64(900)
			for _, name := range []string{"rate_limit", "distribute", "request_body", "validate", "pricing_prepare", "precharge", "channel_handler", "request_prepare", "response_handle", "usage_finalize", "settlement", "log_insert"} {
				data.Phases = append(data.Phases, requesttrace.Phase{Name: name, StartUS: 12, EndUS: &end})
			}
			if workload == "json" {
				data.Attempts[0].Stream = nil
			} else {
				data.Attempts[0].Stream.Count = 66
			}
			if workload == "retry" {
				final := data.Attempts[0]
				final.Index, final.ChannelID, final.StartUS = 2, 43, 1000
				data.Attempts[0].Outcome = "failed"
				data.Attempts = append(data.Attempts, final)
			}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := encodeRequestTraceJSON(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestRequestTraceStorageKeepsIndependentRecords(t *testing.T) {
	for _, name := range []string{"full", "empty", "nested_nil_and_empty", "many_phases"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := requestTraceStorageObservations()
			switch name {
			case "empty":
				data = requesttrace.Data{MeasurementVersion: 2, Method: "GET", Phases: []requesttrace.Phase{},
					Attempts: []requesttrace.Attempt{}, Errors: []requesttrace.Failure{}, Capture: requesttrace.Coverage{Missing: []string{}}}
			case "nested_nil_and_empty":
				data.Attempts = []requesttrace.Attempt{
					{Index: 1, ChannelID: 42, StartUS: 0, Outcome: "failed"},
					{Index: 2, ChannelID: 43, StartUS: 1, Outcome: "failed", Exchanges: []requesttrace.Exchange{}},
					{Index: 3, ChannelID: 44, StartUS: 2, Outcome: "success", Exchanges: []requesttrace.Exchange{
						{Index: 1, Kind: "http", StartUS: 2, BytesRead: 0},
						{Index: 2, Kind: "http", StartUS: 3, BytesRead: 0, Events: []requesttrace.Event{}},
					}},
				}
			case "many_phases":
				for range 40 {
					data.Phases = append(data.Phases, requesttrace.Phase{Name: "request_prepare", StartUS: 12})
				}
			}
			before, err := common.Marshal(data)
			require.NoError(t, err)
			encoded, err := encodeRequestTraceJSON(data)
			require.NoError(t, err)
			other := requestTraceStorageObservations()
			other.Route = "/v1/另一个请求"
			_, err = encodeRequestTraceJSON(other)
			require.NoError(t, err)
			restored, err := DecodeRequestTrace(string(encoded))
			require.NoError(t, err)
			assert.Equal(t, data, *restored)
			after, err := common.Marshal(restored)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestRequestTraceWriterTrimmingAndBudget(t *testing.T) {
	for _, size := range []string{"normal", "events", "exchanges", "unrecoverable", "budget"} {
		t.Run(size, func(t *testing.T) {
			collector := requesttrace.New("trim-fixture", "node", "POST", time.Now())
			collector.Enable("/v1/chat/completions", "chat")
			collector.BeginAttempt(1)
			collector.EndAttempt("success")
			record := collector.Freeze(200, nil)
			require.NotNil(t, record)
			record.Data.Attempts[0].Exchanges = []requesttrace.Exchange{{Index: 1, Kind: "http", Protocol: "HTTP/1.1",
				Events: []requesttrace.Event{{Sequence: 1, Name: "get_conn"}}}}
			switch size {
			case "events":
				record.Data.Attempts[0].Exchanges[0].Events[0].Name = strings.Repeat("e", 70<<10)
			case "exchanges":
				record.Data.Attempts[0].Exchanges[0].Protocol = strings.Repeat("p", 70<<10)
			case "unrecoverable":
				record.Data.Route = strings.Repeat("r", 70<<10)
			}
			writer := &RequestTraceWriter{queue: make(chan *pendingRequestTrace, 1)}
			if size == "budget" {
				writer.pendingBytes = maxRequestTracePendingBytes - 1
			}
			writer.Enqueue(record)
			if size == "unrecoverable" || size == "budget" {
				assert.Empty(t, writer.queue)
				assert.Equal(t, int64(1), writer.dropped.Load())
				if size == "budget" {
					assert.Equal(t, int64(1), writer.droppedByteLimit)
					assert.Zero(t, writer.droppedEncoding)
				} else {
					assert.Equal(t, int64(1), writer.droppedEncoding)
				}
				return
			}
			require.Len(t, writer.queue, 1)
			pending := <-writer.queue
			restored, err := DecodeRequestTrace(string(pending.raw))
			require.NoError(t, err)
			assert.Equal(t, record.Data, *restored)
			assert.Equal(t, record.CaptureStatus, pending.entry.CaptureStatus)
			assert.Equal(t, len(pending.raw), writer.pendingBytes)
			assert.LessOrEqual(t, len(pending.raw), 64<<10)
			switch size {
			case "events":
				assert.Nil(t, restored.Attempts[0].Exchanges[0].Events)
				assert.True(t, restored.Capture.Truncated)
			case "exchanges":
				assert.Nil(t, restored.Attempts[0].Exchanges)
				assert.True(t, restored.Capture.Truncated)
			default:
				assert.Len(t, restored.Attempts[0].Exchanges[0].Events, 1)
				assert.False(t, restored.Capture.Truncated)
			}
		})
	}
}

func TestRequestTraceSamplingRetentionRules(t *testing.T) {
	firstByte := int64(5000)
	stream := true
	for _, item := range []struct {
		name   string
		policy requesttrace.SamplingPolicy
		record requesttrace.Record
		keep   bool
	}{
		{"full", requesttrace.SamplingPolicy{SuccessRate: 100}, requesttrace.Record{Outcome: "success", CaptureStatus: "complete"}, true},
		{"zero_success", requesttrace.SamplingPolicy{}, requesttrace.Record{Outcome: "success", CaptureStatus: "complete"}, false},
		{"failed", requesttrace.SamplingPolicy{}, requesttrace.Record{Outcome: "failed"}, true},
		{"timeout", requesttrace.SamplingPolicy{}, requesttrace.Record{Outcome: "timeout"}, true},
		{"canceled", requesttrace.SamplingPolicy{}, requesttrace.Record{Outcome: "canceled"}, true},
		{"partial", requesttrace.SamplingPolicy{}, requesttrace.Record{Outcome: "success", CaptureStatus: "partial"}, true},
		{"retried", requesttrace.SamplingPolicy{}, requesttrace.Record{Outcome: "success", AttemptCount: 2}, true},
		{"duration_boundary", requesttrace.SamplingPolicy{SlowRequestUS: 5000}, requesttrace.Record{Outcome: "success", DurationUS: 5000}, true},
		{"duration_below", requesttrace.SamplingPolicy{SlowRequestUS: 5000}, requesttrace.Record{Outcome: "success", DurationUS: 4999}, false},
		{"stream_duration", requesttrace.SamplingPolicy{SlowRequestUS: 5000}, requesttrace.Record{Outcome: "success", DurationUS: 50000, IsStream: &stream}, false},
		{"first_byte_boundary", requesttrace.SamplingPolicy{SlowFirstByteUS: 5000}, requesttrace.Record{Outcome: "success", IsStream: &stream, FinalAttemptFirstByteOffsetUS: &firstByte}, true},
		{"first_byte_missing", requesttrace.SamplingPolicy{SlowFirstByteUS: 5000}, requesttrace.Record{Outcome: "success"}, false},
	} {
		t.Run(item.name, func(t *testing.T) { assert.Equal(t, item.keep, item.policy.Keep(&item.record)) })
	}
	collector := requesttrace.New("snapshot", "node", "POST", time.Now())
	collector.Enable("/v1/chat/completions", "chat")
	collector.SetSamplingPolicy(requesttrace.SamplingPolicy{SuccessRate: 17, SlowRequestUS: 5000})
	record := collector.Freeze(200, nil)
	require.NotNil(t, record)
	assert.Equal(t, 17, record.SamplingPolicy.SuccessRate)
	assert.Equal(t, int64(5000), record.SamplingPolicy.SlowRequestUS)
}
