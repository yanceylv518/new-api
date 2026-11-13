package middleware

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	taskadaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayhelper "github.com/QuantumNous/new-api/relay/helper"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestOriginTaskTokenAuthorizationMySQL(t *testing.T) {
	dsn := os.Getenv("ORIGIN_GROUP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("ORIGIN_GROUP_TEST_MYSQL_DSN is not configured")
	}
	config, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(config.DBName, "codex_origin_"))
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousRedis, previousMemory := common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled
	previousRegistry := jsplugin.DefaultRegistry
	previousRatios := ratio_setting.GroupRatio2JSONString()
	previousUsable, previousAuto := setting.UserUsableGroups2JSONString(), setting.AutoGroups2JsonString()
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = previousMaster, previousRedis, previousMemory
		jsplugin.DefaultRegistry = previousRegistry
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousUsable))
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(previousAuto))
		require.NoError(t, sqlDB.Close())
	})
	model.DB = db
	common.SetMainDatabaseType(common.DatabaseTypeMySQL)
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = false, false, false
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.Channel{}, &model.Ability{}, &model.Task{}))
	require.NoError(t, i18n.Init())
	ratio_setting.GetGroupRatioSetting().GroupRatio.Set("aaa", 1)
	ratio_setting.GetGroupRatioSetting().GroupRatio.Set("vip", 0.7)
	ratio_setting.GetGroupRatioSetting().GroupRatio.Set("routing", 1)
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"vip":"VIP","aaa":"AAA","routing":"Routing","auto":"Auto"}`))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["vip","aaa","routing"]`))

	registry := jsplugin.NewRegistry()
	for _, key := range []string{"doubao", "anyllm", "hailuo", "kling", "vidu"} {
		source, sourceErr := builtinplugins.Source(key)
		require.NoError(t, sourceErr)
		_, registerErr := registry.RegisterFactory(source, jsplugin.Options{Key: key})
		require.NoError(t, registerErr)
	}
	jsplugin.DefaultRegistry = registry
	generation := registry.Generation()
	for id := 1; id <= 2; id++ {
		user := model.User{Id: id, Username: fmt.Sprintf("originuser%d", id), Password: "fixture", AffCode: fmt.Sprintf("origin%d", id), Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "vip", Quota: 1000000}
		require.NoError(t, db.Create(&user).Error)
	}
	for _, tc := range []struct {
		key, group, auto string
		user             int
		limited          bool
	}{
		{"vip", "vip", "", 1, false},
		{"aaa", "aaa", "", 1, false},
		{"autovip", "auto", `["vip"]`, 1, false},
		{"autoaaa", "auto", `["aaa"]`, 1, false},
		{"limited", "vip", "", 1, true},
		{"allowmodels", "vip", "", 1, true},
		{"aliasallow", "vip", "", 1, true},
		{"inherit", "", "", 1, false},
		{"foreign", "vip", "", 2, false},
		{"routing", "routing", "", 1, false},
		{"autorouting", "auto", `["routing"]`, 1, false},
		{"routinglimited", "routing", "", 1, true},
	} {
		token := model.Token{UserId: tc.user, Key: "originfixture" + tc.key, Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: tc.group, AutoGroups: tc.auto, ModelLimitsEnabled: tc.limited, ModelLimits: "different-model"}
		if tc.key == "allowmodels" {
			token.ModelLimits = "doubao-seedance-2-5-260628,MiniMax-H3"
		}
		if tc.key == "aliasallow" {
			token.ModelLimits = "shared-video"
		}
		require.NoError(t, db.Create(&token).Error)
	}
	var upstreamRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	addChannel := func(id int, group, key, modelName string) {
		channel := model.Channel{Id: id, Type: constant.ChannelTypeTaskPlugin, Name: fmt.Sprintf("origin%d", id), Key: "fixture", Status: common.ChannelStatusEnabled, Group: group, Models: modelName, BaseURL: &upstream.URL}
		channel.SetSetting(kitdto.ChannelSettings{TaskPluginKey: key})
		require.NoError(t, channel.Insert())
	}
	addChannel(101, "vip", "doubao", "doubao-seedance-2-5-260628")
	addChannel(102, "aaa", "doubao", "doubao-seedance-2-5-260628")
	addChannel(103, "vip", "hailuo", "MiniMax-H3")
	addChannel(104, "aaa", "hailuo", "MiniMax-H3")
	addChannel(105, "vip", "doubao", "doubao-seedance-2-0-260128")
	addChannel(106, "vip", "hailuo", "MiniMax-Hailuo-2.3")
	addAliasChannel := func(id int, group, plugin, alias, target string, priority int64, status int) {
		mapping := fmt.Sprintf(`{"%s":"%s"}`, alias, target)
		key := "fixture"
		if plugin == "kling" {
			key = "test-access|test-secret"
		}
		channel := model.Channel{Id: id, Type: constant.ChannelTypeTaskPlugin, Name: fmt.Sprintf("alias%d", id), Key: key, Status: status, Group: group, Models: alias, ModelMapping: &mapping, Priority: &priority, BaseURL: &upstream.URL}
		channel.SetSetting(kitdto.ChannelSettings{TaskPluginKey: plugin})
		require.NoError(t, channel.Insert())
	}
	addAliasChannel(201, "vip", "doubao", "shared-video", "doubao-seedance-2-0-260128", 0, common.ChannelStatusEnabled)
	addAliasChannel(202, "vip", "anyllm", "shared-video", "doubao-seedance-2-0-260128", 30, common.ChannelStatusEnabled)
	addAliasChannel(203, "aaa", "doubao", "shared-video", "doubao-seedance-2-5-260628", 0, common.ChannelStatusEnabled)
	addAliasChannel(204, "vip", "doubao", "shared-video", "doubao-seedance-2-5-260628", 10, common.ChannelStatusEnabled)
	addAliasChannel(205, "vip", "hailuo", "shared-video", "MiniMax-H3", 40, common.ChannelStatusEnabled)
	addAliasChannel(206, "vip", "doubao", "shared-video", "unknown-upstream-model", 50, common.ChannelStatusEnabled)
	addAliasChannel(207, "vip", "doubao", "shared-video", "doubao-seedance-2-0-260128", 60, common.ChannelStatusManuallyDisabled)
	addAliasChannel(208, "vip", "hailuo", "short-h3", "MiniMax-H3", 0, common.ChannelStatusEnabled)
	addAliasChannel(209, "vip", "kling", "short-kling", "kling-v3", 0, common.ChannelStatusEnabled)
	addAliasChannel(210, "vip", "kling", "short-kling-new", "kling-3.0", 0, common.ChannelStatusEnabled)
	addAliasChannel(211, "vip", "vidu", "short-vidu", "viduq3-turbo", 0, common.ChannelStatusEnabled)
	addAliasChannel(212, "aaa", "doubao", "private-video", "doubao-seedance-2-0-260128", 0, common.ChannelStatusEnabled)
	legacyMapping := `{"legacy-video":"doubao-seedance-2-0-260128"}`
	legacyAlias := model.Channel{Id: 213, Type: constant.ChannelTypeDoubaoVideo, Name: "alias-legacy", Key: "fixture", Status: common.ChannelStatusEnabled, Group: "vip", Models: "legacy-video", ModelMapping: &legacyMapping, BaseURL: &upstream.URL}
	require.NoError(t, legacyAlias.Insert())
	extendedMapping := `{"extended-video":"doubao-seedance-2-0-260128"}`
	extendedAlias := model.Channel{Id: 214, Type: constant.ChannelTypeNewAPI, Name: "alias-extended", Key: "fixture", Status: common.ChannelStatusEnabled, Group: "vip", Models: "extended-video", ModelMapping: &extendedMapping, BaseURL: &upstream.URL}
	extendedAlias.SetSetting(kitdto.ChannelSettings{TaskExtendPluginKeys: []string{"doubao"}})
	require.NoError(t, extendedAlias.Insert())
	for _, item := range []struct {
		id                  int
		group, plugin, mode string
		priority            int64
	}{
		{301, "routing", "doubao", "deny", 60},
		{302, "routing", "anyllm", "allow", 0},
		{303, "aaa", "doubao", "allow", 100},
	} {
		mapping := `{"routing-alias":"doubao-seedance-2-0-260128"}`
		channel := model.Channel{Id: item.id, Type: constant.ChannelTypeTaskPlugin, Name: fmt.Sprint("routing", item.id), Key: "fixture", Status: common.ChannelStatusEnabled, Group: item.group, Models: "doubao-seedance-2-0-260128,routing-alias", ModelMapping: &mapping, Priority: &item.priority, BaseURL: &upstream.URL}
		require.NoError(t, channel.SetSetting(kitdto.ChannelSettings{TaskPluginKey: item.plugin, VideoRoutingRules: []kitdto.VideoRoutingRule{{Model: "doubao-seedance-2-0-260128", Mode: item.mode, Resolutions: []string{"4k"}}}}))
		require.NoError(t, channel.Insert())
	}
	for _, item := range []struct {
		id, platform, modelName string
		channel                 int
	}{
		{"task_doubao_origin", "doubao", "doubao-seedance-2-5-260628", 101},
		{"task_hailuo_origin", "hailuo", "MiniMax-H3", 103},
		{"task_doubao_missing_model", "doubao", "doubao-seedance-2-5-260628", 105},
		{"task_hailuo_missing_model", "hailuo", "MiniMax-H3", 106},
		{"task_doubao_data_model", "doubao", "doubao-seedance-2-5-260628", 101},
		{"task_h3_alias", "hailuo", "short-h3", 208},
	} {
		data, marshalErr := common.Marshal(map[string]any{"id": "local-upstream", "model": item.modelName, "resolution": "768P"})
		require.NoError(t, marshalErr)
		task := model.Task{TaskID: item.id, Platform: constant.TaskPlatform(item.platform), UserId: 1, Group: "vip", ChannelId: item.channel, Status: model.TaskStatusSuccess, Properties: model.Properties{OriginModelName: item.modelName}, PrivateData: model.TaskPrivateData{UpstreamTaskID: "local-upstream"}, Data: data}
		if item.id == "task_doubao_data_model" {
			task.Properties = model.Properties{}
		}
		if item.id == "task_h3_alias" {
			task.Properties.UpstreamModelName = "MiniMax-H3"
		}
		require.NoError(t, db.Create(&task).Error)
	}

	for _, memory := range []bool{false, true} {
		t.Run(fmt.Sprintf("memory_cache_%t", memory), func(t *testing.T) {
			common.MemoryCacheEnabled = memory
			model.InitChannelCache()
			t.Run("video_resolution_routing", func(t *testing.T) {
				for _, tc := range []struct {
					name, path, key, body string
					channel, status       int
					multipart             bool
				}{
					{"native_4k", "/doubao/api/v3/contents/generations/tasks", "routing", `{"model":"doubao-seedance-2-0-260128","content":[{"type":"text","text":"clouds"}],"resolution":"4k","duration":4}`, 302, 204, false},
					{"native_alias_4k", "/doubao/api/v3/contents/generations/tasks", "routing", `{"model":"routing-alias","content":[{"type":"text","text":"clouds"}],"resolution":"4k","duration":4}`, 302, 204, false},
					{"native_normal", "/doubao/api/v3/contents/generations/tasks", "routing", `{"model":"doubao-seedance-2-0-260128","content":[{"type":"text","text":"clouds"}],"resolution":"720p","duration":4}`, 301, 204, false},
					{"video_size", "/v1/videos", "routing", `{"model":"routing-alias","prompt":"clouds","size":"3840x2160","seconds":4}`, 302, 204, false},
					{"video_metadata", "/v1/videos", "routing", `{"model":"routing-alias","prompt":"clouds","metadata":{"resolution":"4k"},"seconds":4}`, 302, 204, false},
					{"video_multipart", "/v1/videos", "routing", `{"model":"routing-alias","prompt":"clouds","metadata":"{\"resolution\":\"4k\"}","seconds":"4"}`, 302, 204, true},
					{"video_unspecified", "/v1/videos", "routing", `{"model":"routing-alias","prompt":"clouds","seconds":4}`, 301, 204, false},
					{"responses_4k", "/v1/responses", "routing", `{"model":"routing-alias","input":"clouds","resolution":"4K","duration":4}`, 302, 204, false},
					{"responses_override", "/v1/responses", "routing", `{"model":"routing-alias","input":"clouds","resolution":"720p","metadata":{"resolution":"4k"},"duration":4}`, 301, 204, false},
					{"responses_size_null_resolution", "/v1/responses", "routing", `{"model":"routing-alias","input":"clouds","size":"3840x2160","metadata":{"resolution":null},"duration":4}`, 0, 400, false},
					{"responses_size_empty_resolution", "/v1/responses", "routing", `{"model":"routing-alias","input":"clouds","size":"3840x2160","metadata":{"resolution":""},"duration":4}`, 0, 400, false},
					{"auto_group", "/v1/videos", "autorouting", `{"model":"routing-alias","prompt":"clouds","resolution":"4k","seconds":4}`, 302, 204, false},
					{"model_limit", "/v1/videos", "routinglimited", `{"model":"routing-alias","prompt":"clouds","resolution":"4k","seconds":4}`, 0, 403, false},
				} {
					t.Run(tc.name, func(t *testing.T) {
						router := gin.New()
						router.Use(func(c *gin.Context) { c.Next(); common.CleanupBodyStorage(c) })
						handlers := []gin.HandlerFunc{TokenAuth()}
						if strings.HasPrefix(tc.path, "/doubao/") {
							bindings := generation.LookupDeclaredRouteCandidates(http.MethodPost, tc.path)
							require.NotEmpty(t, bindings)
							handlers = append([]gin.HandlerFunc{func(c *gin.Context) {
								c.Set(jsplugin.ContextKeyPinnedRoute, jsplugin.PinnedRoute{Generation: generation, Plugin: bindings[0].Plugin, Route: bindings[0].Route, Candidates: bindings})
								c.Next()
							}}, handlers...)
							handlers = append(handlers, PrepareTaskPluginRoute())
						} else {
							handlers = append(handlers, PinTaskPluginEndpoint(), PrepareTaskPluginEndpoint())
						}
						handlers = append(handlers, Distribute(), func(c *gin.Context) {
							assert.Equal(t, tc.channel, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
							modelName := "routing-alias"
							if tc.name == "native_4k" || tc.name == "native_normal" {
								modelName = "doubao-seedance-2-0-260128"
							}
							retry := service.RetryParam{Ctx: c, TokenGroup: common.GetContextKeyString(c, constant.ContextKeyUsingGroup), ModelName: modelName, Retry: common.GetPointer(1)}
							selected, _, selectErr := service.SelectChannelForRequest(c, modelName, &retry)
							require.Nil(t, selectErr)
							require.NotNil(t, selected)
							assert.Equal(t, tc.channel, selected.Id, "retry must preserve resolution and group constraints")
							c.Status(http.StatusNoContent)
						})
						router.POST(tc.path, handlers...)
						body := bytes.NewBufferString(tc.body)
						contentType := "application/json"
						if tc.multipart {
							var fields map[string]string
							require.NoError(t, common.UnmarshalJsonStr(tc.body, &fields))
							body = new(bytes.Buffer)
							writer := multipart.NewWriter(body)
							for key, value := range fields {
								require.NoError(t, writer.WriteField(key, value))
							}
							require.NoError(t, writer.Close())
							contentType = writer.FormDataContentType()
						}
						request := httptest.NewRequest(http.MethodPost, tc.path, body)
						request.Header.Set("Content-Type", contentType)
						request.Header.Set("Authorization", "Bearer sk-originfixture"+tc.key)
						recorder := httptest.NewRecorder()
						router.ServeHTTP(recorder, request)
						assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
					})
				}
			})
			for _, route := range []struct {
				name, path, body, origin, unavailable string
				channel                               int
				native                                bool
			}{
				{"doubao_native", "/doubao/api/v3/contents/generations/tasks", `{"model":"doubao-seedance-2-5-260628","content":[{"type":"draft_task","draft_task":{"id":"task_doubao_origin"}}],"duration":4,"resolution":"480p"}`, "task_doubao_origin", "task_doubao_missing_model", 101, true},
				{"doubao_responses", "/v1/responses", `{"model":"doubao-seedance-2-5-260628","metadata":{"content":[{"type":"draft_task","draft_task":{"id":"task_doubao_origin"}}]},"duration":4,"resolution":"480p"}`, "task_doubao_origin", "task_doubao_missing_model", 101, false},
				{"hailuo_native", "/hailuo/v2/video_regeneration", `{"model":"MiniMax-H3","source_task_id":"task_hailuo_origin","resolution":"2K"}`, "task_hailuo_origin", "task_hailuo_missing_model", 103, true},
				{"hailuo_responses", "/v1/responses", `{"model":"MiniMax-H3","source_task_id":"task_hailuo_origin","resolution":"2K"}`, "task_hailuo_origin", "task_hailuo_missing_model", 103, false},
			} {
				t.Run(route.name, func(t *testing.T) {
					router := gin.New()
					router.Use(func(c *gin.Context) { c.Next(); common.CleanupBodyStorage(c) })
					var handlers []gin.HandlerFunc
					if route.native {
						bindings := generation.LookupDeclaredRouteCandidates(http.MethodPost, route.path)
						require.NotEmpty(t, bindings)
						handlers = append(handlers, func(c *gin.Context) {
							c.Set(jsplugin.ContextKeyPinnedRoute, jsplugin.PinnedRoute{Generation: generation, Plugin: bindings[0].Plugin, Route: bindings[0].Route, Candidates: bindings})
							c.Next()
						}, TokenAuth(), PrepareTaskPluginRoute())
					} else {
						handlers = append(handlers, TokenAuth(), PinTaskPluginEndpoint(), PrepareTaskPluginEndpoint())
					}
					handlers = append(handlers, Distribute(), func(c *gin.Context) {
						response, requestErr := upstream.Client().Get(common.GetContextKeyString(c, constant.ContextKeyChannelBaseUrl) + "/submit")
						require.NoError(t, requestErr)
						response.Body.Close()
						info := &relaycommon.RelayInfo{UsingGroup: common.GetContextKeyString(c, constant.ContextKeyUsingGroup), UserGroup: common.GetContextKeyString(c, constant.ContextKeyUserGroup)}
						ratio := relayhelper.HandleGroupRatio(c, info)
						c.Header("X-Selected-Channel", fmt.Sprint(common.GetContextKeyInt(c, constant.ContextKeyChannelId)))
						c.Header("X-Selected-Auto-Group", common.GetContextKeyString(c, constant.ContextKeyAutoGroup))
						c.Header("X-Test-Group-Ratio", fmt.Sprint(ratio.GroupRatio))
						c.Status(http.StatusNoContent)
					})
					router.POST(route.path, handlers...)
					for _, tc := range []struct {
						name, key string
						status    int
						reach     bool
					}{
						{"same_group", "vip", http.StatusNoContent, true},
						{"other_group_zhCN", "aaa", http.StatusForbidden, false},
						{"other_group_zhTW", "aaa", http.StatusForbidden, false},
						{"other_group_en", "aaa", http.StatusForbidden, false},
						{"auto_group_allowed", "autovip", http.StatusNoContent, true},
						{"auto_group_excluded", "autoaaa", http.StatusForbidden, false},
						{"token_model_restricted", "limited", http.StatusForbidden, false},
						{"token_model_allowed", "allowmodels", http.StatusNoContent, true},
						{"inherited_group", "inherit", http.StatusNoContent, true},
						{"source_model_unavailable", "vip", http.StatusForbidden, false},
						{"other_user", "foreign", http.StatusBadRequest, false},
					} {
						t.Run(tc.name, func(t *testing.T) {
							before := upstreamRequests.Load()
							body := route.body
							if tc.name == "source_model_unavailable" {
								body = strings.ReplaceAll(body, route.origin, route.unavailable)
							}
							request := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(body))
							request.Header.Set("Content-Type", "application/json")
							request.Header.Set("Authorization", "Bearer sk-originfixture"+tc.key)
							switch tc.name {
							case "other_group_zhCN":
								request.Header.Set("Accept-Language", "zh-CN")
							case "other_group_zhTW":
								request.Header.Set("Accept-Language", "zh-TW")
							case "other_group_en":
								request.Header.Set("Accept-Language", "en")
							}
							recorder := httptest.NewRecorder()
							router.ServeHTTP(recorder, request)
							assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
							switch tc.name {
							case "other_group_zhCN":
								assert.Contains(t, recorder.Body.String(), "源任务所属渠道")
							case "other_group_zhTW":
								assert.Contains(t, recorder.Body.String(), "來源任務所屬管道")
							case "other_group_en":
								assert.Contains(t, recorder.Body.String(), "source task")
							}
							if tc.reach {
								assert.Equal(t, before+1, upstreamRequests.Load())
								assert.Equal(t, fmt.Sprint(route.channel), recorder.Header().Get("X-Selected-Channel"))
								assert.Equal(t, "0.7", recorder.Header().Get("X-Test-Group-Ratio"))
								if tc.key == "autovip" {
									assert.Equal(t, "vip", recorder.Header().Get("X-Selected-Auto-Group"))
								}
							} else {
								assert.Equal(t, before, upstreamRequests.Load())
							}
						})
					}
				})
			}
			t.Run("remix_model_limits", func(t *testing.T) {
				router := gin.New()
				router.POST("/v1/videos/:video_id/remix", TokenAuth(), Distribute(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
				for _, tc := range []struct {
					key, task string
					status    int
				}{
					{"allowmodels", "task_doubao_origin", http.StatusNoContent},
					{"allowmodels", "task_doubao_data_model", http.StatusNoContent},
					{"limited", "task_doubao_origin", http.StatusForbidden},
				} {
					request := httptest.NewRequest(http.MethodPost, "/v1/videos/"+tc.task+"/remix", strings.NewReader(`{}`))
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Authorization", "Bearer sk-originfixture"+tc.key)
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)
					assert.Equal(t, tc.status, recorder.Code, recorder.Body.String())
				}
			})
			t.Run("mapped_aliases", func(t *testing.T) {
				previousSecret, previousAddress := common.CryptoSecret, system_setting.TaskPublicAddress
				secret, secretErr := common.GenerateRandomCharsKey(48)
				require.NoError(t, secretErr)
				common.CryptoSecret, system_setting.TaskPublicAddress = secret, "https://gateway.example"
				t.Cleanup(func() { common.CryptoSecret, system_setting.TaskPublicAddress = previousSecret, previousAddress })
				for _, tc := range []struct {
					name, key, route, path, body, alias, upstream, plugin string
					status, channel                                       int
					protocol                                              bool
				}{
					{"shared_priority", "vip", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-0-260128", "anyllm", http.StatusNoContent, 202, false},
					{"legacy_channel_type", "vip", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"legacy-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "legacy-video", "doubao-seedance-2-0-260128", "doubao", http.StatusNoContent, 213, false},
					{"new_api_extension", "vip", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"extended-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "extended-video", "doubao-seedance-2-0-260128", "doubao", http.StatusNoContent, 214, false},
					{"shared_fallback", "vip", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p","callback_url":"https://example.com/callback"}`, "shared-video", "doubao-seedance-2-5-260628", "doubao", http.StatusNoContent, 204, false},
					{"retry_native", "vip", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-5-260628", "doubao", http.StatusNoContent, 204, false},
					{"different_group", "aaa", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-5-260628", "doubao", http.StatusNoContent, 203, false},
					{"auto_group", "autoaaa", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-5-260628", "doubao", http.StatusNoContent, 203, false},
					{"model_allowed", "aliasallow", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-0-260128", "anyllm", http.StatusNoContent, 202, false},
					{"model_forbidden", "limited", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "", "", "", http.StatusForbidden, 0, false},
					{"outside_group", "vip", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"private-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "", "", "", http.StatusBadRequest, 0, false},
					{"duplicate_model", "vip", "/doubao/api/v3/contents/generations/tasks", "", `{"model":"shared-video","model":"shared-video","content":[{"type":"text","text":"A river"}],"duration":4,"resolution":"480p"}`, "", "", "", http.StatusBadRequest, 0, false},
					{"openai_video", "vip", "/v1/videos", "", `{"model":"shared-video","prompt":"A river","seconds":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-0-260128", "anyllm", http.StatusNoContent, 202, true},
					{"openai_video_multipart", "vip", "/v1/videos", "", ``, "shared-video", "doubao-seedance-2-0-260128", "anyllm", http.StatusNoContent, 202, true},
					{"retry_openai", "vip", "/v1/videos", "", `{"model":"shared-video","prompt":"A river","seconds":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-5-260628", "doubao", http.StatusNoContent, 204, true},
					{"responses_candidate_filter", "vip", "/v1/responses", "", `{"model":"shared-video","input":"A river","duration":4,"resolution":"480p","service_tier":"default"}`, "shared-video", "doubao-seedance-2-5-260628", "doubao", http.StatusNoContent, 204, true},
					{"responses_other_group", "aaa", "/v1/responses", "", `{"model":"shared-video","input":"A river","duration":4,"resolution":"480p"}`, "shared-video", "doubao-seedance-2-5-260628", "doubao", http.StatusNoContent, 203, true},
					{"hailuo_native", "vip", "/hailuo/v2/video_generation", "", `{"model":"short-h3","content":[{"type":"text","text":"A river"}],"duration":6,"resolution":"768P"}`, "short-h3", "MiniMax-H3", "hailuo", http.StatusNoContent, 208, false},
					{"kling_model_name", "vip", "/kling/v1/videos/text2video", "", `{"model_name":"short-kling","prompt":"A river","duration":"5","mode":"std"}`, "short-kling", "kling-v3", "kling", http.StatusNoContent, 209, false},
					{"duplicate_model_name", "vip", "/kling/v1/videos/text2video", "", `{"model_name":"short-kling","model_name":"short-kling","prompt":"A river","duration":"5","mode":"std"}`, "", "", "", http.StatusBadRequest, 0, false},
					{"kling_model_path", "vip", "/kling/text-to-video/:model", "/kling/text-to-video/short-kling-new", `{"prompt":"A river","duration":5}`, "short-kling-new", "kling-3.0", "kling", http.StatusNoContent, 210, false},
					{"vidu_native", "vip", "/vidu/ent/v2/text2video", "", `{"model":"short-vidu","prompt":"A river","duration":4,"resolution":"720p"}`, "short-vidu", "viduq3-turbo", "vidu", http.StatusNoContent, 211, false},
				} {
					t.Run(tc.name, func(t *testing.T) {
						router := gin.New()
						router.Use(func(c *gin.Context) { c.Next(); common.CleanupBodyStorage(c) })
						var handlers []gin.HandlerFunc
						if tc.protocol {
							handlers = append(handlers, TokenAuth(), PinTaskPluginEndpoint(), PrepareTaskPluginEndpoint())
						} else {
							bindings := generation.LookupDeclaredRouteCandidates(http.MethodPost, tc.route)
							require.NotEmpty(t, bindings)
							handlers = append(handlers, func(c *gin.Context) {
								c.Set(jsplugin.ContextKeyPinnedRoute, jsplugin.PinnedRoute{Generation: generation, Plugin: bindings[0].Plugin, Route: bindings[0].Route, Candidates: bindings})
								c.Next()
							}, TokenAuth(), PrepareTaskPluginRoute())
						}
						handlers = append(handlers, Distribute(), func(c *gin.Context) {
							if strings.HasPrefix(tc.name, "retry_") {
								retry := 1
								channel, _, selectErr := service.SelectChannelForRequest(c, tc.alias, &service.RetryParam{Ctx: c, TokenGroup: "vip", ModelName: tc.alias, Retry: &retry})
								require.Nil(t, selectErr)
								require.NotNil(t, channel)
								require.Nil(t, SetupContextForSelectedChannel(c, channel, tc.alias))
							}
							pinned := c.MustGet(jsplugin.ContextKeyPinnedPlugin).(jsplugin.PinnedPlugin)
							info := &relaycommon.RelayInfo{UserId: common.GetContextKeyInt(c, constant.ContextKeyUserId), OriginModelName: c.GetString("resolved_task_model"), TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: c.GetString("task_action"), PublicTaskID: "task_fixture"}}
							info.InitChannelMeta(c)
							info.UpstreamModelName = info.OriginModelName
							require.NoError(t, relayhelper.ModelMappedHelper(c, info, nil))
							adaptor := taskadaptor.New(pinned.Plugin)
							adaptor.Init(info)
							if taskErr := adaptor.ValidateRequestAndSetAction(c, info); taskErr != nil {
								c.JSON(taskErr.StatusCode, taskErr)
								return
							}
							body, bodyErr := adaptor.BuildRequestBody(c, info)
							require.NoError(t, bodyErr)
							raw, readErr := io.ReadAll(body)
							require.NoError(t, readErr)
							var sent map[string]any
							require.NoError(t, common.Unmarshal(raw, &sent))
							if sentModel, ok := sent["model"].(string); ok {
								c.Header("X-Test-Body-Model", sentModel)
							} else if sentModel, ok := sent["model_name"].(string); ok {
								c.Header("X-Test-Body-Model", sentModel)
							}
							url, urlErr := adaptor.BuildRequestURL(info)
							require.NoError(t, urlErr)
							c.Header("X-Test-URL", url)
							c.Header("X-Test-Channel", fmt.Sprint(info.ChannelId))
							c.Header("X-Test-Plugin", pinned.Plugin.Meta.Key)
							c.Header("X-Test-Origin", info.OriginModelName)
							c.Header("X-Test-Upstream", info.UpstreamModelName)
							response, requestErr := upstream.Client().Get(upstream.URL + "/submit")
							require.NoError(t, requestErr)
							response.Body.Close()
							c.Status(http.StatusNoContent)
						})
						router.POST(tc.route, handlers...)
						path := tc.path
						if path == "" {
							path = tc.route
						}
						before := upstreamRequests.Load()
						var request *http.Request
						if tc.name == "openai_video_multipart" {
							var body bytes.Buffer
							writer := multipart.NewWriter(&body)
							for key, value := range map[string]string{"model": "shared-video", "prompt": "A river", "seconds": "4", "resolution": "480p"} {
								require.NoError(t, writer.WriteField(key, value))
							}
							require.NoError(t, writer.Close())
							request = httptest.NewRequest(http.MethodPost, path, &body)
							request.Header.Set("Content-Type", writer.FormDataContentType())
						} else {
							request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body))
							request.Header.Set("Content-Type", "application/json")
						}
						request.Header.Set("Authorization", "Bearer sk-originfixture"+tc.key)
						recorder := httptest.NewRecorder()
						router.ServeHTTP(recorder, request)
						require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
						if tc.status == http.StatusNoContent {
							assert.Equal(t, before+1, upstreamRequests.Load())
							assert.Equal(t, fmt.Sprint(tc.channel), recorder.Header().Get("X-Test-Channel"))
							assert.Equal(t, tc.plugin, recorder.Header().Get("X-Test-Plugin"))
							assert.Equal(t, tc.alias, recorder.Header().Get("X-Test-Origin"))
							assert.Equal(t, tc.upstream, recorder.Header().Get("X-Test-Upstream"))
							if tc.name == "kling_model_path" {
								assert.Contains(t, recorder.Header().Get("X-Test-URL"), tc.upstream)
							} else {
								assert.Equal(t, tc.upstream, recorder.Header().Get("X-Test-Body-Model"))
							}
						} else {
							assert.Equal(t, before, upstreamRequests.Load())
						}
					})
				}
			})
			t.Run("alias_task_query_and_list", func(t *testing.T) {
				for _, route := range []struct {
					template, path string
					list           bool
				}{
					{"/hailuo/v2/query/video_generation/:task_id", "/hailuo/v2/query/video_generation/task_h3_alias", false},
					{"/hailuo/v2/query/video_generation", "/hailuo/v2/query/video_generation?filter.model=short-h3", true},
				} {
					bindings := generation.LookupDeclaredRouteCandidates(http.MethodGet, route.template)
					require.NotEmpty(t, bindings)
					router := gin.New()
					router.GET(route.template, func(c *gin.Context) {
						c.Set(jsplugin.ContextKeyPinnedRoute, jsplugin.PinnedRoute{Generation: generation, Plugin: bindings[0].Plugin, Route: bindings[0].Route, Candidates: bindings})
						c.Next()
					}, TokenAuth(), PrepareTaskPluginRoute(), func(c *gin.Context) {
						intent := c.MustGet(jsplugin.ContextKeyNativeRouteIntent).(map[string]any)
						assert.Equal(t, "short-h3", intent["model"])
						count, err := model.CountTaskPluginUserTasks(1, model.TaskPluginTaskFilter{Platforms: []constant.TaskPlatform{"hailuo"}, Model: "short-h3"})
						require.NoError(t, err)
						assert.EqualValues(t, 1, count)
						c.Status(http.StatusNoContent)
					})
					request := httptest.NewRequest(http.MethodGet, route.path, nil)
					request.Header.Set("Authorization", "Bearer sk-originfixturevip")
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)
					if route.list {
						assert.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
					} else {
						assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
						assert.Contains(t, recorder.Body.String(), "task_h3_alias")
					}
				}
			})
		})
	}
}
