package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	driver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func setupSeedanceSyncTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("SEEDANCE_SYNC_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("SEEDANCE_SYNC_TEST_MYSQL_DSN is unset")
	}
	config, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(config.DBName, "codex_seedance_sync_") || config.DBName == "codex_agent_assets", "test requires an isolated database")
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	models := []any{&model.User{}, &model.Token{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.UserModelPricingHistory{}, &model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetDiscovery{}, &model.SeedanceAssetCleanupJob{}, &model.SystemTask{}, &model.SystemTaskLock{}}
	for _, item := range models {
		require.False(t, db.Migrator().HasTable(item), "isolated test database must be empty")
	}
	require.NoError(t, db.AutoMigrate(models...))
	previousDB, previousLog := model.DB, model.LOG_DB
	previousMain, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousMemory := common.RedisEnabled, common.MemoryCacheEnabled
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLog
		common.SetDatabaseTypes(previousMain, previousLogType)
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
		assert.NoError(t, db.Migrator().DropTable(models...))
		assert.NoError(t, connection.Close())
	})
	return db
}

func seedSeedanceSyncTestReplicas(t *testing.T, db *gorm.DB, total int) {
	t.Helper()
	group := &model.SeedanceAssetGroup{UserID: 1, GroupID: "sync-group", Name: "sync group", Status: "Active"}
	require.NoError(t, db.Create(group).Error)
	for index := range total {
		asset := &model.SeedanceAsset{UserID: 1, GroupID: group.GroupID, AssetID: fmt.Sprintf("sync-asset-%d", index), Name: "fixture", AssetType: "Image", Status: "Active"}
		require.NoError(t, db.Create(asset).Error)
		replica := &model.SeedanceAssetReplica{UserID: 1, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: 999, AccountFingerprint: "target", KeyFingerprint: "target", Status: "Pending", ProvisionStatus: model.SeedanceAssetReplicaCreating, PollAttempts: 6, NextPollAt: common.GetTimestamp() - 1}
		require.NoError(t, db.Create(replica).Error)
	}
}

func seedSeedanceDiscoveryChannels(t *testing.T, db *gorm.DB) []*model.Channel {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unexpected upstream request")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	channels := make([]*model.Channel, 0, 2)
	for index := range 2 {
		channel := &model.Channel{Id: index + 1, Type: constant.ChannelTypeDoubaoVideo, Key: fmt.Sprintf("fixture-discovery-%d", index), BaseURL: &upstream.URL, Group: "default", Models: fmt.Sprintf("doubao-seedance-discovery-%d", index), Status: common.ChannelStatusEnabled}
		configureSeedanceTestAssetURL(t, channel)
		require.NoError(t, channel.Insert())
		channels = append(channels, channel)
	}
	return channels
}

func seedSeedanceDiscoveryAssets(t *testing.T, db *gorm.DB, userID, count int, userGroup string) []model.SeedanceAsset {
	t.Helper()
	require.NoError(t, db.Create(&model.User{Id: userID, Username: fmt.Sprintf("discovery-%d", userID), AffCode: fmt.Sprintf("discovery-%d", userID), Group: userGroup, Status: common.UserStatusEnabled}).Error)
	group := model.SeedanceAssetGroup{UserID: userID, ChannelID: 1, GroupID: fmt.Sprintf("discovery-group-%d", userID), Name: "group", GroupType: "AIGC", Status: "Active"}
	require.NoError(t, db.Create(&group).Error)
	localOnly := false
	assets := make([]model.SeedanceAsset, count)
	for index := range assets {
		assets[index] = model.SeedanceAsset{UserID: userID, ChannelID: 1, GroupID: group.GroupID, AssetID: fmt.Sprintf("discovery-%d-%d", userID, index), Name: "asset", AssetType: "Image", LocalOnly: &localOnly, Status: "Active"}
	}
	require.NoError(t, db.CreateInBatches(&assets, 100).Error)
	return assets
}

func seedanceDiscoveryRequest(t *testing.T, userID int, channel *model.Channel) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("id", userID)
	c.Set("token_id", 1)
	c.Set("token_model_limit_enabled", true)
	c.Set("token_model_limit", map[string]bool{channel.Models: true})
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: channel.Id, Source: dto.PinSourceToken, Rank: dto.PinRankToken})
	_, _, err := seedanceAssetStatusExpression(c, "seedance_assets", false)
	require.NoError(t, err)
	return c
}

func TestSeedanceDiscoveryResumesMultipleScopes(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	channels := seedSeedanceDiscoveryChannels(t, db)
	first := seedSeedanceDiscoveryAssets(t, db, 9, 201, "default")
	seedSeedanceDiscoveryAssets(t, db, 10, 101, "default")
	linkOnly := true
	legacy := model.SeedanceAsset{UserID: 9, ChannelID: 1, GroupID: first[0].GroupID, AssetID: "discovery-legacy", Name: "legacy", AssetType: "Image", Status: "Active"}
	link := model.SeedanceAsset{UserID: 9, ChannelID: 1, GroupID: first[0].GroupID, AssetID: "discovery-link", Name: "link", AssetType: "Image", LocalOnly: &linkOnly, Status: "Active"}
	humanGroup := model.SeedanceAssetGroup{UserID: 9, ChannelID: 1, GroupID: "discovery-human", Name: "human", GroupType: "LivenessFace", Status: "Active"}
	require.NoError(t, db.Create(&humanGroup).Error)
	localOnly := false
	human := model.SeedanceAsset{UserID: 9, ChannelID: 1, GroupID: humanGroup.GroupID, AssetID: "discovery-human", Name: "human", AssetType: "Image", LocalOnly: &localOnly, Status: "Active"}
	require.NoError(t, db.Create(&legacy).Error)
	require.NoError(t, db.Create(&link).Error)
	require.NoError(t, db.Create(&human).Error)

	requests := []*gin.Context{seedanceDiscoveryRequest(t, 9, channels[0]), seedanceDiscoveryRequest(t, 9, channels[1]), seedanceDiscoveryRequest(t, 10, channels[0])}
	for _, request := range requests {
		require.NoError(t, scheduleMissingSeedanceAssetTargets(request))
	}
	var jobs []model.SeedanceAssetDiscovery
	require.NoError(t, db.Order("id").Find(&jobs).Error)
	require.Len(t, jobs, 3)
	for index, pair := range []struct {
		user    int
		channel *model.Channel
	}{{9, channels[0]}, {9, channels[1]}, {10, channels[0]}} {
		scope, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{pair.channel.Models}, pair.channel.Id)
		require.NoError(t, err)
		assert.Equal(t, pair.user, jobs[index].UserID)
		assert.Equal(t, scope, jobs[index].Scope)
	}
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Count(&count).Error)
	assert.EqualValues(t, 300, count)
	task, err := model.GetActiveSystemTask(model.SystemTaskTypeSeedanceAssetDiscovery)
	require.NoError(t, err)
	require.NotNil(t, task)
	claimed, ok, err := model.ClaimSystemTask(task.ID, task.Type, "discovery-old", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, ok)
	found, err := service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, db.First(&jobs[0], jobs[0].ID).Error)
	assert.Equal(t, first[99].ID, jobs[0].Cursor)

	// 运行中的同范围请求不得被按类型去重的系统任务吞掉。
	require.NoError(t, scheduleMissingSeedanceAssetTargets(requests[0]))
	require.NoError(t, db.First(&jobs[0], jobs[0].ID).Error)
	assert.EqualValues(t, 2, jobs[0].Revision)
	require.NoError(t, db.Model(&model.SystemTaskLock{}).Where("task_id = ?", claimed.TaskID).Update("locked_until", common.GetTimestamp()-1).Error)
	require.NoError(t, model.ExpireStaleSystemTaskLocks(common.GetTimestamp()))
	require.True(t, (seedanceAssetDiscoveryHandler{}).Enabled())
	task, created, err := service.EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetDiscovery, nil)
	require.NoError(t, err)
	require.True(t, created)
	claimed, ok, err = model.ClaimSystemTask(task.ID, task.Type, "discovery-new", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, ok)
	(seedanceAssetDiscoveryHandler{}).Run(t.Context(), claimed, "discovery-new")
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Count(&count).Error)
	assert.Zero(t, count)
	for _, item := range []struct{ user, channel, expected int }{{9, 1, 201}, {9, 2, 201}, {10, 1, 101}, {10, 2, 0}} {
		require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND channel_id = ?", item.user, item.channel).Count(&count).Error)
		assert.EqualValues(t, item.expected, count)
	}
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id IN ?", []uint{legacy.ID, link.ID, human.ID}).Count(&count).Error)
	assert.Zero(t, count)
	for _, pair := range []struct {
		user    int
		channel *model.Channel
	}{{9, channels[0]}, {9, channels[1]}, {10, channels[0]}} {
		scope, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{pair.channel.Models}, pair.channel.Id)
		require.NoError(t, err)
		require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND channel_id = ? AND sync_scope <> ?", pair.user, pair.channel.Id, scope).Count(&count).Error)
		assert.Zero(t, count)
	}
}

func TestSeedanceDiscoveryRechecksPermissions(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	channels := seedSeedanceDiscoveryChannels(t, db)
	users := []struct {
		id      int
		group   string
		channel *model.Channel
	}{{21, "default", channels[1]}, {22, "discovery-revoked", channels[1]}, {23, "default", channels[0]}}
	for _, user := range users {
		seedSeedanceDiscoveryAssets(t, db, user.id, 101, user.group)
		require.NoError(t, scheduleMissingSeedanceAssetTargets(seedanceDiscoveryRequest(t, user.id, user.channel)))
	}
	special := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
	previous := special.ReadAll()
	t.Cleanup(func() { special.Clear(); special.AddAll(previous) })
	special.Set("discovery-revoked", map[string]string{"-:default": "revoked"})
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 21).Update("status", common.UserStatusDisabled).Error)
	require.NoError(t, db.Model(channels[0]).Update("status", common.ChannelStatusManuallyDisabled).Error)
	task, err := model.GetActiveSystemTask(model.SystemTaskTypeSeedanceAssetDiscovery)
	require.NoError(t, err)
	require.NotNil(t, task)
	claimed, ok, err := model.ClaimSystemTask(task.ID, task.Type, "discovery-permissions", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, ok)
	(seedanceAssetDiscoveryHandler{}).Run(t.Context(), claimed, "discovery-permissions")
	var count int64
	for _, user := range users {
		require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ?", user.id).Count(&count).Error)
		assert.EqualValues(t, 100, count)
	}
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestSeedanceDiscoveryRejectsInvalidScopes(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	channels := seedSeedanceDiscoveryChannels(t, db)
	seedSeedanceDiscoveryAssets(t, db, 31, 1, "default")
	invalid := []string{
		`{"version":1,"models":null,"pinned_channel":1}`,
		`{"version":1,"groups":["default"],"pinned_channel":1}`,
		`{"groups":["default"],"models":null,"pinned_channel":1}`,
		`{"version":3,"groups":["default"],"models":null,"pinned_channel":1}`,
		`{"version":1,"groups":["default"],"models":null,"pinned_channel":null}`,
		`{"version":1,"groups":["default",null],"models":null,"pinned_channel":1}`,
		`{"version":1,"groups":["default"],"models":{},"pinned_channel":1}`,
	}
	for _, scope := range invalid {
		require.NoError(t, model.QueueSeedanceAssetDiscovery(t.Context(), 31, scope))
	}
	for range invalid {
		found, err := service.RunSeedanceAssetDiscoveryBatch(t.Context())
		assert.True(t, found)
		assert.Error(t, err)
	}
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Where("next_attempt_at = ?", -1).Where(map[string]any{"cursor": 0}).Count(&count).Error)
	assert.EqualValues(t, len(invalid), count)

	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{channels[0].Models}, channels[0].Id)
	require.NoError(t, err)
	unrestricted, err := service.EncodeSeedanceAssetSyncScope(nil, nil, 0)
	require.NoError(t, err)
	require.NoError(t, model.QueueSeedanceAssetDiscovery(t.Context(), 31, scope))
	key := model.SeedanceAssetDiscoveryScopeKey(31, scope)
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Where("scope_key = ?", key).Update("scope", unrestricted).Error)
	found, err := service.RunSeedanceAssetDiscoveryBatch(t.Context())
	assert.True(t, found)
	assert.Error(t, err)
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, service.QueueSeedanceAssetDiscovery(t.Context(), 31, []string{"default"}, []string{channels[0].Models}, channels[0].Id))
	for range 2 {
		found, err = service.RunSeedanceAssetDiscoveryBatch(t.Context())
		require.NoError(t, err)
		require.True(t, found)
	}
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND channel_id = ? AND sync_scope = ?", 31, channels[0].Id, scope).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestSeedanceDiscoveryRecoversAfterConcurrentDeletion(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	channels := seedSeedanceDiscoveryChannels(t, db)
	assets := seedSeedanceDiscoveryAssets(t, db, 51, 201, "default")
	otherAssets := seedSeedanceDiscoveryAssets(t, db, 52, 1, "default")
	for _, userID := range []int{51, 52} {
		require.NoError(t, service.QueueSeedanceAssetDiscovery(t.Context(), userID, []string{"default"}, []string{channels[0].Models}, channels[0].Id))
	}

	const callback = "test:discovery-delete-before-schedule-lock"
	var deleted atomic.Bool
	var deletionErr error
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != db.NamingStrategy.TableName("SeedanceAssetGroup") {
			return
		}
		if _, locking := tx.Statement.Clauses["FOR"]; !locking || !deleted.CompareAndSwap(false, true) {
			return
		}
		deletionErr = model.QueueSeedanceAssetDeletion(t.Context(), model.NewSeedanceAssetDeleteCleanupJobs(&assets[10], nil))
	}))
	t.Cleanup(func() { assert.NoError(t, db.Callback().Query().Remove(callback)) })

	found, runErr := service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.True(t, deleted.Load(), "删除必须发生在调度取组锁之前")
	require.NoError(t, deletionErr)
	assert.True(t, found)
	assert.ErrorIs(t, runErr, model.ErrSeedanceAssetUnavailable)
	var job model.SeedanceAssetDiscovery
	require.NoError(t, db.Where("user_id = ?", 51).First(&job).Error)
	assert.Zero(t, job.Cursor, "失败批次不能推进游标")
	assert.Equal(t, 1, job.Attempts)
	assert.Greater(t, job.NextAttemptAt, common.GetTimestamp())

	var deletedAsset model.SeedanceAsset
	require.NoError(t, db.First(&deletedAsset, assets[10].ID).Error)
	assert.Equal(t, "Deleting", deletedAsset.Status)
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ?", assets[10].ID).Count(&count).Error)
	assert.Zero(t, count)

	// 模拟调度器运行：其他用户不应被退避中的任务阻塞。
	task, err := model.GetActiveSystemTask(model.SystemTaskTypeSeedanceAssetDiscovery)
	require.NoError(t, err)
	require.NotNil(t, task)
	claimed, ok, err := model.ClaimSystemTask(task.ID, task.Type, "discovery-concurrent-delete", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, ok)
	(seedanceAssetDiscoveryHandler{}).Run(t.Context(), claimed, "discovery-concurrent-delete")
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND local_asset_id = ?", 52, otherAssets[0].ID).Count(&count).Error)
	assert.EqualValues(t, 1, count)

	// 将等待期置为到期，模拟重启或定时调度，不依赖新的用户请求。
	err = db.Where("user_id = ?", 51).First(&job).Error
	if err == nil {
		require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
		assert.True(t, (seedanceAssetDiscoveryHandler{}).Enabled())
		task, _, err = service.EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetDiscovery, nil)
		require.NoError(t, err)
		claimed, ok, err = model.ClaimSystemTask(task.ID, task.Type, "discovery-recovered", common.GetTimestamp()+60)
		require.NoError(t, err)
		require.True(t, ok)
		(seedanceAssetDiscoveryHandler{}).Run(t.Context(), claimed, "discovery-recovered")
	} else {
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	}
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ?", 51).Count(&count).Error)
	assert.EqualValues(t, 200, count, "未删除的素材均应补排")
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("local_asset_id = ?", assets[10].ID).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.First(&deletedAsset, assets[10].ID).Error)
	assert.Equal(t, "Deleting", deletedAsset.Status)
}

func TestSeedanceDiscoveryDefersRepeatedUnavailableBatches(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	channels := seedSeedanceDiscoveryChannels(t, db)
	assets := seedSeedanceDiscoveryAssets(t, db, 53, 1, "default")
	require.NoError(t, service.QueueSeedanceAssetDiscovery(t.Context(), 53, []string{"default"}, []string{channels[0].Models}, channels[0].Id))

	const callback = "test:discovery-repeated-unavailable"
	var failures atomic.Int32
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != db.NamingStrategy.TableName("SeedanceAssetGroup") {
			return
		}
		if _, locking := tx.Statement.Clauses["FOR"]; locking && failures.Add(1) <= 7 {
			tx.AddError(model.ErrSeedanceAssetUnavailable)
		}
	}))
	t.Cleanup(func() { assert.NoError(t, db.Callback().Query().Remove(callback)) })

	var job model.SeedanceAssetDiscovery
	for attempt := 1; attempt <= 7; attempt++ {
		if attempt > 1 {
			require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
		}
		found, err := service.RunSeedanceAssetDiscoveryBatch(t.Context())
		assert.True(t, found)
		require.ErrorIs(t, err, model.ErrSeedanceAssetUnavailable)
		require.NoError(t, db.Where("user_id = ?", 53).First(&job).Error)
		assert.Zero(t, job.Cursor)
		assert.Equal(t, min(attempt, 6), job.Attempts)
		now := common.GetTimestamp()
		assert.Greater(t, job.NextAttemptAt, now)
		assert.LessOrEqual(t, job.NextAttemptAt, now+300)
		assert.False(t, (seedanceAssetDiscoveryHandler{}).Enabled())
	}
	assert.EqualValues(t, 7, failures.Load())
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ?", 53).Count(&count).Error)
	assert.Zero(t, count)

	// 模拟定时调度恢复，无需再次提交发现请求。
	require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
	require.True(t, (seedanceAssetDiscoveryHandler{}).Enabled())
	task, err := model.GetActiveSystemTask(model.SystemTaskTypeSeedanceAssetDiscovery)
	require.NoError(t, err)
	require.NotNil(t, task)
	claimed, ok, err := model.ClaimSystemTask(task.ID, task.Type, "discovery-retry-recovered", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, ok)
	(seedanceAssetDiscoveryHandler{}).Run(t.Context(), claimed, "discovery-retry-recovered")
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Where("user_id = ?", 53).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND local_asset_id = ?", 53, assets[0].ID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestSeedanceDiscoveryRecoversAfterTransientFailure(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	channels := seedSeedanceDiscoveryChannels(t, db)
	assets := seedSeedanceDiscoveryAssets(t, db, 41, 101, "default")
	require.NoError(t, scheduleMissingSeedanceAssetTargets(seedanceDiscoveryRequest(t, 41, channels[0])))
	found, err := service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	const callback = "test:discovery-insert-failure"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "seedance_asset_replicas" {
			tx.AddError(errors.New("temporary insert failure"))
		}
	}))
	found, err = service.RunSeedanceAssetDiscoveryBatch(t.Context())
	assert.True(t, found)
	assert.ErrorContains(t, err, "temporary insert failure")
	require.NoError(t, db.Callback().Create().Remove(callback))
	var job model.SeedanceAssetDiscovery
	require.NoError(t, db.Where("user_id = ?", 41).First(&job).Error)
	assert.Equal(t, assets[99].ID, job.Cursor)
	assert.Equal(t, 1, job.Attempts)
	assert.Greater(t, job.NextAttemptAt, common.GetTimestamp())
	require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
	found, err = service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ?", 41).Count(&count).Error)
	assert.EqualValues(t, 101, count)
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Where("user_id = ?", 41).Count(&count).Error)
	assert.Zero(t, count)
}

func TestSeedanceAssetSyncDefersUnavailableAccounts(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	const total = 41
	seedSeedanceSyncTestReplicas(t, db, total)
	task, err := model.CreateSystemTask(model.SystemTaskTypeSeedanceAssetSync, nil, nil)
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(task.ID, task.Type, "sync-test", time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, ok)
	(seedanceAssetSyncHandler{}).Run(t.Context(), claimed, "sync-test")
	task, err = model.GetActiveSystemTask(model.SystemTaskTypeSeedanceAssetSync)
	require.NoError(t, err)
	assert.Nil(t, task)
	var deferred int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("next_poll_at > ?", time.Now().Add(10*time.Minute).Unix()).Count(&deferred).Error)
	assert.EqualValues(t, total, deferred)
}

func TestSeedanceAssetSyncStopsImmediateRetryWhenNoProgress(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	seedSeedanceSyncTestReplicas(t, db, 1)
	const callback = "test:reject-seedance-sync-retry"
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "seedance_asset_replicas" {
			tx.AddError(errors.New("retry schedule unavailable"))
		}
	}))
	t.Cleanup(func() { assert.NoError(t, db.Callback().Update().Remove(callback)) })
	task, err := model.CreateSystemTask(model.SystemTaskTypeSeedanceAssetSync, nil, nil)
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(task.ID, task.Type, "sync-test", time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, ok)
	(seedanceAssetSyncHandler{}).Run(t.Context(), claimed, "sync-test")
	active, err := model.GetActiveSystemTask(model.SystemTaskTypeSeedanceAssetSync)
	require.NoError(t, err)
	assert.Nil(t, active)
	assert.True(t, model.HasDueSeedanceAssetSync(common.GetTimestamp()))
}

func TestSeedanceAssetSyncDrainsManyUsersWithoutDuplicateCopies(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	var groupCreates, assetCreates atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"group-copy-%d"}}`, groupCreates.Add(1))
		case "CreateAsset":
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"asset-copy-%d"}}`, assetCreates.Add(1))
		case "GetAsset":
			_, _ = fmt.Fprint(w, `{"Result":{"Status":"Active"}}`)
		default:
			http.Error(w, "unexpected mock action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	channel := &model.Channel{Id: 1, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-sync-target", BaseURL: &upstream.URL, Group: "default", Models: "doubao-seedance-2-0-260128", Status: common.ChannelStatusEnabled}
	settings := channel.GetSetting()
	settings.SeedanceAssetURL = upstream.URL
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	setting := string(encoded)
	channel.Setting = &setting
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "default", Model: channel.Models, ChannelId: channel.Id, Enabled: true}).Error)
	client, err := service.NewSeedanceAssetClient(channel)
	require.NoError(t, err)
	client = client.ForSeedanceModel(channel, channel.Models)
	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, nil, 0)
	require.NoError(t, err)
	const users = 200
	for index := range users {
		id := index + 1
		require.NoError(t, db.Create(&model.User{Id: id, Username: fmt.Sprintf("sync-user-%d", id), AffCode: fmt.Sprintf("sync-%d", id), Group: "default", Status: common.UserStatusEnabled}).Error)
		group := &model.SeedanceAssetGroup{UserID: id, ChannelID: 99, GroupID: fmt.Sprintf("source-group-%d", id), Name: "source", GroupType: "AIGC", Status: "Active", AccountFingerprint: "source"}
		require.NoError(t, db.Create(group).Error)
		asset := &model.SeedanceAsset{UserID: id, ChannelID: 99, GroupID: group.GroupID, AssetID: fmt.Sprintf("source-asset-%d", id), Name: "source.png", AssetType: "Image", Status: "Active", SourceURL: "https://8.8.8.8/source.png", AccountFingerprint: "source"}
		require.NoError(t, db.Create(asset).Error)
		replica := &model.SeedanceAssetReplica{UserID: id, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channel.Id, AccountFingerprint: client.AccountFingerprint, KeyFingerprint: client.KeyFingerprint, SyncScope: scope, Status: "Pending", ProvisionStatus: model.SeedanceAssetReplicaCreating, NextPollAt: common.GetTimestamp() - 1}
		require.NoError(t, db.Create(replica).Error)
	}
	task, err := model.CreateSystemTask(model.SystemTaskTypeSeedanceAssetSync, nil, nil)
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(task.ID, task.Type, "sync-load", time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, ok)
	(seedanceAssetSyncHandler{}).Run(t.Context(), claimed, "sync-load")
	task, err = model.GetActiveSystemTask(model.SystemTaskTypeSeedanceAssetSync)
	require.NoError(t, err)
	assert.Nil(t, task)
	var ready int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("provision_status = ? AND status = ?", model.SeedanceAssetReplicaReady, "Active").Count(&ready).Error)
	assert.EqualValues(t, users, ready)
	assert.EqualValues(t, users, groupCreates.Load())
	assert.EqualValues(t, users, assetCreates.Load())
}

func TestBillingRefundSchedulerIgnoresVideoUpdateSwitch(t *testing.T) {
	db, dialect := openTaskDialectDatabase(t, &model.User{}, &model.Token{}, &model.Channel{}, &model.Task{}, &model.Log{}, &model.BillingRefund{}, &model.VideoTaskPendingLog{}, &model.SystemTask{}, &model.SystemTaskLock{})
	previousDB, previousUpdate, previousRedis := model.DB, constant.UpdateTask, common.RedisEnabled
	previousLogDB, previousConsume, previousMemory := model.LOG_DB, common.LogConsumeEnabled, common.MemoryCacheEnabled
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	model.DB, constant.UpdateTask, common.RedisEnabled = db, false, false
	model.LOG_DB, common.LogConsumeEnabled, common.MemoryCacheEnabled = db, true, false
	common.SetDatabaseTypes(dialect, dialect)
	t.Cleanup(func() {
		model.DB, constant.UpdateTask, common.RedisEnabled = previousDB, previousUpdate, previousRedis
		model.LOG_DB, common.LogConsumeEnabled, common.MemoryCacheEnabled = previousLogDB, previousConsume, previousMemory
		common.SetDatabaseTypes(previousMain, previousLog)
	})
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "refund-owner", Quota: 900}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 1, UserId: 1, Key: "refund-token", RemainQuota: 900, UsedQuota: 100}).Error)
	refund := &model.BillingRefund{RequestID: "no-video-refund", UserID: 1, TokenID: 1, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	handler := billingRefundHandler{}
	require.True(t, handler.Enabled())
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	require.NoError(t, db.First(refund, refund.ID).Error)
	assert.True(t, refund.Completed)
	assert.False(t, handler.Enabled())
	var user model.User
	require.NoError(t, db.First(&user, 1).Error)
	assert.Equal(t, 1000, user.Quota)

	require.NoError(t, db.Model(&model.User{}).Where("id = ?", 1).Update("quota", 900).Error)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", 1).Updates(map[string]any{"remain_quota": 900, "used_quota": 100}).Error)
	require.NoError(t, db.Create(&model.Channel{Id: 1, Name: "prepared-test"}).Error)
	prepared := &model.Task{TaskID: "unsubmitted", UserId: 1, ChannelId: 1, Quota: 100, Status: model.TaskStatusUnknown,
		PrivateData: model.TaskPrivateData{TokenId: 1, BillingSource: service.BillingSourceWallet, SubmissionPending: true,
			SubmissionPrepared: true, InitialAccountingPending: true, SubmissionAttemptID: "attempt",
			SubmissionFunding: &model.TaskSubmissionFunding{ReservedQuota: 100, UpdateToken: true}}}
	require.NoError(t, db.Create(prepared).Error)
	require.True(t, handler.Enabled())
	run, err := model.CreateSystemTask(handler.Type(), nil, nil)
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(run.ID, run.Type, "prepared-recovery", time.Now().Add(time.Minute).Unix())
	require.NoError(t, err)
	require.True(t, ok)
	handler.Run(t.Context(), claimed, "prepared-recovery")
	require.NoError(t, db.First(prepared, prepared.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), prepared.Status)
	require.NoError(t, db.First(&user, 1).Error)
	assert.Equal(t, 1000, user.Quota)
	var token model.Token
	require.NoError(t, db.First(&token, 1).Error)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.False(t, handler.Enabled())
}

func TestBillingRefundHandlerRunsOnMySQL(t *testing.T) {
	db := setupSeedanceSyncTestDB(t)
	additional := []any{&model.Task{}, &model.TaskResource{}, &model.TaskResourceIndexState{}, &model.BillingRefund{}, &model.VideoTaskPendingLog{}, &model.Log{}}
	for _, value := range additional {
		require.False(t, db.Migrator().HasTable(value))
	}
	require.NoError(t, db.AutoMigrate(additional...))
	t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(additional...)) })
	previousUpdate, previousConsume := constant.UpdateTask, common.LogConsumeEnabled
	constant.UpdateTask, common.LogConsumeEnabled = false, true
	t.Cleanup(func() { constant.UpdateTask, common.LogConsumeEnabled = previousUpdate, previousConsume })
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "handler-owner", Quota: 900}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 1, UserId: 1, Key: "handler-token", RemainQuota: 900, UsedQuota: 100}).Error)
	now, err := model.GetTaskSubmissionDatabaseTime(t.Context())
	require.NoError(t, err)
	refund := &model.BillingRefund{RequestID: "handler-preparation", UserID: 1, TokenID: 1, Amount: 100, TokenAmount: 100, NextAttemptAt: model.BillingRefundInFlight, SettlementRetryAt: -now}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	handler := billingRefundHandler{}
	require.True(t, handler.Enabled())
	run, err := model.CreateSystemTask(handler.Type(), nil, nil)
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(run.ID, run.Type, "handler-recovery", now+60)
	require.NoError(t, err)
	require.True(t, ok)
	handler.Run(t.Context(), claimed, "handler-recovery")
	require.NoError(t, db.First(run, run.ID).Error)
	assert.Equal(t, model.SystemTaskStatusSucceeded, run.Status)
	require.NoError(t, db.First(refund, refund.ID).Error)
	assert.True(t, refund.Completed)
	var user model.User
	var token model.Token
	require.NoError(t, db.First(&user, 1).Error)
	require.NoError(t, db.First(&token, 1).Error)
	assert.Equal(t, 1000, user.Quota)
	assert.Equal(t, 1000, token.RemainQuota)
}

func TestSystemTaskListFiltersAndPaginationResponse(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}))
	require.NoError(t, db.Create(&[]model.SystemTask{
		{TaskID: "older", Type: model.SystemTaskTypeModelUpdate, Status: model.SystemTaskStatusFailed},
		{TaskID: "newer", Type: model.SystemTaskTypeModelUpdate, Status: model.SystemTaskStatusFailed},
		{TaskID: "other-status", Type: model.SystemTaskTypeModelUpdate, Status: model.SystemTaskStatusSucceeded},
		{TaskID: "other-type", Type: model.SystemTaskTypeChannelTest, Status: model.SystemTaskStatusFailed},
	}).Error)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/system-task/list?scope=history&type=model_update&status=failed&offset=1&limit=1", nil)
	ListSystemTasks(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool                       `json:"success"`
		Total   int64                      `json:"total"`
		Data    []model.SystemTaskResponse `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.EqualValues(t, 2, response.Total)
	require.Len(t, response.Data, 1)
	assert.Equal(t, "older", response.Data[0].TaskID)
}

func TestSystemTaskInvalidFiltersAreRejected(t *testing.T) {
	for _, tc := range []struct {
		query   string
		handler gin.HandlerFunc
	}{
		{"?scope=invalid", ListSystemTasks},
		{"?status=unknown", ListSystemTasks},
		{"?offset=-1", ListSystemTasks},
		{"?limit=invalid", ListSystemTasks},
		{"?status=unknown", DeleteSystemTaskHistory},
		{"?scope=invalid", DeleteSystemTaskHistory},
	} {
		t.Run(tc.query, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/"+tc.query, nil)
			tc.handler(c)
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.JSONEq(t, `{"success":false,"message":"invalid system task filters"}`, recorder.Body.String())
		})
	}
}

const seedanceScopeTestBase = "doubao-seedance-scope-test"
const seedanceScopeTestExact = seedanceScopeTestBase + "@temperature:0.2"
const seedanceScopeTestOther = seedanceScopeTestBase + "@temperature:0.3"

func setupSeedanceScopeRecoveryMySQL(t *testing.T, failing ...*atomic.Bool) (*gorm.DB, *gin.Engine, *model.Channel, *service.SeedanceAssetClient, *atomic.Int32) {
	t.Helper()
	db := setupSeedanceSyncTestDB(t)
	t.Setenv("LOG_SQL_DSN", "")
	wasMaster := common.IsMasterNode
	common.IsMasterNode = false
	err := model.InitLogDB()
	common.IsMasterNode = wasMaster
	require.NoError(t, err)
	oldRatios, oldGroups := ratio_setting.GroupRatio2JSONString(), setting.UserUsableGroups2JSONString()
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))
	t.Cleanup(func() {
		assert.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(oldRatios))
		assert.NoError(t, setting.UpdateUserUsableGroupsByJSONString(oldGroups))
	})
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") != "GetAsset" {
			t.Errorf("unexpected local upstream action: %s", r.URL.Query().Get("Action"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		upstreamCalls.Add(1)
		if len(failing) > 0 && failing[0].Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"Result":{"Status":"Processing"}}`)
	}))
	t.Cleanup(server.Close)
	service.InitHttpClient()
	channel := &model.Channel{Id: 701, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-upstream", BaseURL: &server.URL, Group: "default", Models: strings.Join([]string{seedanceScopeTestBase, seedanceScopeTestExact, seedanceScopeTestOther}, ","), Status: common.ChannelStatusEnabled}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, channel.Insert())
	client, err := service.NewSeedanceAssetClient(channel)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.User{Id: 701, Username: "scopeowner", AffCode: "scopeowner", Group: "default", Status: common.UserStatusEnabled}).Error)
	tokens := []model.Token{
		{UserId: 701, Key: "scopevalid", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default", ModelLimitsEnabled: true, ModelLimits: seedanceScopeTestExact},
		{UserId: 701, Key: "scopebase", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default", ModelLimitsEnabled: true, ModelLimits: seedanceScopeTestBase},
		{UserId: 701, Key: "scopeother", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default", ModelLimitsEnabled: true, ModelLimits: "doubao-seedance-unavailable"},
		{UserId: 701, Key: "scopeempty", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default", ModelLimitsEnabled: true},
		{UserId: 701, Key: "scopeexpired", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: 1, Group: "default", ModelLimitsEnabled: true, ModelLimits: seedanceScopeTestExact},
	}
	require.NoError(t, db.Create(&tokens).Error)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/assets", middleware.TokenAuth(), ListSeedanceAssets)
	router.GET("/assets/:id", middleware.TokenAuth(), GetSeedanceAsset)
	router.POST("/assets/:id/refresh", middleware.TokenAuth(), RefreshSeedanceAsset)
	return db, router, channel, client, &upstreamCalls
}

func seedanceScopeTestRequest(router *gin.Engine, method, path, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer sk-"+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func seedanceScopeTestV1(t *testing.T) string {
	t.Helper()
	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{seedanceScopeTestBase, seedanceScopeTestExact}, 0)
	require.NoError(t, err)
	return strings.Replace(scope, `"version":2`, `"version":1`, 1)
}

func TestSeedanceVersionOnePrimaryRefreshMySQL(t *testing.T) {
	var failing atomic.Bool
	db, router, channel, client, calls := setupSeedanceScopeRecoveryMySQL(t, &failing)
	group := &model.SeedanceAssetGroup{UserID: 701, ChannelID: channel.Id, GroupID: "human-scope", Name: "human", GroupType: "LivenessFace", Status: "Active", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{UserID: 701, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "human-scope-asset", Name: "human", AssetType: "Image", Status: "Processing", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint, PrimarySyncScope: seedanceScopeTestV1(t), NextPollAt: common.GetTimestamp() - 1}
	require.NoError(t, db.Create(asset).Error)
	path := fmt.Sprintf("/assets/%d/refresh", asset.ID)
	assert.Equal(t, http.StatusUnauthorized, seedanceScopeTestRequest(router, http.MethodPost, path, "scopeexpired").Code)
	assert.Equal(t, http.StatusForbidden, seedanceScopeTestRequest(router, http.MethodPost, path, "scopeother").Code)
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, seedanceScopeTestV1(t), asset.PrimarySyncScope)
	assert.Zero(t, calls.Load())
	failing.Store(true)
	response := seedanceScopeTestRequest(router, http.MethodPost, path, "scopevalid")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	require.NoError(t, db.First(asset, asset.ID).Error)
	want, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{seedanceScopeTestExact}, 0)
	require.NoError(t, err)
	assert.Equal(t, want, asset.PrimarySyncScope)
	assert.Equal(t, "Processing", asset.Status)
	failing.Store(false)
	response = seedanceScopeTestRequest(router, http.MethodPost, path, "scopevalid")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, want, asset.PrimarySyncScope)
	require.NoError(t, db.Model(asset).Update("next_poll_at", common.GetTimestamp()-1).Error)
	summary, err := service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Updated)
	assert.EqualValues(t, 3, calls.Load())
}

func TestSeedanceVersionOneDiscoveryReauthorizationMySQL(t *testing.T) {
	db, router, channel, client, _ := setupSeedanceScopeRecoveryMySQL(t)
	group := model.SeedanceAssetGroup{UserID: 701, ChannelID: channel.Id, GroupID: "scope-discovery", Name: "scope", GroupType: "AIGC", Status: "Active"}
	require.NoError(t, db.Create(&group).Error)
	localOnly := false
	assets := make([]model.SeedanceAsset, 101)
	for index := range assets {
		assets[index] = model.SeedanceAsset{UserID: 701, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: fmt.Sprintf("scope-discovery-%d", index), Name: "asset", AssetType: "Image", Status: "Processing", LocalOnly: &localOnly}
	}
	require.NoError(t, db.CreateInBatches(&assets, 100).Error)
	oldScope := seedanceScopeTestV1(t)
	replicas := make([]model.SeedanceAssetReplica, 0, len(assets)*2)
	for _, asset := range assets {
		for _, name := range []string{seedanceScopeTestBase, seedanceScopeTestExact} {
			replicas = append(replicas, model.SeedanceAssetReplica{UserID: 701, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channel.Id, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(channel, name, client.KeyFingerprint), KeyFingerprint: client.KeyFingerprint, SyncScope: oldScope, ProvisionStatus: model.SeedanceAssetReplicaFailed, Status: "Pending", PollAttempts: model.SeedanceAssetSyncMaxAttempts, NextPollAt: common.GetTimestamp() + 900, LastError: "asset synchronization target is not currently available to this user"})
		}
	}
	replicas[0].ProvisionStatus = model.SeedanceAssetReplicaReady
	replicas[0].Status = "Processing"
	replicas[0].UpstreamAssetID = "existing-upstream"
	replicas[0].LastError = model.SeedanceAssetReviewTimeoutCode
	replicas[0].PollLeaseUntil = common.GetTimestamp() + 60
	replicas[1].PollLeaseUntil = common.GetTimestamp() - 1
	require.NoError(t, db.CreateInBatches(&replicas, 100).Error)
	require.NoError(t, model.QueueSeedanceAssetDiscovery(t.Context(), 701, oldScope))
	found, err := service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.True(t, found)
	require.Error(t, err)
	var oldJob model.SeedanceAssetDiscovery
	require.NoError(t, db.Where("user_id = ? AND scope = ?", 701, oldScope).First(&oldJob).Error)
	assert.EqualValues(t, -1, oldJob.NextAttemptAt)
	assert.Equal(t, "scope requires reauthorization", oldJob.LastError)
	assert.Equal(t, http.StatusUnauthorized, seedanceScopeTestRequest(router, http.MethodGet, "/assets?status=active", "scopeexpired").Code)
	assert.Equal(t, http.StatusOK, seedanceScopeTestRequest(router, http.MethodGet, "/assets?status=active", "scopeempty").Code)
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAssetDiscovery{}).Where("user_id = ? AND next_attempt_at >= 0", 701).Count(&count).Error)
	assert.Zero(t, count)
	response := seedanceScopeTestRequest(router, http.MethodGet, "/assets?status=active", "scopevalid")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	want, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{seedanceScopeTestExact}, 0)
	require.NoError(t, err)
	var job model.SeedanceAssetDiscovery
	require.NoError(t, db.Where("user_id = ? AND scope = ?", 701, want).First(&job).Error)
	var expiredLease model.SeedanceAssetReplica
	require.NoError(t, db.First(&expiredLease, replicas[1].ID).Error)
	assert.Equal(t, want, expiredLease.SyncScope)
	assert.Zero(t, expiredLease.PollLeaseUntil)
	found, err = service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.True(t, found)
	require.ErrorIs(t, err, service.ErrSeedanceAssetServiceTemporary)
	require.NoError(t, db.First(&job, job.ID).Error)
	assert.Zero(t, job.Cursor)
	assert.Greater(t, job.NextAttemptAt, common.GetTimestamp())
	var leased model.SeedanceAssetReplica
	require.NoError(t, db.First(&leased, replicas[0].ID).Error)
	assert.Equal(t, oldScope, leased.SyncScope)
	assert.Equal(t, "existing-upstream", leased.UpstreamAssetID)
	assert.Greater(t, leased.PollLeaseUntil, common.GetTimestamp())
	require.NoError(t, db.Model(&leased).Update("poll_lease_until", 0).Error)
	require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
	for range 2 {
		found, err = service.RunSeedanceAssetDiscoveryBatch(t.Context())
		require.NoError(t, err)
		require.True(t, found)
	}
	var updated, forbidden int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND sync_scope = ?", 701, want).Count(&updated).Error)
	assert.EqualValues(t, len(replicas), updated)
	require.NoError(t, db.First(&leased, replicas[0].ID).Error)
	assert.Equal(t, "existing-upstream", leased.UpstreamAssetID)
	assert.Equal(t, "Processing", leased.Status)
	assert.Zero(t, leased.PollAttempts)
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND account_fingerprint = ?", 701, model.SeedanceAssetModelScopeFingerprint(channel, seedanceScopeTestOther, client.KeyFingerprint)).Count(&forbidden).Error)
	assert.Zero(t, forbidden)
}

func TestSeedanceRequestedModelStatusMySQL(t *testing.T) {
	db, router, channel, client, _ := setupSeedanceScopeRecoveryMySQL(t)
	group := &model.SeedanceAssetGroup{UserID: 701, ChannelID: channel.Id, GroupID: "scope-status", Name: "scope", GroupType: "AIGC", Status: "Active"}
	require.NoError(t, db.Create(group).Error)
	localOnly := false
	asset := &model.SeedanceAsset{UserID: 701, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "scope-status-asset", Name: "asset", AssetType: "Image", Status: "Processing", LocalOnly: &localOnly}
	require.NoError(t, db.Create(asset).Error)
	scope, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{seedanceScopeTestBase}, 0)
	require.NoError(t, err)
	base := &model.SeedanceAssetReplica{UserID: 701, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channel.Id, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(channel, seedanceScopeTestBase, client.KeyFingerprint), KeyFingerprint: client.KeyFingerprint, SyncScope: scope, ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Failed", UpstreamAssetID: "base-asset"}
	exact := &model.SeedanceAssetReplica{UserID: 701, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channel.Id, AccountFingerprint: model.SeedanceAssetModelScopeFingerprint(channel, seedanceScopeTestExact, client.KeyFingerprint), KeyFingerprint: client.KeyFingerprint, SyncScope: scope, ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active", UpstreamAssetID: "exact-asset"}
	require.NoError(t, db.Create(base).Error)
	require.NoError(t, db.Create(exact).Error)
	path := fmt.Sprintf("/assets/%d", asset.ID)
	response := seedanceScopeTestRequest(router, http.MethodGet, path, "scopebase")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var payload struct {
		Data model.SeedanceAsset `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.Equal(t, "Failed", payload.Data.Status)
	response = seedanceScopeTestRequest(router, http.MethodGet, path, "scopevalid")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.Equal(t, "Active", payload.Data.Status)
	require.NoError(t, db.Model(base).Update("status", "Active").Error)
	require.NoError(t, db.Model(exact).Update("status", "Failed").Error)
	response = seedanceScopeTestRequest(router, http.MethodGet, path, "scopebase")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.Equal(t, "Active", payload.Data.Status)
	response = seedanceScopeTestRequest(router, http.MethodGet, path, "scopevalid")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.Equal(t, "Active", payload.Data.Status)
	channel.Models = seedanceScopeTestBase
	require.NoError(t, channel.Update())
	response = seedanceScopeTestRequest(router, http.MethodGet, path, "scopevalid")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
	assert.Equal(t, "Active", payload.Data.Status)
}

func TestSeedanceVersionOneHumanDiscoveryMySQL(t *testing.T) {
	db, router, channel, client, calls := setupSeedanceScopeRecoveryMySQL(t)
	group := &model.SeedanceAssetGroup{UserID: 701, ChannelID: channel.Id, GroupID: "human-discovery", Name: "human", GroupType: "LivenessFace", Status: "Active", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{UserID: 701, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "human-discovery-asset", Name: "human", AssetType: "Image", Status: "Processing", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint, PrimarySyncScope: seedanceScopeTestV1(t), NextPollAt: common.GetTimestamp() + 900, PollLeaseUntil: common.GetTimestamp() + 60}
	require.NoError(t, db.Create(asset).Error)
	require.NoError(t, db.Create(&model.User{Id: 702, Username: "scopeforeign", AffCode: "scopeforeign", Group: "default", Status: common.UserStatusEnabled}).Error)
	foreignGroup := &model.SeedanceAssetGroup{UserID: 702, ChannelID: channel.Id, GroupID: "foreign-human", Name: "foreign", GroupType: "LivenessFace", Status: "Active", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint}
	require.NoError(t, db.Create(foreignGroup).Error)
	foreign := &model.SeedanceAsset{UserID: 702, ChannelID: channel.Id, GroupID: foreignGroup.GroupID, AssetID: "foreign-human-asset", Name: "foreign", AssetType: "Image", Status: "Processing", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint, PrimarySyncScope: seedanceScopeTestV1(t)}
	require.NoError(t, db.Create(foreign).Error)
	response := seedanceScopeTestRequest(router, http.MethodGet, "/assets?status=active", "scopevalid")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	found, err := service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.True(t, found)
	require.ErrorIs(t, err, service.ErrSeedanceAssetServiceTemporary)
	require.NoError(t, db.First(asset, asset.ID).Error)
	assert.Equal(t, seedanceScopeTestV1(t), asset.PrimarySyncScope)
	var job model.SeedanceAssetDiscovery
	require.NoError(t, db.Where("user_id = ? AND next_attempt_at >= 0", 701).First(&job).Error)
	assert.Zero(t, job.Cursor)
	require.NoError(t, db.Model(asset).Update("poll_lease_until", 0).Error)
	require.NoError(t, db.Model(&job).Update("next_attempt_at", 0).Error)
	found, err = service.RunSeedanceAssetDiscoveryBatch(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, db.First(asset, asset.ID).Error)
	want, err := service.EncodeSeedanceAssetSyncScope([]string{"default"}, []string{seedanceScopeTestExact}, 0)
	require.NoError(t, err)
	assert.Equal(t, want, asset.PrimarySyncScope)
	assert.LessOrEqual(t, asset.NextPollAt, common.GetTimestamp())
	require.NoError(t, db.First(foreign, foreign.ID).Error)
	assert.Equal(t, seedanceScopeTestV1(t), foreign.PrimarySyncScope)
	assert.Zero(t, calls.Load())
}
