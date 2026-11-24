package openai

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/requesttimeline"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOaiResponsesHandlerCountsOutputCallsNotDeclarations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	operation_setting.SetToolPriceForTest("priced_fn", 5.0)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest("priced_fn")
	})

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Tools: []map[string]any{
			{"type": "web_search_preview"},
			{"type": "file_search"},
		},
		Output: []dto.ResponsesOutput{
			{Type: dto.BuildInCallWebSearchCall},
			{Type: dto.BuildInCallWebSearchCall},
			{Type: dto.BuildInCallFunctionCall, Name: "priced_fn"},
			{Type: dto.BuildInCallFunctionCall, Name: "unpriced_fn"},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {ToolName: dto.BuildInToolWebSearchPreview, CallCount: 0},
				dto.BuildInToolFileSearch:       {ToolName: dto.BuildInToolFileSearch, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	usage, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview].CallCount)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, "priced_fn")
	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools["priced_fn"].CallCount)
	assert.NotContains(t, info.ResponsesUsageInfo.BuiltInTools, "unpriced_fn")
}

func TestOaiResponsesHandlerDeclaredToolsWithoutOutputCountZero(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Tools: []map[string]any{
			{"type": "web_search_preview"},
			{"type": "file_search"},
		},
		Output: []dto.ResponsesOutput{
			{Type: "message", Role: "assistant"},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {ToolName: dto.BuildInToolWebSearchPreview, CallCount: 0},
				dto.BuildInToolFileSearch:       {ToolName: dto.BuildInToolFileSearch, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolWebSearchPreview].CallCount)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolFileSearch].CallCount)
}

func TestOaiResponsesHandlerCountsCompletedImageGenerationOutputs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Status: []byte(`"completed"`),
		Output: []dto.ResponsesOutput{
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_1",
				Status: "completed",
				Result: "base64-a",
			},
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_2",
				Status: "completed",
				Result: "base64-b",
			},
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_empty",
				Status: "completed",
				Result: "",
			},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{OriginModelName: "gpt-5.1"}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	assert.Equal(t, 2, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
	assert.False(t, c.GetBool("image_generation_call"))
}

func TestOaiResponsesHandlerIncompleteStatusCommitsZeroImageGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, err := common.Marshal(dto.OpenAIResponsesResponse{
		Status: []byte(`"incomplete"`),
		Output: []dto.ResponsesOutput{
			{
				Type:   dto.ResponsesOutputTypeImageGenerationCall,
				ID:     "img_1",
				Status: "completed",
				Result: "base64-a",
			},
		},
		Usage: &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolImageGeneration: {ToolName: dto.BuildInToolImageGeneration, CallCount: 0},
			},
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}

	_, apiErr := OaiResponsesHandler(c, info, resp)
	require.Nil(t, apiErr)
	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func runResponsesImageBillingStream(t *testing.T, events ...string) *relaycommon.RelayInfo {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() {
		constant.StreamingTimeout = oldTimeout
	})

	var body strings.Builder
	for _, event := range events {
		body.WriteString("data: ")
		body.WriteString(event)
		body.WriteString("\n\n")
	}
	body.WriteString("data: [DONE]\n\n")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(common.RequestIdKey, "responses-image-billing-test")
	info := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		DisablePing:     true,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gpt-5.1",
		},
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body.String())),
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}

	_, apiErr := OaiResponsesStreamHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, info.ResponsesUsageInfo)
	require.Contains(t, info.ResponsesUsageInfo.BuiltInTools, dto.BuildInToolImageGeneration)
	return info
}

func TestOaiResponsesStreamHandlerDeduplicatesCompletedImageOutput(t *testing.T) {
	item := `{"type":"image_generation_call","id":"img_1","call_id":"call_1","status":"completed","result":"base64-a"}`
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.output_item.done","output_index":0,"item":`+item+`}`,
		`{"type":"response.completed","response":{"status":"completed","output":[`+item+`],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	)

	assert.Equal(t, 1, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesStreamHandlerDiscardsImageOutputOnIncomplete(t *testing.T) {
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"image_generation_call","id":"img_1","status":"completed","result":"base64-a"}}`,
		`{"type":"response.incomplete","response":{"status":"incomplete"}}`,
	)

	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestOaiResponsesStreamHandlerDoesNotCountPartialImageEvent(t *testing.T) {
	info := runResponsesImageBillingStream(
		t,
		`{"type":"response.image_generation_call.partial_image","output_index":0,"partial_image_b64":"partial-bytes"}`,
		`{"type":"response.completed","response":{"status":"completed","output":[]}}`,
	)

	assert.Equal(t, 0, info.ResponsesUsageInfo.BuiltInTools[dto.BuildInToolImageGeneration].CallCount)
}

func TestResponsesStreamTerminalUsageAccounting(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	for _, tc := range []struct {
		name, event, status string
		timeline, snapshot  bool
	}{
		{"completed", "response.completed", "completed", false, false},
		{"done", "response.done", "completed", false, false},
		{"incomplete", "response.incomplete", "incomplete", false, false},
		{"incomplete with timeline and billing snapshot", "response.incomplete", "incomplete", true, true},
		{"failed", "response.failed", "failed", false, false},
		{"cancelled", "response.cancelled", "cancelled", false, false},
		{"canceled", "response.canceled", "cancelled", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &dto.Usage{
				InputTokens: 120, OutputTokens: 30, TotalTokens: 150,
				InputTokensDetails:     &dto.InputTokenDetails{CachedTokens: 40, CacheWriteTokens: 60},
				CompletionTokenDetails: dto.OutputTokenDetails{ReasoningTokens: 30},
			}
			if tc.snapshot {
				upstream.BillingUsage = dto.NewOpenAIResponsesBillingUsage(upstream)
			}
			status, err := common.Marshal(tc.status)
			require.NoError(t, err)
			terminal, err := common.Marshal(dto.ResponsesStreamResponse{
				Type: tc.event, Response: &dto.OpenAIResponsesResponse{Status: status, Usage: upstream},
			})
			require.NoError(t, err)
			// No body text: this is the reasoning-only truncation regression.
			// A later empty snapshot must not erase the terminal's metering.
			body := "data: " + string(terminal) + "\n\ndata: " +
				`{"type":"` + tc.event + `","response":{"status":"` + tc.status + `","usage":{}}}` + "\n\n"
			c, recorder, resp, info := newResponsesChatTestContext(t, body, true)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			info.RelayMode = relayconstant.RelayModeResponses
			info.RelayFormat = types.RelayFormatOpenAIResponses
			info.SetEstimatePromptTokens(999)
			if tc.timeline {
				info.Timeline = requesttimeline.New(time.Now(), "responses-usage-test", 1, "gpt-test")
				info.Timeline.BeginAttempt(0)
			}
			resp.Header.Set("Msh-Usage-Cache-Write-Tokens-5m", "0")
			resp.Header.Set("Msh-Usage-Cache-Write-Tokens-1h", "60")
			value, apiErr := (&Adaptor{}).DoResponse(c, resp, info)
			require.Nil(t, apiErr)
			usage, ok := value.(*dto.Usage)
			require.True(t, ok)
			require.NotNil(t, usage)
			assert.Equal(t, 120, usage.PromptTokens)
			assert.Equal(t, 30, usage.CompletionTokens)
			assert.Equal(t, 150, usage.TotalTokens)
			assert.Equal(t, 40, usage.PromptTokensDetails.CachedTokens)
			assert.Equal(t, 60, usage.PromptTokensDetails.CacheWriteTokens)
			assert.Equal(t, 30, usage.CompletionTokenDetails.ReasoningTokens)
			assert.Equal(t, &dto.CacheWriteBreakdown{OneHourTokens: 60}, usage.KimiCacheWriteBreakdown())
			assert.Contains(t, recorder.Body.String(), string(terminal), "forward the original event unchanged")
			if tc.snapshot {
				require.NotNil(t, usage.BillingUsage)
				assert.False(t, usage.BillingUsage.Estimated)
				canonical, ok := usage.BillingUsage.CanonicalUsage()
				require.True(t, ok)
				assert.Equal(t, 120, canonical.PromptTokens)
				assert.Equal(t, 30, canonical.CompletionTokens)
				assert.Equal(t, 40, canonical.PromptTokensDetails.CachedTokens)
				assert.Equal(t, &dto.CacheWriteBreakdown{OneHourTokens: 60}, canonical.KimiCacheWriteBreakdown())
			} else {
				assert.Nil(t, usage.BillingUsage, "native passthrough must not introduce a conversion snapshot")
			}
			if tc.timeline {
				record := info.Timeline.Freeze(nil)
				require.NotNil(t, record)
				assert.Equal(t, "response.incomplete", record.Data.FinishReason)
				assert.Equal(t, "captured", record.Data.UpstreamUsageCapture)
			}
		})
	}
}

func TestResponsesStreamMissingUsageEstimation(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	const created = `{"type":"response.created","response":{"status":"in_progress"}}`
	const incomplete = `{"type":"response.incomplete","response":{"status":"incomplete"}}`
	const failed = `{"type":"response.failed","response":{"status":"failed"}}`
	const output = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
	for _, tc := range []struct {
		name            string
		events          []string
		prompt          int
		estimatedText   string
		billingSnapshot bool
	}{
		{name: "reasoning summary before incomplete", events: []string{created, `{"type":"response.reasoning_summary_text.delta","delta":"hello"}`, incomplete}, prompt: 100, estimatedText: "hello"},
		{name: "reasoning text before EOF", events: []string{created, `{"type":"response.reasoning_text.delta","delta":"hello"}`}, prompt: 100, estimatedText: "hello"},
		{name: "tool arguments before EOF", events: []string{created, `{"type":"response.function_call_arguments.delta","delta":"hello"}`}, prompt: 100, estimatedText: "hello"},
		{name: "refusal before EOF", events: []string{created, `{"type":"response.refusal.delta","delta":"hello"}`}, prompt: 100, estimatedText: "hello"},
		{name: "body before EOF", events: []string{created, `{"type":"response.output_text.delta","delta":"hello"}`}, prompt: 100, estimatedText: "hello"},
		{name: "created only before EOF", events: []string{created}, prompt: 100},
		{name: "incomplete without usage or text", events: []string{created, incomplete}, prompt: 100},
		{name: "terminal output without deltas", events: []string{`{"type":"response.completed","response":{"status":"completed","output":[` + output + `]}}`}, prompt: 100, estimatedText: "hello"},
		{name: "terminal output must not duplicate delta", events: []string{`{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"response.completed","response":{"output":[` + output + `]}}`}, prompt: 100, estimatedText: "hello"},
		{name: "explicit failure without output", events: []string{created, failed}},
		{name: "flat error without output", events: []string{created, `{"type":"error","code":"server_error"}`}},
		{name: "response error without output", events: []string{created, `{"type":"response.error"}`}},
		{name: "done with failed status", events: []string{created, `{"type":"response.done","response":{"status":"failed"}}`}},
		{name: "failure after generated output", events: []string{`{"type":"response.reasoning_text.delta","delta":"hello"}`, failed}, prompt: 100, estimatedText: "hello"},
		{name: "no upstream events"},
		{name: "malformed first event", events: []string{`{"type":`}},
		{name: "partial billing snapshot", events: []string{`{"type":"response.reasoning_text.delta","delta":"hello"}`, `{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":4},"billing_usage":{"source":"oai_responses","semantic":"openai","openai_usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":4}}}}}}`}, prompt: 20, estimatedText: "hello", billingSnapshot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body strings.Builder
			for _, event := range tc.events {
				body.WriteString("data: " + event + "\n\n")
			}
			c, _, resp, info := newResponsesChatTestContext(t, body.String(), true)
			info.UpstreamModelName = "test-model"
			info.SetEstimatePromptTokens(100)
			usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			completion := service.CountTextToken(tc.estimatedText, info.UpstreamModelName)
			if tc.estimatedText != "" {
				require.Positive(t, completion)
			}
			assert.Equal(t, tc.prompt, usage.PromptTokens)
			assert.Equal(t, completion, usage.CompletionTokens)
			assert.Equal(t, tc.prompt+completion, usage.TotalTokens)
			if tc.billingSnapshot {
				require.NotNil(t, usage.BillingUsage)
				assert.True(t, usage.BillingUsage.Estimated)
				canonical, ok := usage.BillingUsage.CanonicalUsage()
				require.True(t, ok)
				assert.Equal(t, tc.prompt, canonical.PromptTokens)
				assert.Equal(t, completion, canonical.CompletionTokens)
				assert.Equal(t, 4, canonical.PromptTokensDetails.CachedTokens)
			}
		})
	}
}
