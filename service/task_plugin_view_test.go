package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
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
