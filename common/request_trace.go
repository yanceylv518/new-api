package common

import (
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
)

const (
	RequestTraceContextKey               = "request_trace_collector"
	UpstreamRequestIDCapturedContextKey  = "upstream_request_id_captured"
	maxUpstreamRequestIDHeaders          = 16
	maxUpstreamRequestIDHeaderNameLength = 128
)

type RequestTraceConfig struct {
	Level                    string   `json:"level"`
	PreChannelErrors         bool     `json:"pre_channel_errors_enabled"`
	AnonymousErrors          bool     `json:"anonymous_errors_enabled"`
	UpstreamRequestIDHeaders []string `json:"upstream_request_id_headers,omitempty"`
	CompressionLevel         string   `json:"compression_level,omitempty"`
	SuccessSampleRate        int      `json:"success_sample_rate"`
	RetentionDays            int      `json:"retention_days"`
	DiagnosticChannelIDs     []int    `json:"diagnostic_channel_ids,omitempty"`
	DiagnosticExpiresAt      int64    `json:"diagnostic_expires_at,omitempty"`
}

var requestTraceConfig = struct {
	sync.RWMutex
	value RequestTraceConfig
}{value: RequestTraceConfig{Level: "errors", PreChannelErrors: true, CompressionLevel: "fast", SuccessSampleRate: 100}}

func GetRequestTraceConfig() RequestTraceConfig {
	requestTraceConfig.RLock()
	defer requestTraceConfig.RUnlock()
	config := requestTraceConfig.value
	config.UpstreamRequestIDHeaders = append([]string(nil), config.UpstreamRequestIDHeaders...)
	config.DiagnosticChannelIDs = append([]int(nil), config.DiagnosticChannelIDs...)
	return config
}

func SetRequestTraceOption(key, value string) error {
	config, err := ParseRequestTraceConfig(value)
	if err != nil {
		return err
	}
	requestTraceConfig.Lock()
	defer requestTraceConfig.Unlock()
	requestTraceConfig.value = config
	return nil
}

func ParseRequestTraceConfig(value string) (RequestTraceConfig, error) {
	config := RequestTraceConfig{CompressionLevel: "fast", SuccessSampleRate: 100}
	var fields map[string]json.RawMessage
	if err := UnmarshalJsonStr(value, &fields); err != nil {
		return config, err
	}
	for _, name := range []string{"success_sample_rate", "retention_days", "diagnostic_expires_at"} {
		if strings.TrimSpace(string(fields[name])) == "null" {
			return config, errors.New("request trace " + name + " must be an integer")
		}
	}
	if err := UnmarshalJsonStr(value, &config); err != nil {
		return config, err
	}
	if config.Level != "off" && config.Level != "errors" && config.Level != "all" {
		return config, errors.New("invalid request trace level")
	}
	switch config.CompressionLevel {
	case "high", "best":
		// 旧配置映射到保留的标准档，已保存的数据仍按原格式解码。
		config.CompressionLevel = "standard"
	case "fast", "standard":
	default:
		return config, errors.New("invalid request trace compression level")
	}
	if config.SuccessSampleRate < 0 || config.SuccessSampleRate > 100 {
		return config, errors.New("request trace success sample rate must be between 0 and 100")
	}
	if config.RetentionDays < 0 || config.RetentionDays > 3650 {
		return config, errors.New("request trace retention must be between 0 and 3650 days")
	}
	if len(config.DiagnosticChannelIDs) > 16 {
		return config, errors.New("at most 16 diagnostic channels are allowed")
	}
	var channels []int
	for _, channelID := range config.DiagnosticChannelIDs {
		if channelID <= 0 {
			return config, errors.New("diagnostic channel IDs must be positive integers")
		}
		if !slices.Contains(channels, channelID) {
			channels = append(channels, channelID)
		}
	}
	config.DiagnosticChannelIDs = channels
	if config.DiagnosticExpiresAt < 0 || config.DiagnosticExpiresAt > time.Now().Add(24*time.Hour).Unix() || (len(channels) > 0 && config.DiagnosticExpiresAt == 0) {
		return config, errors.New("diagnostic expiry is required and must be within 24 hours")
	}
	if len(config.UpstreamRequestIDHeaders) > maxUpstreamRequestIDHeaders {
		return config, errors.New("at most 16 upstream request ID response headers are allowed")
	}
	var headers []string
	for _, name := range config.UpstreamRequestIDHeaders {
		name = strings.TrimSpace(name)
		if len(name) > maxUpstreamRequestIDHeaderNameLength || !httpguts.ValidHeaderFieldName(name) {
			return config, errors.New("upstream request ID response headers must be valid HTTP header names of at most 128 characters")
		}
		name = http.CanonicalHeaderKey(name)
		duplicate := false
		for _, existing := range headers {
			if existing == name {
				duplicate = true
				break
			}
		}
		if !duplicate {
			headers = append(headers, name)
		}
	}
	config.UpstreamRequestIDHeaders = headers
	return config, nil
}

func (config RequestTraceConfig) SampleSuccess(requestID string) bool {
	if config.SuccessSampleRate >= 100 {
		return true
	}
	if config.SuccessSampleRate <= 0 {
		return false
	}
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(requestID))
	return digest.Sum64()%100 < uint64(config.SuccessSampleRate)
}

func (config RequestTraceConfig) DiagnoseChannel(channelID int) bool {
	return config.Level != "off" && len(config.DiagnosticChannelIDs) > 0 && config.DiagnosticExpiresAt > time.Now().Unix() && slices.Contains(config.DiagnosticChannelIDs, channelID)
}

func GetUpstreamRequestID(headers http.Header) string {
	requestTraceConfig.RLock()
	defer requestTraceConfig.RUnlock()
	for _, name := range requestTraceConfig.value.UpstreamRequestIDHeaders {
		if value := TraceMetadataString(strings.TrimSpace(headers.Get(name)), 128); value != "" {
			return value
		}
	}
	for _, name := range [...]string{RequestIdKey, "X-Request-Id", "Request-Id", "X-Amzn-RequestId", "X-Goog-Request-Id"} {
		if value := TraceMetadataString(strings.TrimSpace(headers.Get(name)), 128); value != "" {
			return value
		}
	}
	return ""
}

type RequestTraceSummary struct {
	Milliseconds    int64   `json:"ms"`
	RequestBytes    *int64  `json:"req_bytes,omitempty"`
	Status          int     `json:"status"`
	FinishReason    *string `json:"finish_reason,omitempty"`
	Stage           string  `json:"stage,omitempty"`
	ClientErrorType string  `json:"client_error_type,omitempty"`
	Outcome         string  `json:"outcome,omitempty"`
}

type RequestTraceShape struct {
	Messages       int64    `json:"messages"`
	TextCharacters int64    `json:"text_chars"`
	System         bool     `json:"system"`
	Images         int64    `json:"images,omitempty"`
	Videos         int64    `json:"videos,omitempty"`
	Audios         int64    `json:"audios,omitempty"`
	Files          int64    `json:"files,omitempty"`
	Tools          int64    `json:"tools,omitempty"`
	ToolChoice     string   `json:"tool_choice,omitempty"`
	MaxTokens      *int64   `json:"max_tokens,omitempty"`
	Temperature    *float64 `json:"temperature,omitempty"`
	IncludeUsage   bool     `json:"include_usage,omitempty"`
}

type RequestTraceResponse struct {
	Error                 bool             `json:"error,omitempty"`
	ContentType           string           `json:"-"`
	ReasoningTokens       *int64           `json:"reasoning_tokens,omitempty"`
	ReasoningOutputTokens *int64           `json:"reasoning_output_tokens,omitempty"`
	ToolCalls             int64            `json:"tool_calls,omitempty"`
	ToolCallsByName       map[string]int64 `json:"tool_calls_by_name,omitempty"`
	Refused               bool             `json:"refused,omitempty"`
	Done                  bool             `json:"done"`
	EndMarker             string           `json:"end_marker,omitempty"`
	MetadataIncomplete    bool             `json:"metadata_incomplete,omitempty"`
	IncompleteReasons     []string         `json:"incomplete_reasons,omitempty"`
	EndMarkerPolicy       string           `json:"end_marker_policy,omitempty"`
}

type RequestTraceUpstream struct {
	Status             int    `json:"status,omitempty"`
	Protocol           string `json:"proto,omitempty"`
	RequestBytes       *int64 `json:"req_bytes,omitempty"`
	HeaderMilliseconds int64  `json:"header_ms"`
	Milliseconds       *int64 `json:"ms,omitempty"`
	RequestID          string `json:"request_id,omitempty"`
	ResponseID         string `json:"resp_id,omitempty"`
	Model              string `json:"resp_model,omitempty"`
	Object             string `json:"object,omitempty"`
	Created            *int64 `json:"created,omitempty"`
	Fingerprint        string `json:"fingerprint,omitempty"`
	ServiceTier        string `json:"service_tier,omitempty"`
	ErrorType          string `json:"error_type,omitempty"`
	ErrorCode          string `json:"error_code,omitempty"`
}

type RequestTraceClient struct {
	UserAgent        string `json:"ua,omitempty"`
	ContentType      string `json:"content_type,omitempty"`
	WireBytes        *int64 `json:"wire_bytes,omitempty"`
	AnthropicVersion string `json:"anthropic_version,omitempty"`
}

type RequestTraceAdmin struct {
	MetadataLevel         TraceMetadataLevel   `json:"metadata_level,omitempty"`
	ShapeIncomplete       bool                 `json:"shape_incomplete,omitempty"`
	ShapeIncompleteReason string               `json:"shape_incomplete_reason,omitempty"`
	ChannelID             int                  `json:"channel_id,omitempty"`
	Client                RequestTraceClient   `json:"client"`
	Shape                 *RequestTraceShape   `json:"shape,omitempty"`
	Upstream              RequestTraceUpstream `json:"upstream"`
	Response              RequestTraceResponse `json:"resp"`
}

type RequestTraceData struct {
	Version   int                 `json:"version,omitempty"`
	Trace     RequestTraceSummary `json:"trace"`
	AdminInfo *RequestTraceAdmin  `json:"admin_info,omitempty"`
}

// 摘要存储省略未采集字段；详情读取仍用 RequestTraceData 还原既有 API 结构。
func MarshalRequestTraceStorageData(data RequestTraceData) ([]byte, error) {
	admin := data.AdminInfo
	if admin == nil || admin.MetadataLevel != TraceMetadataSummary {
		return Marshal(data)
	}
	type summaryUpstream struct {
		Status             int    `json:"status,omitempty"`
		HeaderMilliseconds int64  `json:"header_ms,omitempty"`
		Milliseconds       *int64 `json:"ms,omitempty"`
		RequestID          string `json:"request_id,omitempty"`
		ErrorType          string `json:"error_type,omitempty"`
		ErrorCode          string `json:"error_code,omitempty"`
	}
	type summaryResponse struct {
		Error              bool     `json:"error,omitempty"`
		Done               bool     `json:"done,omitempty"`
		EndMarker          string   `json:"end_marker,omitempty"`
		MetadataIncomplete bool     `json:"metadata_incomplete,omitempty"`
		IncompleteReasons  []string `json:"incomplete_reasons,omitempty"`
		EndMarkerPolicy    string   `json:"end_marker_policy,omitempty"`
	}
	type summaryAdmin struct {
		MetadataLevel         TraceMetadataLevel `json:"metadata_level"`
		ChannelID             int                `json:"channel_id,omitempty"`
		Shape                 *RequestTraceShape `json:"shape,omitempty"`
		ShapeIncomplete       bool               `json:"shape_incomplete,omitempty"`
		ShapeIncompleteReason string             `json:"shape_incomplete_reason,omitempty"`
		Upstream              summaryUpstream    `json:"upstream"`
		Response              summaryResponse    `json:"resp"`
	}
	return Marshal(struct {
		Version   int                 `json:"version,omitempty"`
		Trace     RequestTraceSummary `json:"trace"`
		AdminInfo summaryAdmin        `json:"admin_info"`
	}{
		Version: data.Version, Trace: data.Trace,
		AdminInfo: summaryAdmin{
			MetadataLevel: admin.MetadataLevel, ChannelID: admin.ChannelID, Shape: admin.Shape,
			ShapeIncomplete: admin.ShapeIncomplete, ShapeIncompleteReason: admin.ShapeIncompleteReason,
			Upstream: summaryUpstream{Status: admin.Upstream.Status, HeaderMilliseconds: admin.Upstream.HeaderMilliseconds, Milliseconds: admin.Upstream.Milliseconds, RequestID: admin.Upstream.RequestID, ErrorType: admin.Upstream.ErrorType, ErrorCode: admin.Upstream.ErrorCode},
			Response: summaryResponse{Error: admin.Response.Error, Done: admin.Response.Done, EndMarker: admin.Response.EndMarker, MetadataIncomplete: admin.Response.MetadataIncomplete, IncompleteReasons: admin.Response.IncompleteReasons, EndMarkerPolicy: admin.Response.EndMarkerPolicy},
		},
	})
}

type RequestTraceAttempt struct {
	Attempt    int
	Started    time.Time
	Finished   time.Time
	ChannelID  int
	Failed     bool
	Diagnostic bool
	Upstream   RequestTraceUpstream
	Observer   *TraceBodyObserver
}

type RequestTraceCollector struct {
	sync.Mutex
	Config                RequestTraceConfig
	SampledSuccess        bool
	Started               time.Time
	Client                RequestTraceClient
	RequestBytes          *int64
	Shape                 *RequestTraceShape
	ShapeIncomplete       bool
	ShapeIncompleteReason string
	Stage                 string
	ErrorLogged           bool
	Failed                bool
	Attempts              []*RequestTraceAttempt
	ClientObserver        *TraceBodyObserver
}

func GetRequestTrace(c *gin.Context) *RequestTraceCollector {
	value, _ := c.Get(RequestTraceContextKey)
	trace, _ := value.(*RequestTraceCollector)
	return trace
}

// 仅由已确认原样转发响应的路径调用，协议名称相同不足以证明没有改写。
func ReuseUpstreamRequestTrace(c *gin.Context, response *http.Response) {
	trace := GetRequestTrace(c)
	if trace == nil || trace.ClientObserver == nil || response == nil {
		return
	}
	if body, ok := response.Body.(*TraceResponseBody); ok {
		trace.ClientObserver.ReuseUpstream(body.Observer)
	}
}

func (t *RequestTraceCollector) ResponseMetadataLevel(channelID int, status int) TraceMetadataLevel {
	if (t.Config.Level == "all" && t.SampledSuccess) || t.Config.DiagnoseChannel(channelID) || status >= 400 {
		return TraceMetadataFull
	}
	return TraceMetadataSummary
}

func SetRequestTraceStage(c *gin.Context, stage string) {
	if trace := GetRequestTrace(c); trace != nil {
		trace.Lock()
		trace.Stage = stage
		trace.Unlock()
	}
}

func (t *RequestTraceCollector) StartAttempt(attempt, channelID int, req *http.Request) *RequestTraceAttempt {
	t.Lock()
	defer t.Unlock()
	a := &RequestTraceAttempt{Attempt: attempt, ChannelID: channelID, Started: time.Now()}
	if count := len(t.Attempts); count > 0 && t.Attempts[count-1].Finished.IsZero() {
		t.Attempts[count-1].Finished = a.Started
	}
	a.Diagnostic = t.Config.DiagnoseChannel(channelID)
	if req.ContentLength >= 0 {
		size := req.ContentLength
		a.Upstream.RequestBytes = &size
	}
	t.Attempts = append(t.Attempts, a)
	t.Stage = "upstream"
	return a
}

func (t *RequestTraceCollector) FinishAttempt(attempt int) {
	t.Lock()
	defer t.Unlock()
	if count := len(t.Attempts); count > 0 && t.Attempts[count-1].Attempt == attempt && t.Attempts[count-1].Finished.IsZero() {
		t.Attempts[count-1].Finished = time.Now()
	}
}

func (t *RequestTraceCollector) CaptureRequest(storage BodyStorage, detailed bool) {
	size := storage.Size()
	t.RequestBytes = &size
	if !detailed || t.Shape != nil {
		return
	}
	if size > TraceMetadataMaxBytes {
		t.ShapeIncomplete = true
		t.ShapeIncompleteReason = "size_limit"
		return
	}
	reader, err := storage.NewReader()
	if err != nil {
		t.ShapeIncomplete = true
		t.ShapeIncompleteReason = "body_unavailable"
		return
	}
	defer reader.Close()
	shape, err := readRequestTraceShape(reader)
	if err != nil {
		t.ShapeIncomplete = true
		switch {
		case errors.Is(err, errTraceRequestTooLarge):
			t.ShapeIncompleteReason = "size_limit"
		case errors.Is(err, errTraceRequestInvalidJSON):
			t.ShapeIncompleteReason = "invalid_json"
		default:
			t.ShapeIncompleteReason = "body_unavailable"
		}
		return
	}
	t.Shape = shape
}

func TraceMetadataString(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, MaskCredentials(value))
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}
