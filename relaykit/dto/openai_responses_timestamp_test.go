package dto

import (
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesCreatedAtAcceptsOnlyIntegerTimestamps(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	minimum := -maximum - 1
	for _, tc := range []struct {
		name  string
		input string
		want  int
	}{
		{"integer", "1792569347", 1792569347},
		{"zero_fraction", "1792569347.0", 1792569347},
		{"multiple_zeroes", "1792569347.000", 1792569347},
		{"zero", "0.0", 0},
		{"negative", "-1.00", -1},
		{"maximum", strconv.Itoa(maximum) + ".0", maximum},
		{"minimum", strconv.Itoa(minimum) + ".0", minimum},
		{"null", "null", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := OpenAIResponsesResponse{CreatedAt: 42}
			require.NoError(t, kitutil.UnmarshalJsonStr(`{"created_at":`+tc.input+`}`, &value))
			assert.Equal(t, tc.want, value.CreatedAt)
			encoded, err := kitutil.Marshal(value)
			require.NoError(t, err)
			assert.Contains(t, string(encoded), `"created_at":`+strconv.Itoa(tc.want)+",")
		})
	}
	value := OpenAIResponsesResponse{CreatedAt: 42}
	require.NoError(t, kitutil.UnmarshalJsonStr(`{"id":"resp_existing"}`, &value))
	assert.Equal(t, 42, value.CreatedAt)
	require.NoError(t, kitutil.UnmarshalJsonStr(`{"created_at":1.0,"created_at":null,"created_at":2.00}`, &value))
	assert.Equal(t, 2, value.CreatedAt)
}

func TestResponsesCreatedAtRejectsFractionalOrInvalidValues(t *testing.T) {
	for _, raw := range []string{
		"1792569347.1", "1.00000000000000000001", "-0.1", "1e3", "1.0e0",
		"9223372036854775808.0", "-9223372036854775809.0",
		`"1792569347.0"`, "true", "[]", "{}",
	} {
		t.Run(raw, func(t *testing.T) {
			var value OpenAIResponsesResponse
			assert.Error(t, kitutil.UnmarshalJsonStr(`{"created_at":`+raw+`}`, &value))
		})
	}
	var value OpenAIResponsesResponse
	assert.Error(t, kitutil.UnmarshalJsonStr(`{"created_at":1.0,"max_output_tokens":96.0}`, &value))
	assert.Error(t, kitutil.UnmarshalJsonStr(`{"created_at":1.0,"usage":{"input_tokens":12.0}}`, &value))
	var chat TextResponse
	assert.Error(t, kitutil.UnmarshalJsonStr(`{"created":1792569347.0}`, &chat))
}

func TestResponsesStreamCreatedAtPreservesCompletionUsage(t *testing.T) {
	var event ResponsesStreamResponse
	require.NoError(t, kitutil.UnmarshalJsonStr(`{"type":"response.completed","response":{"id":"resp_upstream","created_at":1792569347.0,"status":"completed","usage":{"input_tokens":12,"output_tokens":34,"total_tokens":46}}}`, &event))
	require.NotNil(t, event.Response)
	assert.Equal(t, 1792569347, event.Response.CreatedAt)
	assert.JSONEq(t, `"completed"`, string(event.Response.Status))
	require.NotNil(t, event.Response.Usage)
	assert.Equal(t, 12, event.Response.Usage.InputTokens)
	assert.Equal(t, 34, event.Response.Usage.OutputTokens)
	assert.Equal(t, 46, event.Response.Usage.TotalTokens)
}
