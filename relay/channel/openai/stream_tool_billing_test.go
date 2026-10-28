package openai

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectStreamFunctionCallNamesDedupesSameIndex(t *testing.T) {
	seen := make(map[string]struct{})
	var names []string

	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`,
	}
	for _, chunk := range chunks {
		var response dto.ChatCompletionsStreamResponse
		require.NoError(t, common.UnmarshalJsonStr(chunk, &response))
		collectStreamFunctionCallNames(response.Choices, seen, &names)
	}

	require.Len(t, names, 2)
	assert.Equal(t, []string{"get_weather", "get_time"}, names)
}

func TestOaiStreamOptionalTypeDoesNotBreakForwarding(t *testing.T) {
	previous := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previous })
	for _, kind := range []string{"null", "123", `{}`} {
		t.Run(kind, func(t *testing.T) {
			chunk := fmt.Sprintf(`{"type":%s,"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`, kind)
			body := "data: " + chunk + "\n\ndata: " + `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\ndata: [DONE]\n\n"
			c, recorder, response, info := newResponsesChatTestContext(t, body, true)
			info.RelayMode = relayconstant.RelayModeChatCompletions
			usage, apiError := OaiStreamHandler(c, info, response)
			require.Nil(t, apiError)
			require.NotNil(t, usage)
			assert.Equal(t, 1, usage.PromptTokens)
			assert.Equal(t, 2, usage.CompletionTokens)
			assert.Contains(t, recorder.Body.String(), chunk)
			assert.False(t, info.StreamStatus.HasErrors())
		})
	}
}

func TestOaiStreamUsageFallbackIgnoresUnrelatedFields(t *testing.T) {
	previous := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previous })
	for _, fields := range []string{`"created":"invalid","choices":[]`, `"choices":[{"delta":{"content":123}}]`} {
		t.Run(fields, func(t *testing.T) {
			chunk := `{` + fields + `,"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`
			body := "data: " + chunk + "\n\ndata: " + `{"choices":[]}` + "\n\ndata: [DONE]\n\n"
			c, recorder, response, info := newResponsesChatTestContext(t, body, true)
			info.RelayMode = relayconstant.RelayModeChatCompletions
			usage, apiError := OaiStreamHandler(c, info, response)
			require.Nil(t, apiError)
			require.NotNil(t, usage)
			assert.Equal(t, 20, usage.PromptTokens)
			assert.Equal(t, 10, usage.CompletionTokens)
			assert.Contains(t, recorder.Body.String(), chunk)
		})
	}
}
