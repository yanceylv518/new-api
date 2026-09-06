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
	"errors"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"gorm.io/gorm"
)

const (
	maxUserModelPricingRules = 1000
	userModelPricingNameSize = 128
	fullPriceDiscountBPS     = 10000
)

var (
	// ErrUserModelPricingInvalid 表示规则不满足模型名和折扣范围约束。
	ErrUserModelPricingInvalid = errors.New("invalid user model pricing")
	// ErrUserModelPricingRevisionConflict 表示客户端提交的规则基于旧版本。
	ErrUserModelPricingRevisionConflict = errors.New("user model pricing revision conflict")
)

// UserModelPricing 保存单个用户的规范化模型折扣，避免与 users.setting 中的其他偏好共用 JSON 字段。
type UserModelPricing struct {
	Id          int    `json:"id" gorm:"primaryKey"`
	UserId      int    `json:"user_id" gorm:"not null;uniqueIndex:idx_user_model_pricing_user_model,priority:1"`
	ModelName   string `json:"model_name" gorm:"type:varchar(128);not null;uniqueIndex:idx_user_model_pricing_user_model,priority:2"`
	DiscountBPS int    `json:"discount_bps" gorm:"type:int;not null"`
}

func (UserModelPricing) TableName() string {
	return "user_model_pricings"
}

// normalizeUserModelDiscounts 在所有写入边界统一模型名和折扣范围。
// 10000 表示原价，保持旧接口语义但不落入独立表，以免产生无效规则。
func normalizeUserModelDiscounts(discounts map[string]int) (map[string]int, error) {
	if len(discounts) > maxUserModelPricingRules {
		return nil, ErrUserModelPricingInvalid
	}

	normalized := make(map[string]int, len(discounts))
	for modelName, discountBPS := range discounts {
		modelName = ratio_setting.FormatMatchingModelName(strings.TrimSpace(modelName))
		if modelName == "" || len(modelName) > userModelPricingNameSize || discountBPS < 1 || discountBPS > fullPriceDiscountBPS {
			return nil, ErrUserModelPricingInvalid
		}
		if _, exists := normalized[modelName]; exists {
			return nil, ErrUserModelPricingInvalid
		}
		normalized[modelName] = discountBPS
	}
	for modelName, discountBPS := range normalized {
		if discountBPS == fullPriceDiscountBPS {
			delete(normalized, modelName)
		}
	}
	return normalized, nil
}

// readUserModelPricing 在单条查询中读取规则；事务快照保证不会看到替换事务的半成品。
func readUserModelPricing(tx *gorm.DB, userId int) (map[string]int, error) {
	discounts := make(map[string]int)
	var rows []UserModelPricing
	if err := tx.Where("user_id = ?", userId).Order("model_name ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		discounts[row.ModelName] = row.DiscountBPS
	}
	return discounts, nil
}

// GetUserModelPricing 在同一事务内锁定用户行，再读取规则和 revision，保证管理页面拿到同一版本的完整快照。
func GetUserModelPricing(userId int) (map[string]int, int64, error) {
	if userId <= 0 {
		return nil, 0, errors.New("id 为空！")
	}

	var discounts map[string]int
	var revision int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Select("id", "model_pricing_version").First(&user, userId).Error; err != nil {
			return err
		}
		revision = user.ModelPricingVersion
		if revision < 1 {
			revision = 1
		}

		var err error
		discounts, err = readUserModelPricing(tx, userId)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	return discounts, revision, nil
}

// GetUserModelDiscountBPS 读取运行时计费使用的规则，不锁定用户行以避免请求之间互相阻塞。
func GetUserModelDiscountBPS(userId int) (map[string]int, error) {
	if userId <= 0 {
		return nil, errors.New("id 为空！")
	}
	return readUserModelPricing(DB, userId)
}

// ReplaceUserModelPricing 原子替换完整规则集，并校验调用方读取时的 revision。
func ReplaceUserModelPricing(userId int, discounts map[string]int, expectedRevision int64) (int64, error) {
	if expectedRevision <= 0 {
		return 0, ErrUserModelPricingInvalid
	}
	return replaceUserModelPricing(userId, discounts, &expectedRevision)
}

// replaceUserModelPricing 在同一事务中完成版本校验、规则替换和版本递增。
// expectedRevision 为 nil 时仅供内部更新入口使用，对外替换必须携带正 revision。
func replaceUserModelPricing(userId int, discounts map[string]int, expectedRevision *int64) (int64, error) {
	if userId <= 0 || (expectedRevision != nil && *expectedRevision <= 0) {
		return 0, ErrUserModelPricingInvalid
	}
	normalized, err := normalizeUserModelDiscounts(discounts)
	if err != nil {
		return 0, err
	}

	var nextRevision int64
	err = DB.Transaction(func(tx *gorm.DB) error {
		// SQLite 不支持行锁，必须先用带版本条件的原子更新抢占本次写入权，避免两个管理员同时通过旧快照校验。
		revisionQuery := tx.Model(&User{}).Where("id = ?", userId)
		if expectedRevision != nil {
			revisionQuery = revisionQuery.Where("model_pricing_version = ?", *expectedRevision)
		}
		result := revisionQuery.UpdateColumn("model_pricing_version", gorm.Expr("model_pricing_version + ?", 1))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			if expectedRevision != nil {
				var user User
				if err := tx.Select("id").First(&user, userId).Error; err != nil {
					return err
				}
				return ErrUserModelPricingRevisionConflict
			}
			return gorm.ErrRecordNotFound
		}

		if expectedRevision != nil {
			nextRevision = *expectedRevision + 1
		} else {
			var user User
			if err := tx.Select("model_pricing_version").First(&user, userId).Error; err != nil {
				return err
			}
			nextRevision = user.ModelPricingVersion
		}

		if err := tx.Where("user_id = ?", userId).Delete(&UserModelPricing{}).Error; err != nil {
			return err
		}
		if len(normalized) > 0 {
			rows := make([]UserModelPricing, 0, len(normalized))
			for modelName, discountBPS := range normalized {
				rows = append(rows, UserModelPricing{
					UserId:      userId,
					ModelName:   modelName,
					DiscountBPS: discountBPS,
				})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].ModelName < rows[j].ModelName })
			if err := tx.Create(&rows).Error; err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}
	return nextRevision, nil
}
