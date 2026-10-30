// Package requesttimeline observes one streaming request without retaining its body.
package requesttimeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
)

const Version = 1
const MaxDataBytes = 16 << 10

const maxErrorCaptureBytes = 8 << 10

// StreamError bounds diagnostic decoding without copying a large raw error
// object. The relay still parses and forwards the original stream normally.
type StreamError struct {
	Present       bool
	Code, Message string
	OmittedBytes  int
}

func (e *StreamError) UnmarshalJSON(data []byte) error {
	*e = StreamError{}
	if len(data) == 4 && string(data) == "null" {
		return nil
	}
	if len(data) > maxErrorCaptureBytes {
		*e = StreamError{Present: true, Code: "oversized_error_payload", OmittedBytes: len(data)}
		return nil
	}
	var value types.OpenAIError
	if common.Unmarshal(data, &value) != nil {
		*e = StreamError{Present: true, Code: "invalid_error_payload", Message: "Upstream returned an unrecognized error object"}
		return nil
	}
	*e = captureUpstreamError(value)
	return nil
}

// Only read supported scalar fields; formatting an arbitrary error value can
// copy an unbounded nested object before the diagnostic size check runs.
func captureUpstreamError(value any) StreamError {
	var code any
	var fallback, message string
	switch value := value.(type) {
	case nil:
		return StreamError{}
	case *types.OpenAIError:
		if value == nil {
			return StreamError{}
		}
		code, fallback, message = value.Code, value.Type, value.Message
	case types.OpenAIError:
		code, fallback, message = value.Code, value.Type, value.Message
	case map[string]any:
		code = value["code"]
		fallback, _ = value["type"].(string)
		message, _ = value["message"].(string)
	case string:
		fallback, message = "error", value
	default:
		return StreamError{Present: true, Code: "invalid_error_payload", Message: "Unsupported upstream error value; original omitted", OmittedBytes: -1}
	}
	result := StreamError{Present: true, Code: fallback, Message: message}
	switch code := code.(type) {
	case string:
		result.Code = code
	case float64, int, int64:
		result.Code = fmt.Sprint(code)
	}
	return result
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type Failure struct {
	OffsetMS         int64  `json:"offset_ms"`
	Stage            string `json:"stage"`
	Kind             string `json:"kind"`
	Code             string `json:"code"`
	Message          string `json:"message"`
	MessageTruncated bool   `json:"message_truncated"`
}

type Data struct {
	Version           int      `json:"version"`
	StartedAtMS       int64    `json:"started_at_ms"`
	RequestSentMS     *int64   `json:"request_sent_ms"`
	FirstResponseMS   *int64   `json:"first_response_ms"`
	FirstWriteMS      *int64   `json:"first_write_ms"`
	EndedMS           *int64   `json:"ended_ms"`
	TimingValid       bool     `json:"timing_valid"`
	RequestModel      string   `json:"request_model"`
	ChannelID         int      `json:"channel_id"`
	UpstreamRequestID string   `json:"upstream_request_id,omitempty"`
	ResponseID        string   `json:"response_id,omitempty"`
	ResponseModel     string   `json:"response_model,omitempty"`
	FirstChunkType    string   `json:"first_chunk_type,omitempty"`
	FinishReason      string   `json:"finish_reason,omitempty"`
	EndReason         string   `json:"end_reason,omitempty"`
	EndMarkerReceived bool     `json:"end_marker_received"`
	ReturnedUsage     *Usage   `json:"returned_usage"`
	FirstError        *Failure `json:"first_error"`
}

type Record struct {
	RequestID        string
	UserID           int64
	CreatedAt        int64
	UpstreamStatus   *int
	Result           string
	ErrorKind        *string
	RequestBodyBytes *int64
	Data             Data
}

type Trace struct {
	mu               sync.Mutex
	start            time.Time
	closed, skipped  bool
	requests, writes int
	secret           string
	firstErrorAt     time.Time
	record           Record
}

func New(start time.Time, requestID string, userID int, model string) *Trace {
	return &Trace{start: start, record: Record{RequestID: requestID, UserID: int64(userID), Result: "unknown", Data: Data{
		Version: Version, StartedAtMS: start.UnixMilli(), RequestModel: bounded(model, 256), TimingValid: true,
	}}}
}

func bounded(value string, size int) string {
	if len(value) > size {
		value = value[:size]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.Clone(value)
}

// Align millisecond rounding with the existing frt calculation while using the
// monotonic clock where available. C itself retains the exact legacy value.
func (t *Trace) offset(at time.Time) int64 {
	return (at.Sub(t.start) + time.Duration(t.start.Nanosecond()%int(time.Millisecond))).Milliseconds()
}

func (t *Trace) Candidate() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.skipped
}

func (t *Trace) Skip() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.skipped = true
	}
}

func (t *Trace) Request(req *http.Request, channelID int, secret string) *http.Request {
	if t == nil {
		return req
	}
	t.mu.Lock()
	if t.closed || t.skipped {
		t.mu.Unlock()
		return req
	}
	t.requests++
	if t.requests > 1 {
		t.skipped = true
		t.mu.Unlock()
		return req
	}
	t.record.Data.ChannelID, t.secret = channelID, secret
	if req.ContentLength > 0 || req.Body == nil || req.Body == http.NoBody {
		size := req.ContentLength
		if req.Body == nil || req.Body == http.NoBody {
			size = 0
		}
		t.record.RequestBodyBytes = &size
	}
	t.mu.Unlock()
	return req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteRequest: t.wroteRequest}))
}

func (t *Trace) wroteRequest(info httptrace.WroteRequestInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.skipped {
		return
	}
	t.writes++
	if t.writes > 1 {
		t.skipped = true
		return
	}
	if info.Err == nil {
		now := t.offset(time.Now())
		t.record.Data.RequestSentMS = &now
	}
}

func (t *Trace) Response(resp *http.Response, err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.skipped {
		return
	}
	if resp != nil {
		status := resp.StatusCode
		t.record.UpstreamStatus = &status
		for _, key := range []string{common.RequestIdKey, "X-Request-Id", "Request-Id"} {
			if value := resp.Header.Get(key); value != "" {
				t.record.Data.UpstreamRequestID = bounded(value, 256)
				break
			}
		}
		if status != http.StatusOK {
			t.fail("failed", "upstream_http_error", "upstream_response", fmt.Sprint(status), "Upstream returned a non-success status")
		}
	}
	if err != nil {
		kind := "upstream_request_error"
		var timeout interface{ Timeout() bool }
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			kind = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			kind = "client_disconnected"
		}
		result := "failed"
		if kind == "client_disconnected" || kind == "timeout" {
			result = "interrupted"
		}
		t.fail(result, kind, "upstream_request", kind, err.Error())
	}
}

func (t *Trace) FirstResponse(at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.skipped || t.record.Data.FirstResponseMS != nil {
		return
	}
	value := at.UnixMilli() - t.start.UnixMilli()
	t.record.Data.FirstResponseMS = &value
	if value != t.offset(at) {
		t.record.Data.TimingValid = false
	}
}

// markEnd requires mu and reuses its storage on every chunk.
func (t *Trace) markEnd() {
	if t.record.Data.EndedMS == nil {
		t.record.Data.EndedMS = new(int64)
	}
	*t.record.Data.EndedMS = t.offset(time.Now())
}

// PayloadWritten is called after a payload write/flush, never for a heartbeat.
func (t *Trace) PayloadWritten(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.skipped {
		return
	}
	t.markEnd()
	if err != nil {
		var timeout interface{ Timeout() bool }
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			t.fail("interrupted", "timeout", "downstream", "deadline_exceeded", err.Error())
		case errors.Is(err, context.Canceled):
			t.fail("interrupted", "client_disconnected", "downstream", "client_disconnected", err.Error())
		case errors.As(err, &timeout) && timeout.Timeout():
			t.fail("interrupted", "timeout", "downstream_write", "write_timeout", err.Error())
		default:
			t.fail("interrupted", "downstream_write_error", "downstream_write", "write_error", err.Error())
		}
		return
	}
	if t.record.Data.FirstWriteMS == nil {
		first := *t.record.Data.EndedMS
		t.record.Data.FirstWriteMS = &first
	}
}

// fail and failAt require mu. StreamStatus can report an earlier processing
// error at finalization; order by its observation time, not submission order.
func (t *Trace) fail(result, kind, stage, code, message string) {
	t.failAt(time.Now(), result, kind, stage, code, message, 0)
}

func (t *Trace) failAt(at time.Time, result, kind, stage, code, message string, omittedBytes int) {
	if t.record.Data.FirstError != nil && !at.Before(t.firstErrorAt) {
		return
	}
	// Capture time before any sanitizing work. Long input is omitted wholesale,
	// so clipping cannot accidentally retain a partial credential at the edge.
	offset := t.offset(at)
	truncated := omittedBytes != 0
	if omittedBytes > 0 {
		message = fmt.Sprintf("Upstream error payload omitted (%d bytes; capture limit %d bytes)", omittedBytes, maxErrorCaptureBytes)
	}
	if len(message) > maxErrorCaptureBytes {
		message = fmt.Sprintf("Error message omitted (%d bytes; capture limit %d bytes)", len(message), maxErrorCaptureBytes)
		truncated = true
	} else {
		message = common.SanitizeErrorLogContent(message, t.secret)
	}
	if len(code) > maxErrorCaptureBytes {
		code = "oversized_error_code"
	} else {
		code = common.SanitizeErrorLogContent(code, t.secret)
	}
	truncated = truncated || len(message) > 2048
	t.record.Data.FirstError = &Failure{OffsetMS: offset, Stage: stage, Kind: kind, Code: bounded(code, 128), Message: bounded(message, 2048), MessageTruncated: truncated}
	t.firstErrorAt = at
	t.record.Result = result
	value := kind
	t.record.ErrorKind = &value
}

func (t *Trace) ProcessingError(at time.Time, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && !t.skipped {
		t.failAt(at, "failed", "local_error", "response_processing", "stream_processing_error", message, 0)
	}
}

func (t *Trace) BusinessError(code, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && !t.skipped {
		t.fail("failed", "upstream_business_error", "upstream_read", code, message)
	}
}

func (t *Trace) LocalError(code, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed && !t.skipped {
		t.fail("failed", "local_error", "relay", code, message)
		t.markEnd()
	}
}

func (t *Trace) StreamEnd(reason string, clientErr error, processingError bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.skipped {
		return
	}
	t.markEnd()
	t.record.Data.EndReason = reason
	if reason == "done" {
		t.record.Data.EndMarkerReceived = true
	}
	switch {
	case errors.Is(clientErr, context.DeadlineExceeded):
		t.fail("interrupted", "timeout", "downstream", "deadline_exceeded", clientErr.Error())
	case clientErr != nil || reason == "client_gone":
		t.fail("interrupted", "client_disconnected", "downstream", "client_disconnected", "Downstream request was canceled")
	case reason == "timeout":
		t.fail("interrupted", "timeout", "upstream_read", "timeout", "Timed out waiting for upstream stream data")
	case reason == "scanner_error":
		t.fail("interrupted", "upstream_read_error", "upstream_read", "read_error", "Upstream stream read failed")
	case reason == "ping_fail":
		t.fail("interrupted", "downstream_write_error", "downstream_write", "ping_fail", "Downstream heartbeat write failed")
	case reason == "panic":
		t.fail("failed", "local_error", "response_processing", "panic", "Stream processing panicked")
	case processingError:
		t.fail("failed", "local_error", "response_processing", "stream_processing_error", "Stream processing reported an error")
	}
}

func (t *Trace) ObserveChat(response dto.ChatCompletionsStreamResponse, apiError StreamError) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.skipped {
		return
	}
	kind := "metadata"
	for _, choice := range response.Choices {
		switch {
		case len(choice.Delta.ToolCalls) > 0:
			kind = "tool_call"
		case choice.Delta.GetReasoningContent() != "":
			kind = "reasoning"
		case choice.Delta.GetContentString() != "":
			kind = "text"
		case choice.Delta.Role != "" && kind == "metadata":
			kind = "role"
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			t.record.Data.FinishReason = bounded(*choice.FinishReason, 64)
		}
	}
	if apiError.Present {
		kind = "error"
		t.failAt(time.Now(), "failed", "upstream_business_error", "upstream_read", apiError.Code, apiError.Message, apiError.OmittedBytes)
	}
	if t.record.Data.FirstChunkType == "" {
		t.record.Data.FirstChunkType = kind
	}
	if t.record.Data.ResponseID == "" {
		t.record.Data.ResponseID = bounded(response.Id, 256)
	}
	if response.Model != "" && t.record.Data.ResponseModel != response.Model {
		t.record.Data.ResponseModel = bounded(response.Model, 256)
	}
	if response.Usage != nil {
		t.record.Data.ReturnedUsage = &Usage{response.Usage.PromptTokens, response.Usage.CompletionTokens, response.Usage.TotalTokens}
	}
}

func (t *Trace) ObserveResponses(response dto.ResponsesStreamResponse, code, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.skipped {
		return
	}
	if t.record.Data.FirstChunkType == "" {
		t.record.Data.FirstChunkType = bounded(response.Type, 128)
	}
	if r := response.Response; r != nil {
		if r.ID != "" && t.record.Data.ResponseID == "" {
			t.record.Data.ResponseID = bounded(r.ID, 256)
		}
		if r.Model != "" && t.record.Data.ResponseModel != r.Model {
			t.record.Data.ResponseModel = bounded(r.Model, 256)
		}
		if r.Usage != nil {
			t.record.Data.ReturnedUsage = &Usage{r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.TotalTokens}
		}
		if apiError := captureUpstreamError(r.Error); apiError.Present {
			code, message = apiError.Code, apiError.Message
			t.failAt(time.Now(), "failed", "upstream_business_error", "upstream_read", code, message, apiError.OmittedBytes)
		}
	}
	switch response.Type {
	case "response.completed", "response.done":
		t.record.Data.EndMarkerReceived = true
		t.record.Data.FinishReason = "completed"
	case "response.failed", "response.error", "error":
		if code == "" {
			code = response.Type
		}
		t.fail("failed", "upstream_business_error", "upstream_read", code, message)
	case "response.incomplete", "response.cancelled", "response.canceled":
		t.record.Data.FinishReason = response.Type
	}
}

func (t *Trace) Freeze(localErr error) *Record {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	defer func() { t.secret = "" }()
	if t.skipped {
		return nil
	}
	if localErr != nil && t.record.Data.FirstError == nil {
		t.fail("failed", "local_error", "relay", "relay_error", localErr.Error())
	}
	if t.record.Data.FirstError == nil {
		if t.record.Data.EndMarkerReceived && t.record.Data.FirstWriteMS != nil && t.record.Data.FinishReason != "response.incomplete" && t.record.Data.FinishReason != "response.cancelled" && t.record.Data.FinishReason != "response.canceled" {
			t.record.Result = "success"
		} else {
			kind := "unconfirmed_end"
			t.record.ErrorKind = &kind
		}
	}
	previous := int64(0)
	for _, point := range []*int64{t.record.Data.RequestSentMS, t.record.Data.FirstResponseMS, t.record.Data.FirstWriteMS, t.record.Data.EndedMS} {
		if point != nil {
			if *point < previous {
				t.record.Data.TimingValid = false
			}
			previous = *point
		}
	}
	t.record.CreatedAt = time.Now().Unix()
	return &t.record
}
