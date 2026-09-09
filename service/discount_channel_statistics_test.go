package service

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 半额度边界按十进制半入取整，折前、折后和优惠应来自同一份独立计算的期望值。
func TestDiscountTieredHalfQuotaAmounts(t *testing.T) {
	for _, tc := range []struct{ bps, after int }{{4250, 2338}, {5750, 3163}, {8150, 4483}, {2850, 1568}} {
		info := makeRelayInfo(`tier("base", p * 6 + c * 10)`, 1, 1000, 500)
		info.PriceData.AddOtherRatio(types.UserModelDiscountRatioKey, float64(tc.bps)/10000)
		ok, after, result := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
		require.True(t, ok)
		require.NotNil(t, result)
		assert.Equal(t, tc.after, after, "bps=%d", tc.bps)
		assert.Equal(t, types.NewDiscountAmounts(5500, tc.after), info.PriceData.DiscountAmounts)
	}
}

// JSON 重载或调用方传入的金额必须与实际扣款匹配，不能将损坏快照写成可信账单。
func TestDiscountTaskRejectsInconsistentAmounts(t *testing.T) {
	for _, amounts := range []types.DiscountAmounts{
		{Before: 50, After: 19, Savings: 31},
		{Before: 50, After: 20, Savings: 999},
		{Before: -50, After: 20, Savings: -70},
	} {
		t.Run(fmt.Sprint(amounts), func(t *testing.T) {
			truncate(t)
			const id = 75
			seedUser(t, id, 9990)
			seedToken(t, id, id, "discount-invalid-test", 9990)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, 10, 1)
			model.UpdateChannelUsedQuota(id, 15)
			task := makeTask(id, id, 10, id, BillingSourceWallet, 0)
			task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(25, 10)
			require.NoError(t, model.DB.Create(task).Error)
			RecalculateTaskQuotaWithAmounts(context.Background(), task, 20, &amounts, "invalid snapshot")
			assert.Equal(t, 9990, getUserQuota(t, id))
			assert.Equal(t, 9990, getTokenRemainQuota(t, id))
			assert.EqualValues(t, 25, getChannelUsedQuota(t, id))
			assert.Zero(t, countLogs(t))
			var stored model.Task
			require.NoError(t, model.DB.First(&stored, task.ID).Error)
			assert.Equal(t, 10, stored.Quota)
			assert.Equal(t, types.NewDiscountAmounts(25, 10), stored.PrivateData.DiscountAmounts)
		})
	}
}

// 损坏的持久化优惠金额不得直接进入用户可见日志；有效快照必须完整保留三个整数。
func TestDiscountLogRejectsCorruptSnapshot(t *testing.T) {
	other := model.NewLogOther()
	(&types.DiscountAmounts{Before: 200, After: 58, Savings: 999}).AddToLog(other)
	assert.Empty(t, other.Snapshot())
	types.NewDiscountAmounts(200, 58).AddToLog(other)
	assert.Equal(t, map[string]any{"quota_before_discount": 200, "quota_after_discount": 58, "discount_quota": 142}, other.Snapshot())
}

// token 重算沿用截断规则，但十进制折扣不能因浮点误差将精确的 58 额度扣成 57。
func TestDiscountTaskTokenRoundingAndLogAmounts(t *testing.T) {
	truncate(t)
	const id = 74
	seedUser(t, id, 9900)
	seedToken(t, id, id, "discount-rounding-test", 9900)
	seedChannel(t, id)
	seedChargedAccounting(t, id, id, id, 100, 1)
	model.UpdateChannelUsedQuota(id, 245)
	task := makeTask(id, id, 100, id, BillingSourceWallet, 0)
	task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(345, 100)
	task.PrivateData.BillingContext = &model.TaskBillingContext{OriginModelName: "rounding-test", ModelRatio: 2, GroupRatio: 1, OtherRatios: map[string]float64{types.UserModelDiscountRatioKey: 0.29}}
	require.NoError(t, model.DB.Create(task).Error)
	require.True(t, RecalculateTaskQuotaByTokens(context.Background(), task, 100))
	assert.Equal(t, 9942, getUserQuota(t, id))
	assert.Equal(t, 9942, getTokenRemainQuota(t, id))
	assert.Equal(t, 58, task.Quota)
	assert.EqualValues(t, 200, getChannelUsedQuota(t, id))
	entry := getLastLog(t)
	require.NotNil(t, entry)
	assert.Equal(t, model.LogTypeRefund, entry.Type)
	assert.Equal(t, 42, entry.Quota)
	var details struct {
		types.DiscountAmounts
		Scope string `json:"discount_cost_scope"`
	}
	require.NoError(t, common.UnmarshalJsonStr(entry.Other, &details))
	assert.Equal(t, types.DiscountAmounts{Before: 200, After: 58, Savings: 142}, details.DiscountAmounts)
	assert.Equal(t, "task_total", details.Scope)
}

// 折扣任务按提交快照做补扣、退差额与全退；渠道保留历史用量并始终调整折前差额。
func TestDiscountTaskAccountingLifecycle(t *testing.T) {
	for _, actual := range []int{0, 1200, 2000, 3200} {
		t.Run(fmt.Sprint(actual), func(t *testing.T) {
			truncate(t)
			const id = 73
			seedUser(t, id, 8000)
			seedToken(t, id, id, "discount-task-test", 8000)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, 2000, 1)
			model.UpdateChannelUsedQuota(id, 3777)
			task := makeTask(id, id, 2000, id, BillingSourceWallet, 0)
			task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(5000, 2000)
			require.NoError(t, model.DB.Create(task).Error)
			amounts := types.NewDiscountAmounts(actual*5/2, actual)
			RecalculateTaskQuotaWithAmounts(context.Background(), task, actual, amounts, "discount settlement")
			assert.Equal(t, 10000-actual, getUserQuota(t, id))
			assert.Equal(t, 10000-actual, getTokenRemainQuota(t, id))
			assert.Equal(t, actual, getTokenUsedQuota(t, id))
			used, requests := getUserUsageAccounting(t, id)
			assert.Equal(t, actual, used)
			assert.Equal(t, 1, requests)
			assert.EqualValues(t, 777+actual*5/2, getChannelUsedQuota(t, id))
			var stored model.Task
			require.NoError(t, model.DB.First(&stored, task.ID).Error)
			assert.Equal(t, actual, stored.Quota)
			assert.Equal(t, amounts, stored.PrivateData.DiscountAmounts)
			// 重载后的相同金额重算不应再次改变资金或渠道总账。
			RecalculateTaskQuotaWithAmounts(context.Background(), &stored, actual, amounts, "same settlement")
			assert.Equal(t, 10000-actual, getUserQuota(t, id))
			assert.Equal(t, 10000-actual, getTokenRemainQuota(t, id))
			assert.EqualValues(t, 777+actual*5/2, getChannelUsedQuota(t, id))
		})
	}
}

// 同一渠道的相同用量不受用户折扣影响；用户累计仍按各自折后价格计费。
func TestDiscountChannelStatisticsAcrossBillingPaths(t *testing.T) {
	for _, path := range []string{"text", "audio", "realtime"} {
		t.Run(path, func(t *testing.T) {
			truncate(t)
			seedUser(t, 71, 10000)
			seedChannel(t, 71)
			for _, discount := range []float64{1, 0.5} {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest("POST", "/v1/test", nil)
				info := &relaycommon.RelayInfo{UserId: 71, OriginModelName: "discount-statistics", StartTime: time.Now(),
					ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 71}, FinalPreConsumedQuota: common.QuotaFromFloat(1000 * discount),
					PriceData: types.PriceData{UsePrice: true, ModelPrice: 1000 / common.QuotaPerUnit, GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1}}}
				info.PriceData.AddOtherRatio(types.UserModelDiscountRatioKey, discount)
				usage := &dto.Usage{PromptTokens: 100, TotalTokens: 100}
				switch path {
				case "text":
					PostTextConsumeQuota(ctx, info, usage, nil)
				case "audio":
					PostAudioConsumeQuota(ctx, info, usage, "")
				case "realtime":
					PostWssConsumeQuota(ctx, info, info.OriginModelName, &dto.RealtimeUsage{InputTokens: 100, TotalTokens: 100}, "")
				}
			}
			var user model.User
			var channel model.Channel
			require.NoError(t, model.DB.First(&user, 71).Error)
			require.NoError(t, model.DB.First(&channel, 71).Error)
			assert.EqualValues(t, 2000, channel.UsedQuota)
			assert.Equal(t, 1500, user.UsedQuota)
			assert.Equal(t, 2, user.RequestCount)
		})
	}
}

// 折后差额为零也要更新折前渠道统计，且旧轮询对象不能清除数据库中的插件状态。
func TestDiscountTaskZeroDeltaPreservesPluginState(t *testing.T) {
	truncate(t)
	// 隔离导出缓存；零差额日志不得虚增请求数或额度。
	previousExport := common.DataExportEnabled
	common.DataExportEnabled = true
	model.CacheQuotaDataLock.Lock()
	previousCache := model.CacheQuotaData
	model.CacheQuotaData = make(map[string]*model.QuotaData)
	model.CacheQuotaDataLock.Unlock()
	t.Cleanup(func() {
		common.DataExportEnabled = previousExport
		model.CacheQuotaDataLock.Lock()
		model.CacheQuotaData = previousCache
		model.CacheQuotaDataLock.Unlock()
	})
	seedUser(t, 72, 9999)
	seedToken(t, 72, 72, "zero-delta-test", 9999)
	seedChannel(t, 72)
	seedChargedAccounting(t, 72, 72, 72, 1, 1)
	task := makeTask(72, 72, 1, 72, BillingSourceWallet, 0)
	task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(10, 1)
	require.NoError(t, model.DB.Create(task).Error)
	model.UpdateChannelUsedQuota(72, 9)
	stored := *task
	stored.PrivateData.PluginState = []byte(`{"cursor":"newer"}`)
	require.NoError(t, model.DB.Model(task).Update("private_data", stored.PrivateData).Error)
	stale := *task
	RecalculateTaskQuotaWithAmounts(context.Background(), task, 1, types.NewDiscountAmounts(20, 1), "zero delta")
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.JSONEq(t, `{"cursor":"newer"}`, string(stored.PrivateData.PluginState))
	assert.Equal(t, types.NewDiscountAmounts(20, 1), stored.PrivateData.DiscountAmounts)
	assert.EqualValues(t, 20, getChannelUsedQuota(t, 72))
	assert.Equal(t, 9999, getUserQuota(t, 72))
	assert.Equal(t, 9999, getTokenRemainQuota(t, 72))
	var user model.User
	var token model.Token
	require.NoError(t, model.DB.First(&user, 72).Error)
	require.NoError(t, model.DB.First(&token, 72).Error)
	assert.Equal(t, 1, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	assert.Equal(t, 1, token.UsedQuota)
	model.CacheQuotaDataLock.Lock()
	assert.Empty(t, model.CacheQuotaData)
	model.CacheQuotaDataLock.Unlock()
	// 取整后实扣相同也必须留下最终折前/优惠金额，日志额度保持零。
	entry := getLastLog(t)
	require.NotNil(t, entry)
	assert.Equal(t, model.LogTypeConsume, entry.Type)
	assert.Zero(t, entry.Quota)
	var recorded types.DiscountAmounts
	require.NoError(t, common.UnmarshalJsonStr(entry.Other, &recorded))
	assert.Equal(t, types.DiscountAmounts{Before: 20, After: 1, Savings: 19}, recorded)
	RecalculateTaskQuotaWithAmounts(context.Background(), &stored, 1, types.NewDiscountAmounts(20, 1), "retry zero delta")
	assert.EqualValues(t, 1, countLogs(t))
	// 折后相同也不能让旧折前快照覆盖更新后的渠道统计。
	_, err := model.CommitTaskSettlement(context.Background(), &stale, 1, types.NewDiscountAmounts(15, 1))
	require.Error(t, err)
	assert.EqualValues(t, 20, getChannelUsedQuota(t, 72))
	// 快照落库失败时不继续变更渠道，便于重试或人工对账。
	require.NoError(t, model.DB.Delete(task).Error)
	RecalculateTaskQuotaWithAmounts(context.Background(), task, 1, types.NewDiscountAmounts(30, 1), "missing task")
	assert.EqualValues(t, 20, getChannelUsedQuota(t, 72))
	assert.Equal(t, types.NewDiscountAmounts(20, 1), task.PrivateData.DiscountAmounts)
}

// 原价路径保留 rc35 四舍五入规则；原价舍入为零时不能因折扣变成收费。
// 验证终态入口而非仅验证底层计算器，防止其他结算分支继续使用旧浮点折扣。
func TestDiscountTerminalRounding(t *testing.T) {
	for _, test := range []struct {
		name      string
		bps, want int
	}{
		{"adaptor", 2900, 58}, {"tiered", 5750, 3163},
	} {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			const id = 78
			seedUser(t, id, 10000-test.bps)
			seedToken(t, id, id, "deep-terminal-token", 10000-test.bps)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, test.bps, 1)
			model.UpdateChannelUsedQuota(id, 10000-test.bps)
			task := makeTask(id, id, test.bps, id, BillingSourceWallet, 0)
			task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(10000, test.bps)
			task.PrivateData.BillingContext.OtherRatios = map[string]float64{types.UserModelDiscountRatioKey: float64(test.bps) / 10000}
			if test.name == "tiered" {
				task.PrivateData.BillingContext.TieredSnapshot = makeSnapshot(`tier("base",11000)`, 1, 0, 0)
			}
			require.NoError(t, model.DB.Create(task).Error)
			require.True(t, settleTaskBillingOnComplete(context.Background(), &mockAdaptor{adjustReturn: 200}, task, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}))
			t.Logf("terminal branch=%s want=%d charged=%d wallet=%d", test.name, test.want, task.Quota, getUserQuota(t, id))
			assert.Equal(t, test.want, task.Quota)
			assert.Equal(t, 10000-test.want, getUserQuota(t, id))
		})
	}
}

// 在事务写入资金后使最终快照保存失败，确认整笔回滚且重试不得重复扣款。
func TestTaskSettlementRollbackAndRetry(t *testing.T) {
	truncate(t)
	const id = 77
	seedUser(t, id, 9900)
	seedToken(t, id, id, "deep-discount-token", 9900)
	seedChannel(t, id)
	seedChargedAccounting(t, id, id, id, 100, 1)
	model.UpdateChannelUsedQuota(id, 100)
	task := makeTask(id, id, 100, id, BillingSourceWallet, 0)
	task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(200, 100)
	task.PrivateData.BillingContext.OtherRatios = map[string]float64{types.UserModelDiscountRatioKey: 0.5}
	task.PrivateData.BillingContext.TieredSnapshot = makeSnapshot(`tier("final", u("seconds") * 10)`, 1, 0, 0)
	task.PrivateData.BillingContext.TieredSnapshot.TaskUsageBilling = true
	task.PrivateData.BillingContext.TieredSnapshot.QuotaPerUnit = 1
	task.PrivateData.BillingContext.TieredSnapshot.UsageFacts = map[string]any{"seconds": float64(20)}
	task.PrivateData.BillingContext.TieredSnapshot.EstimatedTier = "estimated"
	require.NoError(t, model.DB.Create(task).Error)
	completion := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, UsageFacts: map[string]any{"seconds": float64(40)}}
	const hook = "test:discount-persistence-failure"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		// 仅在资金和统计已经写入事务后的最终快照处失败，避开 SQLite 的取锁语句。
		values, ok := tx.Statement.Dest.(map[string]any)
		if tx.Statement.Table == "tasks" && ok && values["private_data"] != nil {
			tx.AddError(errors.New("injected task snapshot write failure"))
		}
	}))
	t.Cleanup(func() {
		if model.DB.Callback().Update().Get(hook) != nil {
			require.NoError(t, model.DB.Callback().Update().Remove(hook))
		}
	})
	settleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, task, completion)
	require.NoError(t, model.DB.Callback().Update().Remove(hook))
	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, 100, reloaded.Quota)
	assert.Equal(t, types.NewDiscountAmounts(200, 100), reloaded.PrivateData.DiscountAmounts)
	assert.Equal(t, float64(20), reloaded.PrivateData.BillingContext.TieredSnapshot.UsageFacts["seconds"])
	assert.Equal(t, "estimated", task.PrivateData.BillingContext.TieredSnapshot.EstimatedTier)
	assert.Equal(t, float64(20), task.PrivateData.BillingContext.TieredSnapshot.UsageFacts["seconds"])
	assert.Equal(t, 9900, getUserQuota(t, id))
	assert.Equal(t, 9900, getTokenRemainQuota(t, id))
	assert.EqualValues(t, 200, getChannelUsedQuota(t, id))
	assert.Zero(t, countLogs(t))
	settleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, &reloaded, completion)
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.Equal(t, float64(40), reloaded.PrivateData.BillingContext.TieredSnapshot.UsageFacts["seconds"])
	assert.Equal(t, "final", reloaded.PrivateData.BillingContext.TieredSnapshot.EstimatedTier)
	assert.Equal(t, 9800, getUserQuota(t, id))
	assert.Equal(t, 9800, getTokenRemainQuota(t, id))
	assert.EqualValues(t, 400, getChannelUsedQuota(t, id))
	assert.EqualValues(t, 1, countLogs(t))
}

// 多个轮询者持有同一旧快照时，只允许一笔真实差额及一条日志，不能靠内存 task 指针去重。
func TestTaskSettlementConcurrentCopies(t *testing.T) {
	for _, target := range []int{100, 200} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			truncate(t)
			const id = 79
			seedUser(t, id, 9900)
			seedToken(t, id, id, "concurrent-settlement-test", 9900)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, 100, 1)
			model.UpdateChannelUsedQuota(id, 100)
			task := makeTask(id, id, 100, id, BillingSourceWallet, 0)
			task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(200, 100)
			require.NoError(t, model.DB.Create(task).Error)
			var workers sync.WaitGroup
			for range 32 {
				copy := *task
				workers.Add(1)
				go func() {
					defer workers.Done()
					RecalculateTaskQuotaWithAmounts(context.Background(), &copy, target, types.NewDiscountAmounts(400, target), "concurrent retry")
				}()
			}
			workers.Wait()
			assert.Equal(t, 10000-target, getUserQuota(t, id))
			assert.Equal(t, 10000-target, getTokenRemainQuota(t, id))
			assert.EqualValues(t, 400, getChannelUsedQuota(t, id))
			assert.EqualValues(t, 1, countLogs(t))
		})
	}
}

// 金额相同仍需持久化实际用量；并发旧副本只能提交同一目标，不能覆盖更新后的结算版本。
func TestTaskSettlementUsageOnly(t *testing.T) {
	truncate(t)
	const id = 80
	seedUser(t, id, 9950)
	seedToken(t, id, id, "usage-only-test", 9950)
	seedChannel(t, id)
	seedChargedAccounting(t, id, id, id, 50, 1)
	model.UpdateChannelUsedQuota(id, 50)
	task := makeTask(id, id, 50, id, BillingSourceWallet, 0)
	task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(100, 50)
	task.PrivateData.BillingContext.OtherRatios = map[string]float64{types.UserModelDiscountRatioKey: 0.5}
	snapshot := makeSnapshot(`u("resolution") == "1080P" ? tier("1080P", 100) : tier("720P", 100)`, 1, 0, 0)
	snapshot.TaskUsageBilling = true
	snapshot.QuotaPerUnit = 1
	snapshot.UsageFacts = map[string]any{"seconds": 5, "resolution": "720P"}
	snapshot.EstimatedTier = "720P"
	task.PrivateData.BillingContext.TieredSnapshot = snapshot
	require.NoError(t, model.DB.Create(task).Error)
	stored := *task
	stored.PrivateData.PluginState = []byte(`{"cursor":"latest"}`)
	require.NoError(t, model.DB.Model(task).Update("private_data", stored.PrivateData).Error)
	var workers sync.WaitGroup
	for range 8 {
		copy := *task
		workers.Add(1)
		go func() {
			defer workers.Done()
			settleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, &copy, &relaycommon.TaskInfo{
				Status: model.TaskStatusSuccess, UsageFacts: map[string]any{"resolution": "1080P"},
			})
		}()
	}
	workers.Wait()
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	wantSnapshot := *snapshot
	wantSnapshot.UsageFacts = map[string]any{"seconds": float64(5), "resolution": "1080P"}
	wantSnapshot.EstimatedTier = "1080P"
	assert.Equal(t, &wantSnapshot, stored.PrivateData.BillingContext.TieredSnapshot)
	assert.JSONEq(t, `{"cursor":"latest"}`, string(stored.PrivateData.PluginState))
	assert.Equal(t, task.PrivateData.DiscountAmounts, stored.PrivateData.DiscountAmounts)
	assert.EqualValues(t, 1, countLogs(t))
	entry := getLastLog(t)
	require.NotNil(t, entry)
	assert.Equal(t, model.LogTypeConsume, entry.Type)
	assert.Zero(t, entry.Quota)
	var details map[string]any
	require.NoError(t, common.UnmarshalJsonStr(entry.Other, &details))
	assert.Equal(t, "1080P", details["matched_tier"])
	assert.Equal(t, wantSnapshot.UsageFacts, details["usage_facts"])

	// 重载后同目标为幂等成功；旧快照不同目标和冻结价格变动都必须拒绝。
	result, err := model.CommitTaskSettlementWithSnapshot(t.Context(), &stored, 50, types.NewDiscountAmounts(100, 50), &wantSnapshot)
	require.NoError(t, err)
	assert.False(t, result.Updated)
	assert.Zero(t, result.QuotaDelta)
	conflict := wantSnapshot
	conflict.UsageFacts = map[string]any{"resolution": "4K"}
	_, err = model.CommitTaskSettlementWithSnapshot(t.Context(), task, 50, types.NewDiscountAmounts(100, 50), &conflict)
	require.ErrorContains(t, err, "reload before retry")
	changedPrice := stored
	changedBilling := *stored.PrivateData.BillingContext
	changedPrice.PrivateData.BillingContext = &changedBilling
	changedBilling.GroupRatio = 2
	_, err = model.CommitTaskSettlementWithSnapshot(t.Context(), &changedPrice, 50, types.NewDiscountAmounts(100, 50), &wantSnapshot)
	require.ErrorContains(t, err, "reload before retry")
	// 只采纳用量与档位，即使传入副本改变价格也不覆盖提交时冻结的参数。
	alteredInput := wantSnapshot
	alteredInput.ExprString = "999999"
	alteredInput.GroupRatio = 9
	result, err = model.CommitTaskSettlementWithSnapshot(t.Context(), &stored, 50, types.NewDiscountAmounts(100, 50), &alteredInput)
	require.NoError(t, err)
	assert.False(t, result.Updated)
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.Equal(t, &wantSnapshot, stored.PrivateData.BillingContext.TieredSnapshot)
	assert.Equal(t, 9950, getUserQuota(t, id))
	assert.Equal(t, 9950, getTokenRemainQuota(t, id))
	assert.EqualValues(t, 100, getChannelUsedQuota(t, id))
	var user model.User
	require.NoError(t, model.DB.First(&user, id).Error)
	assert.Equal(t, 50, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
}

func TestAudioDiscountPreservesOriginalRounding(t *testing.T) {
	for _, tc := range []struct {
		before                float64
		discount              float64
		wantBefore, wantAfter int
	}{
		{0.4, 1, 0, 0}, {0.4, 0.8, 0, 0}, {0.6, 0.1, 1, 1}, {1.2, 0.0001, 1, 1}, {5.9, 0.8, 6, 5},
	} {
		info := QuotaInfo{UsePrice: true, ModelPrice: tc.before / common.QuotaPerUnit, GroupRatio: 1, ModelDiscount: tc.discount}
		before, after, clamp := calculateAudioQuotaAmounts(info)
		assert.Equal(t, tc.wantBefore, before)
		assert.Equal(t, tc.wantAfter, after)
		assert.Nil(t, clamp)
	}
}
