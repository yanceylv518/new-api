package model

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

type cacheQuotaResult int

var ErrInsufficientWalletQuota = errors.New("wallet quota insufficient")
var ErrInsufficientTokenQuota = errors.New("token quota insufficient")

type BillingQuotaReservation struct {
	UserID, TokenID, Amount          int
	TokenKey                         string
	UpdateToken, TokenUnlimited      bool
	CheckWallet                      bool
	SubscriptionRequestID, ModelName string
	SubscriptionID                   int
	SubscriptionPeriodStart          int64
	Refund                           *BillingRefund
	TaskID                           int64
	TaskPreparation                  bool
}

// ReserveBillingQuota 在同一事务预留资金和令牌，任一步失败都不留下部分数据库扣款。
func ReserveBillingQuota(ctx context.Context, reservation BillingQuotaReservation) (*SubscriptionPreConsumeResult, error) {
	if reservation.UserID <= 0 || reservation.Amount < 0 || reservation.Amount > common.MaxQuota || reservation.UpdateToken && reservation.TokenID <= 0 {
		return nil, errors.New("invalid billing reservation")
	}
	if reservation.Amount == 0 && reservation.Refund == nil {
		return nil, nil
	}
	wallet := reservation.SubscriptionRequestID == "" && reservation.SubscriptionID == 0
	if common.RedisEnabled && reservation.Amount > 0 {
		if reservation.Refund == nil && reservation.TaskID == 0 {
			return nil, errors.New("billing reservation requires a persistent identity")
		}
		if _, err := GetUserCache(reservation.UserID); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrQuotaCachePending, err)
		}
		if reservation.UpdateToken {
			if _, err := GetTokenByKey(reservation.TokenKey, false); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrQuotaCachePending, err)
			}
		}
	}
	var subscriptionNow int64
	if reservation.SubscriptionRequestID != "" {
		subscriptionNow = GetDBTimestamp()
	}
	var subscription *SubscriptionPreConsumeResult
	var cacheTask Task
	var cacheKind, operationID string
	var cacheAttempted, transactionReady bool
	var target int
	if reservation.Refund != nil || reservation.TaskID > 0 {
		nonce, err := common.GenerateRandomCharsKey(24)
		if err != nil {
			return nil, err
		}
		operationID = nonce
		if common.RedisEnabled && reservation.Amount > 0 {
			if reservation.Refund != nil {
				cacheKind = "reserve_refund:" + nonce
			} else {
				cacheKind = "reserve_task:" + nonce
			}
		}
	}
	err := quotaCacheTransaction(DB.WithContext(ctx), func(tx *gorm.DB) error {
		var task *Task
		if reservation.TaskID > 0 {
			var err error
			task, err = lockTaskForUpdate(tx, reservation.TaskID)
			if err != nil {
				return err
			}
			funding := task.PrivateData.SubmissionFunding
			if task.UserId != reservation.UserID || task.PrivateData.TokenId != reservation.TokenID || task.PrivateData.SubscriptionId != reservation.SubscriptionID || task.PrivateData.SubscriptionPeriodStart != reservation.SubscriptionPeriodStart || task.Status != TaskStatusUnknown ||
				!task.PrivateData.SubmissionPending || task.PrivateData.SubmissionFundsCommitted || funding == nil ||
				funding.UpdateToken != reservation.UpdateToken || funding.ReservedQuota < 0 || funding.ReservedQuota > common.MaxQuota-reservation.Amount {
				return errors.New("task reservation is no longer available")
			}
		}
		var pending BillingRefund
		if reservation.Refund != nil && reservation.Refund.ID > 0 {
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", reservation.Refund.ID, reservation.UserID).First(&pending).Error; err != nil {
				return err
			}
			if pending.Completed || pending.SettlementPending || pending.DecisionKnown || pending.TokenID != reservation.TokenID || (pending.TokenTracked || pending.TokenAmount > 0) != reservation.UpdateToken || pending.SubscriptionID != reservation.SubscriptionID || pending.PeriodStart != reservation.SubscriptionPeriodStart || reservation.SubscriptionRequestID != "" || pending.Amount > common.MaxQuota-reservation.Amount {
				return errors.New("task reservation is no longer available")
			}
			if pending.NextAttemptAt != BillingRefundInFlight {
				return errors.New("task reservation is no longer available")
			}
			if pending.SettlementRetryAt < 0 && reservation.TaskPreparation {
				now, err := taskSubmissionDatabaseTime(tx)
				if err != nil {
					return err
				}
				if pending.SettlementRetryAt >= -now {
					return errors.New("task reservation has expired")
				}
			}
		}
		if wallet && reservation.Amount > 0 {
			query := tx.Model(&User{}).Where("id = ? AND quota >= ?", reservation.UserID, -common.MaxWalletQuota+int64(reservation.Amount))
			if reservation.CheckWallet && !common.RedisEnabled {
				query = query.Where("quota >= ?", reservation.Amount)
			}
			result := query.Update("quota", gorm.Expr("quota - ?", reservation.Amount))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrInsufficientWalletQuota
			}
		} else if !wallet && reservation.SubscriptionRequestID != "" {
			var err error
			subscription, err = preConsumeUserSubscriptionTx(tx, reservation.SubscriptionRequestID, reservation.UserID, reservation.ModelName, 0, int64(reservation.Amount), subscriptionNow)
			if err != nil {
				return err
			}
			if subscription.alreadyConsumed {
				return errors.New("subscription reservation already exists")
			}
		} else if !wallet && reservation.Amount > 0 {
			if err := postConsumeUserSubscriptionDeltaTx(tx, reservation.SubscriptionID, int64(reservation.Amount), reservation.SubscriptionPeriodStart, reservation.UserID); err != nil {
				return err
			}
		}
		var user User
		if err := lockForUpdate(tx).Select("id", "status").Where("id = ?", reservation.UserID).First(&user).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return errors.New("billing owner is unavailable")
		}
		if common.RedisEnabled {
			if err := finishVideoQuotaCache(ctx, tx, reservation.UserID); err != nil {
				return err
			}
		}
		cacheTask.UserId = reservation.UserID
		if reservation.Refund != nil {
			refund := reservation.Refund
			if refund.ID > 0 {
				cacheTask.ID, cacheTask.Quota = pending.ID, pending.Amount
				pending.Amount += reservation.Amount
				pending.ReserveOperation = operationID
				if reservation.UpdateToken {
					pending.TokenAmount += reservation.Amount
				}
				if err := tx.Model(&pending).Updates(map[string]any{"amount": pending.Amount, "token_amount": pending.TokenAmount, "reserve_operation": pending.ReserveOperation}).Error; err != nil {
					return err
				}
				*refund = pending
			} else {
				refund.UserID, refund.TokenID, refund.Amount = reservation.UserID, reservation.TokenID, reservation.Amount
				refund.TokenTracked = reservation.UpdateToken
				refund.ReserveOperation = operationID
				if reservation.TaskPreparation {
					now, err := taskSubmissionDatabaseTime(tx)
					if err != nil {
						return err
					}
					// 未结算时用负值记录任务准备租约，保持旧版调度器的处理中标记。
					refund.SettlementRetryAt = -(now + taskSubmissionLeaseSeconds)
				}
				if reservation.UpdateToken {
					refund.TokenAmount = reservation.Amount
				}
				if subscription != nil {
					refund.SubscriptionID, refund.PeriodStart = subscription.UserSubscriptionId, subscription.PeriodStart
				}
				if err := queueBillingRefund(tx, refund, true); err != nil {
					return err
				}
				cacheTask.ID = refund.ID
			}
			target = refund.Amount
		}
		if reservation.TaskID > 0 {
			cacheTask.ID, cacheTask.Quota = task.ID, task.PrivateData.SubmissionFunding.ReservedQuota
			task.PrivateData.SubmissionFunding.ReservedQuota += reservation.Amount
			task.PrivateData.SubmissionFunding.ReserveOperation = operationID
			target = task.PrivateData.SubmissionFunding.ReservedQuota
			if err := tx.Model(task).Update("private_data", task.PrivateData).Error; err != nil {
				return err
			}
		}
		if reservation.UpdateToken && reservation.Amount > 0 {
			query := tx.Model(&Token{}).Where("id = ? AND user_id = ?", reservation.TokenID, reservation.UserID)
			if !common.RedisEnabled && !reservation.TokenUnlimited {
				query = query.Where("remain_quota >= ?", reservation.Amount)
			}
			result := query.Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota - ?", reservation.Amount), "used_quota": gorm.Expr("used_quota + ?", reservation.Amount), "accessed_time": common.GetTimestamp()})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrInsufficientTokenQuota
			}
		} else if reservation.UpdateToken {
			var token Token
			if err := lockForUpdate(tx).Select("id").Where("id = ? AND user_id = ?", reservation.TokenID, reservation.UserID).First(&token).Error; err != nil {
				return err
			}
		}
		if common.RedisEnabled && reservation.Amount > 0 {
			cacheAttempted = true
			tokenKey := ""
			if reservation.UpdateToken {
				tokenKey = reservation.TokenKey
			}
			if err := prepareBillingReservationCache(ctx, &cacheTask, target, wallet, tokenKey, cacheKind, reservation.TokenID, reservation.CheckWallet, reservation.TokenUnlimited); err != nil {
				return err
			}
		}
		transactionReady = true
		return nil
	})
	if err != nil && transactionReady {
		// 提交响应可能丢失；只有核对主库中的预留结果后才能认定成功。
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		committed, checkErr := billingReservationCommitted(checkCtx, reservation, cacheTask.ID, target, operationID)
		cancel()
		if checkErr == nil && committed {
			err = nil
		}
	}
	if cacheAttempted {
		cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var cacheErr error
		if err == nil {
			walletFlag, cacheKey := "0", ""
			if wallet {
				walletFlag = "1"
			}
			if reservation.UpdateToken {
				cacheKey = getTokenCacheKey(reservation.TokenKey)
			}
			cacheErr = applyVideoQuotaCache(cacheCtx, reservation.UserID, cacheTask.ID, -reservation.Amount, walletFlag, cacheKey, cacheKind)
		} else {
			cacheErr = recoverVideoQuotaCache(cacheCtx, reservation.UserID)
		}
		if cacheErr != nil {
			common.SysError("billing reservation cache synchronization pending: " + cacheErr.Error())
		}
	}
	return subscription, err
}

func billingReservationCommitted(ctx context.Context, reservation BillingQuotaReservation, id int64, target int, operationID string) (bool, error) {
	if reservation.Refund != nil {
		var stored BillingRefund
		err := DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, reservation.UserID).First(&stored).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return err == nil && stored.Amount == target && stored.TokenID == reservation.TokenID && stored.ReserveOperation == operationID, err
	}
	if reservation.TaskID > 0 {
		var stored Task
		err := DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, reservation.UserID).First(&stored).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return err == nil && stored.PrivateData.SubmissionFunding != nil && stored.PrivateData.SubmissionFunding.ReservedQuota == target && stored.PrivateData.SubmissionFunding.ReserveOperation == operationID, err
	}
	return false, nil
}

var quotaCacheLastWarning atomic.Int64

// 缓存故障时拒绝不安全预扣；进程内最多每30秒告警一次，避免请求量放大日志开销。
func reportQuotaCacheUnavailable(operation string) {
	now := time.Now().Unix()
	previous := quotaCacheLastWarning.Load()
	if now-previous >= 30 && quotaCacheLastWarning.CompareAndSwap(previous, now) {
		common.SysError("quota_cache_unavailable operation=" + operation + "; refusing stale-balance fallback; verify Redis health")
	}
}

const (
	cacheQuotaInsufficient cacheQuotaResult = iota
	cacheQuotaOK
	cacheQuotaMiss
	cacheQuotaPending
)

const userQuotaReserveScript = `
if tonumber(redis.call('HGET',KEYS[2],'TaskID') or '0')>0 or tonumber(redis.call('HGET',KEYS[1],'VideoQuotaPending') or '0')>0 then return -2 end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
local quota = tonumber(redis.call('HGET', KEYS[1], 'Quota'))
if quota == nil or (ARGV[4]~='0' and quota < tonumber(ARGV[1])) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'Quota', -tonumber(ARGV[1]))
return 1`

const userQuotaDeltaScript = `
if redis.call('HEXISTS',KEYS[2],'Quota')==1 then redis.call('HINCRBY',KEYS[2],'Quota',ARGV[1]) end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or tonumber(redis.call('HGET', KEYS[1], 'CacheSchema') or '0') ~= tonumber(ARGV[3])
  or redis.call('HEXISTS', KEYS[1], 'Quota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'Quota', ARGV[1])
return 1`

const tokenQuotaReserveScript = `
if tonumber(redis.call('HGET',KEYS[2],'TaskID') or '0')>0 or tonumber(redis.call('HGET',KEYS[1],'VideoQuotaPending') or '0')>0 then return -2 end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
local remain = tonumber(redis.call('HGET', KEYS[1], 'RemainQuota'))
if remain == nil or (ARGV[4]~='1' and remain < tonumber(ARGV[1])) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', -tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

const tokenQuotaDeltaScript = `
if redis.call('HEXISTS',KEYS[2],'RemainQuota')==1 then
 redis.call('HINCRBY',KEYS[2],'RemainQuota',ARGV[1])
 redis.call('HINCRBY',KEYS[2],'UsedQuota',-tonumber(ARGV[1]))
end
if tonumber(redis.call('HGET', KEYS[1], 'Id') or '0') ~= tonumber(ARGV[2])
  or redis.call('HEXISTS', KEYS[1], 'RemainQuota') == 0
  or redis.call('HEXISTS', KEYS[1], 'UsedQuota') == 0 then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'RemainQuota', tonumber(ARGV[1]))
redis.call('HINCRBY', KEYS[1], 'UsedQuota', -tonumber(ARGV[1]))
redis.call('HSET', KEYS[1], 'AccessedTime', ARGV[3])
return 1`

func quotaResultFromLua(result int, err error) (cacheQuotaResult, error) {
	if err != nil {
		return cacheQuotaMiss, err
	}
	switch result {
	case 1:
		return cacheQuotaOK, nil
	case 0:
		return cacheQuotaInsufficient, nil
	case -2:
		return cacheQuotaPending, nil
	default:
		return cacheQuotaMiss, nil
	}
}

func cacheTryReserveUserQuota(userID int, amount int64, checkAvailable ...bool) (cacheQuotaResult, error) {
	check := 1
	if len(checkAvailable) > 0 && !checkAvailable[0] {
		check = 0
	}
	result, err := common.RDB.Eval(context.Background(), userQuotaReserveScript,
		[]string{getUserCacheKey(userID), videoUserQuotaKey(userID)}, amount, userID, userCacheSchemaVersion, check).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyUserQuotaDelta(userID int, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), userQuotaDeltaScript,
		[]string{getUserCacheKey(userID), videoUserQuotaKey(userID)}, delta, userID, userCacheSchemaVersion).Int()
	return quotaResultFromLua(result, err)
}

func cacheTryReserveTokenQuota(id int, key string, amount int64, unlimited ...bool) (cacheQuotaResult, error) {
	noLimit := 0
	if len(unlimited) > 0 && unlimited[0] {
		noLimit = 1
	}
	result, err := common.RDB.Eval(context.Background(), tokenQuotaReserveScript,
		[]string{getTokenCacheKey(key), videoTokenQuotaKey(key)}, amount, id, common.GetTimestamp(), noLimit).Int()
	return quotaResultFromLua(result, err)
}

func cacheApplyTokenQuotaDelta(id int, key string, delta int64) (cacheQuotaResult, error) {
	result, err := common.RDB.Eval(context.Background(), tokenQuotaDeltaScript,
		[]string{getTokenCacheKey(key), videoTokenQuotaKey(key)}, delta, id, common.GetTimestamp()).Int()
	return quotaResultFromLua(result, err)
}

// persistUserQuotaDelta 把已在缓存侧预扣成功的增量落库；批量模式下入队，
// 直写模式下要求行存在（用户已删除时报错，交由调用方补偿缓存）。
func persistUserQuotaDelta(id int, delta int) error {
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeUserQuota, id, delta)
		return nil
	}
	result := DB.Model(&User{}).Where("id = ?", id).Update("quota", gorm.Expr("quota + ?", delta))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func persistTokenQuotaDelta(id int, delta int) error {
	if common.BatchUpdateEnabled {
		addNewRecord(BatchUpdateTypeTokenQuota, id, delta)
		return nil
	}
	result := DB.Model(&Token{}).Where("id = ?", id).Updates(
		map[string]any{
			"remain_quota":  gorm.Expr("remain_quota + ?", delta),
			"used_quota":    gorm.Expr("used_quota - ?", delta),
			"accessed_time": common.GetTimestamp(),
		},
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func reserveUserQuotaDB(id int, quota int) (bool, error) {
	result := DB.Model(&User{}).
		Where("id = ? AND quota >= ?", id, quota).
		Update("quota", gorm.Expr("quota - ?", quota))
	return result.RowsAffected == 1, result.Error
}

func reserveTokenQuotaDB(id int, quota int) (bool, error) {
	result := DB.Model(&Token{}).
		Where("id = ? AND remain_quota >= ?", id, quota).
		Updates(map[string]any{
			"remain_quota":  gorm.Expr("remain_quota - ?", quota),
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"accessed_time": common.GetTimestamp(),
		})
	return result.RowsAffected == 1, result.Error
}

// TryReserveUserQuota atomically checks and deducts a user's wallet quota.
// 缓存命中时以缓存余额为准（避免批量模式下过期的数据库余额放大并发超扣）；
// Redis 异常或水合失败时拒绝预扣，避免使用尚未接收批量扣费的旧数据库余额。
func TryReserveUserQuota(id int, quota int) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if !common.RedisEnabled {
		return reserveUserQuotaDB(id, quota)
	}

	result, err := cacheTryReserveUserQuota(id, int64(quota))
	if err == nil && result == cacheQuotaPending {
		if err = recoverVideoQuotaCache(context.Background(), id); err == nil {
			result, err = cacheTryReserveUserQuota(id, int64(quota))
		}
	}
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetUserCache(id); hydrateErr == nil {
			result, err = cacheTryReserveUserQuota(id, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss || result == cacheQuotaPending {
		// Redis启用时可能存在尚未批量落库的预扣；故障不能回退到更高的旧DB余额。
		reportQuotaCacheUnavailable("reserve_user")
		return false, ErrQuotaCachePending
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if err = persistUserQuotaDelta(id, -quota); err != nil {
		compensated, compensateErr := cacheApplyUserQuotaDelta(id, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved user quota: result=%d error=%v", compensated, compensateErr))
		}
		return false, err
	}
	return true, nil
}

// TryReserveTokenQuota atomically checks and deducts a token quota. Unlimited
// tokens skip the balance check but still update remain/used accounting.
func TryReserveTokenQuota(id int, key string, quota int, unlimited bool) (bool, error) {
	if quota < 0 {
		return false, errors.New("quota 不能为负数！")
	}
	if quota == 0 {
		return true, nil
	}
	if unlimited {
		return true, DecreaseTokenQuota(id, key, quota)
	}
	if !common.RedisEnabled {
		return reserveTokenQuotaDB(id, quota)
	}

	result, err := cacheTryReserveTokenQuota(id, key, int64(quota))
	if err == nil && result == cacheQuotaPending {
		if err = recoverVideoTokenQuotaCache(context.Background(), key); err == nil {
			result, err = cacheTryReserveTokenQuota(id, key, int64(quota))
		}
	}
	if err == nil && result == cacheQuotaMiss {
		if _, hydrateErr := GetTokenByKey(key, true); hydrateErr == nil {
			result, err = cacheTryReserveTokenQuota(id, key, int64(quota))
		}
	}
	if err != nil || result == cacheQuotaMiss || result == cacheQuotaPending {
		reportQuotaCacheUnavailable("reserve_token")
		return false, ErrQuotaCachePending
	}
	if result == cacheQuotaInsufficient {
		return false, nil
	}
	if err = persistTokenQuotaDelta(id, -quota); err != nil {
		compensated, compensateErr := cacheApplyTokenQuotaDelta(id, key, int64(quota))
		if compensateErr != nil || compensated != cacheQuotaOK {
			common.SysError(fmt.Sprintf("failed to compensate reserved token quota: result=%d error=%v", compensated, compensateErr))
		}
		return false, err
	}
	return true, nil
}
