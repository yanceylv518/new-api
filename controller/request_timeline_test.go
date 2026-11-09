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
	"regexp"
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
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/advancedcustom"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	"github.com/QuantumNous/new-api/relay/channel/zhipu_4v"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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
	hook.WroteRequest(httptrace.WroteRequestInfo{Err: io.ErrUnexpectedEOF})
	hook.WroteRequest(httptrace.WroteRequestInfo{})
	assert.True(t, retried.Candidate())
	repeated := retried.Freeze(nil)
	require.NotNil(t, repeated)
	require.Len(t, repeated.Data.Attempts, 2)
	assert.Equal(t, "failed", repeated.Data.Attempts[0].Result)
	assert.Equal(t, "unknown", repeated.Data.Attempts[1].Result)
	assert.Equal(t, 2, repeated.Data.AttemptCount)

	// A late callback from a failed attempt must not create an extra retry or
	// overwrite the new attempt's timing. Retry indices can reset across groups.
	guarded := requesttimeline.New(time.Now(), "late-callback", 7, "model")
	guarded.BeginAttempt(0)
	oldRequest := guarded.Request(httptest.NewRequest("POST", "http://first.invalid", nil), 1, "")
	oldHook := httptrace.ContextClientTrace(oldRequest.Context())
	oldHook.WroteRequest(httptrace.WroteRequestInfo{})
	guarded.EndAttempt(errors.New("first failed"))
	guarded.BeginAttempt(0)
	oldHook.WroteRequest(httptrace.WroteRequestInfo{})
	guarded.SelectChannel(2, "")
	guarded.EndAttempt(errors.New("conversion failed before sending"))
	guarded.BeginAttempt(0)
	last := guarded.Freeze(errors.New("no available channel"))
	require.Len(t, last.Data.Attempts, 3)
	assert.Equal(t, 1, last.Data.Attempts[0].Data.ChannelID)
	assert.Equal(t, 2, last.Data.Attempts[1].Data.ChannelID)
	assert.Nil(t, last.Data.Attempts[1].Data.RequestSentMS)
	assert.Equal(t, 0, last.Data.Attempts[2].Data.ChannelID)
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
	invalid := requesttimeline.New(time.Now(), "invalid-diagnostic", 7, "model").Freeze(nil)
	invalid.Data.UpstreamUsage = []byte(`{"broken":`)
	assert.True(t, w.Enqueue(invalid))
	t1 := requesttimeline.New(time.Now(), "queue-1", 7, "model")
	assert.True(t, w.Enqueue(t1.Freeze(nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.Shutdown(ctx))
	require.Len(t, written, 1)
	assert.Equal(t, int64(1), w.Stats().Dropped, "one malformed diagnostic must not prevent the next record from being written")
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

type timelinePanickingError struct{}

func (timelinePanickingError) Error() string { panic("diagnostic error formatting failed") }

func TestRequestTimelineObservationPanicDisablesOnlyThatTrace(t *testing.T) {
	for _, phase := range []string{"transport", "write", "finalize"} {
		t.Run(phase, func(t *testing.T) {
			trace := requesttimeline.New(time.Now(), "observation-panic", 7, "model")
			require.NotPanics(t, func() {
				switch phase {
				case "transport":
					trace.Response(nil, timelinePanickingError{})
				case "write":
					trace.PayloadWritten(timelinePanickingError{})
				case "finalize":
					assert.Nil(t, trace.Freeze(timelinePanickingError{}))
				}
			})
			assert.False(t, trace.Candidate())
			assert.Nil(t, trace.Freeze(nil))
			// A disabled observer still passes through the original HTTP request.
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("original body"))
			assert.Same(t, req, trace.Request(req, 1, "secret"))
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			assert.Equal(t, "original body", string(body))
		})
	}
}

func TestRequestTimelineWriterPanicReleasesDatabaseAndStopsDiagnostics(t *testing.T) {
	user, _ := setupAccessTokenAudit(t)
	preserveTimelineOption(t)
	model.RequestTimelineCaptureEnabled.Store(true)
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	writer := service.InitRequestTimeline()
	require.NotNil(t, writer)
	t.Cleanup(func() { _ = writer.Shutdown(context.Background()) })
	entered := make(chan struct{})
	require.NoError(t, model.LOG_DB.Callback().Create().Before("gorm:create").Register("timeline:panic-insert", func(db *gorm.DB) {
		if db.Statement.Table == "request_timelines" {
			close(entered)
			panic("injected timeline insert panic")
		}
	}))
	t.Cleanup(func() { require.NoError(t, model.LOG_DB.Callback().Create().Remove("timeline:panic-insert")) })
	require.True(t, writer.Enqueue(requesttimeline.New(time.Now(), "writer-panic", user.Id, "model").Freeze(nil)))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timeline insert did not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.Shutdown(ctx))
	assert.False(t, service.RequestTimelineEnabled())
	assert.False(t, writer.Enqueue(requesttimeline.New(time.Now(), "after-panic", user.Id, "model").Freeze(nil)))
	assert.Equal(t, int64(1), writer.Stats().Failed)
	// The diagnostic transaction must not hold the shared DB's only connection.
	var stored model.User
	require.NoError(t, model.DB.WithContext(ctx).First(&stored, user.Id).Error)
	assert.Equal(t, user.Username, stored.Username)
}

type timelinePanickingLog struct{}

func (timelinePanickingLog) Write([]byte) (int, error) { panic("diagnostic log sink failed") }

func TestRequestTimelineLogFailureAndShutdownRemainIsolated(t *testing.T) {
	common.LogWriterMu.Lock()
	original := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = timelinePanickingLog{}
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = original
		common.LogWriterMu.Unlock()
	})
	failed := service.NewRequestTimelineWriter(1, func(context.Context, []*model.RequestTimeline) error {
		return errors.New("database unavailable")
	})
	require.True(t, failed.Enqueue(requesttimeline.New(time.Now(), "log-sink-failure", 7, "model").Freeze(nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, failed.Shutdown(ctx))
	assert.Equal(t, int64(1), failed.Stats().Failed)
	// A recovered logger panic must not strand the lock used for log rotation.
	if common.LogWriterMu.TryLock() {
		common.LogWriterMu.Unlock()
	} else {
		t.Fatal("diagnostic logging retained the shared log-writer lock")
	}

	entered := make(chan struct{})
	var once sync.Once
	blocked := service.NewRequestTimelineWriter(1, func(ctx context.Context, _ []*model.RequestTimeline) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	})
	require.True(t, blocked.Enqueue(requesttimeline.New(time.Now(), "cancel-writer", 7, "model").Freeze(nil)))
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("timeline insert did not start")
	}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	shutdownCancel()
	assert.ErrorIs(t, blocked.Shutdown(shutdownCtx), context.Canceled)
	require.NoError(t, blocked.Shutdown(ctx), "canceling shutdown must also cancel the in-flight insert")
	assert.Zero(t, blocked.Stats().Pending)
	assert.False(t, blocked.Enqueue(requesttimeline.New(time.Now(), "after-shutdown", 7, "model").Freeze(nil)))
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
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeZhipu_v4, constant.ChannelTypeAdvancedCustom, constant.ChannelTypeAnthropic} {
		common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
		for _, protocol := range []struct {
			format types.RelayFormat
			mode   int
			want   bool
		}{
			{types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, true},
			{types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses, true},
			{types.RelayFormatClaude, relayconstant.RelayModeUnknown, true},
			{types.RelayFormatOpenAI, relayconstant.RelayModeCompletions, false},
			{types.RelayFormatOpenAIImage, relayconstant.RelayModeImagesGenerations, false},
			{types.RelayFormatOpenAIRealtime, relayconstant.RelayModeRealtime, false},
		} {
			info.RelayFormat, info.RelayMode = protocol.format, protocol.mode
			assert.Equal(t, protocol.want, service.BeginRequestTimeline(c, info) != nil, "channel=%d protocol=%s mode=%d", channelType, protocol.format, protocol.mode)
		}
	}

	headers := map[string]string{"Msh-Usage-Cache-Write-Tokens-5m": "5120", "Msh-Usage-Cache-Write-Tokens-1h": "0"}
	payload, err := common.Marshal(requesttimeline.Data{Version: 1, StartedAtMS: time.Now().UnixMilli(), TimingValid: true, UpstreamResponseHeaders: headers})
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
	var lookup struct {
		Data struct {
			Timeline requesttimeline.Data `json:"timeline"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &lookup))
	assert.Equal(t, headers, lookup.Data.Timeline.UpstreamResponseHeaders)
	assert.Equal(t, 401, auditRequest(r, "GET", "/api/request-timelines/lookup", "wrong-token").Code)
	ordinaryToken := "ordinary-timeline-token"
	ordinary := &model.User{Username: "timeline-user", Password: "placeholder", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AccessToken: &ordinaryToken, AuthVersion: 1, AffCode: "timeline-user"}
	require.NoError(t, model.DB.Create(ordinary).Error)
	assert.Equal(t, 403, auditRequest(r, "GET", "/api/request-timelines/lookup", ordinaryToken).Code)
	assert.Equal(t, 400, auditRequest(r, "GET", "/api/request-timelines/bad!", token).Code)
	assert.Contains(t, auditRequest(r, "GET", "/api/request-timelines/absent", token).Body.String(), `"status":"missing"`)
	require.NoError(t, model.LOG_DB.Callback().Query().Before("gorm:query").Register("timeline:query-failure", func(db *gorm.DB) {
		if db.Statement.Table == "request_timelines" {
			db.AddError(errors.New("injected timeline query failure"))
		}
	}))
	assert.Equal(t, http.StatusInternalServerError, auditRequest(r, "GET", "/api/request-timelines/lookup", token).Code)
	require.NoError(t, model.LOG_DB.Callback().Query().Remove("timeline:query-failure"))
	assert.Equal(t, http.StatusOK, auditRequest(r, "GET", "/api/request-timelines/lookup", token).Code)
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
	payload, err := common.Marshal(requesttimeline.Data{Version: 1, StartedAtMS: 1000000, RequestModel: "中文模型", TimingValid: true, UpstreamUsage: []byte(`{"prompt_tokens":0,"total_tokens":9,"prompt_tokens_details":{"cached_creation_tokens":2},"vendor":{"note":"完整文本","enabled":true,"values":[null,1]}}`), UpstreamUsageCapture: "captured",
		UpstreamResponseHeaders: map[string]string{"Msh-Usage-Cache-Write-Tokens-5m": "5120", "Msh-Usage-Cache-Write-Tokens-1h": "0"},
		AttemptCount:            2, Attempts: []requesttimeline.Attempt{
			{Number: 1, RetryIndex: 0, Result: "failed", Data: requesttimeline.Data{Version: 1, StartedAtMS: 1000000, ChannelID: 1, FirstError: &requesttimeline.Failure{Kind: "upstream_http_error", Code: "502", Message: "上游失败"}}},
			{Number: 2, RetryIndex: 1, StartedMS: 100, Result: "success", Data: requesttimeline.Data{Version: 1, StartedAtMS: 1000000, ChannelID: 2, ReturnedUsage: &requesttimeline.Usage{InputTokens: 3, OutputTokens: 6, TotalTokens: 9}}},
		}})
	require.NoError(t, err)
	truncatedPayload, err := common.Marshal(requesttimeline.Data{Version: 1, StartedAtMS: 1000000, UpstreamUsageCapture: "truncated", UpstreamUsagePreview: `{"vendor":"未完成`, UpstreamUsageBytes: 9000})
	require.NoError(t, err)
	size, status := int64(0), 200
	entries := []*model.RequestTimeline{
		{RequestID: "Timeline-Case", UserID: 7, CreatedAt: 1000, Result: "success", UpstreamStatus: &status, UpstreamRequestBodyBytes: &size, Data: string(payload)},
		{RequestID: "timeline-case", UserID: 8, CreatedAt: 1001, Result: "unknown", Data: string(truncatedPayload)},
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
	var truncatedData requesttimeline.Data
	require.NoError(t, common.UnmarshalJsonStr(lower.Data, &truncatedData))
	assert.Equal(t, "truncated", truncatedData.UpstreamUsageCapture)
	assert.Equal(t, `{"vendor":"未完成`, truncatedData.UpstreamUsagePreview)
	assert.Equal(t, 9000, truncatedData.UpstreamUsageBytes)
	var decoded requesttimeline.Data
	require.NoError(t, common.UnmarshalJsonStr(found.Data, &decoded))
	assert.Equal(t, "中文模型", decoded.RequestModel)
	assert.JSONEq(t, `{"prompt_tokens":0,"total_tokens":9,"prompt_tokens_details":{"cached_creation_tokens":2},"vendor":{"note":"完整文本","enabled":true,"values":[null,1]}}`, string(decoded.UpstreamUsage))
	assert.Equal(t, "captured", decoded.UpstreamUsageCapture)
	assert.Equal(t, map[string]string{"Msh-Usage-Cache-Write-Tokens-5m": "5120", "Msh-Usage-Cache-Write-Tokens-1h": "0"}, decoded.UpstreamResponseHeaders)
	assert.Nil(t, truncatedData.UpstreamResponseHeaders, "historical records must not gain invented header values")
	require.Len(t, decoded.Attempts, 2)
	assert.Equal(t, "上游失败", decoded.Attempts[0].Data.FirstError.Message)
	assert.Equal(t, "success", decoded.Attempts[1].Result)
	assert.Equal(t, 9, decoded.Attempts[1].Data.ReturnedUsage.TotalTokens)
	var count int64
	require.NoError(t, db.Model(&model.RequestTimeline{}).Count(&count).Error)
	assert.Equal(t, int64(2), count)
	var retained model.Log
	require.NoError(t, db.First(&retained, legacy.Id).Error)
	assert.Equal(t, legacy.Quota, retained.Quota)
	assert.Equal(t, legacy.Other, retained.Other)

	// Exercise panic rollback against each real supported database, including
	// a write that reached the server before its callback failed.
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.Callback().Create().After("gorm:create").Before("gorm:save_after_associations").Register("timeline:rollback", func(tx *gorm.DB) {
		if tx.Statement.Table == "request_timelines" {
			panic("injected failure after diagnostic insert")
		}
	}))
	rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rollbackCancel()
	rollbackEntry := &model.RequestTimeline{RequestID: "rolled-back", UserID: 7, CreatedAt: 1000, Result: "unknown", Data: string(payload)}
	require.Panics(t, func() {
		_ = model.InsertRequestTimelines(rollbackCtx, []*model.RequestTimeline{rollbackEntry})
	})
	require.NoError(t, db.Callback().Create().Remove("timeline:rollback"))
	_, err = model.FindRequestTimeline(rollbackCtx, rollbackEntry.RequestID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound, "partial diagnostic inserts must roll back and release the connection")
	require.NoError(t, db.WithContext(rollbackCtx).First(&retained, legacy.Id).Error)
	assert.Equal(t, legacy.Quota, retained.Quota)

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
	previousMaster := common.IsMasterNode
	common.IsMasterNode = true
	t.Cleanup(func() { common.IsMasterNode = previousMaster })
	startupWriter := service.InitRequestTimeline()
	if startupWriter != nil {
		_ = startupWriter.Shutdown(context.Background())
	}
	assert.Nil(t, startupWriter, "an incompatible timeline table must disable diagnostics without failing startup")
	assert.False(t, service.RequestTimelineStorageReady())
	initializationPanicked := false
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register("timeline:initialization-panic", func(*gorm.DB) {
		initializationPanicked = true
		panic("injected timeline metadata query panic")
	}))
	require.NotPanics(t, func() { startupWriter = service.InitRequestTimeline() })
	require.NoError(t, db.Callback().Row().Remove("timeline:initialization-panic"))
	assert.True(t, initializationPanicked, "the startup failure injection must actually run")
	assert.Nil(t, startupWriter)
	assert.False(t, service.RequestTimelineStorageReady())
	require.NoError(t, db.First(&retained, legacy.Id).Error)
	assert.Equal(t, legacy.Quota, retained.Quota)
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

func TestRequestTimelineRetainsUpstreamUsageWithoutChangingBilling(t *testing.T) {
	old := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = old })
	for _, tc := range []struct {
		name, body, field string
		handler           func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"total_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":4,\"cached_creation_tokens\":2,\"provider_note\":\"provider-extension\"},\"unexpected\":\"provider-extension\"}}\n\ndata: [DONE]\n\n", "prompt_tokens", openai.OaiStreamHandler},
		{"responses", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"created_at\":1700000000,\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":6,\"total_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":2},\"unexpected\":\"provider-extension\"}}}\n\ndata: [DONE]\n\n", "input_tokens", openai.OaiResponsesStreamHandler},
		{"converted", "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"created_at\":1700000000,\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":6,\"total_tokens\":9,\"unexpected\":\"provider-extension\"}}}\n\ndata: [DONE]\n\n", "input_tokens", openai.OaiResponsesToChatStreamHandler},
	} {
		for _, oversized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oversized=%v", tc.name, oversized), func(t *testing.T) {
				body := tc.body
				if oversized {
					body = strings.ReplaceAll(body, "provider-extension", strings.Repeat("x", 9000))
				}
				var outputs []string
				var usages []*dto.Usage
				for _, enabled := range []bool{false, true} {
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
					c.Set(common.RequestIdKey, "raw-usage-test")
					info := relaycommon.GenRelayInfoOpenAI(c, nil)
					info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
					// The converted case also proves usage is captured when the client did not request a usage frame.
					info.IsStream, info.DisablePing, info.ShouldIncludeUsage = true, true, tc.name != "converted"
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
						if oversized {
							assert.Equal(t, "truncated", record.Data.UpstreamUsageCapture)
							assert.NotEmpty(t, record.Data.UpstreamUsagePreview)
							assert.Empty(t, record.Data.UpstreamUsage)
							continue
						}
						encoded, err := common.Marshal(record.Data)
						require.NoError(t, err)
						var data map[string]any
						require.NoError(t, common.Unmarshal(encoded, &data))
						raw, ok := data["upstream_usage"].(map[string]any)
						require.True(t, ok)
						assert.Contains(t, raw, tc.field)
						assert.NotContains(t, raw, "completion_tokens")
						assert.Contains(t, string(encoded), "provider-extension")
						assert.Equal(t, "provider-extension", raw["unexpected"])
						if tc.name == "chat" {
							assert.Equal(t, float64(0), raw["prompt_tokens"])
							assert.Equal(t, float64(4), raw["prompt_tokens_details"].(map[string]any)["cached_tokens"])
							assert.Equal(t, float64(2), raw["prompt_tokens_details"].(map[string]any)["cached_creation_tokens"])
						}
					}
				}
				assert.Equal(t, outputs[0], outputs[1])
				assert.Equal(t, usages[0], usages[1])
			})
		}
	}
}

func TestRequestTimelineUsageCaptureBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, frame, want, status string }{
		{"complete_object", `{"usage":{"prompt_tokens":0,"vendor":{"enabled":true,"note":"text","list":[1,"two",null]},"cost":0.125}}`, `{"prompt_tokens":0,"vendor":{"enabled":true,"note":"text","list":[1,"two",null]},"cost":0.125}`, "captured"},
		{"duplicate_usage", `{"usage":{"prompt_tokens":1},"usage":{"completion_tokens":2,"total_tokens":3}}`, `{"completion_tokens":2,"total_tokens":3}`, "captured"},
		{"large_unrelated_content", `{"padding":"` + strings.Repeat("x", 1<<20) + `","usage":{"prompt_tokens":1}}`, `{"prompt_tokens":1}`, "captured"},
		{"large_usage", `{"usage":{"prompt_tokens":2,"extra":"` + strings.Repeat("x", 9000) + `"}}`, "", "truncated"},
		{"escaped_size", `{"usage":{"note":"` + strings.Repeat("<", 2000) + `"}}`, "", "truncated"},
		{"unicode_boundary", `{"usage":{"note":"` + strings.Repeat("中文", 2000) + `"}}`, "", "truncated"},
		{"empty_object", `{"usage":{}}`, `{}`, "captured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := requesttimeline.New(time.Now(), tc.name, 7, "model")
			var event struct {
				Usage requesttimeline.StreamUsage `json:"usage"`
			}
			require.NoError(t, common.UnmarshalJsonStr(`{"usage":{"prompt_tokens":99}}`, &event))
			trace.CaptureUpstreamUsage(event.Usage)
			event.Usage = requesttimeline.StreamUsage{}
			require.NoError(t, common.UnmarshalJsonStr(tc.frame, &event))
			var billing struct {
				Usage *dto.Usage `json:"usage"`
			}
			require.NoError(t, common.UnmarshalJsonStr(tc.frame, &billing))
			assert.Equal(t, billing.Usage, event.Usage.Value, "raw capture must preserve DTO decoding semantics")
			trace.CaptureUpstreamUsage(event.Usage)
			record := trace.Freeze(nil)
			assert.Equal(t, tc.status, record.Data.UpstreamUsageCapture)
			if tc.want == "" {
				assert.Empty(t, record.Data.UpstreamUsage)
				assert.NotEmpty(t, record.Data.UpstreamUsagePreview)
				assert.True(t, utf8.ValidString(record.Data.UpstreamUsagePreview))
				assert.True(t, strings.HasPrefix(tc.frame, `{"usage":`+record.Data.UpstreamUsagePreview))
				assert.Greater(t, record.Data.UpstreamUsageBytes, len(record.Data.UpstreamUsagePreview))
				encoded, err := common.Marshal(record.Data)
				require.NoError(t, err)
				assert.LessOrEqual(t, len(encoded), requesttimeline.MaxDataBytes)
			} else {
				assert.JSONEq(t, tc.want, string(record.Data.UpstreamUsage))
			}
		})
	}
}

func TestRequestTimelineUpstreamResponseHeaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    map[string]string
	}{
		{"reported_zero", map[string]string{"Msh-Usage-Cache-Write-Tokens-5m": "5120", "Msh-Usage-Cache-Write-Tokens-1h": "0", "Set-Cookie": "session=private"}, map[string]string{"Msh-Usage-Cache-Write-Tokens-5m": "5120", "Msh-Usage-Cache-Write-Tokens-1h": "0"}},
		{"only_1h", map[string]string{"Msh-Usage-Cache-Write-Tokens-1h": "17"}, map[string]string{"Msh-Usage-Cache-Write-Tokens-1h": "17"}},
		{"missing", map[string]string{"Set-Cookie": "session=private"}, nil},
		{"empty", map[string]string{"Msh-Usage-Cache-Write-Tokens-5m": "", "Msh-Usage-Cache-Write-Tokens-1h": ""}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}
			for name, value := range tc.headers {
				response.Header.Set(name, value)
			}
			original := response.Header.Clone()
			trace := requesttimeline.New(time.Now(), tc.name, 7, "model")
			trace.Response(response, nil)
			record := trace.Freeze(nil)
			require.NotNil(t, record)
			raw, err := record.MarshalData()
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, common.Unmarshal(raw, &fields))
			if tc.want == nil {
				assert.NotContains(t, fields, "upstream_response_headers")
			} else {
				var decoded struct {
					Headers map[string]string `json:"upstream_response_headers"`
				}
				require.NoError(t, common.Unmarshal(raw, &decoded))
				assert.Equal(t, tc.want, decoded.Headers)
			}
			assert.Equal(t, original, response.Header, "diagnostics must not modify forwarded headers")
			assert.Nil(t, record.Data.FirstResponseMS, "receiving HTTP headers must not count as the first streamed payload")
			assert.Empty(t, record.Data.UpstreamUsage)
			assert.Nil(t, record.Data.ReturnedUsage, "response headers must not become billable usage")
		})
	}
}

func TestRequestTimelineResponseHeaderBoundsAndFallbackIsolation(t *testing.T) {
	value := strings.Repeat("9", 20<<10)
	response := &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header)}
	response.Header.Set("Msh-Usage-Cache-Write-Tokens-5m", value)
	response.Header.Set("Msh-Usage-Cache-Write-Tokens-1h", "0")
	trace := requesttimeline.New(time.Now(), "header-bounds", 7, "model")
	trace.BeginAttempt(0)
	trace.Response(response, nil)
	trace.EndAttempt(errors.New("retry upstream"))
	trace.BeginAttempt(1)
	trace.Response(&http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, nil)
	record := trace.Freeze(nil)
	require.NotNil(t, record)
	require.Len(t, record.Data.Attempts, 2)
	assert.Nil(t, record.Data.UpstreamResponseHeaders)
	assert.Nil(t, record.Data.Attempts[1].Data.UpstreamResponseHeaders, "fallback must not inherit the previous attempt's headers")
	first := record.Data.Attempts[0].Data
	assert.Equal(t, "0", first.UpstreamResponseHeaders["Msh-Usage-Cache-Write-Tokens-1h"])
	require.NotEmpty(t, first.UpstreamResponseHeaders["Msh-Usage-Cache-Write-Tokens-5m"])
	assert.Less(t, len(first.UpstreamResponseHeaders["Msh-Usage-Cache-Write-Tokens-5m"]), len(value))
	assert.True(t, first.DetailsTruncated)
	assert.Equal(t, value, response.Header.Get("Msh-Usage-Cache-Write-Tokens-5m"))
	raw, err := record.MarshalData()
	require.NoError(t, err)
	assert.LessOrEqual(t, len(raw), requesttimeline.MaxDataBytes)
}

func TestRequestTimelineMassRequestIDHeader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"lowercase", map[string]string{"x-mass-requestid": "mass-lower"}, "mass-lower"},
		{"mixed_case", map[string]string{"X-MaSs-ReQuEsTiD": "mass-mixed"}, "mass-mixed"},
		{"oneapi_priority", map[string]string{"X-Oneapi-Request-Id": "oneapi", "X-Request-Id": "standard", "Request-Id": "legacy", "x-mass-requestid": "mass"}, "oneapi"},
		{"standard_priority", map[string]string{"X-Request-Id": "standard", "Request-Id": "legacy", "x-mass-requestid": "mass"}, "standard"},
		{"legacy_priority", map[string]string{"Request-Id": "legacy", "x-mass-requestid": "mass"}, "legacy"},
		{"empty_preferred_header", map[string]string{"X-Oneapi-Request-Id": "", "x-mass-requestid": "mass-fallback"}, "mass-fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Preserve wire casing here; the real HTTP client parses the response.
				for key, value := range tc.headers {
					w.Header()[key] = []string{value}
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			response, err := upstream.Client().Get(upstream.URL)
			require.NoError(t, err)
			defer response.Body.Close()
			trace := requesttimeline.New(time.Now(), tc.name, 7, "model")
			trace.Response(response, nil)
			record := trace.Freeze(nil)
			require.NotNil(t, record)
			assert.Equal(t, tc.want, record.Data.UpstreamRequestID)
		})
	}
}

func TestRequestTimelineClaudeFailuresAndCaptureBounds(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	for _, tc := range []struct {
		name, body, result, kind string
	}{
		{"business_error", "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"upstream rejected sk-test\"}}\n\n", "failed", "upstream_business_error"},
		{"missing_stop", "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n", "unknown", "unconfirmed_end"},
		{"duplicate_usage", "data: {\"type\":\"message_start\",\"message\":{\"id\":\"r1\",\"model\":\"model\",\"usage\":{\"input_tokens\":2},\"usage\":{\"cache_read_input_tokens\":3}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "success", ""},
		{"malformed", "data: {not-json}\n\n", "failed", "local_error"},
		{"oversized_usage", "data: {\"type\":\"message_start\",\"message\":{\"id\":\"r1\",\"model\":\"model\",\"usage\":{\"input_tokens\":2,\"provider\":\"" + strings.Repeat("x", 9000) + "\"}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n", "success", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outputs []string
			var usages []*dto.Usage
			var relayErrors []*types.NewAPIError
			for _, enabled := range []bool{false, true} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				info := relaycommon.GenRelayInfoClaude(c, nil)
				info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
				info.IsStream, info.DisablePing = true, true
				if enabled {
					info.Timeline = requesttimeline.New(info.StartTime, tc.name, 7, "model")
					info.Timeline.Request(httptest.NewRequest("POST", "http://upstream.invalid", nil), 187, "sk-test")
					restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
					defer restore()
				}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
				usage, relayErr := claude.ClaudeStreamHandler(c, resp, info)
				outputs, usages, relayErrors = append(outputs, w.Body.String()), append(usages, usage), append(relayErrors, relayErr)
				if enabled {
					record := info.Timeline.Freeze(nil)
					require.NotNil(t, record)
					assert.Equal(t, tc.result, record.Result)
					if tc.kind != "" {
						require.NotNil(t, record.ErrorKind)
						assert.Equal(t, tc.kind, *record.ErrorKind)
					}
					if tc.name == "business_error" {
						assert.Equal(t, "overloaded_error", record.Data.FirstError.Code)
						assert.NotContains(t, record.Data.FirstError.Message, "sk-test")
					}
					if tc.name == "oversized_usage" {
						assert.Equal(t, "truncated", record.Data.UpstreamUsageCapture)
						assert.Empty(t, record.Data.UpstreamUsage)
						assert.Equal(t, 5, record.Data.ReturnedUsage.TotalTokens)
					}
					encoded, err := common.Marshal(record.Data)
					require.NoError(t, err)
					assert.LessOrEqual(t, len(encoded), requesttimeline.MaxDataBytes)
				}
			}
			assert.Equal(t, outputs[0], outputs[1])
			assert.Equal(t, usages[0], usages[1])
			assert.Equal(t, relayErrors[0], relayErrors[1])
		})
	}
}

func TestRequestTimelineProtocolRoutes(t *testing.T) {
	setupAccessTokenAudit(t)
	preserveTimelineOption(t)
	model.RequestTimelineCaptureEnabled.Store(true)
	writer := service.InitRequestTimeline()
	require.NotNil(t, writer)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, writer.Shutdown(ctx))
	})
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	chat := "data: {\"id\":\"r1\",\"created\":1700000000,\"model\":\"model\",\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\ndata: [DONE]\n\n"
	responses := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"model\":\"model\",\"created_at\":1700000000,\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":3,\"total_tokens\":8}}}\n\n"
	claudeBody := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r1\",\"model\":\"model\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":3,\"output_tokens\":0,\"provider_extension\":\"retained\"}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, path, body, converter string
		channelType                 int
		format                      types.RelayFormat
	}{
		{"custom_chat", "/v1/chat/completions", chat, "", constant.ChannelTypeAdvancedCustom, types.RelayFormatOpenAI},
		{"custom_responses", "/v1/responses", responses, "", constant.ChannelTypeAdvancedCustom, types.RelayFormatOpenAIResponses},
		{"custom_claude", "/v1/messages", claudeBody, "", constant.ChannelTypeAdvancedCustom, types.RelayFormatClaude},
		{"custom_chat_to_claude", "/v1/chat/completions", claudeBody, relayconvert.ConverterOpenAIChatToClaudeMessages, constant.ChannelTypeAdvancedCustom, types.RelayFormatOpenAI},
		{"custom_chat_to_responses", "/v1/chat/completions", responses, relayconvert.ConverterOpenAIChatToOpenAIResponses, constant.ChannelTypeAdvancedCustom, types.RelayFormatOpenAI},
		{"custom_responses_to_chat", "/v1/responses", chat, relayconvert.ConverterOpenAIResponsesToOpenAIChat, constant.ChannelTypeAdvancedCustom, types.RelayFormatOpenAIResponses},
		{"custom_claude_to_chat", "/v1/messages", chat, relayconvert.ConverterClaudeMessagesToOpenAIChat, constant.ChannelTypeAdvancedCustom, types.RelayFormatClaude},
		{"anthropic_responses", "/v1/responses", claudeBody, "", constant.ChannelTypeAnthropic, types.RelayFormatOpenAIResponses},
		{"zhipu_chat", "/v1/chat/completions", chat, "", constant.ChannelTypeZhipu_v4, types.RelayFormatOpenAI},
		{"zhipu_claude", "/v1/messages", claudeBody, "", constant.ChannelTypeZhipu_v4, types.RelayFormatClaude},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Request-Id", "upstream-route")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			var outputs []string
			var usages []any
			for _, enabled := range []bool{false, true} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"model","stream":true}`))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set(common.RequestIdKey, "protocol-route")
				common.SetContextKey(c, constant.ContextKeyChannelType, tc.channelType)
				info := relaycommon.GenRelayInfoOpenAI(c, nil)
				if tc.format == types.RelayFormatClaude {
					info = relaycommon.GenRelayInfoClaude(c, nil)
				} else if tc.format == types.RelayFormatOpenAIResponses {
					info = relaycommon.GenRelayInfoResponses(c, &dto.OpenAIResponsesRequest{})
				}
				info.IsStream, info.DisablePing, info.ShouldIncludeUsage = true, true, true
				info.OriginModelName = "model"
				info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: 187, ChannelType: tc.channelType, ChannelBaseUrl: upstream.URL, UpstreamModelName: "model", ApiKey: "sk-test"}
				var adaptor channel.Adaptor = &zhipu_4v.Adaptor{}
				if tc.channelType == constant.ChannelTypeAdvancedCustom {
					info.ChannelOtherSettings.AdvancedCustom = &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: tc.path, UpstreamPath: tc.path, Converter: tc.converter}}}
					adaptor = &advancedcustom.Adaptor{}
				} else if tc.channelType == constant.ChannelTypeAnthropic {
					adaptor = &claude.Adaptor{}
				}
				adaptor.Init(info)
				if enabled {
					info.Timeline = service.BeginRequestTimeline(c, info)
					require.NotNil(t, info.Timeline)
					restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
					defer restore()
				}
				requestBody := `{"model":"model","stream":true}`
				if tc.converter != "" {
					var converted any
					var err error
					switch tc.format {
					case types.RelayFormatClaude:
						converted, err = adaptor.ConvertClaudeRequest(c, info, &dto.ClaudeRequest{Model: "model", Stream: common.GetPointer(true), Messages: []dto.ClaudeMessage{{Role: "user", Content: "hello"}}})
					case types.RelayFormatOpenAIResponses:
						converted, err = adaptor.ConvertOpenAIResponsesRequest(c, info, dto.OpenAIResponsesRequest{Model: "model", Stream: common.GetPointer(true), Input: []byte(`"hello"`)})
					default:
						converted, err = adaptor.ConvertOpenAIRequest(c, info, &dto.GeneralOpenAIRequest{Model: "model", Stream: common.GetPointer(true), Messages: []dto.Message{{Role: "user", Content: "hello"}}})
					}
					require.NoError(t, err)
					raw, err := common.Marshal(converted)
					require.NoError(t, err)
					requestBody = string(raw)
				}
				response, err := adaptor.DoRequest(c, info, strings.NewReader(requestBody))
				require.NoError(t, err)
				usage, relayErr := adaptor.DoResponse(c, response.(*http.Response), info)
				require.Nil(t, relayErr)
				outputs = append(outputs, w.Body.String())
				usages = append(usages, usage)
				if enabled {
					record := info.Timeline.Freeze(nil)
					require.NotNil(t, record)
					assert.Equal(t, "success", record.Result)
					assert.Equal(t, "upstream-route", record.Data.UpstreamRequestID)
					assert.Equal(t, "r1", record.Data.ResponseID)
					require.NotNil(t, record.Data.RequestSentMS)
					require.NotNil(t, record.Data.FirstResponseMS)
					require.NotNil(t, record.Data.FirstWriteMS)
					assert.True(t, record.Data.EndMarkerReceived)
					require.NotNil(t, record.Data.ReturnedUsage)
					assert.Equal(t, 8, record.Data.ReturnedUsage.TotalTokens)
					if tc.body == claudeBody {
						assert.Equal(t, "end_turn", record.Data.FinishReason)
						assert.JSONEq(t, `{"message_start":{"input_tokens":2,"cache_read_input_tokens":3,"output_tokens":0,"provider_extension":"retained"},"message_delta":{"output_tokens":3}}`, string(record.Data.UpstreamUsage))
					}
				}
			}
			// Converters may generate timestamps at the time of each request.
			timestamps := regexp.MustCompile(`"created(?:_at)?":\d+`)
			assert.Equal(t, timestamps.ReplaceAllString(outputs[0], `"created":0`), timestamps.ReplaceAllString(outputs[1], `"created":0`), "timeline must preserve the returned stream")
			assert.Equal(t, usages[0], usages[1], "timeline must preserve billing usage")
		})
	}
}

func TestRequestTimelineFallbackRelayPreservesBillingAndResponse(t *testing.T) {
	for _, allFail := range []bool{false, true} {
		t.Run(fmt.Sprintf("all_fail=%v", allFail), func(t *testing.T) {
			previousCache := common.MemoryCacheEnabled
			t.Cleanup(func() {
				common.MemoryCacheEnabled = previousCache
				if previousCache && model.DB != nil {
					model.InitChannelCache()
				}
			})
			user, _ := setupAccessTokenAudit(t)
			sqlDB, err := model.DB.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			preserveTimelineOption(t)
			allowPrivateTaskMediaTest(t)
			previousRatios := ratio_setting.ModelRatio2JSONString()
			t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios)) })
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-3.5-turbo":1}`))
			require.NoError(t, model.DB.AutoMigrate(&model.Token{}, &model.Channel{}, &model.Ability{}, &model.Option{}))
			previousRetry, previousCount, previousTimeout := common.RetryTimes, constant.CountToken, constant.StreamingTimeout
			common.MemoryCacheEnabled, common.RetryTimes, constant.CountToken, constant.StreamingTimeout = true, 1, false, 30
			t.Cleanup(func() {
				common.MemoryCacheEnabled, common.RetryTimes, constant.CountToken = previousCache, previousRetry, previousCount
				constant.StreamingTimeout = previousTimeout
			})
			var upstreamRequests []string
			var requestMu sync.Mutex
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				requestMu.Lock()
				upstreamRequests = append(upstreamRequests, string(body))
				requestMu.Unlock()
				index := attempts.Add(1)
				w.Header().Set("X-Request-Id", fmt.Sprintf("upstream-%d", index))
				if index == 1 {
					w.Header().Set("Msh-Usage-Cache-Write-Tokens-5m", "0")
				} else {
					w.Header().Set("Msh-Usage-Cache-Write-Tokens-1h", "17")
				}
				if index == 1 || allFail {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"type":"upstream_error","message":"provider rejected %s"}}`, r.Header.Get("Authorization")))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"stable\",\"created\":1700000000,\"model\":\"gpt-3.5-turbo\",\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			channels := []model.Channel{
				{Id: 101, Name: "first", Type: constant.ChannelTypeOpenAI, Key: "sk-first-secret", BaseURL: &upstream.URL, Models: "gpt-3.5-turbo", Group: "default", Priority: common.GetPointer(int64(100)), AutoBan: common.GetPointer(0)},
				{Id: 102, Name: "fallback", Type: constant.ChannelTypeOpenAI, Key: "sk-second-secret", BaseURL: &upstream.URL, Models: "gpt-3.5-turbo", Group: "default", Priority: common.GetPointer(int64(90)), AutoBan: common.GetPointer(0)},
			}
			require.NoError(t, model.BatchInsertChannels(channels))
			model.InitChannelCache()
			token := model.Token{UserId: user.Id, Key: "timeline-test-token", Name: "timeline", Status: common.TokenStatusEnabled, RemainQuota: 10000000}
			require.NoError(t, model.DB.Create(&token).Error)
			var outputs []string
			var charges []int
			var tokenCharges []int
			var requestBodies [][]string
			for _, enabled := range []bool{false, true} {
				attempts.Store(0)
				requestMu.Lock()
				upstreamRequests = nil
				requestMu.Unlock()
				require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Updates(map[string]any{"quota": 10000000, "used_quota": 0, "request_count": 0}).Error)
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", token.Id).Updates(map[string]any{"used_quota": 0, "remain_quota": 10000000}).Error)
				model.RequestTimelineCaptureEnabled.Store(enabled)
				var writer *service.RequestTimelineWriter
				if enabled {
					writer = service.InitRequestTimeline()
					require.NotNil(t, writer)
				}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-3.5-turbo","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set(common.RequestIdKey, "fallback-relay")
				common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
				common.SetContextKey(c, constant.ContextKeyUserQuota, 10000000)
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
				common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
				common.SetContextKey(c, constant.ContextKeyTokenUnlimited, false)
				common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-3.5-turbo")
				common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
				require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channels[0], "gpt-3.5-turbo"))
				Relay(c, types.RelayFormatOpenAI)
				assert.Equal(t, int32(2), attempts.Load())
				outputs = append(outputs, w.Body.String())
				requestMu.Lock()
				requestBodies = append(requestBodies, append([]string(nil), upstreamRequests...))
				requestMu.Unlock()
				// Refunds complete asynchronously. Wait for this account and token to
				// reconcile; other tests can have unrelated work in the shared pool.
				var charge, tokenCharge int
				require.EventuallyWithT(t, func(check *assert.CollectT) {
					var account model.User
					var credential model.Token
					require.NoError(check, model.DB.First(&account, user.Id).Error)
					require.NoError(check, model.DB.First(&credential, token.Id).Error)
					charge, tokenCharge = 10000000-account.Quota, credential.UsedQuota
					assert.Equal(check, charge, account.UsedQuota)
					assert.Equal(check, charge, tokenCharge)
					assert.Equal(check, charge, 10000000-credential.RemainQuota)
					if allFail {
						assert.Zero(check, charge)
					} else {
						assert.Positive(check, charge)
					}
				}, 5*time.Second, time.Millisecond)
				charges = append(charges, charge)
				tokenCharges = append(tokenCharges, tokenCharge)
				if enabled {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					require.NoError(t, writer.Shutdown(ctx))
					cancel()
					entry, err := model.FindRequestTimeline(context.Background(), "fallback-relay")
					require.NoError(t, err)
					var data requesttimeline.Data
					require.NoError(t, common.UnmarshalJsonStr(entry.Data, &data))
					require.Len(t, data.Attempts, 2)
					assert.Equal(t, 101, data.Attempts[0].Data.ChannelID)
					assert.Equal(t, "failed", data.Attempts[0].Result)
					assert.Equal(t, "upstream-1", data.Attempts[0].Data.UpstreamRequestID)
					assert.Equal(t, 102, data.Attempts[1].Data.ChannelID)
					assert.Equal(t, "upstream-2", data.Attempts[1].Data.UpstreamRequestID)
					assert.Equal(t, map[string]string{"Msh-Usage-Cache-Write-Tokens-5m": "0"}, data.Attempts[0].Data.UpstreamResponseHeaders)
					assert.Equal(t, map[string]string{"Msh-Usage-Cache-Write-Tokens-1h": "17"}, data.Attempts[1].Data.UpstreamResponseHeaders)
					assert.Equal(t, data.Attempts[1].Data.UpstreamResponseHeaders, data.UpstreamResponseHeaders)
					assert.NotContains(t, entry.Data, "sk-first-secret")
					assert.NotContains(t, entry.Data, "sk-second-secret")
					assert.LessOrEqual(t, len(entry.Data), requesttimeline.MaxDataBytes)
					if allFail {
						assert.Equal(t, "failed", entry.Result)
						assert.Equal(t, "failed", data.Attempts[1].Result)
						assert.Equal(t, 0, charges[len(charges)-1], "all attempts failed: pre-consumption must be refunded")
					} else {
						assert.Equal(t, "success", entry.Result)
						assert.Equal(t, "success", data.Attempts[1].Result)
						assert.Nil(t, data.FirstError)
						require.NotNil(t, data.ReturnedUsage)
						assert.Equal(t, 150, data.ReturnedUsage.TotalTokens)
						assert.Greater(t, charges[len(charges)-1], 0, "successful fallback must actually settle paid usage")
					}
				}
			}
			assert.Equal(t, outputs[0], outputs[1], "timeline must not alter the client response")
			assert.Equal(t, requestBodies[0], requestBodies[1], "timeline must not alter upstream requests")
			assert.Equal(t, charges[0], charges[1], "wallet charge/refund must be identical")
			assert.Equal(t, tokenCharges[0], tokenCharges[1], "token accounting must be identical")
		})
	}
}

func TestRequestTimelineFallbackTerminalAndBoundedHistory(t *testing.T) {
	for _, terminal := range []error{context.Canceled, context.DeadlineExceeded, nil} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			trace := requesttimeline.New(time.Now(), "bounded-fallback", 7, strings.Repeat("<", 256))
			var usage requesttimeline.StreamUsage
			require.NoError(t, common.UnmarshalJsonStr(`{"prompt_tokens":2,"vendor":"`+strings.Repeat("<", 2000)+`"}`, &usage))
			for attempt := 0; attempt < requesttimeline.MaxAttempts+2; attempt++ {
				trace.BeginAttempt(attempt)
				trace.SelectChannel(100+attempt, "sk-test-key")
				trace.ObserveChat(dto.ChatCompletionsStreamResponse{Id: strings.Repeat("<", 256), Model: strings.Repeat("<", 256)}, requesttimeline.StreamError{})
				trace.CaptureUpstreamUsage(usage)
				trace.BusinessError(strings.Repeat("<", 128), "upstream rejected sk-test-key "+strings.Repeat("<", 2048))
				trace.EndAttempt(errors.New("failure"))
			}
			trace.BeginAttempt(requesttimeline.MaxAttempts + 2)
			if terminal == nil {
				trace.ObserveChat(dto.ChatCompletionsStreamResponse{Id: strings.Repeat("<", 256), Model: strings.Repeat("<", 256)}, requesttimeline.StreamError{})
				trace.CaptureUpstreamUsage(usage)
				trace.FirstResponse(time.Now())
				trace.PayloadWritten(nil)
				trace.StreamEnd("done", nil, false)
			}
			record := trace.Freeze(terminal)
			require.NotNil(t, record)
			raw, err := record.MarshalData()
			require.NoError(t, err)
			assert.LessOrEqual(t, len(raw), requesttimeline.MaxDataBytes)
			assert.NotContains(t, string(raw), "sk-test-key")
			assert.Equal(t, requesttimeline.MaxAttempts+3, record.Data.AttemptCount)
			require.GreaterOrEqual(t, len(record.Data.Attempts), 2)
			assert.Equal(t, 1, record.Data.Attempts[0].Number)
			assert.Equal(t, requesttimeline.MaxAttempts+3, record.Data.Attempts[len(record.Data.Attempts)-1].Number)
			assert.Equal(t, record.Data.AttemptCount-len(record.Data.Attempts), record.Data.AttemptsOmitted)
			if terminal == nil {
				assert.Equal(t, "success", record.Result)
				assert.Nil(t, record.Data.FirstError)
			} else {
				assert.Equal(t, "interrupted", record.Result)
				require.NotNil(t, record.Data.FirstError)
			}
		})
	}
}

func TestRequestTimelineClaudeFallbackUsageIsolation(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	bodies := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"failed\",\"model\":\"model\",\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":200}}}\n\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"first attempt failed\"}}\n\n",
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"success\",\"model\":\"model\",\"usage\":{\"input_tokens\":2}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	var outputs []string
	var usages []*dto.Usage
	for _, enabled := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		info := relaycommon.GenRelayInfoClaude(c, nil)
		info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: "model"}
		info.IsStream, info.DisablePing = true, true
		if enabled {
			info.Timeline = requesttimeline.New(info.StartTime, "claude-fallback", 7, "model")
			restore := helper.ObserveRequestTimelineWriter(c, info.Timeline)
			defer restore()
		}
		var firstResponse time.Time
		for index, body := range bodies {
			info.Timeline.BeginAttempt(index)
			info.Timeline.SelectChannel(index+1, "")
			usage, relayErr := claude.ClaudeStreamHandler(c, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, info)
			if index == 0 {
				firstResponse = info.FirstResponseTime
				info.Timeline.EndAttempt(relayErr)
			} else {
				require.Nil(t, relayErr)
				usages = append(usages, usage)
				assert.Equal(t, firstResponse, info.FirstResponseTime, "diagnostics must not reset the relay's request-level first response")
			}
		}
		outputs = append(outputs, w.Body.String())
		if enabled {
			record := info.Timeline.Freeze(nil)
			require.Len(t, record.Data.Attempts, 2)
			assert.Equal(t, "failed", record.Data.Attempts[0].Result)
			assert.Equal(t, 300, record.Data.Attempts[0].Data.ReturnedUsage.InputTokens)
			assert.Equal(t, "success", record.Result)
			assert.Nil(t, record.Data.FirstError)
			assert.Equal(t, 5, record.Data.ReturnedUsage.TotalTokens, "failed attempt cache usage must not leak into the fallback")
			require.NotNil(t, record.Data.Attempts[1].Data.FirstResponseMS)
			assert.True(t, record.Data.Attempts[1].Data.TimingValid)
		}
	}
	assert.Equal(t, outputs[0], outputs[1])
	assert.Equal(t, usages[0], usages[1])
}
