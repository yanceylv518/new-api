package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 投递期限小于租约，失败后给迟到响应留出隔离窗口；批量上限避免轮询挤占正常请求。
const videoLogDeliveryTimeout = 5 * time.Second
const videoLogClaimSeconds = 30
const videoLogRetryBatch = 128
const retiredTaskAccountingPayload = "legacy-accounting-unverified"

const (
	videoLogKindAccounting = "accounting"
	videoLogKindDelivery   = "delivery"
)

// VideoTaskPendingLog 保存独立日志库尚未确认的任务初始及差额日志。
// 初始日志使用负任务 ID，差额日志使用正任务 ID，互不覆盖。
// 负任务 ID 的空 Payload 表示已确认资金的终态任务待初始记账。
// 与资金事务一起插入，成功后删除；不修改历史任务表或日志表结构。
type VideoTaskPendingLog struct {
	TaskID     int64  `gorm:"primaryKey;autoIncrement:false"`
	Kind       string `gorm:"size:16;not null;default:'';index:idx_video_log_kind_due,priority:1"`
	Payload    string `gorm:"type:text;not null"`
	ClaimID    string `gorm:"type:varchar(64)"`
	ClaimUntil int64  `gorm:"index;index:idx_video_log_kind_due,priority:2"`
}

func (pending *VideoTaskPendingLog) BeforeCreate(_ *gorm.DB) error {
	if pending.Payload == "" {
		pending.Kind = videoLogKindAccounting
	} else {
		pending.Kind = videoLogKindDelivery
	}
	return nil
}

// 分批归类小型队列并清理退役标记；完成后通过种类索引确认无待迁移行。
func migrateVideoTaskLogKinds(db *gorm.DB) error {
	for {
		var ids []int64
		if err := db.Model(&VideoTaskPendingLog{}).Where("kind IN ?", []string{"", "manual"}).Limit(videoLogRetryBatch).Pluck("task_id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		// 退役标记不是账务日志；只清除队列项，不修改任务、余额或历史消费。
		if err := db.Where("task_id IN ? AND task_id < 0 AND payload = ?", ids, retiredTaskAccountingPayload).Delete(&VideoTaskPendingLog{}).Error; err != nil {
			return err
		}
		if err := db.Model(&VideoTaskPendingLog{}).Where("task_id IN ? AND kind IN ?", ids, []string{"", "manual"}).UpdateColumn("kind", gorm.Expr(
			"CASE WHEN payload = ? THEN ? ELSE ? END",
			"", videoLogKindAccounting, videoLogKindDelivery,
		)).Error; err != nil {
			return err
		}
	}
}

func queueTaskInitialAccounting(tx *gorm.DB, taskID int64) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&VideoTaskPendingLog{TaskID: -taskID}).Error
}

// RetryTaskInitialAccounting 仅扫描持久化队列，终态任务不再依赖上游轮询触发记账。
func RetryTaskInitialAccounting(ctx context.Context, recoverAccounting func(context.Context, *Task) error) error {
	var ids []int64
	if err := DB.WithContext(ctx).Model(&VideoTaskPendingLog{}).
		Where("kind = ? AND claim_until <= ?", videoLogKindAccounting, common.GetTimestamp()).
		Order("claim_until").Order("task_id").Limit(videoLogRetryBatch).Pluck("task_id", &ids).Error; err != nil {
		return err
	}
	var firstErr error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := func() error {
			attempt, cancel := context.WithTimeout(ctx, videoLogDeliveryTimeout)
			defer cancel()
			claim := common.NewRequestId()
			now := common.GetTimestamp()
			result := DB.WithContext(attempt).Model(&VideoTaskPendingLog{}).
				Where("task_id = ? AND payload = ? AND claim_until <= ?", id, "", now).
				Updates(map[string]any{"claim_id": claim, "claim_until": now + videoLogClaimSeconds})
			if result.Error != nil || result.RowsAffected == 0 {
				return result.Error
			}
			var task Task
			err := DB.WithContext(attempt).Where("id = ?", -id).First(&task).Error
			if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && !task.PrivateData.InitialAccountingPending) {
				return DB.WithContext(attempt).Where("task_id = ? AND payload = ? AND claim_id = ?", id, "", claim).Delete(&VideoTaskPendingLog{}).Error
			}
			if err != nil {
				return err
			}
			return recoverAccounting(attempt, &task)
		}()
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("recover initial accounting for task %d: %w", -id, err)
		}
	}
	return firstErr
}

// DeliverVideoTaskLog 用短暂的条件更新领取投递，外部日志IO不持有主库行锁/连接。
// 租约长于5秒投递期限；失败保留租约以隔离延迟响应，下一轮到期后按稳定ID去重。
// ClickHouse只接收普通查询/插入，不参与主库事务。
func DeliverVideoTaskLog(ctx context.Context, taskID int64) error {
	ctx, cancel := context.WithTimeout(ctx, videoLogDeliveryTimeout)
	defer cancel()
	claim := common.NewRequestId()
	now := common.GetTimestamp()
	result := DB.WithContext(ctx).Model(&VideoTaskPendingLog{}).Where("task_id = ? AND kind = ? AND claim_until <= ?", taskID, videoLogKindDelivery, now).
		Updates(map[string]any{"claim_id": claim, "claim_until": now + videoLogClaimSeconds})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	var pending VideoTaskPendingLog
	if err := DB.WithContext(ctx).Where("task_id = ? AND claim_id = ?", taskID, claim).First(&pending).Error; err != nil {
		return err
	}
	if pending.TaskID < 0 && pending.Payload == retiredTaskAccountingPayload {
		// 清理由旧分类迁移误放入投递队列的退役标记。
		return DB.WithContext(ctx).Where("task_id = ? AND claim_id = ?", taskID, claim).Delete(&pending).Error
	}
	var log Log
	if err := common.UnmarshalJsonStr(pending.Payload, &log); err != nil {
		return fmt.Errorf("decode video billing log: %w", err)
	}
	logDB := LOG_DB.WithContext(ctx)
	var count int64
	if err := logDB.Model(&Log{}).Where("request_id = ? AND created_at = ? AND user_id = ?", log.RequestId, log.CreatedAt, log.UserId).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		if err := logDB.Create(&log).Error; err != nil {
			return err
		}
	}
	return DB.WithContext(ctx).Where("claim_id = ?", claim).Delete(&pending).Error
}

// RetryVideoTaskLogs 每轮只扫描小型待补写表的前128条；失败结束本轮，避免日志库
// 故障时放大请求。即使已无进行中任务，费用恢复任务仍会执行补写。
func RetryVideoTaskLogs(ctx context.Context) error {
	var ids []int64
	if err := DB.WithContext(ctx).Model(&VideoTaskPendingLog{}).Where("kind = ? AND claim_until <= ?", videoLogKindDelivery, common.GetTimestamp()).Order("claim_until").Order("task_id").Limit(videoLogRetryBatch).Pluck("task_id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		if err := DeliverVideoTaskLog(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// HasPendingVideoTaskLogs 让仅剩初始记账或日志补写的终态任务仍能触发恢复。
func HasPendingVideoTaskLogs() bool {
	var id int64
	err := DB.Model(&VideoTaskPendingLog{}).Where("kind IN ? AND claim_until <= ?", []string{videoLogKindAccounting, videoLogKindDelivery}, common.GetTimestamp()).Limit(1).Pluck("task_id", &id).Error
	return err == nil && id != 0
}
