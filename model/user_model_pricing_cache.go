package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/samber/hot/pkg/lru"
	"golang.org/x/sync/singleflight"
)

const (
	userModelPricingQueryTimeout = 5 * time.Second
	userModelPricingCacheTTL     = 30 * time.Second
	userModelPricingFenceTTL     = 120 * time.Second
)

// 缓存只保存完整版本的快照，绝对过期时间从回源开始计算，避免慢查询延长旧版本寿命。
type userModelPricingSnapshot struct {
	discounts hosttypes.UserModelDiscountSnapshot
	revision  int64
	expiresAt time.Time
}

// pricingSnapshotCache 在同一锁内维护版本、LRU 顺序及预算，不启动后台清理协程。
type pricingSnapshotCache struct {
	sync.Mutex
	entries   *lru.LRUCache[string, userModelPricingSnapshot]
	maxUsers  int
	maxBytes  int64
	usedBytes int64
	loads     singleflight.Group
}

var userModelPricingCache = &pricingSnapshotCache{}

// invalidateUserModelPricing 在硬删除提交后清理本地快照及共享版本；鉴权墓碑继续阻止旧身份访问。
func invalidateUserModelPricing(userID int) error {
	cache := userModelPricingCache
	key := fmt.Sprintf("%p:%d", DB, userID)
	cache.Lock()
	if cache.entries != nil {
		if snapshot, exists := cache.entries.Peek(key); exists {
			cache.entries.Delete(key)
			cache.usedBytes -= snapshot.discounts.EstimatedBytes()
		}
	}
	cache.Unlock()
	if !common.RedisEnabled {
		return nil
	}
	if common.RDB == nil {
		return fmt.Errorf("pricing Redis client is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return common.RDB.Del(ctx, userModelPricingVersionKeys(userID)...).Err()
}

// initLocked 延迟读取启动配置，让 .env 初始化先完成；0 可关闭本地缓存。
func (cache *pricingSnapshotCache) initLocked() {
	if cache.entries != nil {
		return
	}
	const defaultUsers = 4096
	const defaultMB = 64
	cache.maxUsers = common.GetEnvOrDefault("USER_MODEL_PRICING_CACHE_USERS", defaultUsers)
	budgetMB := common.GetEnvOrDefault("USER_MODEL_PRICING_CACHE_MB", defaultMB)
	if cache.maxUsers < 0 || cache.maxUsers > 65536 {
		common.SysError("USER_MODEL_PRICING_CACHE_USERS must be 0-65536; using default")
		cache.maxUsers = defaultUsers
	}
	if budgetMB < 0 || budgetMB > 1024 {
		common.SysError("USER_MODEL_PRICING_CACHE_MB must be 0-1024; using default")
		budgetMB = defaultMB
	}
	cache.maxBytes = int64(budgetMB) << 20
	cache.entries = lru.NewLRUCache[string, userModelPricingSnapshot](max(1, cache.maxUsers))
}

// get 只返回版本匹配且未过期的快照；成功命中才提升访问顺序，不续期。
func (cache *pricingSnapshotCache) get(key string, revision int64) (userModelPricingSnapshot, bool) {
	cache.Lock()
	defer cache.Unlock()
	cache.initLocked()
	snapshot, exists := cache.entries.Peek(key)
	if !exists {
		return userModelPricingSnapshot{}, false
	}
	if time.Now().Before(snapshot.expiresAt) {
		if snapshot.revision == revision {
			cache.entries.Get(key)
			return snapshot, true
		}
		return userModelPricingSnapshot{}, false
	}
	cache.entries.Delete(key)
	cache.usedBytes -= snapshot.discounts.EstimatedBytes()
	return userModelPricingSnapshot{}, false
}

// put 拒绝迟到的旧版本，按最近最少使用顺序同时满足用户数和估算内存预算。
func (cache *pricingSnapshotCache) put(key string, snapshot userModelPricingSnapshot) {
	cache.Lock()
	defer cache.Unlock()
	cache.initLocked()
	old, exists := cache.entries.Peek(key)
	if exists && old.revision > snapshot.revision {
		return
	}
	if exists {
		cache.entries.Delete(key)
		cache.usedBytes -= old.discounts.EstimatedBytes()
	}
	cost := snapshot.discounts.EstimatedBytes()
	if cache.maxUsers == 0 || cache.maxBytes == 0 || cost > cache.maxBytes || !time.Now().Before(snapshot.expiresAt) {
		return
	}
	for cache.entries.Len() >= cache.maxUsers || cache.usedBytes+cost > cache.maxBytes {
		_, victim, ok := cache.entries.DeleteOldest()
		if !ok {
			break
		}
		cache.usedBytes -= victim.discounts.EstimatedBytes()
	}
	cache.entries.Set(key, snapshot)
	cache.usedBytes += cost
}

// userModelPricingVersionKeys 使用专用键，不与用户鉴权、余额和偏好缓存共享写入通道。
func userModelPricingVersionKeys(userID int) []string {
	id := strconv.Itoa(userID)
	return []string{"pricing:user:version:" + id, "pricing:user:pending:" + id}
}

// pricingVersionCheck 只允许模型层由 Redis 结果构造，不能从客户端请求注入版本。
type pricingVersionCheck struct {
	floor   int64
	healthy bool
}

func parsePricingVersions(values []interface{}) pricingVersionCheck {
	check := pricingVersionCheck{healthy: true}
	for _, value := range values {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return pricingVersionCheck{}
		}
		version, err := strconv.ParseInt(text, 10, 64)
		if err != nil || version < 1 {
			return pricingVersionCheck{}
		}
		check.floor = max(check.floor, version)
	}
	return check
}

// publishUserModelPricingVersion 单调发布版本屏障；回滚留下的屏障只造成回源，过期后自动恢复。
// 屏障寿命超过查询预算与所有旧快照寿命之和，提交后的发布失败也不会放行旧缓存。
func publishUserModelPricingVersion(ctx context.Context, userID int, revision int64, pending bool) error {
	if common.RDB == nil {
		return fmt.Errorf("pricing Redis client is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// 用十进制字符串比较完整 int64，避免 Lua 双精度数丢失大版本号精度。
	const script = `
local function le(a, b)
  if #a ~= #b then return #a < #b end
  return a <= b
end
local index = 1
if ARGV[2] == 'pending' then index = 2 end
local current = redis.call('GET', KEYS[index]) or '0'
if le(current, ARGV[1]) then
  redis.call('SET', KEYS[index], ARGV[1], 'EX', ARGV[3])
end
if index == 1 then
  local fence = redis.call('GET', KEYS[2]) or '0'
  if le(fence, ARGV[1]) then redis.call('DEL', KEYS[2]) end
end
return 1`
	mode := "committed"
	ttl := 600
	if pending {
		mode = "pending"
		ttl = int(userModelPricingFenceTTL / time.Second)
	}
	return common.RDB.Eval(ctx, script, userModelPricingVersionKeys(userID), revision, mode, ttl).Err()
}

// GetUserModelDiscountSnapshotContext 每次只校验 Redis 版本，命中后直接共享只读快照。
// Redis 缺失或故障时从数据库读取，不能将异常或未知版本解释成原价。无后台永久协程。
func GetUserModelDiscountSnapshotContext(ctx context.Context, userID int) (hosttypes.UserModelDiscountSnapshot, error) {
	return getUserModelDiscountSnapshot(ctx, userID, nil)
}

// getUserModelDiscountSnapshot 复用本次请求 Pipeline 验证结果；独立入口仍自行读取 Redis。
func getUserModelDiscountSnapshot(ctx context.Context, userID int, prefetched *pricingVersionCheck) (hosttypes.UserModelDiscountSnapshot, error) {
	if userID <= 0 || DB == nil {
		return hosttypes.UserModelDiscountSnapshot{}, fmt.Errorf("invalid pricing user or database")
	}
	// 热命中只需要 Redis 操作的截止时间；回源时才分配整个查询的计时器。
	deadline := time.Now().Add(userModelPricingQueryTimeout)
	if err := ctx.Err(); err != nil {
		return hosttypes.UserModelDiscountSnapshot{}, err
	}
	var floor int64
	redisHealthy := false
	if prefetched != nil {
		floor, redisHealthy = prefetched.floor, prefetched.healthy
	} else if common.RedisEnabled && common.RDB != nil {
		redisCtx, redisCancel := context.WithTimeout(ctx, time.Second)
		values, err := common.RDB.MGet(redisCtx, userModelPricingVersionKeys(userID)...).Result()
		redisCancel()
		if err == nil {
			check := parsePricingVersions(values)
			floor, redisHealthy = check.floor, check.healthy
		}
	}
	key := fmt.Sprintf("%p:%d", DB, userID)
	if redisHealthy && floor > 0 {
		if cached, exists := userModelPricingCache.get(key, floor); exists {
			return cached.discounts, nil
		}
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// 同版本的并发未命中只回源一次；发起者取消时，仍有效的等待者最多重试一次。
	for attempt := 0; ; attempt++ {
		result := userModelPricingCache.loads.DoChan(fmt.Sprintf("%s:%d:%t", key, floor, redisHealthy), func() (any, error) {
			snapshot, err := loadUserModelPricingSnapshot(ctx, userID, redisHealthy)
			if err != nil {
				return nil, err
			}
			if !snapshot.expiresAt.IsZero() && snapshot.revision >= floor {
				userModelPricingCache.put(key, snapshot)
			}
			return snapshot.discounts, nil
		})
		select {
		case <-ctx.Done():
			return hosttypes.UserModelDiscountSnapshot{}, ctx.Err()
		case loaded := <-result:
			if loaded.Err != nil {
				if attempt == 0 && ctx.Err() == nil && (errors.Is(loaded.Err, context.Canceled) || errors.Is(loaded.Err, context.DeadlineExceeded)) {
					continue
				}
				return hosttypes.UserModelDiscountSnapshot{}, loaded.Err
			}
			return loaded.Val.(hosttypes.UserModelDiscountSnapshot), nil
		}
	}
}

// GetUserModelDiscountBPSContext 保留可变 Map 的旧接口；请求计费应使用只读快照接口。
func GetUserModelDiscountBPSContext(ctx context.Context, userID int) (map[string]int, error) {
	snapshot, err := GetUserModelDiscountSnapshotContext(ctx, userID)
	if err != nil {
		return nil, err
	}
	return snapshot.Copy(), nil
}
