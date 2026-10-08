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
	hosttypes "github.com/QuantumNous/new-api/types"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	UserId      int    `json:"user_id" gorm:"not null;uniqueIndex:idx_user_model_pricing_slot,priority:1"`
	ModelName   string `json:"model_name" gorm:"type:varchar(128);not null"`
	ModelKey    string `json:"-" gorm:"size:64;not null;uniqueIndex:idx_user_model_pricing_slot,priority:2"`
	DiscountBPS int    `json:"discount_bps" gorm:"type:int;not null"`
	Slot        int    `json:"-" gorm:"not null;default:0;uniqueIndex:idx_user_model_pricing_slot,priority:3"`
	Mode        string `json:"mode" gorm:"size:16;not null;default:single"`
	StartTime   int64  `json:"start_time" gorm:"type:bigint;not null;default:0"`
	EndTime     *int64 `json:"end_time" gorm:"type:bigint"`
}

// userModelPricingModelKey 使用规范化模型名生成稳定键，查询时绕开数据库大小写和重音排序规则。
func userModelPricingModelKey(modelName string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(modelName)))
}

// BeforeCreate 用规范化模型名的固定长度哈希隔离数据库排序规则，保留大小写语义。
func (rule *UserModelPricing) BeforeCreate(_ *gorm.DB) error {
	rule.ModelName = ratio_setting.FormatMatchingModelName(ResolveUserModelPricingName(strings.TrimSpace(rule.ModelName)))
	_, err := normalizeUserModelDiscounts(map[string]int{rule.ModelName: rule.DiscountBPS})
	if err != nil {
		return err
	}
	rule.ModelKey = userModelPricingModelKey(rule.ModelName)
	return nil
}

// UserModelPricingRevision 将版本隔离在小表中，避免为已有用户大表新增列。
type UserModelPricingRevision struct {
	UserId   int   `gorm:"primaryKey;autoIncrement:false"`
	Revision int64 `gorm:"not null"`
}

// lockUserModelPricing 在用户存活锁之后创建并锁定版本，序列化首次配置、读取、改价和硬删除。
func lockUserModelPricing(tx *gorm.DB, userID int) (*UserModelPricingRevision, error) {
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		// SQLite 先获得写锁，避免读事务升级写事务时并发失败；不修改用户业务值。
		if err := tx.Model(&User{}).Where("id = ?", userID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return nil, err
		}
	}
	var user User
	if err := lockForUpdate(tx).Select("id").First(&user, userID).Error; err != nil {
		return nil, err
	}
	row := UserModelPricingRevision{UserId: userID, Revision: 1}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		return nil, err
	}
	if err := lockForUpdate(tx).Where("user_id = ?", userID).First(&row).Error; err != nil {
		return nil, err
	}
	if row.Revision < 1 || row.Revision > 9007199254740991 {
		return nil, ErrUserModelPricingInvalid
	}
	return &row, nil
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
		if name = ratio_setting.FormatMatchingModelName(ResolveUserModelPricingName(strings.TrimSpace(name))); name != "" {
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
		modelName = ratio_setting.FormatMatchingModelName(ResolveUserModelPricingName(strings.TrimSpace(modelName)))
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

// GetUserModelPricing 在同一事务内锁定用户行，再读取规则和 revision，保证管理页面拿到同一版本的完整快照。
func GetUserModelPricing(userId int) (map[string]int, int64, error) {
	return GetUserModelPricingContext(context.Background(), userId)
}

// GetUserModelPricingContext 将管理查询和缓存回源绑定到调用方取消信号及统一时间预算。
func GetUserModelPricingContext(ctx context.Context, userId int) (map[string]int, int64, error) {
	snapshot, err := loadUserModelPricingSnapshot(ctx, userId, false, false)
	return snapshot.discounts.Copy(), snapshot.revision, err
}

func GetUserModelPricingRulesContext(ctx context.Context, userId int) ([]UserModelPricingItem, int64, error) {
	snapshot, err := loadUserModelPricingSnapshot(ctx, userId, false, false)
	if err != nil {
		return nil, 0, err
	}
	configs := snapshot.discounts.Configurations()
	items := make([]UserModelPricingItem, 0, len(configs))
	for name, config := range configs {
		items = append(items, userModelPricingItem(name, config))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModelName < items[j].ModelName })
	return items, snapshot.revision, nil
}

// loadUserModelPricingSnapshot 在同一用户锁内读取规则和发布缓存版本，防止迟到的回源复活旧版本。
func loadUserModelPricingSnapshot(ctx context.Context, userId int, publishVersion, reuseCache bool) (userModelPricingSnapshot, error) {
	var snapshot userModelPricingSnapshot
	var discounts map[string]hosttypes.UserModelDiscountConfig
	if userId <= 0 {
		return snapshot, ErrUserModelPricingInvalid
	}

	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	expiresAt := time.Now().Add(userModelPricingCacheTTL)
	key := fmt.Sprintf("%p:%d", DB, userId)
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		version, err := lockUserModelPricing(tx, userId)
		if err != nil {
			return err
		}
		snapshot.revision = version.Revision
		// 数据库锁内验证版本后才复用快照，其他节点改价不依赖本地失效通知。
		if reuseCache {
			if cached, exists := userModelPricingCache.get(key, version.Revision); exists {
				snapshot = cached
				return nil
			}
		}

		discounts, err = readUserModelPricingSchedules(tx, userId)
		if err != nil {
			return err
		}
		if publishVersion && publishUserModelPricingVersion(ctx, userId, snapshot.revision, false) == nil {
			snapshot.expiresAt = expiresAt
		}
		if reuseCache && !publishVersion {
			snapshot.expiresAt = expiresAt
		}
		return nil
	})
	if err != nil {
		return userModelPricingSnapshot{}, err
	}
	// 规则映射由本次查询独占，事务提交后再构造只读副本，减少用户行锁持有时间。
	if discounts != nil {
		snapshot.discounts = hosttypes.NewUserModelDiscountScheduleSnapshot(discounts)
	}
	if reuseCache && !snapshot.expiresAt.IsZero() {
		userModelPricingCache.put(key, snapshot)
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
	normalized, err := normalizeUserModelDiscounts(discounts)
	if err != nil {
		return 0, err
	}
	items := make([]UserModelPricingItem, 0, len(normalized))
	for name, bps := range normalized {
		items = append(items, UserModelPricingItem{ModelName: name, UserModelPricingPeriod: UserModelPricingPeriod{DiscountBPS: bps}})
	}
	return replaceUserModelPricingSchedules(ctx, userId, items, expectedRevision, 0)
}
