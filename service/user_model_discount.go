package service

import (
	"fmt"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// settleUserModelDiscount 只更新实际用量，保留请求开始时选中的规则、参数和时间。
func settleUserModelDiscount(ctx *gin.Context, info *relaycommon.RelayInfo, input, output, cache float64) {
	evaluation := info.UserModelDiscountEvaluation
	if evaluation == nil || evaluation.Rule.Expression == "" || !discountexpr.DependsOnUsage(evaluation.Rule.Expression) {
		return
	}
	evaluation.Context.InputTokens = input
	evaluation.Context.OutputTokens = output
	evaluation.Context.CacheReadTokens = cache
	bps, err := evaluation.Evaluate()
	if err != nil {
		evaluation.Error = err.Error()
		logger.LogError(ctx, "用户折扣表达式结算失败，保留预扣时冻结的比例: "+err.Error())
		return
	}
	evaluation.DiscountBPS = bps
	evaluation.Error = ""
	info.PriceData.AddOtherRatio(hosttypes.UserModelDiscountRatioKey, float64(bps)/10000)
}

// ApplyImmediateTaskDiscount 与轮询共用冻结规则，供已受理且即时完成的任务更新实际折扣。
func ApplyImmediateTaskDiscount(info *relaycommon.RelayInfo, result *relaycommon.TaskInfo) error {
	if info.UserModelDiscountEvaluation == nil {
		return nil
	}
	task := &model.Task{PrivateData: model.TaskPrivateData{BillingContext: &model.TaskBillingContext{UserDiscount: info.UserModelDiscountEvaluation, OtherRatios: info.PriceData.OtherRatios()}}}
	price, evaluation, err := taskUserModelDiscount(task, result)
	if err != nil {
		return err
	}
	if evaluation != nil {
		info.UserModelDiscountEvaluation = evaluation
		info.PriceData.AddOtherRatio(hosttypes.UserModelDiscountRatioKey, price.UserModelDiscountMultiplier())
	}
	return nil
}

func taskUserModelDiscount(task *model.Task, result *relaycommon.TaskInfo) (*hosttypes.PriceData, *hosttypes.UserModelDiscountEvaluation, error) {
	bc := task.PrivateData.BillingContext
	price := taskBillingContextPriceData(bc)
	if bc == nil || bc.UserDiscount == nil || bc.UserDiscount.Rule.Expression == "" {
		return price, nil, nil
	}
	evaluation := *bc.UserDiscount
	// 时间和请求参数保持提交快照，实际用量只交给依赖用量的条件。
	if discountexpr.DependsOnUsage(evaluation.Rule.Expression) {
		// 顶层整数零无法区分缺失；明确的零值由 UsageFacts 覆盖，缺项保留冻结估算。
		if result.TotalTokens > 0 {
			evaluation.Context.TaskTokens = float64(result.TotalTokens)
		} else if result.CompletionTokens > 0 {
			evaluation.Context.TaskTokens = float64(result.CompletionTokens)
		}
		if result.CompletionTokens > 0 {
			evaluation.Context.OutputTokens = float64(result.CompletionTokens)
		}
		evaluation.Context.ApplyUsageFacts(result.UsageFacts)
	}
	bps, err := evaluation.Evaluate()
	if err != nil {
		return price, nil, fmt.Errorf("task discount expression: %w", err)
	}
	evaluation.DiscountBPS, evaluation.Error = bps, ""
	if price == nil {
		price = &hosttypes.PriceData{}
	}
	price.AddOtherRatio(hosttypes.UserModelDiscountRatioKey, float64(bps)/10000)
	return price, &evaluation, nil
}
