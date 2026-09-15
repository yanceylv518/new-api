package dto

type TaskPluginError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"httpStatus"`
	Retryable  bool   `json:"retryable"`
}

// TaskView 是唯一暴露给 JavaScript 插件的持久化任务形状。
// 它排除归属、渠道、额度、Properties 和上游私有标识，但保留任务动作以便
// 原生查询在任务尚未完成时仍能还原正确的任务类型。
type TaskView struct {
	TaskID     string `json:"task_id"`
	Platform   string `json:"platform"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Progress   string `json:"progress"`
	FailReason string `json:"fail_reason"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
	Data       any    `json:"data,omitempty"`
}
