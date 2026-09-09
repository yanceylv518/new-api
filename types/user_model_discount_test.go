package types

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 构造参数和导出的可变副本不能污染请求共享的只读规则。
func TestUserModelDiscountSnapshotOwnership(t *testing.T) {
	rules := map[string]int{"Model": 8000, "model": 6000}
	snapshot := NewUserModelDiscountSnapshot(rules)
	rules["Model"] = 1
	copy := snapshot.Copy()
	copy["model"] = 1
	assert.Equal(t, 8000, snapshot.DiscountBPS("Model"))
	assert.Equal(t, 6000, snapshot.DiscountBPS("model"))
	assert.Zero(t, snapshot.DiscountBPS("missing"))
	assert.Zero(t, (UserModelDiscountSnapshot{}).DiscountBPS("model"))
}

// 比较实际规则查询操作的分配，不执行网络或数据库 I/O；两组使用相同规则和查询。
func BenchmarkUserModelDiscountLookup(b *testing.B) {
	for _, count := range []int{1, 100, 1000} {
		rules := make(map[string]int, count)
		for index := range count {
			rules[fmt.Sprintf("model-%04d", index)] = 8000
		}
		snapshot := NewUserModelDiscountSnapshot(rules)
		b.Run(fmt.Sprintf("rules=%d/shared", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if snapshot.DiscountBPS("model-0000") != 8000 {
					b.Fatal("incorrect price")
				}
			}
		})
		b.Run(fmt.Sprintf("rules=%d/copy", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if snapshot.Copy()["model-0000"] != 8000 {
					b.Fatal("incorrect price")
				}
			}
		})
	}
}
