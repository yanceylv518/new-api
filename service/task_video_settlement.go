package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	"github.com/QuantumNous/new-api/relay/channel/task/taskcommon"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/shopspring/decimal"
)

// FinalizeVideoTaskBilling 仅在主库原子结算获胜后写一次差额日志，失败不结束任务。
func FinalizeVideoTaskBilling(ctx context.Context, task *model.Task, previous model.TaskStatus, actual int, reason string, clamp *common.QuotaClamp, amounts ...*hosttypes.DiscountAmounts) (bool, error) {
	if task == nil {
		return false, fmt.Errorf("video task is required")
	}
	reserved := task.Quota
	original := task
	final := *task
	task = &final
	if len(amounts) > 0 && amounts[0] != nil {
		task.PrivateData.DiscountAmounts = amounts[0]
	} else if actual == 0 && task.PrivateData.DiscountAmounts != nil {
		task.PrivateData.DiscountAmounts = hosttypes.NewDiscountAmounts(0, 0)
	}
	delta := actual - reserved
	logType, quota := model.LogTypeConsume, delta
	if delta < 0 {
		logType, quota = model.LogTypeRefund, -delta
	}
	other := taskBillingOther(task)
	preConsumed := reserved
	if task.PrivateData.SubmissionFunding != nil {
		preConsumed = task.PrivateData.SubmissionFunding.ReservedQuota
	}
	other.SetPublic("pre_consumed_quota", preConsumed)
	other.SetPublic("actual_quota", actual)
	other.SetPublic("reason", reason)
	if task.PrivateData.DiscountAmounts != nil && actual > 0 {
		task.PrivateData.DiscountAmounts.AddToLog(other)
		other.SetPublic("discount_cost_scope", "task_total")
	}
	attachQuotaSaturationToOther(other, clamp)
	var adjustment *model.Log
	if delta != 0 && (logType != model.LogTypeConsume || common.LogConsumeEnabled) {
		username, _ := model.GetUsernameById(task.UserId, false)
		tokenName := ""
		if task.PrivateData.TokenId > 0 {
			if token, err := model.GetTokenById(task.PrivateData.TokenId); err == nil {
				tokenName = token.Name
			}
		}
		// 请求ID和时间随待补写记录冻结，重试不产生新的日志身份。
		adjustment = &model.Log{UserId: task.UserId, Username: username, CreatedAt: common.GetTimestamp(), Type: logType,
			Content: reason, ChannelId: task.ChannelId, ModelName: taskModelName(task), Quota: quota,
			TokenId: task.PrivateData.TokenId, TokenName: tokenName, Group: task.Group, Other: other.JSONString(), RequestId: common.NewRequestId()}
	}
	won, err := model.FinalizeVideoTask(ctx, task, previous, actual, adjustment)
	if err != nil {
		return false, err
	}
	if won {
		*original = *task
		RecordTaskPerformance(task)
	}
	if model.LOG_DB != model.DB {
		// 终态重放也尝试补日志，但绝不再次调整账务。
		if err := model.DeliverVideoTaskLog(ctx, task.ID); err != nil {
			logger.LogWarn(ctx, "video billing log delivery pending: "+err.Error())
		}
	}
	if won && delta > 0 && common.DataExportEnabled && common.LogConsumeEnabled {
		nodeName := task.PrivateData.NodeName
		if nodeName == "" {
			nodeName = common.NodeName
		}
		model.LogQuotaData(model.QuotaDataLogParams{UserID: task.UserId, Username: adjustment.Username, ModelName: adjustment.ModelName,
			Quota: quota, CreatedAt: adjustment.CreatedAt, UseGroup: task.Group, TokenID: task.PrivateData.TokenId, ChannelID: task.ChannelId, NodeName: nodeName})
	}
	return won, nil
}

// 所有插件的终态与资金一起提交，不能先结束轮询再单独退款。
func atomicVideoSettlementEnabled(adaptor TaskPollingAdaptor) bool {
	return adaptor != nil
}

func requireMeasuredVideoUsage(task *model.Task, result *relaycommon.TaskInfo) error {
	bc := task.PrivateData.BillingContext
	if bc == nil || result == nil {
		return nil
	}
	var keys map[string]bool
	if bc.TieredSnapshot != nil {
		keys = billingexpr.UsedUsageKeys(bc.TieredSnapshot.ExprString)
	}
	discountUses := func(variable string) bool {
		return bc.UserDiscount != nil && bc.UserDiscount.Rule.Expression != "" && discountexpr.UsesUsageVariable(bc.UserDiscount.Rule.Expression, variable)
	}
	plugin := string(task.Platform)
	if task.PrivateData.Execution != nil && task.PrivateData.Execution.TaskPlugin != nil {
		plugin = task.PrivateData.Execution.TaskPlugin.Key
	}
	upstreamModel := strings.ToLower(strings.TrimSpace(task.Properties.UpstreamModelName))
	originModel := strings.ToLower(strings.TrimSpace(task.Properties.OriginModelName))
	if originModel == "" {
		originModel = strings.ToLower(strings.TrimSpace(bc.OriginModelName))
	}
	modelName := upstreamModel
	if modelName == "" {
		modelName = originModel
	}
	// 未声明的上游接入点按提交模型识别；明确的图片模型不继承视频要求。
	seedance := strings.HasPrefix(upstreamModel, "doubao-seedance-") ||
		(plugin == "originocean" && upstreamModel == "maas-doubao-seedance-2.0") ||
		(!strings.HasPrefix(upstreamModel, "doubao-seedream-") && strings.HasPrefix(originModel, "doubao-seedance-"))
	required := []string{}
	if (plugin == "doubao" || plugin == "anyllm" || plugin == "originocean") && seedance {
		if keys["tokens"] || discountUses("task_tokens") {
			required = append(required, "tokens")
		}
		if keys["seconds"] || discountUses("seconds") {
			required = append(required, "seconds")
		}
	}
	if plugin == "hailuo" && modelName == "minimax-h3" {
		operation := task.Action
		if bc.TieredSnapshot != nil {
			if value, ok := bc.TieredSnapshot.UsageFacts["operation"].(string); ok && value != "" {
				operation = value
			}
		}
		if value, ok := result.UsageFacts["operation"].(string); ok && value != "" {
			operation = value
		}
		if operation == "context_ir" {
			for _, field := range []struct{ key, discountVariable string }{{"prompt_tokens", "input_tokens"}, {"completion_tokens", "output_tokens"}, {"tokens", "task_tokens"}} {
				if keys[field.key] || discountUses(field.discountVariable) {
					required = append(required, field.key)
				}
			}
		} else {
			if keys["seconds"] || discountUses("seconds") {
				required = append(required, "seconds")
			}
			if keys["input_video_seconds"] && bc.TieredSnapshot != nil && positiveTaskUsage(bc.TieredSnapshot.UsageFacts["input_video_seconds"]) {
				required = append(required, "input_video_seconds")
			}
			if operation == "regeneration" && keys["input_images"] && bc.TieredSnapshot != nil && positiveTaskUsage(bc.TieredSnapshot.UsageFacts["input_images"]) {
				required = append(required, "input_images")
			}
		}
	}
	for _, key := range required {
		if _, present := result.UsageFacts[key]; !present {
			return fmt.Errorf("task measured usage %s is unavailable", key)
		}
	}
	return nil
}

func positiveTaskUsage(value any) bool {
	switch number := value.(type) {
	case int:
		return number > 0
	case int64:
		return number > 0
	case float64:
		return number > 0
	default:
		return false
	}
}

// settleAtomicVideoTask 在写终态前计算金额，表达式错误保留预扣；主库失败则由下一轮重试。
func settleAtomicVideoTask(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, previous model.TaskStatus, result *relaycommon.TaskInfo) (err error) {
	original := *task
	defer func() {
		if err != nil {
			*task = original
			task.Status = previous
			saveContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, saveErr := model.MarkTaskSettlementPending(saveContext, task, previous, time.Now().Unix()+30); saveErr != nil {
				logger.LogWarn(ctx, "persist pending settlement failed: "+saveErr.Error())
			}
		}
	}()
	actual, reason := task.Quota, "video task settlement"
	var clamp *common.QuotaClamp
	var amounts *hosttypes.DiscountAmounts
	bc := task.PrivateData.BillingContext
	if quote := task.PrivateData.SubmissionQuote; quote != nil {
		if quote.Quota < 0 || quote.Quota > common.MaxQuota || quote.DiscountAmounts != nil && !quote.DiscountAmounts.ValidFor(quote.Quota) {
			return fmt.Errorf("task %s has an invalid submission quote", task.TaskID)
		}
		actual, amounts = quote.Quota, quote.DiscountAmounts
		if quote.BillingContext != nil {
			bc = quote.BillingContext
			task.PrivateData.BillingContext = bc
			original.PrivateData.BillingContext = bc
		}
	}
	if task.Status != model.TaskStatusFailure {
		if err := requireMeasuredVideoUsage(task, result); err != nil {
			return err
		}
		price, evaluation, discountErr := taskUserModelDiscount(task, result)
		if discountErr != nil {
			return discountErr
		}
		if evaluation != nil {
			billing := *bc
			billing.UserDiscount = evaluation
			billing.OtherRatios = price.OtherRatios()
			bc = &billing
			task.PrivateData.BillingContext = bc
		}
	}
	switch {
	case task.Status == model.TaskStatusFailure:
		actual, reason = 0, task.FailReason
	case bc != nil && bc.TieredSnapshot != nil:
		priced, facts, err := EvaluateTaskCompletionUsage(bc.TieredSnapshot, result.UsageFacts)
		if err != nil {
			return fmt.Errorf("task %s pricing failed; retaining reserve: %w", task.TaskID, err)
		} else {
			before, after := priced.ActualQuotaAfterGroup, priced.ActualQuotaAfterGroup
			if price := taskBillingContextPriceData(bc); price != nil && price.UserModelDiscountMultiplier() != 1 {
				value := decimal.NewFromFloat(priced.ActualQuotaBeforeGroup).Mul(decimal.NewFromFloat(bc.TieredSnapshot.GroupRatio))
				var beforeClamp *common.QuotaClamp
				before, beforeClamp = common.QuotaRoundChecked(value.InexactFloat64())
				after, clamp = common.QuotaDiscountDecimalChecked(value, price.UserModelDiscountMultiplier())
				if before > 0 && after == 0 {
					after = 1
				}
				if clamp == nil {
					clamp = beforeClamp
				}
			}
			actual, amounts = after, hosttypes.NewDiscountAmounts(before, after)
			if clamp == nil {
				clamp = priced.Clamp
			}
			billing := *bc
			snapshot := *bc.TieredSnapshot
			snapshot.UsageFacts, snapshot.EstimatedTier = facts, priced.MatchedTier
			billing.TieredSnapshot = &snapshot
			task.PrivateData.BillingContext = &billing
		}
	case bc != nil && bc.PerCallBilling:
		if task.PrivateData.SubmissionQuote == nil {
			amounts = task.PrivateData.DiscountAmounts
		}
		// 基础价格及倍率保持提交快照，仅允许最终用量改变所冻结表达式的折扣。
		if bc.UserDiscount != nil && original.PrivateData.BillingContext != nil && original.PrivateData.BillingContext.UserDiscount != nil &&
			bc.UserDiscount.DiscountBPS != original.PrivateData.BillingContext.UserDiscount.DiscountBPS {
			before := actual
			if amounts.ValidFor(actual) {
				before = amounts.Before
			}
			basis := decimal.NewFromInt(int64(before))
			if bc.QuotaBeforeDiscount != "" {
				var parseErr error
				basis, parseErr = decimal.NewFromString(bc.QuotaBeforeDiscount)
				if parseErr != nil || basis.IsNegative() {
					return fmt.Errorf("task %s has an invalid pre-discount snapshot", task.TaskID)
				}
			}
			price := taskBillingContextPriceData(bc)
			if price.UserModelDiscountMultiplier() == 1 {
				before, clamp = common.QuotaFromDecimalChecked(basis.Truncate(0))
				actual = before
			} else {
				before, clamp = common.QuotaFromDecimalChecked(basis)
				var discountClamp *common.QuotaClamp
				actual, discountClamp = common.QuotaDiscountDecimalChecked(basis, price.UserModelDiscountMultiplier())
				if before > 0 && actual == 0 {
					actual = 1
				}
				if discountClamp != nil {
					clamp = discountClamp
				}
			}
			amounts = hosttypes.NewDiscountAmounts(before, actual)
		}
	default:
		if adjusted := adaptor.AdjustBillingOnComplete(task, result); adjusted > 0 {
			before := adjusted
			actual = adjusted
			if price := taskBillingContextPriceData(task.PrivateData.BillingContext); price != nil {
				actual, clamp = common.QuotaFromFloatChecked(float64(before) * price.UserModelDiscountMultiplier())
				if before > 0 && actual == 0 {
					actual = 1
				}
			}
			amounts = hosttypes.NewDiscountAmounts(before, actual)
		} else {
			tokens := result.TotalTokens
			if tokens == 0 {
				tokens = result.CompletionTokens
			}
			if quota, description, saturation, calculatedAmounts, ok := taskQuotaByTokens(task, tokens); ok {
				actual, reason, clamp, amounts = quota, description, saturation, calculatedAmounts
			}
		}
	}
	_, err = FinalizeVideoTaskBilling(ctx, task, previous, actual, reason, clamp, amounts)
	return err
}
