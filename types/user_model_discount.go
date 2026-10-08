package types

import (
	"sort"
	"time"
)

const (
	UserModelPricingSingle    = "single"
	UserModelPricingScheduled = "scheduled"
)

type UserModelDiscountWindow struct {
	DiscountBPS int    `json:"discount_bps"`
	StartTime   int64  `json:"start_time"`
	EndTime     *int64 `json:"end_time"`
}

func (window UserModelDiscountWindow) ActiveAt(at int64) bool {
	return at >= window.StartTime && (window.EndTime == nil || at < *window.EndTime)
}

type UserModelDiscountConfig struct {
	Mode    string                    `json:"mode"`
	Periods []UserModelDiscountWindow `json:"periods"`
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
		config.Periods = cloneDiscountWindows(config.Periods)
		sort.Slice(config.Periods, func(i, j int) bool { return config.Periods[i].StartTime < config.Periods[j].StartTime })
		snapshot.discounts[name] = config
		snapshot.estimatedBytes += int64(len(name) + ruleOverhead + len(config.Periods)*48)
	}
	return snapshot
}

// DiscountBPS 读取已归一化模型名的折扣，返回 0 表示没有专属规则。
func (snapshot UserModelDiscountSnapshot) DiscountBPS(modelName string) int {
	return snapshot.DiscountBPSAt(modelName, time.Now().Unix())
}

// DiscountBPSAt 使用请求固定时刻求值，时间边界不依赖缓存过期或版本变化。
func (snapshot UserModelDiscountSnapshot) DiscountBPSAt(modelName string, at int64) int {
	periods := snapshot.discounts[modelName].Periods
	i := sort.Search(len(periods), func(i int) bool { return periods[i].StartTime > at }) - 1
	if i >= 0 && periods[i].ActiveAt(at) {
		return periods[i].DiscountBPS
	}
	return 0
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
		if result[i].EndTime != nil {
			end := *result[i].EndTime
			result[i].EndTime = &end
		}
	}
	return result
}

func (snapshot UserModelDiscountSnapshot) Configurations() map[string]UserModelDiscountConfig {
	result := make(map[string]UserModelDiscountConfig, len(snapshot.discounts))
	for name, config := range snapshot.discounts {
		config.Periods = cloneDiscountWindows(config.Periods)
		result[name] = config
	}
	return result
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
