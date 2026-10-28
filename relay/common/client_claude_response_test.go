package common

import (
	"testing"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientClaudeResponseData(t *testing.T) {
	for _, tc := range []struct {
		name, original, model, expected string
	}{
		{
			name:     "JSON keeps content tools and vendor fields",
			original: `{"type":"message","model":"upstream-model","content":[{"type":"text","text":"upstream-model"},{"type":"tool_use","input":{"model":"upstream-model"}}],"vendor":{"model":"upstream-model"},"usage":{"input_tokens":10}}`,
			model:    "client-model",
			expected: `{"type":"message","model":"client-model","content":[{"type":"text","text":"upstream-model"},{"type":"tool_use","input":{"model":"upstream-model"}}],"vendor":{"model":"upstream-model"},"usage":{"input_tokens":10}}`,
		},
		{
			name:     "message_start keeps nested extensions",
			original: `{"type":"message_start","message":{"model":"upstream-model","content":[],"vendor":{"model":"upstream-model"}},"vendor":true}`,
			model:    "client-model",
			expected: `{"type":"message_start","message":{"model":"client-model","content":[],"vendor":{"model":"upstream-model"}},"vendor":true}`,
		},
		{
			name:     "both canonical fields",
			original: `{"type":"message_start","model":"upstream-model","message":{"model":"other-upstream"}}`,
			model:    "client-model",
			expected: `{"type":"message_start","model":"client-model","message":{"model":"client-model"}}`,
		},
		{
			name:     "escaped alias",
			original: `{"type":"message_start","message":{"model":"upstream-model"}}`,
			model:    "别名\"\\\n模型",
			expected: `{"type":"message_start","message":{"model":"别名\"\\\n模型"}}`,
		},
		{
			name:     "same alias preserves bytes",
			original: `{ "type": "message", "model": "client-model" }`,
			model:    "client-model",
		},
		{
			name:     "empty alias preserves bytes",
			original: `{ "type": "message", "model": "upstream-model" }`,
		},
		{
			name:     "delta does not gain a model field",
			original: `{ "type": "content_block_delta", "delta": {"text":"upstream-model"} }`,
			model:    "client-model",
		},
		{
			name:     "empty and null models remain unchanged",
			original: `{ "type": "message_start", "model": "", "message": {"model":null} }`,
			model:    "client-model",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.original)
			var upstream dto.ClaudeResponse
			require.NoError(t, rootcommon.Unmarshal(data, &upstream))
			before, err := rootcommon.Marshal(&upstream)
			require.NoError(t, err)

			client, output, err := ClientClaudeResponseData(&upstream, tc.model, data)
			require.NoError(t, err)
			if tc.expected == "" {
				assert.Equal(t, tc.original, string(output))
			} else {
				assert.JSONEq(t, tc.expected, string(output))
			}
			var decoded dto.ClaudeResponse
			require.NoError(t, rootcommon.Unmarshal(output, &decoded))
			assert.Equal(t, &decoded, client)
			after, err := rootcommon.Marshal(&upstream)
			require.NoError(t, err)
			assert.Equal(t, before, after, "upstream DTO must retain its original model")
			assert.Equal(t, tc.original, string(data), "upstream bytes must remain unchanged")
		})
	}
	t.Run("nil response", func(t *testing.T) {
		client, data, err := ClientClaudeResponseData(nil, "client-model", nil)
		require.NoError(t, err)
		assert.Nil(t, client)
		assert.Nil(t, data)
	})
}
