package common

import (
	"errors"
	"net/http"
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
}

var requestTraceConfig = struct {
	sync.RWMutex
	value RequestTraceConfig
}{value: RequestTraceConfig{Level: "errors", PreChannelErrors: true, CompressionLevel: "fast"}}

func GetRequestTraceConfig() RequestTraceConfig {
	requestTraceConfig.RLock()
	defer requestTraceConfig.RUnlock()
	config := requestTraceConfig.value
	config.UpstreamRequestIDHeaders = append([]string(nil), config.UpstreamRequestIDHeaders...)
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
	config := RequestTraceConfig{CompressionLevel: "fast"}
	if err := UnmarshalJsonStr(value, &config); err != nil {
		return config, err
	}
	if config.Level != "off" && config.Level != "errors" && config.Level != "all" {
		return config, errors.New("invalid request trace level")
	}
	if _, ok := requestTraceStorageEncoders[config.CompressionLevel]; !ok {
		return config, errors.New("invalid request trace compression level")
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
	ResponseBytes   int64   `json:"resp_bytes"`
	Status          int     `json:"status"`
	FinishReason    *string `json:"finish_reason,omitempty"`
	Stage           string  `json:"stage,omitempty"`
	ClientErrorType string  `json:"client_error_type,omitempty"`
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
	Keys           []string `json:"keys,omitempty"`
}

type RequestTraceResponse struct {
	Error                 bool                `json:"error,omitempty"`
	ContentType           string              `json:"content_type,omitempty"`
	ContentCharacters     int64               `json:"content_chars"`
	ReasoningCharacters   int64               `json:"reasoning_chars"`
	ReasoningTokens       *int64              `json:"reasoning_tokens,omitempty"`
	ReasoningOutputTokens *int64              `json:"reasoning_output_tokens,omitempty"`
	ToolCalls             int64               `json:"tool_calls,omitempty"`
	ToolCallsByName       map[string]int64    `json:"tool_calls_by_name,omitempty"`
	Refused               bool                `json:"refused,omitempty"`
	Chunks                int64               `json:"chunks,omitempty"`
	LastChunkMilliseconds int64               `json:"last_chunk_ms,omitempty"`
	Done                  bool                `json:"done"`
	EndMarker             string              `json:"end_marker,omitempty"`
	UsageChunk            bool                `json:"usage_chunk"`
	Keys                  map[string][]string `json:"keys,omitempty"`
	MetadataIncomplete    bool                `json:"metadata_incomplete,omitempty"`
}

type RequestTraceUpstream struct {
	Status             int    `json:"status,omitempty"`
	Protocol           string `json:"proto,omitempty"`
	RequestBytes       *int64 `json:"req_bytes,omitempty"`
	HeaderMilliseconds int64  `json:"header_ms"`
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
	ShapeIncomplete bool                 `json:"shape_incomplete,omitempty"`
	ChannelID       int                  `json:"channel_id,omitempty"`
	Client          RequestTraceClient   `json:"client"`
	Shape           *RequestTraceShape   `json:"shape,omitempty"`
	Upstream        RequestTraceUpstream `json:"upstream"`
	Response        RequestTraceResponse `json:"resp"`
}

type RequestTraceData struct {
	Trace     RequestTraceSummary `json:"trace"`
	AdminInfo *RequestTraceAdmin  `json:"admin_info,omitempty"`
}

type RequestTraceAttempt struct {
	Attempt   int
	Started   time.Time
	ChannelID int
	Failed    bool
	Upstream  RequestTraceUpstream
	Observer  *TraceBodyObserver
}

type RequestTraceCollector struct {
	sync.Mutex
	Config          RequestTraceConfig
	Started         time.Time
	Client          RequestTraceClient
	RequestBytes    *int64
	Shape           *RequestTraceShape
	ShapeIncomplete bool
	Stage           string
	ErrorLogged     bool
	Failed          bool
	Attempts        []*RequestTraceAttempt
	ClientObserver  *TraceBodyObserver
}

func GetRequestTrace(c *gin.Context) *RequestTraceCollector {
	value, _ := c.Get(RequestTraceContextKey)
	trace, _ := value.(*RequestTraceCollector)
	return trace
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
	if req.ContentLength >= 0 {
		size := req.ContentLength
		a.Upstream.RequestBytes = &size
	}
	t.Attempts = append(t.Attempts, a)
	t.Stage = "upstream"
	return a
}

func (t *RequestTraceCollector) CaptureRequest(storage BodyStorage, detailed bool) {
	size := storage.Size()
	t.RequestBytes = &size
	if !detailed {
		return
	}
	if size > TraceMetadataMaxBytes {
		t.ShapeIncomplete = true
		return
	}
	reader, err := storage.NewReader()
	if err != nil {
		t.ShapeIncomplete = true
		return
	}
	defer reader.Close()
	shape, err := readRequestTraceShape(reader)
	if err != nil {
		t.ShapeIncomplete = true
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
