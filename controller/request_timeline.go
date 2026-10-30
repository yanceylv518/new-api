package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/requesttimeline"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func GetRequestTimeline(c *gin.Context) {
	// Defense in depth: the route also uses AdminAuth.
	if c.GetInt("role") < common.RoleAdminUser {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	id := c.Param("request_id")
	if len(id) == 0 || len(id) > 64 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request ID"})
		return
	}
	result := gin.H{"found": false, "enabled": service.RequestTimelineEnabled(), "status": "unavailable", "record": nil, "timeline": nil}
	if !service.RequestTimelineStorageReady() {
		common.ApiSuccess(c, result)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	entry, err := model.FindRequestTimeline(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		result["status"] = "missing"
		common.ApiSuccess(c, result)
		return
	}
	if err != nil {
		common.SysError("request timeline query failed: " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "Unable to load request timeline"})
		return
	}
	result["found"], result["record"] = true, entry
	var data requesttimeline.Data
	if len(entry.Data) > requesttimeline.MaxDataBytes || common.UnmarshalJsonStr(entry.Data, &data) != nil || data.StartedAtMS <= 0 {
		result["status"] = "invalid_data"
	} else if data.Version != requesttimeline.Version {
		result["status"] = "unsupported_version"
	} else {
		result["status"], result["timeline"] = "available", data
	}
	common.ApiSuccess(c, result)
}
