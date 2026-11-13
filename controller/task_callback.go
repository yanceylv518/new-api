package controller

import (
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TaskPluginCallback(c *gin.Context) {
	access := c.QueryArray("access")
	if len(access) != 1 {
		c.Status(http.StatusNotFound)
		return
	}
	target, err := service.ParseTaskCallbackTarget(access[0], c.Param("key"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	var owner model.User
	if err := model.DB.WithContext(c.Request.Context()).Select("id", "status").Where("id = ?", target.UserID).First(&owner).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.Status(http.StatusNotFound)
		} else {
			c.Status(http.StatusServiceUnavailable)
		}
		return
	}
	if owner.Status != common.UserStatusEnabled {
		c.Status(http.StatusNotFound)
		return
	}
	task, found, err := model.GetByTaskId(target.UserID, target.TaskID)
	if err != nil || !found {
		// 回调可能早于提交结果落库；要求上游按其重试策略再次投递。
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if task != nil && task.PrivateDataReadError != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if task == nil || string(task.Platform) != target.PluginKey || task.PrivateData.NativeDeleted {
		c.Status(http.StatusNotFound)
		return
	}
	if task.PrivateData.SubmissionPending {
		// 意图已存在时仍可能没有上游 ID；提交交接后再按完整身份校验回调。
		c.Status(http.StatusServiceUnavailable)
		return
	}
	const maxCallbackBodyBytes = 1 << 20
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxCallbackBodyBytes)
	defer c.Request.Body.Close()
	body, err := io.ReadAll(c.Request.Body)
	var event struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err != nil || common.Unmarshal(body, &event) != nil || event.ID != task.GetUpstreamTaskID() ||
		!slices.Contains([]string{"queued", "pending", "running", "processing", "succeeded", "failed", "cancelled", "expired"}, event.Status) {
		c.Status(http.StatusBadRequest)
		return
	}
	if err := service.ForwardTaskCallback(c.Request.Context(), target, task, body); err != nil {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, gin.H{})
}
