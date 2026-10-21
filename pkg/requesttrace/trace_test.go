package requesttrace

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newFourPointCollector(t *testing.T) (*Collector, *HTTPObservation) {
	t.Helper()
	collector := New("four-point-contract", "test-node", http.MethodPost, time.Now())
	collector.Enable("/v1/chat/completions", "chat")
	collector.BeginAttempt(7)
	observation := collector.StartUpstream("http", false)
	require.NotNil(t, observation)
	return collector, observation
}

func TestFourPointForwardingEndExcludesLaterCancellation(t *testing.T) {
	collector, observation := newFourPointCollector(t)
	observation.FirstByte()
	observation.Response(&http.Response{StatusCode: 200, Header: http.Header{}}, nil)
	before := collector.OffsetUS()
	collector.ForwardingDone(nil)
	after := collector.OffsetUS()
	collector.EndAttempt("success")
	record := collector.Freeze(200, context.Canceled)
	require.NotNil(t, record)
	assert.Equal(t, "success", record.Outcome)
	assert.GreaterOrEqual(t, record.DurationUS, before)
	assert.LessOrEqual(t, record.DurationUS, after)
	assert.Equal(t, record.StartedAtUS+record.DurationUS, record.FinishedAtUS)
	assert.Empty(t, record.Data.Phases)
	assert.Nil(t, record.FirstPayloadWriteOffsetUS)
	assert.Nil(t, record.Data.Response.CancelUS)
}

func TestFourPointRetryPreparationFailureDoesNotBorrowResponse(t *testing.T) {
	collector, observation := newFourPointCollector(t)
	observation.FirstByte()
	observation.Response(&http.Response{StatusCode: 503, Header: http.Header{}}, nil)
	collector.EndAttempt("failed")
	collector.BeginAttempt(8)
	collector.EndAttempt("failed")
	collector.ForwardingDone(nil)
	record := collector.Freeze(500, nil)
	require.NotNil(t, record)
	assert.Equal(t, 2, record.AttemptCount)
	require.Len(t, record.Data.Attempts, 1)
	assert.Nil(t, record.FinalAttemptFirstByteOffsetUS)
	assert.Equal(t, "partial", record.CaptureStatus)
	assert.Contains(t, record.Data.Capture.Missing, "upstream_transport")
	assert.True(t, (SamplingPolicy{}).Keep(record))
}

func TestFourPointMissingEndPreservesObservedLowerBound(t *testing.T) {
	collector, _ := newFourPointCollector(t)
	attemptEnd, exchangeEnd := int64(4000), int64(5000)
	collector.record.Data.Attempts[0].StartUS = 1000
	collector.record.Data.Attempts[0].EndUS = &attemptEnd
	collector.record.Data.Attempts[0].Exchanges[0].StartUS = 1000
	collector.record.Data.Attempts[0].Exchanges[0].EndUS = &exchangeEnd
	collector.EndAttempt("failed")
	record := collector.Freeze(500, nil)
	require.NotNil(t, record)
	assert.Equal(t, exchangeEnd, record.DurationUS)
	assert.Equal(t, "partial", record.CaptureStatus)
	assert.Contains(t, record.Data.Capture.Missing, "forwarding_end_not_observed")
}

func TestFourPointStreamBusinessFailureSurvivesSuccessSampling(t *testing.T) {
	collector, observation := newFourPointCollector(t)
	observation.FirstByte()
	collector.SetStreamResult("eof", true)
	collector.ForwardingDone(nil)
	collector.EndAttempt("success")
	record := collector.Freeze(200, nil)
	require.NotNil(t, record)
	assert.Equal(t, "failed", record.Outcome)
	assert.Equal(t, "failed", record.Data.Attempts[0].Outcome)
	assert.True(t, (SamplingPolicy{}).Keep(record))
	assert.Nil(t, record.Data.Attempts[0].Stream)
}

func TestFourPointTransportFailuresKeepTheirOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome string
	}{{"timeout", context.DeadlineExceeded, "timeout"}, {"canceled", context.Canceled, "canceled"}} {
		t.Run(tc.name, func(t *testing.T) {
			collector, observation := newFourPointCollector(t)
			observation.Response(nil, tc.err)
			collector.EndAttempt("failed")
			collector.ForwardingDone(nil)
			record := collector.Freeze(500, nil)
			require.NotNil(t, record)
			assert.Equal(t, tc.outcome, record.Outcome)
			assert.Nil(t, record.FinalAttemptFirstByteOffsetUS)
			assert.True(t, (SamplingPolicy{}).Keep(record))
		})
	}
}

func TestFourPointMissingFirstByteRemainsMissing(t *testing.T) {
	collector, observation := newFourPointCollector(t)
	observation.Response(&http.Response{StatusCode: 200, Header: http.Header{}}, nil)
	collector.ForwardingDone(nil)
	collector.EndAttempt("success")
	record := collector.Freeze(200, nil)
	require.NotNil(t, record)
	assert.Nil(t, record.FinalAttemptFirstByteOffsetUS)
	assert.Equal(t, "partial", record.CaptureStatus)
	assert.Contains(t, record.Data.Capture.Missing, "upstream_first_byte_not_observed")
}

func TestFourPointFrozenRecordIgnoresLateConcurrentObservers(t *testing.T) {
	collector, observation := newFourPointCollector(t)
	collector.ForwardingDone(nil)
	collector.EndAttempt("success")
	record := collector.Freeze(200, nil)
	require.NotNil(t, record)
	duration := record.DurationUS
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			observation.FirstByte()
			collector.ForwardingDone(context.Canceled)
			collector.BeginAttempt(9)
			collector.Fail("failed")
		}()
	}
	workers.Wait()
	assert.Equal(t, duration, record.DurationUS)
	assert.Equal(t, 1, record.AttemptCount)
	assert.Equal(t, "success", record.Outcome)
	assert.Nil(t, record.Data.Attempts[0].Exchanges[0].FirstByteUS)
	assert.Nil(t, collector.Freeze(500, context.Canceled))
}
