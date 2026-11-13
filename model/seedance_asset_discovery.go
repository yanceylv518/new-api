package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const SystemTaskTypeSeedanceAssetDiscovery = "seedance_asset_discovery"
const SeedanceAssetDiscoveryBatchSize = 100
const seedanceAssetLegacyScopePattern = `{"version":1,%`

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
func RejectSeedanceAssetDiscovery(ctx context.Context, snapshot *SeedanceAssetDiscovery, reason ...string) error {
	message := "invalid discovery scope"
	if len(reason) > 0 {
		message = reason[0]
	}
	return DB.WithContext(ctx).Model(&SeedanceAssetDiscovery{}).
		Where(map[string]any{"id": snapshot.ID, "cursor": snapshot.Cursor, "scan_revision": snapshot.ScanRevision, "revision": snapshot.Revision}).
		Updates(map[string]any{"next_attempt_at": -1, "last_run_at": time.Now().UnixNano(), "last_error": message}).Error
}

// ListSeedanceAssetDiscoveryBatch 限定每次扫描的新素材数量。
func ListSeedanceAssetDiscoveryBatch(ctx context.Context, userID int, afterID uint) ([]SeedanceAsset, error) {
	assetTable := DB.NamingStrategy.TableName("SeedanceAsset")
	groupTable := DB.NamingStrategy.TableName("SeedanceAssetGroup")
	groups := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS g WHERE g.user_id = a.user_id AND g.group_id = a.group_id AND g.status <> ?)", groupTable)
	aigc := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS g WHERE g.user_id = a.user_id AND g.group_id = a.group_id AND g.status <> ? AND LOWER(COALESCE(g.group_type,'')) <> 'livenessface')", groupTable)
	var assets []SeedanceAsset
	err := DB.WithContext(ctx).Table(assetTable+" AS a").Select("a.*").
		Where("a.user_id = ? AND a.id > ? AND a.status <> ?", userID, afterID, "Deleting").
		Where("((a.local_only = ? AND "+aigc+") OR (a.primary_sync_scope LIKE ? AND "+groups+"))", false, "Deleting", seedanceAssetLegacyScopePattern, "Deleting").
		Order("a.id").Limit(SeedanceAssetDiscoveryBatchSize).Find(&assets).Error
	return assets, err
}

// ListMissingSeedanceAssetSyncTargets 同时发现缺失目标和需要当前凭证重授权的在途目标。
func ListMissingSeedanceAssetSyncTargets(ctx context.Context, userID int, scopes []string, afterID uint, assetIDs []uint, primaryAccounts ...[]string) ([]SeedanceAsset, error) {
	var accounts []string
	if len(primaryAccounts) > 0 {
		accounts = primaryAccounts[0]
	}
	if len(scopes) == 0 && len(accounts) == 0 {
		return nil, nil
	}
	assetTable := DB.NamingStrategy.TableName("SeedanceAsset")
	groupTable := DB.NamingStrategy.TableName("SeedanceAssetGroup")
	replicaTable := DB.NamingStrategy.TableName("SeedanceAssetReplica")
	missing := fmt.Sprintf("((SELECT COUNT(DISTINCT s.account_fingerprint) FROM %s AS s WHERE s.user_id = a.user_id AND s.local_asset_id = a.id AND s.account_fingerprint IN ?) + CASE WHEN COALESCE(a.primary_upstream_id,'') <> '' AND a.account_fingerprint IN ? AND NOT EXISTS (SELECT 1 FROM %s AS s WHERE s.user_id = a.user_id AND s.local_asset_id = a.id AND s.account_fingerprint = a.account_fingerprint) THEN 1 ELSE 0 END) < ?", replicaTable, replicaTable)
	groups := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS g WHERE g.user_id = a.user_id AND g.group_id = a.group_id AND g.status <> ? AND LOWER(COALESCE(g.group_type,'')) <> 'livenessface')", groupTable)
	availableGroup := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS g WHERE g.user_id = a.user_id AND g.group_id = a.group_id AND g.status <> ?)", groupTable)
	staleReplica := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS s WHERE s.user_id = a.user_id AND s.local_asset_id = a.id AND s.account_fingerprint IN ? AND s.upstream_deleted_at = 0 AND LOWER(s.status) IN ('pending','processing') AND s.sync_scope LIKE ?)", replicaTable)
	conditions := make([]string, 0, 2)
	args := make([]any, 0)
	if len(scopes) > 0 {
		conditions = append(conditions, "(a.local_only = ? AND "+groups+" AND ("+missing+" OR "+staleReplica+"))")
		args = append(args, false, "Deleting", scopes, scopes, len(scopes), scopes, seedanceAssetLegacyScopePattern)
	}
	if len(accounts) > 0 {
		conditions = append(conditions, "(a.primary_sync_scope LIKE ? AND a.account_fingerprint IN ? AND a.upstream_deleted_at = 0 AND (LOWER(a.status) IN ('pending','processing') OR (LOWER(a.status) = 'failed' AND a.failure_code = ?)) AND (a.local_only IS NULL OR (a.local_only = ? AND COALESCE(a.primary_upstream_id,'') <> '')) AND "+availableGroup+")")
		args = append(args, seedanceAssetLegacyScopePattern, accounts, SeedanceAssetReviewTimeoutCode, false, "Deleting")
	}
	var assets []SeedanceAsset
	query := DB.WithContext(ctx).Table(assetTable+" AS a").Select("a.*").
		Where("a.user_id = ? AND a.id > ? AND a.status <> ?", userID, afterID, "Deleting").
		Where("("+strings.Join(conditions, " OR ")+")", args...)
	if len(assetIDs) > 0 {
		query = query.Where("a.id IN ?", assetIDs)
	}
	err := query.Order("a.id").Limit(SeedanceAssetDiscoveryBatchSize).Find(&assets).Error
	return assets, err
}

// AuthorizeSeedanceAssetPrimaryScope 只在原记录和组仍可用且租约空闲时保存新授权。
func AuthorizeSeedanceAssetPrimaryScope(ctx context.Context, asset *SeedanceAsset, scope string, now int64) (bool, error) {
	groups := DB.WithContext(ctx).Model(&SeedanceAssetGroup{}).Select("group_id").Where("user_id = ? AND status <> ?", asset.UserID, "Deleting")
	result := DB.WithContext(ctx).Model(&SeedanceAsset{}).
		Where("id = ? AND user_id = ? AND updated_at = ? AND primary_sync_scope = ?", asset.ID, asset.UserID, asset.UpdatedAt, asset.PrimarySyncScope).
		Where("status <> ? AND upstream_deleted_at = 0 AND poll_lease_until <= ? AND group_id IN (?)", "Deleting", now, groups).
		Updates(map[string]any{"primary_sync_scope": scope, "poll_attempts": 0, "next_poll_at": now, "poll_lease_until": int64(0), "updated_at": maxSeedanceAssetUpdateTime(asset.UpdatedAt)})
	return result.RowsAffected == 1, result.Error
}

// ReauthorizeSeedanceAssetReplica 在租约到期后替换旧授权，不改变上游素材身份。
func ReauthorizeSeedanceAssetReplica(ctx context.Context, replica *SeedanceAssetReplica, scope string, now int64, restart bool) (bool, error) {
	groups := DB.WithContext(ctx).Model(&SeedanceAssetGroup{}).Select("id").Where("user_id = ? AND status <> ?", replica.UserID, "Deleting")
	assets := DB.WithContext(ctx).Model(&SeedanceAsset{}).Select("id").Where("user_id = ? AND status <> ?", replica.UserID, "Deleting")
	values := map[string]any{"sync_scope": scope, "poll_lease_until": int64(0), "updated_at": maxSeedanceAssetUpdateTime(replica.UpdatedAt)}
	if restart {
		values["poll_attempts"], values["next_poll_at"], values["last_error"] = 0, now, ""
	}
	result := DB.WithContext(ctx).Model(&SeedanceAssetReplica{}).
		Where("id = ? AND user_id = ? AND updated_at = ? AND sync_scope = ?", replica.ID, replica.UserID, replica.UpdatedAt, replica.SyncScope).
		Where("poll_lease_until <= ? AND upstream_deleted_at = 0 AND local_group_id IN (?) AND local_asset_id IN (?)", now, groups, assets).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}
