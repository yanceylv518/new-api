/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
package model

import (
	"context"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

const userModelPricingOverviewPreviewRuleLimit = 3

// UserModelPricingOverviewUser 是折扣总览只读用户信息，避免把账号敏感字段带到管理页面。
type UserModelPricingOverviewUser struct {
	Id          int    `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Group       string `json:"group"`
	Role        int    `json:"role"`
	Status      int    `json:"status"`
}

// UserModelPricingOverviewRule 是总览页面展示的单条用户模型折扣。
type UserModelPricingOverviewRule struct {
	ModelName   string `json:"model_name"`
	DiscountBPS int    `json:"discount_bps"`
}

// UserModelPricingOverviewItem 按用户聚合规则；摘要模式只带少量预览规则，完整规则仍由抽屉分页接口获取。
type UserModelPricingOverviewItem struct {
	User           UserModelPricingOverviewUser   `json:"user"`
	Rules          []UserModelPricingOverviewRule `json:"rules"`
	PreviewRules   []UserModelPricingOverviewRule `json:"preview_rules,omitempty"`
	RuleCount      int                            `json:"rule_count"`
	MinDiscountBPS int                            `json:"min_discount_bps"`
	MaxDiscountBPS int                            `json:"max_discount_bps"`
}

// UserModelPricingOverviewResult 包含分页数据及当前搜索范围内的统计值。
type UserModelPricingOverviewResult struct {
	Items       []UserModelPricingOverviewItem `json:"items"`
	TotalUsers  int64                          `json:"total_users"`
	TotalRules  int64                          `json:"total_rules"`
	TotalModels int64                          `json:"total_models"`
}

// UserModelPricingOverviewFilters 是总览页面的用户筛选条件，和关键字一起作用于统计、分页及规则列表。
type UserModelPricingOverviewFilters struct {
	Group string
	Role  *int
	// modelKeys 由服务端当前启用模型目录生成，避免历史规则污染总览。
	modelKeys []string
}

// getEnabledUserModelPricingModelKeys 将编辑目录转换成查询键，确保总览和编辑使用同一模型可见性边界。
func getEnabledUserModelPricingModelKeys(ctx context.Context) ([]string, error) {
	modelNames, err := GetUserModelPricingModelNames(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(modelNames))
	seen := make(map[string]struct{}, len(modelNames))
	for _, modelName := range modelNames {
		key := userModelPricingModelKey(modelName)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys, nil
}

// userModelPricingOverviewQuery 统一构造权限和搜索条件，保证统计、分页和明细使用同一数据范围。
func userModelPricingOverviewQuery(tx *gorm.DB, keyword string, requesterRole int, filters UserModelPricingOverviewFilters) *gorm.DB {
	query := tx.Model(&UserModelPricing{}).
		Joins("JOIN users ON users.id = user_model_pricings.user_id").
		Where("users.deleted_at IS NULL").
		Where("user_model_pricings.discount_bps > 0 AND user_model_pricings.discount_bps < ?", fullPriceDiscountBPS)
	if filters.modelKeys != nil {
		if len(filters.modelKeys) == 0 {
			return query.Where("1 = 0")
		}
		query = query.Where("user_model_pricings.model_key IN ?", filters.modelKeys)
	}
	if requesterRole != common.RoleRootUser {
		query = query.Where("users.role < ?", requesterRole)
	}
	if group := strings.TrimSpace(filters.Group); group != "" {
		query = query.Where("users."+commonGroupCol+" = ?", group)
	}
	if filters.Role != nil {
		query = query.Where("users.role = ?", *filters.Role)
	}

	keyword = strings.TrimSpace(keyword)
	if keyword == "" {
		return query
	}

	like := "%" + keyword + "%"
	return query.Where(
		"(users.username LIKE ? OR users.display_name LIKE ? OR users.email LIKE ? OR user_model_pricings.model_name LIKE ?)",
		like,
		like,
		like,
		like,
	)
}

// getUserModelPricingOverviewPreview 按模型排序取每个用户的前三条预览规则。
// 只读取当前页用户的已过滤规则，再在 Go 中截断；每用户规则上限为 1000，避免相关子查询重复扫描整张表。
func getUserModelPricingOverviewPreview(tx *gorm.DB, userIDs []int, keyword string, requesterRole int, filters UserModelPricingOverviewFilters) (map[int][]UserModelPricingOverviewRule, error) {
	previewRules := make(map[int][]UserModelPricingOverviewRule, len(userIDs))
	type previewRuleRow struct {
		UserID      int    `gorm:"column:user_id"`
		ModelName   string `gorm:"column:model_name"`
		DiscountBPS int    `gorm:"column:discount_bps"`
	}
	var rows []previewRuleRow
	query := userModelPricingOverviewQuery(tx, keyword, requesterRole, filters).
		Where("user_model_pricings.user_id IN ?", userIDs)
	if err := query.Select("user_model_pricings.user_id, user_model_pricings.model_name, user_model_pricings.discount_bps").
		Order("user_model_pricings.user_id ASC, user_model_pricings.model_name ASC, user_model_pricings.model_key ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if len(previewRules[row.UserID]) >= userModelPricingOverviewPreviewRuleLimit {
			continue
		}
		previewRules[row.UserID] = append(previewRules[row.UserID], UserModelPricingOverviewRule{
			ModelName:   row.ModelName,
			DiscountBPS: row.DiscountBPS,
		})
	}
	return previewRules, nil
}

// userModelPricingOverviewMatchedUserIds 固定搜索结果的用户集合，供统计和分页复用同一个权限边界。
func userModelPricingOverviewMatchedUserIds(tx *gorm.DB, keyword string, requesterRole int, filters UserModelPricingOverviewFilters) *gorm.DB {
	return userModelPricingOverviewQuery(tx, keyword, requesterRole, filters).
		Select("user_model_pricings.user_id").
		Distinct("user_model_pricings.user_id")
}

// GetUserModelPricingOverview 返回管理员可见的用户折扣汇总；分页按用户而不是规则行计算。
func GetUserModelPricingOverview(ctx context.Context, keyword string, requesterRole, startIdx, pageSize int, filters UserModelPricingOverviewFilters, summaryOnly ...bool) (UserModelPricingOverviewResult, error) {
	var result UserModelPricingOverviewResult
	if startIdx < 0 {
		startIdx = 0
	}
	if pageSize < 1 {
		pageSize = 1
	}
	if pageSize > 100 {
		pageSize = 100
	}

	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	activeModelKeys, err := getEnabledUserModelPricingModelKeys(ctx)
	if err != nil {
		return result, err
	}
	filters.modelKeys = activeModelKeys
	result.Items = make([]UserModelPricingOverviewItem, 0)
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		matchedUserIds := userModelPricingOverviewMatchedUserIds(tx, keyword, requesterRole, filters)
		// 三项统计合并为一次扫描；按模型哈希去重，保留大小写不同的模型语义。
		var totals struct {
			TotalUsers  int64
			TotalRules  int64
			TotalModels int64
		}
		if err := userModelPricingOverviewQuery(tx, keyword, requesterRole, filters).
			Select("COUNT(DISTINCT user_model_pricings.user_id) AS total_users, COUNT(*) AS total_rules, COUNT(DISTINCT user_model_pricings.model_key) AS total_models").
			Scan(&totals).Error; err != nil {
			return err
		}
		result.TotalUsers, result.TotalRules, result.TotalModels = totals.TotalUsers, totals.TotalRules, totals.TotalModels
		if result.TotalUsers == 0 {
			return nil
		}

		var userIds []int
		if err := matchedUserIds.
			Order("user_model_pricings.user_id ASC").
			Limit(pageSize).
			Offset(startIdx).
			Pluck("user_model_pricings.user_id", &userIds).Error; err != nil {
			return err
		}
		if len(userIds) == 0 {
			return nil
		}

		var users []UserModelPricingOverviewUser
		if err := tx.Model(&User{}).
			Select([]string{"id", "username", "display_name", "email", "group", "role", "status"}).
			Where("id IN ?", userIds).
			Find(&users).Error; err != nil {
			return err
		}

		type overviewRuleRow struct {
			UserId         int    `gorm:"column:user_id"`
			ModelName      string `gorm:"column:model_name"`
			DiscountBPS    int    `gorm:"column:discount_bps"`
			RuleCount      int
			MinDiscountBPS int
			MaxDiscountBPS int
		}
		var rows []overviewRuleRow
		// 明细沿用相同搜索条件，模型命中不能扩展为该用户的所有规则。
		query := userModelPricingOverviewQuery(tx, keyword, requesterRole, filters).
			Where("user_model_pricings.user_id IN ?", userIds)
		summary := len(summaryOnly) > 0 && summaryOnly[0]
		if summary {
			// 列表仅传输每用户一条摘要，避免规则数放大数据库结果、JSON 和浏览器内存。
			query = query.Select("user_model_pricings.user_id, COUNT(*) AS rule_count, MIN(discount_bps) AS min_discount_bps, MAX(discount_bps) AS max_discount_bps").Group("user_model_pricings.user_id")
		} else {
			query = query.Select("user_model_pricings.user_id, user_model_pricings.model_name, user_model_pricings.discount_bps").Order("user_model_pricings.user_id ASC, user_model_pricings.model_name ASC")
		}
		if err := query.Find(&rows).Error; err != nil {
			return err
		}
		var previewRules map[int][]UserModelPricingOverviewRule
		if summary {
			var err error
			previewRules, err = getUserModelPricingOverviewPreview(tx, userIds, keyword, requesterRole, filters)
			if err != nil {
				return err
			}
		}
		summaries := make(map[int]overviewRuleRow, len(userIds))
		ruleMap := make(map[int][]UserModelPricingOverviewRule, len(userIds))
		for _, row := range rows {
			if summary {
				summaries[row.UserId] = row
				continue
			}
			ruleMap[row.UserId] = append(ruleMap[row.UserId], UserModelPricingOverviewRule{
				ModelName:   row.ModelName,
				DiscountBPS: row.DiscountBPS,
			})
		}
		userMap := make(map[int]UserModelPricingOverviewUser, len(users))
		for _, user := range users {
			userMap[user.Id] = user
		}

		result.Items = make([]UserModelPricingOverviewItem, 0, len(userIds))
		for _, userId := range userIds {
			user, ok := userMap[userId]
			if !ok {
				continue
			}
			rules := ruleMap[userId]
			if rules == nil {
				rules = []UserModelPricingOverviewRule{}
			}
			item := UserModelPricingOverviewItem{
				User:      user,
				Rules:     rules,
				RuleCount: len(rules),
			}
			if summary {
				value := summaries[userId]
				item.PreviewRules = previewRules[userId]
				item.RuleCount, item.MinDiscountBPS, item.MaxDiscountBPS = value.RuleCount, value.MinDiscountBPS, value.MaxDiscountBPS
			}
			result.Items = append(result.Items, item)
		}
		return nil
	})
	if err != nil {
		return UserModelPricingOverviewResult{}, err
	}
	return result, nil
}

// GetUserModelPricingRulePage 按用户索引读取有限明细，搜索和权限范围与总览一致。
func GetUserModelPricingRulePage(ctx context.Context, userId, requesterRole int, keyword string, page int) ([]UserModelPricingOverviewRule, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	// 每用户最多 1000 条规则，限制页码可避免无意义的超大 OFFSET。
	if page < 1 || page > maxUserModelPricingRules/20+1 {
		return nil, 0, ErrUserModelPricingInvalid
	}
	activeModelKeys, err := getEnabledUserModelPricingModelKeys(ctx)
	if err != nil {
		return nil, 0, err
	}
	filters := UserModelPricingOverviewFilters{modelKeys: activeModelKeys}
	items := make([]UserModelPricingOverviewRule, 0)
	var total int64
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := userModelPricingOverviewQuery(tx, keyword, requesterRole, filters).Where("user_model_pricings.user_id = ?", userId)
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Select("user_model_pricings.model_name, user_model_pricings.discount_bps").Order("user_model_pricings.model_name ASC, user_model_pricings.model_key ASC").Offset((page - 1) * 20).Limit(20).Find(&items).Error
	})
	return items, total, err
}
