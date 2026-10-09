package dto

type TaskPluginError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"httpStatus"`
	Retryable  bool   `json:"retryable"`
}

// TaskView 是唯一暴露给 JavaScript 插件的持久化任务形状。
// 它排除归属、渠道、额度和上游私有标识；Action 保留原生任务类型。
// 字段按 JSON 名排序，使有序 JSON 与原有 map 编码保持一致。
type TaskView struct {
	Action     string `json:"action"`
	CreatedAt  int64  `json:"created_at"`
	Data       any    `json:"data,omitempty"`
	FailReason string `json:"fail_reason"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Platform   string `json:"platform"`
	Progress   string `json:"progress"`
	Status     string `json:"status"`
	TaskID     string `json:"task_id"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
}
