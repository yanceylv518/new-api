package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestUpdateChannelPartialFieldsMySQL(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(db.Migrator().CurrentDatabase(), "codex_origin_"))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	previousRegistry := jsplugin.DefaultRegistry
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousType, previousLogType)
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
		jsplugin.DefaultRegistry = previousRegistry
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.AuditLog{}))
	registry := jsplugin.NewRegistry()
	for _, key := range []string{"doubao", "anyllm"} {
		source, sourceErr := builtinplugins.Source(key)
		require.NoError(t, sourceErr)
		_, registerErr := registry.RegisterFactory(source, jsplugin.Options{Key: key})
		require.NoError(t, registerErr)
	}
	jsplugin.DefaultRegistry = registry
	user := model.User{Id: 1, Username: "partial-update-fixture", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "partial-update"}
	require.NoError(t, db.Create(&user).Error)
	for _, tc := range []struct {
		name           string
		kind           int
		setting, other string
	}{
		{"legacy_doubao", constant.ChannelTypeDoubaoVideo, `{"seedance_asset_url":"https://fixture.example/assets","seedance_asset_share_group":"fixture","video_routing_rules":[{"model":"doubao-seedance-2-0-260128","mode":"deny","resolutions":["4k"]}],"future":9007199254740993}`, ""},
		{"doubao_plugin", constant.ChannelTypeTaskPlugin, `{"task_plugin_key":"doubao","seedance_asset_url":"https://fixture.example/assets","seedance_asset_share_group":"fixture","video_routing_rules":[{"model":"doubao-seedance-2-0-260128","mode":"deny","resolutions":["4k"]}],"future":9007199254740993}`, ""},
		{"anyllm_plugin", constant.ChannelTypeTaskPlugin, `{"task_plugin_key":"anyllm","seedance_asset_url":"https://fixture.example/assets","seedance_asset_share_group":"fixture","video_routing_rules":[{"model":"doubao-seedance-2-0-260128","mode":"allow","resolutions":["4k"]}],"future":9007199254740993}`, ""},
		{"new_api_extension", constant.ChannelTypeNewAPI, `{"task_extend_plugin_keys":["doubao"],"video_routing_rules":[{"model":"doubao-seedance-2-0-260128","mode":"deny","resolutions":["4k"]}],"future":9007199254740993}`, ""},
		{"vertex_region", constant.ChannelTypeVertexAi, `{"future":9007199254740993}`, `{"default":"us-central1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := model.Channel{Type: tc.kind, Name: tc.name, Key: "fixture-key", Status: common.ChannelStatusEnabled, BaseURL: common.GetPointer("https://fixture.example/custom"), Models: "doubao-seedance-2-0-260128", Group: "default", Setting: &tc.setting, Other: tc.other, OtherSettings: `{"future":9007199254740993}`}
			require.NoError(t, channel.Insert())
			for _, update := range []map[string]any{{"priority": 8}, {"priority": 0}, {"weight": 15}, {"weight": 0}, {"name": "renamed"}, {"models": "doubao-seedance-2-0-260128,doubao-seedance-2-0-fast-260128"}, {"group": "vip"}} {
				body := map[string]any{"id": channel.Id}
				for key, value := range update {
					body[key] = value
				}
				raw, marshalErr := common.Marshal(body)
				require.NoError(t, marshalErr)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Set("id", 1)
				ctx.Set("role", common.RoleAdminUser)
				ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel/", bytes.NewReader(raw))
				ctx.Request.Header.Set("Content-Type", "application/json")
				UpdateChannel(ctx)
				var response struct {
					Success bool   `json:"success"`
					Message string `json:"message"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				require.True(t, response.Success, "%s: %s", fmt.Sprint(update), response.Message)
				var saved model.Channel
				require.NoError(t, db.First(&saved, channel.Id).Error)
				assert.Equal(t, channel.Type, saved.Type)
				assert.Equal(t, channel.BaseURL, saved.BaseURL)
				assert.Equal(t, channel.Key, saved.Key)
				assert.Equal(t, channel.Setting, saved.Setting)
				assert.Equal(t, channel.OtherSettings, saved.OtherSettings)
				assert.Equal(t, channel.Other, saved.Other)
				if value, edited := update["models"]; edited {
					assert.Equal(t, value, saved.Models)
				} else {
					assert.Equal(t, channel.Models, saved.Models)
				}
				if value, edited := update["group"]; edited {
					assert.Equal(t, value, saved.Group)
				} else {
					assert.Equal(t, channel.Group, saved.Group)
				}
				channel.Models, channel.Group = saved.Models, saved.Group
				if value, ok := update["priority"]; ok {
					assert.EqualValues(t, value, saved.GetPriority())
				}
				if value, ok := update["weight"]; ok {
					assert.EqualValues(t, value, saved.GetWeight())
				}
				if value, ok := update["name"]; ok {
					assert.Equal(t, value, saved.Name)
				}
			}
		})
	}
}

func TestChannelHasSensitiveChanges(t *testing.T) {
	baseURL := "https://api.example.com"
	headerOverride := `{"Authorization":"Bearer {api_key}"}`
	origin := &model.Channel{
		Type:           1,
		Key:            "old-key",
		BaseURL:        &baseURL,
		HeaderOverride: &headerOverride,
		Models:         "gpt-4o",
		Group:          "default",
	}

	t.Run("non-sensitive routing fields", func(t *testing.T) {
		updated := PatchChannel{Channel: *origin}
		updated.Models = "gpt-4o,gpt-4o-mini"
		updated.Group = "vip"

		assert.False(t, channelHasSensitiveChanges(&updated, origin, map[string]any{
			"models": updated.Models,
			"group":  updated.Group,
		}))
	})

	t.Run("key change", func(t *testing.T) {
		updated := PatchChannel{Channel: *origin}
		updated.Key = "new-key"

		assert.True(t, channelHasSensitiveChanges(&updated, origin, map[string]any{"key": updated.Key}))
	})

	t.Run("base url change", func(t *testing.T) {
		updated := PatchChannel{Channel: *origin}
		newBaseURL := "https://leak.example.com"
		updated.BaseURL = &newBaseURL

		assert.True(t, channelHasSensitiveChanges(&updated, origin, map[string]any{"base_url": newBaseURL}))
	})

	t.Run("header override change", func(t *testing.T) {
		updated := PatchChannel{Channel: *origin}
		newHeaderOverride := `{"X-Key":"{api_key}"}`
		updated.HeaderOverride = &newHeaderOverride

		assert.True(t, channelHasSensitiveChanges(&updated, origin, map[string]any{"header_override": newHeaderOverride}))
	})

	t.Run("omitted sensitive fields do not use zero values", func(t *testing.T) {
		updated := PatchChannel{}
		updated.Id = origin.Id
		updated.Priority = origin.Priority

		assert.False(t, channelHasSensitiveChanges(&updated, origin, map[string]any{"priority": 10}))
	})

	t.Run("unknown field fails closed", func(t *testing.T) {
		updated := PatchChannel{Channel: *origin}

		assert.True(t, channelHasSensitiveChanges(&updated, origin, map[string]any{"future_secret_field": "x"}))
	})

	t.Run("status is operational", func(t *testing.T) {
		updated := PatchChannel{Channel: *origin}
		updated.Status = common.ChannelStatusManuallyDisabled

		assert.False(t, channelHasSensitiveChanges(&updated, origin, map[string]any{"status": updated.Status}))
	})

	t.Run("read-only fields are ignored by sensitivity check", func(t *testing.T) {
		updated := PatchChannel{Channel: *origin}
		updated.Balance = 99
		updated.UsedQuota = 100
		updated.ResponseTime = 200

		assert.False(t, channelHasSensitiveChanges(&updated, origin, map[string]any{
			"balance":       updated.Balance,
			"used_quota":    updated.UsedQuota,
			"response_time": updated.ResponseTime,
		}))
	})
}

func TestClearChannelReadOnlyFields(t *testing.T) {
	channel := PatchChannel{Channel: model.Channel{
		CreatedTime:        11,
		TestTime:           22,
		ResponseTime:       33,
		Balance:            44.5,
		BalanceUpdatedTime: 55,
		UsedQuota:          66,
		Models:             "gpt-4o",
		Group:              "default",
	}}

	clearChannelReadOnlyFields(&channel, map[string]any{
		"created_time":         channel.CreatedTime,
		"test_time":            channel.TestTime,
		"response_time":        channel.ResponseTime,
		"balance":              channel.Balance,
		"balance_updated_time": channel.BalanceUpdatedTime,
		"used_quota":           channel.UsedQuota,
		"models":               channel.Models,
		"group":                channel.Group,
	})

	assert.Zero(t, channel.CreatedTime)
	assert.Zero(t, channel.TestTime)
	assert.Zero(t, channel.ResponseTime)
	assert.Zero(t, channel.Balance)
	assert.Zero(t, channel.BalanceUpdatedTime)
	assert.Zero(t, channel.UsedQuota)
	assert.Equal(t, "gpt-4o", channel.Models)
	assert.Equal(t, "default", channel.Group)
}

func TestUpdateChannelRejectsStatusField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/channel/",
		bytes.NewBufferString(`{"id":1,"status":2}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateChannel(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
}

func TestChannelStatusValidation(t *testing.T) {
	assert.True(t, isManageableChannelStatus(common.ChannelStatusEnabled))
	assert.True(t, isManageableChannelStatus(common.ChannelStatusManuallyDisabled))
	assert.False(t, isManageableChannelStatus(common.ChannelStatusAutoDisabled))
	assert.False(t, isManageableChannelStatus(0))
}

// TestChannelFieldsAreClassified guards the fail-closed sensitivity check: every
// JSON field of PatchChannel (including the embedded model.Channel) must be listed
// in channelSensitiveFields, channelNonSensitiveFields, or
// channelOperationalFields. A newly added field that is left unclassified will
// fail this test, forcing a conscious permission decision instead of silently
// defaulting either way.
func TestChannelFieldsAreClassified(t *testing.T) {
	classified := func(name string) bool {
		if _, ok := channelSensitiveFields[name]; ok {
			return true
		}
		if _, ok := channelNonSensitiveFields[name]; ok {
			return true
		}
		if _, ok := channelOperationalFields[name]; ok {
			return true
		}
		_, ok := channelReadOnlyFields[name]
		return ok
	}

	var collect func(rt reflect.Type) []string
	collect = func(rt reflect.Type) []string {
		var names []string
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				names = append(names, collect(field.Type)...)
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			names = append(names, name)
		}
		return names
	}

	for _, name := range collect(reflect.TypeFor[PatchChannel]()) {
		assert.Truef(t, classified(name),
			"channel field %q is not classified; add it to channelSensitiveFields, channelNonSensitiveFields, channelOperationalFields, or channelReadOnlyFields in channel_authz.go", name)
	}
}
