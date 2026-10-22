package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type requestTraceCleanupHandler struct{}

func init() {
	RegisterSystemTaskHandler(requestTraceCleanupHandler{})
}

func (requestTraceCleanupHandler) Type() string {
	return model.SystemTaskTypeRequestTraceCleanup
}

func (requestTraceCleanupHandler) Enabled() bool {
	return common.GetRequestTraceConfig().RetentionDays > 0
}

func (requestTraceCleanupHandler) Interval() time.Duration {
	return time.Hour
}

func (requestTraceCleanupHandler) NewPayload() any {
	days := common.GetRequestTraceConfig().RetentionDays
	return LogCleanupPayload{TargetTimestamp: time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix(), BatchSize: 500}
}

func (requestTraceCleanupHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	payload := LogCleanupPayload{}
	if err := task.DecodePayload(&payload); err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	if payload.TargetTimestamp <= 0 {
		failSystemTask(task, runnerID, errors.New("request trace cleanup target is required"))
		return
	}
	payload.BatchSize = min(500, max(1, payload.BatchSize))
	state := LogCleanupState{Phase: "traces"}
	if err := task.DecodeState(&state); err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	days := common.GetRequestTraceConfig().RetentionDays
	if days > 0 {
		// 配置改为更长保留期时，待执行任务不得沿用较新的删除边界。
		payload.TargetTimestamp = min(payload.TargetTimestamp, time.Now().Add(-time.Duration(days)*24*time.Hour).Unix())
		remaining, err := model.CountOldRequestTraces(ctx, payload.TargetTimestamp)
		if err != nil {
			failSystemTask(task, runnerID, err)
			return
		}
		state.TraceTotal = state.TraceProcessed + remaining
		state.TraceRemaining = remaining
		for {
			currentDays := common.GetRequestTraceConfig().RetentionDays
			if currentDays <= 0 || currentDays > days {
				break
			}
			state.Progress = logCleanupProgress(state.TraceProcessed, state.TraceTotal)
			if err := model.UpdateSystemTaskState(task.TaskID, runnerID, state); err != nil {
				logSystemTaskLockError(ctx, task, err)
				return
			}
			deleted, err := model.DeleteOldRequestTraces(ctx, payload.TargetTimestamp, payload.BatchSize)
			if err != nil {
				failSystemTask(task, runnerID, err)
				return
			}
			if deleted == 0 {
				state.TraceRemaining = 0
				break
			}
			state.TraceProcessed += deleted
			state.TraceRemaining = max(int64(0), state.TraceRemaining-deleted)
			select {
			case <-ctx.Done():
				failSystemTask(task, runnerID, ctx.Err())
				return
			default:
			}
		}
	}
	state.Phase, state.Progress = "done", 100
	if err := model.UpdateSystemTaskState(task.TaskID, runnerID, state); err != nil {
		logSystemTaskLockError(ctx, task, err)
		return
	}
	result := LogCleanupResult{TraceDeletedCount: state.TraceProcessed}
	if err := model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, result, ""); err != nil {
		logSystemTaskLockError(ctx, task, err)
	}
}
