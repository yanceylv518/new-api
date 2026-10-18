package router

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/internal/requesttracetest"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupRequestTraceDatabase(t testing.TB) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousPath, previousMaster := common.SQLitePath, common.IsMasterNode
	previousRedis, previousMemory, previousConsume := common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled
	previousConfig := common.GetRequestTraceConfig()
	common.SQLitePath = filepath.Join(t.TempDir(), "trace.sqlite")
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled = false, false, false, true
	dsn := "local"
	if os.Getenv("REQUEST_TRACE_TEST_DSN") != "" {
		dsn = requesttracetest.MySQLDSN(t)
	}
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	common.SetLogDatabaseType(common.MainDatabaseType())
	db := model.DB
	db.Logger = logger.Default.LogMode(logger.Silent)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	if common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		sqlDB.SetMaxOpenConns(16)
	} else {
		sqlDB.SetMaxOpenConns(1)
	}
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.RequestTrace{}, &model.Option{}, &model.AuditLog{}, &model.UserSession{}))
	require.NoError(t, db.Create(&model.User{Id: 11, Username: "trace-owner", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "trace-owner"}).Error)
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.SQLitePath, common.IsMasterNode = previousPath, previousMaster
		common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled = previousRedis, previousMemory, previousConsume
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previousConfig)))
	})
	return db
}

func TestRequestTraceProtocolMetadata(t *testing.T) {
	cases := []struct {
		name, body, contentType, reason string
		text, reasoning, tools          int64
		done, failed                    bool
	}{
		{"openai-json", `{"id":"chat-id","model":"mock-model","choices":[{"index":0,"message":{"content":"A中😊","reasoning_content":"abc","tool_calls":[{"id":"call-1","function":{"name":"tool","arguments":"private-tool-arguments"}}]},"finish_reason":"stop"}],"usage":{"completion_tokens":3}}`, "application/json", "stop", 3, 3, 1, true, false},
		{"completions-json", `{"choices":[{"text":"abc","finish_reason":"length"}]}`, "application/json", "length", 3, 0, 0, true, false},
		{"openai-null-tools-json", `{"choices":[{"message":{"content":"abc","tool_calls":null,"function_call":null},"finish_reason":"stop"}]}`, "application/json", "stop", 3, 0, 0, true, false},
		{"openai-empty-function-json", `{"choices":[{"message":{"content":"abc","function_call":{"name":"","arguments":""}},"finish_reason":"stop"}]}`, "application/json", "stop", 3, 0, 0, true, false},
		{"openai-tool-with-empty-legacy-json", `{"choices":[{"message":{"tool_calls":[{"id":"call-1","function":{"name":"tool","arguments":"{}"}}],"function_call":{"name":"","arguments":""}},"finish_reason":"tool_calls"}]}`, "application/json", "tool_calls", 0, 0, 1, true, false},
		{"openai-empty-function-sse", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"abc\",\"tool_calls\":null,\"function_call\":null}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"function_call\":{\"name\":\"\",\"arguments\":\"\"}},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "text/event-stream", "stop", 3, 0, 0, true, false},
		{"claude-json", `{"type":"message","id":"msg-1","content":[{"type":"text","text":"你好"},{"type":"thinking","thinking":"abc"},{"type":"tool_use","input":{"secret":"private-tool-arguments"}}],"stop_reason":"end_turn","usage":{"output_tokens":4}}`, "application/json", "end_turn", 2, 3, 1, true, false},
		{"gemini-json", `{"candidates":[{"content":{"parts":[{"text":"你好"},{"text":"abc","thought":true},{"functionCall":{"name":"tool","args":{"secret":"private-tool-arguments"}}}]},"finishReason":"STOP"}],"usageMetadata":{"thoughtsTokenCount":2}}`, "application/json", "STOP", 2, 3, 1, true, false},
		{"responses-json", `{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"你好"}]},{"type":"reasoning","summary":[{"text":"abc"}]},{"type":"function_call","arguments":"private-tool-arguments"}],"usage":{"output_tokens":4}}`, "application/json", "completed", 2, 3, 1, true, false},
		{"openai-sse", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"private-tool-arguments\"}}]}}]}\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"中\",\"reasoning_content\":\"abc\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"more\"}}]}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\ndata: [DONE]\n\n", "text/event-stream", "stop", 2, 3, 1, true, false},
		{"claude-sse", "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"usage\":{\"input_tokens\":3}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"你好\",\"thinking\":\"abc\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"input\":{}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "text/event-stream", "end_turn", 2, 3, 1, true, false},
		{"gemini-sse", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你好\"},{\"text\":\"abc\",\"thought\":true},{\"functionCall\":{\"name\":\"tool\",\"args\":{}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"thoughtsTokenCount\":2}}\n\n", "text/event-stream", "STOP", 2, 3, 1, false, false},
		{"responses-sse", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"abc\"}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"你好\"}]}]}}\n\n", "text/event-stream", "completed", 2, 3, 1, true, false},
		{"responses-stream-error", "data: {\"type\":\"error\",\"code\":\"overloaded\",\"message\":\"private-error-original\"}\n\n", "text/event-stream", "", 0, 0, 0, false, true},
		{"responses-stream-failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"overloaded\",\"message\":\"private-error-original\"}}}\n\n", "text/event-stream", "failed", 0, 0, 0, true, true},
		{"nested-invalid-metadata", `{"id":{"text":"private-message-body"},"error":{"type":{"message":"private-error-original"},"code":{"message":"private-error-original"},"message":"private-error-original"}}`, "application/json", "", 0, 0, 0, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunkSize := range []int{1, 7, len(tc.body)} {
				client := common.NewTraceBodyObserver(time.Now())
				observer := client.NewRelatedObserver()
				for offset := 0; offset < len(tc.body); offset += chunkSize {
					observer.Observe([]byte(tc.body[offset:min(offset+chunkSize, len(tc.body))]), tc.contentType)
				}
				observer.Finish()
				response, upstream, reason := observer.Snapshot()
				assert.Equal(t, tc.text, response.ContentCharacters, "chunk size %d", chunkSize)
				assert.Equal(t, tc.reasoning, response.ReasoningCharacters)
				assert.Equal(t, tc.tools, response.ToolCalls)
				assert.Equal(t, tc.done, response.Done)
				assert.Equal(t, tc.failed, response.Error)
				assert.EqualValues(t, len(tc.body), observer.Size())
				if tc.reason != "" {
					require.NotNil(t, reason)
					assert.Equal(t, tc.reason, *reason)
				}
				encoded := common.GetJsonString(common.RequestTraceData{AdminInfo: &common.RequestTraceAdmin{Upstream: upstream, Response: response}})
				for _, secret := range []string{"private-message-body", "private-tool-arguments", "private-error-original", "你好", "A中😊"} {
					assert.NotContains(t, encoded, secret)
				}
				client.Observe([]byte(tc.body), tc.contentType)
				client.Finish()
				forwarded, forwardedUpstream, forwardedReason := client.Snapshot()
				response.LastChunkMilliseconds, forwarded.LastChunkMilliseconds = 0, 0
				assert.Equal(t, response, forwarded)
				assert.Equal(t, upstream, forwardedUpstream)
				assert.Equal(t, reason, forwardedReason)
			}
		})
	}
}

func TestRequestTraceSharedMetadataPreservesIndependentResults(t *testing.T) {
	t.Run("workspace reuse preserves cached fields and optional values", func(t *testing.T) {
		client := common.NewTraceBodyObserver(time.Now())
		upstream := client.NewRelatedObserver()
		cachedReader := client.NewRelatedObserver()
		first := []byte("data: " + `{"id":"first","created":42,"choices":[{"index":0,"delta":{"content":"A","tool_calls":[{"index":0,"function":{"name":"first","arguments":"{}"}}]}}],"usage":{"completion_tokens_details":{"reasoning_tokens":3}}}` + "\n\n")
		second := []byte("data: " + `{"choices":[{"index":0,"delta":{"content":"B","new_field":1},"finish_reason":"length"}]}` + "\n\n")
		upstream.Observe(first, "text/event-stream")
		upstream.Observe(second, "text/event-stream")
		upstream.Finish()
		response, _, _ := upstream.Snapshot()
		require.Equal(t, []string{"content", "tool_calls", "new_field"}, response.Keys["message"])
		assert.EqualValues(t, 1, response.ToolCalls)
		assert.EqualValues(t, 1, response.ToolCallsByName["first"])
		response.Keys["message"][0] = "changed-by-reader"

		cachedReader.Observe(first, "text/event-stream")
		cachedReader.Finish()
		cachedResponse, cachedInfo, cachedReason := cachedReader.Snapshot()
		assert.Equal(t, []string{"id", "created", "choices", "usage"}, cachedResponse.Keys["root"])
		assert.Equal(t, []string{"content", "tool_calls"}, cachedResponse.Keys["message"])
		require.NotNil(t, cachedResponse.ReasoningTokens)
		assert.EqualValues(t, 3, *cachedResponse.ReasoningTokens)
		require.NotNil(t, cachedInfo.Created)
		assert.EqualValues(t, 42, *cachedInfo.Created)
		assert.Nil(t, cachedReason)

		client.Observe(second, "text/event-stream")
		client.Finish()
		clientResponse, clientInfo, clientReason := client.Snapshot()
		assert.Equal(t, []string{"choices"}, clientResponse.Keys["root"])
		assert.Equal(t, []string{"content", "new_field"}, clientResponse.Keys["message"])
		assert.Nil(t, clientResponse.ReasoningTokens)
		assert.Nil(t, clientInfo.Created)
		assert.Empty(t, clientInfo.ResponseID)
		assert.Zero(t, clientResponse.ToolCalls)
		require.NotNil(t, clientReason)
		assert.Equal(t, "length", *clientReason)
	})
	t.Run("field sets merge choices and retain empty objects", func(t *testing.T) {
		client := common.NewTraceBodyObserver(time.Now())
		upstream := client.NewRelatedObserver()
		upstream.Observe([]byte("data: "+`{"choices":[{"index":0,"delta":{"content":"A"}},{"index":1,"delta":{"reasoning_content":"B"},"finish_reason":"stop"}],"usage":{}}`+"\n\n"), "text/event-stream")
		upstream.Finish()
		response, _, _ := upstream.Snapshot()
		assert.Equal(t, []string{"index", "delta", "finish_reason"}, response.Keys["choices"])
		assert.Equal(t, []string{"content", "reasoning_content"}, response.Keys["message"])
		assert.Contains(t, response.Keys, "usage")
		assert.Nil(t, response.Keys["usage"])
		assert.EqualValues(t, 1, response.ContentCharacters)
		assert.EqualValues(t, 1, response.ReasoningCharacters)

		client.Observe([]byte("data: "+`{"choices":[{"delta":{}}]}`+"\n\n"), "text/event-stream")
		client.Finish()
		response, _, _ = client.Snapshot()
		assert.Contains(t, response.Keys, "message")
		assert.Nil(t, response.Keys["message"])
		assert.NotContains(t, response.Keys, "usage")
	})
	t.Run("snapshot values do not change another observer", func(t *testing.T) {
		client := common.NewTraceBodyObserver(time.Now())
		upstream := client.NewRelatedObserver()
		body := []byte(`{"created":42,"choices":[{"message":{"content":"hello"},"finish_reason":"stop"}],"usage":{"completion_tokens_details":{"reasoning_tokens":3}}}`)
		upstream.Observe(body, "application/json")
		upstream.Finish()
		response, info, reason := upstream.Snapshot()
		require.NotNil(t, info.Created)
		require.NotNil(t, response.ReasoningTokens)
		require.NotNil(t, reason)
		*info.Created, *response.ReasoningTokens, *reason = 99, 99, "changed"
		client.Observe(body, "application/json")
		client.Finish()
		response, info, reason = client.Snapshot()
		require.NotNil(t, info.Created)
		require.NotNil(t, response.ReasoningTokens)
		require.NotNil(t, reason)
		assert.EqualValues(t, 42, *info.Created)
		assert.EqualValues(t, 3, *response.ReasoningTokens)
		assert.Equal(t, "stop", *reason)
	})
	t.Run("converted client content retains independent metadata", func(t *testing.T) {
		client := common.NewTraceBodyObserver(time.Now())
		upstream := client.NewRelatedObserver()
		upstream.Observe([]byte(`{"id":"upstream-id","model":"upstream-model","choices":[{"message":{"content":"original"},"finish_reason":"stop"}]}`), "application/json")
		upstream.Finish()
		client.Observe([]byte(`{"type":"message","id":"client-id","content":[{"type":"text","text":"转换"}],"stop_reason":"end_turn"}`), "application/json")
		client.Finish()
		upstreamResponse, upstreamInfo, upstreamReason := upstream.Snapshot()
		clientResponse, clientInfo, clientReason := client.Snapshot()
		assert.Equal(t, "upstream-id", upstreamInfo.ResponseID)
		assert.Equal(t, "upstream-model", upstreamInfo.Model)
		assert.EqualValues(t, 8, upstreamResponse.ContentCharacters)
		require.NotNil(t, upstreamReason)
		assert.Equal(t, "stop", *upstreamReason)
		assert.Equal(t, "client-id", clientInfo.ResponseID)
		assert.Empty(t, clientInfo.Model)
		assert.EqualValues(t, 2, clientResponse.ContentCharacters)
		require.NotNil(t, clientReason)
		assert.Equal(t, "end_turn", *clientReason)
	})
	t.Run("cached event honors prior error type and explicit empty identifier", func(t *testing.T) {
		client := common.NewTraceBodyObserver(time.Now())
		upstream := client.NewRelatedObserver()
		upstream.Observe([]byte("data: {\"id\":\"initial\",\"error\":{\"type\":\"prior-type\"}}\n\n"), "text/event-stream")
		event := []byte("data: {\"id\":\"\",\"error\":{\"status\":\"OVERLOADED\"}}\n\n")
		upstream.Observe(event, "text/event-stream")
		client.Observe(event, "text/event-stream")
		upstream.Finish()
		client.Finish()
		_, upstreamInfo, _ := upstream.Snapshot()
		_, clientInfo, _ := client.Snapshot()
		assert.Empty(t, upstreamInfo.ResponseID)
		assert.Equal(t, "prior-type", upstreamInfo.ErrorType)
		assert.Equal(t, "OVERLOADED", clientInfo.ErrorType)
	})
	t.Run("long varying stream retains earlier counts on late error", func(t *testing.T) {
		client := common.NewTraceBodyObserver(time.Now())
		upstream := client.NewRelatedObserver()
		var stream strings.Builder
		for index := range 96 {
			fmt.Fprintf(&stream, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A中%02d\",\"tool_calls\":[{\"index\":0}]}}]}\n\n", index)
		}
		stream.WriteString("data: {\"error\":{\"type\":\"overloaded\",\"message\":\"private-late-error\"}}\n\n")
		body := stream.String()
		upstream.Observe([]byte(body), "text/event-stream")
		upstream.Finish()
		for offset := 0; offset < len(body); offset += 7 {
			client.Observe([]byte(body[offset:min(offset+7, len(body))]), "text/event-stream")
		}
		client.Finish()
		for _, observer := range []*common.TraceBodyObserver{upstream, client} {
			response, info, _ := observer.Snapshot()
			assert.True(t, response.Error)
			assert.False(t, response.MetadataIncomplete)
			assert.EqualValues(t, 384, response.ContentCharacters)
			assert.EqualValues(t, 97, response.Chunks)
			assert.EqualValues(t, 1, response.ToolCalls)
			assert.Equal(t, "overloaded", info.ErrorType)
			assert.NotContains(t, common.GetJsonString(response), "private-late-error")
		}
	})
}

func TestRequestTraceCaptureBoundaries(t *testing.T) {
	t.Run("invalid tool index is not retained as metadata", func(t *testing.T) {
		for _, payload := range []string{
			`{"type":"content_block_start","index":{"text":"private-tool-body"},"content_block":{"type":"tool_use"}}`,
			`{"type":"response.output_item.added","output_index":{"text":"private-tool-body"},"item":{"type":"function_call"}}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":{"text":"private-tool-body"}}]}}]}`,
		} {
			observer := common.NewTraceBodyObserver(time.Now())
			observer.Observe([]byte("data: "+payload+"\n\n"), "text/event-stream")
			observer.Finish()
			response, _, _ := observer.Snapshot()
			assert.True(t, response.MetadataIncomplete)
			assert.EqualValues(t, 1, response.ToolCalls)
			assert.NotContains(t, common.GetJsonString(response), "private-tool-body")
		}
	})
	t.Run("response over limit", func(t *testing.T) {
		observer := common.NewTraceBodyObserver(time.Now())
		observer.Observe([]byte(strings.Repeat("x", common.TraceMetadataMaxBytes+1)), "application/json")
		observer.Finish()
		response, _, _ := observer.Snapshot()
		assert.True(t, response.MetadataIncomplete)
		assert.Zero(t, response.ContentCharacters)
	})
	t.Run("oversized stream event recovers", func(t *testing.T) {
		observer := common.NewTraceBodyObserver(time.Now())
		observer.Observe([]byte("data: "+strings.Repeat("x", common.TraceMetadataMaxBytes+1)+"\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"中\"}}]}\n\ndata: [DONE]\n\n"), "text/event-stream")
		observer.Finish()
		response, _, _ := observer.Snapshot()
		assert.True(t, response.MetadataIncomplete)
		assert.EqualValues(t, 1, response.ContentCharacters)
		assert.True(t, response.Done)
	})
	t.Run("independent request reader and body exclusion", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"system","content":"private-system"},{"role":"user","content":[{"type":"text","text":"你好"},{"type":"image_url","image_url":{"url":"data:image/png;base64,private-media"}}]}],"tools":[{"function":{"description":"private-tool-definition"}}],"tool_choice":{"type":"function","function":{"name":"private-tool-name"}},"temperature":0,"max_tokens":0,"stream_options":{"include_usage":true}}`)
		storage, err := common.CreateBodyStorage(body)
		require.NoError(t, err)
		defer storage.Close()
		_, err = io.ReadAll(storage)
		require.NoError(t, err)
		collector := &common.RequestTraceCollector{}
		collector.CaptureRequest(storage, true)
		require.NotNil(t, collector.Shape)
		assert.EqualValues(t, 2, collector.Shape.Messages)
		assert.EqualValues(t, 1, collector.Shape.Images)
		assert.EqualValues(t, 1, collector.Shape.Tools)
		assert.True(t, collector.Shape.System)
		assert.True(t, collector.Shape.IncludeUsage)
		require.NotNil(t, collector.Shape.Temperature)
		assert.Zero(t, *collector.Shape.Temperature)
		require.NotNil(t, collector.Shape.MaxTokens)
		assert.Zero(t, *collector.Shape.MaxTokens)
		assert.NotContains(t, common.GetJsonString(collector.Shape), "private-")
	})
	t.Run("oversized request and malformed response", func(t *testing.T) {
		storage, err := common.CreateBodyStorage([]byte(strings.Repeat("x", common.TraceMetadataMaxBytes+1)))
		require.NoError(t, err)
		defer storage.Close()
		collector := &common.RequestTraceCollector{}
		collector.CaptureRequest(storage, true)
		assert.True(t, collector.ShapeIncomplete)
		assert.Nil(t, collector.Shape)
		observer := common.NewTraceBodyObserver(time.Now())
		observer.Observe([]byte("{broken"), "application/json")
		observer.Finish()
		response, _, _ := observer.Snapshot()
		assert.True(t, response.MetadataIncomplete)
	})
	t.Run("reading and closing preserve the response", func(t *testing.T) {
		payload := `{"choices":[{"message":{"content":"private-response-body"}}]}`
		body := &common.TraceResponseBody{ReadCloser: io.NopCloser(strings.NewReader(payload)), Observer: common.NewTraceBodyObserver(time.Now()), ContentType: "application/json"}
		got, err := io.ReadAll(body)
		require.NoError(t, err)
		assert.Equal(t, payload, string(got))
		require.NoError(t, body.Close())
		response, _, _ := body.Observer.Snapshot()
		assert.EqualValues(t, len("private-response-body"), response.ContentCharacters)
	})
}

func TestRequestTraceCollectorLifecycle(t *testing.T) {
	previousConfig := common.GetRequestTraceConfig()
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all","anonymous_errors_enabled":false}`))
	t.Cleanup(func() {
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previousConfig)))
	})
	for _, scenario := range []struct {
		name   string
		status int
	}{
		{"success", http.StatusOK},
		{"abort", http.StatusForbidden},
		{"panic", http.StatusInternalServerError},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app := gin.New()
			app.Use(gin.RecoveryWithWriter(io.Discard))
			checked := false
			app.Use(func(c *gin.Context) {
				original := c.Writer
				defer func() {
					assert.Nil(t, common.GetRequestTrace(c))
					assert.Same(t, original, c.Writer)
					checked = true
				}()
				c.Next()
			}, middleware.RequestTrace())
			app.POST("/v1/chat/completions", func(c *gin.Context) {
				require.NotNil(t, common.GetRequestTrace(c))
				if scenario.name == "panic" {
					panic("trace lifecycle")
				}
				if scenario.name == "abort" {
					c.AbortWithStatus(scenario.status)
					return
				}
				c.Data(scenario.status, "application/json", []byte(`{"choices":[{"message":{"content":"A"}}]}`))
			})
			response := httptest.NewRecorder()
			app.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
			assert.Equal(t, scenario.status, response.Code)
			assert.True(t, checked)
		})
	}
}

func TestRequestTraceMiddlewareModes(t *testing.T) {
	cases := []struct {
		name, level                                     string
		status, userID                                  int
		preChannel, anonymous, consume, attempt, cancel bool
		rows                                            int
		detailed                                        bool
	}{
		{"off", "off", 200, 11, true, false, true, true, false, 0, false},
		{"success summary", "errors", 200, 11, true, false, true, true, false, 1, false},
		{"all detailed", "all", 200, 11, true, false, true, true, false, 1, true},
		{"upstream failure", "errors", 502, 11, true, false, true, true, false, 1, true},
		{"pre-channel failure", "errors", 400, 11, true, false, true, false, false, 1, true},
		{"pre-channel disabled", "errors", 400, 11, false, false, true, false, false, 0, false},
		{"pre-consume forbidden", "errors", 403, 11, true, false, true, false, false, 0, false},
		{"anonymous disabled", "all", 401, 0, true, false, true, false, false, 0, false},
		{"anonymous enabled", "all", 401, 0, true, true, true, false, false, 1, true},
		{"consume logging disabled", "all", 200, 11, true, false, false, true, false, 0, false},
		{"client cancelled", "all", 200, 11, true, false, true, true, true, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupRequestTraceDatabase(t)
			common.LogConsumeEnabled = tc.consume
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(common.RequestTraceConfig{Level: tc.level, PreChannelErrors: tc.preChannel, AnonymousErrors: tc.anonymous, SuccessSampleRate: 100})))
			engine := gin.New()
			engine.Use(middleware.RequestId(), middleware.RequestTrace(), middleware.BodyStorageCleanup())
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("id", tc.userID)
				c.Set("username", "trace-owner")
				if tc.name == "pre-consume forbidden" {
					common.SetRequestTraceStage(c, "pre_consume")
				}
				storage, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				_ = storage
				if trace := common.GetRequestTrace(c); trace != nil && tc.attempt {
					attempt := trace.StartAttempt(0, 7, c.Request)
					attempt.Upstream.Status = tc.status
				}
				if tc.status >= 400 {
					c.JSON(tc.status, gin.H{"error": gin.H{"type": "mock_error", "code": "mock_code", "message": "private-error-original"}})
				} else {
					c.JSON(tc.status, gin.H{"choices": []gin.H{{"message": gin.H{"content": "private-response-body"}, "finish_reason": "stop"}}})
				}
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"private-message-body"}]}`))
			request.Header.Set("Content-Type", "application/json")
			if tc.cancel {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, tc.status, recorder.Code)
			var rows []model.RequestTrace
			require.NoError(t, db.Find(&rows).Error)
			require.Len(t, rows, tc.rows)
			if len(rows) == 0 {
				return
			}
			assert.Equal(t, recorder.Header().Get(common.RequestIdKey), rows[0].RequestId)
			assert.Equal(t, tc.userID, rows[0].UserId)
			decoded, err := common.DecodeRequestTraceStorage(rows[0].Data)
			require.NoError(t, err)
			for _, secret := range []string{"private-response-body", "private-message-body", "private-error-original"} {
				assert.NotContains(t, decoded, secret)
			}
			var data common.RequestTraceData
			require.NoError(t, common.UnmarshalJsonStr(decoded, &data))
			assert.Equal(t, tc.status, data.Trace.Status)
			assert.EqualValues(t, recorder.Body.Len(), data.Trace.ResponseBytes)
			assert.Equal(t, tc.detailed, data.AdminInfo != nil)
			if tc.detailed {
				require.NotNil(t, data.AdminInfo.Shape)
			}
		})
	}
}

func TestRequestTraceDetailAuthorizationAndLegacyLogs(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}, AdminInfo: &common.RequestTraceAdmin{ChannelID: 7}})
	rows := []model.RequestTrace{{RequestId: "trace-owned", Attempt: 0, UserId: 11, CreatedAt: 1, Data: data}, {RequestId: "trace-owned", Attempt: 1, UserId: 11, CreatedAt: 2, Data: data}, {RequestId: "trace-anonymous", Attempt: -1, UserId: 0, CreatedAt: 3, Data: data}}
	require.NoError(t, model.RecordRequestTraces(t.Context(), rows))
	require.NoError(t, model.RecordRequestTraces(t.Context(), rows))
	var count int64
	require.NoError(t, db.Model(&model.RequestTrace{}).Count(&count).Error)
	assert.EqualValues(t, 3, count)
	engine := gin.New()
	engine.GET("/api/log/trace", middleware.UserAuth(), controller.GetRequestTrace)
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
		pat := rand.Text()
		user := model.User{Username: fmt.Sprintf("trace-role-%d", role), Role: role, Status: common.UserStatusEnabled, Group: "default", AffCode: fmt.Sprintf("trace-aff-%d", role), AccessToken: &pat, AuthVersion: 1}
		if role == common.RoleCommonUser {
			user.Id = 11
			require.NoError(t, db.Model(&model.User{}).Where("id = ?", 11).Updates(map[string]any{"access_token": pat, "auth_version": 1}).Error)
		} else {
			require.NoError(t, db.Create(&user).Error)
		}
		for _, id := range []string{"trace-owned", "trace-anonymous", "missing"} {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/log/trace?request_id="+id, nil)
			request.Header.Set("Authorization", "Bearer "+pat)
			engine.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var result struct {
				Success bool                     `json:"success"`
				Data    []model.RequestTraceView `json:"data"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
			require.True(t, result.Success)
			if id == "missing" || (role == common.RoleCommonUser && id == "trace-anonymous") {
				assert.Empty(t, result.Data)
				continue
			}
			if role == common.RoleCommonUser {
				require.Len(t, result.Data, 1)
				assert.Equal(t, 1, result.Data[0].Attempt)
				assert.Nil(t, result.Data[0].AdminInfo)
			} else {
				require.NotEmpty(t, result.Data)
				assert.NotNil(t, result.Data[0].AdminInfo)
			}
		}
	}
	other, err := model.GetRequestTraces(t.Context(), "trace-owned", 999, false)
	require.NoError(t, err)
	assert.Empty(t, other)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/log/trace?request_id=trace-owned", nil))
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
	legacyOther := `{"model_price":0.01,"admin_info":{"use_channel":["7"]}}`
	require.NoError(t, db.Create(&model.Log{UserId: 11, Type: model.LogTypeConsume, RequestId: "legacy-id", CreatedAt: 4, Other: legacyOther}).Error)
	var traceQueries int
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("trace-list-query-count", func(tx *gorm.DB) {
		if tx.Statement.Table == "request_trace_metadata" {
			traceQueries++
		}
	}))
	_, _, err = model.GetUserLogs(11, model.LogTypeConsume, 0, 0, "", "", 0, 10, "", "", "")
	require.NoError(t, err)
	assert.Zero(t, traceQueries)
	var legacy model.Log
	require.NoError(t, db.Where("request_id = ?", "legacy-id").First(&legacy).Error)
	assert.Equal(t, legacyOther, legacy.Other)
}

func TestRequestTraceConcurrentCollectors(t *testing.T) {
	observer := common.NewTraceBodyObserver(time.Now())
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			observer.Observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"中\"}}]}\n\n"), "text/event-stream")
		}()
	}
	wg.Wait()
	observer.Finish()
	response, _, _ := observer.Snapshot()
	assert.EqualValues(t, 32, response.ContentCharacters)
	assert.EqualValues(t, 32, response.Chunks)
}

func TestRequestTraceGeminiIdentifiersAndDistinctTools(t *testing.T) {
	observer := common.NewTraceBodyObserver(time.Now())
	for _, name := range []string{"first", "second"} {
		payload := fmt.Sprintf("data: {\"responseId\":\"gemini-response\",\"modelVersion\":\"gemini-version\",\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"%s\",\"args\":{}}}]}}]}\n\n", name)
		observer.Observe([]byte(payload), "text/event-stream")
	}
	observer.Finish()
	response, upstream, _ := observer.Snapshot()
	assert.EqualValues(t, 2, response.ToolCalls)
	assert.Equal(t, "gemini-response", upstream.ResponseID)
	assert.Equal(t, "gemini-version", upstream.Model)
	observer = common.NewTraceBodyObserver(time.Now())
	observer.Observe([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"private-error-original"}}`), "application/json")
	observer.Finish()
	response, upstream, _ = observer.Snapshot()
	assert.True(t, response.Error)
	assert.Equal(t, "RESOURCE_EXHAUSTED", upstream.ErrorType)
	assert.Equal(t, "429", upstream.ErrorCode)
}

func TestRequestTraceConcurrentHTTPPersistence(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all","pre_channel_errors_enabled":true,"anonymous_errors_enabled":false}`))
	engine := gin.New()
	engine.Use(middleware.RequestId(), middleware.RequestTrace(), middleware.BodyStorageCleanup())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("id", 11)
		if _, err := common.GetBodyStorage(c); err != nil {
			c.AbortWithStatus(500)
			return
		}
		c.Data(200, "application/json", []byte(`{"choices":[{"message":{"content":"private-concurrent-response"},"finish_reason":"stop"}]}`))
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := client.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"private-concurrent-input"}]}`))
			if !assert.NoError(t, err) {
				return
			}
			_, err = io.Copy(io.Discard, response.Body)
			assert.NoError(t, err)
			assert.NoError(t, response.Body.Close())
			assert.Equal(t, 200, response.StatusCode)
		}()
	}
	wg.Wait()
	var rows []model.RequestTrace
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 32)
	ids := make(map[string]bool)
	for _, row := range rows {
		assert.False(t, ids[row.RequestId])
		ids[row.RequestId] = true
		assert.NotContains(t, row.Data, "private-concurrent-")
	}
}

func TestRequestTraceWriteFailurePreservesForwardedResponse(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all"}`))
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("trace-persistence-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "request_trace_metadata" {
			tx.AddError(errors.New("mock persistence failure"))
		}
	}))
	engine := gin.New()
	engine.Use(middleware.RequestId(), middleware.RequestTrace(), middleware.BodyStorageCleanup())
	payload := `{"choices":[{"message":{"content":"forwarded"},"finish_reason":"stop"}]}`
	engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("id", 11); c.Data(200, "application/json", []byte(payload)) })
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	assert.Equal(t, 200, recorder.Code)
	assert.Equal(t, payload, recorder.Body.String())
	var count int64
	require.NoError(t, db.Model(&model.RequestTrace{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestRequestTraceHTTPAttemptsAndStreamingErrors(t *testing.T) {
	service.InitHttpClient()
	for _, streamedError := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream-error=%t", streamedError), func(t *testing.T) {
			db := setupRequestTraceDatabase(t)
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"errors","pre_channel_errors_enabled":true}`))
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if streamedError {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"error\",\"code\":\"overloaded\",\"message\":\"private-original-error\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if calls.Add(1) == 1 {
					w.Header().Set("X-Goog-Request-Id", "provider-first")
					w.WriteHeader(502)
					_, _ = io.WriteString(w, `{"error":{"type":"overloaded","message":"private-original-error"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"final-id","model":"mock-model","choices":[{"message":{"content":"private-final-response"},"finish_reason":"stop"}]}`)
			}))
			t.Cleanup(upstream.Close)
			engine := gin.New()
			engine.Use(middleware.RequestId(), middleware.RequestTrace(), middleware.BodyStorageCleanup())
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("id", 11)
				c.Set(common.UpstreamRequestIdKey, "stale-id")
				_, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				for retry := range 2 {
					info := &relaycommon.RelayInfo{RetryIndex: retry, RelayFormat: types.RelayFormatOpenAI, RequestURLPath: c.Request.URL.Path, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelId: 7, ChannelBaseUrl: upstream.URL, UpstreamModelName: "mock-model"}}
					adaptor := &openai.Adaptor{}
					adaptor.Init(info)
					response, err := channel.DoApiRequest(adaptor, c, info, strings.NewReader(`{"model":"mock-model"}`))
					require.NoError(t, err)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					if response.StatusCode >= 400 {
						continue
					}
					assert.Empty(t, c.GetString(common.UpstreamRequestIdKey))
					c.Data(response.StatusCode, response.Header.Get("Content-Type"), body)
					break
				}
			})
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"private-input"}]}`)))
			require.Equal(t, 200, recorder.Code)
			rows, err := model.GetRequestTraces(t.Context(), recorder.Header().Get(common.RequestIdKey), 0, true)
			require.NoError(t, err)
			if streamedError {
				require.Len(t, rows, 1)
				require.NotNil(t, rows[0].AdminInfo)
				require.NotNil(t, rows[0].AdminInfo.Shape)
				assert.True(t, rows[0].AdminInfo.Response.Error)
				assert.Equal(t, "upstream", rows[0].Trace.Stage)
			} else {
				require.Len(t, rows, 2)
				require.NotNil(t, rows[0].AdminInfo)
				assert.Equal(t, "provider-first", rows[0].AdminInfo.Upstream.RequestID)
				assert.Greater(t, rows[0].Trace.ResponseBytes, int64(0))
				assert.Equal(t, 502, rows[0].Trace.Status)
				assert.Nil(t, rows[1].AdminInfo)
			}
			var stored []model.RequestTrace
			require.NoError(t, db.Find(&stored).Error)
			for _, row := range stored {
				assert.NotContains(t, row.Data, "private-")
			}
		})
	}
}

func TestRequestTraceUpstreamRequestIDSelection(t *testing.T) {
	previous := common.GetRequestTraceConfig()
	t.Cleanup(func() {
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previous)))
	})
	defaults := []string{common.RequestIdKey, "X-Request-Id", "Request-Id", "X-Amzn-RequestId", "X-Goog-Request-Id"}
	for index, name := range defaults {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"off"}`))
			headers := http.Header{}
			for _, candidate := range defaults[index:] {
				headers.Set(candidate, "id-"+candidate)
			}
			assert.Equal(t, "id-"+name, common.GetUpstreamRequestID(headers))
		})
	}
	for _, tc := range []struct {
		name       string
		configured []string
		response   map[string]string
		expected   string
	}{
		{"custom-before-default", []string{"x-provider-id"}, map[string]string{"X-Provider-Id": "custom", common.RequestIdKey: "default"}, "custom"},
		{"custom-order", []string{"x-first-id", "x-second-id"}, map[string]string{"X-First-Id": "first", "X-Second-Id": "second"}, "first"},
		{"empty-custom-skipped", []string{"x-first-id", "x-second-id"}, map[string]string{"X-First-Id": "  ", "X-Second-Id": "second"}, "second"},
		{"default-fallback", []string{"x-absent"}, map[string]string{"Request-Id": "fallback"}, "fallback"},
		{"nvcf-not-built-in", nil, map[string]string{"Nvcf-Reqid": "not-selected"}, ""},
		{"nvcf-explicit", []string{"nvcf-reqid"}, map[string]string{"Nvcf-Reqid": "selected", "X-Request-Id": "fallback"}, "selected"},
		{"trim-value", nil, map[string]string{"X-Request-Id": "  selected  "}, "selected"},
		{"limit-value", nil, map[string]string{"X-Request-Id": strings.Repeat("a", 200)}, strings.Repeat("a", 128)},
		{"empty-headers", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(common.RequestTraceConfig{Level: "off", UpstreamRequestIDHeaders: tc.configured})))
			headers := http.Header{}
			for name, value := range tc.response {
				headers.Set(name, value)
			}
			assert.Equal(t, tc.expected, common.GetUpstreamRequestID(headers))
		})
	}
}

func TestRequestTraceUpstreamRequestIDHTTPPropagation(t *testing.T) {
	service.InitHttpClient()
	for _, mode := range []string{"off", "errors", "all"} {
		t.Run(mode, func(t *testing.T) {
			db := setupRequestTraceDatabase(t)
			for _, tc := range []struct {
				name       string
				configured []string
				response   map[string]string
				expected   string
				retry      bool
			}{
				{"custom-precedence", []string{"X-Provider-Id"}, map[string]string{"X-Provider-Id": "custom", common.RequestIdKey: "default", "X-Request-Id": "fallback"}, "custom", false},
				{"default-fallback", []string{"X-Missing"}, map[string]string{common.RequestIdKey: "default", "X-Request-Id": "fallback"}, "default", false},
				{"nvcf-ignored", nil, map[string]string{"Nvcf-Reqid": "ignored"}, "", false},
				{"nvcf-enabled", []string{"Nvcf-Reqid"}, map[string]string{"Nvcf-Reqid": "nvcf-selected"}, "nvcf-selected", false},
				{"retry-clears-id", []string{"X-Provider-Id"}, nil, "", true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(common.RequestTraceConfig{Level: mode, UpstreamRequestIDHeaders: tc.configured, SuccessSampleRate: 100})))
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if calls.Add(1) == 1 && tc.retry {
							w.Header().Set("X-Provider-Id", "first-attempt")
							w.WriteHeader(502)
							_, _ = io.WriteString(w, `{"error":{"type":"overloaded","message":"private-error"}}`)
							return
						}
						for name, value := range tc.response {
							w.Header().Set(name, value)
						}
						_, _ = io.WriteString(w, `{"id":"result","choices":[{"message":{"content":"private-response"},"finish_reason":"stop"}]}`)
					}))
					defer upstream.Close()
					app := gin.New()
					app.Use(middleware.RequestId(), middleware.RequestTrace(), middleware.BodyStorageCleanup())
					app.POST("/v1/chat/completions", func(c *gin.Context) {
						c.Set("id", 11)
						c.Set(common.UpstreamRequestIdKey, "stale-context-id")
						_, err := common.GetBodyStorage(c)
						require.NoError(t, err)
						for retry := range 2 {
							info := &relaycommon.RelayInfo{RetryIndex: retry, RelayFormat: types.RelayFormatOpenAI, RequestURLPath: c.Request.URL.Path, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelId: 7, ChannelBaseUrl: upstream.URL, UpstreamModelName: "mock-model"}}
							adaptor := &openai.Adaptor{}
							adaptor.Init(info)
							res, err := channel.DoApiRequest(adaptor, c, info, strings.NewReader(`{"model":"mock-model"}`))
							require.NoError(t, err)
							body, err := io.ReadAll(res.Body)
							require.NoError(t, err)
							require.NoError(t, res.Body.Close())
							if res.StatusCode >= 400 {
								continue
							}
							require.True(t, c.GetBool(common.UpstreamRequestIDCapturedContextKey))
							service.IOCopyBytesGracefully(c, res, body)
							assert.Equal(t, tc.expected, c.GetString(common.UpstreamRequestIdKey))
							require.NoError(t, db.Create(&model.Log{UserId: 11, Type: model.LogTypeConsume, RequestId: c.GetString(common.RequestIdKey), UpstreamRequestId: c.GetString(common.UpstreamRequestIdKey), Other: `{"original":true}`}).Error)
							return
						}
						t.Fatal("no successful final attempt")
					})
					recorder := httptest.NewRecorder()
					app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"private-input"}]}`)))
					require.Equal(t, 200, recorder.Code)
					requestID := recorder.Header().Get(common.RequestIdKey)
					require.NotEmpty(t, requestID)
					assert.NotEqual(t, "default", requestID)
					var original model.Log
					require.NoError(t, db.Where("request_id = ?", requestID).First(&original).Error)
					assert.Equal(t, tc.expected, original.UpstreamRequestId)
					assert.Equal(t, `{"original":true}`, original.Other)
					rows, err := model.GetRequestTraces(t.Context(), requestID, 0, true)
					require.NoError(t, err)
					if mode == "off" {
						assert.Empty(t, rows)
						return
					}
					expectedRows := 1
					if tc.retry {
						expectedRows = 2
					}
					require.Len(t, rows, expectedRows)
					if tc.retry {
						require.NotNil(t, rows[0].AdminInfo)
						assert.Equal(t, "first-attempt", rows[0].AdminInfo.Upstream.RequestID)
					}
					if mode == "all" {
						require.NotNil(t, rows[len(rows)-1].AdminInfo)
						assert.Equal(t, tc.expected, rows[len(rows)-1].AdminInfo.Upstream.RequestID)
					} else {
						assert.Nil(t, rows[len(rows)-1].AdminInfo)
					}
				})
			}
		})
	}
}

func TestRequestTraceHeaderConfigConcurrentReload(t *testing.T) {
	previous := common.GetRequestTraceConfig()
	t.Cleanup(func() {
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previous)))
	})
	configs := []string{`{"level":"off","upstream_request_id_headers":["X-First"]}`, `{"level":"all","upstream_request_id_headers":["X-Second"]}`}
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", configs[0]))
	headers := http.Header{"X-First": []string{"first"}, "X-Second": []string{"second"}}
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range 32 {
				assert.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", configs[(index+worker)%2]))
				id := common.GetUpstreamRequestID(headers)
				assert.Contains(t, []string{"first", "second"}, id)
				snapshot := common.GetRequestTraceConfig()
				snapshot.UpstreamRequestIDHeaders[0] = "X-Caller-Mutation"
			}
		}()
	}
	wg.Wait()
}

func BenchmarkRequestTraceHeaderSelection(b *testing.B) {
	previous := common.GetRequestTraceConfig()
	b.Cleanup(func() { _ = common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previous)) })
	for _, profile := range []string{"default", "custom-first", "custom-last", "custom-miss"} {
		b.Run(profile, func(b *testing.B) {
			config := common.RequestTraceConfig{Level: "off"}
			headers := http.Header{}
			headers.Set("X-Request-Id", "fallback")
			if profile != "default" {
				for index := range 16 {
					config.UpstreamRequestIDHeaders = append(config.UpstreamRequestIDHeaders, fmt.Sprintf("X-Trace-%d", index))
				}
				if profile == "custom-first" {
					headers.Set("X-Trace-0", "selected")
				}
				if profile == "custom-last" {
					headers.Set("X-Trace-15", "selected")
				}
			}
			require.NoError(b, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(config)))
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					common.GetUpstreamRequestID(headers)
				}
			})
		})
	}
}

func TestRequestTraceConfigValidationAndCancelledStorage(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
	valid := `{"level":"all","pre_channel_errors_enabled":true,"anonymous_errors_enabled":false,"upstream_request_id_headers":[" x-provider-id ","X-PROVIDER-ID","Nvcf-Reqid"]}`
	require.NoError(t, model.UpdateOption("RequestTraceConfig", valid))
	invalidConfigs := []string{`{broken`, `{"level":"unknown"}`, `{"level":"all","anonymous_errors_enabled":"yes"}`,
		`{"level":"all","upstream_request_id_headers":"X-Id"}`,
		`{"level":"all","upstream_request_id_headers":[123]}`,
		`{"level":"all","success_sample_rate":-1}`, `{"level":"all","success_sample_rate":101}`,
		`{"level":"all","success_sample_rate":1.5}`, `{"level":"all","retention_days":-1}`,
		`{"level":"all","retention_days":3651}`, `{"level":"all","diagnostic_channel_ids":[0]}`,
		`{"level":"all","diagnostic_channel_ids":[7]}`,
		`{"level":"all","success_sample_rate":null}`, `{"level":"all","retention_days":null}`,
		`{"level":"all","diagnostic_expires_at":null}`,
	}
	for _, name := range []string{"", " ", "X Id", "X:Id", "X\r\nInjected", "请求ID", strings.Repeat("a", 129)} {
		invalidConfigs = append(invalidConfigs, common.GetJsonString(common.RequestTraceConfig{Level: "all", UpstreamRequestIDHeaders: []string{name}}))
	}
	invalidConfigs = append(invalidConfigs, common.GetJsonString(common.RequestTraceConfig{Level: "all", UpstreamRequestIDHeaders: make([]string, 17)}))
	for _, invalid := range invalidConfigs {
		require.Error(t, model.UpdateOption("RequestTraceConfig", invalid))
		assert.Equal(t, common.RequestTraceConfig{Level: "all", PreChannelErrors: true, UpstreamRequestIDHeaders: []string{"X-Provider-Id", "Nvcf-Reqid"}, CompressionLevel: "fast", SuccessSampleRate: 100}, common.GetRequestTraceConfig())
		var option model.Option
		require.NoError(t, db.Where(map[string]any{"key": "RequestTraceConfig"}).First(&option).Error)
		assert.Equal(t, valid, option.Value)
	}
	copied := common.GetRequestTraceConfig()
	copied.UpstreamRequestIDHeaders[0] = "X-Changed-By-Caller"
	assert.Equal(t, "X-Provider-Id", common.GetRequestTraceConfig().UpstreamRequestIDHeaders[0])
	parsed, err := common.ParseRequestTraceConfig(common.GetJsonString(common.RequestTraceConfig{Level: "off", UpstreamRequestIDHeaders: []string{strings.Repeat("A", 128)}}))
	require.NoError(t, err)
	assert.Len(t, parsed.UpstreamRequestIDHeaders[0], 128)
	require.NoError(t, model.UpdateOption("RequestTraceConfig", `{"level":"errors","pre_channel_errors_enabled":true}`))
	assert.Empty(t, common.GetRequestTraceConfig().UpstreamRequestIDHeaders)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, model.RecordRequestTraces(ctx, []model.RequestTrace{{RequestId: "cancelled", Data: `{}`}}), context.Canceled)
	_, err = model.GetRequestTraces(ctx, "cancelled", 11, false)
	assert.ErrorIs(t, err, context.Canceled)
	storage, err := common.CreateBodyStorage([]byte(`{broken`))
	require.NoError(t, err)
	defer storage.Close()
	collector := &common.RequestTraceCollector{}
	collector.CaptureRequest(storage, true)
	assert.True(t, collector.ShapeIncomplete)
}

func TestRequestTraceSamplingAndDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, level  string
		rate         int
		diagnostic   bool
		expired      bool
		otherChannel bool
		retry        bool
		status       int
		details      []bool
	}{
		{name: "zero success sample", level: "all", rate: 0, status: 200},
		{name: "all success detailed", level: "all", rate: 100, status: 200, details: []bool{true}},
		{name: "all success summary", level: "errors", rate: 100, status: 200, details: []bool{false}},
		{name: "diagnostic overrides sample", level: "errors", rate: 0, diagnostic: true, status: 200, details: []bool{true}},
		{name: "expired diagnostic", level: "errors", rate: 0, diagnostic: true, expired: true, status: 200},
		{name: "unmatched diagnostic", level: "errors", rate: 0, diagnostic: true, otherChannel: true, status: 200},
		{name: "off overrides diagnostic", level: "off", rate: 100, diagnostic: true, status: 200},
		{name: "failed request overrides sample", level: "errors", rate: 0, status: 502, details: []bool{true}},
		{name: "failed retry preserves request chain", level: "errors", rate: 0, retry: true, status: 200, details: []bool{true, false}},
		{name: "each retry has its own duration", level: "all", rate: 0, retry: true, status: 200, details: []bool{true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = setupRequestTraceDatabase(t)
			config := common.RequestTraceConfig{Level: tc.level, PreChannelErrors: true, SuccessSampleRate: tc.rate}
			if tc.diagnostic {
				config.DiagnosticChannelIDs = []int{7}
				config.DiagnosticExpiresAt = time.Now().Add(time.Hour).Unix()
				if tc.expired {
					config.DiagnosticExpiresAt = time.Now().Add(-time.Hour).Unix()
				}
			}
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(config)))
			engine := gin.New()
			engine.Use(middleware.RequestId(), middleware.RequestTrace(), middleware.BodyStorageCleanup())
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("id", 11)
				_, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				if trace := common.GetRequestTrace(c); trace != nil {
					base := time.Now().Add(-5 * time.Second)
					trace.Started = base
					channelID := 7
					if tc.otherChannel {
						channelID = 8
					}
					if tc.retry {
						first := trace.StartAttempt(0, channelID, c.Request)
						first.Started, first.Finished = base.Add(time.Second), base.Add(2*time.Second)
						first.Failed, first.Upstream.Status = true, 502
					}
					index := 0
					if tc.retry {
						index = 1
					}
					last := trace.StartAttempt(index, channelID, c.Request)
					last.Started, last.Finished = base.Add(3*time.Second), base.Add(4*time.Second)
					last.Upstream.Status = tc.status
				}
				if tc.status >= 400 {
					c.Data(tc.status, "application/json", []byte(`{"error":{"type":"test_error","message":"private-sampling-error"}}`))
				} else {
					c.Data(200, "application/json", []byte(`{"choices":[{"message":{"content":"private-sampling-body"},"finish_reason":"stop"}]}`))
				}
			})
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"private-sampling-input"}]}`)))
			rows, err := model.GetRequestTraces(t.Context(), recorder.Header().Get(common.RequestIdKey), 0, true)
			require.NoError(t, err)
			require.Len(t, rows, len(tc.details))
			for index, row := range rows {
				assert.Equal(t, tc.details[index], row.AdminInfo != nil)
				assert.GreaterOrEqual(t, row.Trace.Milliseconds, int64(5000))
				if row.AdminInfo != nil {
					require.NotNil(t, row.AdminInfo.Upstream.Milliseconds)
					assert.EqualValues(t, 1000, *row.AdminInfo.Upstream.Milliseconds)
					assert.NotEqual(t, row.Trace.Milliseconds, *row.AdminInfo.Upstream.Milliseconds)
				}
				encoded := common.GetJsonString(row)
				assert.NotContains(t, encoded, "private-sampling-")
			}
			if tc.retry && len(rows) == 2 {
				assert.Equal(t, "failed", rows[0].Trace.Outcome)
				assert.Equal(t, "success", rows[1].Trace.Outcome)
				assert.Equal(t, rows[0].Trace.Milliseconds, rows[1].Trace.Milliseconds)
			}
		})
	}
	parsed, err := common.ParseRequestTraceConfig(`{"level":"errors"}`)
	require.NoError(t, err)
	assert.Equal(t, 100, parsed.SuccessSampleRate)
	parsed.SuccessSampleRate = 35
	selected := 0
	for index := range 100 {
		id := fmt.Sprintf("stable-sample-%d", index)
		keep := parsed.SampleSuccess(id)
		assert.Equal(t, keep, parsed.SampleSuccess(id))
		if keep {
			selected++
		}
	}
	assert.Greater(t, selected, 0)
	assert.Less(t, selected, 100)
}

func TestRequestTraceRootStatusAuthorization(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}))
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("trace-status-history-failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "system_tasks" {
			tx.AddError(errors.New("test cleanup history unavailable"))
		}
	}))
	engine := gin.New()
	engine.GET("/api/log/trace/status", middleware.RootAuth(), controller.GetRequestTraceStatus)
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
		pat := rand.Text()
		user := model.User{Username: fmt.Sprintf("trace-status-%d", role), Role: role, Status: common.UserStatusEnabled, Group: "default", AffCode: fmt.Sprintf("trace-status-aff-%d", role), AccessToken: &pat, AuthVersion: 1}
		require.NoError(t, db.Create(&user).Error)
		request := httptest.NewRequest(http.MethodGet, "/api/log/trace/status", nil)
		request.Header.Set("Authorization", "Bearer "+pat)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		if role != common.RoleRootUser {
			assert.Equal(t, http.StatusForbidden, recorder.Code)
			continue
		}
		require.Equal(t, http.StatusOK, recorder.Code)
		var result struct {
			Success bool `json:"success"`
			Data    struct {
				FailedRows   int64  `json:"failed_rows"`
				CleanupError string `json:"cleanup_error"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
		assert.True(t, result.Success)
		assert.Contains(t, result.Data.CleanupError, "test cleanup history unavailable")
		assert.GreaterOrEqual(t, result.Data.FailedRows, int64(0))
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/log/trace/status", nil))
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}
