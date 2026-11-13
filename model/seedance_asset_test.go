package model

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	driver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestSeedanceAssetValidationSessionSchemaMatrix(t *testing.T) {
	engines := []struct {
		name string
		dsn  string
		open func(string) gorm.Dialector
	}{
		{name: "sqlite", dsn: fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_")), open: sqlite.Open},
		{name: "mysql", dsn: os.Getenv("SEEDANCE_TEST_MYSQL_DSN"), open: mysql.Open},
		{name: "postgres", dsn: os.Getenv("SEEDANCE_TEST_POSTGRES_DSN"), open: postgres.Open},
	}
	for _, engine := range engines {
		engine := engine
		t.Run(engine.name, func(t *testing.T) {
			if engine.dsn == "" {
				t.Skip("database DSN is unset")
			}
			db, err := gorm.Open(engine.open(engine.dsn), &gorm.Config{})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = connection.Close() })
			require.NoError(t, db.AutoMigrate(&SeedanceAssetValidationSession{}))
			require.NoError(t, db.AutoMigrate(&SeedanceAssetValidationSession{}))
			require.True(t, db.Migrator().HasTable(&SeedanceAssetValidationSession{}))
			require.True(t, db.Migrator().HasColumn(&SeedanceAssetValidationSession{}, "project_name"))
			require.True(t, db.Migrator().HasColumn(&SeedanceAssetValidationSession{}, "byted_token_hash"))
		})
	}
}

func TestSeedanceAssetListSummaryTracksUpdatesAndBackfillsExistingRows(t *testing.T) {
	previousDB := DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAsset{}, &SeedanceAssetSchemaMigration{}))
	require.NoError(t, db.Exec("CREATE INDEX idx_seedance_asset_poll_due ON seedance_assets (status, next_poll_at, poll_lease_until, id)").Error)
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	assetCount := int64(0)
	group := &SeedanceAssetGroup{UserID: 1, ChannelID: 2, GroupID: "group-1", Name: "mock", AssetCount: &assetCount}
	require.NoError(t, db.Create(group).Error)
	asset := &SeedanceAsset{
		UserID: 1, ChannelID: 2, GroupID: "group-1", AssetID: "asset-1",
		Name: "cover.png", AssetType: "Image", Status: "pRoCeSsInG",
	}
	require.NoError(t, CreateSeedanceAssetInGroup(context.Background(), asset))
	require.NoError(t, db.First(group, group.ID).Error)
	require.NotNil(t, group.AssetCount)
	require.EqualValues(t, 1, *group.AssetCount)
	require.NotNil(t, asset.StatusKey)
	require.Equal(t, "processing", *asset.StatusKey)
	require.NotNil(t, asset.PendingStatus)
	require.True(t, *asset.PendingStatus)

	legacyAsset := &SeedanceAsset{
		UserID: 1, ChannelID: 2, GroupID: "group-1", AssetID: "asset-2",
		Name: "legacy.png", AssetType: "Image", Status: "pEnDiNg",
	}
	require.NoError(t, db.Create(legacyAsset).Error)
	require.NoError(t, db.Model(legacyAsset).UpdateColumn("status_key", nil).Error)
	require.NoError(t, db.Model(legacyAsset).UpdateColumn("pending_status", nil).Error)

	require.NoError(t, migrateSeedanceAssetListPerformance(db))
	require.False(t, db.Migrator().HasIndex(&SeedanceAsset{}, "idx_seedance_asset_poll_due"))
	require.NoError(t, db.First(legacyAsset, legacyAsset.ID).Error)
	require.NotNil(t, legacyAsset.StatusKey)
	require.Equal(t, "pending", *legacyAsset.StatusKey)
	require.NotNil(t, legacyAsset.PendingStatus)
	require.True(t, *legacyAsset.PendingStatus)
	require.NoError(t, db.First(group, group.ID).Error)
	require.NotNil(t, group.AssetCount)
	require.EqualValues(t, 2, *group.AssetCount)

	updated, err := UpdateSeedanceAssetIfUnchanged(asset.ID, asset.UpdatedAt, map[string]any{"status": "Active"})
	require.NoError(t, err)
	require.True(t, updated)
	require.NoError(t, db.First(asset, asset.ID).Error)
	require.NotNil(t, asset.StatusKey)
	require.Equal(t, "active", *asset.StatusKey)
	require.NotNil(t, asset.PendingStatus)
	require.False(t, *asset.PendingStatus)

	require.NoError(t, migrateSeedanceAssetListPerformance(db))
	require.NoError(t, DeleteSeedanceAssetWithCount(context.Background(), 1, legacyAsset.ID, "pEnDiNg"))
	require.NoError(t, db.First(group, group.ID).Error)
	require.EqualValues(t, 1, *group.AssetCount)
	require.NoError(t, DeleteSeedanceAssetWithCount(context.Background(), 1, legacyAsset.ID, "pEnDiNg"))
	require.NoError(t, db.First(group, group.ID).Error)
	require.EqualValues(t, 1, *group.AssetCount)
	var migrationCount int64
	require.NoError(t, db.Model(&SeedanceAssetSchemaMigration{}).Where("name = ? AND completed = ?", "seedance_asset_list_performance_v1", true).Count(&migrationCount).Error)
	require.EqualValues(t, 1, migrationCount)
}

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

type legacySeedanceAssetGroupName struct {
	ID        uint    `gorm:"primaryKey"`
	UserID    int     `gorm:"not null;uniqueIndex:idx_seedance_asset_group_user_name_key,priority:1"`
	ChannelID int     `gorm:"not null"`
	GroupID   string  `gorm:"size:128;not null"`
	Name      string  `gorm:"size:64;not null"`
	NameKey   *string `gorm:"size:64;uniqueIndex:idx_seedance_asset_group_user_name_key,priority:2"`
}

func (legacySeedanceAssetGroupName) TableName() string { return "seedance_asset_groups" }

func TestSeedanceAssetGroupNamesCanRepeat(t *testing.T) {
	var dialector gorm.Dialector = sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()))
	if dsn := os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN"); dsn != "" {
		dialector = mysql.Open(dsn)
	} else if dsn := os.Getenv("TASK_PLUGIN_TEST_POSTGRES_DSN"); dsn != "" {
		dialector = postgres.Open(dsn)
	}
	db, err := gorm.Open(dialector, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("sg%d_", time.Now().UnixNano())}})
	require.NoError(t, err)
	connection, err := db.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(1)
	t.Cleanup(func() {
		require.NoError(t, db.Migrator().DropTable(&SeedanceAssetGroup{}))
		require.NoError(t, connection.Close())
	})

	legacyDB := db.Table(db.NamingStrategy.TableName("SeedanceAssetGroup"))
	require.NoError(t, legacyDB.AutoMigrate(&legacySeedanceAssetGroupName{}))
	nameKey := "drafts"
	legacy := &legacySeedanceAssetGroupName{UserID: 7, ChannelID: 1, GroupID: "group-1", Name: "Drafts", NameKey: &nameKey}
	require.NoError(t, legacyDB.Create(legacy).Error)
	require.True(t, db.Migrator().HasIndex(&SeedanceAssetGroup{}, "idx_seedance_asset_group_user_name_key"))
	require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}))
	for range 2 {
		require.NoError(t, migrateSeedanceAssetGroupNameUniqueness(db))
	}
	require.False(t, db.Migrator().HasIndex(&SeedanceAssetGroup{}, "idx_seedance_asset_group_user_name_key"))
	var existing SeedanceAssetGroup
	require.NoError(t, db.First(&existing, legacy.ID).Error)
	require.Equal(t, "group-1", existing.GroupID)
	require.Equal(t, "Drafts", existing.Name)
	duplicate := &SeedanceAssetGroup{UserID: 7, ChannelID: 1, GroupID: "group-2", Name: " Drafts "}
	require.NoError(t, db.Create(duplicate).Error)
	require.NotEqual(t, existing.ID, duplicate.ID)
	require.Equal(t, "Drafts", duplicate.Name)
	require.NoError(t, db.Model(duplicate).Update("name", existing.Name).Error)
	var count int64
	require.NoError(t, db.Model(&SeedanceAssetGroup{}).Where("user_id = ? AND name = ?", 7, "Drafts").Count(&count).Error)
	require.EqualValues(t, 2, count)

	require.NoError(t, db.Migrator().DropTable(&SeedanceAssetGroup{}))
	require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}))
	for range 2 {
		require.NoError(t, migrateSeedanceAssetGroupNameUniqueness(db))
	}
	for _, groupID := range []string{"fresh-1", "fresh-2"} {
		require.NoError(t, db.Create(&SeedanceAssetGroup{UserID: 7, ChannelID: 1, GroupID: groupID, Name: "Drafts"}).Error)
	}
}

func TestSeedanceAssetGroupNamesUpgradeOnMySQLStartup(t *testing.T) {
	dsn := os.Getenv("SEEDANCE_GROUP_STARTUP_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("SEEDANCE_GROUP_STARTUP_TEST_MYSQL_DSN is unset")
	}
	config, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(config.DBName, "codex_seedance_names_"))
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	tables, err := db.Migrator().GetTables()
	require.NoError(t, err)
	require.Empty(t, tables, "startup migration requires an empty database")
	connection, err := db.DB()
	require.NoError(t, err)
	previousDB, previousLog := DB, LOG_DB
	DB, LOG_DB = db, db
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLog
		require.NoError(t, connection.Close())
	})
	require.NoError(t, DB.AutoMigrate(&legacySeedanceAssetGroupName{}, &SeedanceAsset{}))
	nameKey := "existing"
	group := &legacySeedanceAssetGroupName{UserID: 7, ChannelID: 1, GroupID: "existing-group", Name: "existing", NameKey: &nameKey}
	require.NoError(t, DB.Create(group).Error)
	asset := &SeedanceAsset{UserID: 7, ChannelID: 1, GroupID: group.GroupID, AssetID: "existing-asset", Name: "image", AssetType: "Image", Status: "Active"}
	require.NoError(t, DB.Create(asset).Error)
	require.True(t, DB.Migrator().HasIndex(&SeedanceAssetGroup{}, "idx_seedance_asset_group_user_name_key"))

	require.NoError(t, migrateDB())
	require.False(t, DB.Migrator().HasIndex(&SeedanceAssetGroup{}, "idx_seedance_asset_group_user_name_key"))
	require.NoError(t, DB.Create(&SeedanceAssetGroup{UserID: 7, ChannelID: 1, GroupID: "new-group", Name: group.Name}).Error)
	require.NoError(t, migrateDB())
	var groups []SeedanceAssetGroup
	require.NoError(t, DB.Where("user_id = ? AND name = ?", 7, group.Name).Order("id").Find(&groups).Error)
	require.Len(t, groups, 2)
	require.Equal(t, group.GroupID, groups[0].GroupID)
	require.Equal(t, "new-group", groups[1].GroupID)
	var stored SeedanceAsset
	require.NoError(t, DB.Where("asset_id = ?", asset.AssetID).First(&stored).Error)
	require.Equal(t, group.GroupID, stored.GroupID)
}

// TestSeedanceAssetValidationSessionPublishesOneGroup 验证认证结果只能发布一次真人素材组，
// 并且并发轮询通过租约被去重。
func TestSeedanceAssetValidationSessionPublishesOneGroup(t *testing.T) {
	previousDB := DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAssetValidationSession{}))
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		_ = sqlDB.Close()
	})
	for index := range 2 {
		creating := &SeedanceAssetValidationSession{
			UserID:             9,
			ChannelID:          2,
			KeyFingerprint:     "key-fingerprint",
			AccountFingerprint: "account-fingerprint",
			Name:               "创建中素材组",
			CallbackStateHash:  fmt.Sprintf("callback-state-%d", index),
			Status:             SeedanceAssetValidationStatusCreating,
			ExpiresAt:          time.Now().Add(time.Hour).Unix(),
		}
		require.NoError(t, db.Create(creating).Error)
	}

	tokenHash := "token-hash"
	session := &SeedanceAssetValidationSession{
		UserID:               9,
		ChannelID:            2,
		KeyFingerprint:       "key-fingerprint",
		AccountFingerprint:   "account-fingerprint",
		Name:                 "真人组",
		Description:          "经过认证的人物肖像",
		Tags:                 "演员, 已认证",
		BytedTokenHash:       &tokenHash,
		BytedTokenCiphertext: "ciphertext",
		Status:               SeedanceAssetValidationStatusPending,
		ExpiresAt:            time.Now().Add(time.Hour).Unix(),
	}
	require.NoError(t, db.Create(session).Error)
	now := time.Now().Unix()
	leaseUntil := now + 30
	claimed, err := ClaimSeedanceAssetValidationSession(session.ID, session.UserID, now, leaseUntil)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = ClaimSeedanceAssetValidationSession(session.ID, session.UserID, now, leaseUntil)
	require.NoError(t, err)
	require.False(t, claimed)

	group, err := CompleteSeedanceAssetValidationSession(context.Background(), session.ID, session.UserID, leaseUntil, now, "upstream-group")
	require.NoError(t, err)
	require.Equal(t, "LivenessFace", group.GroupType)
	require.Equal(t, "upstream-group", group.GroupID)
	require.Equal(t, "经过认证的人物肖像", group.Description)
	require.Equal(t, "演员, 已认证", group.Tags)

	groupAgain, err := CompleteSeedanceAssetValidationSession(context.Background(), session.ID, session.UserID, leaseUntil, now, "upstream-group")
	require.NoError(t, err)
	require.Equal(t, group.ID, groupAgain.ID)

	var saved SeedanceAssetValidationSession
	require.NoError(t, db.First(&saved, session.ID).Error)
	require.Equal(t, SeedanceAssetValidationStatusSucceeded, saved.Status)
	require.Empty(t, saved.BytedTokenCiphertext)
	require.Equal(t, group.ID, saved.LocalGroupID)
}

// TestSeedanceAssetReplicaIsScopedToAccount 验证不同目标凭证各有独立映射，创建租约可去重并恢复过期任务。
func TestSeedanceAssetReplicaIsScopedToAccount(t *testing.T) {
	previousDB := DB
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&SeedanceAssetGroup{}, &SeedanceAssetGroupReplica{}, &SeedanceAsset{}, &SeedanceAssetReplica{}))
	DB = db
	t.Cleanup(func() {
		DB = previousDB
		sqlDB, dbErr := db.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	group := &SeedanceAssetGroup{UserID: 7, ChannelID: 11, GroupID: "source-group", Name: "Videos", Status: "Active", KeyFingerprint: "source-key", AccountFingerprint: "source-account"}
	require.NoError(t, db.Create(group).Error)
	asset := &SeedanceAsset{UserID: 7, ChannelID: 11, GroupID: group.GroupID, AssetID: "source-asset", Name: "clip.mp4", AssetType: "Video", Status: "Active", KeyFingerprint: group.KeyFingerprint, AccountFingerprint: group.AccountFingerprint}
	require.NoError(t, CreateSeedanceAssetInGroup(context.Background(), asset))

	assets, err := FindSeedanceAssetsForUser(context.Background(), 7, []string{asset.AssetID})
	require.NoError(t, err)
	require.Len(t, assets, 1)
	_, err = FindSeedanceAssetsForUser(context.Background(), 8, []string{asset.AssetID})
	require.ErrorIs(t, err, ErrSeedanceAssetUnavailable)

	now := time.Now().Unix()
	leaseUntil := now + 90
	groupReplica, claimed, err := ClaimSeedanceAssetGroupReplica(context.Background(), 7, group.ID, 21, "account-a", "key-a", group.Name, now, leaseUntil)
	require.NoError(t, err)
	require.True(t, claimed)
	duplicateGroupClaim, claimed, err := ClaimSeedanceAssetGroupReplica(context.Background(), 7, group.ID, 21, "account-a", "key-a", group.Name, now, leaseUntil)
	require.NoError(t, err)
	require.False(t, claimed)
	require.Equal(t, groupReplica.ID, duplicateGroupClaim.ID)

	completed, err := CompleteSeedanceAssetGroupReplica(context.Background(), groupReplica.ID, leaseUntil, "remote-group-a", group.Name)
	require.NoError(t, err)
	require.True(t, completed)
	readyGroup, claimed, err := ClaimSeedanceAssetGroupReplica(context.Background(), 7, group.ID, 21, "account-a", "key-a", group.Name, now, leaseUntil)
	require.NoError(t, err)
	require.False(t, claimed)
	require.Equal(t, "remote-group-a", readyGroup.UpstreamGroupID)

	replica, claimed, err := ClaimSeedanceAssetReplica(context.Background(), asset, group.ID, 21, "account-a", "key-a", now, leaseUntil)
	require.NoError(t, err)
	require.True(t, claimed)
	completed, err = CompleteSeedanceAssetReplica(context.Background(), replica.ID, leaseUntil, "remote-group-a", "remote-asset-a", now)
	require.NoError(t, err)
	require.True(t, completed)

	var saved SeedanceAssetReplica
	require.NoError(t, db.First(&saved, replica.ID).Error)
	require.Equal(t, "Processing", saved.Status)
	due, err := ListDueSeedanceAssetReplicas(now, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	claimed, err = ClaimSeedanceAssetReplicaForPolling(saved.ID, saved.UpdatedAt, now, leaseUntil)
	require.NoError(t, err)
	require.True(t, claimed)
	updated, err := UpdateSeedanceAssetReplicaPollState(saved.ID, leaseUntil, map[string]any{"status": "Active", "poll_lease_until": int64(0), "next_poll_at": int64(0)})
	require.NoError(t, err)
	require.True(t, updated)

	otherAccount, claimed, err := ClaimSeedanceAssetReplica(context.Background(), asset, group.ID, 22, "account-b", "key-b", now, leaseUntil)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotEqual(t, replica.ID, otherAccount.ID)
	otherGroup, claimed, err := ClaimSeedanceAssetGroupReplica(context.Background(), 7, group.ID, 22, "account-b", "key-b", group.Name, now, leaseUntil)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotEqual(t, groupReplica.ID, otherGroup.ID)

	recovered, claimed, err := ClaimSeedanceAssetGroupReplica(context.Background(), 7, group.ID, 23, "account-c", "key-c", group.Name, now, leaseUntil)
	require.NoError(t, err)
	require.True(t, claimed)
	staleLease := leaseUntil + 1
	recoveredAgain, claimed, err := ClaimSeedanceAssetGroupReplica(context.Background(), 7, group.ID, 23, "account-c", "key-c", group.Name, staleLease, staleLease+90)
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, recovered.ID, recoveredAgain.ID)
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
