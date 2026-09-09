package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 优惠金额必须来自两个独立取整结果；零费用合法，不能产生负优惠或溢出。
func TestDiscountAmountsAudit(t *testing.T) {
	for _, values := range [][2]int{{100, 81}, {4000, 1}, {0, 0}, {2147483647, 1}} {
		amounts := NewDiscountAmounts(values[0], values[1])
		require.NotNil(t, amounts)
		assert.Equal(t, values[0], amounts.Before)
		assert.Equal(t, values[1], amounts.After)
		assert.Equal(t, values[0]-values[1], amounts.Savings)
	}
	for _, values := range [][2]int{{-1, 0}, {1, -1}, {1, 2}} {
		assert.Nil(t, NewDiscountAmounts(values[0], values[1]))
	}
}
