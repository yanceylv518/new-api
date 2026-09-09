package service

import (
	"context"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	hosttypes "github.com/QuantumNous/new-api/types"
)

// FinalizeVideoTaskBilling 仅在主库原子结算获胜后写一次差额日志，失败不结束任务。
func FinalizeVideoTaskBilling(ctx context.Context, task *model.Task, previous model.TaskStatus, actual int, reason string, clamp *common.QuotaClamp, amounts ...*hosttypes.DiscountAmounts) (bool, error) {
	if task == nil {
		return false, fmt.Errorf("video task is required")
	}
	reserved := task.Quota
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

// atomicVideoSettlementEnabled 仅覆盖本次审查的两个插件；传统适配器继续使用既有生命周期。
func atomicVideoSettlementEnabled(adaptor TaskPollingAdaptor) bool {
	identity, ok := adaptor.(interface{ TaskPluginKey() string })
	return ok && (identity.TaskPluginKey() == "doubao" || identity.TaskPluginKey() == "hailuo")
}

// 超时清理没有适配器实例，优先使用冻结插件身份，旧记录按其历史平台识别。
func atomicVideoTask(task *model.Task) bool {
	if execution := task.PrivateData.Execution; execution != nil && execution.TaskPlugin != nil {
		key := execution.TaskPlugin.Key
		return key == "doubao" || key == "hailuo"
	}
	switch task.Platform {
	case "doubao", "hailuo", "35", "45", "54":
		return true
	default:
		return false
	}
}

// settleAtomicVideoTask 在写终态前计算金额，表达式错误保留预扣；主库失败则由下一轮重试。
func settleAtomicVideoTask(ctx context.Context, adaptor TaskPollingAdaptor, task *model.Task, previous model.TaskStatus, result *relaycommon.TaskInfo) error {
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
			logger.LogWarn(ctx, fmt.Sprintf("task %s pricing failed; retaining reserve: %v", task.TaskID, err))
		} else {
			before, after := priced.ActualQuotaAfterGroup, priced.ActualQuotaAfterGroup
			if price := taskBillingContextPriceData(bc); price != nil && price.UserModelDiscountMultiplier() != 1 {
				var beforeClamp *common.QuotaClamp
				before, beforeClamp = common.QuotaRoundChecked(priced.ActualQuotaBeforeGroup * bc.TieredSnapshot.GroupRatio)
				after, clamp = common.QuotaRoundChecked(float64(before) * price.UserModelDiscountMultiplier())
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
			bc.TieredSnapshot.UsageFacts, bc.TieredSnapshot.EstimatedTier = facts, priced.MatchedTier
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
	_, err := FinalizeVideoTaskBilling(ctx, task, previous, actual, reason, clamp, amounts)
	return err
}
