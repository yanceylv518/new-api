package model

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	filterdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestChannelValidateSettingsRejectsInvalidHTTPTransport(t *testing.T) {
	tests := []struct {
		name    string
		setting dto.ChannelSettings
		wantErr string
	}{
		{
			name:    "auto with shards is valid",
			setting: dto.ChannelSettings{HTTPProtocol: "auto", HTTP2ConnectionShards: 4},
		},
		{
			name:    "http1 with shards greater than one rejected",
			setting: dto.ChannelSettings{HTTPProtocol: "http1", HTTP2ConnectionShards: 2},
			wantErr: "http2_connection_shards",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{}
			channel.SetSetting(tt.setting)
			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestChannelConfigurationPreservesUneditedFields(t *testing.T) {
	raw := `{"task_plugin_key":"doubao","proxy":"https://proxy.example","future":{"large":9007199254740993,"keep":true},"video_routing_rules":[{"model":"doubao-seedance-2-0-260128","mode":"deny","resolutions":["4k"]}]}`
	channel := &Channel{Setting: &raw, OtherSettings: `{"future":{"large":9007199254740993},"allow_service_tier":true}`}
	setting := channel.GetSetting()
	setting.Proxy = ""
	require.NoError(t, channel.SetSetting(setting))
	assert.Contains(t, *channel.Setting, `9007199254740993`)
	assert.Contains(t, *channel.Setting, `"video_routing_rules"`)
	assert.Equal(t, "", channel.GetSetting().Proxy)
	other := channel.GetOtherSettings()
	other.AllowServiceTier = false
	require.NoError(t, channel.SetOtherSettings(other))
	assert.Contains(t, channel.OtherSettings, `9007199254740993`)
	assert.False(t, channel.GetOtherSettings().AllowServiceTier)
	updated, err := MergeChannelConfiguration(raw, map[string]common.RawMessage{"future": common.RawMessage(`{"keep":false,"zero":0}`)})
	require.NoError(t, err)
	assert.Contains(t, updated, `9007199254740993`)
	assert.Contains(t, updated, `"zero":0`)
	assert.Contains(t, updated, `"keep":false`)
}

func TestChannelConfigurationReadsDoNotClearInvalidData(t *testing.T) {
	raw := `{"proxy":`
	channel := &Channel{Setting: &raw, OtherSettings: `{"advanced_custom":`}
	channel.GetSetting()
	channel.GetOtherSettings()
	assert.Equal(t, raw, *channel.Setting)
	assert.Equal(t, `{"advanced_custom":`, channel.OtherSettings)
	require.Error(t, channel.SetSetting(dto.ChannelSettings{}))
	assert.Equal(t, raw, *channel.Setting)
	require.Error(t, channel.SetOtherSettings(dto.ChannelOtherSettings{}))
	assert.Equal(t, `{"advanced_custom":`, channel.OtherSettings)
	require.Error(t, channel.ValidateConfigurationEncoding())
	legacyProxy := `{"proxy":"https://proxy.example/legacy?ignored=true"}`
	channel.Setting, channel.OtherSettings = &legacyProxy, "{}"
	require.NoError(t, channel.ValidateConfigurationEncoding(), "runtime must retain legacy proxy compatibility")
	require.Error(t, channel.ValidateSettings(), "new writes still require a strict proxy URL")
}

func TestChannelConfigurationAcceptsLegacyNull(t *testing.T) {
	raw := "null"
	channel := &Channel{Setting: &raw, OtherSettings: "null"}
	require.NoError(t, channel.ValidateConfigurationEncoding())
	assert.Equal(t, "null", *channel.Setting)
	assert.Equal(t, "null", channel.OtherSettings)
	setting := channel.GetSetting()
	setting.Proxy = "https://proxy.example"
	require.NoError(t, channel.SetSetting(setting))
	assert.Equal(t, setting.Proxy, channel.GetSetting().Proxy)
	assert.Equal(t, "null", channel.OtherSettings)
}

func TestChannelConfigurationPatchMySQL(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is not configured")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(db.Migrator().CurrentDatabase(), "codex_origin_"), "configuration tests require an isolated database")
	previousDB := DB
	previousType := common.MainDatabaseType()
	DB = db
	common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	t.Cleanup(func() {
		DB = previousDB
		common.SetMainDatabaseType(previousType)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	raw := `{"task_plugin_key":"doubao","future":{"large":9007199254740993}}`
	channel := &Channel{Name: "configuration-patch", Type: constant.ChannelTypeTaskPlugin, Key: "fixture-key", BaseURL: common.GetPointer("https://upstream.example"), Group: "vip", Models: "doubao-seedance-2-0-260128", Status: common.ChannelStatusEnabled, Setting: &raw}
	require.NoError(t, channel.Insert())
	defer func() {
		require.NoError(t, db.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error)
		require.NoError(t, db.Delete(&Channel{}, channel.Id).Error)
	}()
	patch := ChannelConfigurationPatch{SettingBase: &raw, SettingPatch: map[string]common.RawMessage{"video_routing_rules": common.RawMessage(`[{"model":"doubao-seedance-2-0-260128","mode":"allow","resolutions":["4k"]}]`)}}
	updated := *channel
	require.NoError(t, patch.Apply(&updated, channel))
	require.NoError(t, updated.ValidateSettings())
	require.NoError(t, updated.UpdateWithConfigurationPatch(patch))
	var saved Channel
	require.NoError(t, db.First(&saved, channel.Id).Error)
	assert.Equal(t, channel.Key, saved.Key)
	assert.Equal(t, channel.BaseURL, saved.BaseURL)
	assert.Equal(t, channel.Group, saved.Group)
	assert.Equal(t, channel.Models, saved.Models)
	assert.Contains(t, *saved.Setting, `9007199254740993`)
	assert.Len(t, saved.GetSetting().VideoRoutingRules, 1)
	stale := *channel
	require.NoError(t, patch.Apply(&stale, channel))
	require.ErrorIs(t, stale.UpdateWithConfigurationPatch(patch), ErrChannelConfigurationConflict)
	var after Channel
	require.NoError(t, db.First(&after, channel.Id).Error)
	assert.Equal(t, saved.Setting, after.Setting)
}

func TestAdvancedCustomChannelRequiresModelListRouteOnlyWhenUpdateChecksEnabled(t *testing.T) {
	inferenceRoute := dto.AdvancedCustomRoute{
		IncomingPath: "/v1/chat/completions",
		UpstreamPath: "/v1/chat/completions",
		Converter:    "none",
	}

	tests := []struct {
		name          string
		checksEnabled bool
		routes        []dto.AdvancedCustomRoute
		wantErr       string
	}{
		{
			name:   "legacy channel without discovery route remains valid",
			routes: []dto.AdvancedCustomRoute{inferenceRoute},
		},
		{
			name:          "enabled checks require discovery route",
			checksEnabled: true,
			routes:        []dto.AdvancedCustomRoute{inferenceRoute},
			wantErr:       dto.AdvancedCustomModelListPath,
		},
		{
			name:          "enabled checks accept discovery route",
			checksEnabled: true,
			routes: []dto.AdvancedCustomRoute{
				inferenceRoute,
				{
					IncomingPath: dto.AdvancedCustomModelListPath,
					UpstreamPath: dto.AdvancedCustomModelListPath,
					Converter:    "none",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channel := &Channel{Type: constant.ChannelTypeAdvancedCustom}
			channel.SetOtherSettings(dto.ChannelOtherSettings{
				UpstreamModelUpdateCheckEnabled: tt.checksEnabled,
				AdvancedCustom: &dto.AdvancedCustomConfig{
					Routes: tt.routes,
				},
			})

			err := channel.ValidateSettings()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestInferencePresetSettingsAndDatabaseRoundTrip(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "presets.db"))
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN is not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN is not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			table := db.Table("inference_preset_channels").Session(&gorm.Session{})
			require.NoError(t, table.AutoMigrate(&Channel{}))
			t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable("inference_preset_channels")) })
			var version string
			if dialect == "sqlite" {
				require.NoError(t, db.Raw("select sqlite_version()").Scan(&version).Error)
			} else {
				require.NoError(t, db.Raw("select version()").Scan(&version).Error)
			}
			t.Logf("%s version: %s", dialect, version)
			for _, channelType := range []int{constant.ChannelTypeVLLM, constant.ChannelTypeSGLang} {
				t.Run(fmt.Sprint(channelType), func(t *testing.T) {
					channel := &Channel{Type: channelType, Key: "EMPTY", Name: "inference", Status: common.ChannelStatusEnabled}
					require.NoError(t, channel.ValidateSettings())
					require.NotNil(t, channel.GetOtherSettings().AdvancedCustom)
					require.NoError(t, table.Create(channel).Error)
					for range 2 {
						var loaded Channel
						require.NoError(t, table.First(&loaded, channel.Id).Error)
						assert.Equal(t, channelType, loaded.Type)
						assert.Empty(t, loaded.OtherSettings)
						defaults := loaded.GetOtherSettings().AdvancedCustom
						require.NotNil(t, defaults)
						assert.True(t, defaults.SupportsPath("/v1/messages"))
						assert.Empty(t, loaded.OtherSettings, "reading defaults must not rewrite saved settings")
						defaults.Routes[0].UpstreamPath = "/changed-locally"
						assert.Equal(t, "/v1/chat/completions", loaded.GetOtherSettings().AdvancedCustom.Routes[0].UpstreamPath)
					}
					settings := dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/chat/completions", UpstreamPath: "/custom/chat", Models: []string{"allowed"}, Auth: &dto.AdvancedCustomRouteAuth{Type: "none"}}}}}
					channel.SetOtherSettings(settings)
					require.NoError(t, channel.ValidateSettings())
					require.NoError(t, table.Save(channel).Error)
					for range 2 {
						var loaded Channel
						require.NoError(t, table.First(&loaded, channel.Id).Error)
						actual := loaded.GetOtherSettings().AdvancedCustom
						require.Equal(t, common.GetAdvancedCustomPreset(channelType), actual, "named channels must ignore editable advanced_custom overrides")
						assert.Equal(t, channel.OtherSettings, loaded.OtherSettings)
						for _, tc := range []struct {
							path, model string
							allowed     bool
						}{
							{"/v1/chat/completions", "allowed", true},
							{"/v1/chat/completions", "other", true},
							{"/v1/messages", "allowed", true},
							{"/v1/images/generations", "allowed", false},
						} {
							ok, _ := ChannelSatisfiesFilters(&loaded, tc.model, []filterdto.ChannelFilter{{Kind: filterdto.FilterRequestPath, RequestPath: tc.path}})
							assert.Equal(t, tc.allowed, ok)
						}
						endpoints := getPricingEndpointTypesForAbility(AbilityWithChannel{ChannelType: channelType, Ability: Ability{Model: "allowed", ChannelId: loaded.Id}}, map[int]*dto.AdvancedCustomConfig{loaded.Id: actual})
						expectedEndpoints := []constant.EndpointType{constant.EndpointTypeOpenAI, constant.EndpointTypeOpenAIResponse, constant.EndpointTypeAnthropic, constant.EndpointTypeEmbeddings}
						if channelType == constant.ChannelTypeSGLang {
							expectedEndpoints = append(expectedEndpoints, constant.EndpointTypeJinaRerank)
						}
						assert.ElementsMatch(t, expectedEndpoints, endpoints)
					}
				})
			}
		})
	}
}
