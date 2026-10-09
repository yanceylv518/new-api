package model

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// TestSeedanceAssetDatabaseLifecycle 在隔离表中验证三种数据库的迁移、归属和状态竞争。
// 外部数据库仅由显式测试 DSN 启用，不读取应用的生产连接配置。
func TestSeedanceAssetDatabaseLifecycle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialector gorm.Dialector
			switch engine {
			case "mysql":
				dsn := os.Getenv("SEEDANCE_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("SEEDANCE_TEST_MYSQL_DSN is unset")
				}
				dialector = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("SEEDANCE_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("SEEDANCE_TEST_POSTGRES_DSN is unset")
				}
				dialector = postgres.Open(dsn)
			default:
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "assets.db"))
			}
			db, err := gorm.Open(dialector, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "seedance_port_test_"}})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			if engine == "sqlite" {
				connection.SetMaxOpenConns(1)
			}
			previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
			common.SetDatabaseTypes(common.DatabaseType(engine), common.DatabaseType(engine))
			t.Cleanup(func() { common.SetDatabaseTypes(previousMain, previousLog) })
			previousDB := DB
			DB = db
			previousCount, previousBytes := constant.PrivateAssetUserMaxCount, constant.PrivateAssetUserMaxBytes
			constant.PrivateAssetUserMaxCount, constant.PrivateAssetUserMaxBytes = 1000, 1<<30
			t.Cleanup(func() {
				constant.PrivateAssetUserMaxCount, constant.PrivateAssetUserMaxBytes = previousCount, previousBytes
			})
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(&SeedanceAssetSize{}, &SeedanceAssetCleanupJob{}, &SeedanceAssetReplica{}, &SeedanceAsset{}, &SeedanceAssetGroupReplica{}, &SeedanceAssetGroup{}))
				DB = previousDB
				assert.NoError(t, connection.Close())
			})
			// 先保存没有大小表的历史素材，再升级并重复迁移，确认已有记录仍可使用。
			require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAssetGroupReplica{}, &SeedanceAsset{}, &SeedanceAssetReplica{}, &SeedanceAssetCleanupJob{}))
			legacy := SeedanceAsset{UserID: 77, AssetID: "legacy-size-unknown", ObjectKey: "fixture/legacy.jpg", Status: "Active"}
			require.NoError(t, db.Create(&legacy).Error)
			require.NoError(t, db.AutoMigrate(&SeedanceAssetSize{}))
			require.NoError(t, db.AutoMigrate(&SeedanceAssetSize{}))
			var legacyStored SeedanceAsset
			require.NoError(t, db.First(&legacyStored, legacy.ID).Error)
			assert.Equal(t, legacy.ObjectKey, legacyStored.ObjectKey)
			limit := constant.PrivateAssetUserMaxBytes
			constant.PrivateAssetUserMaxBytes = 49 << 20
			require.Error(t, CheckSeedanceAssetCapacity(t.Context(), 77, 0))
			constant.PrivateAssetUserMaxBytes = limit
			t.Run("capacity_is_shared_across_groups_and_released_on_delete", func(t *testing.T) {
				count, bytes := constant.PrivateAssetUserMaxCount, constant.PrivateAssetUserMaxBytes
				constant.PrivateAssetUserMaxCount, constant.PrivateAssetUserMaxBytes = 2, 100
				defer func() { constant.PrivateAssetUserMaxCount, constant.PrivateAssetUserMaxBytes = count, bytes }()
				for _, name := range []string{"quota-a", "quota-b"} {
					require.NoError(t, db.Create(&SeedanceAssetGroup{UserID: 31, ChannelID: 7, GroupID: name, Name: name, KeyFingerprint: "account"}).Error)
				}
				results := make(chan error, 20)
				var workers sync.WaitGroup
				for index := range 20 {
					workers.Go(func() {
						group := "quota-a"
						if index%2 == 1 {
							group = "quota-b"
						}
						asset := &SeedanceAsset{UserID: 31, ChannelID: 7, GroupID: group, KeyFingerprint: "account", AssetID: fmt.Sprintf("quota-%d", index), Name: "fixture", AssetType: "Image", Status: "Active", ObjectKey: fmt.Sprintf("fixture/%d", index), UploadBytes: 40}
						results <- CreateSeedanceAssetInGroup(t.Context(), asset)
					})
				}
				workers.Wait()
				close(results)
				accepted := 0
				for err := range results {
					if err == nil {
						accepted++
					} else {
						assert.Contains(t, err.Error(), "limit exceeded")
					}
				}
				assert.Equal(t, 2, accepted)
				var asset SeedanceAsset
				require.NoError(t, db.Where("user_id = ?", 31).First(&asset).Error)
				require.NoError(t, DeleteSeedanceAssetWithCount(t.Context(), 31, asset.ID, ""))
				require.Error(t, CheckSeedanceAssetCapacity(t.Context(), 31, 61))
				require.NoError(t, CheckSeedanceAssetCapacity(t.Context(), 31, 60))
				var sizes int64
				require.NoError(t, db.Model(&SeedanceAssetSize{}).Where("user_id = ?", 31).Count(&sizes).Error)
				assert.EqualValues(t, 1, sizes)
				// 禁用容量限制后删除也清理大小记录，再启用不会残留旧对象的占用。
				constant.PrivateAssetUserMaxBytes = 0
				asset = SeedanceAsset{}
				require.NoError(t, db.Where("user_id = ?", 31).First(&asset).Error)
				require.NoError(t, DeleteSeedanceAssetWithCount(t.Context(), 31, asset.ID, ""))
				require.NoError(t, db.Model(&SeedanceAssetSize{}).Where("user_id = ?", 31).Count(&sizes).Error)
				assert.Zero(t, sizes)
			})
			require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAssetGroupReplica{}, &SeedanceAsset{}, &SeedanceAssetReplica{}, &SeedanceAssetCleanupJob{}))
			group := SeedanceAssetGroup{UserID: 11, ChannelID: 7, GroupID: "group-owned", Name: "素材组", KeyFingerprint: "bound-account"}
			require.NoError(t, db.Create(&group).Error)
			asset := SeedanceAsset{UserID: group.UserID, ChannelID: group.ChannelID, GroupID: group.GroupID, AssetID: "asset-owned", Name: "视频", AssetType: "Video", Status: "Processing", KeyFingerprint: group.KeyFingerprint, ObjectKey: "private-assets/video.mp4", Storage: SeedanceAssetStorage{Region: "cn-hangzhou", Bucket: "fixture-bucket", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com"}}
			require.NoError(t, CreateSeedanceAssetInGroup(context.Background(), &asset))
			require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAssetGroupReplica{}, &SeedanceAsset{}, &SeedanceAssetReplica{}, &SeedanceAssetCleanupJob{}))
			require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAssetGroupReplica{}, &SeedanceAsset{}, &SeedanceAssetReplica{}))
			var stored SeedanceAsset
			require.NoError(t, db.First(&stored, asset.ID).Error)
			assert.Equal(t, asset.Storage, stored.Storage)
			assert.Equal(t, asset.ObjectKey, stored.ObjectKey)
			duplicate := SeedanceAssetGroup{UserID: group.UserID, ChannelID: 7, GroupID: "duplicate", Name: group.Name}
			assert.Error(t, db.Create(&duplicate).Error)
			otherUser := SeedanceAssetGroup{UserID: 12, ChannelID: 7, GroupID: "other-group", Name: group.Name}
			require.NoError(t, db.Create(&otherUser).Error)
			foreign := asset
			foreign.ID, foreign.UserID, foreign.AssetID = 0, 12, "foreign-asset"
			assert.Error(t, CreateSeedanceAssetInGroup(context.Background(), &foreign))
			// 使用数据库读回的时间精度抢占，确保 MySQL 毫秒时间不会破坏 CAS。
			now := time.Now().Unix()
			won, err := ClaimSeedanceAssetForPolling(stored.ID, stored.UpdatedAt, now, now+90)
			require.NoError(t, err)
			require.True(t, won)
			won, err = ClaimSeedanceAssetForPolling(stored.ID, stored.UpdatedAt, now, now+90)
			require.NoError(t, err)
			assert.False(t, won)
			won, err = UpdateSeedanceAssetPollState(stored.ID, now+90, map[string]any{"status": "Active", "poll_lease_until": 0})
			require.NoError(t, err)
			require.True(t, won)
			won, err = UpdateSeedanceAssetPollState(stored.ID, now+90, map[string]any{"status": "Failed"})
			require.NoError(t, err)
			assert.False(t, won)
			require.NoError(t, db.First(&stored, asset.ID).Error)
			assert.Equal(t, "Active", stored.Status)
			// 删除意图落库后，在途上传不能再产生孤儿记录。
			require.NoError(t, db.Model(&group).Update("status", "Deleting").Error)
			deletionJob := SeedanceAssetCleanupJob{
				Kind:          SeedanceAssetCleanupKindGroupDelete,
				UserID:        group.UserID,
				LocalGroupID:  group.ID,
				DedupKey:      "seedance-group-delete-integration",
				Status:        SeedanceAssetCleanupStatusPending,
				NextAttemptAt: time.Now().Unix(),
			}
			require.NoError(t, QueueSeedanceAssetGroupDeletion(context.Background(), deletionJob))
			require.NoError(t, QueueSeedanceAssetGroupDeletion(context.Background(), deletionJob))
			var deletionJobs []SeedanceAssetCleanupJob
			require.NoError(t, db.Where("dedup_key = ?", deletionJob.DedupKey).Find(&deletionJobs).Error)
			require.Len(t, deletionJobs, 1)
			assert.Equal(t, SeedanceAssetCleanupKindGroupDelete, deletionJobs[0].Kind)
			late := asset
			late.ID, late.AssetID = 0, "late-asset"
			assert.Error(t, CreateSeedanceAssetInGroup(context.Background(), &late))
		})
	}
}
