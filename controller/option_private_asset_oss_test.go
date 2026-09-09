package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupPrivateAssetOSSOptionTest(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	// 目标版本把操作审计拆到独立表，测试必须验证真实审计落库路径。
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.SeedanceAsset{}, &model.User{}, &model.AuditLog{}))
	model.DB = db
	model.LOG_DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestGetOptionsHidesPrivateAssetOSSSecret(t *testing.T) {
	common.OptionMapRWMutex.Lock()
	previousMap := common.OptionMap
	common.OptionMap = map[string]string{
		system_setting.PrivateAssetOSSRegionKey:          "cn-hangzhou",
		system_setting.PrivateAssetOSSAccessKeyIDKey:     "test-access-key-id",
		system_setting.PrivateAssetOSSAccessKeySecretKey: "test-access-key-secret",
	}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousMap
		common.OptionMapRWMutex.Unlock()
	})

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/option/", nil)
	GetOptions(context)

	var payload struct {
		Success bool `json:"success"`
		Data    []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	values := make(map[string]string, len(payload.Data))
	for _, option := range payload.Data {
		values[option.Key] = option.Value
	}
	assert.NotContains(t, values, system_setting.PrivateAssetOSSAccessKeySecretKey)
	assert.Equal(t, "test-access-key-id", values[system_setting.PrivateAssetOSSAccessKeyIDKey])
	assert.Equal(t, "true", values[system_setting.PrivateAssetOSSSecretConfiguredKey])
}

func TestUpdateOptionRejectsPartialPrivateAssetOSSUpdate(t *testing.T) {
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/",
		strings.NewReader(`{"key":"private_asset_oss.bucket","value":"other-bucket"}`),
	)

	UpdateOption(context)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{"success":false,"message":"private asset OSS settings must be updated together"}`, response.Body.String())
}

func TestUpdatePrivateAssetOSSSettingsKeepsExistingSecretWhenBlank(t *testing.T) {
	db := setupPrivateAssetOSSOptionTest(t)
	common.OptionMapRWMutex.Lock()
	previousMap := common.OptionMap
	common.OptionMap = system_setting.PrivateAssetOSSOptionValues(system_setting.PrivateAssetOSSSettings{
		Region:          "cn-hangzhou",
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "private-assets",
		Prefix:          "private-assets/",
		AccessKeyID:     "old-id",
		AccessKeySecret: "existing-secret",
	})
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousMap
		common.OptionMapRWMutex.Unlock()
	})
	require.NoError(t, db.Create(&model.Option{
		Key:   system_setting.PrivateAssetOSSAccessKeySecretKey,
		Value: "existing-secret",
	}).Error)

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Set("id", 1)
	context.Set("role", common.RoleRootUser)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/private-asset-oss",
		strings.NewReader(`{"region":"cn-hangzhou","endpoint":"https://oss-cn-hangzhou.aliyuncs.com/","bucket":"private-assets","prefix":"next-prefix","access_key_id":"new-id","access_key_secret":""}`),
	)

	UpdatePrivateAssetOSSSettings(context)

	var payload struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	settings := system_setting.GetPrivateAssetOSSSettings()
	assert.Equal(t, "next-prefix/", settings.Prefix)
	assert.Equal(t, "new-id", settings.AccessKeyID)
	assert.Equal(t, "existing-secret", settings.AccessKeySecret)
	var secretOption model.Option
	require.NoError(t, db.First(&secretOption, "key = ?", system_setting.PrivateAssetOSSAccessKeySecretKey).Error)
	assert.Equal(t, "existing-secret", secretOption.Value)
	var audit model.AuditLog
	require.NoError(t, db.Where("action = ?", "option.private_asset_oss.update").First(&audit).Error)
	encoded, err := common.Marshal(audit)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "existing-secret")
}

func TestUpdatePrivateAssetOSSSettingsRejectsBucketChangeWithStoredObjects(t *testing.T) {
	db := setupPrivateAssetOSSOptionTest(t)
	current := system_setting.PrivateAssetOSSSettings{
		Region:          "cn-hangzhou",
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "private-assets",
		Prefix:          "private-assets/",
		AccessKeyID:     "test-id",
		AccessKeySecret: "test-secret",
	}
	common.OptionMapRWMutex.Lock()
	previousMap := common.OptionMap
	common.OptionMap = system_setting.PrivateAssetOSSOptionValues(current)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousMap
		common.OptionMapRWMutex.Unlock()
	})
	require.NoError(t, db.Create(&model.SeedanceAsset{
		UserID: 1, ChannelID: 1, GroupID: "group-1", AssetID: "asset-1", Name: "cover.png",
		AssetType: "Image", ObjectKey: "private-assets/users/1/asset.png", Status: "Active",
	}).Error)

	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/option/private-asset-oss",
		strings.NewReader(`{"region":"cn-hangzhou","endpoint":"https://oss-cn-hangzhou.aliyuncs.com","bucket":"other-assets","prefix":"private-assets/","access_key_id":"test-id"}`),
	)

	UpdatePrivateAssetOSSSettings(context)

	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
	assert.Contains(t, payload.Message, "cannot be changed")
	assert.Equal(t, "private-assets", system_setting.GetPrivateAssetOSSSettings().Bucket)
}
