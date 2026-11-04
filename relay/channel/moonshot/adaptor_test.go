package moonshot

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
