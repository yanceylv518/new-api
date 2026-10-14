package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Routes require AdminAuth plus the corresponding channel read/write permission.
func GetChannelResourcePolicy(c *gin.Context) {
	id, ok := channelResourcePolicyID(c)
	if !ok {
		return
	}
	// Check existence without loading channel credentials or using a stale cache.
	var channel model.Channel
	if err := model.DB.WithContext(c.Request.Context()).Select("id").First(&channel, id).Error; err != nil {
		respondChannelResourcePolicyError(c, err)
		return
	}
	policy, err := model.GetChannelResourcePolicy(c.Request.Context(), id)
	if err != nil {
		respondChannelResourcePolicyError(c, err)
		return
	}
	if policy == nil {
		policy = &model.ChannelResourcePolicy{ChannelID: id}
	}
	common.ApiSuccess(c, policy)
}

func UpdateChannelResourcePolicy(c *gin.Context) {
	id, ok := channelResourcePolicyID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	body, readErr := io.ReadAll(c.Request.Body)
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); readErr != nil || err != nil || len(fields) != 1 || fields["max_request_body_bytes"] == nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Only max_request_body_bytes is required."})
		return
	}
	var limit *int64
	if err := common.Unmarshal(fields["max_request_body_bytes"], &limit); err != nil || limit == nil || *limit < 0 || *limit > 9007199254740991 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Request body limit must be a non-negative safe integer in bytes."})
		return
	}
	policy := model.ChannelResourcePolicy{ChannelID: id, MaxRequestBodyBytes: *limit}
	if err := model.SetChannelResourcePolicy(c.Request.Context(), policy); err != nil {
		respondChannelResourcePolicyError(c, err)
		return
	}
	common.ApiSuccess(c, policy)
}

func channelResourcePolicyID(c *gin.Context) (int, bool) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "Invalid channel ID."})
		return 0, false
	}
	return id, true
}

func respondChannelResourcePolicyError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "Channel is unavailable."})
		return
	}
	common.SysError("channel resource policy operation failed: " + err.Error())
	c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "Resource policy is temporarily unavailable."})
}
