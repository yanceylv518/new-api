package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	appI18n "github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/expr-lang/expr/parser"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const seedanceCompatMySQLDSN = "SEEDANCE_COMPAT_TEST_MYSQL_DSN"

func seedanceCompatTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(seedanceCompatMySQLDSN)
	if dsn == "" {
		t.Skip(seedanceCompatMySQLDSN + " is not configured")
	}
	config, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	require.Equal(t, "tcp", config.Net)
	host, _, err := net.SplitHostPort(config.Addr)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	require.True(t, ip.IsLoopback(), "test database must be on loopback")
	require.True(t, strings.HasPrefix(config.DBName, "codex_seedance_id_"), "test database must be isolated")
	return dsn
}

func isolatedSeedanceCompatTestDSN(t *testing.T, dsn string) string {
	t.Helper()
	config, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	suffix, err := common.GenerateRandomCharsKey(12)
	require.NoError(t, err)
	name := "codex_seedance_id_" + strings.ToLower(suffix)
	admin, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	connection, err := admin.DB()
	require.NoError(t, err)
	if err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
		_ = connection.Close()
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		assert.NoError(t, admin.Exec("DROP DATABASE `"+name+"`").Error)
		assert.NoError(t, connection.Close())
	})
	config.DBName = name
	return config.FormatDSN()
}

func TestSeedanceCompatVideoMySQL(t *testing.T) {
	dsn := isolatedSeedanceCompatTestDSN(t, seedanceCompatTestDSN(t))
	t.Setenv(seedanceCompatMySQLDSN, dsn)
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	oldMaster, oldRedis, oldMemory := common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled
	oldBatch, oldLogConsume, oldRetry := common.BatchUpdateEnabled, common.LogConsumeEnabled, common.RetryTimes
	oldSecret := common.CryptoSecret
	oldOptions := common.OptionMap
	oldPrices, oldGroups := ratio_setting.ModelPrice2JSONString(), ratio_setting.GroupRatio2JSONString()
	secret, err := common.GenerateRandomCharsKey(48)
	require.NoError(t, err)
	common.CryptoSecret = secret
	common.OptionMap = make(map[string]string)
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = true, false, false
	common.BatchUpdateEnabled, common.LogConsumeEnabled, common.RetryTimes = false, false, 2
	t.Cleanup(func() {
		if model.DB != nil {
			if connection, err := model.DB.DB(); err == nil {
				_ = connection.Close()
			}
		}
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMain, oldLog)
		common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = oldMaster, oldRedis, oldMemory
		common.BatchUpdateEnabled, common.LogConsumeEnabled, common.RetryTimes = oldBatch, oldLogConsume, oldRetry
		common.CryptoSecret = oldSecret
		common.OptionMap = oldOptions
		assert.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldPrices))
		assert.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroups))
	})
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	db := model.DB
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(32)
	require.NoError(t, appI18n.Init())
	service.InitHttpClient()
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"doubao-seedance-2-0-260128":0.0002}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"compat-any":1}`))

	const modelName = "doubao-seedance-2-0-260128"
	upstreamIDs := []string{"task_vendor_shared", "cgt-20240101000000-ab123", "task_vendor_native"}
	var mu sync.Mutex
	var deleteRequests atomic.Int32
	counts := map[string]int{}
	queries := map[string]int{}
	upstreamKeys := map[string]string{}
	requests := map[string]map[string]any{}
	states := map[string]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			deleteRequests.Add(1)
		}
		kind := "doubao"
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			kind = "anyllm"
		} else if strings.HasPrefix(r.URL.Path, "/fixture-kling/") {
			kind = "kling"
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+upstreamKeys[kind] {
			http.Error(w, "invalid upstream authorization", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/fixture-kling/v1/videos/image2video" {
			var body map[string]any
			if err := common.DecodeJson(r.Body, &body); err != nil {
				http.Error(w, "invalid submission", http.StatusBadRequest)
				return
			}
			counts[kind]++
			requests["kling:fixture"] = body
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"task_id":"kling-file-fixture-%d"}}`, counts[kind])
			return
		}
		if r.Method == http.MethodPost && (r.URL.Path == "/api/v3/contents/generations/tasks" || r.URL.Path == "/api/v1/videos/generations") {
			index := counts[kind]
			counts[kind]++
			var body map[string]any
			if err := common.DecodeJson(r.Body, &body); err != nil {
				http.Error(w, "invalid submission", http.StatusBadRequest)
				return
			}
			id := fmt.Sprintf("fixture-%s-%d", kind, index)
			if index < len(upstreamIDs) {
				id = upstreamIDs[index]
			}
			requests[kind+":"+id] = body
			_, _ = fmt.Fprintf(w, `{"id":%q,"status":"queued"}`, id)
			return
		}
		if r.Method == http.MethodDelete && kind == "doubao" && strings.HasPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/") {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		if r.Method == http.MethodGet {
			if kind == "doubao" && strings.HasPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/") {
				id := strings.TrimPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/")
				queries[kind+":"+id]++
				switch states[id] {
				case "running":
					_, _ = fmt.Fprintf(w, `{"id":%q,"status":"running"}`, id)
				case "failed":
					_, _ = fmt.Fprintf(w, `{"id":%q,"status":"failed","error":{"code":"fixture_failure","message":"generation failed"}}`, id)
				default:
					_, _ = fmt.Fprintf(w, `{"id":%q,"model":"doubao-seedance-2-0-260128","status":"succeeded","content":{"video_url":"https://media.example/video.mp4","last_frame_url":"https://media.example/frame.jpg"},"usage":{"completion_tokens":120,"total_tokens":120},"service_tier":"default","draft":false,"execution_expires_after":0,"seed":0,"extension":{"enabled":false,"count":0,"optional":null}}`, id)
				}
				return
			}
			if kind == "anyllm" && strings.HasPrefix(r.URL.Path, "/api/v1/videos/") && strings.HasSuffix(r.URL.Path, "/result") {
				id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/videos/"), "/result")
				queries[kind+":"+id]++
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"succeeded","download_url":"https://media.example/video.mp4","duration":5}`, id)
				return
			}
			if kind == "anyllm" && r.URL.Path == "/api/v1/mediaTaskInfo" {
				id := r.URL.Query().Get("taskId")
				queries[kind+":"+id]++
				_, _ = io.WriteString(w, `{"code":0,"data":{"status":"success","usage":{"totalTokens":120}}}`)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)

	type account struct {
		user       model.User
		token      model.Token
		channel    model.Channel
		accessKey  string
		signingKey string
	}
	accounts := map[string]*account{}
	for _, entry := range []struct{ key, group string }{{"doubao", "default"}, {"anyllm", "compat-any"}, {"other", "default"}} {
		key, keyErr := common.GenerateRandomCharsKey(32)
		require.NoError(t, keyErr)
		user := model.User{Username: "compat-" + entry.key, Password: "test-only", AffCode: "compat-" + entry.key, Group: entry.group, Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Quota: 1000000}
		require.NoError(t, db.Create(&user).Error)
		token := model.Token{UserId: user.Id, Key: key, Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1}
		require.NoError(t, db.Create(&token).Error)
		account := &account{user: user, token: token}
		if entry.key != "other" {
			credential, signingKey, credentialErr := model.IssueTokenSignatureCredential(t.Context(), user.Id, token.Id)
			require.NoError(t, credentialErr)
			account.accessKey, account.signingKey = credential.AccessKey, signingKey
			upstreamKey, keyErr := common.GenerateRandomCharsKey(32)
			require.NoError(t, keyErr)
			upstreamKeys[entry.key] = upstreamKey
			base := upstream.URL
			if entry.key == "anyllm" {
				base += "/api"
			}
			setting := fmt.Sprintf(`{"task_plugin_key":%q}`, entry.key)
			account.channel = model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "compat-" + entry.key, Key: upstreamKey, BaseURL: &base, Setting: &setting, Status: common.ChannelStatusEnabled, Group: entry.group, Models: modelName}
			require.NoError(t, db.Create(&account.channel).Error)
			require.NoError(t, account.channel.AddAbilities(nil))
		}
		accounts[entry.key] = account
	}

	loaded := make([]*jsplugin.LoadedPlugin, 0, 2)
	for _, key := range []string{"doubao", "anyllm"} {
		source, sourceErr := plugins.Source(key)
		require.NoError(t, sourceErr)
		plugin, compileErr := jsplugin.CompilePlugin(source, jsplugin.Options{Key: key})
		require.NoError(t, compileErr)
		loaded = append(loaded, plugin)
	}
	var traceMu sync.Mutex
	var lastTrace []service.PolicyEvent
	handlers := func(generation *jsplugin.RoutingGeneration, binding jsplugin.RouteBinding) []gin.HandlerFunc {
		observe := func(c *gin.Context) {
			c.Next()
			traceMu.Lock()
			lastTrace = service.RequestPolicy(c).Events()
			traceMu.Unlock()
		}
		return append([]gin.HandlerFunc{observe}, productionPluginRouteHandlers(generation, binding)...)
	}
	outer, registry := newPluginRouterTest(t, loaded, handlers)
	outer.Use(middleware.RequestId(), middleware.SeedanceSignatureAuth(), middleware.BodyStorageCleanup())
	outer.NoRoute((&pluginRouteDispatcher{registry: registry}).dispatch)
	request := func(method, path, body string, account *account, signed bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Billing-Tier", "fixture")
		if signed {
			signSeedanceClientRequest(t, req, []byte(body), account.accessKey, account.signingKey, time.Now(), true)
		} else {
			req.Header.Set("Authorization", "Bearer sk-"+account.token.Key)
		}
		if body != "" && !signed {
			req.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		outer.ServeHTTP(response, req)
		return response
	}
	const body = `{"model":"doubao-seedance-2-0-260128","content":[{"type":"text","text":"a fixed camera"}],"duration":5,"resolution":"480p","ratio":"16:9","generate_audio":false}`
	paths := []string{"/doubao/contents/generations/tasks", "/doubao/v1/contents/generations/tasks", "/doubao/api/v3/contents/generations/tasks"}
	format := regexp.MustCompile(`^cgt-[0-9]{14}-[a-z0-9]{5}$`)
	seen := make(map[string]struct{})
	doubaoTaskIDs := make([]string, 0, len(paths))
	compareQueries := func(t *testing.T, account *account, taskID, status string) map[string]any {
		t.Helper()
		var baseline string
		var result map[string]any
		for _, path := range paths {
			for _, signed := range []bool{false, true} {
				response := request(http.MethodGet, path+"/"+taskID, "", account, signed)
				require.Equal(t, http.StatusOK, response.Code, "%s signed=%t: %s", path, signed, response.Body.String())
				assert.Regexp(t, `^[0-9]{14}`, response.Header().Get(common.RequestIdKey))
				if baseline == "" {
					baseline = response.Body.String()
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
				} else {
					assert.Equal(t, baseline, response.Body.String(), "%s signed=%t", path, signed)
				}
			}
		}
		assert.Equal(t, taskID, result["id"])
		assert.Equal(t, status, result["status"])
		return result
	}
	for _, pluginKey := range []string{"doubao", "anyllm"} {
		account := accounts[pluginKey]
		for index, path := range paths {
			var walletBefore model.User
			require.NoError(t, db.First(&walletBefore, account.user.Id).Error)
			signed := index == 1
			response := request(http.MethodPost, path, body, account, signed)
			require.Equal(t, http.StatusOK, response.Code, "%s: %s", path, response.Body.String())
			var created struct {
				ID string `json:"id"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
			assert.Regexp(t, format, created.ID)
			assert.JSONEq(t, fmt.Sprintf(`{"id":%q}`, created.ID), response.Body.String())
			assert.NotEqual(t, upstreamIDs[index], created.ID)
			_, duplicate := seen[created.ID]
			assert.False(t, duplicate)
			seen[created.ID] = struct{}{}
			stored, exists, lookupErr := model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, lookupErr)
			require.True(t, exists)
			require.Equal(t, upstreamIDs[index], stored.GetUpstreamTaskID())
			require.Equal(t, account.channel.Id, stored.ChannelId)
			if pluginKey == "doubao" && index == 0 {
				compareQueries(t, account, created.ID, "queued")
			}
			for _, queryPath := range []string{path, "/doubao/api/v3/contents/generations/tasks"} {
				query := request(http.MethodGet, queryPath+"/"+created.ID, "", account, signed)
				require.Equal(t, http.StatusOK, query.Code, query.Body.String())
				var result struct {
					ID string `json:"id"`
				}
				require.NoError(t, common.Unmarshal(query.Body.Bytes(), &result))
				assert.Equal(t, created.ID, result.ID)
				assert.NotContains(t, query.Body.String(), upstreamIDs[index])
			}
			foreign := request(http.MethodGet, path+"/"+created.ID, "", accounts["other"], false)
			assert.Equal(t, http.StatusNotFound, foreign.Code)
			foreignDelete := request(http.MethodDelete, path+"/"+created.ID, "", accounts["other"], false)
			assert.Equal(t, http.StatusNotFound, foreignDelete.Code)
			assert.Zero(t, deleteRequests.Load(), "越权删除不能到达上游")
			plugin, found := registry.Generation().Get(pluginKey)
			require.True(t, found)
			adaptor := taskplugin.New(plugin)
			if pluginKey == "doubao" && index == 0 {
				mu.Lock()
				states[upstreamIDs[index]] = "running"
				mu.Unlock()
				stored, lookupErr = service.RefreshTaskForManagement(t.Context(), adaptor, &account.channel, stored)
				require.NoError(t, lookupErr)
				require.Equal(t, model.TaskStatus(model.TaskStatusInProgress), stored.Status)
				compareQueries(t, account, created.ID, "running")
				mu.Lock()
				delete(states, upstreamIDs[index])
				mu.Unlock()
			}
			for attempts := 0; attempts < 3 && stored.Status != model.TaskStatusSuccess; attempts++ {
				stored, lookupErr = service.RefreshTaskForManagement(t.Context(), adaptor, &account.channel, stored)
				require.NoError(t, lookupErr)
			}
			require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
			query := request(http.MethodGet, path+"/"+created.ID, "", account, signed)
			require.Equal(t, http.StatusOK, query.Code, query.Body.String())
			assert.Contains(t, query.Body.String(), `"status":"succeeded"`)
			assert.Contains(t, query.Body.String(), "https://media.example/video.mp4")
			assert.NotContains(t, query.Body.String(), upstreamIDs[index])
			if pluginKey == "doubao" {
				result := compareQueries(t, account, created.ID, "succeeded")
				assert.Equal(t, "default", result["service_tier"])
				assert.Equal(t, false, result["draft"])
				assert.EqualValues(t, 0, result["execution_expires_after"])
				assert.EqualValues(t, 0, result["seed"])
				assert.Equal(t, map[string]any{"enabled": false, "count": float64(0), "optional": nil}, result["extension"])
				content, ok := result["content"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "https://media.example/video.mp4", content["video_url"])
				assert.Equal(t, "https://media.example/frame.jpg", content["last_frame_url"])
				usage, ok := result["usage"].(map[string]any)
				require.True(t, ok)
				assert.EqualValues(t, 120, usage["completion_tokens"])
				assert.EqualValues(t, 120, usage["total_tokens"])
				doubaoTaskIDs = append(doubaoTaskIDs, created.ID)
			}
			mu.Lock()
			queryCount := queries[pluginKey+":"+upstreamIDs[index]]
			mu.Unlock()
			assert.Positive(t, queryCount)
			var walletAfter model.User
			require.NoError(t, db.First(&walletAfter, account.user.Id).Error)
			assert.Positive(t, stored.Quota)
			assert.Equal(t, stored.Quota, walletBefore.Quota-walletAfter.Quota, "创建和重复查询只能扣除一次任务费用")
			if index == len(paths)-1 {
				historical := *stored
				historical.ID = 0
				historical.TaskID = "task_legacy_" + pluginKey
				require.NoError(t, db.Create(&historical).Error)
				for _, queryPath := range paths {
					query := request(http.MethodGet, queryPath+"/"+historical.TaskID, "", account, false)
					require.Equal(t, http.StatusOK, query.Code, query.Body.String())
					var result struct {
						ID string `json:"id"`
					}
					require.NoError(t, common.Unmarshal(query.Body.Bytes(), &result))
					assert.Equal(t, historical.TaskID, result.ID)
					assert.NotContains(t, query.Body.String(), upstreamIDs[index])
				}
			}
		}
	}

	t.Run("private asset selects ready account", func(t *testing.T) {
		account := accounts["doubao"]
		originalSetting := account.channel.Setting
		settings := account.channel.GetSetting()
		settings.SeedanceAssetURL = upstream.URL
		require.NoError(t, account.channel.SetSetting(settings))
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", account.channel.Id).Update("setting", *account.channel.Setting).Error)
		priority := int64(100)
		high := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "fixture-pending-channel", Key: "pending-fixture-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Group: account.user.Group, Models: modelName, Priority: &priority}
		require.NoError(t, high.SetSetting(settings))
		require.NoError(t, high.Insert())
		t.Cleanup(func() {
			assert.NoError(t, db.Where("channel_id = ?", high.Id).Delete(&model.Ability{}).Error)
			assert.NoError(t, db.Delete(&high).Error)
			account.channel.Setting = originalSetting
			assert.NoError(t, db.Model(&model.Channel{}).Where("id = ?", account.channel.Id).Update("setting", *originalSetting).Error)
		})
		key := fmt.Sprintf("%x", common.Sha256Raw([]byte(account.channel.Key)))
		group := model.SeedanceAssetGroup{UserID: account.user.Id, ChannelID: account.channel.Id, GroupID: "group-ready-router", Name: "fixture", GroupType: "AIGC", Status: "Active", KeyFingerprint: key, AccountFingerprint: model.SeedanceAssetAccountFingerprint(&account.channel, key)}
		require.NoError(t, db.Create(&group).Error)
		localOnly := false
		asset := model.SeedanceAsset{UserID: account.user.Id, ChannelID: account.channel.Id, GroupID: group.GroupID, AssetID: "asset-ready-router", PrimaryUpstreamID: "asset-ready-upstream", LocalOnly: &localOnly, Name: "fixture", AssetType: "Image", Status: "Active", KeyFingerprint: key, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(&account.channel, modelName, key)}
		require.NoError(t, db.Create(&asset).Error)
		require.NoError(t, db.Create(&model.SeedanceAssetGroupReplica{UserID: account.user.Id, LocalGroupID: group.ID, ChannelID: account.channel.Id, AccountFingerprint: asset.AccountFingerprint, KeyFingerprint: key, UpstreamGroupID: group.GroupID, GroupName: group.Name, Status: model.SeedanceAssetReplicaReady}).Error)
		highKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(high.Key)))
		require.NoError(t, db.Create(&model.SeedanceAssetReplica{UserID: account.user.Id, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: high.Id, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(&high, modelName, highKey), KeyFingerprint: highKey, ProvisionStatus: model.SeedanceAssetReplicaCreating, Status: "Pending", NextPollAt: common.GetTimestamp() + 60}).Error)
		body := `{"model":"doubao-seedance-2-0-260128","content":[{"type":"text","text":"a fixed camera"},{"type":"image_url","image_url":{"url":"asset://asset-ready-router"},"role":"reference_image"}],"duration":5,"resolution":"480p","generate_audio":false}`
		response := request(http.MethodPost, "/doubao/api/v3/contents/generations/tasks", body, account, false)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var created struct {
			ID string `json:"id"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
		stored, exists, lookupErr := model.GetByTaskId(account.user.Id, created.ID)
		require.NoError(t, lookupErr)
		require.True(t, exists)
		require.Equal(t, account.channel.Id, stored.ChannelId)
		mu.Lock()
		sent, marshalErr := common.Marshal(requests["doubao:"+stored.GetUpstreamTaskID()])
		mu.Unlock()
		require.NoError(t, marshalErr)
		assert.Contains(t, string(sent), "asset://asset-ready-upstream")
		assert.NotContains(t, string(sent), "asset://asset-ready-router")
	})

	t.Run("parameters and billing match across video paths", func(t *testing.T) {
		oldModes, err := common.Marshal(billing_setting.GetBillingModeCopy())
		require.NoError(t, err)
		oldExpressions, err := common.Marshal(billing_setting.GetBillingExprCopy())
		require.NoError(t, err)
		t.Cleanup(func() {
			assert.NoError(t, model.UpdateOptionsBulk(map[string]string{
				"billing_setting.billing_mode": string(oldModes),
				"billing_setting.billing_expr": string(oldExpressions),
			}))
		})
		modes, err := common.Marshal(map[string]string{modelName: "tiered_expr"})
		require.NoError(t, err)
		expressions, err := common.Marshal(map[string]string{modelName: `param("duration") == 4 && param("resolution") == "480p" && param("generate_audio") == false && param("seed") == 0 && param("missing") == nil && header("x-billing-tier") == "fixture" ? tier("matched", u("tokens") / 1000000) : tier("wrong", u("tokens") * 20 / 1000000)`})
		require.NoError(t, err)
		require.NoError(t, model.UpdateOptionsBulk(map[string]string{
			"billing_setting.billing_mode": string(modes),
			"billing_setting.billing_expr": string(expressions),
		}))
		const compatibleBody = `{"model":"doubao-seedance-2-0-260128","prompt":"a fixed camera","duration":8,"resolution":"720p","parameters":{"duration":4,"resolution":"480p","generate_audio":false,"seed":0}}`
		const expectedUpstream = `{"model":"doubao-seedance-2-0-260128","content":[{"type":"text","text":"a fixed camera"}],"duration":4,"resolution":"480p","generate_audio":false,"seed":0}`
		account := accounts["doubao"]
		plugin, found := registry.Generation().Get("doubao")
		require.True(t, found)
		var snapshotHash string
		for _, path := range paths {
			for _, signed := range []bool{false, true} {
				var before model.User
				require.NoError(t, db.First(&before, account.user.Id).Error)
				response := request(http.MethodPost, path, compatibleBody, account, signed)
				require.Equal(t, http.StatusOK, response.Code, "%s signed=%t: %s", path, signed, response.Body.String())
				assert.Regexp(t, `^[0-9]{14}`, response.Header().Get(common.RequestIdKey))
				var created struct {
					ID string `json:"id"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
				assert.Regexp(t, format, created.ID)
				assert.JSONEq(t, fmt.Sprintf(`{"id":%q}`, created.ID), response.Body.String())
				stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
				require.NoError(t, err)
				require.True(t, exists)
				require.NotNil(t, stored.PrivateData.BillingContext)
				require.NotNil(t, stored.PrivateData.BillingContext.TieredSnapshot)
				pricingSnapshot := stored.PrivateData.BillingContext.TieredSnapshot
				assert.Equal(t, "matched", pricingSnapshot.EstimatedTier)
				estimatedTokens, ok := pricingSnapshot.UsageFacts["tokens"].(float64)
				require.True(t, ok)
				assert.Equal(t, common.QuotaRound(estimatedTokens/1000000*common.QuotaPerUnit), stored.Quota)
				sealed, err := billingexpr.TaskRequestCiphertext(pricingSnapshot.ExprString, pricingSnapshot.EncryptedRequestContext)
				require.NoError(t, err)
				require.NotEmpty(t, sealed)
				assert.Empty(t, pricingSnapshot.EncryptedRequestContext)
				assert.NotEqual(t, billingexpr.ExprHashString(billingexpr.PublicExpression(pricingSnapshot.ExprString)), pricingSnapshot.ExprHash)
				if snapshotHash == "" {
					snapshotHash = pricingSnapshot.ExprHash
				} else {
					assert.Equal(t, snapshotHash, pricingSnapshot.ExprHash)
				}
				assert.True(t, billingexpr.HasTaskRequestExpression(pricingSnapshot.ExprString))
				_, err = parser.Parse(pricingSnapshot.ExprString)
				require.Error(t, err)
				assert.NotContains(t, sealed, "a fixed camera")
				captured, err := common.OpenTaskSecret(sealed)
				require.NoError(t, err)
				assert.NotContains(t, captured, "a fixed camera")
				assert.Contains(t, captured, `"duration":4`)
				mu.Lock()
				forwarded := requests["doubao:"+stored.GetUpstreamTaskID()]
				mu.Unlock()
				encoded, err := common.Marshal(forwarded)
				require.NoError(t, err)
				assert.JSONEq(t, expectedUpstream, string(encoded), "%s signed=%t", path, signed)
				var reserved model.User
				require.NoError(t, db.First(&reserved, account.user.Id).Error)
				assert.Positive(t, stored.Quota)
				assert.Equal(t, stored.Quota, before.Quota-reserved.Quota)
				stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
				require.NoError(t, err)
				require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
				assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit), stored.Quota)
				compareQueries(t, account, created.ID, "succeeded")
				var after model.User
				require.NoError(t, db.First(&after, account.user.Id).Error)
				assert.Equal(t, stored.Quota, before.Quota-after.Quota, "重复查询不能再次扣费")
			}
		}
		t.Run("multipart task pricing", func(t *testing.T) {
			previousRegistry := jsplugin.DefaultRegistry
			jsplugin.DefaultRegistry = registry
			t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
			protocolRouter := gin.New()
			protocolRouter.Use(middleware.RequestId(), middleware.BodyStorageCleanup())
			SetTaskPluginProtocolRouter(protocolRouter)
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for key, value := range map[string]string{
				"model": modelName, "prompt": "a fixed camera", "duration": "4", "resolution": "480p", "generate_audio": "false", "seed": "0",
			} {
				require.NoError(t, writer.WriteField(key, value))
			}
			require.NoError(t, writer.Close())
			req := httptest.NewRequest(http.MethodPost, "/v1/videos", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("Authorization", "Bearer sk-"+account.token.Key)
			req.Header.Set("X-Billing-Tier", "fixture")
			var before model.User
			require.NoError(t, db.First(&before, account.user.Id).Error)
			response := httptest.NewRecorder()
			protocolRouter.ServeHTTP(response, req)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var created struct {
				ID string `json:"id"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
			stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, err)
			require.True(t, exists)
			require.NotNil(t, stored.PrivateData.BillingContext)
			require.NotNil(t, stored.PrivateData.BillingContext.TieredSnapshot)
			assert.Equal(t, "matched", stored.PrivateData.BillingContext.TieredSnapshot.EstimatedTier)
			stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
			require.NoError(t, err)
			require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
			assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit), stored.Quota)
			var after model.User
			require.NoError(t, db.First(&after, account.user.Id).Error)
			assert.Equal(t, stored.Quota, before.Quota-after.Quota)
		})
		t.Run("json and multipart use the same requested fields", func(t *testing.T) {
			expr, err := common.Marshal(map[string]string{modelName: `param("seconds") == nil ? tier("matched", u("tokens") / 1000000) : tier("wrong", u("tokens") * 20 / 1000000)`})
			require.NoError(t, err)
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expr)}))
			t.Cleanup(func() {
				assert.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expressions)}))
			})
			previousRegistry := jsplugin.DefaultRegistry
			jsplugin.DefaultRegistry = registry
			t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
			protocolRouter := gin.New()
			protocolRouter.Use(middleware.RequestId(), middleware.BodyStorageCleanup())
			SetTaskPluginProtocolRouter(protocolRouter)
			for _, form := range []bool{false, true} {
				var before model.User
				require.NoError(t, db.First(&before, account.user.Id).Error)
				var input bytes.Buffer
				contentType := "application/json"
				if form {
					writer := multipart.NewWriter(&input)
					for key, value := range map[string]string{"model": modelName, "prompt": "a fixed camera", "duration": "4", "resolution": "480p"} {
						require.NoError(t, writer.WriteField(key, value))
					}
					require.NoError(t, writer.Close())
					contentType = writer.FormDataContentType()
				} else {
					input.WriteString(`{"model":"doubao-seedance-2-0-260128","prompt":"a fixed camera","duration":4,"resolution":"480p"}`)
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/videos", &input)
				req.Header.Set("Content-Type", contentType)
				req.Header.Set("Authorization", "Bearer sk-"+account.token.Key)
				response := httptest.NewRecorder()
				protocolRouter.ServeHTTP(response, req)
				require.Equal(t, http.StatusOK, response.Code, "multipart=%t: %s", form, response.Body.String())
				var created struct {
					ID string `json:"id"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
				stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
				require.NoError(t, err)
				require.True(t, exists)
				require.NotNil(t, stored.PrivateData.BillingContext.TieredSnapshot)
				assert.Equal(t, "matched", stored.PrivateData.BillingContext.TieredSnapshot.EstimatedTier, "multipart=%t", form)
				stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
				require.NoError(t, err)
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
				assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit), stored.Quota)
				var after model.User
				require.NoError(t, db.First(&after, account.user.Id).Error)
				assert.Equal(t, stored.Quota, before.Quota-after.Quota, "multipart=%t", form)
			}
		})
		t.Run("json numeric strings and multipart use the same price", func(t *testing.T) {
			expr, err := common.Marshal(map[string]string{modelName: `param("seconds") == 4 ? tier("matched", u("tokens") / 1000000) : tier("wrong", u("tokens") * 20 / 1000000)`})
			require.NoError(t, err)
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expr)}))
			t.Cleanup(func() {
				assert.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expressions)}))
			})
			previousRegistry := jsplugin.DefaultRegistry
			jsplugin.DefaultRegistry = registry
			t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
			protocolRouter := gin.New()
			protocolRouter.Use(middleware.RequestId(), middleware.BodyStorageCleanup())
			SetTaskPluginProtocolRouter(protocolRouter)
			for _, form := range []bool{false, true} {
				var before model.User
				require.NoError(t, db.First(&before, account.user.Id).Error)
				var input bytes.Buffer
				contentType := "application/json"
				if form {
					writer := multipart.NewWriter(&input)
					for key, value := range map[string]string{"model": modelName, "prompt": "a fixed camera", "seconds": "4", "resolution": "480p"} {
						require.NoError(t, writer.WriteField(key, value))
					}
					require.NoError(t, writer.Close())
					contentType = writer.FormDataContentType()
				} else {
					input.WriteString(`{"model":"doubao-seedance-2-0-260128","prompt":"a fixed camera","seconds":"4","resolution":"480p"}`)
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/videos", &input)
				req.Header.Set("Content-Type", contentType)
				req.Header.Set("Authorization", "Bearer sk-"+account.token.Key)
				response := httptest.NewRecorder()
				protocolRouter.ServeHTTP(response, req)
				require.Equal(t, http.StatusOK, response.Code, "multipart=%t: %s", form, response.Body.String())
				var created struct {
					ID string `json:"id"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
				stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
				require.NoError(t, err)
				require.True(t, exists)
				require.NotNil(t, stored.PrivateData.BillingContext.TieredSnapshot)
				assert.Equal(t, "matched", stored.PrivateData.BillingContext.TieredSnapshot.EstimatedTier, "multipart=%t", form)
				stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
				require.NoError(t, err)
				assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
				assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit), stored.Quota)
				var after model.User
				require.NoError(t, db.First(&after, account.user.Id).Error)
				assert.Equal(t, stored.Quota, before.Quota-after.Quota, "multipart=%t", form)
			}
		})
		t.Run("multipart file reference remains available to pricing", func(t *testing.T) {
			const fileModel = "kling-v1"
			source, err := plugins.Source("kling")
			require.NoError(t, err)
			kling, err := jsplugin.CompilePlugin(source, jsplugin.Options{Key: "kling"})
			require.NoError(t, err)
			require.NoError(t, registry.ReplaceOverrides(append(loaded, kling)))
			t.Cleanup(func() { assert.NoError(t, registry.ReplaceOverrides(loaded)) })
			previousRegistry := jsplugin.DefaultRegistry
			jsplugin.DefaultRegistry = registry
			t.Cleanup(func() { jsplugin.DefaultRegistry = previousRegistry })
			upstreamKey, err := common.GenerateRandomCharsKey(32)
			require.NoError(t, err)
			upstreamKey = "sk-" + upstreamKey
			mu.Lock()
			upstreamKeys["kling"] = upstreamKey
			mu.Unlock()
			base := upstream.URL + "/fixture-kling"
			setting := `{"task_plugin_key":"kling"}`
			channel := model.Channel{Type: constant.ChannelTypeTaskPlugin, Name: "compat-kling-file", Key: upstreamKey, BaseURL: &base, Setting: &setting, Status: common.ChannelStatusEnabled, Group: "default", Models: fileModel}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			fileModes, err := common.Marshal(map[string]string{modelName: "tiered_expr", fileModel: "tiered_expr"})
			require.NoError(t, err)
			fileExpr, err := common.Marshal(map[string]string{modelName: `tier("base", u("tokens") / 1000000)`, fileModel: `param("input_reference") != nil && param("seconds") == nil ? tier("matched", u("units") * 0.01) : tier("wrong", u("units") * 0.5)`})
			require.NoError(t, err)
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_mode": string(fileModes), "billing_setting.billing_expr": string(fileExpr)}))
			t.Cleanup(func() {
				assert.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_mode": string(modes), "billing_setting.billing_expr": string(expressions)}))
			})
			protocolRouter := gin.New()
			protocolRouter.Use(middleware.RequestId(), middleware.BodyStorageCleanup())
			SetTaskPluginProtocolRouter(protocolRouter)
			for _, form := range []bool{false, true} {
				var input bytes.Buffer
				contentType := "application/json"
				if form {
					writer := multipart.NewWriter(&input)
					for key, value := range map[string]string{"model": fileModel, "prompt": "a fixed camera", "duration": "5"} {
						require.NoError(t, writer.WriteField(key, value))
					}
					file, err := writer.CreateFormFile("input_reference", "reference.jpg")
					require.NoError(t, err)
					_, err = file.Write([]byte("fixture-image"))
					require.NoError(t, err)
					require.NoError(t, writer.Close())
					contentType = writer.FormDataContentType()
				} else {
					input.WriteString(`{"model":"kling-v1","prompt":"a fixed camera","duration":5,"input_reference":"https://media.example/input.jpg"}`)
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/videos", &input)
				req.Header.Set("Content-Type", contentType)
				req.Header.Set("Authorization", "Bearer sk-"+account.token.Key)
				response := httptest.NewRecorder()
				protocolRouter.ServeHTTP(response, req)
				require.Equal(t, http.StatusOK, response.Code, "multipart=%t: %s", form, response.Body.String())
				var created struct {
					ID string `json:"id"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
				stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
				require.NoError(t, err)
				require.True(t, exists)
				require.NotNil(t, stored.PrivateData.BillingContext.TieredSnapshot)
				assert.Equal(t, "matched", stored.PrivateData.BillingContext.TieredSnapshot.EstimatedTier, "multipart=%t", form)
				if form {
					mu.Lock()
					forwarded := requests["kling:fixture"]
					mu.Unlock()
					assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("fixture-image")), forwarded["image"])
				}
			}
		})
		t.Run("old task writer preserves billing input", func(t *testing.T) {
			response := request(http.MethodPost, paths[0], compatibleBody, account, false)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var created struct {
				ID string `json:"id"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
			stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, err)
			require.True(t, exists)
			var raw string
			require.NoError(t, db.Raw("SELECT private_data FROM tasks WHERE id = ?", stored.ID).Scan(&raw).Error)
			var private, context, snapshot map[string]common.RawMessage
			require.NoError(t, common.Unmarshal([]byte(raw), &private))
			require.NoError(t, common.Unmarshal(private["billing_context"], &context))
			require.NoError(t, common.Unmarshal(context["tiered_snapshot"], &snapshot))
			delete(snapshot, "encrypted_request_context")
			context["tiered_snapshot"], err = common.Marshal(snapshot)
			require.NoError(t, err)
			private["billing_context"], err = common.Marshal(context)
			require.NoError(t, err)
			updated, err := common.Marshal(private)
			require.NoError(t, err)
			require.NoError(t, db.Model(&model.Task{}).Where("id = ?", stored.ID).Update("private_data", string(updated)).Error)
			stored, exists, err = model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, err)
			require.True(t, exists)
			stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
			require.NoError(t, err)
			assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
			assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit), stored.Quota)
		})
		t.Run("nonfinite settlement must not charge", func(t *testing.T) {
			const expression = `param("duration") == 4 && u("tokens") == 120 ? tier("bad", u("tokens") / (u("tokens") - 120)) : tier("normal", u("tokens") / 1000000)`
			plan, err := billingexpr.PlanTaskRequest(expression)
			require.NoError(t, err)
			input, sealed, err := billingexpr.CaptureTaskRequest(plan, billingexpr.RequestInput{Body: []byte(`{"duration":4}`)}, time.Unix(3600, 0))
			require.NoError(t, err)
			input.Usage = map[string]any{"tokens": float64(100)}
			cost, _, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, input)
			require.NoError(t, err)
			assert.InDelta(t, 0.0001, cost, 0.00000001)
			snapshot := &billingexpr.BillingSnapshot{ExprString: billingexpr.MarkTaskRequestExpression(expression), GroupRatio: 1, QuotaPerUnit: common.QuotaPerUnit, TaskUsageBilling: true, EncryptedRequestContext: sealed}
			_, _, err = service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
			require.Error(t, err)

			expr, err := common.Marshal(map[string]string{modelName: expression})
			require.NoError(t, err)
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expr)}))
			t.Cleanup(func() {
				assert.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expressions)}))
			})
			response := request(http.MethodPost, paths[0], compatibleBody, account, false)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var created struct {
				ID string `json:"id"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
			stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, err)
			require.True(t, exists)
			var reserved model.User
			require.NoError(t, db.First(&reserved, account.user.Id).Error)
			_, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
			require.Error(t, err)
			stored, exists, err = model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, err)
			require.True(t, exists)
			assert.NotEqual(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
			var after model.User
			require.NoError(t, db.First(&after, account.user.Id).Error)
			assert.Equal(t, reserved.Quota, after.Quota)
			assert.Equal(t, reserved.UsedQuota, after.UsedQuota)
		})
		t.Run("nonfinite reservation is rejected before submission", func(t *testing.T) {
			const expression = `param("duration") == 4 ? tier("bad", u("tokens") / (param("duration") - 4)) : tier("normal", u("tokens") / 1000000)`
			expr, err := common.Marshal(map[string]string{modelName: expression})
			require.NoError(t, err)
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expr)}))
			t.Cleanup(func() {
				assert.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expressions)}))
			})
			var before model.User
			require.NoError(t, db.First(&before, account.user.Id).Error)
			mu.Lock()
			submissions := counts["doubao"]
			mu.Unlock()
			var response *httptest.ResponseRecorder
			require.NotPanics(t, func() { response = request(http.MethodPost, paths[0], compatibleBody, account, false) })
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
			mu.Lock()
			assert.Equal(t, submissions, counts["doubao"])
			mu.Unlock()
			var after model.User
			require.NoError(t, db.First(&after, account.user.Id).Error)
			assert.Equal(t, before.Quota, after.Quota)
		})
		t.Run("task expression configuration matches execution", func(t *testing.T) {
			schema, _ := plugin.Meta.UsageForModel(modelName)
			dynamic := `param("dur" + "ation") == 4 ? tier("matched", u("tokens") / 1000000) : tier("other", u("tokens") / 1000000)`
			_, err := billingexpr.PlanTaskRequest(dynamic)
			require.Error(t, err)
			assert.Error(t, billing_setting.SmokeTestTaskExpr(dynamic, schema))
			assert.False(t, billing_setting.TaskExprCompatible(dynamic, schema))
			for _, indirect := range []string{
				`let h = header; h("x-billing-tier") == "fixture" ? tier("one", u("tokens")) : tier("two", u("tokens"))`,
				`let h = hour; h("UTC") == 0 ? tier("one", u("tokens")) : tier("two", u("tokens"))`,
				`$env["param"]("duration") == 4 ? tier("one", u("tokens")) : tier("two", u("tokens"))`,
			} {
				_, indirectErr := billingexpr.PlanTaskRequest(indirect)
				assert.Error(t, indirectErr, indirect)
				assert.Error(t, billing_setting.SmokeTestTaskExpr(indirect, schema), indirect)
			}
			alias := `let f = param; f("duration") == 4 ? tier("matched", u("tokens") / 1000000) : tier("other", u("tokens") * 20 / 1000000)`
			plan, err := billingexpr.PlanTaskRequest(alias)
			if err != nil {
				assert.Error(t, billing_setting.SmokeTestTaskExpr(alias, schema))
				return
			}
			input, _, err := billingexpr.CaptureTaskRequest(plan, billingexpr.RequestInput{Body: []byte(`{"duration":4}`)}, time.Unix(3600, 0))
			require.NoError(t, err)
			input.Usage = map[string]any{"tokens": float64(120)}
			_, trace, err := billingexpr.RunExprWithRequest(alias, billingexpr.TokenParams{}, input)
			require.NoError(t, err)
			assert.Equal(t, "matched", trace.MatchedTier)
		})
		t.Run("header-only task pricing", func(t *testing.T) {
			headerExpr, err := common.Marshal(map[string]string{modelName: `header("x-billing-tier") == "fixture" ? tier("matched", u("tokens") / 1000000) : tier("wrong", u("tokens") * 20 / 1000000)`})
			require.NoError(t, err)
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(headerExpr)}))
			t.Cleanup(func() {
				assert.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_expr": string(expressions)}))
			})
			response := request(http.MethodPost, paths[0], body, account, true)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var created struct {
				ID string `json:"id"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
			stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, err)
			require.True(t, exists)
			require.NotNil(t, stored.PrivateData.BillingContext)
			require.NotNil(t, stored.PrivateData.BillingContext.TieredSnapshot)
			assert.Equal(t, "matched", stored.PrivateData.BillingContext.TieredSnapshot.EstimatedTier)
			stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
			require.NoError(t, err)
			require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
			assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit), stored.Quota)
		})
		t.Run("discounted task pricing", func(t *testing.T) {
			_, revision, err := model.GetUserModelPricing(account.user.Id)
			require.NoError(t, err)
			revision, err = model.ReplaceUserModelPricing(account.user.Id, map[string]int{modelName: 8000}, revision)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := model.ReplaceUserModelPricing(account.user.Id, map[string]int{}, revision)
				assert.NoError(t, err)
			})
			for _, path := range paths {
				var before model.User
				require.NoError(t, db.First(&before, account.user.Id).Error)
				response := request(http.MethodPost, path, compatibleBody, account, false)
				require.Equal(t, http.StatusOK, response.Code, "%s: %s", path, response.Body.String())
				var created struct {
					ID string `json:"id"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
				stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
				require.NoError(t, err)
				require.True(t, exists)
				require.NotNil(t, stored.PrivateData.BillingContext)
				require.NotNil(t, stored.PrivateData.BillingContext.UserDiscount)
				assert.Equal(t, 8000, stored.PrivateData.BillingContext.UserDiscount.DiscountBPS)
				stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
				require.NoError(t, err)
				require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), stored.Status)
				assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit*0.8), stored.Quota)
				var after model.User
				require.NoError(t, db.First(&after, account.user.Id).Error)
				assert.Equal(t, stored.Quota, before.Quota-after.Quota)
			}
		})
		var before model.User
		require.NoError(t, db.First(&before, account.user.Id).Error)
		mu.Lock()
		beforeSubmissions := counts["doubao"]
		mu.Unlock()
		var baselineError struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		for index, path := range paths {
			invalid := request(http.MethodPost, path, `{"model":"doubao-seedance-2-0-260128","prompt":"test","parameters":{"duration":31}}`, account, false)
			assert.Equal(t, http.StatusBadRequest, invalid.Code, "%s: %s", path, invalid.Body.String())
			assert.Contains(t, invalid.Body.String(), "duration")
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, common.Unmarshal(invalid.Body.Bytes(), &envelope))
			if index == 0 {
				baselineError = envelope.Error
			} else {
				assert.Equal(t, baselineError, envelope.Error, path)
			}
		}
		mu.Lock()
		assert.Equal(t, beforeSubmissions, counts["doubao"])
		mu.Unlock()
		var after model.User
		require.NoError(t, db.First(&after, account.user.Id).Error)
		assert.Equal(t, before.Quota, after.Quota)
	})

	t.Run("task billing freezes time and preserves legacy snapshots", func(t *testing.T) {
		const expression = `hour("UTC") == 1 ? tier("matched", u("tokens") / 1000000) : tier("wrong", u("tokens") * 20 / 1000000)`
		plan, err := billingexpr.PlanTaskRequest(expression)
		require.NoError(t, err)
		input, sealed, err := billingexpr.CaptureTaskRequest(plan, billingexpr.RequestInput{}, time.Unix(3600, 0))
		require.NoError(t, err)
		input.Usage = map[string]any{"tokens": float64(120)}
		cost, trace, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, input)
		require.NoError(t, err)
		assert.InDelta(t, 0.00012, cost, 0.00000001)
		assert.Equal(t, "matched", trace.MatchedTier)
		snapshot := &billingexpr.BillingSnapshot{ExprString: billingexpr.MarkTaskRequestExpression(expression), GroupRatio: 1, QuotaPerUnit: common.QuotaPerUnit, TaskUsageBilling: true, EncryptedRequestContext: sealed}
		assert.Equal(t, expression, billingexpr.PublicExpression(snapshot.ExprString))
		result, _, err := service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.NoError(t, err)
		assert.Equal(t, "matched", result.MatchedTier)
		assert.Equal(t, common.QuotaRound(120.0/1000000*common.QuotaPerUnit), result.ActualQuotaAfterGroup)
		snapshot.ExprString = `param("duration") == nil ? tier("legacy", u("tokens") / 1000000) : tier("wrong", u("tokens") * 20 / 1000000)`
		snapshot.EncryptedRequestContext = ""
		legacy, _, err := service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.NoError(t, err)
		assert.Equal(t, "legacy", legacy.MatchedTier)
		snapshot.EncryptedRequestContext = sealed
		_, _, err = service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.Error(t, err)
		snapshot.EncryptedRequestContext = "task-sealed:v1:invalid"
		_, _, err = service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.Error(t, err)
		snapshot.ExprString = billingexpr.MarkTaskRequestExpression(expression)
		snapshot.EncryptedRequestContext = ""
		_, _, err = service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.Error(t, err)
		snapshot.ExprString = billingexpr.MarkTaskRequestExpressionWithContext(expression, sealed)
		embedded, _, err := service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.NoError(t, err)
		assert.Equal(t, "matched", embedded.MatchedTier)
		snapshot.ExprString = billingexpr.MarkTaskRequestExpressionWithContext(expression, "task-sealed:v1:invalid")
		_, _, err = service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.Error(t, err)
		snapshot.ExprString = "task_request_snapshot_v2:missing-separator"
		_, _, err = service.EvaluateTaskCompletionUsage(snapshot, map[string]any{"tokens": float64(120)})
		require.Error(t, err)
		_, err = billingexpr.PlanTaskRequest(`param(u("resolution")) == nil ? tier("base", u("tokens") / 1000000) : tier("other", u("tokens") / 1000000)`)
		require.ErrorContains(t, err, "literal")
		largePlan, err := billingexpr.PlanTaskRequest(`param("value") == nil ? tier("base", u("tokens") / 1000000) : tier("other", u("tokens") / 1000000)`)
		require.NoError(t, err)
		_, _, err = billingexpr.CaptureTaskRequest(largePlan, billingexpr.RequestInput{Body: []byte(`{"value":"` + strings.Repeat("x", 17<<10) + `"}`)}, time.Now())
		require.ErrorContains(t, err, "too large")
	})

	t.Run("failed tasks refund once on every path", func(t *testing.T) {
		account := accounts["doubao"]
		plugin, found := registry.Generation().Get("doubao")
		require.True(t, found)
		for _, path := range paths {
			var before model.User
			require.NoError(t, db.First(&before, account.user.Id).Error)
			response := request(http.MethodPost, path, body, account, false)
			require.Equal(t, http.StatusOK, response.Code, "%s: %s", path, response.Body.String())
			var created struct {
				ID string `json:"id"`
			}
			require.NoError(t, common.Unmarshal(response.Body.Bytes(), &created))
			stored, exists, err := model.GetByTaskId(account.user.Id, created.ID)
			require.NoError(t, err)
			require.True(t, exists)
			var reserved model.User
			require.NoError(t, db.First(&reserved, account.user.Id).Error)
			assert.Positive(t, before.Quota-reserved.Quota)
			mu.Lock()
			states[stored.GetUpstreamTaskID()] = "failed"
			mu.Unlock()
			stored, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
			require.NoError(t, err)
			require.Equal(t, model.TaskStatus(model.TaskStatusFailure), stored.Status)
			result := compareQueries(t, account, created.ID, "failed")
			require.Contains(t, result, "error")
			_, err = service.RefreshTaskForManagement(t.Context(), taskplugin.New(plugin), &account.channel, stored)
			require.NoError(t, err)
			var after model.User
			require.NoError(t, db.First(&after, account.user.Id).Error)
			assert.Equal(t, before.Quota, after.Quota, "失败只退回一次预扣额度")
		}
	})

	t.Run("list ownership and deletion match across video paths", func(t *testing.T) {
		require.Len(t, doubaoTaskIDs, len(paths))
		account := accounts["doubao"]
		values := url.Values{"page_size": {"10"}, "filter.status": {"succeeded"}, "filter.task_ids": doubaoTaskIDs}
		var baseline string
		for _, path := range paths {
			for _, signed := range []bool{false, true} {
				response := request(http.MethodGet, path+"?"+values.Encode(), "", account, signed)
				require.Equal(t, http.StatusOK, response.Code, "%s signed=%t: %s", path, signed, response.Body.String())
				if baseline == "" {
					baseline = response.Body.String()
					var result struct {
						Items []map[string]any `json:"items"`
						Total int              `json:"total"`
					}
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &result))
					assert.Equal(t, len(doubaoTaskIDs), result.Total)
					assert.Len(t, result.Items, len(doubaoTaskIDs))
				} else {
					assert.Equal(t, baseline, response.Body.String(), "%s signed=%t", path, signed)
				}
			}
			foreign := request(http.MethodGet, path+"/"+doubaoTaskIDs[0], "", accounts["other"], false)
			assert.Equal(t, http.StatusNotFound, foreign.Code)
			foreignDelete := request(http.MethodDelete, path+"/"+doubaoTaskIDs[0], "", accounts["other"], false)
			assert.Equal(t, http.StatusNotFound, foreignDelete.Code)
			assert.Zero(t, deleteRequests.Load())
			invalid := httptest.NewRequest(http.MethodGet, path+"/"+doubaoTaskIDs[0]+"?value=1", nil)
			signSeedanceClientRequest(t, invalid, nil, account.accessKey, account.signingKey, time.Now(), true)
			invalid.URL.RawQuery = "value=2"
			invalid.RequestURI = invalid.URL.RequestURI()
			invalidResponse := httptest.NewRecorder()
			outer.ServeHTTP(invalidResponse, invalid)
			assert.Equal(t, http.StatusUnauthorized, invalidResponse.Code)
		}
		for index, path := range paths {
			var before model.User
			require.NoError(t, db.First(&before, account.user.Id).Error)
			response := request(http.MethodDelete, path+"/"+doubaoTaskIDs[index], "", account, index == 1)
			require.Equal(t, http.StatusOK, response.Code, "%s: %s", path, response.Body.String())
			assert.JSONEq(t, `{}`, response.Body.String())
			for _, queryPath := range paths {
				deleted := request(http.MethodGet, queryPath+"/"+doubaoTaskIDs[index], "", account, false)
				assert.Equal(t, http.StatusNotFound, deleted.Code)
			}
			var after model.User
			require.NoError(t, db.First(&after, account.user.Id).Error)
			assert.Equal(t, before.Quota, after.Quota, "删除已结算任务不能改变余额")
		}
		assert.EqualValues(t, len(paths), deleteRequests.Load())
	})

	t.Run("allocation failure stays local", func(t *testing.T) {
		const callback = "test:seedance-compat-allocation-failure"
		var attempts atomic.Int32
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "seedance_compat_task_id_sequences" {
				attempts.Add(1)
				tx.AddError(errors.New("test allocation failure"))
			}
		}))
		t.Cleanup(func() { assert.NoError(t, db.Callback().Create().Remove(callback)) })
		var before model.User
		require.NoError(t, db.First(&before, accounts["doubao"].user.Id).Error)
		mu.Lock()
		beforeSubmissions := counts["doubao"]
		mu.Unlock()
		for _, path := range paths {
			attempts.Store(0)
			response := request(http.MethodPost, path, body, accounts["doubao"], false)
			assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
			assert.EqualValues(t, 1, attempts.Load())
			mu.Lock()
			assert.Equal(t, beforeSubmissions, counts["doubao"])
			mu.Unlock()
			var after model.User
			require.NoError(t, db.First(&after, before.Id).Error)
			assert.Equal(t, before.Quota, after.Quota)
			traceMu.Lock()
			trace := append([]service.PolicyEvent(nil), lastTrace...)
			traceMu.Unlock()
			localFailure := false
			for _, event := range trace {
				if event.ErrorSource == "local" && event.Decision.Reason == "local_rejection" {
					localFailure = true
				}
				assert.NotEqual(t, "channel_disable_requested", event.Health)
			}
			assert.True(t, localFailure, "%+v", trace)
		}
	})

	t.Run("historical collisions advance the allocation", func(t *testing.T) {
		future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
		sequence := model.SeedanceCompatTaskIDSequence{ID: 1, LastSecond: future.Unix(), Start: 0, Allocated: 0}
		require.NoError(t, db.Save(&sequence).Error)
		tasks := make([]model.Task, 64)
		for i := range tasks {
			suffix := strconv.FormatInt(int64(i), 36)
			tasks[i] = model.Task{TaskID: "cgt-" + future.Format("20060102150405") + "-" + strings.Repeat("0", 5-len(suffix)) + suffix, UserId: accounts["doubao"].user.Id, Platform: "doubao"}
		}
		require.NoError(t, db.Create(&tasks).Error)
		id, allocErr := model.GenerateSeedanceCompatTaskID(t.Context())
		require.Error(t, allocErr)
		assert.Empty(t, id)
		require.NoError(t, db.First(&sequence, 1).Error)
		assert.EqualValues(t, 64, sequence.Allocated)
		id, allocErr = model.GenerateSeedanceCompatTaskID(t.Context())
		require.NoError(t, allocErr)
		assert.Equal(t, "cgt-"+future.Format("20060102150405")+"-0001s", id)
	})

	t.Run("independent processes allocate unique IDs", func(t *testing.T) {
		future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
		sequence := model.SeedanceCompatTaskIDSequence{ID: 1, LastSecond: future.Unix(), Start: 1000, Allocated: 0}
		require.NoError(t, db.Save(&sequence).Error)
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		t.Cleanup(cancel)
		type worker struct {
			command *exec.Cmd
			input   io.WriteCloser
			output  *bufio.Reader
			waited  bool
		}
		workers := make([]*worker, 0, 2)
		t.Cleanup(func() {
			cancel()
			for _, worker := range workers {
				if !worker.waited {
					_ = worker.command.Process.Kill()
					_ = worker.command.Wait()
				}
			}
		})
		for range 2 {
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSeedanceCompatAllocatorWorker$")
			command.Env = append(os.Environ(), "SEEDANCE_COMPAT_ALLOCATOR_WORKER=1")
			input, pipeErr := command.StdinPipe()
			require.NoError(t, pipeErr)
			output, pipeErr := command.StdoutPipe()
			require.NoError(t, pipeErr)
			require.NoError(t, command.Start())
			worker := &worker{command: command, input: input, output: bufio.NewReader(output)}
			workers = append(workers, worker)
			line, readErr := worker.output.ReadString('\n')
			require.NoError(t, readErr)
			require.Equal(t, "READY\n", line)
		}
		lock := db.Begin()
		require.NoError(t, lock.Error)
		defer lock.Rollback()
		require.NoError(t, lock.Model(&model.SeedanceCompatTaskIDSequence{}).Where("id = ?", 1).Update("id", 1).Error)
		for _, worker := range workers {
			_, writeErr := io.WriteString(worker.input, "start\n")
			require.NoError(t, writeErr)
			require.NoError(t, worker.input.Close())
			line, readErr := worker.output.ReadString('\n')
			require.NoError(t, readErr)
			require.Equal(t, "START\n", line)
		}
		require.NoError(t, lock.Commit().Error)
		allocated := map[string]struct{}{}
		for _, worker := range workers {
			output, readErr := io.ReadAll(worker.output)
			require.NoError(t, readErr)
			waitErr := worker.command.Wait()
			worker.waited = true
			require.NoError(t, waitErr, string(output))
			for _, line := range strings.Split(string(output), "\n") {
				if !strings.HasPrefix(line, "ID:") {
					continue
				}
				id := strings.TrimPrefix(line, "ID:")
				assert.Regexp(t, format, id)
				assert.True(t, strings.HasPrefix(id, "cgt-"+future.Format("20060102150405")+"-"))
				_, duplicate := allocated[id]
				assert.False(t, duplicate, id)
				allocated[id] = struct{}{}
			}
		}
		assert.Len(t, allocated, 16)
		require.NoError(t, db.First(&sequence, 1).Error)
		assert.EqualValues(t, 16, sequence.Allocated)
	})
}

func TestSeedanceCompatAllocatorWorker(t *testing.T) {
	if os.Getenv("SEEDANCE_COMPAT_ALLOCATOR_WORKER") != "1" {
		t.Skip("allocator worker is run by the MySQL integration test")
	}
	dsn := seedanceCompatTestDSN(t)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	defer connection.Close()
	connection.SetMaxOpenConns(8)
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
	_, _ = fmt.Fprintln(os.Stdout, "READY")
	_, err = bufio.NewReader(os.Stdin).ReadString('\n')
	require.NoError(t, err)
	_, _ = fmt.Fprintln(os.Stdout, "START")
	type result struct {
		id  string
		err error
	}
	results := make(chan result, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			id, allocErr := model.GenerateSeedanceCompatTaskID(t.Context())
			results <- result{id: id, err: allocErr}
		})
	}
	workers.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result.err)
		_, _ = fmt.Fprintln(os.Stdout, "ID:"+result.id)
	}
}
