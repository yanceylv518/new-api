package openai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOaiStreamHandlerFinalChoicesAndUsage(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})

	const usageJSON = `{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22,"prompt_tokens_details":{"cached_tokens":7},"completion_tokens_details":{"reasoning_tokens":1}}`
	const stopChoices = `[{"index":0,"delta":{},"finish_reason":"stop"}]`
	for _, tc := range []struct {
		name           string
		choices        string
		finishCount    int
		separateUsage  bool
		forceFormat    bool
		thinkToContent bool
	}{
		{name: "stop", choices: stopChoices, finishCount: 1},
		{name: "content_and_stop", choices: `[{"index":0,"delta":{"content":"!"},"finish_reason":"stop"}]`, finishCount: 1},
		{name: "tool_calls", choices: `[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Shanghai\"}"}}]},"finish_reason":"tool_calls"}]`, finishCount: 1},
		{name: "tool_calls_empty_delta", choices: `[{"index":0,"delta":{},"finish_reason":"tool_calls"}]`, finishCount: 1},
		{name: "length", choices: `[{"index":0,"delta":{},"finish_reason":"length"}]`, finishCount: 1},
		{name: "content_filter", choices: `[{"index":0,"delta":{},"finish_reason":"content_filter"}]`, finishCount: 1},
		{name: "refusal_extension", choices: `[{"index":0,"delta":{"refusal":"Cannot comply"},"finish_reason":"stop"}]`, finishCount: 1},
		{name: "multiple_choices", choices: `[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"length"}]`, finishCount: 2},
		{name: "separate_usage", choices: `[]`, finishCount: 1, separateUsage: true},
		{name: "force_format", choices: stopChoices, finishCount: 1, forceFormat: true},
		{name: "thinking_to_content", choices: stopChoices, finishCount: 1, thinkToContent: true},
	} {
		for _, includeUsage := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/include_usage=%t", tc.name, includeUsage), func(t *testing.T) {
				firstChoices := `[{"index":0,"delta":{"role":"assistant","content":"hello"}}]`
				if tc.separateUsage {
					firstChoices = `[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]`
				}
				if tc.thinkToContent {
					firstChoices = `[{"index":0,"delta":{"role":"assistant","reasoning_content":"thinking"}}]`
				}
				first := fmt.Sprintf(`{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1710000000,"model":"glm-5.3","choices":%s}`, firstChoices)
				last := fmt.Sprintf(`{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1710000000,"model":"glm-5.3","choices":%s,"usage":%s,"obfuscation":"upstream-extension"}`, tc.choices, usageJSON)
				body := "data: " + first + "\n\ndata: " + last + "\n\ndata: [DONE]\n\n"
				c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
				info.RelayMode = relayconstant.RelayModeChatCompletions
				info.ShouldIncludeUsage = includeUsage
				info.ChannelSetting.ForceFormat = tc.forceFormat
				info.ChannelSetting.ThinkingToContent = tc.thinkToContent
				info.ThinkingContentInfo = relaycommon.ThinkingContentInfo{IsFirstThinkingContent: true}

				usage, apiErr := OaiStreamHandler(c, info, resp)
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, 20, usage.PromptTokens)
				assert.Equal(t, 2, usage.CompletionTokens)
				assert.Equal(t, 22, usage.TotalTokens)
				assert.Equal(t, 7, usage.PromptTokensDetails.CachedTokens)
				assert.Equal(t, 1, usage.CompletionTokenDetails.ReasoningTokens)

				var frames []string
				finishCount, doneCount := 0, 0
				for _, line := range strings.Split(recorder.Body.String(), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					payload := strings.TrimPrefix(line, "data: ")
					if payload == "[DONE]" {
						doneCount++
						continue
					}
					require.Zero(t, doneCount, "no chunks may follow [DONE]")
					require.True(t, gjson.Valid(payload))
					frames = append(frames, payload)
					for _, choice := range gjson.Get(payload, "choices").Array() {
						if choice.Get("finish_reason").String() != "" {
							finishCount++
						}
					}
				}
				assert.Equal(t, 1, doneCount)
				assert.Equal(t, tc.finishCount, finishCount, recorder.Body.String())
				wantFrames := 2
				if tc.separateUsage && !includeUsage {
					wantFrames = 1
				}
				require.Len(t, frames, wantFrames)
				if tc.separateUsage && !includeUsage {
					assert.False(t, gjson.Get(frames[0], "usage").Exists())
					return
				}
				final := frames[len(frames)-1]
				if includeUsage {
					assert.Equal(t, int64(22), gjson.Get(final, "usage.total_tokens").Int())
				} else if tc.forceFormat || tc.thinkToContent {
					// The existing formatter serializes absent usage as null.
					assert.Equal(t, "null", gjson.Get(final, "usage").Raw)
				} else {
					assert.False(t, gjson.Get(final, "usage").Exists())
				}
				if !tc.forceFormat && !tc.thinkToContent {
					assert.JSONEq(t, tc.choices, gjson.Get(final, "choices").Raw)
					assert.Equal(t, "upstream-extension", gjson.Get(final, "obfuscation").String())
				} else {
					assert.Equal(t, "stop", gjson.Get(final, "choices.0.finish_reason").String())
				}
				if tc.thinkToContent {
					assert.Equal(t, "<think>\nthinking", gjson.Get(frames[0], "choices.0.delta.content").String())
				}
			})
		}
	}
}

func TestOaiStreamHandlerFinalUsageSurvivesProtocolConversion(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})
	const body = "data: " + `{"id":"chatcmpl-test","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}` + "\n\n" +
		"data: " + `{"id":"chatcmpl-test","object":"chat.completion.chunk","model":"glm-5.3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22}}` + "\n\ndata: [DONE]\n\n"
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatGemini} {
		t.Run(string(format), func(t *testing.T) {
			c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
			info.RelayMode = relayconstant.RelayModeChatCompletions
			info.RelayFormat = format
			info.ShouldIncludeUsage = false
			info.EnsureClaudeConvertInfo()
			usage, apiErr := OaiStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 20, usage.PromptTokens)
			assert.Equal(t, 2, usage.CompletionTokens)
			assert.Equal(t, 22, usage.TotalTokens)
			got := recorder.Body.String()
			if format == types.RelayFormatClaude {
				assert.Contains(t, got, `"stop_reason":"end_turn"`)
				assert.Contains(t, got, `"output_tokens":2`)
				assert.Equal(t, 1, strings.Count(got, "event: message_stop"))
			} else {
				assert.Contains(t, got, `"finishReason":"STOP"`)
				assert.Contains(t, got, `"promptTokenCount":20`)
				assert.Contains(t, got, `"candidatesTokenCount":2`)
			}
		})
	}
}

func TestOaiStreamHandlerPreservesUsageSourcesAndLegacyCompletions(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})
	const usageJSON = `{"prompt_tokens":20,"completion_tokens":2,"total_tokens":22,"cached_tokens":7}`
	for _, tc := range []struct {
		name        string
		first       string
		last        string
		relayMode   int
		channelType int
		cached      int
	}{
		{
			name:        "raw_usage_cache_details",
			first:       `{"choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			last:        `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + usageJSON + `}`,
			relayMode:   relayconstant.RelayModeChatCompletions,
			channelType: constant.ChannelTypeZhipu_v4,
			cached:      7,
		},
		{
			name:        "penultimate_usage_fallback",
			first:       `{"choices":[{"index":0,"delta":{"content":"hello"}}],"usage":` + usageJSON + `}`,
			last:        `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			relayMode:   relayconstant.RelayModeChatCompletions,
			channelType: constant.ChannelTypeZhipu_v4,
			cached:      7,
		},
		{
			name:        "legacy_completions",
			first:       `{"choices":[{"index":0,"text":"hello"}]}`,
			last:        `{"choices":[{"index":0,"text":"!","finish_reason":"stop"}],"usage":` + usageJSON + `}`,
			relayMode:   relayconstant.RelayModeCompletions,
			channelType: constant.ChannelTypeOpenAI,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: " + tc.first + "\n\ndata: " + tc.last + "\n\ndata: [DONE]\n\n"
			c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
			info.RelayMode = tc.relayMode
			info.ChannelType = tc.channelType
			info.ShouldIncludeUsage = false
			usage, apiErr := OaiStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 20, usage.PromptTokens)
			assert.Equal(t, 2, usage.CompletionTokens)
			assert.Equal(t, 22, usage.TotalTokens)
			assert.Equal(t, tc.cached, usage.PromptTokensDetails.CachedTokens)
			got := recorder.Body.String()
			assert.Equal(t, 1, strings.Count(got, "data: [DONE]"))
			var frames []string
			for _, line := range strings.Split(got, "\n") {
				if strings.HasPrefix(line, "data: {") {
					frames = append(frames, strings.TrimPrefix(line, "data: "))
				}
			}
			require.Len(t, frames, 2)
			assert.JSONEq(t, gjson.Get(tc.last, "choices").Raw, gjson.Get(frames[1], "choices").Raw)
			assert.False(t, gjson.Get(frames[1], "usage").Exists())
		})
	}
}
