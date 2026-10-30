package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
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
	"github.com/QuantumNous/new-api/pkg/requesttimeline"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestRequestTimelineBusinessErrorSurvivesDone(t *testing.T) {
	trace := requesttimeline.New(time.Now(), "business-error", 7, "example-model")
	req := httptest.NewRequest("POST", "http://upstream.invalid/v1/chat/completions", strings.NewReader(`{"model":"example-model"}`))
	req = trace.Request(req, 2, "sk-secret-example")
	httptrace.ContextClientTrace(req.Context()).WroteRequest(httptrace.WroteRequestInfo{})
	trace.Response(&http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"upstream-id"}}}, nil)
	trace.FirstResponse(time.Now())
	trace.ObserveChat(dto.ChatCompletionsStreamResponse{Id: "response-id", Model: "example-model"}, requesttimeline.StreamError{Present: true, Code: "overloaded", Message: "do not expose sk-secret-example"})
	trace.StreamEnd("done", nil, false)
	trace.PayloadWritten(nil)
	record := trace.Freeze(nil)
	require.NotNil(t, record)
	assert.Equal(t, "failed", record.Result)
	require.NotNil(t, record.ErrorKind)
	assert.Equal(t, "upstream_business_error", *record.ErrorKind)
	assert.True(t, record.Data.EndMarkerReceived)
	assert.Equal(t, "upstream-id", record.Data.UpstreamRequestID)
	assert.NotContains(t, record.Data.FirstError.Message, "sk-secret-example")
	assert.Nil(t, record.Data.ReturnedUsage)
	trace.BusinessError("late", "must not change frozen data")
	assert.Equal(t, "overloaded", record.Data.FirstError.Code)
}

func TestRequestTimelineUnknownLengthAndRepeatedSend(t *testing.T) {
	trace := requesttimeline.New(time.Now(), "unknown-size", 7, "model")
	req := httptest.NewRequest("POST", "http://upstream.invalid", strings.NewReader("body"))
	req.ContentLength = -1
	req = trace.Request(req, 1, "")
	hook := httptrace.ContextClientTrace(req.Context())
	hook.WroteRequest(httptrace.WroteRequestInfo{})
	trace.FirstResponse(time.Now())
	record := trace.Freeze(nil)
	assert.Nil(t, record.RequestBodyBytes)
	assert.Equal(t, "unknown", record.Result)
	assert.Nil(t, record.Data.FirstWriteMS)

	retried := requesttimeline.New(time.Now(), "retry", 7, "model")
	req = retried.Request(httptest.NewRequest("POST", "http://upstream.invalid", nil), 1, "")
	hook = httptrace.ContextClientTrace(req.Context())
	hook.WroteRequest(httptrace.WroteRequestInfo{})
	hook.WroteRequest(httptrace.WroteRequestInfo{})
	assert.False(t, retried.Candidate())
	assert.Nil(t, retried.Freeze(nil))
}

func TestRequestTimelineResponseWriterIgnoresHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	trace := requesttimeline.New(time.Now(), "writer", 7, "model")
	restore := helper.ObserveRequestTimelineWriter(c, trace)
	defer restore()
	heartbeat := c.Writer.(interface{ WriteHeartbeat([]byte) (int, error) })
	_, err := heartbeat.WriteHeartbeat([]byte(": PING\n\n"))
	require.NoError(t, err)
	c.Writer.Flush()
	trace.StreamEnd("done", nil, false)
	record := trace.Freeze(nil)
	assert.Nil(t, record.Data.FirstWriteMS)
	assert.Equal(t, "unknown", record.Result)
	assert.Equal(t, ": PING\n\n", w.Body.String())
}

func TestRequestTimelineQueueDrainsAndReportsWriteFailures(t *testing.T) {
	var written []*model.RequestTimeline
	w := service.NewRequestTimelineWriter(2, func(_ context.Context, rows []*model.RequestTimeline) error {
		written = append(written, rows...)
		return nil
	})
	t1 := requesttimeline.New(time.Now(), "queue-1", 7, "model")
	assert.True(t, w.Enqueue(t1.Freeze(nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.Shutdown(ctx))
	require.Len(t, written, 1)
	assert.Equal(t, "queue-1", written[0].RequestID)
	var payload requesttimeline.Data
	require.NoError(t, common.UnmarshalJsonStr(written[0].Data, &payload))
	assert.Equal(t, 1, payload.Version)
	assert.False(t, w.Enqueue(requesttimeline.New(time.Now(), "closed", 7, "model").Freeze(nil)))

	failed := service.NewRequestTimelineWriter(2, func(context.Context, []*model.RequestTimeline) error { return errors.New("forced table failure") })
	assert.True(t, failed.Enqueue(requesttimeline.New(time.Now(), "failed-write", 7, "model").Freeze(nil)))
	require.NoError(t, failed.Shutdown(ctx))
	assert.Equal(t, int64(1), failed.Stats().Failed)
}

func preserveTimelineOption(t *testing.T) {
	t.Helper()
	previousEnabled := model.RequestTimelineCaptureEnabled.Load()
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = make(map[string]string, len(previousOptions))
	for key, value := range previousOptions {
		common.OptionMap[key] = value
	}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		model.RequestTimelineCaptureEnabled.Store(previousEnabled)
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
	})
}

func TestRequestTimelineAdminLookupAndCandidateGate(t *testing.T) {
	admin, token := setupAccessTokenAudit(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
	preserveTimelineOption(t)
	model.RequestTimelineCaptureEnabled.Store(false)
	writer := service.InitRequestTimeline()
	require.NotNil(t, writer)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, writer.Shutdown(ctx))
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	c.Set(common.RequestIdKey, "gate")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	info := &relaycommon.RelayInfo{IsStream: true, StartTime: time.Now(), UserId: admin.Id, OriginModelName: "model", RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions}
	assert.False(t, service.RequestTimelineEnabled())
	assert.Nil(t, service.BeginRequestTimeline(c, info))
	require.NoError(t, model.UpdateOption("RequestTimelineEnabled", "true"))
	started := service.BeginRequestTimeline(c, info)
	require.NotNil(t, started)
	var persisted model.Option
	require.NoError(t, model.DB.Where(&model.Option{Key: "RequestTimelineEnabled"}).Take(&persisted).Error)
	assert.Equal(t, "true", persisted.Value)
	require.Error(t, model.UpdateOption("RequestTimelineEnabled", "invalid"))
	assert.True(t, service.RequestTimelineEnabled())
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("timeline:reject-option", func(db *gorm.DB) {
		if db.Statement.Table == "options" {
			db.AddError(errors.New("forced option save failure"))
		}
	}))
	saveErr := model.UpdateOption("RequestTimelineEnabled", "false")
	require.NoError(t, model.DB.Callback().Update().Remove("timeline:reject-option"))
	require.Error(t, saveErr)
	assert.True(t, service.RequestTimelineEnabled(), "failed saves must not change runtime collection")
	require.NoError(t, model.UpdateOption("RequestTimelineEnabled", "false"))
	assert.Nil(t, service.BeginRequestTimeline(c, info))
	assert.True(t, started.Candidate(), "disabling affects new requests, not an existing collector")
	require.NoError(t, model.UpdateOption("RequestTimelineEnabled", "true"))
	info.IsChannelTest = true
	assert.Nil(t, service.BeginRequestTimeline(c, info))
	info.IsChannelTest, info.IsStream = false, false
	assert.Nil(t, service.BeginRequestTimeline(c, info))
	info.IsStream = true
	info.RelayFormat = types.RelayFormatOpenAIRealtime
	assert.Nil(t, service.BeginRequestTimeline(c, info))

	payload, err := common.Marshal(requesttimeline.Data{Version: 1, StartedAtMS: time.Now().UnixMilli(), TimingValid: true})
	require.NoError(t, err)
	require.NoError(t, model.InsertRequestTimelines(context.Background(), []*model.RequestTimeline{{RequestID: "lookup", UserID: int64(admin.Id), CreatedAt: time.Now().Unix(), Result: "unknown", Data: string(payload)}}))
	r := gin.New()
	r.Use(middleware.RequestId(), middleware.AccessTokenAudit())
	r.GET("/api/request-timelines/:request_id", middleware.AdminAuth(), GetRequestTimeline)
	require.NoError(t, model.UpdateOption("RequestTimelineEnabled", "false"))
	response := auditRequest(r, "GET", "/api/request-timelines/lookup", token)
	require.Equal(t, 200, response.Code)
	assert.Contains(t, response.Body.String(), `"found":true`)
	assert.Contains(t, response.Body.String(), `"enabled":false`)
	assert.Contains(t, response.Body.String(), `"status":"available"`)
	assert.Equal(t, 401, auditRequest(r, "GET", "/api/request-timelines/lookup", "wrong-token").Code)
	ordinaryToken := "ordinary-timeline-token"
	ordinary := &model.User{Username: "timeline-user", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AccessToken: &ordinaryToken, AuthVersion: 1, AffCode: "timeline-user"}
	require.NoError(t, model.DB.Create(ordinary).Error)
	assert.Equal(t, 403, auditRequest(r, "GET", "/api/request-timelines/lookup", ordinaryToken).Code)
	assert.Equal(t, 400, auditRequest(r, "GET", "/api/request-timelines/bad!", token).Code)
	assert.Contains(t, auditRequest(r, "GET", "/api/request-timelines/absent", token).Body.String(), `"status":"missing"`)
}

func TestRequestTimelineDatabaseRoundtrip(t *testing.T) {
	dialector := gorm.Dialector(sqlite.Open(filepath.Join(t.TempDir(), "logs.db")))
	dbType := common.DatabaseTypeSQLite
	if dsn := os.Getenv("TIMELINE_TEST_MYSQL_DSN"); dsn != "" {
		dialector, dbType = mysql.Open(dsn), common.DatabaseTypeMySQL
	}
	if dsn := os.Getenv("TIMELINE_TEST_POSTGRES_DSN"); dsn != "" {
		dialector, dbType = postgres.Open(dsn), common.DatabaseTypePostgreSQL
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	require.NoError(t, err)
	mainDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "main.db")), &gorm.Config{})
	require.NoError(t, err)
	previousMain, previousLog, previousType := model.DB, model.LOG_DB, common.LogDatabaseType()
	model.DB, model.LOG_DB = mainDB, db
	if os.Getenv("TIMELINE_TEST_SHARED_DB") == "true" {
		model.DB = db
	}
	common.SetLogDatabaseType(dbType)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousMain, previousLog
		common.SetLogDatabaseType(previousType)
		for _, g := range []*gorm.DB{db, mainDB} {
			sqlDB, err := g.DB()
			if err == nil {
				_ = sqlDB.Close()
			}
		}
	})
	preserveTimelineOption(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Option{}))
	require.NoError(t, model.UpdateOption("RequestTimelineEnabled", "false"))
	assert.False(t, model.RequestTimelineCaptureEnabled.Load())
	require.NoError(t, model.UpdateOption("RequestTimelineEnabled", "true"))
	assert.True(t, model.RequestTimelineCaptureEnabled.Load())
	var savedOption model.Option
	require.NoError(t, model.DB.Where(&model.Option{Key: "RequestTimelineEnabled"}).Take(&savedOption).Error)
	assert.Equal(t, "true", savedOption.Value)
	var version string
	query := "select version()"
	if dbType == common.DatabaseTypeSQLite {
		query = "select sqlite_version()"
	}
	require.NoError(t, db.Raw(query).Scan(&version).Error)
	t.Logf("database: %s %s", dialector.Name(), version)
	require.NoError(t, db.AutoMigrate(&model.Log{}))
	legacy := model.Log{UserId: 7, Type: model.LogTypeConsume, CreatedAt: 1000, Content: "existing log", Quota: 500, RequestId: "legacy-id", Other: `{"frt":3}`}
	require.NoError(t, db.Create(&legacy).Error)
	require.NoError(t, model.MigrateRequestTimelines())
	require.NoError(t, model.MigrateRequestTimelines())
	assert.False(t, mainDB.Migrator().HasTable(&model.RequestTimeline{}))
	payload, err := common.Marshal(requesttimeline.Data{Version: 1, StartedAtMS: 1000000, RequestModel: "中文模型", TimingValid: true})
	require.NoError(t, err)
	size, status := int64(0), 200
	entries := []*model.RequestTimeline{
		{RequestID: "Timeline-Case", UserID: 7, CreatedAt: 1000, Result: "success", UpstreamStatus: &status, UpstreamRequestBodyBytes: &size, Data: string(payload)},
		{RequestID: "timeline-case", UserID: 8, CreatedAt: 1001, Result: "unknown", Data: string(payload)},
	}
	require.NoError(t, model.InsertRequestTimelines(context.Background(), entries))
	require.NoError(t, model.InsertRequestTimelines(context.Background(), entries))
	require.NoError(t, model.MigrateRequestTimelines())
	found, err := model.FindRequestTimeline(context.Background(), "Timeline-Case")
	require.NoError(t, err)
	assert.Equal(t, int64(7), found.UserID)
	require.NotNil(t, found.UpstreamRequestBodyBytes)
	assert.Zero(t, *found.UpstreamRequestBodyBytes)
	lower, err := model.FindRequestTimeline(context.Background(), "timeline-case")
	require.NoError(t, err)
	assert.Equal(t, int64(8), lower.UserID)
	assert.Nil(t, lower.UpstreamRequestBodyBytes)
	var decoded requesttimeline.Data
	require.NoError(t, common.UnmarshalJsonStr(found.Data, &decoded))
	assert.Equal(t, "中文模型", decoded.RequestModel)
	var count int64
	require.NoError(t, db.Model(&model.RequestTimeline{}).Count(&count).Error)
	assert.Equal(t, int64(2), count)
	var retained model.Log
	require.NoError(t, db.First(&retained, legacy.Id).Error)
	assert.Equal(t, legacy.Quota, retained.Quota)
	assert.Equal(t, legacy.Other, retained.Other)

	// Additive fields must preserve both existing records and old writers.
	for _, column := range []struct{ name, ddl string }{
		{"future_note", "ALTER TABLE request_timelines ADD COLUMN future_note TEXT"},
		{"future_flag", "ALTER TABLE request_timelines ADD COLUMN future_flag INTEGER NOT NULL DEFAULT 0"},
	} {
		if !db.Migrator().HasColumn(&model.RequestTimeline{}, column.name) {
			require.NoError(t, db.Exec(column.ddl).Error)
		}
	}
	require.NoError(t, model.MigrateRequestTimelines())
	require.NoError(t, model.MigrateRequestTimelines())
	retainedTimeline, err := model.FindRequestTimeline(context.Background(), "Timeline-Case")
	require.NoError(t, err)
	assert.Equal(t, found.Data, retainedTimeline.Data)
	afterUpgrade := &model.RequestTimeline{RequestID: "after-upgrade", UserID: 7, CreatedAt: 1002, Result: "unknown", Data: string(payload)}
	require.NoError(t, model.InsertRequestTimelines(context.Background(), []*model.RequestTimeline{afterUpgrade}))
	var defaults struct {
		FutureNote *string
		FutureFlag int
	}
	require.NoError(t, db.Table("request_timelines").Select("future_note", "future_flag").Where("request_id = ?", afterUpgrade.RequestID).Take(&defaults).Error)
	assert.Nil(t, defaults.FutureNote)
	assert.Zero(t, defaults.FutureFlag)
	require.NoError(t, db.Where("request_id = ?", afterUpgrade.RequestID).Delete(&model.RequestTimeline{}).Error)

	removed, err := model.DeleteOldRequestTimelineBatch(context.Background(), 1000, 500)
	require.NoError(t, err)
	assert.Zero(t, removed)
	removed, err = model.DeleteOldRequestTimelineBatch(context.Background(), 1001, 500)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)
	_, err = model.FindRequestTimeline(context.Background(), "timeline-case")
	require.NoError(t, err)
	removed, err = model.DeleteOldRequestTimelineBatch(context.Background(), 1002, 500)
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)
	// Required fields without defaults are still incompatible with this writer.
	require.NoError(t, db.Exec("ALTER TABLE request_timelines ADD COLUMN future_required TEXT NOT NULL").Error)
	t.Cleanup(func() { require.NoError(t, db.Migrator().DropColumn(&model.RequestTimeline{}, "future_required")) })
	require.ErrorContains(t, model.MigrateRequestTimelines(), "extra required column")
}

func TestRequestTimelineRealStreamAndFailureAdapters(t *testing.T) {
	previous := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previous })
	for _, tc := range []struct {
		name, body, want string
		handler          func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"chat", "data: {\"id\":\"r1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n", "success", openai.OaiStreamHandler},
		{"chat_error", "data: {\"error\":{\"code\":\"server_error\",\"message\":\"upstream failed\"}}\n\ndata: [DONE]\n\n", "failed", openai.OaiStreamHandler},
		{"responses_error", "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"upstream failed\"}\n\ndata: [DONE]\n\n", "failed", openai.OaiResponsesStreamHandler},
		{"converted_error", "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"upstream failed\"}\n\ndata: [DONE]\n\n", "failed", openai.OaiResponsesToChatStreamHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outputs []string
			for _, enabled := range []bool{false, true} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
				c.Set(common.RequestIdKey, "stream-adapter")
				start := time.Now()
				info := relaycommon.GenRelayInfoOpenAI(c, nil)
				info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}
				info.IsStream, info.DisablePing, info.ShouldIncludeUsage = true, true, true
				start = info.StartTime
				if enabled {
					info.Timeline = requesttimeline.New(start, tc.name, 7, "model")
					restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
					defer restore()
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
				_, relayErr := tc.handler(c, info, resp)
				require.Nil(t, relayErr)
				outputs = append(outputs, w.Body.String())
				if enabled {
					record := info.Timeline.Freeze(nil)
					require.NotNil(t, record)
					assert.Equal(t, tc.want, record.Result)
					require.NotNil(t, record.Data.FirstResponseMS)
					assert.Equal(t, info.FirstResponseTime.UnixMilli()-info.StartTime.UnixMilli(), *record.Data.FirstResponseMS)
					require.NotNil(t, record.Data.FirstWriteMS)
					require.NotNil(t, record.Data.EndedMS)
					assert.LessOrEqual(t, *record.Data.FirstWriteMS, *record.Data.EndedMS)
					if tc.name == "chat" {
						require.NotNil(t, record.Data.ReturnedUsage)
						assert.Equal(t, 3, record.Data.ReturnedUsage.TotalTokens)
					}
				}
			}
			assert.Equal(t, outputs[0], outputs[1], "observing must not change the returned stream")
		})
	}
}

// Real transport callbacks must measure the final body without consuming it.
func TestRequestTimelineTransportAndTerminalStates(t *testing.T) {
	body := `{"model":"中文","stream":true}`
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		received <- string(raw)
		w.Header().Set("X-Request-Id", "upstream-real")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()
	trace := requesttimeline.New(time.Now(), "real-transport", 7, "model")
	req, err := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader(body))
	require.NoError(t, err)
	resp, err := upstream.Client().Do(trace.Request(req, 1, ""))
	require.NoError(t, err)
	defer resp.Body.Close()
	trace.Response(resp, nil)
	record := trace.Freeze(nil)
	assert.Equal(t, body, <-received)
	require.NotNil(t, record.RequestBodyBytes)
	assert.Equal(t, int64(len(body)), *record.RequestBodyBytes)
	require.NotNil(t, record.Data.RequestSentMS)
	assert.Equal(t, "upstream-real", record.Data.UpstreamRequestID)
	for _, tc := range []struct {
		name, reason, want, kind string
		clientErr                error
		processing               bool
	}{
		{"eof_without_marker", "eof", "unknown", "unconfirmed_end", nil, false},
		{"timeout", "timeout", "interrupted", "timeout", nil, false},
		{"client_close_causes_read_error", "scanner_error", "interrupted", "client_disconnected", context.Canceled, false},
		{"upstream_read_failure", "scanner_error", "interrupted", "upstream_read_error", nil, false},
		{"parse_error_then_done", "done", "failed", "local_error", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := requesttimeline.New(time.Now(), tc.name, 7, "model")
			tr.FirstResponse(time.Now())
			tr.PayloadWritten(nil)
			tr.StreamEnd(tc.reason, tc.clientErr, tc.processing)
			row := tr.Freeze(nil)
			assert.Equal(t, tc.want, row.Result)
			require.NotNil(t, row.ErrorKind)
			assert.Equal(t, tc.kind, *row.ErrorKind)
		})
	}
}

type timelineBrokenWriter struct{ header http.Header }

func (w *timelineBrokenWriter) Header() http.Header       { return w.header }
func (w *timelineBrokenWriter) WriteHeader(int)           {}
func (w *timelineBrokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (w *timelineBrokenWriter) Flush()                    {}

func TestRequestTimelineCapturesSwallowedWriteFailure(t *testing.T) {
	c, _ := gin.CreateTestContext(&timelineBrokenWriter{header: make(http.Header)})
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	trace := requesttimeline.New(time.Now(), "broken-writer", 7, "model")
	restore := helper.ObserveRequestTimelineWriter(c, trace)
	defer restore()
	_ = helper.StringData(c, `{"choices":[{"delta":{"content":"hello"}}]}`)
	trace.StreamEnd("done", nil, false)
	record := trace.Freeze(nil)
	assert.Equal(t, "interrupted", record.Result)
	require.NotNil(t, record.ErrorKind)
	assert.Equal(t, "downstream_write_error", *record.ErrorKind)
	assert.Nil(t, record.Data.FirstWriteMS)
}

func TestRequestTimelineQueueFullDropsWithoutBlockingRelay(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	writer := service.NewRequestTimelineWriter(32, func(ctx context.Context, rows []*model.RequestTimeline) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = writer.Shutdown(context.Background())
	})
	// Fill one batch exactly, hold that database operation, then fill its queue.
	for i := 0; i < 32; i++ {
		require.True(t, writer.Enqueue(requesttimeline.New(time.Now(), fmt.Sprint("batch-", i), 7, "model").Freeze(nil)))
	}
	<-started
	for i := 0; i < 32; i++ {
		require.True(t, writer.Enqueue(requesttimeline.New(time.Now(), fmt.Sprint("queued-", i), 7, "model").Freeze(nil)))
	}
	assert.False(t, writer.Enqueue(requesttimeline.New(time.Now(), "overflow", 7, "model").Freeze(nil)))
	assert.Equal(t, int64(1), writer.Stats().Dropped)
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.Shutdown(ctx))
	assert.Equal(t, int64(64), writer.Stats().Written)
}

// Run explicitly with a disposable MySQL database. This measures a real local
// HTTP -> streaming adapter -> HTTP path, including asynchronous MySQL writes.
func BenchmarkRequestTimelineStream(b *testing.B) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	b.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	dsn := os.Getenv("TIMELINE_TEST_MYSQL_DSN")
	if dsn == "" {
		b.Skip("set TIMELINE_TEST_MYSQL_DSN to a disposable database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(b, err)
	oldDB, oldType := model.LOG_DB, common.LogDatabaseType()
	model.LOG_DB = db
	common.SetLogDatabaseType(common.DatabaseTypeMySQL)
	require.NoError(b, model.MigrateRequestTimelines())
	oldOut := gin.DefaultWriter
	gin.DefaultWriter = io.Discard
	b.Cleanup(func() {
		model.LOG_DB = oldDB
		common.SetLogDatabaseType(oldType)
		gin.DefaultWriter = oldOut
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"bench\",\"model\":\"model\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		for i := 0; i < 100; i++ {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fragment\"}}]}\n\n")
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":100,\"total_tokens\":102}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("enabled=%v", enabled), func(b *testing.B) {
			var writer *service.RequestTimelineWriter
			if enabled {
				writer = service.NewRequestTimelineWriter(256, model.InsertRequestTimelines)
			}
			var count atomic.Int64
			prefix := fmt.Sprint("bench-", time.Now().UnixNano(), "-")
			handler := gin.New()
			handler.POST("/v1/chat/completions", func(c *gin.Context) {
				info := relaycommon.GenRelayInfoOpenAI(c, nil)
				info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
				info.IsStream, info.DisablePing, info.ShouldIncludeUsage = true, true, true
				if enabled {
					info.Timeline = requesttimeline.New(info.StartTime, prefix+fmt.Sprint(count.Add(1)), 7, "model")
					restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
					defer restore()
					defer func() { writer.Enqueue(info.Timeline.Freeze(nil)) }()
				}
				req, _ := http.NewRequestWithContext(c.Request.Context(), "POST", upstream.URL, strings.NewReader(`{"stream":true}`))
				resp, err := upstream.Client().Do(info.Timeline.Request(req, 1, ""))
				info.Timeline.Response(resp, err)
				if err != nil {
					c.AbortWithStatus(502)
					return
				}
				_, _ = openai.OaiStreamHandler(c, info, resp)
			})
			server := httptest.NewServer(handler)
			defer server.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resp, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
				require.NoError(b, err)
				_, err = io.Copy(io.Discard, resp.Body)
				require.NoError(b, err)
				require.NoError(b, resp.Body.Close())
				require.Equal(b, 200, resp.StatusCode)
			}
			b.StopTimer()
			if writer != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				require.NoError(b, writer.Shutdown(ctx))
				stats := writer.Stats()
				b.ReportMetric(float64(stats.Dropped+stats.Failed), "lost-records")
				require.Equal(b, int64(b.N), stats.Written)
			}
		})
	}
}

type timelineBlockedLog struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *timelineBlockedLog) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestRequestTimelineDropNeverWaitsForLogOutput(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	writer := service.NewRequestTimelineWriter(32, func(ctx context.Context, rows []*model.RequestTimeline) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	sink := &timelineBlockedLog{entered: make(chan struct{}), release: make(chan struct{})}
	common.LogWriterMu.Lock()
	original := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = sink
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		close(sink.release)
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, writer.Shutdown(ctx))
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = original
		common.LogWriterMu.Unlock()
	})
	for i := 0; i < 32; i++ {
		require.True(t, writer.Enqueue(requesttimeline.New(time.Now(), fmt.Sprint("batch-", i), 7, "model").Freeze(nil)))
	}
	<-started
	for i := 0; i < 32; i++ {
		require.True(t, writer.Enqueue(requesttimeline.New(time.Now(), fmt.Sprint("queue-", i), 7, "model").Freeze(nil)))
	}
	returned := make(chan bool, 1)
	go func() {
		returned <- writer.Enqueue(requesttimeline.New(time.Now(), "overflow", 7, "model").Freeze(nil))
	}()
	select {
	case accepted := <-returned:
		assert.False(t, accepted)
	case <-sink.entered:
		t.Error("queue overflow blocked the request while writing its diagnostic warning")
	case <-time.After(5 * time.Second):
		t.Error("enqueue did not complete")
	}
	assert.Equal(t, int64(1), writer.Stats().Dropped)
}

func TestRequestTimelineParseErrorRetainsOriginalTimeAndCause(t *testing.T) {
	old := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = old })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	info := relaycommon.GenRelayInfoOpenAI(c, nil)
	info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
	info.IsStream, info.DisablePing = true, true
	info.Timeline = requesttimeline.New(info.StartTime, "parse-error", 7, "model")
	restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
	defer restore()
	// A later business failure must not replace the earlier parse failure.
	body := "data: invalid-json\n\ndata: {\"error\":{\"code\":\"late_error\",\"message\":\"later error\"}}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n"
	_, relayErr := openai.OaiStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
	require.Nil(t, relayErr)
	require.NotEmpty(t, info.StreamStatus.Errors)
	first := info.StreamStatus.Errors[0]
	record := info.Timeline.Freeze(nil)
	require.NotNil(t, record.Data.FirstError)
	assert.Equal(t, "failed", record.Result)
	assert.Equal(t, "stream_processing_error", record.Data.FirstError.Code)
	assert.Equal(t, first.Message, record.Data.FirstError.Message)
	assert.Equal(t, first.Timestamp.UnixMilli()-info.StartTime.UnixMilli(), record.Data.FirstError.OffsetMS)
	assert.True(t, record.Data.EndMarkerReceived)
	assert.Equal(t, "done", record.Data.EndReason)
}

func TestRequestTimelineOversizedErrorsAreOmittedWithoutChangingOutput(t *testing.T) {
	old := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = old })
	large := strings.Repeat("upstream failure detail ", 45000)
	for _, tc := range []struct {
		name    string
		payload any
		handler func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"chat", map[string]any{"error": map[string]any{"code": "server_error", "message": large}}, openai.OaiStreamHandler},
		{"responses", map[string]any{"type": "error", "code": "server_error", "message": large}, openai.OaiResponsesStreamHandler},
		{"responses_unknown_error_object", map[string]any{"type": "response.failed", "response": map[string]any{"error": []string{large}}}, openai.OaiResponsesStreamHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := common.Marshal(tc.payload)
			require.NoError(t, err)
			body := "data: " + string(raw) + "\n\ndata: [DONE]\n\n"
			var outputs []string
			var usages []*dto.Usage
			for _, enabled := range []bool{false, true} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
				info := relaycommon.GenRelayInfoOpenAI(c, nil)
				info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
				info.IsStream, info.DisablePing = true, true
				if enabled {
					info.Timeline = requesttimeline.New(info.StartTime, tc.name, 7, "model")
					restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
					defer restore()
				}
				usage, relayErr := tc.handler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
				require.Nil(t, relayErr)
				outputs = append(outputs, w.Body.String())
				usages = append(usages, usage)
				if enabled {
					record := info.Timeline.Freeze(nil)
					require.NotNil(t, record.Data.FirstError)
					assert.Equal(t, "failed", record.Result)
					assert.True(t, record.Data.FirstError.MessageTruncated)
					assert.Contains(t, record.Data.FirstError.Message, "omitted")
					assert.NotContains(t, record.Data.FirstError.Message, "upstream failure detail")
				}
			}
			assert.Equal(t, outputs[0], outputs[1])
			assert.Equal(t, usages[0], usages[1])
		})
	}
}

func TestRequestTimelineBillingUsageUnchanged(t *testing.T) {
	old := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = old })
	cases := []struct {
		name, body string
		handler    func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"chat_reasoning_cache_tools", "data: {\"id\":\"fixed\",\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"reason\",\"content\":\"ok\",\"tool_calls\":[{\"index\":0,\"id\":\"call\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"id\":\"fixed\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120,\"prompt_tokens_details\":{\"cached_tokens\":40},\"completion_tokens_details\":{\"reasoning_tokens\":10}}}\n\ndata: [DONE]\n\n", openai.OaiStreamHandler},
		{"responses_usage", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixed\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"total_tokens\":120,\"input_tokens_details\":{\"cached_tokens\":40},\"output_tokens_details\":{\"reasoning_tokens\":10}}}}\n\ndata: [DONE]\n\n", openai.OaiResponsesStreamHandler},
		{"responses_to_chat", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixed\",\"model\":\"m\",\"status\":\"completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"total_tokens\":120}}}\n\ndata: [DONE]\n\n", openai.OaiResponsesToChatStreamHandler},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, includeUsage := range []bool{false, true} {
				var usages []*dto.Usage
				for _, enabled := range []bool{false, true} {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
					c.Set(common.RequestIdKey, "usage-parity")
					info := relaycommon.GenRelayInfoOpenAI(c, nil)
					info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
					info.IsStream, info.DisablePing, info.ShouldIncludeUsage = true, true, includeUsage
					if enabled {
						info.Timeline = requesttimeline.New(info.StartTime, "usage-parity", 7, "model")
						restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
						defer restore()
					}
					usage, err := tc.handler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))})
					require.Nil(t, err)
					require.NotNil(t, usage)
					usages = append(usages, usage)
				}
				assert.Equal(t, usages[0], usages[1], "billing usage must remain identical with the observer enabled")
			}
		})
	}
}

func BenchmarkRequestTimelineErrorFrame(b *testing.B) {
	previous := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	common.LogWriterMu.Lock()
	out, errOut := gin.DefaultWriter, gin.DefaultErrorWriter
	gin.DefaultWriter, gin.DefaultErrorWriter = io.Discard, io.Discard
	common.LogWriterMu.Unlock()
	b.Cleanup(func() {
		constant.StreamingTimeout = previous
		common.LogWriterMu.Lock()
		gin.DefaultWriter, gin.DefaultErrorWriter = out, errOut
		common.LogWriterMu.Unlock()
	})
	raw, err := common.Marshal(map[string]any{"type": "error", "code": "server_error", "message": strings.Repeat("upstream failure detail ", 1<<20/24+1)})
	require.NoError(b, err)
	body := "data: " + string(raw) + "\n\ndata: [DONE]\n\n"
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprint("enabled=", enabled), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				info := relaycommon.GenRelayInfoOpenAI(c, nil)
				info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
				info.IsStream, info.DisablePing = true, true
				if enabled {
					info.Timeline = requesttimeline.New(info.StartTime, "error-frame", 7, "model")
					_ = helper.ObserveRequestTimelineWriter(c, info.Timeline)
				}
				_, relayErr := openai.OaiResponsesStreamHandler(c, info, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))})
				require.Nil(b, relayErr)
				if enabled {
					_ = info.Timeline.Freeze(nil)
				}
			}
		})
	}
}

func TestRequestTimelineErrorCaptureKeepsBoundedSanitizedFields(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		omitted       bool
	}{
		{"normal", "upstream rejected opaque-test-secret", false},
		{"truncated", strings.Repeat("详细诊断 ", 400) + " opaque-test-secret", false},
		{"omitted", strings.Repeat("x", 9000) + " opaque-test-secret", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := requesttimeline.New(time.Now(), tc.name, 7, "model")
			_ = trace.Request(httptest.NewRequest("POST", "http://upstream.invalid", nil), 1, "opaque-test-secret")
			trace.BusinessError("upstream_error", tc.message)
			record := trace.Freeze(nil)
			require.NotNil(t, record.Data.FirstError)
			failure := record.Data.FirstError
			assert.NotContains(t, failure.Message, "opaque-test-secret")
			assert.LessOrEqual(t, len(failure.Message), 2048)
			assert.True(t, utf8.ValidString(failure.Message))
			if tc.omitted {
				assert.Contains(t, failure.Message, "omitted")
				assert.Contains(t, failure.Message, fmt.Sprint(len(tc.message)))
			}
			assert.Equal(t, tc.name != "normal", failure.MessageTruncated)
		})
	}
	// Null/absent errors remain non-errors; scalar errors keep their useful code.
	for _, tc := range []struct {
		raw     string
		present bool
		code    string
	}{
		{`{"error":null}`, false, ""},
		{`{}`, false, ""},
		{`{"error":{"code":429,"message":"busy"}}`, true, "429"},
		{`{"error":{"type":"busy","message":"busy"}}`, true, "busy"},
	} {
		var event struct {
			Error requesttimeline.StreamError `json:"error"`
		}
		require.NoError(t, common.UnmarshalJsonStr(tc.raw, &event))
		assert.Equal(t, tc.present, event.Error.Present)
		assert.Equal(t, tc.code, event.Error.Code)
	}
}

// This gate only coordinates when a real net/http socket is flushed. It never
// synthesizes a write failure or cancellation; the HTTP client closes the request.
type timelineCancelFlushGate struct {
	http.ResponseWriter
	ctx     context.Context
	entered chan struct{}
}

func (w *timelineCancelFlushGate) Flush() {
	close(w.entered)
	<-w.ctx.Done()
	w.ResponseWriter.(http.Flusher).Flush()
}
func (w *timelineCancelFlushGate) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestRequestTimelineRealHTTPClientCancellationDuringFlush(t *testing.T) {
	for _, businessFailure := range []bool{false, true} {
		t.Run(fmt.Sprint("prior_business_failure=", businessFailure), func(t *testing.T) {
			enteringFlush := make(chan struct{})
			records := make(chan *requesttimeline.Record, 1)
			requestContexts := make(chan error, 1)
			router := gin.New()
			router.POST("/stream", func(c *gin.Context) {
				trace := requesttimeline.New(time.Now(), "real-client-cancel", 7, "model")
				restore := helper.ObserveRequestTimelineWriter(c, trace)
				defer restore()
				if businessFailure {
					trace.BusinessError("overloaded", "upstream business failure")
				}
				_ = helper.StringData(c, `{"choices":[{"delta":{"content":"hello"}}]}`)
				requestContexts <- c.Request.Context().Err()
				trace.StreamEnd("client_gone", c.Request.Context().Err(), false)
				records <- trace.Freeze(nil)
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				router.ServeHTTP(&timelineCancelFlushGate{ResponseWriter: w, ctx: r.Context(), entered: enteringFlush}, r)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/stream", nil)
			require.NoError(t, err)
			clientErrors := make(chan error, 1)
			go func() {
				resp, err := server.Client().Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				clientErrors <- err
			}()
			select {
			case <-enteringFlush:
			case <-time.After(5 * time.Second):
				t.Fatal("real HTTP handler did not start flushing")
			}
			cancel()
			select {
			case err := <-clientErrors:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP client did not cancel")
			}
			var record *requesttimeline.Record
			select {
			case record = <-records:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP handler did not observe cancellation")
			}
			require.ErrorIs(t, <-requestContexts, context.Canceled)
			require.NotNil(t, record)
			require.NotNil(t, record.ErrorKind)
			if businessFailure {
				assert.Equal(t, "failed", record.Result)
				assert.Equal(t, "upstream_business_error", *record.ErrorKind)
				assert.Equal(t, "overloaded", record.Data.FirstError.Code)
			} else {
				assert.Equal(t, "interrupted", record.Result)
				assert.Equal(t, "client_disconnected", *record.ErrorKind)
				assert.Nil(t, record.Data.FirstWriteMS)
			}
		})
	}
}

func TestRequestTimelineDistinguishesCancellationDeadlineAndWriteErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		kind, code string
	}{
		{"canceled", context.Canceled, "client_disconnected", "client_disconnected"},
		{"wrapped_cancel", fmt.Errorf("request ended: %w", context.Canceled), "client_disconnected", "client_disconnected"},
		{"request_deadline", fmt.Errorf("deadline: %w", context.DeadlineExceeded), "timeout", "deadline_exceeded"},
		{"write_timeout", os.ErrDeadlineExceeded, "timeout", "write_timeout"},
		{"broken_write", io.ErrClosedPipe, "downstream_write_error", "write_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := requesttimeline.New(time.Now(), tc.name, 7, "model")
			trace.PayloadWritten(tc.err)
			trace.StreamEnd("client_gone", context.Canceled, false)
			record := trace.Freeze(nil)
			require.NotNil(t, record.ErrorKind)
			assert.Equal(t, "interrupted", record.Result)
			assert.Equal(t, tc.kind, *record.ErrorKind)
			assert.Equal(t, tc.code, record.Data.FirstError.Code)
		})
	}
	trace := requesttimeline.New(time.Now(), "deadline-before-write", 7, "model")
	trace.StreamEnd("client_gone", context.DeadlineExceeded, false)
	record := trace.Freeze(nil)
	require.NotNil(t, record.ErrorKind)
	assert.Equal(t, "timeout", *record.ErrorKind)
	assert.Equal(t, "deadline_exceeded", record.Data.FirstError.Code)
}
