package plugins_test

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 保留可视化编辑器生成的零价项，复现线上 nil * 0，不能改用仅含有效字段的简化表达式。
const h3VisualBillingExpression = `u("operation") == "generation" && u("resolution") == "768P" ? tier("generation:768P", u("seconds") * 0.5 + u("prompt_tokens") * 0 / 1000000 + u("completion_tokens") * 0 / 1000000 + max(u("input_images") - 5, 0) * 0.2 + u("input_video_seconds") * 0) : u("operation") == "generation" && u("resolution") == "2K" ? tier("generation:2K", u("seconds") * 0.8 + u("prompt_tokens") * 0 / 1000000 + u("completion_tokens") * 0 / 1000000 + max(u("input_images") - 5, 0) * 0.2 + u("input_video_seconds") * 0) : u("operation") == "regeneration" && u("resolution") == "2K" ? tier("regeneration:2K", u("seconds") * 0.3 + u("prompt_tokens") * 0 / 1000000 + u("completion_tokens") * 0 / 1000000 + max(u("input_images") - 5, 0) * 0.15 + u("input_video_seconds") * 0) : tier("context_ir", u("seconds") * 0 + u("prompt_tokens") * 23 / 1000000 + u("completion_tokens") * 5.8 / 1000000 + u("input_images") * 0 + u("input_video_seconds") * 0)`

// 经原生解码和宿主用量校验建立快照，避免直接使用 JS 整数绕过真实提交边界。
func videoUsageSnapshot(t *testing.T, key, modelName, decoder, expression string, request map[string]any, origins []relaycommon.OriginTaskRef) (*taskplugin.TaskAdaptor, *model.Task, *billingexpr.BillingSnapshot) {
	t.Helper()
	source, err := builtinplugins.Source(key)
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: key})
	require.NoError(t, err)
	value, err := plugin.Engine.CallMember(t.Context(), "native", decoder, map[string]any{"body": map[string]any{"kind": "json", "value": request}})
	require.NoError(t, err)
	intent, ok := value.(map[string]any)
	require.True(t, ok)
	action, _ := intent["action"].(string)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ctx.Set("task_request", intent["requestBody"])
	info := &relaycommon.RelayInfo{
		OriginModelName: modelName,
		ChannelMeta:     &relaycommon.ChannelMeta{UpstreamModelName: modelName},
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: action, OriginTasks: origins},
	}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	facts, err := adaptor.ExtractUsageFactsValidated(ctx, info)
	require.NoError(t, err)
	snapshot := &billingexpr.BillingSnapshot{
		TaskUsageBilling: true, ExprVersion: 1, ExprString: expression,
		ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 1, UsageFacts: facts,
	}
	task := &model.Task{Action: action, Properties: model.Properties{OriginModelName: modelName, UpstreamModelName: modelName}}
	return adaptor, task, snapshot
}

// 四个价格分支及两种再生成输入都必须可预扣，并按有效实际用量结算；缺失值保持预估。
func TestHailuoVisualExpressionUsage(t *testing.T) {
	for _, tc := range []struct {
		name, decode, resolution string
		images                   int
		source                   bool
		want, withoutImages      int
	}{
		{"generation_768p", "createH3VideoTask", "768P", 0, false, 1250000, 1250000},
		{"generation_2k_free_images", "createH3VideoTask", "2K", 5, false, 2000000, 2000000},
		{"generation_2k_extra_image", "createH3VideoTask", "2K", 6, false, 2100000, 2000000},
		{"regeneration_url", "createH3RegenerationTask", "2K", 6, false, 825000, 750000},
		{"regeneration_source", "createH3RegenerationTask", "2K", 6, true, 825000, 750000},
		{"context_ir", "createH3ContextIRTask", "", 0, false, 29115, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := []any{map[string]any{"type": "text", "text": "a lighthouse in fog"}}
			for index := range tc.images {
				content = append(content, map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": fmt.Sprintf("https://cdn.example/%d.png", index)}})
			}
			if tc.decode == "createH3RegenerationTask" {
				content = append(content, map[string]any{"type": "video_url", "role": "base_video", "video_url": map[string]any{"url": "https://cdn.example/base.mp4"}})
			}
			request := map[string]any{"model": "MiniMax-H3", "duration": 5, "resolution": tc.resolution, "ratio": "16:9", "content": content}
			var origins []relaycommon.OriginTaskRef
			if tc.source {
				request = map[string]any{"model": "MiniMax-H3", "resolution": "2K", "source_task_id": "task_source"}
				origins = []relaycommon.OriginTaskRef{{TaskID: "task_source", UpstreamTaskID: "upstream_source", Status: "SUCCESS", Data: []byte(`{"task":{"model":"MiniMax-H3","resolution":"768P","duration":5,"usage":{"input_image_count":6}}}`)}}
			}
			adaptor, task, snapshot := videoUsageSnapshot(t, "hailuo", "MiniMax-H3", tc.decode, h3VisualBillingExpression, request, origins)
			reserved, err := billingexpr.ComputeTieredQuotaWithRequest(snapshot, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: snapshot.UsageFacts})
			require.NoError(t, err)
			assert.Equal(t, tc.want, reserved.ActualQuotaAfterGroup)
			original := maps.Clone(snapshot.UsageFacts)
			for _, usageCase := range []struct {
				name                string
				usage               map[string]any
				videoQuota, irQuota int
			}{
				{"missing", map[string]any{}, tc.want, tc.want},
				{"invalid", map[string]any{"output_seconds": false, "input_image_count": []any{}, "input_seconds": " ", "prompt_tokens": nil, "completion_tokens": -1}, tc.want, tc.want},
				{"measured", map[string]any{"output_seconds": 5, "input_image_count": tc.images, "input_seconds": 0, "prompt_tokens": 5664, "completion_tokens": 3426}, tc.want, 75071},
				{"zero_optional", map[string]any{"input_image_count": 0, "input_seconds": 0, "prompt_tokens": 0, "completion_tokens": 0}, tc.withoutImages, 0},
			} {
				t.Run(usageCase.name, func(t *testing.T) {
					body, err := common.Marshal(map[string]any{"task": map[string]any{"status": "succeeded", "usage": usageCase.usage}})
					require.NoError(t, err)
					parsed, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK}, body)
					require.NoError(t, err)
					result, _, err := service.EvaluateTaskCompletionUsage(snapshot, parsed.UsageFacts)
					require.NoError(t, err)
					want := usageCase.videoQuota
					if tc.decode == "createH3ContextIRTask" {
						want = usageCase.irQuota
					}
					assert.Equal(t, want, result.ActualQuotaAfterGroup)
					assert.Equal(t, original, snapshot.UsageFacts)
					// 旧快照可能没有不适用字段；完成钩子也必须补齐，而不填零覆盖有效估算。
					legacy := *snapshot
					legacy.UsageFacts = maps.Clone(original)
					inactive := []string{"prompt_tokens", "completion_tokens"}
					if tc.decode == "createH3ContextIRTask" {
						inactive = []string{"seconds", "input_images", "input_video_seconds"}
					}
					for _, key := range inactive {
						delete(legacy.UsageFacts, key)
					}
					result, _, err = service.EvaluateTaskCompletionUsage(&legacy, parsed.UsageFacts)
					require.NoError(t, err)
					assert.Equal(t, want, result.ActualQuotaAfterGroup)
				})
			}
		})
	}
}

// Doubao 全部声明模型和合法分辨率均经实际解码；部分完成用量不得丢掉参考视频定价维度。
func TestDoubaoVisualExpressionUsage(t *testing.T) {
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	for _, modelName := range plugin.Meta.Models {
		schema, _ := plugin.Meta.UsageForModel(modelName)
		for _, resolution := range schema["resolution"].Enum {
			for _, video := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/video=%t", modelName, resolution, video), func(t *testing.T) {
					expression := fmt.Sprintf(`u("resolution") == %q && u("video_input") == "video" ? tier("reference", u("tokens") * 20 / 1000000) : tier("plain", u("tokens") * 40 / 1000000)`, resolution)
					content := []any{map[string]any{"type": "text", "text": "a lighthouse in fog"}}
					if video {
						content = append(content, map[string]any{"type": "video_url", "role": "reference_video", "video_url": map[string]any{"url": "https://cdn.example/ref.mp4"}})
					}
					adaptor, task, snapshot := videoUsageSnapshot(t, "doubao", modelName, "createTask", expression, map[string]any{"model": modelName, "duration": 5, "resolution": resolution, "content": content}, nil)
					reserved, err := billingexpr.ComputeTieredQuotaWithRequest(snapshot, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: snapshot.UsageFacts})
					require.NoError(t, err)
					assert.Positive(t, reserved.ActualQuotaAfterGroup)
					original := maps.Clone(snapshot.UsageFacts)
					for _, tc := range []struct {
						name   string
						usage  map[string]any
						tokens int
					}{
						{"missing", map[string]any{}, -1},
						{"invalid", map[string]any{"completion_tokens": false, "total_tokens": []any{}}, -1},
						{"measured", map[string]any{"completion_tokens": 1000, "total_tokens": 2000}, 1000},
						{"total_fallback", map[string]any{"total_tokens": 2000}, 2000},
						{"explicit_zero", map[string]any{"completion_tokens": 0, "total_tokens": 2000}, 0},
					} {
						t.Run(tc.name, func(t *testing.T) {
							body, err := common.Marshal(map[string]any{"status": "succeeded", "usage": tc.usage})
							require.NoError(t, err)
							parsed, err := adaptor.ParseTaskResult(task, &http.Response{StatusCode: http.StatusOK}, body)
							require.NoError(t, err)
							result, merged, err := service.EvaluateTaskCompletionUsage(snapshot, parsed.UsageFacts)
							require.NoError(t, err)
							want := reserved.ActualQuotaAfterGroup
							if tc.tokens >= 0 {
								want = tc.tokens * 20
								if video {
									want = tc.tokens * 10
								}
							}
							assert.Equal(t, want, result.ActualQuotaAfterGroup)
							assert.Equal(t, original["resolution"], merged["resolution"])
							assert.Equal(t, original["video_input"], merged["video_input"])
							assert.Equal(t, original, snapshot.UsageFacts)
						})
					}
				})
			}
		}
	}
}
