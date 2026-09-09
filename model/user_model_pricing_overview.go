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
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"strings"
)

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

// UserModelPricingOverviewItem 按用户聚合规则，避免前端为每个用户再次请求折扣接口。
type UserModelPricingOverviewItem struct {
	User           UserModelPricingOverviewUser   `json:"user"`
	Rules          []UserModelPricingOverviewRule `json:"rules"`
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

// userModelPricingOverviewQuery 统一构造权限和搜索条件，保证统计、分页和明细使用同一数据范围。
func userModelPricingOverviewQuery(tx *gorm.DB, keyword string, requesterRole int) *gorm.DB {
	query := tx.Model(&UserModelPricing{}).
		Joins("JOIN users ON users.id = user_model_pricings.user_id").
		Where("users.deleted_at IS NULL")
	if requesterRole != common.RoleRootUser {
		query = query.Where("users.role < ?", requesterRole)
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

// userModelPricingOverviewMatchedUserIds 固定搜索结果的用户集合，供统计和分页复用同一个权限边界。
func userModelPricingOverviewMatchedUserIds(tx *gorm.DB, keyword string, requesterRole int) *gorm.DB {
	return userModelPricingOverviewQuery(tx, keyword, requesterRole).
		Select("user_model_pricings.user_id").
		Distinct("user_model_pricings.user_id")
}

// GetUserModelPricingOverview 返回管理员可见的用户折扣汇总；分页按用户而不是规则行计算。
func GetUserModelPricingOverview(ctx context.Context, keyword string, requesterRole, startIdx, pageSize int, summaryOnly ...bool) (UserModelPricingOverviewResult, error) {
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
	result.Items = make([]UserModelPricingOverviewItem, 0)
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		matchedUserIds := userModelPricingOverviewMatchedUserIds(tx, keyword, requesterRole)
		// 三项统计合并为一次扫描；按模型哈希去重，保留大小写不同的模型语义。
		var totals struct {
			TotalUsers  int64
			TotalRules  int64
			TotalModels int64
		}
		if err := userModelPricingOverviewQuery(tx, keyword, requesterRole).
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
		query := userModelPricingOverviewQuery(tx, keyword, requesterRole).
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
	items := make([]UserModelPricingOverviewRule, 0)
	var total int64
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := userModelPricingOverviewQuery(tx, keyword, requesterRole).Where("user_model_pricings.user_id = ?", userId)
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		return query.Select("user_model_pricings.model_name, user_model_pricings.discount_bps").Order("user_model_pricings.model_name ASC, user_model_pricings.model_key ASC").Offset((page - 1) * 20).Limit(20).Find(&items).Error
	})
	return items, total, err
}
