package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type seedanceZeroReader struct{}

func (seedanceZeroReader) Read(data []byte) (int, error) {
	clear(data)
	return len(data), nil
}

type seedanceCountingResponse struct {
	header http.Header
	status int
	bytes  int64
}

func (writer *seedanceCountingResponse) Header() http.Header    { return writer.header }
func (writer *seedanceCountingResponse) WriteHeader(status int) { writer.status = status }
func (writer *seedanceCountingResponse) Write(data []byte) (int, error) {
	writer.bytes += int64(len(data))
	return len(data), nil
}

func TestSeedanceCompatDownloadHonorsConfiguredUploadLimit(t *testing.T) {
	previous := common.GetManagementRateLimits()
	t.Cleanup(func() {
		encoded, err := common.Marshal(previous)
		require.NoError(t, err)
		require.NoError(t, common.LoadManagementRateLimits(string(encoded)))
	})
	settings := previous
	settings.AssetLimits.VideoMiB = 256
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, common.LoadManagementRateLimits(string(encoded)))
	allowPrivateTaskMediaTest(t)
	const size = 201 << 20
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(size))
		_, _ = io.CopyN(w, seedanceZeroReader{}, size)
	}))
	t.Cleanup(upstream.Close)
	writer := &seedanceCountingResponse{header: make(http.Header)}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodGet, "/doubao/api/assets/asset-test/download", nil)
	streamSeedanceCompatAsset(c, &model.SeedanceAsset{Name: "test.mp4", AssetType: "Video", SourceURL: upstream.URL})
	require.Equal(t, http.StatusOK, writer.status)
	assert.EqualValues(t, size, writer.bytes)
}

func TestSeedanceCompatDownloadKeepsStoredFileAfterLimitReduction(t *testing.T) {
	previous := common.GetManagementRateLimits()
	t.Cleanup(func() {
		encoded, err := common.Marshal(previous)
		require.NoError(t, err)
		require.NoError(t, common.LoadManagementRateLimits(string(encoded)))
	})
	settings := previous
	settings.AssetLimits.VideoMiB = 50
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, common.LoadManagementRateLimits(string(encoded)))
	task := setupGenericTaskTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SeedanceAssetSize{}))
	t.Cleanup(func() { assert.NoError(t, model.DB.Migrator().DropTable(&model.SeedanceAssetSize{})) })
	const size = 300 << 20
	require.NoError(t, model.DB.Create(&model.SeedanceAssetSize{AssetID: 91, UserID: task.UserId, Bytes: size}).Error)
	asset := &model.SeedanceAsset{ID: 91, UserID: task.UserId, AssetType: "Video", ObjectKey: "private/test.mp4"}
	for _, contentLength := range []int64{size, -1} {
		limit, err := seedanceCompatDownloadLimit(t.Context(), asset, contentLength)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, limit, int64(size))
	}
	asset.UserID++
	limit, err := seedanceCompatDownloadLimit(t.Context(), asset, size)
	require.NoError(t, err)
	assert.Less(t, limit, int64(size))
}
