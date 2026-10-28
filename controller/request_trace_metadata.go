package controller

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func GetRequestTraceMetadata(c *gin.Context) {
	requestId := c.Query("request_id")
	if requestId == "" || len(requestId) > 64 || strings.TrimSpace(requestId) != requestId {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "无效的请求 ID"})
		return
	}
	userID, admin := c.GetInt("id"), c.GetInt("role") >= common.RoleAdminUser
	pending := model.HasPendingRequestTrace(requestId, userID, admin)
	rows, err := model.GetRequestTraces(c.Request.Context(), requestId, userID, admin)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows, "pending": pending || model.HasPendingRequestTrace(requestId, userID, admin)})
}

func GetRequestTraceMetadataStatus(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
	defer cancel()
	task, err := model.GetLatestRequestTraceCleanup(ctx, model.SystemTaskTypeRequestTraceCleanup)
	var cleanup *model.SystemTaskResponse
	cleanupError := ""
	if err != nil {
		// 可选的清理历史读取失败时，仍返回进程内写入计数。
		cleanupError = common.TraceMetadataString(err.Error(), 256)
	} else if task != nil {
		response := task.ToResponse()
		cleanup = &response
	}
	common.ApiSuccess(c, struct {
		model.RequestTraceWriterStats
		Cleanup      *model.SystemTaskResponse `json:"cleanup,omitempty"`
		CleanupError string                    `json:"cleanup_error,omitempty"`
	}{RequestTraceWriterStats: model.GetRequestTraceWriterStats(), Cleanup: cleanup, CleanupError: cleanupError})
}
