package controller

import (
	"database/sql"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func seedanceCompatCounts(c *gin.Context) (int64, int64, int64, int64, error) {
	userID := c.GetInt("id")
	groups := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetGroup{}).
		Where("user_id = ? AND status <> ?", userID, "Deleting")
	var groupCount int64
	if err := groups.Count(&groupCount).Error; err != nil {
		return 0, 0, 0, 0, err
	}
	visibleGroups := model.DB.Model(&model.SeedanceAssetGroup{}).Select("group_id").Where("user_id = ? AND status <> ?", userID, "Deleting")
	var counts struct {
		Assets   int64
		Active   int64
		Channels int64
	}
	assetTable := model.DB.NamingStrategy.TableName("SeedanceAsset")
	replicaTable := model.DB.NamingStrategy.TableName("SeedanceAssetReplica")
	statusExpr, statusArgs, err := seedanceAssetStatusExpression(c, "a", true)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if err := scheduleMissingSeedanceAssetTargets(c); err != nil {
		return 0, 0, 0, 0, err
	}
	err = model.DB.WithContext(c.Request.Context()).Table(assetTable+" AS a").
		Joins("LEFT JOIN "+replicaTable+" AS r ON r.id = a.compat_optimize_replica_id AND r.user_id = a.user_id AND r.local_asset_id = a.id").
		Select("COUNT(*) AS assets, COALESCE(SUM(CASE WHEN ("+statusExpr+") IN ('active', 'success', 'succeeded') THEN 1 ELSE 0 END), 0) AS active, COUNT(DISTINCT a.channel_id) AS channels", statusArgs...).
		Where("a.user_id = ? AND a.status <> ? AND a.group_id IN (?)", userID, "Deleting", visibleGroups).Scan(&counts).Error
	return groupCount, counts.Assets, counts.Active, counts.Channels, err
}

func SeedanceCompatAssetSummary(c *gin.Context) {
	groups, assets, _, _, err := seedanceCompatCounts(c)
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, seedanceCompatDataResponse{Data: struct {
		GroupCount int64 `json:"group_count"`
		AssetCount int64 `json:"asset_count"`
	}{GroupCount: groups, AssetCount: assets}})
}

type seedanceCompatClientData struct {
	ClientKey    string `json:"clientKey"`
	ClientName   string `json:"clientName"`
	DisplayKey   string `json:"displayKey"`
	GroupCount   int64  `json:"groupCount"`
	AssetCount   int64  `json:"assetCount"`
	ActiveCount  int64  `json:"activeCount"`
	ChannelCount int64  `json:"channelCount"`
}

func SeedanceCompatAssetClientList(c *gin.Context) {
	groups, assets, active, channels, err := seedanceCompatCounts(c)
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	page, size := seedanceCompatPage(c)
	rows := make([]seedanceCompatClientData, 0, 1)
	if page == 1 {
		key := c.GetString("token_key")
		masked := "sk****"
		if len(key) >= 2 {
			masked += key[len(key)-2:]
		}
		rows = append(rows, seedanceCompatClientData{ClientKey: masked, ClientName: c.GetString("token_name"), DisplayKey: masked,
			GroupCount: groups, AssetCount: assets, ActiveCount: active, ChannelCount: channels})
	}
	c.JSON(http.StatusOK, seedanceCompatPageResponse{Data: rows, Total: 1, Page: page, PageSize: size})
}

type seedanceCompatTreeRow struct {
	GroupRowID   uint
	GroupID      string
	Name         string
	Description  string
	GroupType    string
	CompatType   string
	BindModel    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	AssetCount   int64
	AssetID      sql.NullString
	AssetName    sql.NullString
	AssetType    sql.NullString
	AssetStatus  sql.NullString
	FailureCode  sql.NullString
	AssetCreate  sql.NullTime
	AssetUpdate  sql.NullTime
	OptimizeID   sql.NullInt64
	ReplicaID    sql.NullInt64
	ReplicaState sql.NullString
}

// 素材树按数据库游标流式输出，避免大量素材时构造整个嵌套响应。
func SeedanceCompatAssetTree(c *gin.Context) {
	groupTable := model.DB.NamingStrategy.TableName("SeedanceAssetGroup")
	assetTable := model.DB.NamingStrategy.TableName("SeedanceAsset")
	replicaTable := model.DB.NamingStrategy.TableName("SeedanceAssetReplica")
	statusExpr, statusArgs, err := seedanceAssetStatusExpression(c, "a", true)
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	if err := scheduleMissingSeedanceAssetTargets(c); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	counts := model.DB.WithContext(c.Request.Context()).Table(assetTable).
		Select("user_id, group_id, COUNT(*) AS asset_count").
		Where("user_id = ? AND status <> ?", c.GetInt("id"), "Deleting").Group("user_id, group_id")
	selectionArgs := append(append([]any{}, statusArgs...), statusArgs...)
	query := model.DB.WithContext(c.Request.Context()).Table(groupTable+" AS g").
		Select("g.id AS group_row_id, g.group_id, g.name, g.description, g.group_type, g.compat_type, g.bind_model, g.created_at, g.updated_at, "+
			"COALESCE(counts.asset_count, 0) AS asset_count, "+
			"a.asset_id, a.name AS asset_name, a.asset_type, ("+statusExpr+") AS asset_status, a.failure_code, a.created_at AS asset_create, a.updated_at AS asset_update, "+
			"a.compat_optimize_replica_id AS optimize_id, r.id AS replica_id, ("+statusExpr+") AS replica_state", selectionArgs...).
		Joins("LEFT JOIN (?) AS counts ON counts.user_id = g.user_id AND counts.group_id = g.group_id", counts).
		Joins("LEFT JOIN "+assetTable+" AS a ON a.user_id = g.user_id AND a.group_id = g.group_id AND a.status <> ?", "Deleting").
		Joins("LEFT JOIN "+replicaTable+" AS r ON r.id = a.compat_optimize_replica_id AND r.user_id = a.user_id AND r.local_asset_id = a.id").
		Where("g.user_id = ? AND g.status <> ?", c.GetInt("id"), "Deleting").Order("g.id asc, a.id asc")
	rows, err := query.Rows()
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	defer rows.Close()
	c.Header("Content-Type", "application/json; charset=utf-8")
	write := func(value string) bool {
		if _, err := io.WriteString(c.Writer, value); err != nil {
			common.SysError("seedance asset tree response interrupted: " + err.Error())
			return false
		}
		return true
	}
	if !write(`{"data":[`) {
		return
	}
	firstGroup, firstAsset := true, true
	var previousGroup uint
	for rows.Next() {
		var row seedanceCompatTreeRow
		if err := model.DB.ScanRows(rows, &row); err != nil {
			common.SysError("seedance asset tree scan failed: " + err.Error())
			return
		}
		if firstGroup || row.GroupRowID != previousGroup {
			if !firstGroup {
				if !write(`]},`) {
					return
				}
			}
			group := model.SeedanceAssetGroup{GroupID: row.GroupID, Name: row.Name, Description: row.Description,
				GroupType: row.GroupType, CompatType: row.CompatType, BindModel: row.BindModel, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
			encoded, err := common.Marshal(seedanceCompatGroupView(group, row.AssetCount))
			if err != nil {
				common.SysError("seedance asset tree encoding failed: " + err.Error())
				return
			}
			if !write(string(encoded[:len(encoded)-1])) || !write(`,"assets":[`) {
				return
			}
			previousGroup, firstGroup, firstAsset = row.GroupRowID, false, true
		}
		if !row.AssetID.Valid {
			continue
		}
		if !firstAsset {
			if !write(",") {
				return
			}
		}
		asset := model.SeedanceAsset{AssetID: row.AssetID.String, GroupID: row.GroupID, Name: row.AssetName.String,
			AssetType: row.AssetType.String, Status: row.AssetStatus.String, FailureCode: row.FailureCode.String,
			CreatedAt: row.AssetCreate.Time, UpdatedAt: row.AssetUpdate.Time}
		if strings.EqualFold(asset.Status, "failed") && asset.FailureCode == "" {
			asset.FailureCode = "Asset unavailable for this group or model"
		}
		var replica *model.SeedanceAssetReplica
		if row.OptimizeID.Valid && row.OptimizeID.Int64 > 0 {
			if !row.ReplicaID.Valid {
				common.SysError("seedance asset optimization state is unavailable")
				return
			}
			asset.CompatOptimizeReplicaID = uint(row.OptimizeID.Int64)
			replica = &model.SeedanceAssetReplica{ID: uint(row.ReplicaID.Int64), Status: row.ReplicaState.String}
		}
		encoded, err := common.Marshal(seedanceCompatAssetView(asset, replica))
		if err != nil {
			common.SysError("seedance asset tree encoding failed: " + err.Error())
			return
		}
		if !write(string(encoded)) {
			return
		}
		firstAsset = false
	}
	if err := rows.Err(); err != nil {
		common.SysError("seedance asset tree query failed: " + err.Error())
		return
	}
	if !firstGroup {
		if !write(`]}`) {
			return
		}
	}
	write(`]}`)
}
