package channel

import (
	"context"

	"github.com/QuantumNous/new-api/model"
)

// TaskActionResponse 保留管理操作的 HTTP 状态、有限响应体和请求标识，用于错误诊断。
type TaskActionResponse struct {
	StatusCode int
	Body       []byte
	RequestID  string
	// Action 由可选的插件响应解析器归一化；unknown 要求宿主再次确认后才能退款。
	Action string
}

// TaskActionProvider 为已有任务提供取消或删除等管理操作。
// 宿主负责用户归属、插件平台、渠道和响应大小校验，插件只负责构造上游请求。
type TaskActionProvider interface {
	ExecuteTaskAction(ctx context.Context, operation string, task *model.Task, baseURL, key, proxy string) (*TaskActionResponse, error)
}
