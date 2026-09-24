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
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

type fakePrivateAssetOSSClient struct {
	putRequest *oss.PutObjectRequest
	putBody    []byte
	deletedKey string
	putErr     error
	deleteErr  error
}

// 渠道选择必须越过满页的非 Doubao 插件，并跳过所有密钥禁用的账号。
func TestFindSeedanceAssetChannelSupportsPluginBeyondFirstPage(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	previous := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previous; assert.NoError(t, sqlDB.Close()) })
	baseURL, otherPlugin, doubaoPlugin := "https://example.test", `{"task_plugin_key":"other"}`, `{"task_plugin_key":"doubao"}`
	channels := make([]model.Channel, 100)
	for index := range channels {
		channels[index] = model.Channel{Id: index + 2, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "fixture", BaseURL: &baseURL, Setting: &otherPlugin}
	}
	require.NoError(t, db.Create(&channels).Error)
	disabled := model.Channel{Id: 102, Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "disabled", BaseURL: &baseURL,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled}}}
	require.NoError(t, db.Create(&disabled).Error)
	// 普通方舟文本渠道不具备旧代理的素材 Action 能力，不能抢占真正素材渠道。
	textChannel := model.Channel{Id: 1, Type: constant.ChannelTypeVolcEngine, Status: common.ChannelStatusEnabled, Key: "fixture", BaseURL: &baseURL}
	require.NoError(t, db.Create(&textChannel).Error)
	wanted := model.Channel{Id: 103, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "fixture", BaseURL: &baseURL, Setting: &doubaoPlugin}
	require.NoError(t, db.Create(&wanted).Error)
	channel, err := FindSeedanceAssetChannel()
	require.NoError(t, err)
	assert.Equal(t, wanted.Id, channel.Id)
	_, err = NewSeedanceAssetClient(channel)
	require.NoError(t, err)
	_, err = NewSeedanceAssetClient(&channels[0])
	assert.Error(t, err)
	volcengine := wanted
	volcengine.Type = constant.ChannelTypeVolcEngine
	_, err = NewSeedanceAssetClient(&volcengine)
	assert.Error(t, err)
}

func TestPollingResolvesDisabledSourceChannelToMatchingEnabledAlias(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAsset{}))
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; assert.NoError(t, sqlDB.Close()) })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
	}))
	t.Cleanup(upstream.Close)
	baseURL, key := upstream.URL, "same-upstream-key"
	source := &model.Channel{Id: 1, Type: constant.ChannelTypeDoubaoVideo, Key: key, Status: common.ChannelStatusEnabled, BaseURL: &baseURL}
	sourceClient, err := NewSeedanceAssetClient(source)
	require.NoError(t, err)
	source.Status = common.ChannelStatusManuallyDisabled
	alias := *source
	alias.Id = 2
	alias.Status = common.ChannelStatusEnabled
	require.NoError(t, db.Create(source).Error)
	require.NoError(t, db.Create(&alias).Error)

	asset := &model.SeedanceAsset{
		UserID: 7, ChannelID: source.Id, GroupID: "group", AssetID: "asset", Name: "portrait.jpg",
		AssetType: "Image", Status: "Processing", KeyFingerprint: sourceClient.KeyFingerprint,
		AccountFingerprint: sourceClient.AccountFingerprint,
	}
	require.NoError(t, db.Create(asset).Error)
	client, err := ResolveSeedanceAssetClient(context.Background(), source.Id, asset.KeyFingerprint, asset.AccountFingerprint)
	require.NoError(t, err)
	assert.Equal(t, alias.Id, client.ChannelID)

	result := pollSeedanceAsset(context.Background(), asset)
	require.NoError(t, result.err)
	assert.True(t, result.claimed)
	assert.True(t, result.updated)
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, "Active", asset.Status)

	assert.NoError(t, db.Model(&model.Channel{}).Where("id = ?", alias.Id).Update("key", "different-account-key").Error)
	_, err = ResolveSeedanceAssetClient(context.Background(), source.Id, asset.KeyFingerprint, asset.AccountFingerprint)
	assert.Error(t, err, "a channel with a different account credential must not be used as an alias")
}

// 地址检查只使用字面 IP，确保拒绝私网和非 HTTP 地址且不依赖外部 DNS。
func TestSeedanceAssetSourceURLRejectsUnsafeTargets(t *testing.T) {
	settings := system_setting.GetFetchSetting()
	previous := *settings
	*settings = system_setting.FetchSetting{EnableSSRFProtection: true, AllowedPorts: []string{"80", "443"}, ApplyIPFilterForDomain: true}
	t.Cleanup(func() { *settings = previous })
	for _, address := range []string{"http://127.0.0.1/a", "http://169.254.169.254/latest", "https://[::1]/a", "file:///tmp/a", "https://user:pass@8.8.8.8/a", "javascript:alert(1)"} {
		assert.Error(t, ValidateSeedanceAssetSourceURL(address), address)
	}
	assert.NoError(t, ValidateSeedanceAssetSourceURL("https://8.8.8.8/file.png"))
}

// 上游重定向不能触发第二个网络请求，凭证回显不能进入业务错误。
func TestSeedanceAssetClientRejectsRedirectAndRedactsKey(t *testing.T) {
	var redirected bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected = true }))
	t.Cleanup(destination.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "GetAsset" {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"code":"Forbidden","message":"credential fixture-private-key rejected"}`)
	}))
	t.Cleanup(upstream.Close)
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "fixture-private-key", BaseURL: &upstream.URL})
	require.NoError(t, err)
	_, _, err = client.GetSeedanceAsset(t.Context(), "asset")
	assert.ErrorContains(t, err, "307")
	assert.False(t, redirected)
	err = client.DeleteSeedanceAsset(t.Context(), "asset")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fixture-private-key")
}

// 每类上游写操作都必须识别业务失败，仅明确的资源不存在允许幂等删除。
func TestSeedanceMutationsValidateBusinessResponses(t *testing.T) {
	for _, body := range []string{
		`{"code":"operation_failed","message":"rejected"}`,
		`{"ResponseMetadata":{"Error":{"Code":"Forbidden","Message":"rejected"}}}`,
		`{"success":false,"message":"rejected"}`,
		`{"error":{"code":"upstream_failure","message":"rejected","type":"server_error"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			t.Cleanup(server.Close)
			client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &server.URL, Key: "fixture-key", Status: common.ChannelStatusEnabled})
			require.NoError(t, err)
			assert.ErrorContains(t, client.CreateSeedanceAssetGroup(t.Context(), "group", "LivenessFace", &struct{}{}), "rejected")
			assert.ErrorContains(t, client.DeleteSeedanceAsset(t.Context(), "asset"), "rejected")
			assert.ErrorContains(t, client.DeleteSeedanceAssetGroup(t.Context(), "group"), "rejected")
			assert.ErrorContains(t, client.UpdateSeedanceAssetGroup(t.Context(), "group", "renamed", &struct{}{}), "rejected")
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"ResponseMetadata":{"Error":{"Code":"ResourceNotFound.Asset","Message":"already deleted"}}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &server.URL, Key: "fixture-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)
	assert.NoError(t, client.DeleteSeedanceAsset(t.Context(), "asset"))
	assert.NoError(t, client.DeleteSeedanceAssetGroup(t.Context(), "group"))
	_, _, err = client.GetSeedanceAsset(t.Context(), "asset")
	assert.ErrorIs(t, err, ErrSeedanceAssetNotFound)
}

// 非 2xx 响应也必须保留上游正文，不能只返回无上下文的状态码。
func TestSeedanceAssetClientPreservesHTTPErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"upstream_failure","message":"LivenessFace is unavailable","type":"server_error"}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &server.URL, Key: "fixture-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)
	err = client.CreateSeedanceAssetGroup(t.Context(), "group", "LivenessFace", &struct{}{})
	assert.ErrorContains(t, err, "LivenessFace is unavailable")
}

// 创建素材组的类型必须进入上游请求，空值则保持上游默认行为。
func TestCreateSeedanceAssetGroupForwardsGroupType(t *testing.T) {
	var received map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = map[string]string{}
		if err := common.DecodeJson(r.Body, &received); err != nil {
			t.Errorf("decode CreateAssetGroup request: %v", err)
		}
		_, _ = io.WriteString(w, `{"Result":{"Id":"group-created"}}`)
	}))
	t.Cleanup(server.Close)
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &server.URL, Key: "fixture-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)
	var response struct {
		Result struct {
			ID string `json:"Id"`
		} `json:"Result"`
	}
	require.NoError(t, client.CreateSeedanceAssetGroup(t.Context(), "group", "LivenessFace", &response))
	assert.Equal(t, "group", received["Name"])
	assert.Equal(t, "LivenessFace", received["GroupType"])
	assert.Equal(t, "group-created", response.Result.ID)

	require.NoError(t, client.CreateSeedanceAssetGroup(t.Context(), "group", "", &response))
	assert.NotContains(t, received, "GroupType")
}

type seedanceDeadlineTransport struct {
	deadline    time.Time
	hasDeadline bool
}

// 捕获真实请求边界的截止时间，避免用等待三十秒的计时测试验证超时配置。
func (transport *seedanceDeadlineTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.deadline, transport.hasDeadline = request.Context().Deadline()
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"Result":{"Status":"Active"}}`))}, nil
}

func TestSeedanceProxyRequestHasIndependentDeadline(t *testing.T) {
	proxyURL := "http://127.0.0.1:9878"
	proxyClient, err := GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)
	previousTransport, previousTimeout := proxyClient.Transport, proxyClient.Timeout
	transport := &seedanceDeadlineTransport{}
	proxyClient.Transport, proxyClient.Timeout = transport, 0
	t.Cleanup(func() { proxyClient.Transport, proxyClient.Timeout = previousTransport, previousTimeout })
	baseURL, settings := "https://example.test", `{"proxy":"http://127.0.0.1:9878"}`
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &baseURL, Setting: &settings, Key: "fixture-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)
	_, _, err = client.GetSeedanceAsset(t.Context(), "asset")
	require.NoError(t, err)
	assert.True(t, transport.hasDeadline)
	assert.WithinDuration(t, time.Now().Add(seedanceAssetRequestTimeout), transport.deadline, time.Second)
}

// 密钥列表重排不能切换既有素材组的上游账号；被禁用或移除的绑定必须明确失败。
func TestSeedanceClientBindsSelectedAccountAcrossKeyReordering(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
	}))
	t.Cleanup(server.Close)
	channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &server.URL, Key: "disabled-key\nselected-key", Status: common.ChannelStatusEnabled,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled}}}
	client, err := NewSeedanceAssetClient(channel)
	require.NoError(t, err)
	_, _, err = client.GetSeedanceAsset(t.Context(), "asset")
	require.NoError(t, err)
	assert.Equal(t, "Bearer selected-key", authorization)
	channel.Key = "selected-key\nother-key"
	channel.ChannelInfo.MultiKeyStatusList = nil
	bound, err := NewSeedanceAssetClient(channel, client.KeyFingerprint)
	require.NoError(t, err)
	_, _, err = bound.GetSeedanceAsset(t.Context(), "asset")
	require.NoError(t, err)
	assert.Equal(t, "Bearer selected-key", authorization)
	channel.ChannelInfo.MultiKeyStatusList = map[int]int{0: common.ChannelStatusAutoDisabled}
	_, err = NewSeedanceAssetClient(channel, client.KeyFingerprint)
	assert.Error(t, err)
}

// 预览和删除使用对象保存的原位置，当前全局 Bucket 改变不能改变签名目标。
func TestPrivateAssetOSSLocationSurvivesSettingsChange(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	previous := common.OptionMap
	common.OptionMap = system_setting.PrivateAssetOSSOptionValues(system_setting.PrivateAssetOSSSettings{
		Region: "cn-shanghai", Bucket: "new-bucket", Prefix: "private-assets/", AccessKeyID: "fixture-id", AccessKeySecret: "fixture-secret",
	})
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() { common.OptionMapRWMutex.Lock(); common.OptionMap = previous; common.OptionMapRWMutex.Unlock() })
	location := model.SeedanceAssetStorage{Region: "cn-hangzhou", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com", Bucket: "original-bucket"}
	storage, err := NewPrivateAssetOSSStorage(location)
	require.NoError(t, err)
	signedURL, err := storage.PrivateAssetPreviewURL(t.Context(), "private-assets/file.png")
	require.NoError(t, err)
	assert.Contains(t, signedURL, "original-bucket.oss-cn-hangzhou.aliyuncs.com")
	assert.NotContains(t, signedURL, "new-bucket")
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
	if client.deleteErr != nil {
		return nil, client.deleteErr
	}
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
	privateAssetOSSStorageFactory = func(...model.SeedanceAssetStorage) (*PrivateAssetOSSStorage, error) {
		return &PrivateAssetOSSStorage{
			client:   client,
			bucket:   "test-private-assets",
			prefix:   "private-assets/",
			location: model.SeedanceAssetStorage{Region: "test-region", Endpoint: "https://oss.example.com", Bucket: "test-private-assets"},
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
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &baseURL, Key: "test-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)

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
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &baseURL, Key: "test-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)
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
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &baseURL, Key: "test-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)

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
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &baseURL, Key: "test-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)
	_, _, err = client.GetSeedanceAsset(context.Background(), "asset-failed")
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
	client, err := NewSeedanceAssetClient(&model.Channel{Type: constant.ChannelTypeDoubaoVideo, BaseURL: &baseURL, Key: "test-key", Status: common.ChannelStatusEnabled})
	require.NoError(t, err)
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

// 取消时不能返回成功摘要，也不能继续访问数据库或上游。
func TestSeedanceAssetPollingReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := RunSeedanceAssetPollingOnce(ctx, nil)
	assert.ErrorIs(t, err, context.Canceled)
}

// 后台轮询必须把审核终态写回本地，并在终态后停止下一轮调度。
func TestRunSeedanceAssetPollingOnceUpdatesTerminalAsset(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{Type: constant.ChannelTypeDoubaoVideo}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}))
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
	channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Id: 902, Key: "test-key", BaseURL: &baseURL}
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

// 可定位的视频流应直接写入 OSS，避免先复制到第二份临时文件。
func TestStoreSeedanceAssetUploadStreamsSeekableVideo(t *testing.T) {
	client := useFakePrivateAssetOSSStorage(t)
	content := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'm', 'p', '4', '2'}
	upload, err := StoreSeedanceAssetUpload(
		context.Background(),
		bytes.NewReader(content),
		"clip.mp4",
		"video/mp4",
		"",
		int64(len(content)),
		42,
	)
	require.NoError(t, err)
	require.Equal(t, "Video", upload.AssetType)
	assert.Equal(t, int64(len(content)), upload.Size)
	assert.Equal(t, content, client.putBody)
	assert.Equal(t, "video/mp4", *client.putRequest.ContentType)
}

// 上传完成后即使全局工厂已切换，补偿也必须使用原客户端删除原 Bucket 中的对象。
func TestSeedanceUploadRollbackKeepsOriginalStorageClient(t *testing.T) {
	original := useFakePrivateAssetOSSStorage(t)
	content := []byte{137, 80, 78, 71, 13, 10, 26, 10}
	upload, err := StoreSeedanceAssetUpload(t.Context(), bytes.NewReader(content), "cover.png", "image/png", "", int64(len(content)), 42)
	require.NoError(t, err)
	replacement := useFakePrivateAssetOSSStorage(t)
	client := &SeedanceAssetClient{}
	require.NoError(t, RollbackSeedanceAsset(t.Context(), client, "", upload.ObjectKey, upload))
	assert.Equal(t, upload.ObjectKey, original.deletedKey)
	assert.Empty(t, replacement.deletedKey)
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

func TestMapSeedanceAssetsToSelectedAccountCreatesAndReusesReplica(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.SystemTask{},
		&model.SeedanceAssetGroup{},
		&model.SeedanceAssetGroupReplica{},
		&model.SeedanceAsset{},
		&model.SeedanceAssetReplica{},
	))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	fetchSettings := system_setting.GetFetchSetting()
	previousFetchSettings := *fetchSettings
	*fetchSettings = system_setting.FetchSetting{}
	t.Cleanup(func() { *fetchSettings = previousFetchSettings })
	useFakePrivateAssetOSSStorage(t)

	group := &model.SeedanceAssetGroup{
		UserID: 7, ChannelID: 11, GroupID: "source-group", Name: "Originals", GroupType: "LivenessFace",
		Status: "Active", KeyFingerprint: "source-key", AccountFingerprint: "source-account",
	}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{
		UserID: group.UserID, ChannelID: group.ChannelID, GroupID: group.GroupID, AssetID: "source-asset",
		Name: "portrait.jpg", AssetType: "Image", Status: "Active", ObjectKey: "private-assets/users/7/portrait.jpg",
		Storage:        model.SeedanceAssetStorage{Region: "cn-hangzhou", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com", Bucket: "private-assets"},
		KeyFingerprint: group.KeyFingerprint, AccountFingerprint: group.AccountFingerprint,
	}
	require.NoError(t, model.CreateSeedanceAssetInGroup(t.Context(), asset))

	var actions []string
	var groupTypes []string
	var sourceURLs []string
	assetPolls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]string
		if err := common.DecodeJson(request.Body, &payload); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		action := request.URL.Query().Get("Action")
		apiKey := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		actions = append(actions, apiKey+":"+action)
		writer.Header().Set("Content-Type", "application/json")
		switch action {
		case "CreateAssetGroup":
			groupTypes = append(groupTypes, payload["GroupType"])
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"remote-group-%s"}}`, apiKey)
		case "CreateAsset":
			sourceURLs = append(sourceURLs, payload["URL"])
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"remote-asset-%s"}}`, apiKey)
		case "GetAsset":
			assetPolls[apiKey]++
			status := "Active"
			if apiKey == "key-a" && assetPolls[apiKey] == 1 {
				status = "Processing"
			}
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"%s","Status":"%s"}}`, payload["Id"], status)
		default:
			http.Error(writer, "unexpected Action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	newClient := func(id int, key string) *SeedanceAssetClient {
		channel := &model.Channel{Id: id, Type: constant.ChannelTypeDoubaoVideo, Key: key, Status: common.ChannelStatusEnabled, BaseURL: &server.URL}
		client, clientErr := NewSeedanceAssetClientForSelectedKey(channel, key)
		require.NoError(t, clientErr)
		return client
	}
	clientA := newClient(21, "key-a")
	_, err = MapSeedanceAssetsToAccount(t.Context(), 8, clientA, []string{asset.AssetID})
	require.ErrorIs(t, err, model.ErrSeedanceAssetUnavailable)
	assert.Empty(t, actions, "an asset owned by another user must never reach the upstream")

	_, err = MapSeedanceAssetsToAccount(t.Context(), group.UserID, clientA, []string{asset.AssetID})
	require.ErrorIs(t, err, ErrSeedanceAssetMappingPending)

	var replica model.SeedanceAssetReplica
	require.NoError(t, db.Where("local_asset_id = ?", asset.ID).First(&replica).Error)
	require.Equal(t, model.SeedanceAssetReplicaReady, replica.ProvisionStatus)
	require.Equal(t, "Processing", replica.Status)
	require.NoError(t, db.Model(&replica).Update("next_poll_at", int64(0)).Error)

	mapping, err := MapSeedanceAssetsToAccount(t.Context(), group.UserID, clientA, []string{asset.AssetID})
	require.NoError(t, err)
	require.Equal(t, "remote-asset-key-a", mapping[asset.AssetID])
	_, err = MapSeedanceAssetsToAccount(t.Context(), group.UserID, clientA, []string{asset.AssetID})
	require.NoError(t, err)
	require.Equal(t, []string{
		"key-a:CreateAssetGroup", "key-a:CreateAsset", "key-a:GetAsset", "key-a:GetAsset",
	}, actions)

	clientAAlias := newClient(23, "key-a")
	aliasMapping, err := MapSeedanceAssetsToAccount(t.Context(), group.UserID, clientAAlias, []string{asset.AssetID})
	require.NoError(t, err)
	assert.Equal(t, mapping, aliasMapping)
	var groupReplica model.SeedanceAssetGroupReplica
	require.NoError(t, db.Where("local_group_id = ? AND account_fingerprint = ?", group.ID, clientA.AccountFingerprint).First(&groupReplica).Error)
	assert.Equal(t, clientAAlias.ChannelID, groupReplica.ChannelID)
	require.NoError(t, db.Where("local_asset_id = ? AND account_fingerprint = ?", asset.ID, clientA.AccountFingerprint).First(&replica).Error)
	assert.Equal(t, clientAAlias.ChannelID, replica.ChannelID)
	assert.Equal(t, 4, len(actions), "reusing an account alias must not import the asset again")

	clientB := newClient(22, "key-b")
	otherMapping, err := MapSeedanceAssetsToAccount(t.Context(), group.UserID, clientB, []string{asset.AssetID})
	require.NoError(t, err)
	require.Equal(t, "remote-asset-key-b", otherMapping[asset.AssetID])
	require.Equal(t, []string{"LivenessFace", "LivenessFace"}, groupTypes)
	require.Len(t, sourceURLs, 2)
	assert.Equal(t, "https://signed.example/"+asset.ObjectKey, sourceURLs[0])
	assert.Equal(t, sourceURLs[0], sourceURLs[1])

	primaryClient := newClient(31, "primary-key")
	primaryGroup := &model.SeedanceAssetGroup{
		UserID: 8, ChannelID: 30, GroupID: "primary-group", Name: "Primary",
		Status: "Active", KeyFingerprint: primaryClient.KeyFingerprint,
		AccountFingerprint: primaryClient.AccountFingerprint,
	}
	require.NoError(t, db.Create(primaryGroup).Error)
	primaryAsset := &model.SeedanceAsset{
		UserID: primaryGroup.UserID, ChannelID: primaryGroup.ChannelID,
		GroupID: primaryGroup.GroupID, AssetID: "primary-asset", Name: "primary.png",
		AssetType: "Image", Status: "Active", SourceURL: "https://8.8.8.8/primary.png",
		KeyFingerprint: primaryGroup.KeyFingerprint, AccountFingerprint: primaryGroup.AccountFingerprint,
	}
	require.NoError(t, model.CreateSeedanceAssetInGroup(t.Context(), primaryAsset))
	primaryAlias := newClient(32, "primary-key")
	primaryMapping, err := MapSeedanceAssetsToAccount(t.Context(), primaryGroup.UserID, primaryAlias, []string{primaryAsset.AssetID})
	require.NoError(t, err)
	assert.Equal(t, primaryAsset.AssetID, primaryMapping[primaryAsset.AssetID])
	require.NoError(t, db.First(primaryGroup, primaryGroup.ID).Error)
	require.NoError(t, db.First(primaryAsset, primaryAsset.ID).Error)
	assert.Equal(t, primaryAlias.ChannelID, primaryGroup.ChannelID)
	assert.Equal(t, primaryAlias.ChannelID, primaryAsset.ChannelID)
	assert.Equal(t, primaryAlias.AccountFingerprint, primaryAsset.AccountFingerprint)

	var groupReplicaCount, assetReplicaCount int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroupReplica{}).Count(&groupReplicaCount).Error)
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Count(&assetReplicaCount).Error)
	assert.EqualValues(t, 2, groupReplicaCount)
	assert.EqualValues(t, 2, assetReplicaCount)
}

func TestMapSeedanceAssetsToSelectedAccountHandlesMultipleAssetsInOneRequest(t *testing.T) {
	previousDB := model.DB
	databaseEngine := "sqlite"
	maxConnections := 1
	var dialector gorm.Dialector
	mysqlDSN := os.Getenv("SEEDANCE_ASSET_LOAD_TEST_MYSQL_DSN")
	postgresDSN := os.Getenv("SEEDANCE_ASSET_LOAD_TEST_POSTGRES_DSN")
	if mysqlDSN != "" && postgresDSN != "" {
		t.Fatal("set only one SEEDANCE_ASSET_LOAD_TEST database DSN at a time")
	}
	if mysqlDSN != "" {
		databaseEngine = "mysql"
		maxConnections = 16
		dialector = mysql.Open(mysqlDSN)
	} else if postgresDSN != "" {
		databaseEngine = "postgres"
		maxConnections = 16
		dialector = postgres.Open(postgresDSN)
	} else {
		dialector = sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_")))
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("sam%d_", time.Now().UnixNano())},
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(maxConnections)
	sqlDB.SetMaxIdleConns(maxConnections)
	require.NoError(t, db.AutoMigrate(
		&model.SystemTask{},
		&model.SeedanceAssetCleanupJob{},
		&model.SeedanceAssetGroup{},
		&model.SeedanceAssetGroupReplica{},
		&model.SeedanceAsset{},
		&model.SeedanceAssetReplica{},
	))
	var systemTaskQueries atomic.Int64
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("seedance_asset_mapping_count_system_task_queries", func(tx *gorm.DB) {
		if strings.HasSuffix(tx.Statement.Table, "_system_tasks") {
			systemTaskQueries.Add(1)
		}
	}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		assert.NoError(t, db.Migrator().DropTable(
			&model.SeedanceAssetReplica{}, &model.SeedanceAsset{},
			&model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetGroup{},
			&model.SeedanceAssetCleanupJob{}, &model.SystemTask{},
		))
		_ = sqlDB.Close()
	})

	fetchSettings := system_setting.GetFetchSetting()
	previousFetchSettings := *fetchSettings
	*fetchSettings = system_setting.FetchSetting{}
	t.Cleanup(func() { *fetchSettings = previousFetchSettings })

	var ossClientBuilds atomic.Int64
	ossClient := &fakePrivateAssetOSSClient{}
	previousFactory := privateAssetOSSStorageFactory
	privateAssetOSSStorageFactory = func(...model.SeedanceAssetStorage) (*PrivateAssetOSSStorage, error) {
		ossClientBuilds.Add(1)
		return &PrivateAssetOSSStorage{
			client:   ossClient,
			bucket:   "test-private-assets",
			prefix:   "private-assets/",
			location: model.SeedanceAssetStorage{Region: "test-region", Endpoint: "https://oss.example.com", Bucket: "test-private-assets"},
		}, nil
	}
	t.Cleanup(func() { privateAssetOSSStorageFactory = previousFactory })

	group := &model.SeedanceAssetGroup{
		UserID: 41, ChannelID: 42, GroupID: "multi-source-group", Name: "Multi asset request",
		Status: "Active", KeyFingerprint: "source-key", AccountFingerprint: "source-account",
	}
	require.NoError(t, db.Create(group).Error)
	secondGroup := &model.SeedanceAssetGroup{
		UserID: group.UserID, ChannelID: group.ChannelID, GroupID: "second-source-group", Name: "Second group",
		Status: "Active", KeyFingerprint: group.KeyFingerprint, AccountFingerprint: group.AccountFingerprint,
	}
	require.NoError(t, db.Create(secondGroup).Error)
	groups := []*model.SeedanceAssetGroup{group, secondGroup}
	assets := make([]model.SeedanceAsset, 4)
	assetIDs := make([]string, 0, len(assets)+1)
	for index := range assets {
		assetGroup := groups[index%len(groups)]
		assets[index] = model.SeedanceAsset{
			UserID: group.UserID, ChannelID: group.ChannelID, GroupID: assetGroup.GroupID,
			AssetID: fmt.Sprintf("multi-source-asset-%d", index), Name: fmt.Sprintf("asset-%d.jpg", index),
			AssetType: "Image", Status: "Active", ObjectKey: fmt.Sprintf("private-assets/users/41/asset-%d.jpg", index),
			Storage:        model.SeedanceAssetStorage{Region: "test-region", Endpoint: "https://oss.example.com", Bucket: "test-private-assets"},
			KeyFingerprint: group.KeyFingerprint, AccountFingerprint: group.AccountFingerprint,
		}
		require.NoError(t, model.CreateSeedanceAssetInGroup(t.Context(), &assets[index]))
		assetIDs = append(assetIDs, assets[index].AssetID)
	}
	assetIDs = append(assetIDs, assets[0].AssetID)

	var groupCreates, assetCreates, assetGets atomic.Int64
	var activeHTTPRequests, peakHTTPRequests atomic.Int64
	allAssetCreatesStarted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		active := activeHTTPRequests.Add(1)
		for peak := peakHTTPRequests.Load(); active > peak && !peakHTTPRequests.CompareAndSwap(peak, active); peak = peakHTTPRequests.Load() {
		}
		defer activeHTTPRequests.Add(-1)
		var payload map[string]string
		if err := common.DecodeJson(request.Body, &payload); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			groupID := groupCreates.Add(1)
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"multi-remote-group-%d"}}`, groupID)
		case "CreateAsset":
			assetID := assetCreates.Add(1)
			if !strings.HasPrefix(payload["GroupId"], "multi-remote-group-") {
				http.Error(writer, "asset was assigned to an unexpected group", http.StatusBadRequest)
				return
			}
			if assetID == int64(len(assets)) {
				close(allAssetCreatesStarted)
			}
			select {
			case <-allAssetCreatesStarted:
			case <-request.Context().Done():
				return
			}
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"multi-remote-asset-%d"}}`, assetID)
		case "GetAsset":
			assetGets.Add(1)
			status := "Active"
			if request.Header.Get("Authorization") == "Bearer multi-key-pending" {
				status = "Processing"
			}
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":%q,"Status":%q}}`, payload["Id"], status)
		case "DeleteAsset":
			_, _ = writer.Write([]byte(`{"Result":{}}`))
		default:
			http.Error(writer, "unexpected Action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	channel := &model.Channel{Id: 43, Type: constant.ChannelTypeDoubaoVideo, Key: "multi-key", Status: common.ChannelStatusEnabled, BaseURL: &server.URL}
	client, err := NewSeedanceAssetClientForSelectedKey(channel, channel.Key)
	require.NoError(t, err)

	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	mapping, err := MapSeedanceAssetsToAccount(ctx, group.UserID, client, assetIDs)
	require.NoError(t, err)
	firstRequestDuration := time.Since(startedAt)
	require.Len(t, mapping, len(assets))
	uniqueMappings := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		upstreamID := mapping[asset.AssetID]
		assert.True(t, strings.HasPrefix(upstreamID, "multi-remote-asset-"))
		uniqueMappings[upstreamID] = struct{}{}
	}
	assert.Len(t, uniqueMappings, len(assets))
	assert.EqualValues(t, len(groups), groupCreates.Load())
	assert.EqualValues(t, len(assets), assetCreates.Load())
	assert.EqualValues(t, len(assets), assetGets.Load())
	assert.EqualValues(t, seedanceAssetMappingWorkerSize, peakHTTPRequests.Load(), "a multi-asset request should run up to its bounded worker count")
	assert.EqualValues(t, 1, ossClientBuilds.Load(), "one OSS client should be reused for assets in the same storage location")

	reusedMapping, err := MapSeedanceAssetsToAccount(t.Context(), group.UserID, client, assetIDs)
	require.NoError(t, err)
	assert.Equal(t, mapping, reusedMapping)
	assert.EqualValues(t, len(groups), groupCreates.Load())
	assert.EqualValues(t, len(assets), assetCreates.Load())
	assert.EqualValues(t, len(assets), assetGets.Load())
	assert.EqualValues(t, 1, ossClientBuilds.Load())
	assert.Zero(t, systemTaskQueries.Load())

	pendingChannel := &model.Channel{Id: 44, Type: constant.ChannelTypeDoubaoVideo, Key: "multi-key-pending", Status: common.ChannelStatusEnabled, BaseURL: &server.URL}
	pendingClient, err := NewSeedanceAssetClientForSelectedKey(pendingChannel, pendingChannel.Key)
	require.NoError(t, err)
	_, err = MapSeedanceAssetsToAccount(ctx, group.UserID, pendingClient, assetIDs)
	require.ErrorIs(t, err, ErrSeedanceAssetMappingPending)
	assert.EqualValues(t, len(groups)*2, groupCreates.Load())
	assert.EqualValues(t, len(assets)*2, assetCreates.Load())
	assert.EqualValues(t, len(assets)*2, assetGets.Load())
	assert.EqualValues(t, 2, ossClientBuilds.Load())
	assert.EqualValues(t, 1, systemTaskQueries.Load(), "one batch should make one active-task lookup when polling is needed")

	var groupReplicaCount, assetReplicaCount, activePollTasks int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroupReplica{}).Count(&groupReplicaCount).Error)
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Count(&assetReplicaCount).Error)
	require.NoError(t, db.Model(&model.SystemTask{}).Where("type = ? AND status = ?", model.SystemTaskTypeSeedanceAssetPoll, model.SystemTaskStatusPending).Count(&activePollTasks).Error)
	assert.EqualValues(t, len(groups)*2, groupReplicaCount)
	assert.EqualValues(t, len(assets)*2, assetReplicaCount)
	assert.EqualValues(t, 1, activePollTasks)
	t.Logf("multi-asset request: db=%s assets=%d unique_assets=%d upstream group/create/status=%d/%d/%d peak_upstream=%d OSS client constructions=%d first_request=%s",
		databaseEngine, len(assetIDs), len(assets), groupCreates.Load(), assetCreates.Load(), assetGets.Load(), peakHTTPRequests.Load(), ossClientBuilds.Load(), firstRequestDuration)
}

func TestSeedanceAssetReplicaBurstIsStableUnderConcurrentLoad(t *testing.T) {
	const (
		userCount   = 200
		concurrency = 64
	)
	databaseEngine := "sqlite"
	maxConnections := 1
	var dialector gorm.Dialector
	mysqlDSN := os.Getenv("SEEDANCE_ASSET_LOAD_TEST_MYSQL_DSN")
	postgresDSN := os.Getenv("SEEDANCE_ASSET_LOAD_TEST_POSTGRES_DSN")
	if mysqlDSN != "" && postgresDSN != "" {
		t.Fatal("set only one SEEDANCE_ASSET_LOAD_TEST database DSN at a time")
	}
	if mysqlDSN != "" {
		databaseEngine = "mysql"
		maxConnections = concurrency
		dialector = mysql.Open(mysqlDSN)
	} else if postgresDSN != "" {
		databaseEngine = "postgres"
		maxConnections = concurrency
		dialector = postgres.Open(postgresDSN)
	} else {
		dialector = sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_")))
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
		NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("sa%d_", time.Now().UnixNano())},
	})
	require.NoError(t, err)
	databaseVersion := ""
	if databaseEngine != "sqlite" {
		require.NoError(t, db.Raw("SELECT version()").Scan(&databaseVersion).Error)
	}
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(maxConnections)
	sqlDB.SetMaxIdleConns(maxConnections)
	require.NoError(t, db.AutoMigrate(
		&model.SystemTask{},
		&model.SeedanceAssetGroup{},
		&model.SeedanceAssetGroupReplica{},
		&model.SeedanceAsset{},
		&model.SeedanceAssetReplica{},
	))
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		assert.NoError(t, db.Migrator().DropTable(
			&model.SeedanceAssetReplica{}, &model.SeedanceAsset{},
			&model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetGroup{}, &model.SystemTask{},
		))
		assert.NoError(t, sqlDB.Close())
	})
	fetchSettings := system_setting.GetFetchSetting()
	previousFetchSettings := *fetchSettings
	*fetchSettings = system_setting.FetchSetting{}
	t.Cleanup(func() { *fetchSettings = previousFetchSettings })
	useFakePrivateAssetOSSStorage(t)

	assetIDs := make([]string, userCount)
	for index := range userCount {
		userID := index + 1
		groupID := fmt.Sprintf("source-group-%03d", index)
		group := &model.SeedanceAssetGroup{
			UserID: userID, ChannelID: 1, GroupID: groupID, Name: fmt.Sprintf("User %03d", index),
			Status: "Active", KeyFingerprint: fmt.Sprintf("source-key-%03d", index),
			AccountFingerprint: fmt.Sprintf("source-account-%03d", index),
		}
		require.NoError(t, db.Create(group).Error)
		assetIDs[index] = fmt.Sprintf("source-asset-%03d", index)
		asset := &model.SeedanceAsset{
			UserID: userID, ChannelID: group.ChannelID, GroupID: group.GroupID,
			AssetID: assetIDs[index], Name: fmt.Sprintf("asset-%03d.jpg", index),
			AssetType: "Image", Status: "Active", ObjectKey: fmt.Sprintf("private-assets/users/%d/source.jpg", userID),
			Storage:        model.SeedanceAssetStorage{Region: "test-region", Endpoint: "https://oss.example.com", Bucket: "test-private-assets"},
			KeyFingerprint: group.KeyFingerprint, AccountFingerprint: group.AccountFingerprint,
		}
		require.NoError(t, model.CreateSeedanceAssetInGroup(t.Context(), asset))
	}

	var groupCreates, assetCreates, assetGets, invalidRequests atomic.Int64
	var activeHTTPRequests, peakHTTPRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		active := activeHTTPRequests.Add(1)
		for peak := peakHTTPRequests.Load(); active > peak && !peakHTTPRequests.CompareAndSwap(peak, active); peak = peakHTTPRequests.Load() {
		}
		defer activeHTTPRequests.Add(-1)
		var payload map[string]string
		if err := common.DecodeJson(request.Body, &payload); err != nil {
			invalidRequests.Add(1)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		apiKey := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			groupCreates.Add(1)
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"remote-group-%s"}}`, apiKey)
		case "CreateAsset":
			assetCreates.Add(1)
			if !strings.HasPrefix(payload["URL"], "https://signed.example/") {
				invalidRequests.Add(1)
			}
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"remote-asset-%s"}}`, apiKey)
		case "GetAsset":
			assetGets.Add(1)
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":%q,"Status":"Active"}}`, payload["Id"])
		default:
			invalidRequests.Add(1)
			http.Error(writer, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)

	clients := make([]*SeedanceAssetClient, userCount)
	for index := range userCount {
		key := fmt.Sprintf("load-key-%03d", index)
		channel := &model.Channel{
			Id: index + 100, Type: constant.ChannelTypeDoubaoVideo, Key: key,
			Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL,
		}
		clients[index], err = NewSeedanceAssetClientForSelectedKey(channel, key)
		require.NoError(t, err)
	}

	var maximumWorkers atomic.Int64
	runBatch := func(count int, work func(int) (string, error)) ([]string, []error, time.Duration) {
		results := make([]string, count)
		errorsByIndex := make([]error, count)
		start := make(chan struct{})
		semaphore := make(chan struct{}, concurrency)
		var workers sync.WaitGroup
		var activeWorkers, peakWorkers atomic.Int64
		workers.Add(count)
		startedAt := time.Now()
		for index := range count {
			go func(index int) {
				defer workers.Done()
				semaphore <- struct{}{}
				<-start
				active := activeWorkers.Add(1)
				for peak := peakWorkers.Load(); active > peak && !peakWorkers.CompareAndSwap(peak, active); peak = peakWorkers.Load() {
				}
				results[index], errorsByIndex[index] = work(index)
				activeWorkers.Add(-1)
				<-semaphore
			}(index)
		}
		close(start)
		workers.Wait()
		for peak := peakWorkers.Load(); peak > maximumWorkers.Load() && !maximumWorkers.CompareAndSwap(maximumWorkers.Load(), peak); peak = peakWorkers.Load() {
		}
		return results, errorsByIndex, time.Since(startedAt)
	}
	mapAsset := func(userIndex, clientIndex int) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		mapping, err := MapSeedanceAssetsToAccount(ctx, userIndex+1, clients[clientIndex], []string{assetIDs[userIndex]})
		if err != nil {
			return "", err
		}
		return mapping[assetIDs[userIndex]], nil
	}

	initialResults, initialErrors, initialDuration := runBatch(userCount, func(int) (string, error) {
		return mapAsset(0, 0)
	})
	pendingRetries := 0
	for index, err := range initialErrors {
		if errors.Is(err, ErrSeedanceAssetMappingPending) {
			pendingRetries++
			continue
		}
		require.NoError(t, err, "same-account concurrent first use %d", index)
		assert.Equal(t, "remote-asset-load-key-000", initialResults[index])
	}
	_, retryErrors, retryDuration := runBatch(pendingRetries, func(int) (string, error) {
		return mapAsset(0, 0)
	})
	for index, err := range retryErrors {
		require.NoError(t, err, "same-account retry %d", index)
	}

	firstUseResults, firstUseErrors, firstUseDuration := runBatch(userCount-1, func(index int) (string, error) {
		return mapAsset(index+1, index+1)
	})
	for index, err := range firstUseErrors {
		require.NoError(t, err, "independent-account first use %d", index)
		assert.Equal(t, fmt.Sprintf("remote-asset-load-key-%03d", index+1), firstUseResults[index])
	}
	reuseResults, reuseErrors, reuseDuration := runBatch(userCount, func(index int) (string, error) {
		return mapAsset(index, index)
	})
	for index, err := range reuseErrors {
		require.NoError(t, err, "mapping reuse %d", index)
		assert.Equal(t, fmt.Sprintf("remote-asset-load-key-%03d", index), reuseResults[index])
	}

	var groupReplicaCount, assetReplicaCount int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroupReplica{}).Count(&groupReplicaCount).Error)
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Count(&assetReplicaCount).Error)
	assert.EqualValues(t, userCount, groupReplicaCount)
	assert.EqualValues(t, userCount, assetReplicaCount)
	assert.EqualValues(t, userCount, groupCreates.Load())
	assert.EqualValues(t, userCount, assetCreates.Load())
	assert.EqualValues(t, userCount, assetGets.Load())
	assert.Zero(t, invalidRequests.Load())
	assert.LessOrEqual(t, maximumWorkers.Load(), int64(concurrency))

	var localGroups []model.SeedanceAssetGroup
	var localAssets []model.SeedanceAsset
	var groupReplicas []model.SeedanceAssetGroupReplica
	var assetReplicas []model.SeedanceAssetReplica
	require.NoError(t, db.Order("user_id asc").Find(&localGroups).Error)
	require.NoError(t, db.Order("user_id asc").Find(&localAssets).Error)
	require.NoError(t, db.Order("user_id asc").Find(&groupReplicas).Error)
	require.NoError(t, db.Order("user_id asc").Find(&assetReplicas).Error)
	for index := range userCount {
		userID := index + 1
		assert.Equal(t, userID, localGroups[index].UserID)
		assert.Equal(t, userID, localAssets[index].UserID)
		assert.Equal(t, userID, groupReplicas[index].UserID)
		assert.Equal(t, localGroups[index].ID, groupReplicas[index].LocalGroupID)
		assert.Equal(t, clients[index].AccountFingerprint, groupReplicas[index].AccountFingerprint)
		assert.Equal(t, model.SeedanceAssetReplicaReady, groupReplicas[index].Status)
		assert.Equal(t, fmt.Sprintf("remote-group-load-key-%03d", index), groupReplicas[index].UpstreamGroupID)
		assert.Equal(t, userID, assetReplicas[index].UserID)
		assert.Equal(t, localAssets[index].ID, assetReplicas[index].LocalAssetID)
		assert.Equal(t, localGroups[index].ID, assetReplicas[index].LocalGroupID)
		assert.Equal(t, clients[index].AccountFingerprint, assetReplicas[index].AccountFingerprint)
		assert.Equal(t, model.SeedanceAssetReplicaReady, assetReplicas[index].ProvisionStatus)
		assert.Equal(t, "Active", assetReplicas[index].Status)
		assert.Equal(t, fmt.Sprintf("remote-asset-load-key-%03d", index), assetReplicas[index].UpstreamAssetID)
	}

	requestsPerSecond := func(requests int, duration time.Duration) float64 {
		if duration <= 0 {
			return 0
		}
		return float64(requests) / duration.Seconds()
	}
	t.Logf("replica load: db=%s version=%s users/accounts=%d concurrency=%d peak_workers=%d peak_upstream=%d pending_first_use=%d recovered=%d in %.1fms; same-account=%.1f req/s, distinct-account=%.1f req/s, reuse=%.1f req/s; upstream group/asset/status calls=%d/%d/%d",
		databaseEngine, databaseVersion, userCount, concurrency, maximumWorkers.Load(), peakHTTPRequests.Load(), pendingRetries,
		pendingRetries, float64(retryDuration.Microseconds())/1000,
		requestsPerSecond(userCount, initialDuration), requestsPerSecond(userCount-1, firstUseDuration), requestsPerSecond(userCount, reuseDuration),
		groupCreates.Load(), assetCreates.Load(), assetGets.Load())
}
