package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 插件元数据必须按模型限制价格行和示例，同时保留标准版的高分辨率。
func TestDoubaoResolutionProfiles(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	for _, tc := range []struct {
		model       string
		resolutions []string
	}{
		{"doubao-seedance-2-0-260128", []string{"480p", "720p", "1080p", "4k"}},
		{"doubao-seedance-2-0-fast-260128", []string{"480p", "720p"}},
		{"doubao-seedance-2-0-mini-260615", []string{"480p", "720p"}},
	} {
		t.Run(tc.model, func(t *testing.T) {
			schema, examples := plugin.Meta.UsageForModel(tc.model)
			assert.Equal(t, tc.resolutions, schema["resolution"].Enum)
			require.NotEmpty(t, examples)
			for _, example := range examples {
				assert.Contains(t, tc.resolutions, example.Facts["resolution"])
			}
		})
	}
}

// 真实插件边界覆盖三个入口、最终渠道映射和预扣估算，不请求付费上游。
func TestDoubaoResolutionRequestContract(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	for _, model := range []string{"doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615"} {
		for _, tc := range []struct {
			resolution string
			valid      bool
		}{
			{"", true}, {"480p", true}, {"720p", true}, {"1280x720", true},
			{"1080p", false}, {"4K", false}, {"1920x1080", false}, {"3840*2160", false}, {"invalid", false},
		} {
			t.Run(model+"/"+tc.resolution, func(t *testing.T) {
				req := map[string]any{"model": model, "input": "test", "prompt": "test", "seconds": 5, "metadata": map[string]any{"resolution": tc.resolution}}
				ctx := map[string]any{"model": model, "body": map[string]any{"kind": "json", "value": req}}
				for _, protocol := range []string{"openai_responses", "openai_video"} {
					_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{protocol, "decodeRequest"}, ctx)
					if tc.valid {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, "only supports 480p and 720p")
					}
				}
				// 表单上传与 JSON 共用相同限制，不能通过 size 绕过分辨率校验。
				multipart := map[string]any{"model": model, "body": map[string]any{"kind": "multipart", "fields": map[string]any{
					"size": []string{tc.resolution}, "seconds": []string{"5"},
				}}}
				_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, multipart)
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "only supports 480p and 720p")
				}
				nativeBody := map[string]any{"model": model, "resolution": tc.resolution, "content": []any{map[string]any{"type": "text", "text": "test"}}}
				_, err = plugin.Engine.CallMember(t.Context(), "native", "createTask", map[string]any{"body": map[string]any{"kind": "json", "value": nativeBody}})
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "only supports 480p and 720p")
				}
				// 外部模型名是别名时，限制仍以最终上游模型为准。
				driver := map[string]any{"model": "video-alias", "upstreamModel": model, "requestBody": req}
				for _, hook := range []string{"extractUsage", "buildSubmitRequest"} {
					value, callErr := plugin.Engine.Call(t.Context(), hook, driver)
					if !tc.valid {
						require.ErrorContains(t, callErr, "only supports 480p and 720p")
						continue
					}
					require.NoError(t, callErr)
					if hook == "extractUsage" && tc.resolution == "" {
						facts := value.(map[string]any)
						assert.Equal(t, "720p", facts["resolution"])
						assert.EqualValues(t, 108000, facts["tokens"])
					}
				}
			})
		}
	}
	// 标准版的高分辨率在实际提交和预扣用量中均保留，不能受低分辨率 profile 影响。
	for _, tc := range []struct {
		size, resolution string
		tokens           int
	}{
		{"1920x1080", "1080p", 243000}, {"3840x2160", "4k", 972000},
	} {
		ctx := map[string]any{"upstreamModel": "doubao-seedance-2-0-260128", "requestBody": map[string]any{"seconds": 5, "size": tc.size}}
		built, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		require.NoError(t, callErr)
		assert.Equal(t, tc.resolution, built.(map[string]any)["body"].(map[string]any)["resolution"])
		usage, callErr := plugin.Engine.Call(t.Context(), "extractUsage", ctx)
		require.NoError(t, callErr)
		assert.EqualValues(t, tc.tokens, usage.(map[string]any)["tokens"])
	}
}

func TestDoubaoResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "doubao",
		model:     "doubao-seedance-2-0-260128",
		requestBody: map[string]any{
			"model": "doubao-seedance-2-0-260128",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "a running fox"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/frame.png"},
			}}},
			"seconds": 6,
			"size":    "1920x1080",
		},
		wantAction: "image_to_video",
		wantRequest: map[string]any{
			"model":   "doubao-seedance-2-0-260128",
			"prompt":  "a running fox",
			"images":  []any{"https://cdn.example/frame.png"},
			"seconds": float64(6),
			"metadata": map[string]any{
				"resolution": "1080p",
			},
		},
		wantUsageKeys:  []string{"resolution", "tokens", "video_input"},
		wantVendorName: "doubao",
	})
}

// 错误类型的上游 Token 数必须保留预估，不能把 true/[1] 转成一个 Token 后低额结算。
func TestDoubaoCompletionRejectsCoercedTokens(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	for _, invalid := range []any{true, false, []any{1}, []any{}, " ", 1.5, -1, "Infinity"} {
		body := map[string]any{"status": "succeeded", "usage": map[string]any{"completion_tokens": invalid, "total_tokens": invalid}}
		facts, callErr := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", nil, nil, body)
		require.NoError(t, callErr)
		assert.NotContains(t, facts.(map[string]any), "tokens", "invalid=%#v", invalid)
		parsed, callErr := plugin.Engine.Call(t.Context(), "parseTaskResult", nil, body)
		require.NoError(t, callErr)
		assert.NotContains(t, parsed.(map[string]any), "completionTokens", "invalid=%#v", invalid)
	}
	// 明确零用量必须传递给表达式，不能当成缺失继续按预估收费。
	for _, usage := range []map[string]any{{"completion_tokens": 0}, {"total_tokens": 0}, {"completion_tokens": 0, "total_tokens": 0}} {
		facts, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", nil, nil, map[string]any{"status": "succeeded", "usage": usage})
		require.NoError(t, err)
		assert.EqualValues(t, 0, facts.(map[string]any)["tokens"])
	}
}

// 原生接口必须保留官方请求字段、素材引用和状态，并正确解释空的删除成功响应。
// 兼容入口的 duration 必须实际发往上游，所有入口的隐藏 metadata 数量都要校验。
func TestDoubaoEffectiveDurationAndMetadataBounds(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	for _, duration := range []any{10, "10", -1} {
		value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"upstreamModel": "doubao-seedance-2-0-260128", "requestBody": map[string]any{"duration": duration}})
		require.NoError(t, err)
		want := 10
		if duration == -1 {
			want = -1
		}
		assert.EqualValues(t, want, value.(map[string]any)["body"].(map[string]any)["duration"])
	}
	for _, request := range []map[string]any{
		{"seconds": 0.9}, {"duration": 1e20}, {"seconds": []any{5}},
		{"metadata": map[string]any{"duration": 1e20}}, {"metadata": map[string]any{"frames": 1e20}},
	} {
		for _, hook := range []string{"buildSubmitRequest", "extractUsage"} {
			_, err := plugin.Engine.Call(t.Context(), hook, map[string]any{"upstreamModel": "doubao-seedance-2-0-260128", "requestBody": request})
			require.Error(t, err, "hook=%s request=%v", hook, request)
		}
	}
}

func TestDoubaoNativeContract(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	require.Len(t, plugin.Meta.Routes, 4)
	for _, route := range plugin.Meta.Routes {
		assert.Contains(t, []string{"GET", "POST", "DELETE"}, route.Method)
	}
	// 测试夹具通过真实 JS 边界转换 JSON，覆盖插件实际接受和输出的协议。
	call := func(hook, member string, args ...any) map[string]any {
		t.Helper()
		var value any
		var callErr error
		if member == "" {
			value, callErr = plugin.Engine.Call(t.Context(), hook, args...)
		} else {
			value, callErr = plugin.Engine.CallMember(t.Context(), hook, member, args...)
		}
		require.NoError(t, callErr)
		encoded, encodeErr := common.Marshal(value)
		require.NoError(t, encodeErr)
		var result map[string]any
		require.NoError(t, common.Unmarshal(encoded, &result))
		return result
	}
	request := map[string]any{
		"model": "doubao-seedance-2-5-260628", "duration": -1, "resolution": "720p", "seed": 0,
		"generate_audio": false, "watermark": false, "camera_fixed": false, "return_last_frame": true,
		"output_format": "mov", "service_tier": "flex", "omni_reference_task_type": "reference",
		"content": []any{
			map[string]any{"type": "text", "text": "first instruction"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "asset://private-asset"}, "role": "reference_image"},
			map[string]any{"type": "text", "text": "second instruction"},
		},
	}
	intent := call("native", "createTask", map[string]any{"body": map[string]any{"kind": "json", "value": request}})
	driverContext := map[string]any{"requestBody": intent["requestBody"], "upstreamModel": request["model"], "baseUrl": "https://upstream.example/doubao", "apiKey": "fixture-key"}
	built := call("buildSubmitRequest", "", driverContext)
	requestJSON, err := common.Marshal(request)
	require.NoError(t, err)
	bodyJSON, err := common.Marshal(built["body"])
	require.NoError(t, err)
	assert.JSONEq(t, string(requestJSON), string(bodyJSON))
	assert.Equal(t, "https://upstream.example/doubao/api/v3/contents/generations/tasks", built["url"])
	usage := call("extractUsage", "", driverContext)
	assert.EqualValues(t, 648000, usage["tokens"])
	framesUsage := call("extractUsage", "", map[string]any{"requestBody": map[string]any{"duration": 10, "metadata": map[string]any{"frames": 57, "resolution": "720p"}}})
	assert.EqualValues(t, 51300, framesUsage["tokens"])

	submitted := call("parseSubmitResponse", "", driverContext, map[string]any{"body": map[string]any{"id": "upstream-id"}})
	assert.Equal(t, "flex", submitted["taskData"].(map[string]any)["service_tier"])
	created := call("native", "taskCreated", map[string]any{}, map[string]any{"task_id": "public-id", "data": submitted["taskData"]})
	assert.Equal(t, map[string]any{"id": "public-id"}, created)
	status := call("native", "taskStatus", map[string]any{}, map[string]any{"task_id": "public-id", "status": "FAILURE", "fail_reason": "task cancelled by user", "data": map[string]any{"id": "upstream-id", "status": "queued"}})
	assert.Equal(t, "cancelled", status["status"])
	assert.Equal(t, "public-id", status["id"])
	result := call("parseTaskResult", "", map[string]any{}, map[string]any{"status": "cancelled"})
	assert.Equal(t, "task cancelled by user", result["reason"])

	list := call("native", "listTasks", map[string]any{"body": map[string]any{"kind": "none"}, "query": map[string]any{"page_num": []string{"500"}, "page_size": []string{"500"}, "filter.model": []string{"ep-example"}, "filter.service_tier": []string{"flex"}, "filter.task_ids": []string{"public-id"}}})
	assert.Equal(t, "ep-example", list["model"])
	assert.Equal(t, map[string]any{"pageNum": float64(500), "pageSize": float64(500), "serviceTier": "flex", "lookbackSeconds": float64(604800)}, list["listOptions"])
	for _, query := range []map[string]any{{"page_size": []string{"501"}}, {"page_num": []string{"1", "2"}}, {"filter.status": []string{"invalid"}}, {"filter.service_tier": []string{"invalid"}}} {
		_, err = plugin.Engine.CallMember(t.Context(), "native", "listTasks", map[string]any{"body": map[string]any{"kind": "none"}, "query": query})
		require.Error(t, err)
	}
	for _, duration := range []any{0, 1, 31, 4.5, "5", 1e20} {
		_, err = plugin.Engine.CallMember(t.Context(), "native", "createTask", map[string]any{"body": map[string]any{"kind": "json", "value": map[string]any{"model": request["model"], "content": request["content"], "duration": duration}}})
		require.Error(t, err)
	}
	deleteRequest := call("buildTaskActionRequest", "", map[string]any{"operation": "delete", "taskId": "upstream/id", "baseUrl": "https://upstream.example/doubao", "apiKey": "fixture-key"})
	assert.Equal(t, "DELETE", deleteRequest["method"])
	assert.Equal(t, "https://upstream.example/doubao/api/v3/contents/generations/tasks/upstream%2Fid", deleteRequest["url"])
	for state, action := range map[string]string{"QUEUED": "unknown", "IN_PROGRESS": "unknown", "SUCCESS": "deleted", "FAILURE": "deleted"} {
		parsed := call("parseTaskActionResponse", "", map[string]any{"status": state}, map[string]any{"statusCode": 200, "body": map[string]any{}})
		assert.Equal(t, action, parsed["action"])
	}
	_, err = plugin.Engine.Call(t.Context(), "parseTaskActionResponse", map[string]any{}, map[string]any{"statusCode": 200, "body": map[string]any{"error": "not success"}})
	require.Error(t, err)
}
