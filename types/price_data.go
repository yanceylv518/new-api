package types

import (
	"fmt"
	"math"

	"github.com/shopspring/decimal"
)

// UserModelDiscountRatioKey 标识计费快照和日志中的不可变用户模型折扣。
const UserModelDiscountRatioKey = "user_model_discount"

type GroupRatioInfo struct {
	GroupRatio        float64
	GroupSpecialRatio float64
	HasSpecialRatio   bool
}

type PriceData struct {
	FreeModel            bool
	ModelPrice           float64
	ModelRatio           float64
	CompletionRatio      float64
	CacheRatio           float64
	CacheCreationRatio   float64
	CacheCreation5mRatio float64
	CacheCreation1hRatio float64
	ImageRatio           float64
	AudioRatio           float64
	AudioCompletionRatio float64
	otherRatios          map[string]float64
	UsePrice             bool
	Quota                int // 按次计费的最终额度（MJ / Task）
	// 任务提交调整必须沿用未乘倍率的原始额度，避免从折后取整金额反推。
	BaseQuota         int
	HasBaseQuota      bool
	DiscountAmounts   *DiscountAmounts
	QuotaToPreConsume int // 按量计费的预消耗额度
	GroupRatioInfo    GroupRatioInfo
}

func (p *PriceData) AddOtherRatio(key string, ratio float64) {
	// 用户模型折扣是保留倍率，只能表示不高于原价的折扣，避免被通用倍率乘积误当成加价。
	if key == UserModelDiscountRatioKey && ratio > 1 {
		return
	}
	if !isValidOtherRatio(ratio) {
		return
	}
	if p.otherRatios == nil {
		p.otherRatios = make(map[string]float64)
	}
	p.otherRatios[key] = ratio
}

func (p *PriceData) ReplaceOtherRatios(ratios map[string]float64) bool {
	p.otherRatios = nil
	for key, ratio := range ratios {
		p.AddOtherRatio(key, ratio)
	}
	return len(p.otherRatios) > 0
}

func (p *PriceData) HasOtherRatio(key string) bool {
	ratio, ok := p.otherRatios[key]
	return ok && isValidOtherRatio(ratio)
}

func (p *PriceData) OtherRatios() map[string]float64 {
	if len(p.otherRatios) == 0 {
		return nil
	}
	ratios := make(map[string]float64, len(p.otherRatios))
	for key, ratio := range p.otherRatios {
		if isValidOtherRatio(ratio) {
			ratios[key] = ratio
		}
	}
	if len(ratios) == 0 {
		return nil
	}
	return ratios
}

func (p *PriceData) OtherRatioMultiplier() float64 {
	multiplier := 1.0
	for _, ratio := range p.otherRatios {
		if isValidOtherRatio(ratio) && ratio != 1.0 {
			multiplier *= ratio
		}
	}
	return multiplier
}

// OtherRatioMultiplierBeforeDiscount 保留时长、质量等倍率，仅排除用户模型折扣。
func (p *PriceData) OtherRatioMultiplierBeforeDiscount() float64 {
	multiplier := 1.0
	for key, ratio := range p.otherRatios {
		if key != UserModelDiscountRatioKey && isValidOtherRatio(ratio) && ratio != 1 {
			multiplier *= ratio
		}
	}
	return multiplier
}

// ApplyOtherRatiosBeforeDiscount 用原始计费量计算折前金额，不读取或反推折后额度。
func (p *PriceData) ApplyOtherRatiosBeforeDiscount(value decimal.Decimal) decimal.Decimal {
	for key, ratio := range p.otherRatios {
		if key != UserModelDiscountRatioKey && isValidOtherRatio(ratio) && ratio != 1 {
			value = value.Mul(decimal.NewFromFloat(ratio))
		}
	}
	return value
}

// UserModelDiscountMultiplier 返回经过校验的用户模型倍率，缺省时使用公开价格。
func (p *PriceData) UserModelDiscountMultiplier() float64 {
	if ratio, ok := p.otherRatios[UserModelDiscountRatioKey]; ok && isValidOtherRatio(ratio) && ratio <= 1 {
		return ratio
	}
	return 1
}

func (p *PriceData) ApplyOtherRatiosToFloat(value float64) float64 {
	return value * p.OtherRatioMultiplier()
}

func (p *PriceData) ApplyOtherRatiosToDecimal(value decimal.Decimal) decimal.Decimal {
	for _, ratio := range p.otherRatios {
		if isValidOtherRatio(ratio) && ratio != 1.0 {
			value = value.Mul(decimal.NewFromFloat(ratio))
		}
	}
	return value
}

func (p *PriceData) RemoveOtherRatiosFromFloat(value float64) float64 {
	for _, ratio := range p.otherRatios {
		if isValidOtherRatio(ratio) && ratio != 1.0 {
			value /= ratio
		}
	}
	return value
}

func isValidOtherRatio(ratio float64) bool {
	return ratio > 0 && !math.IsInf(ratio, 1)
}

func (p *PriceData) ToSetting() string {
	return fmt.Sprintf("ModelPrice: %f, ModelRatio: %f, CompletionRatio: %f, CacheRatio: %f, GroupRatio: %f, UsePrice: %t, CacheCreationRatio: %f, CacheCreation5mRatio: %f, CacheCreation1hRatio: %f, QuotaToPreConsume: %d, ImageRatio: %f, AudioRatio: %f, AudioCompletionRatio: %f", p.ModelPrice, p.ModelRatio, p.CompletionRatio, p.CacheRatio, p.GroupRatioInfo.GroupRatio, p.UsePrice, p.CacheCreationRatio, p.CacheCreation5mRatio, p.CacheCreation1hRatio, p.QuotaToPreConsume, p.ImageRatio, p.AudioRatio, p.AudioCompletionRatio)
}
