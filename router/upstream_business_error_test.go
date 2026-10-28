package router

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamBusinessErrorFields(t *testing.T) {
	for _, tc := range []struct {
		name, payload, event, message, code string
		status, reported                    int
		absent                              bool
	}{
		{name: "nested error", payload: `{"error":{"type":"server_error","code":"upstream_timeout","message":"failed","status_code":429}}`, message: "failed", code: `"upstream_timeout"`, reported: 429},
		{name: "message without type", payload: `{"error":{"message":"failed"}}`, message: "failed"},
		{name: "string error", payload: `{"error":"failed"}`, message: "failed"},
		{name: "numeric zero code", payload: `{"error":{"code":0}}`, code: "0"},
		{name: "numeric code is not HTTP status", payload: `{"error":{"code":429,"message":"HTTP 503"}}`, message: "HTTP 503", code: "429"},
		{name: "string HTTP status is not inferred", payload: `{"error":{"message":"failed","status_code":"429"}}`, message: "failed"},
		{name: "fractional HTTP status ignored", payload: `{"error":{"message":"failed","http_status":429.5}}`, message: "failed"},
		{name: "out of range HTTP status ignored", payload: `{"error":{"message":"failed","http_status":600}}`, message: "failed"},
		{name: "explicit root HTTP status", payload: `{"error":{"message":"failed"},"http_status":503}`, message: "failed", reported: 503},
		{name: "Responses nested error", payload: `{"type":"response.failed","response":{"status":"failed","error":{"message":"failed","code":"timeout"}}}`, event: "response.failed", message: "failed", code: `"timeout"`},
		{name: "Responses failure without invented fields", payload: `{"type":"response.failed","response":{"status":"failed"}}`, event: "response.failed"},
		{name: "flat Responses error", payload: `{"type":"response.error","code":"timeout","message":"failed"}`, event: "response.error", message: "failed", code: `"timeout"`},
		{name: "flat error", payload: `{"type":"error","code":"timeout","message":"failed"}`, message: "failed", code: `"timeout"`},
		{name: "invalid field shapes ignored", payload: `{"error":{"type":[],"code":{},"message":42,"param":false,"status":[]}}`},
		{name: "oversized number ignored", payload: `{"error":{"code":1234567890123456789012345678901234567890}}`},
		{name: "null error", payload: `{"error":null}`, absent: true},
		{name: "false error", payload: `{"error":false}`, absent: true},
		{name: "empty error", payload: `{"error":{}}`, absent: true},
		{name: "blank string error", payload: `{"error":"  "}`, absent: true},
		{name: "nested tool result is not provider error", payload: `{"choices":[{"message":{"content":{"error":{"message":"tool failed"}}}}]}`, absent: true},
		{name: "refusal is not provider error", payload: `{"choices":[{"message":{"refusal":"blocked"},"finish_reason":"content_filter"}]}`, absent: true},
		{name: "incomplete is not failure", payload: `{"type":"response.incomplete","response":{"status":"incomplete","error":null}}`, event: "response.incomplete", absent: true},
		{name: "missing usage is not failure", payload: `{"choices":[{"finish_reason":"stop"}]}`, absent: true},
		{name: "non-200 is handled by existing HTTP error path", payload: `{"error":{"message":"failed"}}`, status: 502, absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			status, event := tc.status, tc.event
			if status == 0 {
				status = http.StatusOK
			}
			if event == "" {
				event = "error"
			}
			common.CaptureUpstreamBusinessError(c, status, event, tc.payload)
			record := common.GetUpstreamBusinessError(c)
			if tc.absent {
				assert.Nil(t, record)
				return
			}
			require.NotNil(t, record)
			assert.Equal(t, http.StatusOK, record.HTTPStatus)
			assert.Equal(t, event, record.Event)
			assert.Equal(t, tc.message, record.Error.Message)
			assert.Equal(t, tc.code, string(record.Error.Code))
			if tc.reported == 0 {
				assert.Nil(t, record.ReportedStatusCode)
			} else {
				assert.Equal(t, common.GetPointer(tc.reported), record.ReportedStatusCode)
			}
			var saved common.UpstreamBusinessError
			require.NoError(t, common.UnmarshalJsonStr(record.LogContent("billing note"), &saved))
			assert.Equal(t, "billing note", saved.Note)
			assert.Empty(t, record.Note)
		})
	}
}

func TestUpstreamBusinessErrorSanitizationAndLifetime(t *testing.T) {
	t.Run("sanitize every retained field", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("key", "fixture-client-value")
		common.SetContextKey(c, constant.ContextKeyChannelKey, "fixture-channel-value")
		problem := map[string]string{}
		for _, name := range []string{"type", "code", "message", "param", "status"} {
			problem[name] = "fixture-client-value fixture-channel-value data:image/png;base64,aGVsbG8="
		}
		common.CaptureUpstreamBusinessError(c, 200, "error", common.GetJsonString(map[string]any{"error": problem}))
		record := common.GetUpstreamBusinessError(c)
		require.NotNil(t, record)
		content := record.LogContent("")
		assert.NotContains(t, content, "fixture-client-value")
		assert.NotContains(t, content, "fixture-channel-value")
		assert.NotContains(t, content, "aGVsbG8=")
		assert.Contains(t, content, "BASE64_REDACTED")
	})
	t.Run("bounded UTF-8 and first error preserved", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		problem := map[string]string{}
		for _, name := range []string{"type", "code", "message", "param", "status"} {
			problem[name] = strings.Repeat("中😊", 400)
		}
		common.CaptureUpstreamBusinessError(c, 200, "error", common.GetJsonString(map[string]any{"error": problem}))
		record := common.GetUpstreamBusinessError(c)
		require.NotNil(t, record)
		assert.True(t, record.Truncated)
		assert.LessOrEqual(t, len(record.Error.Message), 2048)
		for _, field := range []string{record.Error.Type, record.Error.Param, record.Error.Status} {
			assert.LessOrEqual(t, len(field), 128)
			assert.True(t, utf8.ValidString(field))
		}
		assert.True(t, utf8.ValidString(record.Error.Message))
		common.CaptureUpstreamBusinessError(c, 200, "error", `{"error":"second failure"}`)
		assert.Same(t, record, common.GetUpstreamBusinessError(c))
		common.ClearUpstreamBusinessError(c)
		assert.Nil(t, common.GetUpstreamBusinessError(c))
		common.CaptureUpstreamBusinessError(c, 200, "error", `{"error":"new attempt"}`)
		assert.Equal(t, "new attempt", common.GetUpstreamBusinessError(c).Error.Message)
	})
	t.Run("concurrent requests retain independent errors", func(t *testing.T) {
		var workers sync.WaitGroup
		for index := range 8 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				message := fmt.Sprintf("request-%d", index)
				common.CaptureUpstreamBusinessError(c, 200, "error", common.GetJsonString(map[string]any{"error": message}))
				if assert.NotNil(t, common.GetUpstreamBusinessError(c)) {
					assert.Equal(t, message, common.GetUpstreamBusinessError(c).Error.Message)
				}
			}()
		}
		workers.Wait()
	})
}

func TestRequestTraceBusinessErrorProtocolMatrix(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	service.InitHttpClient()
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"off"}`))
	type handler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	claudeJSON := func(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
		return claude.ClaudeHandler(c, resp, info)
	}
	claudeStream := func(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
		return claude.ClaudeStreamHandler(c, resp, info)
	}
	claudeResponses := func(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
		return claude.ClaudeResponsesStreamHandler(c, resp, info)
	}
	formats := []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, types.RelayFormatClaude, types.RelayFormatGemini}
	jsonHandlers := map[types.RelayFormat][]handler{
		types.RelayFormatOpenAI:          {openai.OpenaiHandler, openai.OaiChatToResponsesHandler, openai.OpenaiHandler, openai.OpenaiHandler},
		types.RelayFormatOpenAIResponses: {openai.OaiResponsesToChatHandler, openai.OaiResponsesHandler, openai.OaiResponsesToChatHandler, openai.OaiResponsesToChatHandler},
		types.RelayFormatClaude:          {claudeJSON, claudeJSON, claudeJSON, claudeJSON},
		types.RelayFormatGemini:          {gemini.GeminiChatHandler, gemini.GeminiResponsesHandler, gemini.GeminiChatHandler, gemini.GeminiTextGenerationHandler},
	}
	streamHandlers := map[types.RelayFormat][]handler{
		types.RelayFormatOpenAI:          {openai.OaiStreamHandler, openai.OaiChatToResponsesStreamHandler, openai.OaiStreamHandler, openai.OaiStreamHandler},
		types.RelayFormatOpenAIResponses: {openai.OaiResponsesToChatStreamHandler, openai.OaiResponsesStreamHandler, openai.OaiResponsesToChatStreamHandler, openai.OaiResponsesToChatStreamHandler},
		types.RelayFormatClaude:          {claudeStream, claudeResponses, claudeStream, claudeStream},
		types.RelayFormatGemini:          {gemini.GeminiChatStreamHandler, gemini.GeminiResponsesStreamHandler, gemini.GeminiChatStreamHandler, gemini.GeminiTextGenerationStreamHandler},
	}
	for _, upstream := range formats {
		for clientIndex, client := range formats {
			for _, stream := range []bool{false, true} {
				for _, typed := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s_to_%s/stream=%t/typed=%t", upstream, client, stream, typed), func(t *testing.T) {
						problem := map[string]any{"code": "upstream_timeout", "message": "provider failed", "status_code": 429}
						if typed {
							problem["type"] = "server_error"
						}
						event := "error"
						payload := common.GetJsonString(map[string]any{"error": problem})
						if upstream == types.RelayFormatClaude {
							payload = common.GetJsonString(map[string]any{"type": "error", "error": problem})
						}
						if upstream == types.RelayFormatOpenAIResponses {
							if stream {
								event = "response.failed"
								payload = common.GetJsonString(map[string]any{"type": event, "response": map[string]any{"status": "failed", "error": problem}})
							} else {
								payload = common.GetJsonString(map[string]any{"status": "failed", "error": problem})
							}
						}
						contentType := "application/json"
						handle := jsonHandlers[upstream][clientIndex]
						if stream {
							contentType, handle = "text/event-stream", streamHandlers[upstream][clientIndex]
							payload = "data: " + payload + "\n\n"
						}
						var calls atomic.Int32
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
							calls.Add(1)
							w.Header().Set("Content-Type", contentType)
							_, _ = io.WriteString(w, payload)
						}))
						t.Cleanup(server.Close)
						path, body := "/v1/chat/completions", requestTraceProtocolRequest(client)
						app := gin.New()
						app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
						var finished *gin.Context
						app.POST(path, func(c *gin.Context) {
							finished = c
							c.Set("id", 11)
							c.Set("username", "trace-owner")
							info := &relaycommon.RelayInfo{RelayFormat: client, FinalRequestRelayFormat: upstream, IsStream: stream, RequestURLPath: path, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelId: 7, ChannelBaseUrl: server.URL, UpstreamModelName: "mock-model"}}
							info.InitRequestConversionChain()
							info.EnsureClaudeConvertInfo()
							adaptor := &openai.Adaptor{}
							adaptor.Init(info)
							resp, err := channel.DoApiRequest(adaptor, c, info, strings.NewReader(body))
							require.NoError(t, err)
							defer resp.Body.Close()
							assert.Equal(t, 200, resp.StatusCode)
							_, relayError := handle(c, info, resp)
							record := common.GetUpstreamBusinessError(c)
							require.NotNil(t, record)
							assert.Equal(t, event, record.Event)
							if relayError != nil {
								model.RecordErrorLog(c, 11, 7, "mock-model", "fixture-token", "existing error", 1, 1, stream, "default", model.NewLogOther())
							} else {
								model.RecordConsumeLog(c, 11, model.RecordConsumeLogParams{ChannelId: 7, ModelName: "mock-model", PromptTokens: 10, CompletionTokens: 9, Quota: 17, Content: "existing billing note", Other: model.NewLogOther()})
							}
						})
						recorder := httptest.NewRecorder()
						app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
						assert.EqualValues(t, 1, calls.Load(), "diagnostic capture must not issue an extra request")
						assert.Nil(t, common.GetUpstreamBusinessError(finished))
						var row model.Log
						require.NoError(t, db.Where("request_id = ?", recorder.Header().Get(common.RequestIdKey)).First(&row).Error)
						var saved common.UpstreamBusinessError
						require.NoError(t, common.UnmarshalJsonStr(row.Content, &saved))
						assert.Equal(t, 200, saved.HTTPStatus)
						assert.Equal(t, common.GetPointer(429), saved.ReportedStatusCode)
						assert.Equal(t, `"upstream_timeout"`, string(saved.Error.Code))
						assert.Equal(t, "provider failed", saved.Error.Message)
						if row.Type == model.LogTypeConsume {
							assert.Equal(t, "existing billing note", saved.Note)
							assert.Equal(t, 17, row.Quota)
						}
						owned, total, err := model.GetUserLogs(11, model.LogTypeUnknown, 0, 0, "", "", 0, 20, "", row.RequestId, "")
						require.NoError(t, err)
						require.EqualValues(t, 1, total)
						require.Len(t, owned, 1)
						assert.Equal(t, row.Content, owned[0].Content)
						_, total, err = model.GetUserLogs(12, model.LogTypeUnknown, 0, 0, "", "", 0, 20, "", row.RequestId, "")
						require.NoError(t, err)
						assert.Zero(t, total)
					})
				}
			}
		}
	}
}

func TestRequestTraceBusinessErrorSettlement(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	require.NoError(t, db.Create(&model.Channel{Id: 7, Name: "business-error-test"}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 21, UserId: 11, Key: "fixture-client-value", Status: common.TokenStatusEnabled, RemainQuota: 1000000}).Error)
	previousBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = false
	t.Cleanup(func() {
		// 退款还会异步更新缓存，排空后才能还原数据库和全局开关。
		require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 5*time.Second, 5*time.Millisecond)
		common.BatchUpdateEnabled = previousBatch
	})
	var totalQuota, totalTokens, requests int
	for _, level := range []string{"off", "errors", "all"} {
		for _, tc := range []struct {
			name          string
			failure       bool
			usage         *dto.Usage
			expectedQuota int
		}{
			{name: "normal success", usage: &dto.Usage{PromptTokens: 10, CompletionTokens: 9, TotalTokens: 19}, expectedQuota: 19},
			{name: "failure with billable partial usage", failure: true, usage: &dto.Usage{PromptTokens: 10, CompletionTokens: 9, TotalTokens: 19}, expectedQuota: 19},
			{name: "failure with explicit zero usage", failure: true, usage: &dto.Usage{}},
			{name: "failure without usage", failure: true},
		} {
			t.Run(level+"/"+tc.name, func(t *testing.T) {
				require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(common.RequestTraceConfig{Level: level, SuccessSampleRate: 0})))
				require.NoError(t, db.Model(&model.User{}).Where("id = ?", 11).Updates(map[string]any{"quota": 1000000, "used_quota": 0, "request_count": 0}).Error)
				require.NoError(t, db.Model(&model.Token{}).Where("id = ?", 21).Updates(map[string]any{"remain_quota": 1000000, "used_quota": 0}).Error)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
				c.Set("id", 11)
				c.Set("username", "trace-owner")
				c.Set("key", "fixture-client-value")
				c.Set(common.RequestIdKey, fmt.Sprintf("billing-%s-%d", level, requests))
				common.SetContextKey(c, constant.ContextKeyChannelKey, "fixture-channel-value")
				info := &relaycommon.RelayInfo{UserId: 11, TokenId: 21, TokenKey: "fixture-client-value", UsingGroup: "default", OriginModelName: "mock-model", ForcePreConsume: true, StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7, UpstreamModelName: "mock-model"}}
				info.UserSetting.BillingPreference = "wallet_only"
				info.PriceData.ModelRatio, info.PriceData.CompletionRatio, info.PriceData.GroupRatioInfo.GroupRatio = 1, 1, 1
				session, billingError := service.NewBillingSession(c, info, 100)
				require.Nil(t, billingError)
				require.NotNil(t, session)
				info.Billing = session
				assert.Equal(t, 100, session.GetPreConsumedQuota())
				if tc.failure {
					common.CaptureUpstreamBusinessError(c, 200, "response.failed", `{"type":"response.failed","response":{"error":{"code":"upstream_timeout","message":"provider failed"}}}`)
				}
				note := "existing billing note"
				if tc.failure {
					note += " fixture-client-value fixture-channel-value data:image/png;base64,aGVsbG8="
				}
				service.PostTextConsumeQuota(c, info, tc.usage, []string{note})
				assert.False(t, session.NeedsRefund())
				session.Refund(c)
				var user model.User
				var token model.Token
				require.NoError(t, db.First(&user, 11).Error)
				require.NoError(t, db.First(&token, 21).Error)
				assert.Equal(t, 1000000-tc.expectedQuota, user.Quota)
				assert.Equal(t, 1000000-tc.expectedQuota, token.RemainQuota)
				var row model.Log
				require.NoError(t, db.Where("request_id = ?", c.GetString(common.RequestIdKey)).First(&row).Error)
				assert.Equal(t, model.LogTypeConsume, row.Type)
				assert.Equal(t, tc.expectedQuota, row.Quota)
				if tc.failure {
					var saved common.UpstreamBusinessError
					require.NoError(t, common.UnmarshalJsonStr(row.Content, &saved))
					assert.Contains(t, saved.Note, "existing billing note")
					assert.NotContains(t, row.Content, "fixture-client-value")
					assert.NotContains(t, row.Content, "fixture-channel-value")
					assert.NotContains(t, row.Content, "aGVsbG8=")
				} else {
					assert.Equal(t, note, row.Content)
				}
				totalQuota += tc.expectedQuota
				totalTokens += row.PromptTokens + row.CompletionTokens
				requests++
				stat, err := model.SumUsedQuota(model.LogTypeConsume, 0, 0, "", "", "", 0, "")
				require.NoError(t, err)
				assert.Equal(t, totalQuota, stat.Quota)
				assert.Equal(t, requests, stat.Rpm)
				assert.Equal(t, totalTokens, stat.Tpm)
			})
		}
	}
	t.Run("existing error return still refunds reservation", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		info := &relaycommon.RelayInfo{UserId: 11, TokenId: 21, TokenKey: "fixture-client-value", ForcePreConsume: true, OriginModelName: "mock-model", ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7, UpstreamModelName: "mock-model"}}
		info.UserSetting.BillingPreference = "wallet_only"
		session, billingError := service.NewBillingSession(c, info, 100)
		require.Nil(t, billingError)
		_, relayError := openai.OpenaiHandler(c, info, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"failed"}}`))})
		require.NotNil(t, relayError)
		assert.Equal(t, 200, relayError.StatusCode)
		require.NotNil(t, common.GetUpstreamBusinessError(c))
		session.Refund(c)
		require.Eventually(t, func() bool {
			var user model.User
			var token model.Token
			return db.First(&user, 11).Error == nil && db.First(&token, 21).Error == nil && user.Quota == 1000000 && token.RemainQuota == 1000000
		}, 3*time.Second, 10*time.Millisecond)
	})
}

func TestRequestTraceBusinessErrorRetryCleanup(t *testing.T) {
	setupRequestTraceDatabase(t)
	service.InitHttpClient()
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"off"}`))
	for _, nextFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("next failure=%t", nextFailure), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			defer server.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			common.CaptureUpstreamBusinessError(c, 200, "error", `{"error":"first attempt"}`)
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, RetryIndex: 1, RequestURLPath: c.Request.URL.Path, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelBaseUrl: server.URL, UpstreamModelName: "mock-model"}}
			adaptor := &openai.Adaptor{}
			adaptor.Init(info)
			resp, err := channel.DoApiRequest(adaptor, c, info, strings.NewReader(`{}`))
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Nil(t, common.GetUpstreamBusinessError(c))
			if nextFailure {
				common.CaptureUpstreamBusinessError(c, 200, "error", `{"error":"second attempt"}`)
				assert.Equal(t, "second attempt", common.GetUpstreamBusinessError(c).Error.Message)
			}
		})
	}
}

func TestUpstreamBusinessErrorTerminalEvents(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, tc := range []struct {
		name, payload, event string
		format               types.RelayFormat
		handle               func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
		absent               bool
	}{
		{name: "Chat flat error", payload: `{"type":"error","code":"timeout","message":"provider failed"}`, event: "error", format: types.RelayFormatOpenAI, handle: openai.OaiStreamHandler},
		{name: "Chat flat error converted to Responses", payload: `{"type":"error","code":"timeout","message":"provider failed"}`, event: "error", format: types.RelayFormatOpenAIResponses, handle: openai.OaiChatToResponsesStreamHandler},
		{name: "Responses flat error", payload: `{"type":"error","code":"timeout","message":"provider failed"}`, event: "error", format: types.RelayFormatOpenAIResponses, handle: openai.OaiResponsesStreamHandler},
		{name: "Responses flat error converted to Chat", payload: `{"type":"error","code":"timeout","message":"provider failed"}`, event: "error", format: types.RelayFormatOpenAI, handle: openai.OaiResponsesToChatStreamHandler},
		{name: "Responses response.error", payload: `{"type":"response.error","code":"timeout","message":"provider failed"}`, event: "response.error", format: types.RelayFormatOpenAIResponses, handle: openai.OaiResponsesStreamHandler},
		{name: "Responses response.error converted to Chat", payload: `{"type":"response.error","code":"timeout","message":"provider failed"}`, event: "response.error", format: types.RelayFormatOpenAI, handle: openai.OaiResponsesToChatStreamHandler},
		{name: "Responses failed without error details", payload: `{"type":"response.failed","response":{"status":"failed","error":null}}`, event: "response.failed", format: types.RelayFormatOpenAIResponses, handle: openai.OaiResponsesStreamHandler},
		{name: "Responses buffered failure converted to Chat", payload: `{"type":"response.failed","response":{"error":{"code":"timeout","message":"provider failed"}}}`, event: "response.failed", format: types.RelayFormatOpenAI, handle: openai.OaiResponsesToChatBufferedStreamHandler},
		{name: "Claude flat error", payload: `{"type":"error","code":"timeout","message":"provider failed"}`, event: "error", format: types.RelayFormatClaude, handle: func(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeStreamHandler(c, resp, info)
		}},
		{name: "Responses output limit", payload: `{"type":"response.incomplete","response":{"status":"incomplete","error":null,"incomplete_details":{"reason":"max_output_tokens"}}}`, absent: true, format: types.RelayFormatOpenAIResponses, handle: openai.OaiResponsesStreamHandler},
		{name: "Chat null placeholder", payload: `{"choices":[],"error":null}`, absent: true, format: types.RelayFormatOpenAI, handle: openai.OaiStreamHandler},
		{name: "Chat false placeholder", payload: `{"choices":[],"error":false}`, absent: true, format: types.RelayFormatOpenAI, handle: openai.OaiStreamHandler},
		{name: "Gemini nested tool error", payload: `{"candidates":[{"content":{"parts":[{"functionResponse":{"name":"tool","response":{"error":"tool failed"}}}]},"finishReason":"STOP"}]}`, absent: true, format: types.RelayFormatGemini, handle: gemini.GeminiTextGenerationStreamHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{RelayFormat: tc.format, IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "mock-model"}}
			info.InitRequestConversionChain()
			info.EnsureClaudeConvertInfo()
			_, _ = tc.handle(c, info, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + tc.payload + "\n\n"))})
			record := common.GetUpstreamBusinessError(c)
			if tc.absent {
				assert.Nil(t, record)
				return
			}
			require.NotNil(t, record)
			assert.Equal(t, tc.event, record.Event)
			assert.Equal(t, 200, record.HTTPStatus)
			assert.Nil(t, record.ReportedStatusCode)
			if strings.Contains(tc.payload, "provider failed") {
				assert.Equal(t, "provider failed", record.Error.Message)
			}
		})
	}
}
