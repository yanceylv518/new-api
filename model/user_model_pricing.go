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
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
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

// UserModelPricing 在独立表中保存单个用户的规范化模型折扣。
type UserModelPricing struct {
	Id          int    `json:"id" gorm:"primaryKey"`
	UserId      int    `json:"user_id" gorm:"not null;uniqueIndex:idx_user_model_pricing_user_key,priority:1"`
	ModelName   string `json:"model_name" gorm:"type:varchar(128);not null"`
	ModelKey    string `json:"-" gorm:"size:64;uniqueIndex:idx_user_model_pricing_user_key,priority:2"`
	DiscountBPS int    `json:"discount_bps" gorm:"type:int;not null"`
}

// BeforeCreate 用规范化模型名的固定长度哈希隔离数据库排序规则，保留大小写语义。
func (rule *UserModelPricing) BeforeCreate(_ *gorm.DB) error {
	rule.ModelName = ratio_setting.FormatMatchingModelName(strings.TrimSpace(rule.ModelName))
	_, err := normalizeUserModelDiscounts(map[string]int{rule.ModelName: rule.DiscountBPS})
	if err != nil {
		return err
	}
	rule.ModelKey = fmt.Sprintf("%x", sha256.Sum256([]byte(rule.ModelName)))
	return nil
}

// InitializeUserModelPricingKeys 在新索引生效后分批回填旧行，最后移除依赖排序规则的旧索引。
func InitializeUserModelPricingKeys() error {
	if err := DB.Transaction(func(tx *gorm.DB) error {
		var rows []UserModelPricing
		if err := tx.Where("model_key IS NULL OR model_key = ?", "").FindInBatches(&rows, 100, func(batch *gorm.DB, _ int) error {
			for _, row := range rows {
				key := fmt.Sprintf("%x", sha256.Sum256([]byte(row.ModelName)))
				if err := tx.Model(&UserModelPricing{}).Where("id = ?", row.Id).UpdateColumn("model_key", key).Error; err != nil {
					return err
				}
			}
			return nil
		}).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	// MySQL DDL 会隐式提交，索引切换必须放在数据回填事务之外，允许失败后再次启动重试。
	if DB.Migrator().HasIndex(&UserModelPricing{}, "idx_user_model_pricing_user_model") {
		return DB.Migrator().DropIndex(&UserModelPricing{}, "idx_user_model_pricing_user_model")
	}
	return nil
}

func (UserModelPricing) TableName() string {
	return "user_model_pricings"
}

// GetUserModelPricingModelNames 返回所有分组的启用模型，不使用操作者或目标用户分组过滤。
func GetUserModelPricingModelNames(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	var channelModels []string
	// 在 Go 中去重，避免 MySQL 的大小写不敏感 DISTINCT 合并不同模型名。
	if err := DB.WithContext(ctx).Model(&Ability{}).Where("enabled = ?", true).Pluck("model", &channelModels).Error; err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(channelModels))
	for _, name := range channelModels {
		if name = strings.TrimSpace(name); name != "" {
			names[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
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
	return GetUserModelPricingContext(context.Background(), userId)
}

// GetUserModelPricingContext 将管理查询和缓存回源绑定到调用方取消信号及统一时间预算。
func GetUserModelPricingContext(ctx context.Context, userId int) (map[string]int, int64, error) {
	snapshot, err := loadUserModelPricingSnapshot(ctx, userId, false)
	return snapshot.discounts, snapshot.revision, err
}

// loadUserModelPricingSnapshot 在同一用户锁内读取规则和发布缓存版本，防止迟到的回源复活旧版本。
func loadUserModelPricingSnapshot(ctx context.Context, userId int, publishVersion bool) (userModelPricingSnapshot, error) {
	var snapshot userModelPricingSnapshot
	if userId <= 0 {
		return snapshot, ErrUserModelPricingInvalid
	}

	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	expiresAt := time.Now().Add(userModelPricingCacheTTL)
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// SQLite 的读事务不阻止 WAL 写入，用无值变化的更新持有写锁直到版本发布完成。
		if publishVersion && common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&User{}).Where("id = ?", userId).UpdateColumn("model_pricing_version", gorm.Expr("model_pricing_version")).Error; err != nil {
				return err
			}
		}
		var user User
		if err := lockForUpdate(tx).Select("id", "model_pricing_version").First(&user, userId).Error; err != nil {
			return err
		}
		snapshot.revision = user.ModelPricingVersion
		if snapshot.revision < 1 {
			snapshot.revision = 1
		}

		var err error
		snapshot.discounts, err = readUserModelPricing(tx, userId)
		if err != nil {
			return err
		}
		if publishVersion && publishUserModelPricingVersion(ctx, userId, snapshot.revision, false) == nil {
			snapshot.expiresAt = expiresAt
		}
		return nil
	})
	if err != nil {
		return userModelPricingSnapshot{}, err
	}
	return snapshot, nil
}

// GetUserModelDiscountBPS 读取运行时规则；缓存命中时不访问数据库。
func GetUserModelDiscountBPS(userId int) (map[string]int, error) {
	return GetUserModelDiscountBPSContext(context.Background(), userId)
}

// ReplaceUserModelPricing 原子替换完整规则集，并校验调用方读取时的 revision。
func ReplaceUserModelPricing(userId int, discounts map[string]int, expectedRevision int64) (int64, error) {
	return ReplaceUserModelPricingContext(context.Background(), userId, discounts, expectedRevision)
}

// ReplaceUserModelPricingContext 限制整个替换事务的生命周期，客户端取消时停止等待和写入。
func ReplaceUserModelPricingContext(ctx context.Context, userId int, discounts map[string]int, expectedRevision int64) (int64, error) {
	if expectedRevision <= 0 {
		return 0, ErrUserModelPricingInvalid
	}
	return replaceUserModelPricing(ctx, userId, discounts, &expectedRevision)
}

// replaceUserModelPricing 在同一事务中完成版本校验、规则替换和版本递增。
// expectedRevision 为 nil 时仅供内部更新入口使用，对外替换必须携带正 revision。
func replaceUserModelPricing(ctx context.Context, userId int, discounts map[string]int, expectedRevision *int64) (int64, error) {
	if userId <= 0 || (expectedRevision != nil && *expectedRevision <= 0) {
		return 0, ErrUserModelPricingInvalid
	}
	normalized, err := normalizeUserModelDiscounts(discounts)
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	var nextRevision int64
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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

		// 提交前建立跨节点屏障；失败则回滚，禁止仍可能命中旧缓存时提交新价格。
		if common.RedisEnabled {
			if err := publishUserModelPricingVersion(ctx, userId, nextRevision, true); err != nil {
				return err
			}
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
	// 保留提交前屏障，由下一次持有用户锁的回源发布已提交版本，避免事务外迟到发布。
	return nextRevision, nil
}
