package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const billingRefundRetention = 7 * 24 * time.Hour
const billingRefundCleanupBatchSize = 1000
const billingRecoveryRecordTimeout = 500 * time.Millisecond
const billingRecoveryBackoffTimeout = 150 * time.Millisecond

var billingRefundCursor atomic.Int64
var billingSettlementCursor atomic.Int64

// BillingRefundInFlight 使旧版退款调度器也不会将处理中请求当作失败。
const BillingRefundInFlight int64 = math.MaxInt64

var ErrBillingRefundStateConflict = errors.New("billing refund state changed")

// BillingRefund 保存提交失败的退款交接，资金与完成标记在同一事务中更新。
type BillingRefund struct {
	ID                int64  `gorm:"primaryKey"`
	RequestID         string `gorm:"type:varchar(64);uniqueIndex"`
	UserID            int
	TokenID           int
	Amount            int
	TokenAmount       int
	TokenTracked      bool   `gorm:"not null;default:false"`
	ReserveOperation  string `gorm:"type:varchar(32);not null;default:''"`
	SubscriptionID    int
	PeriodStart       int64
	Completed         bool `gorm:"index:idx_billing_refund_pending,priority:1;index:idx_billing_refund_cleanup,priority:1"`
	DecisionKnown     bool `gorm:"not null;default:false"`
	Settled           bool `gorm:"not null;default:false"`
	SettledAmount     int  `gorm:"not null;default:0"`
	SettlementPending bool `gorm:"not null;default:false;index:idx_billing_settlement_due,priority:1"`
	// 未进入结算时，负值表示尚未交接任务预扣的租约截止时间。
	SettlementRetryAt int64 `gorm:"not null;default:0;index:idx_billing_settlement_due,priority:2"`
	CachePending      bool  `gorm:"index:idx_billing_refund_cache,priority:1"`
	NextAttemptAt     int64 `gorm:"not null;default:0;index:idx_billing_refund_pending,priority:2"`
	Attempts          int
	CreatedAt         int64 `gorm:"index:idx_billing_refund_cleanup,priority:2"`
	CompletedAt       int64 `gorm:"not null;default:0"`
}

func QueueBillingRefund(ctx context.Context, refund *BillingRefund) error {
	return queueBillingRefund(DB.WithContext(ctx), refund)
}

func queueBillingRefund(tx *gorm.DB, refund *BillingRefund, requireNew ...bool) error {
	if refund.RequestID == "" || len(refund.RequestID) > 64 || refund.UserID <= 0 || refund.Amount < 0 || refund.Amount > common.MaxQuota || refund.TokenAmount < 0 || refund.TokenAmount > common.MaxQuota || refund.TokenAmount > 0 && refund.TokenAmount != refund.Amount {
		return errors.New("invalid billing refund")
	}
	refund.CreatedAt = common.GetTimestamp()
	refund.DecisionKnown = refund.NextAttemptAt != BillingRefundInFlight
	create := tx
	if len(requireNew) == 0 || !requireNew[0] {
		create = create.Clauses(clause.OnConflict{DoNothing: true})
	}
	if err := create.Create(refund).Error; err != nil {
		return err
	}
	var stored BillingRefund
	if err := tx.Where("request_id = ?", refund.RequestID).First(&stored).Error; err != nil {
		return err
	}
	if stored.UserID != refund.UserID || stored.TokenID != refund.TokenID || stored.Amount != refund.Amount || stored.TokenAmount != refund.TokenAmount || stored.TokenTracked != refund.TokenTracked || stored.SubscriptionID != refund.SubscriptionID || stored.PeriodStart != refund.PeriodStart {
		return errors.New("billing refund reservation changed")
	}
	*refund = stored
	return nil
}

// StageBillingTask 把预扣恢复记录与任务意图原子交接。
func StageBillingTask(ctx context.Context, task *Task, refundID int64) error {
	transactionReady := false
	err := quotaCacheTransaction(DB.WithContext(ctx), func(tx *gorm.DB) error {
		var refund BillingRefund
		if refundID > 0 {
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", refundID, task.UserId).First(&refund).Error; err != nil {
				return err
			}
			reserved := task.Quota
			if task.PrivateData.SubmissionFunding != nil {
				reserved = task.PrivateData.SubmissionFunding.ReservedQuota
			}
			if refund.Completed || refund.SettlementPending || refund.DecisionKnown || refund.NextAttemptAt != BillingRefundInFlight || refund.Amount != reserved || refund.TokenID != task.PrivateData.TokenId || refund.SubscriptionID != task.PrivateData.SubscriptionId || refund.PeriodStart != task.PrivateData.SubscriptionPeriodStart || task.PrivateData.SubmissionFunding == nil || (refund.TokenTracked || refund.TokenAmount > 0) != task.PrivateData.SubmissionFunding.UpdateToken {
				return errors.New("task reservation is no longer available")
			}
			if refund.SettlementRetryAt < 0 {
				now, err := taskSubmissionDatabaseTime(tx)
				if err != nil {
					return err
				}
				if refund.SettlementRetryAt >= -now {
					return errors.New("task reservation has expired")
				}
			}
		}
		var user User
		if err := lockForUpdate(tx).Select("id", "status").Where("id = ?", task.UserId).First(&user).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return errors.New("task owner is unavailable")
		}
		if common.RedisEnabled {
			if err := finishVideoQuotaCache(ctx, tx, task.UserId); err != nil {
				return err
			}
		}
		if refundID > 0 {
			if err := tx.Delete(&refund).Error; err != nil {
				return err
			}
		}
		if task.PrivateData.SubmissionPrepared {
			now, err := taskSubmissionDatabaseTime(tx)
			if err != nil {
				return err
			}
			task.PrivateData.NextPollAt = now + taskSubmissionLeaseSeconds
		}
		if err := tx.Create(task).Error; err != nil {
			return err
		}
		transactionReady = true
		return nil
	})
	if err == nil || !transactionReady {
		return err
	}
	// 提交响应丢失时，以数据库中的任务身份确认交接是否已经完成。
	checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stored Task
	checkErr := DB.WithContext(checkCtx).Where("id = ? AND task_id = ? AND user_id = ?", task.ID, task.TaskID, task.UserId).First(&stored).Error
	storedFunding, requestedFunding := stored.PrivateData.SubmissionFunding, task.PrivateData.SubmissionFunding
	sameFunding := storedFunding == nil && requestedFunding == nil || storedFunding != nil && requestedFunding != nil && storedFunding.ReservedQuota == requestedFunding.ReservedQuota && storedFunding.UpdateToken == requestedFunding.UpdateToken
	if checkErr == nil && stored.Status == TaskStatusUnknown && stored.Quota == task.Quota && stored.ChannelId == task.ChannelId && stored.PrivateData.SubmissionPending && stored.PrivateData.SubmissionAttemptID == task.PrivateData.SubmissionAttemptID && stored.PrivateData.SubmissionPrepared == task.PrivateData.SubmissionPrepared && stored.PrivateData.DiscardImmediateResult == task.PrivateData.DiscardImmediateResult && stored.PrivateData.BillingSource == task.PrivateData.BillingSource && stored.PrivateData.TokenId == task.PrivateData.TokenId && stored.PrivateData.SubscriptionId == task.PrivateData.SubscriptionId && stored.PrivateData.SubscriptionPeriodStart == task.PrivateData.SubscriptionPeriodStart && sameFunding {
		return nil
	}
	return err
}

// MarkBillingSettlementReady 持久化已知的最终额度，便于后台恢复结算。
func MarkBillingSettlementReady(ctx context.Context, id int64, userID, reserved, actual int) error {
	if id <= 0 || userID <= 0 || reserved < 0 || reserved > common.MaxQuota || actual < 0 || actual > common.MaxQuota {
		return errors.New("invalid billing settlement")
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var refund BillingRefund
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", id, userID).First(&refund).Error; err != nil {
			return err
		}
		if refund.Amount != reserved {
			return ErrBillingRefundStateConflict
		}
		if refund.Completed {
			if !refund.Settled || refund.SettledAmount != actual {
				return ErrBillingRefundStateConflict
			}
			return nil
		}
		if refund.NextAttemptAt != BillingRefundInFlight {
			return ErrBillingRefundStateConflict
		}
		if refund.SettlementPending {
			if refund.SettledAmount != actual {
				return ErrBillingRefundStateConflict
			}
			return nil
		}
		return tx.Model(&refund).Updates(map[string]any{
			"settlement_pending": true, "settled_amount": actual, "settlement_retry_at": common.GetTimestamp(),
		}).Error
	})
}

// SettleBillingReservation 将普通请求的预扣差额和结算状态原子提交。
func SettleBillingReservation(ctx context.Context, id int64, userID, reserved, actual int) error {
	if id <= 0 || userID <= 0 || reserved < 0 || reserved > common.MaxQuota || actual < 0 || actual > common.MaxQuota {
		return errors.New("invalid billing settlement")
	}
	var refund BillingRefund
	var tokenKey string
	var cachePrepared bool
	var committed bool
	err := quotaCacheTransaction(DB.WithContext(ctx), func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", id, userID).First(&refund).Error; err != nil {
			return err
		}
		if refund.Amount != reserved {
			return ErrBillingRefundStateConflict
		}
		if refund.Completed {
			if !refund.Settled || refund.SettledAmount != actual {
				return ErrBillingRefundStateConflict
			}
			return nil
		}
		if refund.NextAttemptAt != BillingRefundInFlight || !refund.SettlementPending || refund.SettledAmount != actual {
			return ErrBillingRefundStateConflict
		}
		delta := actual - reserved
		wallet := refund.SubscriptionID == 0
		if !wallet && delta != 0 {
			if err := postConsumeUserSubscriptionDeltaTx(tx, refund.SubscriptionID, int64(delta), refund.PeriodStart, userID); err != nil {
				return err
			}
		}
		var user User
		if err := lockForUpdate(tx).Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		if common.RedisEnabled {
			if err := finishVideoQuotaCache(ctx, tx, userID); err != nil {
				return err
			}
		}
		if wallet && delta != 0 {
			query := tx.Model(&User{}).Where("id = ?", userID)
			if delta < 0 {
				query = query.Where("quota <= ?", common.MaxWalletQuota+int64(delta))
			} else {
				query = query.Where("quota >= ?", -common.MaxWalletQuota+int64(delta))
			}
			result := query.Update("quota", gorm.Expr("quota - ?", delta))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("billing wallet unavailable or quota limit exceeded")
			}
		}
		if (refund.TokenTracked || refund.TokenAmount > 0) && refund.TokenID > 0 {
			var token Token
			err := lockForUpdate(tx).Where("id = ? AND user_id = ?", refund.TokenID, userID).First(&token).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil {
				tokenKey = token.Key
				if delta != 0 {
					if err := tx.Model(&token).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", delta), "used_quota": gorm.Expr("used_quota + ?", delta), "accessed_time": common.GetTimestamp()}).Error; err != nil {
						return err
					}
				}
			}
		}
		cachePrepared = common.RedisEnabled && delta != 0 && (wallet || tokenKey != "")
		if cachePrepared {
			cacheTask := &Task{ID: refund.ID, UserId: userID, Quota: reserved}
			if err := prepareQuotaCache(ctx, tx, cacheTask, actual, wallet, tokenKey, "refund"); err != nil {
				return err
			}
		}
		if err := tx.Model(&refund).Updates(map[string]any{
			"completed": true, "settled": true, "settled_amount": actual,
			"settlement_pending": false, "settlement_retry_at": 0,
			"cache_pending": cachePrepared, "next_attempt_at": common.GetTimestamp(), "completed_at": common.GetTimestamp(),
		}).Error; err != nil {
			return err
		}
		refund.Completed, refund.Settled, refund.SettledAmount, refund.CachePending = true, true, actual, cachePrepared
		committed = true
		return nil
	})
	if err != nil || !common.RedisEnabled || !refund.CachePending {
		return err
	}
	if committed {
		wallet, cacheKey := "0", ""
		if refund.SubscriptionID == 0 {
			wallet = "1"
		}
		if tokenKey != "" {
			cacheKey = getTokenCacheKey(tokenKey)
		}
		err = applyVideoQuotaCache(ctx, userID, refund.ID, reserved-actual, wallet, cacheKey, "refund")
	} else {
		err = recoverVideoQuotaCache(ctx, userID)
	}
	if err != nil {
		common.SysError("billing settlement cache synchronization pending: " + err.Error())
		return nil
	}
	return DB.WithContext(ctx).Model(&BillingRefund{}).Where("id = ? AND completed = ? AND settled = ?", refund.ID, true, true).Update("cache_pending", false).Error
}

// MarkBillingRefundReady 将确认失败的预扣交给退款调度器。
func MarkBillingRefundReady(ctx context.Context, id int64, userID int) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var refund BillingRefund
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", id, userID).First(&refund).Error; err != nil {
			return err
		}
		if refund.Completed {
			if refund.Settled {
				return ErrBillingRefundStateConflict
			}
			return nil
		}
		if refund.SettlementPending {
			return ErrBillingRefundStateConflict
		}
		return tx.Model(&refund).Updates(map[string]any{"decision_known": true, "next_attempt_at": common.GetTimestamp(), "settlement_retry_at": 0}).Error
	})
}

// markExpiredTaskReservation 只恢复租约到期且尚未交接的任务预扣。
func markExpiredTaskReservation(ctx context.Context, id int64) (bool, error) {
	ready := false
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var refund BillingRefund
		if err := lockForUpdate(tx).Where("id = ?", id).First(&refund).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if refund.Completed || refund.SettlementPending || refund.DecisionKnown || refund.NextAttemptAt != BillingRefundInFlight || refund.SettlementRetryAt >= 0 {
			return nil
		}
		now, err := taskSubmissionDatabaseTime(tx)
		if err != nil {
			return err
		}
		if refund.SettlementRetryAt < -now {
			return nil
		}
		if err := tx.Model(&refund).Updates(map[string]any{"decision_known": true, "next_attempt_at": now, "settlement_retry_at": 0}).Error; err != nil {
			return err
		}
		ready = true
		return nil
	})
	return ready && err == nil, err
}

func CompleteBillingRefund(ctx context.Context, id int64) error {
	var refund BillingRefund
	var tokenKey string
	var completed bool
	err := quotaCacheTransaction(DB.WithContext(ctx), func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("id = ?", id).First(&refund).Error; err != nil {
			return err
		}
		if refund.Completed {
			return nil
		}
		if !refund.DecisionKnown || refund.NextAttemptAt == BillingRefundInFlight || refund.SettlementPending {
			return ErrBillingRefundStateConflict
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
		if err := tx.Model(&refund).Updates(map[string]any{"completed": true, "cache_pending": common.RedisEnabled, "completed_at": common.GetTimestamp()}).Error; err != nil {
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

func dueBillingRecords(ctx context.Context, condition string, args []any, cursor *atomic.Int64) ([]BillingRefund, error) {
	load := func(after int64) ([]BillingRefund, error) {
		var records []BillingRefund
		query := DB.WithContext(ctx).Where(condition, args...)
		if after > 0 {
			query = query.Where("id > ?", after)
		}
		err := query.Order("id ASC").Limit(100).Find(&records).Error
		return records, err
	}
	after := cursor.Load()
	records, err := load(after)
	if err == nil && after > 0 && len(records) == 0 {
		cursor.CompareAndSwap(after, 0)
		return load(0)
	}
	return records, err
}

func RetryBillingRefunds(ctx context.Context) error {
	now := common.GetTimestamp()
	condition := "completed = ? AND settlement_pending = ? AND ((decision_known = ? AND next_attempt_at <= ?) OR (decision_known = ? AND next_attempt_at = ? AND settlement_retry_at < 0 AND settlement_retry_at >= ?))"
	args := []any{false, false, true, now, false, BillingRefundInFlight, -now}
	if common.RedisEnabled {
		condition = "((completed = ? AND settlement_pending = ? AND ((decision_known = ? AND next_attempt_at <= ?) OR (decision_known = ? AND next_attempt_at = ? AND settlement_retry_at < 0 AND settlement_retry_at >= ?))) OR (completed = ? AND cache_pending = ? AND next_attempt_at <= ?))"
		args = append(args, true, true, now)
	}
	pending, err := dueBillingRecords(ctx, condition, args, &billingRefundCursor)
	if err != nil {
		return err
	}
	settlements, err := dueBillingRecords(ctx, "completed = ? AND settlement_pending = ? AND settlement_retry_at <= ?", []any{false, true, now}, &billingSettlementCursor)
	if err != nil {
		return err
	}
	var result error
	// 清理限时执行，既能定期处理到期流水，也不会阻塞后续退款。
	cleanup, cancel := context.WithTimeout(ctx, billingRecoveryRecordTimeout)
	cutoff := time.Now().Add(-billingRefundRetention).Unix()
	for cleanup.Err() == nil {
		var expired []int64
		if err := DB.WithContext(cleanup).Model(&BillingRefund{}).Where("completed = ? AND cache_pending = ? AND ((completed_at > 0 AND completed_at < ?) OR (completed_at = 0 AND created_at < ?))", true, false, cutoff, cutoff).Order("created_at ASC, id ASC").Limit(billingRefundCleanupBatchSize).Pluck("id", &expired).Error; err != nil {
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				result = errors.Join(result, err)
			}
			break
		}
		if len(expired) == 0 {
			break
		}
		if err := DB.WithContext(cleanup).Where("id IN ? AND completed = ? AND cache_pending = ?", expired, true, false).Delete(&BillingRefund{}).Error; err != nil {
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				result = errors.Join(result, err)
			}
			break
		}
		if len(expired) < billingRefundCleanupBatchSize {
			break
		}
	}
	cancel()
	for i := 0; i < len(pending) || i < len(settlements); i++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		if i < len(pending) {
			refund := pending[i]
			billingRefundCursor.Store(refund.ID)
			canRefund := true
			if refund.NextAttemptAt == BillingRefundInFlight && refund.SettlementRetryAt < 0 && !refund.DecisionKnown {
				attempt, cancel := context.WithTimeout(ctx, billingRecoveryRecordTimeout)
				ready, err := markExpiredTaskReservation(attempt, refund.ID)
				cancel()
				if err != nil {
					result = errors.Join(result, fmt.Errorf("task reservation %d: %w", refund.ID, err))
				}
				canRefund = ready
			}
			if canRefund {
				attempt, cancel := context.WithTimeout(ctx, billingRecoveryRecordTimeout)
				err := CompleteBillingRefund(attempt, refund.ID)
				cancel()
				if err != nil {
					result = errors.Join(result, fmt.Errorf("refund %d: %w", refund.ID, err))
					delay := min(int64(15)<<min(refund.Attempts, 6), int64(900))
					backoff, cancel := context.WithTimeout(ctx, billingRecoveryBackoffTimeout)
					err := DB.WithContext(backoff).Model(&BillingRefund{}).Where("id = ? AND (completed = ? OR cache_pending = ?)", refund.ID, false, true).
						Updates(map[string]any{"attempts": refund.Attempts + 1, "next_attempt_at": common.GetTimestamp() + delay + refund.ID%5}).Error
					cancel()
					result = errors.Join(result, err)
				}
			}
		}
		if i < len(settlements) && ctx.Err() == nil {
			settlement := settlements[i]
			billingSettlementCursor.Store(settlement.ID)
			attempt, cancel := context.WithTimeout(ctx, billingRecoveryRecordTimeout)
			err := SettleBillingReservation(attempt, settlement.ID, settlement.UserID, settlement.Amount, settlement.SettledAmount)
			cancel()
			if err != nil {
				result = errors.Join(result, fmt.Errorf("settlement %d: %w", settlement.ID, err))
				delay := min(int64(15)<<min(settlement.Attempts, 6), int64(900))
				backoff, cancel := context.WithTimeout(ctx, billingRecoveryBackoffTimeout)
				err := DB.WithContext(backoff).Model(&BillingRefund{}).Where("id = ? AND completed = ? AND settlement_pending = ?", settlement.ID, false, true).
					Updates(map[string]any{"attempts": settlement.Attempts + 1, "settlement_retry_at": common.GetTimestamp() + delay + settlement.ID%5}).Error
				cancel()
				result = errors.Join(result, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(result, err)
	}
	return result
}

func HasExpiredBillingRefunds() bool {
	var id int64
	err := DB.Model(&BillingRefund{}).
		Where("completed = ? AND cache_pending = ? AND ((completed_at > 0 AND completed_at < ?) OR (completed_at = 0 AND created_at < ?))", true, false, time.Now().Add(-billingRefundRetention).Unix(), time.Now().Add(-billingRefundRetention).Unix()).
		Limit(1).Pluck("id", &id).Error
	return err == nil && id != 0
}

func HasPendingBillingRefunds() bool {
	var id int64
	now := common.GetTimestamp()
	query := DB.Model(&BillingRefund{}).Where("completed = ? AND ((settlement_pending = ? AND settlement_retry_at <= ?) OR (settlement_pending = ? AND ((decision_known = ? AND next_attempt_at <= ?) OR (decision_known = ? AND next_attempt_at = ? AND settlement_retry_at < 0 AND settlement_retry_at >= ?))))", false, true, now, false, true, now, false, BillingRefundInFlight, -now)
	if common.RedisEnabled {
		query = DB.Model(&BillingRefund{}).Where("(completed = ? AND ((settlement_pending = ? AND settlement_retry_at <= ?) OR (settlement_pending = ? AND ((decision_known = ? AND next_attempt_at <= ?) OR (decision_known = ? AND next_attempt_at = ? AND settlement_retry_at < 0 AND settlement_retry_at >= ?))))) OR (completed = ? AND cache_pending = ? AND next_attempt_at <= ?)", false, true, now, false, true, now, false, BillingRefundInFlight, -now, true, true, now)
	}
	err := query.Limit(1).Pluck("id", &id).Error
	return err == nil && id != 0
}
