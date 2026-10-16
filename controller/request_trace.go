package controller

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func GetRequestTrace(c *gin.Context) {
	requestId := c.Query("request_id")
	if requestId == "" || len(requestId) > 64 || strings.TrimSpace(requestId) != requestId {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "无效的请求 ID"})
		return
	}
	rows, err := model.GetRequestTraces(c.Request.Context(), requestId, c.GetInt("id"), c.GetInt("role") >= common.RoleAdminUser)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, rows)
}
