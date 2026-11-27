package service

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
)

func TestPublicTaskMediaURLsPreserveUpstreamLinksWithoutChangingSnapshot(t *testing.T) {
	const snapshot = `{"model":"private-model","usage":{"total_tokens":40594},"content":{"video_url":"/v1/videos/task_media/content","last_frame_url":"/v1/tasks/task_media/artifacts/last-frame/content"}}`
	task := &model.Task{TaskID: "task_media", Platform: "anyllm", Status: model.TaskStatusSuccess,
		Properties: model.Properties{OriginModelName: "public-model", UpstreamModelName: "private-model"}, Data: []byte(snapshot),
		PrivateData: model.TaskPrivateData{PluginState: []byte(`{"media":{"video":"https://cdn.example/video.mp4?Signature=video","last_frame":"https://cdn.example/frame.jpg?Signature=frame"}}`)}}
	data, err := PublicTaskData(task)
	require.NoError(t, err)
	var response struct {
		Model   string            `json:"model"`
		Content map[string]string `json:"content"`
	}
	require.NoError(t, common.Unmarshal(data, &response))
	assert.Equal(t, "public-model", response.Model)
	assert.Equal(t, "https://cdn.example/video.mp4?Signature=video", response.Content["video_url"])
	assert.Equal(t, "https://cdn.example/frame.jpg?Signature=frame", response.Content["last_frame_url"])
	assert.Equal(t, snapshot, string(task.Data))
	for _, platform := range []constant.TaskPlatform{"doubao", "fixture"} {
		task.Platform = platform
		data, err = PublicTaskData(task)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "access=")
	}
	task.Platform, task.Status = "anyllm", model.TaskStatusInProgress
	data, err = PublicTaskData(task)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "access=")
}

func TestBuildTaskPluginViewRewritesOnlyStructuredTaskIDFields(t *testing.T) {
	const (
		privateTaskID = "upstream-task-123"
		publicTaskID  = "task_public_123"
		resultURL     = "https://cdn.example.com/results/upstream-task-123/video.mp4"
	)

	taskData, err := common.Marshal(map[string]any{
		"task_id": privateTaskID,
		"id":      privateTaskID,
		"taskId":  privateTaskID,
		"url":     resultURL,
		"message": "completed upstream-task-123",
		"nested": []any{
			map[string]any{
				"task_id": privateTaskID,
				"url":     resultURL,
			},
			privateTaskID,
		},
		privateTaskID: "opaque map key",
	})
	require.NoError(t, err)
	task := &model.Task{
		TaskID: publicTaskID,
		Action: "context_ir",
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: privateTaskID,
		},
		Data: taskData,
	}

	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)

	data, ok := view.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, publicTaskID, data["task_id"])
	assert.Equal(t, publicTaskID, data["id"])
	assert.Equal(t, publicTaskID, data["taskId"])
	assert.Equal(t, resultURL, data["url"])
	assert.Equal(t, "completed upstream-task-123", data["message"])
	assert.Equal(t, "opaque map key", data[privateTaskID])
	assert.Equal(t, "context_ir", view.Action)

	nested, ok := data["nested"].([]any)
	require.True(t, ok)
	nestedData, ok := nested[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, publicTaskID, nestedData["task_id"])
	assert.Equal(t, resultURL, nestedData["url"])
	assert.Equal(t, privateTaskID, nested[1])

}

func TestBuildTaskPluginViewOmitsPrivatePollState(t *testing.T) {
	task := &model.Task{
		TaskID: "task_public_view",
		Data:   []byte(`{"ok":true}`),
		PrivateData: model.TaskPrivateData{
			PluginState:  []byte(`{"req_key":"secret"}`),
			PollFailures: 7,
		},
	}

	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)
	encoded, err := common.Marshal(view)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, common.Unmarshal(encoded, &payload))
	assert.NotContains(t, payload, "plugin_state")
	assert.NotContains(t, payload, "poll_failures")
	assert.NotContains(t, payload, "private_data")
}

func TestTaskAliasPublicViewPreservesStoredModelsAndOpaqueValues(t *testing.T) {
	const raw = `{"model":"real-model","tokens":9007199254740993,"data":[{"model_name":"real-model"},{"model":"unrelated-model"}],"error":{"message":"real-model is unavailable"},"prompt":"use real-model","url":"https://cdn.example/real-model.mp4"}`
	task := &model.Task{
		TaskID: "task_alias", FailReason: "real-model is unavailable",
		Properties: model.Properties{OriginModelName: "public-model", UpstreamModelName: "real-model"},
		Data:       []byte(raw),
	}
	data, err := PublicTaskData(task)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"tokens":9007199254740993`)
	assert.Contains(t, string(data), `"model":"public-model"`)
	assert.Contains(t, string(data), `"model_name":"public-model"`)
	assert.Contains(t, string(data), `"model":"unrelated-model"`)
	assert.Contains(t, string(data), `"prompt":"use real-model"`)
	assert.Contains(t, string(data), `"url":"https://cdn.example/real-model.mp4"`)
	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)
	assert.Equal(t, "public-model is unavailable", view.FailReason)
	assert.Equal(t, raw, string(task.Data))
	assert.Equal(t, "real-model", task.Properties.UpstreamModelName)
	assert.Equal(t, "real-model is unavailable", task.FailReason)
	assert.Equal(t, "real-model-vip cannot use real-model-vip", PublicTaskModelText("real-model-vip cannot use real-model", "real-model-vip", "real-model"))
}

func TestBuildTaskPluginViewEncodesKeysInMapOrder(t *testing.T) {
	task := &model.Task{
		TaskID:     "task_public_order",
		Action:     "generate",
		Platform:   "fixture",
		Status:     model.TaskStatusSuccess,
		Progress:   "100%",
		FailReason: "none",
		CreatedAt:  1700000000,
		UpdatedAt:  1700000100,
		FinishTime: 1700000200,
		Data:       []byte(`{"b":1,"a":{"10":true,"2":false}}`),
	}

	view, err := BuildTaskPluginView(task)
	require.NoError(t, err)
	encoded, err := common.Marshal(view)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, common.Unmarshal(encoded, &payload))
	require.Len(t, payload, 10)
	assert.Equal(t, "generate", payload["action"])
	reencoded, err := common.Marshal(payload)
	require.NoError(t, err)
	assert.Equal(t, string(reencoded), string(encoded))
}

func TestZZEngineSafetyIdentifierLifecycle(t *testing.T) {
	source, err := builtinplugins.Source("zzengine")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "zzengine"})
	require.NoError(t, err)
	const modelName = "doubao-seedance-2-5-260628"
	const upstreamTaskID = "upstream-task-zzengine"
	const exactIdentifier = " upstream-task-zzengine seedance-2.5 \"<>&\\\" "

	callbacks := make(chan []byte, 1)
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodPost, request.Method)
		body, readErr := io.ReadAll(request.Body)
		assert.NoError(t, readErr)
		callbacks <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(destination.Close)
	previousClient := taskCallbackHTTPClient
	taskCallbackHTTPClient = destination.Client()
	t.Cleanup(func() { taskCallbackHTTPClient = previousClient })

	for _, tc := range []struct {
		name    string
		surface string
		fields  map[string]any
		want    string
		present bool
	}{
		{"native exact", "native", map[string]any{"safety_identifier": exactIdentifier}, exactIdentifier, true},
		{"native empty", "native", map[string]any{"safety_identifier": ""}, "", true},
		{"native absent", "native", nil, "", false},
		{"native null", "native", map[string]any{"safety_identifier": nil}, "", false},
		{"native limit", "native", map[string]any{"safety_identifier": strings.Repeat("a", 64)}, strings.Repeat("a", 64), true},
		{"video top-level wins", "openai_video", map[string]any{"safety_identifier": "user-top", "metadata": map[string]any{"safety_identifier": "user-meta"}}, "user-top", true},
		{"video metadata", "openai_video", map[string]any{"metadata": map[string]any{"safety_identifier": "user-meta"}}, "user-meta", true},
		{"multipart exact", "multipart", map[string]any{"safety_identifier": exactIdentifier}, exactIdentifier, true},
		{"multipart metadata", "multipart", map[string]any{"metadata": `{"safety_identifier":"form-meta"}`}, "form-meta", true},
		{"responses top-level", "openai_responses", map[string]any{"safety_identifier": "responses-top"}, "responses-top", true},
		{"responses metadata", "openai_responses", map[string]any{"metadata": map[string]any{"safety_identifier": "responses-meta"}}, "responses-meta", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{"model": modelName, "duration": 4, "resolution": "480p", "prompt": "a bird"}
			requestBody := map[string]any{"kind": "json", "value": request}
			export, path := "protocols", []string{tc.surface, "decodeRequest"}
			switch tc.surface {
			case "native":
				export, path = "native", []string{"createTask"}
				delete(request, "prompt")
				request["content"] = []any{map[string]any{"type": "text", "text": "a bird"}}
			case "openai_responses":
				delete(request, "prompt")
				request["input"] = "a bird"
			case "multipart":
				path = []string{"openai_video", "decodeRequest"}
				fields := map[string][]string{"model": {modelName}, "duration": {"4"}, "resolution": {"480p"}, "prompt": {"a bird"}}
				for key, value := range tc.fields {
					fields[key] = []string{value.(string)}
				}
				requestBody = map[string]any{"kind": "multipart", "fields": fields}
			}
			maps.Copy(request, tc.fields)
			value, err := plugin.Engine.CallPath(t.Context(), export, path, map[string]any{"model": modelName, "body": requestBody})
			require.NoError(t, err)
			intent, ok := value.(map[string]any)
			require.True(t, ok)
			driver := map[string]any{"model": modelName, "upstreamModel": modelName, "baseUrl": "https://upstream.example", "requestBody": intent["requestBody"]}
			originalRequest, err := common.Marshal(intent["requestBody"])
			require.NoError(t, err)
			descriptor, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", driver)
			require.NoError(t, err)
			built, ok := descriptor.(map[string]any)
			require.True(t, ok)
			body, ok := built["body"].(map[string]any)
			require.True(t, ok)
			assert.NotContains(t, body, "safety_identifier")
			afterBuild, err := common.Marshal(intent["requestBody"])
			require.NoError(t, err)
			assert.JSONEq(t, string(originalRequest), string(afterBuild))

			value, err = plugin.Engine.Call(t.Context(), "parseSubmitResponse", driver, map[string]any{
				"body": map[string]any{"id": upstreamTaskID, "status": "queued", "safety_identifier": "upstream-account"},
			})
			require.NoError(t, err)
			accepted, ok := value.(map[string]any)
			require.True(t, ok)
			state, err := common.Marshal(accepted["state"])
			require.NoError(t, err)
			data, err := common.Marshal(accepted["taskData"])
			require.NoError(t, err)
			task := &model.Task{
				TaskID: model.GenerateTaskID(), Platform: "zzengine", Action: "text_to_video", Status: model.TaskStatusQueued,
				UserId: 9001, Quota: 12345, Data: data,
				Properties:  model.Properties{OriginModelName: modelName, UpstreamModelName: "seedance-2.5"},
				PrivateData: model.TaskPrivateData{UpstreamTaskID: upstreamTaskID, PluginState: state},
			}
			require.NoError(t, model.DB.Create(task).Error)
			t.Cleanup(func() { assert.NoError(t, model.DB.Delete(&model.Task{}, task.ID).Error) })
			var stored model.Task
			require.NoError(t, model.DB.First(&stored, task.ID).Error)
			assert.JSONEq(t, string(state), string(stored.PrivateData.PluginState))
			view, err := BuildTaskPluginView(&stored)
			require.NoError(t, err)
			public, ok := view.Data.(map[string]any)
			require.True(t, ok)
			if tc.present {
				assert.Equal(t, tc.want, public["safety_identifier"])
			} else {
				assert.NotContains(t, public, "safety_identifier")
			}
			created, err := plugin.Engine.CallPath(t.Context(), "native", []string{"taskCreated"}, map[string]any{}, view)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"id": task.TaskID}, created)

			statuses := []string{"succeeded"}
			if tc.name == "native exact" {
				statuses = []string{"queued", "running", "succeeded", "failed", "cancelled", "expired"}
			}
			for _, status := range statuses {
				upstream := map[string]any{
					"id": upstreamTaskID, "model": "seedance-2.5", "status": status,
					"safety_identifier": map[string]any{"unexpected": "upstream-account"},
					"content":           map[string]any{"video_url": "https://cdn.example/video.mp4"},
					"usage":             map[string]any{"completion_tokens": 1000, "total_tokens": 1200},
					"duration":          4,
				}
				if status == "failed" || status == "expired" {
					upstream["error"] = map[string]any{"code": "generation_failed", "message": "reference could not be processed"}
				}
				upstreamRaw, err := common.Marshal(upstream)
				require.NoError(t, err)
				var pollState any
				require.NoError(t, common.Unmarshal(stored.PrivateData.PluginState, &pollState))
				polled, err := plugin.Engine.Call(t.Context(), "parseTaskResult", map[string]any{
					"taskId": upstreamTaskID, "publicTaskId": task.TaskID, "model": modelName, "upstreamModel": "seedance-2.5", "state": pollState,
				}, upstream)
				require.NoError(t, err)
				result, ok := polled.(map[string]any)
				require.True(t, ok)
				stored.Data, err = common.Marshal(result["taskData"])
				require.NoError(t, err)
				stored.Status = model.TaskStatus(result["status"].(string))
				stored.FailReason, _ = result["reason"].(string)
				require.NoError(t, model.DB.Save(&stored).Error)
				var reloaded model.Task
				require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
				assert.Equal(t, 12345, reloaded.Quota)
				assert.JSONEq(t, string(state), string(reloaded.PrivateData.PluginState))
				view, err := BuildTaskPluginView(&reloaded)
				require.NoError(t, err)
				queried, err := plugin.Engine.CallPath(t.Context(), "native", []string{"taskStatus"}, map[string]any{}, view)
				require.NoError(t, err)
				query, ok := queried.(map[string]any)
				require.True(t, ok)
				assert.Equal(t, task.TaskID, query["id"])
				assert.Equal(t, status, query["status"])
				if tc.present {
					assert.Equal(t, tc.want, query["safety_identifier"])
				} else {
					assert.NotContains(t, query, "safety_identifier")
				}
				listed, err := plugin.Engine.CallPath(t.Context(), "native", []string{"taskList"}, map[string]any{}, map[string]any{"items": []any{view}, "total": 1})
				require.NoError(t, err)
				list, ok := listed.(map[string]any)
				require.True(t, ok)
				items, ok := list["items"].([]any)
				require.True(t, ok)
				require.Len(t, items, 1)
				assert.Equal(t, query, items[0])
				video, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "render"}, map[string]any{}, view)
				require.NoError(t, err)
				assert.Equal(t, query, video)
				require.NoError(t, ForwardTaskCallback(t.Context(), &TaskCallbackTarget{URL: destination.URL}, &reloaded, upstreamRaw))
				var callback map[string]any
				require.NoError(t, common.Unmarshal(<-callbacks, &callback))
				assert.Equal(t, task.TaskID, callback["id"])
				if tc.present {
					assert.Equal(t, tc.want, callback["safety_identifier"])
				} else {
					assert.NotContains(t, callback, "safety_identifier")
				}
				assert.JSONEq(t, string(stored.Data), string(reloaded.Data))
			}
		})
	}
}

func TestZZEngineSafetyIdentifierRejectsInvalidInputs(t *testing.T) {
	source, err := builtinplugins.Source("zzengine")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "zzengine"})
	require.NoError(t, err)
	for _, value := range []any{42, false, map[string]any{"id": "user"}, strings.Repeat("a", 65), "line\nbreak", "中文"} {
		_, err := plugin.Engine.CallPath(t.Context(), "native", []string{"createTask"}, map[string]any{
			"body": map[string]any{"kind": "json", "value": map[string]any{
				"model": "doubao-seedance-2-5-260628", "duration": 4, "resolution": "480p", "safety_identifier": value,
				"content": []any{map[string]any{"type": "text", "text": "a bird"}},
			}},
		})
		require.ErrorContains(t, err, "safety_identifier must be an ASCII string of at most 64 characters")
	}
}

func TestZZEngineSafetyIdentifierLegacyAndOtherPlugins(t *testing.T) {
	for _, platform := range []constant.TaskPlatform{"zzengine", "doubao", "qstart", "anyllm", "originocean"} {
		t.Run(string(platform), func(t *testing.T) {
			task := &model.Task{Platform: platform, Status: model.TaskStatusSuccess, Data: []byte(`{"status":"succeeded","safety_identifier":"upstream-only"}`)}
			for _, state := range []string{"", `{}`, `{"video_input":"none"}`} {
				task.PrivateData.PluginState = []byte(state)
				data, err := PublicTaskData(task)
				require.NoError(t, err)
				var response map[string]any
				require.NoError(t, common.Unmarshal(data, &response))
				if platform == "zzengine" {
					assert.NotContains(t, response, "safety_identifier")
				} else {
					assert.Equal(t, "upstream-only", response["safety_identifier"])
				}
			}
		})
	}
}
