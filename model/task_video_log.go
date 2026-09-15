package model

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// 投递期限小于租约，失败后给迟到响应留出隔离窗口；批量上限避免轮询挤占正常请求。
const videoLogDeliveryTimeout = 5 * time.Second
const videoLogClaimSeconds = 30
const videoLogRetryBatch = 128

// VideoTaskPendingLog 仅保存独立日志库尚未确认的视频差额日志。
// 与资金事务一起插入，成功后删除；不修改历史任务表或日志表结构。
type VideoTaskPendingLog struct {
	TaskID     int64  `gorm:"primaryKey;autoIncrement:false"`
	Payload    string `gorm:"type:text;not null"`
	ClaimID    string `gorm:"type:varchar(64)"`
	ClaimUntil int64  `gorm:"index"`
}

// DeliverVideoTaskLog 用短暂的条件更新领取投递，外部日志IO不持有主库行锁/连接。
// 租约长于5秒投递期限；失败保留租约以隔离延迟响应，下一轮到期后按稳定ID去重。
// ClickHouse只接收普通查询/插入，不参与主库事务。
func DeliverVideoTaskLog(ctx context.Context, taskID int64) error {
	ctx, cancel := context.WithTimeout(ctx, videoLogDeliveryTimeout)
	defer cancel()
	claim := common.NewRequestId()
	now := common.GetTimestamp()
	result := DB.WithContext(ctx).Model(&VideoTaskPendingLog{}).Where("task_id = ? AND claim_until <= ?", taskID, now).
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
// 故障时放大请求。即使已无进行中任务，轮询入口仍会执行补写。
func RetryVideoTaskLogs(ctx context.Context) error {
	var ids []int64
	if err := DB.WithContext(ctx).Model(&VideoTaskPendingLog{}).Where("claim_until <= ?", common.GetTimestamp()).Order("task_id").Limit(videoLogRetryBatch).Pluck("task_id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		if err := DeliverVideoTaskLog(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// HasPendingVideoTaskLogs 让仅剩失败日志的终态任务仍能触发补写轮询。
func HasPendingVideoTaskLogs() bool {
	var id int64
	err := DB.Model(&VideoTaskPendingLog{}).Limit(1).Pluck("task_id", &id).Error
	return err == nil && id != 0
}
