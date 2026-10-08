package model

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
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
				assert.NoError(t, db.Migrator().DropTable(&UserModelPricingHistory{}, &UserModelPricing{}, &UserModelPricingRevision{}, &Task{}, &Midjourney{}, &Ability{}, &User{}))
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
			// 先创建目标用户、能力和独立折扣表，重复安装折扣表必须保持幂等。
			require.NoError(t, db.AutoMigrate(&User{}, &Task{}, &Midjourney{}, &Ability{}))
			user := User{Username: "pricing-integration", Password: "unused"}
			require.NoError(t, db.Create(&user).Error)
			for range 2 {
				require.NoError(t, db.AutoMigrate(&UserModelPricing{}, &UserModelPricingRevision{}, &UserModelPricingHistory{}))
				require.NoError(t, migrateUserModelPricingScheduleIndex(db))
			}
			assertUserModelPricingScheduleMigration(t, db)
			assertUserModelPricingHistoryCapacityAndUpgrade(t, db)
			// 启用能力是总览判断模型是否仍存在的权威目录。
			require.NoError(t, db.Create(&[]Ability{
				{Group: "default", Model: "Model-A", ChannelId: 1, Enabled: true},
				{Group: "default", Model: "model-a", ChannelId: 2, Enabled: true},
				{Group: "default", Model: "café", ChannelId: 3, Enabled: true},
				{Group: "default", Model: "cafe", ChannelId: 4, Enabled: true},
			}).Error)
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
			// 总览及分页在真实引擎验证，统计不能因排序规则合并大小写不同的模型。
			overview, err := GetUserModelPricingOverview(t.Context(), "", common.RoleRootUser, 0, 20, UserModelPricingOverviewFilters{}, true)
			require.NoError(t, err)
			assert.EqualValues(t, 4, overview.TotalModels)
			require.Len(t, overview.Items, 1)
			assert.Empty(t, overview.Items[0].Rules)
			assert.Equal(t, 4, overview.Items[0].RuleCount)
			assert.Equal(t, 6000, overview.Items[0].MinDiscountBPS)
			page, total, err := GetUserModelPricingRulePage(t.Context(), user.Id, common.RoleRootUser, "Model-A", 1)
			require.NoError(t, err)
			require.NotEmpty(t, page)
			assert.EqualValues(t, len(page), total)
			for _, rule := range page {
				assert.Contains(t, []string{"Model-A", "model-a"}, rule.ModelName)
			}
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
			assertUserModelPricingScheduledOverview(t)
		})
	}
}

type legacyUserModelPricingForMigration struct {
	Id          int    `gorm:"primaryKey"`
	UserId      int    `gorm:"not null;uniqueIndex:idx_user_model_pricing_user_key,priority:1"`
	ModelName   string `gorm:"size:128;not null"`
	ModelKey    string `gorm:"size:64;not null;uniqueIndex:idx_user_model_pricing_user_key,priority:2"`
	DiscountBPS int    `gorm:"not null"`
}

func (legacyUserModelPricingForMigration) TableName() string { return "user_model_pricings" }

func TestUserModelPricingScheduleMigrationSQLite(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	for range 2 {
		require.NoError(t, db.AutoMigrate(&UserModelPricing{}, &UserModelPricingRevision{}, &UserModelPricingHistory{}))
		require.NoError(t, migrateUserModelPricingScheduleIndex(db))
	}
	assertUserModelPricingScheduleMigration(t, db)
	assertUserModelPricingHistoryCapacityAndUpgrade(t, db)
}

func assertUserModelPricingHistoryCapacityAndUpgrade(t *testing.T, db *gorm.DB) {
	t.Helper()
	before := hosttypes.UserModelDiscountConfig{Mode: "single", Periods: []hosttypes.UserModelDiscountWindow{{DiscountBPS: 8000}}}
	legacy, err := newUserModelPricingHistory(987654321, common.RoleCommonUser, 1, "history-capacity-model", "create", 1, time.Now().Unix(), hosttypes.UserModelDiscountConfig{}, before)
	require.NoError(t, err)
	require.NoError(t, db.Create(&legacy).Error)
	if db.Dialector.Name() == "mysql" {
		// 重建原有 TEXT 列，验证含旧数据升级，而不只验证空表的新建类型。
		require.NoError(t, db.Exec("ALTER TABLE user_model_pricing_histories MODIFY COLUMN before_json TEXT NOT NULL, MODIFY COLUMN after_json TEXT NOT NULL").Error)
	}
	after := hosttypes.UserModelDiscountConfig{Mode: "scheduled", Periods: make([]hosttypes.UserModelDiscountWindow, 1000)}
	base := time.Now().Unix()
	for i := range after.Periods {
		end := base + int64(i+1)*3600
		after.Periods[i] = hosttypes.UserModelDiscountWindow{DiscountBPS: 6000, StartTime: base + int64(i)*3600, EndTime: &end}
	}
	after.Periods[999].EndTime = nil
	large, err := newUserModelPricingHistory(legacy.UserId, common.RoleCommonUser, 1, legacy.ModelName, "update", 2, base, after, after)
	require.NoError(t, err)
	require.Greater(t, len(large.BeforeJSON), 65535)
	for attempt := range 2 {
		require.NoError(t, db.AutoMigrate(&UserModelPricingHistory{}))
		var kept UserModelPricingHistory
		require.NoError(t, db.First(&kept, legacy.Id).Error)
		assert.Equal(t, legacy.BeforeJSON, kept.BeforeJSON)
		assert.Equal(t, legacy.AfterJSON, kept.AfterJSON)
		columns, err := db.Migrator().ColumnTypes(&UserModelPricingHistory{})
		require.NoError(t, err)
		for _, column := range columns {
			if column.Name() != "before_json" && column.Name() != "after_json" {
				continue
			}
			expected := "text"
			if db.Dialector.Name() == "mysql" {
				expected = "mediumtext"
			}
			assert.Equal(t, expected, strings.ToLower(column.DatabaseTypeName()))
		}
		if attempt == 0 {
			require.NoError(t, db.Create(&large).Error)
		}
		var stored UserModelPricingHistory
		require.NoError(t, db.First(&stored, large.Id).Error)
		assert.Equal(t, large.BeforeJSON, stored.BeforeJSON)
		assert.Equal(t, large.AfterJSON, stored.AfterJSON)
		var decoded hosttypes.UserModelDiscountConfig
		require.NoError(t, common.UnmarshalJsonStr(string(stored.AfterJSON), &decoded))
		assert.Equal(t, after, decoded)
	}
}

func assertUserModelPricingScheduleMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&UserModelPricing{}).Count(&count).Error)
	require.Zero(t, count, "migration fixture requires an empty isolated rules table")
	require.NoError(t, db.Migrator().DropTable(&UserModelPricing{}))
	require.NoError(t, db.AutoMigrate(&legacyUserModelPricingForMigration{}))
	legacy := legacyUserModelPricingForMigration{UserId: 12345, ModelName: "migration-model", ModelKey: userModelPricingModelKey("migration-model"), DiscountBPS: 8000}
	require.NoError(t, db.Create(&legacy).Error)
	for range 2 {
		require.NoError(t, db.AutoMigrate(&UserModelPricing{}, &UserModelPricingRevision{}, &UserModelPricingHistory{}))
		require.NoError(t, migrateUserModelPricingScheduleIndex(db))
		var row UserModelPricing
		require.NoError(t, db.First(&row, legacy.Id).Error)
		assert.Equal(t, 8000, row.DiscountBPS)
		assert.Equal(t, "single", row.Mode)
		assert.Zero(t, row.StartTime)
		assert.Zero(t, row.Slot)
		assert.Nil(t, row.EndTime)
		assert.True(t, db.Migrator().HasIndex(&UserModelPricing{}, "idx_user_model_pricing_slot"))
		assert.False(t, db.Migrator().HasIndex(&UserModelPricing{}, "idx_user_model_pricing_user_key"))
	}
	require.NoError(t, db.Create(&UserModelPricing{UserId: legacy.UserId, ModelName: legacy.ModelName, Slot: 1, DiscountBPS: 7000}).Error)
	require.Error(t, db.Create(&UserModelPricing{UserId: legacy.UserId, ModelName: legacy.ModelName, Slot: 1, DiscountBPS: 6000}).Error)
	require.NoError(t, db.Where("user_id = ?", legacy.UserId).Delete(&UserModelPricing{}).Error)
}
