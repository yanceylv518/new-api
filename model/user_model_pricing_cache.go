package model

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"golang.org/x/sync/singleflight"
)

const (
	userModelPricingQueryTimeout = 5 * time.Second
	userModelPricingCacheTTL     = 30 * time.Second
	userModelPricingCacheLimit   = 256
	userModelPricingFenceTTL     = 120 * time.Second
)

// 缓存只保存完整版本的快照，绝对过期时间从回源开始计算，避免慢查询延长旧版本寿命。
type userModelPricingSnapshot struct {
	discounts map[string]int
	revision  int64
	expiresAt time.Time
}

var userModelPricingCache = struct {
	sync.Mutex
	entries map[string]userModelPricingSnapshot
	loads   singleflight.Group
}{entries: make(map[string]userModelPricingSnapshot)}

// userModelPricingVersionKeys 使用专用键，不与用户鉴权、余额和偏好缓存共享写入通道。
func userModelPricingVersionKeys(userID int) []string {
	return []string{fmt.Sprintf("pricing:user:version:%d", userID), fmt.Sprintf("pricing:user:pending:%d", userID)}
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

// GetUserModelDiscountBPSContext 每次只校验 Redis 版本，命中后复用本地快照；返回副本防止调用方污染缓存。
// Redis 缺失或故障时从数据库读取，不能将异常或未知版本解释成原价。无后台永久协程。
func GetUserModelDiscountBPSContext(ctx context.Context, userID int) (map[string]int, error) {
	if userID <= 0 || DB == nil {
		return nil, fmt.Errorf("invalid pricing user or database")
	}
	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var floor int64
	redisHealthy := false
	if common.RedisEnabled && common.RDB != nil {
		redisCtx, redisCancel := context.WithTimeout(ctx, time.Second)
		values, err := common.RDB.MGet(redisCtx, userModelPricingVersionKeys(userID)...).Result()
		redisCancel()
		if err == nil {
			redisHealthy = true
			for _, value := range values {
				if value == nil {
					continue
				}
				version, parseErr := strconv.ParseInt(fmt.Sprint(value), 10, 64)
				if parseErr != nil || version < 1 {
					redisHealthy = false
					break
				}
				if version > floor {
					floor = version
				}
			}
		}
	}
	key := fmt.Sprintf("%p:%d", DB, userID)
	if redisHealthy && floor > 0 {
		userModelPricingCache.Lock()
		cached, exists := userModelPricingCache.entries[key]
		userModelPricingCache.Unlock()
		if exists && cached.revision == floor && time.Now().Before(cached.expiresAt) {
			return maps.Clone(cached.discounts), nil
		}
	}
	// 同版本的并发未命中只回源一次；发起者取消时，仍有效的等待者最多重试一次。
	for attempt := 0; ; attempt++ {
		result := userModelPricingCache.loads.DoChan(fmt.Sprintf("%s:%d:%t", key, floor, redisHealthy), func() (any, error) {
			snapshot, err := loadUserModelPricingSnapshot(ctx, userID, redisHealthy)
			if err != nil {
				return nil, err
			}
			if !snapshot.expiresAt.IsZero() && snapshot.revision >= floor {
				userModelPricingCache.Lock()
				if old, exists := userModelPricingCache.entries[key]; !exists || snapshot.revision >= old.revision {
					if !exists && len(userModelPricingCache.entries) >= userModelPricingCacheLimit {
						for victim := range userModelPricingCache.entries {
							delete(userModelPricingCache.entries, victim)
							break
						}
					}
					userModelPricingCache.entries[key] = snapshot
				}
				userModelPricingCache.Unlock()
			}
			return snapshot.discounts, nil
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case loaded := <-result:
			if loaded.Err != nil {
				if attempt == 0 && ctx.Err() == nil && (errors.Is(loaded.Err, context.Canceled) || errors.Is(loaded.Err, context.DeadlineExceeded)) {
					continue
				}
				return nil, loaded.Err
			}
			return maps.Clone(loaded.Val.(map[string]int)), nil
		}
	}
}
