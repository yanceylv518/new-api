package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKlingNativeRouteSubmitPollSettleAndQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousBatchUpdate := common.BatchUpdateEnabled
	previousLogConsume := common.LogConsumeEnabled
	previousRedisEnabled := common.RedisEnabled
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	database, dialect := openTaskDialectDatabase(t, &model.User{}, &model.Channel{}, &model.Task{}, &model.VideoTaskPendingLog{}, &model.Log{})
	model.DB = database
	model.LOG_DB = database
	common.SetDatabaseTypes(dialect, dialect)
	common.MemoryCacheEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = false
	if previousRedisEnabled {
		common.RedisEnabled = false
	}
	previousModelRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"kling-v1":1}`))
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.BatchUpdateEnabled = previousBatchUpdate
		common.LogConsumeEnabled = previousLogConsume
		if previousRedisEnabled {
			common.RedisEnabled = true
		}
		common.SetDatabaseTypes(previousMainType, previousLogType)
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousModelRatios))
	})
	require.NoError(t, database.Create(&model.User{
		Id:       7,
		Username: "native-route-user",
		Group:    "default",
		Quota:    1_000_000,
	}).Error)

	var submitCalls atomic.Int32
	var queryCalls atomic.Int32
	var extensionCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/video-extend":
			extensionCalls.Add(1)
			assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"kling-extension","task_status":"submitted"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/video-extend/kling-extension":
			queryCalls.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"kling-extension","task_status":"succeed","final_unit_deduction":"1","task_result":{"videos":[{"id":"extension-video","url":"https://cdn.example/extended.mp4","duration":"5"}]}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/text2video":
			id := submitCalls.Add(1)
			body, readErr := io.ReadAll(r.Body)
			if !assert.NoError(t, readErr) {
				http.Error(w, "read request", http.StatusInternalServerError)
				return
			}
			assert.Contains(t, string(body), `"model_name":"kling-v1"`)
			_, _ = fmt.Fprintf(w, `{"code":0,"message":"","data":{"task_id":"kling-private-%d","task_status":"submitted"}}`, id)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/videos/text2video/kling-private-"):
			queryCalls.Add(1)
			id := strings.TrimPrefix(r.URL.Path, "/v1/videos/text2video/")
			_, _ = fmt.Fprintf(w, `{"code":0,"message":"","data":{"task_id":%q,"task_status":"succeed","task_status_msg":"","task_result":{"videos":[{"id":"video-private","url":"https://cdn.example/video.mp4","duration":"5"}]},"final_unit_deduction":"1"}}`, id)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	channel := model.Channel{
		Type:    constant.ChannelTypeKling,
		Name:    "kling-native-e2e",
		Key:     "sk-test",
		BaseURL: &upstream.URL,
		Status:  common.ChannelStatusEnabled,
		Models:  "kling-v1",
		Group:   "default",
	}
	require.NoError(t, database.Create(&channel).Error)

	generation := pluginruntime.DefaultRegistry.Generation()
	require.NotNil(t, generation)
	submitBinding, found := generation.LookupDeclaredRoute(http.MethodPost, "/kling/v1/videos/text2video")
	require.True(t, found)
	require.Equal(t, "kling", submitBinding.Plugin.Meta.Key)

	submitRecorder := httptest.NewRecorder()
	submitContext, _ := gin.CreateTestContext(submitRecorder)
	submitContext.Request = httptest.NewRequest(
		http.MethodPost,
		"/kling/v1/videos/text2video",
		bytes.NewBufferString(`{"model_name":"kling-v1","prompt":"a lighthouse"}`),
	)
	submitContext.Request.Header.Set("Content-Type", "application/json")
	submitContext.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: generation,
		Plugin:     submitBinding.Plugin,
		Route:      submitBinding.Route,
	})
	common.SetContextKey(submitContext, constant.ContextKeyUserId, 7)
	common.SetContextKey(submitContext, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(submitContext, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(submitContext, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(submitContext, constant.ContextKeyUserQuota, 1_000_000)

	middleware.PrepareTaskPluginRoute()(submitContext)
	require.False(t, submitContext.IsAborted(), submitRecorder.Body.String())
	require.Equal(t, "kling-v1", submitContext.GetString("resolved_task_model"))
	require.Equal(t, "text_to_video", submitContext.GetString("task_action"))
	require.Nil(t, middleware.SetupContextForSelectedChannel(submitContext, &channel, "kling-v1"))

	relayInfo := &relaycommon.RelayInfo{
		UserId:          7,
		UserGroup:       "default",
		UsingGroup:      "default",
		UserQuota:       1_000_000,
		TokenGroup:      "default",
		OriginModelName: "kling-v1",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			Action:        submitContext.GetString("task_action"),
			PublicTaskID:  "task_kling_public",
			LockedChannel: &channel,
		},
	}
	relayInfo.IsPlayground = true
	relayInfo.ForcePreConsume = true
	relayInfo.UserSetting.BillingPreference = "wallet_only"

	outcome, taskErr := executeTaskSubmissionWith(submitContext, relayInfo, relay.RelayTaskSubmit)
	require.Nil(t, taskErr)
	require.NotNil(t, outcome)
	require.NotNil(t, relayInfo.Billing)
	require.False(t, submitContext.Writer.Written())

	presentTaskSubmission(submitContext, outcome)
	require.Equal(t, http.StatusOK, submitRecorder.Code)
	assert.Contains(t, submitRecorder.Body.String(), `"task_id":"task_kling_public"`)
	assert.NotContains(t, submitRecorder.Body.String(), "kling-private-1")
	assert.Equal(t, int32(1), submitCalls.Load())

	var persisted model.Task
	require.NoError(t, database.Where("task_id = ?", "task_kling_public").First(&persisted).Error)
	assert.Equal(t, constant.TaskPlatform("kling"), persisted.Platform)
	assert.Equal(t, "kling-private-1", persisted.PrivateData.UpstreamTaskID)
	assert.Equal(t, model.TaskStatus(model.TaskStatusNotStart), persisted.Status)

	previousAdaptorFactory := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		return relay.GetTaskAdaptor(platform)
	}
	t.Cleanup(func() { service.GetTaskAdaptorFunc = previousAdaptorFactory })
	service.DispatchPlatformUpdate(
		context.Background(),
		persisted.Platform,
		map[int][]string{channel.Id: {"kling-private-1"}},
		map[string]*model.Task{"kling-private-1": &persisted},
	)

	require.NoError(t, database.Where("task_id = ?", "task_kling_public").First(&persisted).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), persisted.Status)
	assert.Equal(t, "100%", persisted.Progress)
	assert.Equal(t, 1, persisted.Quota)
	assert.Equal(t, int32(1), queryCalls.Load())
	var settledUser model.User
	require.NoError(t, database.First(&settledUser, 7).Error)
	assert.Equal(t, 999_999, settledUser.Quota)

	queryBinding, found := generation.LookupDeclaredRoute(http.MethodGet, "/kling/v1/videos/text2video/:task_id")
	require.True(t, found)
	queryRecorder := httptest.NewRecorder()
	queryContext, _ := gin.CreateTestContext(queryRecorder)
	queryContext.Request = httptest.NewRequest(http.MethodGet, "/kling/v1/videos/text2video/task_kling_public", nil)
	queryContext.Params = gin.Params{{Key: "task_id", Value: "task_kling_public"}}
	queryContext.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: generation,
		Plugin:     queryBinding.Plugin,
		Route:      queryBinding.Route,
	})
	common.SetContextKey(queryContext, constant.ContextKeyUserId, 7)

	middleware.PrepareTaskPluginRoute()(queryContext)

	require.True(t, queryContext.IsAborted())
	require.Equal(t, http.StatusOK, queryRecorder.Code)
	assert.Contains(t, queryRecorder.Body.String(), `"task_id":"task_kling_public"`)
	assert.Contains(t, queryRecorder.Body.String(), `"task_status":"succeed"`)
	assert.NotContains(t, queryRecorder.Body.String(), "kling-private-1")
	assert.NotContains(t, queryRecorder.Body.String(), upstream.URL)

	t.Run("continuation ownership and original credential", func(t *testing.T) {
		require.NoError(t, database.Create(&model.User{Id: 8, Username: "other-native-user", AffCode: "other-native-user", Group: "default", Quota: 1000}).Error)
		binding, found := generation.LookupDeclaredRoute(http.MethodPost, "/kling/v1/videos/video-extend")
		require.True(t, found)
		channel.Key = "sk-rotated\nsk-test"
		channel.ChannelInfo.IsMultiKey = true
		require.NoError(t, database.Model(&channel).Select("key", "channel_info").Updates(&channel).Error)
		for _, userID := range []int{8, 7} {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/kling/v1/videos/video-extend", strings.NewReader(`{"model_name":"kling-v1","video_id":"video-private","prompt":"continue"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: binding.Plugin, Route: binding.Route})
			common.SetContextKey(c, constant.ContextKeyUserId, userID)
			common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
			middleware.PrepareTaskPluginRoute()(c)
			if userID == 8 {
				require.True(t, c.IsAborted())
				assert.Equal(t, http.StatusNotFound, recorder.Code)
				assert.Zero(t, extensionCalls.Load())
				continue
			}
			require.False(t, c.IsAborted(), recorder.Body.String())
			require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, "kling-v1"))
			assert.Equal(t, "sk-test", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
			info := &relaycommon.RelayInfo{UserId: 7, UserQuota: 999999, UsingGroup: "default", TokenGroup: "default", OriginModelName: "kling-v1", IsPlayground: true, ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: "video_extend", PublicTaskID: "task_kling_extension", LockedChannel: &channel}}
			info.UserSetting.BillingPreference = "wallet_only"
			outcome, err := executeTaskSubmissionWith(c, info, relay.RelayTaskSubmit)
			require.Nil(t, err)
			require.NotNil(t, outcome)
			assert.EqualValues(t, 1, extensionCalls.Load())
			presentTaskSubmission(c, outcome)
			var extension model.Task
			require.NoError(t, database.Where("task_id = ?", "task_kling_extension").First(&extension).Error)
			service.DispatchPlatformUpdate(t.Context(), extension.Platform, map[int][]string{channel.Id: {extension.GetUpstreamTaskID()}}, map[string]*model.Task{extension.GetUpstreamTaskID(): &extension})
			require.NoError(t, database.First(&extension, extension.ID).Error)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), extension.Status)
			assert.Equal(t, 1, extension.Quota)
		}
		channel.Key, channel.ChannelInfo.IsMultiKey = "sk-test", false
		require.NoError(t, database.Model(&channel).Select("key", "channel_info").Updates(&channel).Error)
	})

	t.Run("concurrent native requests", func(t *testing.T) {
		const taskCount, workers, initialQuota = 128, 16, 1_000_000
		connection, err := database.DB()
		require.NoError(t, err)
		if dialect != common.DatabaseTypeSQLite {
			connection.SetMaxOpenConns(workers)
			connection.SetMaxIdleConns(workers)
		}
		t.Cleanup(func() { connection.SetMaxOpenConns(1); connection.SetMaxIdleConns(1) })
		users := make([]model.User, taskCount)
		for index := range users {
			id := 1000 + index
			users[index] = model.User{Id: id, Username: fmt.Sprintf("native-load-%d", id), AffCode: fmt.Sprintf("native-load-%d", id), Group: "default", Quota: initialQuota}
		}
		require.NoError(t, database.CreateInBatches(users, 32).Error)
		jobs := make(chan int, workers)
		results := make(chan error, taskCount)
		var group sync.WaitGroup
		started := time.Now()
		for range workers {
			group.Go(func() {
				for index := range jobs {
					results <- func() error {
						userID := 1000 + index
						publicID := fmt.Sprintf("task_kling_load_%d", index)
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodPost, "/kling/v1/videos/text2video", bytes.NewBufferString(`{"model_name":"kling-v1","prompt":"a lighthouse"}`))
						c.Request.Header.Set("Content-Type", "application/json")
						c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: submitBinding.Plugin, Route: submitBinding.Route})
						common.SetContextKey(c, constant.ContextKeyUserId, userID)
						common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
						common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
						common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
						common.SetContextKey(c, constant.ContextKeyUserQuota, initialQuota)
						middleware.PrepareTaskPluginRoute()(c)
						if c.IsAborted() {
							return fmt.Errorf("prepare %s: %s", publicID, recorder.Body.String())
						}
						if apiErr := middleware.SetupContextForSelectedChannel(c, &channel, "kling-v1"); apiErr != nil {
							return fmt.Errorf("channel %s: %v", publicID, apiErr)
						}
						info := &relaycommon.RelayInfo{UserId: userID, UserGroup: "default", UsingGroup: "default", UserQuota: initialQuota, TokenGroup: "default", OriginModelName: "kling-v1", TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: c.GetString("task_action"), PublicTaskID: publicID, LockedChannel: &channel}}
						info.IsPlayground, info.ForcePreConsume = true, true
						info.UserSetting.BillingPreference = "wallet_only"
						outcome, taskErr := executeTaskSubmissionWith(c, info, relay.RelayTaskSubmit)
						if taskErr != nil || outcome == nil {
							return fmt.Errorf("submit %s: %v", publicID, taskErr)
						}
						presentTaskSubmission(c, outcome)
						if recorder.Code != http.StatusOK {
							return fmt.Errorf("submit %s: HTTP %d", publicID, recorder.Code)
						}
						var task model.Task
						if err := database.Where("task_id = ?", publicID).First(&task).Error; err != nil {
							return err
						}
						upstreamID := task.GetUpstreamTaskID()
						service.DispatchPlatformUpdate(context.Background(), task.Platform, map[int][]string{channel.Id: {upstreamID}}, map[string]*model.Task{upstreamID: &task})
						queryRecorder := httptest.NewRecorder()
						queryContext, _ := gin.CreateTestContext(queryRecorder)
						queryContext.Request = httptest.NewRequest(http.MethodGet, "/kling/v1/videos/text2video/"+publicID, nil)
						queryContext.Params = gin.Params{{Key: "task_id", Value: publicID}}
						queryContext.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: queryBinding.Plugin, Route: queryBinding.Route})
						common.SetContextKey(queryContext, constant.ContextKeyUserId, userID)
						middleware.PrepareTaskPluginRoute()(queryContext)
						if !queryContext.IsAborted() || queryRecorder.Code != http.StatusOK || !strings.Contains(queryRecorder.Body.String(), `"task_status":"succeed"`) {
							return fmt.Errorf("query %s: HTTP %d %s", publicID, queryRecorder.Code, queryRecorder.Body.String())
						}
						return nil
					}()
				}
			})
		}
		for index := range taskCount {
			jobs <- index
		}
		close(jobs)
		group.Wait()
		close(results)
		for err := range results {
			require.NoError(t, err)
		}
		var storedTasks []model.Task
		require.NoError(t, database.Where("user_id >= ? AND user_id < ?", 1000, 1000+taskCount).Order("user_id").Find(&storedTasks).Error)
		require.Len(t, storedTasks, taskCount)
		require.NoError(t, database.Where("id >= ? AND id < ?", 1000, 1000+taskCount).Order("id").Find(&users).Error)
		for index := range taskCount {
			assert.Equal(t, 1000+index, storedTasks[index].UserId)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), storedTasks[index].Status)
			assert.Equal(t, 1, storedTasks[index].Quota)
			assert.Equal(t, initialQuota-1, users[index].Quota)
			assert.Equal(t, 1, users[index].UsedQuota)
			assert.Equal(t, 1, users[index].RequestCount)
		}
		var storedChannel model.Channel
		require.NoError(t, database.First(&storedChannel, channel.Id).Error)
		assert.EqualValues(t, taskCount+2, storedChannel.UsedQuota)
		assert.EqualValues(t, taskCount+1, submitCalls.Load())
		assert.EqualValues(t, taskCount+2, queryCalls.Load())
		t.Logf("NATIVE_LOAD engine=%s tasks=%d workers=%d elapsed=%s accounting=exact", dialect, taskCount, workers, time.Since(started))
	})
}
