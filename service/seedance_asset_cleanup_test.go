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
	"time"

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
	require.NoError(t, db.Create(seedanceAssetTestChannel(t, channel)).Error)
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
	assert.Eventually(t, func() bool { return deleteCalls.Load() == 1 }, 2*time.Second, 10*time.Millisecond)

	var remaining int64
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
	var storedJob model.SeedanceAssetCleanupJob
	assert.ErrorIs(t, db.First(&storedJob, job.ID).Error, gorm.ErrRecordNotFound)
}

// 素材组清理独立删除本地组、素材和清理任务，并尽力请求上游删除。
func TestRunSeedanceAssetGroupCleanupOnceDeletesRemoteAndLocal(t *testing.T) {
	previousDB := model.DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetCleanupJob{}, &model.SeedanceAssetValidationSession{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	var deleteGroupCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("Action") == "DeleteAssetGroup" {
			deleteGroupCalls.Add(1)
			_, _ = writer.Write([]byte(`{"code":"NotFound.20260929104047407D4D891C847F1BBB31","message":"resource does not exist"}`))
			return
		}
		_, _ = writer.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	channel := &model.Channel{Id: 803, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(seedanceAssetTestChannel(t, channel)).Error)
	group := &model.SeedanceAssetGroup{UserID: 9, ChannelID: channel.Id, GroupID: "group-delete", Name: "Delete me", Status: "Deleting"}
	require.NoError(t, db.Create(group).Error)
	replica := &model.SeedanceAssetGroupReplica{
		ID: 99, UserID: group.UserID, LocalGroupID: group.ID, ChannelID: channel.Id,
		UpstreamGroupID: "replica-group-delete", GroupName: group.Name, Status: "Deleting",
	}
	require.NoError(t, db.Create(replica).Error)
	asset := &model.SeedanceAsset{UserID: 9, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "asset-delete", Name: "cover.png", AssetType: "Image", Status: "Deleting"}
	require.NoError(t, db.Create(asset).Error)
	job := NewSeedanceAssetGroupDeleteCleanupJob(group)

	summary, err := RunSeedanceAssetCleanupOnce(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Completed)
	assert.Eventually(t, func() bool { return deleteGroupCalls.Load() == 2 }, 2*time.Second, 10*time.Millisecond)
	var remaining int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Where("id = ?", group.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
	require.NoError(t, db.Model(&model.SeedanceAssetGroupReplica{}).Where("id = ?", replica.ID).Count(&remaining).Error)
	assert.Zero(t, remaining)
	assert.ErrorIs(t, db.Where("dedup_key = ?", job.DedupKey).First(&model.SeedanceAssetCleanupJob{}).Error, gorm.ErrRecordNotFound)
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
	require.NoError(t, db.Create(seedanceAssetTestChannel(t, channel)).Error)
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
	client, err := NewSeedanceAssetClient(seedanceAssetTestChannel(t, source))
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
	assert.Eventually(t, func() bool { return deleteCalls.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
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

func TestSeedanceCleanupDeletesLocallyWhenUpstreamFails(t *testing.T) {
	db := model.DB
	require.NoError(t, db.AutoMigrate(&model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetSize{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetCleanupJob{}))
	for _, scenario := range []string{"removed", "primary-removed", "replaced-key", "enabled-alias", "disabled", "upstream-failure", "primary-upstream-failure", "unsupported-delete", "oss-failure", "database-failure", "single-asset", "single-upstream-failure"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if scenario == "upstream-failure" || scenario == "primary-upstream-failure" || scenario == "single-upstream-failure" {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"code":"temporary_failure","message":"retry later"}`))
					return
				}
				if scenario == "unsupported-delete" {
					_, _ = w.Write([]byte(`{"code":"unsupported_action","message":"当前素材接口不支持 DeleteAsset"}`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer upstream.Close()
			ossClient := useFakePrivateAssetOSSStorage(t)
			channel := seedanceAssetTestChannel(t, &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-retired-key", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled})
			require.NoError(t, db.Create(channel).Error)
			t.Cleanup(func() { require.NoError(t, db.Delete(channel).Error) })
			client, err := NewSeedanceAssetClient(channel)
			require.NoError(t, err)
			count := int64(1)
			group := &model.SeedanceAssetGroup{UserID: 733, ChannelID: channel.Id, GroupID: "cleanup-group-" + scenario, Name: scenario, Status: "Deleting", AssetCount: &count, UpstreamDeletedAt: 1}
			if scenario == "primary-removed" || scenario == "primary-upstream-failure" {
				group.UpstreamDeletedAt = 0
				group.KeyFingerprint, group.AccountFingerprint = client.KeyFingerprint, client.AccountFingerprint
			}
			require.NoError(t, db.Create(group).Error)
			asset := &model.SeedanceAsset{UserID: group.UserID, GroupID: group.GroupID, ChannelID: channel.Id, AssetID: "cleanup-asset-" + scenario, Name: "fixture.jpg", AssetType: "Image", Status: "Deleting", ObjectKey: "private-assets/fixture.jpg", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint}
			require.NoError(t, db.Create(asset).Error)
			require.NoError(t, db.Create(&model.SeedanceAssetSize{AssetID: asset.ID, UserID: asset.UserID, Bytes: 1024}).Error)
			replica := &model.SeedanceAssetGroupReplica{UserID: group.UserID, LocalGroupID: group.ID, ChannelID: channel.Id, UpstreamGroupID: "replica-" + scenario, GroupName: scenario, Status: model.SeedanceAssetReplicaReady, KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint}
			require.NoError(t, db.Create(replica).Error)
			t.Cleanup(func() {
				for _, table := range []any{&model.SeedanceAssetCleanupJob{}, &model.SeedanceAssetSize{}, &model.SeedanceAssetReplica{}, &model.SeedanceAsset{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetGroup{}} {
					require.NoError(t, db.Where("user_id = ?", group.UserID).Delete(table).Error)
				}
			})
			switch scenario {
			case "removed", "primary-removed", "enabled-alias", "oss-failure", "single-asset":
				require.NoError(t, db.Delete(channel).Error)
			case "replaced-key":
				require.NoError(t, db.Model(channel).Update("key", "fixture-replacement-key").Error)
			case "disabled":
				require.NoError(t, db.Model(channel).Update("status", common.ChannelStatusManuallyDisabled).Error)
			}
			if scenario == "enabled-alias" {
				alias := *channel
				alias.Id = 0
				require.NoError(t, db.Create(&alias).Error)
				t.Cleanup(func() { require.NoError(t, db.Delete(&alias).Error) })
			}
			if scenario == "oss-failure" {
				ossClient.deleteErr = errors.New("fixture OSS unavailable")
			}
			if scenario == "database-failure" {
				require.NoError(t, db.Callback().Query().Before("gorm:query").Register("fixture:channel_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "seedance_assets" {
						_ = tx.AddError(errors.New("fixture database unavailable"))
					}
				}))
				t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove("fixture:channel_failure")) })
			}
			job := NewSeedanceAssetGroupDeleteCleanupJob(group)
			if scenario == "single-asset" || scenario == "single-upstream-failure" {
				mapping := &model.SeedanceAssetReplica{UserID: asset.UserID, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channel.Id, AccountFingerprint: client.AccountFingerprint, KeyFingerprint: client.KeyFingerprint, UpstreamAssetID: "replica-asset", UpstreamGroupID: replica.UpstreamGroupID, Status: "Active", ProvisionStatus: model.SeedanceAssetReplicaReady}
				require.NoError(t, db.Create(mapping).Error)
				jobs := NewSeedanceAssetDeleteCleanupJobs(asset, []model.SeedanceAssetReplica{*mapping})
				for index := range jobs {
					require.NoError(t, model.CreateSeedanceAssetCleanupJob(&jobs[index]))
				}
				require.NoError(t, executeSeedanceAssetCleanup(t.Context(), &jobs[1]))
				require.NoError(t, executeSeedanceAssetCleanup(t.Context(), &jobs[0]))
				assert.Zero(t, jobs[0].UpstreamDoneAt)
				assert.Zero(t, jobs[1].UpstreamDoneAt)
			} else {
				require.NoError(t, model.CreateSeedanceAssetCleanupJob(&job))
				summary, err := RunSeedanceAssetCleanupOnce(t.Context(), nil)
				require.NoError(t, err)
				failure := scenario == "oss-failure" || scenario == "database-failure"
				var stored model.SeedanceAssetCleanupJob
				if failure {
					require.NoError(t, db.First(&stored, job.ID).Error)
					assert.Equal(t, 1, summary.RetryScheduled)
					assert.NotEmpty(t, stored.LastError)
					assert.Equal(t, model.SeedanceAssetCleanupStatusPending, stored.Status)
					require.NoError(t, db.First(replica, replica.ID).Error)
					return
				}
				assert.Equal(t, 1, summary.Completed)
				var remaining int64
				require.NoError(t, db.Model(&model.SeedanceAssetCleanupJob{}).Where("id = ?", job.ID).Count(&remaining).Error)
				assert.Zero(t, remaining)
				require.NoError(t, db.Model(&model.SeedanceAssetCleanupJob{}).Where("user_id = ?", group.UserID).Count(&remaining).Error)
				assert.Zero(t, remaining, "上游失败不能留下额外清理记录")
				require.NoError(t, db.Model(&model.SeedanceAssetGroupReplica{}).Where("id = ?", replica.ID).Count(&remaining).Error)
				assert.Zero(t, remaining)
				require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Where("id = ?", group.ID).Count(&remaining).Error)
				assert.Zero(t, remaining)
			}
			assert.Equal(t, asset.ObjectKey, ossClient.deletedKey)
			for _, table := range []any{&model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetSize{}} {
				var remaining int64
				require.NoError(t, db.Model(table).Where("user_id = ?", asset.UserID).Count(&remaining).Error)
				assert.Zero(t, remaining)
			}
			if scenario == "primary-upstream-failure" || scenario == "single-upstream-failure" {
				assert.Eventually(t, func() bool { return calls.Load() == 2 }, 2*time.Second, 10*time.Millisecond)
			} else if scenario == "enabled-alias" || scenario == "upstream-failure" || scenario == "unsupported-delete" {
				assert.Eventually(t, func() bool { return calls.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
			} else {
				assert.Zero(t, calls.Load())
			}
		})
	}
}

func TestSeedanceCleanupDoesNotWaitForSlowUpstream(t *testing.T) {
	db := model.DB
	if !common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		t.Skip("需要隔离的 MySQL 测试数据库")
	}
	require.NoError(t, db.AutoMigrate(&model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetSize{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetCleanupJob{}, &model.SeedanceAssetValidationSession{}))
	for _, kind := range []string{"asset", "group"} {
		t.Run(kind, func(t *testing.T) {
			started := make(chan struct{}, 8)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				started <- struct{}{}
				<-release
				http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
			}))
			defer server.Close()
			ossClient := useFakePrivateAssetOSSStorage(t)
			channel := seedanceAssetTestChannel(t, &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled})
			require.NoError(t, db.Create(channel).Error)
			client, err := NewSeedanceAssetClient(channel)
			require.NoError(t, err)
			count := int64(1)
			group := &model.SeedanceAssetGroup{UserID: 983431, ChannelID: channel.Id, GroupID: "slow-" + kind, Name: kind, Status: "Active", AssetCount: &count, KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint}
			require.NoError(t, db.Create(group).Error)
			asset := &model.SeedanceAsset{UserID: group.UserID, GroupID: group.GroupID, ChannelID: channel.Id, AssetID: "slow-asset-" + kind, Name: "fixture.jpg", AssetType: "Image", Status: "Active", ObjectKey: "private-assets/slow.jpg", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint}
			require.NoError(t, db.Create(asset).Error)
			leaseUntil := common.GetTimestamp() + 60
			replica := &model.SeedanceAssetReplica{UserID: asset.UserID, LocalGroupID: group.ID, LocalAssetID: asset.ID, ChannelID: channel.Id, KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint, UpstreamGroupID: "remote-group", UpstreamAssetID: "remote-copy", Status: "Active", ProvisionStatus: model.SeedanceAssetReplicaCreating, PollLeaseUntil: leaseUntil}
			require.NoError(t, db.Create(replica).Error)
			groupReplica := &model.SeedanceAssetGroupReplica{UserID: group.UserID, LocalGroupID: group.ID, ChannelID: channel.Id, KeyFingerprint: client.KeyFingerprint, AccountFingerprint: client.AccountFingerprint, UpstreamGroupID: "remote-group", GroupName: kind, Status: model.SeedanceAssetReplicaReady}
			require.NoError(t, db.Create(groupReplica).Error)
			t.Cleanup(func() {
				for _, table := range []any{&model.SeedanceAssetCleanupJob{}, &model.SeedanceAssetSize{}, &model.SeedanceAssetReplica{}, &model.SeedanceAsset{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetGroup{}} {
					require.NoError(t, db.Where("user_id = ?", group.UserID).Delete(table).Error)
				}
				require.NoError(t, db.Delete(channel).Error)
			})
			if kind == "asset" {
				require.NoError(t, model.QueueSeedanceAssetDeletion(t.Context(), NewSeedanceAssetDeleteCleanupJobs(asset, []model.SeedanceAssetReplica{*replica})))
			} else {
				require.NoError(t, model.QueueSeedanceAssetGroupDeletion(t.Context(), NewSeedanceAssetGroupDeleteCleanupJob(group)))
			}
			updated, err := model.CompleteSeedanceAssetReplica(t.Context(), replica.ID, leaseUntil, "remote-group", "late-copy", common.GetTimestamp())
			require.NoError(t, err)
			assert.False(t, updated, "删除屏障必须阻止在途创建写回")
			type result struct {
				summary SeedanceAssetCleanupSummary
				err     error
			}
			done := make(chan result, 1)
			go func() {
				summary, err := RunSeedanceAssetCleanupOnce(context.Background(), nil)
				done <- result{summary, err}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				close(release)
				<-done
				t.Fatal("模拟上游未收到删除请求")
			}
			var completed bool
			var outcome result
			select {
			case outcome = <-done:
				completed = true
			case <-time.After(time.Second):
			}
			var assetCount, replicaCount, groupCount, groupReplicaCount int64
			require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("id = ?", asset.ID).Count(&assetCount).Error)
			require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("id = ?", replica.ID).Count(&replicaCount).Error)
			require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Where("id = ?", group.ID).Count(&groupCount).Error)
			require.NoError(t, db.Model(&model.SeedanceAssetGroupReplica{}).Where("id = ?", groupReplica.ID).Count(&groupReplicaCount).Error)
			t.Logf("上游未响应时: 清理完成=%v, 素材=%d, 素材映射=%d, 组=%d", completed, assetCount, replicaCount, groupCount)
			close(release)
			if !completed {
				outcome = <-done
			}
			assert.True(t, completed, "本地清理不得等待上游响应")
			assert.Zero(t, assetCount)
			assert.Zero(t, replicaCount)
			if kind == "group" {
				assert.Zero(t, groupCount)
				assert.Zero(t, groupReplicaCount)
			}
			if completed {
				assert.Equal(t, asset.ObjectKey, ossClient.deletedKey)
			}
			require.NoError(t, outcome.err)
			assert.Zero(t, outcome.summary.RetryScheduled)
			var remainingJobs int64
			require.NoError(t, db.Model(&model.SeedanceAssetCleanupJob{}).Where("user_id = ?", group.UserID).Count(&remainingJobs).Error)
			assert.Zero(t, remainingJobs, "上游失败不得产生额外清理记录")
		})
	}
}

func TestSeedanceUpstreamCompensationStillRetriesMySQL(t *testing.T) {
	db := model.DB
	if !common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		t.Skip("需要隔离的 MySQL 测试数据库")
	}
	require.NoError(t, db.AutoMigrate(&model.SeedanceAssetCleanupJob{}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"code":"temporary_failure","message":"retry later"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	channel := seedanceAssetTestChannel(t, &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled})
	require.NoError(t, db.Create(channel).Error)
	job := &model.SeedanceAssetCleanupJob{Kind: model.SeedanceAssetCleanupKindUpstreamAsset, ChannelID: channel.Id, UpstreamID: "orphan", DedupKey: "mysql-slow-delete-compensation", Status: model.SeedanceAssetCleanupStatusPending, NextAttemptAt: common.GetTimestamp()}
	require.NoError(t, db.Create(job).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Delete(job).Error)
		require.NoError(t, db.Delete(channel).Error)
	})
	summary, err := RunSeedanceAssetCleanupOnce(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.RetryScheduled)
	var stored model.SeedanceAssetCleanupJob
	require.NoError(t, db.First(&stored, job.ID).Error)
	assert.Equal(t, 1, stored.Attempts)
	assert.NotEmpty(t, stored.LastError)
}
