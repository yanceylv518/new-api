package relay

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayhelper "github.com/QuantumNous/new-api/relay/helper"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	driver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func setupRelayChannelDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousType := common.MainDatabaseType()
	previousCache := common.MemoryCacheEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.Channel{}))
	model.DB = database
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.MemoryCacheEnabled = previousCache
		require.NoError(t, sqlDB.Close())
	})
	return database
}

func TestApplyChannelPinPreservesOriginTasksAndRetryMode(t *testing.T) {
	database := setupRelayChannelDB(t)
	channel := &model.Channel{Name: "origin-channel", Key: "sk-test", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeDoubaoVideo}
	require.NoError(t, database.Create(channel).Error)
	originTask := &model.Task{
		TaskID: "task-lock", ChannelId: channel.Id, Action: "text_to_video", Status: model.TaskStatusSuccess,
		PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-task-lock"},
		Data:        []byte(`{"id":"upstream-task-lock"}`),
	}

	for _, tc := range []struct {
		name     string
		tokenPin bool
		apply    func(*gin.Context, *relaycommon.RelayInfo) *dto.TaskError
	}{
		{name: "origin affinity", apply: ApplyOriginTaskAffinity},
		{name: "same channel retry", apply: ApplyChannelPin},
		{name: "token pin suppresses channel lock", tokenPin: true, apply: ApplyChannelPin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			common.SetContextKey(c, constant.ContextKeyOriginTasks, []*model.Task{originTask})
			constraints := service.GetChannelConstraints(c)
			constraints.AddPin(dto.ChannelPin{ChannelId: channel.Id, Source: dto.PinSourceOriginTask, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySameChannel})
			if tc.tokenPin {
				constraints.AddPin(dto.ChannelPin{ChannelId: channel.Id, Source: dto.PinSourceToken, Rank: dto.PinRankToken, RetryMode: dto.PinRetrySingleAttempt})
			}
			info := &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
			require.Nil(t, tc.apply(c, info))
			if tc.tokenPin {
				assert.Nil(t, info.LockedChannel)
			} else {
				locked, ok := info.LockedChannel.(*model.Channel)
				require.True(t, ok)
				require.NotNil(t, locked)
				assert.Equal(t, channel.Id, locked.Id)
			}
			require.Len(t, info.OriginTasks, 1)
			assert.Equal(t, "task-lock", info.OriginTasks[0].TaskID)
			assert.Equal(t, "upstream-task-lock", info.OriginTasks[0].UpstreamTaskID)
			assert.Equal(t, "text_to_video", info.OriginTasks[0].Action)
			assert.Equal(t, string(model.TaskStatusSuccess), info.OriginTasks[0].Status)
			assert.Equal(t, []byte(originTask.Data), info.OriginTasks[0].Data)
		})
	}
}

func TestResolveOriginTaskTokenAuthorizationMySQL(t *testing.T) {
	dsn := os.Getenv("ORIGIN_GROUP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ORIGIN_GROUP_TEST_MYSQL_DSN is not configured")
	}
	dsnConfig, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(dsnConfig.DBName, "codex_origin_"))
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousRedis, previousMemory := common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled
	previousRegistry, previousSecret := pluginruntime.DefaultRegistry, common.CryptoSecret
	previousRatios := ratio_setting.GroupRatio2JSONString()
	previousUsable := setting.UserUsableGroups2JSONString()
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = previousMaster, previousRedis, previousMemory
		pluginruntime.DefaultRegistry, common.CryptoSecret = previousRegistry, previousSecret
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousUsable))
		require.NoError(t, sqlDB.Close())
	})
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = false, false, false
	common.CryptoSecret = "fixture-origin-task-secret"
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.Task{}, &model.User{}, &model.Token{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}))
	require.NoError(t, i18n.Init())
	ratio_setting.GetGroupRatioSetting().GroupRatio.Set("aaa", 1)
	ratio_setting.GetGroupRatioSetting().GroupRatio.Set("vip", 0.7)
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"vip":"VIP","aaa":"AAA","auto":"Auto"}`))
	registry := pluginruntime.NewRegistry()
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	_, err = registry.RegisterFactory(source, pluginruntime.Options{Key: "sora"})
	require.NoError(t, err)
	doubaoSource, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	_, err = registry.RegisterFactory(doubaoSource, pluginruntime.Options{Key: "doubao"})
	require.NoError(t, err)
	pluginruntime.DefaultRegistry = registry
	service.InitHttpClient()
	upstreamPaths := make(chan string, 4)
	upstreamModels := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths <- r.URL.EscapedPath()
		if strings.HasSuffix(r.URL.Path, "/api/v3/contents/generations/tasks") {
			var body struct {
				Model string `json:"model"`
			}
			if common.DecodeJson(r.Body, &body) == nil {
				upstreamModels <- body.Model
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"upstream-new-task","status":"queued"}`))
	}))
	t.Cleanup(upstream.Close)
	channel := model.Channel{Id: 2001, Name: "legacy-origin", Key: "fixture", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeTaskPlugin, Group: "vip", Models: "legacy-model"}
	channel.SetSetting(kitdto.ChannelSettings{TaskPluginKey: "doubao"})
	require.NoError(t, channel.Insert())
	task := model.Task{TaskID: "legacy-origin-task", Platform: "doubao", UserId: 7, ChannelId: channel.Id, Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "legacy-model"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-origin"}}
	require.NoError(t, db.Create(&task).Error)
	extended := model.Channel{Id: 2002, Name: "legacy-extension", Key: "fixture", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeNewAPI, Group: "vip", Models: "legacy-model"}
	extended.SetSetting(kitdto.ChannelSettings{TaskExtendPluginKeys: []string{"doubao"}})
	require.NoError(t, extended.Insert())
	extendedTask := model.Task{TaskID: "legacy-extension-task", Platform: "doubao", UserId: 7, ChannelId: extended.Id, Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "legacy-model"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-extension"}}
	require.NoError(t, db.Create(&extendedTask).Error)
	soraChannel := model.Channel{Id: 2003, Name: "sora-origin", Key: "fixture", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeTaskPlugin, Group: "vip", Models: "sora-2", BaseURL: &upstream.URL}
	soraChannel.SetSetting(kitdto.ChannelSettings{TaskPluginKey: "sora"})
	require.NoError(t, soraChannel.Insert())
	soraTask := model.Task{TaskID: "sora-origin-task", Platform: "sora", UserId: 7, ChannelId: soraChannel.Id, Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "sora-2"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-sora-origin"}}
	require.NoError(t, db.Create(&soraTask).Error)
	soraEscapedTask := model.Task{TaskID: "sora-escaped-task", Platform: "sora", UserId: 7, ChannelId: soraChannel.Id, Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "sora-2"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "source/segment?value#fragment"}}
	require.NoError(t, db.Create(&soraEscapedTask).Error)
	soraExtended := model.Channel{Id: 2004, Name: "sora-extension", Key: "fixture", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeNewAPI, Group: "vip", Models: "sora-2", BaseURL: &upstream.URL}
	soraExtended.SetSetting(kitdto.ChannelSettings{TaskExtendPluginKeys: []string{"sora"}})
	require.NoError(t, soraExtended.Insert())
	aliasMapping := `{"local-seedance":"doubao-seedance-2-0-260128"}`
	aliasChannel := model.Channel{Id: 2005, Name: "alias-video", Key: "fixture", Status: common.ChannelStatusEnabled, Type: constant.ChannelTypeTaskPlugin, Group: "vip", Models: "local-seedance", ModelMapping: &aliasMapping, BaseURL: &upstream.URL}
	aliasChannel.SetSetting(kitdto.ChannelSettings{TaskPluginKey: "doubao"})
	require.NoError(t, aliasChannel.Insert())
	soraLegacyTask := model.Task{TaskID: "sora-legacy-task", Platform: constant.TaskPlatform(strconv.Itoa(constant.ChannelTypeSora)), UserId: 7, ChannelId: soraExtended.Id, Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "sora-2"}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-sora-legacy"}}
	require.NoError(t, db.Create(&soraLegacyTask).Error)
	soraUnmappedTask := model.Task{TaskID: "sora-unmapped-task", Platform: "sora", UserId: 7, ChannelId: soraChannel.Id, Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: "sora-2"}}
	require.NoError(t, db.Create(&soraUnmappedTask).Error)
	user := model.User{Id: 7, Username: "legacyuser", Password: "fixture", AffCode: "legacyuser", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "vip", Quota: 10000000}
	require.NoError(t, db.Create(&user).Error)
	for _, tc := range []struct {
		key, group, auto, limits string
	}{
		{"vip", "vip", "", ""},
		{"aaa", "aaa", "", ""},
		{"autovip", "auto", `["vip"]`, ""},
		{"autoaaa", "auto", `["aaa"]`, ""},
		{"limited", "vip", "", "another-model"},
		{"allowed", "vip", "", "legacy-model"},
	} {
		token := model.Token{UserId: user.Id, Key: "legacyfixture" + tc.key, Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: tc.group, AutoGroups: tc.auto, ModelLimitsEnabled: tc.limits != "", ModelLimits: tc.limits}
		require.NoError(t, db.Create(&token).Error)
	}
	for _, memory := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory_cache_%t", memory), func(t *testing.T) {
			common.MemoryCacheEnabled = memory
			model.InitChannelCache()
			for _, tc := range []struct {
				name, group string
				autoGroups  []string
				modelLimits map[string]bool
				wantStatus  int
			}{
				{"same_group", "vip", nil, nil, 0},
				{"other_group", "aaa", nil, nil, http.StatusForbidden},
				{"auto_allowed", "auto", []string{"vip"}, nil, 0},
				{"auto_excluded", "auto", []string{"aaa"}, nil, http.StatusForbidden},
				{"model_forbidden", "vip", nil, map[string]bool{"another-model": true}, http.StatusForbidden},
				{"model_allowed", "vip", nil, map[string]bool{"legacy-model": true}, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos/legacy-origin-task/remix", nil)
					if tc.name == "other_group" {
						c.Request.Header.Set("Accept-Language", "zh-CN")
					}
					c.Params = gin.Params{{Key: "video_id", Value: "legacy-origin-task"}}
					common.SetContextKey(c, constant.ContextKeyUserGroup, "vip")
					common.SetContextKey(c, constant.ContextKeyUsingGroup, tc.group)
					if tc.autoGroups != nil {
						common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, tc.autoGroups)
					}
					if tc.modelLimits != nil {
						common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
						common.SetContextKey(c, constant.ContextKeyTokenModelLimit, tc.modelLimits)
					}
					info := &relaycommon.RelayInfo{UserId: 7, UsingGroup: tc.group, UserGroup: "vip", ChannelMeta: &relaycommon.ChannelMeta{}, TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
					taskErr := ResolveOriginTask(c, info)
					if tc.wantStatus != 0 {
						if assert.NotNil(t, taskErr) {
							assert.Equal(t, tc.wantStatus, taskErr.StatusCode)
							if tc.name == "other_group" {
								assert.Contains(t, taskErr.Message, "源任务所属渠道")
							}
						}
						assert.Nil(t, info.LockedChannel)
						return
					}
					require.Nil(t, taskErr)
					locked, ok := info.LockedChannel.(*model.Channel)
					require.True(t, ok)
					assert.Equal(t, channel.Id, locked.Id)
					assert.Equal(t, "upstream-origin", info.OriginTaskID)
					if tc.group == "auto" {
						assert.Equal(t, "vip", common.GetContextKeyString(c, constant.ContextKeyAutoGroup))
						assert.Equal(t, "vip", info.UsingGroup)
						assert.InDelta(t, 0.7, relayhelper.HandleGroupRatio(c, info).GroupRatio, 0.000001)
					}
				})
			}
			t.Run("http_remix", func(t *testing.T) {
				router := gin.New()
				router.Use(gin.Recovery())
				router.POST("/v1/videos/:video_id/remix", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) {
					info, genErr := relaycommon.GenRelayInfo(c, relaytypes.RelayFormatTask, nil, nil)
					require.NoError(t, genErr)
					if taskErr := ResolveOriginTask(c, info); taskErr != nil {
						c.Status(taskErr.StatusCode)
						return
					}
					c.Header("X-Selected-Channel", fmt.Sprint(info.ChannelId))
					c.Header("X-Selected-Plugin", c.GetString("task_plugin_key"))
					c.Header("X-Origin-Task", info.OriginTaskID)
					c.Header("X-Selected-Auto-Group", common.GetContextKeyString(c, constant.ContextKeyAutoGroup))
					c.Status(http.StatusNoContent)
				})
				for _, tc := range []struct {
					key, task, upstream string
					status              int
					channel             int
				}{
					{"vip", "legacy-origin-task", "upstream-origin", http.StatusNoContent, channel.Id},
					{"aaa", "legacy-origin-task", "", http.StatusForbidden, 0},
					{"autovip", "legacy-origin-task", "upstream-origin", http.StatusNoContent, channel.Id},
					{"autoaaa", "legacy-origin-task", "", http.StatusForbidden, 0},
					{"limited", "legacy-origin-task", "", http.StatusForbidden, 0},
					{"allowed", "legacy-origin-task", "upstream-origin", http.StatusNoContent, channel.Id},
					{"vip", "legacy-extension-task", "upstream-extension", http.StatusNoContent, extended.Id},
					{"aaa", "legacy-extension-task", "", http.StatusForbidden, 0},
				} {
					req := httptest.NewRequest(http.MethodPost, "/v1/videos/"+tc.task+"/remix", strings.NewReader(`{}`))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer sk-legacyfixture"+tc.key)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, req)
					assert.Equal(t, tc.status, response.Code, tc.key)
					if tc.status == http.StatusNoContent && response.Code == tc.status {
						assert.Equal(t, fmt.Sprint(tc.channel), response.Header().Get("X-Selected-Channel"))
						assert.Equal(t, "doubao", response.Header().Get("X-Selected-Plugin"))
						assert.Equal(t, tc.upstream, response.Header().Get("X-Origin-Task"))
						if tc.key == "autovip" {
							assert.Equal(t, "vip", response.Header().Get("X-Selected-Auto-Group"))
						}
					}
				}
			})
			t.Run("sora_remix_submit", func(t *testing.T) {
				router := gin.New()
				router.POST("/v1/videos/:video_id/remix", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) {
					info, genErr := relaycommon.GenRelayInfo(c, relaytypes.RelayFormatTask, nil, nil)
					require.NoError(t, genErr)
					if taskErr := ResolveOriginTask(c, info); taskErr != nil {
						c.String(taskErr.StatusCode, taskErr.Message)
						return
					}
					info.Billing = &imageReservation{limit: 1 << 30}
					result, taskErr := RelayTaskSubmit(c, info)
					if taskErr != nil {
						c.String(taskErr.StatusCode, taskErr.Message)
						return
					}
					require.NotNil(t, result)
					c.Header("X-Task-Action", info.Action)
					c.Header("X-Group-Ratio", fmt.Sprint(info.PriceData.GroupRatioInfo.GroupRatio))
					c.Status(http.StatusNoContent)
				})
				for _, key := range []string{"aaa", "autoaaa", "limited"} {
					req := httptest.NewRequest(http.MethodPost, "/v1/videos/sora-origin-task/remix", strings.NewReader(`{"prompt":"local test"}`))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer sk-legacyfixture"+key)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, req)
					assert.Equal(t, http.StatusForbidden, response.Code, key)
				}
				assert.Empty(t, upstreamPaths)
				for _, tc := range []struct{ task, path string }{
					{"sora-origin-task", "/v1/videos/upstream-sora-origin/remix"},
					{"sora-escaped-task", "/v1/videos/source%2Fsegment%3Fvalue%23fragment/remix"},
					{"sora-legacy-task", "/v1/videos/upstream-sora-legacy/remix"},
					{"sora-unmapped-task", "/v1/videos/sora-unmapped-task/remix"},
				} {
					req := httptest.NewRequest(http.MethodPost, "/v1/videos/"+tc.task+"/remix", strings.NewReader(`{"prompt":"local test"}`))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer sk-legacyfixtureautovip")
					response := httptest.NewRecorder()
					router.ServeHTTP(response, req)
					require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
					assert.Equal(t, "remix", response.Header().Get("X-Task-Action"))
					assert.Equal(t, "0.7", response.Header().Get("X-Group-Ratio"))
					select {
					case path := <-upstreamPaths:
						assert.Equal(t, tc.path, path)
					default:
						t.Error("mock upstream was not called")
					}
				}
			})
			t.Run("mapped_alias_submit_and_billing", func(t *testing.T) {
				saveBillingConfig(t)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
					"billing_setting.billing_mode": `{"doubao-seedance-2-0-260128":"tiered_expr"}`,
					"billing_setting.billing_expr": `{"doubao-seedance-2-0-260128":"tier(\"alias-test\", u(\"tokens\") / 1000000)"}`,
				}))
				route := "/doubao/api/v3/contents/generations/tasks"
				bindings := registry.Generation().LookupDeclaredRouteCandidates(http.MethodPost, route)
				require.NotEmpty(t, bindings)
				router := gin.New()
				router.Use(func(c *gin.Context) { c.Next(); common.CleanupBodyStorage(c) })
				router.POST(route, func(c *gin.Context) {
					c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: registry.Generation(), Plugin: bindings[0].Plugin, Route: bindings[0].Route, Candidates: bindings})
					c.Next()
				}, middleware.TokenAuth(), middleware.PrepareTaskPluginRoute(), middleware.Distribute(), func(c *gin.Context) {
					info, genErr := relaycommon.GenRelayInfo(c, relaytypes.RelayFormatTask, nil, nil)
					require.NoError(t, genErr)
					info.UserSetting.BillingPreference = "wallet_only"
					info.UserModelDiscountBPS = hosttypes.NewUserModelDiscountSnapshot(map[string]int{"local-seedance": 8000, "doubao-seedance-2-0-260128": 3000})
					beforeQuota, quotaErr := model.GetUserQuota(info.UserId, false)
					require.NoError(t, quotaErr)
					var beforeToken model.Token
					require.NoError(t, db.First(&beforeToken, info.TokenId).Error)
					result, taskErr := RelayTaskSubmit(c, info)
					if taskErr != nil {
						c.String(taskErr.StatusCode, taskErr.Message)
						return
					}
					require.NotNil(t, result)
					require.NotNil(t, info.TieredBillingSnapshot)
					assert.Equal(t, "local-seedance", info.OriginModelName)
					assert.Equal(t, "doubao-seedance-2-0-260128", info.UpstreamModelName)
					assert.Equal(t, "local-seedance", info.TieredBillingSnapshot.ModelName)
					assert.Equal(t, `tier("alias-test", u("tokens") / 1000000)`, info.TieredBillingSnapshot.ExprString)
					assert.InDelta(t, 0.7, info.TieredBillingSnapshot.GroupRatio, 0.000001)
					assert.InDelta(t, 0.8, info.PriceData.UserModelDiscountMultiplier(), 0.000001)
					assert.Positive(t, info.PriceData.Quota)
					require.NotNil(t, info.Billing)
					reserved := info.Billing.GetPreConsumedQuota()
					assert.Equal(t, info.PriceData.Quota, reserved)
					afterQuota, quotaErr := model.GetUserQuota(info.UserId, false)
					require.NoError(t, quotaErr)
					assert.Equal(t, beforeQuota-reserved, afterQuota)
					var afterToken model.Token
					require.NoError(t, db.First(&afterToken, info.TokenId).Error)
					assert.Equal(t, beforeToken.UsedQuota+reserved, afterToken.UsedQuota)
					require.NoError(t, info.Billing.Settle(0))
					refundedQuota, quotaErr := model.GetUserQuota(info.UserId, false)
					require.NoError(t, quotaErr)
					assert.Equal(t, beforeQuota, refundedQuota)
					require.NoError(t, db.First(&afterToken, info.TokenId).Error)
					assert.Equal(t, beforeToken.UsedQuota, afterToken.UsedQuota)
					c.Status(http.StatusNoContent)
				})
				req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(`{"model":"local-seedance","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer sk-legacyfixturevip")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
				select {
				case path := <-upstreamPaths:
					assert.Equal(t, "/api/v3/contents/generations/tasks", path)
				default:
					t.Error("mock upstream was not called")
				}
				select {
				case sentModel := <-upstreamModels:
					assert.Equal(t, "doubao-seedance-2-0-260128", sentModel)
				default:
					t.Error("mock upstream model was not recorded")
				}
			})
		})
	}
}

func TestTaskModel2DtoNormalizesLegacyAction(t *testing.T) {
	task := &model.Task{Action: "firstTailGenerate"}

	dtoTask := TaskModel2Dto(task)

	assert.Equal(t, constant.TaskActionFirstTailToVideo, dtoTask.Action)
	assert.Equal(t, "firstTailGenerate", task.Action)
}

const mappingOrderSubmitPlugin = `
export const meta = {apiVersion:1,key:"maporder",name:"Map Order",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

const mappingOrderRewritePlugin = `
export const meta = {apiVersion:1,key:"maporder-rw",name:"Map Order RW",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel}, rewriteModel:"rewritten"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func pinMappingOrderPlugin(t *testing.T, c *gin.Context, source string) {
	t.Helper()
	plugin, err := pluginruntime.NewRegistry().Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
}

func newTaskSubmitContext(t *testing.T, originalModel, mapping string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, originalModel)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://provider.example")
	if mapping != "" {
		c.Set("model_mapping", mapping)
	}
	c.Set("task_request", map[string]any{"prompt": "p"})
	return c, &relaycommon.RelayInfo{TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
}

func TestRelayTaskSubmitMapsBeforeValidateWhenOriginSet(t *testing.T) {
	const mapping = `{"alias-model":"mid-model","mid-model":"declared-model"}`

	c, info := newTaskSubmitContext(t, "alias-model", mapping)
	pinMappingOrderPlugin(t, c, mappingOrderSubmitPlugin)
	info.OriginModelName = "alias-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "alias-model", info.OriginModelName)
	assert.Equal(t, "declared-model", info.UpstreamModelName)
	assert.True(t, info.IsModelMapped)
}

func TestRelayTaskSubmitDeclaredNameWithoutMappingIsUnchanged(t *testing.T) {
	c, info := newTaskSubmitContext(t, "declared-model", "")
	pinMappingOrderPlugin(t, c, mappingOrderSubmitPlugin)
	info.OriginModelName = "declared-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "declared-model", info.OriginModelName)
	assert.Equal(t, "declared-model", info.UpstreamModelName)
	assert.False(t, info.IsModelMapped)
}

func TestRelayTaskSubmitDoesNotApplyMappingTwice(t *testing.T) {
	c, info := newTaskSubmitContext(t, "alias-model", `{"alias-model":"declared-model"}`)
	pinMappingOrderPlugin(t, c, mappingOrderRewritePlugin)
	info.OriginModelName = "alias-model"

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, "rewritten", info.UpstreamModelName, "late mapping would overwrite rewriteModel with the chain tail")
	assert.Equal(t, "alias-model", info.OriginModelName)
}

func TestRelayTaskSubmitEmptyOriginKeepsLateMapping(t *testing.T) {
	plugin, err := pluginruntime.NewRegistry().Register(mappingOrderSubmitPlugin, pluginruntime.Options{})
	require.NoError(t, err)
	synthesized := service.CoverTaskActionToModelName(constant.TaskPlatform(plugin.Meta.Key), "text_to_video")
	c, info := newTaskSubmitContext(t, "pre-validate-upstream",
		`{"pre-validate-upstream":"should-not-apply-early","`+synthesized+`":"legacy-tail"}`)
	c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
	info.OriginModelName = ""

	_, taskErr := RelayTaskSubmit(c, info)
	require.NotNil(t, taskErr)
	assert.Equal(t, "model_price_error", taskErr.Code)
	assert.Equal(t, synthesized, info.OriginModelName)
	assert.Equal(t, "legacy-tail", info.UpstreamModelName)
	assert.True(t, info.IsModelMapped)
}

const billingFallbackPlugin = `
export const meta = {apiVersion:1,key:"bill-fallback",name:"Bill Fallback",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) {
  return {url: ctx.baseUrl+"/submit", method:"POST", body:{upstreamModel: ctx.upstreamModel, model: ctx.model}, action:"text_to_video"};
}
export function parseSubmitResponse(){return {taskId:"1"};}
export function buildQueryRequest(){return {url:"https://provider.example"};}
export function parseTaskResult(){return {status:"SUCCESS"};}
`

func saveBillingConfig(t *testing.T) {
	t.Helper()
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		saved[key] = value
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})
}

func TestRelayTaskSubmitAliasBillingIdentityAndExprFallback(t *testing.T) {
	const mapping = `{"alias-model":"declared-model"}`
	const aliasExpr = `tier("alias", 2)`
	const tailExpr = `tier("tail", 3)`
	const legacyExpr = `tier("legacy", u("old_units") * 2)`

	tests := []struct {
		name       string
		modes      map[string]string
		exprs      map[string]string
		wantTiered bool
		wantExpr   string
		source     string
	}{
		{
			name:       "alias own tiered wins",
			modes:      map[string]string{"alias-model": "tiered_expr", "declared-model": "tiered_expr"},
			exprs:      map[string]string{"alias-model": aliasExpr, "declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   aliasExpr,
		},
		{
			name:       "fallback uses tail expr",
			modes:      map[string]string{"declared-model": "tiered_expr"},
			exprs:      map[string]string{"declared-model": tailExpr},
			wantTiered: true,
			wantExpr:   tailExpr,
		},
		{
			name:       "stored expression keeps running after schema narrows",
			modes:      map[string]string{"declared-model": "tiered_expr"},
			exprs:      map[string]string{"declared-model": legacyExpr},
			wantTiered: true,
			wantExpr:   legacyExpr,
			source: strings.Replace(billingFallbackPlugin, `fetchMode:"per_task"`, `fetchMode:"per_task", usageSchema:{old_units:{type:"number",unit:"count"}}, usageProfiles:[{models:["declared-model"],schema:{seconds:{type:"number",unit:"second"}}}]`, 1) + `
export function extractUsage(){return {old_units:2};}
`,
		},
		{
			name:  "missing mapped expression does not use legacy ratio",
			modes: map[string]string{"declared-model": "tiered_expr"},
		},
		{
			name:       "neither tiered uses ordinary pricing",
			wantTiered: false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			saveBillingConfig(t)
			if testCase.name == "missing mapped expression does not use legacy ratio" {
				previousRatios := ratio_setting.ModelRatio2JSONString()
				t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios)) })
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"declared-model":3}`))
			}
			if len(testCase.modes) > 0 {
				modeJSON, marshalErr := common.Marshal(testCase.modes)
				require.NoError(t, marshalErr)
				exprJSON, marshalErr := common.Marshal(testCase.exprs)
				require.NoError(t, marshalErr)
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
					"billing_setting.billing_mode": string(modeJSON),
					"billing_setting.billing_expr": string(exprJSON),
				}))
				if testCase.wantExpr == aliasExpr {
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("alias-model"))
				} else {
					require.Equal(t, billing_setting.BillingModeRatio, billing_setting.GetBillingMode("alias-model"))
					require.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("declared-model"))
				}
			}

			c, info := newTaskSubmitContext(t, "alias-model", mapping)
			c.Set("group", "default")
			info.UserGroup = "default"
			info.UsingGroup = "default"
			source := testCase.source
			if source == "" {
				source = billingFallbackPlugin
			}
			pinMappingOrderPlugin(t, c, source)
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{DeclaredModel: "declared-model"})
			info.OriginModelName = "alias-model"
			// 上游映射不能把用户公开模型的 80% 折扣换成上游名的 30%。
			info.UserModelDiscountBPS = hosttypes.NewUserModelDiscountSnapshot(map[string]int{"alias-model": 8000, "declared-model": 3000})

			_, taskErr := RelayTaskSubmit(c, info)
			require.NotNil(t, taskErr)
			assert.Equal(t, "alias-model", info.OriginModelName)
			assert.Equal(t, "declared-model", info.UpstreamModelName)
			assert.True(t, info.IsModelMapped)

			task := model.InitTask(constant.TaskPlatform("bill-fallback"), info)
			assert.Equal(t, "alias-model", task.Properties.OriginModelName)
			assert.Equal(t, "declared-model", task.Properties.UpstreamModelName)

			if testCase.wantTiered {
				require.NotNil(t, info.TieredBillingSnapshot, "submission error: %+v", taskErr)
				assert.Equal(t, "alias-model", info.TieredBillingSnapshot.ModelName)
				assert.Equal(t, testCase.wantExpr, info.TieredBillingSnapshot.ExprString)
				assert.Equal(t, billingexpr.ExprHashString(testCase.wantExpr), info.TieredBillingSnapshot.ExprHash)
				before, after := 1000000, 800000
				if testCase.wantExpr == tailExpr {
					before, after = 1500000, 1200000
				} else if testCase.wantExpr == legacyExpr {
					before, after = 2000000, 1600000
				}
				assert.Equal(t, hosttypes.NewDiscountAmounts(before, after), info.PriceData.DiscountAmounts)
				assert.NotEqual(t, "model_price_error", taskErr.Code)
				if testCase.wantExpr == legacyExpr {
					assert.Equal(t, 4*common.QuotaPerUnit, info.TieredBillingSnapshot.EstimatedQuotaBeforeGroup)
				}
			} else {
				assert.Nil(t, info.TieredBillingSnapshot)
				assert.Equal(t, "model_price_error", taskErr.Code)
			}
		})
	}
}

func TestTaskAliasPerCallPricingAndDiscount(t *testing.T) {
	previousPrices := ratio_setting.ModelPrice2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices)) })
	for _, tc := range []struct {
		name, prices, upstream string
		want                   float64
		discounts              map[string]int
		wantDiscount           float64
	}{
		{"mapped_price", `{"declared-model":3}`, "", 3, map[string]int{"alias-model": 8000, "declared-model": 3000}, 0.8},
		{"declared_price_before_vendor_endpoint", `{"declared-model":3,"final-model":4}`, "final-model", 3, map[string]int{"alias-model": 8000, "declared-model": 3000}, 0.8},
		{"alias_override", `{"declared-model":3,"alias-model":2,"final-model":4}`, "final-model", 2, map[string]int{"alias-model": 8000, "declared-model": 3000}, 0.8},
		{"alias_free_price", `{"declared-model":3,"alias-model":0}`, "", 0, map[string]int{"alias-model": 8000}, 0.8},
		{"unset_alias_discount_is_full_price", `{"declared-model":3}`, "", 3, map[string]int{"declared-model": 3000}, 1},
		{"explicit_full_alias_discount_is_independent", `{"declared-model":3}`, "", 3, map[string]int{"alias-model": 10000, "declared-model": 3000}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(tc.prices))
			c, info := newTaskSubmitContext(t, "alias-model", `{"alias-model":"declared-model"}`)
			c.Set("group", "default")
			c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{DeclaredModel: "declared-model"})
			info.OriginModelName = "alias-model"
			info.BillingModelName = model.ResolveTaskPriceModel("bill-fallback", "alias-model", "declared-model")
			info.DiscountModelName = "alias-model"
			if tc.upstream != "" {
				info.ChannelMeta = &relaycommon.ChannelMeta{UpstreamModelName: tc.upstream}
			}
			info.UserGroup, info.UsingGroup = "default", "default"
			info.StartTime = time.Now()
			info.UserModelDiscountBPS = hosttypes.NewUserModelDiscountSnapshot(tc.discounts)
			price, err := relayhelper.ModelPriceHelperPerCall(c, info)
			require.NoError(t, err)
			assert.Equal(t, tc.want, price.ModelPrice)
			assert.InDelta(t, tc.wantDiscount, price.UserModelDiscountMultiplier(), 0.000001)
			assert.Equal(t, "alias-model", info.OriginModelName)
		})
	}
}

func TestSharedTaskBillingExpressionSelectionAndFrozenSettlement(t *testing.T) {
	const baseExpr = `tier("base", u("seconds") * 2)`
	const alphaExpr = `tier("alpha", u("seconds") * 3)`
	const betaExpr = `tier("beta", u("credits") * 5)`
	const aliasExpr = `tier("alias", u("credits") * 7)`
	for _, tc := range []struct {
		name, plugin, model, mapping, modelExpr, mode, wantExpr string
		variants                                                map[string]string
		wantPriceError, profiled                                bool
	}{
		{name: "executing plugin override", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-alpha::declared-model": alphaExpr, "billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "override ignores model mode", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "ratio", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "model expression fallback", plugin: "billing-alpha", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", wantExpr: baseExpr},
		{name: "alias override precedes mapped override", plugin: "billing-beta", model: "alias-model", mapping: `{"alias-model":"declared-model"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr, "billing-beta::alias-model": aliasExpr}, wantExpr: aliasExpr},
		{name: "mapped override precedes model fallback", plugin: "billing-beta", model: "alias-model", mapping: `{"alias-model":"declared-model"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr},
		{name: "unconfigured plugin cannot use another schema", plugin: "billing-beta", model: "declared-model", modelExpr: baseExpr, mode: "tiered_expr", wantPriceError: true},
		{name: "missing usage in skipped branch remains incompatible", plugin: "billing-beta", model: "declared-model", modelExpr: `true ? tier("free", 0) : tier("missing", u("seconds"))`, mode: "tiered_expr", wantPriceError: true},
		{name: "fixed pricing is still rejected", plugin: "billing-beta", model: "declared-model", variants: map[string]string{"billing-beta::declared-model": `tier("fixed", fixed(1))`}, wantPriceError: true},
		{name: "endpoint mapping keeps the declared profile", plugin: "billing-beta", model: "declared-model", mapping: `{"declared-model":"ep-endpoint"}`, modelExpr: baseExpr, mode: "tiered_expr", variants: map[string]string{"billing-beta::declared-model": betaExpr}, wantExpr: betaExpr, profiled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saveBillingConfig(t)
			registry := pluginruntime.NewRegistry()
			for _, spec := range []struct{ key, field, unit string }{{"billing-alpha", "seconds", "second"}, {"billing-beta", "credits", "credit"}} {
				source := strings.ReplaceAll(billingFallbackPlugin, "bill-fallback", spec.key)
				schema := `usageSchema:{` + spec.field + `:{type:"number",unit:"` + spec.unit + `"}}`
				if tc.profiled && spec.key == "billing-beta" {
					// The endpoint ID is undeclared, so only the declared model's profile carries this schema.
					schema = `usageSchema:{seconds:{type:"number",unit:"second"}},usageProfiles:[{models:["declared-model"],schema:{` + spec.field + `:{type:"number",unit:"` + spec.unit + `"}}}]`
				}
				source = strings.Replace(source, `fetchMode:"per_task"`, `fetchMode:"per_task",`+schema, 1)
				source += `export function extractUsage(){return {` + spec.field + `:2};}`
				_, err := registry.Register(source, pluginruntime.Options{})
				require.NoError(t, err)
			}
			variants := tc.variants
			if variants == nil {
				variants = map[string]string{}
			}
			rawVariants, err := common.Marshal(variants)
			require.NoError(t, err)
			modes, err := common.Marshal(map[string]string{"declared-model": tc.mode})
			require.NoError(t, err)
			expressions, err := common.Marshal(map[string]string{"declared-model": tc.modelExpr})
			require.NoError(t, err)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				billing_setting.PluginBillingExprOption: string(rawVariants), "billing_setting.billing_mode": string(modes), "billing_setting.billing_expr": string(expressions),
			}))
			c, info := newTaskSubmitContext(t, tc.model, tc.mapping)
			c.Set("group", "default")
			c.Set("task_plugin_key", tc.plugin)
			info.UserGroup = "default"
			info.UsingGroup = "default"
			info.OriginModelName = tc.model
			plugin, ok := registry.Generation().Get(tc.plugin)
			require.True(t, ok)
			c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Generation: registry.Generation(), Plugin: plugin})
			_, taskErr := RelayTaskSubmit(c, info)
			require.NotNil(t, taskErr) // This fixture stops at reservation, before upstream submission.
			if tc.wantPriceError {
				assert.Equal(t, "model_price_error", taskErr.Code)
				assert.Nil(t, info.TieredBillingSnapshot)
				return
			}
			require.NotNil(t, info.TieredBillingSnapshot, "submission error: %+v", taskErr)
			assert.Equal(t, tc.wantExpr, info.TieredBillingSnapshot.ExprString)
			assert.NotEqual(t, "model_price_error", taskErr.Code)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{billing_setting.PluginBillingExprOption: `{}`, "billing_setting.billing_expr": `{}`}))
			field := "seconds"
			if tc.plugin == "billing-beta" {
				field = "credits"
			}
			result, usage, err := service.EvaluateTaskCompletionUsage(info.TieredBillingSnapshot, map[string]any{field: float64(4)})
			require.NoError(t, err)
			assert.Equal(t, float64(4), usage[field])
			assert.Equal(t, float64(2), info.TieredBillingSnapshot.UsageFacts[field])
			assert.Equal(t, 2*info.TieredBillingSnapshot.EstimatedQuotaAfterGroup, result.ActualQuotaAfterGroup)
			assert.Equal(t, tc.wantExpr, info.TieredBillingSnapshot.ExprString)
		})
	}
}

// Issue #7478: task APIs answer 201 Created or 202 Accepted on submission.
// Every 2xx must reach parseSubmitResponse; only other statuses are upstream
// failures that keep the upstream status and body.
func TestRelayTaskSubmitAcceptsAnySuccessfulUpstreamStatus(t *testing.T) {
	service.InitHttpClient()
	const source = `
export const meta = {apiVersion:1,key:"status-echo",name:"Status Echo",version:"1.0.0",author:{name:"Test"},models:["declared-model"],fetchMode:"per_task"};
export function buildSubmitRequest(ctx) { return {url: ctx.baseUrl+"/submit", method:"POST", body:{model: ctx.model}, action:"text_to_video"}; }
export function parseSubmitResponse(ctx, response) { return {taskId: response.body.id, taskData: {status: response.statusCode}}; }
export function buildQueryRequest(ctx) { return {url: ctx.baseUrl+"/query"}; }
export function parseTaskResult() { return {status:"SUCCESS"}; }
`
	for _, tc := range []struct {
		status   int
		wantCode string
	}{
		{status: http.StatusOK},
		{status: http.StatusCreated},
		{status: http.StatusAccepted},
		{status: http.StatusBadRequest, wantCode: "fail_to_fetch_task"},
		{status: http.StatusBadGateway, wantCode: "fail_to_fetch_task"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			saveBillingConfig(t)
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				"billing_setting.billing_mode": `{"declared-model":"tiered_expr"}`,
				"billing_setting.billing_expr": `{"declared-model":"tier(\"flat\", 3)"}`,
			}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"id":"job-42","message":"upstream body"}`))
			}))
			defer server.Close()

			c, info := newTaskSubmitContext(t, "declared-model", "")
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
			c.Set("group", "default")
			info.UserGroup, info.UsingGroup = "default", "default"
			info.OriginModelName = "declared-model"
			// An existing reservation skips pre-consume so the fixture reaches the upstream call.
			info.Billing = &imageReservation{limit: 1 << 30}
			pinMappingOrderPlugin(t, c, source)

			result, taskErr := RelayTaskSubmit(c, info)
			if tc.wantCode != "" {
				require.NotNil(t, taskErr)
				assert.Equal(t, tc.wantCode, taskErr.Code)
				assert.Equal(t, tc.status, taskErr.StatusCode)
				assert.Contains(t, taskErr.Message, "upstream body")
				assert.Nil(t, result)
				return
			}
			require.Nil(t, taskErr, "submission error: %+v", taskErr)
			require.NotNil(t, result)
			assert.Equal(t, "job-42", result.UpstreamTaskID)
			assert.JSONEq(t, `{"status":`+strconv.Itoa(tc.status)+`}`, string(result.TaskData))
		})
	}
}
