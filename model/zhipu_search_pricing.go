package model

import (
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// ZhipuSearchPrice 统一目录展示和请求计费的单次价格来源。
func ZhipuSearchPrice(engine string) (float64, error) {
	if !dto.IsZhipuSearchEngine(engine) {
		return 0, fmt.Errorf("unsupported Zhipu search engine %q", engine)
	}
	if billing_setting.GetBillingMode(engine) == billing_setting.BillingModeTieredExpr || ratio_setting.HasConfiguredModelRatio(engine) {
		return 0, fmt.Errorf("Zhipu search engine %s requires per-request pricing", engine)
	}
	price, configured := ratio_setting.GetModelPrice(engine, false)
	if !configured {
		var err error
		price, err = operation_setting.ZhipuSearchDefaultPrice(engine)
		if err != nil {
			return 0, err
		}
	}
	if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0, fmt.Errorf("invalid Zhipu search price")
	}
	return price, nil
}
