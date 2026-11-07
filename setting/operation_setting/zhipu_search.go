package operation_setting

import (
	"fmt"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// ZhipuSearchDefaultPrice 返回人民币官方价格换算后的美元单次价格。
func ZhipuSearchDefaultPrice(engine string) (float64, error) {
	var pricePerThousandCNY float64
	switch engine {
	case dto.ZhipuSearchStd:
		pricePerThousandCNY = 10
	case dto.ZhipuSearchPro:
		pricePerThousandCNY = 30
	case dto.ZhipuSearchProSogou, dto.ZhipuSearchProQuark:
		pricePerThousandCNY = 50
	default:
		return 0, fmt.Errorf("unsupported Zhipu search engine %q", engine)
	}
	common.OptionMapRWMutex.RLock()
	rate := USDExchangeRate
	common.OptionMapRWMutex.RUnlock()
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0, fmt.Errorf("invalid USD exchange rate for Zhipu search pricing")
	}
	return pricePerThousandCNY / rate / 1000, nil
}
