package plugins_test

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestViduResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "vidu",
		model:     "viduq2",
		requestBody: map[string]any{
			"model": "viduq2",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "move between frames"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/first.png"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/last.png"},
			}}},
			"seconds": 8,
			"size":    "720p",
		},
		wantAction: "first_tail_to_video",
		wantRequest: map[string]any{
			"model":    "viduq2",
			"prompt":   "move between frames",
			"images":   []any{"https://cdn.example/first.png", "https://cdn.example/last.png"},
			"duration": float64(8),
			"size":     "720p",
		},
		wantUsageKeys:       []string{"duration", "resolution"},
		wantSubmitUsageKeys: []string{"duration", "resolution"},
		wantVendorName:      "vidu",
	})
}

func loadViduPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("vidu")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "vidu"})
	require.NoError(t, err)
	return plugin
}

func callViduNative(t *testing.T, plugin *jsplugin.LoadedPlugin, name string, ctx map[string]any, args ...any) any {
	t.Helper()
	value, err := plugin.Engine.CallPath(t.Context(), "native", []string{name}, append([]any{ctx}, args...)...)
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var decoded any
	require.NoError(t, common.Unmarshal(encoded, &decoded))
	return decoded
}

func callViduHook(t *testing.T, plugin *jsplugin.LoadedPlugin, name string, args ...any) map[string]any {
	t.Helper()
	value, err := plugin.Engine.Call(t.Context(), name, args...)
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, common.Unmarshal(encoded, &decoded))
	return decoded
}

func TestViduNativeRoutesCoverDocumentedSurface(t *testing.T) {
	generation := jsplugin.DefaultRegistry.Generation()
	require.NotNil(t, generation)
	routes := map[string]string{
		"/vidu/ent/v2/text2video":      "text_to_video",
		"/vidu/ent/v2/img2video":       "image_to_video",
		"/vidu/ent/v2/reference2video": "reference_to_video",
		"/vidu/ent/v2/start-end2video": "first_tail_to_video",
		"/vidu/ent/v2/multiframe":      "multiframe",
		"/vidu/ent/v2/template":        "template",
		"/vidu/ent/v2/template-story":  "template_story",
	}
	for path, action := range routes {
		binding, found := generation.LookupDeclaredRoute(http.MethodPost, path)
		require.Truef(t, found, "missing Vidu route %s", path)
		assert.Equal(t, "vidu", binding.Plugin.Meta.Key)
		assert.Equal(t, jsplugin.RouteTypeSubmit, binding.Route.Type)
		assert.Equal(t, action, binding.Route.Action)
	}
	binding, found := generation.LookupDeclaredRoute(http.MethodGet, "/vidu/ent/v2/tasks")
	require.True(t, found)
	assert.Equal(t, "vidu", binding.Plugin.Meta.Key)
	assert.Equal(t, jsplugin.RouteTypeDynamic, binding.Route.Type)
	assert.Equal(t, "list", binding.Route.Action)
}

func TestViduQ3ResolutionProfilesAndValidation(t *testing.T) {
	plugin := loadViduPlugin(t)
	proSchema, _ := plugin.Meta.UsageForModel("viduq3-pro")
	turboSchema, _ := plugin.Meta.UsageForModel("viduq3-turbo")
	otherQ3Schema, _ := plugin.Meta.UsageForModel("viduq3-mix")
	assert.Equal(t, []string{"720p", "1080p"}, proSchema["resolution"].Enum)
	assert.Equal(t, []string{"540p", "720p", "1080p"}, turboSchema["resolution"].Enum)
	assert.Equal(t, []string{"360p", "540p", "720p", "1080p"}, otherQ3Schema["resolution"].Enum)

	for _, tc := range []struct {
		name       string
		model      string
		resolution string
		wantError  string
	}{
		{name: "pro rejects 360p", model: "viduq3-pro", resolution: "360p", wantError: "does not support 360p"},
		{name: "pro rejects 540p", model: "viduq3-pro", resolution: "540p", wantError: "does not support 540p"},
		{name: "turbo rejects 360p", model: "viduq3-turbo", resolution: "360p", wantError: "does not support 360p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := plugin.Engine.CallPath(t.Context(), "native", []string{"createTextVideoTask"}, map[string]any{
				"body": map[string]any{"kind": "json", "value": map[string]any{
					"model": tc.model, "prompt": "a lighthouse", "resolution": tc.resolution,
				}},
			})
			require.ErrorContains(t, err, tc.wantError)
		})
	}

	value, err := plugin.Engine.CallPath(t.Context(), "native", []string{"createTextVideoTask"}, map[string]any{
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": "viduq3-turbo", "prompt": "a lighthouse", "resolution": "540p",
		}},
	})
	require.NoError(t, err)
	assert.Equal(t, "submit", value.(map[string]any)["kind"])

	_, err = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"action": "text_to_video", "model": "viduq3-pro", "upstreamModel": "viduq3-pro",
		"baseUrl": "https://provider.example", "apiKey": "provider-key",
		"requestBody": map[string]any{"model": "viduq3-pro", "prompt": "a lighthouse", "resolution": "360p"},
	})
	require.ErrorContains(t, err, "does not support 360p")
}

func TestViduNativeSubmitRequestsKeepOfficialFields(t *testing.T) {
	plugin := loadViduPlugin(t)
	cases := []struct {
		name       string
		decoder    string
		body       map[string]any
		action     string
		model      string
		path       string
		mustFields map[string]any
		noModel    bool
	}{
		{
			name:    "text",
			decoder: "createTextVideoTask",
			body:    map[string]any{"model": "viduq3-pro", "prompt": "a lighthouse", "audio": true, "callback_url": "https://client.example/callback"},
			action:  "text_to_video",
			model:   "viduq3-pro",
			path:    "/ent/v2/text2video",
			mustFields: map[string]any{
				"audio":        true,
				"callback_url": "https://client.example/callback",
			},
		},
		{
			name:       "image",
			decoder:    "createImageVideoTask",
			body:       map[string]any{"model": "viduq2", "images": []any{"https://cdn.example/start.png"}, "audio": false},
			action:     "image_to_video",
			model:      "viduq2",
			path:       "/ent/v2/img2video",
			mustFields: map[string]any{"images": []any{"https://cdn.example/start.png"}, "audio": false},
		},
		{
			name:       "reference",
			decoder:    "createReferenceVideoTask",
			body:       map[string]any{"model": "viduq3-mix", "images": []any{"https://cdn.example/ref.png"}, "prompt": "walk"},
			action:     "reference_to_video",
			model:      "viduq3-mix",
			path:       "/ent/v2/reference2video",
			mustFields: map[string]any{"images": []any{"https://cdn.example/ref.png"}},
		},
		{
			name:       "first and last frame",
			decoder:    "createFirstTailVideoTask",
			body:       map[string]any{"model": "viduq2-pro", "images": []any{"https://cdn.example/first.png", "https://cdn.example/last.png"}},
			action:     "first_tail_to_video",
			model:      "viduq2-pro",
			path:       "/ent/v2/start-end2video",
			mustFields: map[string]any{"images": []any{"https://cdn.example/first.png", "https://cdn.example/last.png"}},
		},
		{
			name:    "multiframe",
			decoder: "createMultiframeTask",
			body: map[string]any{
				"model":       "viduq2-turbo",
				"start_image": "https://cdn.example/start.png",
				"image_settings": []any{
					map[string]any{"key_image": "https://cdn.example/middle.png", "duration": 3},
					map[string]any{"key_image": "https://cdn.example/end.png", "prompt": "fade"},
				},
				"payload": "trace-1",
			},
			action:     "multiframe",
			model:      "viduq2-turbo",
			path:       "/ent/v2/multiframe",
			mustFields: map[string]any{"payload": "trace-1"},
		},
		{
			name:       "template",
			decoder:    "createTemplateTask",
			body:       map[string]any{"template": "hugging", "images": []any{"https://cdn.example/photo.png"}, "area": "china"},
			action:     "template",
			model:      "viduq2",
			path:       "/ent/v2/template",
			mustFields: map[string]any{"template": "hugging", "area": "china"},
			noModel:    true,
		},
		{
			name:       "template story",
			decoder:    "createTemplateStoryTask",
			body:       map[string]any{"story": "love_story", "images": []any{"https://cdn.example/photo.png"}, "payload": "trace-2"},
			action:     "template_story",
			model:      "viduq2",
			path:       "/ent/v2/template-story",
			mustFields: map[string]any{"story": "love_story", "payload": "trace-2"},
			noModel:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decoded := callViduNative(t, plugin, tc.decoder, map[string]any{"body": map[string]any{"kind": "json", "value": tc.body}}).(map[string]any)
			assert.Equal(t, "submit", decoded["kind"])
			assert.Equal(t, tc.action, decoded["action"])
			assert.Equal(t, tc.model, decoded["model"])
			requestBody := decoded["requestBody"].(map[string]any)
			descriptor := callViduHook(t, plugin, "buildSubmitRequest", map[string]any{
				"action": tc.action, "model": tc.model, "upstreamModel": tc.model,
				"baseUrl": "https://provider.example", "apiKey": "provider-key", "requestBody": requestBody,
			})
			assert.Equal(t, "https://provider.example"+tc.path, descriptor["url"])
			headers := descriptor["headers"].(map[string]any)
			assert.Equal(t, "Bearer provider-key", headers["Authorization"])
			upstreamBody := descriptor["body"].(map[string]any)
			if tc.noModel {
				assert.NotContains(t, upstreamBody, "model")
			}
			for key, value := range tc.mustFields {
				assert.Equal(t, value, upstreamBody[key], key)
			}
		})
	}
}

func TestViduNativeQueryUsesTaskListAndPreservesResults(t *testing.T) {
	plugin := loadViduPlugin(t)
	queryValue, err := plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{
		"taskId":  "upstream-task",
		"baseUrl": "https://provider.example",
		"apiKey":  "provider-key",
	})
	require.NoError(t, err)
	query := queryValue.(map[string]any)
	assert.Equal(t, "https://provider.example/ent/v2/tasks?task_ids=upstream-task", query["url"])
	assert.Equal(t, "Bearer provider-key", query["headers"].(map[string]any)["Authorization"])

	gatewayQueryValue, err := plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{
		"taskId":   "upstream-task",
		"baseUrl":  "https://example.com",
		"apiKey":   "provider-key",
		"upstream": map[string]any{"kind": "new_api"},
	})
	require.NoError(t, err)
	gatewayQuery := gatewayQueryValue.(map[string]any)
	assert.Equal(t, "https://example.com/vidu/ent/v2/tasks?task_ids=upstream-task", gatewayQuery["url"])

	prefixedQueryValue, err := plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{
		"taskId":  "upstream-task",
		"baseUrl": "https://example.com/vidu/",
		"apiKey":  "provider-key",
	})
	require.NoError(t, err)
	prefixedQuery := prefixedQueryValue.(map[string]any)
	assert.Equal(t, "https://example.com/vidu/ent/v2/tasks?task_ids=upstream-task", prefixedQuery["url"])

	body := map[string]any{
		"next_page_token": "next",
		"tasks": []any{map[string]any{
			"id":    "upstream-task",
			"state": "success",
			"creations": []any{
				map[string]any{"url": "https://cdn.example/video.mp4"},
				map[string]any{"watermarked_url": "https://cdn.example/video-wm.mp4"},
			},
		}},
	}
	parsedValue, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"taskId": "upstream-task"}, body)
	require.NoError(t, err)
	parsed := parsedValue.(map[string]any)
	assert.Equal(t, "SUCCESS", parsed["status"])
	assert.Equal(t, "https://cdn.example/video.mp4", parsed["url"])
	_, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", map[string]any{}, map[string]any{
		"statusCode": 400,
		"body":       map[string]any{"code": "invalid_request", "message": "invalid template"},
	})
	require.ErrorContains(t, err, "invalid template")

	usageValue, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{"taskId": "upstream-task"}, nil, body)
	require.NoError(t, err)
	assert.Nil(t, usageValue)

	artifacts := callViduNative(t, plugin, "viduTaskList", map[string]any{}, map[string]any{
		"items": []any{map[string]any{
			"task_id": "public-task",
			"status":  "SUCCESS",
			"data":    body,
		}},
		"total": 21, "page_num": 1, "page_size": 20,
	}).(map[string]any)
	assert.Equal(t, "2", artifacts["next_page_token"])
	tasks := artifacts["tasks"].([]any)
	require.Len(t, tasks, 1)
	assert.Equal(t, "public-task", tasks[0].(map[string]any)["id"])
}

func TestViduNativeListDecodesOfficialFilters(t *testing.T) {
	plugin := loadViduPlugin(t)
	value := callViduNative(t, plugin, "decodeViduTaskList", map[string]any{
		"body": map[string]any{"kind": "none"},
		"query": map[string][]string{
			"task_ids":         {"public-task-1,public-task-2"},
			"states":           {"created,processing"},
			"model_versions":   {"q2"},
			"templates":        {"hugging"},
			"resolutions":      {"720p"},
			"created_at.from":  {"100"},
			"created_at.to":    {"200"},
			"pager.pagesz":     {"50"},
			"pager.page_token": {"3"},
		},
	}).(map[string]any)
	assert.Equal(t, []any{"public-task-1", "public-task-2"}, value["taskIds"])
	assert.Equal(t, []any{"queued", "running"}, value["statuses"])
	assert.Contains(t, value["models"], "viduq2")
	assert.Equal(t, map[string]any{"template": []any{"hugging"}, "resolution": []any{"720p"}}, value["dataFilters"])
	listOptions := value["listOptions"].(map[string]any)
	assert.Equal(t, float64(3), listOptions["pageNum"])
	assert.Equal(t, float64(50), listOptions["pageSize"])
	assert.Equal(t, float64(100), listOptions["createdAfter"])
	assert.Equal(t, float64(200), listOptions["createdBefore"])
}
