package model

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
			previousDB := DB
			DB = db
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(&SeedanceAsset{}, &SeedanceAssetGroup{}))
				DB = previousDB
				assert.NoError(t, connection.Close())
			})
			// 基线没有素材表；首次创建后插入真实记录，再重复迁移确认数据与约束保留。
			require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAsset{}, &SeedanceAssetCleanupJob{}))
			group := SeedanceAssetGroup{UserID: 11, ChannelID: 7, GroupID: "group-owned", Name: "素材组", KeyFingerprint: "bound-account"}
			require.NoError(t, db.Create(&group).Error)
			asset := SeedanceAsset{UserID: group.UserID, ChannelID: group.ChannelID, GroupID: group.GroupID, AssetID: "asset-owned", Name: "视频", AssetType: "Video", Status: "Processing", KeyFingerprint: group.KeyFingerprint, ObjectKey: "private-assets/video.mp4", Storage: SeedanceAssetStorage{Region: "cn-hangzhou", Bucket: "fixture-bucket", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com"}}
			require.NoError(t, CreateSeedanceAssetInGroup(context.Background(), &asset))
			require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAsset{}, &SeedanceAssetCleanupJob{}))
			require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAsset{}))
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
			late := asset
			late.ID, late.AssetID = 0, "late-asset"
			assert.Error(t, CreateSeedanceAssetInGroup(context.Background(), &late))
		})
	}
}
