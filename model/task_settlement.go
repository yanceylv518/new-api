package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

// sameTaskAmounts 比较包含空值语义的金额快照，折后相同但折前不同也属于不同结算版本。
func sameTaskAmounts(a, b *types.DiscountAmounts) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// CommitTaskSettlement 以持久化任务金额作为重试凭据，原子提交资金、用量和任务快照。
// 返回真正提交的折后差额；目标已完成时返回零。提交结果未知时可用同一目标重试，不能反向补偿猜测。
func CommitTaskSettlement(ctx context.Context, task *Task, target int, amounts *types.DiscountAmounts) (int, error) {
	result, err := CommitTaskSettlementWithSnapshot(ctx, task, target, amounts, nil)
	return result.QuotaDelta, err
}

// TaskSettlementResult 区分真实更新与同目标重试；仅元数据变化也需要记录最终结算日志。
type TaskSettlementResult struct {
	QuotaDelta int
	Updated    bool
}

// CommitTaskSettlementWithSnapshot 将最终用量、命中档位与金额一并提交。
// snapshot 只提供最终用量和档位，冻结的价格参数取自 task；旧版本只能重试同一完整目标。
// 返回成功后才同步调用方快照，失败不留下看似已结算的内存状态。
func CommitTaskSettlementWithSnapshot(ctx context.Context, task *Task, target int, amounts *types.DiscountAmounts, snapshot *billingexpr.BillingSnapshot) (TaskSettlementResult, error) {
	if task == nil || task.ID == 0 || target < 0 || target > common.MaxQuota || (amounts != nil && !amounts.ValidFor(target)) {
		return TaskSettlementResult{}, errors.New("invalid persisted task settlement")
	}
	var expectedBilling, targetBilling []byte
	var settledBilling TaskBillingContext
	if snapshot != nil {
		bc := task.PrivateData.BillingContext
		if bc == nil || bc.TieredSnapshot == nil {
			return TaskSettlementResult{}, errors.New("missing task billing snapshot")
		}
		var err error
		expectedBilling, err = common.Marshal(bc)
		if err != nil {
			return TaskSettlementResult{}, fmt.Errorf("encode expected task billing: %w", err)
		}
		// 不修改共享快照；JSON 副本隔离用量 map，并统一重载后数字的比较语义。
		settledBilling = *bc
		settledSnapshot := *bc.TieredSnapshot
		settledSnapshot.UsageFacts = snapshot.UsageFacts
		settledSnapshot.EstimatedTier = snapshot.EstimatedTier
		settledBilling.TieredSnapshot = &settledSnapshot
		targetBilling, err = common.Marshal(&settledBilling)
		if err != nil {
			return TaskSettlementResult{}, fmt.Errorf("encode settled task billing: %w", err)
		}
		settledBilling = TaskBillingContext{}
		if err := common.Unmarshal(targetBilling, &settledBilling); err != nil {
			return TaskSettlementResult{}, fmt.Errorf("decode settled task billing: %w", err)
		}
	}
	var committed Task
	var tokenKey string
	delta := 0
	updated := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// SQLite 先获取写锁；MySQL/PostgreSQL 使用统一行锁，所有结算都先锁任务。
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&Task{}).Where("id = ?", task.ID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).First(&committed, task.ID).Error; err != nil {
			return err
		}
		if committed.Quota < 0 || committed.Quota > common.MaxQuota {
			return errors.New("invalid persisted task quota")
		}
		if committed.UserId != task.UserId || committed.ChannelId != task.ChannelId {
			return errors.New("task accounting identity changed")
		}
		sameAmounts := sameTaskAmounts(amounts, committed.PrivateData.DiscountAmounts)
		expectedState := committed.Quota == task.Quota && sameTaskAmounts(task.PrivateData.DiscountAmounts, committed.PrivateData.DiscountAmounts)
		targetState := committed.Quota == target && sameAmounts
		if snapshot != nil {
			persistedBilling, err := common.Marshal(committed.PrivateData.BillingContext)
			if err != nil {
				return fmt.Errorf("encode persisted task billing: %w", err)
			}
			// 金额和用量共同构成结算版本，避免金额相同的旧用量覆盖新结果。
			expectedState = expectedState && bytes.Equal(persistedBilling, expectedBilling)
			targetState = targetState && bytes.Equal(persistedBilling, targetBilling)
		}
		if !expectedState && !targetState {
			return errors.New("task settlement changed; reload before retry")
		}
		delta = target - committed.Quota
		channelDelta := delta
		if committed.PrivateData.DiscountAmounts.ValidFor(committed.Quota) && amounts.ValidFor(target) {
			channelDelta = amounts.Before - committed.PrivateData.DiscountAmounts.Before
		}
		// 已持久化同一目标不再触碰资金和统计，也不生成第二笔日志。
		if targetState {
			return nil
		}
		if delta != 0 {
			var user User
			if err := lockForUpdate(tx).First(&user, committed.UserId).Error; err != nil {
				return err
			}
			if committed.PrivateData.BillingSource == "subscription" && committed.PrivateData.SubscriptionId > 0 {
				var sub UserSubscription
				if err := lockForUpdate(tx).First(&sub, committed.PrivateData.SubscriptionId).Error; err != nil {
					return err
				}
				// 冻结的资金来源仍须属于任务用户；损坏记录不能导致跨用户扣费或整数溢出。
				if sub.UserId != committed.UserId || sub.AmountUsed < 0 || (delta > 0 && sub.AmountUsed > math.MaxInt64-int64(delta)) {
					return errors.New("invalid task subscription accounting")
				}
				used := sub.AmountUsed + int64(delta)
				if used < 0 {
					used = 0
				}
				if sub.AmountTotal > 0 && used > sub.AmountTotal {
					return fmt.Errorf("subscription used exceeds total: %d", used)
				}
				if err := tx.Model(&sub).Update("amount_used", used).Error; err != nil {
					return err
				}
			} else {
				// 实际用量补扣延续允许欠费的原契约；退款不能突破钱包上限。
				if delta < 0 && user.Quota > common.MaxWalletQuota+delta {
					return ErrWalletQuotaLimitExceeded
				}
				if err := tx.Model(&user).Update("quota", gorm.Expr("quota - ?", delta)).Error; err != nil {
					return err
				}
			}
			if committed.PrivateData.TokenId > 0 {
				var token Token
				err := lockForUpdate(tx).First(&token, committed.PrivateData.TokenId).Error
				// 令牌删除不妨碍用户获得退款；查询故障不能当成令牌不存在。
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil {
					if token.UserId != committed.UserId {
						return errors.New("task token belongs to another user")
					}
					tokenKey = token.Key
					if err := tx.Model(&token).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", delta), "used_quota": gorm.Expr("used_quota + ?", delta), "accessed_time": common.GetTimestamp()}).Error; err != nil {
						return err
					}
				}
			}
			if err := tx.Model(&user).Update("used_quota", gorm.Expr("used_quota + ?", delta)).Error; err != nil {
				return err
			}
		}
		if committed.ChannelId > 0 && channelDelta != 0 {
			if err := tx.Model(&Channel{}).Where("id = ?", committed.ChannelId).Update("used_quota", gorm.Expr("used_quota + ?", channelDelta)).Error; err != nil {
				return err
			}
		}
		committed.Quota = target
		committed.PrivateData.DiscountAmounts = amounts
		if snapshot != nil {
			committed.PrivateData.BillingContext = &settledBilling
		}
		if err := tx.Model(&Task{}).Where("id = ?", committed.ID).Updates(map[string]any{"quota": target, "private_data": committed.PrivateData}).Error; err != nil {
			return err
		}
		updated = true
		return nil
	})
	if err != nil {
		return TaskSettlementResult{}, err
	}
	// 普通请求恢复缓存预扣和批量落库后，不能删除缓存再从尚未刷盘的数据库水合。
	// 只对本次确实提交的差额做守卫式缓存增量；同一目标重试的 delta 为零，不重复调整。
	if common.RedisEnabled && delta != 0 {
		if committed.PrivateData.BillingSource != "subscription" || committed.PrivateData.SubscriptionId <= 0 {
			if err := cacheIncrUserQuota(task.UserId, -int64(delta)); err != nil {
				common.SysError("task settlement wallet cache update failed: " + err.Error())
			}
		}
		if tokenKey != "" {
			if _, err := cacheApplyTokenQuotaDelta(committed.PrivateData.TokenId, tokenKey, -int64(delta)); err != nil {
				common.SysError("task settlement token cache update failed: " + err.Error())
			}
		}
	}
	task.Quota = committed.Quota
	task.PrivateData.DiscountAmounts = committed.PrivateData.DiscountAmounts
	task.PrivateData.BillingContext = committed.PrivateData.BillingContext
	return TaskSettlementResult{QuotaDelta: delta, Updated: updated}, nil
}
