package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const SystemTaskTypeSeedanceAssetDiscovery = "seedance_asset_discovery"
const SeedanceAssetDiscoveryBatchSize = 100

type SeedanceAssetDiscovery struct {
	ID            uint   `gorm:"primaryKey"`
	UserID        int    `gorm:"not null;index"`
	ScopeKey      string `gorm:"size:64;not null;uniqueIndex"`
	Scope         string `gorm:"type:text;not null"`
	Cursor        uint   `gorm:"not null;default:0"`
	Revision      uint64 `gorm:"not null"`
	ScanRevision  uint64 `gorm:"not null"`
	LastRunAt     int64  `gorm:"not null;default:0"`
	NextAttemptAt int64  `gorm:"not null;default:0;index"`
	Attempts      int    `gorm:"not null;default:0"`
	LastError     string `gorm:"size:128"`
}

func SeedanceAssetDiscoveryScopeKey(userID int, scope string) string {
	return fmt.Sprintf("%x", common.Sha256Raw([]byte(fmt.Sprintf("%d:%s", userID, scope))))
}

// QueueSeedanceAssetDiscovery 合并相同范围的请求，并保留扫描期间收到的请求。
func QueueSeedanceAssetDiscovery(ctx context.Context, userID int, scope string) error {
	if userID <= 0 || scope == "" {
		return errors.New("invalid asset discovery scope")
	}
	key := SeedanceAssetDiscoveryScopeKey(userID, scope)
	job := SeedanceAssetDiscovery{UserID: userID, ScopeKey: key, Scope: scope, Revision: 1, ScanRevision: 1}
	return DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "scope_key"}},
		DoUpdates: clause.Assignments(map[string]any{
			"scope": scope, "revision": gorm.Expr("revision + ?", 1),
			"next_attempt_at": gorm.Expr("CASE WHEN next_attempt_at < 0 THEN 0 ELSE next_attempt_at END"), "last_error": "",
		}),
	}).Create(&job).Error
}

func NextSeedanceAssetDiscovery(ctx context.Context, now int64) (*SeedanceAssetDiscovery, error) {
	var job SeedanceAssetDiscovery
	err := DB.WithContext(ctx).Where("next_attempt_at >= ? AND next_attempt_at <= ?", 0, now).Order("last_run_at").Order("id").First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &job, err
}

func HasDueSeedanceAssetDiscovery(now int64) bool {
	var job SeedanceAssetDiscovery
	err := DB.Select("id").Where("next_attempt_at >= ? AND next_attempt_at <= ?", 0, now).Limit(1).Find(&job).Error
	if err != nil {
		common.SysError("failed to check Seedance asset discovery: " + err.Error())
		return false
	}
	return job.ID != 0
}

// AdvanceSeedanceAssetDiscovery 仅推进当前扫描；新请求会在本轮结束后重新扫描。
func AdvanceSeedanceAssetDiscovery(ctx context.Context, snapshot *SeedanceAssetDiscovery, cursor uint, complete bool) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job SeedanceAssetDiscovery
		if err := lockForUpdate(tx).Where("id = ?", snapshot.ID).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if job.Cursor != snapshot.Cursor || job.ScanRevision != snapshot.ScanRevision {
			return nil
		}
		if complete && job.Revision == job.ScanRevision {
			return tx.Delete(&job).Error
		}
		values := map[string]any{"cursor": cursor, "last_run_at": time.Now().UnixNano(), "next_attempt_at": 0, "attempts": 0, "last_error": ""}
		if complete {
			values["cursor"], values["scan_revision"] = 0, job.Revision
		}
		return tx.Model(&job).Updates(values).Error
	})
}

func DeferSeedanceAssetDiscovery(ctx context.Context, snapshot *SeedanceAssetDiscovery, now int64) error {
	attempts := min(snapshot.Attempts+1, 6)
	delay := min(int64(15)<<uint(attempts-1), int64(300))
	return DB.WithContext(ctx).Model(&SeedanceAssetDiscovery{}).
		Where(map[string]any{"id": snapshot.ID, "cursor": snapshot.Cursor, "scan_revision": snapshot.ScanRevision, "revision": snapshot.Revision}).
		Updates(map[string]any{"attempts": attempts, "next_attempt_at": now + delay, "last_run_at": time.Now().UnixNano(), "last_error": "temporary discovery error"}).Error
}

// RejectSeedanceAssetDiscovery 保留损坏范围供排查，并等待同范围的新请求修复。
func RejectSeedanceAssetDiscovery(ctx context.Context, snapshot *SeedanceAssetDiscovery) error {
	return DB.WithContext(ctx).Model(&SeedanceAssetDiscovery{}).
		Where(map[string]any{"id": snapshot.ID, "cursor": snapshot.Cursor, "scan_revision": snapshot.ScanRevision, "revision": snapshot.Revision}).
		Updates(map[string]any{"next_attempt_at": -1, "last_run_at": time.Now().UnixNano(), "last_error": "invalid discovery scope"}).Error
}

// ListSeedanceAssetDiscoveryBatch 限定每次扫描的新素材数量。
func ListSeedanceAssetDiscoveryBatch(ctx context.Context, userID int, afterID uint) ([]SeedanceAsset, error) {
	assetTable := DB.NamingStrategy.TableName("SeedanceAsset")
	groupTable := DB.NamingStrategy.TableName("SeedanceAssetGroup")
	groups := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS g WHERE g.user_id = a.user_id AND g.group_id = a.group_id AND g.status <> ? AND LOWER(COALESCE(g.group_type,'')) <> 'livenessface')", groupTable)
	var assets []SeedanceAsset
	err := DB.WithContext(ctx).Table(assetTable+" AS a").Select("a.*").
		Where("a.user_id = ? AND a.id > ? AND a.status <> ? AND a.local_only = ?", userID, afterID, "Deleting", false).
		Where(groups, "Deleting").Order("a.id").Limit(SeedanceAssetDiscoveryBatchSize).Find(&assets).Error
	return assets, err
}

// ListMissingSeedanceAssetSyncTargets 只发现新素材，游标按素材 ID 递增。
func ListMissingSeedanceAssetSyncTargets(ctx context.Context, userID int, scopes []string, afterID uint, assetIDs []uint) ([]SeedanceAsset, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	assetTable := DB.NamingStrategy.TableName("SeedanceAsset")
	groupTable := DB.NamingStrategy.TableName("SeedanceAssetGroup")
	replicaTable := DB.NamingStrategy.TableName("SeedanceAssetReplica")
	missing := fmt.Sprintf("((SELECT COUNT(DISTINCT s.account_fingerprint) FROM %s AS s WHERE s.user_id = a.user_id AND s.local_asset_id = a.id AND s.account_fingerprint IN ?) + CASE WHEN COALESCE(a.primary_upstream_id,'') <> '' AND a.account_fingerprint IN ? AND NOT EXISTS (SELECT 1 FROM %s AS s WHERE s.user_id = a.user_id AND s.local_asset_id = a.id AND s.account_fingerprint = a.account_fingerprint) THEN 1 ELSE 0 END) < ?", replicaTable, replicaTable)
	groups := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS g WHERE g.user_id = a.user_id AND g.group_id = a.group_id AND g.status <> ? AND LOWER(COALESCE(g.group_type,'')) <> 'livenessface')", groupTable)
	var assets []SeedanceAsset
	query := DB.WithContext(ctx).Table(assetTable+" AS a").Select("a.*").
		Where("a.user_id = ? AND a.id > ? AND a.status <> ? AND a.local_only = ?", userID, afterID, "Deleting", false).
		Where(groups, "Deleting").Where(missing, scopes, scopes, len(scopes))
	if len(assetIDs) > 0 {
		query = query.Where("a.id IN ?", assetIDs)
	}
	err := query.Order("a.id").Limit(SeedanceAssetDiscoveryBatchSize).Find(&assets).Error
	return assets, err
}
