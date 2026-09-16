package plugins_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hailuo 原生路由必须覆盖官方 V2 接口清单，并保留公开任务 ID 的网关边界。
func TestHailuoNativeRoutesAndContextIR(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	wantRoutes := []struct {
		path   string
		method string
		type_  jsplugin.RouteType
		action string
		models []string
	}{
		{path: "/hailuo/v2/video_generation", method: http.MethodPost, type_: jsplugin.RouteTypeSubmit, models: []string{"MiniMax-H3"}},
		{path: "/hailuo/v2/query/video_generation/:task_id", method: http.MethodGet, type_: jsplugin.RouteTypeQuery, models: []string{"MiniMax-H3"}},
		{path: "/hailuo/v2/query/video_generation", method: http.MethodGet, type_: jsplugin.RouteTypeDynamic, action: "list", models: []string{"MiniMax-H3"}},
		{path: "/hailuo/v2/video_generation/:task_id", method: http.MethodDelete, type_: jsplugin.RouteTypeDynamic, action: "delete", models: []string{"MiniMax-H3"}},
		{path: "/hailuo/v2/h3_context_ir", method: http.MethodPost, type_: jsplugin.RouteTypeSubmit, models: []string{"MiniMax-H3"}},
		{path: "/hailuo/v2/video_regeneration", method: http.MethodPost, type_: jsplugin.RouteTypeSubmit, models: []string{"MiniMax-H3"}},
	}
	for _, want := range wantRoutes {
		found := false
		for _, route := range plugin.Meta.Routes {
			if route.Method == want.method && route.Path == want.path {
				found = true
				assert.Equal(t, want.type_, route.Type)
				assert.Equal(t, want.action, route.Action)
				assert.Equal(t, want.models, route.Models)
				break
			}
		}
		assert.True(t, found, want.method+" "+want.path)
	}
}

func TestHailuoNativeContextIRHooks(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	request := map[string]any{
		"model":    "MiniMax-H3",
		"content":  []any{map[string]any{"type": "text", "text": "a lighthouse in fog"}},
		"duration": 5,
		"ratio":    "16:9",
	}
	value, err := plugin.Engine.CallPath(t.Context(), "native", []string{"createH3ContextIRTask"}, map[string]any{
		"body": map[string]any{"kind": "json", "value": request},
	})
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var intent map[string]any
	require.NoError(t, common.Unmarshal(encoded, &intent))
	assert.Equal(t, "submit", intent["kind"])
	assert.Equal(t, "MiniMax-H3", intent["model"])
	assert.Equal(t, "context_ir", intent["action"])

	descriptor := callHailuoHook(t, plugin, "buildSubmitRequest", map[string]any{
		"requestBody":   request,
		"model":         "MiniMax-H3",
		"upstreamModel": "MiniMax-H3",
		"action":        "context_ir",
		"baseUrl":       "https://api.minimax.example",
		"apiKey":        "secret",
	})
	assert.Equal(t, "https://api.minimax.example/v2/h3_context_ir", descriptor["url"])
	assert.Equal(t, "context_ir", descriptor["action"])

	// 走宿主真实用量校验，确保删除总量字段后不会触发未声明数量的时长上限。
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/hailuo/v2/h3_context_ir", nil)
	ctx.Set("task_request", request)
	info := &relaycommon.RelayInfo{
		OriginModelName: "MiniMax-H3",
		ChannelMeta:     &relaycommon.ChannelMeta{UpstreamModelName: "MiniMax-H3"},
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{Action: "context_ir"},
	}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	facts, err := adaptor.ExtractUsageFactsValidated(ctx, info)
	require.NoError(t, err)
	assert.Equal(t, "context_ir", facts["operation"])
	assert.Equal(t, float64(10), facts["prompt_tokens"])
	assert.Equal(t, float64(10000), facts["completion_tokens"])
	assert.NotContains(t, facts, "tokens")
	// 定价元数据必须暴露独立输入、输出单价，并保持官方模型名和操作值。
	schema, _ := plugin.Meta.UsageForModel("MiniMax-H3")
	assert.Equal(t, "H3-Context-IR", schema["operation"].EnumLabels["context_ir"]["zh"])
	assert.Equal(t, "H3-Context-IR", schema["operation"].EnumLabels["context_ir"]["en"])
	assert.Equal(t, "token", schema["prompt_tokens"].Unit)
	assert.Equal(t, "token", schema["completion_tokens"].Unit)
	assert.NotContains(t, schema, "tokens")

	var completion any
	require.NoError(t, common.UnmarshalJsonStr(`{"task":{"id":"upstream","status":"succeeded","task_type":"h3_context_ir","usage":{"total_tokens":9090,"prompt_tokens":5664,"completion_tokens":3426},"content":{"prompt":"enhanced"}}}`, &completion))
	completionFacts := callHailuoHook(t, plugin, "extractUsageOnComplete", map[string]any{
		"action": "context_ir",
	}, nil, completion)
	assert.Equal(t, map[string]any{"operation": "context_ir", "tokens": float64(9090), "prompt_tokens": float64(5664), "completion_tokens": float64(3426), "seconds": float64(0), "input_images": float64(0), "input_video_seconds": float64(0)}, completionFacts)
}

// 原生创建和再生成路由必须把官方请求转换为宿主可持久化的提交意图。
func TestHailuoNativeCreateAndRegenerationHooks(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	createBody := map[string]any{
		"model":          "MiniMax-H3",
		"content":        []any{map[string]any{"type": "text", "text": "a lighthouse in fog"}},
		"resolution":     "768P",
		"duration":       5,
		"ratio":          "16:9",
		"callback_url":   "https://client.example/callback",
		"aigc_watermark": false,
	}
	value, err := plugin.Engine.CallPath(t.Context(), "native", []string{"createH3VideoTask"}, map[string]any{
		"body": map[string]any{"kind": "json", "value": createBody},
	})
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var intent map[string]any
	require.NoError(t, common.Unmarshal(encoded, &intent))
	assert.Equal(t, "submit", intent["kind"])
	assert.Equal(t, "text_to_video", intent["action"])
	// 原生解码后再构造上游请求，确保可选顶层字段不会在两个阶段之间丢失。
	descriptor := callHailuoHook(t, plugin, "buildSubmitRequest", map[string]any{
		"requestBody": intent["requestBody"], "action": intent["action"],
		"model": "MiniMax-H3", "upstreamModel": "MiniMax-H3", "baseUrl": "https://api.minimax.example",
	})
	upstreamBody := descriptor["body"].(map[string]any)
	assert.Equal(t, "https://client.example/callback", upstreamBody["callback_url"])
	assert.Equal(t, false, upstreamBody["aigc_watermark"])

	regeneration := map[string]any{
		"model":          "MiniMax-H3",
		"source_task_id": "task_public",
		"resolution":     "2K",
	}
	value, err = plugin.Engine.CallPath(t.Context(), "native", []string{"createH3RegenerationTask"}, map[string]any{
		"body": map[string]any{"kind": "json", "value": regeneration},
	})
	require.NoError(t, err)
	encoded, err = common.Marshal(value)
	require.NoError(t, err)
	intent = map[string]any{}
	require.NoError(t, common.Unmarshal(encoded, &intent))
	assert.Equal(t, "submit", intent["kind"])
	assert.Equal(t, "regeneration", intent["action"])
	assert.Equal(t, []any{"task_public"}, intent["originTaskIds"])

	_, err = plugin.Engine.CallPath(t.Context(), "native", []string{"createH3RegenerationTask"}, map[string]any{
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": "MiniMax-H3", "source_task_id": "task_public",
		}},
	})
	require.ErrorContains(t, err, "resolution is required")
}

// 查询与列表渲染必须恢复官方 task 字段，同时覆盖 Context-IR 的文本产物。
func TestHailuoNativeTaskRenderers(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	task := map[string]any{
		"task_id": "task_public",
		"status":  "SUCCESS",
		"action":  "context_ir",
		"data": map[string]any{"task": map[string]any{
			"id": "upstream", "model": "MiniMax-H3", "status": "succeeded",
			"task_type": "h3_context_ir", "content": map[string]any{"prompt": "enhanced"},
		}},
	}
	value, err := plugin.Engine.CallPath(t.Context(), "native", []string{"h3TaskStatus"}, map[string]any{}, task)
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var rendered map[string]any
	require.NoError(t, common.Unmarshal(encoded, &rendered))
	assert.Equal(t, "task_public", rendered["task"].(map[string]any)["id"])
	assert.Equal(t, "succeeded", rendered["task"].(map[string]any)["status"])
	assert.Equal(t, "enhanced", rendered["task"].(map[string]any)["content"].(map[string]any)["prompt"])
	assert.Equal(t, "h3_context_ir", rendered["task"].(map[string]any)["task_type"])
	assert.Equal(t, "text", rendered["task"].(map[string]any)["modality"])

	// 排队阶段尚无上游快照，仍应输出官方任务类型和文本模态。
	value, err = plugin.Engine.CallPath(t.Context(), "native", []string{"h3TaskStatus"}, map[string]any{}, map[string]any{
		"task_id": "task_pending", "status": "QUEUED", "action": "context_ir",
	})
	require.NoError(t, err)
	encoded, err = common.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(encoded, &rendered))
	assert.Equal(t, "h3_context_ir", rendered["task"].(map[string]any)["task_type"])
	assert.Equal(t, "text", rendered["task"].(map[string]any)["modality"])
}

// Context-IR 仅使用合法的实际 Token 数覆盖预扣，缺失或畸形用量必须保留估算。
// 媒体用量中的布尔、数组和空白字符串不能转成零后错误退还已预留的费用。
func TestHailuoVideoCompletionRejectsCoercedUsage(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	for _, invalid := range []any{false, true, " ", []any{}, []any{0}, map[string]any{}, -1, "Infinity"} {
		facts := callHailuoHook(t, plugin, "extractUsageOnComplete", map[string]any{"action": "text_to_video"}, nil,
			map[string]any{"task": map[string]any{"status": "succeeded", "usage": map[string]any{
				"input_image_count": invalid, "input_seconds": invalid, "output_seconds": 5,
			}}})
		assert.NotContains(t, facts, "input_images", "invalid=%#v", invalid)
		assert.NotContains(t, facts, "input_video_seconds", "invalid=%#v", invalid)
		assert.EqualValues(t, 5, facts["seconds"])
	}
}

// 完成响应不能把已提交任务切换到另一种价格；只有缺少任务身份时才采用厂商提示。
func TestHailuoCompletionPreservesSubmittedOperation(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	for action, operation := range map[string]string{"text_to_video": "generation", "regeneration": "regeneration", "context_ir": "context_ir"} {
		facts := callHailuoHook(t, plugin, "extractUsageOnComplete", map[string]any{"action": action}, nil,
			map[string]any{"task": map[string]any{"task_type": "h3_context_ir", "usage": map[string]any{}}})
		assert.Equal(t, operation, facts["operation"])
	}
}

func TestHailuoNativeContextIRCompletionTokenBoundaries(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	for _, tc := range []struct {
		name  string
		usage map[string]any
		want  map[string]any
	}{
		{"measured", map[string]any{"total_tokens": 9090}, map[string]any{"tokens": float64(9090), "operation": "context_ir"}},
		{"explicit_zero", map[string]any{"total_tokens": 0}, map[string]any{"tokens": float64(0), "operation": "context_ir"}},
		{"missing", map[string]any{}, map[string]any{"operation": "context_ir"}},
		{"null", map[string]any{"total_tokens": nil}, map[string]any{"operation": "context_ir"}},
		{"empty", map[string]any{"total_tokens": " "}, map[string]any{"operation": "context_ir"}},
		{"boolean", map[string]any{"total_tokens": false}, map[string]any{"operation": "context_ir"}},
		{"negative", map[string]any{"total_tokens": -1}, map[string]any{"operation": "context_ir"}},
		{"fractional", map[string]any{"total_tokens": 1.5}, map[string]any{"operation": "context_ir"}},
		{"overflow", map[string]any{"total_tokens": 2147483648}, map[string]any{"operation": "context_ir"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 不适用的视频用量恒为零，实际 Token 缺失时仍不输出该字段。
			tc.want["seconds"], tc.want["input_images"], tc.want["input_video_seconds"] = float64(0), float64(0), float64(0)
			assert.Equal(t, tc.want, callHailuoHook(t, plugin, "extractUsageOnComplete", nil, nil, map[string]any{
				"task": map[string]any{"task_type": "h3_context_ir", "usage": tc.usage},
			}))
		})
	}
}

// 三项用量分别校验，缺失或异常的字段不能被推算为零或覆盖其他字段的有效值。
func TestHailuoNativeContextIRSplitTokenBoundaries(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	for _, field := range []string{"prompt_tokens", "completion_tokens"} {
		for _, tc := range []struct {
			name  string
			value any
			want  any
		}{
			{"measured", 3426, float64(3426)},
			{"explicit_zero", 0, float64(0)},
			{"numeric_string", "5664", float64(5664)},
			{"maximum", 2147483647, float64(2147483647)},
			{"null", nil, nil},
			{"empty", " ", nil},
			{"boolean", false, nil},
			{"negative", -1, nil},
			{"fractional", 1.5, nil},
			{"overflow", 2147483648, nil},
			{"non_finite", "Infinity", nil},
			{"object", map[string]any{}, nil},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				facts := callHailuoHook(t, plugin, "extractUsageOnComplete", nil, nil, map[string]any{
					"task": map[string]any{"task_type": "h3_context_ir", "usage": map[string]any{"total_tokens": 9090, field: tc.value}},
				})
				want := map[string]any{"operation": "context_ir", "tokens": float64(9090), "seconds": float64(0), "input_images": float64(0), "input_video_seconds": float64(0)}
				if tc.want != nil {
					want[field] = tc.want
				}
				assert.Equal(t, want, facts)
			})
		}
	}
}

// 实际结算只累加已配置的输入、输出单价；总量保留给旧表达式，不应自动重复计费。
func TestHailuoNativeContextIRSplitBillingSettlement(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	const expression = `tier("ir", u("prompt_tokens") * 2 / 1000000 + u("completion_tokens") * 8 / 1000000)`
	for _, tc := range []struct {
		name      string
		usage     map[string]any
		wantQuota int
	}{
		{"official_usage", map[string]any{"total_tokens": 9090, "prompt_tokens": 5664, "completion_tokens": 3426}, 19368},
		{"without_total", map[string]any{"prompt_tokens": 5664, "completion_tokens": 3426}, 19368},
		{"explicit_zero", map[string]any{"prompt_tokens": 0, "completion_tokens": 0}, 0},
		{"total_only_keeps_estimates", map[string]any{"total_tokens": 9090}, 40010},
		{"invalid_output_keeps_output_estimate", map[string]any{"prompt_tokens": 5664, "completion_tokens": -1}, 45664},
		{"invalid_input_keeps_input_estimate", map[string]any{"prompt_tokens": false, "completion_tokens": 3426}, 13714},
	} {
		t.Run(tc.name, func(t *testing.T) {
			estimated := callHailuoHook(t, plugin, "extractUsage", map[string]any{
				"model": "MiniMax-H3", "upstreamModel": "MiniMax-H3", "action": "context_ir",
				"requestBody": map[string]any{"prompt": "a lighthouse in fog"},
			})
			snapshot := &billingexpr.BillingSnapshot{
				TaskUsageBilling: true, ExprVersion: 1, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 1,
				UsageFacts: estimated,
			}
			facts := callHailuoHook(t, plugin, "extractUsageOnComplete", nil, nil, map[string]any{
				"task": map[string]any{"task_type": "h3_context_ir", "usage": tc.usage},
			})
			result, _, err := service.EvaluateTaskCompletionUsage(snapshot, facts)
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuota, result.ActualQuotaAfterGroup)
			assert.Equal(t, float64(10000), snapshot.UsageFacts["completion_tokens"])
		})
	}
}

// 上游异步确认的取消必须与管理接口共享标记，供本地查询过滤和退款使用。
func TestHailuoNativePolledCancellation(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	result := callHailuoHook(t, plugin, "parseTaskResult", nil, map[string]any{
		"task": map[string]any{"status": "cancelled"},
	})
	assert.Equal(t, "FAILURE", result["status"])
	assert.Equal(t, "task cancelled by user", result["reason"])
	value, err := plugin.Engine.CallPath(t.Context(), "native", []string{"h3TaskStatus"}, map[string]any{}, map[string]any{
		"task_id": "task_cancelled", "status": result["status"], "fail_reason": result["reason"],
	})
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var rendered map[string]any
	require.NoError(t, common.Unmarshal(encoded, &rendered))
	assert.Equal(t, "cancelled", rendered["task"].(map[string]any)["status"])
}

// 将插件实际用量交给宿主表达式结算，覆盖按秒、再生成和 Context-IR 三种价格分支。
func TestHailuoNativeOperationBillingSettlement(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	const expression = `u("operation") == "context_ir" ? tier("ir", u("tokens") * 2 / 1000000) : u("operation") == "regeneration" ? tier("regen", u("seconds") * 0.2) : tier("video", u("seconds") * 0.1)`
	for _, tc := range []struct {
		operation string
		taskType  string
		usage     map[string]any
		wantQuota int
	}{
		{"generation", "generation", map[string]any{"output_seconds": 5}, 250000},
		{"regeneration", "regeneration", map[string]any{"output_seconds": 5}, 500000},
		{"context_ir", "h3_context_ir", map[string]any{"total_tokens": 9090}, 9090},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			snapshot := &billingexpr.BillingSnapshot{
				TaskUsageBilling: true, ExprVersion: 1, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 1,
				UsageFacts: map[string]any{"operation": tc.operation, "tokens": float64(10000), "seconds": float64(15)},
			}
			facts := callHailuoHook(t, plugin, "extractUsageOnComplete", nil, nil, map[string]any{
				"task": map[string]any{"task_type": tc.taskType, "usage": tc.usage},
			})
			result, _, err := service.EvaluateTaskCompletionUsage(snapshot, facts)
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuota, result.ActualQuotaAfterGroup)
			assert.Equal(t, float64(15), snapshot.UsageFacts["seconds"])
		})
	}
}

// 免费数量来自管理员保存的表达式，预扣与结算复用快照，真实图片总数不提前减免。
func TestHailuoConfigurableImageAllowanceSettlement(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	for _, tc := range []struct {
		name, operation                                 string
		free, submitted, actual, wantReserve, wantFinal int
	}{
		{"generation_boundary", "generation", 5, 6, 5, 100000, 0},
		{"generation_excess", "generation", 5, 9, 6, 400000, 100000},
		{"generation_zero_allowance", "generation", 0, 5, 5, 500000, 500000},
		{"regeneration_custom_allowance", "regeneration", 7, 9, 8, 200000, 100000},
		{"regeneration_free", "regeneration", 5, 5, 5, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := []any{map[string]any{"type": "text", "text": "keep the scene"}}
			if tc.operation == "regeneration" {
				content = append(content, map[string]any{"type": "video_url", "role": "base_video", "video_url": map[string]any{"url": "https://cdn.example/base.mp4"}})
			}
			for range tc.submitted {
				content = append(content, map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": "https://cdn.example/image.png"}})
			}
			facts := callHailuoHook(t, plugin, "extractUsage", map[string]any{"model": "MiniMax-H3", "upstreamModel": "MiniMax-H3", "action": tc.operation, "requestBody": map[string]any{"content": content}})
			assert.Equal(t, float64(tc.submitted), facts["input_images"])
			expression := fmt.Sprintf(`tier("image", max(u("input_images") - %d, 0) * 0.2)`, tc.free)
			snapshot := &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprVersion: 1, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 1, UsageFacts: facts}
			reserve, _, err := service.EvaluateTaskCompletionUsage(snapshot, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.wantReserve, reserve.ActualQuotaAfterGroup)
			measured := callHailuoHook(t, plugin, "extractUsageOnComplete", nil, nil, map[string]any{"task": map[string]any{"task_type": tc.operation, "usage": map[string]any{"input_image_count": tc.actual}}})
			settled, _, err := service.EvaluateTaskCompletionUsage(snapshot, measured)
			require.NoError(t, err)
			assert.Equal(t, tc.wantFinal, settled.ActualQuotaAfterGroup)
			assert.Equal(t, float64(tc.submitted), snapshot.UsageFacts["input_images"])
		})
	}
}

// 源任务再生成以原素材总张数计费，未知用量只影响预估，不能当成免费素材。
func TestHailuoRegenerationSourceImageUsage(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	for _, tc := range []struct {
		name  string
		count any
		want  float64
	}{
		{"zero", 0, 0}, {"nine", 9, 9}, {"missing", nil, 9}, {"negative", -1, 9}, {"boolean", false, 9}, {"overflow", 10, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := callHailuoHook(t, plugin, "extractUsage", map[string]any{
				"model": "MiniMax-H3", "upstreamModel": "MiniMax-H3", "action": "regeneration", "requestBody": map[string]any{"source_task_id": "public"},
				"originTasks": []any{map[string]any{"taskId": "public", "upstreamTaskId": "upstream", "status": "SUCCESS", "data": map[string]any{"task": map[string]any{"model": "MiniMax-H3", "resolution": "768P", "duration": 5, "usage": map[string]any{"input_image_count": tc.count}}}}},
			})
			assert.Equal(t, tc.want, facts["input_images"])
		})
	}
}

// Responses 的最终文本输入在再生成解码之后仍可用于真实上游请求。
func TestHailuoRegenerationPreservesResponsesPrompt(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	value, err := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "decodeRequest"}, map[string]any{
		"model": "MiniMax-H3", "upstreamModel": "MiniMax-H3",
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": "MiniMax-H3", "input": "preserve the original lighting", "resolution": "2K",
			"content": []any{map[string]any{"type": "video_url", "role": "base_video", "video_url": map[string]any{"url": "https://cdn.example/source.mp4"}}},
		}},
	})
	require.NoError(t, err)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var intent map[string]any
	require.NoError(t, common.Unmarshal(encoded, &intent))
	descriptor := callHailuoHook(t, plugin, "buildSubmitRequest", map[string]any{
		"requestBody": intent["requestBody"], "action": intent["action"],
		"model": "MiniMax-H3", "upstreamModel": "MiniMax-H3", "baseUrl": "https://api.minimax.example",
	})
	assert.Equal(t, "https://api.minimax.example/v2/video_regeneration", descriptor["url"])
	assert.Equal(t, "preserve the original lighting", descriptor["body"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"])
}

func TestHailuoNativeTaskActionRequest(t *testing.T) {
	plugin := loadHailuoPlugin(t)
	descriptor := callHailuoHook(t, plugin, "buildTaskActionRequest", map[string]any{
		"operation":     "delete",
		"taskId":        "424010985738629",
		"model":         "MiniMax-H3",
		"upstreamModel": "MiniMax-H3",
		"baseUrl":       "https://api.minimax.example",
		"apiKey":        "secret",
	})
	assert.Equal(t, http.MethodDelete, descriptor["method"])
	assert.Equal(t, "https://api.minimax.example/v2/video_generation/424010985738629", descriptor["url"])
	assert.Equal(t, map[string]any{"Accept": "application/json", "Authorization": "Bearer secret"}, descriptor["headers"])

	_, err := plugin.Engine.Call(t.Context(), "buildTaskActionRequest", map[string]any{
		"operation":     "delete",
		"taskId":        "424010738629",
		"model":         "MiniMax-Hailuo-2.3",
		"upstreamModel": "MiniMax-Hailuo-2.3",
		"baseUrl":       "https://api.minimax.example",
		"apiKey":        "secret",
	})
	require.ErrorContains(t, err, "requires MiniMax-H3")
}
