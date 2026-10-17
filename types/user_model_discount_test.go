package types

import (
	"fmt"
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserModelDiscountTemporaryFallbackFollowsPermanentSnapshot(t *testing.T) {
	end := int64(200)
	makeSnapshot := func(bps int, expression string) UserModelDiscountSnapshot {
		var permanent *UserModelDiscountWindow
		if bps != 10000 {
			permanent = &UserModelDiscountWindow{DiscountBPS: bps}
		}
		return NewUserModelDiscountScheduleSnapshot(map[string]UserModelDiscountConfig{"model": {
			Mode: UserModelPricingLayered, Permanent: permanent,
			Periods: []UserModelDiscountWindow{{DiscountBPS: 10000, StartTime: 100, EndTime: &end, Expression: expression}},
		}})
	}
	expression := `(hour("Asia/Shanghai") < 0) ? 0.6 : permanent_discount`
	oldRule, source := makeSnapshot(8000, expression).RuleAt("model", 150)
	old := UserModelDiscountEvaluation{Rule: oldRule, Source: source, Context: discountexpr.Context{At: 150}}
	for _, bps := range []int{7500, 2900, 1, 10000} {
		fresh := makeSnapshot(bps, expression)
		rule, source := fresh.RuleAt("model", 150)
		evaluation := UserModelDiscountEvaluation{Rule: rule, Source: source, Context: discountexpr.Context{At: 150}}
		got, err := evaluation.Evaluate()
		require.NoError(t, err)
		assert.Equal(t, bps, got)
		got, err = old.Evaluate()
		require.NoError(t, err)
		assert.Equal(t, 8000, got)
		after, _ := fresh.RuleAt("model", 200)
		assert.Equal(t, bps, after.DiscountBPS)
	}
	matchedRule, source := makeSnapshot(7500, `(hour("Asia/Shanghai") >= 0) ? 0.6 : permanent_discount`).RuleAt("model", 150)
	matched := UserModelDiscountEvaluation{Rule: matchedRule, Source: source, Context: discountexpr.Context{At: 150}}
	got, err := matched.Evaluate()
	require.NoError(t, err)
	assert.Equal(t, 6000, got)
	matched.Rule.PermanentDiscountBPS = 0
	_, err = matched.Evaluate()
	require.Error(t, err)
}

func TestUserModelDiscountLayeredBoundariesAndFrozenOwnership(t *testing.T) {
	end1, end2, end3 := int64(200), int64(300), int64(450)
	permanent := UserModelDiscountWindow{DiscountBPS: 8000}
	snapshot := NewUserModelDiscountScheduleSnapshot(map[string]UserModelDiscountConfig{"model": {Mode: UserModelPricingLayered, Permanent: &permanent, Periods: []UserModelDiscountWindow{
		{DiscountBPS: 5000, StartTime: 100, EndTime: &end1}, {DiscountBPS: 6000, StartTime: 200, EndTime: &end2}, {DiscountBPS: 10000, StartTime: 400, EndTime: &end3},
	}}})
	for _, tc := range []struct {
		at     int64
		bps    int
		source string
	}{{99, 8000, "permanent"}, {100, 5000, "temporary"}, {199, 5000, "temporary"}, {200, 6000, "temporary"}, {299, 6000, "temporary"}, {300, 8000, "permanent"}, {399, 8000, "permanent"}, {400, 10000, "temporary"}, {450, 8000, "permanent"}} {
		rule, source := snapshot.RuleAt("model", tc.at)
		assert.Equal(t, tc.bps, rule.DiscountBPS)
		assert.Equal(t, tc.source, source)
	}
	frozen, _ := snapshot.RuleAt("model", 199)
	permanent.DiscountBPS = 1
	end1 = 101
	exported := snapshot.Configurations()
	exported["model"].Permanent.DiscountBPS = 1
	*exported["model"].Periods[0].EndTime = 101
	assert.Equal(t, 5000, snapshot.DiscountBPSAt("model", 199))
	assert.Equal(t, 8000, snapshot.DiscountBPSAt("model", 450))
	assert.Equal(t, int64(200), *frozen.EndTime)
}

func TestUserModelDiscountLayeredExpressionWithActualUsage(t *testing.T) {
	end := int64(200)
	snapshot := NewUserModelDiscountScheduleSnapshot(map[string]UserModelDiscountConfig{"model": {Mode: UserModelPricingLayered, Permanent: &UserModelDiscountWindow{DiscountBPS: 10000, Expression: "0.8"}, Periods: []UserModelDiscountWindow{{DiscountBPS: 10000, Expression: "output_tokens >= 500 ? 0.5 : 0.6", StartTime: 100, EndTime: &end}}}})
	rule, source := snapshot.RuleAt("model", 199)
	evaluation := UserModelDiscountEvaluation{Rule: rule, Source: source, Context: discountexpr.Context{At: 199, OutputTokens: 100}}
	bps, err := evaluation.Evaluate()
	assert.NoError(t, err)
	assert.Equal(t, 6000, bps)
	evaluation.Context.OutputTokens = 500
	bps, err = evaluation.Evaluate()
	assert.NoError(t, err)
	assert.Equal(t, 5000, bps)
	next, _ := snapshot.RuleAt("model", 200)
	assert.Equal(t, "0.8", next.Expression)
	assert.Equal(t, "temporary", evaluation.Source)
}

func TestUserModelDiscountLayeredParallelDenseLookup(t *testing.T) {
	snapshots := make([]UserModelDiscountSnapshot, 1000)
	for user := range 1000 {
		windows := make([]UserModelDiscountWindow, 1000)
		for i := range windows {
			end := int64(i*20 + 10)
			windows[i] = UserModelDiscountWindow{StartTime: int64(i * 20), EndTime: &end, DiscountBPS: 1000 + user}
		}
		snapshots[user] = NewUserModelDiscountScheduleSnapshot(map[string]UserModelDiscountConfig{"model": {Mode: UserModelPricingLayered, Permanent: &UserModelDiscountWindow{DiscountBPS: 8000}, Periods: windows}})
	}
	var workers sync.WaitGroup
	for user := range 1000 {
		workers.Go(func() {
			for i := range 100 {
				assert.Equal(t, 1000+user, snapshots[user].DiscountBPSAt("model", int64(i*20)))
				assert.Equal(t, 8000, snapshots[user].DiscountBPSAt("model", int64(i*20+10)))
			}
		})
	}
	workers.Wait()
}

func BenchmarkUserModelDiscountLayeredLookup(b *testing.B) {
	windows := make([]UserModelDiscountWindow, 1000)
	for i := range windows {
		end := int64(i*20 + 10)
		windows[i] = UserModelDiscountWindow{StartTime: int64(i * 20), EndTime: &end, DiscountBPS: 5000}
	}
	snapshot := NewUserModelDiscountScheduleSnapshot(map[string]UserModelDiscountConfig{"model": {Mode: UserModelPricingLayered, Permanent: &UserModelDiscountWindow{DiscountBPS: 8000}, Periods: windows}})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			snapshot.DiscountBPSAt("model", 10000)
		}
	})
}

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

func TestUserModelDiscountScheduleBoundariesAndOwnership(t *testing.T) {
	firstEnd, secondEnd := int64(200), int64(300)
	configs := map[string]UserModelDiscountConfig{"model": {Mode: UserModelPricingScheduled, Periods: []UserModelDiscountWindow{
		{DiscountBPS: 6000, StartTime: 200, EndTime: &secondEnd},
		{DiscountBPS: 4000, StartTime: 400},
		{DiscountBPS: 8000, StartTime: 100, EndTime: &firstEnd},
	}}}
	snapshot := NewUserModelDiscountScheduleSnapshot(configs)
	for _, tc := range []struct {
		at   int64
		bps  int
		next int64
	}{
		{99, 0, 100}, {100, 8000, 200}, {199, 8000, 200},
		{200, 6000, 300}, {299, 6000, 300}, {300, 0, 400},
		{399, 0, 400}, {400, 4000, 0}, {253402300799, 4000, 0},
	} {
		assert.Equal(t, tc.bps, snapshot.DiscountBPSAt("model", tc.at), "at=%d", tc.at)
		assert.Equal(t, tc.next, snapshot.NextChangeAt("model", tc.at), "at=%d", tc.at)
	}
	firstEnd = 101
	configs["model"].Periods[2].DiscountBPS = 1
	exported := snapshot.Configurations()
	*exported["model"].Periods[0].EndTime = 101
	assert.Equal(t, 8000, snapshot.DiscountBPSAt("model", 199))
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
