package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 空的附加倍率也必须生成可持久化的任务计费快照，不能因原价请求触发 nil map panic。
func TestBuildTaskBillingOtherRatiosInitializesEmptyMap(t *testing.T) {
	priceData := types.PriceData{}

	ratios := buildTaskBillingOtherRatios(&priceData)

	require.NotNil(t, ratios)
	assert.Equal(t, 1.0, ratios[types.UserModelDiscountRatioKey])
}

// 已有视频倍率时，构造快照还必须保留原倍率并追加用户折扣。
func TestBuildTaskBillingOtherRatiosPreservesExistingRatios(t *testing.T) {
	priceData := types.PriceData{}
	priceData.AddOtherRatio("video_input", 0.5)

	ratios := buildTaskBillingOtherRatios(&priceData)

	require.Len(t, ratios, 2)
	assert.Equal(t, 0.5, ratios["video_input"])
	assert.Equal(t, 1.0, ratios[types.UserModelDiscountRatioKey])
}
