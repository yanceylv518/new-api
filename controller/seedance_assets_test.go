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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestSeedanceAssetPreviewURLPreservesSourceAfterTerminal(t *testing.T) {
	asset := &model.SeedanceAsset{
		Status:    "Processing",
		SourceURL: "https://assets.example.com/uploads/source.png",
	}

	previewURL, err := seedanceAssetPreviewURL(context.Background(), asset)
	require.NoError(t, err)
	require.Equal(t, asset.SourceURL, previewURL)

	asset.Status = "Active"
	previewURL, err = seedanceAssetPreviewURL(context.Background(), asset)
	require.NoError(t, err)
	require.Equal(t, asset.SourceURL, previewURL)
	asset.Status = "Failed"
	previewURL, err = seedanceAssetPreviewURL(context.Background(), asset)
	require.NoError(t, err)
	require.Equal(t, asset.SourceURL, previewURL)
}

func TestSeedanceAssetValidationSessionCreatesAndPublishesLocalGroup(t *testing.T) {
	previousDB := model.DB
	previousCryptoSecret := common.CryptoSecret
	previousServerAddress := system_setting.ServerAddress
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("Action") {
		case "CreateVisualValidateSession":
			_, _ = w.Write([]byte(`{"Result":{"BytedToken":"validation-token","H5Link":"https://example.com/validate"}}`))
		case "GetVisualValidateResult":
			_, _ = w.Write([]byte(`{"Result":{"GroupId":"group-liveness"}}`))
		case "GetAssetGroup":
			_, _ = w.Write([]byte(`{"Result":{"Id":"group-liveness","GroupType":"LivenessFace","Status":"Active"}}`))
		default:
			_, _ = w.Write([]byte(`{"Result":{"Status":"Active"}}`))
		}
	}))
	t.Cleanup(func() {
		model.DB = previousDB
		common.CryptoSecret = previousCryptoSecret
		system_setting.ServerAddress = previousServerAddress
		server.Close()
		_ = sqlDB.Close()
	})
	model.DB = db
	common.CryptoSecret = "validation-controller-test-secret"
	t.Setenv("CRYPTO_SECRET", "validation-controller-test-secret")
	system_setting.ServerAddress = "http://localhost:3000"
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetValidationSession{}))
	channel := &model.Channel{Id: 1, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled, Models: "doubao-seedance-2-0-fast-260128", Group: "default"}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, channel.Insert())

	createWriter := httptest.NewRecorder()
	createContext, _ := gin.CreateTestContext(createWriter)
	createContext.Set("id", 9)
	createContext.Request = httptest.NewRequest(http.MethodPost, "/api/user/seedance/validation-sessions", strings.NewReader(`{"name":"真人素材组","description":"审核通过后可用","tags":"演员, 已认证"}`))
	createContext.Request.Header.Set("Content-Type", "application/json")
	CreateSeedanceAssetValidationSession(createContext)
	require.Equal(t, http.StatusAccepted, createWriter.Code)
	var created struct {
		Success bool
		Data    service.SeedanceAssetValidationSessionView
	}
	require.NoError(t, common.Unmarshal(createWriter.Body.Bytes(), &created))
	require.True(t, created.Success)
	require.Equal(t, model.SeedanceAssetValidationStatusPending, created.Data.Status)
	assert.Equal(t, "真人素材组", created.Data.Name)
	assert.Equal(t, "审核通过后可用", created.Data.Description)
	assert.Equal(t, "演员, 已认证", created.Data.Tags)

	getWriter := httptest.NewRecorder()
	getContext, _ := gin.CreateTestContext(getWriter)
	getContext.Set("id", 9)
	getContext.Params = gin.Params{{Key: "id", Value: fmt.Sprint(created.Data.ID)}}
	getContext.Request = httptest.NewRequest(http.MethodGet, "/api/user/seedance/validation-sessions/1", nil)
	GetSeedanceAssetValidationSession(getContext)
	require.Equal(t, http.StatusOK, getWriter.Code)
	var refreshed struct {
		Success bool
		Data    service.SeedanceAssetValidationSessionView
	}
	require.NoError(t, common.Unmarshal(getWriter.Body.Bytes(), &refreshed))
	require.True(t, refreshed.Success)
	require.Equal(t, model.SeedanceAssetValidationStatusSucceeded, refreshed.Data.Status)
	require.Equal(t, "group-liveness", refreshed.Data.GroupID)
	var group model.SeedanceAssetGroup
	require.NoError(t, db.Where("user_id = ?", 9).First(&group).Error)
	assert.Equal(t, "LivenessFace", group.GroupType)
	assert.Equal(t, "审核通过后可用", group.Description)
	assert.Equal(t, "演员, 已认证", group.Tags)
}

func TestSeedanceAssetValidationFallsBackWhenActionIsUnsupported(t *testing.T) {
	previousDB := model.DB
	previousCryptoSecret := common.CryptoSecret
	previousServerAddress := system_setting.ServerAddress
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	var unsupportedCalls, successfulCalls atomic.Int32
	unsupportedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unsupportedCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_action","message":"visual validation is not supported"}}`))
	}))
	successServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		successfulCalls.Add(1)
		if r.URL.Query().Get("Action") != "CreateVisualValidateSession" {
			t.Fatalf("unexpected action %q", r.URL.Query().Get("Action"))
		}
		_, _ = w.Write([]byte(`{"Result":{"BytedToken":"fallback-token","H5Link":"https://example.com/fallback"}}`))
	}))
	t.Cleanup(func() {
		model.DB = previousDB
		common.CryptoSecret = previousCryptoSecret
		system_setting.ServerAddress = previousServerAddress
		unsupportedServer.Close()
		successServer.Close()
		_ = sqlDB.Close()
	})
	model.DB = db
	common.CryptoSecret = "validation-fallback-test-secret"
	t.Setenv("CRYPTO_SECRET", "validation-fallback-test-secret")
	system_setting.ServerAddress = "http://localhost:3000"
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetValidationSession{}))
	firstURL, secondURL := unsupportedServer.URL, successServer.URL
	channels := []*model.Channel{
		{Id: 1, Type: constant.ChannelTypeDoubaoVideo, Key: "unsupported-key", BaseURL: &firstURL, Status: common.ChannelStatusEnabled},
		{Id: 2, Type: constant.ChannelTypeDoubaoVideo, Key: "supported-key", BaseURL: &secondURL, Status: common.ChannelStatusEnabled},
	}
	for _, channel := range channels {
		channel.Models, channel.Group = "doubao-seedance-2-0-fast-260128", "default"
		configureSeedanceTestAssetURL(t, channel)
		require.NoError(t, channel.Insert())
	}

	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Set("id", 9)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/seedance/validation-sessions", strings.NewReader(`{"name":"fallback真人组"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	CreateSeedanceAssetValidationSession(c)

	require.Equal(t, http.StatusAccepted, writer.Code, writer.Body.String())
	assert.EqualValues(t, 1, unsupportedCalls.Load())
	assert.EqualValues(t, 1, successfulCalls.Load())
	var session model.SeedanceAssetValidationSession
	require.NoError(t, db.First(&session).Error)
	assert.Equal(t, 2, session.ChannelID)
	assert.Equal(t, model.SeedanceAssetValidationStatusPending, session.Status)
}

func TestCreateSeedanceAssetGroupRejectsGroupTypeOverride(t *testing.T) {
	for _, groupType := range []string{"", "AIGC", "LivenessFace"} {
		t.Run(groupType, func(t *testing.T) {
			writer := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(writer)
			c.Set("id", 9)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/user/seedance/asset-groups",
				strings.NewReader(fmt.Sprintf(`{"name":"test","GroupType":%q}`, groupType)))
			c.Request.Header.Set("Content-Type", "application/json")

			CreateSeedanceAssetGroup(c)

			var response struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
			}
			require.NoError(t, common.Unmarshal(writer.Body.Bytes(), &response))
			assert.False(t, response.Success)
			assert.NotEmpty(t, response.Message)
		})
	}
}

func TestCreateSeedanceAssetGroupCreatesAIGCType(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	var upstreamRequest map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := common.DecodeJson(r.Body, &upstreamRequest); err != nil {
			t.Errorf("decode CreateAssetGroup request: %v", err)
			return
		}
		_, _ = w.Write([]byte(`{"Result":{"Id":"group-aigc"}}`))
	}))
	t.Cleanup(func() {
		model.DB = previousDB
		server.Close()
		_ = sqlDB.Close()
	})
	model.DB = db
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}))
	channel := &model.Channel{
		Id: 1, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key",
		BaseURL: &server.URL, Status: common.ChannelStatusEnabled,
		Models: "doubao-seedance-2-0-fast-260128", Group: "default",
	}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, channel.Insert())

	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Set("id", 9)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/seedance/asset-groups",
		strings.NewReader(`{"name":"AIGC references"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	CreateSeedanceAssetGroup(c)

	require.Equal(t, http.StatusOK, writer.Code, writer.Body.String())
	assert.Equal(t, map[string]string{"Name": "AIGC references", "GroupType": "AIGC"}, upstreamRequest)
	var group model.SeedanceAssetGroup
	require.NoError(t, db.Where("group_id = ?", "group-aigc").First(&group).Error)
	assert.Equal(t, "AIGC", group.GroupType)
}

func TestSeedanceAssetV2ActionUsesOfficialValidationContract(t *testing.T) {
	previousDB := model.DB
	previousCryptoSecret := common.CryptoSecret
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	var received []map[string]string
	validationResultCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, common.DecodeJson(r.Body, &body))
		received = append(received, body)
		switch r.URL.Query().Get("Action") {
		case "CreateVisualValidateSession":
			_, _ = w.Write([]byte(`{"Result":{"BytedToken":"validation-token","H5Link":"https://example.com/validate"}}`))
		case "GetVisualValidateResult":
			validationResultCalls++
			if validationResultCalls == 1 {
				_, _ = w.Write([]byte(`{"Result":{}}`))
				return
			}
			_, _ = w.Write([]byte(`{"Result":{"GroupId":"group-liveness"}}`))
		case "GetAssetGroup":
			_, _ = w.Write([]byte(`{"Result":{"Id":"group-liveness","GroupType":"LivenessFace","Status":"Active"}}`))
		default:
			t.Fatalf("unexpected action %q", r.URL.Query().Get("Action"))
		}
	}))
	t.Cleanup(func() {
		model.DB = previousDB
		common.CryptoSecret = previousCryptoSecret
		server.Close()
		_ = sqlDB.Close()
	})
	model.DB = db
	common.CryptoSecret = "validation-official-contract-test-secret"
	t.Setenv("CRYPTO_SECRET", "validation-official-contract-test-secret")
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetValidationSession{}))
	channel := &model.Channel{Id: 1, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled, Models: "doubao-seedance-2-0-fast-260128", Group: "default"}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, channel.Insert())

	request := func(userID int, query, body string) *httptest.ResponseRecorder {
		writer := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(writer)
		c.Set("id", userID)
		c.Request = httptest.NewRequest(http.MethodPost, "/doubao/v2/assets?"+query, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		SeedanceAssetV2Action(c)
		return writer
	}

	missingCallback := request(9, "Action=CreateVisualValidateSession&Version=2024-01-01", `{}`)
	assert.Equal(t, http.StatusBadRequest, missingCallback.Code)
	assert.Empty(t, received)

	missingVersion := request(9, "Action=CreateVisualValidateSession", `{"CallbackURL":"https://example.com/callback"}`)
	assert.Equal(t, http.StatusBadRequest, missingVersion.Code)
	assert.Empty(t, received)

	created := request(9, "Action=CreateVisualValidateSession&Version=2024-01-01", `{"CallbackURL":"https://example.com/callback","ProjectName":"project-a"}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var createdBody struct {
		Result struct {
			BytedToken string `json:"BytedToken"`
			H5Link     string `json:"H5Link"`
		} `json:"Result"`
	}
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &createdBody))
	require.Equal(t, "validation-token", createdBody.Result.BytedToken)
	require.Equal(t, "https://example.com/validate", createdBody.Result.H5Link)
	require.Len(t, received, 1)
	callback, err := url.Parse(received[0]["CallbackURL"])
	require.NoError(t, err)
	assert.Equal(t, "/v1/seedance/validation-sessions/1/callback", callback.Path)
	assert.NotEmpty(t, callback.Query().Get("state"))
	assert.Equal(t, "project-a", received[0]["ProjectName"])

	pending := request(9, "Action=GetVisualValidateResult&Version=2024-01-01", `{"BytedToken":"validation-token","ProjectName":"project-a"}`)
	assert.Equal(t, http.StatusConflict, pending.Code)
	var pendingBody struct {
		ResponseMetadata struct {
			Error struct {
				Code string `json:"Code"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	require.NoError(t, common.Unmarshal(pending.Body.Bytes(), &pendingBody))
	assert.Equal(t, "ValidationPending", pendingBody.ResponseMetadata.Error.Code)
	require.Len(t, received, 2)

	result := request(9, "Action=GetVisualValidateResult&Version=2024-01-01", `{"BytedToken":"validation-token","ProjectName":"project-a"}`)
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	var resultBody struct {
		Result struct {
			GroupID string `json:"GroupId"`
		} `json:"Result"`
	}
	require.NoError(t, common.Unmarshal(result.Body.Bytes(), &resultBody))
	assert.Equal(t, "group-liveness", resultBody.Result.GroupID)
	require.Len(t, received, 3)
	assert.Equal(t, map[string]string{"BytedToken": "validation-token", "ProjectName": "project-a"}, received[2])

	outsider := request(10, "Action=GetVisualValidateResult&Version=2024-01-01", `{"BytedToken":"validation-token"}`)
	assert.Equal(t, http.StatusNotFound, outsider.Code)
	assert.Len(t, received, 3)
}

// 每个回归用独立内存库与回环上游，避免碰触实际账号、文件或数据库。
func setupSeedanceControllerRegression(t *testing.T, upstream http.HandlerFunc) (*gorm.DB, *model.SeedanceAssetGroup, *model.SeedanceAsset) {
	t.Helper()
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(
		&model.Channel{Type: constant.ChannelTypeDoubaoVideo},
		&model.User{}, &model.Ability{},
		&model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{},
		&model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetDiscovery{}, &model.SeedanceAssetCleanupJob{}, &model.SystemTask{}, &model.SeedanceAssetValidationSession{},
	))
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; _ = sqlDB.Close() })
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	require.NoError(t, db.Create(&model.User{Id: 9, Username: "seedance-owner", AffCode: "seedance-owner", Group: "default", Status: common.UserStatusEnabled}).Error)
	channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Id: 1, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled, Models: "doubao-seedance-2-0-fast-260128", Group: "default"}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, channel.Insert())
	group := &model.SeedanceAssetGroup{UserID: 9, ChannelID: 1, GroupID: "group-test", Name: "Before"}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{UserID: 9, ChannelID: 1, GroupID: group.GroupID, AssetID: "asset-test", Name: "preview", AssetType: "Video", Status: "Processing"}
	require.NoError(t, db.Create(asset).Error)
	return db, group, asset
}

type seedanceRegressionResponse struct {
	Success    bool            `json:"success"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data"`
	Total      int             `json:"total"`
	Page       int             `json:"page"`
	HasPending bool            `json:"has_pending"`
}

// 真实 multipart 请求经过解析、归属、文件类型和 OSS 配置边界；所有失败都不得调用上游或落库。
// OSS 成功上传、签名和补偿由 service 的可控存储替身覆盖，此处不接触真实云端。
func TestUploadSeedanceAssetMultipartFailureBoundaries(t *testing.T) {
	var upstreamCalls atomic.Int32
	db, group, _ := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{"Result":{"Id":"unexpected"}}`))
	})
	common.OptionMapRWMutex.Lock()
	previous := common.OptionMap
	common.OptionMap = system_setting.PrivateAssetOSSOptionValues(system_setting.PrivateAssetOSSSettings{})
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previous
		common.OptionMapRWMutex.Unlock()
	})
	for _, scenario := range []struct {
		name, groupID, assetType, expected string
		userID                             int
	}{
		{"missing group", "", "Image", "", 9},
		{"foreign group", group.GroupID, "Image", "record not found", 10},
		{"type mismatch", group.GroupID, "Video", "does not match", 9},
		{"OSS unavailable", group.GroupID, "Image", "OSS is not configured", 9},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			require.NoError(t, writer.WriteField("group_id", scenario.groupID))
			require.NoError(t, writer.WriteField("asset_type", scenario.assetType))
			part, err := writer.CreateFormFile("file", "cover.png")
			require.NoError(t, err)
			_, err = part.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			response := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(response)
			c.Set("id", scenario.userID)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/user/seedance/assets/upload", &body)
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			UploadSeedanceAsset(c)
			var payload seedanceRegressionResponse
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
			assert.False(t, payload.Success)
			if scenario.expected != "" {
				assert.Contains(t, payload.Message, scenario.expected)
			}
			var count int64
			require.NoError(t, db.Model(&model.SeedanceAsset{}).Count(&count).Error)
			assert.EqualValues(t, 1, count)
			assert.Zero(t, upstreamCalls.Load())
		})
	}
}

func invokeSeedanceRegression(t *testing.T, handler gin.HandlerFunc, method, path, id, body string) seedanceRegressionResponse {
	t.Helper()
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Set("id", 9)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	var payload seedanceRegressionResponse
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	return payload
}

// 归属检查必须先于上游调用，猜测其他用户的本地 ID 不得查询或修改资源。
func TestSeedanceControllerRejectsForeignOwnership(t *testing.T) {
	var calls atomic.Int32
	db, group, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	require.NoError(t, db.Model(group).Update("user_id", 10).Error)
	require.NoError(t, db.Model(asset).Update("user_id", 10).Error)
	for _, operation := range []struct {
		method  string
		handler gin.HandlerFunc
	}{
		{http.MethodPost, RefreshSeedanceAsset},
		{http.MethodDelete, DeleteSeedanceAsset},
		{http.MethodDelete, DeleteSeedanceAssetGroup},
		{http.MethodPut, UpdateSeedanceAssetGroup},
	} {
		payload := invokeSeedanceRegression(t, operation.handler, operation.method, "/", "1", `{"name":"Changed"}`)
		assert.False(t, payload.Success)
	}
	assert.Zero(t, calls.Load())
	require.NoError(t, db.First(group, group.ID).Error)
	assert.Equal(t, "Before", group.Name)
	require.NoError(t, db.First(asset, asset.ID).Error)
}

// 改名失败保留本地原值；用户删除以本站为准，上游拒绝不阻止本地清理。
func TestSeedanceControllerHandlesUpstreamBusinessFailure(t *testing.T) {
	for _, operation := range []struct {
		name, method string
		handler      gin.HandlerFunc
		deferred     bool
	}{
		{"delete asset", http.MethodDelete, DeleteSeedanceAsset, true},
		{"delete group", http.MethodDelete, DeleteSeedanceAssetGroup, true},
		{"rename group", http.MethodPut, UpdateSeedanceAssetGroup, false},
	} {
		t.Run(operation.name, func(t *testing.T) {
			db, group, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"code":"operation_failed","message":"rejected"}`))
			})
			payload := invokeSeedanceRegression(t, operation.handler, operation.method, "/", "1", `{"name":"After"}`)
			if operation.deferred {
				require.True(t, payload.Success, payload.Message)
				summary, err := service.RunSeedanceAssetCleanupOnce(context.Background(), nil)
				require.NoError(t, err)
				assert.Equal(t, 1, summary.Completed)
				assert.Zero(t, summary.RetryScheduled)
				if operation.name == "delete asset" {
					require.ErrorIs(t, db.First(asset, asset.ID).Error, gorm.ErrRecordNotFound)
				} else {
					require.ErrorIs(t, db.First(group, group.ID).Error, gorm.ErrRecordNotFound)
					require.ErrorIs(t, db.First(asset, asset.ID).Error, gorm.ErrRecordNotFound)
				}
				return
			}
			assert.False(t, payload.Success)
			assert.Contains(t, payload.Message, "rejected")
			require.NoError(t, db.First(group, group.ID).Error)
			require.NoError(t, db.First(asset, asset.ID).Error)
			assert.Equal(t, "Before", group.Name)
		})
	}
}

func TestSeedanceDeleteGroupQueuesWhenSourceChannelIsDisabled(t *testing.T) {
	var upstreamCalls atomic.Int32
	db, group, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", group.ChannelID).
		Update("status", common.ChannelStatusManuallyDisabled).Error)

	payload := invokeSeedanceRegression(t, DeleteSeedanceAssetGroup, http.MethodDelete, "/", fmt.Sprint(group.ID), "")
	assert.True(t, payload.Success, payload.Message)
	require.NoError(t, db.First(group, group.ID).Error)
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, "Deleting", group.Status)
	assert.Equal(t, "Deleting", asset.Status)
	var jobs int64
	require.NoError(t, db.Model(&model.SeedanceAssetCleanupJob{}).Count(&jobs).Error)
	assert.EqualValues(t, 1, jobs)
	assert.Zero(t, upstreamCalls.Load())
}

// 已经卡在 Deleting 的历史素材组再次发起删除时，必须补建清理任务并最终移除本地记录。
func TestSeedanceDeletingGroupCanResumeCleanup(t *testing.T) {
	var upstreamCalls atomic.Int32
	db, group, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "DeleteAssetGroup" {
			upstreamCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	require.NoError(t, db.Model(group).Update("status", "Deleting").Error)
	require.NoError(t, db.Model(asset).Updates(map[string]any{
		"status": "Deleting", "status_key": "deleting", "pending_status": true,
	}).Error)

	payload := invokeSeedanceRegression(t, DeleteSeedanceAssetGroup, http.MethodDelete, "/", fmt.Sprint(group.ID), "")
	require.True(t, payload.Success, payload.Message)
	var jobs int64
	require.NoError(t, db.Model(&model.SeedanceAssetCleanupJob{}).Count(&jobs).Error)
	assert.EqualValues(t, 1, jobs)

	summary, err := service.RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Completed)
	assert.EqualValues(t, 1, upstreamCalls.Load())
	var remaining int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Where("id = ?", group.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
}

func TestSeedanceGroupManagementUsesMatchingEnabledChannelAlias(t *testing.T) {
	var upstreamCalls atomic.Int32
	db, group, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Query().Get("Action") == "GetAsset" {
			_, _ = w.Write([]byte(`{"Result":{"Status":"Active"}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	var source model.Channel
	require.NoError(t, db.First(&source, group.ChannelID).Error)
	sourceClient, err := service.NewSeedanceAssetClient(&source)
	require.NoError(t, err)
	group.KeyFingerprint = sourceClient.KeyFingerprint
	group.AccountFingerprint = sourceClient.AccountFingerprint
	asset.KeyFingerprint = sourceClient.KeyFingerprint
	asset.AccountFingerprint = sourceClient.AccountFingerprint
	require.NoError(t, db.Save(group).Error)
	require.NoError(t, db.Save(asset).Error)

	alias := source
	alias.Id = 2
	alias.Status = common.ChannelStatusEnabled
	require.NoError(t, db.Create(&alias).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", source.Id).
		Update("status", common.ChannelStatusManuallyDisabled).Error)

	refreshed := invokeSeedanceRegression(t, RefreshSeedanceAsset, http.MethodPost, "/", fmt.Sprint(asset.ID), "")
	require.True(t, refreshed.Success, refreshed.Message)
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, "Active", asset.Status)

	updated := invokeSeedanceRegression(t, UpdateSeedanceAssetGroup, http.MethodPut, "/", fmt.Sprint(group.ID), `{"name":"After"}`)
	require.True(t, updated.Success, updated.Message)
	require.NoError(t, db.First(group, group.ID).Error)
	assert.Equal(t, "After", group.Name)

	deleted := invokeSeedanceRegression(t, DeleteSeedanceAssetGroup, http.MethodDelete, "/", fmt.Sprint(group.ID), "")
	require.True(t, deleted.Success, deleted.Message)
	summary, err := service.RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Completed)
	assert.EqualValues(t, 3, upstreamCalls.Load())
	var remaining int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Where("id = ?", group.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
}

func TestSeedanceRefreshReturnsPersistedStatus(t *testing.T) {
	db, _, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"Result":{"Status":"Active"}}`)) })
	payload := invokeSeedanceRegression(t, RefreshSeedanceAsset, http.MethodPost, "/", fmt.Sprint(asset.ID), "")
	require.True(t, payload.Success, payload.Message)
	var returned model.SeedanceAsset
	require.NoError(t, common.Unmarshal(payload.Data, &returned))
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, "Active", returned.Status)
	// SQL 与 JSON 可使用不同的时区对象，但必须表示同一精确时刻。
	assert.WithinDuration(t, asset.UpdatedAt, returned.UpdatedAt, 0)
}

func setupSeedanceScopedRegression(t *testing.T, handlers ...http.HandlerFunc) (*gorm.DB, *model.SeedanceAssetGroup, []*model.Channel, []*service.SeedanceAssetClient) {
	t.Helper()
	db, dialect := openTaskDialectDatabase(t, &model.Channel{}, &model.User{}, &model.Ability{},
		&model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAsset{},
		&model.SeedanceAssetReplica{}, &model.SystemTask{})
	previousDB, previousMain, previousLog := model.DB, common.MainDatabaseType(), common.LogDatabaseType()
	model.DB = db
	common.SetDatabaseTypes(dialect, dialect)
	t.Cleanup(func() { model.DB = previousDB; common.SetDatabaseTypes(previousMain, previousLog) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unexpected upstream request")
		w.WriteHeader(http.StatusInternalServerError)
	})
	if len(handlers) > 0 {
		handler = handlers[0]
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	require.NoError(t, db.Create(&model.User{Id: 9, Username: "seedance-owner", AffCode: "seedance-owner", Group: "default", Status: common.UserStatusEnabled}).Error)
	channels := make([]*model.Channel, 0, 2)
	clients := make([]*service.SeedanceAssetClient, 0, 2)
	for index := range 2 {
		name := fmt.Sprintf("doubao-seedance-model-%d", index)
		channel := &model.Channel{Id: index + 1, Type: constant.ChannelTypeDoubaoVideo, Key: fmt.Sprintf("fixture-key-%d", index), BaseURL: &server.URL, Status: common.ChannelStatusEnabled, Models: name, Group: "default"}
		configureSeedanceTestAssetURL(t, channel)
		require.NoError(t, channel.Insert())
		client, err := service.NewSeedanceAssetClient(channel)
		require.NoError(t, err)
		channels = append(channels, channel)
		clients = append(clients, client.ForSeedanceModel(channel, name))
	}
	group := &model.SeedanceAssetGroup{UserID: 9, ChannelID: 1, GroupID: "group-scoped", Name: "group", GroupType: "AIGC"}
	require.NoError(t, db.Create(group).Error)
	return db, group, channels, clients
}

func TestSeedanceProjectionPreservesScopedReviewTimeout(t *testing.T) {
	db, group, _, clients := setupSeedanceScopedRegression(t)
	localOnly := false
	asset := model.SeedanceAsset{UserID: 9, ChannelID: 1, GroupID: group.GroupID, AssetID: "asset-timeout", Name: "asset", AssetType: "Image", LocalOnly: &localOnly, Status: "Active"}
	require.NoError(t, db.Create(&asset).Error)
	for index, client := range clients {
		replica := model.SeedanceAssetReplica{UserID: 9, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: client.ChannelID, AccountFingerprint: client.AccountFingerprint, KeyFingerprint: client.KeyFingerprint, UpstreamAssetID: "upstream", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active"}
		if index == 1 {
			replica.Status, replica.LastError, replica.PollAttempts = "Processing", model.SeedanceAssetReviewTimeoutCode, model.SeedanceAssetSyncMaxAttempts
		}
		require.NoError(t, db.Create(&replica).Error)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("id", 9)
	assets := []model.SeedanceAsset{asset}
	require.NoError(t, projectSeedanceAssetStatuses(c, assets, false))
	assert.Equal(t, "Failed", assets[0].Status)
	assert.Equal(t, model.SeedanceAssetReviewTimeoutCode, assets[0].FailureCode)

	c.Set(seedanceStatusModelsKey, map[string][]string{"doubao-seedance-model-0": {clients[0].AccountFingerprint}})
	assets = []model.SeedanceAsset{asset}
	require.NoError(t, projectSeedanceAssetStatuses(c, assets, false))
	assert.Equal(t, "Active", assets[0].Status)
	assert.Empty(t, assets[0].FailureCode)

	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ? AND account_fingerprint = ?", asset.ID, clients[1].AccountFingerprint).
		Updates(map[string]any{"status": "Failed", "last_error": "", "poll_attempts": 0}).Error)
	c.Set(seedanceStatusModelsKey, map[string][]string{"doubao-seedance-model-0": {clients[0].AccountFingerprint}, "doubao-seedance-model-1": {clients[1].AccountFingerprint}})
	asset.FailureCode = model.SeedanceAssetReviewTimeoutCode
	assets = []model.SeedanceAsset{asset}
	require.NoError(t, projectSeedanceAssetStatuses(c, assets, false))
	assert.Equal(t, "Failed", assets[0].Status)
	assert.Equal(t, "Asset unavailable for this group or model", assets[0].FailureCode)

	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ? AND account_fingerprint = ?", asset.ID, clients[1].AccountFingerprint).
		Updates(map[string]any{"status": "Processing", "last_error": model.SeedanceAssetReviewTimeoutCode, "poll_attempts": model.SeedanceAssetSyncMaxAttempts}).Error)
	rejected := model.SeedanceAssetReplica{UserID: 9, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: 3, AccountFingerprint: strings.Repeat("a", 64), KeyFingerprint: strings.Repeat("b", 64), UpstreamAssetID: "rejected", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Failed"}
	require.NoError(t, db.Create(&rejected).Error)
	c.Set(seedanceStatusModelsKey, map[string][]string{"model-with-alternative": {clients[0].AccountFingerprint, clients[1].AccountFingerprint}, "model-rejected": {rejected.AccountFingerprint}})
	assets = []model.SeedanceAsset{asset}
	require.NoError(t, projectSeedanceAssetStatuses(c, assets, false))
	assert.Equal(t, "Failed", assets[0].Status)
	assert.Equal(t, "Asset unavailable for this group or model", assets[0].FailureCode)

	require.NoError(t, db.Model(group).Update("group_type", "LivenessFace").Error)
	assets = []model.SeedanceAsset{asset}
	require.NoError(t, projectSeedanceAssetStatuses(c, assets, false))
	assert.Equal(t, "Active", assets[0].Status)

	localOnly = true
	asset.Status, asset.FailureCode = "Failed", model.SeedanceAssetReviewTimeoutCode
	assets = []model.SeedanceAsset{asset}
	require.NoError(t, projectSeedanceAssetStatuses(c, assets, false))
	assert.Equal(t, "Failed", assets[0].Status)
	assert.Equal(t, model.SeedanceAssetReviewTimeoutCode, assets[0].FailureCode)
}

func TestSeedanceRefreshRetriesReplicaWhenPrimaryChannelDisabled(t *testing.T) {
	db, group, channels, clients := setupSeedanceScopedRegression(t)
	localOnly := false
	asset := model.SeedanceAsset{UserID: 9, ChannelID: channels[0].Id, GroupID: group.GroupID, AssetID: "asset-optimized", PrimaryUpstreamID: "primary", Name: "asset", AssetType: "Image", LocalOnly: &localOnly, Status: "Failed", AccountFingerprint: clients[0].AccountFingerprint, KeyFingerprint: clients[0].KeyFingerprint}
	require.NoError(t, db.Create(&asset).Error)
	replica := model.SeedanceAssetReplica{UserID: 9, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channels[1].Id, AccountFingerprint: clients[1].AccountFingerprint, KeyFingerprint: clients[1].KeyFingerprint, ProvisionStatus: model.SeedanceAssetReplicaFailed, Status: "Pending", LastError: "temporary failure", PollAttempts: model.SeedanceAssetSyncMaxAttempts}
	require.NoError(t, db.Create(&replica).Error)
	require.NoError(t, db.Model(channels[0]).Update("status", common.ChannelStatusManuallyDisabled).Error)
	payload := invokeSeedanceRegression(t, RefreshSeedanceAsset, http.MethodPost, "/", fmt.Sprint(asset.ID), "")
	require.True(t, payload.Success, payload.Message)
	require.NoError(t, db.First(&replica, replica.ID).Error)
	assert.Zero(t, replica.PollAttempts)
	assert.Empty(t, replica.LastError)
	assert.Positive(t, replica.NextPollAt)

	require.NoError(t, db.Model(&replica).Updates(map[string]any{"last_error": "temporary failure", "poll_attempts": model.SeedanceAssetSyncMaxAttempts, "next_poll_at": 0}).Error)
	require.NoError(t, db.Model(group).Update("group_type", "LivenessFace").Error)
	payload = invokeSeedanceRegression(t, RefreshSeedanceAsset, http.MethodPost, "/", fmt.Sprint(asset.ID), "")
	assert.False(t, payload.Success)
	require.NoError(t, db.First(&replica, replica.ID).Error)
	assert.Equal(t, model.SeedanceAssetSyncMaxAttempts, replica.PollAttempts)

	require.NoError(t, db.Model(&asset).Update("local_only", true).Error)
	payload = invokeSeedanceRegression(t, RefreshSeedanceAsset, http.MethodPost, "/", fmt.Sprint(asset.ID), "")
	assert.True(t, payload.Success, payload.Message)
	require.NoError(t, db.First(&replica, replica.ID).Error)
	assert.Equal(t, model.SeedanceAssetSyncMaxAttempts, replica.PollAttempts)

	require.NoError(t, db.Model(group).Update("group_type", "AIGC").Error)
	require.NoError(t, db.Model(&asset).Update("local_only", false).Error)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Set("id", 9)
	c.Set("token_id", 1)
	c.Set("token_model_limit_enabled", true)
	c.Set("token_model_limit", map[string]bool{"doubao-seedance-denied": true})
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(asset.ID)}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	RefreshSeedanceAsset(c)
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	require.NoError(t, db.First(&replica, replica.ID).Error)
	assert.Equal(t, model.SeedanceAssetSyncMaxAttempts, replica.PollAttempts)
}

func TestSeedanceRefreshRetriesReplicaWhenPrimaryRequestFails(t *testing.T) {
	var calls atomic.Int32
	db, group, channels, clients := setupSeedanceScopedRegression(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	localOnly := false
	asset := model.SeedanceAsset{UserID: 9, ChannelID: channels[0].Id, GroupID: group.GroupID, AssetID: "asset-primary-error", PrimaryUpstreamID: "primary", Name: "asset", AssetType: "Image", LocalOnly: &localOnly, Status: "Failed", AccountFingerprint: clients[0].AccountFingerprint, KeyFingerprint: clients[0].KeyFingerprint}
	require.NoError(t, db.Create(&asset).Error)
	replica := model.SeedanceAssetReplica{UserID: 9, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channels[1].Id, AccountFingerprint: clients[1].AccountFingerprint, KeyFingerprint: clients[1].KeyFingerprint, ProvisionStatus: model.SeedanceAssetReplicaFailed, Status: "Pending", LastError: "temporary failure", PollAttempts: model.SeedanceAssetSyncMaxAttempts}
	require.NoError(t, db.Create(&replica).Error)
	payload := invokeSeedanceRegression(t, RefreshSeedanceAsset, http.MethodPost, "/", fmt.Sprint(asset.ID), "")
	require.True(t, payload.Success, payload.Message)
	require.Positive(t, calls.Load())
	require.NoError(t, db.First(&replica, replica.ID).Error)
	assert.Zero(t, replica.PollAttempts)
	assert.Empty(t, replica.LastError)
	assert.Positive(t, replica.NextPollAt)
}

// 明确同步上游创建和分组删除的先后顺序，证明过期创建会被拒绝并清理上游资源。
func TestSeedanceCreateCannotOutliveDeletedGroup(t *testing.T) {
	var cleanupCalls atomic.Int32
	db, group, _ := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("Action") {
		case "CreateAsset":
			payload := invokeSeedanceRegression(t, DeleteSeedanceAssetGroup, http.MethodDelete, "/", "1", "")
			assert.True(t, payload.Success, payload.Message)
			_, _ = w.Write([]byte(`{"Result":{"Id":"late-asset"}}`))
		case "DeleteAsset":
			cleanupCalls.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	payload := invokeSeedanceRegression(t, CreateSeedanceAsset, http.MethodPost, "/", "", `{"group_id":"group-test","source_url":"https://8.8.8.8/file.png","asset_type":"Image","name":"late"}`)
	assert.False(t, payload.Success)
	summary, err := service.RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Completed)
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("group_id = ?", group.GroupID).Count(&count).Error)
	assert.Zero(t, count)
	assert.EqualValues(t, 1, cleanupCalls.Load())
}

// 已确认的上游删除状态必须持久化，本地数据库短暂失败后重试不能再次依赖上游。
func TestSeedanceDeleteResumesAfterLocalFailure(t *testing.T) {
	var calls atomic.Int32
	db, _, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{}`)) })
	const callback = "review:delete-failure"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) { tx.AddError(errors.New("local deletion failed")) }))
	payload := invokeSeedanceRegression(t, DeleteSeedanceAsset, http.MethodDelete, "/", fmt.Sprint(asset.ID), "")
	require.True(t, payload.Success, payload.Message)
	summary, err := service.RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.RetryScheduled)
	require.NoError(t, db.Callback().Delete().Remove(callback))
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, "Deleting", asset.Status)
	cleanupJob := service.NewSeedanceAssetDeleteCleanupJob(asset)
	require.NoError(t, db.Where("dedup_key = ?", cleanupJob.DedupKey).First(&cleanupJob).Error)
	assert.Positive(t, cleanupJob.UpstreamDoneAt)
	payload = invokeSeedanceRegression(t, DeleteSeedanceAsset, http.MethodDelete, "/", fmt.Sprint(asset.ID), "")
	require.True(t, payload.Success, payload.Message)
	require.NoError(t, db.Where("dedup_key = ?", cleanupJob.DedupKey).First(&cleanupJob).Error)
	assert.Positive(t, cleanupJob.UpstreamDoneAt)
	summary, err = service.RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Completed)
	assert.EqualValues(t, 1, calls.Load())
}

// 批量删除先原子入队；单条本地删除失败不会阻断其他素材，失败项保留供重试。
func TestSeedanceDeleteAssetBatchReturnsPerAssetResults(t *testing.T) {
	var calls atomic.Int32
	db, group, first := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	})
	second := &model.SeedanceAsset{
		UserID: 9, ChannelID: 1, GroupID: group.GroupID, AssetID: "asset-second",
		Name: "second", AssetType: "Image", Status: "Active",
	}
	require.NoError(t, db.Create(second).Error)
	const callback = "test:batch-local-delete-failure"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) {
		if asset, ok := tx.Statement.Dest.(*model.SeedanceAsset); ok && asset.ID == second.ID {
			tx.AddError(errors.New("local deletion failed"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Delete().Remove(callback)) })

	body := fmt.Sprintf(`{"ids":[%d,%d,%d,999999]}`, first.ID, second.ID, first.ID)
	payload := invokeSeedanceRegression(t, DeleteSeedanceAssetBatch, http.MethodPost, "/", "", body)
	assert.True(t, payload.Success)
	var result seedanceAssetBatchDeleteResult
	require.NoError(t, common.Unmarshal(payload.Data, &result))
	assert.Empty(t, result.DeletedIDs)
	assert.Empty(t, result.FailedIDs)
	assert.Equal(t, []uint{first.ID, second.ID}, result.PendingIDs)
	assert.Zero(t, calls.Load())

	var remaining []model.SeedanceAsset
	require.NoError(t, db.Where("id IN ?", []uint{first.ID, second.ID}).Find(&remaining).Error)
	require.Len(t, remaining, 2)
	assert.Equal(t, "Deleting", remaining[0].Status)
	assert.Equal(t, "Deleting", remaining[1].Status)
	require.NotNil(t, remaining[0].StatusKey)
	require.NotNil(t, remaining[1].StatusKey)
	require.NotNil(t, remaining[0].PendingStatus)
	require.NotNil(t, remaining[1].PendingStatus)
	assert.Equal(t, "deleting", *remaining[0].StatusKey)
	assert.Equal(t, "deleting", *remaining[1].StatusKey)
	assert.True(t, *remaining[0].PendingStatus)
	assert.True(t, *remaining[1].PendingStatus)

	summary, err := service.RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Candidates)
	assert.Equal(t, 1, summary.Completed)
	assert.Equal(t, 1, summary.RetryScheduled)
	assert.EqualValues(t, 2, calls.Load())

	remaining = nil
	require.NoError(t, db.Where("id IN ?", []uint{first.ID, second.ID}).Find(&remaining).Error)
	require.Len(t, remaining, 1)
	assert.Equal(t, second.ID, remaining[0].ID)
	assert.Equal(t, "Deleting", remaining[0].Status)
	require.NotNil(t, remaining[0].StatusKey)
	require.NotNil(t, remaining[0].PendingStatus)
	assert.Equal(t, "deleting", *remaining[0].StatusKey)
	assert.True(t, *remaining[0].PendingStatus)
}

// 覆盖第 201 条素材、跨页搜索、用户隔离和筛选下的后台轮询提示。
func TestSeedancePaginationSearchesBeyondFirstTwoHundredAssets(t *testing.T) {
	db, group, initial := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	initial.Status = "pRoCeSsInG"
	require.NoError(t, db.Save(initial).Error)
	require.NoError(t, db.Model(initial).Update("name", "old%file").Error)
	rows := make([]model.SeedanceAsset, 201)
	for index := range rows {
		rows[index] = model.SeedanceAsset{UserID: 9, ChannelID: 1, GroupID: group.GroupID, AssetID: fmt.Sprintf("asset-%d", index), Name: "new", AssetType: "Image", Status: "Active"}
	}
	rows[200].UserID = 10
	require.NoError(t, db.Create(&rows).Error)
	groupAssetCount := int64(201)
	require.NoError(t, db.Model(group).UpdateColumn("asset_count", groupAssetCount).Error)
	for _, scenario := range []struct {
		query              string
		total, count, page int
	}{
		{"/?p=3&page_size=100", 201, 1, 3},
		{"/?search=old%25", 1, 1, 1},
		{"/?asset_type=Video&status=Processing", 1, 1, 1},
		{"/?status=Active&page_size=100", 200, 100, 1},
	} {
		payload := invokeSeedanceRegression(t, ListSeedanceAssets, http.MethodGet, scenario.query, "", "")
		require.True(t, payload.Success, payload.Message)
		var returned []model.SeedanceAsset
		require.NoError(t, common.Unmarshal(payload.Data, &returned))
		assert.Len(t, returned, scenario.count)
		assert.Equal(t, scenario.total, payload.Total)
		assert.Equal(t, scenario.page, payload.Page)
		assert.True(t, payload.HasPending)
	}
}

func TestUpdateSeedanceAssetGroupUpdatesMetadataWithoutDeletingStoredAssets(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{Type: constant.ChannelTypeDoubaoVideo}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}))
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
		_, _ = writer.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Id: 71, Key: "test-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, db.Create(channel).Error)
	group := &model.SeedanceAssetGroup{UserID: 9, ChannelID: channel.Id, GroupID: "group-1", Name: "Before"}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{
		UserID: 9, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "asset-1",
		Name: "cover.png", AssetType: "Image", ObjectKey: "private-assets/users/9/asset.png", Status: "Active",
	}
	require.NoError(t, db.Create(asset).Error)

	response := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(response)
	requestContext.Set("id", 9)
	requestContext.Params = gin.Params{{Key: "id", Value: fmt.Sprint(group.ID)}}
	requestContext.Request = httptest.NewRequest(http.MethodPut, "/api/user/seedance/asset-groups/1", strings.NewReader(`{"name":"After","description":"Character portrait","tags":"actor, verified"}`))
	requestContext.Request.Header.Set("Content-Type", "application/json")

	UpdateSeedanceAssetGroup(requestContext)

	var payload struct {
		Success bool                     `json:"success"`
		Data    model.SeedanceAssetGroup `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	assert.Equal(t, "After", payload.Data.Name)
	assert.Equal(t, "Character portrait", payload.Data.Description)
	assert.Equal(t, "actor, verified", payload.Data.Tags)
	assert.Equal(t, "UpdateAssetGroup", upstreamAction)
	var storedAssetCount int64
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&storedAssetCount).Error)
	assert.EqualValues(t, 1, storedAssetCount)
}

func BenchmarkSeedanceAssetModelStatusMySQL(b *testing.B) {
	dsn := os.Getenv("SEEDANCE_STATUS_BENCH_MYSQL_DSN")
	if dsn == "" {
		b.Skip("SEEDANCE_STATUS_BENCH_MYSQL_DSN is not configured")
	}
	config, err := mysqldriver.ParseDSN(dsn)
	require.NoError(b, err)
	require.Equal(b, "tcp", config.Net)
	host, _, err := net.SplitHostPort(config.Addr)
	require.NoError(b, err)
	require.True(b, net.ParseIP(host).IsLoopback())
	require.True(b, strings.HasPrefix(config.DBName, "codex_seedance_status_"))
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(b, err)
	connection, err := db.DB()
	require.NoError(b, err)
	previousDB := model.DB
	model.DB = db
	b.Cleanup(func() {
		model.DB = previousDB
		assert.NoError(b, connection.Close())
	})
	require.NoError(b, db.AutoMigrate(&model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}))
	group := model.SeedanceAssetGroup{UserID: 1, ChannelID: 1, GroupID: "benchmark-group", Name: "benchmark", GroupType: "AIGC", Status: "Active"}
	require.NoError(b, db.Create(&group).Error)
	localOnly := false
	asset := model.SeedanceAsset{UserID: 1, ChannelID: 1, GroupID: group.GroupID, AssetID: "benchmark-asset", Name: "benchmark", AssetType: "Image", LocalOnly: &localOnly, Status: "Processing"}
	require.NoError(b, db.Create(&asset).Error)
	models := make(map[string][]string, 4)
	var last model.SeedanceAssetReplica
	for index := range 4 {
		fingerprint := fmt.Sprintf("%064x", index+1)
		models[fmt.Sprintf("model-%d", index+1)] = []string{fingerprint}
		replica := model.SeedanceAssetReplica{UserID: 1, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: index + 1,
			AccountFingerprint: fingerprint, KeyFingerprint: fingerprint, UpstreamGroupID: "group", UpstreamAssetID: "asset", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active"}
		require.NoError(b, db.Create(&replica).Error)
		last = replica
	}
	for _, scenario := range []struct {
		name   string
		count  int
		state  string
		status string
	}{
		{"one_model_active", 1, "Active", "active"},
		{"four_models_active", 4, "Active", "active"},
		{"four_models_processing", 4, "Processing", "processing"},
		{"four_models_failed", 4, "Failed", "failed"},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			require.NoError(b, db.Model(&last).Updates(map[string]any{"status": scenario.state, "provision_status": model.SeedanceAssetReplicaReady, "last_error": "", "poll_attempts": 0}).Error)
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			context.Set("id", 1)
			context.Set(seedanceStatusAccountsKey, []string{""})
			activeModels := make(map[string][]string, scenario.count)
			for index := range scenario.count {
				name := fmt.Sprintf("model-%d", index+1)
				activeModels[name] = models[name]
			}
			context.Set(seedanceStatusModelsKey, activeModels)
			expression, args, err := seedanceAssetStatusExpression(context, "a", false)
			require.NoError(b, err)
			b.ResetTimer()
			for range b.N {
				require.NoError(b, scheduleMissingSeedanceAssetTargets(context))
				var result struct{ Status string }
				require.NoError(b, db.Table(db.NamingStrategy.TableName("SeedanceAsset")+" AS a").
					Select(expression+" AS status", args...).Where("a.id = ?", asset.ID).Scan(&result).Error)
				require.Equal(b, scenario.status, result.Status)
			}
		})
	}
}
