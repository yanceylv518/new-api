package discountexpr

import (
	"github.com/stretchr/testify/require"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiscountExpressionValues(t *testing.T) {
	cases := []struct {
		source  string
		context Context
		bps     int
	}{
		{"0.29", Context{}, 2900}, {"0.0001", Context{}, 1}, {"1", Context{}, 10000}, {"0.33335", Context{}, 3334},
		{"output_tokens >= 10000 ? 0.6 : 0.8", Context{OutputTokens: 9999}, 8000},
		{"output_tokens >= 10000 ? 0.6 : 0.8", Context{OutputTokens: 10000}, 6000},
		{"(param(\"service_tier\") != nil && param(\"service_tier\") == \"flex\") ? 0.5 : 0.8", Context{}, 8000},
		{"(param(\"service_tier\") != nil && param(\"service_tier\") == \"flex\") ? 0.5 : 0.8", Context{Params: map[string]any{"service_tier": "flex"}}, 5000},
		{"(param(\"n\") != nil && param(\"n\") >= 2) ? 0.5 : 0.8", Context{}, 8000},
		{"(param(\"n\") != nil && param(\"n\") >= 2) ? 0.5 : 0.8", Context{Params: map[string]any{"n": float64(2)}}, 5000},
		{"(param(\"generate_audio\") != nil && param(\"generate_audio\") == true) ? 0.7 : 0.8", Context{Params: map[string]any{"generate_audio": true}}, 7000},
		{"task_tokens >= 5000 ? 0.5 : 0.8", Context{TaskTokens: 5000}, 5000},
		{"min(0.8, max(0.4, units / 10))", Context{Units: 5}, 5000},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			require.NoError(t, Validate(tc.source))
			bps, err := Evaluate(tc.source, tc.context)
			require.NoError(t, err)
			require.Equal(t, tc.bps, bps)
		})
	}
}

func TestDiscountExpressionRejectsInvalidInputs(t *testing.T) {
	for _, source := range []string{"", "0", "1.01", "true", "0.8 +", "param(\"secret\") == nil ? 0.5 : 1", "hour(\"Invalid/Zone\") > 1 ? 0.5 : 1", "[1, 2]", strings.Repeat("1+", 300) + "1", strings.Repeat(" ", 4097)} {
		t.Run(source[:min(50, len(source))], func(t *testing.T) { require.Error(t, Validate(source)) })
	}
	for _, value := range []float64{-1, math.NaN(), math.Inf(1), math.MaxInt32 + 1} {
		_, err := Evaluate("0.8", Context{OutputTokens: value})
		require.Error(t, err)
	}
	_, err := Evaluate("output_tokens > 0 ? 1.1 : 0.8", Context{OutputTokens: 1})
	require.Error(t, err)
}

func TestDiscountExpressionUsesExplicitTimezone(t *testing.T) {
	at := time.Date(2030, 10, 8, 2, 30, 0, 0, time.UTC).Unix()
	bps, err := Evaluate(`hour("Asia/Shanghai") == 10 && minute("Asia/Shanghai") == 30 && weekday("Asia/Shanghai") == 2 ? 0.6 : 0.8`, Context{At: at})
	require.NoError(t, err)
	require.Equal(t, 6000, bps)
}

func TestDiscountExpressionTimeEditorWeekMatrix(t *testing.T) {
	cases := []struct {
		source string
		match  func(day, hour, minute int) bool
	}{
		{`(weekday("Asia/Shanghai") != 0 && weekday("Asia/Shanghai") != 6) ? 0.6 : 0.8`, func(day, _, _ int) bool { return day >= 1 && day <= 5 }},
		{`(weekday("Asia/Shanghai") == 0 || weekday("Asia/Shanghai") == 6) ? 0.6 : 0.8`, func(day, _, _ int) bool { return day == 0 || day == 6 }},
		{`(hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 18) ? 0.6 : 0.8`, func(_, hour, _ int) bool { return hour >= 9 && hour < 18 }},
		{`(minute("Asia/Shanghai") < 30) ? 0.6 : 0.8`, func(_, _, minute int) bool { return minute < 30 }},
		{`(hour("Asia/Shanghai") >= 22 || hour("Asia/Shanghai") < 8) ? 0.6 : 0.8`, func(_, hour, _ int) bool { return hour >= 22 || hour < 8 }},
		{`(weekday("Asia/Shanghai") == 0 && hour("Asia/Shanghai") == 0 && minute("Asia/Shanghai") == 0) ? 0.6 : 0.8`, func(day, hour, minute int) bool { return day == 0 && hour == 0 && minute == 0 }},
		{`(weekday("Asia/Shanghai") != 0 && weekday("Asia/Shanghai") != 6 && hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 18) ? 0.6 : 0.8`, func(day, hour, _ int) bool { return day >= 1 && day <= 5 && hour >= 9 && hour < 18 }},
	}
	zone, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	start := time.Date(2030, 10, 6, 0, 0, 0, 0, zone)
	require.Equal(t, time.Sunday, start.Weekday())
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			for day := range 7 {
				for hour := range 24 {
					for minute := range 60 {
						at := start.Add(time.Duration(day*1440+hour*60+minute) * time.Minute).Unix()
						expected := 8000
						if tc.match(day, hour, minute) {
							expected = 6000
						}
						got, err := Evaluate(tc.source, Context{At: at})
						require.NoError(t, err)
						require.Equal(t, expected, got, "day=%d hour=%d minute=%d", day, hour, minute)
						for _, permanent := range []int{1, 7500, 10000} {
							inherited := strings.TrimSuffix(tc.source, "0.8") + "permanent_discount"
							want := permanent
							if tc.match(day, hour, minute) {
								want = 6000
							}
							actual, err := Evaluate(inherited, Context{At: at, PermanentDiscountBPS: permanent})
							require.NoError(t, err)
							require.Equal(t, want, actual, "permanent=%d day=%d hour=%d minute=%d", permanent, day, hour, minute)
						}
					}
				}
			}
		})
	}
}

func BenchmarkDiscountExpressionTimeEditor(b *testing.B) {
	source := `(weekday("Asia/Shanghai") != 0 && weekday("Asia/Shanghai") != 6) ? 0.6 : (hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 18) ? 0.5 : 0.8`
	context := Context{At: time.Date(2030, 10, 6, 12, 30, 0, 0, time.UTC).Unix(), PermanentDiscountBPS: 8000}
	for _, tc := range []struct{ name, source string }{
		{"literal_fallback", source},
		{"inherited_fallback", strings.TrimSuffix(source, "0.8") + "permanent_discount"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			got, err := Evaluate(tc.source, context)
			require.NoError(b, err)
			require.Equal(b, 8000, got)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := Evaluate(tc.source, context); err != nil {
						b.Error(err)
					}
				}
			})
		})
	}
}

func TestDiscountExpressionCacheConcurrentIsolation(t *testing.T) {
	var workers sync.WaitGroup
	for user := range 100 {
		workers.Go(func() {
			for request := range 100 {
				output := float64(user*100 + request)
				bps, err := Evaluate("output_tokens >= 5000 ? 0.6 : 0.8", Context{OutputTokens: output})
				if err != nil {
					t.Error(err)
					return
				}
				expected := 8000
				if output >= 5000 {
					expected = 6000
				}
				if bps != expected {
					t.Errorf("user=%d output=%v got=%d expected=%d", user, output, bps, expected)
				}
			}
		})
	}
	workers.Wait()
}

func BenchmarkDiscountExpressionCached(b *testing.B) {
	source := `(param("n") != nil && param("n") >= 2) ? 0.5 : (output_tokens >= 10000) ? 0.6 : 0.8`
	context := Context{OutputTokens: 1000, Params: map[string]any{"n": float64(2)}}
	_, err := Evaluate(source, context)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := Evaluate(source, context); err != nil {
				b.Error(err)
			}
		}
	})
}
