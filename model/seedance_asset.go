package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/reasoning"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SeedanceAssetAccountFingerprint 按素材入口和管理员声明的共享范围区分账号。
func SeedanceAssetAccountFingerprint(channel *Channel, keyFingerprint string) string {
	if channel == nil || strings.TrimSpace(keyFingerprint) == "" {
		return ""
	}
	settings := channel.GetSetting()
	baseURL := strings.TrimRight(strings.TrimSpace(settings.SeedanceAssetURL), "/")
	if baseURL == "" {
		return ""
	}
	identity := keyFingerprint
	if settings.SeedanceAssetShareGroup != "" {
		identity = "shared:" + settings.SeedanceAssetShareGroup
	}
	if channel.Type == constant.ChannelTypeTaskPlugin && settings.TaskPluginKey == "anyllm" {
		return seedanceAssetAccountFingerprint(baseURL, "json-action", identity)
	}
	if channel.Type == constant.ChannelTypeTaskPlugin && settings.TaskPluginKey == "originocean" {
		return seedanceAssetAccountFingerprint(baseURL, "origin-rest", identity)
	}
	if legacyBase, found := strings.CutSuffix(baseURL, "/v2/assets"); found {
		return seedanceAssetAccountFingerprint(legacyBase, "/v2/assets", identity)
	}
	return seedanceAssetAccountFingerprint(baseURL, "", identity)
}

// SeedanceAssetAccountFingerprints 兼容旧入口分支和尾斜杠摘要，不回填素材或副本大表。
func SeedanceAssetAccountFingerprints(channel *Channel, keyFingerprint string) []string {
	canonical := SeedanceAssetAccountFingerprint(channel, keyFingerprint)
	if canonical == "" {
		return nil
	}
	result := []string{canonical}
	if channel.GetSetting().SeedanceAssetShareGroup != "" {
		return result
	}
	baseURL := strings.TrimSpace(channel.GetSetting().SeedanceAssetURL)
	protocol := ""
	if channel.Type == constant.ChannelTypeTaskPlugin && channel.GetSetting().TaskPluginKey == "anyllm" {
		protocol = "json-action"
	} else if channel.Type == constant.ChannelTypeTaskPlugin && channel.GetSetting().TaskPluginKey == "originocean" {
		protocol = "origin-rest"
	}
	for _, endpoint := range []string{baseURL, strings.TrimRight(baseURL, "/")} {
		fingerprint := seedanceAssetAccountFingerprint(endpoint, protocol, keyFingerprint)
		if !slices.Contains(result, fingerprint) {
			result = append(result, fingerprint)
		}
	}
	return result
}

// SeedanceAssetSharedAccountMatches 不将未配置共享组的渠道扩展到其他密钥。
func SeedanceAssetSharedAccountMatches(channel *Channel, accountFingerprint string) bool {
	return channel != nil && channel.GetSetting().SeedanceAssetShareGroup != "" &&
		SeedanceAssetAccountFingerprintMatches(channel, "shared", accountFingerprint)
}

func SeedanceAssetAccountFingerprintMatches(channel *Channel, keyFingerprint, accountFingerprint string) bool {
	if accountFingerprint == "" {
		return false
	}
	if SeedanceAssetAccountFingerprint(channel, keyFingerprint) == accountFingerprint {
		return true
	}
	return slices.Contains(SeedanceAssetAccountFingerprints(channel, keyFingerprint), accountFingerprint)
}

func seedanceAssetAccountFingerprint(baseURL, assetPath, keyFingerprint string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(baseURL+"\x00"+assetPath+"\x00"+keyFingerprint)))
}

// SeedanceAssetModelScopeFingerprint 保留明确配置的共享组，否则按模型、渠道和密钥区分。
func SeedanceAssetModelScopeFingerprint(channel *Channel, modelName, keyFingerprint string) string {
	if channel == nil || channel.Id <= 0 || strings.TrimSpace(modelName) == "" || strings.TrimSpace(keyFingerprint) == "" {
		return ""
	}
	settings := channel.GetSetting()
	if settings.SeedanceAssetShareGroup != "" {
		return SeedanceAssetAccountFingerprint(channel, keyFingerprint)
	}
	assetURL := strings.TrimRight(strings.TrimSpace(settings.SeedanceAssetURL), "/")
	if assetURL == "" {
		return ""
	}
	mappedModel := modelName
	mapping := make(map[string]string)
	if raw := channel.GetModelMapping(); raw != "" {
		if err := common.UnmarshalJsonStr(raw, &mapping); err != nil {
			mappedModel = "invalid:" + raw
		} else {
			visited := map[string]bool{mappedModel: true}
			for {
				next := mapping[mappedModel]
				if next == "" {
					next = mapping[reasoning.BaseModelName(mappedModel)]
				}
				if next == "" || next == mappedModel {
					break
				}
				if visited[next] {
					mappedModel = "cycle:" + next
					break
				}
				visited[next] = true
				mappedModel = next
			}
		}
	}
	identity := fmt.Sprintf("seedance-model-v1\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s",
		channel.Id, modelName, keyFingerprint, assetURL, strings.TrimRight(channel.GetBaseURL(), "/"), settings.TaskPluginKey, mappedModel)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
}

// SeedanceAssetModelScopeMatches 仅恢复原渠道及原密钥的逐模型映射。
func SeedanceAssetModelScopeMatches(channel *Channel, keyFingerprint, fingerprint string) bool {
	if channel == nil || keyFingerprint == "" || fingerprint == "" {
		return false
	}
	for _, modelName := range channel.GetModels() {
		if SeedanceAssetModelScopeFingerprint(channel, modelName, keyFingerprint) == fingerprint {
			return true
		}
	}
	return false
}

// SeedanceAssetGroup 保存用户在火山方舟 Seedance 素材库中的本地授权映射。
type SeedanceAssetGroup struct {
	ID          uint   `gorm:"primaryKey" json:"id"`
	UserID      int    `gorm:"index;not null;index:idx_seedance_asset_group_user_group,priority:1" json:"user_id"`
	ChannelID   int    `gorm:"index;not null" json:"channel_id"`
	GroupID     string `gorm:"size:128;not null;index:idx_seedance_asset_group_user_group,priority:2" json:"group_id"`
	AssetCount  *int64 `gorm:"column:asset_count" json:"-"`
	Name        string `gorm:"size:64;not null" json:"name"`
	Description string `gorm:"type:text" json:"description,omitempty"`
	Tags        string `gorm:"size:512" json:"tags,omitempty"`
	GroupType   string `gorm:"size:128" json:"group_type,omitempty"`
	BindModel   string `gorm:"size:191" json:"-"`
	CompatType  string `gorm:"size:128" json:"-"`
	// 删除状态先持久化，阻止在途上传在分组清理后重新入库。
	Status             string    `gorm:"size:16;not null;default:Active" json:"status"`
	UpstreamDeletedAt  int64     `gorm:"not null;default:0" json:"-"`
	KeyFingerprint     string    `gorm:"size:64" json:"-"`
	AccountFingerprint string    `gorm:"size:64" json:"-"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	DeletionError      string    `gorm:"-" json:"deletion_error,omitempty"`
	DeletionRetryAt    int64     `gorm:"-" json:"deletion_retry_at,omitempty"`
}

// LoadSeedanceAssetGroupDeletionStates 批量补充当前用户删除任务的阻塞原因，不增加表列。
func LoadSeedanceAssetGroupDeletionStates(ctx context.Context, userID int, groups []SeedanceAssetGroup) error {
	ids := make([]uint, 0)
	for _, group := range groups {
		if group.Status == "Deleting" {
			ids = append(ids, group.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var jobs []SeedanceAssetCleanupJob
	if err := DB.WithContext(ctx).Select("local_group_id", "last_error", "next_attempt_at", "lease_until").
		Where("user_id = ? AND local_group_id IN ? AND kind = ? AND status = ?", userID, ids, SeedanceAssetCleanupKindGroupDelete, SeedanceAssetCleanupStatusPending).
		Find(&jobs).Error; err != nil {
		return err
	}
	byGroup := make(map[uint]SeedanceAssetCleanupJob, len(jobs))
	for _, job := range jobs {
		byGroup[job.LocalGroupID] = job
	}
	now := time.Now().Unix()
	for index := range groups {
		if job, found := byGroup[groups[index].ID]; found {
			groups[index].DeletionRetryAt = job.NextAttemptAt
			if job.LeaseUntil <= now {
				groups[index].DeletionError = job.LastError
			}
		}
	}
	return nil
}

// SeedanceAssetGroupReplica 保存本地素材组在其他上游账号中的对应组。
type SeedanceAssetGroupReplica struct {
	ID                 uint   `gorm:"primaryKey"`
	UserID             int    `gorm:"index;not null"`
	LocalGroupID       uint   `gorm:"index;not null;uniqueIndex:idx_seedance_group_replica_account,priority:1"`
	ChannelID          int    `gorm:"index;not null"`
	AccountFingerprint string `gorm:"size:64;not null;uniqueIndex:idx_seedance_group_replica_account,priority:2"`
	KeyFingerprint     string `gorm:"size:64;not null"`
	UpstreamGroupID    string `gorm:"size:128"`
	GroupName          string `gorm:"size:64;not null"`
	Status             string `gorm:"size:16;not null;index"`
	LeaseUntil         int64  `gorm:"not null;default:0"`
	UpstreamDeletedAt  int64  `gorm:"not null;default:0"`
	LastError          string `gorm:"size:512"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// SeedanceAssetStorage 固定对象所属地域和 Bucket，不保存凭证；密钥轮换仍使用系统配置。
type SeedanceAssetStorage struct {
	Region   string `gorm:"size:64"`
	Endpoint string `gorm:"size:2048"`
	Bucket   string `gorm:"size:64"`
}

// BeforeSave 在数据库边界清理素材组名称的首尾空格。
func (group *SeedanceAssetGroup) BeforeSave(_ *gorm.DB) error {
	group.Name = strings.TrimSpace(group.Name)
	return nil
}

// SeedanceAsset 保存上游素材状态及其所属用户，所有读取必须带 UserID 条件。
type SeedanceAsset struct {
	UploadBytes             int64                `gorm:"-" json:"-"`
	ID                      uint                 `gorm:"primaryKey;index:idx_seedance_asset_user_group_id,priority:3;index:idx_seedance_asset_status_key_poll_due,priority:4;index:idx_seedance_asset_user_group_pending_status,priority:4" json:"id"`
	UserID                  int                  `gorm:"index;not null;index:idx_seedance_asset_user_group_id,priority:1;index:idx_seedance_asset_user_group_pending_status,priority:1" json:"user_id"`
	ChannelID               int                  `gorm:"index;not null" json:"channel_id"`
	GroupID                 string               `gorm:"index;size:128;not null;index:idx_seedance_asset_user_group_id,priority:2;index:idx_seedance_asset_user_group_pending_status,priority:2" json:"group_id"`
	AssetID                 string               `gorm:"uniqueIndex;size:128;not null" json:"asset_id"`
	PrimaryUpstreamID       string               `gorm:"size:128" json:"-"`
	PrimarySyncScope        string               `gorm:"type:text" json:"-"`
	LocalOnly               *bool                `json:"-"`
	CompatOptimizeReplicaID uint                 `json:"-"`
	Name                    string               `gorm:"size:64;not null" json:"name"`
	AssetType               string               `gorm:"size:16;not null" json:"asset_type"`
	SourceURL               string               `gorm:"size:2048;not null" json:"-"`
	ObjectKey               string               `gorm:"index;size:640" json:"-"`
	Storage                 SeedanceAssetStorage `gorm:"embedded;embeddedPrefix:oss_" json:"-"`
	KeyFingerprint          string               `gorm:"size:64" json:"-"`
	AccountFingerprint      string               `gorm:"size:64" json:"-"`
	UpstreamDeletedAt       int64                `gorm:"not null;default:0" json:"-"`
	PreviewURL              string               `gorm:"size:4096" json:"preview_url,omitempty"`
	Status                  string               `gorm:"index;size:32;not null" json:"status"`
	StatusKey               *string              `gorm:"size:32;index:idx_seedance_asset_status_key_poll_due,priority:1" json:"-"`
	PendingStatus           *bool                `gorm:"index:idx_seedance_asset_user_group_pending_status,priority:3" json:"-"`
	FailureCode             string               `gorm:"size:128" json:"failure_code,omitempty"`
	PollAttempts            int                  `gorm:"not null;default:0" json:"-"`
	NextPollAt              int64                `gorm:"index;index:idx_seedance_asset_status_key_poll_due,priority:2;not null;default:0" json:"-"`
	PollLeaseUntil          int64                `gorm:"index;index:idx_seedance_asset_status_key_poll_due,priority:3;not null;default:0" json:"-"`
	CreatedAt               time.Time            `json:"created_at"`
	UpdatedAt               time.Time            `json:"updated_at"`
}

// UpstreamAssetID 区分稳定的公开素材 ID 和优化后的上游素材 ID。
func (asset *SeedanceAsset) UpstreamAssetID() string {
	if asset == nil || asset.LocalOnly != nil && (*asset.LocalOnly || asset.PrimaryUpstreamID == "") {
		return ""
	}
	if asset.PrimaryUpstreamID != "" {
		return asset.PrimaryUpstreamID
	}
	return asset.AssetID
}

// IsReplicaManaged 表示本站素材没有独占的主账号，审核状态由账号副本汇总。
func (asset *SeedanceAsset) IsReplicaManaged() bool {
	return asset != nil && asset.LocalOnly != nil && !*asset.LocalOnly && asset.PrimaryUpstreamID == ""
}

// SeedanceAssetSize 独立记录已知对象大小，避免给大素材表新增或回填列。
type SeedanceAssetSize struct {
	AssetID uint  `gorm:"primaryKey"`
	UserID  int   `gorm:"index;not null"`
	Bytes   int64 `gorm:"not null"`
}

func CheckSeedanceAssetCapacity(ctx context.Context, userID int, incomingBytes int64) error {
	return checkSeedanceAssetCapacity(DB.WithContext(ctx), userID, incomingBytes, common.GetManagementRateLimits().AssetLimits)
}

func checkSeedanceAssetCapacity(tx *gorm.DB, userID int, incomingBytes int64, limits common.PrivateAssetLimits) error {
	if incomingBytes < 0 {
		return errors.New("invalid private asset size")
	}
	if limits.UserMaxCount > 0 {
		var count int64
		if err := tx.Model(&SeedanceAsset{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
			return err
		}
		if count >= limits.UserMaxCount {
			return errors.New("private asset count limit exceeded")
		}
	}
	if limits.UserStorageMiB > 0 {
		maxBytes := limits.UserStorageMiB << 20
		var used int64
		assets := tx.NamingStrategy.TableName("SeedanceAsset")
		sizes := tx.NamingStrategy.TableName("SeedanceAssetSize")
		// 旧对象没有大小记录时按单文件上限预留，不请求 OSS，也不低估已占容量。
		if err := tx.Table(assets+" AS a").Joins("LEFT JOIN "+sizes+" AS s ON s.asset_id = a.id").Where("a.user_id = ? AND a.object_key <> ?", userID, "").Select("COALESCE(SUM(CASE WHEN s.asset_id IS NULL THEN ? ELSE s.bytes END), 0)", int64(50<<20)).Scan(&used).Error; err != nil {
			return err
		}
		if incomingBytes > maxBytes || used > maxBytes-incomingBytes {
			return errors.New("private asset storage limit exceeded")
		}
	}
	return nil
}

type SeedanceAssetSchemaMigration struct {
	Name      string `gorm:"primaryKey;size:64"`
	Completed bool
}

// SeedanceAssetStatusIndexKey 保留上游原始状态，同时生成用于索引查询的规范键。
func SeedanceAssetStatusIndexKey(status string) string {
	return strings.ToLower(strings.TrimSpace(status))
}

// SeedanceAssetStatusRequiresRefresh 判断状态是否仍需要页面刷新或后台清理。
func SeedanceAssetStatusRequiresRefresh(status string) bool {
	switch SeedanceAssetStatusIndexKey(status) {
	case "processing", "pending", "deleting":
		return true
	default:
		return false
	}
}

func (asset *SeedanceAsset) BeforeSave(_ *gorm.DB) error {
	statusKey := SeedanceAssetStatusIndexKey(asset.Status)
	asset.StatusKey = &statusKey
	pendingStatus := SeedanceAssetStatusRequiresRefresh(statusKey)
	asset.PendingStatus = &pendingStatus
	return nil
}

// migrateSeedanceAssetGroupNameUniqueness 移除旧版本的素材组名称唯一索引。
func migrateSeedanceAssetGroupNameUniqueness(db *gorm.DB) error {
	const index = "idx_seedance_asset_group_user_name_key"
	if !db.Migrator().HasIndex(&SeedanceAssetGroup{}, index) {
		return nil
	}
	if err := db.Migrator().DropIndex(&SeedanceAssetGroup{}, index); err != nil {
		return fmt.Errorf("remove Seedance asset group name index: %w", err)
	}
	return nil
}

// migrateSeedanceAssetListPerformance 为升级前素材补齐状态键和分组计数。
func migrateSeedanceAssetListPerformance(db *gorm.DB) error {
	if db == nil {
		return errors.New("database is not initialized")
	}
	if db.Migrator().HasIndex(&SeedanceAsset{}, "idx_seedance_asset_poll_due") {
		if err := db.Migrator().DropIndex(&SeedanceAsset{}, "idx_seedance_asset_poll_due"); err != nil {
			return err
		}
	}

	const migrationName = "seedance_asset_list_performance_v1"
	return db.Transaction(func(tx *gorm.DB) error {
		migration := SeedanceAssetSchemaMigration{Name: migrationName}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&migration).Error; err != nil {
			return err
		}
		if tx.Dialector.Name() == "sqlite" {
			if err := tx.Model(&SeedanceAssetSchemaMigration{}).
				Where("name = ?", migrationName).
				UpdateColumn("completed", gorm.Expr("completed")).Error; err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).Where("name = ?", migrationName).First(&migration).Error; err != nil {
			return err
		}
		if migration.Completed {
			return nil
		}
		if err := tx.Model(&SeedanceAsset{}).
			Where("(status_key IS NULL OR status_key = '') AND LOWER(status) IN ?", []string{"processing", "pending", "deleting"}).
			UpdateColumn("status_key", gorm.Expr("LOWER(status)")).Error; err != nil {
			return err
		}
		if err := tx.Model(&SeedanceAsset{}).
			Where("pending_status IS NULL AND LOWER(status) IN ?", []string{"processing", "pending", "deleting"}).
			UpdateColumn("pending_status", true).Error; err != nil {
			return err
		}
		assetStatement := &gorm.Statement{DB: tx}
		if err := assetStatement.Parse(&SeedanceAsset{}); err != nil {
			return err
		}
		groupStatement := &gorm.Statement{DB: tx}
		if err := groupStatement.Parse(&SeedanceAssetGroup{}); err != nil {
			return err
		}
		assetCounts := tx.Model(&SeedanceAsset{}).Select("COUNT(*)").Where(
			assetStatement.Table + ".user_id = " + groupStatement.Table + ".user_id AND " +
				assetStatement.Table + ".group_id = " + groupStatement.Table + ".group_id",
		)
		if err := tx.Model(&SeedanceAssetGroup{}).Where("id > 0").
			UpdateColumn("asset_count", assetCounts).Error; err != nil {
			return err
		}
		return tx.Model(&migration).UpdateColumn("completed", true).Error
	})
}

// SeedanceAssetReplica 保存本地素材在目标上游账号中的素材 ID 和独立审核状态。
type SeedanceAssetReplica struct {
	ID                 uint      `gorm:"primaryKey;index:idx_seedance_asset_replica_poll_due,priority:4"`
	UserID             int       `gorm:"index;not null"`
	LocalGroupID       uint      `gorm:"index;not null"`
	LocalAssetID       uint      `gorm:"index;not null;uniqueIndex:idx_seedance_asset_replica_account,priority:1"`
	ChannelID          int       `gorm:"index;not null"`
	AccountFingerprint string    `gorm:"size:64;not null;uniqueIndex:idx_seedance_asset_replica_account,priority:2"`
	KeyFingerprint     string    `gorm:"size:64;not null"`
	SyncScope          string    `gorm:"type:text" json:"-"`
	UpstreamGroupID    string    `gorm:"size:128;not null"`
	UpstreamAssetID    string    `gorm:"size:128;not null"`
	ProvisionStatus    string    `gorm:"size:16;not null;index"`
	Status             string    `gorm:"size:32;not null;index:idx_seedance_asset_replica_poll_due,priority:1"`
	PreviewURL         string    `gorm:"size:4096"`
	PollAttempts       int       `gorm:"not null;default:0"`
	NextPollAt         int64     `gorm:"not null;default:0;index:idx_seedance_asset_replica_poll_due,priority:2"`
	PollLeaseUntil     int64     `gorm:"not null;default:0;index:idx_seedance_asset_replica_poll_due,priority:3"`
	UpstreamDeletedAt  int64     `gorm:"not null;default:0"`
	LastError          string    `gorm:"size:512"`
	CreatedAt          time.Time `json:"-"`
	UpdatedAt          time.Time `json:"-"`
}

type SeedanceAssetSyncTarget struct {
	ChannelID          int
	AccountFingerprint string
	KeyFingerprint     string
	SyncScope          string
}

const (
	SeedanceAssetReplicaCreating = "Creating"
	SeedanceAssetReplicaReady    = "Ready"
	SeedanceAssetReplicaFailed   = "Failed"
)

const MaxSeedanceAssetAffinityReferences = 32

const SeedanceAssetSyncMaxAttempts = 6

const SeedanceAssetReviewTimeoutCode = "review_timeout"

// ErrSeedanceAssetUnavailable 表示素材不存在、已删除或不属于当前用户。
var ErrSeedanceAssetUnavailable = errors.New("private asset is unavailable")

var ErrSeedanceAssetCommitUncertain = errors.New("asset changes could not be confirmed; check the asset library before retrying")

// CreateSeedanceAssetGroup 在提交确认丢失时保留已创建的上游组。
func CreateSeedanceAssetGroup(ctx context.Context, group *SeedanceAssetGroup) error {
	ready := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(group).Error; err != nil {
			return err
		}
		ready = true
		return nil
	})
	if err == nil || !ready {
		return err
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stored SeedanceAssetGroup
	if checkErr := DB.WithContext(checkCtx).Where("user_id = ? AND group_id = ? AND channel_id = ? AND account_fingerprint = ?", group.UserID, group.GroupID, group.ChannelID, group.AccountFingerprint).First(&stored).Error; checkErr == nil {
		*group = stored
		return nil
	}
	return ErrSeedanceAssetCommitUncertain
}

// CreateSeedanceAssetInGroup 与分组删除共享数据库锁，仅允许仍可用的分组接收最终素材。
// 外部上传不持有锁，失败由调用方补偿；锁只覆盖状态校验与本地落库。
func CreateSeedanceAssetInGroup(ctx context.Context, asset *SeedanceAsset, syncTargets ...SeedanceAssetSyncTarget) error {
	ready := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// SQLite 无行锁，先执行无值变化的更新取得写锁，避免读快照升级写锁的竞态。
		if tx.Dialector.Name() == "sqlite" {
			if err := tx.Model(&SeedanceAssetGroup{}).Where("user_id = ? AND group_id = ?", asset.UserID, asset.GroupID).
				UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
				return err
			}
		}
		var group SeedanceAssetGroup
		limits := common.GetManagementRateLimits().AssetLimits
		if limits.UserMaxCount > 0 || limits.UserStorageMiB > 0 {
			// 各素材组先锁同一用户的首组，不同组并发创建也不能突破累计配额。
			var guard SeedanceAssetGroup
			if err := lockForUpdate(tx).Where("user_id = ?", asset.UserID).Order("id").First(&guard).Error; err != nil {
				return err
			}
			if err := checkSeedanceAssetCapacity(tx, asset.UserID, asset.UploadBytes, limits); err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).Where("user_id = ? AND group_id = ?", asset.UserID, asset.GroupID).First(&group).Error; err != nil {
			return err
		}
		if group.Status == "Deleting" || group.ChannelID != asset.ChannelID || group.KeyFingerprint != asset.KeyFingerprint {
			return errors.New("asset group is unavailable or being deleted")
		}
		if err := tx.Create(asset).Error; err != nil {
			return err
		}
		if asset.UploadBytes > 0 {
			if err := tx.Create(&SeedanceAssetSize{AssetID: asset.ID, UserID: asset.UserID, Bytes: asset.UploadBytes}).Error; err != nil {
				return err
			}
		}
		if len(syncTargets) > 0 {
			if strings.EqualFold(group.GroupType, "LivenessFace") || asset.LocalOnly != nil && *asset.LocalOnly {
				return errors.New("this asset cannot be copied to other accounts")
			}
			replicas := make([]SeedanceAssetReplica, 0, len(syncTargets))
			for _, target := range syncTargets {
				replicas = append(replicas, SeedanceAssetReplica{
					UserID: asset.UserID, LocalGroupID: group.ID, LocalAssetID: asset.ID,
					ChannelID: target.ChannelID, AccountFingerprint: target.AccountFingerprint,
					KeyFingerprint: target.KeyFingerprint, SyncScope: target.SyncScope, ProvisionStatus: SeedanceAssetReplicaCreating,
					Status: "Pending", NextPollAt: time.Now().Unix(),
				})
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "local_asset_id"}, {Name: "account_fingerprint"}}, DoNothing: true,
			}).CreateInBatches(&replicas, 100).Error; err != nil {
				return err
			}
		}
		if group.AssetCount != nil {
			if err := tx.Model(&SeedanceAssetGroup{}).
				Where("user_id = ? AND group_id = ? AND asset_count IS NOT NULL", asset.UserID, asset.GroupID).
				UpdateColumn("asset_count", gorm.Expr("asset_count + ?", 1)).Error; err != nil {
				return err
			}
		}
		ready = true
		return nil
	})
	if err == nil || !ready {
		return err
	}
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stored SeedanceAsset
	if checkErr := DB.WithContext(checkCtx).Where("user_id = ? AND group_id = ? AND asset_id = ? AND object_key = ?", asset.UserID, asset.GroupID, asset.AssetID, asset.ObjectKey).First(&stored).Error; checkErr == nil {
		*asset = stored
		return nil
	}
	return ErrSeedanceAssetCommitUncertain
}

// DeleteSeedanceAssetWithCount 删除指定用户的素材，并在同一事务内维护素材组计数。
func DeleteSeedanceAssetWithCount(ctx context.Context, userID int, assetID uint, expectedStatus string) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if userID <= 0 || assetID == 0 {
		return errors.New("invalid Seedance asset owner or ID")
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "sqlite" {
			if err := tx.Model(&SeedanceAsset{}).Where("id = ? AND user_id = ?", assetID, userID).
				UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
				return err
			}
		}
		query := lockForUpdate(tx).Where("id = ? AND user_id = ?", assetID, userID)
		if expectedStatus != "" {
			query = query.Where("status = ?", expectedStatus)
		}
		var asset SeedanceAsset
		result := query.Limit(1).Find(&asset)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		result = tx.Delete(&asset)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		if asset.ObjectKey != "" {
			if err := tx.Where("asset_id = ? AND user_id = ?", asset.ID, asset.UserID).Delete(&SeedanceAssetSize{}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&SeedanceAssetGroup{}).
			Where("user_id = ? AND group_id = ? AND asset_count IS NOT NULL", asset.UserID, asset.GroupID).
			UpdateColumn("asset_count", gorm.Expr("CASE WHEN asset_count > 0 THEN asset_count - 1 ELSE 0 END")).Error
	})
}

// FindSeedanceAssetsForUser 只解析当前用户拥有且未进入删除流程的本地素材。
func FindSeedanceAssetsForUser(ctx context.Context, userID int, assetIDs []string) ([]SeedanceAsset, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if userID <= 0 || len(assetIDs) == 0 || len(assetIDs) > MaxSeedanceAssetAffinityReferences {
		return nil, errors.New("invalid private asset reference list")
	}
	uniqueIDs := make([]string, 0, len(assetIDs))
	seen := make(map[string]struct{}, len(assetIDs))
	for _, assetID := range assetIDs {
		assetID = strings.TrimSpace(assetID)
		if assetID == "" || len(assetID) > 128 {
			return nil, errors.New("invalid private asset reference")
		}
		if _, exists := seen[assetID]; exists {
			continue
		}
		seen[assetID] = struct{}{}
		uniqueIDs = append(uniqueIDs, assetID)
	}
	var assets []SeedanceAsset
	if err := DB.WithContext(ctx).
		Where("user_id = ? AND asset_id IN ?", userID, uniqueIDs).
		Where("LOWER(status) <> ?", "deleting").
		Find(&assets).Error; err != nil {
		return nil, err
	}
	if len(assets) != len(uniqueIDs) {
		return nil, ErrSeedanceAssetUnavailable
	}
	assetsByID := make(map[string]SeedanceAsset, len(assets))
	for _, asset := range assets {
		assetsByID[asset.AssetID] = asset
	}
	ordered := make([]SeedanceAsset, 0, len(uniqueIDs))
	for _, assetID := range uniqueIDs {
		asset, exists := assetsByID[assetID]
		if !exists {
			return nil, ErrSeedanceAssetUnavailable
		}
		ordered = append(ordered, asset)
	}
	return ordered, nil
}

// FindSeedanceAssetGroupReplicas 返回本地组的账号副本，供同步、重命名及删除流程复用。
func FindSeedanceAssetGroupReplicas(ctx context.Context, userID int, localGroupID uint) ([]SeedanceAssetGroupReplica, error) {
	var replicas []SeedanceAssetGroupReplica
	err := DB.WithContext(ctx).Where("user_id = ? AND local_group_id = ?", userID, localGroupID).
		Order("id asc").Find(&replicas).Error
	return replicas, err
}

// FindSeedanceAssetGroupReplica 定位一个本地组在指定账号命名空间中的映射。
func FindSeedanceAssetGroupReplica(ctx context.Context, userID int, localGroupID uint, accountFingerprint string) (*SeedanceAssetGroupReplica, error) {
	var replica SeedanceAssetGroupReplica
	err := DB.WithContext(ctx).Where("user_id = ? AND local_group_id = ? AND account_fingerprint = ?", userID, localGroupID, accountFingerprint).First(&replica).Error
	if err != nil {
		return nil, err
	}
	return &replica, nil
}

// FindSeedanceAssetGroupReplicasForAccount 一次读取请求涉及的组副本，避免逐组查询。
func FindSeedanceAssetGroupReplicasForAccount(ctx context.Context, userID int, localGroupIDs []uint, accountFingerprint string, compatibleFingerprints ...string) (map[uint]*SeedanceAssetGroupReplica, error) {
	replicas := make(map[uint]*SeedanceAssetGroupReplica, len(localGroupIDs))
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if len(localGroupIDs) == 0 {
		return replicas, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var rows []SeedanceAssetGroupReplica
	if err := DB.WithContext(ctx).
		Where("user_id = ? AND local_group_id IN ? AND account_fingerprint IN ?", userID, localGroupIDs, append([]string{accountFingerprint}, compatibleFingerprints...)).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for index := range rows {
		if replicas[rows[index].LocalGroupID] == nil || rows[index].AccountFingerprint == accountFingerprint {
			replicas[rows[index].LocalGroupID] = &rows[index]
		}
	}
	return replicas, nil
}

// FindSeedanceAssetReplicas 返回本地素材的账号副本，供映射和清理流程复用。
func FindSeedanceAssetReplicas(ctx context.Context, userID int, localAssetID uint) ([]SeedanceAssetReplica, error) {
	var replicas []SeedanceAssetReplica
	err := DB.WithContext(ctx).Where("user_id = ? AND local_asset_id = ?", userID, localAssetID).
		Order("id asc").Find(&replicas).Error
	return replicas, err
}

// FindSeedanceAssetReplica 定位一个本地素材在指定账号命名空间中的映射。
func FindSeedanceAssetReplica(ctx context.Context, userID int, localAssetID uint, accountFingerprint string) (*SeedanceAssetReplica, error) {
	var replica SeedanceAssetReplica
	err := DB.WithContext(ctx).Where("user_id = ? AND local_asset_id = ? AND account_fingerprint = ?", userID, localAssetID, accountFingerprint).First(&replica).Error
	if err != nil {
		return nil, err
	}
	return &replica, nil
}

// FindSeedanceAssetReplicasForAccount 一次读取请求涉及的素材副本，避免逐素材查询。
func FindSeedanceAssetReplicasForAccount(ctx context.Context, userID int, localAssetIDs []uint, accountFingerprint string, compatibleFingerprints ...string) (map[uint]*SeedanceAssetReplica, error) {
	replicas := make(map[uint]*SeedanceAssetReplica, len(localAssetIDs))
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if len(localAssetIDs) == 0 {
		return replicas, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var rows []SeedanceAssetReplica
	if err := DB.WithContext(ctx).
		Where("user_id = ? AND local_asset_id IN ? AND account_fingerprint IN ?", userID, localAssetIDs, append([]string{accountFingerprint}, compatibleFingerprints...)).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for index := range rows {
		if replicas[rows[index].LocalAssetID] == nil || rows[index].AccountFingerprint == accountFingerprint {
			replicas[rows[index].LocalAssetID] = &rows[index]
		}
	}
	return replicas, nil
}

// ClaimSeedanceAssetGroupReplica 用短租约确保并发首次调用只创建一个上游组。
func ClaimSeedanceAssetGroupReplica(ctx context.Context, userID int, localGroupID uint, channelID int, accountFingerprint, keyFingerprint, groupName string, now, leaseUntil int64) (*SeedanceAssetGroupReplica, bool, error) {
	return claimSeedanceAssetGroupReplica(ctx, userID, localGroupID, channelID, accountFingerprint, keyFingerprint, groupName, now, leaseUntil, nil, false)
}

// ClaimSeedanceAssetGroupReplicaWithSnapshot 使用批量预读结果认领组副本；并发更新仍由条件写入裁决。
func ClaimSeedanceAssetGroupReplicaWithSnapshot(ctx context.Context, userID int, localGroupID uint, channelID int, accountFingerprint, keyFingerprint, groupName string, now, leaseUntil int64, snapshot *SeedanceAssetGroupReplica) (*SeedanceAssetGroupReplica, bool, error) {
	return claimSeedanceAssetGroupReplica(ctx, userID, localGroupID, channelID, accountFingerprint, keyFingerprint, groupName, now, leaseUntil, snapshot, true)
}

func claimSeedanceAssetGroupReplica(ctx context.Context, userID int, localGroupID uint, channelID int, accountFingerprint, keyFingerprint, groupName string, now, leaseUntil int64, snapshot *SeedanceAssetGroupReplica, hasSnapshot bool) (*SeedanceAssetGroupReplica, bool, error) {
	if DB == nil || userID <= 0 || localGroupID == 0 || channelID <= 0 || accountFingerprint == "" || keyFingerprint == "" || leaseUntil <= now {
		return nil, false, errors.New("invalid Seedance group replica claim")
	}
	if hasSnapshot && snapshot != nil && (snapshot.UserID != userID || snapshot.LocalGroupID != localGroupID || snapshot.AccountFingerprint != accountFingerprint) {
		return nil, false, errors.New("Seedance group replica snapshot does not match claim")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var group SeedanceAssetGroup
	if err := DB.WithContext(ctx).Where("id = ? AND user_id = ?", localGroupID, userID).First(&group).Error; err != nil {
		return nil, false, err
	}
	if group.Status == "Deleting" {
		return nil, false, errors.New("asset group is being deleted")
	}
	lookup := func() (*SeedanceAssetGroupReplica, error) {
		var replica SeedanceAssetGroupReplica
		err := DB.WithContext(ctx).Where("user_id = ? AND local_group_id = ? AND account_fingerprint = ?", userID, localGroupID, accountFingerprint).First(&replica).Error
		if err != nil {
			return nil, err
		}
		return &replica, nil
	}
	replica := snapshot
	var err error
	if !hasSnapshot {
		replica, err = lookup()
	} else if snapshot == nil {
		err = gorm.ErrRecordNotFound
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		replica = &SeedanceAssetGroupReplica{
			UserID:             userID,
			LocalGroupID:       localGroupID,
			ChannelID:          channelID,
			AccountFingerprint: accountFingerprint,
			KeyFingerprint:     keyFingerprint,
			GroupName:          groupName,
			Status:             SeedanceAssetReplicaCreating,
			LeaseUntil:         leaseUntil,
		}
		created := false
		createErr := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "sqlite" {
				if err := tx.Model(&SeedanceAssetGroup{}).Where("id = ? AND user_id = ?", localGroupID, userID).
					UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
					return err
				}
			}
			var group SeedanceAssetGroup
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", localGroupID, userID, "Deleting").First(&group).Error; err != nil {
				return err
			}
			var existing SeedanceAssetGroupReplica
			err := tx.Where("user_id = ? AND local_group_id = ? AND account_fingerprint = ?", userID, localGroupID, accountFingerprint).First(&existing).Error
			if err == nil {
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			created = true
			return tx.Create(replica).Error
		})
		if createErr != nil {
			return nil, false, createErr
		}
		if created {
			return replica, true, nil
		}
		replica, err = lookup()
	}
	if err != nil {
		return nil, false, err
	}
	if replica.Status == SeedanceAssetReplicaReady && replica.UpstreamGroupID != "" {
		return replica, false, nil
	}
	if replica.Status == SeedanceAssetReplicaCreating && replica.LeaseUntil > now {
		return replica, false, nil
	}
	updated := DB.WithContext(ctx).Model(&SeedanceAssetGroupReplica{}).
		Where("id = ? AND (status = ? OR (status = ? AND lease_until <= ?))", replica.ID, SeedanceAssetReplicaFailed, SeedanceAssetReplicaCreating, now).
		Updates(map[string]any{
			"channel_id":          channelID,
			"key_fingerprint":     keyFingerprint,
			"group_name":          groupName,
			"status":              SeedanceAssetReplicaCreating,
			"lease_until":         leaseUntil,
			"upstream_group_id":   "",
			"upstream_deleted_at": int64(0),
			"last_error":          "",
		})
	if updated.Error != nil {
		return nil, false, updated.Error
	}
	if updated.RowsAffected == 1 {
		replica.ChannelID = channelID
		replica.KeyFingerprint = keyFingerprint
		replica.GroupName = groupName
		replica.Status = SeedanceAssetReplicaCreating
		replica.LeaseUntil = leaseUntil
		return replica, true, nil
	}
	replica, err = lookup()
	return replica, false, err
}

// CompleteSeedanceAssetGroupReplica 仅在本地组仍可用且租约未被替换时发布新映射。
func CompleteSeedanceAssetGroupReplica(ctx context.Context, id uint, leaseUntil int64, upstreamGroupID, groupName string) (bool, error) {
	if DB == nil || id == 0 || leaseUntil <= 0 || strings.TrimSpace(upstreamGroupID) == "" {
		return false, errors.New("invalid Seedance group replica completion")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	updated := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var replica SeedanceAssetGroupReplica
		if err := tx.First(&replica, id).Error; err != nil {
			return err
		}
		if tx.Dialector.Name() == "sqlite" {
			if err := tx.Model(&SeedanceAssetGroup{}).Where("id = ? AND user_id = ?", replica.LocalGroupID, replica.UserID).
				UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
				return err
			}
		}
		var group SeedanceAssetGroup
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", replica.LocalGroupID, replica.UserID, "Deleting").First(&group).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		result := tx.Model(&replica).
			Where("id = ? AND status = ? AND lease_until = ?", id, SeedanceAssetReplicaCreating, leaseUntil).
			Updates(map[string]any{
				"upstream_group_id": upstreamGroupID,
				"group_name":        groupName,
				"status":            SeedanceAssetReplicaReady,
				"lease_until":       int64(0),
				"last_error":        "",
			})
		if result.Error != nil {
			return result.Error
		}
		updated = result.RowsAffected == 1
		return nil
	})
	if err != nil && updated {
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var stored SeedanceAssetGroupReplica
		if checkErr := DB.WithContext(checkCtx).Where("id = ? AND upstream_group_id = ? AND status = ?", id, upstreamGroupID, SeedanceAssetReplicaReady).First(&stored).Error; checkErr == nil {
			return true, nil
		}
		return false, ErrSeedanceAssetCommitUncertain
	}
	return updated, err
}

// FailSeedanceAssetGroupReplica 释放创建失败的租约，保留短错误用于运维排查。
func FailSeedanceAssetGroupReplica(id uint, leaseUntil int64, message string) error {
	return DB.Model(&SeedanceAssetGroupReplica{}).
		Where("id = ? AND status = ? AND lease_until = ?", id, SeedanceAssetReplicaCreating, leaseUntil).
		Updates(map[string]any{"status": SeedanceAssetReplicaFailed, "lease_until": int64(0), "last_error": truncateSeedanceAssetMappingError(message)}).Error
}

// ClaimSeedanceAssetReplica 用租约阻止同一素材在目标账号被并发重复导入。
func ClaimSeedanceAssetReplica(ctx context.Context, asset *SeedanceAsset, localGroupID uint, channelID int, accountFingerprint, keyFingerprint string, now, leaseUntil int64) (*SeedanceAssetReplica, bool, error) {
	return claimSeedanceAssetReplica(ctx, asset, localGroupID, channelID, accountFingerprint, keyFingerprint, now, leaseUntil, nil, false)
}

// ClaimSeedanceAssetReplicaWithSnapshot 使用批量预读结果认领素材副本；租约更新仍使用条件写入。
func ClaimSeedanceAssetReplicaWithSnapshot(ctx context.Context, asset *SeedanceAsset, localGroupID uint, channelID int, accountFingerprint, keyFingerprint string, now, leaseUntil int64, snapshot *SeedanceAssetReplica) (*SeedanceAssetReplica, bool, error) {
	return claimSeedanceAssetReplica(ctx, asset, localGroupID, channelID, accountFingerprint, keyFingerprint, now, leaseUntil, snapshot, true)
}

func claimSeedanceAssetReplica(ctx context.Context, asset *SeedanceAsset, localGroupID uint, channelID int, accountFingerprint, keyFingerprint string, now, leaseUntil int64, snapshot *SeedanceAssetReplica, hasSnapshot bool) (*SeedanceAssetReplica, bool, error) {
	if DB == nil || asset == nil || asset.ID == 0 || localGroupID == 0 || channelID <= 0 || accountFingerprint == "" || keyFingerprint == "" || leaseUntil <= now {
		return nil, false, errors.New("invalid Seedance asset replica claim")
	}
	if hasSnapshot && snapshot != nil && (snapshot.UserID != asset.UserID || snapshot.LocalAssetID != asset.ID || snapshot.AccountFingerprint != accountFingerprint) {
		return nil, false, errors.New("Seedance asset replica snapshot does not match claim")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lookup := func() (*SeedanceAssetReplica, error) {
		var replica SeedanceAssetReplica
		err := DB.WithContext(ctx).Where("user_id = ? AND local_asset_id = ? AND account_fingerprint = ?", asset.UserID, asset.ID, accountFingerprint).First(&replica).Error
		if err != nil {
			return nil, err
		}
		return &replica, nil
	}
	replica := snapshot
	var err error
	if !hasSnapshot {
		replica, err = lookup()
	} else if snapshot == nil {
		err = gorm.ErrRecordNotFound
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		replica = &SeedanceAssetReplica{
			UserID:             asset.UserID,
			LocalGroupID:       localGroupID,
			LocalAssetID:       asset.ID,
			ChannelID:          channelID,
			AccountFingerprint: accountFingerprint,
			KeyFingerprint:     keyFingerprint,
			ProvisionStatus:    SeedanceAssetReplicaCreating,
			PollLeaseUntil:     leaseUntil,
		}
		created := false
		createErr := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if tx.Dialector.Name() == "sqlite" {
				if err := tx.Model(&SeedanceAssetGroup{}).Where("id = ? AND user_id = ?", localGroupID, asset.UserID).
					UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
					return err
				}
			}
			var group SeedanceAssetGroup
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", localGroupID, asset.UserID, "Deleting").First(&group).Error; err != nil {
				return err
			}
			var current SeedanceAsset
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", asset.ID, asset.UserID, "Deleting").First(&current).Error; err != nil {
				return err
			}
			var existing SeedanceAssetReplica
			err := tx.Where("user_id = ? AND local_asset_id = ? AND account_fingerprint = ?", asset.UserID, asset.ID, accountFingerprint).First(&existing).Error
			if err == nil {
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			// 创建副本也受删除屏障保护，删除快照完成后不能新增漏清理的映射。
			created = true
			return tx.Create(replica).Error
		})
		if createErr != nil {
			return nil, false, createErr
		}
		if created {
			return replica, true, nil
		}
		replica, err = lookup()
	}
	if err != nil {
		return nil, false, err
	}
	if replica.ProvisionStatus == SeedanceAssetReplicaReady {
		if replica.ChannelID != channelID || replica.KeyFingerprint != keyFingerprint {
			updated := DB.WithContext(ctx).Model(&SeedanceAssetReplica{}).
				Where("id = ? AND provision_status = ?", replica.ID, SeedanceAssetReplicaReady).
				Updates(map[string]any{"channel_id": channelID, "key_fingerprint": keyFingerprint})
			if updated.Error != nil {
				return nil, false, updated.Error
			}
			if updated.RowsAffected == 1 {
				replica.ChannelID = channelID
				replica.KeyFingerprint = keyFingerprint
			} else {
				replica, err = lookup()
				if err != nil {
					return nil, false, err
				}
			}
		}
		return replica, false, nil
	}
	if replica.ProvisionStatus == SeedanceAssetReplicaCreating && replica.PollLeaseUntil > now {
		return replica, false, nil
	}
	updated := DB.WithContext(ctx).Model(&SeedanceAssetReplica{}).
		Where("id = ? AND (provision_status = ? OR (provision_status = ? AND poll_lease_until <= ?))", replica.ID, SeedanceAssetReplicaFailed, SeedanceAssetReplicaCreating, now).
		Updates(map[string]any{
			"channel_id":          channelID,
			"key_fingerprint":     keyFingerprint,
			"provision_status":    SeedanceAssetReplicaCreating,
			"poll_lease_until":    leaseUntil,
			"upstream_group_id":   "",
			"upstream_asset_id":   "",
			"upstream_deleted_at": int64(0),
			"last_error":          "",
		})
	if updated.Error != nil {
		return nil, false, updated.Error
	}
	if updated.RowsAffected == 1 {
		replica.ChannelID = channelID
		replica.KeyFingerprint = keyFingerprint
		replica.ProvisionStatus = SeedanceAssetReplicaCreating
		replica.PollLeaseUntil = leaseUntil
		return replica, true, nil
	}
	replica, err = lookup()
	return replica, false, err
}

// CompleteSeedanceAssetReplica 保存目标账号素材 ID，并立即安排审核状态轮询。
func CompleteSeedanceAssetReplica(ctx context.Context, id uint, leaseUntil int64, upstreamGroupID, upstreamAssetID string, now int64) (bool, error) {
	if DB == nil || id == 0 || leaseUntil <= 0 || strings.TrimSpace(upstreamGroupID) == "" || strings.TrimSpace(upstreamAssetID) == "" {
		return false, errors.New("invalid Seedance asset replica completion")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	updated := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var replica SeedanceAssetReplica
		if err := tx.First(&replica, id).Error; err != nil {
			return err
		}
		if tx.Dialector.Name() == "sqlite" {
			if err := tx.Model(&SeedanceAssetGroup{}).Where("id = ? AND user_id = ?", replica.LocalGroupID, replica.UserID).
				UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
				return err
			}
		}
		var group SeedanceAssetGroup
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", replica.LocalGroupID, replica.UserID, "Deleting").First(&group).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		var asset SeedanceAsset
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ? AND status <> ?", replica.LocalAssetID, replica.UserID, "Deleting").First(&asset).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		result := tx.Model(&replica).
			Where("id = ? AND provision_status = ? AND poll_lease_until = ?", id, SeedanceAssetReplicaCreating, leaseUntil).
			Updates(map[string]any{
				"upstream_group_id": upstreamGroupID,
				"upstream_asset_id": upstreamAssetID,
				"provision_status":  SeedanceAssetReplicaReady,
				"status":            "Processing",
				"poll_attempts":     0,
				"next_poll_at":      now,
				"poll_lease_until":  int64(0),
				"last_error":        "",
			})
		if result.Error != nil {
			return result.Error
		}
		updated = result.RowsAffected == 1
		return nil
	})
	if err != nil && updated {
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var stored SeedanceAssetReplica
		if checkErr := DB.WithContext(checkCtx).Where("id = ? AND upstream_group_id = ? AND upstream_asset_id = ? AND provision_status = ?", id, upstreamGroupID, upstreamAssetID, SeedanceAssetReplicaReady).First(&stored).Error; checkErr == nil {
			return true, nil
		}
		return false, ErrSeedanceAssetCommitUncertain
	}
	return updated, err
}

// FailSeedanceAssetReplica 释放失败导入的租约，避免把上游错误伪装成可用映射。
func FailSeedanceAssetReplica(id uint, leaseUntil int64, message string) error {
	return DB.Model(&SeedanceAssetReplica{}).
		Where("id = ? AND provision_status = ? AND poll_lease_until = ?", id, SeedanceAssetReplicaCreating, leaseUntil).
		Updates(map[string]any{"provision_status": SeedanceAssetReplicaFailed, "poll_lease_until": int64(0), "last_error": truncateSeedanceAssetMappingError(message)}).Error
}

// ListDueSeedanceAssetReplicas 返回到期且租约空闲的目标账号副本。
func ListDueSeedanceAssetReplicas(now int64, limit int) ([]*SeedanceAssetReplica, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if limit <= 0 {
		limit = 1
	}
	var replicas []*SeedanceAssetReplica
	err := DB.Model(&SeedanceAssetReplica{}).
		Where("local_asset_id IN (?)", DB.Model(&SeedanceAsset{}).Select("id").Where("status <> ?", "Deleting")).
		Where("provision_status = ?", SeedanceAssetReplicaReady).
		Where("LOWER(status) IN ?", []string{"processing", "pending"}).
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Order("next_poll_at ASC").Order("id ASC").Limit(limit).Find(&replicas).Error
	return replicas, err
}

// HasDueSeedanceAssetReplicas 判断是否存在需要后台同步的目标账号副本。
func HasDueSeedanceAssetReplicas(now int64) bool {
	if DB == nil {
		return false
	}
	var marker struct{ ID uint }
	result := DB.Model(&SeedanceAssetReplica{}).
		Where("local_asset_id IN (?)", DB.Model(&SeedanceAsset{}).Select("id").Where("status <> ?", "Deleting")).
		Where("provision_status = ?", SeedanceAssetReplicaReady).
		Where("LOWER(status) IN ?", []string{"processing", "pending"}).
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Select("id").Limit(1).Find(&marker)
	return result.Error == nil && result.RowsAffected > 0
}

// ClaimSeedanceAssetReplicaForPolling 使用更新时间和租约避免重复查询同一上游素材。
func ClaimSeedanceAssetReplicaForPolling(id uint, expectedUpdatedAt time.Time, now, leaseUntil int64) (bool, error) {
	if leaseUntil <= now {
		return false, nil
	}
	result := DB.Model(&SeedanceAssetReplica{}).
		Where("id = ? AND updated_at = ? AND provision_status = ?", id, expectedUpdatedAt, SeedanceAssetReplicaReady).
		Where("LOWER(status) IN ?", []string{"processing", "pending"}).
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Updates(map[string]any{"poll_lease_until": leaseUntil, "updated_at": maxSeedanceAssetUpdateTime(expectedUpdatedAt)})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// UpdateSeedanceAssetReplicaPollState 只允许当前租约持有者更新审核状态。
func UpdateSeedanceAssetReplicaPollState(id uint, leaseUntil int64, updates map[string]any) (bool, error) {
	if leaseUntil <= 0 || len(updates) == 0 {
		return false, nil
	}
	var current SeedanceAssetReplica
	if err := DB.Select("updated_at").Where("id = ? AND poll_lease_until = ?", id, leaseUntil).First(&current).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	values := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		values[key] = value
	}
	values["updated_at"] = maxSeedanceAssetUpdateTime(current.UpdatedAt)
	result := DB.Model(&SeedanceAssetReplica{}).
		Where("id = ? AND poll_lease_until = ? AND updated_at = ?", id, leaseUntil, current.UpdatedAt).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func truncateSeedanceAssetMappingError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 512 {
		return message[:512]
	}
	return message
}

// ListDueSeedanceAssets 返回到达轮询时间且没有有效租约的处理中素材。
func ListDueSeedanceAssets(now int64, limit int) ([]*SeedanceAsset, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if limit <= 0 {
		limit = 1
	}
	var assets []*SeedanceAsset
	err := DB.Where("(status_key IN ('processing', 'pending') OR (status_key = ? AND failure_code = ?))", "failed", SeedanceAssetReviewTimeoutCode).
		Where("(local_only IS NULL OR local_only = ? OR primary_upstream_id <> ?)", true, "").
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Order("next_poll_at ASC").Order("id ASC").Limit(limit).Find(&assets).Error
	return assets, err
}

// HasDueSeedanceAssets 判断是否存在需要后台同步的素材，供系统任务调度器决定是否创建任务。
func HasDueSeedanceAssets(now int64) bool {
	if DB == nil {
		return false
	}
	var marker struct {
		ID uint
	}
	result := DB.Model(&SeedanceAsset{}).
		Where("(status_key IN ('processing', 'pending') OR (status_key = ? AND failure_code = ?))", "failed", SeedanceAssetReviewTimeoutCode).
		Where("(local_only IS NULL OR local_only = ? OR primary_upstream_id <> ?)", true, "").
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Select("id").Limit(1).Find(&marker)
	return result.Error == nil && result.RowsAffected > 0 || HasDueSeedanceAssetReplicas(now)
}

// ClaimSeedanceAssetForPolling 使用读取时的更新时间抢占素材，避免过期列表结果重复发起上游请求。
func ClaimSeedanceAssetForPolling(id uint, expectedUpdatedAt time.Time, now, leaseUntil int64) (bool, error) {
	if leaseUntil <= now {
		return false, nil
	}
	// MySQL 时间戳精度为毫秒；同一时钟刻度内也必须产生严格更新的版本。
	updatedAt := maxSeedanceAssetUpdateTime(expectedUpdatedAt)
	result := DB.Model(&SeedanceAsset{}).
		Where("id = ? AND updated_at = ?", id, expectedUpdatedAt).
		Where("(status_key IN ('processing', 'pending') OR (status_key = ? AND failure_code = ?))", "failed", SeedanceAssetReviewTimeoutCode).
		Where("(next_poll_at = 0 OR next_poll_at <= ?)", now).
		Where("(poll_lease_until = 0 OR poll_lease_until <= ?)", now).
		Updates(map[string]any{
			"poll_lease_until": leaseUntil,
			"updated_at":       updatedAt,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// UpdateSeedanceAssetIfUnchanged 仅在素材仍保持读取版本时写入，防止手动刷新覆盖后台任务的新状态。
func UpdateSeedanceAssetIfUnchanged(id uint, expectedUpdatedAt time.Time, updates map[string]any) (bool, error) {
	if len(updates) == 0 {
		return false, nil
	}
	values := seedanceAssetStatusUpdateValues(updates)
	// 时间戳承担 CAS 版本，避免同毫秒更新让旧请求仍满足条件。
	values["updated_at"] = maxSeedanceAssetUpdateTime(expectedUpdatedAt)
	result := DB.Model(&SeedanceAsset{}).
		Where("id = ? AND updated_at = ?", id, expectedUpdatedAt).
		Where("status <> ? AND poll_lease_until = ?", "Deleting", 0).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// UpdateSeedanceAssetPollState 只允许当前轮询租约持有者提交状态，旧请求即使晚返回也不能回写。
func UpdateSeedanceAssetPollState(id uint, leaseUntil int64, updates map[string]any) (bool, error) {
	if leaseUntil <= 0 || len(updates) == 0 {
		return false, nil
	}
	// 读取当前租约版本，最终条件同时检查版本与租约，防止释放后的晚返回覆盖。
	var current SeedanceAsset
	if err := DB.Select("updated_at").Where("id = ? AND poll_lease_until = ?", id, leaseUntil).First(&current).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	values := seedanceAssetStatusUpdateValues(updates)
	values["updated_at"] = maxSeedanceAssetUpdateTime(current.UpdatedAt)
	result := DB.Model(&SeedanceAsset{}).
		Where("id = ? AND poll_lease_until = ?", id, leaseUntil).
		Where("updated_at = ? AND status <> ?", current.UpdatedAt, "Deleting").
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func seedanceAssetStatusUpdateValues(updates map[string]any) map[string]any {
	values := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		values[key] = value
	}
	if status, ok := values["status"].(string); ok {
		statusKey := SeedanceAssetStatusIndexKey(status)
		values["status_key"] = statusKey
		values["pending_status"] = SeedanceAssetStatusRequiresRefresh(statusKey)
	}
	return values
}

// maxSeedanceAssetUpdateTime 保证所有受支持数据库在毫秒精度下仍有严格递增的 CAS 版本。
func maxSeedanceAssetUpdateTime(previous time.Time) time.Time {
	now := time.Now()
	if now.After(previous.Add(time.Millisecond)) {
		return now
	}
	return previous.Add(time.Millisecond)
}
