package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

// 视频结算标记与余额影子在同一个Lua操作内建立；主库决定提交后才能消费标记。
// 影子保留尚未批量落库的预扣，缓存过期/淘汰时不能直接用落后的DB余额覆盖它。
var ErrQuotaCachePending = errors.New("quota cache synchronization is pending; retry later")

func videoUserQuotaKey(id int) string      { return fmt.Sprintf("quota:video:user:%d", id) }
func videoTokenQuotaKey(key string) string { return "quota:video:" + getTokenCacheKey(key) }

const prepareVideoQuotaScript = `
local previous=tonumber(redis.call('HGET',KEYS[3],'TaskID') or '0')
if previous>0 then return previous end
local uq=redis.call('HGET',KEYS[1],'Quota') or redis.call('HGET',KEYS[3],'Quota')
local tq=redis.call('HGET',KEYS[2],'RemainQuota') or redis.call('HGET',KEYS[4],'RemainQuota')
local used=redis.call('HGET',KEYS[2],'UsedQuota') or redis.call('HGET',KEYS[4],'UsedQuota')
redis.call('HSET',KEYS[3],'TaskID',ARGV[1],'Actual',ARGV[2],'Reserved',ARGV[3],'Wallet',ARGV[4],'TokenCacheKey',ARGV[5])
if uq then redis.call('HSET',KEYS[3],'Quota',uq) end
if redis.call('EXISTS',KEYS[1])==1 then redis.call('HSET',KEYS[1],'VideoQuotaPending',ARGV[1]) end
if ARGV[5]~='' then
 redis.call('HSET',KEYS[4],'TaskID',ARGV[1],'UserID',ARGV[6])
 if tq then redis.call('HSET',KEYS[4],'RemainQuota',tq) end
 if used then redis.call('HSET',KEYS[4],'UsedQuota',used) end
 if redis.call('EXISTS',KEYS[2])==1 then redis.call('HSET',KEYS[2],'VideoQuotaPending',ARGV[1]) end
end
return 0`

const finishVideoQuotaScript = `
if redis.call('HGET',KEYS[3],'TaskID')~=ARGV[1] then return 0 end
local delta=tonumber(ARGV[2])
local wallet=ARGV[3]=='1'
local uq=redis.call('HGET',KEYS[3],'Quota')
if uq and wallet then redis.call('HINCRBY',KEYS[3],'Quota',delta) end
if redis.call('EXISTS',KEYS[1])==1 then
 if uq then redis.call('HSET',KEYS[1],'Quota',redis.call('HGET',KEYS[3],'Quota')) end
 redis.call('HDEL',KEYS[1],'VideoQuotaPending')
 redis.call('DEL',KEYS[3])
else
 if uq then redis.call('HSET',KEYS[3],'TaskID','0') else redis.call('DEL',KEYS[3]) end
end
if ARGV[4]~='' and redis.call('HGET',KEYS[4],'TaskID')==ARGV[1] then
 local tq=redis.call('HGET',KEYS[4],'RemainQuota')
 local used=redis.call('HGET',KEYS[4],'UsedQuota')
 if tq then redis.call('HINCRBY',KEYS[4],'RemainQuota',delta) end
 if used then redis.call('HINCRBY',KEYS[4],'UsedQuota',-delta) end
 if redis.call('EXISTS',KEYS[2])==1 then
  if tq then redis.call('HSET',KEYS[2],'RemainQuota',redis.call('HGET',KEYS[4],'RemainQuota')) end
  if used then redis.call('HSET',KEYS[2],'UsedQuota',redis.call('HGET',KEYS[4],'UsedQuota')) end
  redis.call('HDEL',KEYS[2],'VideoQuotaPending')
  redis.call('DEL',KEYS[4])
 else
  if tq and used then redis.call('HSET',KEYS[4],'TaskID','0') else redis.call('DEL',KEYS[4]) end
 end
end
return 1`

// Redis缓存脚本，正常结算只发送SHA和参数，避免反复传输、解析较长Lua。
var prepareVideoQuota = redis.NewScript(prepareVideoQuotaScript)
var finishVideoQuota = redis.NewScript(finishVideoQuotaScript)

// 调用方已持有用户行写锁，因此旧标记对应的事务必定已提交或回滚。
func prepareVideoQuotaCache(ctx context.Context, tx *gorm.DB, task *Task, actual int, wallet bool, tokenKey string) error {
	if !common.RedisEnabled {
		return nil
	}
	keys := []string{getUserCacheKey(task.UserId), getTokenCacheKey(tokenKey), videoUserQuotaKey(task.UserId), videoTokenQuotaKey(tokenKey)}
	// 标记只保存HMAC缓存键，不向Redis新增明文API令牌。
	cacheKey := ""
	if tokenKey != "" {
		cacheKey = getTokenCacheKey(tokenKey)
	}
	walletFlag := 0
	if wallet {
		walletFlag = 1
	}
	for range 2 {
		previous, err := prepareVideoQuota.Run(ctx, common.RDB, keys, task.ID, actual, task.Quota, walletFlag, cacheKey, task.UserId).Int64()
		if err != nil {
			return fmt.Errorf("prepare video quota cache: %w", err)
		}
		if previous == 0 {
			return nil
		}
		if err := finishVideoQuotaCache(ctx, tx, task.UserId); err != nil {
			return err
		}
	}
	return ErrQuotaCachePending
}

// 根据主库终态判断是否应用差额，Lua以任务ID消费一次；重复恢复不会再次加减。
func finishVideoQuotaCache(ctx context.Context, tx *gorm.DB, userID int) error {
	values, err := common.RDB.HGetAll(ctx, videoUserQuotaKey(userID)).Result()
	if err != nil {
		return err
	}
	taskID, err := strconv.ParseInt(values["TaskID"], 10, 64)
	if len(values) == 0 || values["TaskID"] == "0" {
		return nil
	}
	if err != nil || taskID <= 0 {
		return ErrQuotaCachePending
	}
	actual, err := strconv.Atoi(values["Actual"])
	if err != nil {
		return ErrQuotaCachePending
	}
	reserved, err := strconv.Atoi(values["Reserved"])
	if err != nil {
		return ErrQuotaCachePending
	}
	var stored Task
	err = tx.Where("id = ? AND user_id = ?", taskID, userID).First(&stored).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	delta := 0
	if err == nil && (stored.Status == TaskStatusSuccess || stored.Status == TaskStatusFailure) && stored.Quota == actual {
		delta = reserved - actual
	}
	key := values["TokenCacheKey"]
	return applyVideoQuotaCache(ctx, userID, taskID, delta, values["Wallet"], key)
}

// 正常提交只消费自己的标记，不读取可能属于另一未提交事务的新标记。
func applyVideoQuotaCache(ctx context.Context, userID int, taskID int64, delta int, wallet, key string) error {
	return finishVideoQuota.Run(ctx, common.RDB, []string{getUserCacheKey(userID), key, videoUserQuotaKey(userID), "quota:video:" + key}, taskID, delta, wallet, key).Err()
}

// 热缓存通过内嵌标记触发恢复；冷缓存先检查标记，再持行锁读取，禁止旧快照晚回填。
func recoverVideoQuotaCache(ctx context.Context, userID int) error {
	if !common.RedisEnabled {
		return nil
	}
	// 异常恢复不能无限等待另一事务的行锁；超时交给调用方稍后重试。
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	id, err := common.RDB.HGet(ctx, videoUserQuotaKey(userID), "TaskID").Int64()
	if errors.Is(err, redis.Nil) || err == nil && id == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockSQLiteQuotaCache(tx.Unscoped().Model(&User{}).Where("id = ?", userID)); err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx.Unscoped()).Select("id").Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		return finishVideoQuotaCache(ctx, tx, userID)
	})
}

// SQLite没有SELECT FOR UPDATE，WAL读事务也不会阻止另一写事务提交。
// 冷填充/恢复先取得真实写锁，再读快照；其他数据库继续使用行级FOR UPDATE。
func lockSQLiteQuotaCache(query *gorm.DB) error {
	if query.Dialector.Name() != "sqlite" {
		return nil
	}
	return query.UpdateColumn("id", gorm.Expr("id")).Error
}

func recoverVideoTokenQuotaCache(ctx context.Context, key string) error {
	if !common.RedisEnabled {
		return nil
	}
	userID, err := common.RDB.HGet(ctx, videoTokenQuotaKey(key), "UserID").Int()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	if err != nil {
		return err
	}
	return recoverVideoQuotaCache(ctx, userID)
}
