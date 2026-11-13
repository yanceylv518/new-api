package controller

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const seedanceAssetActionContextKey = "seedance_asset_library_action"

type seedanceCompatOrderedJSON struct {
	fields gin.H
	order  []string
}

func (object seedanceCompatOrderedJSON) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(object.fields))
	for _, key := range object.order {
		if _, ok := object.fields[key]; ok {
			keys = append(keys, key)
		}
	}
	var extra []string
	for key := range object.fields {
		if !slices.Contains(object.order, key) {
			extra = append(extra, key)
		}
	}
	slices.Sort(extra)
	keys = append(keys, extra...)
	var output bytes.Buffer
	output.WriteByte('{')
	for index, key := range keys {
		name, err := common.Marshal(key)
		if err != nil {
			return nil, err
		}
		value, err := common.Marshal(object.fields[key])
		if err != nil {
			return nil, err
		}
		if index != 0 {
			output.WriteByte(',')
		}
		output.Write(name)
		output.WriteByte(':')
		output.Write(value)
	}
	output.WriteByte('}')
	return output.Bytes(), nil
}

func writeSeedanceAssetOfficialResponse(c *gin.Context, status int, response gin.H) {
	if !c.GetBool("seedance_compat_ark_action") {
		c.JSON(status, response)
		return
	}
	metadata, _ := response["ResponseMetadata"].(gin.H)
	action, _ := metadata["Action"].(string)
	fields := gin.H{"ResponseMetadata": seedanceCompatOrderedJSON{fields: metadata,
		order: []string{"RequestId", "Action", "Version", "Service", "Region", "Error"}}}
	if result, ok := response["Result"].(gin.H); ok {
		groupOrder := []string{"Id", "Name", "Description", "GroupType", "ProjectName", "CreateTime", "UpdateTime", "Status"}
		assetOrder := []string{"Id", "GroupId", "URL", "AssetType", "Name", "Status", "LastInferenceTime", "ProjectName", "CreateTime", "UpdateTime"}
		order := []string{"Id", "GroupId", "BytedToken", "H5Link", "CallbackURL"}
		switch action {
		case "GetAssetGroup":
			order = groupOrder
		case "GetAsset":
			order = assetOrder
		case "ListAssetGroups", "ListAssets":
			itemOrder := groupOrder
			if action == "ListAssets" {
				itemOrder = assetOrder
			}
			if items, ok := result["Items"].([]gin.H); ok {
				ordered := make([]seedanceCompatOrderedJSON, 0, len(items))
				for _, item := range items {
					ordered = append(ordered, seedanceCompatOrderedJSON{fields: item, order: itemOrder})
				}
				result["Items"] = ordered
			}
			order = []string{"Items", "NextToken", "PageNumber", "PageSize", "TotalCount"}
		}
		fields["Result"] = seedanceCompatOrderedJSON{fields: result, order: order}
	}
	c.JSON(status, seedanceCompatOrderedJSON{fields: fields, order: []string{"ResponseMetadata", "Result"}})
}

type seedanceAssetActionRequest struct {
	ID          string  `json:"Id"`
	Name        *string `json:"Name"`
	Description *string `json:"Description"`
	GroupType   string  `json:"GroupType"`
	GroupID     string  `json:"GroupId"`
	URL         string  `json:"URL"`
	AssetType   string  `json:"AssetType"`
	Model       string  `json:"model"`
	ProjectName string  `json:"ProjectName"`
	PageNumber  *int    `json:"PageNumber"`
	PageSize    *int    `json:"PageSize"`
	MaxResults  *int    `json:"MaxResults"`
	NextToken   *string `json:"NextToken"`
	SortBy      string  `json:"SortBy"`
	SortOrder   string  `json:"SortOrder"`
	Filter      *struct {
		GroupID   string   `json:"GroupId"`
		GroupIDs  []string `json:"GroupIds"`
		GroupType string   `json:"GroupType"`
		Name      string   `json:"Name"`
		AssetType string   `json:"AssetType"`
		Statuses  []string `json:"Statuses"`
	} `json:"Filter"`
}

// handleSeedanceAssetLibraryAction 只转换协议边界，创建、查询和删除复用网页的执行路径。
func handleSeedanceAssetLibraryAction(c *gin.Context, action string) {
	allowed := map[string]string{
		"CreateAssetGroup": "Name Description GroupType model",
		"ListAssetGroups":  "Filter PageNumber PageSize SortBy SortOrder",
		"GetAssetGroup":    "Id",
		"UpdateAssetGroup": "Id Name Description",
		"DeleteAssetGroup": "Id",
		"CreateAsset":      "GroupId URL AssetType Name",
		"ListAssets":       "Filter PageNumber PageSize SortBy SortOrder",
		"GetAsset":         "Id",
		"UpdateAsset":      "Id Name",
		"DeleteAsset":      "Id",
	}
	fields, supported := allowed[action]
	if !supported {
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, action, "unsupported asset Action")
		return
	}
	compat := c.GetBool("seedance_compat_ark_action")
	if compat {
		fields += " ProjectName"
		if action == "ListAssetGroups" || action == "ListAssets" {
			fields += " MaxResults NextToken"
		}
	}
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query["Action"]) != 1 || len(query["Version"]) != 1 {
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, action, "Action and Version must appear exactly once")
		return
	}
	c.Set(seedanceAssetActionContextKey, action)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	defer c.Request.Body.Close()
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "invalid or oversized JSON request")
		return
	}
	var object map[string]any
	var request seedanceAssetActionRequest
	if err := common.Unmarshal(raw, &object); err != nil || object == nil {
		seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "request must be a JSON object")
		return
	}
	for field, value := range object {
		if value == nil || !strings.Contains(" "+fields+" ", " "+field+" ") {
			seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "unsupported or null field: "+field)
			return
		}
	}
	if err := common.Unmarshal(raw, &request); err != nil {
		seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "invalid field type")
		return
	}
	if compat && request.ProjectName != "" && request.ProjectName != "default" {
		seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "only the default asset project is supported")
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	request.GroupID = strings.TrimSpace(request.GroupID)
	c.Set("seedance_asset_action_id", request.ID)
	maxDescription := 256
	if compat {
		maxDescription = 300
	}
	if utf8.RuneCountInString(request.ID) > 128 || utf8.RuneCountInString(request.GroupID) > 128 ||
		utf8.RuneCountInString(request.Model) > 191 || len(request.URL) > 2048 ||
		request.Name != nil && (strings.TrimSpace(*request.Name) == "" || utf8.RuneCountInString(*request.Name) > 64) ||
		request.Description != nil && utf8.RuneCountInString(*request.Description) > maxDescription {
		seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "invalid Id, GroupId, model, Name or Description")
		return
	}

	var handler gin.HandlerFunc
	var payload any
	switch action {
	case "CreateAssetGroup":
		if request.Name == nil || request.GroupType != "" && request.GroupType != "AIGC" {
			seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "Name is required; GroupType must be AIGC; use the validation session flow for LivenessFace")
			return
		}
		payload = gin.H{"name": *request.Name, "model": request.Model, "description": request.Description}
		handler = CreateSeedanceAssetGroup
	case "CreateAsset":
		if compat && request.AssetType == "" && request.GroupID != "" && request.URL != "" {
			var group model.SeedanceAssetGroup
			if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND group_id = ? AND status <> ?", c.GetInt("id"), request.GroupID, "Deleting").First(&group).Error; err != nil {
				seedanceAssetAPIError(c, err)
				return
			}
			sourceURL := strings.TrimSpace(request.URL)
			if err := service.ValidateSeedanceAssetSourceURL(sourceURL); err != nil {
				seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "invalid or disallowed asset source URL")
				return
			}
			assetType, err := service.DetectSeedanceAssetURLType(c.Request.Context(), sourceURL)
			if err != nil {
				seedanceAssetActionError(c, http.StatusBadGateway, "OperationFailed", "asset media type could not be determined")
				return
			}
			request.AssetType = assetType
		}
		payload = gin.H{"group_id": request.GroupID, "source_url": request.URL, "asset_type": request.AssetType, "name": request.Name}
		handler = CreateSeedanceAsset
	case "ListAssetGroups", "ListAssets":
		if err := prepareSeedanceAssetActionList(c, request, object["Filter"], action == "ListAssetGroups"); err != nil {
			seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", err.Error())
			return
		}
		if action == "ListAssetGroups" {
			handler = ListSeedanceAssetGroups
		} else {
			handler = ListSeedanceAssets
		}
	default:
		if request.ID == "" {
			seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "Id is required")
			return
		}
		if strings.HasSuffix(action, "AssetGroup") {
			var group model.SeedanceAssetGroup
			query := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND group_id = ?", c.GetInt("id"), request.ID)
			if compat {
				query = query.Where("status <> ?", "Deleting")
			}
			if err := query.First(&group).Error; err != nil {
				seedanceAssetAPIError(c, err)
				return
			}
			c.Params = append(c.Params, gin.Param{Key: "id", Value: strconv.FormatUint(uint64(group.ID), 10)})
			switch action {
			case "GetAssetGroup":
				handler = GetSeedanceAssetGroup
			case "DeleteAssetGroup":
				handler = DeleteSeedanceAssetGroup
			case "UpdateAssetGroup":
				if request.Name == nil && request.Description == nil {
					seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "Name or Description is required")
					return
				}
				name := group.Name
				if request.Name != nil {
					name = *request.Name
				}
				payload = gin.H{"name": name, "description": request.Description}
				handler = UpdateSeedanceAssetGroup
			}
		} else {
			var asset model.SeedanceAsset
			query := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND asset_id = ?", c.GetInt("id"), request.ID)
			if compat {
				groups := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetGroup{}).
					Select("group_id").Where("user_id = ? AND status <> ?", c.GetInt("id"), "Deleting")
				query = query.Where("status <> ? AND group_id IN (?)", "Deleting", groups)
			}
			if err := query.First(&asset).Error; err != nil {
				seedanceAssetAPIError(c, err)
				return
			}
			c.Params = append(c.Params, gin.Param{Key: "id", Value: strconv.FormatUint(uint64(asset.ID), 10)})
			switch action {
			case "GetAsset":
				handler = GetSeedanceAsset
			case "DeleteAsset":
				handler = DeleteSeedanceAsset
			case "UpdateAsset":
				updateSeedanceAssetAction(c, &asset, request.Name)
				return
			}
		}
	}
	if payload != nil {
		body, err := common.Marshal(payload)
		if err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Request.Header.Set("Content-Type", "application/json")
	}
	handler(c)
}

func prepareSeedanceAssetActionList(c *gin.Context, request seedanceAssetActionRequest, rawFilter any, groups bool) error {
	page, size := 1, 10
	compat := c.GetBool("seedance_compat_ark_action")
	if compat {
		pageMode := request.PageNumber != nil || request.PageSize != nil
		if pageMode && (request.PageNumber == nil || request.PageSize == nil || request.MaxResults != nil || request.NextToken != nil) {
			return errors.New("PageNumber and PageSize must be supplied together and cannot be combined with NextToken or MaxResults")
		}
		if !pageMode {
			if request.MaxResults != nil && *request.MaxResults > 0 {
				size = min(*request.MaxResults, 100)
			}
			offset := 0
			if request.NextToken != nil && *request.NextToken != "" {
				if len(*request.NextToken) > 64 {
					return errors.New("invalid NextToken")
				}
				decoded, err := base64.StdEncoding.Strict().DecodeString(*request.NextToken)
				if err != nil {
					return errors.New("invalid NextToken")
				}
				parsed, err := strconv.ParseInt(string(decoded), 10, 32)
				if err != nil || parsed < 0 || parsed > 2147483547 {
					return errors.New("invalid NextToken")
				}
				offset = int(parsed)
			}
			c.Set("seedance_compat_ark_token_page", true)
			c.Set("seedance_compat_ark_offset", offset)
		}
	}
	if request.PageNumber != nil {
		page = *request.PageNumber
	}
	if request.PageSize != nil {
		size = *request.PageSize
	}
	if compat {
		page = max(1, page)
		if size <= 0 {
			size = 10
		}
		size = min(size, 100)
	}
	if page < 1 || page > 2147483647/100 || size < 1 || size > 100 {
		return errors.New("PageNumber must be positive and at most 21474836; PageSize must be between 1 and 100")
	}
	values := url.Values{"p": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(size)}, "sort_by": {"created_at"}}
	if request.SortBy != "" && request.SortBy != "CreateTime" && request.SortBy != "UpdateTime" && request.SortBy != "Name" && !(compat && !groups && request.SortBy == "GroupId") {
		return errors.New("SortBy must be CreateTime, UpdateTime or Name")
	}
	switch request.SortBy {
	case "UpdateTime":
		values.Set("sort_by", "updated_at")
	case "Name":
		values.Set("sort_by", "name")
	case "GroupId":
		values.Set("sort_by", "group_id")
	}
	if request.SortOrder != "" && request.SortOrder != "Asc" && request.SortOrder != "Desc" {
		return errors.New("SortOrder must be Asc or Desc")
	}
	values.Set("sort_order", strings.ToLower(request.SortOrder))
	if request.Filter != nil {
		filter := request.Filter
		allowed := " Name GroupId AssetType Statuses "
		if groups {
			allowed = " Name GroupType "
		}
		if compat {
			allowed += "GroupIds "
			if !groups {
				allowed += "GroupType "
			}
		}
		for field, value := range rawFilter.(map[string]any) {
			if value == nil || !strings.Contains(allowed, " "+field+" ") {
				return fmt.Errorf("unsupported or null Filter field: %s", field)
			}
		}
		if utf8.RuneCountInString(filter.Name) > 128 || utf8.RuneCountInString(filter.GroupID) > 128 {
			return errors.New("Filter.Name and Filter.GroupId must be at most 128 characters")
		}
		values.Set("search", filter.Name)
		if compat {
			if len(filter.GroupIDs) > 100 || filter.GroupID != "" && len(filter.GroupIDs) > 0 {
				return errors.New("Filter.GroupIds must contain at most 100 IDs and cannot be combined with GroupId")
			}
			for _, id := range filter.GroupIDs {
				values.Add("group_ids", id)
			}
		}
		if !groups {
			if len(filter.GroupIDs) == 0 {
				values.Set("group_id", filter.GroupID)
			}
			values.Set("asset_type", filter.AssetType)
		}
		if len(filter.Statuses) > 100 {
			return errors.New("Filter.Statuses must have at most 100 items")
		}
		for _, status := range filter.Statuses {
			if status != "Active" && status != "Processing" && status != "Failed" && status != "Deleting" {
				return errors.New("unsupported Filter.Statuses value")
			}
			values.Add("statuses", status)
		}
		if filter.AssetType != "" && filter.AssetType != "Image" && filter.AssetType != "Video" && filter.AssetType != "Audio" {
			return errors.New("Filter.AssetType must be Image, Video or Audio")
		}
	}
	if groups || compat {
		groupType := ""
		if groups && !compat {
			groupType = "AIGC"
		}
		if request.Filter != nil && request.Filter.GroupType != "" {
			groupType = request.Filter.GroupType
		}
		if groupType != "" && groupType != "AIGC" && groupType != "LivenessFace" {
			return errors.New("Filter.GroupType must be AIGC or LivenessFace")
		}
		c.Set("seedance_asset_action_group_type", groupType)
	}
	// Gin 可能已缓存原始查询串；通过显式参数传递，避免分页回退为默认值。
	c.Set("seedance_asset_action_query", values)
	return nil
}

func updateSeedanceAssetAction(c *gin.Context, asset *model.SeedanceAsset, name *string) {
	if name == nil {
		seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", "Name is required")
		return
	}
	if asset.Status == "Deleting" {
		seedanceAssetActionError(c, http.StatusConflict, "ResourceDeleting", "asset is being deleted")
		return
	}
	newName := strings.TrimSpace(*name)
	// 名称是平台素材的展示信息；不改变 URL、类型、组或已有跨账号副本。
	result := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAsset{}).
		Where("user_id = ? AND id = ? AND status <> ?", asset.UserID, asset.ID, "Deleting").Update("name", newName)
	if result.Error != nil {
		seedanceAssetAPIError(c, result.Error)
		return
	}
	if result.RowsAffected == 0 {
		var current model.SeedanceAsset
		if err := model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND id = ? AND status <> ?", asset.UserID, asset.ID, "Deleting").First(&current).Error; err != nil {
			seedanceAssetAPIError(c, err)
			return
		}
	}
	writeSeedanceAssetOfficialResponse(c, http.StatusOK, seedanceAssetOfficialResponse("UpdateAsset", gin.H{"Id": asset.AssetID}))
}

func seedanceAssetActionError(c *gin.Context, status int, code, message string) {
	writeSeedanceAssetOfficialErrorWithCode(c, status, c.GetString(seedanceAssetActionContextKey), code, message)
}

// seedanceAssetAPIError 保留原 REST 契约，仅在 Action 入口转换错误外形。
func seedanceAssetAPIError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrSeedanceAssetServiceTemporary) {
		c.Header("Retry-After", "5")
		if c.GetString(seedanceAssetActionContextKey) != "" {
			seedanceAssetActionError(c, http.StatusServiceUnavailable, "ServiceUnavailable", service.ErrSeedanceAssetServiceTemporary.Error())
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": service.ErrSeedanceAssetServiceTemporary.Error(), "code": "asset_service_unavailable"})
		}
		return
	}
	if c.GetString(seedanceAssetActionContextKey) == "" {
		common.ApiError(c, err)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		seedanceAssetActionError(c, http.StatusNotFound, "ResourceNotFound", "resource not found")
		return
	}
	common.SysError("Seedance asset Action failed: " + err.Error())
	seedanceAssetActionError(c, http.StatusBadGateway, "OperationFailed", err.Error())
}

func seedanceAssetAPIErrorMsg(c *gin.Context, message string) {
	if c.GetString(seedanceAssetActionContextKey) == "" {
		common.ApiErrorMsg(c, message)
		return
	}
	seedanceAssetActionError(c, http.StatusBadRequest, "ValidationError", message)
}

func seedanceAssetAPIErrorI18n(c *gin.Context, key string) {
	seedanceAssetAPIErrorMsg(c, common.TranslateMessage(c, key))
}

func seedanceAssetAPISuccess(c *gin.Context, data any) {
	seedanceAssetAPIResponse(c, http.StatusOK, gin.H{"success": true, "message": "", "data": data})
}

// seedanceAssetAPIResponse 在业务执行完成后呈现协议，不暴露本地用户、渠道及副本信息。
func seedanceAssetAPIResponse(c *gin.Context, status int, response gin.H) {
	action := c.GetString(seedanceAssetActionContextKey)
	if action == "" {
		c.JSON(status, response)
		return
	}
	if success, _ := response["success"].(bool); !success {
		message, _ := response["message"].(string)
		seedanceAssetActionError(c, status, "ValidationError", message)
		return
	}
	result := gin.H{}
	compat := c.GetBool("seedance_compat_ark_action")
	switch data := response["data"].(type) {
	case model.SeedanceAssetGroup:
		result = seedanceAssetActionGroup(data, compat)
	case *model.SeedanceAssetGroup:
		result = seedanceAssetActionGroup(*data, compat)
	case model.SeedanceAsset:
		result = seedanceAssetActionAsset(data, compat)
	case *model.SeedanceAsset:
		result = seedanceAssetActionAsset(*data, compat)
	case []model.SeedanceAssetGroup:
		items := make([]gin.H, 0, len(data))
		for _, group := range data {
			items = append(items, seedanceAssetActionGroup(group, compat))
		}
		result = gin.H{"Items": items}
	case []model.SeedanceAsset:
		items := make([]gin.H, 0, len(data))
		for _, asset := range data {
			items = append(items, seedanceAssetActionAsset(asset, compat))
		}
		result = gin.H{"Items": items}
	}
	if action == "CreateAssetGroup" || action == "UpdateAssetGroup" || action == "CreateAsset" {
		result = gin.H{"Id": result["Id"]}
	}
	if action == "ListAssetGroups" || action == "ListAssets" {
		if c.GetBool("seedance_compat_ark_token_page") {
			offset := c.GetInt("seedance_compat_ark_offset")
			items, _ := result["Items"].([]gin.H)
			next := offset + len(items)
			if total, ok := response["total"].(int64); ok && int64(next) < total {
				result["NextToken"] = base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(next)))
			}
		} else {
			result["TotalCount"], result["PageNumber"], result["PageSize"] = response["total"], response["page"], response["page_size"]
		}
	}
	if action == "DeleteAssetGroup" || action == "DeleteAsset" {
		if c.GetBool("seedance_compat_ark_action") {
			status = http.StatusOK
			result = gin.H{}
		} else {
			result = gin.H{"Status": "Deleting"}
			if action == "DeleteAssetGroup" {
				result["Id"] = c.GetString("seedance_asset_action_id")
			}
		}
	}
	if status == http.StatusAccepted && action == "CreateAsset" {
		status = http.StatusOK
	}
	writeSeedanceAssetOfficialResponse(c, status, seedanceAssetOfficialResponse(action, result))
}

func seedanceAssetActionGroup(group model.SeedanceAssetGroup, compat bool) gin.H {
	result := gin.H{
		"Id": group.GroupID, "Name": group.Name, "Description": group.Description,
		"GroupType": group.GroupType, "Status": group.Status,
		"CreateTime": group.CreatedAt.UTC().Format(time.RFC3339), "UpdateTime": group.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if compat {
		result["ProjectName"] = "default"
		delete(result, "Status")
	}
	return result
}

func seedanceAssetActionAsset(asset model.SeedanceAsset, compat bool) gin.H {
	status := asset.Status
	switch strings.ToLower(status) {
	case "success", "succeeded", "active":
		status = "Active"
	case "pending", "processing":
		status = "Processing"
	case "failed":
		status = "Failed"
	}
	previewURL := ""
	if status == "Active" {
		previewURL = asset.PreviewURL
	}
	result := gin.H{
		"Id": asset.AssetID, "Name": asset.Name, "GroupId": asset.GroupID, "AssetType": asset.AssetType,
		"Status": status, "URL": previewURL,
		"CreateTime": asset.CreatedAt.UTC().Format(time.RFC3339), "UpdateTime": asset.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if compat {
		result["ProjectName"] = "default"
	}
	return result
}
