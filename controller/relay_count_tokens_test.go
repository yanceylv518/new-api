package controller

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type imageFetchTransport func(*http.Request) (*http.Response, error)

func (f imageFetchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func mockImageDownload(t *testing.T, transport imageFetchTransport) {
	t.Helper()
	fetch := system_setting.GetFetchSetting()
	originalFetch := *fetch
	*fetch = system_setting.FetchSetting{EnableSSRFProtection: true, AllowedPorts: []string{"443"}}
	if service.GetSSRFProtectedHTTPClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetSSRFProtectedHTTPClient()
	originalTransport := client.Transport
	client.Transport = transport
	t.Cleanup(func() {
		client.Transport = originalTransport
		*fetch = originalFetch
	})
}

func TestRelayRejectsInvalidImageURLWhenFetchingMedia(t *testing.T) {
	count, media, nonStream := constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream
	mockImageDownload(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/missing":
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
		case "/unavailable":
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
		case "/dns":
			return nil, &net.DNSError{Name: req.URL.Hostname(), IsNotFound: true, Err: "no such host"}
		case "/proxy":
			return nil, &net.DNSError{Name: "proxy.invalid", IsNotFound: true, Err: "no such host"}
		case "/timeout":
			return nil, &net.DNSError{Name: req.URL.Hostname(), IsTimeout: true, Err: "timeout"}
		default:
			t.Fatalf("unexpected image download")
			return nil, nil
		}
	})
	constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream = true, true, true
	t.Cleanup(func() {
		constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream = count, media, nonStream
	})

	for _, tc := range []struct {
		path   string
		format types.RelayFormat
		input  string
		field  string
	}{
		{"/v1/chat/completions", types.RelayFormatOpenAI, `"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%s}}]}]`, "image_url.url"},
		{"/v1/messages", types.RelayFormatClaude, `"max_tokens":32,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":%s}}]}]`, "image URL"},
		{"/v1/responses", types.RelayFormatOpenAIResponses, `"input":[{"role":"user","content":[{"type":"input_image","image_url":%s}]}]`, "image_url"},
	} {
		for _, urlCase := range []struct {
			name    string
			jsonURL string
			reason  string
			status  int
		}{
			{"newline", `"https://example.com/image?signature=private\nextra"`, "invalid control character", 400},
			{"nul", `"https://example.com/image?signature=private\u0000extra"`, "invalid control character", 400},
			{"bad_port", `"https://example.com:99999/image?signature=private"`, "invalid port", 400},
			{"port_not_allowed", `"https://example.invalid:65535/image"`, "port 65535 is not allowed", 500},
			{"not_found", `"https://example.invalid/missing?signature=private"`, "returned HTTP 404", 400},
			{"private_or_reserved_ip", `"https://192.0.2.1/image?signature=private"`, "not allowed", 400},
			{"no_such_host", `"https://example.invalid/dns?signature=private"`, "host could not be found", 400},
			{"dns_timeout", `"https://example.invalid/timeout"`, "timeout", 500},
			{"proxy_dns", `"https://example.invalid/proxy"`, "no such host", 500},
			{"image_server_unavailable", `"https://example.invalid/unavailable"`, "status code: 503", 500},
		} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.path, urlCase.name, stream), func(t *testing.T) {
					body := fmt.Sprintf(`{"model":"test","stream":%t,%s}`, stream, fmt.Sprintf(tc.input, urlCase.jsonURL))
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
					ctx.Request.Header.Set("Content-Type", "application/json")

					Relay(ctx, tc.format)

					require.Equal(t, urlCase.status, recorder.Code)
					var response struct {
						Error struct {
							Code    string `json:"code"`
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}
					require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
					assert.Contains(t, response.Error.Message, urlCase.reason)
					if urlCase.status != http.StatusBadRequest {
						return
					}
					if tc.format == types.RelayFormatClaude {
						assert.Equal(t, "invalid_request_error", response.Error.Type)
					} else {
						assert.Equal(t, "invalid_request", response.Error.Code)
					}
					assert.Contains(t, response.Error.Message, tc.field)
					assert.NotContains(t, recorder.Body.String(), "signature=private")
					assert.NotContains(t, recorder.Body.String(), "192.0.2.1")
				})
			}
		}
	}
}

func TestImageTokenEstimationFetchCount(t *testing.T) {
	media, nonStream := constant.GetMediaToken, constant.GetMediaTokenNotStream
	maxSize := constant.MaxFileDownloadMB
	constant.GetMediaToken, constant.GetMediaTokenNotStream = true, false
	constant.MaxFileDownloadMB = 1
	downloads := 0
	mockImageDownload(t, func(req *http.Request) (*http.Response, error) {
		downloads++
		const gif1x1 = "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw=="
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"image/gif"}},
			Body:       io.NopCloser(base64.NewDecoder(base64.StdEncoding, strings.NewReader(gif1x1))),
		}, nil
	})
	t.Cleanup(func() {
		constant.MaxFileDownloadMB = maxSize
		constant.GetMediaToken, constant.GetMediaTokenNotStream = media, nonStream
	})
	for _, tc := range []struct {
		info relaycommon.RelayInfo
		want int
	}{
		{relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions}, 0},
		{relaycommon.RelayInfo{RelayFormat: types.RelayFormatClaude}, 0},
		{relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAIResponses, RelayMode: relayconstant.RelayModeResponses}, 0},
		{relaycommon.RelayInfo{RelayFormat: types.RelayFormatGemini, IsStream: true}, 0},
		{relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions, IsStream: true}, 1},
	} {
		t.Run(fmt.Sprintf("%s/stream=%t", tc.info.RelayFormat, tc.info.IsStream), func(t *testing.T) {
			downloads = 0
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			imageURL := "https://example.invalid/image.gif"
			if tc.want == 0 {
				imageURL += "\nextra text"
			}
			source := types.NewURLFileSource(imageURL)
			meta := &types.TokenCountMeta{Files: []*types.FileMeta{types.NewImageFileMeta(source, "auto")}}
			_, err := service.CountRequestToken(ctx, meta, &tc.info)
			require.NoError(t, err)
			assert.Equal(t, tc.want, downloads)
			assert.Equal(t, tc.want > 0, source.HasCache())
		})
	}
}

func TestCountClaudeTokensReturnsInputTokensWhenRelayCountingDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalCountToken := constant.CountToken
	constant.CountToken = false
	t.Cleanup(func() {
		constant.CountToken = originalCountToken
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/messages/count_tokens?beta=true",
		strings.NewReader(`{
			"model":"gemini-3.6-flash",
			"messages":[{"role":"user","content":"count this prompt"}],
			"tools":[{"name":"lookup","description":"Look up a value","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}]
		}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(ctx, constant.ContextKeyOriginalModel, "gemini-3.6-flash")

	CountClaudeTokens(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		InputTokens int `json:"input_tokens"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Positive(t, response.InputTokens)
}

func TestCountClaudeTokensRejectsMissingMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/messages/count_tokens",
		strings.NewReader(`{"model":"gemini-3.6-flash"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	CountClaudeTokens(ctx)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	var response struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, "error", response.Type)
	assert.Equal(t, "invalid_request_error", response.Error.Type)
	assert.Contains(t, response.Error.Message, "messages")
}
