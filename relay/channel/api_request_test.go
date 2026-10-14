package channel

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoRequestPropagatesTokenConcurrencyCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "waiting_for_headers"
		if stream {
			name = "streaming_body"
		}
		t.Run(name, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			parent, cancel := context.WithCancel(context.Background())
			lease, err := service.AcquireTokenConcurrency(parent, client, 42, 1)
			require.NoError(t, err)
			t.Cleanup(func() { _ = lease.Close() })
			entered, canceled := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: started\n\n")
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
				close(canceled)
			}))
			t.Cleanup(upstream.Close)
			t.Cleanup(cancel)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/relay", bytes.NewBufferString("{}")).WithContext(lease.Context())
			// Deliberately start with a detached request, as legacy adaptors do.
			req, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewBufferString("{}"))
			require.NoError(t, err)
			result := make(chan error, 1)
			go func() {
				resp, err := doRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
				if err == nil {
					_, err = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
				}
				result <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream request did not start")
			}
			cancel()
			select {
			case err := <-result:
				assert.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("upstream I/O did not stop")
			}
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream connection remained open")
			}
		})
	}
}

func TestDoRequestKeepsTokenConcurrencyLeaseAcrossAttempts(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	lease, err := service.AcquireTokenConcurrency(context.Background(), client, 42, 1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lease.Close() })
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/relay", bytes.NewBufferString("{}")).WithContext(lease.Context())
	for _, attempt := range []struct {
		path   string
		status int
	}{{"/first", 502}, {"/second", 200}} {
		req, err := http.NewRequest(http.MethodPost, upstream.URL+attempt.path, bytes.NewBufferString("{}"))
		require.NoError(t, err)
		resp, err := doRequest(c, req, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}})
		require.NoError(t, err)
		assert.Equal(t, attempt.status, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
		_, err = service.AcquireTokenConcurrency(context.Background(), client, 42, 1)
		assert.ErrorIs(t, err, service.ErrTokenConcurrencyExceeded, "intermediate attempts must not release the slot")
	}
	require.NoError(t, lease.Close())
	next, err := service.AcquireTokenConcurrency(context.Background(), client, 42, 1)
	require.NoError(t, err)
	require.NoError(t, next.Close())
}

func TestDoRequestCapturesPreferredUpstreamRequestID(t *testing.T) {
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name       string
		setPrimary bool
		primary    string
		fallback   string
		want       string
	}{
		{name: "fallback", fallback: "provider-request", want: "provider-request"},
		{name: "preferred", setPrimary: true, primary: "oneapi-request", fallback: "provider-request", want: "oneapi-request"},
		{name: "empty primary", setPrimary: true, fallback: "provider-request", want: "provider-request"},
		{name: "missing", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.setPrimary {
					w.Header().Set(common.RequestIdKey, test.primary)
				}
				if test.fallback != "" {
					w.Header().Set("X-Request-Id", test.fallback)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/relay", bytes.NewReader([]byte("request")))
			req, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewReader([]byte("request")))
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

			resp, err := doRequest(ctx, req, info)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, test.want, ctx.GetString(common.UpstreamRequestIdKey))
		})
	}
}

func TestCopiedUpstreamHeadersPreserveRequestIDPriority(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.True(t, service.ShouldCopyUpstreamHeader(ctx, "x-request-id", []string{"provider-request"}))
	require.Equal(t, "provider-request", ctx.GetString(common.UpstreamRequestIdKey))
	require.False(t, service.ShouldCopyUpstreamHeader(ctx, "x-oneapi-request-id", []string{"oneapi-request"}))
	require.Equal(t, "oneapi-request", ctx.GetString(common.UpstreamRequestIdKey))
	require.True(t, service.ShouldCopyUpstreamHeader(ctx, "X-REQUEST-ID", []string{"later-provider-request"}))
	require.Equal(t, "oneapi-request", ctx.GetString(common.UpstreamRequestIdKey))
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
