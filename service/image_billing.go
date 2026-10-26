package service

import (
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// PrepareImageBillingForRequest reserves the effective outbound image quantity
// before each attempt, including channel retries and parameter overrides. The
// client request body stays frozen; only the independent quantity is refreshed.
func PrepareImageBillingForRequest(c *gin.Context, info *relaycommon.RelayInfo, count int) *types.NewAPIError {
	if count < 1 || count > dto.MaxImageN {
		return types.NewErrorWithStatusCode(fmt.Errorf("image_count must be an integer between 1 and %d", dto.MaxImageN), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	info.ImageRequestCount = count
	var quota int
	var err error
	if snap := info.TieredBillingSnapshot; snap != nil && snap.BillingMode == "tiered_expr" {
		// Token-only image expressions must not acquire a quantity multiplier.
		if snap.EstimatedImageCount == nil {
			return nil
		}
		request := billingexpr.RequestInput{}
		if info.BillingRequestInput != nil {
			request = *info.BillingRequestInput
		}
		request.ImageCount = &count
		cost, trace, runErr := billingexpr.RunExprByHashWithRequest(snap.ExprString, snap.ExprHash, billingexpr.TokenParams{
			P: float64(snap.EstimatedPromptTokens), C: float64(snap.EstimatedCompletionTokens), Len: float64(snap.EstimatedPromptTokens),
		}, request)
		if runErr != nil {
			return types.NewErrorWithStatusCode(runErr, types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		beforeGroup := cost / 1_000_000 * snap.QuotaPerUnit
		if trace.BillingUnit != billingexpr.BillingUnitRequest && snap.PreConsumeMultiplier != 0 {
			beforeGroup *= snap.PreConsumeMultiplier
		}
		groupRatio := info.PriceData.GroupRatioInfo.GroupRatio
		if math.IsNaN(beforeGroup) || math.IsInf(beforeGroup, 0) || math.IsNaN(groupRatio) || math.IsInf(groupRatio, 0) {
			return types.NewErrorWithStatusCode(fmt.Errorf("invalid image reservation amount"), types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		discountRatio := info.PriceData.UserModelDiscountMultiplier()
		var original int
		var originalClamp *common.QuotaClamp
		if discountRatio != 1 {
			beforeValue := decimal.NewFromFloat(beforeGroup).Mul(decimal.NewFromFloat(groupRatio))
			quota, err = common.QuotaDiscountDecimalStrict(beforeValue, discountRatio)
			original, originalClamp = common.QuotaFromDecimalChecked(beforeValue)
		} else {
			quota, err = billingexpr.QuotaRoundStrict(beforeGroup * groupRatio)
			original, originalClamp = common.QuotaRoundChecked(beforeGroup * groupRatio)
		}
		if err == nil {
			if original > 0 && quota == 0 && discountRatio < 1 {
				quota = 1
			}
			noteQuotaClamp(info, originalClamp)
			info.PriceData.DiscountAmounts = hosttypes.NewDiscountAmounts(original, quota)
			snap.EstimatedImageCount = trace.ImageCount
			snap.EstimatedQuotaBeforeGroup = beforeGroup
			snap.EstimatedQuotaAfterGroup = quota
			snap.EstimatedTier = trace.MatchedTier
			snap.EstimatedBillingUnit = trace.BillingUnit
			snap.EstimatedFixedPrice = trace.FixedPrice
			snap.GroupRatio = info.PriceData.GroupRatioInfo.GroupRatio
		}
	} else {
		quantity := 1
		if info.PriceData.UsePrice {
			quantity = count
		}
		// Overwrite the per-attempt ratio so a failed attempt cannot leak its
		// quantity into another channel.
		info.PriceData.AddOtherRatio("n", float64(quantity))
		base := info.ImageQuotaBeforeGroup
		if info.PriceData.UsePrice {
			base = info.PriceData.ModelPrice * common.QuotaPerUnit
		}
		quota, err = common.QuotaFromFloatStrict(info.PriceData.ApplyOtherRatiosToFloat(base * info.PriceData.GroupRatioInfo.GroupRatio))
	}
	if err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	info.PriceData.QuotaToPreConsume = quota
	if quota == 0 && info.Billing == nil {
		return nil
	}
	info.PriceData.FreeModel = false
	if info.Billing == nil {
		return PreConsumeBilling(c, quota, info)
	}
	if err := info.Billing.Reserve(quota); err != nil {
		var apiErr *types.NewAPIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry())
	}
	info.FinalPreConsumedQuota = info.Billing.GetPreConsumedQuota()
	return nil
}
