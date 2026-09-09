package model

import (
	"sort"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AccountingBatchReceipt 只保存批次提交身份，不保存逐请求日志；正常确认后删除。
// 不能用 TTL 清理仍可能重试的标识，否则提交应答丢失后会重复应用资金增量。
type AccountingBatchReceipt struct {
	ID      string `gorm:"primaryKey;size:64"`
	Attempt string `gorm:"size:64;not null"`
}

var accountingBatchMu sync.Mutex
var pendingAccountingBatch *accountingBatch

// accountingBatch 在失败或提交结果未知时保留同一批次和增量；新记录继续进入下一批。
type accountingBatch struct {
	id        string
	stores    []map[int]int
	committed bool
}

// commitAccountingBatch 原子提交用户资金、令牌资金和统计；重试已提交批次不重复扣费。
func commitAccountingBatch(batch *accountingBatch) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		attempt := common.GetRandomString(32)
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&AccountingBatchReceipt{ID: batch.id, Attempt: attempt}).Error; err != nil {
			return err
		}
		// MySQL clientFoundRows 会影响冲突空更新的 RowsAffected，用实际插入身份区分首次提交。
		var receipt AccountingBatchReceipt
		if err := tx.First(&receipt, "id = ?", batch.id).Error; err != nil {
			return err
		}
		if receipt.Attempt != attempt {
			return nil
		}
		// 顺序与任务结算一致：先用户、后令牌、最后渠道；同类行按 ID 排序避免相反锁序。
		userIDs := make(map[int]bool)
		for _, typ := range []int{BatchUpdateTypeUserQuota, BatchUpdateTypeUsedQuota, BatchUpdateTypeRequestCount} {
			for id := range batch.stores[typ] {
				userIDs[id] = true
			}
		}
		ids := make([]int, 0, len(userIDs))
		for id := range userIDs {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			values := map[string]any{}
			for typ, col := range map[int]string{BatchUpdateTypeUserQuota: "quota", BatchUpdateTypeUsedQuota: "used_quota", BatchUpdateTypeRequestCount: "request_count"} {
				if delta := batch.stores[typ][id]; delta != 0 {
					values[col] = gorm.Expr(col+" + ?", delta)
				}
			}
			if len(values) > 0 {
				if err := tx.Model(&User{}).Where("id = ?", id).Updates(values).Error; err != nil {
					return err
				}
			}
		}
		for _, typ := range []int{BatchUpdateTypeTokenQuota, BatchUpdateTypeChannelUsedQuota} {
			ids = ids[:0]
			for id := range batch.stores[typ] {
				ids = append(ids, id)
			}
			sort.Ints(ids)
			for _, id := range ids {
				delta := batch.stores[typ][id]
				var result *gorm.DB
				if typ == BatchUpdateTypeTokenQuota {
					// 全退使资金净增量为零时，也要保留原批量实现更新最近访问时间的语义。
					result = tx.Model(&Token{}).Where("id = ?", id).Updates(map[string]any{"remain_quota": gorm.Expr("remain_quota + ?", delta), "used_quota": gorm.Expr("used_quota - ?", delta), "accessed_time": common.GetTimestamp()})
				} else {
					if delta == 0 {
						continue
					}
					result = tx.Model(&Channel{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", delta))
				}
				if result.Error != nil {
					return result.Error
				}
			}
		}
		return nil
	})
}

// flushAccountingBatch 串行拥有批次；失败时不丢增量，确认提交后只重试清理标识。
func flushAccountingBatch() {
	accountingBatchMu.Lock()
	defer accountingBatchMu.Unlock()
	if pendingAccountingBatch == nil {
		stores := make([]map[int]int, BatchUpdateTypeCount)
		hasData := false
		for i := 0; i < BatchUpdateTypeCount; i++ {
			batchUpdateLocks[i].Lock()
			stores[i] = batchUpdateStores[i]
			batchUpdateStores[i] = make(map[int]int)
			hasData = hasData || len(stores[i]) > 0
			batchUpdateLocks[i].Unlock()
		}
		if !hasData {
			return
		}
		pendingAccountingBatch = &accountingBatch{id: common.GetRandomString(32), stores: stores}
	}
	if !pendingAccountingBatch.committed {
		if err := commitAccountingBatch(pendingAccountingBatch); err != nil {
			common.SysError("accounting batch retained for retry: " + err.Error())
			return
		}
		pendingAccountingBatch.committed = true
	}
	// DELETE 已成功但应答丢失时，下次只能重试删除，绝不能因标识已消失而再次计费。
	if err := DB.Delete(&AccountingBatchReceipt{}, "id = ?", pendingAccountingBatch.id).Error; err != nil {
		common.SysError("accounting batch cleanup retained for retry: " + err.Error())
		return
	}
	pendingAccountingBatch = nil
}
