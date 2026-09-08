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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// 每个回归用独立内存库与回环上游，避免碰触实际账号、文件或数据库。
func setupSeedanceControllerRegression(t *testing.T, upstream http.HandlerFunc) (*gorm.DB, *model.SeedanceAssetGroup, *model.SeedanceAsset) {
	t.Helper()
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAssetGroup{}, &model.SeedanceAsset{}))
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; _ = sqlDB.Close() })
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	channel := &model.Channel{Id: 1, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(channel).Error)
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

// HTTP 成功不代表业务成功，失败的写操作不能丢失授权映射或更新本地名称。
func TestSeedanceControllerRejectsUpstreamBusinessFailure(t *testing.T) {
	for _, operation := range []struct {
		name, method string
		handler      gin.HandlerFunc
	}{
		{"delete asset", http.MethodDelete, DeleteSeedanceAsset},
		{"delete group", http.MethodDelete, DeleteSeedanceAssetGroup},
		{"rename group", http.MethodPut, UpdateSeedanceAssetGroup},
	} {
		t.Run(operation.name, func(t *testing.T) {
			db, group, asset := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"code":"operation_failed","message":"rejected"}`))
			})
			payload := invokeSeedanceRegression(t, operation.handler, operation.method, "/", "1", `{"name":"After"}`)
			assert.False(t, payload.Success)
			assert.Contains(t, payload.Message, "rejected")
			require.NoError(t, db.First(group, group.ID).Error)
			require.NoError(t, db.First(asset, asset.ID).Error)
			assert.Equal(t, "Before", group.Name)
		})
	}
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
	payload := invokeSeedanceRegression(t, CreateSeedanceAsset, http.MethodPost, "/", "", `{"group_id":"group-test","source_url":"https://example.com/file.png","asset_type":"Image","name":"late"}`)
	assert.False(t, payload.Success)
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
	assert.False(t, payload.Success)
	require.NoError(t, db.Callback().Delete().Remove(callback))
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Positive(t, asset.UpstreamDeletedAt)
	assert.Equal(t, "Deleting", asset.Status)
	payload = invokeSeedanceRegression(t, DeleteSeedanceAsset, http.MethodDelete, "/", fmt.Sprint(asset.ID), "")
	assert.True(t, payload.Success, payload.Message)
	assert.EqualValues(t, 1, calls.Load())
}

// 覆盖第 201 条素材、跨页搜索、用户隔离和筛选下的后台轮询提示。
func TestSeedancePaginationSearchesBeyondFirstTwoHundredAssets(t *testing.T) {
	db, group, initial := setupSeedanceControllerRegression(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	require.NoError(t, db.Model(initial).Update("name", "old%file").Error)
	rows := make([]model.SeedanceAsset, 201)
	for index := range rows {
		rows[index] = model.SeedanceAsset{UserID: 9, ChannelID: 1, GroupID: group.GroupID, AssetID: fmt.Sprintf("asset-%d", index), Name: "new", AssetType: "Image", Status: "Active"}
	}
	rows[200].UserID = 10
	require.NoError(t, db.Create(&rows).Error)
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

func TestUpdateSeedanceAssetGroupDoesNotDeleteStoredAssets(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAssetGroup{}, &model.SeedanceAsset{}))
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

	channel := &model.Channel{Id: 71, Key: "test-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
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
	requestContext.Request = httptest.NewRequest(http.MethodPut, "/api/user/seedance/asset-groups/1", strings.NewReader(`{"name":"After"}`))
	requestContext.Request.Header.Set("Content-Type", "application/json")

	UpdateSeedanceAssetGroup(requestContext)

	var payload struct {
		Success bool                     `json:"success"`
		Data    model.SeedanceAssetGroup `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	assert.Equal(t, "After", payload.Data.Name)
	assert.Equal(t, "UpdateAssetGroup", upstreamAction)
	var storedAssetCount int64
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&storedAssetCount).Error)
	assert.EqualValues(t, 1, storedAssetCount)
}
