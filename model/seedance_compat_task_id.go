package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const seedanceCompatSuffixSpace int64 = 36 * 36 * 36 * 36 * 36

type SeedanceCompatTaskIDSequence struct {
	ID         int `gorm:"primaryKey;autoIncrement:false"`
	LastSecond int64
	Start      int64
	Allocated  int64
}

// GenerateSeedanceCompatTaskID 在数据库中分配五位后缀，避免多节点并发重复。
func GenerateSeedanceCompatTaskID(ctx context.Context) (string, error) {
	var taskID string
	var allocationErr error
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		sequence := SeedanceCompatTaskIDSequence{ID: 1}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&sequence).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).First(&sequence, 1).Error; err != nil {
			return err
		}
		if now := time.Now().UTC().Unix(); now > sequence.LastSecond {
			start, err := common.GenerateRandomCharsKey(5)
			if err != nil {
				return err
			}
			sequence.LastSecond = now
			sequence.Start, err = strconv.ParseInt(strings.ToLower(start), 36, 64)
			if err != nil {
				return err
			}
			sequence.Allocated = 0
		}
		// 历史任务可能已使用相同格式的上游 ID，分配时跳过已有记录。
		for range 64 {
			if sequence.Allocated >= seedanceCompatSuffixSpace {
				allocationErr = fmt.Errorf("seedance task ID suffix space exhausted")
				break
			}
			value := (sequence.Start + sequence.Allocated) % seedanceCompatSuffixSpace
			sequence.Allocated++
			suffix := strconv.FormatInt(value, 36)
			suffix = strings.Repeat("0", 5-len(suffix)) + suffix
			candidate := "cgt-" + time.Unix(sequence.LastSecond, 0).UTC().Format("20060102150405") + "-" + suffix
			var existing Task
			result := tx.Select("id").Where("task_id = ?", candidate).Limit(1).Find(&existing)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				taskID = candidate
				break
			}
		}
		// 保存已跳过的后缀，下次请求才能继续检查后续编号。
		if taskID == "" && allocationErr == nil {
			allocationErr = fmt.Errorf("could not allocate seedance task ID")
		}
		return tx.Model(&sequence).Updates(map[string]any{
			"last_second": sequence.LastSecond,
			"start":       sequence.Start,
			"allocated":   sequence.Allocated,
		}).Error
	})
	if err != nil {
		return "", err
	}
	return taskID, allocationErr
}
