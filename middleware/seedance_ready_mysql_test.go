package middleware

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type seedanceReadyFixture struct {
	engine  *gin.Engine
	db      *gorm.DB
	replica *model.SeedanceAssetReplica
	group   *model.SeedanceAssetGroup
	asset   *model.SeedanceAsset
	high    *model.Channel
}

func setSeedanceReadyChannelSetting(t testing.TB, channel *model.Channel, setting kitdto.ChannelSettings) {
	t.Helper()
	channel.SetSetting(setting)
	require.Equal(t, setting, channel.GetSetting())
}

func newSeedanceReadyFixture(t testing.TB) *seedanceReadyFixture {
	t.Helper()
	dsn := os.Getenv("SEEDANCE_READY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("SEEDANCE_READY_TEST_MYSQL_DSN is not configured")
	}
	config, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	require.Equal(t, "tcp", config.Net)
	host, _, err := net.SplitHostPort(config.Addr)
	require.NoError(t, err)
	require.True(t, net.ParseIP(host).IsLoopback())
	require.True(t, strings.HasPrefix(config.DBName, "codex_seedance_ready_"))
	name := config.DBName
	adminConfig := *config
	adminConfig.DBName = ""
	admin, err := gorm.Open(mysql.Open(adminConfig.FormatDSN()), &gorm.Config{})
	require.NoError(t, err)
	adminConn, err := admin.DB()
	require.NoError(t, err)
	require.NoError(t, admin.Exec("CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error)
	t.Cleanup(func() {
		require.NoError(t, admin.Exec("DROP DATABASE `"+name+"`").Error)
		require.NoError(t, adminConn.Close())
	})

	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	oldMaster, oldRedis, oldMemory := common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled
	oldGroups, oldUsable := ratio_setting.GroupRatio2JSONString(), setting.UserUsableGroups2JSONString()
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = true, false, false
	t.Setenv("SQL_DSN", config.FormatDSN())
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	db := model.DB
	dbConn, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, dbConn.Close())
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.SetDatabaseTypes(oldMain, oldLog)
		common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = oldMaster, oldRedis, oldMemory
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(oldUsable))
	})
	require.NoError(t, i18n.Init())
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"vip":1,"aaa":1}`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"vip":"VIP","aaa":"AAA"}`))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "seedance-ready-fixture", Password: "fixture", AffCode: "seedance-ready-fixture", Status: common.UserStatusEnabled, Group: "vip"}).Error)

	const modelName = "doubao-seedance-2-0-260128"
	baseURL := "https://video.example.test"
	assetURL := "https://assets.example.test"
	highPriority, lowPriority := int64(10), int64(0)
	high := &model.Channel{Id: 101, Type: constant.ChannelTypeTaskPlugin, Name: "high", Key: "high-test-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &baseURL, Priority: &highPriority}
	low := &model.Channel{Id: 102, Type: constant.ChannelTypeTaskPlugin, Name: "low", Key: "low-test-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &baseURL, Priority: &lowPriority}
	for _, channel := range []*model.Channel{high, low} {
		setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: assetURL})
		require.NoError(t, channel.Insert())
	}
	lowKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(low.Key)))
	highKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(high.Key)))
	lowAccount := model.SeedanceAssetAccountFingerprint(low, lowKey)
	lowModelAccount := model.SeedanceAssetModelScopeFingerprint(low, modelName, lowKey)
	highAccount := model.SeedanceAssetModelScopeFingerprint(high, modelName, highKey)
	group := &model.SeedanceAssetGroup{UserID: 1, ChannelID: low.Id, GroupID: "group-test", Name: "test", GroupType: "AIGC", Status: "Active", KeyFingerprint: lowKey, AccountFingerprint: lowAccount}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{UserID: 1, ChannelID: low.Id, GroupID: group.GroupID, AssetID: "asset-test", Name: "test", AssetType: "Image", Status: "Active", SourceURL: "https://example.test/image.png", KeyFingerprint: lowKey, AccountFingerprint: lowModelAccount}
	require.NoError(t, db.Create(asset).Error)
	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{modelName}, 0)
	require.NoError(t, err)
	replica := &model.SeedanceAssetReplica{UserID: 1, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: high.Id, AccountFingerprint: highAccount, KeyFingerprint: highKey, SyncScope: scope, ProvisionStatus: model.SeedanceAssetReplicaCreating, Status: "Pending", NextPollAt: time.Now().Unix() + 60}
	require.NoError(t, db.Create(replica).Error)
	require.NoError(t, db.Create(&model.SeedanceAssetGroupReplica{UserID: 1, LocalGroupID: group.ID, ChannelID: high.Id, AccountFingerprint: highAccount, KeyFingerprint: highKey, UpstreamGroupID: "group-high", GroupName: group.Name, Status: model.SeedanceAssetReplicaReady}).Error)
	require.NoError(t, db.Create(&model.SeedanceAssetGroupReplica{UserID: 1, LocalGroupID: group.ID, ChannelID: low.Id, AccountFingerprint: lowModelAccount, KeyFingerprint: lowKey, UpstreamGroupID: group.GroupID, GroupName: group.Name, Status: model.SeedanceAssetReplicaReady}).Error)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set("id", 1)
		c.Set("expected_task_plugin_key", "doubao")
		group := c.GetHeader("X-Test-Group")
		if group == "" {
			group = "vip"
		}
		common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "vip")
		if group == "auto" {
			common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, []string{"vip", "aaa"})
		}
		if allowed := c.GetHeader("X-Test-Allowed-Model"); allowed != "" {
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{allowed: true})
		}
		if pin := c.GetHeader("X-Test-Pin"); pin != "" {
			id, err := strconv.Atoi(pin)
			if err == nil {
				service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: id, Source: dto.PinSourceToken, Rank: dto.PinRankToken})
			}
		}
		defer common.CleanupBodyStorage(c)
		c.Next()
	})
	engine.POST("/v1/videos", func(c *gin.Context) {
		if c.GetHeader("X-Test-Skip-Readiness") == "" {
			Distribute()(c)
			return
		}
		constraints := service.GetChannelConstraints(c)
		constraints.AddFilter(dto.ChannelFilter{Kind: dto.FilterRequestPath, RequestPath: c.Request.URL.Path})
		service.AppendTaskPluginIdentityFilter(c, "doubao")
		request, _, err := getModelRequest(c)
		if err != nil {
			c.AbortWithStatus(http.StatusBadRequest)
			return
		}
		channel, _, selectErr := service.SelectChannelForRequest(c, request.Model, &service.RetryParam{
			Ctx: c, ModelName: request.Model, TokenGroup: "vip", RequestPath: c.Request.URL.Path, Retry: common.GetPointer(0),
		})
		if selectErr != nil {
			c.AbortWithStatus(selectErr.StatusCode)
			return
		}
		if setupErr := SetupContextForSelectedChannel(c, channel, request.Model); setupErr != nil {
			c.AbortWithStatus(setupErr.StatusCode)
			return
		}
		c.Next()
	}, func(c *gin.Context) {
		c.Header("X-Test-Channel", strconv.Itoa(common.GetContextKeyInt(c, constant.ContextKeyChannelId)))
		c.Header("X-Test-Key", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
		if mapping, ok := common.GetContextKeyType[map[string]string](c, constant.ContextKeySeedanceAssetMapping); ok {
			c.Header("X-Test-Mapped-Asset", mapping["asset-test"])
		}
		c.Status(http.StatusOK)
	})
	return &seedanceReadyFixture{engine: engine, db: db, replica: replica, group: group, asset: asset, high: high}
}

func (fixture *seedanceReadyFixture) submit() *httptest.ResponseRecorder {
	body := `{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-test"}}]}`
	return fixture.submitWith(body, "", "")
}

func (fixture *seedanceReadyFixture) submitWith(body, group, pin string, allowed ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Test-Group", group)
	request.Header.Set("X-Test-Pin", pin)
	if len(allowed) > 0 {
		request.Header.Set("X-Test-Allowed-Model", allowed[0])
	}
	response := httptest.NewRecorder()
	fixture.engine.ServeHTTP(response, request)
	return response
}

func TestSeedanceAssetReadyRoutingMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	response := fixture.submit()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "102", response.Header().Get("X-Test-Channel"))
	require.NoError(t, fixture.db.Model(fixture.group).Update("group_type", "LivenessFace").Error)
	require.NoError(t, fixture.db.Model(fixture.asset).Update("account_fingerprint", fixture.group.AccountFingerprint).Error)
	response = fixture.submit()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "102", response.Header().Get("X-Test-Channel"))
	require.NoError(t, fixture.db.Model(fixture.replica).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaReady, "status": "Active", "upstream_asset_id": "asset-high", "upstream_group_id": "group-high", "next_poll_at": 0,
	}).Error)
	humanBase := "https://video.example.test"
	humanPriority := int64(100)
	humanMulti := &model.Channel{Id: 106, Type: constant.ChannelTypeTaskPlugin, Name: "human-multi", Key: "high-test-key\nlow-test-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: "doubao-seedance-2-0-260128", BaseURL: &humanBase, Priority: &humanPriority, ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	setSeedanceReadyChannelSetting(t, humanMulti, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: "https://assets.example.test"})
	require.NoError(t, humanMulti.Insert())
	require.NoError(t, fixture.db.Model(fixture.asset).Update("status", "Processing").Error)
	response = fixture.submit()
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("X-Test-Channel"))
	require.NoError(t, fixture.db.Model(fixture.asset).Update("status", "Active").Error)
	response = fixture.submit()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "106", response.Header().Get("X-Test-Channel"))
	require.Equal(t, "low-test-key", response.Header().Get("X-Test-Key"))
	require.NoError(t, fixture.db.Where("channel_id = ?", humanMulti.Id).Delete(&model.Ability{}).Error)
	require.NoError(t, fixture.db.Delete(humanMulti).Error)
	require.NoError(t, fixture.db.Model(fixture.replica).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaCreating, "status": "Pending", "upstream_asset_id": "", "next_poll_at": time.Now().Unix() + 60,
	}).Error)
	require.NoError(t, fixture.db.Model(fixture.group).Update("group_type", "AIGC").Error)
	var low model.Channel
	require.NoError(t, fixture.db.First(&low, 102).Error)
	require.NoError(t, fixture.db.Model(fixture.asset).Update("account_fingerprint", model.SeedanceAssetModelScopeFingerprint(&low, "doubao-seedance-2-0-260128", fixture.asset.KeyFingerprint)).Error)

	response = fixture.submitWith(`{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-test"}}]}`, "vip", "101")
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("X-Test-Channel"))

	require.NoError(t, fixture.db.Model(fixture.replica).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaReady, "status": "Active", "upstream_asset_id": "asset-high", "upstream_group_id": "group-high", "next_poll_at": 0,
	}).Error)
	response = fixture.submit()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "101", response.Header().Get("X-Test-Channel"))

	require.NoError(t, fixture.db.Model(fixture.replica).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaCreating, "status": "Pending", "upstream_asset_id": "", "next_poll_at": time.Now().Unix() + 60,
	}).Error)
	require.NoError(t, fixture.db.Model(fixture.asset).Update("status", "Processing").Error)
	response = fixture.submit()
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Equal(t, "5", response.Header().Get("Retry-After"))

	require.NoError(t, fixture.db.Model(fixture.replica).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaFailed, "status": "Pending", "next_poll_at": 0, "last_error": "fixture failure",
	}).Error)
	require.NoError(t, fixture.db.Model(fixture.asset).Update("status", "Failed").Error)
	response = fixture.submit()
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())

	require.NoError(t, fixture.db.Model(fixture.asset).Update("status", "Active").Error)
	require.NoError(t, fixture.db.Model(fixture.replica).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaCreating, "status": "Pending", "next_poll_at": time.Now().Unix() + 60, "last_error": "",
	}).Error)
	highKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(fixture.high.Key)))
	second := &model.SeedanceAsset{UserID: 1, ChannelID: fixture.high.Id, GroupID: fixture.group.GroupID, AssetID: "asset-second", Name: "second", AssetType: "Image", Status: "Active", SourceURL: "https://example.test/second.png", KeyFingerprint: highKey, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(fixture.high, "doubao-seedance-2-0-260128", highKey)}
	require.NoError(t, fixture.db.Create(second).Error)
	response = fixture.submitWith(`{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-test"}},{"type":"image_url","image_url":{"url":"asset://asset-second"}}]}`, "vip", "")
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("X-Test-Channel"))

	baseURL := "https://video.example.test"
	priority := int64(100)
	multi := &model.Channel{Id: 104, Type: constant.ChannelTypeTaskPlugin, Name: "multi", Key: "unready-test-key\nlow-test-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: "doubao-seedance-2-0-260128", BaseURL: &baseURL, Priority: &priority, ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	setSeedanceReadyChannelSetting(t, multi, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: "https://assets.example.test"})
	require.NoError(t, multi.Insert())
	require.NoError(t, fixture.db.Create(&model.SeedanceAssetGroupReplica{UserID: 1, LocalGroupID: fixture.group.ID, ChannelID: multi.Id, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(multi, "doubao-seedance-2-0-260128", fixture.asset.KeyFingerprint), KeyFingerprint: fixture.asset.KeyFingerprint, UpstreamGroupID: "group-multi", GroupName: fixture.group.Name, Status: model.SeedanceAssetReplicaReady}).Error)
	require.NoError(t, fixture.db.Create(&model.SeedanceAssetReplica{UserID: 1, LocalGroupID: fixture.group.ID, LocalAssetID: fixture.asset.ID, ChannelID: multi.Id, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(multi, "doubao-seedance-2-0-260128", fixture.asset.KeyFingerprint), KeyFingerprint: fixture.asset.KeyFingerprint, UpstreamAssetID: "asset-multi", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active"}).Error)
	response = fixture.submit()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "104", response.Header().Get("X-Test-Channel"))
	require.Equal(t, "low-test-key", response.Header().Get("X-Test-Key"))

	other := &model.Channel{Id: 105, Type: constant.ChannelTypeTaskPlugin, Name: "other-group", Key: "other-test-key", Status: common.ChannelStatusEnabled, Group: "aaa", Models: "doubao-seedance-2-0-260128", BaseURL: &baseURL, Priority: &priority}
	setSeedanceReadyChannelSetting(t, other, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: "https://assets.example.test"})
	require.NoError(t, other.Insert())
	response = fixture.submitWith(`{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-test"}}]}`, "aaa", "")
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("X-Test-Channel"))
	response = fixture.submitWith(`{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-test"}}]}`, "aaa", "102")
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.Empty(t, response.Header().Get("X-Test-Channel"))
}

func TestSeedanceModelModifierReadyFallbackMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	const baseModel = "doubao-seedance-2-0-260128"
	const requested = baseModel + "@temperature:0.2"
	const forbidden = baseModel + "@temperature:0.3"
	require.NoError(t, fixture.db.Model(fixture.high).Update("status", common.ChannelStatusManuallyDisabled).Error)
	var low model.Channel
	require.NoError(t, fixture.db.First(&low, 102).Error)
	low.Models = strings.Join([]string{baseModel, requested, forbidden}, ",")
	require.NoError(t, low.Update())
	key := fmt.Sprintf("%x", common.Sha256Raw([]byte(low.Key)))
	exactAccount := model.SeedanceAssetModelScopeFingerprint(&low, requested, key)
	otherAccount := model.SeedanceAssetModelScopeFingerprint(&low, forbidden, key)
	exactReplica := &model.SeedanceAssetReplica{
		UserID: 1, LocalGroupID: fixture.group.ID, LocalAssetID: fixture.asset.ID, ChannelID: low.Id,
		AccountFingerprint: exactAccount, KeyFingerprint: key, SyncScope: `{"version":1,"groups":["vip"],"models":["obsolete"],"pinned_channel":0}`,
		ProvisionStatus: model.SeedanceAssetReplicaCreating, Status: "Pending", NextPollAt: time.Now().Unix() + 60,
	}
	require.NoError(t, fixture.db.Create(exactReplica).Error)
	body := `{"model":"` + requested + `","content":[{"type":"image_url","image_url":{"url":"asset://asset-test"}}]}`
	for _, memoryCache := range []bool{false, true} {
		common.MemoryCacheEnabled = memoryCache
		if memoryCache {
			model.InitChannelCache()
		}
		response := fixture.submitWith(body, "vip", "", requested)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, "102", response.Header().Get("X-Test-Channel"))
		require.Equal(t, "asset-test", response.Header().Get("X-Test-Mapped-Asset"))
	}
	var count int64
	require.NoError(t, fixture.db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ? AND account_fingerprint = ?", fixture.asset.ID, otherAccount).Count(&count).Error)
	require.Zero(t, count, "an unauthorized modifier must not acquire a synchronization target")
	response := fixture.submitWith(strings.Replace(body, requested, forbidden, 1), "vip", "", requested)
	require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	require.NoError(t, fixture.db.First(exactReplica, exactReplica.ID).Error)
	authorizedScope, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{requested}, 0)
	require.NoError(t, err)
	require.Equal(t, authorizedScope, exactReplica.SyncScope)
	scopes, err := service.SeedanceAssetModelScopes(t.Context(), 1, []string{"vip"}, []string{requested}, 0)
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	require.ElementsMatch(t, []string{fixture.asset.AccountFingerprint, exactAccount}, scopes[baseModel])
	require.NoError(t, fixture.db.Create(&model.SeedanceAssetGroupReplica{UserID: 1, LocalGroupID: fixture.group.ID, ChannelID: low.Id, AccountFingerprint: exactAccount, KeyFingerprint: key, UpstreamGroupID: "group-exact", GroupName: fixture.group.Name, Status: model.SeedanceAssetReplicaReady}).Error)
	require.NoError(t, fixture.db.Model(exactReplica).Updates(map[string]any{"provision_status": model.SeedanceAssetReplicaReady, "status": "Active", "upstream_asset_id": "asset-exact", "upstream_group_id": "group-exact", "next_poll_at": 0}).Error)
	for _, memoryCache := range []bool{false, true} {
		common.MemoryCacheEnabled = memoryCache
		if memoryCache {
			model.InitChannelCache()
		}
		response := fixture.submitWith(body, "vip", "", requested)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, "102", response.Header().Get("X-Test-Channel"))
		require.Equal(t, "asset-exact", response.Header().Get("X-Test-Mapped-Asset"))
	}
	low.Models = baseModel
	require.NoError(t, low.Update())
	common.MemoryCacheEnabled = true
	model.InitChannelCache()
	low.Models = strings.Join([]string{baseModel, requested, forbidden}, ",")
	require.NoError(t, low.Update())
	response = fixture.submitWith(body, "vip", "", requested)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "asset-test", response.Header().Get("X-Test-Mapped-Asset"), "the mapping must follow the ability actually selected from cache")
	for _, request := range []struct{ group, pin string }{{"vip", "102"}, {"auto", ""}, {"auto", "102"}} {
		response = fixture.submitWith(body, request.group, request.pin, requested)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, "102", response.Header().Get("X-Test-Channel"))
		require.Equal(t, "asset-test", response.Header().Get("X-Test-Mapped-Asset"))
	}
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id = ?", low.Id).Update("status", common.ChannelStatusManuallyDisabled).Error)
	response = fixture.submitWith(body, "vip", "", requested)
	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String(), "a disabled database channel must not be used from stale cache")
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id = ?", low.Id).Update("status", common.ChannelStatusEnabled).Error)
	model.InitChannelCache()
	oldSettings := low.GetSetting()
	newSettings := oldSettings
	newSettings.SeedanceAssetURL = "https://changed-assets.example.test"
	setSeedanceReadyChannelSetting(t, &low, newSettings)
	require.NoError(t, low.Update())
	changedAccount := model.SeedanceAssetModelScopeFingerprint(&low, requested, key)
	require.NoError(t, fixture.db.Create(&model.SeedanceAssetReplica{UserID: 1, LocalGroupID: fixture.group.ID, LocalAssetID: fixture.asset.ID, ChannelID: low.Id, AccountFingerprint: changedAccount, KeyFingerprint: key, UpstreamAssetID: "asset-changed", UpstreamGroupID: "group-changed", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active"}).Error)
	response = fixture.submitWith(body, "vip", "", requested)
	require.Equal(t, http.StatusConflict, response.Code, response.Body.String(), "stale channel settings must not map a different account")
	setSeedanceReadyChannelSetting(t, &low, oldSettings)
	require.NoError(t, low.Update())
	fixture.high.Status = common.ChannelStatusEnabled
	require.NoError(t, fixture.high.Update())
	require.NoError(t, fixture.db.Model(fixture.replica).Updates(map[string]any{"provision_status": model.SeedanceAssetReplicaReady, "status": "Active", "upstream_asset_id": "asset-high", "upstream_group_id": "group-high", "next_poll_at": 0}).Error)
	for _, memoryCache := range []bool{false, true} {
		common.MemoryCacheEnabled = memoryCache
		if memoryCache {
			model.InitChannelCache()
		}
		response := fixture.submitWith(body, "vip", "", requested)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, "102", response.Header().Get("X-Test-Channel"), "a ready exact ability takes precedence over a higher-priority base ability")
		require.Equal(t, "asset-exact", response.Header().Get("X-Test-Mapped-Asset"))
	}
	low.Key = "low-test-key\nsecond-test-key"
	low.ChannelInfo.IsMultiKey = true
	require.NoError(t, low.Update())
	require.NoError(t, fixture.db.Model(exactReplica).Updates(map[string]any{"provision_status": model.SeedanceAssetReplicaCreating, "status": "Pending", "upstream_asset_id": "", "next_poll_at": time.Now().Unix() + 60}).Error)
	secondKey := fmt.Sprintf("%x", common.Sha256Raw([]byte("second-test-key")))
	secondAccount := model.SeedanceAssetModelScopeFingerprint(&low, requested, secondKey)
	require.NoError(t, fixture.db.Create(&model.SeedanceAssetGroupReplica{UserID: 1, LocalGroupID: fixture.group.ID, ChannelID: low.Id, AccountFingerprint: secondAccount, KeyFingerprint: secondKey, UpstreamGroupID: "group-exact-second", GroupName: fixture.group.Name, Status: model.SeedanceAssetReplicaReady}).Error)
	require.NoError(t, fixture.db.Create(&model.SeedanceAssetReplica{UserID: 1, LocalGroupID: fixture.group.ID, LocalAssetID: fixture.asset.ID, ChannelID: low.Id, AccountFingerprint: secondAccount, KeyFingerprint: secondKey, UpstreamAssetID: "asset-exact-second", UpstreamGroupID: "group-exact-second", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active"}).Error)
	for _, memoryCache := range []bool{false, true} {
		common.MemoryCacheEnabled = memoryCache
		if memoryCache {
			model.InitChannelCache()
		}
		response := fixture.submitWith(body, "vip", "", requested)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Equal(t, "102", response.Header().Get("X-Test-Channel"))
		require.Equal(t, "second-test-key", response.Header().Get("X-Test-Key"))
		require.Equal(t, "asset-exact-second", response.Header().Get("X-Test-Mapped-Asset"))
	}
}

func TestOriginTaskPinRespectsTokenModelAndGroupMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	const modelName = "doubao-seedance-2-0-260128"
	for _, tc := range []struct {
		name, group, allowed string
		want                 int
	}{
		{"forbidden model", "vip", modelName + "@temperature:0.2", http.StatusForbidden},
		{"forbidden group", "aaa", modelName, http.StatusForbidden},
		{"allowed", "vip", modelName, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"`+modelName+`"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set("id", 1)
			c.Set("token_id", 1)
			c.Set("expected_task_plugin_key", "doubao")
			common.SetContextKey(c, constant.ContextKeyUsingGroup, tc.group)
			common.SetContextKey(c, constant.ContextKeyUserGroup, "vip")
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{tc.allowed: true})
			service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: fixture.high.Id, Source: dto.PinSourceOriginTask, Rank: dto.PinRankOriginTask})
			Distribute()(c)
			common.CleanupBodyStorage(c)
			require.Equal(t, tc.want, recorder.Code, recorder.Body.String())
		})
	}
}

func TestSeedanceAsyncAccountSynchronizationMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	var highReady atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"group-%s"}}`, key)
		case "CreateAsset":
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"asset-%s"}}`, key)
		case "GetAsset":
			status := "Active"
			if key == "high-async-key" && !highReady.Load() {
				status = "Processing"
			}
			_, _ = fmt.Fprintf(w, `{"Result":{"Status":%q}}`, status)
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	highPriority, lowPriority := int64(10), int64(0)
	const modelName = "doubao-seedance-2-0-260128"
	high := &model.Channel{Id: 201, Type: constant.ChannelTypeTaskPlugin, Name: "async-high", Key: "high-async-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL, Priority: &highPriority}
	low := &model.Channel{Id: 202, Type: constant.ChannelTypeTaskPlugin, Name: "async-low", Key: "low-async-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL, Priority: &lowPriority}
	for _, channel := range []*model.Channel{high, low} {
		setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL})
		require.NoError(t, channel.Insert())
	}
	lowKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(low.Key)))
	group := &model.SeedanceAssetGroup{UserID: 1, ChannelID: low.Id, GroupID: "group-async", Name: "async", GroupType: "AIGC", Status: "Active", KeyFingerprint: lowKey, AccountFingerprint: model.SeedanceAssetAccountFingerprint(low, lowKey)}
	require.NoError(t, fixture.db.Create(group).Error)
	localOnly := false
	asset := &model.SeedanceAsset{UserID: 1, ChannelID: low.Id, GroupID: group.GroupID, AssetID: "asset-async", Name: "async", AssetType: "Image", Status: "Processing", SourceURL: "https://8.8.8.8/fixture.png", KeyFingerprint: lowKey, AccountFingerprint: group.AccountFingerprint, LocalOnly: &localOnly}
	require.NoError(t, service.CreateSeedanceAssetWithSynchronization(t.Context(), asset, []string{"vip"}, nil))
	require.Empty(t, asset.UpstreamAssetID())
	replicas, err := model.FindSeedanceAssetReplicas(t.Context(), 1, asset.ID)
	require.NoError(t, err)
	require.Len(t, replicas, 2)
	for _, replica := range replicas {
		require.Equal(t, "Pending", replica.Status)
	}
	summary, err := service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, 2, summary.Candidates)
	require.NoError(t, fixture.db.First(asset, asset.ID).Error)
	require.Equal(t, "Active", asset.Status)
	response := fixture.submitWith(`{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-async"}}]}`, "vip", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "202", response.Header().Get("X-Test-Channel"))

	highReady.Store(true)
	require.NoError(t, fixture.db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ? AND channel_id = ?", asset.ID, high.Id).Update("next_poll_at", common.GetTimestamp()).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	response = fixture.submitWith(`{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-async"}}]}`, "vip", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, "201", response.Header().Get("X-Test-Channel"))

	require.NoError(t, fixture.db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ?", asset.ID).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaFailed, "status": "Pending", "upstream_asset_id": "", "next_poll_at": 0, "last_error": "temporary fixture error",
	}).Error)
	require.NoError(t, model.RefreshReplicaManagedSeedanceAsset(t.Context(), asset.UserID, asset.ID))
	require.NoError(t, fixture.db.First(asset, asset.ID).Error)
	require.Equal(t, "Failed", asset.Status)
	require.NoError(t, service.RetrySeedanceAssetSynchronization(t.Context(), asset.UserID, asset.ID, []string{"vip"}, nil))
	require.NoError(t, fixture.db.First(asset, asset.ID).Error)
	require.Equal(t, "Processing", asset.Status)
	_, err = service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, fixture.db.First(asset, asset.ID).Error)
	require.Equal(t, "Active", asset.Status)
}

func TestSeedanceSlowAccountDoesNotBlockOtherAccountsMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	highStarted, lowStarted, releaseHigh := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHigh) }) }
	defer release()
	var highCreates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"group-%s"}}`, key)
		case "CreateAsset":
			if key == "gate-high" {
				if highCreates.Add(1) == 1 {
					close(highStarted)
				}
				<-releaseHigh
			} else {
				close(lowStarted)
			}
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"asset-%s"}}`, key)
		case "GetAsset":
			_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	const modelName = "doubao-seedance-2-0-260128"
	high := &model.Channel{Id: 301, Type: constant.ChannelTypeTaskPlugin, Name: "gate-high", Key: "gate-high", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	low := &model.Channel{Id: 302, Type: constant.ChannelTypeTaskPlugin, Name: "gate-low", Key: "gate-low", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	for _, channel := range []*model.Channel{high, low} {
		setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL})
		require.NoError(t, channel.Insert())
	}
	lowKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(low.Key)))
	group := &model.SeedanceAssetGroup{UserID: 1, ChannelID: low.Id, GroupID: "group-gate", Name: "gate", GroupType: "AIGC", Status: "Active", KeyFingerprint: lowKey, AccountFingerprint: model.SeedanceAssetAccountFingerprint(low, lowKey)}
	require.NoError(t, fixture.db.Create(group).Error)
	targets := make([]model.SeedanceAssetReplica, 0, 5)
	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, nil, 0)
	require.NoError(t, err)
	localOnly := false
	for index := range 5 {
		asset := &model.SeedanceAsset{UserID: 1, ChannelID: low.Id, GroupID: group.GroupID, AssetID: fmt.Sprintf("asset-gate-%d", index), Name: "gate", AssetType: "Image", Status: "Processing", SourceURL: "https://8.8.8.8/fixture.png", KeyFingerprint: lowKey, AccountFingerprint: group.AccountFingerprint, LocalOnly: &localOnly}
		require.NoError(t, fixture.db.Create(asset).Error)
		target := high
		if index == 4 {
			target = low
		}
		key := fmt.Sprintf("%x", common.Sha256Raw([]byte(target.Key)))
		targets = append(targets, model.SeedanceAssetReplica{UserID: 1, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: target.Id, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(target, modelName, key), KeyFingerprint: key, SyncScope: scope, ProvisionStatus: model.SeedanceAssetReplicaCreating, Status: "Pending", NextPollAt: common.GetTimestamp()})
	}
	require.NoError(t, model.ScheduleSeedanceAssetSyncTargets(t.Context(), 1, targets[:4]))
	type outcome struct {
		summary service.SeedanceAssetPollSummary
		err     error
	}
	completed := make(chan outcome, 1)
	go func() {
		summary, err := service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
		completed <- outcome{summary: summary, err: err}
	}()
	select {
	case <-highStarted:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("slow account did not start")
	}
	require.NoError(t, model.ScheduleSeedanceAssetSyncTargets(t.Context(), 1, targets[4:]))
	select {
	case <-lowStarted:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("slow account blocked another account")
	}
	require.EqualValues(t, 1, highCreates.Load())
	release()
	select {
	case result := <-completed:
		require.NoError(t, result.err)
		require.Equal(t, 5, result.summary.Candidates)
		require.Equal(t, 5, result.summary.Updated)
	case <-time.After(5 * time.Second):
		t.Fatal("synchronization did not complete")
	}
}

func TestSeedanceScopeUpgradeMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	var before model.SeedanceAssetReplica
	require.NoError(t, fixture.db.First(&before, fixture.replica.ID).Error)
	require.NoError(t, fixture.db.Migrator().DropColumn(&model.SeedanceAssetReplica{}, "sync_scope"))
	require.NoError(t, fixture.db.Migrator().DropColumn(&model.SeedanceAsset{}, "primary_sync_scope"))
	require.NoError(t, fixture.db.Migrator().DropTable(&model.SeedanceAssetDiscovery{}))
	for range 2 {
		func() {
			require.NoError(t, model.InitDB())
			migrated := model.DB
			connection, err := migrated.DB()
			require.NoError(t, err)
			defer func() {
				require.NoError(t, connection.Close())
				model.DB = fixture.db
			}()
			require.True(t, migrated.Migrator().HasColumn(&model.SeedanceAssetReplica{}, "sync_scope"))
			require.True(t, migrated.Migrator().HasColumn(&model.SeedanceAsset{}, "primary_sync_scope"))
			require.True(t, migrated.Migrator().HasTable(&model.SeedanceAssetDiscovery{}))
			require.True(t, migrated.Migrator().HasIndex(&model.SeedanceAssetReplica{}, "idx_seedance_asset_replica_account"))
			var after model.SeedanceAssetReplica
			require.NoError(t, migrated.First(&after, before.ID).Error)
			require.Equal(t, before.UserID, after.UserID)
			require.Equal(t, before.ChannelID, after.ChannelID)
			require.Equal(t, before.AccountFingerprint, after.AccountFingerprint)
			require.Equal(t, before.Status, after.Status)
			require.Empty(t, after.SyncScope)
			var existing model.SeedanceAsset
			require.NoError(t, migrated.First(&existing, fixture.asset.ID).Error)
			require.Equal(t, fixture.asset.AssetID, existing.AssetID)
			require.Empty(t, existing.PrimarySyncScope)
		}()
	}
}

func TestSeedanceSharedSyncRespectsTokenGroupMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("Action") {
		case "CreateAsset":
			_, _ = io.WriteString(w, `{"Result":{"Id":"asset-shared"}}`)
		case "GetAsset":
			_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	const modelName, otherModel = "doubao-seedance-2-0-260128", "doubao-seedance-2-5-260628"
	vip := &model.Channel{Id: 601, Type: constant.ChannelTypeTaskPlugin, Name: "vip-shared", Key: "vip-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName + "," + otherModel, BaseURL: &upstream.URL}
	aaa := &model.Channel{Id: 602, Type: constant.ChannelTypeTaskPlugin, Name: "aaa-shared", Key: "aaa-key", Status: common.ChannelStatusEnabled, Group: "aaa", Models: modelName, BaseURL: &upstream.URL}
	vipAlt := &model.Channel{Id: 603, Type: constant.ChannelTypeTaskPlugin, Name: "vip-alternate", Key: "vip-alt-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	for _, channel := range []*model.Channel{vip, aaa, vipAlt} {
		setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL, SeedanceAssetShareGroup: "shared"})
		require.NoError(t, channel.Insert())
	}
	key := fmt.Sprintf("%x", common.Sha256Raw([]byte(vip.Key)))
	group := &model.SeedanceAssetGroup{UserID: 1, ChannelID: vip.Id, GroupID: "group-shared", Name: "shared", GroupType: "AIGC", Status: "Active", KeyFingerprint: key, AccountFingerprint: model.SeedanceAssetAccountFingerprint(vip, key)}
	require.NoError(t, fixture.db.Create(group).Error)
	localOnly := false
	asset := &model.SeedanceAsset{UserID: 1, ChannelID: vip.Id, GroupID: group.GroupID, AssetID: "asset-shared-local", Name: "shared", AssetType: "Image", Status: "Processing", SourceURL: "https://8.8.8.8/fixture.png", KeyFingerprint: key, AccountFingerprint: group.AccountFingerprint, LocalOnly: &localOnly}
	require.NoError(t, service.CreateSeedanceAssetWithSynchronization(t.Context(), asset, []string{"vip"}, []string{modelName}, vip.Id))
	var original model.SeedanceAssetReplica
	require.NoError(t, fixture.db.Where("local_asset_id = ?", asset.ID).First(&original).Error)
	originalScope := original.SyncScope
	require.NoError(t, fixture.db.Model(&original).Update("sync_scope", `{"version":1}`).Error)
	_, err := service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, upstreamCalls.Load(), "an incomplete persisted scope must fail closed")
	require.NoError(t, fixture.db.Model(&original).Updates(map[string]any{"sync_scope": originalScope, "status": "Pending", "provision_status": model.SeedanceAssetReplicaCreating, "next_poll_at": common.GetTimestamp(), "poll_attempts": 0, "last_error": ""}).Error)
	require.NoError(t, fixture.db.Where("channel_id = ? AND model = ?", vip.Id, modelName).Delete(&model.Ability{}).Error)
	require.NoError(t, fixture.db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ?", asset.ID).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaReady, "status": "Pending", "upstream_asset_id": "asset-shared", "next_poll_at": common.GetTimestamp(),
	}).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, upstreamCalls.Load(), "the pinned vip request must not use an unauthorized model or channel")
	require.NoError(t, fixture.db.Model(vip).Updates(map[string]any{"status": common.ChannelStatusEnabled, "group": "aaa"}).Error)
	require.NoError(t, fixture.db.Model(&model.Ability{}).Where("channel_id = ?", vip.Id).Update("group", "aaa").Error)
	require.NoError(t, fixture.db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ?", asset.ID).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaCreating, "status": "Pending", "upstream_asset_id": "", "next_poll_at": common.GetTimestamp(), "poll_attempts": 0, "poll_lease_until": 0, "last_error": "",
	}).Error)
	_, err = service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, upstreamCalls.Load(), "the vip request must not upload after its channel moves to aaa")

	require.NoError(t, service.EnsureSeedanceAssetSynchronization(t.Context(), 1, []model.SeedanceAsset{*asset}, []string{"aaa"}, nil, 0))
	require.NoError(t, service.RetrySeedanceAssetSynchronization(t.Context(), 1, asset.ID, []string{"aaa"}, nil))
	_, err = service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Greater(t, upstreamCalls.Load(), int32(0), "a later aaa-authorized request can restore the shared target")
	require.NoError(t, fixture.db.First(asset, asset.ID).Error)
	require.Equal(t, "Active", asset.Status)
}

func TestSeedanceModelScopeIgnoresUnrelatedMappingChanges(t *testing.T) {
	baseURL := "https://video.example.test"
	mapping := `{"video-alias":"intermediate","intermediate":"doubao-seedance-2-0-260128","other":"first"}`
	channel := &model.Channel{Id: 501, Type: constant.ChannelTypeTaskPlugin, BaseURL: &baseURL, ModelMapping: &mapping}
	setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: "https://assets.example.test"})
	const alias, key = "video-alias", "key-fingerprint"
	original := model.SeedanceAssetModelScopeFingerprint(channel, alias, key)
	require.NotEmpty(t, original)
	updated := `{"other":"second","intermediate":"doubao-seedance-2-0-260128","video-alias":"intermediate"}`
	channel.ModelMapping = &updated
	require.Equal(t, original, model.SeedanceAssetModelScopeFingerprint(channel, alias, key), "unrelated mappings must not invalidate an existing asset")
	updated = `{"video-alias":"intermediate","intermediate":"doubao-seedance-2-5-260628","other":"second"}`
	require.NotEqual(t, original, model.SeedanceAssetModelScopeFingerprint(channel, alias, key), "the actual upstream model changed")
	require.NotEqual(t, original, model.SeedanceAssetModelScopeFingerprint(channel, alias, "other-key"))
}

func TestSeedanceIndependentModelScopesSyncConcurrentlyMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	firstStarted, secondStarted, releaseFirst := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer release()
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			_, _ = io.WriteString(w, `{"Result":{"Id":"group-concurrent"}}`)
		case "CreateAsset":
			ordinal := creates.Add(1)
			if ordinal == 1 {
				close(firstStarted)
				<-releaseFirst
			} else if ordinal == 2 {
				close(secondStarted)
			}
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"asset-concurrent-%d"}}`, ordinal)
		case "GetAsset":
			_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	const firstModel, secondModel = "doubao-seedance-2-0-260128", "doubao-seedance-2-5-260628"
	channel := &model.Channel{Id: 501, Type: constant.ChannelTypeTaskPlugin, Name: "same-account", Key: "same-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: firstModel + "," + secondModel, BaseURL: &upstream.URL}
	setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL})
	require.NoError(t, channel.Insert())
	key := fmt.Sprintf("%x", common.Sha256Raw([]byte(channel.Key)))
	group := &model.SeedanceAssetGroup{UserID: 1, ChannelID: channel.Id, GroupID: "group-independent", Name: "independent", GroupType: "AIGC", Status: "Active", KeyFingerprint: key, AccountFingerprint: model.SeedanceAssetAccountFingerprint(channel, key)}
	require.NoError(t, fixture.db.Create(group).Error)
	localOnly := false
	asset := &model.SeedanceAsset{UserID: 1, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "asset-independent", Name: "independent", AssetType: "Image", Status: "Processing", SourceURL: "https://8.8.8.8/fixture.png", KeyFingerprint: key, AccountFingerprint: group.AccountFingerprint, LocalOnly: &localOnly}
	require.NoError(t, service.CreateSeedanceAssetWithSynchronization(t.Context(), asset, []string{"vip"}, nil))
	completed := make(chan error, 1)
	go func() {
		_, err := service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
		completed <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first model did not start synchronization")
	}
	select {
	case <-secondStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("one model blocked another independent model")
	}
	release()
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("synchronization did not complete")
	}
}

func TestSeedanceDeleteDuringSyncCleansLateUpstreamAssetMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	started, releaseRequest := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRequest) }) }
	defer release()
	var deletes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("Action") {
		case "CreateAsset":
			close(started)
			<-releaseRequest
			_, _ = io.WriteString(w, `{"Result":{"Id":"late-upstream-asset"}}`)
		case "DeleteAsset":
			deletes.Add(1)
			_, _ = io.WriteString(w, `{"Result":{}}`)
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	const modelName = "doubao-seedance-2-0-260128"
	channel := &model.Channel{Id: 701, Type: constant.ChannelTypeTaskPlugin, Name: "late-delete", Key: "late-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL, SeedanceAssetShareGroup: "late-delete"})
	require.NoError(t, channel.Insert())
	key := fmt.Sprintf("%x", common.Sha256Raw([]byte(channel.Key)))
	group := &model.SeedanceAssetGroup{UserID: 1, ChannelID: channel.Id, GroupID: "group-late", Name: "late", GroupType: "AIGC", Status: "Active", KeyFingerprint: key, AccountFingerprint: model.SeedanceAssetAccountFingerprint(channel, key)}
	require.NoError(t, fixture.db.Create(group).Error)
	localOnly := false
	asset := &model.SeedanceAsset{UserID: 1, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "asset-late-local", Name: "late", AssetType: "Image", Status: "Processing", SourceURL: "https://8.8.8.8/fixture.png", KeyFingerprint: key, AccountFingerprint: group.AccountFingerprint, LocalOnly: &localOnly}
	require.NoError(t, service.CreateSeedanceAssetWithSynchronization(t.Context(), asset, []string{"vip"}, nil))
	completed := make(chan error, 1)
	go func() {
		_, err := service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
		completed <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream creation did not start")
	}
	replicas, err := model.FindSeedanceAssetReplicas(t.Context(), 1, asset.ID)
	require.NoError(t, err)
	require.NoError(t, model.QueueSeedanceAssetDeletion(t.Context(), service.NewSeedanceAssetDeleteCleanupJobs(asset, replicas)))
	_, err = service.RunSeedanceAssetCleanupOnce(t.Context(), nil)
	require.NoError(t, err)
	var count int64
	require.NoError(t, fixture.db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&count).Error)
	require.Zero(t, count)
	release()
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight synchronization did not complete")
	}
	require.EqualValues(t, 1, deletes.Load(), "the late upstream asset must be rolled back")
	require.NoError(t, fixture.db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ?", asset.ID).Count(&count).Error)
	require.Zero(t, count)
}

func TestSeedancePrimaryReviewScopeMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	var originQueries, foreignQueries, allowedQueries atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") != "GetAsset" {
			http.Error(w, "unexpected action", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") == "Bearer foreign-key" {
			foreignQueries.Add(1)
		}
		if r.Header.Get("Authorization") == "Bearer origin-key" {
			originQueries.Add(1)
		}
		if r.Header.Get("Authorization") == "Bearer allowed-key" {
			allowedQueries.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Result":{"Id":"primary-upstream","Status":"Active"}}`)
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	const modelName = "doubao-seedance-2-0-260128"
	origin := &model.Channel{Id: 801, Type: constant.ChannelTypeTaskPlugin, Name: "primary-origin", Key: "origin-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	foreign := &model.Channel{Id: 802, Type: constant.ChannelTypeTaskPlugin, Name: "primary-foreign", Key: "foreign-key", Status: common.ChannelStatusEnabled, Group: "aaa", Models: modelName, BaseURL: &upstream.URL}
	for _, channel := range []*model.Channel{origin, foreign} {
		setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL, SeedanceAssetShareGroup: "primary-review"})
		require.NoError(t, channel.Insert())
	}
	key := fmt.Sprintf("%x", common.Sha256Raw([]byte(origin.Key)))
	localOnly := false
	asset := &model.SeedanceAsset{
		UserID: 1, ChannelID: origin.Id, GroupID: fixture.group.GroupID, AssetID: "primary-local", PrimaryUpstreamID: "primary-upstream",
		Name: "primary", AssetType: "Image", LocalOnly: &localOnly, KeyFingerprint: key,
		AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(origin, modelName, key), Status: "Processing", NextPollAt: common.GetTimestamp(),
	}
	require.NoError(t, fixture.db.Create(asset).Error)
	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{modelName}, 0)
	require.NoError(t, err)
	require.True(t, fixture.db.Migrator().HasColumn(&model.SeedanceAsset{}, "primary_sync_scope"))
	require.NoError(t, fixture.db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).UpdateColumn("primary_sync_scope", scope).Error)
	require.NoError(t, fixture.db.Model(origin).Update("status", common.ChannelStatusManuallyDisabled).Error)
	summary, err := service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Positive(t, summary.Claimed)
	require.Zero(t, foreignQueries.Load(), "primary review must not query a channel outside the initiating scope")
	var current model.SeedanceAsset
	require.NoError(t, fixture.db.First(&current, asset.ID).Error)
	require.Equal(t, "Processing", current.Status)

	priority := int64(100)
	allowed := &model.Channel{Id: 803, Type: constant.ChannelTypeTaskPlugin, Name: "primary-allowed", Key: "allowed-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL, Priority: &priority}
	setSeedanceReadyChannelSetting(t, allowed, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL, SeedanceAssetShareGroup: "primary-review"})
	require.NoError(t, allowed.Insert())
	require.NoError(t, fixture.db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).UpdateColumn("next_poll_at", common.GetTimestamp()).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, allowedQueries.Load())
	require.Zero(t, foreignQueries.Load())
	require.NoError(t, fixture.db.First(&current, asset.ID).Error)
	require.Equal(t, "Active", current.Status)

	pinned, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{modelName}, origin.Id)
	require.NoError(t, err)
	for _, modelScoped := range []bool{false, true} {
		_, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, asset.AccountFingerprint, pinned, modelScoped)
		require.ErrorIs(t, err, service.ErrSeedanceAssetScopeUnavailable)
	}
	otherModel, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{"doubao-seedance-other"}, 0)
	require.NoError(t, err)
	for _, modelScoped := range []bool{false, true} {
		_, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, asset.AccountFingerprint, otherModel, modelScoped)
		require.ErrorIs(t, err, service.ErrSeedanceAssetScopeUnavailable)
	}
	noModels, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{}, 0)
	require.NoError(t, err)
	_, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, asset.AccountFingerprint, noModels, false)
	require.ErrorIs(t, err, service.ErrSeedanceAssetScopeUnavailable)
	_, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, asset.AccountFingerprint, `{"version":1}`, true)
	require.ErrorIs(t, err, service.ErrSeedanceAssetScopeUnavailable)

	humanAccount := model.SeedanceAssetAccountFingerprint(origin, key)
	human, err := service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, humanAccount, scope, false)
	require.NoError(t, err)
	require.Equal(t, allowed.Id, human.ChannelID)
	require.Equal(t, humanAccount, human.AccountFingerprint)
	human, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, "", scope, false)
	require.NoError(t, err)
	require.Equal(t, allowed.Id, human.ChannelID)

	require.NoError(t, fixture.db.Model(origin).Update("status", common.ChannelStatusEnabled).Error)
	for _, modelScoped := range []bool{false, true} {
		resolved, err := service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, humanAccount, scope, modelScoped)
		require.NoError(t, err)
		require.Equal(t, origin.Id, resolved.ChannelID)
		require.Equal(t, key, resolved.KeyFingerprint)
	}
	require.NoError(t, fixture.db.Model(&model.User{}).Where("id = ?", 1).Update("status", common.UserStatusDisabled).Error)
	_, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, humanAccount, scope, false)
	require.ErrorIs(t, err, service.ErrSeedanceAssetScopeUnavailable)
	require.NoError(t, fixture.db.Model(&model.User{}).Where("id = ?", 1).Update("status", common.UserStatusEnabled).Error)
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"aaa":"AAA"}`))
	require.NoError(t, fixture.db.Model(&model.User{}).Where("id = ?", 1).Update("group", "aaa").Error)
	_, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, origin.Id, key, humanAccount, scope, false)
	require.ErrorIs(t, err, service.ErrSeedanceAssetScopeUnavailable)
	require.NoError(t, fixture.db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Updates(map[string]any{
		"status": "Processing", "status_key": model.SeedanceAssetStatusIndexKey("Processing"), "next_poll_at": common.GetTimestamp(),
		"poll_attempts": 0, "poll_lease_until": 0,
	}).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, originQueries.Load())
	require.EqualValues(t, 1, allowedQueries.Load())
	require.Zero(t, foreignQueries.Load())
	require.NoError(t, fixture.db.First(&current, asset.ID).Error)
	require.Equal(t, "Processing", current.Status)
	require.NoError(t, fixture.db.Model(&model.User{}).Where("id = ?", 1).Update("group", "vip").Error)
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"vip":"VIP","aaa":"AAA"}`))

	independent := &model.Channel{Id: 804, Type: constant.ChannelTypeTaskPlugin, Name: "independent-origin", Key: "independent-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	sameAccount := &model.Channel{Id: 805, Type: constant.ChannelTypeTaskPlugin, Name: "independent-alternate", Key: "independent-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	for _, channel := range []*model.Channel{independent, sameAccount} {
		setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL})
		require.NoError(t, channel.Insert())
	}
	independentKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(independent.Key)))
	modelAccount := model.SeedanceAssetModelScopeFingerprint(independent, modelName, independentKey)
	independentHuman := model.SeedanceAssetAccountFingerprint(independent, independentKey)
	require.NoError(t, fixture.db.Model(independent).Update("status", common.ChannelStatusManuallyDisabled).Error)
	_, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, independent.Id, independentKey, modelAccount, scope, true)
	require.ErrorIs(t, err, service.ErrSeedanceAssetScopeUnavailable)
	resolved, err := service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, independent.Id, independentKey, independentHuman, scope, false)
	require.NoError(t, err)
	require.Equal(t, sameAccount.Id, resolved.ChannelID)
	require.Equal(t, independentKey, resolved.KeyFingerprint)

	multi := &model.Channel{Id: 806, Type: constant.ChannelTypeTaskPlugin, Name: "multi-primary", Key: "first-key\nbound-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL,
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModeRandom}}
	setSeedanceReadyChannelSetting(t, multi, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL, SeedanceAssetShareGroup: "primary-review"})
	require.NoError(t, multi.Insert())
	boundKey := fmt.Sprintf("%x", common.Sha256Raw([]byte("bound-key")))
	for _, modelScoped := range []bool{false, true} {
		resolved, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, multi.Id, boundKey, humanAccount, scope, modelScoped)
		require.NoError(t, err)
		require.Equal(t, multi.Id, resolved.ChannelID)
		require.Equal(t, boundKey, resolved.KeyFingerprint)
	}
	multi.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
	require.NoError(t, fixture.db.Model(multi).Update("channel_info", multi.ChannelInfo).Error)
	resolved, err = service.ResolveSeedanceAssetClientWithinScope(t.Context(), 1, multi.Id, boundKey, humanAccount, scope, false)
	require.NoError(t, err)
	require.Equal(t, multi.Id, resolved.ChannelID)
	require.NotEqual(t, boundKey, resolved.KeyFingerprint)

	local := true
	link := &model.SeedanceAsset{UserID: 1, ChannelID: fixture.group.ChannelID, GroupID: fixture.group.GroupID, AssetID: "link-for-promotion", Name: "link", AssetType: "Image", LocalOnly: &local, Status: "Active"}
	require.NoError(t, fixture.db.Create(link).Error)
	require.NoError(t, model.PromoteLocalSeedanceAsset(t.Context(), 1, link.ID, link.GroupID, link.UpdatedAt, origin.Id, key, asset.AccountFingerprint, "promoted-upstream", scope))
	require.NoError(t, fixture.db.First(link, link.ID).Error)
	require.Equal(t, scope, link.PrimarySyncScope)
	require.Equal(t, "promoted-upstream", link.PrimaryUpstreamID)
	require.NotNil(t, link.LocalOnly)
	require.False(t, *link.LocalOnly)
}

func TestSeedanceHumanReviewScopeMySQL(t *testing.T) {
	fixture := newSeedanceReadyFixture(t)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	var originQueries, foreignQueries, allowedQueries atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") != "GetAsset" {
			http.Error(w, "unexpected action", http.StatusBadRequest)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer human-origin":
			originQueries.Add(1)
		case "Bearer human-foreign":
			foreignQueries.Add(1)
		case "Bearer human-allowed":
			allowedQueries.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
	}))
	t.Cleanup(upstream.Close)
	service.InitHttpClient()
	const modelName = "doubao-seedance-2-0-260128"
	origin := &model.Channel{Id: 811, Type: constant.ChannelTypeTaskPlugin, Name: "human-origin", Key: "human-origin", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	foreign := &model.Channel{Id: 812, Type: constant.ChannelTypeTaskPlugin, Name: "human-foreign", Key: "human-foreign", Status: common.ChannelStatusEnabled, Group: "aaa", Models: modelName, BaseURL: &upstream.URL}
	allowed := &model.Channel{Id: 813, Type: constant.ChannelTypeTaskPlugin, Name: "human-allowed", Key: "human-allowed", Status: common.ChannelStatusEnabled, Group: "vip", Models: modelName, BaseURL: &upstream.URL}
	for _, channel := range []*model.Channel{origin, foreign, allowed} {
		setSeedanceReadyChannelSetting(t, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: upstream.URL, SeedanceAssetShareGroup: "human-review"})
		require.NoError(t, channel.Insert())
	}
	key := fmt.Sprintf("%x", common.Sha256Raw([]byte(origin.Key)))
	account := model.SeedanceAssetAccountFingerprint(origin, key)
	group := &model.SeedanceAssetGroup{UserID: 1, ChannelID: origin.Id, GroupID: "human-review-group", Name: "human", GroupType: "LivenessFace", Status: "Active", KeyFingerprint: key, AccountFingerprint: account}
	require.NoError(t, fixture.db.Create(group).Error)
	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{modelName}, 0)
	require.NoError(t, err)
	asset := &model.SeedanceAsset{UserID: 1, ChannelID: origin.Id, GroupID: group.GroupID, AssetID: "human-review-asset", Name: "human", AssetType: "Image", KeyFingerprint: key, AccountFingerprint: account, PrimarySyncScope: scope, Status: "Processing", NextPollAt: common.GetTimestamp()}
	require.NoError(t, fixture.db.Create(asset).Error)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{origin.Id, allowed.Id}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, foreignQueries.Load())
	require.Zero(t, allowedQueries.Load())
	var current model.SeedanceAsset
	require.NoError(t, fixture.db.First(&current, asset.ID).Error)
	require.Equal(t, "Processing", current.Status)

	require.NoError(t, fixture.db.Model(allowed).Update("status", common.ChannelStatusEnabled).Error)
	require.NoError(t, fixture.db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).UpdateColumn("next_poll_at", common.GetTimestamp()).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, foreignQueries.Load())
	require.EqualValues(t, 1, allowedQueries.Load())
	require.NoError(t, fixture.db.First(&current, asset.ID).Error)
	require.Equal(t, "Active", current.Status)

	pinned, err := service.EncodeSeedanceAssetSyncScope([]string{"vip"}, []string{modelName}, origin.Id)
	require.NoError(t, err)
	pinnedAsset := &model.SeedanceAsset{UserID: 1, ChannelID: origin.Id, GroupID: group.GroupID, AssetID: "human-pinned-asset", Name: "human pinned", AssetType: "Image", KeyFingerprint: key, AccountFingerprint: account, PrimarySyncScope: pinned, Status: "Processing", NextPollAt: common.GetTimestamp()}
	require.NoError(t, fixture.db.Create(pinnedAsset).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, foreignQueries.Load())
	require.EqualValues(t, 1, allowedQueries.Load())
	current = model.SeedanceAsset{}
	require.NoError(t, fixture.db.First(&current, pinnedAsset.ID).Error)
	require.Equal(t, "Processing", current.Status)

	require.NoError(t, fixture.db.Model(allowed).Update("status", common.ChannelStatusManuallyDisabled).Error)
	legacy := &model.SeedanceAsset{UserID: 1, ChannelID: origin.Id, GroupID: group.GroupID, AssetID: "human-legacy-asset", Name: "human legacy", AssetType: "Image", KeyFingerprint: key, AccountFingerprint: account, Status: "Processing", NextPollAt: common.GetTimestamp()}
	require.NoError(t, fixture.db.Create(legacy).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, foreignQueries.Load(), "legacy human assets without a scope must not query an unauthorized shared channel")
	require.Zero(t, originQueries.Load())
	require.EqualValues(t, 1, allowedQueries.Load())
	current = model.SeedanceAsset{}
	require.NoError(t, fixture.db.First(&current, legacy.ID).Error)
	require.Equal(t, "Processing", current.Status)
	require.Positive(t, current.PollAttempts)
	require.Positive(t, current.NextPollAt)
	require.Zero(t, current.PollLeaseUntil)

	require.NoError(t, fixture.db.Model(&model.SeedanceAsset{}).Where("id = ?", pinnedAsset.ID).UpdateColumn("next_poll_at", common.GetTimestamp()+3600).Error)
	require.NoError(t, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{origin.Id, allowed.Id}).Update("status", common.ChannelStatusEnabled).Error)
	require.NoError(t, fixture.db.Model(&model.SeedanceAsset{}).Where("id = ?", legacy.ID).UpdateColumn("next_poll_at", common.GetTimestamp()).Error)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	require.Zero(t, originQueries.Load(), "legacy human assets without a scope must not query even the original channel")
	require.Zero(t, foreignQueries.Load())
	require.EqualValues(t, 1, allowedQueries.Load())
	current = model.SeedanceAsset{}
	require.NoError(t, fixture.db.First(&current, legacy.ID).Error)
	require.Equal(t, "Processing", current.Status)
}

func BenchmarkSeedanceAssetReadinessRouting(b *testing.B) {
	fixture := newSeedanceReadyFixture(b)
	const modelName = "doubao-seedance-2-0-260128"
	var low model.Channel
	require.NoError(b, fixture.db.First(&low, 102).Error)
	lowKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(low.Key)))
	highKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(fixture.high.Key)))
	lowScope := model.SeedanceAssetModelScopeFingerprint(&low, modelName, lowKey)
	highScope := model.SeedanceAssetModelScopeFingerprint(fixture.high, modelName, highKey)
	require.NoError(b, fixture.db.Model(fixture.replica).Update("account_fingerprint", highScope).Error)
	require.NoError(b, fixture.db.Model(&model.SeedanceAssetGroupReplica{}).
		Where("user_id = ? AND local_group_id = ? AND channel_id = ?", 1, fixture.group.ID, fixture.high.Id).
		Update("account_fingerprint", highScope).Error)
	require.NoError(b, fixture.db.Create(&model.SeedanceAssetGroupReplica{UserID: 1, LocalGroupID: fixture.group.ID, ChannelID: low.Id,
		AccountFingerprint: lowScope, KeyFingerprint: lowKey, UpstreamGroupID: "group-low", GroupName: fixture.group.Name, Status: model.SeedanceAssetReplicaReady}).Error)
	require.NoError(b, fixture.db.Create(&model.SeedanceAssetReplica{UserID: 1, LocalGroupID: fixture.group.ID, LocalAssetID: fixture.asset.ID, ChannelID: low.Id,
		AccountFingerprint: lowScope, KeyFingerprint: lowKey, UpstreamGroupID: "group-low", UpstreamAssetID: "asset-low", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active"}).Error)
	for _, scenario := range []struct {
		name          string
		ready         bool
		skipReadiness bool
	}{{"pending_with_readiness", false, false}, {"pending_without_readiness", false, true}, {"ready_with_readiness", true, false}, {"ready_without_readiness", true, true}} {
		b.Run(scenario.name, func(b *testing.B) {
			values := map[string]any{"provision_status": model.SeedanceAssetReplicaCreating, "status": "Pending", "upstream_asset_id": "", "next_poll_at": time.Now().Unix() + 60}
			if scenario.ready {
				values = map[string]any{"provision_status": model.SeedanceAssetReplicaReady, "status": "Active", "upstream_asset_id": "asset-high", "upstream_group_id": "group-high", "next_poll_at": 0}
			}
			require.NoError(b, fixture.db.Model(fixture.replica).Updates(values).Error)
			var success, conflict int
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				body := `{"model":"doubao-seedance-2-0-260128","content":[{"type":"image_url","image_url":{"url":"asset://asset-test"}}]}`
				request := httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				if scenario.skipReadiness {
					request.Header.Set("X-Test-Skip-Readiness", "1")
				}
				response := httptest.NewRecorder()
				fixture.engine.ServeHTTP(response, request)
				if response.Code == http.StatusOK {
					success++
				} else if response.Code == http.StatusConflict {
					conflict++
				} else {
					b.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
				}
			}
			if !scenario.ready && scenario.skipReadiness && conflict != b.N || (!scenario.skipReadiness || scenario.ready) && success != b.N {
				b.Fatalf("unexpected outcomes: success=%d conflict=%d", success, conflict)
			}
			b.ReportMetric(float64(success)/float64(b.N), "success/op")
			b.ReportMetric(float64(conflict)/float64(b.N), "conflict/op")
		})
	}
}

func BenchmarkSeedanceAssetSyncTargetsMySQL(b *testing.B) {
	fixture := newSeedanceReadyFixture(b)
	require.NoError(b, fixture.db.Model(&model.Channel{}).Where("id IN ?", []int{101, 102}).Update("status", common.ChannelStatusManuallyDisabled).Error)
	modelNames := []string{"doubao-seedance-2-0-260128", "doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615", "doubao-seedance-2-5-260628"}
	baseURL := "https://video.example.test"
	channels := []*model.Channel{
		{Id: 211, Type: constant.ChannelTypeTaskPlugin, Name: "benchmark-one", Key: "benchmark-fixture-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: strings.Join(modelNames, ","), BaseURL: &baseURL},
		{Id: 212, Type: constant.ChannelTypeTaskPlugin, Name: "benchmark-two", Key: "benchmark-fixture-key", Status: common.ChannelStatusEnabled, Group: "vip", Models: strings.Join(modelNames, ","), BaseURL: &baseURL},
	}
	for _, channel := range channels {
		setSeedanceReadyChannelSetting(b, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: "https://assets.example.test"})
		require.NoError(b, channel.Insert())
	}
	serial := 0
	for _, scenario := range []struct {
		name       string
		shareGroup string
		perModel   int
		targets    int
	}{
		{"separate_models_and_channels", "", 2, 8},
		{"explicit_shared_group", "benchmark-shared", 1, 1},
	} {
		b.Run(scenario.name, func(b *testing.B) {
			for _, channel := range channels {
				setSeedanceReadyChannelSetting(b, channel, kitdto.ChannelSettings{TaskPluginKey: "doubao", SeedanceAssetURL: "https://assets.example.test", SeedanceAssetShareGroup: scenario.shareGroup})
				require.NoError(b, channel.Update())
			}
			scopes, err := service.SeedanceAssetModelScopes(b.Context(), 1, []string{"vip"}, nil, 0)
			require.NoError(b, err)
			require.Len(b, scopes, len(modelNames))
			for _, name := range modelNames {
				require.Len(b, scopes[name], scenario.perModel)
			}
			var asset model.SeedanceAsset
			b.ResetTimer()
			for range b.N {
				serial++
				localOnly := false
				asset = model.SeedanceAsset{UserID: 1, ChannelID: fixture.group.ChannelID, GroupID: fixture.group.GroupID,
					AssetID: fmt.Sprintf("benchmark-sync-%d", serial), Name: "benchmark", AssetType: "Image", Status: "Processing",
					SourceURL: "https://example.test/image.png", KeyFingerprint: fixture.group.KeyFingerprint, LocalOnly: &localOnly}
				require.NoError(b, service.CreateSeedanceAssetWithSynchronization(b.Context(), &asset, []string{"vip"}, nil))
			}
			b.StopTimer()
			var count int64
			require.NoError(b, fixture.db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND local_asset_id = ?", 1, asset.ID).Count(&count).Error)
			require.EqualValues(b, scenario.targets, count)
			b.ReportMetric(float64(count), "targets/op")
		})
	}
}
