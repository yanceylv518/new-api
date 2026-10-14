package controller

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// These handlers are registered only on the admin user-management routes.
func GetUserTokenResourcePolicies(c *gin.Context) {
	user, ok := getManageableUser(c)
	if !ok {
		return
	}
	page := common.GetPageQuery(c)
	if page.Page < 1 || page.PageSize < 1 || page.Page > math.MaxInt/page.PageSize {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid pagination."})
		return
	}
	items, err := model.GetUserTokenResourcePolicies(c.Request.Context(), user.Id, page.GetStartIdx(), page.GetPageSize())
	if err != nil {
		common.SysError("failed to list token resource policies: " + err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Resource policy is temporarily unavailable."})
		return
	}
	total, err := model.CountUserTokens(user.Id)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Resource policy is temporarily unavailable."})
		return
	}
	page.SetItems(items)
	page.SetTotal(int(total))
	defaults, err := setting.GetConfiguredTokenResourceLimitSettings()
	if err != nil {
		common.SysError("failed to read token resource defaults: " + err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Resource policy is temporarily unavailable."})
		return
	}
	// Add metadata without changing existing pagination or policy fields.
	common.ApiSuccess(c, struct {
		*common.PageInfo
		Defaults setting.TokenResourceLimitSettings `json:"defaults"`
	}{PageInfo: page, Defaults: defaults})
}

func UpdateUserTokenResourcePolicy(c *gin.Context) {
	user, ok := getManageableUser(c)
	if !ok {
		return
	}
	tokenID, err := strconv.Atoi(c.Param("token_id"))
	if err != nil || tokenID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid token ID."})
		return
	}
	if _, err := model.GetTokenByIds(tokenID, user.Id); err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"success": false, "message": "Token is unavailable."})
		return
	}
	// PUT replaces both fields. Require explicit null to reset inheritance;
	// reject incomplete/unknown fields instead of silently erasing a limit.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	body, readErr := io.ReadAll(c.Request.Body)
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); readErr != nil || err != nil || len(fields) != 2 || fields["max_request_body_bytes"] == nil || fields["max_concurrent_requests"] == nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Both resource limit fields are required."})
		return
	}
	policy := model.TokenResourcePolicy{TokenID: tokenID}
	if err := common.Unmarshal(fields["max_request_body_bytes"], &policy.MaxRequestBodyBytes); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid request body limit."})
		return
	}
	if err := common.Unmarshal(fields["max_concurrent_requests"], &policy.MaxConcurrentRequests); err != nil || policy.Validate() != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid resource limits."})
		return
	}
	if err := model.SetTokenResourcePolicy(c.Request.Context(), policy); err != nil {
		common.SysError("failed to update token resource policy: " + err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Resource policy is temporarily unavailable."})
		return
	}
	common.ApiSuccess(c, policy)
}
