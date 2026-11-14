package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// BillingSession — 统一计费会话
// ---------------------------------------------------------------------------

// BillingSession 封装单次请求的预扣费/结算/退款生命周期。
// 实现 relaycommon.BillingSettler 接口。
type BillingSession struct {
	relayInfo           *relaycommon.RelayInfo
	funding             FundingSource
	preConsumedQuota    int  // 实际预扣额度（信任用户可能为 0）
	tokenConsumed       int  // 令牌额度实际扣减量
	extraReserved       int  // 发送前补充预扣的额度（订阅退款时需要单独回滚）
	trusted             bool // 是否命中信任额度旁路
	fundingSettled      bool // funding.Settle 已成功，资金来源已提交
	settled             bool // Settle 全部完成（资金 + 令牌）
	refunded            bool // 退款已持久化交接
	refundRequestID     string
	reservationRefundID int64
	ordinaryReservation bool
	submissionTaskID    int64
	mu                  sync.Mutex
}

// Settle 根据实际消耗额度进行结算。
func (s *BillingSession) Settle(actualQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return nil
	}
	if s.refunded {
		return model.ErrBillingRefundStateConflict
	}
	delta := actualQuota - s.preConsumedQuota
	if s.ordinaryReservation {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// 已知请求成功；即使终态写入失败，也不能把结果不明的预扣当作失败退款。
		s.fundingSettled = true
		if err := refundWithRetry(func() error {
			return model.MarkBillingSettlementReady(ctx, s.reservationRefundID, s.relayInfo.UserId, s.preConsumedQuota, actualQuota)
		}); err != nil {
			return err
		}
		if err := refundWithRetry(func() error {
			return model.SettleBillingReservation(ctx, s.reservationRefundID, s.relayInfo.UserId, s.preConsumedQuota, actualQuota)
		}); err != nil {
			return err
		}
		if s.funding.Source() == BillingSourceSubscription {
			s.relayInfo.SubscriptionPostDelta += int64(delta)
		}
		s.settled = true
		return nil
	}
	if delta == 0 {
		s.settled = true
		return nil
	}
	// 1) 调整资金来源（仅在尚未提交时执行，防止重复调用）
	if !s.fundingSettled {
		if err := s.funding.Settle(delta); err != nil {
			return err
		}
		s.fundingSettled = true
	}
	// 2) 调整令牌额度
	var tokenErr error
	if !s.relayInfo.IsPlayground {
		if delta > 0 {
			tokenErr = model.DecreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, delta)
		} else {
			tokenErr = model.IncreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, -delta)
		}
		if tokenErr != nil {
			// 资金来源已提交，令牌调整失败只能记录日志；标记 settled 防止 Refund 误退资金
			common.SysLog(fmt.Sprintf("error adjusting token quota after funding settled (userId=%d, tokenId=%d, delta=%d): %s",
				s.relayInfo.UserId, s.relayInfo.TokenId, delta, tokenErr.Error()))
		}
	}
	// 3) 更新 relayInfo 上的订阅 PostDelta（用于日志）
	if s.funding.Source() == BillingSourceSubscription {
		s.relayInfo.SubscriptionPostDelta += int64(delta)
	}
	s.settled = true
	return tokenErr
}

// Refund 先持久化退款交接，再异步原子退还资金和令牌；失败由独立退款任务继续恢复。
func (s *BillingSession) Refund(c *gin.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled || s.refunded || !s.needsRefundLocked() {
		return
	}
	if s.refundRequestID == "" {
		s.refundRequestID = s.relayInfo.RequestId
		if s.refundRequestID == "" {
			s.refundRequestID = model.GenerateTaskID()
		}
	}
	refund := &model.BillingRefund{RequestID: s.refundRequestID, UserID: s.relayInfo.UserId, TokenID: s.relayInfo.TokenId, Amount: s.preConsumedQuota, TokenAmount: s.tokenConsumed}
	if s.relayInfo.IsPlayground {
		refund.TokenAmount = 0
	}
	if funding, ok := s.funding.(*SubscriptionFunding); ok {
		refund.SubscriptionID, refund.PeriodStart = funding.subscriptionId, funding.periodStart
	}
	if s.reservationRefundID > 0 {
		// 已持久化的预扣只能按原 ID 恢复，不能在旧记录清理后重新创建退款。
		refund.ID = s.reservationRefundID
		if s.ordinaryReservation {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 30*time.Second)
			defer cancel()
			if err := refundWithRetry(func() error { return model.MarkBillingRefundReady(ctx, refund.ID, s.relayInfo.UserId) }); err != nil {
				logger.LogError(c, "persist billing refund decision failed: "+err.Error())
				return
			}
		}
	} else {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 30*time.Second)
		defer cancel()
		if err := refundWithRetry(func() error { return model.QueueBillingRefund(ctx, refund) }); err != nil {
			logger.LogError(c, "persist billing refund failed: "+err.Error())
			return
		}
	}
	s.refunded = true
	logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 退款已交接（refund_id=%d）", s.relayInfo.UserId, refund.ID))
	gopool.Go(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := refundWithRetry(func() error { return model.CompleteBillingRefund(ctx, refund.ID) }); err != nil {
			common.SysError(fmt.Sprintf("billing refund %d pending: %s", refund.ID, err.Error()))
		}
	})
}

// NeedsRefund 返回是否存在需要退还的预扣状态。
func (s *BillingSession) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

func (s *BillingSession) needsRefundLocked() bool {
	if s.settled || s.refunded || s.fundingSettled {
		// fundingSettled 时资金来源已提交结算，不能再退预扣费
		return false
	}
	if s.ordinaryReservation && s.reservationRefundID > 0 {
		return true
	}
	if s.tokenConsumed > 0 {
		return true
	}
	// 订阅可能在 tokenConsumed=0 时仍预扣了额度
	if sub, ok := s.funding.(*SubscriptionFunding); ok && sub.preConsumed > 0 {
		return true
	}
	return false
}

// GetPreConsumedQuota 返回实际预扣的额度。
func (s *BillingSession) GetPreConsumedQuota() int {
	return s.preConsumedQuota
}

// CompleteTaskSubmission 标记已由任务提交事务完成的资金及令牌结算。
func (s *BillingSession) CompleteTaskSubmission(actualQuota int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled {
		return
	}
	if s.funding.Source() == BillingSourceSubscription {
		s.relayInfo.SubscriptionPostDelta += int64(actualQuota - s.preConsumedQuota)
	}
	s.fundingSettled, s.settled = true, true
}

// CompleteImmediateTask 兼容原有即时任务结算调用。
func (s *BillingSession) CompleteImmediateTask(actualQuota int) {
	s.CompleteTaskSubmission(actualQuota)
}

func (s *BillingSession) Reserve(targetQuota int) error {
	return s.reserve(targetQuota, false)
}

// ReserveBeforeTaskSubmit 在发送前原子补足预留。
func (s *BillingSession) ReserveBeforeTaskSubmit(targetQuota int) error {
	return s.reserve(targetQuota, true)
}

func (s *BillingSession) reserve(targetQuota int, requireAvailableQuota bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	imageRequest := false
	if s.relayInfo != nil {
		_, imageRequest = s.relayInfo.Request.(*dto.ImageRequest)
		imageRequest = imageRequest || s.relayInfo.ImageRequestCount > 0
	}
	requireAvailableQuota = requireAvailableQuota || imageRequest
	if s.settled || s.refunded || s.trusted && !requireAvailableQuota || targetQuota <= s.preConsumedQuota {
		return nil
	}

	delta := targetQuota - s.preConsumedQuota
	if delta <= 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.reserveQuota(ctx, delta, requireAvailableQuota, false); err != nil {
		return billingReservationError(err)
	}

	s.preConsumedQuota += delta
	s.tokenConsumed += delta
	s.extraReserved += delta
	if requireAvailableQuota {
		s.trusted = false
	}
	s.syncRelayInfo()
	return nil
}

// ---------------------------------------------------------------------------
// PreConsume — 统一预扣费入口（含信任额度旁路）
// ---------------------------------------------------------------------------

// preConsume 在信任检查后原子预留资金和令牌。
func (s *BillingSession) preConsume(c *gin.Context, quota int) *types.NewAPIError {
	effectiveQuota := quota

	// ---- 信任额度旁路 ----
	if s.shouldTrust(c) {
		s.trusted = true
		effectiveQuota = 0
		logger.LogInfo(c, fmt.Sprintf("用户 %d 额度充足, 信任且不需要预扣费 (funding=%s)", s.relayInfo.UserId, s.funding.Source()))
	} else if effectiveQuota > 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 需要预扣费 %s (funding=%s)", s.relayInfo.UserId, logger.FormatQuota(effectiveQuota), s.funding.Source()))
	}

	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	if err := s.reserveQuota(ctx, effectiveQuota, true, true); err != nil {
		return billingReservationError(err)
	}

	s.preConsumedQuota = effectiveQuota
	s.tokenConsumed = effectiveQuota

	// ---- 同步 RelayInfo 兼容字段 ----
	s.syncRelayInfo()

	return nil
}

func (s *BillingSession) reserveQuota(ctx context.Context, amount int, checkWallet, initial bool) error {
	reservation := model.BillingQuotaReservation{UserID: s.relayInfo.UserId, TokenID: s.relayInfo.TokenId, TokenKey: s.relayInfo.TokenKey, Amount: amount, UpdateToken: !s.relayInfo.IsPlayground, TokenUnlimited: s.relayInfo.TokenUnlimited, CheckWallet: checkWallet}
	reservation.TaskID = s.submissionTaskID
	reservation.TaskPreparation = s.funding.Source() == BillingSourceWallet && s.submissionTaskID == 0 && s.relayInfo.TaskRelayInfo != nil && s.relayInfo.PublicTaskID != ""
	if s.reservationRefundID == 0 && s.submissionTaskID == 0 {
		if s.refundRequestID == "" {
			s.refundRequestID = s.relayInfo.RequestId
			if s.refundRequestID == "" {
				s.refundRequestID = model.GenerateTaskID()
			}
		}
		reservation.Refund = &model.BillingRefund{RequestID: s.refundRequestID, NextAttemptAt: model.BillingRefundInFlight}
	} else if s.reservationRefundID > 0 {
		reservation.Refund = &model.BillingRefund{ID: s.reservationRefundID}
	}
	switch funding := s.funding.(type) {
	case *WalletFunding:
		if _, err := model.ReserveBillingQuota(ctx, reservation); err != nil {
			return err
		}
		funding.consumed += amount
		if reservation.Refund != nil {
			s.reservationRefundID = reservation.Refund.ID
			s.ordinaryReservation = reservation.Refund.NextAttemptAt == model.BillingRefundInFlight
		}
		return nil
	case *SubscriptionFunding:
		if initial {
			reservation.SubscriptionRequestID, reservation.ModelName = funding.requestId, funding.modelName
		} else {
			reservation.SubscriptionID, reservation.SubscriptionPeriodStart = funding.subscriptionId, funding.periodStart
		}
		result, err := model.ReserveBillingQuota(ctx, reservation)
		if err != nil {
			return err
		}
		if initial && result != nil {
			funding.subscriptionId, funding.periodStart, funding.preConsumed = result.UserSubscriptionId, result.PeriodStart, result.PreConsumed
			funding.AmountTotal, funding.AmountUsedAfter = result.AmountTotal, result.AmountUsedAfter
			if plan, err := model.GetSubscriptionPlanInfoByUserSubscriptionId(result.UserSubscriptionId); err == nil && plan != nil {
				funding.PlanId, funding.PlanTitle = plan.PlanId, plan.PlanTitle
			}
		}
		if reservation.Refund != nil {
			s.reservationRefundID = reservation.Refund.ID
			s.ordinaryReservation = reservation.Refund.NextAttemptAt == model.BillingRefundInFlight
		}
		return nil
	default:
		return fmt.Errorf("unsupported funding source: %s", s.funding.Source())
	}
}

func (s *BillingSession) StageTaskSubmission(ctx context.Context, task *model.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task.PrivateData.SubmissionFunding = &model.TaskSubmissionFunding{ReservedQuota: s.preConsumedQuota, UpdateToken: !s.relayInfo.IsPlayground}
	if funding, ok := s.funding.(*SubscriptionFunding); ok {
		task.PrivateData.SubscriptionPeriodStart = funding.periodStart
	}
	if err := model.StageBillingTask(ctx, task, s.reservationRefundID); err != nil {
		return err
	}
	s.reservationRefundID = 0
	s.ordinaryReservation = false
	s.submissionTaskID = task.ID
	return nil
}

func billingReservationError(err error) *types.NewAPIError {
	if errors.Is(err, model.ErrQuotaCachePending) {
		return types.NewErrorWithStatusCode(errors.New("quota synchronization is temporarily unavailable"), types.ErrorCode("quota_cache_unavailable"), http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	if errors.Is(err, model.ErrInsufficientTokenQuota) {
		return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	if errors.Is(err, model.ErrInsufficientWalletQuota) || strings.Contains(err.Error(), "no active subscription") || strings.Contains(err.Error(), "subscription quota insufficient") {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
}

// shouldTrust 统一信任额度检查，适用于钱包和订阅。
func (s *BillingSession) shouldTrust(c *gin.Context) bool {
	// 异步任务（ForcePreConsume=true）必须预扣全额，不允许信任旁路
	if s.relayInfo.ForcePreConsume {
		return false
	}

	trustQuota := operation_setting.GetQuotaSetting().TrustQuotaUSD * common.QuotaPerUnit
	if trustQuota <= 0 || math.IsNaN(trustQuota) || math.IsInf(trustQuota, 0) {
		return false
	}

	// 检查令牌是否充足
	tokenTrusted := s.relayInfo.TokenUnlimited
	if !tokenTrusted {
		tokenQuota := c.GetInt("token_quota")
		tokenTrusted = float64(tokenQuota) > trustQuota
	}
	if !tokenTrusted {
		return false
	}

	switch s.funding.Source() {
	case BillingSourceWallet:
		return float64(s.relayInfo.UserQuota) > trustQuota
	case BillingSourceSubscription:
		// 订阅不能启用信任旁路。原因：
		// 1. PreConsumeUserSubscription 要求 amount>0 来创建预扣记录并锁定订阅
		// 2. SubscriptionFunding.PreConsume 忽略参数，始终用 s.amount 预扣
		// 3. 若信任旁路将 effectiveQuota 设为 0，会导致 preConsumedQuota 与实际订阅预扣不一致
		return false
	default:
		return false
	}
}

// syncRelayInfo 将 BillingSession 的状态同步到 RelayInfo 的兼容字段上。
func (s *BillingSession) syncRelayInfo() {
	info := s.relayInfo
	info.FinalPreConsumedQuota = s.preConsumedQuota
	info.BillingSource = s.funding.Source()

	if sub, ok := s.funding.(*SubscriptionFunding); ok {
		info.SubscriptionId = sub.subscriptionId
		info.SubscriptionPeriodStart = sub.periodStart
		info.SubscriptionPreConsumed = sub.preConsumed + int64(s.extraReserved)
		info.SubscriptionPostDelta = 0
		info.SubscriptionAmountTotal = sub.AmountTotal
		info.SubscriptionAmountUsedAfterPreConsume = sub.AmountUsedAfter + int64(s.extraReserved)
		info.SubscriptionPlanId = sub.PlanId
		info.SubscriptionPlanTitle = sub.PlanTitle
	} else {
		info.SubscriptionId = 0
		info.SubscriptionPeriodStart = 0
		info.SubscriptionPreConsumed = 0
	}
}

// ---------------------------------------------------------------------------
// NewBillingSession 工厂 — 根据计费偏好创建会话并处理回退
// ---------------------------------------------------------------------------

// NewBillingSession 根据用户计费偏好创建 BillingSession，处理 subscription_first / wallet_first 的回退。
func NewBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int) (*BillingSession, *types.NewAPIError) {
	if relayInfo == nil {
		return nil, types.NewError(fmt.Errorf("relayInfo is nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	pref := common.NormalizeBillingPreference(relayInfo.UserSetting.BillingPreference)

	// 钱包路径需要先检查用户额度
	tryWallet := func() (*BillingSession, *types.NewAPIError) {
		userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if userQuota <= 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		if userQuota-preConsumedQuota < 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("预扣费额度失败, 用户剩余额度: %s, 需要预扣费额度: %s", logger.FormatQuota(userQuota), logger.FormatQuota(preConsumedQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		relayInfo.UserQuota = userQuota

		session := &BillingSession{
			relayInfo: relayInfo,
			funding:   &WalletFunding{userId: relayInfo.UserId},
		}
		if apiErr := session.preConsume(c, preConsumedQuota); apiErr != nil {
			return nil, apiErr
		}
		return session, nil
	}

	trySubscription := func() (*BillingSession, *types.NewAPIError) {
		subConsume := int64(preConsumedQuota)
		if subConsume <= 0 {
			subConsume = 1
		}
		session := &BillingSession{
			relayInfo: relayInfo,
			funding: &SubscriptionFunding{
				requestId: relayInfo.RequestId,
				userId:    relayInfo.UserId,
				modelName: relayInfo.GetBillingModelName(),
				amount:    subConsume,
			},
		}
		// 必须传 subConsume 而非 preConsumedQuota，保证 SubscriptionFunding.amount、
		// preConsume 参数和 FinalPreConsumedQuota 三者一致，避免订阅多扣费。
		if apiErr := session.preConsume(c, int(subConsume)); apiErr != nil {
			return nil, apiErr
		}
		return session, nil
	}

	switch pref {
	case "subscription_only":
		return trySubscription()
	case "wallet_only":
		return tryWallet()
	case "wallet_first":
		session, err := tryWallet()
		if err != nil {
			if err.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				return trySubscription()
			}
			return nil, err
		}
		return session, nil
	case "subscription_first":
		fallthrough
	default:
		hasSub, subCheckErr := model.HasActiveUserSubscription(relayInfo.UserId)
		if subCheckErr != nil {
			return nil, types.NewError(subCheckErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if !hasSub {
			return tryWallet()
		}
		session, apiErr := trySubscription()
		if apiErr != nil {
			if apiErr.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				// 仅当用户的活跃订阅允许钱包回退时才回退到钱包，否则返回订阅额度不足错误
				allowOverflow, overflowErr := model.UserActiveSubscriptionsAllowWalletOverflow(relayInfo.UserId)
				if overflowErr != nil {
					return nil, types.NewError(overflowErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
				}
				if allowOverflow {
					return tryWallet()
				}
				return nil, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}
}
