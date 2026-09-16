package jsplugin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 删除前的同步查询必须随调用方取消而结束，不能占用连接一直等待上游。
func TestTaskAdaptorFetchTaskWithContextCancellation(t *testing.T) {
	service.InitHttpClient()
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	plugin, err := pluginruntime.NewRegistry().Register(`
export const meta = {apiVersion:1,key:"cancel-query",name:"Cancel Query",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task"};
export function buildSubmitRequest() { return {url:"https://example.com/submit"}; }
export function parseSubmitResponse() { return {taskId:"upstream-task"}; }
export function buildQueryRequest(ctx) { return {url:ctx.baseUrl+"/tasks/"+ctx.taskId}; }
export function parseTaskResult() { return {status:"QUEUED"}; }
`, pluginruntime.Options{})
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() {
		response, fetchErr := New(plugin).FetchTaskWithContext(ctx, server.URL, "", &model.Task{TaskID: "task"}, "")
		if response != nil {
			_ = response.Body.Close()
		}
		result <- fetchErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("query did not reach the controlled upstream")
	}
	cancel()
	select {
	case err = <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("query did not terminate after cancellation")
	}
}

// 管理请求必须使用上游任务 ID、渠道凭证和受限的 DELETE 方法，并返回有界响应。
func TestTaskAdaptorExecuteTaskAction(t *testing.T) {
	service.InitHttpClient()
	var method, path, authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		authorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"task_id":"upstream-task","action":"cancelled","status":"cancelled"}`)
	}))
	defer server.Close()
	plugin, err := pluginruntime.NewRegistry().Register(`
export const meta = {apiVersion:1,key:"action-test",name:"Action Test",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task"};
export function buildSubmitRequest() { return {url:"https://example.com/submit"}; }
export function parseSubmitResponse() { return {taskId:"upstream-task"}; }
export function buildQueryRequest() { return {url:"https://example.com/query"}; }
export function parseTaskResult() { return {status:"SUCCESS"}; }
export function buildTaskActionRequest(ctx) { return {url:ctx.baseUrl+"/tasks/"+ctx.taskId,method:"DELETE",headers:{Accept:"application/json",Authorization:"Bearer "+ctx.apiKey}}; }
`, pluginruntime.Options{})
	require.NoError(t, err)

	response, err := New(plugin).ExecuteTaskAction(context.Background(), "delete", &model.Task{
		TaskID: "public-task", PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task"},
		Properties: model.Properties{OriginModelName: "model"},
	}, server.URL, "secret", "")
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, http.MethodDelete, method)
	assert.Equal(t, "/tasks/upstream-task", path)
	assert.Equal(t, "Bearer secret", authorization)
	assert.JSONEq(t, `{"task_id":"upstream-task","action":"cancelled","status":"cancelled"}`, string(response.Body))
}

// 官方管理接口允许 200 空响应体，宿主应把它交给插件确认而不是误判为非法 JSON。
func TestTaskAdaptorExecuteTaskActionAllowsEmptyResponse(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	plugin, err := pluginruntime.NewRegistry().Register(`
export const meta = {apiVersion:1,key:"empty-action",name:"Empty Action",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task"};
export function buildSubmitRequest() { return {url:"https://example.com/submit"}; }
export function parseSubmitResponse() { return {taskId:"upstream-task"}; }
export function buildQueryRequest() { return {url:"https://example.com/query"}; }
export function parseTaskResult() { return {status:"SUCCESS"}; }
export function buildTaskActionRequest(ctx) { return {url:ctx.baseUrl+"/tasks/"+ctx.taskId,method:"DELETE",headers:{Authorization:"Bearer "+ctx.apiKey}}; }
export function parseTaskActionResponse(ctx, response) { if (response.body !== null) throw new Error("empty body was not normalized"); return {action:"unknown"}; }
`, pluginruntime.Options{})
	require.NoError(t, err)

	response, err := New(plugin).ExecuteTaskAction(context.Background(), "delete", &model.Task{
		TaskID: "public-task", PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task"},
		Properties: model.Properties{OriginModelName: "model"},
	}, server.URL, "secret", "")
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, "unknown", response.Action)
}

// 宿主必须拒绝插件试图借管理钩子发送非 DELETE 或无凭证请求的行为。
func TestTaskAdaptorExecuteTaskActionRejectsUnsafeDescriptor(t *testing.T) {
	plugin, err := pluginruntime.NewRegistry().Register(`
export const meta = {apiVersion:1,key:"unsafe-action",name:"Unsafe Action",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task"};
export function buildSubmitRequest() { return {url:"https://example.com/submit"}; }
export function parseSubmitResponse() { return {taskId:"upstream-task"}; }
export function buildQueryRequest() { return {url:"https://example.com/query"}; }
export function parseTaskResult() { return {status:"SUCCESS"}; }
export function buildTaskActionRequest() { return {url:"https://example.com/task",method:"POST",credentialless:true}; }
`, pluginruntime.Options{})
	require.NoError(t, err)
	_, err = New(plugin).ExecuteTaskAction(context.Background(), "delete", &model.Task{TaskID: "task"}, "https://example.com", "secret", "")
	require.ErrorContains(t, err, "authenticated JSON request")
}

// 管理请求遇到重定向时必须停在原地址，不能把凭证带到未校验的目标主机。
func TestTaskAdaptorExecuteTaskActionDoesNotFollowRedirect(t *testing.T) {
	service.InitHttpClient()
	targetReached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		targetReached = true
	}))
	defer server.Close()
	plugin, err := pluginruntime.NewRegistry().Register(`
export const meta = {apiVersion:1,key:"redirect-action",name:"Redirect Action",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task"};
export function buildSubmitRequest() { return {url:"https://example.com/submit"}; }
export function parseSubmitResponse() { return {taskId:"upstream-task"}; }
export function buildQueryRequest() { return {url:"https://example.com/query"}; }
export function parseTaskResult() { return {status:"SUCCESS"}; }
export function buildTaskActionRequest(ctx) { return {url:ctx.baseUrl+"/redirect",method:"DELETE",headers:{Authorization:"Bearer "+ctx.apiKey}}; }
`, pluginruntime.Options{})
	require.NoError(t, err)

	response, err := New(plugin).ExecuteTaskAction(context.Background(), "delete", &model.Task{TaskID: "task"}, server.URL, "secret", "")
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusFound, response.StatusCode)
	assert.False(t, targetReached)
}
