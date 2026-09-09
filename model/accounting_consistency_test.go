package model

import (
	"errors"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 整批写入中途失败不得遗失资金；未知提交重试和下一批新增资金不能重复或相互覆盖。
func TestAccountingBatchRollbackAndUnknownCommit(t *testing.T) {
	resetBatchUpdateTestState(t)
	common.BatchUpdateEnabled = true
	user := createReserveTestUser(t, 100)
	token := createReserveTestToken(t, 100)
	channel := Channel{Name: "batch-rollback", Key: "test-only", UsedQuota: 30}
	require.NoError(t, DB.Create(&channel).Error)
	t.Cleanup(func() {
		assert.NoError(t, DB.Delete(&user).Error)
		assert.NoError(t, DB.Delete(&token).Error)
		assert.NoError(t, DB.Delete(&channel).Error)
	})
	addNewRecord(BatchUpdateTypeUserQuota, user.Id, -8)
	addNewRecord(BatchUpdateTypeTokenQuota, token.Id, -8)
	UpdateUserUsedQuotaAndRequestCount(user.Id, 8)
	UpdateChannelUsedQuota(channel.Id, 16)
	const hook = "test:batch-channel-failure"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(hook, func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			tx.AddError(errors.New("injected channel write failure"))
		}
	}))
	batchUpdate()
	require.NoError(t, DB.Callback().Update().Remove(hook))
	require.NotNil(t, pendingAccountingBatch)
	assert.Equal(t, 100, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 100, getTokenFromDB(t, token.Id).RemainQuota)
	var receipts int64
	require.NoError(t, DB.Model(&AccountingBatchReceipt{}).Count(&receipts).Error)
	assert.Zero(t, receipts)
	// 前一批失败期间的新请求保留在下一批，不能被旧批次快照覆盖。
	addNewRecord(BatchUpdateTypeUserQuota, user.Id, -3)
	addNewRecord(BatchUpdateTypeTokenQuota, token.Id, -3)
	UpdateUserUsedQuotaAndRequestCount(user.Id, 3)
	UpdateChannelUsedQuota(channel.Id, 6)
	// 事务已经成功、调用方尚未收到应答：pending 标识必须阻止重放已提交的资金。
	require.NoError(t, commitAccountingBatch(pendingAccountingBatch))
	batchUpdate()
	assert.Equal(t, 92, getUserQuotaFromDB(t, user.Id))
	assert.Equal(t, 92, getTokenFromDB(t, token.Id).RemainQuota)
	batchUpdate()
	batchUpdate()
	var actual User
	require.NoError(t, DB.First(&actual, user.Id).Error)
	assert.Equal(t, 89, actual.Quota)
	assert.Equal(t, 11, actual.UsedQuota)
	assert.Equal(t, 2, actual.RequestCount)
	gotToken := getTokenFromDB(t, token.Id)
	assert.Equal(t, 89, gotToken.RemainQuota)
	assert.Equal(t, 11, gotToken.UsedQuota)
	require.NoError(t, DB.First(&channel, channel.Id).Error)
	assert.EqualValues(t, 52, channel.UsedQuota)
	// 清理标识的应答丢失后只能再次删除，不能重新执行已完成的扣款。
	addNewRecord(BatchUpdateTypeUserQuota, user.Id, -5)
	const deleteHook = "test:batch-delete-response-loss"
	require.NoError(t, DB.Callback().Delete().After("gorm:commit_or_rollback_transaction").Register(deleteHook, func(tx *gorm.DB) {
		if tx.Statement.Table == "accounting_batch_receipts" {
			tx.AddError(errors.New("injected delete response loss"))
		}
	}))
	batchUpdate()
	require.NoError(t, DB.Callback().Delete().Remove(deleteHook))
	batchUpdate()
	assert.Equal(t, 84, getUserQuotaFromDB(t, user.Id))
	require.NoError(t, DB.Model(&AccountingBatchReceipt{}).Count(&receipts).Error)
	assert.Zero(t, receipts)
	// 预扣后全退的净额为零，也要保留令牌最近访问时间更新。
	require.NoError(t, DB.Model(&token).Update("accessed_time", 0).Error)
	addNewRecord(BatchUpdateTypeTokenQuota, token.Id, -7)
	addNewRecord(BatchUpdateTypeTokenQuota, token.Id, 7)
	batchUpdate()
	gotToken = getTokenFromDB(t, token.Id)
	assert.Equal(t, 89, gotToken.RemainQuota)
	assert.Equal(t, 11, gotToken.UsedQuota)
	assert.Positive(t, gotToken.AccessedTime)
}

// 在隔离数据库验证旧表升级、新标识表重复迁移及两种 MySQL RowsAffected 模式。
func TestAccountingExternalDatabases(t *testing.T) {
	if os.Getenv("PRICING_EXTERNAL_TESTS") != "1" {
		t.Skip("requires isolated Compose")
	}
	for _, engine := range []struct {
		name    string
		dialect gorm.Dialector
		kind    common.DatabaseType
	}{
		{"mysql", mysql.Open("root@tcp(127.0.0.1:13326)/pricing_review?charset=utf8mb4&parseTime=true&timeout=5s"), common.DatabaseTypeMySQL},
		{"mysql_found_rows", mysql.Open("root@tcp(127.0.0.1:13326)/pricing_review?charset=utf8mb4&parseTime=true&timeout=5s&clientFoundRows=true"), common.DatabaseTypeMySQL},
		{"postgres", postgres.Open("postgres://postgres@127.0.0.1:15427/pricing_review?sslmode=disable&connect_timeout=5"), common.DatabaseTypePostgreSQL},
	} {
		t.Run(engine.name, func(t *testing.T) {
			db, err := gorm.Open(engine.dialect, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			conn, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, conn.Close()) })
			if db.Migrator().HasTable(&User{}) {
				var n int64
				require.NoError(t, db.Model(&User{}).Count(&n).Error)
				require.Zero(t, n, "isolated database in use")
			}
			oldDB, oldKind := DB, common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(engine.kind)
			initCol()
			schema := []any{&User{}, &Token{}, &Channel{}, &AccountingBatchReceipt{}}
			t.Cleanup(func() {
				assert.NoError(t, db.Migrator().DropTable(schema...))
				DB = oldDB
				common.SetMainDatabaseType(oldKind)
				initCol()
			})
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &Channel{}))
			user := createReserveTestUser(t, 123)
			for range 2 {
				require.NoError(t, db.AutoMigrate(schema...))
			}
			assert.Equal(t, 123, getUserQuotaFromDB(t, user.Id))
			require.NoError(t, db.Delete(&user).Error)
			t.Run("rollback_and_response_loss", TestAccountingBatchRollbackAndUnknownCommit)
		})
	}
}
