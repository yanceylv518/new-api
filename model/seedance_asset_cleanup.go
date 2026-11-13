package model

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	SeedanceAssetCleanupKindUpstreamAsset = "upstream_asset"
	SeedanceAssetCleanupKindUpstreamGroup = "upstream_group"
	SeedanceAssetCleanupKindOSSObject     = "oss_object"
	SeedanceAssetCleanupKindAssetDelete   = "asset_delete"
	SeedanceAssetCleanupKindReplicaDelete = "asset_replica_delete"
	SeedanceAssetCleanupKindGroupDelete   = "asset_group_delete"

	SeedanceAssetCleanupStatusPending   = "pending"
	SeedanceAssetCleanupStatusCompleted = "completed"
)

// SeedanceAssetCleanupJob 持久化上游素材、素材组和 OSS 对象的补偿清理。
// 外部 API 与本地数据库无法共享事务，因此失败的清理必须拥有独立的重试记录。
type SeedanceAssetCleanupJob struct {
	ID                 uint                 `gorm:"primaryKey;index:idx_seedance_asset_cleanup_due,priority:4"`
	Kind               string               `gorm:"size:32;not null;index"`
	UserID             int                  `gorm:"index"`
	LocalGroupID       uint                 `gorm:"index"`
	LocalAssetID       uint                 `gorm:"index"`
	LocalMappingID     uint                 `gorm:"index"`
	ChannelID          int                  `gorm:"index"`
	KeyFingerprint     string               `gorm:"size:64"`
	AccountFingerprint string               `gorm:"size:64"`
	UpstreamID         string               `gorm:"size:128"`
	UpstreamDoneAt     int64                `gorm:"not null"`
	ObjectKey          string               `gorm:"size:1023"`
	Storage            SeedanceAssetStorage `gorm:"embedded;embeddedPrefix:oss_"`
	DedupKey           string               `gorm:"size:64;not null;uniqueIndex"`
	Status             string               `gorm:"size:16;not null;index;index:idx_seedance_asset_cleanup_due,priority:1"`
	Attempts           int                  `gorm:"not null"`
	NextAttemptAt      int64                `gorm:"not null;index;index:idx_seedance_asset_cleanup_due,priority:2"`
	LeaseUntil         int64                `gorm:"not null;index;index:idx_seedance_asset_cleanup_due,priority:3"`
	LastError          string               `gorm:"type:text"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

// CreateSeedanceAssetCleanupJob 按资源摘要去重创建清理任务，避免同一孤儿资源重复积压。
func CreateSeedanceAssetCleanupJob(job *SeedanceAssetCleanupJob) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	if job == nil || strings.TrimSpace(job.Kind) == "" || strings.TrimSpace(job.DedupKey) == "" {
		return errors.New("invalid Seedance asset cleanup job")
	}
	if job.Status == "" {
		job.Status = SeedanceAssetCleanupStatusPending
	}
	if job.NextAttemptAt == 0 {
		job.NextAttemptAt = common.GetTimestamp()
	}

	var existing SeedanceAssetCleanupJob
	lookup := DB.Where("dedup_key = ?", job.DedupKey).First(&existing)
	if lookup.Error == nil {
		return nil
	}
	if !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
		return lookup.Error
	}
	if err := DB.Create(job).Error; err != nil {
		// 多节点同时记录同一资源时，唯一键竞争结果等价于已入队。
		if findErr := DB.Where("dedup_key = ?", job.DedupKey).First(&existing).Error; findErr == nil {
			return nil
		}
		return err
	}
	return nil
}

// NewSeedanceAssetDeleteCleanupJobs 为原素材和全部账号副本创建独立的删除任务。
func NewSeedanceAssetDeleteCleanupJobs(asset *SeedanceAsset, replicas []SeedanceAssetReplica) []SeedanceAssetCleanupJob {
	if asset == nil {
		return nil
	}
	jobs := make([]SeedanceAssetCleanupJob, 0, len(replicas)+1)
	jobs = append(jobs, SeedanceAssetCleanupJob{
		Kind: SeedanceAssetCleanupKindAssetDelete, UserID: asset.UserID, LocalAssetID: asset.ID,
		ChannelID: asset.ChannelID, KeyFingerprint: asset.KeyFingerprint, AccountFingerprint: asset.AccountFingerprint,
		UpstreamID: asset.UpstreamAssetID(), UpstreamDoneAt: asset.UpstreamDeletedAt,
		ObjectKey: asset.ObjectKey, Storage: asset.Storage,
		Status: SeedanceAssetCleanupStatusPending, NextAttemptAt: common.GetTimestamp(),
	})
	for _, replica := range replicas {
		jobs = append(jobs, SeedanceAssetCleanupJob{
			Kind: SeedanceAssetCleanupKindReplicaDelete, UserID: replica.UserID,
			LocalAssetID: replica.LocalAssetID, LocalMappingID: replica.ID,
			ChannelID: replica.ChannelID, KeyFingerprint: replica.KeyFingerprint, AccountFingerprint: replica.AccountFingerprint,
			UpstreamID: replica.UpstreamAssetID, UpstreamDoneAt: replica.UpstreamDeletedAt,
			Status: SeedanceAssetCleanupStatusPending, NextAttemptAt: common.GetTimestamp(),
		})
	}
	for index := range jobs {
		job := &jobs[index]
		resourceID := job.LocalAssetID
		if job.Kind == SeedanceAssetCleanupKindReplicaDelete {
			resourceID = job.LocalMappingID
		}
		// 与既有清理任务使用相同摘要，保持幂等身份不变。
		hash := sha256.New()
		for _, part := range []string{job.Kind, fmt.Sprintf("%d", job.UserID), fmt.Sprintf("%d", resourceID)} {
			_, _ = hash.Write([]byte(part))
			_, _ = hash.Write([]byte{0})
		}
		job.DedupKey = hex.EncodeToString(hash.Sum(nil))
	}
	return jobs
}

// QueueSeedanceAssetDeletion 锁内读取完整副本快照，再标记删除并建立清理任务。
func QueueSeedanceAssetDeletion(ctx context.Context, jobs []SeedanceAssetCleanupJob) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	if len(jobs) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	assets := make([]SeedanceAsset, 0, len(jobs))
	seen := make(map[uint]bool, len(jobs))
	for _, job := range jobs {
		if (job.Kind != SeedanceAssetCleanupKindAssetDelete && job.Kind != SeedanceAssetCleanupKindReplicaDelete) || job.UserID == 0 || job.LocalAssetID == 0 || strings.TrimSpace(job.DedupKey) == "" {
			return errors.New("invalid Seedance asset deletion job")
		}
		if seen[job.LocalAssetID] {
			continue
		}
		var asset SeedanceAsset
		if err := DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.LocalAssetID, job.UserID).First(&asset).Error; err != nil {
			return err
		}
		assets = append(assets, asset)
		seen[asset.ID] = true
	}
	slices.SortFunc(assets, func(left, right SeedanceAsset) int {
		if order := cmp.Compare(left.UserID, right.UserID); order != 0 {
			return order
		}
		if order := strings.Compare(left.GroupID, right.GroupID); order != 0 {
			return order
		}
		return cmp.Compare(left.ID, right.ID)
	})
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lockedAssets := make([]SeedanceAsset, 0, len(assets))
		for _, snapshot := range assets {
			// 与导入完成、组删除一致，先锁组再锁素材；SQLite 先取得写锁。
			if tx.Dialector.Name() == "sqlite" {
				if err := tx.Model(&SeedanceAsset{}).Where("id = ? AND user_id = ?", snapshot.ID, snapshot.UserID).
					UpdateColumn("updated_at", gorm.Expr("updated_at")).Error; err != nil {
					return err
				}
			}
			var group SeedanceAssetGroup
			if err := lockForUpdate(tx).Where("user_id = ? AND group_id = ?", snapshot.UserID, snapshot.GroupID).First(&group).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			var asset SeedanceAsset
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", snapshot.ID, snapshot.UserID).First(&asset).Error; err != nil {
				return err
			}
			// 已进入删除状态的素材允许幂等重新入队，避免 MySQL 将无值变化更新报告为零行。
			if asset.Status != "Deleting" || asset.PollLeaseUntil != 0 {
				if err := tx.Model(&asset).Updates(map[string]any{
					"status": "Deleting", "status_key": SeedanceAssetStatusIndexKey("Deleting"), "pending_status": true,
					"poll_lease_until": 0,
				}).Error; err != nil {
					return err
				}
			}
			lockedAssets = append(lockedAssets, asset)
		}
		// 所有删除屏障就绪后才建立读快照，既包含已完成导入，又避免空范围间隙锁。
		freshJobs := make([]SeedanceAssetCleanupJob, 0, len(jobs))
		for _, asset := range lockedAssets {
			var replicas []SeedanceAssetReplica
			if err := tx.Where("user_id = ? AND local_asset_id = ?", asset.UserID, asset.ID).Order("id ASC").Find(&replicas).Error; err != nil {
				return err
			}
			freshJobs = append(freshJobs, NewSeedanceAssetDeleteCleanupJobs(&asset, replicas)...)
		}
		for index := range freshJobs {
			job := &freshJobs[index]
			if job.Status == "" {
				job.Status = SeedanceAssetCleanupStatusPending
			}
			if job.NextAttemptAt == 0 {
				job.NextAttemptAt = common.GetTimestamp()
			}
			var existing SeedanceAssetCleanupJob
			lookup := tx.Where("dedup_key = ?", job.DedupKey).First(&existing)
			switch {
			case lookup.Error == nil:
				if err := lockForUpdate(tx).Where("id = ?", existing.ID).First(&existing).Error; err != nil {
					return err
				}
				if existing.LeaseUntil > common.GetTimestamp() {
					continue
				}
				updates := map[string]any{
					"channel_id": job.ChannelID, "key_fingerprint": job.KeyFingerprint, "account_fingerprint": job.AccountFingerprint,
					"upstream_id": job.UpstreamID, "upstream_done_at": job.UpstreamDoneAt,
					"object_key": job.ObjectKey, "next_attempt_at": common.GetTimestamp(), "lease_until": int64(0),
				}
				if existing.UpstreamID == job.UpstreamID {
					updates["upstream_done_at"] = max(existing.UpstreamDoneAt, job.UpstreamDoneAt)
				}
				if existing.Status == SeedanceAssetCleanupStatusCompleted {
					updates["status"] = SeedanceAssetCleanupStatusPending
					updates["attempts"] = 0
					updates["last_error"] = ""
				}
				if err := tx.Model(&existing).Updates(updates).Error; err != nil {
					return err
				}
			case errors.Is(lookup.Error, gorm.ErrRecordNotFound):
				if err := tx.Create(job).Error; err != nil {
					return err
				}
			default:
				return lookup.Error
			}
		}
		return nil
	})
}

// QueueSeedanceAssetGroupDeletion 原子标记素材组及其素材，并建立可重试的组删除任务。
// 组删除涉及多个上游账号和 OSS，不能只依赖当前 HTTP 请求完成。
func QueueSeedanceAssetGroupDeletion(ctx context.Context, job SeedanceAssetCleanupJob) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	if job.Kind != SeedanceAssetCleanupKindGroupDelete || job.UserID <= 0 || job.LocalGroupID == 0 || strings.TrimSpace(job.DedupKey) == "" {
		return errors.New("invalid Seedance asset group deletion job")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := common.GetTimestamp()
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var group SeedanceAssetGroup
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", job.LocalGroupID, job.UserID).First(&group).Error; err != nil {
			return err
		}
		if group.Status != "Deleting" {
			if err := tx.Model(&group).Update("status", "Deleting").Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&SeedanceAssetValidationSession{}).
			Where("user_id = ? AND local_group_id = ? AND status = ?", group.UserID, group.ID, SeedanceAssetValidationStatusSucceeded).
			Updates(map[string]any{
				"status":         SeedanceAssetValidationStatusCancelled,
				"local_group_id": uint(0), "upstream_group_id": "", "launch_url": "",
			}).Error; err != nil {
			return err
		}
		if err := tx.Model(&SeedanceAsset{}).
			Where("user_id = ? AND group_id = ?", group.UserID, group.GroupID).
			Updates(map[string]any{
				"status": "Deleting", "status_key": SeedanceAssetStatusIndexKey("Deleting"), "pending_status": true,
				"poll_lease_until": 0,
			}).Error; err != nil {
			return err
		}

		if job.Status == "" {
			job.Status = SeedanceAssetCleanupStatusPending
		}
		if job.NextAttemptAt == 0 {
			job.NextAttemptAt = now
		}
		var existing SeedanceAssetCleanupJob
		lookup := tx.Where("dedup_key = ?", job.DedupKey).First(&existing)
		switch {
		case lookup.Error == nil:
			updates := map[string]any{"next_attempt_at": now}
			if existing.Status == SeedanceAssetCleanupStatusCompleted {
				updates["status"] = SeedanceAssetCleanupStatusPending
				updates["attempts"] = 0
				updates["last_error"] = ""
			}
			if existing.LeaseUntil <= now {
				updates["lease_until"] = int64(0)
			}
			return tx.Model(&existing).Updates(updates).Error
		case errors.Is(lookup.Error, gorm.ErrRecordNotFound):
			return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&job).Error
		default:
			return lookup.Error
		}
	})
}

// SeedanceAssetGroupDeletionDedupKey 为素材组删除任务生成稳定的去重键。
func SeedanceAssetGroupDeletionDedupKey(userID int, localGroupID uint) string {
	return fmt.Sprintf("%s:%d:%d", SeedanceAssetCleanupKindGroupDelete, userID, localGroupID)
}

// EnsureSeedanceAssetGroupDeletionJobs 为历史遗留的 Deleting 素材组自动补建清理任务。
func EnsureSeedanceAssetGroupDeletionJobs(ctx context.Context, limit int) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !DB.Migrator().HasTable(&SeedanceAssetGroup{}) {
		return nil
	}
	if limit <= 0 {
		limit = 50
	}
	var groups []SeedanceAssetGroup
	if err := DB.WithContext(ctx).Where("status = ?", "Deleting").Order("id asc").Limit(limit).Find(&groups).Error; err != nil {
		return err
	}
	var jobs []SeedanceAssetCleanupJob
	if err := DB.WithContext(ctx).Where("kind = ? AND local_group_id > 0", SeedanceAssetCleanupKindGroupDelete).Find(&jobs).Error; err != nil {
		return err
	}
	existing := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		existing[fmt.Sprintf("%d:%d", job.UserID, job.LocalGroupID)] = struct{}{}
	}
	for index := range groups {
		group := &groups[index]
		if _, exists := existing[fmt.Sprintf("%d:%d", group.UserID, group.ID)]; exists {
			continue
		}
		job := SeedanceAssetCleanupJob{
			Kind:          SeedanceAssetCleanupKindGroupDelete,
			UserID:        group.UserID,
			LocalGroupID:  group.ID,
			DedupKey:      SeedanceAssetGroupDeletionDedupKey(group.UserID, group.ID),
			Status:        SeedanceAssetCleanupStatusPending,
			NextAttemptAt: common.GetTimestamp(),
		}
		if err := QueueSeedanceAssetGroupDeletion(ctx, job); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	return nil
}

// HasDueSeedanceAssetCleanupJobs 判断是否存在可执行的补偿任务。
func HasDueSeedanceAssetCleanupJobs(now int64) bool {
	if DB == nil {
		return false
	}
	if err := EnsureSeedanceAssetGroupDeletionJobs(context.Background(), 50); err != nil {
		common.SysError("failed to backfill Seedance asset group cleanup jobs: " + err.Error())
		return false
	}
	var marker struct {
		ID uint
	}
	result := DB.Model(&SeedanceAssetCleanupJob{}).
		Where("status = ?", SeedanceAssetCleanupStatusPending).
		Where("(next_attempt_at = 0 OR next_attempt_at <= ?)", now).
		Where("(lease_until = 0 OR lease_until <= ?)", now).
		Select("id").Limit(1).Find(&marker)
	return result.Error == nil && result.RowsAffected > 0
}

// ListDueSeedanceAssetCleanupJobs 返回当前到期且没有有效租约的补偿任务。
func ListDueSeedanceAssetCleanupJobs(now int64, limit int) ([]*SeedanceAssetCleanupJob, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	if err := EnsureSeedanceAssetGroupDeletionJobs(context.Background(), 50); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 1
	}
	var jobs []*SeedanceAssetCleanupJob
	err := DB.Where("status = ?", SeedanceAssetCleanupStatusPending).
		Where("(next_attempt_at = 0 OR next_attempt_at <= ?)", now).
		Where("(lease_until = 0 OR lease_until <= ?)", now).
		Order("id asc").Limit(limit).Find(&jobs).Error
	return jobs, err
}

// ClaimSeedanceAssetCleanupJob 用读取版本和租约条件抢占任务，防止多节点重复清理。
func ClaimSeedanceAssetCleanupJob(id uint, expectedUpdatedAt time.Time, now, leaseUntil int64) (bool, error) {
	if id == 0 || leaseUntil <= now {
		return false, nil
	}
	result := DB.Model(&SeedanceAssetCleanupJob{}).
		Where("id = ? AND updated_at = ?", id, expectedUpdatedAt).
		Where("status = ?", SeedanceAssetCleanupStatusPending).
		Where("(next_attempt_at = 0 OR next_attempt_at <= ?)", now).
		Where("(lease_until = 0 OR lease_until <= ?)", now).
		Updates(map[string]any{
			"lease_until": leaseUntil,
			"updated_at":  maxSeedanceAssetUpdateTime(expectedUpdatedAt),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompleteSeedanceAssetCleanupJob 删除已完成任务，避免每次素材删除都永久增加清理表数据量。
func CompleteSeedanceAssetCleanupJob(id uint, leaseUntil int64) (bool, error) {
	result := DB.Where("id = ? AND lease_until = ?", id, leaseUntil).Delete(&SeedanceAssetCleanupJob{})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// MarkSeedanceAssetCleanupUpstreamDone 标记上游阶段已结束；本站删除允许放弃失败的上游操作。
func MarkSeedanceAssetCleanupUpstreamDone(id uint, leaseUntil, doneAt int64) (bool, error) {
	if doneAt <= 0 {
		doneAt = common.GetTimestamp()
	}
	result := DB.Model(&SeedanceAssetCleanupJob{}).
		Where("id = ? AND lease_until = ?", id, leaseUntil).
		Updates(map[string]any{
			"upstream_done_at": doneAt,
			"updated_at":       time.Now(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// RescheduleSeedanceAssetCleanupJob 记录失败原因并用退避时间释放租约。
func RescheduleSeedanceAssetCleanupJob(id uint, leaseUntil int64, attempts int, nextAttemptAt int64, lastError string) (bool, error) {
	result := DB.Model(&SeedanceAssetCleanupJob{}).
		Where("id = ? AND lease_until = ?", id, leaseUntil).
		Updates(map[string]any{
			"status":          SeedanceAssetCleanupStatusPending,
			"attempts":        attempts,
			"next_attempt_at": nextAttemptAt,
			"lease_until":     int64(0),
			"last_error":      lastError,
			"updated_at":      time.Now(),
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}
