package model

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"gorm.io/gorm"
)

// TaskCancelledReason 是任务插件管理接口取消上游任务时使用的稳定内部标记。
const TaskCancelledReason = "task cancelled by user"

// TaskPluginTaskFilter 描述插件任务列表的服务端过滤条件。
// 模型过滤基于持久化的 Properties JSON，保证 SQLite、MySQL、PostgreSQL 查询一致。
type TaskPluginTaskFilter struct {
	Platforms        []constant.TaskPlatform
	TaskIDs          []string
	Model            string
	Statuses         []TaskStatus
	Actions          []string
	CancelledOnly    bool
	ExcludeCancelled bool
	ExcludeExpired   bool
	ServiceTier      string
	CreatedAfter     int64
	CreatedBefore    int64
}

// ListTaskPluginUserTasks 只返回当前用户可见且属于插件历史平台标识的任务。
func ListTaskPluginUserTasks(userID int, filter TaskPluginTaskFilter, offset, limit int) ([]*Task, error) {
	if offset < 0 || limit <= 0 {
		return nil, fmt.Errorf("task list pagination is invalid")
	}
	var tasks []*Task
	if err := applyTaskPluginTaskFilter(DB.Where("user_id = ?", userID), filter).
		Order("id desc").Limit(limit).Offset(offset).Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// CountTaskPluginUserTasks 使用与分页列表相同的过滤条件统计总数，避免总数与页面不一致。
func CountTaskPluginUserTasks(userID int, filter TaskPluginTaskFilter) (int64, error) {
	var total int64
	if err := applyTaskPluginTaskFilter(DB.Model(&Task{}).Where("user_id = ?", userID), filter).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// applyTaskPluginTaskFilter 将插件列表谓词收敛在模型层，控制器无需直接执行方言 SQL。
func applyTaskPluginTaskFilter(query *gorm.DB, filter TaskPluginTaskFilter) *gorm.DB {
	if len(filter.Platforms) > 0 {
		query = query.Where("platform IN ?", filter.Platforms)
	}
	if len(filter.TaskIDs) > 0 {
		query = query.Where("task_id IN ?", filter.TaskIDs)
	}
	if filter.Model != "" {
		query = applyTaskPluginModelFilter(query, filter.Model)
	}
	// 官方列表的时间窗口直接复用现有索引；服务等级保存在原有 JSON 快照，无需新增表列。
	if filter.CreatedAfter > 0 {
		query = query.Where("created_at >= ?", filter.CreatedAfter)
	}
	if filter.CreatedBefore > 0 {
		query = query.Where("created_at < ?", filter.CreatedBefore)
	}
	if filter.ServiceTier != "" {
		query = query.Where("COALESCE("+taskPluginDataField(query, "service_tier")+", 'default') = ?", filter.ServiceTier)
	}
	if filter.ExcludeExpired {
		query = query.Where("COALESCE("+taskPluginDataField(query, "status")+", '') <> ?", "expired")
	}
	if len(filter.Statuses) > 0 {
		query = query.Where("status IN ?", filter.Statuses)
	}
	if len(filter.Actions) > 0 {
		query = query.Where("action IN ?", filter.Actions)
	}
	if filter.CancelledOnly {
		query = query.Where("status = ? AND fail_reason = ?", TaskStatusFailure, TaskCancelledReason)
	} else if filter.ExcludeCancelled {
		query = query.Where("fail_reason IS NULL OR fail_reason <> ?", TaskCancelledReason)
	}
	return query
}

// taskPluginDataField 读取固定业务字段的 JSON 标量，保持三个数据库对缺省/null 的处理一致。
// 字段名由内部调用点提供，不接受用户输入；其他任务列表谓词仍使用参数绑定。
func taskPluginDataField(query *gorm.DB, field string) string {
	column := query.Statement.Quote("data")
	switch query.Dialector.Name() {
	case "mysql":
		return "NULLIF(JSON_UNQUOTE(JSON_EXTRACT(" + column + ", '$." + field + "')), 'null')"
	case "postgres":
		return column + "->>'" + field + "'"
	case "sqlite":
		// 项目固定的 SQLite 驱动支持 JSON1，空快照和旧的非 JSON 数据按缺省字段处理。
		return "CASE WHEN json_valid(" + column + ") THEN json_extract(" + column + ", '$." + field + "') END"
	default:
		return "NULL"
	}
}

// applyTaskPluginModelFilter 按数据库方言读取 JSON 模型字段。
// 直接对 JSON 做 LIKE 在 MySQL 会受规范化空格影响，在 PostgreSQL 也不合法，
// 因此优先使用各方言的 JSON 提取操作，并保留未知方言的文本兜底。
func applyTaskPluginModelFilter(query *gorm.DB, modelName string) *gorm.DB {
	column := query.Statement.Quote("properties")
	switch query.Dialector.Name() {
	case "mysql":
		return query.Where(
			"(JSON_UNQUOTE(JSON_EXTRACT("+column+", '$.origin_model_name')) = ? OR JSON_UNQUOTE(JSON_EXTRACT("+column+", '$.upstream_model_name')) = ?)",
			modelName,
			modelName,
		)
	case "postgres":
		return query.Where(
			"("+column+"->>'origin_model_name' = ? OR "+column+"->>'upstream_model_name' = ?)",
			modelName,
			modelName,
		)
	case "sqlite":
		// SQLite 的 JSON1 扩展不是所有部署都编译启用，使用应用写入的紧凑 JSON 文本。
		condition := taskPluginPropertiesLikeCondition(query)
		return query.Where("("+condition+" OR "+condition+")", taskPluginModelPattern(modelName), taskPluginUpstreamModelPattern(modelName))
	default:
		condition := taskPluginPropertiesLikeCondition(query)
		return query.Where("("+condition+" OR "+condition+")", taskPluginModelPattern(modelName), taskPluginUpstreamModelPattern(modelName))
	}
}

// taskPluginPropertiesLikeCondition 为未知数据库方言提供可读的文本筛选表达式。
func taskPluginPropertiesLikeCondition(query *gorm.DB) string {
	column := query.Statement.Quote("properties")
	return "CAST(" + column + " AS TEXT) LIKE ? ESCAPE '!'"
}

// taskPluginModelPattern 转义 LIKE 元字符，并匹配稳定的 JSON 键值。
func taskPluginModelPattern(modelName string) string {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(modelName)
	return `%"origin_model_name":"` + escaped + `"%`
}

// taskPluginUpstreamModelPattern 返回模型映射后的 JSON 键值匹配模式。
func taskPluginUpstreamModelPattern(modelName string) string {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(modelName)
	return `%"upstream_model_name":"` + escaped + `"%`
}
