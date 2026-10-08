package model

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

type userModelPricingHistoryJSON string

// 千段排期的历史快照可能超过 MySQL TEXT 的 64 KiB 上限。
func (userModelPricingHistoryJSON) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "mysql" {
		return "mediumtext"
	}
	return "text"
}

type UserModelPricingHistory struct {
	Id          int64                       `json:"id" gorm:"primaryKey"`
	UserId      int                         `json:"user_id" gorm:"not null;index:idx_pricing_history_user_id,priority:1"`
	SubjectRole int                         `json:"-" gorm:"not null"`
	ActorId     int                         `json:"actor_id" gorm:"not null"`
	ModelName   string                      `json:"model_name" gorm:"type:varchar(128);not null"`
	Action      string                      `json:"action" gorm:"size:24;not null"`
	Revision    int64                       `json:"revision" gorm:"type:bigint;not null"`
	CreatedAt   int64                       `json:"created_at" gorm:"type:bigint;not null;index:idx_pricing_history_user_id,priority:2"`
	BeforeJSON  userModelPricingHistoryJSON `json:"-" gorm:"not null"`
	AfterJSON   userModelPricingHistoryJSON `json:"-" gorm:"not null"`
}

type UserModelPricingHistoryItem struct {
	UserModelPricingHistory
	Before      *hosttypes.UserModelDiscountConfig `json:"before" gorm:"-"`
	After       *hosttypes.UserModelDiscountConfig `json:"after" gorm:"-"`
	UserDeleted bool                               `json:"user_deleted"`
}

func newUserModelPricingHistory(userID, role, actorID int, name, action string, revision, now int64, before, after hosttypes.UserModelDiscountConfig) (UserModelPricingHistory, error) {
	var beforeValue, afterValue *hosttypes.UserModelDiscountConfig
	if len(before.Periods) > 0 {
		beforeValue = &before
	}
	if len(after.Periods) > 0 {
		afterValue = &after
	}
	beforeJSON, err := common.Marshal(beforeValue)
	if err != nil {
		return UserModelPricingHistory{}, err
	}
	afterJSON, err := common.Marshal(afterValue)
	if err != nil {
		return UserModelPricingHistory{}, err
	}
	return UserModelPricingHistory{
		UserId: userID, SubjectRole: role, ActorId: actorID, ModelName: name, Action: action,
		Revision: revision, CreatedAt: now, BeforeJSON: userModelPricingHistoryJSON(beforeJSON), AfterJSON: userModelPricingHistoryJSON(afterJSON),
	}, nil
}

// archiveDeletedUserModelPricing 在硬删除的用户锁内保存最后配置，历史不参与运行时计价。
func archiveDeletedUserModelPricing(tx *gorm.DB, userID, actorID int) error {
	configs, err := readUserModelPricingSchedules(tx, userID)
	if err != nil {
		return err
	}
	var user User
	if err := tx.Unscoped().Select("id", "role").First(&user, userID).Error; err != nil {
		return err
	}
	// 冻结删除时的权限边界，避免用户升权后旧记录重新向低权限管理员开放。
	if err := tx.Model(&UserModelPricingHistory{}).Where("user_id = ?", userID).UpdateColumn("subject_role", user.Role).Error; err != nil {
		return err
	}
	if len(configs) == 0 {
		return nil
	}
	var version UserModelPricingRevision
	if err := tx.Where("user_id = ?", userID).First(&version).Error; err != nil {
		return err
	}
	if version.Revision >= 9007199254740991 {
		return ErrUserModelPricingInvalid
	}
	now := time.Now().Unix()
	history := make([]UserModelPricingHistory, 0, len(configs))
	for name, config := range configs {
		entry, err := newUserModelPricingHistory(userID, user.Role, actorID, name, "user_deleted", version.Revision+1, now, config, hosttypes.UserModelDiscountConfig{})
		if err != nil {
			return err
		}
		history = append(history, entry)
	}
	return tx.CreateInBatches(&history, userModelPricingWriteBatchSize).Error
}

func GetUserModelPricingHistory(ctx context.Context, userID, requesterRole, page, pageSize int) ([]UserModelPricingHistoryItem, int64, error) {
	if userID < 0 || requesterRole < common.RoleAdminUser || page < 1 || pageSize < 1 || pageSize > 100 || page > 1000000 {
		return nil, 0, ErrUserModelPricingInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	items := make([]UserModelPricingHistoryItem, 0)
	var total int64
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&UserModelPricingHistory{}).
			Joins("LEFT JOIN users ON users.id = user_model_pricing_histories.user_id")
		if userID > 0 {
			query = query.Where("user_model_pricing_histories.user_id = ?", userID)
		}
		if requesterRole != common.RoleRootUser {
			query = query.Where("COALESCE(users.role, user_model_pricing_histories.subject_role) < ?", requesterRole)
		}
		if err := query.Count(&total).Error; err != nil {
			return err
		}
		if err := query.Select("user_model_pricing_histories.*, CASE WHEN users.id IS NULL OR users.deleted_at IS NOT NULL THEN 1 ELSE 0 END AS user_deleted").
			Order("user_model_pricing_histories.id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Scan(&items).Error; err != nil {
			return err
		}
		for i := range items {
			if err := common.UnmarshalJsonStr(string(items[i].BeforeJSON), &items[i].Before); err != nil {
				return err
			}
			if err := common.UnmarshalJsonStr(string(items[i].AfterJSON), &items[i].After); err != nil {
				return err
			}
		}
		return nil
	})
	return items, total, err
}
