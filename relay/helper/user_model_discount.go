package helper

import (
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// PrepareUserModelDiscount 冻结规则及请求参数；重试和二次定价不重新选择时间段。
func PrepareUserModelDiscount(c *gin.Context, info *relaycommon.RelayInfo, inputTokens, outputTokens float64, facts map[string]any) error {
	if info == nil || info.IsChannelTest || info.UserModelDiscountEvaluation != nil {
		return nil
	}
	name := ratio_setting.FormatMatchingModelName(model.ResolveUserModelPricingName(info.GetBillingModelName()))
	rule, source := info.UserModelDiscountBPS.RuleAt(name, info.StartTime.Unix())
	if source == "" {
		return nil
	}
	evaluation := &hosttypes.UserModelDiscountEvaluation{Rule: rule, Source: source, Context: discountexpr.Context{At: info.StartTime.Unix(), InputTokens: inputTokens, OutputTokens: outputTokens, Count: 1}}
	if rule.Expression != "" {
		parameters, err := discountexpr.Parameters(rule.Expression)
		if err != nil {
			return err
		}
		usageDependent := discountexpr.DependsOnUsage(rule.Expression)
		// 时间条件不读取请求正文，也不冻结与条件无关的时长和数量。
		if usageDependent || len(parameters) > 0 {
			input, err := ResolveIncomingBillingExprRequestInput(c, info)
			if err != nil {
				return err
			}
			var fallbackBody []byte
			if value, exists := c.Get("task_request"); exists {
				if normalized, ok := value.(map[string]any); ok {
					// 兼容 multipart/协议转换后的请求，只读取规则所需的公开标量。
					keys := append([]string{"duration", "seconds", "n"}, parameters...)
					selected := make(map[string]any, len(keys))
					for _, key := range keys {
						switch scalar := normalized[key].(type) {
						case string, bool, float64, float32, int, int64:
							selected[key] = scalar
						}
					}
					fallbackBody, err = common.Marshal(selected)
					if err != nil {
						return err
					}
				}
			}
			if usageDependent {
				duration := gjson.GetBytes(input.Body, "duration")
				if !duration.Exists() {
					duration = gjson.GetBytes(fallbackBody, "duration")
				}
				// -1 是智能时长选项，保留在 param 中，不作为负数计费用量。
				if duration.Type == gjson.Number && duration.Float() != -1 {
					evaluation.Context.Seconds = duration.Float()
				}
				seconds := gjson.GetBytes(input.Body, "seconds")
				if !seconds.Exists() {
					seconds = gjson.GetBytes(fallbackBody, "seconds")
				}
				if seconds.Type == gjson.Number && seconds.Float() != -1 {
					evaluation.Context.Seconds = seconds.Float()
				}
				count := gjson.GetBytes(input.Body, "n")
				if !count.Exists() {
					count = gjson.GetBytes(fallbackBody, "n")
				}
				if count.Type == gjson.Number {
					evaluation.Context.Count = count.Float()
				}
			}
			if len(parameters) > 0 {
				evaluation.Context.Params = make(map[string]any, len(parameters))
				for _, key := range parameters {
					value := gjson.GetBytes(input.Body, key)
					if !value.Exists() {
						value = gjson.GetBytes(fallbackBody, key)
					}
					if value.Exists() && !value.IsArray() && !value.IsObject() {
						if len(value.Raw) > 512 {
							return fmt.Errorf("discount request parameter is too long")
						}
						evaluation.Context.Params[key] = value.Value()
					}
				}
			}
			if usageDependent {
				evaluation.Context.ApplyUsageFacts(facts)
			}
		}
	}
	bps, err := evaluation.Evaluate()
	if err != nil {
		return err
	}
	evaluation.DiscountBPS = bps
	info.UserModelDiscountEvaluation = evaluation
	return nil
}
