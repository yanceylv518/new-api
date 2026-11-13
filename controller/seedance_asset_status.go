package controller

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const seedanceStatusAccountsKey = "seedance_asset_status_accounts"
const seedanceStatusModelsKey = "seedance_asset_status_models"

// seedanceStatusAccounts 返回当前凭证允许的素材账号，不扩展令牌分组或模型权限。
func seedanceStatusAccounts(c *gin.Context) ([]string, error) {
	if value, exists := c.Get(seedanceStatusAccountsKey); exists {
		if accounts, ok := value.([]string); ok {
			return accounts, nil
		}
	}
	accounts, err := service.SeedanceAssetStatusAccounts(c.Request.Context(), c.GetInt("id"), seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c))
	if err != nil && !errors.Is(err, service.ErrSeedanceAssetChannelUnavailable) {
		return nil, err
	}
	if len(accounts) == 0 {
		accounts = []string{""}
	}
	c.Set(seedanceStatusAccountsKey, accounts)
	return accounts, nil
}

// seedanceAssetStatusExpression 让本站素材状态反映当前凭证范围内的副本，而非其他分组的副本。
func seedanceAssetStatusExpression(c *gin.Context, alias string, _ bool) (string, []any, error) {
	expression, args, _, _, err := seedanceAssetStatusExpressions(c, alias)
	return expression, args, err
}

func seedanceAssetStatusExpressions(c *gin.Context, alias string) (string, []any, string, []any, error) {
	accounts, err := seedanceStatusAccounts(c)
	if err != nil {
		return "", nil, "", nil, err
	}
	table := model.DB.NamingStrategy.TableName("SeedanceAssetReplica")
	groupTable := model.DB.NamingStrategy.TableName("SeedanceAssetGroup")
	match := fmt.Sprintf("s.user_id = %s.user_id AND s.local_asset_id = %s.id AND s.account_fingerprint IN ? AND NOT EXISTS (SELECT 1 FROM %s AS hg WHERE hg.user_id = %s.user_id AND hg.group_id = %s.group_id AND LOWER(hg.group_type) = 'livenessface')", alias, alias, groupTable, alias, alias)
	ready := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS s WHERE %s AND s.provision_status = ? AND s.upstream_asset_id <> '' AND s.upstream_deleted_at = 0 AND LOWER(s.status) IN ('active','success','succeeded'))", table, match)
	pending := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS s WHERE %s AND s.upstream_deleted_at = 0 AND (s.provision_status = ? OR (s.provision_status = ? AND s.next_poll_at > 0 AND s.poll_attempts < ?) OR (s.provision_status = ? AND LOWER(s.status) IN ('pending','processing') AND (COALESCE(s.last_error,'') = '' OR s.poll_attempts < ?))))", table, match)
	sourceAccount := fmt.Sprintf("COALESCE(NULLIF(%s.account_fingerprint,''), (SELECT NULLIF(g.account_fingerprint,'') FROM %s AS g WHERE g.user_id = %s.user_id AND g.group_id = %s.group_id AND g.status <> 'Deleting' LIMIT 1), '')", alias, groupTable, alias, alias)
	source := fmt.Sprintf("(%s.local_only IS NULL OR COALESCE(%s.primary_upstream_id,'') <> '') AND COALESCE(%s.upstream_deleted_at,0) = 0", alias, alias, alias)
	human := fmt.Sprintf("CASE WHEN (%s) AND %s = '' THEN LOWER(%s.status) WHEN ((%s) AND %s IN ? AND LOWER(%s.status) IN ('active','success','succeeded')) OR %s THEN 'active' WHEN ((%s) AND %s IN ? AND LOWER(%s.status) IN ('pending','processing')) OR %s THEN 'processing' ELSE 'failed' END", source, sourceAccount, alias, source, sourceAccount, alias, ready, source, sourceAccount, alias, pending)
	humanArgs := []any{accounts, accounts, model.SeedanceAssetReplicaReady, accounts, accounts, model.SeedanceAssetReplicaCreating, model.SeedanceAssetReplicaFailed, model.SeedanceAssetSyncMaxAttempts, model.SeedanceAssetReplicaReady, model.SeedanceAssetSyncMaxAttempts}
	if c.GetInt("token_id") == 0 {
		human = fmt.Sprintf("LOWER(%s.status)", alias)
		humanArgs = nil
	}
	value, exists := c.Get(seedanceStatusModelsKey)
	models, _ := value.(map[string][]string)
	if !exists {
		models, err = service.SeedanceAssetModelScopes(c.Request.Context(), c.GetInt("id"), seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c))
		if err != nil {
			return "", nil, "", nil, err
		}
		c.Set(seedanceStatusModelsKey, models)
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	slices.Sort(names)
	readyModels, failedModels := make([]string, 0, len(names)), make([]string, 0, len(names))
	readyArgs, failedArgs := make([]any, 0), make([]any, 0)
	timeoutModels := make([]string, 0, len(names))
	timeoutArgs := make([]any, 0)
	for _, name := range names {
		scopes := models[name]
		if len(scopes) == 0 {
			readyModels = append(readyModels, "1 = 0")
			failedModels = append(failedModels, "1 = 1")
			continue
		}
		readyModels = append(readyModels, fmt.Sprintf("(((%s) AND %s IN ? AND LOWER(%s.status) IN ('active','success','succeeded')) OR EXISTS (SELECT 1 FROM %s AS s WHERE s.user_id = %s.user_id AND s.local_asset_id = %s.id AND s.account_fingerprint IN ? AND s.provision_status = 'Ready' AND s.upstream_asset_id <> '' AND s.upstream_deleted_at = 0 AND LOWER(s.status) IN ('active','success','succeeded')))", source, sourceAccount, alias, table, alias, alias))
		readyArgs = append(readyArgs, scopes, scopes)
		failedModels = append(failedModels, fmt.Sprintf("((SELECT COUNT(DISTINCT s.account_fingerprint) FROM %s AS s WHERE s.user_id = %s.user_id AND s.local_asset_id = %s.id AND s.account_fingerprint IN ? AND (s.upstream_deleted_at <> 0 OR (s.provision_status = 'Failed' AND (s.next_poll_at = 0 OR s.poll_attempts >= ?)) OR (s.provision_status = 'Ready' AND (LOWER(s.status) = 'failed' OR (COALESCE(s.last_error,'') <> '' AND s.poll_attempts >= ?))))) + CASE WHEN (%s) AND %s IN ? AND LOWER(%s.status) = 'failed' AND NOT EXISTS (SELECT 1 FROM %s AS s WHERE s.user_id = %s.user_id AND s.local_asset_id = %s.id AND s.account_fingerprint = %s) THEN 1 ELSE 0 END) = ?", table, alias, alias, source, sourceAccount, alias, table, alias, alias, sourceAccount))
		failedArgs = append(failedArgs, scopes, model.SeedanceAssetSyncMaxAttempts, model.SeedanceAssetSyncMaxAttempts, scopes, len(scopes))
		timeout := fmt.Sprintf("(((%s) AND %s IN ? AND LOWER(%s.status) = 'failed' AND %s.failure_code = ? AND NOT EXISTS (SELECT 1 FROM %s AS s WHERE s.user_id = %s.user_id AND s.local_asset_id = %s.id AND s.account_fingerprint = %s)) OR EXISTS (SELECT 1 FROM %s AS s WHERE s.user_id = %s.user_id AND s.local_asset_id = %s.id AND s.account_fingerprint IN ? AND s.upstream_deleted_at = 0 AND s.last_error = ? AND s.poll_attempts >= ?))", source, sourceAccount, alias, alias, table, alias, alias, sourceAccount, table, alias, alias)
		timeoutModels = append(timeoutModels, "("+failedModels[len(failedModels)-1]+" AND "+timeout+")")
		timeoutArgs = append(timeoutArgs, scopes, model.SeedanceAssetSyncMaxAttempts, model.SeedanceAssetSyncMaxAttempts, scopes, len(scopes), scopes, model.SeedanceAssetReviewTimeoutCode, scopes, model.SeedanceAssetReviewTimeoutCode, model.SeedanceAssetSyncMaxAttempts)
	}
	if len(names) == 0 {
		readyModels = append(readyModels, "1 = 0")
		failedModels = append(failedModels, "1 = 1")
	}
	isHuman := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS hg WHERE hg.user_id = %s.user_id AND hg.group_id = %s.group_id AND LOWER(hg.group_type) = 'livenessface')", groupTable, alias, alias)
	expression := fmt.Sprintf("CASE WHEN LOWER(%s.status) = 'deleting' THEN 'deleting' WHEN %s.local_only = ? THEN LOWER(%s.status) WHEN %s THEN (%s) WHEN (%s) THEN 'active' WHEN (%s) THEN 'failed' ELSE 'processing' END", alias, alias, alias, isHuman, human, strings.Join(readyModels, " AND "), strings.Join(failedModels, " OR "))
	args := append([]any{true}, humanArgs...)
	args = append(args, readyArgs...)
	args = append(args, failedArgs...)
	timeoutExpression := "1 = 0"
	if len(timeoutModels) > 0 {
		timeoutExpression = strings.Join(timeoutModels, " OR ")
	}
	return expression, args, timeoutExpression, timeoutArgs, nil
}

// scheduleMissingSeedanceAssetTargets 限量补排当前凭证可见的新增同步目标。
func scheduleMissingSeedanceAssetTargets(c *gin.Context) error {
	value, _ := c.Get(seedanceStatusModelsKey)
	models, _ := value.(map[string][]string)
	scopes := make([]string, 0)
	seen := make(map[string]struct{})
	for _, fingerprints := range models {
		for _, fingerprint := range fingerprints {
			if _, exists := seen[fingerprint]; !exists {
				seen[fingerprint] = struct{}{}
				scopes = append(scopes, fingerprint)
			}
		}
	}
	if len(scopes) == 0 {
		return nil
	}
	assets, err := model.ListMissingSeedanceAssetSyncTargets(c.Request.Context(), c.GetInt("id"), scopes, 0, nil)
	if err != nil {
		return err
	}
	if len(assets) == 0 {
		return nil
	}
	groups, allowedModels, channel := seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)
	if err := service.QueueSeedanceAssetDiscovery(c.Request.Context(), c.GetInt("id"), groups, allowedModels, channel); err != nil {
		return err
	}
	return service.EnsureSeedanceAssetSynchronization(c.Request.Context(), c.GetInt("id"), assets, groups, allowedModels, channel)
}

// projectSeedanceAssetStatuses 只修改返回值，数据库中的汇总状态仍供后台调度使用。
func projectSeedanceAssetStatuses(c *gin.Context, assets []model.SeedanceAsset, compat bool) error {
	ids := make([]uint, 0, len(assets))
	for _, asset := range assets {
		if asset.LocalOnly == nil || !*asset.LocalOnly {
			ids = append(ids, asset.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if err := service.EnsureSeedanceAssetSynchronization(c.Request.Context(), c.GetInt("id"), assets, seedanceAssetSynchronizationGroups(c), seedanceAssetSynchronizationModels(c), seedanceAssetSynchronizationChannel(c)); err != nil {
		return err
	}
	expression, args, timeoutExpression, timeoutArgs, err := seedanceAssetStatusExpressions(c, "a")
	if err != nil {
		return err
	}
	table := model.DB.NamingStrategy.TableName("SeedanceAsset")
	query := model.DB.WithContext(c.Request.Context()).Table(table + " AS a")
	if compat {
		replicas := model.DB.NamingStrategy.TableName("SeedanceAssetReplica")
		query = query.Joins("LEFT JOIN " + replicas + " AS r ON r.id = a.compat_optimize_replica_id AND r.user_id = a.user_id AND r.local_asset_id = a.id")
	}
	type statusRow struct {
		ID             uint
		Status         string
		ReviewTimedOut bool
		Human          bool
	}
	var rows []statusRow
	groupTable := model.DB.NamingStrategy.TableName("SeedanceAssetGroup")
	human := fmt.Sprintf("EXISTS (SELECT 1 FROM %s AS hg WHERE hg.user_id = a.user_id AND hg.group_id = a.group_id AND LOWER(hg.group_type) = 'livenessface')", groupTable)
	args = append(args, timeoutArgs...)
	if err := query.Select("a.id, "+expression+" AS status, ("+timeoutExpression+") AS review_timed_out, "+human+" AS human", args...).Where("a.user_id = ? AND a.id IN ?", c.GetInt("id"), ids).Scan(&rows).Error; err != nil {
		return err
	}
	statuses := make(map[uint]statusRow, len(rows))
	for _, row := range rows {
		statuses[row.ID] = row
	}
	for index := range assets {
		asset := &assets[index]
		if asset.LocalOnly != nil && *asset.LocalOnly {
			continue
		}
		row := statuses[asset.ID]
		switch row.Status {
		case "active":
			asset.Status, asset.FailureCode = "Active", ""
		case "processing":
			asset.Status, asset.FailureCode = "Processing", ""
		case "deleting":
			asset.Status = "Deleting"
		default:
			asset.Status = "Failed"
			if asset.LocalOnly != nil && row.ReviewTimedOut && !row.Human {
				asset.FailureCode = model.SeedanceAssetReviewTimeoutCode
			} else if asset.LocalOnly != nil && !row.Human || asset.FailureCode != model.SeedanceAssetReviewTimeoutCode {
				asset.FailureCode = "Asset unavailable for this group or model"
			}
		}
	}
	return nil
}
