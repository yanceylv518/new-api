package relay

import (
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// remix 重建基础价格后，仍应保留原任务的时长和分辨率倍率，并使用当前折扣。
func TestMergeInheritedTaskBillingRatiosKeepsCurrentDiscount(t *testing.T) {
	priceData := types.PriceData{}
	priceData.AddOtherRatio(types.UserModelDiscountRatioKey, 0.5)

	mergeInheritedTaskBillingRatios(&priceData, map[string]float64{
		"seconds":                       8,
		"size":                          1.666667,
		types.UserModelDiscountRatioKey: 0.25,
	})

	require.Equal(t, 8.0, priceData.OtherRatios()["seconds"])
	assert.Equal(t, 1.666667, priceData.OtherRatios()["size"])
	assert.Equal(t, 0.5, priceData.UserModelDiscountMultiplier())
}
