package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var seedanceCompatTimeZone = time.FixedZone("UTC+8", 8*60*60)

type seedanceCompatWriter struct {
	gin.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *seedanceCompatWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *seedanceCompatWriter) WriteHeaderNow() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}

func (w *seedanceCompatWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	return w.body.Write(data)
}

func (w *seedanceCompatWriter) WriteString(data string) (int, error) {
	w.WriteHeaderNow()
	return w.body.WriteString(data)
}

func (w *seedanceCompatWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *seedanceCompatWriter) Size() int {
	if w.status == 0 {
		return -1
	}
	return w.body.Len()
}

func (w *seedanceCompatWriter) Written() bool { return w.status != 0 }

type seedanceCompatResult struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type seedanceCompatDataResponse struct {
	Data any `json:"data"`
}

type seedanceCompatPageResponse struct {
	Data     any   `json:"data"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

type seedanceCompatSuccessResponse struct {
	Success bool `json:"success"`
	Data    any  `json:"data"`
}

type seedanceCompatSuccessPageResponse struct {
	Success  bool  `json:"success"`
	Data     any   `json:"data"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

type seedanceCompatGroupData struct {
	GroupID     string `json:"group_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	GroupType   string `json:"group_type"`
	BindModel   string `json:"bind_model"`
	AssetCount  int64  `json:"asset_count"`
	Status      int    `json:"status"`
	CreateTime  string `json:"create_time"`
	UpdateTime  string `json:"update_time"`
}

type seedanceCompatAssetData struct {
	AssetID      string `json:"assetId"`
	AssetName    string `json:"assetName"`
	AssetType    string `json:"assetType"`
	Status       string `json:"status"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	CreatedTime  string `json:"createdTime"`
	UpdatedTime  string `json:"updatedTime"`
	GroupID      string `json:"groupId,omitempty"`
}

func seedanceCompatCall(c *gin.Context, handler gin.HandlerFunc) (seedanceCompatResult, int, error) {
	original := c.Writer
	writer := &seedanceCompatWriter{ResponseWriter: original}
	c.Writer = writer
	defer func() { c.Writer = original }()
	handler(c)
	var result seedanceCompatResult
	if err := common.Unmarshal(writer.body.Bytes(), &result); err != nil {
		return result, http.StatusBadGateway, err
	}
	return result, writer.Status(), nil
}

func seedanceCompatError(c *gin.Context, status int, message string) {
	c.JSON(status, gin.H{"error": message})
}

func seedanceCompatChannelError(c *gin.Context, err error) {
	code := ""
	switch {
	case errors.Is(err, errSeedanceAssetModelNotPermitted):
		code = "AssetModelNotPermitted"
	case errors.Is(err, errSeedanceAssetNoChannel):
		code = "NoPermittedAssetChannel"
	default:
		common.SysError("seedance asset channel selection failed: " + err.Error())
		seedanceCompatError(c, http.StatusServiceUnavailable, "asset service is temporarily unavailable")
		return
	}
	c.JSON(http.StatusForbidden, struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{Error: "model is unavailable", Code: code})
}

func seedanceCompatResultError(c *gin.Context, result seedanceCompatResult, status int, err error) bool {
	if err == nil && result.Success {
		return false
	}
	if status < 400 {
		status = http.StatusBadRequest
	}
	message := "asset request failed"
	if status == http.StatusNotFound {
		message = "asset resource not found"
	} else if status == http.StatusServiceUnavailable {
		message = "asset service is temporarily unavailable"
	}
	seedanceCompatError(c, status, message)
	return true
}

func seedanceCompatSetBody(c *gin.Context, value any) error {
	body, err := common.Marshal(value)
	if err != nil {
		return err
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	return nil
}

func seedanceCompatPage(c *gin.Context) (int, int) {
	page, err := strconv.Atoi(c.Query("page"))
	if err != nil || page < 1 || page > 21474836 {
		page = 1
	}
	size, err := strconv.Atoi(c.Query("page_size"))
	if err != nil || size < 1 || size > 100 {
		size = 20
	}
	return page, size
}

func seedanceCompatGroup(c *gin.Context, id string) (*model.SeedanceAssetGroup, error) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 128 {
		return nil, gorm.ErrRecordNotFound
	}
	var group model.SeedanceAssetGroup
	query := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND group_id = ? AND status <> ?", c.GetInt("id"), id, "Deleting")
	if err := query.First(&group).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		alternate := ""
		if strings.HasPrefix(id, "mg-") {
			alternate = "group-" + strings.TrimPrefix(id, "mg-")
		} else if strings.HasPrefix(id, "group-") {
			alternate = "mg-" + strings.TrimPrefix(id, "group-")
		}
		if alternate == "" {
			return nil, err
		}
		if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND group_id = ? AND status <> ?", c.GetInt("id"), alternate, "Deleting").First(&group).Error; err != nil {
			return nil, err
		}
	}
	return &group, nil
}

func seedanceCompatAsset(c *gin.Context, id string) (*model.SeedanceAsset, error) {
	id = strings.TrimPrefix(strings.TrimSpace(id), "asset://")
	if id == "" || len(id) > 128 {
		return nil, gorm.ErrRecordNotFound
	}
	var asset model.SeedanceAsset
	groups := model.DB.Model(&model.SeedanceAssetGroup{}).Select("group_id").Where("user_id = ? AND status <> ?", c.GetInt("id"), "Deleting")
	err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND asset_id = ? AND status <> ?", c.GetInt("id"), id, "Deleting").
		Where("group_id IN (?)", groups).First(&asset).Error
	if err != nil {
		return nil, err
	}
	return &asset, nil
}

func seedanceCompatLookupError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		seedanceCompatError(c, http.StatusNotFound, "asset resource not found")
		return
	}
	seedanceCompatError(c, http.StatusInternalServerError, "asset lookup failed")
}

func seedanceCompatGroupView(group model.SeedanceAssetGroup, count int64) seedanceCompatGroupData {
	kind := group.GroupType
	if group.CompatType != "" {
		kind = group.CompatType
	}
	return seedanceCompatGroupData{
		GroupID: group.GroupID, Name: group.Name, Description: group.Description,
		GroupType: kind, BindModel: group.BindModel, AssetCount: count, Status: 1,
		CreateTime: group.CreatedAt.In(seedanceCompatTimeZone).Format(time.RFC3339),
		UpdateTime: group.UpdatedAt.In(seedanceCompatTimeZone).Format(time.RFC3339),
	}
}

func seedanceCompatAssetView(asset model.SeedanceAsset, replicas ...*model.SeedanceAssetReplica) seedanceCompatAssetData {
	status := strings.ToUpper(asset.Status)
	if asset.CompatOptimizeReplicaID != 0 && len(replicas) > 0 && replicas[0] != nil && replicas[0].ID == asset.CompatOptimizeReplicaID {
		status = strings.ToUpper(replicas[0].Status)
	}
	switch status {
	case "SUCCESS", "SUCCEEDED":
		status = "ACTIVE"
	case "PENDING":
		status = "PROCESSING"
	}
	view := seedanceCompatAssetData{
		AssetID: asset.AssetID, AssetName: asset.Name, AssetType: asset.AssetType, Status: status,
		CreatedTime: asset.CreatedAt.In(seedanceCompatTimeZone).Format("2006-01-02 15:04:05"),
		UpdatedTime: asset.UpdatedAt.In(seedanceCompatTimeZone).Format("2006-01-02 15:04:05"),
	}
	if status == "FAILED" {
		message := strings.TrimSpace(asset.FailureCode)
		if message == model.SeedanceAssetReviewTimeoutCode {
			message = "Asset review timed out. Please retry later."
		}
		if message == "" {
			message = "Asset review failed. Check the asset content and specifications."
		}
		view.ErrorMessage = message
	}
	return view
}

func seedanceCompatAssetGroupView(asset model.SeedanceAsset, replicas ...*model.SeedanceAssetReplica) seedanceCompatAssetData {
	view := seedanceCompatAssetView(asset, replicas...)
	view.GroupID = asset.GroupID
	return view
}

func seedanceCompatReplicas(c *gin.Context, assets []model.SeedanceAsset) (map[uint]*model.SeedanceAssetReplica, error) {
	ids := make([]uint, 0, len(assets))
	for _, asset := range assets {
		if asset.CompatOptimizeReplicaID != 0 {
			ids = append(ids, asset.CompatOptimizeReplicaID)
		}
	}
	result := make(map[uint]*model.SeedanceAssetReplica, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	var replicas []model.SeedanceAssetReplica
	if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND id IN ?", c.GetInt("id"), ids).Find(&replicas).Error; err != nil {
		return nil, err
	}
	for index := range replicas {
		result[replicas[index].ID] = &replicas[index]
	}
	for _, asset := range assets {
		id := asset.CompatOptimizeReplicaID
		if id != 0 && (result[id] == nil || result[id].LocalAssetID != asset.ID) {
			return nil, errors.New("asset optimization state is unavailable")
		}
	}
	return result, nil
}

func ListSeedanceCompatGroups(c *gin.Context) {
	page, size := seedanceCompatPage(c)
	query := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetGroup{}).
		Where("user_id = ? AND status <> ?", c.GetInt("id"), "Deleting")
	if kind := strings.TrimSpace(c.Query("group_type")); kind != "" {
		query = query.Where("group_type = ?", kind)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	groups := make([]model.SeedanceAssetGroup, 0)
	if err := query.Order("id desc").Offset((page - 1) * size).Limit(size).Find(&groups).Error; err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	groupIDs := make([]string, 0, len(groups))
	for _, group := range groups {
		groupIDs = append(groupIDs, group.GroupID)
	}
	counts := make(map[string]int64, len(groups))
	if len(groupIDs) > 0 {
		var rows []struct {
			GroupID string
			Count   int64
		}
		if err := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).
			Select("group_id, COUNT(*) AS count").Where("user_id = ? AND group_id IN ? AND status <> ?", c.GetInt("id"), groupIDs, "Deleting").
			Group("group_id").Scan(&rows).Error; err != nil {
			seedanceCompatLookupError(c, err)
			return
		}
		for _, row := range rows {
			counts[row.GroupID] = row.Count
		}
	}
	views := make([]seedanceCompatGroupData, 0, len(groups))
	for _, group := range groups {
		views = append(views, seedanceCompatGroupView(group, counts[group.GroupID]))
	}
	c.JSON(http.StatusOK, seedanceCompatPageResponse{Data: views, Total: total, Page: page, PageSize: size})
}

func CreateSeedanceCompatGroup(c *gin.Context) {
	var request struct {
		Name        string  `json:"name"`
		Description *string `json:"description"`
		BindModel   string  `json:"bind_model"`
		Type        string  `json:"type"`
		GroupType   string  `json:"group_type"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Name) == "" || strings.TrimSpace(request.BindModel) == "" {
		seedanceCompatError(c, http.StatusBadRequest, "name and bind_model are required")
		return
	}
	kind := request.GroupType
	if kind == "" {
		kind = request.Type
	}
	if kind != "" && !strings.EqualFold(kind, "AIGC") {
		seedanceCompatError(c, http.StatusBadRequest, "real-person groups require verification")
		return
	}
	choices, err := selectSeedanceAssetValidationChoices(c, strings.TrimSpace(request.BindModel), true)
	if err != nil {
		seedanceCompatChannelError(c, err)
		return
	}
	if err := seedanceCompatSetBody(c, gin.H{"name": request.Name, "description": request.Description, "model": choices[0].model}); err != nil {
		seedanceCompatError(c, http.StatusInternalServerError, "asset request failed")
		return
	}
	c.Set("seedance_compat_asset_api", true)
	c.Set("seedance_compat_group_type", kind)
	c.Set("seedance_compat_bind_model", strings.TrimSpace(request.BindModel))
	c.Set("seedance_compat_channel", choices[0].channel)
	result, status, err := seedanceCompatCall(c, CreateSeedanceAssetGroup)
	if seedanceCompatResultError(c, result, status, err) {
		return
	}
	var group model.SeedanceAssetGroup
	if err := common.Unmarshal(result.Data, &group); err != nil {
		seedanceCompatError(c, http.StatusInternalServerError, "asset response is invalid")
		return
	}
	group.BindModel = strings.TrimSpace(request.BindModel)
	group.CompatType = kind
	c.JSON(http.StatusOK, seedanceCompatDataResponse{Data: seedanceCompatGroupView(group, 0)})
}

func UpdateSeedanceCompatGroup(c *gin.Context) {
	group, err := seedanceCompatGroup(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	var request struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Name == nil && request.Description == nil {
		seedanceCompatError(c, http.StatusBadRequest, "name or description is required")
		return
	}
	name := group.Name
	if request.Name != nil {
		name = *request.Name
	}
	if err := seedanceCompatSetBody(c, gin.H{"name": name, "description": request.Description}); err != nil {
		seedanceCompatError(c, http.StatusInternalServerError, "asset request failed")
		return
	}
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(group.ID), 10)}}
	result, status, err := seedanceCompatCall(c, UpdateSeedanceAssetGroup)
	if seedanceCompatResultError(c, result, status, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
}

func DeleteSeedanceCompatGroup(c *gin.Context) {
	group, err := seedanceCompatGroup(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(group.ID), 10)}}
	result, status, err := seedanceCompatCall(c, DeleteSeedanceAssetGroup)
	if seedanceCompatResultError(c, result, status, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
}

func ListSeedanceCompatAssets(c *gin.Context) {
	page, size := seedanceCompatPage(c)
	groups := model.DB.Model(&model.SeedanceAssetGroup{}).Select("group_id").Where("user_id = ? AND status <> ?", c.GetInt("id"), "Deleting")
	assetTable := model.DB.NamingStrategy.TableName("SeedanceAsset")
	replicaTable := model.DB.NamingStrategy.TableName("SeedanceAssetReplica")
	query := model.DB.WithContext(c.Request.Context()).Table(assetTable+" AS a").
		Joins("LEFT JOIN "+replicaTable+" AS r ON r.id = a.compat_optimize_replica_id AND r.user_id = a.user_id AND r.local_asset_id = a.id").
		Where("a.user_id = ? AND a.status <> ? AND a.group_id IN (?)", c.GetInt("id"), "Deleting", groups)
	if id := strings.TrimSpace(c.Query("group_id")); id != "" {
		group, err := seedanceCompatGroup(c, id)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				c.JSON(http.StatusOK, seedanceCompatPageResponse{Data: []seedanceCompatAssetData{}, Total: 0, Page: page, PageSize: size})
				return
			}
			seedanceCompatLookupError(c, err)
			return
		}
		query = query.Where("a.group_id = ?", group.GroupID)
	}
	if kind := strings.TrimSpace(c.Query("asset_type")); kind != "" {
		query = query.Where("a.asset_type = ?", kind)
	}
	statusExpr, statusArgs, err := seedanceAssetStatusExpression(c, "a", true)
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	if err := scheduleMissingSeedanceAssetTargets(c); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	if status := strings.ToLower(strings.TrimSpace(c.Query("status"))); status != "" {
		args := append([]any{}, statusArgs...)
		switch status {
		case "active":
			query = query.Where("("+statusExpr+") IN ?", append(args, []string{"active", "success", "succeeded"})...)
		case "processing":
			query = query.Where("("+statusExpr+") IN ?", append(args, []string{"processing", "pending"})...)
		default:
			query = query.Where("("+statusExpr+") = ?", append(args, status)...)
		}
	}
	if name := strings.TrimSpace(c.Query("name")); name != "" {
		keyword := "%" + strings.NewReplacer("#", "##", "%", "#%", "_", "#_").Replace(strings.ToLower(name)) + "%"
		query = query.Where("LOWER(a.name) LIKE ? ESCAPE '#'", keyword)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	assets := make([]model.SeedanceAsset, 0)
	if err := query.Select("a.*").Order("a.id desc").Offset((page - 1) * size).Limit(size).Find(&assets).Error; err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	if err := projectSeedanceAssetStatuses(c, assets, true); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	views := make([]seedanceCompatAssetData, 0, len(assets))
	if _, err := seedanceCompatReplicas(c, assets); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	for _, asset := range assets {
		views = append(views, seedanceCompatAssetView(asset))
	}
	c.JSON(http.StatusOK, seedanceCompatPageResponse{Data: views, Total: total, Page: page, PageSize: size})
}

func GetSeedanceCompatAsset(c *gin.Context) {
	asset, err := seedanceCompatAsset(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	assets := []model.SeedanceAsset{*asset}
	if err := projectSeedanceAssetStatuses(c, assets, true); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	asset = &assets[0]
	if _, err := seedanceCompatReplicas(c, []model.SeedanceAsset{*asset}); err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	c.JSON(http.StatusOK, seedanceCompatDataResponse{Data: seedanceCompatAssetView(*asset)})
}

func CreateSeedanceCompatAsset(c *gin.Context) {
	var request struct {
		GroupID   string `json:"group_id"`
		URL       string `json:"url"`
		Name      string `json:"name"`
		AssetType string `json:"asset_type"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.GroupID) == "" || strings.TrimSpace(request.URL) == "" {
		seedanceCompatError(c, http.StatusBadRequest, "group_id and url are required")
		return
	}
	group, err := seedanceCompatGroup(c, request.GroupID)
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	if request.AssetType == "" {
		request.AssetType = "Image"
	}
	if request.Name == "" {
		if parsed, err := url.Parse(request.URL); err == nil {
			request.Name = path.Base(parsed.Path)
		}
	}
	if err := seedanceCompatSetBody(c, gin.H{"group_id": group.GroupID, "source_url": request.URL, "name": request.Name, "asset_type": request.AssetType}); err != nil {
		seedanceCompatError(c, http.StatusInternalServerError, "asset request failed")
		return
	}
	result, status, err := seedanceCompatCall(c, CreateSeedanceAsset)
	if seedanceCompatResultError(c, result, status, err) {
		return
	}
	var asset model.SeedanceAsset
	if err := common.Unmarshal(result.Data, &asset); err != nil {
		seedanceCompatError(c, http.StatusInternalServerError, "asset response is invalid")
		return
	}
	c.JSON(http.StatusOK, seedanceCompatDataResponse{Data: seedanceCompatAssetGroupView(asset)})
}

func DeleteSeedanceCompatAsset(c *gin.Context) {
	asset, err := seedanceCompatAsset(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(asset.ID), 10)}}
	result, status, err := seedanceCompatCall(c, DeleteSeedanceAsset)
	if seedanceCompatResultError(c, result, status, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
}

func UpdateSeedanceCompatAsset(c *gin.Context) {
	asset, err := seedanceCompatAsset(c, c.Param("id"))
	if err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	var request struct {
		Name    *string `json:"name"`
		GroupID *string `json:"group_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || request.Name == nil && request.GroupID == nil {
		seedanceCompatError(c, http.StatusBadRequest, "name or group_id is required")
		return
	}
	if request.Name != nil && (strings.TrimSpace(*request.Name) == "" || len([]rune(*request.Name)) > 64) {
		seedanceCompatError(c, http.StatusBadRequest, "invalid asset name")
		return
	}
	if request.GroupID != nil {
		groupID := strings.TrimSpace(*request.GroupID)
		if groupID == "" {
			seedanceCompatError(c, http.StatusBadRequest, "group_id cannot be empty")
			return
		}
		if groupID != asset.GroupID {
			group, err := seedanceCompatGroup(c, groupID)
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				seedanceCompatLookupError(c, err)
				return
			}
			if group == nil || group.GroupID != asset.GroupID {
				seedanceCompatError(c, http.StatusBadRequest, "changing asset group is not supported")
				return
			}
		}
	}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		groups := model.DB.Model(&model.SeedanceAssetGroup{}).Select("group_id").
			Where("user_id = ? AND status <> ?", c.GetInt("id"), "Deleting")
		result := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).
			Where("id = ? AND user_id = ? AND group_id = ? AND status <> ?", asset.ID, c.GetInt("id"), asset.GroupID, "Deleting").
			Where("group_id IN (?)", groups).Update("name", name)
		if result.Error != nil {
			seedanceCompatLookupError(c, result.Error)
			return
		}
		if result.RowsAffected == 0 {
			current, err := seedanceCompatAsset(c, asset.AssetID)
			if err != nil {
				seedanceCompatLookupError(c, err)
				return
			}
			if current.GroupID != asset.GroupID || current.Name != name {
				seedanceCompatError(c, http.StatusConflict, "asset changed while updating")
				return
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "ok"})
}
