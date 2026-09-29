package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestTaskLogDTOSeparatesUserAdminAndRootDetails(t *testing.T) {
	task := &model.Task{
		TaskID:   "task_public",
		Platform: "document-parser",
		PrivateData: model.TaskPrivateData{
			Key:            "channel-secret-canary",
			UpstreamTaskID: "upstream-private",
			NodeName:       "node-a",
			Execution: &model.TaskExecutionSnapshot{
				RequestID:   "request-public",
				RequestPath: "/v1/documents",
				TaskPlugin: &model.TaskPluginSnapshot{
					Key:     "document-parser",
					Name:    "Document Parser",
					Version: "1.2.3",
					Author: &model.TaskPluginAuthorSnapshot{
						Name: "Community Author",
						URL:  "https://plugins.example/author",
					},
					APIVersion: 1,
					Generation: 42,
				},
			},
		},
	}

	userView := tasksToDto([]*model.Task{task}, false, common.RoleCommonUser)[0]
	assert.Nil(t, userView.AdminInfo)
	assert.Nil(t, userView.RootInfo)

	adminView := tasksToDto([]*model.Task{task}, false, common.RoleAdminUser)[0]
	require.NotNil(t, adminView.AdminInfo)
	require.NotNil(t, adminView.AdminInfo.TaskPlugin)
	assert.Equal(t, "document-parser", adminView.AdminInfo.TaskPlugin.Key)
	assert.Equal(t, "Document Parser", adminView.AdminInfo.TaskPlugin.Name)
	assert.Equal(t, "1.2.3", adminView.AdminInfo.TaskPlugin.Version)
	require.NotNil(t, adminView.AdminInfo.TaskPlugin.Author)
	assert.Equal(t, "Community Author", adminView.AdminInfo.TaskPlugin.Author.Name)
	assert.Equal(t, "https://plugins.example/author", adminView.AdminInfo.TaskPlugin.Author.URL)
	assert.Equal(t, "request-public", adminView.AdminInfo.RequestID)
	assert.Equal(t, "/v1/documents", adminView.AdminInfo.RequestPath)
	assert.Nil(t, adminView.RootInfo)

	rootView := tasksToDto([]*model.Task{task}, false, common.RoleRootUser)[0]
	require.NotNil(t, rootView.AdminInfo)
	require.NotNil(t, rootView.RootInfo)
	require.NotNil(t, rootView.RootInfo.TaskPlugin)
	assert.Equal(t, 1, rootView.RootInfo.TaskPlugin.APIVersion)
	assert.Equal(t, uint64(42), rootView.RootInfo.TaskPlugin.Generation)
	assert.Equal(t, "upstream-private", rootView.RootInfo.UpstreamTaskID)
	assert.Equal(t, "node-a", rootView.RootInfo.NodeName)

	adminJSON, err := common.Marshal(adminView)
	require.NoError(t, err)
	assert.NotContains(t, string(adminJSON), "channel-secret-canary")
	assert.NotContains(t, string(adminJSON), "upstream-private")

	rootJSON, err := common.Marshal(rootView)
	require.NoError(t, err)
	assert.NotContains(t, string(rootJSON), "channel-secret-canary")
	assert.Contains(t, string(rootJSON), "upstream-private")
}

func TestTaskLogDTODoesNotInventHistoricalPluginProvenance(t *testing.T) {
	task := &model.Task{
		TaskID:   "task_without_snapshot",
		Platform: "document-parser",
	}

	adminView := tasksToDto([]*model.Task{task}, false, common.RoleAdminUser)[0]

	assert.Nil(t, adminView.AdminInfo)
	assert.Nil(t, adminView.RootInfo)
}

func TestTaskLogDTOReplacesLegacyVideoURLWithAvailabilityFlag(t *testing.T) {
	task := &model.Task{
		TaskID:     "task_legacy_video",
		Platform:   "jimeng",
		Action:     constant.TaskActionTextToVideo,
		Status:     model.TaskStatusSuccess,
		FailReason: "https://private-upstream.invalid/video.mp4?signature=secret",
	}

	view := tasksToDto([]*model.Task{task}, false, common.RoleCommonUser)[0]
	assert.True(t, view.LegacyVideoAvailable)
	assert.Empty(t, view.ResultURL)
	assert.Empty(t, view.FailReason)
	encoded, err := common.Marshal(view)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private-upstream.invalid")
	assert.NotContains(t, string(encoded), "result_url")
	assert.Contains(t, string(encoded), "legacy_video_available")
}

func TestTaskLogDTOKeepsFailureReasonAndDoesNotMarkPluginTaskLegacy(t *testing.T) {
	failed := &model.Task{
		TaskID:     "task_failed",
		Platform:   "jimeng",
		Action:     constant.TaskActionTextToVideo,
		Status:     model.TaskStatusFailure,
		FailReason: "provider rejected the request",
	}
	failedView := tasksToDto([]*model.Task{failed}, false, common.RoleCommonUser)[0]
	assert.Equal(t, "provider rejected the request", failedView.FailReason)
	assert.False(t, failedView.LegacyVideoAvailable)

	pluginTask := &model.Task{
		TaskID:     "task_plugin_video",
		Platform:   "community-video",
		Action:     constant.TaskActionTextToVideo,
		Status:     model.TaskStatusSuccess,
		FailReason: "https://stale-upstream.invalid/plugin-video.mp4",
		PrivateData: model.TaskPrivateData{
			ResultURL: "https://private-upstream.invalid/plugin-video.mp4",
			Execution: &model.TaskExecutionSnapshot{
				TaskPlugin: &model.TaskPluginSnapshot{Key: "community-video"},
			},
		},
	}
	pluginView := tasksToDto([]*model.Task{pluginTask}, false, common.RoleCommonUser)[0]
	assert.False(t, pluginView.LegacyVideoAvailable)
	assert.Empty(t, pluginView.ResultURL)
	assert.Empty(t, pluginView.FailReason)
}

func TestGetTaskRequestSnapshotEnforcesOwnershipAndRedactsBody(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open("file:task_request_snapshot_controller?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.AutoMigrate(&model.Task{}, &model.TaskRequestSnapshot{}))
	task := &model.Task{TaskID: "task_request_view", UserId: 31, Platform: "doubao", Action: constant.TaskActionTextToVideo}
	require.NoError(t, db.Create(task).Error)
	body, err := common.Marshal(map[string]any{
		"prompt": "keep this",
		"image":  "[base64 content omitted]",
	})
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.TaskRequestSnapshot{
		TaskID:        task.ID,
		PublicTaskID:  task.TaskID,
		UserID:        task.UserId,
		Platform:      task.Platform,
		Model:         "video-model",
		Body:          model.LongText(body),
		BodyBytes:     len(body),
		Base64Omitted: true,
		CreatedAt:     common.GetTimestamp(),
	}).Error)

	request := func(userID, role int) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodGet, "/api/task/"+task.TaskID+"/request", nil)
		context.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
		context.Set("id", userID)
		context.Set("role", role)
		GetTaskRequestSnapshot(context)
		return recorder
	}

	ownerResponse := request(task.UserId, common.RoleCommonUser)
	var ownerPayload map[string]any
	require.NoError(t, common.Unmarshal(ownerResponse.Body.Bytes(), &ownerPayload))
	assert.Equal(t, true, ownerPayload["success"])
	assert.Contains(t, ownerResponse.Body.String(), "base64 content omitted")

	foreignResponse := request(99, common.RoleCommonUser)
	var foreignPayload map[string]any
	require.NoError(t, common.Unmarshal(foreignResponse.Body.Bytes(), &foreignPayload))
	assert.Equal(t, false, foreignPayload["success"])

	adminResponse := request(99, common.RoleAdminUser)
	var adminPayload map[string]any
	require.NoError(t, common.Unmarshal(adminResponse.Body.Bytes(), &adminPayload))
	assert.Equal(t, true, adminPayload["success"])
}
