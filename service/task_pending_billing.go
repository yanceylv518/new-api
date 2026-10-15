package service

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// prepareTaskBilling 与任务终态一起保存，恢复时不再依赖上游或当前适配器配置。
func prepareTaskBilling(task *model.Task, result *relaycommon.TaskInfo, adaptor TaskPollingAdaptor) {
	adjusted := 0
	bc := task.PrivateData.BillingContext
	if adaptor != nil && (bc == nil || (bc.TieredSnapshot == nil && !bc.PerCallBilling)) {
		adjusted = adaptor.AdjustBillingOnComplete(task, result)
	}
	task.PrivateData.BillingResult = &model.TaskBillingResult{
		TotalTokens: result.TotalTokens, CompletionTokens: result.CompletionTokens,
		UsageFacts: result.UsageFacts, AdjustedQuota: adjusted,
	}
	task.BillingRetryAt = time.Now().Unix()
}

func finishPendingTaskBilling(ctx context.Context, task *model.Task) error {
	result := task.PrivateData.BillingResult
	if result == nil {
		return fmt.Errorf("task %s is missing its pending billing result", task.TaskID)
	}
	handled := settleTaskBillingOnComplete(ctx, nil, task, &relaycommon.TaskInfo{
		Status: string(task.Status), TotalTokens: result.TotalTokens,
		CompletionTokens: result.CompletionTokens, UsageFacts: result.UsageFacts,
	})
	if !handled {
		if task.Status == model.TaskStatusFailure && !RefundTaskQuota(ctx, task, task.FailReason) {
			return fmt.Errorf("task %s refund remains pending", task.TaskID)
		}
		return model.CompleteTaskBilling(ctx, task)
	}
	if task.BillingRetryAt > 0 {
		return fmt.Errorf("task %s settlement remains pending", task.TaskID)
	}
	return nil
}

func retryTaskBillingLater(ctx context.Context, task *model.Task, err error) {
	logger.LogError(ctx, err.Error())
	if retryErr := model.DelayTaskBilling(ctx, task); retryErr != nil {
		logger.LogError(ctx, fmt.Sprintf("task %s billing retry scheduling failed: %v", task.TaskID, retryErr))
	}
}

// 表达式或历史快照无效时沿用既有保留预扣规则，避免每轮重试同一个确定性计算错误。
func keepTaskPrecharge(ctx context.Context, task *model.Task) {
	if err := model.CompleteTaskBilling(ctx, task); err != nil {
		logger.LogError(ctx, fmt.Sprintf("task %s precharge decision persistence failed: %v", task.TaskID, err))
	}
}
