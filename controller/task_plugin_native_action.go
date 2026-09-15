package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay/channel"
	taskjsplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	maxTaskPluginNativeListPage = 100000
	maxTaskPluginNativePageSize = 100
)

// RelayTaskPluginNativeAction 处理动态任务插件管理路由。
// 中间件已经完成意图校验，这里只负责用户隔离、任务生命周期和结果渲染。
func RelayTaskPluginNativeAction(c *gin.Context) {
	pinned, ok := nativeRoutePin(c)
	if !ok {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_plugin_route_failed", "Task plugin route is unavailable")
		return
	}
	requestValue, exists := c.Get(pluginruntime.ContextKeyRouteRequest)
	requestContext, requestOK := requestValue.(pluginruntime.RouteRequestContext)
	if !exists || !requestOK {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_plugin_route_failed", "Task plugin request context is unavailable")
		return
	}
	intentValue, exists := c.Get(pluginruntime.ContextKeyNativeRouteIntent)
	intent, intentOK := intentValue.(map[string]any)
	if !exists || !intentOK {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_plugin_route_failed", "Task plugin management intent is unavailable")
		return
	}

	switch pinned.Route.Action {
	case "list":
		renderTaskPluginNativeList(c, pinned, requestContext, intent)
	case "delete":
		executeTaskPluginNativeDelete(c, pinned, requestContext, intent)
	default:
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_plugin_route_failed", "Unsupported task plugin management operation")
	}
}

func nativeRoutePin(c *gin.Context) (pluginruntime.PinnedRoute, bool) {
	value, exists := c.Get(pluginruntime.ContextKeyPinnedRoute)
	if !exists {
		return pluginruntime.PinnedRoute{}, false
	}
	pinned, ok := value.(pluginruntime.PinnedRoute)
	return pinned, ok && pinned.Plugin != nil && pinned.Generation != nil
}

// renderTaskPluginNativeList 构建有界的本地任务视图。
// 渠道可能复用同一个上游账号，因此列表只能返回当前用户在网关创建的任务。
func renderTaskPluginNativeList(
	c *gin.Context,
	pinned pluginruntime.PinnedRoute,
	requestContext pluginruntime.RouteRequestContext,
	intent map[string]any,
) {
	filter, err := taskPluginNativeListFilter(pinned.Plugin.Meta, requestContext.Query, intent)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	page, pageSize := 0, 0
	// 原生列表可声明自己的官方分页和时间窗口，宿主再次验证边界；旧插件保留原来的默认限制。
	if raw, present := intent["listOptions"]; present {
		var options struct {
			PageNum         int    `json:"pageNum"`
			PageSize        int    `json:"pageSize"`
			ServiceTier     string `json:"serviceTier"`
			LookbackSeconds int64  `json:"lookbackSeconds"`
		}
		encoded, encodeErr := common.Marshal(raw)
		if encodeErr != nil || common.Unmarshal(encoded, &options) != nil || options.PageNum < 1 || options.PageNum > 500 || options.PageSize < 1 || options.PageSize > 500 || options.LookbackSeconds < 0 || options.LookbackSeconds > 604800 || (options.ServiceTier != "" && options.ServiceTier != "default" && options.ServiceTier != "flex") {
			respondTaskPluginNativeActionError(c, http.StatusBadRequest, "invalid_request", "Task list options are invalid")
			return
		}
		page, pageSize = options.PageNum, options.PageSize
		filter.ServiceTier = options.ServiceTier
		if options.LookbackSeconds > 0 {
			filter.CreatedBefore = time.Now().Unix()
			filter.CreatedAfter = filter.CreatedBefore - options.LookbackSeconds
		}
	} else {
		page, pageSize, err = parseTaskPluginNativePagination(requestContext.Query)
		if err != nil {
			respondTaskPluginNativeActionError(c, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	total, err := model.CountTaskPluginUserTasks(userID, filter)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_list_failed", "Failed to query task list")
		return
	}
	tasks, err := model.ListTaskPluginUserTasks(userID, filter, (page-1)*pageSize, pageSize)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_list_failed", "Failed to query task list")
		return
	}
	views := make([]any, 0, len(tasks))
	for _, task := range tasks {
		view, viewErr := service.BuildTaskPluginView(task)
		if viewErr != nil {
			respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_list_failed", "Failed to build task list")
			return
		}
		viewValue, valueErr := taskPluginProtocolJSONValue(view)
		if valueErr != nil {
			respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_list_failed", "Failed to build task list")
			return
		}
		views = append(views, viewValue)
	}
	result := map[string]any{
		"items":     views,
		"total":     total,
		"page_num":  page,
		"page_size": pageSize,
	}
	renderTaskPluginNativeResult(c, pinned, requestContext, result)
}

func parseTaskPluginNativePagination(query map[string][]string) (int, int, error) {
	page, err := parseTaskPluginNativePositiveQuery(query, "page_num", 1, maxTaskPluginNativeListPage)
	if err != nil {
		return 0, 0, err
	}
	pageSize, err := parseTaskPluginNativePositiveQuery(query, "page_size", 20, maxTaskPluginNativePageSize)
	if err != nil {
		return 0, 0, err
	}
	return page, pageSize, nil
}

func parseTaskPluginNativePositiveQuery(query map[string][]string, key string, fallback, maximum int) (int, error) {
	values := query[key]
	if len(values) == 0 {
		return fallback, nil
	}
	if len(values) != 1 {
		return 0, fmt.Errorf("%s must be provided once", key)
	}
	valueText := strings.TrimSpace(values[0])
	if valueText == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(valueText)
	if err != nil || value < 1 || value > maximum {
		return 0, fmt.Errorf("%s must be between 1 and %d", key, maximum)
	}
	return value, nil
}

func taskPluginNativeListFilter(meta pluginruntime.Meta, query map[string][]string, intent map[string]any) (model.TaskPluginTaskFilter, error) {
	modelName, ok := intent["model"].(string)
	if (!ok && intent["model"] != nil) || len(modelName) > 191 {
		return model.TaskPluginTaskFilter{}, fmt.Errorf("model is not served by this plugin")
	}
	filter := model.TaskPluginTaskFilter{
		Platforms: taskPluginNativePlatforms(meta),
		Model:     modelName,
	}
	if rawIDs, present := intent["taskIds"]; present {
		ids, valid := taskPluginNativeTaskIDs(rawIDs)
		if !valid {
			return model.TaskPluginTaskFilter{}, fmt.Errorf("filter.task_ids is invalid")
		}
		filter.TaskIDs = ids
	}
	status, err := taskPluginNativeSingleQuery(query, "filter.status")
	if err != nil {
		return model.TaskPluginTaskFilter{}, err
	}
	switch status {
	case "":
	case "queued":
		filter.Statuses = []model.TaskStatus{model.TaskStatusNotStart, model.TaskStatusSubmitted, model.TaskStatusQueued}
	case "running":
		filter.Statuses = []model.TaskStatus{model.TaskStatusInProgress}
	case "succeeded":
		filter.Statuses = []model.TaskStatus{model.TaskStatusSuccess}
	case "failed":
		filter.Statuses = []model.TaskStatus{model.TaskStatusFailure}
		filter.ExcludeCancelled = true
		// 网关把过期也记为 FAILURE，但官方 failed 筛选不能混入 expired。
		filter.ExcludeExpired = true
	case "cancelled":
		filter.CancelledOnly = true
	default:
		return model.TaskPluginTaskFilter{}, fmt.Errorf("filter.status is invalid")
	}
	taskType, err := taskPluginNativeSingleQuery(query, "filter.task_type")
	if err != nil {
		return model.TaskPluginTaskFilter{}, err
	}
	switch taskType {
	case "":
	case "generation":
		filter.Actions = []string{"text_to_video", "image_to_video", "first_tail_to_video", "reference_to_video", "remix"}
	case "h3_context_ir":
		filter.Actions = []string{"context_ir"}
	case "regeneration":
		filter.Actions = []string{"regeneration"}
	default:
		return model.TaskPluginTaskFilter{}, fmt.Errorf("filter.task_type is invalid")
	}
	return filter, nil
}

func taskPluginNativeSingleQuery(query map[string][]string, key string) (string, error) {
	values := query[key]
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("%s must be provided once", key)
	}
	return strings.TrimSpace(values[0]), nil
}

func taskPluginNativeTaskIDs(value any) ([]string, bool) {
	var values []any
	switch typed := value.(type) {
	case []any:
		values = typed
	case []string:
		values = make([]any, len(typed))
		for index, id := range typed {
			values[index] = id
		}
	default:
		return nil, false
	}
	if len(values) > 100 {
		return nil, false
	}
	ids := make([]string, 0, len(values))
	for _, value := range values {
		id, stringOK := value.(string)
		id = strings.TrimSpace(id)
		if !stringOK || id == "" {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

func taskPluginNativePlatforms(meta pluginruntime.Meta) []constant.TaskPlatform {
	platforms := []constant.TaskPlatform{constant.TaskPlatform(meta.Key)}
	for _, channelType := range meta.ChannelTypes {
		if channelType <= 0 || channelType == constant.ChannelTypeTaskPlugin {
			continue
		}
		platform := constant.TaskPlatform(strconv.Itoa(channelType))
		if !slices.Contains(platforms, platform) {
			platforms = append(platforms, platform)
		}
	}
	return platforms
}

func executeTaskPluginNativeDelete(
	c *gin.Context,
	pinned pluginruntime.PinnedRoute,
	requestContext pluginruntime.RouteRequestContext,
	intent map[string]any,
) {
	taskID, ok := intent["taskId"].(string)
	if !ok || strings.TrimSpace(taskID) == "" {
		respondTaskPluginNativeActionError(c, http.StatusBadRequest, "invalid_request", "task_id is required")
		return
	}
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	task, exists, err := model.GetByTaskId(userID, strings.TrimSpace(taskID))
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_lookup_failed", "Failed to query task")
		return
	}
	if !exists || task == nil || !taskPluginNativeTaskOwned(pinned.Plugin.Meta, task) {
		respondTaskPluginNativeActionError(c, http.StatusNotFound, "task_not_found", "Task not found")
		return
	}
	models := pinned.Plugin.Meta.Models
	if len(pinned.Route.Models) > 0 {
		models = pinned.Route.Models
	}
	if !taskPluginNativeTaskMatchesModels(models, task) {
		respondTaskPluginNativeActionError(c, http.StatusNotFound, "task_not_found", "Task not found")
		return
	}
	channelModel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil || channelModel == nil {
		respondTaskPluginNativeActionError(c, http.StatusBadRequest, "task_channel_unavailable", "Task channel is unavailable")
		return
	}
	if channelModel.Status != common.ChannelStatusEnabled {
		respondTaskPluginNativeActionError(c, http.StatusBadRequest, "task_channel_disabled", "Task channel is disabled")
		return
	}
	if !taskPluginNativeChannelOwned(pinned.Plugin.Meta, channelModel) {
		respondTaskPluginNativeActionError(c, http.StatusBadRequest, "task_channel_unavailable", "Task channel is unavailable")
		return
	}
	adaptor := taskjsplugin.New(pinned.Plugin)
	// 删除可能移除上游最终用量，必须先通过既有结算链同步本地状态。
	actionContext, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	task, err = service.RefreshTaskForManagement(actionContext, adaptor, channelModel, task)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_sync_failed", "Failed to synchronize task before cancellation or deletion")
		return
	}
	baseURL := channelModel.GetBaseURL()
	key := channelModel.Key
	if task.PrivateData.Key != "" {
		key = task.PrivateData.Key
	}
	responseProvider, ok := any(adaptor).(channel.TaskActionProvider)
	if !ok {
		respondTaskPluginNativeActionError(c, http.StatusNotImplemented, "task_action_not_supported", "Task action is not supported")
		return
	}
	response, err := responseProvider.ExecuteTaskAction(actionContext, "delete", task, baseURL, key, channelModel.GetSetting().Proxy)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_action_failed", "Task action failed")
		return
	}
	if response == nil {
		respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_action_failed", "Task action returned an empty response")
		return
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		respondTaskPluginUpstreamActionError(c, task, response, key)
		return
	}
	var payload struct {
		TaskID string `json:"task_id"`
		Action string `json:"action"`
		Status string `json:"status"`
	}
	if response.Action != "" {
		payload.TaskID = task.GetUpstreamTaskID()
		payload.Action, payload.Status = response.Action, response.Action
	} else if err = common.Unmarshal(response.Body, &payload); err != nil {
		respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_action_failed", "Task action response is invalid")
		return
	}
	if payload.Action == "unknown" {
		// 空响应无法区分排队取消和刚完成后删除，直接查询上游验证；不得走会把 404 当失败退款的轮询入口。
		confirmation, fetchErr := adaptor.FetchTaskWithContext(actionContext, baseURL, key, task, channelModel.GetSetting().Proxy)
		if fetchErr != nil {
			respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_cancel_pending", "Task operation succeeded; cancellation confirmation is pending")
			return
		}
		defer confirmation.Body.Close()
		const maxConfirmationBytes = 1024 * 1024
		confirmationBody, readErr := io.ReadAll(io.LimitReader(confirmation.Body, maxConfirmationBytes+1))
		if readErr != nil || len(confirmationBody) > maxConfirmationBytes {
			respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_cancel_pending", "Task cancellation confirmation is invalid")
			return
		}
		if confirmation.StatusCode == http.StatusNotFound || confirmation.StatusCode == http.StatusGone {
			payload.Action, payload.Status = "deleted", "deleted"
		} else if confirmation.StatusCode >= 200 && confirmation.StatusCode < 300 {
			result, parseErr := adaptor.ParseTaskResult(task, confirmation, confirmationBody)
			if parseErr != nil || result == nil || result.Status != string(model.TaskStatusFailure) || result.Reason != model.TaskCancelledReason {
				respondTaskPluginNativeActionError(c, http.StatusServiceUnavailable, "task_cancel_pending", "Task operation succeeded; cancellation has not been confirmed")
				return
			}
			payload.Action, payload.Status = "cancelled", "cancelled"
		} else {
			respondTaskPluginUpstreamActionError(c, task, &channel.TaskActionResponse{StatusCode: confirmation.StatusCode, Body: confirmationBody, RequestID: confirmation.Header.Get("X-Request-Id")}, key)
			return
		}
	}
	action := strings.ToLower(strings.TrimSpace(payload.Action))
	status := strings.ToLower(strings.TrimSpace(payload.Status))
	if strings.TrimSpace(payload.TaskID) != task.GetUpstreamTaskID() || action == "" || status == "" || action != status {
		respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_action_failed", "Task action response is invalid")
		return
	}
	if action != "cancelled" && action != "deleted" {
		respondTaskPluginNativeActionError(c, http.StatusBadGateway, "task_action_failed", "Task action response is invalid")
		return
	}
	if action == "deleted" && !retainTaskPluginUnsettledDeletion(c, task) {
		respondTaskPluginNativeActionError(c, http.StatusServiceUnavailable, "task_settlement_unavailable", "Upstream task was deleted before final usage was available; reserved quota was retained for reconciliation")
		return
	}
	if action == "cancelled" {
		if !markTaskPluginTaskCancelled(c, task) {
			respondTaskPluginNativeActionError(c, http.StatusServiceUnavailable, "task_cancel_pending", "Upstream task was cancelled; local settlement is pending")
			return
		}
	}
	view, err := service.BuildTaskPluginView(task)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_action_failed", "Failed to build task action result")
		return
	}
	viewValue, err := taskPluginProtocolJSONValue(view)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_action_failed", "Failed to build task action result")
		return
	}
	renderTaskPluginNativeResult(c, pinned, requestContext, map[string]any{
		"task":    viewValue,
		"task_id": task.TaskID,
		"action":  action,
		"status":  status,
	})
}

// respondTaskPluginUpstreamActionError 保留上游 JSON 错误的诊断字段。
// 原始响应、HTML、凭证和上游任务 ID 不得直接返回或记录；失败不会修改任务或触发退款。
func respondTaskPluginUpstreamActionError(c *gin.Context, task *model.Task, response *channel.TaskActionResponse, key string) {
	const maxErrorBodyBytes = 64 * 1024
	const maxErrorMessageRunes = 1024
	const maxErrorIdentifierRunes = 128
	status := response.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	code, message, requestID := "task_action_failed", "Task action failed", response.RequestID
	var payload struct {
		Error struct {
			Code      json.RawMessage `json:"code"`
			Type      string          `json:"type"`
			Message   string          `json:"message"`
			RequestID string          `json:"request_id"`
		} `json:"error"`
		BaseResp struct {
			StatusCode json.RawMessage `json:"status_code"`
			StatusMsg  string          `json:"status_msg"`
		} `json:"base_resp"`
		RequestID string `json:"request_id"`
	}
	if len(response.Body) <= maxErrorBodyBytes && common.Unmarshal(response.Body, &payload) == nil {
		var rawCode json.RawMessage
		if strings.TrimSpace(payload.Error.Message) != "" {
			message = payload.Error.Message
			rawCode = payload.Error.Code
			if strings.TrimSpace(payload.Error.Type) != "" {
				code = payload.Error.Type
			}
		} else if strings.TrimSpace(payload.BaseResp.StatusMsg) != "" {
			message = payload.BaseResp.StatusMsg
			rawCode = payload.BaseResp.StatusCode
		}
		// 错误码只接受字符串或数字，不把对象、数组等任意响应内容展示给用户。
		if kind := common.GetJsonType(rawCode); kind == "string" || kind == "number" {
			if value := strings.TrimSpace(common.JsonRawMessageToString(rawCode)); value != "" {
				code = value
			}
		}
		if requestID == "" {
			requestID = payload.Error.RequestID
			if requestID == "" {
				requestID = payload.RequestID
			}
		}
	}
	// 先移除完整凭证再截断，避免错误消息恰好在密钥中间截断而泄漏前缀。
	secrets := []string{key, strings.TrimSpace(c.GetHeader("Authorization"))}
	if fields := strings.Fields(c.GetHeader("Authorization")); len(fields) == 2 {
		secrets = append(secrets, fields[1])
	}
	for _, value := range []*string{&code, &message, &requestID} {
		for _, secret := range secrets {
			if secret != "" {
				*value = strings.ReplaceAll(*value, secret, "[REDACTED]")
			}
		}
		if upstreamID := task.GetUpstreamTaskID(); upstreamID != "" {
			*value = strings.ReplaceAll(*value, upstreamID, task.TaskID)
		}
		// 点分错误码是厂商协议标识，不能误按域名掩码；消息和 URL 仍执行通用脱敏。
		if value == &message || strings.ContainsAny(*value, "/?@=&") {
			*value = common.MaskSensitiveInfo(*value)
		}
		*value = strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, *value))
		limit := maxErrorIdentifierRunes
		if value == &message {
			limit = maxErrorMessageRunes
		}
		runes := []rune(*value)
		if len(runes) > limit {
			*value = string(runes[:limit]) + "…"
		}
	}
	if code == "" {
		code = "task_action_failed"
	}
	if message == "" {
		message = "Task action failed"
	}
	if requestID != "" {
		c.Header("X-Upstream-Request-Id", requestID)
	}
	logger.LogWarn(c, fmt.Sprintf("task_plugin action=delete task=%s channel_id=%d upstream_status=%d upstream_code=%q upstream_request_id=%q message=%q", task.TaskID, task.ChannelId, response.StatusCode, code, requestID, message))
	taskErr := &dto.TaskError{
		Code: code, Message: message, StatusCode: status,
		UpstreamError: &dto.TaskPluginError{Code: code, Message: message},
	}
	c.Abort()
	if !middleware.RespondTaskPluginError(c, taskErr) {
		respondTaskError(c, taskErr)
	}
}

func taskPluginNativeTaskOwned(meta pluginruntime.Meta, task *model.Task) bool {
	if task == nil {
		return false
	}
	return slices.Contains(taskPluginNativePlatforms(meta), task.Platform)
}

// retainTaskPluginUnsettledDeletion 防止查询与删除之间的上游状态变化被误算为失败退款。
// 若其他轮询已获得终态则沿用其结算；否则保留预扣并关闭轮询，明确要求对账。
func retainTaskPluginUnsettledDeletion(c *gin.Context, task *model.Task) bool {
	for range 2 {
		if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
			return true
		}
		previousStatus := task.Status
		task.Status = model.TaskStatusFailure
		task.Progress = "100%"
		task.FailReason = "upstream task deleted before final usage was available; reserved quota retained"
		task.FinishTime = time.Now().Unix()
		won, err := task.UpdateWithStatus(previousStatus)
		if err != nil {
			logger.LogError(c, fmt.Sprintf("删除任务待对账状态保存失败 task=%s err=%v", task.TaskID, err))
			return false
		}
		if won {
			logger.LogWarn(c, fmt.Sprintf("上游删除与完成竞态，保留预扣待对账 task=%s quota=%d", task.TaskID, task.Quota))
			return false
		}
		latest, exists, loadErr := model.GetByTaskId(task.UserId, task.TaskID)
		if loadErr != nil || !exists || latest == nil {
			return false
		}
		*task = *latest
	}
	return false
}

// taskPluginNativeTaskMatchesModels 验证任务的任一持久化模型身份属于允许范围。
// 同时检查两个字段以兼容模型映射和早期任务记录，两个字段都缺失时拒绝。
func taskPluginNativeTaskMatchesModels(models []string, task *model.Task) bool {
	if task == nil {
		return false
	}
	return slices.Contains(models, strings.TrimSpace(task.Properties.OriginModelName)) ||
		slices.Contains(models, strings.TrimSpace(task.Properties.UpstreamModelName))
}

// taskPluginNativeChannelOwned 验证任务原渠道仍由当前插件驱动。
// 防止渠道被重新绑定后，管理请求误发给另一个插件或上游服务。
func taskPluginNativeChannelOwned(meta pluginruntime.Meta, channelModel *model.Channel) bool {
	if channelModel == nil {
		return false
	}
	if channelModel.Type == constant.ChannelTypeTaskPlugin {
		return channelModel.GetSetting().TaskPluginKey == meta.Key
	}
	return slices.Contains(meta.ChannelTypes, channelModel.Type)
}

// markTaskPluginTaskCancelled 仅在上游确认取消后关闭本地任务。
// 终态和退款同事务提交；未获胜时重新加载任务，避免向调用方返回过期状态。
// 返回 false 表示本地更新或退款尚未完成，调用方必须返回待处理错误。
func markTaskPluginTaskCancelled(c *gin.Context, task *model.Task) bool {
	if task == nil || task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure {
		return task != nil
	}
	original := *task
	previousStatus := task.Status
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.FailReason = model.TaskCancelledReason
	if task.FinishTime == 0 {
		task.FinishTime = time.Now().Unix()
	}
	won, err := service.FinalizeVideoTaskBilling(c, task, previousStatus, 0, model.TaskCancelledReason, nil)
	if err != nil {
		logger.LogError(c, fmt.Sprintf("取消任务本地状态更新失败 task=%s err=%v", task.TaskID, err))
		*task = original
		return false
	}
	if !won {
		latest, exists, loadErr := model.GetByTaskId(task.UserId, task.TaskID)
		if loadErr != nil {
			logger.LogError(c, fmt.Sprintf("取消任务 CAS 失败且无法重新加载 task=%s err=%v", task.TaskID, loadErr))
		} else if exists && latest != nil {
			*task = *latest
		}
		return loadErr == nil && exists && latest != nil && (task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure)
	}
	return true
}

func renderTaskPluginNativeResult(c *gin.Context, pinned pluginruntime.PinnedRoute, requestContext pluginruntime.RouteRequestContext, input any) {
	result, err := pinned.Plugin.Engine.CallPath(c.Request.Context(), "native", []string{pinned.Route.Render}, requestContext.JSValue(), input)
	if err != nil {
		respondTaskPluginNativeActionError(c, http.StatusInternalServerError, "task_plugin_render_failed", "Task plugin renderer failed")
		return
	}
	c.Abort()
	c.JSON(http.StatusOK, result)
}

func respondTaskPluginNativeActionError(c *gin.Context, status int, code, message string) {
	taskErr := service.TaskErrorWrapperLocal(errors.New(message), code, status)
	c.Abort()
	if middleware.RespondTaskPluginError(c, taskErr) {
		return
	}
	respondTaskError(c, taskErr)
}
