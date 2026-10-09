package plugins_test

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
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
		{"doubao-seedance-2-5-260628", []string{"480p", "720p", "1080p"}},
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

func TestDoubaoOpenAIImageTransport(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	model := "doubao-seedream-4-0-250828"

	decode := func(body map[string]any) map[string]any {
		t.Helper()
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_image", "decodeRequest"}, map[string]any{
			"protocol":      "openai_image",
			"operation":     "generate",
			"model":         model,
			"upstreamModel": model,
			"body":          map[string]any{"kind": "json", "value": body},
		})
		require.NoError(t, callErr)
		decoded, ok := value.(map[string]any)
		require.True(t, ok)
		return decoded
	}

	decoded := decode(map[string]any{
		"model":           model,
		"prompt":          "a blue glass bird",
		"size":            "2k",
		"response_format": "b64_json",
	})
	assert.Equal(t, "submit", decoded["kind"])
	assert.Equal(t, "text_to_image", decoded["action"])
	requestBody, ok := decoded["requestBody"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, requestBody, "response_format", "response_format is handled by the host")

	value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"baseUrl":       "https://ark.example",
		"apiKey":        "test-key",
		"model":         model,
		"upstreamModel": model,
		"requestBody":   requestBody,
		"action":        decoded["action"],
	})
	require.NoError(t, err)
	descriptor, ok := value.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://ark.example/api/v3/images/generations", descriptor["url"])
	assert.Equal(t, "text_to_image", descriptor["action"])

	multipartContext := map[string]any{
		"protocol":      "openai_image",
		"operation":     "edit",
		"model":         model,
		"upstreamModel": model,
		"body": map[string]any{
			"kind":   "multipart",
			"fields": map[string]any{"prompt": []string{"make the feathers brighter"}, "size": []string{"1K"}},
			"files":  []any{map[string]any{"field": "image[]", "ref": "request_file:image[]", "mimeType": "image/png"}},
		},
	}
	value, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_image", "decodeRequest"}, multipartContext)
	require.NoError(t, err)
	decoded, ok = value.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image_to_image", decoded["action"])
	requestBody, ok = decoded["requestBody"].(map[string]any)
	require.True(t, ok)
	images, ok := requestBody["image"].([]any)
	require.True(t, ok)
	require.Len(t, images, 1)
	assert.Equal(t, "request_file:image[]", images[0].(map[string]any)["__fileRef"])

	completed := map[string]any{
		"task_id":    "task_image",
		"status":     "SUCCESS",
		"created_at": float64(1700000000),
		"data": map[string]any{
			"data":  []any{map[string]any{"url": "https://cdn.example/image.png", "size": "2048x2048"}},
			"usage": map[string]any{"generated_images": float64(1)},
		},
	}
	value, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_image", "render"}, map[string]any{"model": model}, completed)
	require.NoError(t, err)
	response, ok := value.(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1700000000, response["created"])
	data, ok := response["data"].([]any)
	require.True(t, ok)
	require.Len(t, data, 1)
	assert.Equal(t, "https://cdn.example/image.png", data[0].(map[string]any)["url"])
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
						require.Error(t, err)
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
					require.Error(t, err)
				}
				nativeBody := map[string]any{"model": model, "resolution": tc.resolution, "content": []any{map[string]any{"type": "text", "text": "test"}}}
				_, err = plugin.Engine.CallMember(t.Context(), "native", "createTask", map[string]any{"body": map[string]any{"kind": "json", "value": nativeBody}})
				if tc.valid {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				// 外部模型名是别名时，限制仍以最终上游模型为准。
				driver := map[string]any{"model": "video-alias", "upstreamModel": model, "requestBody": req}
				for _, hook := range []string{"extractUsage", "buildSubmitRequest"} {
					value, callErr := plugin.Engine.Call(t.Context(), hook, driver)
					if !tc.valid {
						require.Error(t, callErr)
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

const (
	doubaoImageRoute = "/doubao/api/v3/images/generations"
	doubaoBaseURL    = "https://ark.cn-beijing.volces.com"
)

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

func newDoubaoPlugin(t *testing.T) (*jsplugin.Registry, *jsplugin.LoadedPlugin) {
	t.Helper()
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	registry := jsplugin.NewRegistry()
	plugin, err := registry.RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	return registry, plugin
}

func decodeDoubaoImage(t *testing.T, registry *jsplugin.Registry, plugin *jsplugin.LoadedPlugin, body map[string]any) (map[string]any, error) {
	t.Helper()
	binding, found := registry.Generation().LookupDeclaredRoute(http.MethodPost, doubaoImageRoute)
	require.True(t, found)
	require.Equal(t, jsplugin.RouteTypeSubmit, binding.Route.Type)
	value, err := plugin.Engine.CallPath(t.Context(), "native", []string{binding.Route.Decode}, map[string]any{
		"path": doubaoImageRoute, "body": map[string]any{"kind": "json", "value": body},
	})
	if err != nil {
		return nil, err
	}
	return alibabaObject(t, value), nil
}

// Runs the production host validation, body conversion and usage extraction
// for one normalized image request without contacting Volcengine.
func submitDoubaoImage(t *testing.T, plugin *jsplugin.LoadedPlugin, action string, request map[string]any) (map[string]any, map[string]any, string) {
	t.Helper()
	modelName := request["model"].(string)
	// RelayTask copies the decode action onto the relay info before the adaptor runs.
	info := &relaycommon.RelayInfo{
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelBaseUrl: doubaoBaseURL, UpstreamModelName: modelName},
		OriginModelName: modelName,
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: "task_public", Action: action},
	}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, doubaoImageRoute, nil)
	c.Set("task_request", request)
	require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
	reader, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	encoded, err := io.ReadAll(reader)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(encoded, &body))
	facts, err := adaptor.ExtractUsageFactsValidated(c, info)
	require.NoError(t, err)
	url, err := adaptor.BuildRequestURL(info)
	require.NoError(t, err)
	return body, alibabaObject(t, facts), url
}

func TestDoubaoImageSubmission(t *testing.T) {
	registry, plugin := newDoubaoPlugin(t)
	reference := "https://cdn.example/reference.png"
	// Every model reports the four facts released with the shared image schema,
	// including those its own profile omits, so expressions saved before the
	// per-model profiles keep their charge.
	noImages := map[string]any{"images_up_to_1_5k": float64(0), "images_above_1_5k": float64(0), "input_images": float64(0), "layer_decomposition": false}
	facts := func(overrides map[string]any) map[string]any {
		merged := map[string]any{}
		maps.Copy(merged, noImages)
		maps.Copy(merged, overrides)
		return merged
	}
	for _, tc := range []struct {
		name       string
		model      string
		body       map[string]any
		wantAction string
		wantBody   map[string]any // upstream fields whose value differs from the request
		wantFacts  map[string]any
	}{
		{"text to image with a preset size", "doubao-seedream-4-0-250828",
			map[string]any{"prompt": "a cat", "size": "2K", "watermark": false, "response_format": "url", "stream": false, "seed": 42, "guidance_scale": 2.5},
			"text_to_image", nil, facts(map[string]any{"images_above_1_5k": float64(1)})},
		{"1K reserves the lower pricing tier", "doubao-seedream-4-0-250828",
			map[string]any{"prompt": "a cat", "size": "1K", "optimize_prompt_options": nil},
			"text_to_image", nil, facts(map[string]any{"images_up_to_1_5k": float64(1)})},
		{"lowercase presets are normalized", "doubao-seedream-4-0-250828",
			map[string]any{"prompt": "a cat", "size": "2k"},
			"text_to_image", map[string]any{"size": "2K"}, facts(map[string]any{"images_above_1_5k": float64(1)})},
		{"pixel sizes are normalized to WxH", "doubao-seedream-4-5-251128",
			map[string]any{"prompt": "a banner", "size": "3750 * 1250"},
			"text_to_image", map[string]any{"size": "3750x1250"}, facts(map[string]any{"images_above_1_5k": float64(1)})},
		{"the 2.61 megapixel boundary belongs to the lower tier", "doubao-seedream-5-0-pro-260628",
			map[string]any{"prompt": "a poster", "size": "1500X1740"},
			"text_to_image", map[string]any{"size": "1500x1740"}, facts(map[string]any{"images_up_to_1_5k": float64(1)})},
		{"5.0 pro 1.5K with fast prompt optimization", "doubao-seedream-5-0-pro-260628",
			map[string]any{"prompt": "a poster", "size": "1.5K", "optimize_prompt_options": map[string]any{"mode": "fast"}, "output_format": "png"},
			"text_to_image", nil, facts(map[string]any{"images_up_to_1_5k": float64(1)})},
		{"group generation estimates max_images and reference images", "doubao-seedream-5-0-lite-260128",
			map[string]any{"prompt": "a brand kit", "image": reference, "size": "2K", "sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": 4}, "output_format": "png", "tools": []any{map[string]any{"type": "web_search"}}},
			"image_to_image", nil, facts(map[string]any{"images_above_1_5k": float64(4), "input_images": float64(1)})},
		{"group generation is capped by reference images", "doubao-seedream-4-5-251128",
			map[string]any{"prompt": "four seasons", "image": []any{reference, "https://cdn.example/second.png"}, "sequential_image_generation": "auto"},
			"image_to_image", nil, facts(map[string]any{"images_above_1_5k": float64(13), "input_images": float64(2)})},
		{"base64 references count as input images", "doubao-seedream-5-0-pro-260628",
			map[string]any{"prompt": "a cat", "image": []any{reference, "data:image/png;base64,iVBORw0KGgo="}, "size": "1K"},
			"image_to_image", nil, facts(map[string]any{"images_up_to_1_5k": float64(1), "input_images": float64(2)})},
		{"transparent background on 5.0 pro", "doubao-seedream-5-0-pro-260628",
			map[string]any{"prompt": "cut out the cat", "image": reference, "background": "transparent", "output_format": "png", "size": "1K"},
			"image_to_image", nil, facts(map[string]any{"images_up_to_1_5k": float64(1), "input_images": float64(1)})},
		{"background is forwarded to other models for upstream to decide", "doubao-seedream-5-0-lite-260128",
			map[string]any{"prompt": "cut out the cat", "background": "transparent", "size": "2K"},
			"text_to_image", nil, facts(map[string]any{"images_above_1_5k": float64(1)})},
		{"layer decomposition with auto size reserves every output at the higher tier", "doubao-seedream-5-0-pro-260628",
			map[string]any{"image": reference, "layer_decomposition": true, "size": "auto"},
			"image_to_image", nil, facts(map[string]any{"images_above_1_5k": float64(17), "input_images": float64(1), "layer_decomposition": true})},
		{"layer decomposition at 1K reserves the lower tier", "doubao-seedream-5-0-pro-260628",
			map[string]any{"image": reference, "layer_decomposition": true, "size": "1k"},
			"image_to_image", map[string]any{"size": "1K"}, facts(map[string]any{"images_up_to_1_5k": float64(17), "input_images": float64(1), "layer_decomposition": true})},
		{"5.0 flash layer decomposition at 1.5K", "doubao-seedream-5-0-flash-260915",
			map[string]any{"image": reference, "layer_decomposition": true, "size": "1.5K", "output_format": "png"},
			"image_to_image", nil, facts(map[string]any{"images_up_to_1_5k": float64(17), "input_images": float64(1), "layer_decomposition": true})},
		{"endpoint ids use the permissive profile", "ep-20260918-seedream",
			map[string]any{"prompt": "a cat", "size": "4K", "sequential_image_generation": "auto"},
			"text_to_image", nil, facts(map[string]any{"images_above_1_5k": float64(15)})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": tc.model}
			maps.Copy(request, tc.body)
			resolved, err := decodeDoubaoImage(t, registry, plugin, request)
			require.NoError(t, err)
			assert.Equal(t, tc.model, resolved["model"])
			assert.Equal(t, tc.wantAction, resolved["action"])
			body, facts, url := submitDoubaoImage(t, plugin, tc.wantAction, resolved["requestBody"].(map[string]any))
			assert.Equal(t, doubaoBaseURL+"/api/v3/images/generations", url)
			assert.Equal(t, tc.model, body["model"])
			assert.NotContains(t, body, "stream")
			for key, expected := range alibabaObject(t, tc.body) {
				if key == "stream" {
					continue
				}
				if override, ok := tc.wantBody[key]; ok {
					expected = override
				}
				assert.Contains(t, body, key)
				assert.Equal(t, expected, body[key], key)
			}
			assert.Equal(t, tc.wantFacts, facts)
		})
	}

	// Legacy per-call pricing multiplies the price by every ratio, so the
	// reservation ratio is only the total requested output count: one 1K image
	// with two reference images reserves price × 1, not price × 1 × 2.
	t.Run("legacy per-call pricing reserves only the total output count", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			model string
			body  map[string]any
			want  float64
		}{
			{"1K image with two reference images", "doubao-seedream-5-0-pro-260628", map[string]any{"prompt": "a cat", "image": []any{reference, "https://cdn.example/second.png"}, "size": "1K"}, 1},
			{"group generation", "doubao-seedream-5-0-lite-260128", map[string]any{"prompt": "a brand kit", "image": reference, "size": "2K", "sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": 4}}, 4},
			{"layer decomposition", "doubao-seedream-5-0-pro-260628", map[string]any{"image": reference, "layer_decomposition": true, "size": "auto"}, 17},
		} {
			request := map[string]any{"model": tc.model}
			maps.Copy(request, tc.body)
			resolved, err := decodeDoubaoImage(t, registry, plugin, request)
			require.NoError(t, err, tc.name)
			ctx := map[string]any{"upstreamModel": tc.model, "model": tc.model, "action": resolved["action"], "requestBody": resolved["requestBody"]}
			ctx["usagePurpose"] = "billing_ratios"
			value, err := plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err, tc.name)
			assert.Equal(t, map[string]any{"image_count": tc.want}, alibabaObject(t, value), tc.name)
			ctx["usagePurpose"] = "facts"
			value, err = plugin.Engine.Call(t.Context(), "extractUsage", ctx)
			require.NoError(t, err, tc.name)
			facts := alibabaObject(t, value)
			assert.NotContains(t, facts, "image_count", tc.name)
			assert.Equal(t, tc.want, facts["images_up_to_1_5k"].(float64)+facts["images_above_1_5k"].(float64), tc.name)
		}
	})

	// Each model's pricing lists only the tiers it can output and layer
	// decomposition only where it is supported.
	t.Run("image models serve Responses and OpenAI Images host protocols", func(t *testing.T) {
		layered := []string{"images_up_to_1_5k", "images_above_1_5k", "input_images", "layer_decomposition"}
		oneTier := []string{"images_above_1_5k", "input_images"}
		for name, usageKeys := range map[string][]string{
			"doubao-seedream-5-0-pro-260628":   layered,
			"doubao-seedream-5-0-flash-260915": layered,
			"doubao-seedream-5-0-lite-260128":  oneTier,
			"doubao-seedream-5-0-260128":       oneTier,
			"doubao-seedream-4-5-251128":       oneTier,
			"doubao-seedream-4-0-250828":       {"images_up_to_1_5k", "images_above_1_5k", "input_images"},
		} {
			_, found := registry.Generation().LookupEndpoint(http.MethodPost, "/v1/responses", name)
			assert.True(t, found, name)
			for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
				binding, found := registry.Generation().LookupEndpoint(http.MethodPost, path, name)
				require.True(t, found, name+" "+path)
				assert.Equal(t, "openai_image", binding.Protocol)
			}
			_, found = registry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", name)
			assert.False(t, found, name)
			schema, examples := plugin.Meta.UsageForModel(name)
			assert.ElementsMatch(t, usageKeys, keysOf(schema), name)
			assert.NotEmpty(t, examples, name)
		}
		_, found := registry.Generation().LookupEndpoint(http.MethodPost, "/v1/videos", "doubao-seedance-2-0-260128")
		assert.True(t, found)
		_, found = registry.Generation().LookupEndpoint(http.MethodPost, "/v1/images/generations", "doubao-seedance-2-0-260128")
		assert.False(t, found)
		schema, _ := plugin.Meta.UsageForModel("doubao-seedance-2-0-260128")
		assert.ElementsMatch(t, []string{"tokens", "resolution", "video_input"}, keysOf(schema))
		schema, examples := plugin.Meta.UsageForModel("doubao-seedance-1-5-pro-251215")
		assert.ElementsMatch(t, []string{"tokens", "resolution", "generate_audio"}, keysOf(schema))
		assert.Equal(t, "boolean", schema["generate_audio"].Type)
		assert.NotEmpty(t, examples)
	})
}

// Capability profiles follow the Ark model list: pricing lists only the
// resolutions each Seedance model offers, reference video input only on
// Seedance 2.x, and audio output only on Seedance 1.5 pro.
func TestDoubaoSeedanceUsageFacts(t *testing.T) {
	_, plugin := newDoubaoPlugin(t)
	const (
		pro10  = "doubao-seedance-1-0-pro-250528"
		pro15  = "doubao-seedance-1-5-pro-251215"
		v20    = "doubao-seedance-2-0-260128"
		fast20 = "doubao-seedance-2-0-fast-260128"
		mini20 = "doubao-seedance-2-0-mini-260615"
		v25    = "doubao-seedance-2-5-260628"
	)
	families := []struct {
		models      []string
		resolutions []string
		fields      []string
	}{
		{[]string{pro10, "doubao-seedance-1-0-lite-t2v", "doubao-seedance-1-0-lite-i2v"}, []string{"480p", "720p", "1080p"}, []string{"resolution", "tokens"}},
		{[]string{pro15}, []string{"480p", "720p", "1080p"}, []string{"generate_audio", "resolution", "tokens"}},
		{[]string{v20}, []string{"480p", "720p", "1080p", "4k"}, []string{"resolution", "tokens", "video_input"}},
		{[]string{fast20, mini20}, []string{"480p", "720p"}, []string{"resolution", "tokens", "video_input"}},
		{[]string{v25}, []string{"480p", "720p", "1080p"}, []string{"resolution", "tokens", "video_input"}},
	}
	profiled := make([]string, 0, len(plugin.Meta.Models))
	for _, family := range families {
		for _, name := range family.models {
			profiled = append(profiled, name)
			t.Run(name, func(t *testing.T) {
				schema, examples := plugin.Meta.UsageForModel(name)
				assert.Equal(t, family.fields, slices.Sorted(maps.Keys(schema)))
				assert.Equal(t, family.resolutions, schema["resolution"].Enum)
				require.NotEmpty(t, examples)
				for _, example := range examples {
					assert.Equal(t, family.fields, slices.Sorted(maps.Keys(example.Facts)), example.Label)
					assert.Contains(t, family.resolutions, example.Facts["resolution"], example.Label)
				}
			})
		}
	}
	videoModels := make([]string, 0, len(profiled))
	for _, name := range plugin.Meta.Models {
		if strings.Contains(name, "seedance") {
			videoModels = append(videoModels, name)
		}
	}
	assert.ElementsMatch(t, videoModels, profiled, "every Seedance model selects a capability profile")

	// A table price saved when every Seedance model declared video_input: its
	// last row is the unconditioned fallback.
	const sharedMatrix = `u("resolution") == "480p" && u("video_input") == "none" ? tier("480p·none", u("tokens") * 2 / 1000000) : ` +
		`u("resolution") == "480p" && u("video_input") == "video" ? tier("480p·video", u("tokens") * 3 / 1000000) : ` +
		`u("resolution") == "720p" && u("video_input") == "none" ? tier("720p·none", u("tokens") * 4 / 1000000) : ` +
		`u("resolution") == "720p" && u("video_input") == "video" ? tier("720p·video", u("tokens") * 5 / 1000000) : ` +
		`u("resolution") == "1080p" && u("video_input") == "none" ? tier("1080p·none", u("tokens") * 6 / 1000000) : ` +
		`u("resolution") == "1080p" && u("video_input") == "video" ? tier("1080p·video", u("tokens") * 7 / 1000000) : ` +
		`u("resolution") == "4k" && u("video_input") == "none" ? tier("4k·none", u("tokens") * 8 / 1000000) : ` +
		`tier("4k·video", u("tokens") * 9 / 1000000)`
	for _, tc := range []struct {
		name    string
		model   string
		request map[string]any
		want    map[string]any
		wantErr string
	}{
		{"1.5 pro defaults to audio", pro15, map[string]any{"metadata": map[string]any{"resolution": "720p"}},
			map[string]any{"tokens": float64(108000), "resolution": "720p", "generate_audio": true, "video_input": "none"}, ""},
		{"1.5 pro silent output", pro15, map[string]any{"metadata": map[string]any{"resolution": "720p", "generate_audio": false}},
			map[string]any{"tokens": float64(108000), "resolution": "720p", "generate_audio": false, "video_input": "none"}, ""},
		{"2.0 has no audio fact", v20, map[string]any{"metadata": map[string]any{"resolution": "720p", "generate_audio": false}},
			map[string]any{"tokens": float64(108000), "resolution": "720p", "video_input": "none"}, ""},
		{"2.0 offers 4k", v20, map[string]any{"metadata": map[string]any{"resolution": "4k"}},
			map[string]any{"tokens": float64(972000), "resolution": "4k", "video_input": "none"}, ""},
		{"1.0 pro reports no reference video although its profile omits it", pro10, map[string]any{"metadata": map[string]any{"resolution": "1080p"}},
			map[string]any{"tokens": float64(243000), "resolution": "1080p", "video_input": "none"}, ""},
		{"mini reserves its highest tier when the resolution is left to Ark", mini20, map[string]any{},
			map[string]any{"tokens": float64(108000), "resolution": "720p", "video_input": "none"}, ""},
		{"mini rejects 1080p", mini20, map[string]any{"metadata": map[string]any{"resolution": "1080p"}}, nil, mini20 + " resolution must be one of 480p, 720p"},
		{"mini rejects a 1080p size", mini20, map[string]any{"size": "1920x1080"}, nil, mini20 + " resolution must be one of 480p, 720p"},
		{"fast rejects 4k", fast20, map[string]any{"metadata": map[string]any{"resolution": "4k"}}, nil, fast20 + " resolution must be one of 480p, 720p"},
		{"2.5 rejects 4k", v25, map[string]any{"metadata": map[string]any{"resolution": "4K"}}, nil, v25 + " resolution must be one of 480p, 720p, 1080p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": tc.model, "prompt": "a cat", "seconds": float64(5)}
			maps.Copy(request, tc.request)
			info := &relaycommon.RelayInfo{
				ChannelMeta:     &relaycommon.ChannelMeta{ChannelBaseUrl: doubaoBaseURL, UpstreamModelName: tc.model},
				OriginModelName: tc.model,
				TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: "task_public", Action: "text_to_video"},
			}
			adaptor := taskplugin.New(plugin)
			adaptor.Init(info)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
			c.Set("task_request", request)
			taskErr := adaptor.ValidateRequestAndSetAction(c, info)
			if tc.wantErr != "" {
				require.NotNil(t, taskErr)
				assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
				assert.Contains(t, taskErr.Message, tc.wantErr)
				return
			}
			require.Nil(t, taskErr)
			reader, err := adaptor.BuildRequestBody(c, info)
			require.NoError(t, err)
			encoded, err := io.ReadAll(reader)
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, common.Unmarshal(encoded, &body))
			metadata, _ := tc.request["metadata"].(map[string]any)
			assert.Equal(t, metadata["generate_audio"], body["generate_audio"])
			facts, err := adaptor.ExtractUsageFactsValidated(c, info)
			require.NoError(t, err)
			assert.Equal(t, tc.want, alibabaObject(t, facts))
			_, trace, err := billingexpr.RunExprWithRequest(sharedMatrix, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: facts})
			require.NoError(t, err)
			assert.Equal(t, tc.want["resolution"].(string)+"·none", trace.MatchedTier, "saved table prices keep the no-video tier")
		})
	}

	// 上游渠道映射成未知 endpoint 时，不能让 2.5 回退到包含 4K 的默认 profile。
	t.Run("mapped 2.5 keeps the declared resolution limit", func(t *testing.T) {
		const model25 = "doubao-seedance-2-5-260628"
		request := map[string]any{
			"model": model25, "prompt": "test", "seconds": float64(5),
			"metadata": map[string]any{"resolution": "4k"},
		}
		ctx := map[string]any{
			"model": model25, "upstreamModel": "provider-seedance-2-5-endpoint",
			"requestBody": request,
		}
		for _, hook := range []string{"buildSubmitRequest", "extractUsage"} {
			_, err := plugin.Engine.Call(t.Context(), hook, ctx)
			require.Error(t, err, hook)
			assert.Contains(t, err.Error(), model25)
		}
		for _, protocol := range []string{"openai_responses", "openai_video"} {
			_, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{protocol, "decodeRequest"}, map[string]any{
				"model": model25, "upstreamModel": "provider-seedance-2-5-endpoint",
				"body": map[string]any{"kind": "json", "value": request},
			})
			require.Error(t, err, protocol)
		}
	})

	t.Run("completion overlays only resolutions the model offers", func(t *testing.T) {
		for _, tc := range []struct {
			model      string
			resolution string
			want       map[string]any
		}{
			{mini20, "1080p", map[string]any{"tokens": float64(90000)}},
			{v25, "4k", map[string]any{"tokens": float64(90000)}},
			{v20, "4k", map[string]any{"tokens": float64(90000), "resolution": "4k"}},
		} {
			queryContext := map[string]any{"model": tc.model, "upstreamModel": tc.model, "action": "text_to_video"}
			body := map[string]any{
				"status":  "succeeded",
				"usage":   map[string]any{"completion_tokens": 90000},
				"content": map[string]any{"video_url": "https://cdn.example/video.mp4", "resolution": tc.resolution},
			}
			value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", queryContext, map[string]any{"status": "SUCCESS"}, body)
			require.NoError(t, err, tc.model)
			assert.Equal(t, tc.want, alibabaObject(t, value), tc.model)
		}
	})
}

func keysOf(schema map[string]jsplugin.UsageFieldSchema) []string {
	keys := make([]string, 0, len(schema))
	for key := range schema {
		keys = append(keys, key)
	}
	return keys
}

// Rejections mirror the Ark API reference; the model-capability cases were
// confirmed against live 400 responses (2026-09).
func TestDoubaoImageValidation(t *testing.T) {
	registry, plugin := newDoubaoPlugin(t)
	const pro, flash, lite, v45, v40 = "doubao-seedream-5-0-pro-260628", "doubao-seedream-5-0-flash-260915", "doubao-seedream-5-0-lite-260128", "doubao-seedream-4-5-251128", "doubao-seedream-4-0-250828"
	reference := "https://cdn.example/1.png"
	manyReferences := make([]any, 0, 11)
	for index := range 11 {
		manyReferences = append(manyReferences, fmt.Sprintf("https://cdn.example/%d.png", index))
	}
	for _, tc := range []struct {
		name    string
		model   string
		body    map[string]any
		wantErr string
	}{
		{"prompt is required", v40, map[string]any{}, "prompt is required"},
		{"client streaming is rejected", v40, map[string]any{"prompt": "a cat", "stream": true}, "stream is not supported"},
		{"base64 responses are not deliverable", v40, map[string]any{"prompt": "a cat", "response_format": "b64_json"}, "response_format must be url"},
		{"5.0 pro rejects group generation", pro, map[string]any{"prompt": "a cat", "sequential_image_generation": "auto"}, "sequential_image_generation is not supported"},
		{"5.0 pro rejects sequential_image_generation even when disabled", pro, map[string]any{"prompt": "a cat", "sequential_image_generation": "disabled"}, "sequential_image_generation is not supported"},
		{"5.0 flash rejects group generation", flash, map[string]any{"prompt": "a cat", "sequential_image_generation": "auto"}, "sequential_image_generation is not supported"},
		{"sequential_image_generation is an enum", lite, map[string]any{"prompt": "a cat", "sequential_image_generation": "on"}, "must be auto or disabled"},
		{"sequential options must be an object", lite, map[string]any{"prompt": "a cat", "sequential_image_generation": "auto", "sequential_image_generation_options": 3}, "sequential_image_generation_options must be an object"},
		{"max_images is bounded", lite, map[string]any{"prompt": "a cat", "sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": 16}}, "max_images must be an integer between 1 and 15"},
		{"reference images are bounded", pro, map[string]any{"prompt": "a cat", "image": manyReferences}, "at most 10 reference images"},
		{"fast prompt optimization is unsupported on 5.0 flash", flash, map[string]any{"prompt": "a cat", "optimize_prompt_options": map[string]any{"mode": "fast"}}, "mode fast is not supported"},
		{"fast prompt optimization is unsupported on 5.0 lite", lite, map[string]any{"prompt": "a cat", "optimize_prompt_options": map[string]any{"mode": "fast"}}, "mode fast is not supported"},
		{"fast prompt optimization is unsupported on 4.5", v45, map[string]any{"prompt": "a cat", "optimize_prompt_options": map[string]any{"mode": "fast"}}, "mode fast is not supported"},
		{"prompt optimization mode is an enum", v40, map[string]any{"prompt": "a cat", "optimize_prompt_options": map[string]any{"mode": "turbo"}}, "must be standard or fast"},
		{"prompt optimization options must be an object", v40, map[string]any{"prompt": "a cat", "optimize_prompt_options": 123}, "optimize_prompt_options must be an object"},
		{"output_format is an enum", pro, map[string]any{"prompt": "a cat", "output_format": "webp"}, "output_format must be png or jpeg"},
		{"output_format follows the model", v40, map[string]any{"prompt": "a cat", "output_format": "png"}, "output_format is not supported"},
		{"background is a string enum", pro, map[string]any{"prompt": "a cat", "image": reference, "background": true}, "background must be opaque or transparent"},
		{"transparent background needs exactly one input image", pro, map[string]any{"prompt": "a cat", "background": "transparent"}, "requires exactly one input image"},
		{"transparent background rejects jpeg output", pro, map[string]any{"prompt": "a cat", "image": reference, "background": "transparent", "output_format": "jpeg"}, "cannot be combined with output_format jpeg"},
		{"tools must be objects with a type", lite, map[string]any{"prompt": "a cat", "tools": []any{map[string]any{}}}, "tools must be an array of objects with a string type"},
		{"tools must be an array", lite, map[string]any{"prompt": "a cat", "tools": map[string]any{"type": "web_search"}}, "tools must be an array of objects with a string type"},
		{"tools follow the model", pro, map[string]any{"prompt": "a cat", "tools": []any{map[string]any{"type": "web_search"}}}, "tools are not supported"},
		{"seed must be an integer", v40, map[string]any{"prompt": "a cat", "seed": "abc"}, "seed must be an integer"},
		{"seed rejects fractions", v40, map[string]any{"prompt": "a cat", "seed": 1.5}, "seed must be an integer"},
		{"guidance_scale must be a number", v40, map[string]any{"prompt": "a cat", "guidance_scale": "x"}, "guidance_scale must be a number"},
		{"size must be a string", v40, map[string]any{"prompt": "a cat", "size": 2048}, "size must be a string"},
		{"presets follow the model", v45, map[string]any{"prompt": "a cat", "size": "1K"}, "size must be one of 2K, 4K"},
		{"5.0 flash tops out at 2K", flash, map[string]any{"prompt": "a cat", "size": "4K"}, "size must be one of 1K, 1.5K, 2K"},
		{"pixel sizes follow the model", v45, map[string]any{"prompt": "a cat", "size": "1500x1500"}, "outside the model's pixel"},
		{"aspect ratios are bounded", v40, map[string]any{"prompt": "a cat", "size": "8192x480"}, "outside the model's pixel"},
		{"layer decomposition needs one image", pro, map[string]any{"layer_decomposition": true}, "requires exactly one input image"},
		{"layer decomposition follows the model", v40, map[string]any{"prompt": "a cat", "image": reference, "layer_decomposition": true}, "layer_decomposition is not supported"},
		{"layer decomposition sizes are presets", pro, map[string]any{"image": reference, "layer_decomposition": true, "size": "2048x2048"}, "layer decomposition sizes must be one of"},
		{"auto size needs layer decomposition", v40, map[string]any{"prompt": "a cat", "size": "auto"}, "size auto is only supported"},
		{"image references must be URLs", v40, map[string]any{"prompt": "a cat", "image": "ftp://cdn.example/1.png"}, "image must be an HTTP URL"},
		{"base64 references need the full data URI", v40, map[string]any{"prompt": "a cat", "image": "data:image/png,iVBORw0KGgo="}, "image must be an HTTP URL"},
		{"base64 formats are lowercase", v40, map[string]any{"prompt": "a cat", "image": "data:image/PNG;base64,iVBORw0KGgo="}, "image must be an HTTP URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": tc.model}
			maps.Copy(request, tc.body)
			_, err := decodeDoubaoImage(t, registry, plugin, request)
			require.ErrorContains(t, err, tc.wantErr)
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
	require.Len(t, plugin.Meta.Routes, 5)
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

// Fixtures mirror real Ark responses captured on 2026-09-18 (signed URLs replaced).
func TestDoubaoImageResults(t *testing.T) {
	_, plugin := newDoubaoPlugin(t)
	const lite, pro = "doubao-seedream-5-0-lite-260128", "doubao-seedream-5-0-pro-260628"
	first, second := "https://ark-content.example/1.jpeg?sig=secret", "https://ark-content.example/2.jpeg?sig=secret"
	// 5.0 lite group of two plus one failed member; Ark echoes the canonical model name.
	groupBody := map[string]any{
		"model":   "doubao-seedream-5-0-260128",
		"created": 1789733151,
		"data": []any{
			map[string]any{"url": first, "size": "2848x1600"},
			map[string]any{"url": second, "size": "2848x1600"},
			map[string]any{"error": map[string]any{"code": "OutputImageSensitiveContentDetected", "message": "blocked"}},
		},
		"usage": map[string]any{"generated_images": 2, "output_tokens": 35600, "total_tokens": 35600},
	}
	// 5.0 pro layer decomposition with size auto: base and first layer above 2.61 MP, second layer below.
	layerBody := map[string]any{
		"model":   pro,
		"created": 1789733460,
		"data": []any{
			map[string]any{"url": "https://ark-content.example/base.jpeg?sig=secret", "size": "2848x1600", "output_format": "jpeg", "z_index": 0},
			map[string]any{"url": "https://ark-content.example/layer1.png?sig=secret", "size": "2848x1600", "output_format": "png", "z_index": 1, "name": "背景场景", "description": "室内木桌背景",
				"bounding_box": map[string]any{"absolute": []any{0, 0, 2848, 1600}, "normalized": []any{0, 0, 1000, 999}}},
			map[string]any{"url": "https://ark-content.example/layer2.png?sig=secret", "size": "1125x956", "output_format": "png", "z_index": 2, "name": "主体橘猫", "description": "坐姿橘猫",
				"bounding_box": map[string]any{"absolute": []any{955, 622, 2080, 1578}, "normalized": []any{335, 389, 730, 986}}},
		},
		"usage": map[string]any{"input_images": 1, "generated_images": 3, "output_tokens": 42316, "total_tokens": 42316},
	}
	// 5.0 pro single 1K image from two reference images.
	referenceBody := map[string]any{
		"model":   pro,
		"created": 1789733390,
		"data":    []any{map[string]any{"url": first, "size": "1424x800", "output_format": "jpeg"}},
		"usage":   map[string]any{"input_images": 2, "generated_images": 1, "output_tokens": 4450, "total_tokens": 4450},
	}

	parseResponse := func(t *testing.T, model string, request map[string]any, payload map[string]any) *relaycommon.TaskInfo {
		t.Helper()
		info := &relaycommon.RelayInfo{OriginModelName: model, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: model, ChannelBaseUrl: doubaoBaseURL}, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_public"}}
		adaptor := taskplugin.New(plugin)
		adaptor.Init(info)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, doubaoImageRoute, nil)
		c.Set("task_request", request)
		encoded, err := common.Marshal(payload)
		require.NoError(t, err)
		parsed, taskErr := adaptor.ParseResponse(c, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(encoded))}, info)
		require.Nil(t, taskErr)
		require.NotNil(t, parsed.Immediate)
		assert.Equal(t, "task_public", parsed.UpstreamTaskID)
		assert.JSONEq(t, string(encoded), string(parsed.TaskData))
		return parsed.Immediate
	}
	groupRequest := map[string]any{"model": lite, "prompt": "a cat", "sequential_image_generation": "auto"}
	queryContext := map[string]any{"upstreamModel": lite, "model": lite, "action": "text_to_image"}
	proQuery := map[string]any{"upstreamModel": pro, "model": pro, "action": "text_to_image"}

	t.Run("a single-tier group settles every delivered image", func(t *testing.T) {
		immediate := parseResponse(t, lite, groupRequest, groupBody)
		assert.Equal(t, "SUCCESS", immediate.Status)
		assert.Equal(t, "100%", immediate.Progress)
		assert.Equal(t, first, immediate.Url)
		assert.Equal(t, map[string]any{"images_above_1_5k": float64(2)}, immediate.UsageFacts)

		value, err := plugin.Engine.CallPath(t.Context(), "native", []string{"imageCreated"}, map[string]any{}, map[string]any{"task_id": "task_public", "status": "SUCCESS", "data": groupBody})
		require.NoError(t, err)
		assert.Equal(t, alibabaObject(t, groupBody), alibabaObject(t, value))

		value, err = plugin.Engine.Call(t.Context(), "extractUsageOnSubmit", queryContext, groupBody)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"image_count": float64(2)}, alibabaObject(t, value), "legacy per-call pricing settles on the delivered count")
	})

	// Legacy per-call pricing multiplies the price by every ratio the plugin
	// returns, so the submit-time settlement carries only the total delivered
	// output count; a mixed-tier result with a reference image must settle 5
	// images, not price × 4 × 1 × 1.
	t.Run("legacy per-call pricing settles the total output count", func(t *testing.T) {
		tieredBody := map[string]any{
			"model": pro,
			"data": []any{
				map[string]any{"url": first, "size": "1024x1024"},
				map[string]any{"url": second, "size": "1024x1024"},
				map[string]any{"url": first, "size": "1024x1024"},
				map[string]any{"url": second, "size": "1024x1024"},
				map[string]any{"url": first, "size": "2848x1600"},
			},
			"usage": map[string]any{"input_images": 1, "generated_images": 5},
		}
		value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", proQuery, map[string]any{"status": "SUCCESS"}, tieredBody)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"images_up_to_1_5k": float64(4), "images_above_1_5k": float64(1), "input_images": float64(1)}, alibabaObject(t, value), "task expressions keep the tiered facts")
		for _, tc := range []struct {
			name string
			body map[string]any
			want float64
		}{
			{"mixed tiers with a reference image", tieredBody, 5},
			{"layer decomposition", layerBody, 3},
			{"single image from two reference images", referenceBody, 1},
		} {
			value, err := plugin.Engine.Call(t.Context(), "extractUsageOnSubmit", queryContext, tc.body)
			require.NoError(t, err, tc.name)
			assert.Equal(t, map[string]any{"image_count": tc.want}, alibabaObject(t, value), tc.name)
		}
		value, err = plugin.Engine.Call(t.Context(), "extractUsageOnSubmit", queryContext, map[string]any{"data": []any{map[string]any{"error": map[string]any{"code": "x"}}}})
		require.NoError(t, err)
		assert.Empty(t, alibabaObject(t, value), "no delivered image leaves the reservation in place")
	})

	t.Run("mixed-tier layers settle per layer with the input image count", func(t *testing.T) {
		request := map[string]any{"model": pro, "image": "https://cdn.example/photo.png", "layer_decomposition": true, "size": "auto"}
		immediate := parseResponse(t, pro, request, layerBody)
		assert.Equal(t, "SUCCESS", immediate.Status)
		assert.Equal(t, map[string]any{"images_up_to_1_5k": float64(1), "images_above_1_5k": float64(2), "input_images": float64(1)}, immediate.UsageFacts)
	})

	t.Run("reference images settle from usage.input_images", func(t *testing.T) {
		request := map[string]any{"model": pro, "prompt": "a cat", "image": []any{"https://cdn.example/a.png", "https://cdn.example/b.png", "https://cdn.example/c.png"}, "size": "1K"}
		immediate := parseResponse(t, pro, request, referenceBody)
		assert.Equal(t, map[string]any{"images_up_to_1_5k": float64(1), "images_above_1_5k": float64(0), "input_images": float64(2)}, immediate.UsageFacts)
	})

	t.Run("invalid completion counts retain the reservation", func(t *testing.T) {
		for _, count := range []any{-1, 0, 1.5, 99, "2", 3} {
			payload := map[string]any{"data": []any{map[string]any{"url": first, "size": "2048x2048"}}, "usage": map[string]any{"generated_images": count}}
			immediate := parseResponse(t, lite, groupRequest, payload)
			assert.Equal(t, "SUCCESS", immediate.Status)
			assert.Nil(t, immediate.UsageFacts, "count %v must not replace the reservation", count)
		}
	})

	t.Run("invalid input image counts keep the estimate while tiers settle", func(t *testing.T) {
		for _, count := range []any{-1, 1.5, 15, "2"} {
			payload := map[string]any{"data": []any{map[string]any{"url": first, "size": "1424x800"}}, "usage": map[string]any{"generated_images": 1, "input_images": count}}
			value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", proQuery, map[string]any{"status": "SUCCESS"}, payload)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"images_up_to_1_5k": float64(1), "images_above_1_5k": float64(0)}, alibabaObject(t, value), "input_images %v", count)
		}
	})

	t.Run("missing sizes keep the estimated tiers", func(t *testing.T) {
		payload := map[string]any{"data": []any{map[string]any{"url": first, "size": "2048x2048"}, map[string]any{"url": second}}, "usage": map[string]any{"generated_images": 2, "input_images": 1}}
		value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", proQuery, map[string]any{"status": "SUCCESS"}, payload)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"input_images": float64(1)}, alibabaObject(t, value))
		// A single-tier model needs no size to know the tier.
		value, err = plugin.Engine.Call(t.Context(), "extractUsageOnComplete", queryContext, map[string]any{"status": "SUCCESS"}, payload)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"images_above_1_5k": float64(2), "input_images": float64(1)}, alibabaObject(t, value))
	})

	t.Run("missing usage settles from image payload sizes", func(t *testing.T) {
		payload := map[string]any{"data": []any{map[string]any{"url": first, "size": "2848x1600"}, map[string]any{"url": second, "size": "1024x1024"}, map[string]any{"error": map[string]any{"code": "x"}}}}
		value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", proQuery, map[string]any{"status": "SUCCESS"}, payload)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"images_up_to_1_5k": float64(1), "images_above_1_5k": float64(1)}, alibabaObject(t, value))
	})

	t.Run("failed and empty results do not complete a task", func(t *testing.T) {
		for _, payload := range []map[string]any{
			{"error": map[string]any{"code": "InvalidParameter", "message": "The parameter `seed` specified in the request is not valid. Request id: 0217", "param": "", "type": ""}},
			{"data": []any{map[string]any{"error": map[string]any{"code": "OutputImageSensitiveContentDetected", "message": "blocked"}}}, "usage": map[string]any{"generated_images": 0}},
			{"data": []any{}},
		} {
			_, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", map[string]any{"upstreamModel": lite, "publicTaskId": "task_public", "requestBody": map[string]any{"model": lite, "prompt": "a cat"}}, map[string]any{"statusCode": 200, "body": payload})
			require.Error(t, err)
		}
	})

	t.Run("artifacts and Responses output use gateway URLs", func(t *testing.T) {
		value, err := plugin.Engine.Call(t.Context(), "listArtifacts", map[string]any{"taskId": "task_public", "status": "SUCCESS", "action": "text_to_image", "data": groupBody})
		require.NoError(t, err)
		encoded, err := common.Marshal(value)
		require.NoError(t, err)
		var artifacts []map[string]any
		require.NoError(t, common.Unmarshal(encoded, &artifacts))
		require.Len(t, artifacts, 2)
		assert.Equal(t, "image-1", artifacts[0]["key"])
		assert.Equal(t, "image", artifacts[0]["type"])
		assert.Equal(t, "image-2", artifacts[1]["key"])

		value, err = plugin.Engine.Call(t.Context(), "buildContentRequest", map[string]any{"artifactKey": "image-2", "action": "text_to_image", "data": groupBody, "clientRequest": map[string]any{"method": "GET"}})
		require.NoError(t, err)
		content := alibabaObject(t, value)
		assert.Equal(t, second, content["url"])
		assert.Equal(t, true, content["credentialless"])

		gateway := map[string]any{
			"model": lite,
			"artifacts": map[string]any{
				"image-1": map[string]any{"key": "image-1", "type": "image", "url": "https://gateway.example/v1/tasks/task_public/artifacts/image-1/content?access=a"},
				"image-2": map[string]any{"key": "image-2", "type": "image", "url": "https://gateway.example/v1/tasks/task_public/artifacts/image-2/content?access=b"},
			},
		}
		task := map[string]any{"task_id": "task_public", "status": "SUCCESS", "progress": "100%", "created_at": 10, "updated_at": 20, "data": groupBody}
		value, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "renderFinal"}, gateway, task)
		require.NoError(t, err)
		machine := relay.NewPluginResponsesMachine("task_public", lite, 10, relay.DefaultPluginProtocolLimits())
		response, err := machine.FinalResponse(value, "SUCCESS")
		require.NoError(t, err)
		text := response["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
		assert.Contains(t, text, "artifacts/image-1/content")
		assert.Contains(t, text, "artifacts/image-2/content")
		assert.NotContains(t, text, "ark-content.example")

		_, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "renderFinal"}, map[string]any{"model": lite}, task)
		require.ErrorContains(t, err, "image artifact is unavailable")
	})
}

func TestDoubaoImageResponsesDecode(t *testing.T) {
	_, plugin := newDoubaoPlugin(t)
	const model = "doubao-seedream-5-0-lite-260128"
	decode := func(t *testing.T, body map[string]any) (map[string]any, error) {
		t.Helper()
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "decodeRequest"}, map[string]any{
			"model": body["model"], "stream": false, "body": map[string]any{"kind": "json", "value": body},
		})
		if err != nil {
			return nil, err
		}
		return alibabaObject(t, value), nil
	}

	resolved, err := decode(t, map[string]any{
		"model": model,
		"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "a poster"},
			map[string]any{"type": "input_image", "image_url": "https://cdn.example/ref.png"},
		}}},
		"size":                                "2K",
		"sequential_image_generation":         "auto",
		"sequential_image_generation_options": map[string]any{"max_images": 3},
		"watermark":                           false,
		// Host-owned background execution flag: must never become Ark's transparency option.
		"background": true,
		// Only Ark's web_search tool is forwarded, stripped to its type; function tools are dropped.
		"tools": []any{
			map[string]any{"type": "web_search", "search_context_size": "low"},
			map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{}},
		},
		"metadata": map[string]any{"ignored": true},
	})
	require.NoError(t, err)
	assert.Equal(t, model, resolved["model"])
	assert.Equal(t, "image_to_image", resolved["action"])
	request := resolved["requestBody"].(map[string]any)
	assert.Equal(t, map[string]any{
		"model": model, "prompt": "a poster", "image": []any{"https://cdn.example/ref.png"},
		"size": "2K", "sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": float64(3)}, "watermark": false,
		"tools": []any{map[string]any{"type": "web_search"}},
	}, request)
	body, facts, url := submitDoubaoImage(t, plugin, "image_to_image", request)
	assert.Equal(t, doubaoBaseURL+"/api/v3/images/generations", url)
	assert.Equal(t, []any{"https://cdn.example/ref.png"}, body["image"])
	assert.NotContains(t, body, "background")
	assert.Equal(t, map[string]any{"images_up_to_1_5k": float64(0), "images_above_1_5k": float64(3), "input_images": float64(1), "layer_decomposition": false}, facts)

	t.Run("function-only tools and background never reach a model without tool support", func(t *testing.T) {
		resolved, err := decode(t, map[string]any{"model": "doubao-seedream-4-0-250828", "input": "a cat", "background": true, "tools": []any{map[string]any{"type": "function", "name": "lookup"}}})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"model": "doubao-seedream-4-0-250828", "prompt": "a cat"}, resolved["requestBody"])
	})

	_, err = decode(t, map[string]any{"model": model, "input": "a cat", "response_format": "b64_json"})
	require.ErrorContains(t, err, "response_format must be url")
}

func decodeDoubaoOpenAIImage(t *testing.T, plugin *jsplugin.LoadedPlugin, operation, model string, body map[string]any) (map[string]any, error) {
	t.Helper()
	path := "/v1/images/generations"
	if operation == "edit" {
		path = "/v1/images/edits"
	}
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_image", "decodeRequest"}, map[string]any{
		"protocol": "openai_image", "operation": operation, "model": model, "path": path, "method": http.MethodPost, "body": body,
	})
	if err != nil {
		return nil, err
	}
	return alibabaObject(t, value), nil
}

// OpenAI Images requests reach Ark with the same body, action and usage facts
// as the equivalent native request. Host-owned and OpenAI-only fields are not
// forwarded, and the facts never gain an image_count.
func TestDoubaoOpenAIImageProtocol(t *testing.T) {
	registry, plugin := newDoubaoPlugin(t)
	const pro, lite, v40 = "doubao-seedream-5-0-pro-260628", "doubao-seedream-5-0-lite-260128", "doubao-seedream-4-0-250828"
	reference := "https://cdn.example/reference.png"
	facts := func(lower, higher, inputs float64, layered bool) map[string]any {
		return map[string]any{"images_up_to_1_5k": lower, "images_above_1_5k": higher, "input_images": inputs, "layer_decomposition": layered}
	}
	for _, tc := range []struct {
		name      string
		model     string
		operation string
		native    map[string]any // the equivalent native route body
		extra     map[string]any // OpenAI fields accepted but not forwarded
		wantFacts map[string]any
	}{
		{"text to image", v40, "generate",
			map[string]any{"prompt": "a cat", "size": "2K", "seed": 42, "watermark": false},
			map[string]any{"n": 1, "response_format": "b64_json", "quality": "high", "user": "u1"}, facts(0, 1, 0, false)},
		{"1K reserves the lower tier", pro, "generate",
			map[string]any{"prompt": "a cat", "size": "1K", "output_format": "png"},
			map[string]any{"response_format": "url", "style": "vivid", "stream": false}, facts(1, 0, 0, false)},
		{"group generation with a reference image", lite, "edit",
			map[string]any{"prompt": "a brand kit", "image": reference, "size": "2K", "sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": 4}, "tools": []any{map[string]any{"type": "web_search"}}},
			map[string]any{"n": nil}, facts(0, 4, 1, false)},
		{"layer decomposition", pro, "edit",
			map[string]any{"image": []any{reference}, "layer_decomposition": true, "size": "auto"},
			nil, facts(0, 17, 1, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nativeBody := map[string]any{"model": tc.model}
			maps.Copy(nativeBody, tc.native)
			native, err := decodeDoubaoImage(t, registry, plugin, nativeBody)
			require.NoError(t, err)
			openaiBody := map[string]any{"model": tc.model}
			maps.Copy(openaiBody, tc.native)
			maps.Copy(openaiBody, tc.extra)
			resolved, err := decodeDoubaoOpenAIImage(t, plugin, tc.operation, tc.model, map[string]any{"kind": "json", "value": openaiBody})
			require.NoError(t, err)
			assert.Equal(t, tc.model, resolved["model"])
			assert.Equal(t, native["action"], resolved["action"])
			assert.Equal(t, native["requestBody"], resolved["requestBody"])
			body, facts, url := submitDoubaoImage(t, plugin, resolved["action"].(string), resolved["requestBody"].(map[string]any))
			assert.Equal(t, doubaoBaseURL+"/api/v3/images/generations", url)
			for key := range tc.extra {
				assert.NotContains(t, body, key)
			}
			assert.Equal(t, tc.wantFacts, facts)
		})
	}

	t.Run("multipart edits send uploads as Base64 data URLs", func(t *testing.T) {
		resolved, err := decodeDoubaoOpenAIImage(t, plugin, "edit", lite, map[string]any{"kind": "multipart",
			"fields": map[string][]string{
				"model": {lite}, "prompt": {"merge the two"}, "image": {reference}, "size": {"2K"}, "n": {"1"}, "response_format": {"b64_json"}, "watermark": {"false"}, "seed": {"7"},
				"sequential_image_generation": {"auto"}, "sequential_image_generation_options": {`{"max_images":2}`},
			},
			"files": []any{
				map[string]any{"ref": "request_file:image[]", "field": "image[]", "filename": "a.png", "mimeType": "image/png", "size": 10},
				map[string]any{"ref": "request_file:image[]#1", "field": "image[]", "filename": "b.jpg", "mimeType": "IMAGE/JPEG", "size": 10},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "image_to_image", resolved["action"])
		images := []any{
			reference,
			map[string]any{"__fileRef": "request_file:image[]", "encoding": "dataUrl", "mimeType": "image/png", "maxBytes": float64(31457280)},
			map[string]any{"__fileRef": "request_file:image[]#1", "encoding": "dataUrl", "mimeType": "image/jpeg", "maxBytes": float64(31457280)},
		}
		assert.Equal(t, map[string]any{
			"model": lite, "prompt": "merge the two", "image": images, "size": "2K", "watermark": false, "seed": float64(7),
			"sequential_image_generation": "auto", "sequential_image_generation_options": map[string]any{"max_images": float64(2)},
		}, resolved["requestBody"])
		driver := map[string]any{"upstreamModel": lite, "model": lite, "action": "image_to_image", "requestBody": resolved["requestBody"], "baseUrl": doubaoBaseURL, "apiKey": "k", "usagePurpose": "facts"}
		value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", driver)
		require.NoError(t, err)
		assert.Equal(t, images, alibabaObject(t, value)["body"].(map[string]any)["image"], "placeholders reach the upstream body for the host to inline")
		value, err = plugin.Engine.Call(t.Context(), "extractUsage", driver)
		require.NoError(t, err)
		assert.Equal(t, facts(0, 2, 3, false), alibabaObject(t, value))
		driver["usagePurpose"] = "billing_ratios"
		value, err = plugin.Engine.Call(t.Context(), "extractUsage", driver)
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"image_count": float64(2)}, alibabaObject(t, value))
	})

	t.Run("requests Ark cannot honor are rejected before billing", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			model     string
			operation string
			body      map[string]any
			wantErr   string
		}{
			{"counts other than one", v40, "generate", map[string]any{"kind": "json", "value": map[string]any{"prompt": "a cat", "n": 2}}, "n must be 1"},
			{"multipart counts other than one", lite, "edit", map[string]any{"kind": "multipart", "fields": map[string][]string{"prompt": {"a cat"}, "image": {reference}, "n": {"4"}}}, "n must be 1"},
			{"streaming", v40, "generate", map[string]any{"kind": "json", "value": map[string]any{"prompt": "a cat", "stream": true}}, "stream is not supported"},
			{"unknown response formats", v40, "generate", map[string]any{"kind": "json", "value": map[string]any{"prompt": "a cat", "response_format": "base64"}}, "response_format must be url or b64_json"},
			{"edits without an image", v40, "edit", map[string]any{"kind": "json", "value": map[string]any{"prompt": "a cat"}}, "image is required"},
			{"masks", v40, "edit", map[string]any{"kind": "json", "value": map[string]any{"prompt": "a cat", "image": reference, "mask": reference}}, "mask is not supported"},
			{"mask uploads", v40, "edit", map[string]any{"kind": "multipart", "fields": map[string][]string{"prompt": {"a cat"}}, "files": []any{
				map[string]any{"ref": "request_file:image", "field": "image", "filename": "a.png", "mimeType": "image/png", "size": 10},
				map[string]any{"ref": "request_file:mask", "field": "mask", "filename": "m.png", "mimeType": "image/png", "size": 10},
			}}, "mask is not supported"},
			{"non-boolean multipart flags", v40, "edit", map[string]any{"kind": "multipart", "fields": map[string][]string{"prompt": {"a cat"}, "image": {reference}, "watermark": {"yes"}}}, "watermark must be true or false"},
			{"model capabilities", pro, "generate", map[string]any{"kind": "json", "value": map[string]any{"prompt": "a cat", "sequential_image_generation": "auto"}}, "sequential_image_generation is not supported"},
		} {
			_, err := decodeDoubaoOpenAIImage(t, plugin, tc.operation, tc.model, tc.body)
			require.ErrorContains(t, err, tc.wantErr, tc.name)
		}
	})

	t.Run("render returns the delivered image URLs", func(t *testing.T) {
		first, second := "https://ark-content.example/1.jpeg?sig=secret", "https://ark-content.example/2.jpeg?sig=secret"
		body := map[string]any{
			"model": lite, "created": 1789733151,
			"data": []any{
				map[string]any{"url": first, "size": "2848x1600"},
				map[string]any{"error": map[string]any{"code": "OutputImageSensitiveContentDetected", "message": "blocked"}},
				map[string]any{"url": second, "size": "2848x1600"},
			},
			"usage": map[string]any{"generated_images": 2},
		}
		value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_image", "render"}, map[string]any{"model": lite}, map[string]any{"task_id": "task_public", "status": "SUCCESS", "created_at": 1789733150, "data": body})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"created": float64(1789733150), "data": []any{map[string]any{"url": first}, map[string]any{"url": second}}, "metadata": map[string]any{"usage": map[string]any{"generated_images": float64(2)}}}, alibabaObject(t, value))
	})
}

// Channel model mapping may send a declared Seedream model to an Ark endpoint ID.
// The declared model still drives capability checks and the image usage profile.
func TestDoubaoImageEndpointMappingKeepsDeclaredModel(t *testing.T) {
	_, plugin := newDoubaoPlugin(t)
	const declared, endpoint = "doubao-seedream-5-0-pro-260628", "ep-20260918-seedream"
	for _, tc := range []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{"declared capabilities still apply", map[string]any{"prompt": "a cat", "sequential_image_generation": "auto"}, "sequential_image_generation is not supported"},
		{"the endpoint receives the request", map[string]any{"prompt": "a cat", "size": "2K"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": declared}
			maps.Copy(request, tc.body)
			info := &relaycommon.RelayInfo{
				ChannelMeta:     &relaycommon.ChannelMeta{ChannelBaseUrl: doubaoBaseURL, UpstreamModelName: endpoint},
				OriginModelName: declared,
				TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: "task_public", Action: "text_to_image"},
			}
			adaptor := taskplugin.New(plugin)
			adaptor.Init(info)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, doubaoImageRoute, nil)
			c.Set("task_request", request)
			taskErr := adaptor.ValidateRequestAndSetAction(c, info)
			if tc.wantErr != "" {
				require.NotNil(t, taskErr)
				assert.Equal(t, "plugin_request_invalid", taskErr.Code)
				assert.Contains(t, taskErr.Message, tc.wantErr)
				return
			}
			require.Nil(t, taskErr)
			reader, err := adaptor.BuildRequestBody(c, info)
			require.NoError(t, err)
			encoded, err := io.ReadAll(reader)
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, common.Unmarshal(encoded, &body))
			assert.Equal(t, endpoint, body["model"])
			facts, err := adaptor.ExtractUsageFactsValidated(c, info)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"images_up_to_1_5k": float64(0), "images_above_1_5k": float64(1), "input_images": float64(0), "layer_decomposition": false}, alibabaObject(t, facts))
		})
	}
}
