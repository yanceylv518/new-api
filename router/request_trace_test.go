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
	"testing/iotest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/internal/requesttracetest"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
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
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.RequestTraceMetadata{}, &model.Option{}, &model.AuditLog{}, &model.UserSession{}))
	require.NoError(t, db.Create(&model.User{Id: 11, Username: "trace-owner", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "trace-owner"}).Error)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, model.StopRequestTraceWriter(ctx))
		require.NoError(t, sqlDB.Close())
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.SQLitePath, common.IsMasterNode = previousPath, previousMaster
		common.RedisEnabled, common.MemoryCacheEnabled, common.LogConsumeEnabled = previousRedis, previousMemory, previousConsume
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previousConfig)))
	})
	model.StartRequestTraceWriter()
	return db
}

func awaitRequestTraceWrites(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return model.GetRequestTraceWriterStats().QueuedRows == 0 }, 5*time.Second, 5*time.Millisecond)
}

func TestRequestTraceProtocolMetadata(t *testing.T) {
	cases := []struct {
		name, body, contentType, reason string
		tools                           int64
		done, failed                    bool
	}{
		{"openai-json", `{"id":"chat-id","model":"mock-model","choices":[{"index":0,"message":{"content":"A中😊","reasoning_content":"abc","tool_calls":[{"id":"call-1","function":{"name":"tool","arguments":"private-tool-arguments"}}]},"finish_reason":"stop"}],"usage":{"completion_tokens":3}}`, "application/json", "stop", 1, true, false},
		{"completions-json", `{"choices":[{"text":"abc","finish_reason":"length"}]}`, "application/json", "length", 0, true, false},
		{"openai-null-tools-json", `{"choices":[{"message":{"content":"abc","tool_calls":null,"function_call":null},"finish_reason":"stop"}]}`, "application/json", "stop", 0, true, false},
		{"openai-empty-function-json", `{"choices":[{"message":{"content":"abc","function_call":{"name":"","arguments":""}},"finish_reason":"stop"}]}`, "application/json", "stop", 0, true, false},
		{"openai-tool-with-empty-legacy-json", `{"choices":[{"message":{"tool_calls":[{"id":"call-1","function":{"name":"tool","arguments":"{}"}}],"function_call":{"name":"","arguments":""}},"finish_reason":"tool_calls"}]}`, "application/json", "tool_calls", 1, true, false},
		{"openai-empty-function-sse", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"abc\",\"tool_calls\":null,\"function_call\":null}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"function_call\":{\"name\":\"\",\"arguments\":\"\"}},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", "text/event-stream", "stop", 0, true, false},
		{"claude-json", `{"type":"message","id":"msg-1","content":[{"type":"text","text":"你好"},{"type":"thinking","thinking":"abc"},{"type":"tool_use","input":{"secret":"private-tool-arguments"}}],"stop_reason":"end_turn","usage":{"output_tokens":4}}`, "application/json", "end_turn", 1, true, false},
		{"gemini-json", `{"candidates":[{"content":{"parts":[{"text":"你好"},{"text":"abc","thought":true},{"functionCall":{"name":"tool","args":{"secret":"private-tool-arguments"}}}]},"finishReason":"STOP"}],"usageMetadata":{"thoughtsTokenCount":2}}`, "application/json", "STOP", 1, true, false},
		{"responses-json", `{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"你好"}]},{"type":"reasoning","summary":[{"text":"abc"}]},{"type":"function_call","arguments":"private-tool-arguments"}],"usage":{"output_tokens":4}}`, "application/json", "completed", 1, true, false},
		{"openai-sse", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"A\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"private-tool-arguments\"}}]}}]}\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"中\",\"reasoning_content\":\"abc\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"more\"}}]}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\ndata: [DONE]\n\n", "text/event-stream", "stop", 1, true, false},
		{"claude-sse", "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"usage\":{\"input_tokens\":3}}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"你好\",\"thinking\":\"abc\"}}\n\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"input\":{}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "text/event-stream", "end_turn", 1, true, false},
		{"gemini-sse", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"你好\"},{\"text\":\"abc\",\"thought\":true},{\"functionCall\":{\"name\":\"tool\",\"args\":{}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"thoughtsTokenCount\":2}}\n\n", "text/event-stream", "STOP", 1, false, false},
		{"responses-sse", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"abc\"}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"status\":\"completed\",\"output\":[{\"content\":[{\"type\":\"output_text\",\"text\":\"你好\"}]}]}}\n\n", "text/event-stream", "completed", 1, true, false},
		{"responses-stream-error", "data: {\"type\":\"error\",\"code\":\"overloaded\",\"message\":\"private-error-original\"}\n\n", "text/event-stream", "", 0, false, true},
		{"responses-stream-failed", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"overloaded\",\"message\":\"private-error-original\"}}}\n\n", "text/event-stream", "failed", 0, true, true},
		{"nested-invalid-metadata", `{"id":{"text":"private-message-body"},"error":{"type":{"message":"private-error-original"},"code":{"message":"private-error-original"},"message":"private-error-original"}}`, "application/json", "", 0, true, true},
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
				assert.Equal(t, tc.tools, response.ToolCalls)
				assert.Equal(t, tc.done, response.Done)
				assert.Equal(t, tc.failed, response.Error)
				if tc.reason != "" {
					require.NotNil(t, reason)
					assert.Equal(t, tc.reason, *reason)
				}
				encoded := common.GetJsonString(common.RequestTraceData{AdminInfo: &common.RequestTraceAdmin{Upstream: upstream, Response: response}})
				for _, field := range []string{`"resp_bytes"`, `"content_chars"`, `"reasoning_chars"`, `"chunks"`, `"last_chunk_ms"`, `"usage_chunk"`, `"keys"`} {
					assert.NotContains(t, encoded, field)
				}
				for _, secret := range []string{"private-message-body", "private-tool-arguments", "private-error-original", "你好", "A中😊"} {
					assert.NotContains(t, encoded, secret)
				}
				client.Observe([]byte(tc.body), tc.contentType)
				client.Finish()
				forwarded, forwardedUpstream, forwardedReason := client.Snapshot()
				assert.Equal(t, response, forwarded)
				assert.Equal(t, upstream, forwardedUpstream)
				assert.Equal(t, reason, forwardedReason)
			}
		})
	}
}

func TestRequestTraceToolNameNullDelta(t *testing.T) {
	cases := []struct {
		name       string
		deltas     []string
		body       string
		names      map[string]int64
		incomplete bool
	}{
		{
			name: "complete name survives null argument delta",
			deltas: []string{
				`{"tool_calls":[{"index":0,"function":{"name":"trace_test_echo","arguments":""}}]}`,
				`{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"{}"}}]}`,
			},
			names: map[string]int64{"trace_test_echo": 1},
		},
		{
			name: "fragmented name ignores null and missing name deltas",
			deltas: []string{
				`{"tool_calls":[{"index":0,"function":{"name":null,"arguments":""}}]}`,
				`{"tool_calls":[{"index":0,"function":{"name":"trace_","arguments":""}}]}`,
				`{"tool_calls":[{"index":0,"function":{"arguments":"{"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"}"}}]}`,
				`{"tool_calls":[{"index":0,"function":{"name":"test_echo","arguments":""}}]}`,
			},
			names: map[string]int64{"trace_test_echo": 1},
		},
		{
			name: "legacy function name survives null argument delta",
			deltas: []string{
				`{"function_call":{"name":"trace_test_echo","arguments":""}}`,
				`{"function_call":{"name":null,"arguments":"{}"}}`,
			},
			names: map[string]int64{"trace_test_echo": 1},
		},
		{
			name: "non-string name delta stays invalid",
			deltas: []string{
				`{"tool_calls":[{"index":0,"function":{"name":"trace_test_echo","arguments":""}}]}`,
				`{"tool_calls":[{"index":0,"function":{"name":42,"arguments":"{}"}}]}`,
			},
			incomplete: true,
		},
		{
			name:       "non-stream null name stays invalid",
			body:       `{"choices":[{"message":{"tool_calls":[{"function":{"name":null,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			incomplete: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := tc.body, "application/json"
			if len(tc.deltas) > 0 {
				var stream strings.Builder
				for _, delta := range tc.deltas {
					fmt.Fprintf(&stream, "data: {\"choices\":[{\"index\":0,\"delta\":%s}]}\n\n", delta)
				}
				stream.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
				body, contentType = stream.String(), "text/event-stream"
			}
			for _, chunkSize := range []int{1, len(body)} {
				observer := common.NewTraceBodyObserver(time.Now())
				for offset := 0; offset < len(body); offset += chunkSize {
					observer.Observe([]byte(body[offset:min(offset+chunkSize, len(body))]), contentType)
				}
				observer.Finish()
				response, _, reason := observer.Snapshot()
				assert.EqualValues(t, 1, response.ToolCalls)
				assert.Equal(t, tc.names, response.ToolCallsByName)
				assert.Equal(t, tc.incomplete, response.MetadataIncomplete)
				if tc.incomplete {
					assert.Contains(t, response.IncompleteReasons, "invalid_tool_name")
				} else {
					assert.Empty(t, response.IncompleteReasons)
				}
				assert.True(t, response.Done)
				require.NotNil(t, reason)
				assert.Equal(t, "tool_calls", *reason)
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
		assert.EqualValues(t, 1, response.ToolCalls)
		assert.EqualValues(t, 1, response.ToolCallsByName["first"])
		response.ToolCallsByName["first"] = 99

		cachedReader.Observe(first, "text/event-stream")
		cachedReader.Finish()
		cachedResponse, cachedInfo, cachedReason := cachedReader.Snapshot()
		assert.EqualValues(t, 1, cachedResponse.ToolCallsByName["first"])
		require.NotNil(t, cachedResponse.ReasoningTokens)
		assert.EqualValues(t, 3, *cachedResponse.ReasoningTokens)
		require.NotNil(t, cachedInfo.Created)
		assert.EqualValues(t, 42, *cachedInfo.Created)
		assert.Nil(t, cachedReason)

		client.Observe(second, "text/event-stream")
		client.Finish()
		clientResponse, clientInfo, clientReason := client.Snapshot()
		assert.Nil(t, clientResponse.ReasoningTokens)
		assert.Nil(t, clientInfo.Created)
		assert.Empty(t, clientInfo.ResponseID)
		assert.Zero(t, clientResponse.ToolCalls)
		require.NotNil(t, clientReason)
		assert.Equal(t, "length", *clientReason)
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
		_, upstreamInfo, upstreamReason := upstream.Snapshot()
		_, clientInfo, clientReason := client.Snapshot()
		assert.Equal(t, "upstream-id", upstreamInfo.ResponseID)
		assert.Equal(t, "upstream-model", upstreamInfo.Model)
		require.NotNil(t, upstreamReason)
		assert.Equal(t, "stop", *upstreamReason)
		assert.Equal(t, "client-id", clientInfo.ResponseID)
		assert.Empty(t, clientInfo.Model)
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
	})
	t.Run("oversized stream event recovers", func(t *testing.T) {
		observer := common.NewTraceBodyObserver(time.Now())
		observer.Observe([]byte("data: "+strings.Repeat("x", common.TraceMetadataMaxBytes+1)+"\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"中\"}}]}\n\ndata: [DONE]\n\n"), "text/event-stream")
		observer.Finish()
		response, _, _ := observer.Snapshot()
		assert.True(t, response.MetadataIncomplete)
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
		assert.True(t, response.Done)
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
			}, middleware.RequestTraceMetadata())
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

func TestRequestTraceResponseController(t *testing.T) {
	previous := common.GetRequestTraceConfig()
	t.Cleanup(func() {
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previous)))
	})
	for _, level := range []string{"off", "errors", "all"} {
		t.Run(level, func(t *testing.T) {
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", fmt.Sprintf(`{"level":%q}`, level)))
			checked := make(chan []error, 1)
			app := gin.New()
			app.Use(middleware.RequestId(), middleware.RequestTraceMetadata())
			app.POST("/v1/chat/completions", func(c *gin.Context) {
				controller := http.NewResponseController(c.Writer)
				checked <- []error{
					controller.SetWriteDeadline(time.Now().Add(5 * time.Second)),
					controller.SetReadDeadline(time.Now().Add(5 * time.Second)),
					controller.EnableFullDuplex(),
					controller.Flush(),
				}
				c.String(http.StatusOK, "ok")
			})
			server := httptest.NewServer(app)
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Post(server.URL+"/v1/chat/completions", "application/json", nil)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "ok", string(body))
			for index, err := range <-checked {
				assert.NoError(t, err, "HTTP capability %s", []string{"write deadline", "read deadline", "full duplex", "flush"}[index])
			}
		})
	}
}

func TestRequestTracePanicPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, level                string
		userID, rows, status       int
		attempt, stream, anonymous bool
	}{
		{name: "summary before dispatch", level: "errors", userID: 11, rows: 1, status: 500},
		{name: "full before dispatch", level: "all", userID: 11, rows: 1, status: 500},
		{name: "active attempt", level: "errors", userID: 11, rows: 1, status: 500, attempt: true},
		{name: "headers already sent", level: "errors", userID: 11, rows: 1, status: 200, attempt: true, stream: true},
		{name: "off", level: "off", userID: 11, status: 500},
		{name: "anonymous disabled", level: "errors", status: 500},
		{name: "anonymous enabled", level: "errors", rows: 1, status: 500, anonymous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupRequestTraceDatabase(t)
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(common.RequestTraceConfig{Level: tc.level, PreChannelErrors: true, AnonymousErrors: tc.anonymous, SuccessSampleRate: 0})))
			app := gin.New()
			app.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
				c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"type": "new_api_panic", "message": fmt.Sprint(recovered)}})
				c.Abort()
			}))
			cleaned := false
			app.Use(func(c *gin.Context) {
				original := c.Writer
				defer func() {
					assert.Nil(t, common.GetRequestTrace(c))
					assert.Same(t, original, c.Writer)
					storage, _ := c.Get(common.KeyBodyStorage)
					assert.Nil(t, storage)
					cleaned = true
				}()
				c.Next()
			}, middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
			app.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("id", tc.userID)
				_, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				if tc.attempt {
					trace := common.GetRequestTrace(c)
					attempt := trace.StartAttempt(0, 7, c.Request)
					attempt.Upstream.Status = http.StatusOK
					attempt.Observer = common.NewTraceBodyObserverWithMode(trace.Started, trace.ResponseMetadataLevel(7, http.StatusOK))
					common.SetRequestTraceStage(c, "upstream")
				}
				if tc.stream {
					c.Header("Content-Type", "text/event-stream")
					_, err := c.Writer.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"private-response\"}}]}\n\n")
					require.NoError(t, err)
					c.Writer.Flush()
				}
				panic("private-panic-payload")
			})
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"private-input"}]}`)))
			assert.Equal(t, tc.status, recorder.Code)
			assert.True(t, cleaned)
			awaitRequestTraceWrites(t)
			var stored []model.RequestTraceMetadata
			require.NoError(t, db.Where("request_id = ?", recorder.Header().Get(common.RequestIdKey)).Find(&stored).Error)
			require.Len(t, stored, tc.rows)
			if tc.rows == 0 {
				return
			}
			encoded, err := common.DecodeRequestTraceStorage(stored[0].Data)
			require.NoError(t, err)
			assert.NotContains(t, encoded, "private-")
			var data common.RequestTraceData
			require.NoError(t, common.UnmarshalJsonStr(encoded, &data))
			assert.Equal(t, "failed", data.Trace.Outcome)
			assert.Equal(t, tc.status, data.Trace.Status)
			require.NotNil(t, data.AdminInfo)
			assert.Equal(t, common.TraceMetadataFull, data.AdminInfo.MetadataLevel)
			assert.True(t, data.AdminInfo.Response.Error)
			require.NotNil(t, data.AdminInfo.Shape)
			assert.EqualValues(t, 1, data.AdminInfo.Shape.Messages)
			if tc.stream {
				assert.False(t, data.AdminInfo.Response.Done)
				assert.Contains(t, data.AdminInfo.Response.IncompleteReasons, "capture_upgraded")
			} else {
				assert.Equal(t, "new_api_panic", data.Trace.ClientErrorType)
			}
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
			engine.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
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
			var rows []model.RequestTraceMetadata
			awaitRequestTraceWrites(t)
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
			require.NotNil(t, data.AdminInfo)
			assert.Equal(t, tc.detailed, data.AdminInfo.Shape != nil)
			if tc.detailed {
				require.NotNil(t, data.AdminInfo.Shape)
			}
		})
	}
}

func TestRequestTraceDetailAuthorizationAndLegacyLogs(t *testing.T) {
	db := setupRequestTraceDatabase(t)
	data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}, AdminInfo: &common.RequestTraceAdmin{ChannelID: 7}})
	rows := []model.RequestTraceMetadata{{RequestId: "trace-owned", Attempt: 0, UserId: 11, CreatedAt: 1, Data: data}, {RequestId: "trace-owned", Attempt: 1, UserId: 11, CreatedAt: 2, Data: data}, {RequestId: "trace-anonymous", Attempt: -1, UserId: 0, CreatedAt: 3, Data: data}}
	require.NoError(t, model.RecordRequestTraces(t.Context(), rows))
	require.NoError(t, model.RecordRequestTraces(t.Context(), rows))
	var count int64
	require.NoError(t, db.Model(&model.RequestTraceMetadata{}).Count(&count).Error)
	assert.EqualValues(t, 3, count)
	engine := gin.New()
	engine.GET("/api/log/trace", middleware.UserAuth(), controller.GetRequestTraceMetadata)
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
	observer.ObserveString("data: [DONE]\n\n", "text/event-stream")
	observer.Finish()
	response, _, _ := observer.Snapshot()
	assert.Equal(t, "[DONE]", response.EndMarker)
	assert.True(t, response.Done)
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
	engine.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
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
	var rows []model.RequestTraceMetadata
	awaitRequestTraceWrites(t)
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
	engine.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
	payload := `{"choices":[{"message":{"content":"forwarded"},"finish_reason":"stop"}]}`
	engine.POST("/v1/chat/completions", func(c *gin.Context) { c.Set("id", 11); c.Data(200, "application/json", []byte(payload)) })
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
	assert.Equal(t, 200, recorder.Code)
	assert.Equal(t, payload, recorder.Body.String())
	var count int64
	awaitRequestTraceWrites(t)
	require.NoError(t, db.Model(&model.RequestTraceMetadata{}).Count(&count).Error)
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
			engine.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
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
			awaitRequestTraceWrites(t)
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
				assert.Equal(t, 502, rows[0].Trace.Status)
				require.NotNil(t, rows[1].AdminInfo)
				assert.Equal(t, common.TraceMetadataSummary, rows[1].AdminInfo.MetadataLevel)
			}
			var stored []model.RequestTraceMetadata
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
					app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
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
					awaitRequestTraceWrites(t)
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
						require.NotNil(t, rows[len(rows)-1].AdminInfo)
						assert.Equal(t, common.TraceMetadataSummary, rows[len(rows)-1].AdminInfo.MetadataLevel)
						assert.Equal(t, tc.expected, rows[len(rows)-1].AdminInfo.Upstream.RequestID)
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
	assert.ErrorIs(t, model.RecordRequestTraces(ctx, []model.RequestTraceMetadata{{RequestId: "cancelled", Data: `{}`}}), context.Canceled)
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
			engine.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
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
			awaitRequestTraceWrites(t)
			rows, err := model.GetRequestTraces(t.Context(), recorder.Header().Get(common.RequestIdKey), 0, true)
			require.NoError(t, err)
			require.Len(t, rows, len(tc.details))
			for index, row := range rows {
				require.NotNil(t, row.AdminInfo)
				assert.Equal(t, tc.details[index], row.AdminInfo.Shape != nil)
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

func TestRequestTracePriorityHTTPAdmission(t *testing.T) {
	for _, kind := range []string{"failed", "diagnostic", "retry"} {
		t.Run(kind, func(t *testing.T) {
			db := setupRequestTraceDatabase(t)
			config := common.RequestTraceConfig{Level: "errors", PreChannelErrors: true, SuccessSampleRate: 100}
			if kind == "diagnostic" {
				config.DiagnosticChannelIDs = []int{7}
				config.DiagnosticExpiresAt = time.Now().Add(time.Hour).Unix()
			}
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(config)))
			entered, released := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(released) }) }
			t.Cleanup(release)
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register("trace-priority-gate", func(tx *gorm.DB) {
				if tx.Statement.Table != (model.RequestTraceMetadata{}).TableName() {
					return
				}
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-released:
				case <-tx.Statement.Context.Done():
					tx.AddError(tx.Statement.Context.Err())
				}
			}))
			filled := false
			for start := 0; start < model.GetRequestTraceWriterStats().QueueRowLimit; start += 128 {
				rows := make([]model.RequestTraceMetadata, 128)
				for index := range rows {
					rows[index] = model.RequestTraceMetadata{RequestId: fmt.Sprintf("regular-%d", start+index), UserId: 11, Data: `{"trace":{"status":200}}`}
				}
				err := model.SubmitRequestTraces(rows)
				if errors.Is(err, model.ErrRequestTraceDropped) {
					filled = true
					break
				}
				require.NoError(t, err)
			}
			require.True(t, filled)
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("trace writer did not reach database boundary")
			}
			before := model.GetRequestTraceWriterStats()
			app := gin.New()
			app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
			app.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("id", 11)
				_, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				trace := common.GetRequestTrace(c)
				if kind == "diagnostic" {
					trace.StartAttempt(0, 7, c.Request)
				}
				if kind == "retry" {
					attempt := trace.StartAttempt(0, 7, c.Request)
					attempt.Failed, attempt.Upstream.Status = true, 502
					trace.StartAttempt(1, 7, c.Request).Upstream.Status = 200
				}
				if kind == "failed" {
					c.Data(502, "application/json", []byte(`{"error":{"type":"upstream_error"}}`))
				} else {
					c.Data(200, "application/json", []byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
				}
			})
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
			requestID := recorder.Header().Get(common.RequestIdKey)
			assert.True(t, model.HasPendingRequestTrace(requestID, 11, false))
			assert.Equal(t, before.DroppedPriorityRows, model.GetRequestTraceWriterStats().DroppedPriorityRows)
			release()
			awaitRequestTraceWrites(t)
			traces, err := model.GetRequestTraces(t.Context(), requestID, 0, true)
			require.NoError(t, err)
			expected := 1
			if kind == "retry" {
				expected = 2
			}
			assert.Len(t, traces, expected)
		})
	}
}

func TestRequestTraceChatTerminalUsage(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, tc := range []struct {
		name, reason            string
		delta                   gin.H
		includeUsage, usageOnly bool
	}{
		{name: "finish reason without usage requested", reason: "stop", delta: gin.H{}},
		{name: "tool invocation without usage requested", reason: "tool_calls", delta: gin.H{"tool_calls": []gin.H{{"index": 0, "id": "call-test", "type": "function", "function": gin.H{"name": "search", "arguments": "{}"}}}}},
		{name: "refusal without usage requested", delta: gin.H{"refusal": "declined"}},
		{name: "role without usage requested", delta: gin.H{"role": "assistant"}},
		{name: "usage requested", reason: "stop", delta: gin.H{}, includeUsage: true},
		{name: "standalone usage omitted", usageOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choices := []gin.H{}
			if !tc.usageOnly {
				choice := gin.H{"index": 0, "delta": tc.delta}
				if tc.reason != "" {
					choice["finish_reason"] = tc.reason
				}
				choices = append(choices, choice)
			}
			payload := common.GetJsonString(gin.H{"id": "test-frame", "model": "mock-model", "choices": choices, "usage": gin.H{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}})
			app := gin.New()
			app.POST("/v1/chat/completions", func(c *gin.Context) {
				info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions, IsStream: true, ShouldIncludeUsage: tc.includeUsage, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "mock-model"}}
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\ndata: [DONE]\n\n"))}
				usage, apiError := openai.OaiStreamHandler(c, info, response)
				require.Nil(t, apiError)
				require.NotNil(t, usage)
				assert.Equal(t, 2, usage.PromptTokens)
				assert.Equal(t, 3, usage.CompletionTokens)
			})
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`)))
			assert.Equal(t, 1, strings.Count(recorder.Body.String(), "data: [DONE]"))
			frames := 0
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: {") {
					continue
				}
				frames++
				var frame map[string]any
				require.NoError(t, common.UnmarshalJsonStr(strings.TrimPrefix(line, "data: "), &frame))
				_, hasUsage := frame["usage"]
				assert.Equal(t, tc.includeUsage, hasUsage)
				if !tc.usageOnly {
					encodedChoices := common.GetJsonString(frame["choices"])
					for key := range tc.delta {
						assert.Contains(t, encodedChoices, `"`+key+`"`)
					}
					if tc.reason != "" {
						assert.Contains(t, encodedChoices, tc.reason)
					}
				}
			}
			if tc.usageOnly {
				assert.Zero(t, frames)
			} else {
				assert.Equal(t, 1, frames)
			}
		})
	}
}

func TestRequestTraceCompactSummaryStorage(t *testing.T) {
	level := common.TraceMetadataSummary
	data := common.RequestTraceData{Version: 1, Trace: common.RequestTraceSummary{Status: 200, Milliseconds: 20}, AdminInfo: &common.RequestTraceAdmin{MetadataLevel: level, ChannelID: 7, Client: common.RequestTraceClient{UserAgent: "client-agent", ContentType: "application/json"}, Upstream: common.RequestTraceUpstream{Status: 200, RequestID: "provider-id", HeaderMilliseconds: 4}, Response: common.RequestTraceResponse{Done: true, EndMarker: "[DONE]", MetadataIncomplete: true}}}
	encoded, err := common.MarshalRequestTraceStorageData(data)
	require.NoError(t, err)
	for _, field := range []string{`"content_chars"`, `"reasoning_chars"`, `"client"`, `"tool_calls"`} {
		assert.NotContains(t, string(encoded), field)
	}
	var restored common.RequestTraceData
	require.NoError(t, common.Unmarshal(encoded, &restored))
	assert.Equal(t, data.Trace, restored.Trace)
	require.NotNil(t, restored.AdminInfo)
	assert.Equal(t, "provider-id", restored.AdminInfo.Upstream.RequestID)
	assert.EqualValues(t, 4, restored.AdminInfo.Upstream.HeaderMilliseconds)
	assert.Equal(t, data.AdminInfo.Response, restored.AdminInfo.Response)
	assert.Contains(t, common.GetJsonString(restored), `"client":{}`)
	data.AdminInfo.MetadataLevel = common.TraceMetadataFull
	full, err := common.MarshalRequestTraceStorageData(data)
	require.NoError(t, err)
	assert.JSONEq(t, common.GetJsonString(data), string(full))
}

func requestTraceProtocolRequest(format types.RelayFormat) string {
	switch format {
	case types.RelayFormatOpenAI:
		return `{"model":"client-model","messages":[{"role":"system","content":"S"},{"role":"user","content":"Hi中😊"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},{"type":"function","function":{"name":"unused_tool","parameters":{"type":"object"}}}],"tool_choice":"auto","max_tokens":128,"temperature":0,"stream_options":{"include_usage":true}}`
	case types.RelayFormatOpenAIResponses:
		return `{"model":"client-model","instructions":"S","input":[{"role":"user","content":[{"type":"input_text","text":"Hi中😊"}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"unused_tool","parameters":{"type":"object"}}],"tool_choice":"auto","max_output_tokens":128,"temperature":0}`
	case types.RelayFormatClaude:
		return `{"model":"client-model","system":"S","messages":[{"role":"user","content":"Hi中😊"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"unused_tool","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"},"max_tokens":128,"temperature":0}`
	case types.RelayFormatGemini:
		return `{"systemInstruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"Hi中😊"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}},{"name":"unused_tool","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},"generationConfig":{"maxOutputTokens":128,"temperature":0}}`
	}
	return ""
}

func requestTraceProtocolPayload(format types.RelayFormat, stream bool) string {
	var body string
	var events []string
	switch format {
	case types.RelayFormatOpenAI:
		body = `{"id":"source-id","object":"chat.completion","created":1700000000,"model":"upstream-model","system_fingerprint":"fp-source","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"ok","tool_calls":[{"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":9,"total_tokens":19,"completion_tokens_details":{"reasoning_tokens":3}}}`
		events = []string{
			`{"id":"source-id","object":"chat.completion.chunk","created":1700000000,"model":"upstream-model","system_fingerprint":"fp-source","service_tier":"priority","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":null,"arguments":"{}"}}]},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":9,"total_tokens":19,"completion_tokens_details":{"reasoning_tokens":3}}}`,
		}
	case types.RelayFormatOpenAIResponses:
		body = `{"id":"source-id","object":"response","created_at":1700000000,"status":"completed","model":"upstream-model","service_tier":"priority","output":[{"type":"function_call","id":"fc-source","call_id":"call_lookup","name":"lookup","arguments":"{}","status":"completed"}],"usage":{"input_tokens":10,"output_tokens":9,"total_tokens":19,"output_tokens_details":{"reasoning_tokens":3}}}`
		events = []string{
			`{"type":"response.created","response":{"id":"source-id","object":"response","created_at":1700000000,"status":"in_progress","model":"upstream-model","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc-source","call_id":"call_lookup","name":"lookup","arguments":"","status":"in_progress"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc-source","delta":"{}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-source","call_id":"call_lookup","name":"lookup","arguments":"{}","status":"completed"}}`,
			`{"type":"response.completed","response":` + body + `}`,
		}
	case types.RelayFormatClaude:
		body = `{"id":"source-id","type":"message","model":"upstream-model","role":"assistant","content":[{"type":"tool_use","id":"call_lookup","name":"lookup","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":9}}`
		events = []string{
			`{"type":"message_start","message":{"id":"source-id","type":"message","model":"upstream-model","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_lookup","name":"lookup","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		}
	case types.RelayFormatGemini:
		body = `{"responseId":"source-id","modelVersion":"upstream-model","candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":6,"thoughtsTokenCount":3,"totalTokenCount":19}}`
		events = []string{body}
	}
	if !stream {
		return body
	}
	var result strings.Builder
	for _, event := range events {
		fmt.Fprintf(&result, "data: %s\n\n", event)
	}
	if format == types.RelayFormatOpenAI {
		result.WriteString("data: [DONE]\n\n")
	}
	return result.String()
}

func requestTraceProtocolRefusalPayload(format types.RelayFormat, stream bool) string {
	var body string
	var events []string
	switch format {
	case types.RelayFormatOpenAI:
		body = `{"id":"source-id","object":"chat.completion","created":1700000000,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"refusal":"private-refusal-body"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":10,"completion_tokens":9,"total_tokens":19,"completion_tokens_details":{"reasoning_tokens":3}}}`
		events = []string{
			`{"id":"source-id","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","refusal":"private-refusal-body"},"finish_reason":null}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":10,"completion_tokens":9,"total_tokens":19,"completion_tokens_details":{"reasoning_tokens":3}}}`,
		}
	case types.RelayFormatOpenAIResponses:
		body = `{"id":"source-id","object":"response","created_at":1700000000,"status":"completed","model":"upstream-model","output":[{"id":"msg-source","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"private-refusal-body"}]}],"usage":{"input_tokens":10,"output_tokens":9,"total_tokens":19,"output_tokens_details":{"reasoning_tokens":3}}}`
		events = []string{
			`{"type":"response.created","response":{"id":"source-id","object":"response","model":"upstream-model","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg-source","type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.refusal.delta","output_index":0,"content_index":0,"item_id":"msg-source","delta":"private-refusal-body"}`,
			`{"type":"response.completed","response":` + body + `}`,
		}
	case types.RelayFormatClaude:
		body = `{"id":"source-id","type":"message","model":"upstream-model","role":"assistant","content":[{"type":"text","text":"private-refusal-body"}],"stop_reason":"refusal","usage":{"input_tokens":10,"output_tokens":9}}`
		events = []string{
			`{"type":"message_start","message":{"id":"source-id","type":"message","model":"upstream-model","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"private-refusal-body"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":9}}`,
			`{"type":"message_stop"}`,
		}
	case types.RelayFormatGemini:
		body = `{"responseId":"source-id","modelVersion":"upstream-model","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"private-refusal-body"}]},"finishReason":"SAFETY"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":6,"thoughtsTokenCount":3,"totalTokenCount":19}}`
		events = []string{body}
	}
	if !stream {
		return body
	}
	var result strings.Builder
	for _, event := range events {
		fmt.Fprintf(&result, "data: %s\n\n", event)
	}
	if format == types.RelayFormatOpenAI {
		result.WriteString("data: [DONE]\n\n")
	}
	return result.String()
}

func TestRequestTraceRequestShapeFormats(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, types.RelayFormatClaude, types.RelayFormatGemini} {
		t.Run(string(format), func(t *testing.T) {
			storage, err := common.CreateBodyStorage([]byte(requestTraceProtocolRequest(format)))
			require.NoError(t, err)
			defer storage.Close()
			collector := &common.RequestTraceCollector{}
			collector.CaptureRequest(storage, true)
			require.NotNil(t, collector.Shape)
			shape := collector.Shape
			messages := int64(1)
			if format == types.RelayFormatOpenAI {
				messages = 2
			}
			assert.Equal(t, messages, shape.Messages)
			assert.EqualValues(t, 5, shape.TextCharacters)
			assert.True(t, shape.System)
			assert.EqualValues(t, 2, shape.Tools)
			assert.Equal(t, "auto", shape.ToolChoice)
			require.NotNil(t, shape.MaxTokens)
			assert.EqualValues(t, 128, *shape.MaxTokens)
			require.NotNil(t, shape.Temperature)
			assert.Zero(t, *shape.Temperature)
			assert.Equal(t, format == types.RelayFormatOpenAI, shape.IncludeUsage)
			assert.False(t, collector.ShapeIncomplete)
		})
	}
}

func TestRequestTraceDecodedRequestShapeParity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		claude     bool
	}{
		{"chat", requestTraceProtocolRequest(types.RelayFormatOpenAI), false},
		{"claude", requestTraceProtocolRequest(types.RelayFormatClaude), true},
		{"media", `{"messages":[{"role":"developer","content":[{"type":"text","text":"Hi中😊"},{"type":"image_url","image_url":null},{"type":"input_audio","input_audio":{}},{"type":"file","file":{}}]},{"role":"tool","content":"result"}],"max_tokens":0,"temperature":0}`, false},
		{"legacy", `{"prompt":["Hi","中😊"],"functions":[{"name":"lookup"}],"function_call":{"name":"lookup"},"max_completion_tokens":0}`, false},
		{"token-prompt", `{"prompt":[1,2,3],"input":["A","B"]}`, false},
		{"null-tools", `{"messages":[],"tools":[null,{}],"tool_choice":null,"function_call":"auto"}`, false},
		{"compat-declarations", `{"messages":[],"tools":[{"type":"function","functionDeclarations":[{"name":"first"},{"name":"second"}]}]}`, false},
		{"compat-system", `{"messages":[{"role":"user","content":"Hi"}],"system":"system","instructions":"instructions"}`, false},
		{"escaped-key", `{"messa\u0067es":[{"role":"user","content":"Hi"}]}`, false},
		{"case-key", `{"Messages":[{"role":"user","content":"Hi"}]}`, false},
		{"duplicate-key", `{"messages":[{"content":"first"}],"messages":[{"content":"last"}]}`, false},
		{"claude-prompt-null", `{"messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"ok"}],"prompt":null}`, true},
		{"claude-tools", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"中😊"}]}],"system":[],"tools":[null,{"name":"lookup"}],"tool_choice":{"type":"any"},"temperature":0}`, true},
	} {
		for _, level := range []string{"all", "errors"} {
			t.Run(tc.name+"/"+level, func(t *testing.T) {
				original, err := common.CreateBodyStorage([]byte(tc.body))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, original.Close()) })
				want := &common.RequestTraceCollector{}
				want.CaptureRequest(original, true)
				context, _ := gin.CreateTestContext(httptest.NewRecorder())
				context.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
				context.Request.Header.Set("Content-Type", "application/json")
				trace := &common.RequestTraceCollector{Config: common.RequestTraceConfig{Level: level}, SampledSuccess: true}
				context.Set(common.RequestTraceContextKey, trace)
				t.Cleanup(func() { common.CleanupBodyStorage(context) })
				var value any = &dto.GeneralOpenAIRequest{}
				if tc.claude {
					value = &dto.ClaudeRequest{}
				}
				require.NoError(t, common.UnmarshalBodyReusable(context, value))
				if level == "errors" {
					assert.Nil(t, trace.Shape)
				}
				// 转换器修改业务 DTO 后，详情仍需保留客户端原始统计。
				switch request := value.(type) {
				case *dto.GeneralOpenAIRequest:
					request.Messages, request.Prompt = nil, "changed"
					if request.Temperature != nil {
						*request.Temperature = 1
					}
				case *dto.ClaudeRequest:
					request.Messages, request.System = nil, "changed"
				}
				storage, err := common.GetBodyStorage(context)
				require.NoError(t, err)
				trace.CaptureRequest(storage, true)
				assert.Equal(t, want.Shape, trace.Shape)
				assert.Equal(t, want.ShapeIncomplete, trace.ShapeIncomplete)
				assert.Equal(t, want.ShapeIncompleteReason, trace.ShapeIncompleteReason)
				assert.NotContains(t, common.GetJsonString(trace.Shape), "changed")
			})
		}
	}
}

func TestRequestTraceProtocolFieldMatrix(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, types.RelayFormatClaude, types.RelayFormatGemini} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				body := requestTraceProtocolPayload(format, stream)
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
				}
				for _, chunkSize := range []int{1, len(body)} {
					observer := common.NewTraceBodyObserver(time.Now())
					for offset := 0; offset < len(body); offset += chunkSize {
						observer.Observe([]byte(body[offset:min(offset+chunkSize, len(body))]), contentType)
					}
					observer.Finish()
					response, upstream, reason := observer.Snapshot()
					assert.Equal(t, "source-id", upstream.ResponseID)
					assert.Equal(t, "upstream-model", upstream.Model)
					assert.EqualValues(t, 1, response.ToolCalls)
					assert.Equal(t, map[string]int64{"lookup": 1}, response.ToolCallsByName)
					assert.False(t, response.MetadataIncomplete)
					assert.False(t, response.Error)
					require.NotNil(t, reason)
					reasons := map[types.RelayFormat]string{types.RelayFormatOpenAI: "tool_calls", types.RelayFormatOpenAIResponses: "completed", types.RelayFormatClaude: "tool_use", types.RelayFormatGemini: "STOP"}
					assert.Equal(t, reasons[format], *reason)
					if format == types.RelayFormatClaude {
						assert.Nil(t, response.ReasoningTokens)
					} else {
						require.NotNil(t, response.ReasoningTokens)
						assert.EqualValues(t, 3, *response.ReasoningTokens)
					}
					if format == types.RelayFormatOpenAI || format == types.RelayFormatOpenAIResponses {
						require.NotNil(t, upstream.Created)
						assert.EqualValues(t, 1700000000, *upstream.Created)
						assert.Equal(t, "priority", upstream.ServiceTier)
					}
					if format == types.RelayFormatOpenAI {
						assert.Equal(t, "fp-source", upstream.Fingerprint)
					}
					if stream {
						markers := map[types.RelayFormat]string{types.RelayFormatOpenAI: "[DONE]", types.RelayFormatOpenAIResponses: "response.completed", types.RelayFormatClaude: "message_stop"}
						assert.Equal(t, markers[format], response.EndMarker)
						assert.Equal(t, format != types.RelayFormatGemini, response.Done)
					} else {
						assert.True(t, response.Done)
					}
				}
			})
		}
	}
}

func TestRequestTraceDecodedMetadataParity(t *testing.T) {
	formats := []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, types.RelayFormatClaude, types.RelayFormatGemini}
	textFrames := map[types.RelayFormat]string{
		types.RelayFormatOpenAI:          `{"id":"source-id","model":"upstream-model","object":"chat.completion.chunk","created":1700000000,"system_fingerprint":"fp-source","choices":[{"index":0,"delta":{"content":"ordinary text with \"error\" and tool_calls","refusal":null,"tool_calls":null},"finish_reason":null}]}`,
		types.RelayFormatOpenAIResponses: `{"type":"response.output_text.delta","delta":"ordinary text","output_index":0,"content_index":0}`,
		types.RelayFormatClaude:          `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ordinary text"}}`,
		types.RelayFormatGemini:          `{"responseId":"source-id","modelVersion":"upstream-model","candidates":[{"content":{"parts":[{"text":"ordinary text"}]},"finishReason":null}]}`,
	}
	for _, format := range formats {
		for _, mode := range []string{"json", "sse", "text-only", "plain-json", "plain-sse"} {
			stream := mode != "json" && mode != "plain-json"
			for _, level := range []common.TraceMetadataLevel{common.TraceMetadataFull, common.TraceMetadataSummary} {
				t.Run(fmt.Sprintf("%s/%s/%s", format, mode, level), func(t *testing.T) {
					contentType := "application/json"
					payload := requestTraceProtocolPayload(format, stream)
					if strings.HasPrefix(mode, "plain-") {
						payload = strings.ReplaceAll(payload, `,"service_tier":"priority"`, "")
						payload = strings.ReplaceAll(payload, `"name":null,`, "")
					}
					if stream {
						contentType = "text/event-stream"
						if mode == "text-only" {
							payload = "data: " + textFrames[format] + "\n\n"
							if format == types.RelayFormatOpenAI {
								payload = "data: " + `{"id":null,"created":0,"system_fingerprint":null,"choices":[{"delta":{"content":"text","annotations":[{"id":"nested-id","model":"nested-model"}]},"finish_reason":null}]}` + "\n\n"
							} else if format == types.RelayFormatGemini {
								payload = "data: " + `{"responseId":null,"modelVersion":123,"candidates":[{"content":{"parts":[{"text":"text"}]}}]}` + "\n\n"
							}
						}
						payload = "data: " + textFrames[format] + "\n\n" + payload
					}
					original := common.NewTraceBodyObserverWithMode(time.Now(), level)
					original.ObserveString(payload, contentType)
					original.Finish()
					wantResponse, wantUpstream, wantReason := original.Snapshot()

					decoded := common.NewTraceBodyObserverWithMode(time.Now(), level)
					context, _ := gin.CreateTestContext(httptest.NewRecorder())
					context.Set(common.RequestTraceContextKey, &common.RequestTraceCollector{})
					response := &http.Response{Header: http.Header{"Content-Type": {contentType}}, ContentLength: int64(len(payload)), Body: &common.TraceResponseBody{ReadCloser: io.NopCloser(strings.NewReader(payload)), Observer: decoded, ContentType: contentType}}
					common.UseDecodedRequestTrace(context, response)
					body, err := io.ReadAll(response.Body)
					require.NoError(t, err)
					require.NoError(t, response.Body.Close())
					frames := []string{string(body)}
					if stream {
						frames = strings.Split(strings.TrimSpace(string(body)), "\n\n")
						if strings.Contains(string(body), "data: [DONE]") {
							common.RecordRequestTraceDone(response)
						}
					}
					for _, frame := range frames {
						if stream {
							frame = strings.TrimSpace(strings.TrimPrefix(frame, "data:"))
							if frame == "[DONE]" {
								common.RecordRequestTraceDone(response)
								continue
							}
						}
						var value any
						switch format {
						case types.RelayFormatOpenAI:
							if stream {
								value = &dto.ChatCompletionsStreamResponse{}
							} else {
								value = &dto.OpenAITextResponse{}
							}
						case types.RelayFormatOpenAIResponses:
							if stream {
								value = &dto.ResponsesStreamResponse{}
							} else {
								value = &dto.OpenAIResponsesResponse{}
							}
						case types.RelayFormatClaude:
							value = &dto.ClaudeResponse{}
						case types.RelayFormatGemini:
							value = &dto.GeminiChatResponse{}
						}
						require.NoError(t, common.UnmarshalRequestTraceResponse(context, []byte(frame), value))
					}
					decoded.Finish()
					gotResponse, gotUpstream, gotReason := decoded.Snapshot()
					assert.Equal(t, wantResponse, gotResponse)
					assert.Equal(t, wantUpstream, gotUpstream)
					assert.Equal(t, wantReason, gotReason)
					assert.Equal(t, original.MetadataLevel(), decoded.MetadataLevel())
				})
			}
		}
	}
}

type requestTraceShortWriter struct {
	*httptest.ResponseRecorder
}

func (w requestTraceShortWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	n, _ := w.ResponseRecorder.Write(data[:min(5, len(data))])
	return n, io.ErrShortWrite
}

func TestRequestTraceDecodedDelivery(t *testing.T) {
	previous := common.GetRequestTraceConfig()
	require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all"}`))
	t.Cleanup(func() {
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previous)))
	})
	for _, stream := range []bool{false, true} {
		for _, short := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/short=%t", stream, short), func(t *testing.T) {
				var observer *common.TraceBodyObserver
				app := gin.New()
				app.Use(middleware.RequestTraceMetadata())
				app.POST("/v1/chat/completions", func(c *gin.Context) {
					observer = common.GetRequestTrace(c).ClientObserver
					if stream {
						helper.SetEventStreamHeaders(c)
						_ = helper.ObjectData(c, &dto.ChatCompletionsStreamResponse{Choices: []dto.ChatCompletionsStreamResponseChoice{{Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{{Function: dto.FunctionResponse{Name: "lookup"}}}}}}})
					} else {
						c.Header("Content-Type", "application/json")
						service.IOCopyBytesGracefully(c, nil, []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"lookup"}}]},"finish_reason":"tool_calls"}]}`))
					}
				})
				recorder := httptest.NewRecorder()
				var writer http.ResponseWriter = recorder
				if short {
					writer = requestTraceShortWriter{recorder}
				}
				app.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
				require.NotNil(t, observer)
				response, _, _ := observer.Snapshot()
				assert.Equal(t, short, response.Error)
				assert.Equal(t, short, response.MetadataIncomplete)
				if short {
					assert.Zero(t, response.ToolCalls)
					assert.False(t, response.Done)
					assert.Contains(t, response.IncompleteReasons, "client_delivery_incomplete")
				} else {
					assert.Equal(t, map[string]int64{"lookup": 1}, response.ToolCallsByName)
					assert.Equal(t, !stream, response.Done)
				}
			})
		}
	}
}

func TestRequestTraceResponsesJSONWithSSEHeader(t *testing.T) {
	formats := []types.RelayFormat{types.RelayFormatOpenAIResponses, types.RelayFormatOpenAI, types.RelayFormatClaude, types.RelayFormatGemini}
	for _, level := range []string{"all", "errors"} {
		for _, format := range formats {
			t.Run(level+"/"+string(format), func(t *testing.T) {
				setupRequestTraceDatabase(t)
				require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(common.RequestTraceConfig{Level: level, SuccessSampleRate: 100})))
				payload := requestTraceProtocolPayload(types.RelayFormatOpenAIResponses, false)
				path := "/v1/chat/completions"
				switch format {
				case types.RelayFormatOpenAIResponses:
					path = "/v1/responses"
				case types.RelayFormatClaude:
					path = "/v1/messages"
				case types.RelayFormatGemini:
					path = "/v1beta/models/client-model:generateContent"
				}
				app := gin.New()
				app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
				app.POST(path, func(c *gin.Context) {
					c.Set("id", 11)
					_, err := common.GetBodyStorage(c)
					require.NoError(t, err)
					info := &relaycommon.RelayInfo{RelayFormat: format, FinalRequestRelayFormat: types.RelayFormatOpenAIResponses, RequestURLPath: path, RelayMode: relayconstant.RelayModeChatCompletions, OriginModelName: "client-model", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelId: 7, UpstreamModelName: "upstream-model"}}
					info.InitRequestConversionChain()
					trace := common.GetRequestTrace(c)
					attempt := trace.StartAttempt(0, 7, c.Request)
					attempt.Upstream.Status = http.StatusOK
					attempt.Observer = trace.ClientObserver.NewRelatedObserverWithMode(trace.ResponseMetadataLevel(7, http.StatusOK))
					response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, ContentLength: int64(len(payload)), Body: &common.TraceResponseBody{ReadCloser: io.NopCloser(strings.NewReader(payload)), ContentType: "text/event-stream", Observer: attempt.Observer}}
					handler := openai.OaiResponsesToChatHandler
					if format == types.RelayFormatOpenAIResponses {
						handler = openai.OaiResponsesHandler
					}
					usage, relayError := handler(c, info, response)
					require.Nil(t, relayError)
					require.NotNil(t, usage)
					assert.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
					assert.EqualValues(t, 10, usage.PromptTokens)
					assert.EqualValues(t, 9, usage.CompletionTokens)
					model.RecordConsumeLog(c, 11, model.RecordConsumeLogParams{ChannelId: 7, ModelName: "client-model", PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, Other: model.NewLogOther()})
					trace.FinishAttempt(0)
				})
				writer := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(requestTraceProtocolRequest(format)))
				request.Header.Set("Content-Type", "application/json")
				app.ServeHTTP(writer, request)
				require.Equal(t, http.StatusOK, writer.Code)
				assert.Equal(t, "text/event-stream", writer.Header().Get("Content-Type"))
				var body map[string]any
				require.NoError(t, common.Unmarshal(writer.Body.Bytes(), &body))
				awaitRequestTraceWrites(t)
				rows, err := model.GetRequestTraces(t.Context(), writer.Header().Get(common.RequestIdKey), 0, true)
				require.NoError(t, err)
				require.Len(t, rows, 1)
				admin := rows[0].AdminInfo
				require.NotNil(t, admin)
				assert.True(t, admin.Response.Done)
				assert.Empty(t, admin.Response.EndMarker)
				if level == "errors" && format == types.RelayFormatGemini {
					assert.Equal(t, "not_applicable", admin.Response.EndMarkerPolicy)
				} else {
					assert.Empty(t, admin.Response.EndMarkerPolicy)
				}
				assert.False(t, admin.Response.Error)
				assert.Equal(t, level == "errors", admin.Response.MetadataIncomplete)
				if level == "all" {
					assert.EqualValues(t, 1, admin.Response.ToolCalls)
					assert.Equal(t, map[string]int64{"lookup": 1}, admin.Response.ToolCallsByName)
					assert.Equal(t, common.GetPointer(int64(3)), admin.Response.ReasoningTokens)
					assert.Equal(t, "source-id", admin.Upstream.ResponseID)
					assert.Equal(t, "upstream-model", admin.Upstream.Model)
				} else {
					assert.Equal(t, common.TraceMetadataSummary, admin.MetadataLevel)
					assert.Zero(t, admin.Response.ToolCalls)
					assert.Empty(t, admin.Response.ToolCallsByName)
				}
				if format == types.RelayFormatOpenAIResponses {
					assert.JSONEq(t, payload, writer.Body.String())
					assert.Equal(t, common.GetPointer("completed"), rows[0].Trace.FinishReason)
				}
			})
		}
	}
}

func TestRequestTraceProtocolEdgeFields(t *testing.T) {
	hosted := `{"id":"source-id","object":"response","status":"completed","output":[{"type":"web_search_call","id":"ws-1","status":"completed"},{"type":"file_search_call","id":"fs-1","status":"completed"},{"type":"image_generation_call","id":"img-1","status":"completed"},{"type":"function_call","id":"fc-1","name":"lookup","arguments":"{}"}]}`
	hostedStream := "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws-1"}}` + "\n\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws-1","status":"completed"}}` + "\n\ndata: " + `{"type":"response.completed","response":` + hosted + `}` + "\n\n"
	for _, tc := range []struct {
		name, body, contentType, marker string
		tools                           map[string]int64
		failed, refused                 bool
	}{
		{name: "Responses done alias", body: strings.ReplaceAll(requestTraceProtocolPayload(types.RelayFormatOpenAIResponses, true), `"type":"response.completed"`, `"type":"response.done"`), contentType: "text/event-stream", marker: "response.done", tools: map[string]int64{"lookup": 1}},
		{name: "Responses content filter", body: "data: " + `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}` + "\n\n", contentType: "text/event-stream", marker: "response.incomplete", refused: true},
		{name: "Responses hosted tools JSON", body: hosted, contentType: "application/json", tools: map[string]int64{"web_search_call": 1, "file_search_call": 1, "image_generation_call": 1, "lookup": 1}},
		{name: "Responses hosted tools SSE", body: hostedStream, contentType: "text/event-stream", marker: "response.completed", tools: map[string]int64{"web_search_call": 1, "file_search_call": 1, "image_generation_call": 1, "lookup": 1}},
		{name: "string error", body: `{"error":"private-error-original"}`, contentType: "application/json", failed: true},
		{name: "Claude refusal", body: `{"id":"source-id","type":"message","content":[{"type":"text","text":"private-refusal-body"}],"stop_reason":"refusal"}`, contentType: "application/json", refused: true},
		{name: "Gemini blocked prompt", body: `{"promptFeedback":{"blockReason":"SAFETY"}}`, contentType: "application/json", refused: true},
		{name: "Gemini usage-only SSE", body: "data: " + `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}]}` + "\n\ndata: " + `{"usageMetadata":{"thoughtsTokenCount":3}}` + "\n\n", contentType: "text/event-stream", tools: map[string]int64{"lookup": 1}},
	} {
		for _, level := range []common.TraceMetadataLevel{common.TraceMetadataFull, common.TraceMetadataSummary} {
			t.Run(tc.name+"/"+string(level), func(t *testing.T) {
				observer := common.NewTraceBodyObserverWithMode(time.Now(), level)
				observer.ObserveString(tc.body, tc.contentType)
				observer.Finish()
				response, _, _ := observer.Snapshot()
				assert.Equal(t, tc.failed, response.Error)
				assert.Equal(t, tc.refused, response.Refused)
				assert.False(t, response.MetadataIncomplete)
				assert.Equal(t, tc.marker, response.EndMarker)
				if level == common.TraceMetadataFull {
					var tools int64
					for _, count := range tc.tools {
						tools += count
					}
					assert.Equal(t, tools, response.ToolCalls)
					assert.Equal(t, tc.tools, response.ToolCallsByName)
				} else {
					assert.Zero(t, response.ToolCalls)
				}
				assert.NotContains(t, common.GetJsonString(response), "private-")
			})
		}
	}
}

func TestRequestTraceReasoningTokenFields(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want        *int64
	}{
		{name: "missing", usage: `{}`},
		{name: "explicit zero", usage: `{"completion_tokens_details":{"reasoning_tokens":0}}`, want: common.GetPointer(int64(0))},
		{name: "flat upstream field", usage: `{"reasoning_tokens":3}`, want: common.GetPointer(int64(3))},
		{name: "standard field precedes flat field", usage: `{"completion_tokens_details":{"reasoning_tokens":0},"reasoning_tokens":3}`, want: common.GetPointer(int64(0))},
		{name: "null", usage: `{"completion_tokens_details":{"reasoning_tokens":null}}`},
		{name: "fraction", usage: `{"completion_tokens_details":{"reasoning_tokens":1.5}}`},
		{name: "negative", usage: `{"completion_tokens_details":{"reasoning_tokens":-1}}`},
		{name: "overflow", usage: `{"completion_tokens_details":{"reasoning_tokens":9223372036854775808}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := common.NewTraceBodyObserver(time.Now())
			body := `{"choices":[],"usage":` + tc.usage + `}`
			observer.ObserveString(body, "application/json")
			observer.Finish()
			response, _, _ := observer.Snapshot()
			assert.Equal(t, tc.want, response.ReasoningTokens)
			decoded := common.NewTraceBodyObserver(time.Now())
			decoded.Prepare("application/json", int64(len(body)))
			var value dto.OpenAITextResponse
			if err := common.UnmarshalJsonStr(body, &value); err != nil {
				decoded.ObserveDecoded([]byte(body), nil)
			} else {
				decoded.ObserveDecoded([]byte(body), &value)
			}
			decoded.Finish()
			actual, _, _ := decoded.Snapshot()
			assert.Equal(t, response, actual)
		})
	}
}

func TestRequestTraceAlternateRequestFields(t *testing.T) {
	snake := strings.NewReplacer("systemInstruction", "system_instruction", "toolConfig", "tool_config", "functionCallingConfig", "function_calling_config", "functionDeclarations", "function_declarations", "generationConfig", "generation_config", "maxOutputTokens", "max_output_tokens").Replace(requestTraceProtocolRequest(types.RelayFormatGemini))
	for _, tc := range []struct {
		name, body, choice    string
		messages, text, tools int64
		system                bool
	}{
		{name: "Completions string prompt", body: `{"prompt":"Hi中😊","max_tokens":128,"temperature":0}`, messages: 1, text: 4},
		{name: "Responses null instructions", body: `{"input":"Hi中😊","instructions":null,"max_output_tokens":128,"temperature":0}`, messages: 1, text: 4},
		{name: "Completions batch prompts", body: `{"prompt":["Hi","中😊"],"max_tokens":128,"temperature":0}`, messages: 2, text: 4},
		{name: "OpenAI legacy functions", body: `{"messages":[{"role":"user","content":"Hi中😊"}],"functions":[{"name":"lookup"}],"function_call":"auto","max_tokens":128,"temperature":0}`, messages: 1, text: 4, tools: 1, choice: "auto"},
		{name: "Gemini snake case", body: snake, messages: 1, text: 5, tools: 2, choice: "auto", system: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage, err := common.CreateBodyStorage([]byte(tc.body))
			require.NoError(t, err)
			defer storage.Close()
			collector := &common.RequestTraceCollector{}
			collector.CaptureRequest(storage, true)
			require.NotNil(t, collector.Shape)
			shape := collector.Shape
			assert.Equal(t, tc.messages, shape.Messages)
			assert.Equal(t, tc.text, shape.TextCharacters)
			assert.Equal(t, tc.tools, shape.Tools)
			assert.Equal(t, tc.choice, shape.ToolChoice)
			assert.Equal(t, tc.system, shape.System)
			require.NotNil(t, shape.MaxTokens)
			assert.EqualValues(t, 128, *shape.MaxTokens)
			require.NotNil(t, shape.Temperature)
			assert.Zero(t, *shape.Temperature)
		})
	}
}

func TestRequestTraceResponseMediaTypes(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		stream      bool
	}{
		{contentType: "text/event-stream; charset=utf-8", stream: true},
		{contentType: "Text/Event-Stream ; charset=utf-8", stream: true},
		{contentType: `application/json; example="text/event-stream"`},
	} {
		for _, prepared := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prepared=%t", tc.contentType, prepared), func(t *testing.T) {
				observer := common.NewTraceBodyObserver(time.Now())
				if prepared {
					observer.Prepare(tc.contentType, -1)
				}
				observer.ObserveString(requestTraceProtocolPayload(types.RelayFormatOpenAI, tc.stream), tc.contentType)
				observer.Finish()
				response, _, reason := observer.Snapshot()
				assert.Equal(t, map[string]int64{"lookup": 1}, response.ToolCallsByName)
				assert.True(t, response.Done)
				assert.False(t, response.MetadataIncomplete)
				require.NotNil(t, reason)
				assert.Equal(t, "tool_calls", *reason)
			})
		}
	}
}

func TestRequestTraceRequestMediaFields(t *testing.T) {
	for _, tc := range []struct {
		name, body                    string
		images, audios, videos, files int64
	}{
		{name: "OpenAI", body: `{"messages":[{"role":"user","content":[{"type":"text","text":"Hi"},{"type":"image_url","image_url":{"url":"private-media"}},{"type":"input_audio","input_audio":{"data":"private-media"}},{"type":"video_url","video_url":{"url":"private-media"}},{"type":"file","file":{"file_data":"private-media"}}]}]}`, images: 1, audios: 1, videos: 1, files: 1},
		{name: "Responses", body: `{"input":[{"role":"user","content":[{"type":"input_text","text":"Hi"},{"type":"input_image","image_url":"private-media"},{"type":"input_file","file_data":"private-media"}]}]}`, images: 1, files: 1},
		{name: "Claude", body: `{"messages":[{"role":"user","content":[{"type":"text","text":"Hi"},{"type":"image","source":{"data":"private-media"}},{"type":"document","source":{"data":"private-media"}}]}]}`, images: 1, files: 1},
		{name: "Gemini", body: `{"contents":[{"parts":[{"text":"Hi"},{"inlineData":{"mimeType":"image/png","data":"private-media"}},{"fileData":{"mimeType":"audio/wav","fileUri":"private-media"}},{"fileData":{"mimeType":"video/mp4","fileUri":"private-media"}},{"inlineData":{"mimeType":"application/pdf","data":"private-media"}}]}]}`, images: 1, audios: 1, videos: 1, files: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage, err := common.CreateBodyStorage([]byte(tc.body))
			require.NoError(t, err)
			defer storage.Close()
			collector := &common.RequestTraceCollector{}
			collector.CaptureRequest(storage, true)
			require.NotNil(t, collector.Shape)
			assert.EqualValues(t, 1, collector.Shape.Messages)
			assert.EqualValues(t, 2, collector.Shape.TextCharacters)
			assert.Equal(t, tc.images, collector.Shape.Images)
			assert.Equal(t, tc.audios, collector.Shape.Audios)
			assert.Equal(t, tc.videos, collector.Shape.Videos)
			assert.Equal(t, tc.files, collector.Shape.Files)
			assert.NotContains(t, common.GetJsonString(collector.Shape), "private-media")
		})
	}
}

func TestRequestTraceToolResultText(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "OpenAI", body: `{"messages":[{"role":"tool","tool_call_id":"call-1","content":"Hi中😊"}]}`},
		{name: "Claude", body: `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"Hi中😊"}]}]}`},
		{name: "Responses", body: `{"input":[{"type":"function_call_output","call_id":"call-1","output":"Hi中😊"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage, err := common.CreateBodyStorage([]byte(tc.body))
			require.NoError(t, err)
			defer storage.Close()
			collector := &common.RequestTraceCollector{}
			collector.CaptureRequest(storage, true)
			require.NotNil(t, collector.Shape)
			assert.EqualValues(t, 1, collector.Shape.Messages)
			assert.EqualValues(t, 4, collector.Shape.TextCharacters)
			assert.NotContains(t, common.GetJsonString(collector.Shape), "Hi中😊")
		})
	}
}

func TestRequestTraceConversionFieldMatrix(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
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
	jsonHandlers := map[types.RelayFormat]map[types.RelayFormat]handler{
		types.RelayFormatOpenAI: {
			types.RelayFormatOpenAI: openai.OpenaiHandler, types.RelayFormatOpenAIResponses: openai.OaiChatToResponsesHandler,
			types.RelayFormatClaude: openai.OpenaiHandler, types.RelayFormatGemini: openai.OpenaiHandler,
		},
		types.RelayFormatOpenAIResponses: {
			types.RelayFormatOpenAI: openai.OaiResponsesToChatHandler, types.RelayFormatOpenAIResponses: openai.OaiResponsesHandler,
			types.RelayFormatClaude: openai.OaiResponsesToChatHandler, types.RelayFormatGemini: openai.OaiResponsesToChatHandler,
		},
		types.RelayFormatClaude: {
			types.RelayFormatOpenAI: claudeJSON, types.RelayFormatOpenAIResponses: claudeJSON,
			types.RelayFormatClaude: claudeJSON, types.RelayFormatGemini: claudeJSON,
		},
		types.RelayFormatGemini: {
			types.RelayFormatOpenAI: gemini.GeminiChatHandler, types.RelayFormatOpenAIResponses: gemini.GeminiResponsesHandler,
			types.RelayFormatClaude: gemini.GeminiChatHandler, types.RelayFormatGemini: gemini.GeminiTextGenerationHandler,
		},
	}
	streamHandlers := map[types.RelayFormat]map[types.RelayFormat]handler{
		types.RelayFormatOpenAI: {
			types.RelayFormatOpenAI: openai.OaiStreamHandler, types.RelayFormatOpenAIResponses: openai.OaiChatToResponsesStreamHandler,
			types.RelayFormatClaude: openai.OaiStreamHandler, types.RelayFormatGemini: openai.OaiStreamHandler,
		},
		types.RelayFormatOpenAIResponses: {
			types.RelayFormatOpenAI: openai.OaiResponsesToChatStreamHandler, types.RelayFormatOpenAIResponses: openai.OaiResponsesStreamHandler,
			types.RelayFormatClaude: openai.OaiResponsesToChatStreamHandler, types.RelayFormatGemini: openai.OaiResponsesToChatStreamHandler,
		},
		types.RelayFormatClaude: {
			types.RelayFormatOpenAI: claudeStream, types.RelayFormatOpenAIResponses: claudeResponses,
			types.RelayFormatClaude: claudeStream, types.RelayFormatGemini: claudeStream,
		},
		types.RelayFormatGemini: {
			types.RelayFormatOpenAI: gemini.GeminiChatStreamHandler, types.RelayFormatOpenAIResponses: gemini.GeminiResponsesStreamHandler,
			types.RelayFormatClaude: gemini.GeminiChatStreamHandler, types.RelayFormatGemini: gemini.GeminiTextGenerationStreamHandler,
		},
	}
	formats := []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, types.RelayFormatClaude, types.RelayFormatGemini}
	for _, upstreamFormat := range formats {
		for _, clientFormat := range formats {
			for _, mode := range []struct{ stream, refused bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
				stream, refused := mode.stream, mode.refused
				t.Run(fmt.Sprintf("%s_to_%s/stream=%t/refused=%t", upstreamFormat, clientFormat, stream, refused), func(t *testing.T) {
					setupRequestTraceDatabase(t)
					require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all"}`))
					body := requestTraceProtocolRequest(clientFormat)
					if stream && clientFormat != types.RelayFormatGemini {
						var fields map[string]any
						require.NoError(t, common.Unmarshal([]byte(body), &fields))
						fields["stream"] = true
						encoded, err := common.Marshal(fields)
						require.NoError(t, err)
						body = string(encoded)
					}
					path := "/v1/chat/completions"
					if clientFormat == types.RelayFormatClaude {
						path = "/v1/messages"
					} else if clientFormat == types.RelayFormatOpenAIResponses {
						path = "/v1/responses"
					} else if clientFormat == types.RelayFormatGemini {
						path = "/v1beta/models/client-model:generateContent"
						if stream {
							path = "/v1beta/models/client-model:streamGenerateContent"
						}
					}
					app := gin.New()
					app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
					app.POST(path, func(c *gin.Context) {
						c.Set("id", 11)
						_, err := common.GetBodyStorage(c)
						require.NoError(t, err)
						info := &relaycommon.RelayInfo{RelayFormat: clientFormat, FinalRequestRelayFormat: upstreamFormat, IsStream: stream, RequestURLPath: path, RelayMode: relayconstant.RelayModeChatCompletions, ShouldIncludeUsage: true, OriginModelName: "client-model", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelId: 7, UpstreamModelName: "upstream-model"}}
						info.InitRequestConversionChain()
						info.ChannelSetting.PassThroughBodyEnabled = true
						if upstreamFormat != clientFormat {
							var request any
							switch clientFormat {
							case types.RelayFormatOpenAI:
								request = &dto.GeneralOpenAIRequest{}
							case types.RelayFormatOpenAIResponses:
								request = &dto.OpenAIResponsesRequest{}
							case types.RelayFormatClaude:
								request = &dto.ClaudeRequest{}
							case types.RelayFormatGemini:
								request = &dto.GeminiChatRequest{}
							}
							require.NoError(t, common.Unmarshal([]byte(body), request))
							converted, err := service.ConvertRequest(c, info, upstreamFormat, request)
							require.NoError(t, err)
							require.NotNil(t, converted.Value)
						}
						trace := common.GetRequestTrace(c)
						attempt := trace.StartAttempt(0, 7, c.Request)
						attempt.Upstream.Status, attempt.Upstream.Protocol, attempt.Upstream.RequestID = 200, "HTTP/2.0", "source-request-id"
						attempt.Observer = trace.ClientObserver.NewRelatedObserver()
						contentType := "application/json"
						handle := jsonHandlers[upstreamFormat][clientFormat]
						if stream {
							contentType, handle = "text/event-stream", streamHandlers[upstreamFormat][clientFormat]
						}
						payload := requestTraceProtocolPayload(upstreamFormat, stream)
						if refused {
							payload = requestTraceProtocolRefusalPayload(upstreamFormat, stream)
						}
						response := &http.Response{StatusCode: 200, Proto: "HTTP/2.0", Header: http.Header{"Content-Type": []string{contentType}}, Body: &common.TraceResponseBody{ReadCloser: io.NopCloser(strings.NewReader(payload)), ContentType: contentType, Observer: attempt.Observer}}
						usage, relayError := handle(c, info, response)
						require.Nil(t, relayError)
						require.NotNil(t, usage)
						other := model.NewLogOther()
						service.AppendRelayLogAdminInfo(c, info, other)
						model.RecordConsumeLog(c, 11, model.RecordConsumeLogParams{ChannelId: 7, ModelName: "client-model", PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, Other: other})
						trace.FinishAttempt(0)
					})
					recorder := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
					request.Header.Set("Content-Type", "application/json")
					app.ServeHTTP(recorder, request)
					require.Equal(t, 200, recorder.Code)
					awaitRequestTraceWrites(t)
					requestID := recorder.Header().Get(common.RequestIdKey)
					rows, err := model.GetRequestTraces(t.Context(), requestID, 0, true)
					require.NoError(t, err)
					require.Len(t, rows, 1)
					record := rows[0]
					require.NotNil(t, record.AdminInfo)
					admin := record.AdminInfo
					assert.Equal(t, "source-id", admin.Upstream.ResponseID)
					assert.Equal(t, "upstream-model", admin.Upstream.Model)
					assert.Equal(t, "source-request-id", admin.Upstream.RequestID)
					assert.Equal(t, "HTTP/2.0", admin.Upstream.Protocol)
					assert.Equal(t, 200, admin.Upstream.Status)
					require.NotNil(t, record.Trace.RequestBytes)
					assert.EqualValues(t, len(body), *record.Trace.RequestBytes)
					require.NotNil(t, admin.Shape)
					assert.EqualValues(t, 5, admin.Shape.TextCharacters)
					assert.True(t, admin.Shape.System)
					assert.EqualValues(t, 2, admin.Shape.Tools)
					assert.Equal(t, "auto", admin.Shape.ToolChoice)
					if refused {
						assert.Zero(t, admin.Response.ToolCalls)
						assert.Empty(t, admin.Response.ToolCallsByName)
					} else {
						assert.EqualValues(t, 1, admin.Response.ToolCalls)
						assert.Equal(t, map[string]int64{"lookup": 1}, admin.Response.ToolCallsByName)
					}
					assert.Equal(t, refused, admin.Response.Refused)
					assert.False(t, admin.Response.MetadataIncomplete)
					assert.False(t, admin.Response.Error)
					assert.Equal(t, "success", record.Trace.Outcome)
					if upstreamFormat == types.RelayFormatClaude {
						assert.Nil(t, admin.Response.ReasoningTokens)
					} else {
						require.NotNil(t, admin.Response.ReasoningTokens)
						assert.EqualValues(t, 3, *admin.Response.ReasoningTokens)
					}
					require.NotNil(t, record.Trace.FinishReason)
					reasons := map[types.RelayFormat]string{types.RelayFormatOpenAI: "tool_calls", types.RelayFormatOpenAIResponses: "completed", types.RelayFormatClaude: "tool_use", types.RelayFormatGemini: "STOP"}
					if !refused {
						assert.Equal(t, reasons[clientFormat], *record.Trace.FinishReason)
					}
					if stream {
						markers := map[types.RelayFormat]string{types.RelayFormatOpenAI: "[DONE]", types.RelayFormatOpenAIResponses: "response.completed", types.RelayFormatClaude: "message_stop"}
						marker := markers[clientFormat]
						if refused && clientFormat == types.RelayFormatOpenAIResponses && upstreamFormat != types.RelayFormatOpenAIResponses {
							marker = "response.incomplete"
						}
						assert.Equal(t, marker, admin.Response.EndMarker)
					}
					assert.NotContains(t, common.GetJsonString(record), "unused_tool")
					assert.NotContains(t, common.GetJsonString(record), "Hi中😊")
					assert.NotContains(t, common.GetJsonString(record), "private-refusal-body")
					owned, err := model.GetRequestTraces(t.Context(), requestID, 11, false)
					require.NoError(t, err)
					require.Len(t, owned, 1)
					assert.Nil(t, owned[0].AdminInfo)
					other, err := model.GetRequestTraces(t.Context(), requestID, 12, false)
					require.NoError(t, err)
					assert.Empty(t, other)
				})
			}
		}
	}
}

func TestRequestTraceNativeRelayMetadata(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	chat := `{"id":"chat-id","object":"chat.completion","model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	claudeJSON := `{"id":"claude-id","type":"message","model":"mock-model","role":"assistant","content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	geminiJSON := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"你好"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	for _, tc := range []struct {
		name, payload, reason string
		format                types.RelayFormat
		stream                bool
		handler               func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{name: "OpenAI native", payload: chat, reason: "stop", format: types.RelayFormatOpenAI, handler: openai.OpenaiHandler},
		{name: "OpenAI converted to Claude", payload: chat, reason: "end_turn", format: types.RelayFormatClaude, handler: openai.OpenaiHandler},
		{name: "Responses native", payload: `{"id":"resp-id","object":"response","status":"completed","model":"mock-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"你好"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, reason: "completed", format: types.RelayFormatOpenAIResponses, handler: openai.OaiResponsesHandler},
		{name: "Claude native", payload: claudeJSON, reason: "end_turn", format: types.RelayFormatClaude, handler: func(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeHandler(c, response, info)
		}},
		{name: "Gemini native", payload: geminiJSON, reason: "STOP", format: types.RelayFormatGemini, handler: gemini.GeminiTextGenerationHandler},
		{name: "Gemini native stream", payload: "data: " + geminiJSON + "\n\n", reason: "STOP", format: types.RelayFormatGemini, stream: true, handler: gemini.GeminiTextGenerationStreamHandler},
		{name: "Claude native stream", payload: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"claude-id\",\"type\":\"message\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n", reason: "end_turn", format: types.RelayFormatClaude, stream: true, handler: func(c *gin.Context, info *relaycommon.RelayInfo, response *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeStreamHandler(c, response, info)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupRequestTraceDatabase(t)
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", `{"level":"all"}`))
			app := gin.New()
			app.Use(middleware.RequestId(), middleware.RequestTraceMetadata(), middleware.BodyStorageCleanup())
			app.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("id", 11)
				_, err := common.GetBodyStorage(c)
				require.NoError(t, err)
				trace := common.GetRequestTrace(c)
				attempt := trace.StartAttempt(0, 7, c.Request)
				attempt.Upstream.Status = 200
				attempt.Observer = common.NewTraceBodyObserver(time.Now())
				contentType := "application/json"
				if tc.stream {
					contentType = "text/event-stream"
				}
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: &common.TraceResponseBody{ReadCloser: io.NopCloser(strings.NewReader(tc.payload)), ContentType: contentType, Observer: attempt.Observer}}
				info := &relaycommon.RelayInfo{RelayFormat: tc.format, IsStream: tc.stream, RequestURLPath: c.Request.URL.Path, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, ChannelId: 7, UpstreamModelName: "mock-model"}}
				info.ChannelSetting.PassThroughBodyEnabled = true
				usage, relayError := tc.handler(c, info, response)
				require.Nil(t, relayError)
				require.NotNil(t, usage)
			})
			recorder := httptest.NewRecorder()
			app.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"测试"}]}`)))
			require.Equal(t, 200, recorder.Code)
			awaitRequestTraceWrites(t)
			rows, err := model.GetRequestTraces(t.Context(), recorder.Header().Get(common.RequestIdKey), 0, true)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.NotNil(t, rows[0].AdminInfo)
			require.NotNil(t, rows[0].Trace.FinishReason)
			assert.Equal(t, tc.reason, *rows[0].Trace.FinishReason)
			assert.Equal(t, common.TraceMetadataFull, rows[0].AdminInfo.MetadataLevel)
			assert.False(t, rows[0].AdminInfo.Response.MetadataIncomplete)
		})
	}
}

func TestRequestTraceSummaryAndPromotion(t *testing.T) {
	t.Run("related full observer does not leak detailed counts into summary", func(t *testing.T) {
		client := common.NewTraceBodyObserverWithMode(time.Now(), common.TraceMetadataSummary)
		upstream := client.NewRelatedObserverWithMode(common.TraceMetadataFull)
		body := `{"id":"source-id","choices":[{"message":{"content":"你好"},"finish_reason":"stop"}]}`
		upstream.ObserveString(body, "application/json")
		upstream.Finish()
		client.ObserveString(body, "application/json")
		client.Finish()
		_, fullInfo, _ := upstream.Snapshot()
		_, summaryInfo, reason := client.Snapshot()
		assert.Equal(t, "source-id", fullInfo.ResponseID)
		assert.Empty(t, summaryInfo.ResponseID)
		require.NotNil(t, reason)
		assert.Equal(t, "stop", *reason)
	})
	for _, tc := range []struct {
		name, payload, reason, marker string
		error, full, partial          bool
	}{
		{name: "chat null finish reasons", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"私密\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", reason: "stop", marker: "[DONE]"},
		{name: "tool deltas stay summary", payload: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"search\"}}]},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n", marker: "[DONE]"},
		{name: "error words in text do not fail", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"error response.failed\"}}]}\n\ndata: [DONE]\n\n", marker: "[DONE]"},
		{name: "null error stays summary", payload: "data: {\"error\":null}\n\ndata: [DONE]\n\n", marker: "[DONE]"},
		{name: "claude stop", payload: "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n", reason: "end_turn", marker: "message_stop"},
		{name: "gemini stop", payload: "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n", reason: "STOP"},
		{name: "responses complete", payload: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{}}}\n\n", reason: "completed", marker: "response.completed"},
		{name: "first event error", payload: "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded\",\"code\":\"busy\"}}\n\n", error: true, full: true},
		{name: "late error", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"私密\"}}]}\n\ndata: {\"error\":{\"type\":\"overloaded\"}}\n\n", error: true, full: true, partial: true},
		{name: "responses failed", payload: "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", reason: "failed", marker: "response.failed", error: true, full: true},
		{name: "responses incomplete", payload: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"私密\"}\n\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", reason: "incomplete:max_output_tokens", marker: "response.incomplete", full: true, partial: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observer := common.NewTraceBodyObserverWithMode(time.Now(), common.TraceMetadataSummary)
			// 网络分片可切在字段名、UTF-8 字符或事件分隔符内部。
			for offset := 0; offset < len(tc.payload); offset += 7 {
				observer.ObserveString(tc.payload[offset:min(offset+7, len(tc.payload))], "text/event-stream")
			}
			observer.Finish()
			response, _, reason := observer.Snapshot()
			assert.Equal(t, tc.error, response.Error)
			assert.Equal(t, tc.full, observer.MetadataLevel() == common.TraceMetadataFull)
			assert.Equal(t, tc.partial, response.MetadataIncomplete)
			assert.Equal(t, tc.marker, response.EndMarker)
			if tc.reason != "" {
				require.NotNil(t, reason)
				assert.Equal(t, tc.reason, *reason)
			}
			assert.Zero(t, response.ToolCalls)
			if tc.partial {
				assert.Contains(t, response.IncompleteReasons, "capture_upgraded")
			}
		})
	}
	t.Run("summary JSON size limit still marks incomplete metadata", func(t *testing.T) {
		payload := `{"choices":[{"message":{"content":"` + strings.Repeat("x", 64<<10) + `"},"finish_reason":"stop"}]}`
		for _, level := range []common.TraceMetadataLevel{common.TraceMetadataSummary, common.TraceMetadataFull} {
			observer := common.NewTraceBodyObserverWithMode(time.Now(), level)
			observer.Prepare("application/json", int64(len(payload)))
			observer.ObserveString(payload, "application/json")
			observer.Finish()
			response, _, _ := observer.Snapshot()
			assert.Equal(t, level == common.TraceMetadataSummary, response.MetadataIncomplete)
			if level == common.TraceMetadataSummary {
				assert.Contains(t, response.IncompleteReasons, "size_limit")
			}
		}
	})
	t.Run("upstream read failure upgrades and preserves the failed request", func(t *testing.T) {
		observer := common.NewTraceBodyObserverWithMode(time.Now(), common.TraceMetadataSummary)
		body := &common.TraceResponseBody{
			ReadCloser: io.NopCloser(io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"私密\"}}]}\n\n"), iotest.ErrReader(io.ErrUnexpectedEOF))),
			Observer:   observer, ContentType: "text/event-stream",
		}
		_, err := io.ReadAll(body)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
		require.NoError(t, body.Close())
		response, _, _ := observer.Snapshot()
		assert.True(t, response.Error)
		assert.Equal(t, common.TraceMetadataFull, observer.MetadataLevel())
		assert.Contains(t, response.IncompleteReasons, "upstream_read_error")
		assert.Contains(t, response.IncompleteReasons, "capture_upgraded")
	})
	t.Run("reuse preserves source and delivery failure stays local", func(t *testing.T) {
		source := common.NewTraceBodyObserver(time.Now())
		source.ObserveString(`{"choices":[{"message":{"content":"你好"},"finish_reason":"stop"}]}`, "application/json")
		source.Finish()
		client := common.NewTraceBodyObserver(time.Now())
		client.ReuseUpstream(source)
		client.ObserveString("forwarded", "application/json")
		client.DeliveryFailed()
		client.Finish()
		response, _, _ := client.Snapshot()
		assert.True(t, response.Error)
		assert.False(t, response.Done)
		assert.Contains(t, response.IncompleteReasons, "client_delivery_incomplete")
		upstream, _, _ := source.Snapshot()
		assert.False(t, upstream.Error)
		assert.True(t, upstream.Done)
	})
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
	engine.GET("/api/log/trace/status", middleware.RootAuth(), controller.GetRequestTraceMetadataStatus)
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
