package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 管理动态路由允许无请求体，并把解码后的意图交给后续宿主操作处理器。
func TestPrepareTaskPluginManagementRoutesAcceptEmptyBody(t *testing.T) {
	plugin, err := pluginruntime.CompilePlugin(`
export const meta = {apiVersion:1,key:"management-route",name:"Management",version:"1.0.0",author:{name:"Test"},models:["model"],fetchMode:"per_task",routes:[
  {method:"GET",path:"/vendor/tasks",type:"dynamic",action:"list",models:["model"],decode:"decodeList",render:"render"},
  {method:"DELETE",path:"/vendor/tasks/:task_id",type:"dynamic",action:"delete",models:["model"],decode:"decodeDelete",render:"render"}
]};
export const native = {
  decodeList: function(ctx) { return {kind:"query",model:"model",taskIds:[]}; },
  decodeDelete: function(ctx) { return {kind:"delete",model:"model",taskId:ctx.params.task_id}; },
  render: function() { return {}; }
};
export function buildSubmitRequest(){return {url:"https://example.com"}}
export function parseSubmitResponse(){return {taskId:"one"}}
export function buildQueryRequest(){return {url:"https://example.com"}}
export function parseTaskResult(){return {status:"SUCCESS"}}
`, pluginruntime.Options{})
	require.NoError(t, err)

	for index, testCase := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/vendor/tasks"},
		{method: http.MethodDelete, path: "/vendor/tasks/public-task"},
	} {
		t.Run(testCase.method, func(t *testing.T) {
			reached := false
			router := gin.New()
			route := plugin.Meta.Routes[index]
			handler := func(c *gin.Context) {
				reached = true
				_, exists := c.Get(pluginruntime.ContextKeyNativeRouteIntent)
				assert.True(t, exists)
				c.Status(http.StatusNoContent)
			}
			if testCase.method == http.MethodGet {
				router.GET(testCase.path, pinTaskPluginRoute(plugin, index), PrepareTaskPluginRoute(), handler)
			} else {
				router.DELETE(testCase.path, func(c *gin.Context) {
					c.Params = gin.Params{{Key: "task_id", Value: "public-task"}}
					c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Plugin: plugin, Route: route})
					c.Next()
				}, PrepareTaskPluginRoute(), handler)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(testCase.method, testCase.path, nil))
			assert.True(t, reached)
			assert.Equal(t, http.StatusNoContent, recorder.Code)
		})
	}
}

// 查询路由声明模型范围后，旧版同平台但不同模型的任务必须被视为不存在。
func TestTaskPluginRouteModelScope(t *testing.T) {
	route := pluginruntime.Route{Models: []string{"MiniMax-H3"}}
	assert.True(t, taskPluginRouteMatchesTaskModel(route, &model.Task{
		Properties: model.Properties{OriginModelName: "MiniMax-H3"},
	}))
	assert.True(t, taskPluginRouteMatchesTaskModel(route, &model.Task{
		Properties: model.Properties{UpstreamModelName: "MiniMax-H3"},
	}))
	assert.False(t, taskPluginRouteMatchesTaskModel(route, &model.Task{
		Properties: model.Properties{OriginModelName: "MiniMax-Hailuo-2.3"},
	}))
}
