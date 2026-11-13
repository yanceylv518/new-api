package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const billingRefundRetention = 7 * 24 * time.Hour
const billingRefundCleanupBatchSize = 1000

// BillingRefund 保存提交失败的退款交接，资金与完成标记在同一事务中更新。
type BillingRefund struct {
	ID             int64  `gorm:"primaryKey"`
	RequestID      string `gorm:"type:varchar(64);uniqueIndex"`
	UserID         int
	TokenID        int
	Amount         int
	TokenAmount    int
	SubscriptionID int
	PeriodStart    int64
	Completed      bool  `gorm:"index:idx_billing_refund_pending,priority:1;index:idx_billing_refund_cleanup,priority:1"`
	CachePending   bool  `gorm:"index:idx_billing_refund_cache,priority:1"`
	NextAttemptAt  int64 `gorm:"not null;default:0;index:idx_billing_refund_pending,priority:2"`
	Attempts       int
	CreatedAt      int64 `gorm:"index:idx_billing_refund_cleanup,priority:2"`
}

func QueueBillingRefund(ctx context.Context, refund *BillingRefund) error {
	return queueBillingRefund(DB.WithContext(ctx), refund)
}

func queueBillingRefund(tx *gorm.DB, refund *BillingRefund) error {
	if refund.RequestID == "" || len(refund.RequestID) > 64 || refund.UserID <= 0 || refund.Amount < 0 || refund.Amount > common.MaxQuota || refund.TokenAmount < 0 || refund.TokenAmount > common.MaxQuota || refund.TokenAmount > 0 && refund.TokenAmount != refund.Amount {
		return errors.New("invalid billing refund")
	}
	refund.CreatedAt = common.GetTimestamp()
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(refund).Error; err != nil {
		return err
	}
	var stored BillingRefund
	if err := tx.Where("request_id = ?", refund.RequestID).First(&stored).Error; err != nil {
		return err
	}
	if stored.UserID != refund.UserID || stored.TokenID != refund.TokenID || stored.Amount != refund.Amount || stored.TokenAmount != refund.TokenAmount || stored.SubscriptionID != refund.SubscriptionID || stored.PeriodStart != refund.PeriodStart {
		return errors.New("billing refund reservation changed")
	}
	*refund = stored
	return nil
}

// StageBillingTask 把预扣恢复记录与任务意图原子交接，后台退款已完成时禁止再提交上游。
func StageBillingTask(ctx context.Context, task *Task, refundID int64) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if refundID > 0 {
			var refund BillingRefund
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", refundID, task.UserId).First(&refund).Error; err != nil {
				return err
			}
			reserved := task.Quota
			if task.PrivateData.SubmissionFunding != nil {
				reserved = task.PrivateData.SubmissionFunding.ReservedQuota
			}
			if refund.Completed || refund.Amount != reserved {
				return errors.New("task reservation is no longer available")
			}
			if err := tx.Delete(&refund).Error; err != nil {
				return err
			}
		}
		var user User
		if err := lockForUpdate(tx).Select("id", "status").Where("id = ?", task.UserId).First(&user).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return errors.New("task owner is unavailable")
		}
		return tx.Create(task).Error
	})
}

func CompleteBillingRefund(ctx context.Context, id int64) error {
	var refund BillingRefund
	var tokenKey string
	var completed bool
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("id = ?", id).First(&refund).Error; err != nil {
			return err
		}
		if refund.Completed {
			return nil
		}
		if refund.SubscriptionID > 0 {
			if err := postConsumeUserSubscriptionDeltaTx(tx, refund.SubscriptionID, -int64(refund.Amount), refund.PeriodStart, refund.UserID); err != nil {
				return err
			}
			if err := tx.Model(&SubscriptionPreConsumeRecord{}).Where("request_id = ? AND user_id = ?", refund.RequestID, refund.UserID).Update("status", "refunded").Error; err != nil {
				return err
			}
		}
		// 锁住用户后再建立缓存恢复标记，避免与视频终态结算交错。
		var user User
		if err := lockForUpdate(tx).Where("id = ?", refund.UserID).First(&user).Error; err != nil {
			return err
		}
		if refund.SubscriptionID == 0 {
			if int64(user.Quota) > common.MaxWalletQuota-int64(refund.Amount) {
				return errors.New("billing refund quota overflow")
			}
			if err := tx.Model(&user).Update("quota", gorm.Expr("quota + ?", refund.Amount)).Error; err != nil {
				return err
			}
		}
		if refund.TokenAmount > 0 && refund.TokenID > 0 {
			var token Token
			err := lockForUpdate(tx).Where("id = ? AND user_id = ?", refund.TokenID, refund.UserID).First(&token).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil {
				tokenKey = token.Key
				if err := tx.Model(&token).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", refund.TokenAmount), "used_quota": gorm.Expr("used_quota - ?", refund.TokenAmount)}).Error; err != nil {
					return err
				}
			}
		}
		cacheTask := &Task{ID: refund.ID, UserId: refund.UserID, Quota: refund.Amount}
		if err := prepareQuotaCache(ctx, tx, cacheTask, 0, refund.SubscriptionID == 0, tokenKey, "refund"); err != nil {
			return err
		}
		if err := tx.Model(&refund).Updates(map[string]any{"completed": true, "cache_pending": common.RedisEnabled}).Error; err != nil {
			return err
		}
		completed = true
		return nil
	})
	if err != nil || !common.RedisEnabled {
		return err
	}
	wallet, cacheKey := "0", ""
	if refund.SubscriptionID == 0 {
		wallet = "1"
	}
	if tokenKey != "" {
		cacheKey = getTokenCacheKey(tokenKey)
	}
	if completed {
		err = applyVideoQuotaCache(ctx, refund.UserID, refund.ID, refund.Amount, wallet, cacheKey, "refund")
	} else if refund.Completed && refund.CachePending {
		err = recoverVideoQuotaCache(ctx, refund.UserID)
	} else {
		return nil
	}
	if err != nil {
		common.SysError("billing refund cache synchronization pending: " + err.Error())
		return nil
	}
	return DB.WithContext(ctx).Model(&BillingRefund{}).Where("id = ?", refund.ID).Update("cache_pending", false).Error
}

func RetryBillingRefunds(ctx context.Context) error {
	var pending []BillingRefund
	if err := DB.WithContext(ctx).Where("completed = ? AND next_attempt_at <= ?", false, common.GetTimestamp()).Order("next_attempt_at ASC, id ASC").Limit(100).Find(&pending).Error; err != nil {
		return err
	}
	if common.RedisEnabled {
		var cachePending []BillingRefund
		if err := DB.WithContext(ctx).Where("cache_pending = ? AND completed = ? AND next_attempt_at <= ?", true, true, common.GetTimestamp()).Order("next_attempt_at ASC, id ASC").Limit(100).Find(&cachePending).Error; err != nil {
			return err
		}
		pending = append(pending, cachePending...)
	}
	var result error
	// 先清理到期流水，持续的退款故障不能使保留期限失效。
	var expired []int64
	if err := DB.WithContext(ctx).Model(&BillingRefund{}).Where("completed = ? AND cache_pending = ? AND created_at < ?", true, false, time.Now().Add(-billingRefundRetention).Unix()).Order("created_at ASC, id ASC").Limit(billingRefundCleanupBatchSize).Pluck("id", &expired).Error; err != nil {
		result = errors.Join(result, err)
	} else if len(expired) > 0 {
		if err := DB.WithContext(ctx).Where("id IN ? AND completed = ? AND cache_pending = ?", expired, true, false).Delete(&BillingRefund{}).Error; err != nil {
			result = errors.Join(result, err)
		}
	}
	for _, refund := range pending {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		if err := CompleteBillingRefund(ctx, refund.ID); err != nil {
			result = errors.Join(result, fmt.Errorf("refund %d: %w", refund.ID, err))
			delay := min(int64(15)<<min(refund.Attempts, 6), int64(900))
			if err := DB.WithContext(ctx).Model(&BillingRefund{}).Where("id = ? AND (completed = ? OR cache_pending = ?)", refund.ID, false, true).
				Updates(map[string]any{"attempts": refund.Attempts + 1, "next_attempt_at": common.GetTimestamp() + delay + refund.ID%5}).Error; err != nil {
				result = errors.Join(result, err)
			}
		}
	}
	return result
}

func HasExpiredBillingRefunds() bool {
	var id int64
	err := DB.Model(&BillingRefund{}).
		Where("completed = ? AND cache_pending = ? AND created_at < ?", true, false, time.Now().Add(-billingRefundRetention).Unix()).
		Limit(1).Pluck("id", &id).Error
	return err == nil && id != 0
}

func HasPendingBillingRefunds() bool {
	var id int64
	err := DB.Model(&BillingRefund{}).
		Where("(completed = ? OR cache_pending = ?) AND next_attempt_at <= ?", false, true, common.GetTimestamp()).
		Limit(1).Pluck("id", &id).Error
	return err == nil && id != 0
}
