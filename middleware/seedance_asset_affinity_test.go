package middleware

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestExtractSeedanceAssetIDsFromJSONOnlyReadsMediaFields(t *testing.T) {
	raw := []byte(`{"model":"doubao-seedance-2-0-fast-260128","prompt":"asset://ignore-this-text","content":[{"type":"image_url","image_url":{"url":"asset://asset-image"}},{"type":"video_url","video_url":{"url":"asset://asset-video"}}],"metadata":{"content":[{"image_url":{"url":"asset://asset-image"}}]}}`)
	ids, err := extractSeedanceAssetIDsFromJSON(raw)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"asset-image", "asset-video"}, ids)
}

func TestApplySeedanceAssetAffinityPinsUserAssetAccount(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SeedanceAsset{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.Create(&model.SeedanceAsset{
		UserID: 7, ChannelID: 21, GroupID: "group-a", AssetID: "asset-image", Name: "image", AssetType: "Image", Status: "Active", KeyFingerprint: "account-a",
	}).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/video/generations", strings.NewReader(`{"model":"doubao-seedance-2-0-fast-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-image"}}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", 7)

	require.NoError(t, applySeedanceAssetAffinity(c))
	assert.Equal(t, 21, common.GetContextKeyInt(c, constant.ContextKeySeedanceAssetChannelId))
	assert.Equal(t, "account-a", common.GetContextKeyString(c, constant.ContextKeySeedanceAssetKeyFingerprint))
	constraints, ok := common.GetContextKeyType[*dto.ChannelConstraints](c, constant.ContextKeyChannelConstraints)
	require.True(t, ok)
	pin, found, _ := constraints.ResolvedPin()
	require.True(t, found)
	assert.Equal(t, dto.PinSourceSeedanceAsset, pin.Source)
	assert.Equal(t, dto.PinRetrySameChannel, pin.RetryMode)
}

func TestSetupContextForSelectedChannelUsesBoundSeedanceKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	channel := &model.Channel{
		Id:          22,
		Type:        constant.ChannelTypeDoubaoVideo,
		Key:         "account-a\naccount-b",
		Status:      common.ChannelStatusEnabled,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true},
	}
	fingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte("account-b")))
	common.SetContextKey(c, constant.ContextKeySeedanceAssetKeyFingerprint, fingerprint)

	setupErr := SetupContextForSelectedChannel(c, channel, "doubao-seedance-2-0-fast-260128")
	require.Nil(t, setupErr, "setup exact bound key")
	assert.Equal(t, "account-b", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	assert.Equal(t, 1, common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex))
}
