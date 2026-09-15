package plugins_test

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

// 并发执行真实插件解码、预估、请求构建和完成用量计费；每次校验金额及快照隔离。
// 不包含厂商生成耗时和数据库 I/O，事务吞吐另由真实数据库负载测试测量。
func BenchmarkVideoPluginBilling(b *testing.B) {
	for _, tc := range []struct {
		key, model, decode, operation, expression string
		expected                                  int
	}{
		{"doubao", "doubao-seedance-2-0-fast-260128", "createTask", "generation", `tier("video",u("tokens")*37/1000000)`, 18500},
		{"doubao", "doubao-seedance-2-0-mini-260615", "createTask", "generation", `tier("video",u("tokens")*23/1000000)`, 11500},
		{"doubao", "doubao-seedance-2-0-260128", "createTask", "generation", `tier("video",u("tokens")*46/1000000)`, 23000},
		{"hailuo", "MiniMax-H3", "createH3VideoTask", "generation", `tier("video",u("seconds")*0.5+max(u("input_images")-5,0)*0.2)`, 1350000},
		{"hailuo", "MiniMax-H3", "createH3ContextIRTask", "context_ir", `tier("ir",u("prompt_tokens")*2/1000000+u("completion_tokens")*8/1000000)`, 18000},
		{"hailuo", "MiniMax-H3", "createH3RegenerationTask", "regeneration", `tier("regen",u("seconds")*0.3)`, 750000},
	} {
		b.Run(tc.model+"/"+tc.operation, func(b *testing.B) {
			source, err := builtinplugins.Source(tc.key)
			require.NoError(b, err)
			plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: tc.key})
			require.NoError(b, err)
			resolution := "720p"
			if tc.key == "hailuo" {
				resolution = "768P"
			}
			request := map[string]any{"model": tc.model, "duration": 5, "resolution": resolution, "ratio": "16:9", "content": []any{map[string]any{"type": "text", "text": "a lighthouse in fog"}}}
			if tc.key == "hailuo" && tc.operation == "generation" {
				content := request["content"].([]any)
				for index := range 9 {
					content = append(content, map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": fmt.Sprintf("https://cdn.example/%d.png", index)}})
				}
				request["content"] = content
			}
			if tc.operation == "regeneration" {
				request["resolution"] = "2K"
				request["content"] = []any{map[string]any{"type": "text", "text": "a lighthouse in fog"}, map[string]any{"type": "video_url", "role": "base_video", "video_url": map[string]any{"url": "https://cdn.example/base.mp4"}}}
			}
			var completed any = map[string]any{"status": "succeeded", "usage": map[string]any{"completion_tokens": 1000}}
			if tc.key == "hailuo" {
				completed = map[string]any{"task": map[string]any{"status": "succeeded", "usage": map[string]any{"output_seconds": 5, "input_image_count": 0, "prompt_tokens": 10000, "completion_tokens": 2000}}}
				if tc.operation == "generation" {
					completed.(map[string]any)["task"].(map[string]any)["usage"].(map[string]any)["input_image_count"] = 6
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					intent, err := plugin.Engine.CallMember(b.Context(), "native", tc.decode, map[string]any{"body": map[string]any{"kind": "json", "value": request}})
					if err != nil {
						b.Error(err)
						return
					}
					driver := map[string]any{"model": tc.model, "upstreamModel": tc.model, "action": intent.(map[string]any)["action"], "requestBody": intent.(map[string]any)["requestBody"], "baseUrl": "https://upstream.example"}
					estimated, err := plugin.Engine.Call(b.Context(), "extractUsage", driver)
					if err != nil {
						b.Error(err)
						return
					}
					_, err = plugin.Engine.Call(b.Context(), "buildSubmitRequest", driver)
					if err != nil {
						b.Error(err)
						return
					}
					facts, err := plugin.Engine.Call(b.Context(), "extractUsageOnComplete", map[string]any{"action": driver["action"]}, nil, completed)
					if err != nil {
						b.Error(err)
						return
					}
					// 复现宿主边界的 JSON 数值归一化，避免将 Sobek 内部整数类型直接交给表达式。
					wire, err := common.Marshal([]any{estimated, facts})
					if err != nil {
						b.Error(err)
						return
					}
					var normalized []map[string]any
					if err = common.Unmarshal(wire, &normalized); err != nil {
						b.Error(err)
						return
					}
					snapshot := &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprVersion: 1, ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), QuotaPerUnit: 500000, GroupRatio: 1, UsageFacts: normalized[0]}
					result, _, err := service.EvaluateTaskCompletionUsage(snapshot, normalized[1])
					if err != nil || result.ActualQuotaAfterGroup != tc.expected {
						b.Error(fmt.Sprintf("billing mismatch: got %d want %d err=%v", result.ActualQuotaAfterGroup, tc.expected, err))
						return
					}
					if tc.key == "doubao" && snapshot.UsageFacts["tokens"] == float64(1000) {
						b.Error("completion overwrote reserved snapshot")
						return
					}
				}
			})
		})
	}
}
