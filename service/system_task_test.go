package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// withSystemTaskRegistry swaps the package registry for the given handlers for
// the duration of a test and restores the original registry afterward.
func withSystemTaskRegistry(t *testing.T, handlers ...SystemTaskHandler) {
	t.Helper()
	systemTaskHandlersMu.Lock()
	saved := systemTaskHandlers
	systemTaskHandlers = map[string]SystemTaskHandler{}
	for _, h := range handlers {
		systemTaskHandlers[h.Type()] = h
	}
	systemTaskHandlersMu.Unlock()
	t.Cleanup(func() {
		systemTaskHandlersMu.Lock()
		systemTaskHandlers = saved
		systemTaskHandlersMu.Unlock()
	})
}

type stubScheduledHandler struct {
	taskType string
	enabled  bool
	interval time.Duration
	onRun    func(ctx context.Context, task *model.SystemTask, runnerID string)
}

type stubSystemTaskRunResult struct {
	taskID   string
	taskType string
	err      error
}

func (h *stubScheduledHandler) Type() string { return h.taskType }

func (h *stubScheduledHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if h.onRun != nil {
		h.onRun(ctx, task, runnerID)
	}
}

func (h *stubScheduledHandler) Enabled() bool           { return h.enabled }
func (h *stubScheduledHandler) Interval() time.Duration { return h.interval }
func (h *stubScheduledHandler) NewPayload() any         { return nil }

func countSystemTasks(t *testing.T, taskType string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, model.DB.Model(&model.SystemTask{}).Where("type = ?", taskType).Count(&count).Error)
	return count
}

func TestRequestTraceRetentionMySQL(t *testing.T) {
	if !common.UsingLogDatabase(common.DatabaseTypeMySQL) {
		t.Skip("request trace retention integration requires MySQL")
	}
	for _, scenario := range []string{"retention", "disabled", "extended-before-run", "disabled-after-batch", "cancelled", "manual-cleanup", "manual-log-read-failure"} {
		t.Run(scenario, func(t *testing.T) {
			truncate(t)
			previous := common.GetRequestTraceConfig()
			t.Cleanup(func() {
				assert.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previous)))
			})
			days := 7
			if scenario == "disabled" || scenario == "manual-cleanup" || scenario == "manual-log-read-failure" {
				days = 0
			}
			config := common.RequestTraceConfig{Level: "errors", CompressionLevel: "fast", SuccessSampleRate: 100, RetentionDays: days}
			require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(config)))
			now := time.Now()
			old, recent := now.Add(-10*24*time.Hour).Unix(), now.Add(-2*24*time.Hour).Unix()
			data := common.GetJsonString(common.RequestTraceData{Trace: common.RequestTraceSummary{Status: 200}})
			rows := []model.RequestTrace{
				{RequestId: "retention-old-a", UserId: 11, CreatedAt: old, Data: data},
				{RequestId: "retention-old-b", UserId: 11, CreatedAt: old, Data: data},
				{RequestId: "retention-recent", UserId: 11, CreatedAt: recent, Data: data},
			}
			require.NoError(t, model.RecordRequestTraces(t.Context(), rows))
			require.NoError(t, model.DB.Create(&model.Log{UserId: 11, CreatedAt: old, Other: `{"unchanged":true}`}).Error)
			payload := LogCleanupPayload{TargetTimestamp: now.Add(-7 * 24 * time.Hour).Unix(), BatchSize: 1}
			if scenario == "extended-before-run" {
				payload.TargetTimestamp = now.Add(-24 * time.Hour).Unix()
			}
			taskType := model.SystemTaskTypeRequestTraceCleanup
			if scenario == "manual-cleanup" || scenario == "manual-log-read-failure" {
				taskType = model.SystemTaskTypeLogCleanup
			}
			task, err := model.CreateSystemTask(taskType, payload, nil)
			require.NoError(t, err)
			claimed, locked, err := model.ClaimSystemTask(task.ID, taskType, "trace-retention-runner", common.GetTimestamp()+60)
			require.NoError(t, err)
			require.True(t, locked)
			if scenario == "disabled-after-batch" {
				require.NoError(t, model.LOG_DB.Callback().Delete().After("gorm:delete").Register("disable-trace-retention", func(tx *gorm.DB) {
					if tx.Statement.Table == (model.RequestTrace{}).TableName() {
						config.RetentionDays = 0
						tx.AddError(common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(config)))
					}
				}))
				t.Cleanup(func() { assert.NoError(t, model.LOG_DB.Callback().Delete().Remove("disable-trace-retention")) })
			}
			ctx := t.Context()
			if scenario == "manual-log-read-failure" {
				require.NoError(t, model.LOG_DB.Callback().Query().Before("gorm:query").Register("fail-log-cleanup-count", func(tx *gorm.DB) {
					if tx.Statement.Table == "logs" {
						tx.AddError(errors.New("test log count unavailable"))
					}
				}))
			}
			if scenario == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			if scenario == "manual-cleanup" || scenario == "manual-log-read-failure" {
				runLogCleanupTask(ctx, claimed, "trace-retention-runner")
			} else {
				requestTraceCleanupHandler{}.Run(ctx, claimed, "trace-retention-runner")
			}
			if scenario == "manual-log-read-failure" {
				require.NoError(t, model.LOG_DB.Callback().Query().Remove("fail-log-cleanup-count"))
			}
			stored, err := model.GetSystemTaskByTaskID(task.TaskID)
			require.NoError(t, err)
			require.NotNil(t, stored)
			var remaining, logs int64
			require.NoError(t, model.LOG_DB.Model(&model.RequestTrace{}).Count(&remaining).Error)
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logs).Error)
			if scenario == "cancelled" {
				assert.Equal(t, model.SystemTaskStatusFailed, stored.Status)
				assert.EqualValues(t, 3, remaining)
				return
			}
			if scenario == "manual-log-read-failure" {
				assert.Equal(t, model.SystemTaskStatusFailed, stored.Status)
				var state LogCleanupState
				require.NoError(t, stored.DecodeState(&state))
				assert.Equal(t, "logs", state.Phase)
				assert.EqualValues(t, 2, state.TraceProcessed)
				assert.EqualValues(t, 1, remaining)
				assert.EqualValues(t, 1, logs)
				return
			}
			assert.Equal(t, model.SystemTaskStatusSucceeded, stored.Status)
			var state LogCleanupState
			require.NoError(t, stored.DecodeState(&state))
			assert.Equal(t, "done", state.Phase)
			assert.Equal(t, 100, state.Progress)
			switch scenario {
			case "disabled":
				assert.EqualValues(t, 3, remaining)
			case "disabled-after-batch":
				assert.EqualValues(t, 2, remaining)
				assert.EqualValues(t, 1, state.TraceProcessed)
			default:
				assert.EqualValues(t, 1, remaining)
				assert.EqualValues(t, 2, state.TraceProcessed)
			}
			if scenario == "manual-cleanup" {
				assert.Zero(t, logs)
				assert.EqualValues(t, 1, state.Processed)
			} else {
				assert.EqualValues(t, 1, logs)
			}
		})
	}
	t.Run("scheduled cleanup respects enablement and due interval", func(t *testing.T) {
		truncate(t)
		previous := common.GetRequestTraceConfig()
		t.Cleanup(func() {
			assert.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(previous)))
		})
		config := common.RequestTraceConfig{Level: "off", CompressionLevel: "fast", SuccessSampleRate: 100}
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(config)))
		handler := requestTraceCleanupHandler{}
		withSystemTaskRegistry(t, handler)
		runSystemTaskScheduler()
		assert.Zero(t, countSystemTasks(t, handler.Type()))
		config.RetentionDays = 7
		require.NoError(t, common.SetRequestTraceOption("RequestTraceConfig", common.GetJsonString(config)))
		before := time.Now().Add(-7 * 24 * time.Hour).Unix()
		runSystemTaskScheduler()
		require.EqualValues(t, 1, countSystemTasks(t, handler.Type()))
		task, err := model.GetLatestSystemTask(handler.Type())
		require.NoError(t, err)
		require.NotNil(t, task)
		var payload LogCleanupPayload
		require.NoError(t, task.DecodePayload(&payload))
		assert.GreaterOrEqual(t, payload.TargetTimestamp, before)
		assert.LessOrEqual(t, payload.TargetTimestamp, time.Now().Add(-7*24*time.Hour).Unix())
		claimed, locked, err := model.ClaimSystemTask(task.ID, handler.Type(), "trace-scheduler", common.GetTimestamp()+60)
		require.NoError(t, err)
		require.True(t, locked)
		handler.Run(t.Context(), claimed, "trace-scheduler")
		runSystemTaskScheduler()
		assert.EqualValues(t, 1, countSystemTasks(t, handler.Type()))
		require.NoError(t, model.DB.Model(&model.SystemTask{}).Where("task_id = ?", task.TaskID).Update("updated_at", common.GetTimestamp()-7200).Error)
		runSystemTaskScheduler()
		assert.EqualValues(t, 2, countSystemTasks(t, handler.Type()))
	})
}

func TestSystemTaskSchedulerCreatesWhenDueAndDedups(t *testing.T) {
	truncate(t)

	handler := &stubScheduledHandler{taskType: "test_scheduled", enabled: true, interval: time.Minute}
	withSystemTaskRegistry(t, handler)

	runSystemTaskScheduler()
	require.Equal(t, int64(1), countSystemTasks(t, handler.taskType))

	// An active (pending) row already exists, so a second pass must not create
	// another row.
	runSystemTaskScheduler()
	require.Equal(t, int64(1), countSystemTasks(t, handler.taskType))

	// Finish the run; with a fresh updated_at the next run is not due yet.
	latest, err := model.GetLatestSystemTask(handler.taskType)
	require.NoError(t, err)
	require.NotNil(t, latest)
	_, claimed, err := model.ClaimSystemTask(latest.ID, handler.taskType, "runner-a", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, model.FinishSystemTask(latest.TaskID, "runner-a", model.SystemTaskStatusSucceeded, nil, ""))

	runSystemTaskScheduler()
	require.Equal(t, int64(1), countSystemTasks(t, handler.taskType))

	// Backdate the finished row beyond the interval -> the job becomes due again.
	require.NoError(t, model.DB.Model(&model.SystemTask{}).
		Where("task_id = ?", latest.TaskID).
		Update("updated_at", common.GetTimestamp()-120).Error)

	runSystemTaskScheduler()
	require.Equal(t, int64(2), countSystemTasks(t, handler.taskType))
}

func TestSystemTaskSchedulerSkipsDisabled(t *testing.T) {
	truncate(t)

	handler := &stubScheduledHandler{taskType: "test_disabled", enabled: false, interval: time.Minute}
	withSystemTaskRegistry(t, handler)

	runSystemTaskScheduler()
	assert.Equal(t, int64(0), countSystemTasks(t, handler.taskType))
}

func TestSystemTaskClaimPassDispatchesByType(t *testing.T) {
	truncate(t)

	ran := make(chan stubSystemTaskRunResult, 1)
	handler := &stubScheduledHandler{
		taskType: "test_dispatch",
		enabled:  true,
		interval: time.Minute,
		onRun: func(_ context.Context, task *model.SystemTask, runnerID string) {
			ran <- stubSystemTaskRunResult{
				taskType: task.Type,
				err:      model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, nil, ""),
			}
		},
	}
	withSystemTaskRegistry(t, handler)

	_, err := model.CreateSystemTask(handler.taskType, nil, nil)
	require.NoError(t, err)

	runSystemTaskClaimPass("runner-dispatch")

	select {
	case got := <-ran:
		require.NoError(t, got.err)
		assert.Equal(t, handler.taskType, got.taskType)
	case <-time.After(2 * time.Second):
		t.Fatal("claimed task was not dispatched to its handler")
	}

	require.Eventually(t, func() bool {
		latest, err := model.GetLatestSystemTask(handler.taskType)
		return err == nil && latest != nil && latest.Status == model.SystemTaskStatusSucceeded
	}, 2*time.Second, 20*time.Millisecond)
}

func TestSystemTaskClaimPassDispatchesEarliestPendingByType(t *testing.T) {
	truncate(t)

	ran := make(chan stubSystemTaskRunResult, 2)
	handlerA := &stubScheduledHandler{
		taskType: "test_dispatch_a",
		enabled:  true,
		interval: time.Minute,
		onRun: func(_ context.Context, task *model.SystemTask, runnerID string) {
			ran <- stubSystemTaskRunResult{
				taskID: task.TaskID,
				err:    model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, nil, ""),
			}
		},
	}
	handlerB := &stubScheduledHandler{
		taskType: "test_dispatch_b",
		enabled:  true,
		interval: time.Minute,
		onRun: func(_ context.Context, task *model.SystemTask, runnerID string) {
			ran <- stubSystemTaskRunResult{
				taskID: task.TaskID,
				err:    model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, nil, ""),
			}
		},
	}
	withSystemTaskRegistry(t, handlerA, handlerB)

	firstA, err := model.CreateSystemTask(handlerA.taskType, nil, nil)
	require.NoError(t, err)
	secondTaskID, err := model.GenerateSystemTaskID()
	require.NoError(t, err)
	secondA := &model.SystemTask{
		TaskID: secondTaskID,
		Type:   handlerA.taskType,
		Status: model.SystemTaskStatusPending,
	}
	require.NoError(t, model.DB.Create(secondA).Error)
	firstB, err := model.CreateSystemTask(handlerB.taskType, nil, nil)
	require.NoError(t, err)

	runSystemTaskClaimPass("runner-dispatch")

	got := map[string]bool{}
	for range 2 {
		select {
		case result := <-ran:
			require.NoError(t, result.err)
			got[result.taskID] = true
		case <-time.After(2 * time.Second):
			t.Fatal("claimed tasks were not dispatched to their handlers")
		}
	}

	assert.True(t, got[firstA.TaskID])
	assert.True(t, got[firstB.TaskID])
	assert.False(t, got[secondA.TaskID])

	require.Eventually(t, func() bool {
		reloaded, err := model.GetSystemTaskByTaskID(secondA.TaskID)
		return err == nil && reloaded != nil && reloaded.Status == model.SystemTaskStatusPending
	}, 2*time.Second, 20*time.Millisecond)
}

func TestEnqueueSystemTaskReportsCreatedAndExistingActive(t *testing.T) {
	truncate(t)

	first, created, err := EnqueueSystemTask("test_enqueue", map[string]bool{"manual": true})
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, first)

	existing, created, err := EnqueueSystemTask("test_enqueue", nil)
	require.NoError(t, err)
	require.False(t, created)
	require.NotNil(t, existing)
	assert.Equal(t, first.TaskID, existing.TaskID)

	_, claimed, err := model.ClaimSystemTask(first.ID, first.Type, "runner-a", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, model.FinishSystemTask(first.TaskID, "runner-a", model.SystemTaskStatusSucceeded, nil, ""))

	second, created, err := EnqueueSystemTask("test_enqueue", nil)
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, second)
	assert.NotEqual(t, first.TaskID, second.TaskID)
}
