package model

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	commonRelay "github.com/QuantumNous/new-api/relay/common"
)

const (
	taskRequestSnapshotBase64Marker   = "[base64 content omitted]"
	taskRequestSnapshotRedactedMarker = "[redacted]"
	taskRequestSnapshotTooLargeMarker = "[omitted: too_large]"
	taskRequestSnapshotTruncatedKey   = "_snapshot_truncated"
	taskRequestSnapshotWriteTimeoutMs = 500
	taskRequestSnapshotMaxTimeoutMs   = 5000
)

// TaskRequestSnapshot 保存视频任务的一份脱敏请求快照，与 Task 分离以避免轮询反复读写请求体。
type TaskRequestSnapshot struct {
	ID            int64                 `json:"id" gorm:"primaryKey;autoIncrement"`
	TaskID        int64                 `json:"task_id" gorm:"uniqueIndex;not null"`
	PublicTaskID  string                `json:"public_task_id" gorm:"size:191;index;not null"`
	UserID        int                   `json:"user_id" gorm:"index;not null"`
	Platform      constant.TaskPlatform `json:"platform" gorm:"size:30;not null"`
	Model         string                `json:"model" gorm:"size:191;not null"`
	Body          LongText              `json:"-" gorm:"not null"`
	BodyBytes     int                   `json:"body_bytes" gorm:"not null"`
	Base64Omitted bool                  `json:"base64_omitted" gorm:"not null"`
	Truncated     bool                  `json:"truncated" gorm:"not null"`
	CreatedAt     int64                 `json:"created_at" gorm:"index;not null"`
}

type taskRequestSnapshotSanitizer struct {
	maxDepth      int
	maxItems      int
	maxBytes      int
	usedBytes     int
	base64Omitted bool
	truncated     bool
}

// NewTaskRequestSnapshot 将已解析的用户请求转换为有界且不含凭证的 JSON 快照，不修改原请求。
func NewTaskRequestSnapshot(task *Task, requestBody any) (*TaskRequestSnapshot, error) {
	if task == nil || !constant.TaskRequestSnapshotEnabled.Load() || requestBody == nil {
		return nil, nil
	}
	if !isVideoTaskAction(task.Action) {
		return nil, nil
	}
	maxBytes := constant.TaskRequestSnapshotMaxBytes
	if maxBytes <= 0 {
		return nil, errors.New("task request snapshot byte limit must be positive")
	}
	maxDepth := constant.TaskRequestSnapshotMaxDepth
	if maxDepth <= 0 {
		maxDepth = 16
	}
	maxItems := constant.TaskRequestSnapshotMaxItems
	if maxItems <= 0 {
		maxItems = 1000
	}

	normalized, err := normalizeTaskRequestSnapshotValue(requestBody)
	if err != nil {
		return nil, fmt.Errorf("normalize task request snapshot: %w", err)
	}
	sanitizer := &taskRequestSnapshotSanitizer{
		maxDepth: maxDepth,
		maxItems: maxItems,
		maxBytes: maxBytes,
	}
	sanitized := sanitizer.visit(normalized, "", 0)
	encoded, err := common.Marshal(sanitized)
	if err != nil {
		return nil, fmt.Errorf("marshal task request snapshot: %w", err)
	}
	if len(encoded) > maxBytes {
		sanitizer.truncated = true
		sanitized = map[string]any{
			taskRequestSnapshotTruncatedKey: true,
			"reason":                        taskRequestSnapshotTooLargeMarker,
		}
		encoded, err = common.Marshal(sanitized)
		if err != nil {
			return nil, fmt.Errorf("marshal truncated task request snapshot: %w", err)
		}
	}

	return &TaskRequestSnapshot{
		PublicTaskID:  task.TaskID,
		UserID:        task.UserId,
		Platform:      task.Platform,
		Model:         task.Properties.OriginModelName,
		Body:          LongText(encoded),
		BodyBytes:     len(encoded),
		Base64Omitted: sanitizer.base64Omitted,
		Truncated:     sanitizer.truncated,
		CreatedAt:     common.GetTimestamp(),
	}, nil
}

func isVideoTaskAction(action string) bool {
	switch constant.NormalizeTaskAction(action) {
	case constant.TaskActionImageToVideo,
		constant.TaskActionTextToVideo,
		constant.TaskActionFirstTailToVideo,
		constant.TaskActionReferenceToVideo,
		constant.TaskActionRemix:
		return true
	default:
		return strings.Contains(strings.ToLower(action), "video")
	}
}

func normalizeTaskRequestSnapshotValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil, string, bool, float64, int, int32, int64, uint, uint32, uint64:
		return value, nil
	case map[string]any, []any, []byte:
		return value, nil
	case commonRelay.TaskSubmitReq:
		return normalizeTaskSubmitRequestSnapshotValue(typed), nil
	case *commonRelay.TaskSubmitReq:
		if typed == nil {
			return nil, nil
		}
		return normalizeTaskSubmitRequestSnapshotValue(*typed), nil
	default:
		encoded, err := common.Marshal(value)
		if err != nil {
			return nil, err
		}
		var normalized any
		if err = common.Unmarshal(encoded, &normalized); err != nil {
			return nil, err
		}
		return normalized, nil
	}
}

func normalizeTaskSubmitRequestSnapshotValue(request commonRelay.TaskSubmitReq) map[string]any {
	value := map[string]any{"prompt": request.Prompt}
	if request.Model != "" {
		value["model"] = request.Model
	}
	if request.Mode != "" {
		value["mode"] = request.Mode
	}
	if request.Image != "" {
		value["image"] = request.Image
	}
	if len(request.Images) > 0 {
		images := make([]any, len(request.Images))
		for index, image := range request.Images {
			images[index] = image
		}
		value["images"] = images
	}
	if request.Size != "" {
		value["size"] = request.Size
	}
	if request.Duration != 0 {
		value["duration"] = request.Duration
	}
	if request.Seconds != "" {
		value["seconds"] = request.Seconds
	}
	if request.InputReference != "" {
		value["input_reference"] = request.InputReference
	}
	if request.Metadata != nil {
		value["metadata"] = request.Metadata
	}
	return value
}

func (s *taskRequestSnapshotSanitizer) visit(value any, key string, depth int) any {
	if depth >= s.maxDepth {
		return s.omittedValue()
	}
	if isTaskRequestSnapshotSensitiveKey(key) {
		return taskRequestSnapshotRedactedMarker
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, min(len(typed), s.maxItems))
		for itemKey := range typed {
			if len(keys) >= s.maxItems {
				break
			}
			keys = append(keys, itemKey)
		}
		if len(typed) > s.maxItems {
			s.truncated = true
		}
		sort.Strings(keys)
		result := make(map[string]any, min(len(keys), s.maxItems))
		for _, itemKey := range keys {
			if !s.reserve(len(itemKey) + 4) {
				break
			}
			result[itemKey] = s.visit(typed[itemKey], itemKey, depth+1)
		}
		return result
	case []any:
		result := make([]any, 0, min(len(typed), s.maxItems))
		if len(typed) > s.maxItems {
			s.truncated = true
		}
		for index := range min(len(typed), s.maxItems) {
			item := typed[index]
			result = append(result, s.visit(item, key, depth+1))
		}
		return result
	case string:
		if isTaskRequestSnapshotBase64Field(key) ||
			isTaskRequestSnapshotDataURI(typed) ||
			isTaskRequestSnapshotRawBase64Field(key, typed) {
			s.base64Omitted = true
			return taskRequestSnapshotBase64Marker
		}
		if !s.reserve(len(typed) + 2) {
			return s.omittedValue()
		}
		return sanitizeTaskRequestSnapshotURL(typed)
	case []byte:
		s.base64Omitted = true
		return taskRequestSnapshotBase64Marker
	default:
		return typed
	}
}

func (s *taskRequestSnapshotSanitizer) reserve(size int) bool {
	if size <= 0 {
		return true
	}
	if s.usedBytes > s.maxBytes || size > s.maxBytes-s.usedBytes {
		s.truncated = true
		return false
	}
	s.usedBytes += size
	return true
}

func (s *taskRequestSnapshotSanitizer) omittedValue() string {
	s.truncated = true
	return taskRequestSnapshotTooLargeMarker
}

func isTaskRequestSnapshotSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, ".", "_")
	compact := strings.ReplaceAll(normalized, "_", "")
	switch normalized {
	case "api_key", "apikey", "access_token", "auth_token", "authorization",
		"cookie", "password", "client_secret", "secret", "token":
		return true
	default:
		return strings.Contains(compact, "apikey") ||
			strings.Contains(compact, "password") ||
			strings.Contains(compact, "authorization") ||
			strings.Contains(compact, "cookie") ||
			strings.Contains(compact, "secret") ||
			strings.Contains(compact, "credential") ||
			strings.HasSuffix(normalized, "_token") ||
			strings.HasSuffix(normalized, "_secret") ||
			strings.HasSuffix(normalized, "_api_key") ||
			strings.HasSuffix(compact, "token") ||
			strings.HasSuffix(compact, "secret")
	}
}

func isTaskRequestSnapshotBase64Field(key string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(key)), "base64")
}

func isTaskRequestSnapshotDataURI(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(lower, "data:") {
		return false
	}
	comma := strings.IndexByte(lower, ',')
	return comma > 5 && strings.Contains(lower[:comma], ";base64")
}

func isTaskRequestSnapshotRawBase64Field(key, value string) bool {
	normalizedKey := strings.ToLower(strings.TrimSpace(key))
	normalizedKey = strings.ReplaceAll(normalizedKey, "-", "_")
	normalizedKey = strings.ReplaceAll(normalizedKey, ".", "_")
	if normalizedKey != "data" &&
		!strings.Contains(normalizedKey, "image") &&
		!strings.Contains(normalizedKey, "video") &&
		!strings.Contains(normalizedKey, "audio") &&
		!strings.Contains(normalizedKey, "media") &&
		!strings.Contains(normalizedKey, "reference") {
		return false
	}
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < 8 {
		return false
	}
	paddingStarted := false
	paddingCount := 0
	for _, character := range trimmed {
		if character == '=' {
			paddingStarted = true
			paddingCount++
			if paddingCount > 2 {
				return false
			}
			continue
		}
		if paddingStarted {
			return false
		}
		if (character < 'A' || character > 'Z') &&
			(character < 'a' || character > 'z') &&
			(character < '0' || character > '9') &&
			character != '+' && character != '/' {
			return false
		}
	}
	return true
}

func sanitizeTaskRequestSnapshotURL(value string) string {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(trimmed), "http://") &&
		!strings.HasPrefix(strings.ToLower(trimmed), "https://") {
		return value
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return value
	}
	changed := false
	if parsed.User != nil {
		parsed.User = nil
		changed = true
	}
	if parsed.RawQuery != "" {
		query := parsed.Query()
		for key, values := range query {
			if !isTaskRequestSnapshotSensitiveQueryKey(key) {
				continue
			}
			query[key] = []string{taskRequestSnapshotRedactedMarker}
			if len(values) > 0 {
				changed = true
			}
		}
		if changed {
			parsed.RawQuery = query.Encode()
		}
	}
	if !changed {
		return value
	}
	return parsed.String()
}

func isTaskRequestSnapshotSensitiveQueryKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, ".", "_")
	compact := strings.ReplaceAll(normalized, "_", "")
	return strings.Contains(normalized, "signature") ||
		strings.Contains(normalized, "credential") ||
		strings.Contains(normalized, "authorization") ||
		strings.Contains(compact, "accesskey") ||
		strings.Contains(compact, "apikey") ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "token")
}

// GetTaskRequestSnapshot 只在调用方完成任务归属校验后读取快照，保持任务列表 SQL 不变。
func GetTaskRequestSnapshot(ctx context.Context, taskID int64) (*TaskRequestSnapshot, error) {
	var snapshot TaskRequestSnapshot
	err := DB.WithContext(ctx).Where("task_id = ?", taskID).First(&snapshot).Error
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

// SaveTaskRequestSnapshot 在任务主表持久化后写入快照。快照属于尽力而为的诊断数据，
// 诊断表故障不能让已经被上游受理的视频任务失败。
func SaveTaskRequestSnapshot(ctx context.Context, taskID int64, snapshot *TaskRequestSnapshot) error {
	if snapshot == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stored := *snapshot
	stored.TaskID = taskID
	timeoutMilliseconds := constant.TaskRequestSnapshotWriteTimeoutMilliseconds
	if timeoutMilliseconds <= 0 || timeoutMilliseconds > taskRequestSnapshotMaxTimeoutMs {
		timeoutMilliseconds = taskRequestSnapshotWriteTimeoutMs
	}
	writeContext, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		time.Duration(timeoutMilliseconds)*time.Millisecond,
	)
	defer cancel()
	return DB.WithContext(writeContext).Create(&stored).Error
}
