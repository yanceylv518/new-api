package controller

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// seedanceLibraryQuery 仅查询本地授权映射；参数不透传上游，也不能改变用户归属。
type seedanceLibraryQuery struct {
	groups, ids, assetIDs, statuses, assetTypes []string
	search, order                               string
	after, before                               *time.Time
	page                                        *common.PageInfo
	paginated                                   bool
}

// parseSeedanceLibraryQuery 先完成全部校验再访问数据库。批量参数支持逗号或重复键，
// 单复数互斥，限制原始元素数以防去重前的无界输入；排序只能来自固定白名单。
func parseSeedanceLibraryQuery(c *gin.Context, groups bool) (*seedanceLibraryQuery, error) {
	q := &seedanceLibraryQuery{page: common.GetPageQuery(c)}
	// URL.Query 会静默丢弃格式错误的参数，必须显式拒绝，避免意外退回全量查询。
	values, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid query string")
	}
	for _, key := range []string{"search", "sort_by", "sort_order", "created_after", "created_before", "p", "page_size", "ps", "size"} {
		if len(values[key]) > 1 {
			return nil, fmt.Errorf("%s must appear once", key)
		}
	}
	q.paginated = !groups || values.Has("p") || values.Has("page_size") || values.Has("ps") || values.Has("size")
	for _, key := range []string{"p", "page_size", "ps", "size"} {
		if values.Has(key) {
			if n, err := strconv.ParseInt(values.Get(key), 10, 32); err != nil || n < 1 {
				return nil, fmt.Errorf("%s must be a positive integer", key)
			}
		}
	}
	q.page.PageSize = max(1, min(100, q.page.PageSize))
	for _, field := range []struct {
		single, multiple string
		target           *[]string
	}{
		{"group_id", "group_ids", &q.groups}, {"", "ids", &q.ids},
		{"asset_id", "asset_ids", &q.assetIDs}, {"status", "statuses", &q.statuses},
		{"asset_type", "asset_types", &q.assetTypes},
	} {
		if groups && (field.multiple == "asset_ids" || field.multiple == "asset_types") && (values.Has(field.single) || values.Has(field.multiple)) {
			return nil, fmt.Errorf("%s is not supported for groups", field.multiple)
		}
		if field.single != "" && values.Has(field.single) && values.Has(field.multiple) {
			return nil, fmt.Errorf("use either %s or %s", field.single, field.multiple)
		}
		key := field.multiple
		if field.single != "" && values.Has(field.single) {
			key = field.single
		}
		if !values.Has(key) {
			continue
		}
		items := values[key]
		if key == field.single && len(items) != 1 {
			return nil, fmt.Errorf("%s must appear once", key)
		}
		seen := make(map[string]bool)
		count := 0
		for _, item := range items {
			parts := []string{item}
			if key == field.multiple {
				parts = strings.Split(item, ",")
			}
			for _, part := range parts {
				count++
				part = strings.TrimSpace(part)
				// 旧单值筛选的空字符串继续表示不筛选；批量空元素视为错误。
				if part == "" && key == field.single {
					continue
				}
				if count > 100 || part == "" || utf8.RuneCountInString(part) > 128 {
					return nil, fmt.Errorf("%s requires 1 to 100 non-empty values of at most 128 characters", key)
				}
				if key == "ids" {
					n, err := strconv.ParseInt(part, 10, 64)
					if err != nil || n <= 0 {
						return nil, fmt.Errorf("ids must contain positive integers")
					}
					part = strconv.FormatInt(n, 10)
				}
				if field.multiple == "statuses" || field.multiple == "asset_types" {
					part = strings.ToLower(part)
				}
				if !seen[part] {
					*field.target = append(*field.target, part)
					seen[part] = true
				}
			}
		}
	}
	// 将上游历史状态别名统一为用户可见分类，保持原有单状态筛选语义。
	var statuses []string
	for _, status := range q.statuses {
		switch status {
		case "all":
			if len(q.statuses) != 1 {
				return nil, fmt.Errorf("all cannot be combined with other statuses")
			}
		case "active":
			statuses = append(statuses, "active")
			if !groups {
				statuses = append(statuses, "success", "succeeded")
			}
		case "deleting":
			statuses = append(statuses, "deleting")
		case "processing", "failed":
			if groups {
				return nil, fmt.Errorf("unsupported group status")
			}
			statuses = append(statuses, status)
			if status == "processing" {
				statuses = append(statuses, "pending")
			}
		default:
			return nil, fmt.Errorf("unsupported status")
		}
	}
	q.statuses = statuses
	for _, kind := range q.assetTypes {
		if kind == "all" && len(q.assetTypes) == 1 {
			q.assetTypes = nil
			break
		}
		if kind != "image" && kind != "video" && kind != "audio" {
			return nil, fmt.Errorf("unsupported asset type")
		}
	}
	q.search = strings.TrimSpace(values.Get("search"))
	if utf8.RuneCountInString(q.search) > 128 {
		return nil, fmt.Errorf("search exceeds 128 characters")
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{{"created_after", &q.after}, {"created_before", &q.before}} {
		if values.Has(field.name) {
			parsed, err := time.Parse(time.RFC3339Nano, values.Get(field.name))
			if err != nil || parsed.Year() < 1000 || parsed.Year() > 9999 {
				return nil, fmt.Errorf("%s must be an RFC3339 timestamp", field.name)
			}
			utc := parsed.UTC()
			*field.target = &utc
		}
	}
	if q.after != nil && q.before != nil && !q.after.Before(*q.before) {
		return nil, fmt.Errorf("created_after must precede created_before")
	}
	column := values.Get("sort_by")
	switch column {
	case "":
		column = "id"
	case "id", "created_at", "updated_at", "name":
	default:
		return nil, fmt.Errorf("unsupported sort_by")
	}
	direction := strings.ToLower(values.Get("sort_order"))
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		return nil, fmt.Errorf("sort_order must be asc or desc")
	}
	q.order = column + " " + direction
	if column != "id" {
		q.order += ", id " + direction
	}
	return q, nil
}

// scope 保留用户级和组级边界；has_pending 只使用这层，不受终态或名称筛选影响。
func (q *seedanceLibraryQuery) scope(db *gorm.DB) *gorm.DB {
	if len(q.groups) > 0 {
		db = db.Where("group_id IN ?", q.groups)
	}
	return db
}

// filter 所有值均参数绑定，SQL 字段来自内部固定集合，同一字段多值为 OR、不同字段为 AND。
func (q *seedanceLibraryQuery) filter(db *gorm.DB, groups bool) *gorm.DB {
	if len(q.ids) > 0 {
		db = db.Where("id IN ?", q.ids)
	}
	if len(q.assetIDs) > 0 {
		db = db.Where("asset_id IN ?", q.assetIDs)
	}
	if len(q.statuses) > 0 {
		db = db.Where("LOWER(status) IN ?", q.statuses)
	}
	if len(q.assetTypes) > 0 {
		db = db.Where("LOWER(asset_type) IN ?", q.assetTypes)
	}
	if q.after != nil {
		db = db.Where("created_at >= ?", *q.after)
	}
	if q.before != nil {
		db = db.Where("created_at < ?", *q.before)
	}
	if q.search != "" {
		keyword := "%" + strings.NewReplacer("#", "##", "%", "#%", "_", "#_").Replace(strings.ToLower(q.search)) + "%"
		if groups {
			db = db.Where("(LOWER(name) LIKE ? ESCAPE '#' OR LOWER(group_id) LIKE ? ESCAPE '#')", keyword, keyword)
		} else {
			db = db.Where("(LOWER(name) LIKE ? ESCAPE '#' OR LOWER(asset_id) LIKE ? ESCAPE '#')", keyword, keyword)
		}
	}
	return db
}

// GetSeedanceAssetGroup 只读当前用户的本地组，不枚举上游账号中其他用户的数据。
func GetSeedanceAssetGroup(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid id"})
		return
	}
	var group model.SeedanceAssetGroup
	err = model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND id = ?", c.GetInt("id"), id).First(&group).Error
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "resource not found"})
		return
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, group)
}

// GetSeedanceAsset 返回同步到本地的状态及临时预览，不隐式刷新或调用上游。
func GetSeedanceAsset(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid id"})
		return
	}
	var asset model.SeedanceAsset
	err = model.DB.WithContext(c.Request.Context()).Where("user_id = ? AND id = ?", c.GetInt("id"), id).First(&asset).Error
	if err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "resource not found"})
		return
	}
	if err != nil {
		common.ApiError(c, err)
		return
	}
	asset.PreviewURL, err = seedanceAssetPreviewURL(c.Request.Context(), &asset)
	if err != nil {
		asset.PreviewURL = ""
		common.SysError(fmt.Sprintf("failed to sign Seedance asset preview; asset_id=%d: %v", asset.ID, err))
	}
	common.ApiSuccess(c, asset)
}
