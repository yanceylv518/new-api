package service

import (
	"context"
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type requestTraceCleanupHandler struct {
	timeline bool
}

func init() {
	RegisterSystemTaskHandler(requestTraceCleanupHandler{})
	RegisterSystemTaskHandler(requestTraceCleanupHandler{timeline: true})
}

func (h requestTraceCleanupHandler) Type() string {
	if h.timeline {
		return model.SystemTaskTypeRequestTimelineCleanup
	}
	return model.SystemTaskTypeRequestTraceCleanup
}

func (h requestTraceCleanupHandler) retentionDays() int {
	if h.timeline {
		return model.RequestTraceRetentionDays()
	}
	return common.GetRequestTraceConfig().RetentionDays
}

func (h requestTraceCleanupHandler) Enabled() bool {
	return h.retentionDays() > 0
}

func (h requestTraceCleanupHandler) Interval() time.Duration {
	if h.timeline {
		return 10 * time.Minute
	}
	return time.Hour
}

func (h requestTraceCleanupHandler) NewPayload() any {
	days := h.retentionDays()
	return LogCleanupPayload{TargetTimestamp: time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix(), BatchSize: 500}
}

func StartRequestTraceCleanupTask(ctx context.Context, taskType string) (*model.SystemTask, error) {
	if taskType != model.SystemTaskTypeRequestTraceCleanup && taskType != model.SystemTaskTypeRequestTimelineCleanup {
		return nil, errors.New("invalid request trace cleanup type")
	}
	h := requestTraceCleanupHandler{timeline: taskType == model.SystemTaskTypeRequestTimelineCleanup}
	days, err := model.GetSavedRequestTraceRetentionDays(ctx, h.timeline)
	if err != nil {
		return nil, err
	}
	if days <= 0 {
		return nil, errors.New("save a positive retention period before cleaning expired traces")
	}
	payload := LogCleanupPayload{TargetTimestamp: time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix(), BatchSize: 500}
	task, _, err := EnqueueSystemTask(h.Type(), payload)
	return task, err
}

func (h requestTraceCleanupHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	completed := false
	if h.timeline {
		requestTimelineCleanupPending.Store(true)
		defer func() {
			requestTimelineCleanupPending.Store(false)
			if !completed {
				requestTimelineCleanupFailed.Add(1)
			}
		}()
	}
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
	days, err := model.GetSavedRequestTraceRetentionDays(ctx, h.timeline)
	if err != nil {
		failSystemTask(task, runnerID, err)
		return
	}
	if days > 0 {
		// 配置改为更长保留期时，待执行任务不得沿用较新的删除边界。
		payload.TargetTimestamp = min(payload.TargetTimestamp, time.Now().Add(-time.Duration(days)*24*time.Hour).Unix())
		cutoff := payload.TargetTimestamp
		countRows := model.CountOldRequestTraces
		deleteRows := model.DeleteOldRequestTraces
		if h.timeline {
			cutoff *= 1000000
			countRows = model.CountOldRequestTimelines
			deleteRows = model.DeleteOldRequestTimelines
		}
		queryTimeout := 30 * time.Second
		if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
			queryTimeout = 10 * time.Minute
		}
		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		remaining, err := countRows(queryCtx, cutoff)
		cancel()
		if err != nil {
			failSystemTask(task, runnerID, err)
			return
		}
		state.TraceTotal = state.TraceProcessed + remaining
		state.TraceRemaining = remaining
		for {
			state.Progress = logCleanupProgress(state.TraceProcessed, state.TraceTotal)
			if err := model.UpdateSystemTaskState(task.TaskID, runnerID, state); err != nil {
				logSystemTaskLockError(ctx, task, err)
				return
			}
			currentDays, err := model.GetSavedRequestTraceRetentionDays(ctx, h.timeline)
			if err != nil {
				failSystemTask(task, runnerID, err)
				return
			}
			if currentDays <= 0 || currentDays > days {
				failSystemTask(task, runnerID, errors.New("cleanup stopped because the retention period was extended or disabled"))
				return
			}
			queryCtx, cancel = context.WithTimeout(ctx, queryTimeout)
			deleted, err := deleteRows(queryCtx, cutoff, payload.BatchSize)
			cancel()
			if err != nil {
				failSystemTask(task, runnerID, err)
				return
			}
			if deleted == 0 {
				state.TraceRemaining = 0
				break
			}
			state.TraceProcessed += deleted
			if h.timeline {
				requestTimelineCleanupDeleted.Add(deleted)
			}
			state.TraceRemaining = max(int64(0), state.TraceRemaining-deleted)
			// 批次之间让出数据库资源；取消时不再开始下一批。
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				failSystemTask(task, runnerID, ctx.Err())
				return
			case <-timer.C:
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
		return
	}
	completed = true
}
