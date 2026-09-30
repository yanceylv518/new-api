package plugins_test

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKlingResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "kling",
		model:     "kling-v2-master",
		requestBody: map[string]any{
			"model": "kling-v2-master",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "camera orbit"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/frame.png"},
			}}},
			"seconds": 10,
			"metadata": map[string]any{
				"mode": "pro",
			},
		},
		wantAction: "image_to_video",
		wantRequest: map[string]any{
			"model":    "kling-v2-master",
			"prompt":   "camera orbit",
			"image":    "https://cdn.example/frame.png",
			"duration": float64(10),
			"metadata": map[string]any{"mode": "pro"},
		},
		wantUsageKeys:  []string{"units"},
		wantVendorName: "kling",
	})
}

func TestKlingRequestURLsRespectConfiguredPrefix(t *testing.T) {
	source, err := builtinplugins.Source("kling")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "kling"})
	require.NoError(t, err)

	cases := []struct {
		name    string
		baseURL string
		apiKey  string
		gateway bool
		want    string
	}{
		{"official", "https://provider.example", "access|secret", false, "https://provider.example"},
		{"official trailing slash", "https://provider.example/", "access|secret", false, "https://provider.example"},
		{"gateway root", "https://provider.example", "gateway-token", true, "https://provider.example/kling"},
		{"gateway prefix", "https://provider.example/kling", "gateway-token", true, "https://provider.example/kling"},
		{"gateway prefix trailing slashes", "https://provider.example/kling///", "gateway-token", true, "https://provider.example/kling"},
		{"nested gateway prefix", "https://provider.example/proxy/kling/", "gateway-token", true, "https://provider.example/proxy/kling"},
		{"legacy relay prefix", "https://provider.example/kling/", "sk-test", false, "https://provider.example/kling"},
		{"vendor custom prefix", "https://provider.example/kling/", "access|secret", false, "https://provider.example/kling"},
		{"different suffix", "https://provider.example/kling-proxy/", "gateway-token", true, "https://provider.example/kling-proxy/kling"},
	}
	hooks := []struct {
		name   string
		hook   string
		action string
		path   string
	}{
		{"native submit", "buildSubmitRequest", "text_to_video", "/v1/videos/text2video"},
		{"native query", "buildQueryRequest", "text_to_video", "/v1/videos/text2video/upstream-task"},
		{"proxy", "buildNativeRequest", "presets_voices", "/v1/general/presets-voices"},
		{"new submit", "buildSubmitRequest", "new_text_to_video", "/text-to-video/kling-v3"},
		{"new query", "buildQueryRequest", "new_text_to_video", "/tasks?task_ids=upstream-task"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, hook := range hooks {
				t.Run(hook.name, func(t *testing.T) {
					ctx := map[string]any{
						"baseUrl": tc.baseURL, "apiKey": tc.apiKey, "action": hook.action,
						"model": "kling-v3", "upstreamModel": "kling-v3", "taskId": "upstream-task",
						"path": "/kling/v1/general/presets-voices", "method": "GET",
						"requestBody": map[string]any{"prompt": "camera orbit", "duration": 5},
					}
					if tc.gateway {
						ctx["upstream"] = map[string]any{"kind": "new_api"}
					}
					value, callErr := plugin.Engine.Call(t.Context(), hook.hook, ctx)
					require.NoError(t, callErr)
					descriptor := value.(map[string]any)
					assert.Equal(t, tc.want+hook.path, descriptor["url"])
					if tc.gateway || tc.apiKey == "sk-test" {
						assert.Equal(t, "Bearer "+tc.apiKey, descriptor["headers"].(map[string]any)["Authorization"])
					}
				})
			}
		})
	}
}

func TestKlingNativeRoutesCoverShuyanTaskSurface(t *testing.T) {
	generation := jsplugin.DefaultRegistry.Generation()
	require.NotNil(t, generation)

	postPaths := []string{
		"/kling/v1/videos/text2video",
		"/kling/v1/videos/image2video",
		"/kling/v1/videos/omni-video",
		"/kling/v1/videos/multi-image2video",
		"/kling/v1/videos/motion-control",
		"/kling/v1/videos/multi-elements",
		"/kling/v1/videos/multi-elements/init-selection",
		"/kling/v1/videos/multi-elements/add-selection",
		"/kling/v1/videos/multi-elements/delete-selection",
		"/kling/v1/videos/multi-elements/clear-selection",
		"/kling/v1/videos/multi-elements/preview-selection",
		"/kling/v1/videos/video-extend",
		"/kling/v1/videos/identify-face",
		"/kling/v1/videos/advanced-lip-sync",
		"/kling/v1/videos/avatar/image2video",
		"/kling/v1/audio/text-to-audio",
		"/kling/v1/audio/video-to-audio",
		"/kling/v1/audio/tts",
		"/kling/v1/general/custom-voices",
		"/kling/v1/general/delete-voices",
		"/kling/v1/videos/image-recognize",
		"/kling/v1/general/advanced-custom-elements",
		"/kling/v1/general/delete-elements",
		"/kling/v1/videos/effects",
	}
	for _, path := range postPaths {
		binding, found := generation.LookupDeclaredRoute(http.MethodPost, path)
		require.Truef(t, found, "missing Kling route %s", path)
		assert.Equal(t, "kling", binding.Plugin.Meta.Key)
	}

	queryPaths := []string{
		"/kling/v1/videos/text2video/:task_id",
		"/kling/v1/videos/image2video/:task_id",
		"/kling/v1/videos/omni-video/:task_id",
		"/kling/v1/videos/multi-image2video/:task_id",
		"/kling/v1/videos/motion-control/:task_id",
		"/kling/v1/videos/multi-elements/:task_id",
		"/kling/v1/videos/video-extend/:task_id",
		"/kling/v1/videos/advanced-lip-sync/:task_id",
		"/kling/v1/videos/avatar/image2video/:task_id",
		"/kling/v1/audio/text-to-audio/:task_id",
		"/kling/v1/audio/video-to-audio/:task_id",
		"/kling/v1/general/custom-voices/:task_id",
		"/kling/v1/general/advanced-custom-elements/:task_id",
		"/kling/v1/videos/effects/:task_id",
	}
	for _, path := range queryPaths {
		binding, found := generation.LookupDeclaredRoute(http.MethodGet, path)
		require.Truef(t, found, "missing Kling query route %s", path)
		assert.Equal(t, jsplugin.RouteTypeQuery, binding.Route.Type)
	}

	for _, path := range []string{
		"/kling/v1/general/presets-voices",
		"/kling/v1/general/advanced-presets-elements",
	} {
		binding, found := generation.LookupDeclaredRoute(http.MethodGet, path)
		require.Truef(t, found, "missing Kling proxy route %s", path)
		assert.Equal(t, jsplugin.RouteTypeDynamic, binding.Route.Type)
		assert.Equal(t, "proxy", binding.Route.Action)
	}

	newSubmitRoutes := map[string]string{
		"/kling/text-to-video/:model":  "new_text_to_video",
		"/kling/image-to-video/:model": "new_image_to_video",
		"/kling/omni-video/:model":     "new_omni_video",
		"/kling/motion-control/:model": "new_motion_control",
	}
	for path, action := range newSubmitRoutes {
		binding, found := generation.LookupDeclaredRoute(http.MethodPost, path)
		require.Truef(t, found, "missing Kling new route %s", path)
		assert.Equal(t, jsplugin.RouteTypeSubmit, binding.Route.Type)
		assert.Equal(t, action, binding.Route.Action)
		assert.Equal(t, "newTaskCreated", binding.Route.Render)
	}
	binding, found := generation.LookupDeclaredRoute(http.MethodGet, "/kling/tasks")
	require.True(t, found)
	assert.Equal(t, jsplugin.RouteTypeDynamic, binding.Route.Type)
	assert.Equal(t, "list", binding.Route.Action)
	assert.Equal(t, "newTaskList", binding.Route.Render)
}

func TestKlingV3BillingProfilesExposeOfficialDimensions(t *testing.T) {
	source, err := builtinplugins.Source("kling")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "kling"})
	require.NoError(t, err)

	v3Schema, _ := plugin.Meta.UsageForModel("kling-v3")
	assert.Equal(t, []string{"generation", "motion_control", "4k"}, v3Schema["operation"].Enum)
	assert.Equal(t, []string{"720p", "1080p", "4k"}, v3Schema["resolution"].Enum)
	assert.Equal(t, []string{"silent", "sound"}, v3Schema["audio"].Enum)
	omniSchema, _ := plugin.Meta.UsageForModel("kling-v3-omni")
	assert.Equal(t, []string{"silent", "sound", "reference_video"}, omniSchema["input_mode"].Enum)
}

func TestKlingV3BillingFactsFollowRequestAndCompletion(t *testing.T) {
	source, err := builtinplugins.Source("kling")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "kling"})
	require.NoError(t, err)

	cases := []struct {
		name string
		ctx  map[string]any
		want map[string]any
	}{
		{
			name: "v3 motion control",
			ctx: map[string]any{
				"action": "motion_control", "model": "kling-v3", "upstreamModel": "kling-v3",
				"requestBody": map[string]any{"duration": 5, "mode": "pro", "keep_original_sound": "yes"},
			},
			want: map[string]any{"seconds": int64(5), "resolution": "1080p", "operation": "motion_control", "audio": "sound"},
		},
		{
			name: "v3 generation 4k",
			ctx: map[string]any{
				"action": "text_to_video", "model": "kling-v3", "upstreamModel": "kling-v3",
				"requestBody": map[string]any{"duration": 5, "mode": "4k", "sound": true},
			},
			want: map[string]any{"seconds": int64(5), "resolution": "4k", "operation": "4k", "audio": "sound"},
		},
		{
			name: "v3 omni reference video",
			ctx: map[string]any{
				"action": "omni_video", "model": "kling-v3-omni", "upstreamModel": "kling-v3-omni",
				"requestBody": map[string]any{"duration": 5, "mode": "std", "video_url": "https://cdn.example/reference.mp4", "sound": true},
			},
			want: map[string]any{"seconds": int64(5), "resolution": "720p", "input_mode": "reference_video"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, callErr := plugin.Engine.Call(t.Context(), "extractUsage", tc.ctx)
			require.NoError(t, callErr)
			assert.Equal(t, tc.want, value)
		})
	}

	value, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{
		"model": "kling-v3", "upstreamModel": "kling-v3", "action": "text_to_video",
	}, map[string]any{}, map[string]any{
		"data": map[string]any{
			"task_result": map[string]any{"videos": []any{map[string]any{"duration": "7.5", "url": "https://cdn.example/result.mp4"}}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"seconds": 7.5}, value)
}

func TestKlingNativeRequestKeepsOfficialFieldsAndMediaResults(t *testing.T) {
	source, err := builtinplugins.Source("kling")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "kling"})
	require.NoError(t, err)

	decodedValue, err := plugin.Engine.CallMember(t.Context(), "native", "decodeSubmit", map[string]any{
		"action":        "motion_control",
		"model":         "kling-v3",
		"upstreamModel": "kling-v3",
		"body": map[string]any{
			"kind": "json",
			"value": map[string]any{
				"model_name":            "kling-v3",
				"image_url":             "https://cdn.example/person.png",
				"video_url":             "https://cdn.example/motion.mp4",
				"character_orientation": "image",
				"keep_original_sound":   "yes",
				"mode":                  "pro",
			},
		},
	})
	require.NoError(t, err)
	decoded := decodedValue.(map[string]any)
	assert.Equal(t, "motion_control", decoded["action"])
	requestBody := decoded["requestBody"].(map[string]any)
	assert.Equal(t, "https://cdn.example/motion.mp4", requestBody["video_url"])
	assert.Equal(t, "image", requestBody["character_orientation"])
	assert.Equal(t, "yes", requestBody["keep_original_sound"])

	descriptorValue, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"action":        "motion_control",
		"model":         "kling-v3",
		"upstreamModel": "kling-v3",
		"baseUrl":       "https://provider.example",
		"apiKey":        "access|secret",
		"requestBody":   requestBody,
	})
	require.NoError(t, err)
	descriptor := descriptorValue.(map[string]any)
	assert.Equal(t, "https://provider.example/v1/videos/motion-control", descriptor["url"])
	assert.Equal(t, "POST", descriptor["method"])

	taskResultValue, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{"action": "text_to_audio", "taskId": "audio-task"}, map[string]any{
		"code": 0,
		"data": map[string]any{
			"task_id":              "audio-task",
			"task_status":          "succeed",
			"task_result":          map[string]any{"audios": []any{map[string]any{"url": "https://cdn.example/result.mp3"}}},
			"final_unit_deduction": "2",
		},
	})
	require.NoError(t, err)
	taskResult := taskResultValue.(map[string]any)
	assert.Equal(t, "SUCCESS", taskResult["status"])
	assert.Equal(t, "https://cdn.example/result.mp3", taskResult["url"])

	artifactsValue, err := plugin.Engine.Call(t.Context(), "listArtifacts", map[string]any{
		"status": "SUCCESS",
		"data": map[string]any{
			"code": 0,
			"data": map[string]any{
				"task_id":     "audio-task",
				"task_result": map[string]any{"audios": []any{map[string]any{"url": "https://cdn.example/result.mp3"}}},
			},
		},
	})
	require.NoError(t, err)
	artifacts := artifactsValue.([]any)
	require.Len(t, artifacts, 1)
	assert.Equal(t, "audio", artifacts[0].(map[string]any)["type"])
}

func TestKlingNewNativeAPIUsesPathModelAndUnifiedQuery(t *testing.T) {
	source, err := builtinplugins.Source("kling")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "kling"})
	require.NoError(t, err)

	body := map[string]any{
		"prompt": "a slow camera orbit",
		"settings": map[string]any{
			"resolution": "1080p",
			"duration":   float64(5),
			"audio":      "native",
		},
		"options": map[string]any{"external_task_id": "client-001"},
	}
	decodedValue, err := plugin.Engine.CallMember(t.Context(), "native", "decodeNewSubmit", map[string]any{
		"action":        "new_text_to_video",
		"params":        map[string]string{"model": "kling-3.0"},
		"body":          map[string]any{"kind": "json", "value": body},
		"model":         "kling-3.0",
		"upstreamModel": "kling-3.0",
	})
	require.NoError(t, err)
	decoded := decodedValue.(map[string]any)
	assert.Equal(t, "kling-3.0", decoded["model"])
	assert.Equal(t, "new_text_to_video", decoded["action"])

	requestBody := decoded["requestBody"].(map[string]any)
	descriptorValue, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"action":        "new_text_to_video",
		"model":         "kling-3.0",
		"upstreamModel": "kling-3.0",
		"baseUrl":       "https://provider.example",
		"apiKey":        "access|secret",
		"requestBody":   requestBody,
	})
	require.NoError(t, err)
	descriptor := descriptorValue.(map[string]any)
	assert.Equal(t, "https://provider.example/text-to-video/kling-3.0", descriptor["url"])
	assert.Equal(t, requestBody, descriptor["body"])

	parsedValue, err := plugin.Engine.Call(t.Context(), "parseSubmitResponse", map[string]any{"action": "new_text_to_video"}, map[string]any{
		"statusCode": 202,
		"body": map[string]any{
			"code": 0,
			"data": map[string]any{"id": "upstream-001", "status": "submitted", "external_id": "client-001"},
		},
	})
	require.NoError(t, err)
	parsed := parsedValue.(map[string]any)
	assert.Equal(t, "upstream-001", parsed["taskId"])

	createdValue, err := plugin.Engine.CallMember(t.Context(), "native", "newTaskCreated", map[string]any{}, map[string]any{
		"task_id": "task_public",
		"data": map[string]any{
			"code": 0,
			"data": map[string]any{"id": "upstream-001", "status": "submitted"},
		},
	})
	require.NoError(t, err)
	created := createdValue.(map[string]any)
	assert.Equal(t, "task_public", created["data"].(map[string]any)["id"])

	queryValue, err := plugin.Engine.Call(t.Context(), "buildQueryRequest", map[string]any{
		"action":  "new_text_to_video",
		"taskId":  "upstream-001",
		"baseUrl": "https://provider.example",
		"apiKey":  "access|secret",
	})
	require.NoError(t, err)
	query := queryValue.(map[string]any)
	assert.Equal(t, "https://provider.example/tasks?task_ids=upstream-001", query["url"])

	resultValue, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{
		"action": "new_text_to_video",
		"taskId": "upstream-001",
	}, map[string]any{
		"code": 0,
		"data": []any{map[string]any{
			"id": "upstream-001", "status": "succeeded", "message": "",
			"outputs": []any{map[string]any{"type": "video", "url": "https://cdn.example/result.mp4", "duration": "7.5"}},
		}},
	})
	require.NoError(t, err)
	result := resultValue.(map[string]any)
	assert.Equal(t, "SUCCESS", result["status"])
	assert.Equal(t, "https://cdn.example/result.mp4", result["url"])

	usageValue, err := plugin.Engine.Call(t.Context(), "extractUsageOnComplete", map[string]any{
		"action":        "new_text_to_video",
		"model":         "kling-3.0",
		"upstreamModel": "kling-3.0",
	}, map[string]any{}, map[string]any{
		"code": 0,
		"data": []any{map[string]any{
			"id": "upstream-001", "status": "succeeded",
			"outputs": []any{map[string]any{"type": "video", "url": "https://cdn.example/result.mp4", "duration": "7.5"}},
		}},
	})
	require.NoError(t, err)
	usage := usageValue.(map[string]any)
	assert.Equal(t, 1.5, usage["units"])

	listValue, err := plugin.Engine.CallMember(t.Context(), "native", "decodeNewTaskList", map[string]any{
		"body":  map[string]any{"kind": "none"},
		"query": map[string][]string{"external_task_ids": []string{"client-001"}},
	})
	require.NoError(t, err)
	listIntent := listValue.(map[string]any)
	assert.Equal(t, []any{"client-001"}, listIntent["externalTaskIds"])
	assert.Equal(t, []any{"new_text_to_video", "new_image_to_video", "new_omni_video", "new_motion_control"}, listIntent["actions"])

	listResponseValue, err := plugin.Engine.CallMember(t.Context(), "native", "newTaskList", map[string]any{}, map[string]any{
		"items": []any{map[string]any{
			"task_id": "task_public",
			"status":  "SUCCESS",
			"data": map[string]any{
				"code": 0,
				"data": []any{map[string]any{
					"id": "task_public", "status": "succeeded", "external_id": "client-001",
					"outputs": []any{map[string]any{"type": "video", "url": "https://cdn.example/result.mp4", "duration": "7.5"}},
				}},
			},
		}},
	})
	require.NoError(t, err)
	listResponse := listResponseValue.(map[string]any)
	assert.EqualValues(t, 0, listResponse["code"])
	listItems := listResponse["data"].([]any)
	require.Len(t, listItems, 1)
	assert.Equal(t, "task_public", listItems[0].(map[string]any)["id"])
}
