package moonshot

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKimiChatCacheForwardingAndSettlement(t *testing.T) {
	service.InitHttpClient()
	info := &relaycommon.RelayInfo{
		ChannelMeta:    &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "kimi-k3.1-pro"},
		RelayMode:      relayconstant.RelayModeChatCompletions,
		RelayFormat:    types.RelayFormatOpenAI,
		RequestURLPath: "/v1/chat/completions",
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	var request dto.GeneralOpenAIRequest
	err := common.UnmarshalJsonStr(`{"model":"kimi-k3.1-pro","messages":[{"role":"user","content":"Reply only OK."}],"prompt_cache_options":{"mode":"implicit","ttl":"1h"}}`, &request)
	require.NoError(t, err)
	adaptor := &openai.Adaptor{}
	converted, err := adaptor.ConvertOpenAIRequest(c, info, &request)
	require.NoError(t, err)
	wire, err := common.Marshal(converted)
	require.NoError(t, err)

	const expr = `tier("kimi", p*13 + c*68 + cr*1.3 + cc*13 + cc1h*26)`
	for _, tc := range []struct {
		name, five, hour                    string
		writes, hits, want5m, want1h, quota int
	}{
		{"cold 1h", "0", "1000", 1000, 0, 0, 1000, 26266},
		{"warm hit", "0", "0", 0, 1000, 0, 0, 1566},
		{"mixed TTL", "700", "300", 1000, 0, 700, 300, 17166},
		{"5m despite requested 1h", "1000", "0", 1000, 0, 1000, 0, 13266},
		{"missing headers", "", "", 1000, 0, 1000, 0, 13266},
		{"inconsistent total", "700", "700", 1000, 0, 1000, 0, 13266},
		{"invalid header", "-1", "1001", 1000, 0, 1000, 0, 13266},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = c.Request.Clone(c.Request.Context())
			body := fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":1010,"completion_tokens":2,"total_tokens":1012,"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}}}`, tc.hits, tc.writes)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/v1/chat/completions", r.URL.Path)
				var forwarded struct {
					Options map[string]string `json:"prompt_cache_options"`
				}
				if assert.NoError(t, common.DecodeJson(r.Body, &forwarded)) {
					assert.Equal(t, map[string]string{"mode": "implicit", "ttl": "1h"}, forwarded.Options)
				}
				if tc.five != "" {
					w.Header().Set("Msh-Usage-Cache-Write-Tokens-5m", tc.five)
				}
				if tc.hour != "" {
					w.Header().Set("Msh-Usage-Cache-Write-Tokens-1h", tc.hour)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			info.ChannelBaseUrl = server.URL
			response, err := adaptor.DoRequest(ctx, info, bytes.NewReader(wire))
			require.NoError(t, err)
			result, apiErr := adaptor.DoResponse(ctx, response.(*http.Response), info)
			require.Nil(t, apiErr)
			u := result.(*dto.Usage)
			params := service.BuildTieredTokenParams(u, false, map[string]bool{"cr": true, "cc": true, "cc1h": true})
			assert.Equal(t, float64(tc.want5m), params.CC)
			assert.Equal(t, float64(tc.want1h), params.CC1h)
			assert.Equal(t, float64(tc.hits), params.CR)
			assert.Equal(t, 10.0, params.P)
			info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: expr, ExprHash: billingexpr.ExprHashString(expr), GroupRatio: 1, QuotaPerUnit: 1000000}
			applied, quota, detail := service.TryTieredSettle(info, params)
			require.True(t, applied)
			require.NotNil(t, detail)
			assert.Equal(t, tc.quota, quota)
		})
	}
}

func TestKimiResponsesCacheUsesNativeBillingSnapshot(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "kimi-k3.1-pro"},
		RelayFormat: types.RelayFormatOpenAIResponses, RelayMode: relayconstant.RelayModeResponses,
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Msh-Usage-Cache-Write-Tokens-5m": {"0"}, "Msh-Usage-Cache-Write-Tokens-1h": {"1000"}},
		Body: io.NopCloser(strings.NewReader(`{"status":"completed","usage":{
			"input_tokens":1010,"output_tokens":2,"input_tokens_details":{"cache_write_tokens":1000},
			"billing_usage":{"source":"oai_responses","openai_usage":{"input_tokens":1010,"output_tokens":2,"input_tokens_details":{"cache_write_tokens":1000}}}
		}}`)),
	}
	result, apiErr := (&openai.Adaptor{}).DoResponse(c, resp, info)
	require.Nil(t, apiErr)
	usage := result.(*dto.Usage)
	require.NotNil(t, usage.KimiCacheWriteBreakdown())
	u, ok := usage.BillingUsage.CanonicalUsage()
	require.True(t, ok)
	params := service.BuildTieredTokenParams(u, false, map[string]bool{"cc": true, "cc1h": true})
	assert.Equal(t, 1000.0, params.CC1h)
	assert.Zero(t, params.CC)
	assert.Equal(t, 10.0, params.P)
	// When the price expression omits cc1h, these tokens remain ordinary input.
	params = service.BuildTieredTokenParams(u, false, map[string]bool{"cc": true})
	assert.Equal(t, 1010.0, params.P)
}

func TestStreamResponsePreservesCacheWriteHeaders(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})

	for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude} {
		for _, tc := range []struct {
			name       string
			write5m    int
			write1h    int
			omitHeader bool
		}{
			{name: "1h", write1h: 5120},
			{name: "5m", write5m: 5120},
			{name: "absent", write1h: 5120, omitHeader: true},
		} {
			t.Run(string(format)+"/"+tc.name, func(t *testing.T) {
				path := "/v1/chat/completions"
				usageJSON := `{"prompt_tokens":5319,"completion_tokens":40,"total_tokens":5359,"prompt_tokens_details":{"cache_write_tokens":5120}}`
				body := "data: " + `{"id":"chatcmpl-cache-test","object":"chat.completion.chunk","model":"kimi-k3.1-pro","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":"stop"}]}` + "\n\n" +
					"data: " + `{"id":"chatcmpl-cache-test","object":"chat.completion.chunk","model":"kimi-k3.1-pro","choices":[],"usage":` + usageJSON + "}\n\ndata: [DONE]\n\n"
				if format == types.RelayFormatClaude {
					path = "/v1/messages"
					usageJSON = fmt.Sprintf(`{"input_tokens":199,"output_tokens":40,"cache_creation_input_tokens":5120,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}`, tc.write5m, tc.write1h)
					body = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg-cache-test","type":"message","role":"assistant","model":"kimi-k3.1-pro","content":[],"usage":` + usageJSON + "}}\n\n" +
						"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + usageJSON + "}\n\n" +
						"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, path, nil)
				resp := &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
				}
				resp.Header.Set("Content-Type", "text/event-stream")
				resp.Header.Set("X-Reasoning-Included", "true")
				resp.Header.Set("X-Codex-Turn-State", "test-state")
				resp.Header.Set("Msh-Org-Id", "upstream-only")
				if !tc.omitHeader {
					resp.Header.Set("msh-usage-cache-write-tokens-5m", fmt.Sprint(tc.write5m))
					resp.Header.Set("msh-usage-cache-write-tokens-1h", fmt.Sprint(tc.write1h))
				}
				info := &relaycommon.RelayInfo{
					ChannelMeta:        &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeMoonshot, UpstreamModelName: "kimi-k3.1-pro"},
					IsStream:           true,
					RelayMode:          relayconstant.RelayModeChatCompletions,
					RelayFormat:        format,
					ShouldIncludeUsage: true,
					DisablePing:        true,
				}

				result, apiErr := (&Adaptor{}).DoResponse(c, resp, info)
				require.Nil(t, apiErr)
				usage, ok := result.(*dto.Usage)
				require.True(t, ok)
				require.NotNil(t, usage)
				assert.Equal(t, 5120, usage.PromptTokensDetails.CacheCreationTokensTotal())
				assert.Equal(t, 40, usage.CompletionTokens)
				if format == types.RelayFormatOpenAI && !tc.omitHeader {
					split := usage.KimiCacheWriteBreakdown()
					require.NotNil(t, split)
					assert.Equal(t, tc.write1h, split.OneHourTokens)
				}
				assert.Contains(t, recorder.Body.String(), usageJSON, "上游 usage 应保持不变")

				// Result 获取首次写入时提交的响应头，与客户端实际收到的内容一致。
				downstream := recorder.Result()
				defer downstream.Body.Close()
				assert.Equal(t, http.StatusOK, downstream.StatusCode)
				assert.Equal(t, "text/event-stream", downstream.Header.Get("Content-Type"))
				assert.Equal(t, "true", downstream.Header.Get("X-Reasoning-Included"))
				assert.Equal(t, "test-state", downstream.Header.Get("X-Codex-Turn-State"))
				assert.Empty(t, downstream.Header.Get("Msh-Org-Id"))
				for _, name := range []string{"Msh-Usage-Cache-Write-Tokens-5m", "Msh-Usage-Cache-Write-Tokens-1h"} {
					assert.Equal(t, resp.Header.Values(name), downstream.Header.Values(name), name)
				}
			})
		}
	}
}

func TestConvertOpenAIRequestKimiK26UsesOnlyAllowedTemperature(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:       "kimi-k2.6",
		Temperature: common.GetPointer[float64](0.7),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kimi-k2.6",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, convertedRequest.Temperature)
	require.Equal(t, 1.0, *convertedRequest.Temperature)
}

func TestConvertOpenAIRequestKimiK26KeepsOmittedTemperatureOmitted(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model: "kimi-k2.6",
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kimi-k2.6",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.Nil(t, convertedRequest.Temperature)
}

func TestConvertOpenAIRequestOtherMoonshotModelKeepsTemperature(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{
		Model:       "kimi-k2.5",
		Temperature: common.GetPointer[float64](0.7),
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "kimi-k2.5",
		},
	}

	converted, err := (&Adaptor{}).ConvertOpenAIRequest(nil, info, request)

	require.NoError(t, err)
	convertedRequest, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, convertedRequest.Temperature)
	require.Equal(t, 0.7, *convertedRequest.Temperature)
}
