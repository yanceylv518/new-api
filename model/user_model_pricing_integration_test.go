package model

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestUserModelPricingExternalDatabases 仅连接专用 Compose 的固定回环端口，显式启用后才运行。
// 测试在独立 pricing_review 数据库创建并清理表，不允许接收任意外部 DSN。
func TestUserModelPricingExternalDatabases(t *testing.T) {
	if os.Getenv("PRICING_EXTERNAL_TESTS") != "1" {
		t.Skip("requires tools/pricing-load/compose.yml and PRICING_EXTERNAL_TESTS=1")
	}
	for _, engine := range []struct {
		name    string
		typeID  common.DatabaseType
		dialect gorm.Dialector
		redisDB int
	}{
		{"mysql", common.DatabaseTypeMySQL, mysql.Open("root@tcp(127.0.0.1:13326)/pricing_review?charset=utf8mb4&parseTime=true&timeout=5s"), 14},
		{"postgres", common.DatabaseTypePostgreSQL, postgres.Open("postgres://postgres@127.0.0.1:15427/pricing_review?sslmode=disable&connect_timeout=5"), 15},
	} {
		t.Run(engine.name, func(t *testing.T) {
			db, err := gorm.Open(engine.dialect, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(8)
			t.Cleanup(func() { _ = sqlDB.Close() })
			// 拒绝覆盖正在进行的负载测试，表清理权限仅在确认隔离库为空后取得。
			if db.Migrator().HasTable(&User{}) {
				var count int64
				require.NoError(t, db.Model(&User{}).Count(&count).Error)
				require.Zero(t, count, "isolated database is occupied by another test")
			}
			oldDB, oldRedis, oldEnabled := DB, common.RDB, common.RedisEnabled
			oldType := common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(engine.typeID)
			initCol()
			common.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:16389", DB: engine.redisDB})
			common.RedisEnabled = true
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(&UserModelPricing{}, &UserModelPricingRevision{}, &Task{}, &Midjourney{}, &User{}))
				assert.NoError(t, common.RDB.FlushDB(context.Background()).Err())
				_ = common.RDB.Close()
				DB, common.RDB, common.RedisEnabled = oldDB, oldRedis, oldEnabled
				common.SetMainDatabaseType(oldType)
				initCol()
			})
			require.NoError(t, common.RDB.Ping(t.Context()).Err())
			var version string
			require.NoError(t, db.Raw("SELECT VERSION()").Scan(&version).Error)
			t.Logf("engine=%s version=%s", engine.name, version)
			// 先创建不带折扣列的目标用户表，再重复安装两张新表。
			require.NoError(t, db.AutoMigrate(&User{}, &Task{}, &Midjourney{}))
			user := User{Username: "pricing-integration", Password: "unused"}
			require.NoError(t, db.Create(&user).Error)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&UserModelPricing{}, &UserModelPricingRevision{}))
			}
			require.False(t, db.Migrator().HasColumn(&User{}, "model_pricing_version"))
			// 两种任务都复用原有 JSON 列；更新金额时必须保留运行状态，重载后可用于退款。
			task := Task{UserId: user.Id, TaskID: "pricing-json-task", Quota: 8}
			task.PrivateData.PluginState = []byte(`{"cursor":"kept"}`)
			require.NoError(t, db.Create(&task).Error)
			task.PrivateData.DiscountAmounts = hosttypes.NewDiscountAmounts(10, 8)
			require.NoError(t, task.UpdateQuotaAndDiscountAmounts())
			var reloaded Task
			require.NoError(t, db.First(&reloaded, task.ID).Error)
			assert.Equal(t, task.PrivateData.DiscountAmounts, reloaded.PrivateData.DiscountAmounts)
			assert.JSONEq(t, `{"cursor":"kept"}`, string(reloaded.PrivateData.PluginState))
			mj := Midjourney{UserId: user.Id, MjId: "pricing-json-mj", Quota: 8, Properties: `{"seed":42}`}
			require.NoError(t, mj.SetDiscountAmounts(hosttypes.NewDiscountAmounts(10, 8)))
			require.NoError(t, db.Create(&mj).Error)
			require.NoError(t, db.First(&mj, mj.Id).Error)
			amounts, err := mj.DiscountAmounts()
			require.NoError(t, err)
			assert.Equal(t, hosttypes.NewDiscountAmounts(10, 8), amounts)
			got, err := GetUserModelDiscountBPSContext(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Empty(t, got)
			// MySQL 默认不区分大小写和重音，哈希唯一键必须支持这些不同模型同时存在。
			discounts := map[string]int{"Model-A": 8000, "model-a": 6000, "café": 7000, "cafe": 9000}
			revision, err := ReplaceUserModelPricingContext(t.Context(), user.Id, discounts, 1)
			require.NoError(t, err)
			assert.EqualValues(t, 2, revision)
			got, err = GetUserModelDiscountBPSContext(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Equal(t, discounts, got)
			require.Error(t, db.Create(&UserModelPricing{UserId: user.Id, ModelName: "Model-A", DiscountBPS: 1000}).Error)
			_, err = ReplaceUserModelPricingContext(t.Context(), user.Id, map[string]int{}, 1)
			require.ErrorIs(t, err, ErrUserModelPricingRevisionConflict)
			// 两个真实连接同时提交相同版本，必须恰好一个成功，另一个返回版本冲突。
			var wg sync.WaitGroup
			start := make(chan struct{})
			results := make(chan error, 2)
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := ReplaceUserModelPricingContext(t.Context(), user.Id, map[string]int{"Model-A": 5000}, 2)
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			var succeeded, conflicted int
			for err := range results {
				if err == nil {
					succeeded++
				} else if errors.Is(err, ErrUserModelPricingRevisionConflict) {
					conflicted++
				} else {
					require.NoError(t, err)
				}
			}
			assert.Equal(t, 1, succeeded)
			assert.Equal(t, 1, conflicted)
			got, err = GetUserModelDiscountBPSContext(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Equal(t, map[string]int{"Model-A": 5000}, got)
			// 持有真实行锁时，管理查询必须受调用方的截止时间限制，取消后连接仍可复用。
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			require.NoError(t, lockForUpdate(tx).First(&User{}, user.Id).Error)
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			_, _, readErr := GetUserModelPricingContext(ctx, user.Id)
			cancel()
			require.NoError(t, tx.Rollback().Error)
			require.Error(t, readErr)
			require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
			revision, err = ReplaceUserModelPricingContext(t.Context(), user.Id, map[string]int{}, 3)
			require.NoError(t, err)
			assert.EqualValues(t, 4, revision)
			got, err = GetUserModelDiscountBPSContext(t.Context(), user.Id)
			require.NoError(t, err)
			assert.Empty(t, got)
		})
	}
}
