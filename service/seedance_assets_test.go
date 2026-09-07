/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type fakePrivateAssetOSSClient struct {
	putRequest *oss.PutObjectRequest
	putBody    []byte
	deletedKey string
	putErr     error
}

func (client *fakePrivateAssetOSSClient) PutObject(_ context.Context, request *oss.PutObjectRequest, _ ...func(*oss.Options)) (*oss.PutObjectResult, error) {
	if client.putErr != nil {
		return nil, client.putErr
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	client.putRequest = request
	client.putBody = body
	return &oss.PutObjectResult{}, nil
}

func (client *fakePrivateAssetOSSClient) DeleteObject(_ context.Context, request *oss.DeleteObjectRequest, _ ...func(*oss.Options)) (*oss.DeleteObjectResult, error) {
	client.deletedKey = *request.Key
	return &oss.DeleteObjectResult{}, nil
}

func (client *fakePrivateAssetOSSClient) Presign(_ context.Context, request any, _ ...func(*oss.PresignOptions)) (*oss.PresignResult, error) {
	getRequest := request.(*oss.GetObjectRequest)
	return &oss.PresignResult{URL: "https://signed.example/" + *getRequest.Key}, nil
}

// useFakePrivateAssetOSSStorage 将素材测试限制在进程内，避免依赖真实云服务。
func useFakePrivateAssetOSSStorage(t *testing.T) *fakePrivateAssetOSSClient {
	t.Helper()
	client := &fakePrivateAssetOSSClient{}
	previousFactory := privateAssetOSSStorageFactory
	privateAssetOSSStorageFactory = func() (*PrivateAssetOSSStorage, error) {
		return &PrivateAssetOSSStorage{
			client: client,
			bucket: "test-private-assets",
			prefix: "private-assets/",
		}, nil
	}
	t.Cleanup(func() { privateAssetOSSStorageFactory = previousFactory })
	return client
}

func TestSeedanceAssetClientUsesDocumentedIDFields(t *testing.T) {
	var requests []struct {
		action string
		body   map[string]string
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		var payload map[string]string
		if err := common.Unmarshal(body, &payload); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		requests = append(requests, struct {
			action string
			body   map[string]string
		}{action: request.URL.Query().Get("Action"), body: payload})
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Query().Get("Action") {
		case "GetAsset":
			_, _ = writer.Write([]byte(`{"Result":{"Id":"asset-1","Status":"Active","URL":"https://cdn.example/asset.mp4"}}`))
		default:
			_, _ = writer.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	baseURL := server.URL
	client := NewSeedanceAssetClient(&model.Channel{BaseURL: &baseURL, Key: "test-key"})

	status, previewURL, err := client.GetSeedanceAsset(context.Background(), "asset-1")
	require.NoError(t, err)
	require.Equal(t, "Active", status)
	require.Equal(t, "https://cdn.example/asset.mp4", previewURL)
	require.NoError(t, client.DeleteSeedanceAsset(context.Background(), "asset-1"))
	require.NoError(t, client.DeleteSeedanceAssetGroup(context.Background(), "group-1"))
	require.NoError(t, client.UpdateSeedanceAssetGroup(context.Background(), "group-1", "Renamed", &struct{}{}))

	require.Len(t, requests, 4)
	require.Equal(t, map[string]string{"Id": "asset-1"}, requests[0].body)
	require.Equal(t, map[string]string{"Id": "asset-1"}, requests[1].body)
	require.Equal(t, map[string]string{"Id": "group-1"}, requests[2].body)
	require.Equal(t, map[string]string{"Id": "group-1", "Name": "Renamed"}, requests[3].body)
}

// 本地落库失败时，补偿逻辑必须同时清理上游素材和已上传的 OSS 对象。
func TestRollbackSeedanceAssetCleansUpRemoteAndObject(t *testing.T) {
	ossClient := useFakePrivateAssetOSSStorage(t)
	var upstreamAction string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamAction = request.URL.Query().Get("Action")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	client := NewSeedanceAssetClient(&model.Channel{BaseURL: &baseURL, Key: "test-key"})
	objectKey := "private-assets/users/42/2026/09/07/asset.png"
	require.NoError(t, RollbackSeedanceAsset(context.Background(), client, "asset-rollback", objectKey))
	require.Equal(t, "DeleteAsset", upstreamAction)
	require.Equal(t, objectKey, ossClient.deletedKey)
}

func TestSeedanceAssetClientExtractsWrappedMediaResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"code":"success","data":{"status":"SUCCESS","data":{"content":{"video_url":"https://cdn.example/wrapped.mp4"}}}}`))
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	client := NewSeedanceAssetClient(&model.Channel{BaseURL: &baseURL, Key: "test-key"})

	status, previewURL, err := client.GetSeedanceAsset(context.Background(), "asset-wrapped")
	require.NoError(t, err)
	require.Equal(t, "SUCCESS", status)
	require.Equal(t, "https://cdn.example/wrapped.mp4", previewURL)
}

// 上游以 HTTP 200 返回业务失败时，客户端必须透传可读的失败原因。
func TestSeedanceAssetClientReturnsBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"code":"fail_to_fetch_task","message":"素材尚未同步到可用账号，请稍后重试"}`))
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	client := NewSeedanceAssetClient(&model.Channel{BaseURL: &baseURL, Key: "test-key"})
	_, _, err := client.GetSeedanceAsset(context.Background(), "asset-failed")
	require.EqualError(t, err, "seedance asset API request failed: 素材尚未同步到可用账号，请稍后重试")
}

// 上游请求必须绑定调用方上下文，避免浏览器取消后仍占用请求和连接资源。
func TestSeedanceAssetClientHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-release
		_, _ = writer.Write([]byte(`{"Result":{"Status":"Active"}}`))
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	client := NewSeedanceAssetClient(&model.Channel{BaseURL: &baseURL, Key: "test-key"})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, err := client.GetSeedanceAsset(ctx, "asset-cancelled")
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled upstream request did not return")
	}
}

// 审核状态进入终态后必须停止轮询，处理中状态则按固定上限退避。
func TestSeedanceAssetPollScheduleStopsAtTerminalState(t *testing.T) {
	attempts, nextPollAt := SeedanceAssetPollSchedule("SUCCESS", 3, 100)
	require.Equal(t, 0, attempts)
	require.Equal(t, int64(0), nextPollAt)

	attempts, nextPollAt = SeedanceAssetPollSchedule("Processing", 1, 100)
	require.Equal(t, 2, attempts)
	require.Equal(t, int64(110), nextPollAt)

	attempts, nextPollAt = SeedanceAssetPollSchedule("pending", 99, 100)
	require.Equal(t, 6, attempts)
	require.Equal(t, int64(160), nextPollAt)
}

// 后台轮询必须把审核终态写回本地，并在终态后停止下一轮调度。
func TestRunSeedanceAssetPollingOnceUpdatesTerminalAsset(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAsset{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	var upstreamAction string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamAction = request.URL.Query().Get("Action")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"Result":{"Status":"Active","URL":"https://cdn.example/asset.mp4"}}`))
	}))
	t.Cleanup(server.Close)
	baseURL := server.URL
	channel := &model.Channel{Id: 902, Key: "test-key", BaseURL: &baseURL}
	require.NoError(t, db.Create(channel).Error)
	asset := &model.SeedanceAsset{
		UserID: 1, ChannelID: channel.Id, GroupID: "group-1", AssetID: "asset-poll-1",
		Name: "clip.mp4", AssetType: "Video", Status: "Processing",
	}
	require.NoError(t, db.Create(asset).Error)

	summary, err := RunSeedanceAssetPollingOnce(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, summary.Candidates)
	require.Equal(t, 1, summary.Updated)

	var current model.SeedanceAsset
	require.NoError(t, db.First(&current, asset.ID).Error)
	require.Equal(t, "Active", current.Status)
	require.Equal(t, "GetAsset", upstreamAction)
	require.Zero(t, current.PollAttempts)
	require.Zero(t, current.NextPollAt)
	require.Zero(t, current.PollLeaseUntil)
}

func TestStoreSeedanceAssetUploadPersistsInOSSUntilDeleted(t *testing.T) {
	client := useFakePrivateAssetOSSStorage(t)
	content := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	upload, err := StoreSeedanceAssetUpload(
		context.Background(),
		bytes.NewReader(content),
		"cover.png",
		"image/png",
		"",
		int64(len(content)),
		42,
	)
	require.NoError(t, err)
	require.Equal(t, "Image", upload.AssetType)
	require.Equal(t, int64(len(content)), upload.Size)
	require.True(t, strings.HasPrefix(upload.ObjectKey, "private-assets/users/42/"))
	require.True(t, strings.HasSuffix(upload.ObjectKey, ".png"))
	require.Equal(t, "https://signed.example/"+upload.ObjectKey, upload.URL)
	require.Equal(t, content, client.putBody)
	require.Equal(t, "image/png", *client.putRequest.ContentType)

	require.NoError(t, RemoveSeedanceAssetObject(context.Background(), upload.ObjectKey))
	require.Equal(t, upload.ObjectKey, client.deletedKey)
}

// OSS 上传返回错误时也必须清理可能已经写入的对象，避免取消或断网形成孤儿文件。
func TestStoreSeedanceAssetUploadCleansUpAfterPutFailure(t *testing.T) {
	client := useFakePrivateAssetOSSStorage(t)
	client.putErr = fmt.Errorf("upload interrupted")
	_, err := StoreSeedanceAssetUpload(
		context.Background(),
		bytes.NewReader([]byte("image-bytes")),
		"cover.png",
		"image/png",
		"",
		int64(len("image-bytes")),
		42,
	)
	require.Error(t, err)
	require.NotEmpty(t, client.deletedKey)
}

func TestStoreSeedanceAssetUploadRejectsDeclaredTypeMismatch(t *testing.T) {
	_, err := StoreSeedanceAssetUpload(
		context.Background(),
		bytes.NewReader([]byte("image-bytes")),
		"cover.png",
		"image/png",
		"Video",
		int64(len("image-bytes")),
		1,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match")
}

// progressive JPEG 必须在提交上游前转为兼容的 baseline 编码。
func TestStoreSeedanceAssetUploadNormalizesProgressiveJPEG(t *testing.T) {
	client := useFakePrivateAssetOSSStorage(t)
	progressiveJPEG, err := base64.StdEncoding.DecodeString("/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAMCAgMCAgMDAwMEAwMEBQgFBQQEBQoHBwYIDAoMDAsKCwsNDhIQDQ4RDgsLEBYQERMUFRUVDA8XGBYUGBIUFRT/2wBDAQMEBAUEBQkFBQkUDQsNFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBT/wgARCAAIAAgDASIAAhEBAxEB/8QAFQABAQAAAAAAAAAAAAAAAAAAAAT/xAAUAQEAAAAAAAAAAAAAAAAAAAAH/9oADAMBAAIQAxAAAAGEKhf/AP/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAQUCf//EABQRAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQMBAT8Bf//EABQRAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQIBAT8Bf//EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEABj8Cf//EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAT8hf//aAAwDAQACAAMAAAAQ8//EABQRAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQMBAT8Qf//EABQRAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQIBAT8Qf//EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAT8Qf//Z")
	require.NoError(t, err)
	progressive, err := isProgressiveJPEG(progressiveJPEG)
	require.NoError(t, err)
	require.True(t, progressive)

	upload, err := StoreSeedanceAssetUpload(
		context.Background(),
		bytes.NewReader(progressiveJPEG),
		"progressive.jpg",
		"image/jpeg",
		"Image",
		int64(len(progressiveJPEG)),
		1,
	)
	require.NoError(t, err)
	progressive, err = isProgressiveJPEG(client.putBody)
	require.NoError(t, err)
	require.False(t, progressive)
	decoded, err := jpeg.Decode(bytes.NewReader(client.putBody))
	require.NoError(t, err)
	require.Equal(t, 8, decoded.Bounds().Dx())
	require.Equal(t, 8, decoded.Bounds().Dy())
	require.Equal(t, int64(len(client.putBody)), upload.Size)
}
