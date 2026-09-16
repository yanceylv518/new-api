package plugins_test

import (
	"strings"
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
					"size": []string{tc.resolution}, "seconds": []string{"5"}, "prompt": []string{"test"},
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
		ctx := map[string]any{"upstreamModel": "doubao-seedance-2-0-260128", "requestBody": map[string]any{"seconds": 5, "size": tc.size, "prompt": "test"}}
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
		value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{"upstreamModel": "doubao-seedance-2-0-260128", "requestBody": map[string]any{"duration": duration, "prompt": "test"}})
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

// 官方不同模型的时长、帧数和服务等级限制必须在所有提交入口保持一致。
func TestDoubaoModelCapabilityValidation(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	textContent := []any{map[string]any{"type": "text", "text": "test"}}
	callNative := func(model string, body map[string]any) error {
		body["model"] = model
		body["content"] = textContent
		_, callErr := plugin.Engine.CallMember(t.Context(), "native", "createTask", map[string]any{"body": map[string]any{"kind": "json", "value": body}})
		return callErr
	}
	for _, tc := range []struct {
		name, model string
		duration    any
		valid       bool
	}{
		{"2.0 minimum", "doubao-seedance-2-0-260128", 4, true},
		{"2.0 rejects 16 seconds", "doubao-seedance-2-0-260128", 16, false},
		{"fast accepts smart duration", "doubao-seedance-2-0-fast-260128", -1, true},
		{"mini rejects short duration", "doubao-seedance-2-0-mini-260615", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := callNative(tc.model, map[string]any{"duration": tc.duration})
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	for _, tc := range []struct {
		name, model string
		body        map[string]any
		valid       bool
		want        string
	}{
		{"2.0 rejects frames", "doubao-seedance-2-0-260128", map[string]any{"frames": 57}, false, "does not support frames"},
		{"2.0 rejects flex", "doubao-seedance-2-0-260128", map[string]any{"service_tier": "flex"}, false, "does not support service_tier"},
		{"2.0 rejects draft", "doubao-seedance-2-0-260128", map[string]any{"draft": true, "resolution": "480p"}, false, "does not support draft tasks"},
		{"invalid ratio", "doubao-seedance-2-0-260128", map[string]any{"ratio": "2:1"}, false, "ratio must be"},
		{"priority upper bound", "doubao-seedance-2-0-260128", map[string]any{"priority": 10}, false, "priority must be"},
		{"zero resolution is not default", "doubao-seedance-2-0-260128", map[string]any{"resolution": 0}, false, "must be a string"},
		{"false ratio is not default", "doubao-seedance-2-0-260128", map[string]any{"ratio": false}, false, "ratio must be a string"},
		{"camera_fixed rejected before upstream", "doubao-seedance-2-0-260128", map[string]any{"camera_fixed": true}, false, "does not support camera_fixed"},
		{"mov rejected before upstream", "doubao-seedance-2-0-260128", map[string]any{"output_format": "mov"}, false, "only supports mp4 output_format"},
		{"mp4 is accepted", "doubao-seedance-2-0-260128", map[string]any{"output_format": "mp4"}, true, ""},
		{"callback syntax", "doubao-seedance-2-0-260128", map[string]any{"callback_url": "not-a-url"}, false, "callback_url must be"},
		{"callback accepted", "doubao-seedance-2-0-260128", map[string]any{"callback_url": "https://callback.example/path?task=test"}, true, ""},
		{"safety identifier wrong type", "doubao-seedance-2-0-260128", map[string]any{"safety_identifier": 123}, false, "safety_identifier must be"},
		{"safety identifier limit", "doubao-seedance-2-0-260128", map[string]any{"safety_identifier": strings.Repeat("a", 64)}, true, ""},
		{"safety identifier overflow", "doubao-seedance-2-0-260128", map[string]any{"safety_identifier": strings.Repeat("a", 65)}, false, "safety_identifier must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := callNative(tc.model, tc.body)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
	draftContent := []any{map[string]any{"type": "draft_task", "draft_task": map[string]any{"id": "draft-public"}}}
	_, err = plugin.Engine.CallMember(t.Context(), "native", "createTask", map[string]any{"body": map[string]any{"kind": "json", "value": map[string]any{
		"model": "doubao-seedance-2-0-260128", "content": draftContent,
	}}})
	require.ErrorContains(t, err, "does not support draft_task")
}

// 内容角色和模型能力的组合错误必须在预扣前被拦截，合法参考场景仍应透传。
func TestDoubaoContentCapabilityValidation(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	call := func(model string, content []any) error {
		_, callErr := plugin.Engine.CallMember(t.Context(), "native", "createTask", map[string]any{"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": model, "duration": 5, "content": content,
		}}})
		return callErr
	}
	textItem := map[string]any{"type": "text", "text": "test"}
	videoItem := map[string]any{"type": "video_url", "role": "reference_video", "video_url": map[string]any{"url": "https://cdn.example/ref.mp4"}}
	audioItem := map[string]any{"type": "audio_url", "role": "reference_audio", "audio_url": map[string]any{"url": "https://cdn.example/ref.mp3"}}
	imageItem := map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": "https://cdn.example/ref.png"}}
	for _, tc := range []struct {
		name, model, want string
		content           []any
		valid             bool
	}{
		{"reference video is valid on 2.0", "doubao-seedance-2-0-260128", "", []any{textItem, videoItem}, true},
		{"audio requires visual input", "doubao-seedance-2-0-260128", "audio input requires", []any{textItem, audioItem}, false},
		{"frame and reference are exclusive", "doubao-seedance-2-0-260128", "cannot be mixed", []any{textItem, map[string]any{"type": "image_url", "role": "first_frame", "image_url": map[string]any{"url": "https://cdn.example/frame.png"}}, imageItem}, false},
		{"unknown image role is rejected", "doubao-seedance-2-0-260128", "image role is invalid", []any{textItem, map[string]any{"type": "image_url", "role": "middle_frame", "image_url": map[string]any{"url": "https://cdn.example/frame.png"}}}, false},
		{"zero image role is rejected", "doubao-seedance-2-0-260128", "role must be a string", []any{textItem, map[string]any{"type": "image_url", "role": 0, "image_url": map[string]any{"url": "https://cdn.example/frame.png"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := call(tc.model, tc.content)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

// OpenAI 兼容解码产生的顶层 content 和官方选项必须完整进入最终上游请求。
func TestDoubaoCompatibilityPreservesOfficialContent(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	content := []any{
		map[string]any{"type": "text", "text": "test"},
		map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": "asset://reference"}},
	}
	decoded, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model":         "doubao-seedance-2-0-260128",
		"upstreamModel": "doubao-seedance-2-0-260128",
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": "doubao-seedance-2-0-260128", "content": content, "duration": 5,
			"resolution": "720p", "generate_audio": true, "priority": 3,
		}},
	})
	require.NoError(t, err)
	intent := decoded.(map[string]any)
	built, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"requestBody": intent["requestBody"], "upstreamModel": "doubao-seedance-2-0-260128",
		"baseUrl": "https://upstream.example", "apiKey": "fixture-key",
	})
	require.NoError(t, err)
	body := built.(map[string]any)["body"].(map[string]any)
	assert.Equal(t, true, body["generate_audio"])
	assert.EqualValues(t, 3, body["priority"])
	assert.Equal(t, content, body["content"])

	// 表单字符串必须转换为官方标量类型，不能把 false/0 丢掉或作为字符串透传。
	decoded, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
		"model": "doubao-seedance-2-0-260128", "upstreamModel": "doubao-seedance-2-0-260128",
		"body": map[string]any{"kind": "multipart", "fields": map[string]any{
			"prompt": []string{"test"}, "duration": []string{"4"}, "resolution": []string{"480p"},
			"generate_audio": []string{"false"}, "seed": []string{"0"}, "priority": []string{"0"},
		}},
	})
	require.NoError(t, err)
	intent = decoded.(map[string]any)
	built, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"requestBody": intent["requestBody"], "upstreamModel": "doubao-seedance-2-0-260128", "baseUrl": "https://upstream.example",
	})
	require.NoError(t, err)
	body = built.(map[string]any)["body"].(map[string]any)
	assert.Equal(t, false, body["generate_audio"])
	assert.EqualValues(t, 0, body["seed"])
	assert.EqualValues(t, 0, body["priority"])

	// 真实上游Mini回显seed不一致时，网关必须仍保留调用者的值，不能用回填掩盖上游差异。
	native, err := plugin.Engine.CallMember(t.Context(), "native", "createTask", map[string]any{
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": "doubao-seedance-2-0-mini-260615", "duration": 4, "resolution": "720p", "seed": 123, "priority": 1,
			"content": []any{map[string]any{"type": "text", "text": "test"}},
		}},
	})
	require.NoError(t, err)
	built, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"requestBody": native.(map[string]any)["requestBody"], "upstreamModel": "doubao-seedance-2-0-oinone", "baseUrl": "https://upstream.example",
	})
	require.NoError(t, err)
	body = built.(map[string]any)["body"].(map[string]any)
	assert.Equal(t, "doubao-seedance-2-0-oinone", body["model"])
	assert.EqualValues(t, 123, body["seed"])
	assert.EqualValues(t, 1, body["priority"])
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
		"output_format": "mov", "omni_reference_task_type": "reference",
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
	assert.Equal(t, "default", submitted["taskData"].(map[string]any)["service_tier"])
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
