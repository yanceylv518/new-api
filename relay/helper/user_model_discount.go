package helper

import (
	"fmt"
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
			if usageDependent {
				if duration := gjson.GetBytes(input.Body, "duration"); duration.Type == gjson.Number {
					evaluation.Context.Seconds = duration.Float()
				}
				if seconds := gjson.GetBytes(input.Body, "seconds"); seconds.Type == gjson.Number {
					evaluation.Context.Seconds = seconds.Float()
				}
				if count := gjson.GetBytes(input.Body, "n"); count.Type == gjson.Number {
					evaluation.Context.Count = count.Float()
				}
			}
			if len(parameters) > 0 {
				evaluation.Context.Params = make(map[string]any, len(parameters))
				for _, key := range parameters {
					value := gjson.GetBytes(input.Body, key)
					if value.Exists() && !value.IsArray() && !value.IsObject() {
						if len(value.Raw) > 512 {
							return fmt.Errorf("discount request parameter is too long")
						}
						evaluation.Context.Params[key] = value.Value()
					}
				}
			}
			if usageDependent {
				for key, value := range facts {
					number, ok := value.(float64)
					if !ok {
						switch integer := value.(type) {
						case int:
							number = float64(integer)
						case int64:
							number = float64(integer)
						default:
							continue
						}
					}
					switch key {
					case "seconds":
						evaluation.Context.Seconds = number
					case "count", "clips":
						evaluation.Context.Count = number
					case "units":
						evaluation.Context.Units = number
					case "tokens":
						evaluation.Context.OutputTokens = number
						evaluation.Context.TaskTokens = number
					}
				}
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
