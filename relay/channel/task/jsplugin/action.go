package jsplugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/service"
)

// ExecuteTaskAction 调用插件声明的管理请求构造钩子，并限制操作为无文件的 JSON HTTP 请求。
// 上游响应体只在内存中保留一个有界副本，避免管理接口被大响应拖垮进程。
func (a *TaskAdaptor) ExecuteTaskAction(ctx context.Context, operation string, task *model.Task, baseURL, key, proxy string) (*channel.TaskActionResponse, error) {
	if a == nil || a.plugin == nil {
		return nil, fmt.Errorf("task plugin adaptor is unavailable")
	}
	if task == nil {
		return nil, fmt.Errorf("task is required")
	}
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return nil, fmt.Errorf("task operation is required")
	}
	if !a.hasHook(ctx, "buildTaskActionRequest") {
		return nil, fmt.Errorf("task plugin does not support operation %q", operation)
	}
	// 管理操作不应沿用视频生成可能无限等待的超时设置，取消请求必须有明确结束时间。
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	queryContext, err := a.queryContext(task, key, baseURL, proxy)
	if err != nil {
		return nil, err
	}
	queryContext["operation"] = operation
	queryContext["status"] = string(task.Status)
	value, err := a.plugin.Engine.Call(ctx, "buildTaskActionRequest", queryContext)
	if err != nil {
		return nil, err
	}
	var descriptor requestDescriptor
	if err = convert(value, &descriptor); err != nil {
		return nil, err
	}
	bodyType := strings.ToLower(strings.TrimSpace(descriptor.BodyType))
	if descriptor.Credentialless || (bodyType != "" && bodyType != "json") || len(descriptor.Parts) > 0 {
		return nil, fmt.Errorf("task action request must be an authenticated JSON request")
	}
	if strings.TrimSpace(descriptor.URL) == "" {
		return nil, fmt.Errorf("task action request URL is empty")
	}
	if err = pluginruntime.ValidateRequestURL(descriptor.URL, baseURL, a.plugin.Meta.AllowedHosts); err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(descriptor.Method))
	if method == "" {
		method = http.MethodDelete
	}
	if operation == "delete" && method != http.MethodDelete {
		return nil, fmt.Errorf("delete task action must use DELETE")
	}

	var body io.Reader
	if descriptor.Body != nil {
		if bodyText, ok := descriptor.Body.(string); ok {
			body = strings.NewReader(bodyText)
		} else {
			encoded, marshalErr := common.Marshal(descriptor.Body)
			if marshalErr != nil {
				return nil, fmt.Errorf("task action request body is invalid")
			}
			body = bytes.NewReader(encoded)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, descriptor.URL, body)
	if err != nil {
		return nil, err
	}
	for name, headerValue := range descriptor.Headers {
		request.Header.Set(name, headerValue)
	}
	client, err := service.GetHttpClientWithProxy(proxy)
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("task action HTTP client is unavailable")
	}
	// 管理接口不接受跨主机重定向，避免初始 URL 校验后请求被转发到其他地址。
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	started := time.Now()
	response, err := clientCopy.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxTaskPluginPersistedJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if len(responseBody) > maxTaskPluginPersistedJSONBytes {
		return nil, fmt.Errorf("task action response exceeds size limit")
	}
	logger.LogDebug(
		ctx,
		"task_plugin subsystem=adaptor event=action_response_received plugin=%q operation=%q status=%d elapsed_ms=%d",
		a.plugin.Meta.Key,
		operation,
		response.StatusCode,
		time.Since(started).Milliseconds(),
	)
	// 只保留关联请求所需的响应头，避免把 Cookie 等无关上游信息带入公共响应。
	requestID := response.Header.Get("X-Request-Id")
	if requestID == "" {
		requestID = response.Header.Get("Request-Id")
	}
	if requestID == "" {
		requestID = response.Header.Get("X-Tt-Logid")
	}
	result := &channel.TaskActionResponse{StatusCode: response.StatusCode, Body: responseBody, RequestID: requestID}
	// 厂商成功响应各不相同；可选解析器只归一化操作结果，不改变 HTTP 错误处理。
	if response.StatusCode >= 200 && response.StatusCode < 300 && a.hasHook(ctx, "parseTaskActionResponse") {
		var body any
		if err = common.Unmarshal(responseBody, &body); err != nil {
			return nil, fmt.Errorf("task action response is invalid JSON")
		}
		value, parseErr := a.plugin.Engine.Call(ctx, "parseTaskActionResponse", queryContext, map[string]any{"statusCode": response.StatusCode, "body": body})
		if parseErr != nil {
			return nil, fmt.Errorf("task action response parser failed")
		}
		var parsed struct {
			Action string `json:"action"`
		}
		if err = convert(value, &parsed); err != nil || (parsed.Action != "cancelled" && parsed.Action != "deleted" && parsed.Action != "unknown") {
			return nil, fmt.Errorf("task action response parser returned an invalid action")
		}
		result.Action = parsed.Action
	}
	return result, nil
}

var _ channel.TaskActionProvider = (*TaskAdaptor)(nil)
