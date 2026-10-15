package service

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserModelDiscountPerCallTimeRuleKeepsSubmittedAmount(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after int
		price, size   float64
	}{
		{"fractional_base", 9, 7, 9.99 / common.QuotaPerUnit, 1},
		{"patched_per_call_ignores_size", 100, 80, 100 / common.QuotaPerUnit, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			seedUser(t, 71, 10000-tc.after)
			seedToken(t, 71, 71, "time-rule-test", 10000-tc.after)
			seedChannel(t, 71)
			seedChargedAccounting(t, 71, 71, 71, tc.after, 1)
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 71).Update("used_quota", tc.before).Error)
			task := makeTask(71, 71, tc.after, 71, BillingSourceWallet, 0)
			task.Status = model.TaskStatusSuccess
			task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(tc.before, tc.after)
			bc := task.PrivateData.BillingContext
			bc.PerCallBilling, bc.ModelPrice = true, tc.price
			bc.OtherRatios = map[string]float64{types.UserModelDiscountRatioKey: 0.8, "size": tc.size}
			bc.UserDiscount = &types.UserModelDiscountEvaluation{Rule: types.UserModelDiscountWindow{Expression: `permanent_discount`, PermanentDiscountBPS: 8000}, DiscountBPS: 8000, Source: "temporary", Context: discountexpr.Context{At: 199}}
			persistTaskForBillingTest(t, task)
			for attempt := range 2 {
				var restored model.Task
				require.NoError(t, model.DB.First(&restored, task.ID).Error)
				settled := settleTaskBillingOnComplete(t.Context(), &mockAdaptor{adjustReturn: 2000}, &restored, &relaycommon.TaskInfo{TotalTokens: 9999})
				assert.False(t, settled, fmt.Sprint("attempt ", attempt))
				assert.Equal(t, tc.after, restored.Quota)
				assert.Equal(t, task.PrivateData.DiscountAmounts, restored.PrivateData.DiscountAmounts)
			}
			assert.Equal(t, 10000-tc.after, getUserQuota(t, 71))
			assert.Equal(t, 10000-tc.after, getTokenRemainQuota(t, 71))
			assert.Zero(t, countLogs(t))
			var channel model.Channel
			require.NoError(t, model.DB.First(&channel, 71).Error)
			assert.EqualValues(t, tc.before, channel.UsedQuota)
		})
	}
}

func TestUserModelDiscountPerCallUsageSettlesSubmittedBasis(t *testing.T) {
	for _, source := range []string{BillingSourceWallet, BillingSourceSubscription} {
		for _, tc := range []struct {
			name, basis, expression string
			before, precharge, want int
			wantBPS                 int
		}{
			{"legacy_fixed_price", "", "task_tokens >= 5000 ? 0.5 : 0.8", 100, 80, 50, 5000},
			{"fixed_price", "100", "task_tokens >= 5000 ? 0.5 : 0.8", 100, 80, 50, 5000},
			{"fractional_basis", "10.8", "task_tokens >= 5000 ? 0.5 : 0.8", 11, 9, 5, 5000},
			{"legacy_fractional_basis", "", "task_tokens >= 5000 ? 0.5 : 0.8", 11, 9, 6, 5000},
			{"adjusted_submission_basis", "300", "task_tokens >= 5000 ? 0.5 : 0.8", 300, 240, 150, 5000},
			{"minimum_charge", "1", "task_tokens >= 5000 ? 0.01 : 0.8", 1, 1, 1, 100},
			{"invalid_basis", "invalid", "task_tokens >= 5000 ? 0.5 : 0.8", 100, 80, 80, 8000},
			{"negative_basis", "-100", "task_tokens >= 5000 ? 0.5 : 0.8", 100, 80, 80, 8000},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				truncate(t)
				const id = 74
				wallet := 10000
				subscriptionID := 0
				if source == BillingSourceWallet {
					wallet -= tc.precharge
				} else {
					subscriptionID = id
					seedSubscription(t, id, id, 10000, int64(tc.precharge))
				}
				seedUser(t, id, wallet)
				seedToken(t, id, id, "per-call-usage-test", 10000-tc.precharge)
				seedChannel(t, id)
				seedChargedAccounting(t, id, id, id, tc.precharge, 1)
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", id).Update("used_quota", tc.before).Error)
				task := makeTask(id, id, tc.precharge, id, source, subscriptionID)
				task.Status = model.TaskStatusSuccess
				task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(tc.before, tc.precharge)
				task.PrivateData.PluginState = []byte(`{"cursor":"retained"}`)
				bc := task.PrivateData.BillingContext
				bc.PerCallBilling, bc.QuotaBeforeDiscount = true, tc.basis
				bc.ModelPrice = 100 / common.QuotaPerUnit
				bc.OtherRatios = map[string]float64{types.UserModelDiscountRatioKey: 0.8, "size": 2}
				bc.UserDiscount = &types.UserModelDiscountEvaluation{
					Rule: types.UserModelDiscountWindow{Expression: tc.expression}, DiscountBPS: 8000,
					Source: "temporary", Context: discountexpr.Context{At: 199},
				}
				persistTaskForBillingTest(t, task)
				for range 2 {
					var restored model.Task
					require.NoError(t, model.DB.First(&restored, task.ID).Error)
					assert.Equal(t, tc.basis, restored.PrivateData.BillingContext.QuotaBeforeDiscount)
					require.True(t, settleTaskBillingOnComplete(t.Context(), &mockAdaptor{adjustReturn: 2000}, &restored, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, TotalTokens: 5000}))
					require.NoError(t, model.DB.First(&restored, task.ID).Error)
					assert.Equal(t, tc.want, restored.Quota)
					assert.Equal(t, types.NewDiscountAmounts(tc.before, tc.want), restored.PrivateData.DiscountAmounts)
					assert.Equal(t, tc.wantBPS, restored.PrivateData.BillingContext.UserDiscount.DiscountBPS)
					assert.Equal(t, tc.basis, restored.PrivateData.BillingContext.QuotaBeforeDiscount)
					assert.JSONEq(t, `{"cursor":"retained"}`, string(restored.PrivateData.PluginState))
				}
				if source == BillingSourceWallet {
					assert.Equal(t, 10000-tc.want, getUserQuota(t, id))
				} else {
					assert.Equal(t, 10000, getUserQuota(t, id))
					assert.EqualValues(t, tc.want, getSubscriptionUsed(t, id))
				}
				assert.Equal(t, 10000-tc.want, getTokenRemainQuota(t, id))
				assert.Equal(t, tc.want, getTokenUsedQuota(t, id))
				used, requests := getUserUsageAccounting(t, id)
				assert.Equal(t, tc.want, used)
				assert.Equal(t, 1, requests)
				assert.EqualValues(t, tc.before, getChannelUsedQuota(t, id))
				if tc.wantBPS == 8000 {
					assert.Zero(t, countLogs(t))
					return
				}
				assert.EqualValues(t, 1, countLogs(t))
				entry := getLastLog(t)
				require.NotNil(t, entry)
				assert.Equal(t, tc.precharge-tc.want, entry.Quota)
				if tc.precharge > tc.want {
					assert.Equal(t, model.LogTypeRefund, entry.Type)
				} else {
					assert.Equal(t, model.LogTypeConsume, entry.Type)
				}
				var amounts types.DiscountAmounts
				require.NoError(t, common.UnmarshalJsonStr(entry.Other, &amounts))
				assert.Equal(t, *types.NewDiscountAmounts(tc.before, tc.want), amounts)
			})
		}
	}
}

func TestUserModelDiscountTaskKeepsSerializedPermanentFallback(t *testing.T) {
	submitted := &types.UserModelDiscountEvaluation{Rule: types.UserModelDiscountWindow{
		Expression: `(hour("Asia/Shanghai") < 0) ? 0.6 : permanent_discount`, PermanentDiscountBPS: 8000,
	}, Source: "temporary", Context: discountexpr.Context{At: 199}, DiscountBPS: 8000}
	task := &model.Task{PrivateData: model.TaskPrivateData{BillingContext: &model.TaskBillingContext{UserDiscount: submitted}}}
	encoded, err := common.Marshal(task.PrivateData)
	require.NoError(t, err)
	restored := &model.Task{}
	require.NoError(t, common.Unmarshal(encoded, &restored.PrivateData))
	submitted.Rule.PermanentDiscountBPS = 7500
	_, evaluation, err := taskUserModelDiscount(restored, map[string]any{"task_tokens": 10000}, -1, -1)
	require.NoError(t, err)
	require.NotNil(t, evaluation)
	assert.Equal(t, 8000, evaluation.DiscountBPS)
	assert.Equal(t, 8000, evaluation.Rule.PermanentDiscountBPS)
}

func TestUserModelDiscountPerCallFullPriceRounding(t *testing.T) {
	for _, tc := range []struct {
		name, basis                                             string
		initialBPS, finalBPS, before, charged, wantBefore, want int
	}{
		{"unchanged_full_price", "10.8", 10000, 10000, 10, 10, 10, 10},
		{"returns_to_full_price", "10.8", 8000, 10000, 11, 9, 10, 10},
		{"full_price_becomes_discounted", "10.8", 10000, 5000, 10, 10, 11, 5},
		{"legacy_unchanged_discount", "", 8000, 8000, 10, 9, 10, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			const id = 75
			seedUser(t, id, 10000-tc.charged)
			seedToken(t, id, id, "rounding-regression", 10000-tc.charged)
			seedChannel(t, id)
			seedChargedAccounting(t, id, id, id, tc.charged, 1)
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", id).Update("used_quota", tc.before).Error)
			task := makeTask(id, id, tc.charged, id, BillingSourceWallet, 0)
			task.Status = model.TaskStatusSuccess
			task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(tc.before, tc.charged)
			bc := task.PrivateData.BillingContext
			bc.PerCallBilling, bc.QuotaBeforeDiscount = true, tc.basis
			bc.UserDiscount = &types.UserModelDiscountEvaluation{
				Rule:        types.UserModelDiscountWindow{Expression: fmt.Sprintf("task_tokens > 0 ? %g : %g", float64(tc.finalBPS)/10000, float64(tc.initialBPS)/10000)},
				DiscountBPS: tc.initialBPS, Context: discountexpr.Context{At: 199},
			}
			persistTaskForBillingTest(t, task)
			for range 2 {
				var restored model.Task
				require.NoError(t, model.DB.First(&restored, task.ID).Error)
				require.True(t, settleTaskBillingOnComplete(t.Context(), &mockAdaptor{}, &restored, &relaycommon.TaskInfo{TotalTokens: 4000}))
				assert.Equal(t, tc.want, restored.Quota)
				assert.Equal(t, types.NewDiscountAmounts(tc.wantBefore, tc.want), restored.PrivateData.DiscountAmounts)
			}
			assert.Equal(t, 10000-tc.want, getUserQuota(t, id))
			assert.Equal(t, 10000-tc.want, getTokenRemainQuota(t, id))
			assert.EqualValues(t, tc.wantBefore, getChannelUsedQuota(t, id))
			assert.EqualValues(t, 1, countLogs(t))
		})
	}
}

func TestUserModelDiscountTaskSettlesFrozenFallbackAcrossPaths(t *testing.T) {
	for _, path := range []string{"tokens", "adaptor", "tiered"} {
		t.Run(path, func(t *testing.T) {
			truncate(t)
			seedUser(t, 73, 9920)
			seedToken(t, 73, 73, "task-frozen-fallback-test", 9920)
			seedChannel(t, 73)
			seedChargedAccounting(t, 73, 73, 73, 80, 1)
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 73).Update("used_quota", 100).Error)
			task := makeTask(73, 73, 80, 73, BillingSourceWallet, 0)
			task.Status = model.TaskStatusSuccess
			task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(100, 80)
			bc := task.PrivateData.BillingContext
			bc.ModelRatio = 2
			bc.OtherRatios = map[string]float64{types.UserModelDiscountRatioKey: 0.8}
			bc.UserDiscount = &types.UserModelDiscountEvaluation{
				Rule:        types.UserModelDiscountWindow{Expression: `(hour("Asia/Shanghai") < 0) ? 0.6 : permanent_discount`, PermanentDiscountBPS: 8000},
				DiscountBPS: 8000, Source: "temporary", Context: discountexpr.Context{At: 199},
			}
			adaptor := &mockAdaptor{}
			result := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, TotalTokens: 100}
			if path == "adaptor" {
				adaptor.adjustReturn = 200
			}
			if path == "tiered" {
				bc.TieredSnapshot = &billingexpr.BillingSnapshot{ExprString: `tier("final", u("seconds") * 100)`, GroupRatio: 1, QuotaPerUnit: 1, TaskUsageBilling: true, UsageFacts: map[string]any{"seconds": float64(1)}}
				result.UsageFacts = map[string]any{"seconds": float64(2)}
			}
			persistTaskForBillingTest(t, task)
			bc.UserDiscount.Rule.PermanentDiscountBPS = 7500
			for range 2 {
				var restored model.Task
				require.NoError(t, model.DB.First(&restored, task.ID).Error)
				require.True(t, settleTaskBillingOnComplete(t.Context(), adaptor, &restored, result))
				require.NoError(t, model.DB.First(&restored, task.ID).Error)
				assert.Equal(t, 160, restored.Quota)
				assert.Equal(t, types.NewDiscountAmounts(200, 160), restored.PrivateData.DiscountAmounts)
				assert.Equal(t, 8000, restored.PrivateData.BillingContext.UserDiscount.Rule.PermanentDiscountBPS)
				assert.Equal(t, int64(199), restored.PrivateData.BillingContext.UserDiscount.Context.At)
			}
			assert.Equal(t, 9840, getUserQuota(t, 73))
			assert.Equal(t, 9840, getTokenRemainQuota(t, 73))
			assert.EqualValues(t, 1, countLogs(t))
			entry := getLastLog(t)
			require.NotNil(t, entry)
			assert.Equal(t, 80, entry.Quota)
			var amounts types.DiscountAmounts
			require.NoError(t, common.UnmarshalJsonStr(entry.Other, &amounts))
			assert.Equal(t, types.DiscountAmounts{Before: 200, After: 160, Savings: 40}, amounts)
			var channel model.Channel
			require.NoError(t, model.DB.First(&channel, 73).Error)
			assert.EqualValues(t, 200, channel.UsedQuota)
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
			price, evaluation, err := taskUserModelDiscount(task, tc.facts, -1, -1)
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
