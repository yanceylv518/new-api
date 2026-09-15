package model

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// FinalizeVideoTask 将任务终态、最终额度、钱包/订阅、令牌和用量统计作为一个事务提交。
// fromStatus 是调用方读取的原状态；只有持有该状态的事务可以结算，失败则保持可重试的非终态。
// 可选差额日志与账务一并持久化；返回 false 表示另一请求已完成此任务。
func FinalizeVideoTask(ctx context.Context, task *Task, fromStatus TaskStatus, actualQuota int, adjustmentLogs ...*Log) (bool, error) {
	if task == nil || task.ID <= 0 || (task.Status != TaskStatusSuccess && task.Status != TaskStatusFailure) || actualQuota < 0 || actualQuota > math.MaxInt32 {
		return false, fmt.Errorf("invalid video task settlement")
	}
	var won bool
	var delta int
	var tokenKey string
	var wallet bool
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var stored Task
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.ID, task.UserId).First(&stored).Error; err != nil {
			return err
		}
		if stored.Status != fromStatus || stored.Status == TaskStatusSuccess || stored.Status == TaskStatusFailure {
			return nil
		}
		if stored.Quota < 0 || stored.Quota > math.MaxInt32 || stored.Quota != task.Quota {
			return fmt.Errorf("video task reserved quota changed")
		}
		delta = actualQuota - stored.Quota
		wallet = stored.PrivateData.BillingSource != "subscription" || stored.PrivateData.SubscriptionId <= 0
		if delta != 0 {
			if wallet {
				query := tx.Model(&User{}).Where("id = ?", stored.UserId)
				if delta < 0 {
					query = query.Where("quota <= ?", common.MaxWalletQuota+int64(delta))
				} else {
					// 结算允许记录有限欠费，但不能让异常余额越过可精确表示的范围。
					query = query.Where("quota >= ?", -common.MaxWalletQuota+int64(delta))
				}
				result := query.Updates(map[string]any{"quota": gorm.Expr("quota - ?", delta), "used_quota": gorm.Expr("used_quota + ?", delta)})
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return fmt.Errorf("video task wallet unavailable or quota limit exceeded")
				}
			} else {
				var subscription UserSubscription
				if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", stored.PrivateData.SubscriptionId, stored.UserId).First(&subscription).Error; err != nil {
					return err
				}
				if delta > 0 && subscription.AmountUsed > math.MaxInt64-int64(delta) {
					return fmt.Errorf("subscription quota overflow")
				}
				used := max(subscription.AmountUsed+int64(delta), 0)
				if subscription.AmountTotal > 0 && used > subscription.AmountTotal {
					return fmt.Errorf("subscription quota exceeded")
				}
				if err := tx.Model(&subscription).Update("amount_used", used).Error; err != nil {
					return err
				}
				result := tx.Model(&User{}).Where("id = ?", stored.UserId).Update("used_quota", gorm.Expr("used_quota + ?", delta))
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return fmt.Errorf("video task subscription owner unavailable")
				}
			}
			if stored.PrivateData.TokenId > 0 {
				var token Token
				err := tx.Where("id = ? AND user_id = ?", stored.PrivateData.TokenId, stored.UserId).First(&token).Error
				// 已删除令牌不阻断用户资金对账；仍存在的令牌必须与资金一起成功更新。
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil {
					tokenKey = token.Key
					if err := tx.Model(&token).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", delta), "used_quota": gorm.Expr("used_quota + ?", delta), "accessed_time": common.GetTimestamp()}).Error; err != nil {
						return err
					}
				}
			}
			// 用户/令牌行锁仍被事务持有，先建立恢复标记，再允许终态与资金一起提交。
			if err := prepareVideoQuotaCache(ctx, tx, &stored, actualQuota, wallet, tokenKey); err != nil {
				return err
			}
			// Redis往返不占用共享渠道行锁，避免不同用户的结算互相排队。
			if err := tx.Model(&Channel{}).Where("id = ?", stored.ChannelId).Update("used_quota", gorm.Expr("used_quota + ?", delta)).Error; err != nil {
				return err
			}
		}
		// 复制后更新，回滚不能把调用方预扣快照误改成已结算额度。
		final := *task
		final.Quota = actualQuota
		result := tx.Model(&final).Where("status = ? AND quota = ?", fromStatus, stored.Quota).Select("*").Updates(&final)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("video task settlement lost status ownership")
		}
		// 同库日志直接参与资金事务；独立日志库只在主库持久化待补写记录。
		if len(adjustmentLogs) > 0 && adjustmentLogs[0] != nil {
			log := adjustmentLogs[0]
			if LOG_DB == DB {
				if err := tx.Create(log).Error; err != nil {
					return err
				}
			} else {
				payload, err := common.Marshal(log)
				if err != nil {
					return err
				}
				if err := tx.Create(&VideoTaskPendingLog{TaskID: task.ID, Payload: string(payload)}).Error; err != nil {
					return err
				}
			}
		}
		won = true
		return nil
	})
	if err != nil || !won {
		return false, err
	}
	task.Quota = actualQuota
	// 单次Lua同时更新两个缓存；失败保留标记，下次读取/预扣会先恢复而不是使用旧余额。
	if delta != 0 && common.RedisEnabled {
		flag := "0"
		if wallet {
			flag = "1"
		}
		cacheKey := ""
		if tokenKey != "" {
			cacheKey = getTokenCacheKey(tokenKey)
		}
		if err := applyVideoQuotaCache(ctx, task.UserId, task.ID, -delta, flag, cacheKey); err != nil {
			common.SysError("video task cache synchronization pending: " + err.Error())
		}
	}
	return true, nil
}
