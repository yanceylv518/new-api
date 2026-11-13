package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	relayhost "github.com/QuantumNous/new-api/relay"
	relaychannel "github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func videoAuditPlugin(t *testing.T, key string) *pluginruntime.LoadedPlugin {
	t.Helper()
	previous := pluginruntime.DefaultRegistry
	registry := pluginruntime.NewRegistry()
	pluginruntime.DefaultRegistry = registry
	t.Cleanup(func() { pluginruntime.DefaultRegistry = previous })
	source, err := builtinplugins.Source(key)
	require.NoError(t, err)
	plugin, err := registry.RegisterFactory(source, pluginruntime.Options{Key: key})
	require.NoError(t, err)
	return plugin
}

func TestRealtimeVideoQueryRefundsConfirmedFailure(t *testing.T) {
	setupNativeActionTestDB(t)
	videoAuditPlugin(t, "google")
	service.InitHttpClient()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"operations/review","done":true,"error":{"code":400,"message":"Generation failed"}}`))
	}))
	t.Cleanup(upstream.Close)
	const id = 942001
	require.NoError(t, model.DB.Create(&model.User{Id: id, Username: "realtime-video", Quota: 900, UsedQuota: 100, Group: "default", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.Channel{Id: id, Name: "realtime", Type: constant.ChannelTypeGemini, Key: "fixture", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, UsedQuota: 100}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: id, UserId: id, Key: "realtime-token", RemainQuota: 900, UsedQuota: 100}).Error)
	task := &model.Task{TaskID: "task_realtime_refund", Platform: "google", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusInProgress, Data: []byte(`{}`), Properties: model.Properties{OriginModelName: "veo-3.0-generate-001"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: base64.RawURLEncoding.EncodeToString([]byte("operations/review")), BillingSource: service.BillingSourceWallet, TokenId: id, BillingContext: &model.TaskBillingContext{PerCallBilling: true}}}
	require.NoError(t, model.DB.Create(task).Error)
	for range 2 {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set("id", id)
		c.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+task.TaskID, nil)
		require.Nil(t, relayhost.RelayTaskFetch(c, relayconstant.RelayModeVideoFetchByID))
		assert.Contains(t, recorder.Body.String(), `"status":"failed"`)
	}
	var user model.User
	var token model.Token
	require.NoError(t, model.DB.First(&user, id).Error)
	require.NoError(t, model.DB.First(&token, id).Error)
	assert.Equal(t, 1000, user.Quota)
	assert.Equal(t, 1000, token.RemainQuota)
}

func TestWrappedVideoResponseSettlesActualUsage(t *testing.T) {
	setupNativeActionTestDB(t)
	plugin := videoAuditPlugin(t, "doubao")
	service.InitHttpClient()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"success","data":{"task_id":"upstream","status":"SUCCESS","progress":"100%","data":{"status":"succeeded","duration":10,"resolution":"480p","usage":{"completion_tokens":400},"content":{"video_url":"https://media.example/video.mp4"}}}}`))
	}))
	t.Cleanup(upstream.Close)
	const id = 942002
	require.NoError(t, model.DB.Create(&model.User{Id: id, Username: "wrapped-video", Quota: 800, UsedQuota: 200}).Error)
	channel := &model.Channel{Id: id, Name: "wrapped", Type: constant.ChannelTypeTaskPlugin, Key: "fixture", BaseURL: &upstream.URL, UsedQuota: 200}
	require.NoError(t, model.DB.Create(channel).Error)
	snapshot := &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ModelName: "doubao-seedance-2-0-fast-260128", ExprString: `tier("video", u("tokens") * 2 / 1000000)`, ExprVersion: 1, GroupRatio: 1, QuotaPerUnit: 500000, TaskUsageBilling: true, UsageFacts: map[string]any{"tokens": float64(200), "resolution": "480p", "video_input": "none"}}
	task := &model.Task{TaskID: "task_wrapped_usage", Platform: "doubao", UserId: id, ChannelId: id, Quota: 200, Status: model.TaskStatusInProgress, Properties: model.Properties{OriginModelName: snapshot.ModelName}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream", BillingSource: service.BillingSourceWallet, BillingContext: &model.TaskBillingContext{TieredSnapshot: snapshot}}}
	require.NoError(t, model.DB.Create(task).Error)
	completed, err := service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), channel, task)
	require.NoError(t, err)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), completed.Status)
	assert.Equal(t, 400, completed.Quota)
	var persisted map[string]any
	require.NoError(t, common.Unmarshal(completed.Data, &persisted))
	assert.Equal(t, float64(400), persisted["usage"].(map[string]any)["completion_tokens"])
	var user model.User
	require.NoError(t, model.DB.First(&user, id).Error)
	assert.Equal(t, 600, user.Quota)
}

func TestVertexInlineVideoArtifactRemainsRetrievable(t *testing.T) {
	setupNativeActionTestDB(t)
	plugin := videoAuditPlugin(t, "vertex-ai")
	allowPrivateTaskMediaTest(t)
	payload := base64.StdEncoding.EncodeToString([]byte("video-binary-content"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Empty(t, r.Header.Get("Range"))
		assert.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
		assert.Equal(t, "present", r.Header.Get("X-Configured"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"done":true,"response":{"videos":[{"bytesBase64Encoded":"%s","mimeType":"video/mp4"}]}}`, payload)
	}))
	t.Cleanup(upstream.Close)
	const id = 942003
	require.NoError(t, model.DB.Create(&model.User{Id: id, Username: "inline-video", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.Channel{Id: id, Type: constant.ChannelTypeNewAPI, Key: "fixture", BaseURL: &upstream.URL}).Error)
	task := &model.Task{TaskID: "task_inline_video", Platform: "vertex-ai", UserId: id, ChannelId: id, Status: model.TaskStatusSuccess, Data: []byte(`{"done":true,"response":{"videos":[{"mimeType":"video/mp4"}]}}`), Properties: model.Properties{OriginModelName: "veo-3.0-generate-001"}, PrivateData: model.TaskPrivateData{Key: "fixture", BaseURL: upstream.URL, ChannelType: constant.ChannelTypeTaskPlugin, TransportSaved: true, UpstreamTaskID: base64.RawURLEncoding.EncodeToString([]byte("projects/project/locations/global/publishers/google/models/veo-3.0-generate-001/operations/job")), Execution: &model.TaskExecutionSnapshot{TaskPlugin: &model.TaskPluginSnapshot{Key: "vertex-ai"}}}}
	require.NoError(t, model.DB.Create(task).Error)
	artifacts, err := projectTaskArtifacts(task)
	require.NoError(t, err)
	require.Len(t, artifacts, 1)
	for _, tc := range []struct {
		method, byteRange string
		status            int
		body              string
	}{
		{http.MethodGet, "", http.StatusOK, "video-binary-content"},
		{http.MethodGet, "bytes=0-4", http.StatusPartialContent, "video"},
		{http.MethodHead, "", http.StatusOK, ""},
	} {
		recorder := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(recorder)
		requestContext.Request = httptest.NewRequest(tc.method, "/v1/videos/"+task.TaskID+"/content", nil)
		requestContext.Request.Header.Set("Range", tc.byteRange)
		adaptor := taskplugin.New(plugin)
		adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeNewAPI, ChannelBaseUrl: upstream.URL, ApiKey: "fixture", HeadersOverride: map[string]any{"X-Configured": "present", "Host": "unreachable.invalid", "Authorization": "Bearer {api_key}", "X-Client": "{client_header:Authorization}"}}})
		descriptor, err := adaptor.BuildContentRequest(task, "video", relaychannel.TaskArtifactClientRequest{Method: tc.method})
		require.NoError(t, err)
		assert.NotContains(t, descriptor.Headers, "Host")
		assert.NotContains(t, descriptor.Headers, "X-Client")
		// 删除原渠道后，制品仍使用任务的提交账号和地址。
		require.NoError(t, model.DB.Delete(&model.Channel{}, id).Error)
		require.Nil(t, proxyTaskMedia(requestContext, task, descriptor))
		assert.Equal(t, tc.status, recorder.Code)
		assert.Equal(t, tc.body, recorder.Body.String())
		assert.Equal(t, "video/mp4", recorder.Header().Get("Content-Type"))
	}
}

func TestInlineVideoRejectsInvalidUpstreamContent(t *testing.T) {
	task := setupGenericTaskTest(t)
	allowPrivateTaskMediaTest(t)
	previousLimit := taskMediaDataURLMaxEncodedBytes
	taskMediaDataURLMaxEncodedBytes = 128
	t.Cleanup(func() { taskMediaDataURLMaxEncodedBytes = previousLimit })
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"invalid JSON", "<html>invalid</html>", http.StatusOK},
		{"missing video", `{"done":true}`, http.StatusOK},
		{"invalid base64", `{"video":"???"}`, http.StatusOK},
		{"oversized JSON", strings.Repeat("a", 129), http.StatusOK},
		{"partial JSON", `{"video":"dmlkZW8="}`, http.StatusPartialContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/video", nil)
			err := proxyTaskMedia(c, task, &relaychannel.TaskContentRequest{URL: server.URL, ContentType: "video/mp4", Base64ContentPaths: []string{"video"}})
			require.NotNil(t, err)
			var mediaError *taskMediaProxyError
			require.ErrorAs(t, err, &mediaError)
			assert.Equal(t, http.StatusBadGateway, mediaError.status)
			assert.False(t, c.Writer.Written())
			assert.Empty(t, recorder.Body.String())
		})
	}
}

func TestVideoHeadersAndQueryRedirectRespectChannelPolicy(t *testing.T) {
	plugin := videoAuditPlugin(t, "doubao")
	service.InitHttpClient()
	var foreignHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { foreignHits.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(foreign.Close)
	headers := make(chan http.Header, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, foreign.URL, http.StatusFound)
			return
		}
		headers <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"upstream","status":"running"}`))
	}))
	t.Cleanup(upstream.Close)
	const name = "doubao-seedance-2-0-fast-260128"
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: upstream.URL, ApiKey: "current-key", UpstreamModelName: name, HeadersOverride: map[string]any{"X-Configured": "present", "Authorization": "Bearer {api_key}", "X-Client": "{client_header:X-Client}"}}, OriginModelName: name, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_headers"}}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	c.Request.Header.Set("X-Client", "client-value")
	c.Set("task_request", map[string]any{"model": name, "prompt": "waves", "duration": 4, "size": "480p"})
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	response, err := adaptor.DoRequest(c, info, body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, "client-value", (<-headers).Get("X-Client"))
	response, err = adaptor.FetchTask(upstream.URL, "saved-key", &model.Task{TaskID: "upstream"}, "")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	query := <-headers
	assert.Equal(t, "present", query.Get("X-Configured"))
	assert.Equal(t, "Bearer saved-key", query.Get("Authorization"))
	assert.Empty(t, query.Get("X-Client"))
	_, err = adaptor.FetchTask(upstream.URL, "saved-key", &model.Task{TaskID: "redirect"}, "")
	require.ErrorContains(t, err, "cross-origin")
	assert.Zero(t, foreignHits.Load())
}

// 大媒体使用真实流式HTTP读写而非内存Recorder；分别检查完整传输、上游截断和下游取消。
func TestVideoMediaLargeTransferAndDisconnect(t *testing.T) {
	if os.Getenv("VIDEO_DEEP_AUDIT") != "1" {
		t.Skip("explicit large media audit only")
	}
	for _, mode := range []string{"complete", "upstream-truncated", "downstream-cancel"} {
		t.Run(mode, func(t *testing.T) {
			task := setupGenericTaskTest(t)
			allowPrivateTaskMediaTest(t)
			const size = 128 * 1024 * 1024
			upstreamEnded := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(upstreamEnded)
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Length", fmt.Sprint(size))
				chunk := make([]byte, 32768)
				for sent := 0; sent < size; sent += len(chunk) {
					if _, err := w.Write(chunk); err != nil {
						return
					}
					if sent >= 1024*1024 {
						if mode == "upstream-truncated" {
							return
						}
						if mode == "downstream-cancel" {
							w.(http.Flusher).Flush()
							<-r.Context().Done()
							return
						}
					}
				}
			}))
			defer upstream.Close()
			proxyErrors := make(chan error, 1)
			engine := gin.New()
			engine.GET("/video", func(c *gin.Context) {
				proxyErrors <- proxyTaskMedia(c, task, &relaychannel.TaskContentRequest{URL: upstream.URL, Method: http.MethodGet, Credentialless: true})
			})
			proxy := httptest.NewServer(engine)
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/video", nil)
			require.NoError(t, err)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			if mode == "downstream-cancel" {
				_, err = io.CopyN(io.Discard, response.Body, 1024*1024)
				require.NoError(t, err)
				cancel()
				response.Body.Close()
				select {
				case <-upstreamEnded:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "upstream did not release after client cancellation")
				}
				select {
				case <-proxyErrors:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "proxy did not release after client cancellation")
				}
				return
			}
			count, readErr := io.Copy(io.Discard, response.Body)
			proxyErr := <-proxyErrors
			if mode == "complete" {
				require.NoError(t, readErr)
				require.NoError(t, proxyErr)
				assert.EqualValues(t, size, count)
			} else {
				assert.Error(t, readErr)
				// 响应头已写出后控制器只记录流错误，返回nil避免追加JSON；客户端必须观察到截断。
				assert.NoError(t, proxyErr)
				assert.Less(t, count, int64(size))
			}
			t.Logf("MEDIA mode=%s bytes=%d", mode, count)
		})
	}
}
