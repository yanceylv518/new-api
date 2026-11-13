package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrTaskResourceIndexPending = errors.New("resource index is being prepared; retry later or provide origin_task_id")
var ErrTaskResourceIndexUnavailable = errors.New("plugin must declare the resource paths or the request must provide origin_task_id")

const taskResourceBackfillBatchSize = 32

// TaskResource 仅索引插件声明的资源字段，不复制响应、凭据或任意文本。
type TaskResource struct {
	TaskID      int64                 `gorm:"primaryKey;autoIncrement:false"`
	ResourceKey string                `gorm:"size:64;primaryKey;index:idx_task_resource_owner,priority:3"`
	UserID      int                   `gorm:"not null;index:idx_task_resource_owner,priority:1"`
	Platform    constant.TaskPlatform `gorm:"size:30;not null;index:idx_task_resource_owner,priority:2"`
	Action      string                `gorm:"size:40;not null;index:idx_task_resource_owner,priority:4"`
}

type TaskResourceIndexState struct {
	ID        string `gorm:"size:64;primaryKey"`
	AfterID   int64
	UpperID   int64
	Completed bool
}

type taskResourceIndexDefinition struct {
	ID        string
	Paths     []string
	Platforms []constant.TaskPlatform
}

func taskResourceIndexForMeta(meta jsplugin.Meta) taskResourceIndexDefinition {
	paths := slices.Clone(meta.ResourcePaths)
	slices.Sort(paths)
	platforms := []constant.TaskPlatform{constant.TaskPlatform(meta.Key)}
	for _, channelType := range meta.ChannelTypes {
		platforms = append(platforms, constant.TaskPlatform(strconv.Itoa(channelType)))
	}
	slices.Sort(platforms)
	encoded, _ := common.Marshal([]any{paths, platforms})
	return taskResourceIndexDefinition{ID: fmt.Sprintf("%x", sha256.Sum256(encoded)), Paths: paths, Platforms: platforms}
}

func taskResourceKey(path, id string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(path+"\x00"+id)))
}

// AfterSave 与终态写入共用事务，索引失败不能留下无法检索的成功任务。
func (task *Task) AfterSave(tx *gorm.DB) error {
	return saveTaskResourceIndex(tx, task)
}

func saveTaskResourceIndex(tx *gorm.DB, task *Task) error {
	if task.ID <= 0 || task.Status != TaskStatusSuccess {
		return nil
	}
	var paths []string
	if execution := task.PrivateData.Execution; execution != nil {
		paths = slices.Clone(execution.ResourcePaths)
	}
	generation := jsplugin.DefaultRegistry.Generation()
	plugin, exists := generation.Get(string(task.Platform))
	if !exists {
		if channelType, err := strconv.Atoi(string(task.Platform)); err == nil {
			plugin, exists = generation.GetByChannelType(channelType)
		}
	}
	// 同时保留冻结路径和当前路径，覆盖补建已越过的在途任务及插件停用场景。
	if exists {
		for _, path := range plugin.Meta.ResourcePaths {
			if !slices.Contains(paths, path) {
				paths = append(paths, path)
			}
		}
	}
	if len(paths) == 0 {
		return nil
	}
	return indexTaskResources(tx, task, taskResourceIndexDefinition{Paths: paths})
}

func indexTaskResources(tx *gorm.DB, task *Task, definition taskResourceIndexDefinition) error {
	if !task.ResultRetrievable() {
		return tx.Where("task_id = ?", task.ID).Delete(&TaskResource{}).Error
	}
	rows := make([]TaskResource, 0)
	seen := make(map[string]bool)
	for _, path := range definition.Paths {
		value := gjson.GetBytes(task.Data, path)
		values := []gjson.Result{value}
		if value.IsArray() {
			values = value.Array()
		}
		for _, item := range values {
			if item.Type != gjson.String || strings.TrimSpace(item.Str) == "" || len(item.Str) > 191 {
				continue
			}
			key := taskResourceKey(path, item.Str)
			if seen[key] {
				continue
			}
			seen[key] = true
			rows = append(rows, TaskResource{TaskID: task.ID, ResourceKey: key, UserID: task.UserId, Platform: task.Platform, Action: task.Action})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(&rows, 100).Error
}

// HasPendingTaskResourceIndexes 仅检查每个当前插件的固定进度行，不扫描任务历史。
func HasPendingTaskResourceIndexes() bool {
	for _, plugin := range jsplugin.DefaultRegistry.Generation().Plugins() {
		if len(plugin.Meta.ResourcePaths) == 0 {
			continue
		}
		definition := taskResourceIndexForMeta(plugin.Meta)
		var state TaskResourceIndexState
		err := DB.First(&state, "id = ?", definition.ID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) || err == nil && !state.Completed {
			return true
		}
	}
	return false
}

// BackfillTaskResourceIndexes 在后台按固定上界和检查点补建，短事务每次最多加载32条任务。
func BackfillTaskResourceIndexes(ctx context.Context) error {
	for _, plugin := range jsplugin.DefaultRegistry.Generation().Plugins() {
		if len(plugin.Meta.ResourcePaths) == 0 {
			continue
		}
		definition := taskResourceIndexForMeta(plugin.Meta)
		var state TaskResourceIndexState
		err := DB.WithContext(ctx).First(&state, "id = ?", definition.ID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			var upper int64
			if err := DB.WithContext(ctx).Model(&Task{}).Order("id DESC").Limit(1).Pluck("id", &upper).Error; err != nil {
				return err
			}
			state = TaskResourceIndexState{ID: definition.ID, UpperID: upper}
			if err := DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&state).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		for !state.Completed {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if tx.Dialector.Name() == "sqlite" {
					if err := tx.Model(&TaskResourceIndexState{}).Where("id = ?", definition.ID).UpdateColumn("after_id", gorm.Expr("after_id")).Error; err != nil {
						return err
					}
				}
				if err := lockForUpdate(tx).First(&state, "id = ?", definition.ID).Error; err != nil {
					return err
				}
				if state.Completed {
					return nil
				}
				var ids []int64
				if err := tx.Model(&Task{}).Where("id > ? AND id <= ?", state.AfterID, state.UpperID).Order("id").Limit(taskResourceBackfillBatchSize).Pluck("id", &ids).Error; err != nil {
					return err
				}
				if len(ids) == 0 {
					state.AfterID, state.Completed = state.UpperID, true
				} else {
					var tasks []Task
					if err := tx.Where("id IN ? AND platform IN ? AND status = ?", ids, definition.Platforms, TaskStatusSuccess).Find(&tasks).Error; err != nil {
						return err
					}
					for i := range tasks {
						if err := indexTaskResources(tx, &tasks[i], definition); err != nil {
							return err
						}
					}
					state.AfterID = ids[len(ids)-1]
					state.Completed = state.AfterID >= state.UpperID
				}
				return tx.Model(&state).Updates(map[string]any{"after_id": state.AfterID, "completed": state.Completed}).Error
			}); err != nil {
				return err
			}
		}
	}
	return nil
}
