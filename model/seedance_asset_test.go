package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestSeedanceAssetPollingClaimRejectsStaleReaders 验证后台轮询租约和版本条件能够阻止旧读取结果覆盖新状态。
func TestSeedanceAssetPollingClaimRejectsStaleReaders(t *testing.T) {
	previousDB := DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&SeedanceAsset{}))
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	asset := &SeedanceAsset{
		UserID: 1, ChannelID: 2, GroupID: "group-1", AssetID: "asset-1",
		Name: "cover.png", AssetType: "Image", Status: "Processing",
	}
	require.NoError(t, db.Create(asset).Error)

	var loaded SeedanceAsset
	require.NoError(t, db.First(&loaded, asset.ID).Error)
	now := time.Now().Unix()
	claimed, err := ClaimSeedanceAssetForPolling(loaded.ID, loaded.UpdatedAt, now, now+90)
	require.NoError(t, err)
	require.True(t, claimed)

	updated, err := UpdateSeedanceAssetIfUnchanged(loaded.ID, loaded.UpdatedAt, map[string]any{"status": "Active"})
	require.NoError(t, err)
	require.False(t, updated)

	var current SeedanceAsset
	require.NoError(t, db.First(&current, asset.ID).Error)
	require.Equal(t, "Processing", current.Status)
	require.Equal(t, now+90, current.PollLeaseUntil)
}

// TestListDueSeedanceAssetsHonorsBackoff 验证退避期间的素材不会重复进入轮询批次。
func TestListDueSeedanceAssetsHonorsBackoff(t *testing.T) {
	previousDB := DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&SeedanceAsset{}))
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	asset := &SeedanceAsset{
		UserID: 1, ChannelID: 2, GroupID: "group-1", AssetID: "asset-2",
		Name: "clip.mp4", AssetType: "Video", Status: "pending", NextPollAt: 200,
	}
	require.NoError(t, db.Create(asset).Error)

	due, err := ListDueSeedanceAssets(100, 20)
	require.NoError(t, err)
	require.Empty(t, due)

	require.NoError(t, db.Model(asset).Update("next_poll_at", 100).Error)
	due, err = ListDueSeedanceAssets(100, 20)
	require.NoError(t, err)
	require.Len(t, due, 1)
}

// TestSeedanceAssetGroupNameIsUniquePerUser 验证同一用户不能创建重复或仅大小写不同的素材组名称。
func TestSeedanceAssetGroupNameIsUniquePerUser(t *testing.T) {
	previousDB := DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}))
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	first := &SeedanceAssetGroup{UserID: 7, ChannelID: 1, GroupID: "group-1", Name: "Drafts"}
	require.NoError(t, db.Create(first).Error)

	duplicated, err := IsSeedanceAssetGroupNameDuplicated(0, first.UserID, " drafts ")
	require.NoError(t, err)
	require.True(t, duplicated)

	duplicated, err = IsSeedanceAssetGroupNameDuplicated(first.ID, first.UserID, first.Name)
	require.NoError(t, err)
	require.False(t, duplicated)

	duplicate := &SeedanceAssetGroup{UserID: first.UserID, ChannelID: 1, GroupID: "group-2", Name: "drafts"}
	require.Error(t, db.Create(duplicate).Error)

	differentUser := &SeedanceAssetGroup{UserID: 8, ChannelID: 1, GroupID: "group-3", Name: first.Name}
	require.NoError(t, db.Create(differentUser).Error)
}

// 同一补偿目标重复入队时只保留一条记录，避免故障恢复后重复调用外部删除接口。
func TestSeedanceAssetCleanupJobIsDeduplicated(t *testing.T) {
	previousDB := DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&SeedanceAssetCleanupJob{}))
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	first := &SeedanceAssetCleanupJob{
		Kind: SeedanceAssetCleanupKindOSSObject, DedupKey: "same-cleanup-target",
		Status: SeedanceAssetCleanupStatusPending, NextAttemptAt: 100,
	}
	require.NoError(t, CreateSeedanceAssetCleanupJob(first))
	second := &SeedanceAssetCleanupJob{
		Kind: SeedanceAssetCleanupKindOSSObject, DedupKey: first.DedupKey,
		Status: SeedanceAssetCleanupStatusPending, NextAttemptAt: 100,
	}
	require.NoError(t, CreateSeedanceAssetCleanupJob(second))

	var count int64
	require.NoError(t, db.Model(&SeedanceAssetCleanupJob{}).Count(&count).Error)
	require.Equal(t, int64(1), count)
}
