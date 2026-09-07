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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
