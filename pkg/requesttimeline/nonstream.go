package requesttimeline

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

// SetNonStreaming freezes the client mode before an adapter changes its upstream mode.
func (t *Trace) SetNonStreaming() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.finishObservation()
	if !t.closed && !t.skipped {
		t.record.Data.NonStreaming = true
	}
}

// IsNonStreaming reports the original client mode even when an adapter uses SSE upstream.
func (t *Trace) IsNonStreaming() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.record.Data.NonStreaming
}

// responseUsage captures JSON without mapping it through a billing DTO, which
// would drop unknown provider fields and conflate missing fields with zeros.
type responseUsage struct{ captured StreamUsage }

func (u *responseUsage) UnmarshalJSON(data []byte) error {
	// The admin API accepts an object or an explicitly reported null.
	if data[0] == '{' || string(data) == "null" {
		u.captured.capture(data)
	}
	return nil
}

// ObserveJSONResponse is called on the buffered upstream body before conversion
// or usage estimation. It keeps only bounded metadata and usage, never content.
// Diagnostic parsing is independent of the adapter's parsing and error handling.
func (t *Trace) ObserveJSONResponse(body []byte, providerUsagePaths ...string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.finishObservation()
	if t.closed || t.skipped || !t.record.Data.NonStreaming {
		return
	}
	type usageFields struct {
		Usage         responseUsage `json:"usage"`
		UsageMetadata responseUsage `json:"usageMetadata"`
	}
	var response struct {
		usageFields
		ID           string `json:"id"`
		Model        string `json:"model"`
		Status       string `json:"status"`
		StopReason   string `json:"stop_reason"`
		ResponseID   string `json:"responseId"`
		ModelVersion string `json:"modelVersion"`
		Candidates   []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Error   StreamError `json:"error"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := common.Unmarshal(body, &response); err != nil {
		// A compatible upstream may give optional metadata a different type.
		// Decode usage independently: a metadata error must never hide it.
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) || common.Unmarshal(body, &response.usageFields) != nil {
			return
		}
	}
	usage := response.Usage
	if usage.captured.Bytes == 0 {
		usage = response.UsageMetadata
	}
	for _, path := range providerUsagePaths {
		if usage.captured.Bytes > 0 {
			break
		}
		field := gjson.GetBytes(body, path)
		if field.Exists() && (field.IsObject() || field.Type == gjson.Null) {
			// A projection with no matching fields is not reported usage.
			if strings.Contains(path, "{") && field.Raw == "{}" {
				continue
			}
			usage.captured.capture([]byte(field.Raw))
			break
		}
	}
	if usage.captured.Bytes > 0 {
		t.captureUpstreamUsage(usage.captured)
	}
	// Response() already retained the HTTP status and classified the failure.
	if t.record.UpstreamStatus != nil && *t.record.UpstreamStatus != http.StatusOK {
		return
	}
	if t.record.Data.FirstResponseMS == nil {
		first := t.offset(time.Now())
		t.record.Data.FirstResponseMS = &first
	}
	t.record.Data.ResponseID = bounded(response.ID, 256)
	if response.ID == "" {
		t.record.Data.ResponseID = bounded(response.ResponseID, 256)
	}
	t.record.Data.ResponseModel = bounded(response.Model, 256)
	if response.Model == "" {
		t.record.Data.ResponseModel = bounded(response.ModelVersion, 256)
	}
	t.record.Data.FirstChunkType = "metadata"
	t.record.Data.EndReason = "response_body"
	t.record.Data.EndMarkerReceived = true
	t.record.Data.FinishReason = "completed"
	if response.StopReason != "" {
		t.record.Data.FinishReason = bounded(response.StopReason, 128)
	} else if len(response.Choices) > 0 && response.Choices[0].FinishReason != "" {
		t.record.Data.FinishReason = bounded(response.Choices[0].FinishReason, 128)
	} else if len(response.Candidates) > 0 && response.Candidates[0].FinishReason != "" {
		t.record.Data.FinishReason = bounded(response.Candidates[0].FinishReason, 128)
	} else if response.Status != "" && response.Status != "completed" {
		t.record.Data.FinishReason = bounded("response."+response.Status, 128)
	}
	if response.Error.Present {
		t.failAt(time.Now(), "failed", "upstream_business_error", "upstream_read", response.Error.Code, response.Error.Message, response.Error.OmittedBytes)
	} else if response.Status == "failed" {
		t.fail("failed", "upstream_business_error", "upstream_read", "response.failed", "Upstream response failed")
	}
}
