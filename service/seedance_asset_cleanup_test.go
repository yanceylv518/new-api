package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 独立内存库验证后台清理任务的外部删除、本地删除和租约状态转换。
func TestRunSeedanceAssetCleanupOnceDeletesRemoteAndLocal(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetCleanupJob{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	var deleteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("Action") == "DeleteAsset" {
			deleteCalls.Add(1)
		}
		_, _ = writer.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	channel := &model.Channel{Id: 801, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(channel).Error)
	asset := &model.SeedanceAsset{
		UserID: 9, ChannelID: channel.Id, GroupID: "group-1", AssetID: "asset-cleanup-1",
		Name: "clip.mp4", AssetType: "Video", Status: "Deleting",
	}
	require.NoError(t, db.Create(asset).Error)
	job := NewSeedanceAssetDeleteCleanupJob(asset)
	require.NoError(t, db.Create(&job).Error)

	summary, err := RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Candidates)
	assert.Equal(t, 1, summary.Claimed)
	assert.Equal(t, 1, summary.Completed)
	assert.Equal(t, int32(1), deleteCalls.Load())

	var remaining int64
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
	var storedJob model.SeedanceAssetCleanupJob
	assert.ErrorIs(t, db.First(&storedJob, job.ID).Error, gorm.ErrRecordNotFound)
}

// 外部清理失败必须保留任务、推进退避时间和原始错误，不能把失败伪装成已完成。
func TestRunSeedanceAssetCleanupOnceReschedulesFailure(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAssetCleanupJob{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"code":"temporary_failure","message":"retry later"}`))
	}))
	t.Cleanup(server.Close)
	channel := &model.Channel{Id: 802, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(channel).Error)
	job := &model.SeedanceAssetCleanupJob{
		Kind:          model.SeedanceAssetCleanupKindUpstreamAsset,
		ChannelID:     channel.Id,
		UpstreamID:    "asset-retry-1",
		DedupKey:      "cleanup-retry-1",
		Status:        model.SeedanceAssetCleanupStatusPending,
		NextAttemptAt: common.GetTimestamp(),
	}
	require.NoError(t, db.Create(job).Error)

	summary, err := RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Candidates)
	assert.Equal(t, 1, summary.RetryScheduled)
	assert.Equal(t, 1, summary.Errors)

	var storedJob model.SeedanceAssetCleanupJob
	require.NoError(t, db.First(&storedJob, job.ID).Error)
	assert.Equal(t, model.SeedanceAssetCleanupStatusPending, storedJob.Status)
	assert.Equal(t, 1, storedJob.Attempts)
	assert.Greater(t, storedJob.NextAttemptAt, common.GetTimestamp())
	assert.Contains(t, storedJob.LastError, "retry later")
}

func TestSeedanceAssetCleanupUsesEnabledAliasAfterSourceChannelDisable(t *testing.T) {
	var deleteCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "DeleteAsset" {
			deleteCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetCleanupJob{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	baseURL := upstream.URL
	source := &model.Channel{Id: 811, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &baseURL, Status: common.ChannelStatusEnabled}
	client, err := NewSeedanceAssetClient(source)
	require.NoError(t, err)
	source.Status = common.ChannelStatusManuallyDisabled
	alias := *source
	alias.Id = 812
	alias.Status = common.ChannelStatusEnabled
	require.NoError(t, db.Create(source).Error)
	require.NoError(t, db.Create(&alias).Error)

	asset := &model.SeedanceAsset{
		UserID: 7, ChannelID: source.Id, GroupID: "group", AssetID: "remote-asset", Name: "portrait.jpg",
		AssetType: "Image", Status: "Deleting", KeyFingerprint: client.KeyFingerprint,
		AccountFingerprint: client.AccountFingerprint,
	}
	require.NoError(t, db.Create(asset).Error)
	job := NewSeedanceAssetDeleteCleanupJob(asset)
	require.NoError(t, model.CreateSeedanceAssetCleanupJob(&job))

	summary, err := RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Completed)
	assert.EqualValues(t, 1, deleteCalls.Load())
	var remaining int64
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
}

func TestSeedanceReplicaCleanupDoesNotRequireChannelAfterRemoteDelete(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	replica := &model.SeedanceAssetReplica{
		UserID: 7, LocalGroupID: 3, LocalAssetID: 9, ChannelID: 999,
		AccountFingerprint: "retired-account", KeyFingerprint: "retired-key",
		UpstreamGroupID: "remote-group", UpstreamAssetID: "remote-asset",
		ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active",
	}
	require.NoError(t, db.Create(replica).Error)
	job := &model.SeedanceAssetCleanupJob{
		Kind: model.SeedanceAssetCleanupKindReplicaDelete, UserID: replica.UserID,
		LocalAssetID: replica.LocalAssetID, LocalMappingID: replica.ID,
		ChannelID: replica.ChannelID, KeyFingerprint: replica.KeyFingerprint,
		UpstreamID: replica.UpstreamAssetID, UpstreamDoneAt: common.GetTimestamp(),
	}

	require.NoError(t, executeSeedanceAssetCleanup(t.Context(), job))
	var remaining int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("id = ?", replica.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)

	asset := &model.SeedanceAsset{
		UserID: 7, ChannelID: 999, GroupID: "retired-group", AssetID: "remote-old-asset",
		Name: "old.jpg", AssetType: "Image", Status: "Deleting",
	}
	require.NoError(t, db.Create(asset).Error)
	assetJob := &model.SeedanceAssetCleanupJob{
		Kind: model.SeedanceAssetCleanupKindAssetDelete, UserID: asset.UserID,
		LocalAssetID: asset.ID, ChannelID: asset.ChannelID, KeyFingerprint: "retired-key",
		UpstreamID: asset.AssetID, UpstreamDoneAt: common.GetTimestamp(),
	}
	require.NoError(t, executeSeedanceAssetCleanup(t.Context(), assetJob))
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
}

// OSS 删除失败时必须留下可恢复的对象清理任务，不能只依赖一次请求内的日志。
func TestRollbackSeedanceAssetQueuesFailedOSSCleanup(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SeedanceAssetCleanupJob{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	client := useFakePrivateAssetOSSStorage(t)
	upload, err := StoreSeedanceAssetUpload(t.Context(), strings.NewReader("image fixture"), "cover.png", "image/png", "", int64(len("image fixture")), 42)
	require.NoError(t, err)
	client.deleteErr = errors.New("OSS unavailable")
	err = RollbackSeedanceAsset(t.Context(), &SeedanceAssetClient{}, "", upload.ObjectKey, upload)
	require.ErrorContains(t, err, "OSS asset cleanup failed")

	var jobs []model.SeedanceAssetCleanupJob
	require.NoError(t, db.Find(&jobs).Error)
	require.Len(t, jobs, 1)
	assert.Equal(t, model.SeedanceAssetCleanupKindOSSObject, jobs[0].Kind)
	assert.Equal(t, upload.ObjectKey, jobs[0].ObjectKey)
}
