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
		assert.True(t, amounts.ValidFor(values[1]))
		assert.Equal(t, values[0], amounts.ChannelQuota(values[1]))
	}
	for _, values := range [][2]int{{-1, 0}, {1, -1}, {1, 2}} {
		assert.Nil(t, NewDiscountAmounts(values[0], values[1]))
	}
}

// 从 JSON 读出的字段不会经过构造函数；损坏或不属于本次扣款的快照不能用于渠道原价。
func TestDiscountAmountsRejectCorruptPersistedValues(t *testing.T) {
	for _, amounts := range []*DiscountAmounts{
		nil,
		{Before: 100, After: 20, Savings: 999},
		{Before: 100, After: 19, Savings: 81},
		{Before: -1, After: 20, Savings: -21},
		{Before: 2147483648, After: 20, Savings: 2147483628},
	} {
		assert.False(t, amounts.ValidFor(20))
		assert.Equal(t, 20, amounts.ChannelQuota(20))
	}
}
