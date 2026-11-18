package channel

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/requesttimeline"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoRequestCapturesPreferredUpstreamRequestID(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	require.NoError(t, requesttimeline.ConfigureRequestIDHeaders("x-maas-requestid, X-Custom-Trace"))
	t.Cleanup(func() {
		require.NoError(t, requesttimeline.ConfigureRequestIDHeaders(requesttimeline.DefaultRequestIDHeaders))
	})
	for _, test := range []struct {
		name         string
		headers      map[string]string
		want         string
		wantTimeline string
	}{
		{name: "fallback", headers: map[string]string{"X-Request-Id": "provider-request", "Request-Id": "legacy"}, want: "provider-request"},
		{name: "preferred", headers: map[string]string{common.RequestIdKey: "oneapi-request", "X-Request-Id": "provider-request"}, want: "oneapi-request"},
		{name: "empty primary", headers: map[string]string{common.RequestIdKey: "", "X-Request-Id": "provider-request"}, want: "provider-request"},
		{name: "legacy", headers: map[string]string{"Request-Id": "legacy", "x-maas-requestid": "maas"}, want: "legacy"},
		{name: "maas", headers: map[string]string{"x-MaAs-ReQuEsTiD": "maas", "X-Custom-Trace": "custom"}, want: "maas"},
		{name: "configured", headers: map[string]string{"X-Custom-Trace": "custom"}, want: "custom"},
		{name: "128 characters", headers: map[string]string{"X-Custom-Trace": strings.Repeat("x", 128)}, want: strings.Repeat("x", 128)},
		{name: "long configured", headers: map[string]string{"X-Custom-Trace": strings.Repeat("x", 129)}, want: strings.Repeat("x", 128), wantTimeline: strings.Repeat("x", 129)},
		{name: "long primary", headers: map[string]string{common.RequestIdKey: strings.Repeat("p", 129), "X-Request-Id": "fallback"}, want: strings.Repeat("p", 128), wantTimeline: strings.Repeat("p", 129)},
		{name: "UTF-8 boundary", headers: map[string]string{"X-Custom-Trace": strings.Repeat("x", 127) + "界尾"}, want: strings.Repeat("x", 127) + "界", wantTimeline: strings.Repeat("x", 127) + "界尾"},
		{name: "missing", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, status := range []int{http.StatusOK, http.StatusBadGateway} {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					for key, value := range test.headers {
						w.Header()[key] = []string{value}
					}
					w.WriteHeader(status)
				}))
				defer upstream.Close()

				for _, timelineEnabled := range []bool{false, true} {
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx.Set(common.UpstreamRequestIdKey, "previous-attempt")
					ctx.Request = httptest.NewRequest(http.MethodPost, "/relay", bytes.NewReader([]byte("request")))
					req, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewReader([]byte("request")))
					require.NoError(t, err)
					info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}
					if timelineEnabled {
						info.Timeline = requesttimeline.New(time.Now(), "local-request", 1, "model")
					}

					resp, err := doRequest(ctx, req, info)
					require.NoError(t, err)
					defer resp.Body.Close()
					assert.Equal(t, test.want, ctx.GetString(common.UpstreamRequestIdKey), "status=%d timeline=%t", status, timelineEnabled)
					// Copying response headers must not restore an unbounded log ID.
					for key, values := range resp.Header {
						service.ShouldCopyUpstreamHeader(ctx, key, values)
					}
					assert.Equal(t, test.want, ctx.GetString(common.UpstreamRequestIdKey))
					for key, value := range test.headers {
						assert.Equal(t, value, resp.Header.Get(key), "forwarded headers must stay unchanged")
					}
					if timelineEnabled {
						wantTimeline := test.wantTimeline
						if wantTimeline == "" {
							wantTimeline = test.want
						}
						assert.Equal(t, wantTimeline, info.Timeline.Freeze(nil).Data.UpstreamRequestID)
					}
				}
			}
		})
	}
}

func TestCopiedUpstreamHeadersPreserveRequestIDPriority(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, service.ShouldCopyUpstreamHeader(ctx, "x-request-id", []string{strings.Repeat("f", 129)}))
	assert.Equal(t, strings.Repeat("f", 128), ctx.GetString(common.UpstreamRequestIdKey))
	require.False(t, service.ShouldCopyUpstreamHeader(ctx, "x-oneapi-request-id", []string{strings.Repeat("p", 129)}))
	assert.Equal(t, strings.Repeat("p", 128), ctx.GetString(common.UpstreamRequestIdKey))
	require.True(t, service.ShouldCopyUpstreamHeader(ctx, "X-REQUEST-ID", []string{"later-provider-request"}))
	assert.Equal(t, strings.Repeat("p", 128), ctx.GetString(common.UpstreamRequestIdKey))
}

func TestDoRequestKeepsRequestIDConfigurationSnapshot(t *testing.T) {
	service.InitHttpClient()
	require.NoError(t, requesttimeline.ConfigureRequestIDHeaders("X-Original-Trace"))
	t.Cleanup(func() {
		require.NoError(t, requesttimeline.ConfigureRequestIDHeaders(requesttimeline.DefaultRequestIDHeaders))
	})
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{},
		Timeline:    requesttimeline.New(time.Now(), "local-request", 1, "model"),
	}
	require.NoError(t, requesttimeline.ConfigureRequestIDHeaders("X-New-Trace"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Original-Trace", "original-id")
		w.Header().Set("X-New-Trace", "new-id")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/relay", bytes.NewBufferString("request"))
	req, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewBufferString("request"))
	require.NoError(t, err)
	resp, err := doRequest(c, req, info)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, "original-id", c.GetString(common.UpstreamRequestIdKey))
	assert.Equal(t, "original-id", info.Timeline.Freeze(nil).Data.UpstreamRequestID)

	// A failed next attempt must clear the prior ID even without response headers.
	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	req, err = http.NewRequestWithContext(requestContext, http.MethodPost, upstream.URL, bytes.NewBufferString("request"))
	require.NoError(t, err)
	_, err = doRequest(c, req, info)
	require.Error(t, err)
	assert.Empty(t, c.GetString(common.UpstreamRequestIdKey))
}

func TestDoRequestClearsRequestIDBeforeZhipuPreflight(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(common.UpstreamRequestIdKey, "previous-attempt")
	req, err := http.NewRequest(http.MethodPost, "ftp://provider.example/search", bytes.NewBufferString("request"))
	require.NoError(t, err)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{},
		RelayMode:   relayconstant.RelayModeZhipuWebSearch,
	}
	resp, err := doRequest(c, req, info)
	require.ErrorContains(t, err, "upstream URL must use http or https")
	assert.Nil(t, resp)
	assert.Empty(t, c.GetString(common.UpstreamRequestIdKey))
	assert.False(t, info.ZhipuWebSearchAttempted, "local validation must not prevent channel fallback")
}

func TestNewTaskAPIRequestInheritsClientCancellation(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)

	upstream, err := newTaskAPIRequest(c, "https://provider.example/tasks", nil)
	require.NoError(t, err)
	cancel()

	require.ErrorIs(t, upstream.Context().Err(), context.Canceled)
}

func TestProcessHeaderOverride_ChannelTestSkipsPassthroughRules(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Empty(t, headers)
}

func TestProcessHeaderOverride_ChannelTestSkipsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: true,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	_, ok := headers["x-upstream-trace"]
	require.False(t, ok)
}

func TestProcessHeaderOverride_NonTestKeepsClientHeaderPlaceholder(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Upstream-Trace": "{client_header:X-Trace-Id}",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-upstream-trace"])
}

func TestProcessHeaderOverride_RuntimeOverrideIsFinalHeaderMap(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		IsChannelTest:             false,
		UseRuntimeHeadersOverride: true,
		RuntimeHeadersOverride: map[string]any{
			"x-static":  "runtime-value",
			"x-runtime": "runtime-only",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
				"X-Legacy": "legacy-only",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "runtime-value", headers["x-static"])
	require.Equal(t, "runtime-only", headers["x-runtime"])
	_, exists := headers["x-legacy"]
	require.False(t, exists)
}

func TestProcessHeaderOverride_PassthroughSkipsAcceptEncoding(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Trace-Id", "trace-123")
	ctx.Request.Header.Set("Accept-Encoding", "gzip")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		ChannelMeta: &relaycommon.ChannelMeta{
			HeadersOverride: map[string]any{
				"*": "",
			},
		},
	}

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "trace-123", headers["x-trace-id"])

	_, hasAcceptEncoding := headers["accept-encoding"]
	require.False(t, hasAcceptEncoding)
}

func TestProcessHeaderOverride_PassHeadersTemplateSetsRuntimeHeaders(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Originator", "Codex CLI")
	ctx.Request.Header.Set("Session_id", "sess-123")

	info := &relaycommon.RelayInfo{
		IsChannelTest: false,
		RequestHeaders: map[string]string{
			"Originator": "Codex CLI",
			"Session_id": "sess-123",
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ParamOverride: map[string]any{
				"operations": []any{
					map[string]any{
						"mode":  "pass_headers",
						"value": []any{"Originator", "Session_id", "X-Codex-Beta-Features"},
					},
				},
			},
			HeadersOverride: map[string]any{
				"X-Static": "legacy-value",
			},
		},
	}

	_, err := relaycommon.ApplyParamOverrideWithRelayInfo([]byte(`{"model":"gpt-4.1"}`), info)
	require.NoError(t, err)
	require.True(t, info.UseRuntimeHeadersOverride)
	require.Equal(t, "Codex CLI", info.RuntimeHeadersOverride["originator"])
	require.Equal(t, "sess-123", info.RuntimeHeadersOverride["session_id"])
	_, exists := info.RuntimeHeadersOverride["x-codex-beta-features"]
	require.False(t, exists)
	require.Equal(t, "legacy-value", info.RuntimeHeadersOverride["x-static"])

	headers, err := processHeaderOverride(info, ctx)
	require.NoError(t, err)
	require.Equal(t, "Codex CLI", headers["originator"])
	require.Equal(t, "sess-123", headers["session_id"])
	_, exists = headers["x-codex-beta-features"]
	require.False(t, exists)

	upstreamReq := httptest.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	applyHeaderOverrideToRequest(upstreamReq, headers)
	require.Equal(t, "Codex CLI", upstreamReq.Header.Get("Originator"))
	require.Equal(t, "sess-123", upstreamReq.Header.Get("Session_id"))
	require.Empty(t, upstreamReq.Header.Get("X-Codex-Beta-Features"))
}
