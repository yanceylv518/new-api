package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwardTaskCallbackUsesExistingMediaProxy(t *testing.T) {
	setting := `{"task_media_proxy_enabled":true}`
	channel := &model.Channel{Name: "callback-media-test", Key: "fixture-key", Setting: &setting}
	require.NoError(t, model.DB.Create(channel).Error)
	t.Cleanup(func() { assert.NoError(t, model.DB.Delete(channel).Error) })

	previousAddress, previousClient := system_setting.TaskPublicAddress, taskCallbackHTTPClient
	system_setting.TaskPublicAddress = "https://gateway.example"
	t.Cleanup(func() {
		system_setting.TaskPublicAddress, taskCallbackHTTPClient = previousAddress, previousClient
	})
	bodies := make(chan []byte, 1)
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(destination.Close)
	taskCallbackHTTPClient = destination.Client()
	task := &model.Task{TaskID: "callback-task", UserId: 7, ChannelId: channel.Id, Status: model.TaskStatusQueued}
	const source = "https://upstream.example/video.mp4?signature=private"
	body := []byte(`{"id":"upstream-id","status":"succeeded","content":{"video_url":"` + source + `"}}`)
	err := ForwardTaskCallback(context.Background(), &TaskCallbackTarget{URL: destination.URL}, task, body)
	require.NoError(t, err)
	var received struct {
		Content struct {
			VideoURL string `json:"video_url"`
		} `json:"content"`
	}
	require.NoError(t, common.Unmarshal(<-bodies, &received))
	assert.NotContains(t, received.Content.VideoURL, "upstream.example")
	parsed, err := url.Parse(received.Content.VideoURL)
	require.NoError(t, err)
	assert.Equal(t, "gateway.example", parsed.Host)
	access := parsed.Query().Get(TaskArtifactAccessQueryParameter)
	assert.Len(t, access, 43)
	assert.True(t, VerifyTaskArtifactAccess(access, task.TaskID, "response-media-video"))
	assert.False(t, VerifyTaskArtifactAccess(access+"x", task.TaskID, "response-media-video"))
	assert.False(t, VerifyTaskArtifactAccess(access, "other-task", "response-media-video"))
	assert.False(t, VerifyTaskArtifactAccess(access, task.TaskID, "response-media-last-frame"))
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), task.Status)
	assert.NotContains(t, received.Content.VideoURL, "signature=private")
}

func TestTaskArtifactAccessBindsTaskAndKey(t *testing.T) {
	previousSecret := common.CryptoSecret
	common.CryptoSecret = "task-artifact-access-test-secret"
	t.Cleanup(func() { common.CryptoSecret = previousSecret })

	access, err := IssueTaskArtifactAccess("task-1", "video-main")
	require.NoError(t, err)
	assert.Len(t, access, 43)
	assert.NotContains(t, access, ".")
	assert.True(t, VerifyTaskArtifactAccess(access, "task-1", "video-main"))
	assert.False(t, VerifyTaskArtifactAccess(access, "task-2", "video-main"))
	assert.False(t, VerifyTaskArtifactAccess(access, "task-1", "video-other"))
	assert.False(t, VerifyTaskArtifactAccess(access+"x", "task-1", "video-main"))

	common.CryptoSecret = "another-node-secret"
	assert.False(t, VerifyTaskArtifactAccess(access, "task-1", "video-main"))
}

func TestBuildTaskArtifactContentURLUsesConfiguredAddressAndPreservesPrefix(t *testing.T) {
	previousSecret := common.CryptoSecret
	previousPublicAddress := system_setting.TaskPublicAddress
	previousServerAddress := system_setting.ServerAddress
	common.CryptoSecret = "task-artifact-url-test-secret"
	system_setting.TaskPublicAddress = "https://media.example/gateway/prefix/"
	system_setting.ServerAddress = "https://fallback.invalid"
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		system_setting.TaskPublicAddress = previousPublicAddress
		system_setting.ServerAddress = previousServerAddress
	})

	contentURL, err := BuildTaskArtifactContentURL("task-public", "video-main")
	require.NoError(t, err)
	parsed, err := url.Parse(contentURL)
	require.NoError(t, err)
	assert.Equal(t, "media.example", parsed.Host)
	assert.Equal(t, "/gateway/prefix/v1/tasks/task-public/artifacts/video-main/content", parsed.Path)
	assert.True(t, VerifyTaskArtifactAccess(
		parsed.Query().Get(TaskArtifactAccessQueryParameter),
		"task-public",
		"video-main",
	))
}

func TestBuildTaskArtifactContentURLFallsBackOnlyToServerAddress(t *testing.T) {
	previousSecret := common.CryptoSecret
	previousPublicAddress := system_setting.TaskPublicAddress
	previousServerAddress := system_setting.ServerAddress
	common.CryptoSecret = "task-artifact-fallback-test-secret"
	system_setting.TaskPublicAddress = ""
	system_setting.ServerAddress = "https://gateway.example/root"
	t.Cleanup(func() {
		common.CryptoSecret = previousSecret
		system_setting.TaskPublicAddress = previousPublicAddress
		system_setting.ServerAddress = previousServerAddress
	})

	contentURL, err := BuildTaskArtifactContentURL("task-fallback", "audio")
	require.NoError(t, err)
	assert.Contains(t, contentURL, "https://gateway.example/root/v1/tasks/task-fallback/artifacts/audio/content")

	system_setting.TaskPublicAddress = "not-a-url"
	_, err = BuildTaskArtifactContentURL("task-fallback", "audio")
	assert.Error(t, err)
}

func TestValidateTaskArtifactBaseURLOnlyAcceptsSafeAbsoluteHTTPURLs(t *testing.T) {
	for _, valid := range []string{
		"http://localhost:3000",
		"https://gateway.example",
		"https://gateway.example/prefix/path/",
	} {
		assert.NoError(t, ValidateTaskArtifactBaseURL(valid), valid)
	}
	for _, invalid := range []string{
		"",
		"/relative",
		"ftp://gateway.example",
		"https://user:secret@gateway.example",
		"https://gateway.example/path?tenant=1",
		"https://gateway.example/path#fragment",
		" https://gateway.example",
		"https://gateway.example ",
	} {
		assert.Error(t, ValidateTaskArtifactBaseURL(invalid), invalid)
	}
}
