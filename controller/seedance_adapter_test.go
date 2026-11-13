package controller

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSeedanceValidationCallbackUsesBoundSession(t *testing.T) {
	setupNativeActionTestDB(t)
	t.Setenv("CRYPTO_SECRET", common.CryptoSecret)
	require.NoError(t, model.DB.AutoMigrate(&model.SeedanceAssetValidationSession{}, &model.SeedanceAssetGroup{}))
	t.Cleanup(func() {
		require.NoError(t, model.DB.Migrator().DropTable(&model.SeedanceAssetValidationSession{}, &model.SeedanceAssetGroup{}))
	})
	previousAddress := system_setting.TaskPublicAddress
	system_setting.TaskPublicAddress = "https://gateway.example"
	t.Cleanup(func() { system_setting.TaskPublicAddress = previousAddress })
	var callbackURL string
	var resultCalls, tokenNumber int
	ready := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, common.DecodeJson(r.Body, &body))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("Action") {
		case "CreateVisualValidateSession":
			tokenNumber++
			callbackURL = body["CallbackURL"]
			_, _ = fmt.Fprintf(w, `{"Result":{"BytedToken":"fixture-token-%d","H5Link":"https://validation.example/launch"}}`, tokenNumber)
		case "GetVisualValidateResult":
			resultCalls++
			if !ready {
				_, _ = io.WriteString(w, `{"Result":{}}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"Result":{"GroupId":"group-%s"}}`, body["BytedToken"])
		default:
			_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
		}
	}))
	t.Cleanup(upstream.Close)
	channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-channel-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled}
	settings := channel.GetSetting()
	settings.SeedanceAssetURL = upstream.URL
	channel.SetSetting(settings)
	require.NoError(t, model.DB.Create(channel).Error)
	for _, route := range []string{"standard", "compat"} {
		for _, scenario := range []string{"state_only", "matching_token", "wrong_token", "empty_token", "duplicate_token", "duplicate_state", "wrong_state", "malformed_query", "expired", "pending"} {
			t.Run(route+"/"+scenario, func(t *testing.T) {
				ready = scenario != "pending"
				start, err := service.StartSeedanceAssetValidationSessionWithFallback(t.Context(), 9, []*model.Channel{channel}, service.SeedanceAssetValidationGroupDetails{CallbackURL: "https://client.example/callback?customer_state=keep"}, "", "")
				require.NoError(t, err)
				callback, err := url.Parse(callbackURL)
				require.NoError(t, err)
				query := callback.Query()
				query.Set("resultCode", "10000")
				query.Set("status", "success")
				query.Set("GroupId", "untrusted-group")
				query.Set("session_id", "999999")
				wantStatus, wantQueries := http.StatusNotFound, 0
				switch scenario {
				case "state_only", "matching_token":
					wantStatus, wantQueries = http.StatusFound, 1
				case "wrong_token":
					query.Set("bytedToken", "wrong-token")
				case "empty_token":
					query.Set("bytedToken", "")
				case "duplicate_token":
					query["bytedToken"] = []string{start.BytedToken, start.BytedToken}
				case "duplicate_state":
					query.Add("state", query.Get("state"))
				case "wrong_state":
					query.Set("state", "wrong-state")
				case "expired":
					require.NoError(t, model.DB.Model(start.Session).Update("expires_at", time.Now().Add(-time.Minute).Unix()).Error)
					wantStatus = http.StatusFound
				case "pending":
					wantStatus, wantQueries = http.StatusServiceUnavailable, 1
					if route == "compat" {
						wantStatus = http.StatusOK
					}
				}
				if scenario == "matching_token" {
					query.Set("bytedToken", start.BytedToken)
				}
				path := "/doubao/api/assets/liveness/callback"
				if route == "standard" {
					path = fmt.Sprintf("/v1/seedance/validation-sessions/%d/callback", start.Session.ID)
				}
				rawQuery := query.Encode()
				if scenario == "malformed_query" {
					rawQuery += "&invalid=%zz"
				}
				writer := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(writer)
				c.Request = httptest.NewRequest(http.MethodGet, path+"?"+rawQuery, nil)
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(start.Session.ID)}}
				beforeCalls := resultCalls
				if route == "standard" {
					SeedanceAssetValidationCallback(c)
				} else {
					SeedanceCompatValidationCallback(c)
				}
				assert.Equal(t, wantStatus, writer.Code, writer.Body.String())
				assert.Equal(t, wantQueries, resultCalls-beforeCalls)
				assert.Equal(t, "no-store", writer.Header().Get("Cache-Control"))
				stored, err := model.GetSeedanceAssetValidationSession(t.Context(), 9, start.Session.ID)
				require.NoError(t, err)
				if wantStatus == http.StatusFound {
					redirect, err := url.Parse(writer.Header().Get("Location"))
					require.NoError(t, err)
					assert.Equal(t, "keep", redirect.Query().Get("customer_state"))
					assert.Empty(t, redirect.Query().Get("state"))
					assert.Equal(t, fmt.Sprint(stored.ID), redirect.Query().Get("session_id"))
					if scenario == "expired" {
						assert.Equal(t, model.SeedanceAssetValidationStatusExpired, stored.Status)
						assert.Equal(t, "failed", redirect.Query().Get("status"))
						assert.Empty(t, redirect.Query().Get("GroupId"))
						assert.NotEqual(t, "10000", redirect.Query().Get("resultCode"))
					} else {
						assert.Equal(t, model.SeedanceAssetValidationStatusSucceeded, stored.Status)
						assert.Equal(t, "group-"+start.BytedToken, redirect.Query().Get("GroupId"))
						assert.Equal(t, "success", redirect.Query().Get("status"))
						assert.Equal(t, "10000", redirect.Query().Get("resultCode"))
						if route == "standard" {
							SeedanceAssetValidationCallback(c)
						} else {
							SeedanceCompatValidationCallback(c)
						}
						var groups int64
						require.NoError(t, model.DB.Model(&model.SeedanceAssetGroup{}).Where("group_id = ?", stored.UpstreamGroupID).Count(&groups).Error)
						assert.EqualValues(t, 1, groups)
					}
				} else {
					assert.Equal(t, model.SeedanceAssetValidationStatusPending, stored.Status)
					assert.Zero(t, stored.LocalGroupID)
				}
			})
		}
	}
}

func TestTaskCallbackRejectsUnavailableOwner(t *testing.T) {
	setupNativeActionTestDB(t)
	previousAddress := system_setting.TaskPublicAddress
	system_setting.TaskPublicAddress = "https://gateway.example"
	t.Cleanup(func() { system_setting.TaskPublicAddress = previousAddress })
	user := model.User{Username: "callback-owner", AffCode: "callback-owner", Status: common.UserStatusEnabled}
	require.NoError(t, model.DB.Create(&user).Error)
	task := model.Task{TaskID: "callback-task", UserId: user.Id, Platform: "anyllm", Status: model.TaskStatusQueued, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task"}}
	require.NoError(t, model.DB.Create(&task).Error)
	callbackURL, err := service.BuildTaskCallbackURL(user.Id, task.TaskID, string(task.Platform), service.TaskCallbackRequest{URL: "https://callback.example.com/notify", BodyField: "callback_url"})
	require.NoError(t, err)
	request := func() int {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Params = gin.Params{{Key: "key", Value: task.TaskID}}
		c.Request = httptest.NewRequest(http.MethodPost, callbackURL, strings.NewReader(`{}`))
		TaskPluginCallback(c)
		c.Writer.WriteHeaderNow()
		return recorder.Code
	}
	// 无效事件体确保任何场景都不会发出外部回调请求。
	assert.Equal(t, http.StatusBadRequest, request())
	require.NoError(t, model.DB.Model(&user).Update("status", common.UserStatusDisabled).Error)
	assert.Equal(t, http.StatusNotFound, request())
	require.NoError(t, model.DB.Model(&user).Update("status", common.UserStatusEnabled).Error)
	const callback = "test:callback-owner-query-failure"
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == model.DB.NamingStrategy.TableName("users") {
			tx.AddError(errors.New("owner database unavailable"))
		}
	}))
	assert.Equal(t, http.StatusServiceUnavailable, request())
	require.NoError(t, model.DB.Callback().Query().Remove(callback))
	require.NoError(t, model.DB.Delete(&user).Error)
	assert.Equal(t, http.StatusNotFound, request())
}

func TestSeedanceDraftNativeRequestKeepsSingleContent(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin := nativeActionTestPlugin(t, source)
	body := map[string]any{"model": "doubao-seedance-2-5-260628", "content": []any{map[string]any{"type": "draft_task", "draft_task": map[string]any{"id": "public-draft"}}}}
	request := pluginruntime.RouteRequestContext{Method: http.MethodPost, Path: "/doubao/api/v3/contents/generations/tasks", Body: map[string]any{"kind": "json", "value": body}}
	intent, err := plugin.Engine.CallPath(t.Context(), "native", []string{"createTask"}, request.JSValue())
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, request.Path, nil)
	c.Set(pluginruntime.ContextKeyRouteRequest, request)
	c.Set("task_request", intent.(map[string]any)["requestBody"])
	info := &relaycommon.RelayInfo{OriginModelName: "doubao-seedance-2-5-260628", ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeDoubaoVideo, ChannelBaseUrl: "https://upstream.example", ApiKey: "fixture-key", UpstreamModelName: "doubao-seedance-2-5"}, TaskRelayInfo: &relaycommon.TaskRelayInfo{OriginTasks: []relaycommon.OriginTaskRef{{TaskID: "public-draft", UpstreamTaskID: "cgt-draft", Status: "SUCCESS", VideoInput: "none"}}}}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
	reader, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	encoded, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"doubao-seedance-2-5","content":[{"type":"draft_task","draft_task":{"id":"cgt-draft"}}]}`, string(encoded))
}

func TestSeedanceNativeMissingTaskEnvelope(t *testing.T) {
	for _, key := range []string{"doubao", "anyllm"} {
		source, err := builtinplugins.Source(key)
		require.NoError(t, err)
		plugin := nativeActionTestPlugin(t, source)
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			writer := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(method, "/doubao/api/v3/contents/generations/tasks/missing", nil)
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Plugin: plugin})
			c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Method: method, Path: c.Request.URL.Path, Params: map[string]string{"task_id": "missing"}})
			respondTaskPluginNativeActionError(c, http.StatusNotFound, "task_not_found", "Task not found")
			assert.Equal(t, http.StatusNotFound, writer.Code)
			if method == http.MethodGet {
				assert.JSONEq(t, `{"code":"task_not_exist","message":"task_not_exist","data":null}`, writer.Body.String())
			} else {
				assert.JSONEq(t, `{"error":{"code":"task_not_found","message":"Task not found"}}`, writer.Body.String())
			}
		}
	}
}

func TestSeedanceAdapterKeepsNativeEntryAndPrivateResult(t *testing.T) {
	setupNativeActionTestDB(t)
	previousAddress := system_setting.TaskPublicAddress
	system_setting.TaskPublicAddress = "https://gateway.example"
	t.Cleanup(func() { system_setting.TaskPublicAddress = previousAddress })
	service.InitHttpClient()
	registry := pluginruntime.NewRegistry()
	for _, key := range []string{"doubao", "anyllm"} {
		source, err := builtinplugins.Source(key)
		require.NoError(t, err)
		_, err = registry.RegisterFactory(source, pluginruntime.Options{})
		require.NoError(t, err)
	}
	generation := registry.Generation()
	const path = "/doubao/api/v3/contents/generations/tasks"
	binding, found := generation.LookupDeclaredRoute("POST", path)
	require.True(t, found)
	require.Equal(t, "doubao", binding.Plugin.Meta.Key)
	candidates := generation.LookupDeclaredRouteCandidates("POST", path)
	require.Len(t, candidates, 2)
	for _, config := range []struct{ key, basePath string }{{"doubao", ""}, {"anyllm", "/api/v1"}, {"anyllm", "/api"}} {
		t.Run(config.key+config.basePath, func(t *testing.T) {
			selected := config.key
			var received map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, common.DecodeJson(r.Body, &received))
				if selected == "anyllm" {
					assert.Equal(t, "/api/v1/videos/generations", r.URL.Path)
					assert.Equal(t, "task_gateway", r.Header.Get("Idempotency-Key"))
				} else {
					assert.Equal(t, "/api/v3/contents/generations/tasks", r.URL.Path)
				}
				assert.Equal(t, "Bearer fixture-channel-key", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"private-video-id","status":"queued","result_url":"https://private.example/internal-task","metadata":{"accepted":true},"submission_extension":0}`)
			}))
			defer server.Close()
			baseURL := server.URL + config.basePath
			setting := `{"task_plugin_key":"` + selected + `"}`
			upstream := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "fixture-channel-key", BaseURL: &baseURL, Setting: &setting}
			require.NoError(t, model.DB.Create(upstream).Error)
			router := gin.New()
			var task *model.Task
			var adaptor *taskplugin.TaskAdaptor
			router.POST(path, func(c *gin.Context) {
				c.Set("id", 7)
				common.SetContextKey(c, constant.ContextKeyUserId, 7)
				c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: binding.Plugin, Route: binding.Route, Candidates: candidates})
			}, middleware.PrepareTaskPluginRoute(), func(c *gin.Context) {
				require.Nil(t, middleware.SetupContextForSelectedChannel(c, upstream, "doubao-seedance-2-0-260128"))
				pinned, ok := common.GetContextKeyType[pluginruntime.PinnedPlugin](c, pluginruntime.ContextKeyPinnedPlugin)
				require.True(t, ok)
				require.Equal(t, selected, pinned.Plugin.Meta.Key)
				info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: upstream.Type, ApiKey: upstream.Key, ChannelBaseUrl: baseURL, UpstreamModelName: "doubao-seedance-2-0-260128"}, OriginModelName: "doubao-seedance-2-0-260128", TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_gateway"}}
				adaptor = taskplugin.New(pinned.Plugin)
				adaptor.Init(info)
				require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
				body, err := adaptor.BuildRequestBody(c, info)
				require.NoError(t, err)
				response, err := adaptor.DoRequest(c, info, body)
				require.NoError(t, err)
				defer response.Body.Close()
				parsed, taskErr := adaptor.ParseResponse(c, response, info)
				require.Nil(t, taskErr)
				task = &model.Task{TaskID: "task_gateway", UserId: 7, Platform: constant.TaskPlatform(selected), Status: model.TaskStatusQueued, Data: parsed.TaskData, Properties: model.Properties{OriginModelName: info.OriginModelName, UpstreamModelName: info.UpstreamModelName}, PrivateData: model.TaskPrivateData{UpstreamTaskID: parsed.UpstreamTaskID, PluginState: parsed.PluginState}}
				c.JSON(http.StatusOK, gin.H{"id": task.TaskID})
			})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"doubao-seedance-2-0-260128","content":[{"type":"text","text":"fixed camera"},{"type":"image_url","image_url":{"url":"https://media.example/input.jpg"},"role":"reference_image"}],"resolution":"480p","ratio":"16:9","duration":4,"generate_audio":false,"seed":0}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.JSONEq(t, `{"id":"task_gateway"}`, recorder.Body.String())
			assert.Equal(t, "doubao-seedance-2-0-260128", received["model"])
			assert.EqualValues(t, 4, received["duration"])
			assert.Equal(t, false, received["generate_audio"])
			assert.EqualValues(t, 0, received["seed"])
			assert.Equal(t, "16:9", received["ratio"])
			compatible := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"doubao-seedance-2-0-260128","prompt":"fixed camera","content":[{"type":"image_url","image_url":{"url":"https://media.example/input.jpg"},"role":"reference_image"}],"resolution":"720p","duration":8,"parameters":{"resolution":"480p","duration":4,"generate_audio":false,"seed":0}}`))
			compatible.Header.Set("Content-Type", "application/json")
			compatibleResponse := httptest.NewRecorder()
			router.ServeHTTP(compatibleResponse, compatible)
			require.Equal(t, http.StatusOK, compatibleResponse.Code, compatibleResponse.Body.String())
			assert.EqualValues(t, 4, received["duration"])
			assert.Equal(t, "480p", received["resolution"])
			assert.Equal(t, false, received["generate_audio"])
			assert.EqualValues(t, 0, received["seed"])
			if selected != "anyllm" {
				return
			}
			for _, body := range []string{
				`{"status":"running","metadata":{"stages":[{"name":"render","enabled":false,"count":0,"optional":null,"authorization":"Bearer fixture-channel-key"}]},"task_id":"private-video-id","api_key":"fixture-channel-key","__proto__":{"polluted":true}}`,
				`{"status":"succeeded","download_url":"https://private.example/video.mp4","duration":6.25,"resolution":"1080p","ratio":"16:9","seed":7,"service_tier":"default","draft":false,"execution_expires_after":0,"metadata":{"result":{"width":1920}},"content":{"format":"mp4","video_url":"https://private.example/video.mp4"},"usage":{"total_tokens":9999,"details":{"result_units":0}}}`,
				`{"code":0,"data":{"status":"pending","internal_provider":"private-provider"}}`,
				`{"code":0,"data":{"status":"success","usage":{"totalTokens":123,"completion_tokens":123,"details":{"seconds":6.25},"apiKey":"fixture-channel-key","provider":"private-provider"}},"internal_provider":"private-provider"}`,
				`{"id":"private-video-id","model":"private-upstream-model","status":"succeeded","download_url":"https://private.example/video.mp4","last_frame_url":"https://private.example/frame.jpg","internal_provider":"private-provider","request-id":"private-request-id","metadata":{"result":{"height":1080},"trace_id":"private-trace-id"},"content":{"codec":"h264"},"usage":{"total_tokens":9999},"new_business_field":{"values":[0,false,null]}}`,
			} {
				result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, []byte(body))
				require.NoError(t, err)
				require.NotEmpty(t, result.TaskData)
				task.Data = result.TaskData
				task.Status = model.TaskStatus(result.Status)
				if len(result.PluginState) > 0 {
					task.PrivateData.PluginState = result.PluginState
				}
				assert.NotContains(t, string(task.Data), "private-provider")
				assert.NotContains(t, string(task.Data), "fixture-channel-key")
				assert.NotContains(t, string(task.Data), "private-video-id")
				assert.NotContains(t, string(task.Data), "private-upstream-model")
				assert.NotContains(t, string(task.Data), "private-request-id")
				assert.NotContains(t, string(task.Data), "private-trace-id")
				if result.Status == model.TaskStatusSuccess {
					assert.Equal(t, 123, result.TotalTokens)
					assert.EqualValues(t, 6.25, result.UsageFacts["seconds"])
					assert.Equal(t, "1080p", result.UsageFacts["resolution"])
					assert.Equal(t, "https://private.example/video.mp4", result.Url)
				}
			}
			require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
			artifacts, err := adaptor.ListArtifacts(task)
			require.NoError(t, err)
			require.Len(t, artifacts, 2)
			content, err := adaptor.BuildContentRequest(task, "video", channel.TaskArtifactClientRequest{Method: http.MethodGet})
			require.NoError(t, err)
			assert.Equal(t, "https://private.example/video.mp4", content.URL)
			assert.True(t, content.Credentialless)
			assert.Empty(t, content.Headers)
			for _, artifact := range []string{"video", "last-frame"} {
				head, err := adaptor.BuildContentRequest(task, artifact, channel.TaskArtifactClientRequest{Method: http.MethodHead})
				require.NoError(t, err)
				assert.Equal(t, http.MethodGet, head.Method)
				assert.True(t, head.Credentialless)
				assert.Empty(t, head.Headers)
			}
			media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(t, r.Header.Get("Authorization"))
				if r.Method != http.MethodGet {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "image/jpeg")
				w.Header().Set("Content-Length", "4")
				_, _ = io.WriteString(w, "jpeg")
			}))
			defer media.Close()
			var state map[string]any
			require.NoError(t, common.Unmarshal(task.PrivateData.PluginState, &state))
			state["media"].(map[string]any)["last_frame"] = media.URL + "/signed-frame.jpg"
			task.PrivateData.PluginState, err = common.Marshal(state)
			require.NoError(t, err)
			task.ChannelId = upstream.Id
			allowPrivateTaskMediaTest(t)
			head, err := adaptor.BuildContentRequest(task, "last-frame", channel.TaskArtifactClientRequest{Method: http.MethodHead})
			require.NoError(t, err)
			headRecorder := httptest.NewRecorder()
			headContext, _ := gin.CreateTestContext(headRecorder)
			headContext.Request = httptest.NewRequest(http.MethodHead, "/v1/tasks/task_gateway/artifacts/last-frame/content", nil)
			require.NoError(t, proxyTaskMedia(headContext, task, head))
			assert.Equal(t, http.StatusOK, headRecorder.Code)
			assert.Equal(t, "4", headRecorder.Header().Get("Content-Length"))
			assert.Empty(t, headRecorder.Body.String())
			queryBinding, found := generation.LookupDeclaredRoute("GET", path+"/:task_id")
			require.True(t, found)
			require.NoError(t, model.DB.Create(task).Error)
			t.Cleanup(func() { require.NoError(t, model.DB.Delete(task).Error) })
			query := gin.New()
			query.GET(path+"/:task_id", func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyUserId, 7)
				c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: queryBinding.Plugin, Route: queryBinding.Route, Candidates: generation.LookupDeclaredRouteCandidates("GET", path+"/:task_id")})
			}, middleware.PrepareTaskPluginRoute())
			queryRecorder := httptest.NewRecorder()
			query.ServeHTTP(queryRecorder, httptest.NewRequest(http.MethodGet, path+"/task_gateway", nil))
			require.Equal(t, http.StatusOK, queryRecorder.Code, queryRecorder.Body.String())
			videoURL := "https://private.example/video.mp4"
			frameURL := "https://private.example/frame.jpg"
			var publicResponse struct {
				Content           map[string]string `json:"content"`
				RequestParameters map[string]any    `json:"request_parameters"`
				Metadata          map[string]any    `json:"metadata"`
				Usage             map[string]any    `json:"usage"`
				Extension         map[string]any    `json:"new_business_field"`
			}
			require.NoError(t, common.Unmarshal(queryRecorder.Body.Bytes(), &publicResponse))
			assert.Equal(t, videoURL, publicResponse.Content["video_url"])
			assert.Equal(t, frameURL, publicResponse.Content["last_frame_url"])
			assert.Equal(t, "mp4", publicResponse.Content["format"])
			assert.Equal(t, "h264", publicResponse.Content["codec"])
			assert.Equal(t, true, publicResponse.Metadata["accepted"])
			assert.Equal(t, map[string]any{"width": float64(1920), "height": float64(1080)}, publicResponse.Metadata["result"])
			assert.Equal(t, []any{map[string]any{"name": "render", "enabled": false, "count": float64(0), "optional": nil}}, publicResponse.Metadata["stages"])
			assert.Equal(t, []any{float64(0), false, nil}, publicResponse.Extension["values"])
			assert.EqualValues(t, 123, publicResponse.Usage["total_tokens"])
			assert.EqualValues(t, 123, publicResponse.Usage["completion_tokens"])
			assert.Equal(t, map[string]any{"result_units": float64(0), "seconds": float64(6.25)}, publicResponse.Usage["details"])
			assert.NotContains(t, publicResponse.Usage, "totalTokens")
			assert.NotContains(t, publicResponse.Usage, "provider")
			assert.NotContains(t, publicResponse.Usage, "apiKey")
			assert.NotContains(t, queryRecorder.Body.String(), `"data":`)
			assert.NotContains(t, queryRecorder.Body.String(), `"polluted":`)
			assert.Contains(t, queryRecorder.Body.String(), `"submission_extension":0`)
			assert.Contains(t, queryRecorder.Body.String(), `"draft":false`)
			assert.Contains(t, queryRecorder.Body.String(), `"execution_expires_after":0`)
			view, err := service.BuildTaskPluginView(task)
			require.NoError(t, err)
			for _, candidate := range candidates {
				if candidate.Plugin.Meta.Key != selected {
					continue
				}
				listed, err := candidate.Plugin.Engine.CallPath(t.Context(), "native", []string{"taskList"}, map[string]any{}, map[string]any{"items": []dto.TaskView{view}, "total": 1})
				require.NoError(t, err)
				encoded, err := common.Marshal(listed)
				require.NoError(t, err)
				assert.JSONEq(t, `{"items":[`+queryRecorder.Body.String()+`],"total":1}`, string(encoded))
			}
			videoResponse, err := adaptor.ConvertToOpenAIVideo(task)
			require.NoError(t, err)
			var videoObject map[string]any
			require.NoError(t, common.Unmarshal(videoResponse, &videoObject))
			assert.Equal(t, videoURL, videoObject["download_url"])
			assert.Contains(t, queryRecorder.Body.String(), `"total_tokens":123`)
			assert.EqualValues(t, 4, publicResponse.RequestParameters["duration"])
			assert.Equal(t, "480p", publicResponse.RequestParameters["resolution"])
			assert.EqualValues(t, 0, publicResponse.RequestParameters["seed"])
			assert.Equal(t, false, publicResponse.RequestParameters["generate_audio"])
			assert.NotContains(t, publicResponse.RequestParameters, "content")
			assert.NotContains(t, publicResponse.RequestParameters, "prompt")
			assert.Contains(t, queryRecorder.Body.String(), `"resolution":"1080p"`)
			assert.Contains(t, queryRecorder.Body.String(), `"ratio":"16:9"`)
			assert.Contains(t, queryRecorder.Body.String(), `"seed":7`)
			assert.NotContains(t, strings.ToLower(queryRecorder.Body.String()), "anyllm")
			assert.Contains(t, queryRecorder.Body.String(), "https://private.example/video.mp4")
			require.NoError(t, model.DB.Model(task).UpdateColumn("status", model.TaskStatusInProgress).Error)
			pendingRecorder := httptest.NewRecorder()
			query.ServeHTTP(pendingRecorder, httptest.NewRequest(http.MethodGet, path+"/task_gateway", nil))
			require.Equal(t, http.StatusOK, pendingRecorder.Code, pendingRecorder.Body.String())
			assert.Contains(t, pendingRecorder.Body.String(), `"status":"running"`)
		})
	}
}

func TestSeedanceAdapterCompatibleEntriesKeepModelsAndAssetReferences(t *testing.T) {
	source, err := builtinplugins.Source("anyllm")
	require.NoError(t, err)
	plugin, err := pluginruntime.CompilePlugin(source, pluginruntime.Options{})
	require.NoError(t, err)
	for _, tc := range []struct {
		protocol string
		body     map[string]any
	}{
		{"openai_video", map[string]any{"prompt": "keep this prompt", "content": []any{map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": "asset://asset-owned"}}}}},
		{"openai_responses", map[string]any{"input": "keep this prompt", "metadata": map[string]any{"content": []any{map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": "asset://asset-owned"}}}}}},
	} {
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{tc.protocol, "decodeRequest"}, map[string]any{"model": "doubao-seedance-2-0-260128", "body": map[string]any{"kind": "json", "value": tc.body}})
		require.NoError(t, err)
		encoded, err := common.Marshal(value)
		require.NoError(t, err)
		assert.Equal(t, 1, strings.Count(string(encoded), "keep this prompt"))
		assert.Contains(t, string(encoded), "asset://asset-owned")
	}
	multipart, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "doubao-seedance-2-0-260128",
		"body":  map[string]any{"kind": "multipart", "fields": map[string]any{"prompt": []string{"animate"}, "seconds": []string{"4"}, "size": []string{"1280x720"}, "watermark": []string{"false"}}},
	})
	require.NoError(t, err)
	encodedMultipart, err := common.Marshal(multipart)
	require.NoError(t, err)
	assert.Contains(t, string(encodedMultipart), `"resolution":"720p"`)
	assert.Contains(t, string(encodedMultipart), `"watermark":false`)
	for _, protocol := range []string{"openai_video", "openai_responses"} {
		body := map[string]any{"model": "doubao-seedance-2-0-260128", "prompt": "fixed camera", "seconds": 4, "resolution": "480p", "input_reference": "asset://asset-owned", "seed": 12345}
		if protocol == "openai_responses" {
			body["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "fixed camera"}, map[string]any{"type": "input_image", "image_url": "asset://asset-owned"}}}}
			delete(body, "input_reference")
		}
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{protocol, "decodeRequest"}, map[string]any{"model": body["model"], "body": map[string]any{"kind": "json", "value": body}})
		require.NoError(t, err)
		encoded, err := common.Marshal(value)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), "doubao-seedance-2-0-260128")
		assert.Contains(t, string(encoded), "asset://asset-owned")
		assert.Contains(t, string(encoded), `"seed":12345`)
		assert.NotContains(t, strings.ToLower(string(encoded)), "anyllm")
	}
	for _, seed := range []any{-2, 4294967296, 0.5, true, "invalid"} {
		_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "doubao-seedance-2-0-260128",
			"body":  map[string]any{"kind": "json", "value": map[string]any{"model": "doubao-seedance-2-0-260128", "prompt": "fixed camera", "seed": seed}},
		})
		require.ErrorContains(t, err, "seed")
	}
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "doubao-seedance-2-0-260128",
		"body":  map[string]any{"kind": "json", "value": map[string]any{"prompt": "fixed camera", "seed": "12345", "priority": "0", "execution_expires_after": "3600"}},
	})
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"seed":12345`)
	assert.Contains(t, string(encoded), `"priority":0`)
	assert.Contains(t, string(encoded), `"execution_expires_after":3600`)
	for _, protocol := range []string{"openai_video", "openai_responses"} {
		for _, seconds := range []any{"4", "-1", "4.5", "", true, 0, "9007199254740993"} {
			body := map[string]any{"prompt": "fixed camera", "seconds": seconds}
			if protocol == "openai_responses" {
				body["input"] = "fixed camera"
			}
			value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{protocol, "decodeRequest"}, map[string]any{
				"model": "doubao-seedance-2-0-260128", "body": map[string]any{"kind": "json", "value": body},
			})
			if seconds != "4" && seconds != "-1" {
				require.Error(t, err)
				continue
			}
			require.NoError(t, err)
			request := value.(map[string]any)["requestBody"].(map[string]any)
			if seconds == "-1" {
				assert.Equal(t, true, request["automatic_duration"])
			} else {
				assert.EqualValues(t, 4, request["duration"])
			}
		}
	}
	for _, strict := range []bool{false, true} {
		ctx := map[string]any{"model": "doubao-seedance-2-0-260128", "body": map[string]any{"kind": "json", "value": map[string]any{"prompt": "fixed camera", "seconds": "4", "duration": 4}}}
		if strict {
			_, err = plugin.Engine.CallPath(t.Context(), "native", []string{"createTask"}, ctx)
			require.Error(t, err)
		} else {
			_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, ctx)
			require.NoError(t, err)
		}
	}
	for _, status := range []int{400, 401, 403, 429, 500, 502} {
		value, err := plugin.Engine.Call(t.Context(), "publicErrorMessage", map[string]any{"httpStatus": status, "local": false, "message": "supplier-canary https://private.example/route"})
		require.NoError(t, err)
		assert.NotContains(t, value, "supplier-canary")
		assert.NotContains(t, value, "private.example")
	}
}

func TestSeedanceAdapterMatchesDoubaoCommonInputs(t *testing.T) {
	plugins := make(map[string]*pluginruntime.LoadedPlugin)
	for _, key := range []string{"doubao", "anyllm"} {
		source, err := builtinplugins.Source(key)
		require.NoError(t, err)
		plugins[key], err = pluginruntime.CompilePlugin(source, pluginruntime.Options{})
		require.NoError(t, err)
	}
	const modelName = "doubao-seedance-2-0-260128"
	for _, tc := range []struct {
		name, protocol string
		body           map[string]any
	}{
		{"video image", "openai_video", map[string]any{"prompt": "animate", "image": "asset://asset-owned"}},
		{"video images", "openai_video", map[string]any{"prompt": "animate", "images": []any{"asset://asset-owned"}}},
		{"video metadata image", "openai_video", map[string]any{"prompt": "animate", "metadata": map[string]any{"image": "asset://asset-owned"}}},
		{"responses string message", "openai_responses", map[string]any{"input": []any{map[string]any{"role": "user", "content": "animate"}}}},
		{"responses image object", "openai_responses", map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "animate"}, map[string]any{"type": "input_image", "image_url": map[string]any{"url": "asset://asset-owned"}},
		}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var baseline any
			for _, key := range []string{"doubao", "anyllm"} {
				body := make(map[string]any, len(tc.body)+1)
				for field, value := range tc.body {
					body[field] = value
				}
				body["model"] = modelName
				decoded, err := plugins[key].Engine.CallPath(t.Context(), "protocols", []string{tc.protocol, "decodeRequest"}, map[string]any{
					"model": modelName, "body": map[string]any{"kind": "json", "value": body},
				})
				require.NoError(t, err)
				intent, ok := decoded.(map[string]any)
				require.True(t, ok)
				descriptor, err := plugins[key].Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
					"model": modelName, "upstreamModel": modelName, "requestBody": intent["requestBody"],
					"baseUrl": "https://channel.example/api", "apiKey": "fixture-key", "publicTaskId": "task_public",
				})
				require.NoError(t, err)
				request, ok := descriptor.(map[string]any)
				require.True(t, ok)
				upstream, ok := request["body"].(map[string]any)
				require.True(t, ok)
				if key == "doubao" {
					baseline = upstream["content"]
				} else {
					assert.Equal(t, baseline, upstream["content"])
					if tc.name != "responses string message" {
						encoded, err := common.Marshal(upstream["content"])
						require.NoError(t, err)
						assert.Contains(t, string(encoded), "asset://asset-owned")
					}
				}
			}
		})
	}
	for _, body := range []map[string]any{
		{"content": []any{map[string]any{"type": "image_url", "role": "invalid", "image_url": map[string]any{"url": "asset://asset-owned"}}}},
		{"prompt": "animate", "tools": []any{map[string]any{"type": "unknown"}}},
		{"content": []any{map[string]any{"type": "audio_url", "role": "reference_audio", "audio_url": map[string]any{"url": "asset://asset-owned"}}}},
	} {
		for _, key := range []string{"doubao", "anyllm"} {
			_, err := plugins[key].Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": modelName, "body": map[string]any{"kind": "json", "value": body},
			})
			require.Error(t, err)
		}
	}
	for _, modelName := range []string{
		"doubao-seedance-2-0-260128", "doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615", "doubao-seedance-2-5-260628",
	} {
		t.Run(modelName+" supported fields", func(t *testing.T) {
			var baseline map[string]any
			for _, key := range []string{"doubao", "anyllm"} {
				body := map[string]any{"model": modelName, "prompt": "animate", "resolution": "480p", "seconds": 4,
					"ratio": "16:9", "generate_audio": false, "watermark": false, "return_last_frame": true,
					"seed": 0, "priority": 0, "execution_expires_after": 3600, "output_format": "mp4", "tools": []any{map[string]any{"type": "web_search"}}}
				decoded, err := plugins[key].Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
					"model": modelName, "body": map[string]any{"kind": "json", "value": body},
				})
				require.NoError(t, err)
				intent, ok := decoded.(map[string]any)
				require.True(t, ok)
				descriptor, err := plugins[key].Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
					"model": modelName, "upstreamModel": modelName, "requestBody": intent["requestBody"],
					"baseUrl": "https://channel.example/api", "apiKey": "fixture-key", "publicTaskId": "task_public",
				})
				require.NoError(t, err)
				request, ok := descriptor.(map[string]any)
				require.True(t, ok)
				upstream, ok := request["body"].(map[string]any)
				require.True(t, ok)
				if key == "doubao" {
					baseline = upstream
					continue
				}
				for _, field := range []string{"model", "content", "resolution", "duration", "ratio", "generate_audio", "watermark", "return_last_frame", "seed", "priority", "execution_expires_after", "output_format", "tools"} {
					assert.Equal(t, baseline[field], upstream[field], field)
				}
			}
		})
	}
	t.Run("mapped alias keeps public identity", func(t *testing.T) {
		const alias = "video-public-alias"
		decoded, err := plugins["anyllm"].Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": alias, "upstreamModel": modelName, "body": map[string]any{"kind": "json", "value": map[string]any{
				"model": alias, "prompt": "animate", "image": "asset://asset-owned", "resolution": "480p", "seconds": 4,
			}},
		})
		require.NoError(t, err)
		intent, ok := decoded.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, alias, intent["model"])
		ctx := map[string]any{"model": alias, "upstreamModel": modelName, "requestBody": intent["requestBody"],
			"baseUrl": "https://channel.example/api", "apiKey": "fixture-key", "publicTaskId": "task_alias"}
		descriptor, err := plugins["anyllm"].Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.NoError(t, err)
		request, ok := descriptor.(map[string]any)
		require.True(t, ok)
		upstream, ok := request["body"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, modelName, upstream["model"])
		created, err := plugins["anyllm"].Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"id": "private-id"}})
		require.NoError(t, err)
		result, ok := created.(map[string]any)
		require.True(t, ok)
		public, err := common.Marshal(result["taskData"])
		require.NoError(t, err)
		assert.Contains(t, string(public), alias)
		assert.NotContains(t, string(public), modelName)
		assert.NotContains(t, strings.ToLower(string(public)), "anyllm")
	})
}

func TestSeedanceAdapterMatchesDoubaoResponsesPresentation(t *testing.T) {
	var baseline any
	for _, key := range []string{"doubao", "anyllm"} {
		source, err := builtinplugins.Source(key)
		require.NoError(t, err)
		plugin, err := pluginruntime.CompilePlugin(source, pluginruntime.Options{})
		require.NoError(t, err)
		ctx := map[string]any{"artifacts": map[string]any{"video": map[string]any{"url": "/v1/videos/task_public/content"}}}
		task := map[string]any{"status": "IN_PROGRESS", "progress": "34%"}
		progress, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "renderEvents"}, ctx, task, nil)
		require.NoError(t, err)
		final, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "renderFinal"}, ctx, task)
		require.NoError(t, err)
		result := []any{progress, final}
		if key == "doubao" {
			baseline = result
		} else {
			assert.Equal(t, baseline, result)
			encoded, err := common.Marshal(final)
			require.NoError(t, err)
			assert.Contains(t, string(encoded), `\u003cvideo controls src=`)
			assert.NotContains(t, strings.ToLower(string(encoded)), "anyllm")
		}
	}
}

func TestSeedanceAdapterKeepsFailureDiagnosticsPrivate(t *testing.T) {
	source, err := builtinplugins.Source("anyllm")
	require.NoError(t, err)
	plugin, err := pluginruntime.CompilePlugin(source, pluginruntime.Options{})
	require.NoError(t, err)
	adaptor := taskplugin.New(plugin)
	task := &model.Task{TaskID: "task_failure", Platform: "anyllm", Properties: model.Properties{OriginModelName: "doubao-seedance-2-0-260128"},
		PrivateData: model.TaskPrivateData{Key: "fixture-channel-secret", PluginState: []byte(`{"phase":"result","facts":{"tokens":123,"resolution":"480p","video_input":"none"}}`)}}
	body := []byte(`{"status":"failed","failure_stage":"generation","request-id":"request-fixture","error":{"code":"InvalidParameter","type":"upstream_error","message":"AnyLLM: reference image is invalid fixture-channel-secret sk-provider-secret https://private.example/path token=other-secret"}}`)
	result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, body)
	require.NoError(t, err)
	require.Equal(t, string(model.TaskStatusFailure), result.Status)
	assert.Contains(t, result.Reason, "reference image is invalid")
	task.Status, task.Data, task.PrivateData.PluginState = model.TaskStatusFailure, result.TaskData, result.PluginState
	var state struct {
		Diagnostic struct {
			Stage     string `json:"stage"`
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Message   string `json:"message"`
		} `json:"diagnostic"`
	}
	require.NoError(t, common.Unmarshal(task.PrivateData.PluginState, &state))
	assert.Equal(t, "generation", state.Diagnostic.Stage)
	assert.Equal(t, "upstream_error", state.Diagnostic.Type)
	assert.Equal(t, "request-fixture", state.Diagnostic.RequestID)
	assert.Contains(t, state.Diagnostic.Message, "reference image is invalid")
	assert.NotContains(t, state.Diagnostic.Message, "fixture-channel-secret")
	assert.NotContains(t, state.Diagnostic.Message, "sk-provider-secret")
	assert.NotContains(t, state.Diagnostic.Message, "private.example")
	assert.NotContains(t, state.Diagnostic.Message, "other-secret")
	user := tasksToDto([]*model.Task{task}, false, common.RoleCommonUser)[0]
	public, err := common.Marshal(user)
	require.NoError(t, err)
	assert.Contains(t, string(public), "reference image is invalid")
	assert.NotContains(t, string(public), "request-fixture")
	assert.NotContains(t, string(public), "private.example")
	assert.Contains(t, string(public), `"code":"InvalidParameter"`)
	assert.NotContains(t, strings.ToLower(string(public)), "anyllm")
	malformed := []byte(`{"status":"failed","request-id":"fixture-channel-secret","error":{"type":"fixture-channel-secret","message":"rejected"}}`)
	result, err = adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, malformed)
	require.NoError(t, err)
	assert.NotContains(t, string(result.PluginState), "fixture-channel-secret")
}

func TestSeedanceAdapterNativeListFilters(t *testing.T) {
	setupNativeActionTestDB(t)
	source, err := builtinplugins.Source("anyllm")
	require.NoError(t, err)
	registry := pluginruntime.NewRegistry()
	_, err = registry.RegisterFactory(source, pluginruntime.Options{})
	require.NoError(t, err)
	generation := registry.Generation()
	const path = "/doubao/api/v3/contents/generations/tasks"
	binding, found := generation.LookupDeclaredRoute(http.MethodGet, path)
	require.True(t, found)
	_, found = generation.LookupDeclaredRoute(http.MethodDelete, path+"/:task_id")
	assert.False(t, found)
	now := time.Now().Unix()
	for _, row := range []struct {
		id, plugin, tier string
		user             int
		submitted        int64
	}{
		{"list-flex", "anyllm", "flex", 7, now - 60},
		{"list-default", "anyllm", "default", 7, now - 60},
		{"list-old", "anyllm", "flex", 7, now - 8*86400},
		{"list-other-user", "anyllm", "flex", 8, now - 60},
		{"list-other-plugin", "doubao", "flex", 7, now - 60},
	} {
		data, err := common.Marshal(map[string]any{"id": row.id, "service_tier": row.tier, "status": "succeeded"})
		require.NoError(t, err)
		require.NoError(t, model.DB.Create(&model.Task{TaskID: row.id, Platform: constant.TaskPlatform(row.plugin), UserId: row.user, Action: "text_to_video",
			Status: model.TaskStatusSuccess, CreatedAt: row.submitted, SubmitTime: row.submitted, Data: data, Properties: model.Properties{OriginModelName: "doubao-seedance-2-5-260628"}}).Error)
	}
	router := gin.New()
	router.GET(path, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUserId, 7)
		c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: binding.Plugin, Route: binding.Route})
	}, middleware.PrepareTaskPluginRoute(), RelayTaskPluginNativeAction)
	for _, tc := range []struct {
		query, wanted string
		code          int
	}{
		{"?page_size=500&filter.service_tier=flex&filter.status=succeeded", "list-flex", http.StatusOK},
		{"", "list-default", http.StatusOK},
		{"?page_num=2&page_size=1&filter.service_tier=flex", "", http.StatusOK},
		{"?page_size=501", "", http.StatusBadRequest},
		{"?filter.service_tier=invalid", "", http.StatusBadRequest},
		{"?filter.service_tier=flex&filter.service_tier=default", "", http.StatusBadRequest},
		{"?filter.status=invalid", "", http.StatusBadRequest},
		{"?filter.task_ids=", "", http.StatusBadRequest},
		{"?unknown=1", "", http.StatusBadRequest},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path+tc.query, nil))
		require.Equal(t, tc.code, recorder.Code, tc.query+": "+recorder.Body.String())
		if tc.code != http.StatusOK {
			continue
		}
		var result struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
		assert.Equal(t, 1, result.Total)
		if tc.wanted == "" {
			assert.Empty(t, result.Items)
		} else {
			require.Len(t, result.Items, 1)
			assert.Equal(t, tc.wanted, result.Items[0]["id"])
		}
	}
}

func TestSeedanceAdapterCompletionDurationDoesNotUseEstimates(t *testing.T) {
	for _, key := range []string{"doubao", "anyllm"} {
		source, err := builtinplugins.Source(key)
		require.NoError(t, err)
		plugin, err := pluginruntime.CompilePlugin(source, pluginruntime.Options{})
		require.NoError(t, err)
		adaptor := taskplugin.New(plugin)
		for _, duration := range []any{nil, 4.5, -1, 0, 3601, "4.5"} {
			body := map[string]any{"status": "succeeded", "download_url": "https://media.example/video.mp4", "usage": map[string]any{"total_tokens": 123}}
			if duration != nil {
				body["duration"] = duration
			}
			encoded, err := common.Marshal(body)
			require.NoError(t, err)
			task := &model.Task{TaskID: "task_duration", Platform: constant.TaskPlatform(key), Properties: model.Properties{OriginModelName: "doubao-seedance-2-0-260128"},
				PrivateData: model.TaskPrivateData{PluginState: []byte(`{"phase":"final","facts":{"tokens":123,"resolution":"480p","video_input":"none"}}`)}}
			result, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, encoded)
			require.NoError(t, err)
			if duration == 4.5 {
				assert.EqualValues(t, 4.5, result.UsageFacts["seconds"])
			} else {
				assert.NotContains(t, result.UsageFacts, "seconds")
			}
		}
		if key != "anyllm" {
			continue
		}
		ctx := map[string]any{"model": "doubao-seedance-2-0-260128", "publicTaskId": "task_duration", "requestBody": map[string]any{
			"model": "doubao-seedance-2-0-260128", "content": []any{map[string]any{"type": "text", "text": "animate"}}, "resolution": "480p", "automatic_duration": true,
		}}
		facts, err := plugin.Engine.Call(t.Context(), "extractUsage", ctx)
		require.NoError(t, err)
		assert.EqualValues(t, 15, facts.(map[string]any)["seconds"])
		created, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", ctx, map[string]any{"body": map[string]any{"id": "private-id"}})
		require.NoError(t, err)
		state := created.(map[string]any)["state"].(map[string]any)
		assert.NotContains(t, state["facts"], "seconds")
	}
}

func TestSeedanceTaskSnapshotKeepsClientAssetReference(t *testing.T) {
	setupNativeActionTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.TaskRequestSnapshot{}))
	t.Cleanup(func() { require.NoError(t, model.DB.Migrator().DropTable(&model.TaskRequestSnapshot{})) })
	previousEnabled, previousMax := constant.TaskRequestSnapshotEnabled.Load(), constant.TaskRequestSnapshotMaxBytes
	constant.TaskRequestSnapshotEnabled.Store(true)
	constant.TaskRequestSnapshotMaxBytes = 256 << 10
	t.Cleanup(func() {
		constant.TaskRequestSnapshotEnabled.Store(previousEnabled)
		constant.TaskRequestSnapshotMaxBytes = previousMax
	})
	previousConsume := common.LogConsumeEnabled
	common.LogConsumeEnabled = false
	t.Cleanup(func() { common.LogConsumeEnabled = previousConsume })
	c := taskSubmissionTestContext()
	c.Set("task_request", map[string]any{"image_url": "https://storage.example/private?signature=fixture"})
	c.Set(pluginruntime.ContextKeyClientRequestBody, map[string]any{"image_url": "asset://asset-local"})
	c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Body: map[string]any{
		"kind": string(pluginruntime.BodyJSON), "value": map[string]any{"image_url": "asset://upstream-private"},
	}})
	events := []string{}
	info := taskSubmissionRelayInfo(&taskSubmissionTestBilling{events: &events})
	info.Action = constant.TaskActionImageToVideo
	outcome, taskErr := executeTaskSubmissionWith(c, info, func(*gin.Context, *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		return &relay.TaskSubmitResult{UpstreamTaskID: "upstream-private", Platform: "doubao"}, nil
	})
	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	snapshot, err := model.GetTaskRequestSnapshot(t.Context(), outcome.Task.ID)
	require.NoError(t, err)
	assert.Contains(t, string(snapshot.Body), "asset://asset-local")
	assert.NotContains(t, string(snapshot.Body), "storage.example")
	assert.NotContains(t, string(snapshot.Body), "asset://upstream-private")
}

func TestSeedancePricingIdentityDoesNotLeakOrMutateCachedVariants(t *testing.T) {
	cached := []model.Pricing{{ModelName: "doubao-seedance-2-0-260128", BillingPluginVariants: []model.PricingPluginVariant{{PluginKey: "anyllm", PluginName: "Doubao Video（AnyLLM）", BillingExpr: `u("tokens") * 0.1`}, {PluginKey: "doubao", PluginName: "Doubao Video", BillingExpr: `u("tokens") * 0.2`}}}}
	user := pricingForViewer(cached, false)
	encoded, err := common.Marshal(user)
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(encoded)), "anyllm")
	assert.Equal(t, cached[0].BillingPluginVariants[0].BillingExpr, user[0].BillingPluginVariants[0].BillingExpr)
	assert.NotEqual(t, user[0].BillingPluginVariants[0].PluginKey, user[0].BillingPluginVariants[1].PluginKey)
	assert.Equal(t, "Doubao Video（AnyLLM）", cached[0].BillingPluginVariants[0].PluginName)
	admin := pricingForViewer(cached, true)
	assert.Equal(t, "anyllm", admin[0].BillingPluginVariants[0].PluginKey)
	assert.Equal(t, "Doubao Video（AnyLLM）", admin[0].BillingPluginVariants[0].PluginName)
}

func TestSeedanceErrorLogDetailsRemainAdminOnly(t *testing.T) {
	setupNativeActionTestDB(t)
	row := &model.Log{UserId: 7, TokenId: 81, Type: model.LogTypeError, Content: "AnyLLM https://private.example request failed", Other: `{"admin_info":{"task_plugin":{"key":"anyllm","name":"Doubao Video（AnyLLM）"}}}`}
	require.NoError(t, model.DB.Create(row).Error)
	user, err := model.GetLogByTokenId(81)
	require.NoError(t, err)
	require.Len(t, user, 1)
	encoded, err := common.Marshal(user)
	require.NoError(t, err)
	assert.NotContains(t, strings.ToLower(string(encoded)), "anyllm")
	assert.NotContains(t, string(encoded), "private.example")
	var persisted model.Log
	require.NoError(t, model.DB.First(&persisted, row.Id).Error)
	model.FormatAdminLogs([]*model.Log{&persisted})
	assert.Contains(t, persisted.Content, "AnyLLM")
	assert.Contains(t, persisted.Other, "anyllm")
}
