package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupNativeActionTestDB(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousRedis, previousBatch := common.RedisEnabled, common.BatchUpdateEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Channel{}, &model.Task{}, &model.Token{}, &model.Log{}))
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB = database
	model.LOG_DB = database
	common.MemoryCacheEnabled = false
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.RedisEnabled, common.BatchUpdateEnabled = previousRedis, previousBatch
		assert.NoError(t, sqlDB.Close())
	})
}

// 两个取消请求或轮询线程持有同一旧快照时，只能退款一次且已完成任务不可退款。
func TestRelayTaskPluginNativeActionCancellationRefundOnce(t *testing.T) {
	setupNativeActionTestDB(t)
	user := &model.User{Username: "cancel-test", Quota: 900, UsedQuota: 100}
	require.NoError(t, model.DB.Create(user).Error)
	channel := &model.Channel{UsedQuota: 100}
	require.NoError(t, model.DB.Create(channel).Error)
	token := &model.Token{UserId: user.Id, Key: "cancel-test-token", RemainQuota: 900, UsedQuota: 100}
	require.NoError(t, model.DB.Create(token).Error)
	task := &model.Task{TaskID: "cancel-refund", UserId: user.Id, ChannelId: channel.Id, Quota: 100,
		Status: model.TaskStatusQueued, PrivateData: model.TaskPrivateData{TokenId: token.Id}}
	require.NoError(t, model.DB.Create(task).Error)
	stale := *task
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodDelete, "/tasks/cancel-refund", nil)
	require.True(t, markTaskPluginTaskCancelled(c, task))
	require.True(t, markTaskPluginTaskCancelled(c, &stale))
	completed := &model.Task{TaskID: "completed-task", UserId: user.Id, ChannelId: channel.Id, Status: model.TaskStatusSuccess, Quota: 50}
	require.NoError(t, model.DB.Create(completed).Error)
	require.True(t, markTaskPluginTaskCancelled(c, completed))
	assert.Equal(t, 50, completed.Quota)

	require.NoError(t, model.DB.First(user, user.Id).Error)
	require.NoError(t, model.DB.First(channel, channel.Id).Error)
	require.NoError(t, model.DB.First(token, token.Id).Error)
	assert.Equal(t, 1000, user.Quota)
	assert.Zero(t, user.UsedQuota)
	assert.Zero(t, channel.UsedQuota)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	var refunds int64
	require.NoError(t, model.DB.Model(&model.Log{}).Where("type = ?", model.LogTypeRefund).Count(&refunds).Error)
	assert.EqualValues(t, 1, refunds)
	assert.Zero(t, stale.Quota)
	assert.Equal(t, model.TaskCancelledReason, stale.FailReason)
}

// 资金退款失败时必须保留额度并恢复轮询资格，修复资金来源后才能完成取消结算。
func TestRelayTaskPluginNativeActionCancellationRefundFailure(t *testing.T) {
	setupNativeActionTestDB(t)
	user := &model.User{Username: "retry-cancel", Quota: 900, UsedQuota: 100}
	require.NoError(t, model.DB.Create(user).Error)
	task := &model.Task{TaskID: "retry-cancel", UserId: user.Id, Status: model.TaskStatusQueued, Quota: 100}
	require.NoError(t, model.DB.Create(task).Error)
	const callback = "test:reject-cancel-funding"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("funding unavailable"))
		}
	}))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodDelete, "/tasks/retry-cancel", nil)
	assert.False(t, markTaskPluginTaskCancelled(c, task))
	require.NoError(t, model.DB.Callback().Update().Remove(callback))
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), persisted.Status)
	assert.Equal(t, 100, persisted.Quota)
	require.True(t, markTaskPluginTaskCancelled(c, &persisted))
	require.NoError(t, model.DB.First(user, user.Id).Error)
	assert.Equal(t, 1000, user.Quota)
}

func nativeActionTestPlugin(t *testing.T, source string) *pluginruntime.LoadedPlugin {
	t.Helper()
	plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	return plugin
}

const nativeActionPluginSource = `
export const meta = {apiVersion:1,key:"native-action",name:"Native Action",version:"1.0.0",author:{name:"Test"},channelTypes:[35],models:["MiniMax-H3"],fetchMode:"per_task",usageSchema:{seconds:{type:"number",unit:"second"}}};
export const native = {
  listResult: function(ctx,result) { return {total:result.total, ids:result.items.map(function(item){return item.task_id;})}; },
  deleteResult: function(ctx,result) { return {task_id:result.task_id,action:result.action,status:result.status}; },
  error: function(ctx,error) { return {error:{code:error.code,message:error.message}}; }
};
export function buildSubmitRequest() { return {url:"https://example.com/submit"}; }
export function parseSubmitResponse() { return {taskId:"upstream"}; }
export function buildQueryRequest(ctx) { return {url:ctx.baseUrl+"/query/"+ctx.taskId,headers:{Authorization:"Bearer "+ctx.apiKey}}; }
export function parseTaskResult(ctx,body) { return {status:body.status}; }
export function extractUsageOnComplete(task,result,body) { return body.usage || null; }
export function buildTaskActionRequest(ctx) { return {url:ctx.baseUrl+"/v2/video_generation/"+ctx.taskId,method:"DELETE",headers:{Authorization:"Bearer "+ctx.apiKey}}; }
`

// 列表管理路由必须按用户、模型和分页读取任务，并把结果交给插件包络渲染。
func TestRelayTaskPluginNativeActionListsOwnedTasks(t *testing.T) {
	setupNativeActionTestDB(t)
	plugin := nativeActionTestPlugin(t, nativeActionPluginSource)
	for _, task := range []*model.Task{
		{TaskID: "h3-old", UserId: 7, Platform: "native-action", Action: "text_to_video", Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "MiniMax-H3"}},
		{TaskID: "h3-new", UserId: 7, Platform: "35", Action: "context_ir", Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "MiniMax-H3"}},
		{TaskID: "other-user", UserId: 8, Platform: "native-action", Action: "text_to_video", Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "MiniMax-H3"}},
	} {
		require.NoError(t, model.DB.Create(task).Error)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/hailuo/v2/query/video_generation?page_num=1&page_size=1", nil)
	common.SetContextKey(c, constant.ContextKeyUserId, 7)
	c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: &pluginruntime.RoutingGeneration{},
		Plugin:     plugin,
		Route:      pluginruntime.Route{Action: "list", Render: "listResult"},
	})
	c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{
		Path: "/hailuo/v2/query/video_generation", Method: http.MethodGet,
		Query: map[string][]string{"page_num": {"1"}, "page_size": {"1"}},
	})
	c.Set(pluginruntime.ContextKeyNativeRouteIntent, map[string]any{"kind": "query", "model": "MiniMax-H3", "taskIds": []any{}})

	RelayTaskPluginNativeAction(c)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"total":2,"ids":["h3-new"]}`, recorder.Body.String())
}

// 删除必须使用渠道绑定的上游任务 ID，并在确认取消后关闭本地任务的轮询计费生命周期。
func TestRelayTaskPluginNativeActionDeletesOwnedTask(t *testing.T) {
	setupNativeActionTestDB(t)
	service.InitHttpClient()
	plugin := nativeActionTestPlugin(t, nativeActionPluginSource)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"status":"QUEUED"}`))
			return
		}
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/v2/video_generation/upstream-task", r.URL.Path)
		assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"task_id":"upstream-task","action":"cancelled","status":"cancelled"}`))
	}))
	defer upstream.Close()
	channel := &model.Channel{Type: 35, Key: "secret", Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL}
	require.NoError(t, model.DB.Create(channel).Error)
	task := &model.Task{
		TaskID: "public-task", UserId: 7, Platform: "native-action", ChannelId: channel.Id,
		Action: "text_to_video", Status: model.TaskStatusQueued, Quota: 0,
		Properties:  model.Properties{OriginModelName: "MiniMax-H3"},
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task"},
	}
	require.NoError(t, model.DB.Create(task).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodDelete, "/hailuo/v2/video_generation/public-task", nil)
	common.SetContextKey(c, constant.ContextKeyUserId, 7)
	c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
		Generation: &pluginruntime.RoutingGeneration{},
		Plugin:     plugin,
		Route:      pluginruntime.Route{Action: "delete", Render: "deleteResult"},
	})
	c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{
		Path: "/hailuo/v2/video_generation/public-task", Method: http.MethodDelete,
		Params: map[string]string{"task_id": "public-task"}, Query: map[string][]string{},
	})
	c.Set(pluginruntime.ContextKeyNativeRouteIntent, map[string]any{"kind": "delete", "taskId": "public-task"})

	RelayTaskPluginNativeAction(c)

	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"task_id":"public-task","action":"cancelled","status":"cancelled"}`, recorder.Body.String())
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), persisted.Status)
	assert.Equal(t, model.TaskCancelledReason, persisted.FailReason)
}

// 真实管理链必须保留上游业务错误，同时隐藏密钥、私有任务标识和非 JSON 页面；拒绝取消不得退款。
func TestRelayTaskPluginNativeActionPreservesUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, message, requestID string
		status                               int
	}{
		{name: "running task", status: 400, body: `{"type":"error","error":{"type":"task_running","message":"Running tasks cannot be cancelled","http_code":"400"}}`, code: "task_running", message: "Running tasks cannot be cancelled", requestID: "upstream-request-1"},
		{name: "upstream server error", status: 503, body: `{"error":{"code":"overloaded","message":"Please retry later"}}`, code: "overloaded", message: "Please retry later"},
		{name: "Ark dotted code", status: 400, body: `{"error":{"code":"InvalidParameter.TaskStatus","message":"Running tasks cannot be cancelled"}}`, code: "InvalidParameter.TaskStatus", message: "Running tasks cannot be cancelled"},
		{name: "numeric provider code", status: 403, body: `{"base_resp":{"status_code":1004,"status_msg":"Permission denied"},"request_id":"body-request"}`, code: "1004", message: "Permission denied", requestID: "body-request"},
		{name: "private identifiers", status: 400, body: `{"error":{"code":"task_running","message":"task upstream-task api-key-test Bearer client-key-test\r\nhttp://user:password@example.com/?token=private","request_id":"api-key-test"}}`, code: "task_running", message: "task public-task [REDACTED] [REDACTED]  http://***.com/?token=***", requestID: "[REDACTED]"},
		{name: "HTML gateway error", status: 502, body: `<html>api-key-test backend stack trace</html>`, code: "task_action_failed", message: "Task action failed"},
		{name: "invalid code type", status: 400, body: `{"error":{"code":{"secret":"api-key-test"},"type":"invalid_state","message":"Cannot cancel"}}`, code: "invalid_state", message: "Cannot cancel"},
		{name: "oversized response", status: 502, body: `{"error":{"message":"` + strings.Repeat("x", 64*1024) + `"}}`, code: "task_action_failed", message: "Task action failed"},
		{name: "redact before truncating", status: 400, body: `{"error":{"code":"invalid_state","message":"` + strings.Repeat("界", 1020) + `api-key-test"}}`, code: "invalid_state", message: strings.Repeat("界", 1020) + "[RED…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupNativeActionTestDB(t)
			service.InitHttpClient()
			plugin := nativeActionTestPlugin(t, nativeActionPluginSource)
			deletes := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"status":"IN_PROGRESS"}`))
					return
				}
				assert.Equal(t, http.MethodDelete, r.Method)
				assert.Equal(t, "/v2/video_generation/upstream-task", r.URL.Path)
				deletes++
				if tc.name == "running task" {
					w.Header().Set("X-Request-Id", tc.requestID)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			channel := &model.Channel{Type: 35, Key: "api-key-test", Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL}
			require.NoError(t, model.DB.Create(channel).Error)
			task := &model.Task{
				TaskID: "public-task", UserId: 7, Platform: "native-action", ChannelId: channel.Id,
				Action: "text_to_video", Status: model.TaskStatusInProgress, Quota: 100,
				Properties: model.Properties{OriginModelName: "MiniMax-H3"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task"},
			}
			require.NoError(t, model.DB.Create(task).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodDelete, "/hailuo/v2/video_generation/public-task", nil)
			c.Request.Header.Set("Authorization", "Bearer client-key-test")
			common.SetContextKey(c, constant.ContextKeyUserId, 7)
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: &pluginruntime.RoutingGeneration{}, Plugin: plugin, Route: pluginruntime.Route{Action: "delete", Render: "deleteResult"}})
			c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Path: c.Request.URL.Path, Method: http.MethodDelete})
			c.Set(pluginruntime.ContextKeyNativeRouteIntent, map[string]any{"kind": "delete", "taskId": task.TaskID})

			RelayTaskPluginNativeAction(c)

			require.Equal(t, tc.status, recorder.Code)
			var payload struct {
				Error struct{ Code, Message string }
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Equal(t, tc.code, payload.Error.Code)
			assert.Equal(t, tc.message, payload.Error.Message)
			assert.Equal(t, tc.requestID, recorder.Header().Get("X-Upstream-Request-Id"))
			assert.NotContains(t, recorder.Body.String(), "api-key-test")
			assert.NotContains(t, recorder.Body.String(), "client-key-test")
			assert.NotContains(t, recorder.Body.String(), "upstream-task")
			assert.Equal(t, 1, deletes)
			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.Equal(t, model.TaskStatus(model.TaskStatusInProgress), persisted.Status)
			assert.Equal(t, 100, persisted.Quota)
		})
	}
}

// 方舟的空成功响应必须通过二次查询区分取消和删除，404 不能触发失败退款。
func TestDoubaoNativeDeletionConfirmation(t *testing.T) {
	for _, scenario := range []string{"cancelled", "completed", "deleted_during_completion", "confirmation_unavailable", "running_rejected"} {
		t.Run(scenario, func(t *testing.T) {
			setupNativeActionTestDB(t)
			service.InitHttpClient()
			source, err := builtinplugins.Source("doubao")
			require.NoError(t, err)
			plugin := nativeActionTestPlugin(t, source)
			queries, deletes := 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v3/contents/generations/tasks/upstream-id", r.URL.Path)
				if r.Method == http.MethodDelete {
					deletes++
					if scenario == "running_rejected" {
						w.WriteHeader(400)
						_, _ = w.Write([]byte(`{"error":{"code":"InvalidTaskState","message":"Running tasks cannot be cancelled"}}`))
						return
					}
					_, _ = w.Write([]byte(`{}`))
					return
				}
				queries++
				if deletes == 0 {
					_, _ = w.Write([]byte(`{"id":"upstream-id","status":"queued","service_tier":"default"}`))
				} else if scenario == "deleted_during_completion" {
					w.WriteHeader(404)
					_, _ = w.Write([]byte(`{"error":{"code":"TaskNotFound","message":"Task was deleted"}}`))
				} else if scenario == "confirmation_unavailable" {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"error":{"code":"ServiceUnavailable","message":"Retry later"}}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"upstream-id","status":"cancelled"}`))
				}
			}))
			defer upstream.Close()
			user := &model.User{Username: "doubao-cancel", Quota: 900, UsedQuota: 100}
			require.NoError(t, model.DB.Create(user).Error)
			channel := &model.Channel{Type: 54, Key: "fixture-key", Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL, UsedQuota: 100}
			require.NoError(t, model.DB.Create(channel).Error)
			task := &model.Task{TaskID: "public-id", UserId: user.Id, ChannelId: channel.Id, Platform: "doubao", Status: model.TaskStatusQueued, Quota: 100,
				Properties: model.Properties{OriginModelName: "doubao-seedance-2-0-260128"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-id"}}
			if scenario == "completed" {
				task.Status = model.TaskStatusSuccess
			}
			require.NoError(t, model.DB.Create(task).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodDelete, "/doubao/api/v3/contents/generations/tasks/public-id", nil)
			common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: &pluginruntime.RoutingGeneration{}, Plugin: plugin, Route: pluginruntime.Route{Action: "delete", Render: "taskDeleted"}})
			c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Method: http.MethodDelete, Path: c.Request.URL.Path})
			c.Set(pluginruntime.ContextKeyNativeRouteIntent, map[string]any{"kind": "delete", "taskId": task.TaskID})

			RelayTaskPluginNativeAction(c)

			assert.Equal(t, 1, deletes)
			require.NoError(t, model.DB.First(user, user.Id).Error)
			require.NoError(t, model.DB.First(task, task.ID).Error)
			if scenario == "cancelled" {
				assert.Equal(t, 200, recorder.Code)
				assert.JSONEq(t, `{}`, recorder.Body.String())
				assert.Equal(t, 2, queries)
				assert.Equal(t, 1000, user.Quota)
				assert.Zero(t, task.Quota)
				assert.Equal(t, model.TaskCancelledReason, task.FailReason)
			} else {
				assert.Equal(t, 900, user.Quota)
				assert.Equal(t, 100, task.Quota)
				switch scenario {
				case "completed":
					assert.Equal(t, 200, recorder.Code)
					assert.JSONEq(t, `{}`, recorder.Body.String())
					assert.Zero(t, queries)
				case "running_rejected":
					assert.Equal(t, 400, recorder.Code)
					assert.Contains(t, recorder.Body.String(), "InvalidTaskState")
				default:
					assert.Equal(t, 503, recorder.Code)
				}
			}
		})
	}
}

// 上游已成功但本地仍排队时，删除前必须按实际用量结算；查询失败则禁止删除。
func TestRelayTaskPluginNativeActionSynchronizesBeforeDelete(t *testing.T) {
	for _, scenario := range []string{"completed", "query_failed", "completed_during_delete"} {
		t.Run(scenario, func(t *testing.T) {
			queryFails := scenario == "query_failed"
			setupNativeActionTestDB(t)
			service.InitHttpClient()
			plugin := nativeActionTestPlugin(t, nativeActionPluginSource)
			deletes := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if queryFails {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"error":"temporarily unavailable"}`))
						return
					}
					if scenario == "completed_during_delete" {
						_, _ = w.Write([]byte(`{"status":"QUEUED"}`))
						return
					}
					_, _ = w.Write([]byte(`{"status":"SUCCESS","usage":{"seconds":5}}`))
					return
				}
				deletes++
				_, _ = w.Write([]byte(`{"task_id":"upstream-task","action":"deleted","status":"deleted"}`))
			}))
			defer upstream.Close()
			user := &model.User{Username: "delete-completed", Quota: 9000, UsedQuota: 1000}
			require.NoError(t, model.DB.Create(user).Error)
			channel := &model.Channel{Type: 35, Key: "secret", Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL, UsedQuota: 1000}
			require.NoError(t, model.DB.Create(channel).Error)
			task := &model.Task{
				TaskID: "public-task", UserId: user.Id, Platform: "native-action", ChannelId: channel.Id,
				Action: "text_to_video", Status: model.TaskStatusQueued, Quota: 1000,
				Properties: model.Properties{OriginModelName: "MiniMax-H3"},
				PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task", BillingContext: &model.TaskBillingContext{
					TieredSnapshot: &billingexpr.BillingSnapshot{
						TaskUsageBilling: true, ExprString: `u("seconds") * 0.0002`, ExprVersion: 1, GroupRatio: 1, QuotaPerUnit: 500000,
						UsageFacts: map[string]any{"seconds": float64(10)},
					},
				}},
			}
			require.NoError(t, model.DB.Create(task).Error)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodDelete, "/hailuo/v2/video_generation/public-task", nil)
			common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{
				Generation: &pluginruntime.RoutingGeneration{}, Plugin: plugin,
				Route: pluginruntime.Route{Action: "delete", Render: "deleteResult"},
			})
			c.Set(pluginruntime.ContextKeyRouteRequest, pluginruntime.RouteRequestContext{Method: http.MethodDelete})
			c.Set(pluginruntime.ContextKeyNativeRouteIntent, map[string]any{"kind": "delete", "taskId": "public-task"})
			RelayTaskPluginNativeAction(c)
			require.NoError(t, model.DB.First(user, user.Id).Error)
			require.NoError(t, model.DB.First(task, task.ID).Error)
			if queryFails {
				assert.Equal(t, http.StatusBadGateway, recorder.Code)
				assert.Zero(t, deletes)
				assert.Equal(t, 9000, user.Quota)
				assert.Equal(t, 1000, task.Quota)
				return
			}
			if scenario == "completed_during_delete" {
				assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
				assert.Equal(t, 1, deletes)
				assert.Equal(t, 9000, user.Quota)
				assert.Equal(t, 1000, task.Quota)
				assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), task.Status)
				assert.Contains(t, task.FailReason, "reserved quota retained")
				return
			}
			assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			assert.Equal(t, 1, deletes)
			assert.Equal(t, 9500, user.Quota)
			assert.Equal(t, 500, task.Quota)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
		})
	}
}
