package types

import "math"

// DiscountAmounts 记录同一计费事件的整数额度快照，优惠只指用户模型折扣。
// 折前额度由原始计费量独立计算，不能用已取整的折后额度除以折扣反推。
type DiscountAmounts struct {
	Before  int `json:"quota_before_discount"`
	After   int `json:"quota_after_discount"`
	Savings int `json:"discount_quota"`
}

// NewDiscountAmounts 的输入须经统一额度转换器校验；非法组合不生成误导性的金额快照。
func NewDiscountAmounts(before, after int) *DiscountAmounts {
	if before < 0 || after < 0 || before < after || before > math.MaxInt32 {
		return nil
	}
	return &DiscountAmounts{Before: before, After: after, Savings: before - after}
}

// ValidFor 校验持久化快照的完整算术约束和所属扣款；JSON 重载不经过构造函数，必须再次验证。
func (amounts *DiscountAmounts) ValidFor(charged int) bool {
	return amounts != nil && charged >= 0 && amounts.After == charged &&
		amounts.Before >= charged && amounts.Before <= math.MaxInt32 &&
		amounts.Savings == amounts.Before-charged
}

// AddToLog 仅通过公开字段写入契约输出金额，保持 types 不依赖 model。
func (amounts *DiscountAmounts) AddToLog(other interface{ SetPublic(string, any) bool }) {
	if amounts == nil || !amounts.ValidFor(amounts.After) || other == nil {
		return
	}
	other.SetPublic("quota_before_discount", amounts.Before)
	other.SetPublic("quota_after_discount", amounts.After)
	other.SetPublic("discount_quota", amounts.Savings)
}

// ChannelQuota 返回与本次扣费匹配的折前额度；历史快照缺失时保留已知扣费口径，不反推。
func (amounts *DiscountAmounts) ChannelQuota(charged int) int {
	if amounts.ValidFor(charged) {
		return amounts.Before
	}
	return charged
}
