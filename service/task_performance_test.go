package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 终态任务的模型身份和总延迟必须进入性能汇总，供模型广场读取。
func TestRecordTaskPerformancePublishesTerminalTaskMetrics(t *testing.T) {
	require.NoError(t, model.DB.AutoMigrate(&model.PerfMetric{}))
	modelName := "task-performance-" + t.Name()
	now := time.Now().Unix()
	task := &model.Task{
		Status:     model.TaskStatusSuccess,
		Group:      "default",
		SubmitTime: now - 6,
		FinishTime: now,
		PrivateData: model.TaskPrivateData{
			BillingContext: &model.TaskBillingContext{OriginModelName: modelName},
		},
	}

	RecordTaskPerformance(task)

	var result perfmetrics.QueryResult
	require.Eventually(t, func() bool {
		var err error
		result, err = perfmetrics.Query(perfmetrics.QueryParams{Model: modelName, Hours: 24})
		return err == nil && len(result.Groups) == 1 && len(result.Groups[0].Series) == 1
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, float64(100), result.Groups[0].SuccessRate)
	assert.Equal(t, int64(6000), result.Groups[0].AvgLatencyMs)
	assert.Zero(t, result.Groups[0].AvgTps)
}
