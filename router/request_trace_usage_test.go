package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func requestTraceUsageFixture(payload string) string {
	extension := `"vendor_extension":{"wide":9007199254740993,"zero":0,"nullable":null,"array":[{"modality":"text","tokens":0}],"label":"上游原始用量"},`
	payload = strings.ReplaceAll(payload, `"usage":{`, `"usage":{`+extension)
	return strings.ReplaceAll(payload, `"usageMetadata":{`, `"usageMetadata":{`+extension)
}

// 同一组原始用量契约同时保护原生与转换路径，避免用客户端 DTO 补出的字段代替上游数据。
func assertRequestTraceUsage(t *testing.T, usage *common.RequestTraceUsage, format types.RelayFormat, stream bool) {
	t.Helper()
	require.NotNil(t, usage)
	formats := map[types.RelayFormat]string{types.RelayFormatOpenAI: "openai", types.RelayFormatOpenAIResponses: "openai_responses", types.RelayFormatClaude: "claude", types.RelayFormatGemini: "gemini"}
	assert.Equal(t, formats[format], usage.Format)
	parts := 1
	if stream && format == types.RelayFormatClaude {
		parts = 2
	}
	require.Len(t, usage.Parts, parts)
	assert.False(t, usage.Incomplete)
	for _, part := range usage.Parts {
		assert.True(t, gjson.Valid(part.RawValue))
		assert.Contains(t, part.RawValue, "\n  ")
		assert.Equal(t, "9007199254740993", gjson.Get(part.RawValue, "vendor_extension.wide").Raw)
		assert.Equal(t, "0", gjson.Get(part.RawValue, "vendor_extension.zero").Raw)
		assert.Equal(t, "null", gjson.Get(part.RawValue, "vendor_extension.nullable").Raw)
		assert.Equal(t, "0", gjson.Get(part.RawValue, "vendor_extension.array.0.tokens").Raw)
		assert.Empty(t, part.Value)
	}
}

func TestRequestTraceUsageNativeHTTP(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	type responseHandler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	claudeJSON := func(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response) (*dto.Usage, *types.NewAPIError) {
		return claude.ClaudeHandler(c, response, info)
	}
	claudeStream := func(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response) (*dto.Usage, *types.NewAPIError) {
		return claude.ClaudeStreamHandler(c, response, info)
	}
	compactJSON := func(c *gin.Context, _ *relaycommon.RelayInfo, response *http.Response) (*dto.Usage, *types.NewAPIError) {
		return openai.OaiResponsesCompactionHandler(c, response)
	}
	for _, tc := range []struct {
		name, path   string
		format       types.RelayFormat
		mode         int
		json, stream responseHandler
	}{
		{"chat", "/v1/chat/completions", types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, openai.OpenaiHandler, openai.OaiStreamHandler},
		{"completions", "/v1/completions", types.RelayFormatOpenAI, relayconstant.RelayModeCompletions, openai.OpenaiHandler, openai.OaiStreamHandler},
		{"responses", "/v1/responses", types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses, openai.OaiResponsesHandler, openai.OaiResponsesStreamHandler},
		{"responses_compact", "/v1/responses/compact", types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponsesCompact, compactJSON, nil},
		{"messages", "/v1/messages", types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, claudeJSON, claudeStream},
		{"gemini", "/v1beta/models/client-model:generateContent", types.RelayFormatGemini, relayconstant.RelayModeGemini, gemini.GeminiTextGenerationHandler, gemini.GeminiTextGenerationStreamHandler},
	} {
		for _, level := range []string{"all", "errors"} {
			streams := []bool{false, true}
			if tc.stream == nil {
				streams = []bool{false}
			}
			for _, stream := range streams {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, level, stream), func(t *testing.T) {
					db := setupRequestTraceDatabase(t)
					require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", fmt.Sprintf(`{"level":%q,"compression_level":"fast"}`, level)))
					path := tc.path
					if stream && tc.format == types.RelayFormatGemini {
						path = strings.Replace(path, ":generateContent", ":streamGenerateContent", 1)
					}
					payload := requestTraceProtocolPayload(tc.format, stream)
					if tc.name == "responses_compact" {
						payload = `{"id":"source-id","object":"response.compaction","model":"upstream-model","output":[{"id":"cmp-1","type":"compaction","encrypted_content":"private-compaction-body"}],"usage":{"input_tokens":10,"output_tokens":9,"total_tokens":19}}`
					}
					if tc.name == "completions" {
						payload = `{"id":"source-id","object":"text_completion","model":"upstream-model","choices":[{"index":0,"text":"ok","finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":9,"total_tokens":19}}`
						if stream {
							payload = "data: " + payload + "\n\ndata: [DONE]\n\n"
						}
					}
					payload = requestTraceUsageFixture(payload)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						assert.Equal(t, path, r.URL.Path)
						contentType := "application/json"
						if stream {
							contentType = "text/event-stream"
						}
						w.Header().Set("Content-Type", contentType)
						_, err := io.WriteString(w, payload)
						assert.NoError(t, err)
					}))
					t.Cleanup(upstream.Close)
					app := gin.New()
					app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
					app.POST(path, func(c *gin.Context) {
						c.Set("id", 11)
						_, err := common.GetBodyStorage(c)
						require.NoError(t, err)
						info := &relaycommon.RelayInfo{RelayFormat: tc.format, FinalRequestRelayFormat: tc.format, OriginModelName: "client-model", IsStream: stream, RequestURLPath: path, RelayMode: tc.mode, ShouldIncludeUsage: true, DisablePing: true, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelId: 7, UpstreamModelName: "upstream-model"}}
						info.InitRequestConversionChain()
						request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL+path, strings.NewReader(requestTraceProtocolRequest(tc.format)))
						require.NoError(t, err)
						request.Header.Set("Content-Type", "application/json")
						response, err := channel.DoRequest(c, request, info)
						require.NoError(t, err)
						handler := tc.json
						if stream {
							handler = tc.stream
						}
						usage, relayError := handler(c, info, response)
						require.Nil(t, relayError)
						require.NotNil(t, usage)
						assert.Equal(t, 10, usage.PromptTokens)
						assert.Equal(t, 9, usage.CompletionTokens)
						model.RecordConsumeLog(c, 11, model.RecordConsumeLogParams{ChannelId: 7, ModelName: "client-model", PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, Other: model.NewLogOther()})
						common.GetRequestTrace(c).FinishAttempt(0)
					})
					recorder := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(requestTraceProtocolRequest(tc.format)))
					request.Header.Set("Content-Type", "application/json")
					app.ServeHTTP(recorder, request)
					require.Equal(t, http.StatusOK, recorder.Code)
					awaitRequestTraceWrites(t)
					id := recorder.Header().Get(common.RequestIdKey)
					rows, err := model.GetRequestTraces(t.Context(), id, 0, true)
					require.NoError(t, err)
					require.Len(t, rows, 1)
					require.NotNil(t, rows[0].AdminInfo)
					assertRequestTraceUsage(t, rows[0].AdminInfo.Upstream.Usage, tc.format, stream)
					var stored model.RequestTraceMetadata
					require.NoError(t, db.Where("request_id = ?", id).First(&stored).Error)
					assert.True(t, strings.HasPrefix(stored.Data, "zstd-base64-v1:"))
					decoded, err := common.DecodeRequestTraceStorage(stored.Data)
					require.NoError(t, err)
					assert.Equal(t, "9007199254740993", gjson.Get(decoded, "admin_info.upstream.usage.parts.0.value.vendor_extension.wide").Raw)
					assert.False(t, gjson.Get(decoded, "admin_info.upstream.usage.parts.0.raw_value").Exists())
				})
			}
		}
	}
}

func TestRequestTraceUsageSDK(t *testing.T) {
	for _, mode := range []string{"json", "sse", "json_business_error"} {
		t.Run(mode, func(t *testing.T) {
			setupRequestTraceDatabase(t)
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all"}`))
			stream := mode == "sse"
			payload := requestTraceProtocolPayload(types.RelayFormatClaude, stream)
			if mode == "json_business_error" {
				payload = `{"type":"error","error":{"type":"overloaded_error","message":"private-error-body"},"usage":{"input_tokens":10,"output_tokens":0}}`
			}
			payload = requestTraceUsageFixture(payload)
			app := gin.New()
			app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
			app.POST("/v1/messages", func(c *gin.Context) {
				c.Set("id", 11)
				_, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				common.StartSDKRequestTrace(c, 0, 7, 0, stream)
				defer common.FinishSDKRequestTrace(c, 0)
				info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatClaude, OriginModelName: "client-model", IsStream: stream, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7, UpstreamModelName: "upstream-model"}}
				info.InitRequestConversionChain()
				responseInfo := &claude.ClaudeResponseInfo{Usage: &dto.Usage{}}
				if stream {
					for _, line := range strings.Split(payload, "\n") {
						if strings.HasPrefix(line, "data:") {
							require.Nil(t, claude.HandleStreamResponseData(c, info, responseInfo, strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
						}
					}
				} else {
					responseError := claude.HandleClaudeResponseData(c, info, responseInfo, nil, []byte(payload))
					if mode == "json_business_error" {
						require.NotNil(t, responseError)
						c.JSON(http.StatusBadGateway, gin.H{"error": "upstream failure"})
					} else {
						require.Nil(t, responseError)
					}
				}
				model.RecordConsumeLog(c, 11, model.RecordConsumeLogParams{ChannelId: 7, ModelName: "client-model", Other: model.NewLogOther()})
			})
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestTraceProtocolRequest(types.RelayFormatClaude))))
			status := http.StatusOK
			if mode == "json_business_error" {
				status = http.StatusBadGateway
			}
			require.Equal(t, status, recorder.Code)
			awaitRequestTraceWrites(t)
			rows, err := model.GetRequestTraces(t.Context(), recorder.Header().Get(common.RequestIdKey), 0, true)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assertRequestTraceUsage(t, rows[0].AdminInfo.Upstream.Usage, types.RelayFormatClaude, stream)
			assert.NotContains(t, common.GetJsonString(rows), "private-error-body")
		})
	}
}

func TestRequestTraceUsageFragments(t *testing.T) {
	limitFrames := make([]string, 33)
	for index := range limitFrames {
		limitFrames[index] = fmt.Sprintf(`{"usage":{"counter":%d}}`, index)
	}
	for _, tc := range []struct {
		name       string
		frames     []string
		parts      int
		incomplete bool
		last       string
	}{
		{"cumulative snapshots replace", []string{`{"choices":[],"usage":{"completion_tokens":1}}`, `{"choices":[],"usage":{"completion_tokens":2}}`, `{"choices":[],"usage":{"completion_tokens":2}}`}, 1, false, `{"completion_tokens":2}`},
		{"unknown protocol preserves updates", []string{`{"usage":{"unit":1}}`, `{"usage":{"unit":2}}`}, 2, false, `{"unit":2}`},
		{"partial objects preserve fields", []string{`{"choices":[],"usage":{"prompt_tokens":10}}`, `{"choices":[],"usage":{"completion_tokens":9}}`}, 2, false, `{"completion_tokens":9}`},
		{"array snapshots remain original", []string{`{"choices":[],"usage":{"details":[1]}}`, `{"choices":[],"usage":{"details":[2]}}`, `{"choices":[],"usage":{"details":[1]}}`}, 2, false, `{"details":[2]}`},
		{"explicit zero remains zero", []string{`{"choices":[],"usage":{"completion_tokens":5}}`, `{"choices":[],"usage":{"completion_tokens":0}}`}, 1, false, `{"completion_tokens":0}`},
		{"null is distinct from missing", []string{`{"choices":[],"usage":null}`}, 1, false, `null`},
		{"missing usage is not fabricated", []string{`{"choices":[]}`}, 0, false, ""},
		{"empty object is retained", []string{`{"choices":[],"usage":{}}`}, 1, false, `{}`},
		{"choice-local usage is captured", []string{`{"choices":[{"index":0,"usage":{"cached_tokens":0}},{"index":1,"usage":{"cached_tokens":2}}]}`}, 2, false, `{"cached_tokens":2}`},
		{"interrupted stream retains reported usage", []string{`{"choices":[],"usage":{"prompt_tokens":10}}`}, 1, true, `{"prompt_tokens":10}`},
		{"large usage is marked partial", []string{`{"choices":[],"usage":{"detail":"` + strings.Repeat("x", 64<<10) + `"}}`}, 0, true, ""},
		{"fragment count is bounded", limitFrames, 32, true, `{"counter":31}`},
		{"invalid usage is not persisted", []string{`{"choices":[],"usage":{"invalid":}}`}, 0, true, ""},
		{"invalid UTF8 is not persisted", []string{`{"choices":[],"usage":{"invalid":"` + string([]byte{0xff}) + `"}}`}, 0, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := common.NewTraceBodyObserver(time.Now()).NewRelatedObserver()
			for _, frame := range tc.frames {
				observer.ObserveString("data: "+frame+"\n\n", "text/event-stream")
			}
			if tc.name != "interrupted stream retains reported usage" {
				observer.ObserveString("data: [DONE]\n\n", "text/event-stream")
			}
			observer.Finish()
			_, upstream, _ := observer.Snapshot()
			require.NotNil(t, upstream.Usage)
			require.Len(t, upstream.Usage.Parts, tc.parts)
			assert.Equal(t, tc.incomplete, upstream.Usage.Incomplete)
			if tc.last != "" {
				assert.JSONEq(t, tc.last, string(upstream.Usage.Parts[len(upstream.Usage.Parts)-1].Value))
			}
		})
	}
}

func TestRequestTraceUsageRetryPersistence(t *testing.T) {
	setupRequestTraceDatabase(t)
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all"}`))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload := `{"error":{"type":"server_error","message":"private-error-body"},"usage":{"prompt_tokens":10,"attempt_marker":"retry"}}`
		if r.URL.Query().Get("attempt") == "0" {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			payload = `{"id":"source-id","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"private-response-body"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":9,"total_tokens":29,"attempt_marker":"final"}}`
		}
		_, err := io.WriteString(w, requestTraceUsageFixture(payload))
		assert.NoError(t, err)
	}))
	t.Cleanup(upstream.Close)
	app := gin.New()
	app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
	app.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("id", 11)
		_, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, OriginModelName: "client-model", RequestURLPath: c.Request.URL.Path, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7, UpstreamModelName: "upstream-model"}}
		info.InitRequestConversionChain()
		for attempt := range 2 {
			info.RetryIndex = attempt
			request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, fmt.Sprintf("%s?attempt=%d", upstream.URL, attempt), strings.NewReader(`{"model":"upstream-model"}`))
			require.NoError(t, err)
			response, err := channel.DoRequest(c, request, info)
			require.NoError(t, err)
			if response.StatusCode >= 400 {
				_, err = io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
			} else {
				usage, responseError := openai.OpenaiHandler(c, info, response)
				require.Nil(t, responseError)
				model.RecordConsumeLog(c, 11, model.RecordConsumeLogParams{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, Other: model.NewLogOther()})
			}
			common.GetRequestTrace(c).FinishAttempt(attempt)
		}
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestTraceProtocolRequest(types.RelayFormatOpenAI)))
	request.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	awaitRequestTraceWrites(t)
	rows, err := model.GetRequestTraces(t.Context(), recorder.Header().Get(common.RequestIdKey), 0, true)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for index, expected := range []struct{ marker, tokens, outcome string }{{"retry", "10", "failed"}, {"final", "20", "success"}} {
		assert.Equal(t, index, rows[index].Attempt)
		assert.Equal(t, expected.outcome, rows[index].Trace.Outcome)
		require.NotNil(t, rows[index].AdminInfo.Upstream.Usage)
		require.Len(t, rows[index].AdminInfo.Upstream.Usage.Parts, 1)
		value := rows[index].AdminInfo.Upstream.Usage.Parts[0].RawValue
		assert.Equal(t, expected.marker, gjson.Get(value, "attempt_marker").String())
		assert.Equal(t, expected.tokens, gjson.Get(value, "prompt_tokens").Raw)
	}
	assert.NotContains(t, common.GetJsonString(rows), "private-error-body")
	assert.NotContains(t, common.GetJsonString(rows), "private-response-body")
}

func TestRequestTraceUsageLargeAndLegacyStorage(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	raw := `{"vendor_blob":"` + strings.Repeat("x", (64<<10)-80) + `","wide":9007199254740993}`
	data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}, AdminInfo: &common.RequestTraceAdmin{Upstream: common.RequestTraceUpstream{Usage: &common.RequestTraceUsage{Format: "openai", Parts: []common.RequestTraceUsagePart{{Path: "usage", Value: []byte(raw)}}}}}})
	require.Greater(t, len(data), 65535)
	require.NoError(t, db.Create(&model.RequestTraceMetadata{RequestId: "large-raw-usage", UserId: 11, Data: data}).Error)
	views, err := model.GetRequestTraces(t.Context(), "large-raw-usage", 0, true)
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, "9007199254740993", gjson.Get(views[0].AdminInfo.Upstream.Usage.Parts[0].RawValue, "wide").Raw)
	assert.Equal(t, (64<<10)-80, len(gjson.Get(views[0].AdminInfo.Upstream.Usage.Parts[0].RawValue, "vendor_blob").String()))
	var stored model.RequestTraceMetadata
	require.NoError(t, db.Where("request_id = ?", "large-raw-usage").First(&stored).Error)
	assert.Equal(t, data, stored.Data)
	legacy := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}, AdminInfo: &common.RequestTraceAdmin{}})
	require.NoError(t, model.RecordRequestTraces(t.Context(), []model.RequestTraceMetadata{{RequestId: "legacy-no-usage", UserId: 11, Data: legacy}}))
	views, err = model.GetRequestTraces(t.Context(), "legacy-no-usage", 0, true)
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Nil(t, views[0].AdminInfo.Upstream.Usage)
}
