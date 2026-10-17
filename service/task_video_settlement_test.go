package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

// 仅模拟不可控的上游查询，状态推进和账务都经过生产轮询入口。
type atomicVideoPollingFixture struct {
	scriptedPollingAdaptor
	key string
}

func (a *atomicVideoPollingFixture) TaskPluginKey() string { return a.key }

// 成功/失败回包遇到主库故障时必须保持原状态；重试后再次投递不能重复记账或记录日志。
func TestVideoPollingMockLoadDatabaseMatrix(t *testing.T) {
	const users, workers, initialQuota = 200, 64, 100000000
	count := 4096
	if value := os.Getenv("VIDEO_SERVICE_AUDIT_TASKS"); value != "" {
		var err error
		count, err = strconv.Atoi(value)
		require.NoError(t, err)
		require.GreaterOrEqual(t, count, users)
		require.LessOrEqual(t, count, 65536)
	}
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialect gorm.Dialector
			if engine == "mysql" {
				dsn := os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("test MySQL DSN is unset")
				}
				dialect = mysql.Open(dsn)
			} else {
				dsn := os.Getenv("TASK_PLUGIN_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("test PostgreSQL DSN is unset")
				}
				dialect = postgres.Open(dsn)
			}
			db, err := gorm.Open(dialect, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "video_service_audit_"}, Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(workers)
			connection.SetMaxIdleConns(workers)
			oldDB, oldLog := model.DB, model.LOG_DB
			oldMain, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
			model.DB, model.LOG_DB = db, db
			common.SetDatabaseTypes(common.DatabaseType(engine), common.DatabaseType(engine))
			models := []any{&model.Task{}, &model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}}
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(models...))
				model.DB, model.LOG_DB = oldDB, oldLog
				common.SetDatabaseTypes(oldMain, oldLogType)
				assert.NoError(t, connection.Close())
			})
			require.NoError(t, db.AutoMigrate(models...))
			require.NoError(t, db.AutoMigrate(models...))
			expected, reserved := make([]int, users), make([]int, users)
			tasks := make([]model.Task, count)
			for index := range tasks {
				owner := index % users
				reserved[owner] += 100
				expression := `tier("video",u("tokens")*0.5/1000000)`
				if index%2 == 1 {
					expression = `tier("video",u("seconds")*0.0004)`
				}
				actual := 125
				if index%2 == 1 {
					actual = 300
				}
				if index%5 == 0 {
					actual = 0
				}
				expected[owner] += actual
				tasks[index] = model.Task{TaskID: fmt.Sprintf("service-load-%d", index), UserId: 1000 + owner, ChannelId: 1, Quota: 100, Status: model.TaskStatusQueued, Progress: "10%", PrivateData: model.TaskPrivateData{TokenId: 1000 + owner, UpstreamTaskID: fmt.Sprint(index), DiscountAmounts: types.NewDiscountAmounts(200, 100), BillingContext: &model.TaskBillingContext{OtherRatios: map[string]float64{types.UserModelDiscountRatioKey: 0.5}, TieredSnapshot: &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprVersion: 1, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 1}}}}
			}
			totalReserved, totalActual := 0, 0
			for owner := range users {
				id := 1000 + owner
				totalReserved += reserved[owner]
				totalActual += expected[owner]
				require.NoError(t, db.Create(&model.User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: initialQuota - reserved[owner], UsedQuota: reserved[owner]}).Error)
				require.NoError(t, db.Create(&model.Token{Id: id, UserId: id, Key: fmt.Sprintf("service-load-token-%d", id), RemainQuota: initialQuota - reserved[owner], UsedQuota: reserved[owner]}).Error)
			}
			require.NoError(t, db.Create(&model.Channel{Id: 1, UsedQuota: int64(totalReserved * 2)}).Error)
			require.NoError(t, db.CreateInBatches(tasks, 64).Error)
			type outcome struct {
				err     error
				elapsed time.Duration
			}
			jobs := make(chan int, workers*2)
			results := make(chan outcome, count*2)
			var group sync.WaitGroup
			start := time.Now()
			for range workers {
				group.Go(func() {
					for index := range jobs {
						copy := tasks[index]
						facts := map[string]any{"tokens": float64(1000)}
						if index%2 == 1 {
							facts = map[string]any{"seconds": float64(3)}
						}
						status := model.TaskStatusSuccess
						if index%5 == 0 {
							status = model.TaskStatusFailure
						}
						adaptor := &atomicVideoPollingFixture{key: []string{"doubao", "hailuo", "kling", "vidu", "alibaba", "jimeng", "sora"}[index%7], scriptedPollingAdaptor: scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: status, UsageFacts: facts}}}
						begin := time.Now()
						err := updateVideoSingleTask(t.Context(), adaptor, &model.Channel{Id: 1}, copy.GetUpstreamTaskID(), map[string]*model.Task{copy.GetUpstreamTaskID(): &copy})
						results <- outcome{err, time.Since(begin)}
					}
				})
			}
			for index := range tasks {
				jobs <- index
				jobs <- index
			}
			close(jobs)
			group.Wait()
			close(results)
			elapsed := time.Since(start)
			latencies := make([]time.Duration, 0, count*2)
			for result := range results {
				require.NoError(t, result.err)
				latencies = append(latencies, result.elapsed)
			}
			for owner := range users {
				var user model.User
				var token model.Token
				require.NoError(t, db.First(&user, 1000+owner).Error)
				require.NoError(t, db.First(&token, 1000+owner).Error)
				assert.Equal(t, initialQuota-expected[owner], user.Quota)
				assert.Equal(t, expected[owner], user.UsedQuota)
				assert.Equal(t, user.Quota, token.RemainQuota)
				assert.Equal(t, user.UsedQuota, token.UsedQuota)
			}
			var channel model.Channel
			require.NoError(t, db.First(&channel, 1).Error)
			assert.EqualValues(t, totalActual*2, channel.UsedQuota)
			var logs []model.Log
			require.NoError(t, db.Find(&logs).Error)
			require.Len(t, logs, count)
			seen := make(map[string]bool, count)
			for _, entry := range logs {
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(entry.Other, &other))
				id, ok := other["task_id"].(string)
				require.True(t, ok)
				assert.False(t, seen[id])
				seen[id] = true
				index, err := strconv.Atoi(id[len("service-load-"):])
				require.NoError(t, err)
				assert.Equal(t, tasks[index].UserId, entry.UserId)
				assert.Equal(t, tasks[index].PrivateData.TokenId, entry.TokenId)
			}
			var pending int64
			require.NoError(t, db.Model(&model.Task{}).Where("status NOT IN ?", []string{model.TaskStatusSuccess, model.TaskStatusFailure}).Count(&pending).Error)
			assert.Zero(t, pending)
			slices.Sort(latencies)
			t.Logf("SERVICE_LOAD engine=%s tasks=%d attempts=%d users=%d workers=%d shared_channels=1 elapsed=%s attempts_per_second=%.1f p50=%s p95=%s p99=%s token_seconds_discount_accounting_logs=exact", engine, count, len(latencies), users, workers, elapsed, float64(len(latencies))/elapsed.Seconds(), latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)*99/100])
		})
	}
}

func TestUserModelDiscountActualUsageKeepsFrozenTimeAndSource(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	evaluation := &types.UserModelDiscountEvaluation{Rule: types.UserModelDiscountWindow{Expression: "output_tokens >= 500 ? 0.29 : 0.8"}, Source: "temporary", Context: discountexpr.Context{At: 199, OutputTokens: 100}, DiscountBPS: 8000}
	info := &relaycommon.RelayInfo{UserModelDiscountEvaluation: evaluation}
	info.PriceData.AddOtherRatio(types.UserModelDiscountRatioKey, 0.8)
	settleUserModelDiscount(ctx, info, 1000, 500, 20)
	assert.Equal(t, 2900, evaluation.DiscountBPS)
	assert.Equal(t, int64(199), evaluation.Context.At)
	assert.Equal(t, "temporary", evaluation.Source)
	assert.Equal(t, 0.29, info.PriceData.OtherRatios()[types.UserModelDiscountRatioKey])
	settleUserModelDiscount(ctx, info, 2000, 1000, 40)
	assert.Equal(t, 2900, evaluation.DiscountBPS)
}

func TestUserModelDiscountEvaluationFailureKeepsReservedRatio(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	evaluation := &types.UserModelDiscountEvaluation{Rule: types.UserModelDiscountWindow{Expression: "output_tokens > 0 ? 1.1 : 0.8"}, Context: discountexpr.Context{}, DiscountBPS: 8000}
	info := &relaycommon.RelayInfo{UserModelDiscountEvaluation: evaluation}
	info.PriceData.AddOtherRatio(types.UserModelDiscountRatioKey, 0.8)
	settleUserModelDiscount(ctx, info, 10, 20, 0)
	assert.NotEmpty(t, evaluation.Error)
	assert.Equal(t, 8000, evaluation.DiscountBPS)
	assert.Equal(t, 0.8, info.PriceData.OtherRatios()[types.UserModelDiscountRatioKey])
}

func TestUserModelDiscountTimeRuleIgnoresUsageChangesAtSettlement(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	source := `(hour("Asia/Shanghai") == 8 && minute("Asia/Shanghai") == 3) ? 0.29 : 0.8`
	evaluation := &types.UserModelDiscountEvaluation{Rule: types.UserModelDiscountWindow{Expression: source}, Source: "temporary", Context: discountexpr.Context{At: 199}, DiscountBPS: 2900}
	info := &relaycommon.RelayInfo{UserModelDiscountEvaluation: evaluation}
	info.PriceData.AddOtherRatio(types.UserModelDiscountRatioKey, 0.29)
	for _, usage := range []int{0, 1, 10000, 1000000} {
		settleUserModelDiscount(ctx, info, float64(usage), float64(usage), float64(usage))
		assert.Equal(t, 2900, evaluation.DiscountBPS)
		assert.Equal(t, int64(199), evaluation.Context.At)
		assert.Equal(t, source, evaluation.Rule.Expression)
		assert.Equal(t, 0.29, info.PriceData.OtherRatios()[types.UserModelDiscountRatioKey])
	}
}

func TestUserModelDiscountTaskActualFactsDoNotMutateSubmittedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		expression string
		facts      map[string]any
		want       int
	}{
		{"task_tokens >= 5000 ? 0.5 : 0.8", map[string]any{"task_tokens": 5000}, 5000},
		{"seconds >= 10 ? 0.6 : 0.8", map[string]any{"seconds": float64(10)}, 6000},
		{"count >= 2 && units >= 3 ? 0.7 : 0.8", map[string]any{"clips": 2, "units": int64(3)}, 7000},
		{`(hour("Asia/Shanghai") == 8 && minute("Asia/Shanghai") == 3) ? 0.29 : 0.8`, map[string]any{"task_tokens": 1000000}, 2900},
		{`(hour("Asia/Shanghai") == 8 && minute("Asia/Shanghai") == 3) ? 0.29 : 0.8`, map[string]any{"seconds": -1, "task_tokens": int64(2147483648)}, 2900},
	} {
		t.Run(tc.expression, func(t *testing.T) {
			submitted := &types.UserModelDiscountEvaluation{Rule: types.UserModelDiscountWindow{Expression: tc.expression}, Source: "temporary", Context: discountexpr.Context{At: 199}, DiscountBPS: 8000}
			task := &model.Task{}
			task.PrivateData.BillingContext = &model.TaskBillingContext{OtherRatios: map[string]float64{types.UserModelDiscountRatioKey: 0.8, "size": 2}, UserDiscount: submitted}
			price, evaluation, err := taskUserModelDiscount(task, &relaycommon.TaskInfo{UsageFacts: tc.facts})
			require.NoError(t, err)
			require.NotNil(t, evaluation)
			assert.Equal(t, tc.want, evaluation.DiscountBPS)
			assert.Equal(t, "temporary", evaluation.Source)
			assert.Equal(t, int64(199), evaluation.Context.At)
			assert.Equal(t, 2.0, price.OtherRatios()["size"])
			assert.Equal(t, float64(tc.want)/10000, price.OtherRatios()[types.UserModelDiscountRatioKey])
			assert.Equal(t, 8000, submitted.DiscountBPS)
			assert.Zero(t, submitted.Context.TaskTokens)
			assert.Equal(t, 0.8, task.PrivateData.BillingContext.OtherRatios[types.UserModelDiscountRatioKey])
		})
	}
}

func TestUserModelDiscountTaskCompletionUsage(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		result                relaycommon.TaskInfo
		input, output, tokens float64
		bps                   int
		invalid               bool
	}{
		{name: "missing usage keeps estimates", input: 6000, output: 1000, tokens: 7000, bps: 5000},
		{name: "H3 split aliases update independently", result: relaycommon.TaskInfo{UsageFacts: map[string]any{"prompt_tokens": 4000, "completion_tokens": 2000, "tokens": 6000}}, input: 4000, output: 2000, tokens: 6000, bps: 8000},
		{name: "partial input keeps other estimates", result: relaycommon.TaskInfo{UsageFacts: map[string]any{"prompt_tokens": 4000}}, input: 4000, output: 1000, tokens: 7000, bps: 8000},
		{name: "total does not overwrite output", result: relaycommon.TaskInfo{TotalTokens: 9000}, input: 6000, output: 1000, tokens: 9000, bps: 5000},
		{name: "positive top-level token counts", result: relaycommon.TaskInfo{TotalTokens: 9000, CompletionTokens: 1500}, input: 6000, output: 1500, tokens: 9000, bps: 8000},
		{name: "completion-only aggregate fallback", result: relaycommon.TaskInfo{CompletionTokens: 500}, input: 6000, output: 500, tokens: 500, bps: 8000},
		{name: "explicit aggregate zero overrides positive total", result: relaycommon.TaskInfo{TotalTokens: 9000, UsageFacts: map[string]any{"tokens": 0}}, input: 6000, output: 1000, bps: 8000},
		{name: "explicit split zeros", result: relaycommon.TaskInfo{TotalTokens: 9000, CompletionTokens: 1500, UsageFacts: map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}, bps: 8000},
		{name: "canonical fields beat aliases and top-level counts", result: relaycommon.TaskInfo{TotalTokens: 9999, CompletionTokens: 9999, UsageFacts: map[string]any{"input_tokens": 6000, "prompt_tokens": 1, "output_tokens": 1000, "completion_tokens": 5000, "task_tokens": 7000, "tokens": 99, "total_tokens": 1}}, input: 6000, output: 1000, tokens: 7000, bps: 5000},
		{name: "non-numeric usage retains estimates", result: relaycommon.TaskInfo{UsageFacts: map[string]any{"input_tokens": false, "output_tokens": "0", "tokens": nil}}, input: 6000, output: 1000, tokens: 7000, bps: 5000},
		{name: "negative completion usage is rejected", result: relaycommon.TaskInfo{UsageFacts: map[string]any{"completion_tokens": -1}}, invalid: true},
	} {
		for _, immediate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/immediate=%t", tc.name, immediate), func(t *testing.T) {
				submitted := &types.UserModelDiscountEvaluation{
					Rule: types.UserModelDiscountWindow{Expression: `input_tokens >= 5000 && output_tokens <= 1000 && task_tokens >= 5000 ? 0.5 : 0.8`}, Source: "temporary", DiscountBPS: 5000,
					Context: discountexpr.Context{At: 199, InputTokens: 6000, OutputTokens: 1000, TaskTokens: 7000, Seconds: 1, Count: 1, Units: 8, Params: map[string]any{"duration": float64(-1)}},
				}
				frozen := *submitted
				var evaluation *types.UserModelDiscountEvaluation
				var price *types.PriceData
				var err error
				if immediate {
					info := &relaycommon.RelayInfo{UserModelDiscountEvaluation: submitted}
					info.PriceData.AddOtherRatio(types.UserModelDiscountRatioKey, 0.5)
					info.PriceData.AddOtherRatio("size", 2)
					err = ApplyImmediateTaskDiscount(info, &tc.result)
					evaluation, price = info.UserModelDiscountEvaluation, &info.PriceData
				} else {
					task := &model.Task{PrivateData: model.TaskPrivateData{BillingContext: &model.TaskBillingContext{UserDiscount: submitted, OtherRatios: map[string]float64{types.UserModelDiscountRatioKey: 0.5, "size": 2}}}}
					price, evaluation, err = taskUserModelDiscount(task, &tc.result)
					assert.Same(t, submitted, task.PrivateData.BillingContext.UserDiscount)
					assert.Equal(t, 0.5, task.PrivateData.BillingContext.OtherRatios[types.UserModelDiscountRatioKey])
				}
				assert.Equal(t, frozen, *submitted, "submitted discount snapshot must remain frozen")
				if tc.invalid {
					require.Error(t, err)
					assert.Equal(t, 0.5, price.UserModelDiscountMultiplier())
					return
				}
				require.NoError(t, err)
				require.NotNil(t, evaluation)
				want := frozen.Context
				want.InputTokens, want.OutputTokens, want.TaskTokens = tc.input, tc.output, tc.tokens
				assert.Equal(t, want, evaluation.Context)
				assert.Equal(t, frozen.Rule, evaluation.Rule)
				assert.Equal(t, frozen.Source, evaluation.Source)
				assert.Equal(t, tc.bps, evaluation.DiscountBPS)
				assert.Equal(t, float64(tc.bps)/10000, price.UserModelDiscountMultiplier())
				assert.Equal(t, 2.0, price.OtherRatios()["size"])
			})
		}
	}
}

func TestVideoLayeredDiscountAtomicSettlement(t *testing.T) {
	truncate(t)
	const initial = 100000
	for index, tc := range []struct {
		name, pricing, discount, basis             string
		reserved, before, actual, finalBefore, bps int
		facts                                      map[string]any
		estimatedFacts                             map[string]any
		frozenUsage                                *discountexpr.Context
		failed, fundingFailure                     bool
	}{
		{name: "per-call frozen time", discount: `hour("Asia/Shanghai") == 10 ? 0.5 : 0.9`, basis: "100.9", reserved: 50, before: 101, actual: 50, finalBefore: 101, bps: 5000},
		{name: "per-call actual seconds", discount: `seconds >= 3 ? 0.5 : 0.8`, basis: "100.9", reserved: 81, before: 101, actual: 50, finalBefore: 101, bps: 8000, facts: map[string]any{"seconds": float64(4)}},
		{name: "per-call original rounding", discount: `seconds >= 3 ? 1.0 : 0.8`, basis: "100.9", reserved: 81, before: 101, actual: 100, finalBefore: 100, bps: 8000, facts: map[string]any{"seconds": float64(4)}},
		{name: "token pricing", pricing: `tier("video", u("tokens") * 2 / 1000000)`, discount: `task_tokens >= 2000 ? 0.5 : 0.8`, reserved: 800, before: 1000, actual: 1000, finalBefore: 2000, bps: 8000, facts: map[string]any{"tokens": float64(2000)}},
		{name: "missing tokens retain estimated charge and discount", pricing: `tier("video", u("tokens") * 2 / 1000000)`, discount: `task_tokens >= 2000 ? 0.5 : 0.8`, reserved: 2500, before: 5000, actual: 2500, finalBefore: 5000, bps: 5000, estimatedFacts: map[string]any{"tokens": 5000}, frozenUsage: &discountexpr.Context{TaskTokens: 5000}},
		{name: "explicit zero tokens refund estimate", pricing: `tier("video", u("tokens") * 2 / 1000000)`, discount: `task_tokens >= 2000 ? 0.5 : 0.8`, reserved: 2500, before: 5000, actual: 0, finalBefore: 0, bps: 5000, facts: map[string]any{"tokens": 0}, estimatedFacts: map[string]any{"tokens": 5000}, frozenUsage: &discountexpr.Context{TaskTokens: 5000}},
		{name: "H3 partial usage keeps frozen input discount", pricing: `tier("context", (u("prompt_tokens") + u("completion_tokens")) * 2 / 1000000)`, discount: `input_tokens >= 5000 ? 0.5 : 0.8`, reserved: 3500, before: 7000, actual: 4000, finalBefore: 8000, bps: 5000, facts: map[string]any{"completion_tokens": 2000}, estimatedFacts: map[string]any{"prompt_tokens": 6000, "completion_tokens": 1000}, frozenUsage: &discountexpr.Context{InputTokens: 6000, OutputTokens: 1000, TaskTokens: 7000}},
		{name: "H3 actual input changes discount", pricing: `tier("context", (u("prompt_tokens") + u("completion_tokens")) * 2 / 1000000)`, discount: `input_tokens >= 5000 ? 0.5 : 0.8`, reserved: 3500, before: 7000, actual: 4800, finalBefore: 6000, bps: 5000, facts: map[string]any{"prompt_tokens": 4000, "completion_tokens": 2000, "tokens": 6000}, estimatedFacts: map[string]any{"prompt_tokens": 6000, "completion_tokens": 1000}, frozenUsage: &discountexpr.Context{InputTokens: 6000, OutputTokens: 1000, TaskTokens: 7000}},
		{name: "second pricing and retry", pricing: `tier("video", u("seconds") * 0.001)`, discount: `seconds >= 3 ? 0.5 : 0.8`, reserved: 400, before: 500, actual: 1000, finalBefore: 2000, bps: 8000, facts: map[string]any{"seconds": float64(4)}, fundingFailure: true},
		{name: "confirmed failure refunds reserve", discount: `seconds >= 3 ? 0.5 : 0.8`, basis: "100.9", reserved: 81, before: 101, actual: 0, finalBefore: 0, bps: 8000, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := 32000 + index
			require.NoError(t, model.DB.Create(&model.User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: initial - tc.reserved, UsedQuota: tc.reserved}).Error)
			require.NoError(t, model.DB.Create(&model.Token{Id: id, UserId: id, Key: fmt.Sprintf("discount-fixture-%d", id), RemainQuota: initial - tc.reserved, UsedQuota: tc.reserved}).Error)
			require.NoError(t, model.DB.Create(&model.Channel{Id: id, UsedQuota: int64(tc.before)}).Error)
			at := time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC).Unix()
			evaluation := &types.UserModelDiscountEvaluation{Rule: types.UserModelDiscountWindow{Expression: tc.discount, DiscountBPS: 10000}, Source: "temporary", DiscountBPS: tc.bps, Context: discountexpr.Context{At: at, Seconds: 1, TaskTokens: 1000}}
			if tc.frozenUsage != nil {
				evaluation.Context = *tc.frozenUsage
				evaluation.Context.At = at
			}
			billing := &model.TaskBillingContext{ModelPrice: 99, GroupRatio: 7, OtherRatios: map[string]float64{types.UserModelDiscountRatioKey: float64(tc.bps) / 10000}, UserDiscount: evaluation, QuotaBeforeDiscount: tc.basis, PerCallBilling: tc.pricing == ""}
			if tc.pricing != "" {
				billing.TieredSnapshot = &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprVersion: 1, ExprString: tc.pricing, ExprHash: billingexpr.ExprHashString(tc.pricing), GroupRatio: 1, QuotaPerUnit: 500000, UsageFacts: tc.estimatedFacts}
			}
			task := &model.Task{TaskID: fmt.Sprintf("discount-task-%d", id), UserId: id, ChannelId: id, Quota: tc.reserved, Status: model.TaskStatusQueued, PrivateData: model.TaskPrivateData{TokenId: id, BillingContext: billing, DiscountAmounts: types.NewDiscountAmounts(tc.before, tc.reserved)}}
			require.NoError(t, model.DB.Create(task).Error)
			var submitted model.Task
			require.NoError(t, model.DB.First(&submitted, task.ID).Error)
			status := model.TaskStatus(model.TaskStatusSuccess)
			if tc.failed {
				status = model.TaskStatusFailure
			}
			result := &relaycommon.TaskInfo{Status: string(status), UsageFacts: tc.facts}
			adaptor := &atomicVideoPollingFixture{key: "doubao"}
			if tc.fundingFailure {
				require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("discount-funding-error", func(tx *gorm.DB) {
					if tx.Statement.Table == "tokens" {
						tx.AddError(errors.New("temporary token database failure"))
					}
				}))
				task.Status = status
				err := settleAtomicVideoTask(t.Context(), adaptor, task, model.TaskStatusQueued, result)
				require.Error(t, err)
				require.NoError(t, model.DB.Callback().Update().Remove("discount-funding-error"))
				var pending model.Task
				require.NoError(t, model.DB.First(&pending, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), pending.Status)
				assert.Equal(t, tc.bps, pending.PrivateData.BillingContext.UserDiscount.DiscountBPS)
				*task = pending
			}
			task.Status = status
			require.NoError(t, settleAtomicVideoTask(t.Context(), adaptor, task, model.TaskStatusQueued, result))
			// 旧请求重放不能产生第二笔资金变动。
			submitted.Status = status
			require.NoError(t, settleAtomicVideoTask(t.Context(), adaptor, &submitted, model.TaskStatusQueued, result))
			var stored model.Task
			var user model.User
			var token model.Token
			var channel model.Channel
			require.NoError(t, model.DB.First(&stored, task.ID).Error)
			require.NoError(t, model.DB.First(&user, id).Error)
			require.NoError(t, model.DB.First(&token, id).Error)
			require.NoError(t, model.DB.First(&channel, id).Error)
			assert.Equal(t, tc.actual, stored.Quota)
			assert.Equal(t, tc.finalBefore, stored.PrivateData.DiscountAmounts.Before)
			assert.Equal(t, initial-tc.actual, user.Quota)
			assert.Equal(t, tc.actual, user.UsedQuota)
			assert.Equal(t, initial-tc.actual, token.RemainQuota)
			assert.Equal(t, tc.actual, token.UsedQuota)
			assert.EqualValues(t, tc.finalBefore, channel.UsedQuota)
			assert.Equal(t, at, stored.PrivateData.BillingContext.UserDiscount.Context.At)
			assert.Equal(t, tc.bps, evaluation.DiscountBPS, "submitted rule snapshot remains immutable")
		})
	}
}

func TestVideoPollingAtomicSettlementRetry(t *testing.T) {
	for _, key := range []string{"hailuo", "doubao", "kling", "vidu", "alibaba", "jimeng", "sora"} {
		for _, status := range []string{model.TaskStatusSuccess, model.TaskStatusFailure} {
			t.Run(key+"/"+status, func(t *testing.T) {
				truncate(t)
				seedUser(t, 900, 9000)
				seedChannel(t, 900)
				seedToken(t, 900, 900, "audit-video-token", 9000)
				seedChargedAccounting(t, 900, 900, 900, 1000, 1)
				expression := `tier("video",u("seconds")*0.0002)`
				usage := map[string]any{"seconds": float64(5)}
				if key == "doubao" {
					expression = `tier("video",u("tokens")*0.5/1000000)`
					usage = map[string]any{"tokens": float64(2000)}
				}
				task := &model.Task{TaskID: "atomic-poll", UserId: 900, ChannelId: 900, Quota: 1000, Status: model.TaskStatusQueued, Group: "default", PrivateData: model.TaskPrivateData{TokenId: 900, UpstreamTaskID: "upstream-audit", BillingContext: &model.TaskBillingContext{TieredSnapshot: &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprVersion: 1, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 1, UsageFacts: usage}}}}
				task.PrivateData.Execution = &model.TaskExecutionSnapshot{UseChannel: []string{"899", "900"}}
				require.NoError(t, model.DB.Create(task).Error)
				channel := &model.Channel{Id: 900}
				adaptor := &atomicVideoPollingFixture{key: key, scriptedPollingAdaptor: scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: status, UsageFacts: usage, Reason: "upstream failure"}}}
				require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register("audit-funding-failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "tokens" {
						tx.AddError(errors.New("token store unavailable"))
					}
				}))
				err := updateVideoSingleTask(t.Context(), adaptor, channel, "upstream-audit", map[string]*model.Task{"upstream-audit": task})
				require.Error(t, err)
				require.NoError(t, model.DB.Callback().Update().Remove("audit-funding-failure"))
				var pending model.Task
				require.NoError(t, model.DB.First(&pending, task.ID).Error)
				assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), pending.Status)
				assert.Equal(t, 9000, getUserQuota(t, 900))
				assert.Zero(t, countLogs(t))
				original := pending
				require.NoError(t, updateVideoSingleTask(t.Context(), adaptor, channel, "upstream-audit", map[string]*model.Task{"upstream-audit": &pending}))
				require.NoError(t, updateVideoSingleTask(t.Context(), adaptor, channel, "upstream-audit", map[string]*model.Task{"upstream-audit": &original}))
				actual := 500
				if status == model.TaskStatusFailure {
					actual = 0
				}
				assert.Equal(t, 10000-actual, getUserQuota(t, 900))
				assert.Equal(t, int64(1), countLogs(t))
				log := getLastLog(t)
				require.NotNil(t, log)
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
				adminInfo, ok := other["admin_info"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, []any{"899", "900"}, adminInfo["use_channel"])
				var completed model.Task
				require.NoError(t, model.DB.First(&completed, task.ID).Error)
				assert.Equal(t, actual, completed.Quota)
				assert.Equal(t, model.TaskStatus(status), completed.Status)
			})
		}
	}
}

// 同一个用户的不同任务并发退款时日志按任务隔离，重复投递不能覆盖或额外记录旧快照。
func TestVideoSettlementConcurrentLogs(t *testing.T) {
	truncate(t)
	seedUser(t, 950, 1000000)
	seedChannel(t, 950)
	const tasksCount = 128
	seedChargedAccounting(t, 950, 950, 0, tasksCount*100, tasksCount)
	tasks := make([]model.Task, tasksCount)
	for index := range tasks {
		tasks[index] = model.Task{TaskID: fmt.Sprintf("log-audit-%d", index), UserId: 950, ChannelId: 950, Quota: 100, Status: model.TaskStatusQueued, Group: "default"}
		tasks[index].PrivateData.Execution = &model.TaskExecutionSnapshot{UseChannel: []string{strconv.Itoa(index + 1), "950"}}
	}
	require.NoError(t, model.DB.CreateInBatches(tasks, 32).Error)
	var workers sync.WaitGroup
	results := make(chan error, tasksCount*2)
	jobs := make(chan int, 16)
	for range 16 {
		workers.Go(func() {
			for index := range jobs {
				copy := tasks[index]
				copy.Status = model.TaskStatusFailure
				_, err := FinalizeVideoTaskBilling(context.Background(), &copy, model.TaskStatusQueued, 0, "audit refund", nil)
				results <- err
			}
		})
	}
	for index := range tasks {
		jobs <- index
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(tasksCount), countLogs(t))
	assert.Equal(t, 1000000+tasksCount*100, getUserQuota(t, 950))
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Find(&logs).Error)
	seen := make(map[string]bool, len(logs))
	for _, entry := range logs {
		assert.Equal(t, 100, entry.Quota)
		var other map[string]any
		require.NoError(t, common.UnmarshalJsonStr(entry.Other, &other))
		var index int
		_, err := fmt.Sscanf(other["task_id"].(string), "log-audit-%d", &index)
		require.NoError(t, err)
		adminInfo, ok := other["admin_info"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, []any{strconv.Itoa(index + 1), "950"}, adminInfo["use_channel"])
		assert.False(t, seen[entry.Other])
		seen[entry.Other] = true
	}
}

// 超时回收也走事务；再次清理不能重复退款。
func TestVideoTimeoutKeepsReserveUntilUpstreamOutcomeIsConfirmed(t *testing.T) {
	truncate(t)
	seedUser(t, 960, 900)
	seedChannel(t, 960)
	oldTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = oldTimeout })
	task := &model.Task{TaskID: "timeout-audit", Platform: "doubao", UserId: 960, ChannelId: 960, Quota: 100, Status: model.TaskStatusQueued, Progress: "30%", SubmitTime: time.Now().Add(-time.Hour).Unix()}
	require.NoError(t, model.DB.Create(task).Error)
	sweepTimedOutTasks(t.Context())
	sweepTimedOutTasks(t.Context())
	assert.Equal(t, 900, getUserQuota(t, 960))
	assert.Zero(t, countLogs(t))
	var retained model.Task
	require.NoError(t, model.DB.First(&retained, task.ID).Error)
	assert.True(t, retained.PrivateData.ReconciliationRequired)
	assert.Equal(t, 100, retained.Quota)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), retained.Status)
}

// 渠道查询失败不能直接结束已预扣的视频任务，否则短暂数据库故障会使后续结算永远丢失。
func TestVideoChannelLookupFailureKeepsTaskPending(t *testing.T) {
	truncate(t)
	task := &model.Task{TaskID: "channel-lookup-audit", Platform: "doubao", UserId: 970, ChannelId: 970, Quota: 100, Status: model.TaskStatusQueued, Progress: "30%", PrivateData: model.TaskPrivateData{UpstreamTaskID: "upstream-lookup-audit"}}
	require.NoError(t, model.DB.Create(task).Error)
	err := updateVideoTasks(t.Context(), "doubao", 970, []string{"upstream-lookup-audit"}, map[string]*model.Task{"upstream-lookup-audit": task})
	require.Error(t, err)
	var pending model.Task
	require.NoError(t, model.DB.First(&pending, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), pending.Status)
	assert.Equal(t, 100, pending.Quota)
}

func TestTaskPollingCursorVisitsBeyondBatchAndWraps(t *testing.T) {
	truncate(t)
	tasks := make([]model.Task, 7)
	for index := range tasks {
		tasks[index] = model.Task{TaskID: fmt.Sprintf("cursor-%d", index), Status: model.TaskStatusQueued, Progress: "10%"}
	}
	require.NoError(t, model.DB.Create(&tasks).Error)
	seen := map[int64]bool{}
	var cursor int64
	for range 4 {
		batch, err := model.GetUnfinishedSyncTasksAfter(2, cursor)
		require.NoError(t, err)
		require.Len(t, batch, 2)
		for _, task := range batch {
			seen[task.ID] = true
		}
		cursor = batch[len(batch)-1].ID
	}
	assert.Len(t, seen, len(tasks))
	assert.Equal(t, tasks[0].ID, cursor)
}

// 不同渠道/账号可以返回相同上游 ID，轮询必须始终更新各自的网关任务和用户账务。
func TestRunVideoPollingScopesSharedUpstreamIDs(t *testing.T) {
	truncate(t)
	previousLimit := constant.TaskQueryLimit
	constant.TaskQueryLimit = 100
	t.Cleanup(func() { constant.TaskQueryLimit = previousLimit })
	const expression = `tier("video",u("tokens")*0.5/1000000)`
	for _, id := range []int{980, 981} {
		require.NoError(t, model.DB.Create(&model.User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Quota: 9000, UsedQuota: 1000}).Error)
		seedTaskPollingChannel(t, id, true)
		task := &model.Task{TaskID: fmt.Sprintf("owned-task-%d", id), Platform: "doubao", UserId: id, ChannelId: id, Quota: 1000, Status: model.TaskStatusQueued, SubmitTime: time.Now().Unix(), Group: "default", PrivateData: model.TaskPrivateData{UpstreamTaskID: "same-upstream-id", BillingContext: &model.TaskBillingContext{TieredSnapshot: &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprVersion: 1, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 1, UsageFacts: map[string]any{"tokens": float64(4000)}}}}}
		task.Progress = "30%"
		require.NoError(t, model.DB.Create(task).Error)
	}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return &atomicVideoPollingFixture{key: "doubao", scriptedPollingAdaptor: scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, UsageFacts: map[string]any{"tokens": float64(2000)}}}}
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })
	RunTaskPollingOnce(t.Context(), nil)
	for _, id := range []int{980, 981} {
		var task model.Task
		require.NoError(t, model.DB.Where("user_id = ?", id).First(&task).Error)
		assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
		assert.Equal(t, 500, task.Quota)
		assert.Equal(t, 9500, getUserQuota(t, id))
	}
}
