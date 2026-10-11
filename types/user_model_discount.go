package types

import (
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	"sort"
	"time"
)

const (
	UserModelPricingSingle    = "single"
	UserModelPricingScheduled = "scheduled"
	UserModelPricingLayered   = "layered"
)

type UserModelDiscountWindow struct {
	DiscountBPS int    `json:"discount_bps"`
	StartTime   int64  `json:"start_time"`
	EndTime     *int64 `json:"end_time"`
	Expression  string `json:"expression,omitempty"`
	// 仅在选中临时规则时冻结永久比例，随任务快照保存，不写入规则表。
	PermanentDiscountBPS int `json:"permanent_discount_bps,omitempty"`
}

func (window UserModelDiscountWindow) ActiveAt(at int64) bool {
	return at >= window.StartTime && (window.EndTime == nil || at < *window.EndTime)
}

type UserModelDiscountConfig struct {
	Mode      string                    `json:"mode"`
	Periods   []UserModelDiscountWindow `json:"periods"`
	Permanent *UserModelDiscountWindow  `json:"permanent,omitempty"`
}

// UserModelDiscountSnapshot 是请求可共享的只读折扣快照；零值表示没有专属折扣。
// 内部映射不对外暴露，改价只替换快照，已开始的请求继续持有原版本。
type UserModelDiscountSnapshot struct {
	discounts      map[string]UserModelDiscountConfig
	estimatedBytes int64
}

// NewUserModelDiscountSnapshot 复制输入以取得独立所有权，只在回源或显式构造快照时调用。
func NewUserModelDiscountSnapshot(discounts map[string]int) UserModelDiscountSnapshot {
	configs := make(map[string]UserModelDiscountConfig, len(discounts))
	for name, bps := range discounts {
		configs[name] = UserModelDiscountConfig{Mode: UserModelPricingSingle, Periods: []UserModelDiscountWindow{{DiscountBPS: bps}}}
	}
	return NewUserModelDiscountScheduleSnapshot(configs)
}

func NewUserModelDiscountScheduleSnapshot(configs map[string]UserModelDiscountConfig) UserModelDiscountSnapshot {
	// 预算包含映射预留空间、字符串和缓存条目的保守估算，不代表进程精确 RSS。
	const snapshotOverhead = 256
	const ruleOverhead = 128
	snapshot := UserModelDiscountSnapshot{discounts: make(map[string]UserModelDiscountConfig, len(configs)), estimatedBytes: snapshotOverhead}
	for name, config := range configs {
		if config.Permanent != nil {
			permanent := cloneDiscountWindow(*config.Permanent)
			config.Permanent = &permanent
		}
		config.Periods = cloneDiscountWindows(config.Periods)
		sort.Slice(config.Periods, func(i, j int) bool { return config.Periods[i].StartTime < config.Periods[j].StartTime })
		snapshot.discounts[name] = config
		snapshot.estimatedBytes += int64(len(name) + ruleOverhead + len(config.Periods)*64)
		if config.Permanent != nil {
			snapshot.estimatedBytes += int64(64 + len(config.Permanent.Expression))
		}
		for _, period := range config.Periods {
			snapshot.estimatedBytes += int64(len(period.Expression))
		}
	}
	return snapshot
}

// DiscountBPS 读取已归一化模型名的折扣，返回 0 表示没有专属规则。
func (snapshot UserModelDiscountSnapshot) DiscountBPS(modelName string) int {
	return snapshot.DiscountBPSAt(modelName, time.Now().Unix())
}

// DiscountBPSAt 使用请求固定时刻求值，时间边界不依赖缓存过期或版本变化。
func (snapshot UserModelDiscountSnapshot) DiscountBPSAt(modelName string, at int64) int {
	rule, source := snapshot.RuleAt(modelName, at)
	if source == "" {
		return 0
	}
	if rule.Expression != "" {
		return 0
	}
	return rule.DiscountBPS
}

func (snapshot UserModelDiscountSnapshot) RuleAt(modelName string, at int64) (UserModelDiscountWindow, string) {
	config := snapshot.discounts[modelName]
	periods := config.Periods
	i := sort.Search(len(periods), func(i int) bool { return periods[i].StartTime > at }) - 1
	if i >= 0 && periods[i].ActiveAt(at) {
		window := cloneDiscountWindow(periods[i])
		if config.Mode != UserModelPricingLayered && window.EndTime == nil {
			return window, "permanent"
		}
		if config.Mode == UserModelPricingLayered {
			window.PermanentDiscountBPS = 10000
			if config.Permanent != nil {
				window.PermanentDiscountBPS = config.Permanent.DiscountBPS
			}
		}
		return window, "temporary"
	}
	if config.Permanent != nil {
		return cloneDiscountWindow(*config.Permanent), "permanent"
	}
	return UserModelDiscountWindow{DiscountBPS: 10000}, ""
}

func (snapshot UserModelDiscountSnapshot) NextChangeAt(modelName string, at int64) int64 {
	var next int64
	for _, period := range snapshot.discounts[modelName].Periods {
		if period.StartTime > at && (next == 0 || period.StartTime < next) {
			next = period.StartTime
		}
		if period.EndTime != nil && *period.EndTime > at && (next == 0 || *period.EndTime < next) {
			next = *period.EndTime
		}
	}
	return next
}

func cloneDiscountWindows(periods []UserModelDiscountWindow) []UserModelDiscountWindow {
	result := append([]UserModelDiscountWindow(nil), periods...)
	for i := range result {
		result[i] = cloneDiscountWindow(result[i])
	}
	return result
}

func cloneDiscountWindow(window UserModelDiscountWindow) UserModelDiscountWindow {
	if window.EndTime != nil {
		end := *window.EndTime
		window.EndTime = &end
	}
	return window
}

func (snapshot UserModelDiscountSnapshot) Configurations() map[string]UserModelDiscountConfig {
	result := make(map[string]UserModelDiscountConfig, len(snapshot.discounts))
	for name, config := range snapshot.discounts {
		if config.Permanent != nil {
			permanent := cloneDiscountWindow(*config.Permanent)
			config.Permanent = &permanent
		}
		config.Periods = cloneDiscountWindows(config.Periods)
		result[name] = config
	}
	return result
}

type UserModelDiscountEvaluation struct {
	Rule        UserModelDiscountWindow `json:"rule"`
	Source      string                  `json:"source"`
	Context     discountexpr.Context    `json:"context"`
	DiscountBPS int                     `json:"discount_bps"`
	Error       string                  `json:"error,omitempty"`
}

func (evaluation *UserModelDiscountEvaluation) Evaluate() (int, error) {
	if evaluation.Rule.Expression == "" {
		return evaluation.Rule.DiscountBPS, nil
	}
	context := evaluation.Context
	context.PermanentDiscountBPS = evaluation.Rule.PermanentDiscountBPS
	return discountexpr.Evaluate(evaluation.Rule.Expression, context)
}

func (evaluation *UserModelDiscountEvaluation) AddToLog(other interface{ SetPublic(string, any) bool }) {
	if evaluation == nil {
		return
	}
	other.SetPublic("user_discount_source", evaluation.Source)
	if evaluation.Rule.Expression != "" {
		other.SetPublic("user_discount_expression", evaluation.Rule.Expression)
		other.SetPublic("user_discount_expression_bps", evaluation.DiscountBPS)
	}
	if evaluation.Error != "" {
		other.SetPublic("user_discount_error", evaluation.Error)
	}
}

// Copy 仅供需要可变规则集的管理或兼容调用方使用，不用于请求热路径。
func (snapshot UserModelDiscountSnapshot) Copy() map[string]int {
	result := make(map[string]int, len(snapshot.discounts))
	now := time.Now().Unix()
	for name := range snapshot.discounts {
		if bps := snapshot.DiscountBPSAt(name, now); bps != 0 {
			result[name] = bps
		}
	}
	return result
}

// EstimatedBytes 返回缓存准入使用的估算成本；在途请求持有的旧版本不属于缓存预算。
func (snapshot UserModelDiscountSnapshot) EstimatedBytes() int64 {
	return snapshot.estimatedBytes
}
