package service

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
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
	other.SetPublic("pre_consumed_quota", reserved)
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

// settleAtomicVideoTask 在写终态前计算金额，表达式错误保留预扣；主库失败则由下一轮重试。
func settleAtomicVideoTask(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, previous model.TaskStatus, result *relaycommon.TaskInfo) (err error) {
	original := *task
	defer func() {
		if err != nil {
			*task = original
			task.Status = previous
			task.PrivateData.ReconciliationRequired = true
			task.PrivateData.ReconciliationReason = "task settlement is pending; reserved quota retained"
			task.PrivateData.NextPollAt = time.Now().Unix() + 30
			if _, saveErr := task.UpdateWithStatus(previous); saveErr != nil {
				logger.LogWarn(ctx, "persist pending settlement failed: "+saveErr.Error())
			}
		}
	}()
	actual, reason := task.Quota, "video task settlement"
	var clamp *common.QuotaClamp
	var amounts *hosttypes.DiscountAmounts
	bc := task.PrivateData.BillingContext
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
		// 按次价格在提交时已固定，终态只提交状态。
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
