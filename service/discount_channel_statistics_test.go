package service

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	seedUser(t, 72, 10000)
	seedChannel(t, 72)
	task := makeTask(72, 72, 1, 0, BillingSourceWallet, 0)
	task.PrivateData.DiscountAmounts = types.NewDiscountAmounts(10, 1)
	require.NoError(t, model.DB.Create(task).Error)
	model.UpdateChannelUsedQuota(72, 10)
	stored := *task
	stored.PrivateData.PluginState = []byte(`{"cursor":"newer"}`)
	require.NoError(t, model.DB.Model(task).Update("private_data", stored.PrivateData).Error)
	RecalculateTaskQuotaWithAmounts(context.Background(), task, 1, types.NewDiscountAmounts(20, 1), "zero delta")
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.JSONEq(t, `{"cursor":"newer"}`, string(stored.PrivateData.PluginState))
	assert.Equal(t, types.NewDiscountAmounts(20, 1), stored.PrivateData.DiscountAmounts)
	assert.EqualValues(t, 20, getChannelUsedQuota(t, 72))
	assert.Equal(t, 10000, getUserQuota(t, 72))
	// 快照落库失败时不继续变更渠道，便于重试或人工对账。
	require.NoError(t, model.DB.Delete(task).Error)
	RecalculateTaskQuotaWithAmounts(context.Background(), task, 1, types.NewDiscountAmounts(30, 1), "missing task")
	assert.EqualValues(t, 20, getChannelUsedQuota(t, 72))
	assert.Equal(t, types.NewDiscountAmounts(20, 1), task.PrivateData.DiscountAmounts)
}

// 原价路径保留 rc35 四舍五入规则；原价舍入为零时不能因折扣变成收费。
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
