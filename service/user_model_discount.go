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

func taskUserModelDiscount(task *model.Task, facts map[string]any, input, output float64) (*hosttypes.PriceData, *hosttypes.UserModelDiscountEvaluation, error) {
	bc := task.PrivateData.BillingContext
	price := taskBillingContextPriceData(bc)
	if bc == nil || bc.UserDiscount == nil || bc.UserDiscount.Rule.Expression == "" {
		return price, nil, nil
	}
	evaluation := *bc.UserDiscount
	// 时间和请求参数保持提交快照，实际用量只交给依赖用量的条件。
	if discountexpr.DependsOnUsage(evaluation.Rule.Expression) {
		if input >= 0 {
			evaluation.Context.InputTokens = input
		}
		if output >= 0 {
			evaluation.Context.OutputTokens = output
		}
		for key, value := range facts {
			var number float64
			switch v := value.(type) {
			case float64:
				number = v
			case int:
				number = float64(v)
			case int64:
				number = float64(v)
			default:
				continue
			}
			switch key {
			case "seconds":
				evaluation.Context.Seconds = number
			case "count", "clips":
				evaluation.Context.Count = number
			case "units":
				evaluation.Context.Units = number
			case "tokens":
				evaluation.Context.OutputTokens = number
				evaluation.Context.TaskTokens = number
			case "task_tokens":
				evaluation.Context.TaskTokens = number
			case "input_tokens":
				evaluation.Context.InputTokens = number
			case "output_tokens":
				evaluation.Context.OutputTokens = number
			}
		}
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
