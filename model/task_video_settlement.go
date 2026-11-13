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
	var final Task
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var stored Task
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.ID, task.UserId).First(&stored).Error; err != nil {
			return err
		}
		if stored.Status != fromStatus || stored.Status == TaskStatusSuccess || stored.Status == TaskStatusFailure {
			return nil
		}
		if stored.PrivateData.InitialAccountingPending {
			return fmt.Errorf("task initial accounting is pending")
		}
		if stored.Quota < 0 || stored.Quota > math.MaxInt32 || stored.Quota != task.Quota {
			return fmt.Errorf("video task reserved quota changed")
		}
		delta = actualQuota - stored.Quota
		channelDelta := delta
		if stored.PrivateData.DiscountAmounts.ValidFor(stored.Quota) && task.PrivateData.DiscountAmounts.ValidFor(actualQuota) {
			channelDelta = task.PrivateData.DiscountAmounts.Before - stored.PrivateData.DiscountAmounts.Before
		}
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
				periodStart := stored.PrivateData.SubscriptionPeriodStart
				if periodStart == 0 {
					periodStart = stored.SubmitTime
				}
				if err := postConsumeUserSubscriptionDeltaTx(tx, stored.PrivateData.SubscriptionId, int64(delta), periodStart, stored.UserId); err != nil {
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
		}
		// 渠道统计使用折前差额，折后金额相同也可能需要调整。
		if stored.ChannelId > 0 && channelDelta != 0 {
			if err := tx.Model(&Channel{}).Where("id = ?", stored.ChannelId).Update("used_quota", gorm.Expr("used_quota + ?", channelDelta)).Error; err != nil {
				return err
			}
		}
		// 复制后更新，回滚不能把调用方预扣快照误改成已结算额度。
		final = *task
		final.Quota = actualQuota
		final.PrivateData.InitialAccountingPending = stored.PrivateData.InitialAccountingPending
		final.PrivateData.SubmissionFundsCommitted = stored.PrivateData.SubmissionFundsCommitted
		final.PrivateData.ResponsesBackground = stored.PrivateData.ResponsesBackground || final.PrivateData.ResponsesBackground
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
	*task = final
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

// CommitTaskInitialAccounting 将初始统计和待入账标记一起提交，恢复或重放不会重复增加用量。
func CommitTaskInitialAccounting(ctx context.Context, task *Task, initialLogs ...*Log) (bool, error) {
	var stored Task
	updated := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.ID, task.UserId).First(&stored).Error; err != nil {
			return err
		}
		if stored.PrivateData.InitialAccountingPending && (stored.Status == TaskStatusSuccess || stored.Status == TaskStatusFailure) && !stored.PrivateData.SubmissionFundsCommitted {
			return fmt.Errorf("terminal task submission funding is unconfirmed")
		}
		if stored.PrivateData.SubmissionFunding != nil {
			return fmt.Errorf("task submission funding handoff is pending")
		}
		if stored.PrivateData.InitialAccountingPending {
			if task.Quota != stored.Quota || task.ChannelId != stored.ChannelId || !sameTaskAmounts(task.PrivateData.DiscountAmounts, stored.PrivateData.DiscountAmounts) {
				return fmt.Errorf("task initial accounting snapshot changed; retry with current task")
			}
			if len(initialLogs) > 0 && initialLogs[0] != nil && (initialLogs[0].UserId != stored.UserId || initialLogs[0].Quota != stored.Quota) {
				return fmt.Errorf("task initial accounting log does not match current task")
			}
		}
		// 同事务消费恢复标记；失败回滚保留入口，独立日志库随后写入完整日志。
		if err := tx.Where("task_id = ? AND kind = ?", -stored.ID, videoLogKindAccounting).Delete(&VideoTaskPendingLog{}).Error; err != nil {
			return err
		}
		if !stored.PrivateData.InitialAccountingPending {
			return nil
		}
		if stored.Quota < 0 || stored.Quota > common.MaxQuota {
			return fmt.Errorf("invalid initial task quota")
		}
		userUpdate := tx.Model(&User{}).Where("id = ?", stored.UserId).Updates(map[string]any{"used_quota": gorm.Expr("used_quota + ?", stored.Quota), "request_count": gorm.Expr("request_count + ?", 1)})
		if userUpdate.Error != nil {
			return userUpdate.Error
		}
		if userUpdate.RowsAffected != 1 {
			return fmt.Errorf("initial task accounting user is unavailable")
		}
		before := stored.PrivateData.DiscountAmounts.ChannelQuota(stored.Quota)
		if err := tx.Model(&Channel{}).Where("id = ?", stored.ChannelId).Update("used_quota", gorm.Expr("used_quota + ?", before)).Error; err != nil {
			return err
		}
		if len(initialLogs) > 0 && initialLogs[0] != nil {
			log := initialLogs[0]
			if LOG_DB == DB {
				if err := tx.Create(log).Error; err != nil {
					return err
				}
			} else {
				payload, err := common.Marshal(log)
				if err != nil {
					return err
				}
				if err := tx.Create(&VideoTaskPendingLog{TaskID: -stored.ID, Payload: string(payload)}).Error; err != nil {
					return err
				}
			}
		}
		stored.PrivateData.InitialAccountingPending = false
		if err := tx.Model(&Task{}).Where("id = ?", stored.ID).Update("private_data", stored.PrivateData).Error; err != nil {
			return err
		}
		updated = true
		return nil
	})
	if err == nil {
		task.PrivateData.InitialAccountingPending = stored.PrivateData.InitialAccountingPending
	}
	return updated, err
}

// TaskSubmissionFunding 指定提交交接时已预扣的额度和令牌调整范围。
type TaskSubmissionFunding struct {
	ReservedQuota int  `json:"reserved_quota"`
	UpdateToken   bool `json:"update_token"`
}

// ImmediateTaskFunding 保持已有调用兼容。
type ImmediateTaskFunding = TaskSubmissionFunding

// ReplaceTaskSubmissionIntent 在重试渠道前原子替换已拒绝的意图，不能先删除唯一的预扣恢复记录。
func ReplaceTaskSubmissionIntent(ctx context.Context, previous, next *Task) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var stored Task
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", previous.ID, previous.UserId).First(&stored).Error; err != nil {
			return err
		}
		if stored.Status != TaskStatusUnknown || !stored.PrivateData.SubmissionPending || stored.TaskID != next.TaskID || stored.UserId != next.UserId {
			return fmt.Errorf("task submission intent is no longer replaceable")
		}
		next.ID, next.CreatedAt = stored.ID, stored.CreatedAt
		next.PrivateData.SubmissionFunding = stored.PrivateData.SubmissionFunding
		if err := tx.Delete(&stored).Error; err != nil {
			return err
		}
		return tx.Create(next).Error
	})
}

// PersistTaskSubmission 交接提交结果时保留数据库中的入账标记，旧请求快照不能重新打开已完成的初始入账。
func PersistTaskSubmission(ctx context.Context, task *Task, complete bool, funding ...TaskSubmissionFunding) (bool, error) {
	var final Task
	won := false
	var delta int
	var tokenKey string
	var wallet bool
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var stored Task
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.ID, task.UserId).First(&stored).Error; err != nil {
			return err
		}
		if stored.Status != TaskStatusUnknown || !stored.PrivateData.SubmissionPending {
			return nil
		}
		if complete {
			if task.Quota < 0 || task.Quota > common.MaxQuota || task.ChannelId != stored.ChannelId {
				return fmt.Errorf("invalid task submission handoff")
			}
			final = *task
			final.CreatedAt, final.TaskID = stored.CreatedAt, stored.TaskID
			final.PrivateData.InitialAccountingPending = stored.PrivateData.InitialAccountingPending
			submissionFunding := stored.PrivateData.SubmissionFunding
			if len(funding) > 0 {
				if submissionFunding != nil && *submissionFunding != funding[0] {
					return fmt.Errorf("task submission reservation changed")
				}
				submissionFunding = &funding[0]
			}
			if submissionFunding != nil {
				reserved := submissionFunding.ReservedQuota
				if reserved < final.Quota || reserved > common.MaxQuota {
					return fmt.Errorf("invalid task submission reservation")
				}
				delta = final.Quota - reserved
				if delta != 0 {
					var err error
					wallet, tokenKey, err = refundTaskSubmissionFunding(ctx, tx, &stored, reserved, final.Quota, submissionFunding.UpdateToken)
					if err != nil {
						return err
					}
				}
				final.PrivateData.SubmissionFundsCommitted = true
			}
			final.PrivateData.SubmissionFunding = nil
			if final.PrivateData.InitialAccountingPending && final.PrivateData.SubmissionFundsCommitted && (final.Status == TaskStatusSuccess || final.Status == TaskStatusFailure) {
				if err := queueTaskInitialAccounting(tx, final.ID); err != nil {
					return err
				}
			}
			if !stored.PrivateData.InitialAccountingPending {
				delta := final.Quota - stored.Quota
				if delta != 0 {
					if err := tx.Model(&User{}).Where("id = ?", stored.UserId).Update("used_quota", gorm.Expr("used_quota + ?", delta)).Error; err != nil {
						return err
					}
				}
				beforeDelta := final.PrivateData.DiscountAmounts.ChannelQuota(final.Quota) - stored.PrivateData.DiscountAmounts.ChannelQuota(stored.Quota)
				if beforeDelta != 0 {
					if err := tx.Model(&Channel{}).Where("id = ?", stored.ChannelId).Update("used_quota", gorm.Expr("used_quota + ?", beforeDelta)).Error; err != nil {
						return err
					}
				}
			}
		} else {
			final = stored
			if funding := stored.PrivateData.SubmissionFunding; funding != nil {
				if funding.ReservedQuota < stored.Quota || funding.ReservedQuota > common.MaxQuota || stored.Quota < 0 {
					return fmt.Errorf("invalid task submission reservation")
				}
				delta = stored.Quota - funding.ReservedQuota
				if delta != 0 {
					var err error
					wallet, tokenKey, err = refundTaskSubmissionFunding(ctx, tx, &stored, funding.ReservedQuota, stored.Quota, funding.UpdateToken)
					if err != nil {
						return err
					}
				}
				final.PrivateData.SubmissionFunding = nil
				final.PrivateData.SubmissionFundsCommitted = true
			}
			final.PrivateData.SubmissionResponse, final.PrivateData.SubmissionContext = task.PrivateData.SubmissionResponse, task.PrivateData.SubmissionContext
			final.PrivateData.ReconciliationRequired, final.PrivateData.ReconciliationReason, final.PrivateData.NextPollAt = true, task.PrivateData.ReconciliationReason, 0
		}
		if err := tx.Model(&Task{}).Where("id = ?", stored.ID).Select("*").Updates(&final).Error; err != nil {
			return err
		}
		if err := saveTaskResourceIndex(tx, &final); err != nil {
			return err
		}
		won = true
		return nil
	})
	if err == nil && won {
		*task = final
		if delta != 0 && common.RedisEnabled {
			flag, cacheKey := "0", ""
			if wallet {
				flag = "1"
			}
			if tokenKey != "" {
				cacheKey = getTokenCacheKey(tokenKey)
			}
			if cacheErr := applyVideoQuotaCache(ctx, task.UserId, task.ID, -delta, flag, cacheKey); cacheErr != nil {
				common.SysError("task submission cache synchronization pending: " + cacheErr.Error())
			}
		}
	}
	return won, err
}

// refundTaskSubmissionFunding 与提交交接同事务退还预扣差额，统计仍由初始记账负责。
func refundTaskSubmissionFunding(ctx context.Context, tx *gorm.DB, task *Task, reserved, actual int, updateToken bool) (bool, string, error) {
	delta := actual - reserved
	wallet := task.PrivateData.BillingSource != "subscription" || task.PrivateData.SubscriptionId <= 0
	if wallet {
		result := tx.Model(&User{}).Where("id = ? AND quota <= ?", task.UserId, common.MaxWalletQuota+int64(delta)).
			Update("quota", gorm.Expr("quota - ?", delta))
		if result.Error != nil {
			return false, "", result.Error
		}
		if result.RowsAffected != 1 {
			return false, "", fmt.Errorf("immediate task wallet unavailable or quota limit exceeded")
		}
	} else {
		periodStart := task.PrivateData.SubscriptionPeriodStart
		if periodStart == 0 {
			periodStart = task.SubmitTime
		}
		if err := postConsumeUserSubscriptionDeltaTx(tx, task.PrivateData.SubscriptionId, int64(delta), periodStart, task.UserId); err != nil {
			return false, "", err
		}
		var user User
		if err := lockForUpdate(tx).Select("id").Where("id = ?", task.UserId).First(&user).Error; err != nil {
			return false, "", err
		}
	}
	var tokenKey string
	if updateToken && task.PrivateData.TokenId > 0 {
		var token Token
		err := tx.Where("id = ? AND user_id = ?", task.PrivateData.TokenId, task.UserId).First(&token).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, "", err
		}
		if err == nil {
			tokenKey = token.Key
			if err := tx.Model(&token).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", delta), "used_quota": gorm.Expr("used_quota + ?", delta), "accessed_time": common.GetTimestamp()}).Error; err != nil {
				return false, "", err
			}
		}
	}
	cacheTask := *task
	cacheTask.Quota = reserved
	if err := prepareVideoQuotaCache(ctx, tx, &cacheTask, actual, wallet, tokenKey); err != nil {
		return false, "", err
	}
	return wallet, tokenKey, nil
}

// ConfirmTaskSubmissionFunds 在既有结算成功后开放终态任务的初始记账重试。
func ConfirmTaskSubmissionFunds(ctx context.Context, task *Task) error {
	var confirmed bool
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var stored Task
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", task.ID, task.UserId).First(&stored).Error; err != nil {
			return err
		}
		if !stored.PrivateData.InitialAccountingPending {
			return nil
		}
		if stored.Status != TaskStatusSuccess && stored.Status != TaskStatusFailure {
			return fmt.Errorf("task submission is not terminal")
		}
		stored.PrivateData.SubmissionFundsCommitted = true
		if err := tx.Model(&Task{}).Where("id = ?", stored.ID).Update("private_data", stored.PrivateData).Error; err != nil {
			return err
		}
		if err := queueTaskInitialAccounting(tx, stored.ID); err != nil {
			return err
		}
		confirmed = true
		return nil
	})
	if err == nil && confirmed {
		task.PrivateData.SubmissionFundsCommitted = true
	}
	return err
}
