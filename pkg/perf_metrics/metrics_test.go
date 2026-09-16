package perfmetrics

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	if err := db.AutoMigrate(&model.PerfMetric{}); err != nil {
		panic(err)
	}
	model.DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	os.Exit(m.Run())
}

// 异步任务的终态样本必须进入模型汇总，供模型卡片立即读取成功率和延迟。
func TestRecordTaskCompletionFeedsModelSummary(t *testing.T) {
	hotBuckets = sync.Map{}
	t.Cleanup(func() { hotBuckets = sync.Map{} })

	now := time.Now().Unix()
	RecordTaskCompletion("doubao-seedance-2-0-260128", "default", now-7, now, true)

	result, err := Query(QueryParams{Model: "doubao-seedance-2-0-260128", Hours: 24})
	require.NoError(t, err)
	require.Len(t, result.Groups, 1)
	require.Len(t, result.Groups[0].Series, 1)
	assert.Equal(t, float64(100), result.Groups[0].SuccessRate)
	assert.Equal(t, int64(7000), result.Groups[0].AvgLatencyMs)
	assert.Zero(t, result.Groups[0].AvgTps)

	summary, err := QuerySummaryAll(24, nil)
	require.NoError(t, err)
	require.Len(t, summary.Models, 1)
	assert.Equal(t, "doubao-seedance-2-0-260128", summary.Models[0].ModelName)
	assert.Equal(t, int64(1), summary.Models[0].RequestCount)
	assert.Equal(t, float64(100), summary.Models[0].SuccessRate)
	assert.Equal(t, int64(7000), summary.Models[0].AvgLatencyMs)
}
