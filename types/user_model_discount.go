package types

import "maps"

// UserModelDiscountSnapshot 是请求可共享的只读折扣快照；零值表示没有专属折扣。
// 内部映射不对外暴露，改价只替换快照，已开始的请求继续持有原版本。
type UserModelDiscountSnapshot struct {
	discounts      map[string]int
	estimatedBytes int64
}

// NewUserModelDiscountSnapshot 复制输入以取得独立所有权，只在回源或显式构造快照时调用。
func NewUserModelDiscountSnapshot(discounts map[string]int) UserModelDiscountSnapshot {
	// 预算包含映射预留空间、字符串和缓存条目的保守估算，不代表进程精确 RSS。
	const snapshotOverhead = 256
	const ruleOverhead = 128
	snapshot := UserModelDiscountSnapshot{discounts: maps.Clone(discounts), estimatedBytes: snapshotOverhead}
	for name := range discounts {
		snapshot.estimatedBytes += int64(len(name) + ruleOverhead)
	}
	return snapshot
}

// DiscountBPS 读取已归一化模型名的折扣，返回 0 表示没有专属规则。
func (snapshot UserModelDiscountSnapshot) DiscountBPS(modelName string) int {
	return snapshot.discounts[modelName]
}

// Copy 仅供需要可变规则集的管理或兼容调用方使用，不用于请求热路径。
func (snapshot UserModelDiscountSnapshot) Copy() map[string]int {
	return maps.Clone(snapshot.discounts)
}

// EstimatedBytes 返回缓存准入使用的估算成本；在途请求持有的旧版本不属于缓存预算。
func (snapshot UserModelDiscountSnapshot) EstimatedBytes() int64 {
	return snapshot.estimatedBytes
}
