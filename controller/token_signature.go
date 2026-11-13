package controller

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func tokenSignatureID(c *gin.Context, bodyID *int) (int, bool) {
	tokenID := c.GetInt("token_id")
	if bodyID != nil && *bodyID != tokenID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "token does not match the authenticated API key"})
		return 0, false
	}
	if raw := strings.TrimSpace(c.Query("token_id")); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "token_id must be a positive integer"})
			return 0, false
		}
		if id != tokenID {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "token does not match the authenticated API key"})
			return 0, false
		}
	}
	return tokenID, tokenID > 0
}

func GetTokenSignatureCredential(c *gin.Context) {
	tokenID, ok := tokenSignatureID(c, nil)
	if !ok {
		return
	}
	credential, err := model.GetTokenSignatureCredential(c.Request.Context(), tokenID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiSuccess(c, gin.H{"token_id": tokenID, "enabled": false})
		return
	}
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "signature credentials are temporarily unavailable"})
		return
	}
	common.ApiSuccess(c, gin.H{"token_id": tokenID, "enabled": true, "access_key": credential.AccessKey, "created_at": credential.CreatedAt})
}

func IssueTokenSignatureCredential(c *gin.Context) {
	var request struct {
		TokenID *int `json:"token_id"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 4096))
	if common.IsRequestBodyTooLargeError(err) {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body is too large"})
		return
	}
	if err != nil || (len(strings.TrimSpace(string(body))) != 0 && common.Unmarshal(body, &request) != nil) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	tokenID, ok := tokenSignatureID(c, request.TokenID)
	if !ok {
		return
	}
	credential, secret, err := model.IssueTokenSignatureCredential(c.Request.Context(), c.GetInt("id"), tokenID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "signature credentials could not be issued"})
		return
	}
	actorRole := 0
	if actor, cacheErr := model.GetUserCache(c.GetInt("id")); cacheErr == nil {
		actorRole = actor.Role
	}
	model.RecordAuditLog(c, model.AuditLog{UserId: c.GetInt("id"), ActorRole: actorRole, Category: model.AuditCategorySecurity,
		Action: "token.aksk.issue", Content: "API token signature credential issued", AuthMethod: "api_key", Success: true,
		Other: model.AuditOther{Op: &model.AuditOperation{Action: "token.aksk.issue", Params: model.AuditFields{"token_id": tokenID}}}})
	common.ApiSuccess(c, gin.H{"token_id": tokenID, "access_key": credential.AccessKey, "secret_key": secret, "created_at": credential.CreatedAt})
}

func RevokeTokenSignatureCredential(c *gin.Context) {
	tokenID, ok := tokenSignatureID(c, nil)
	if !ok {
		return
	}
	if err := model.RevokeTokenSignatureCredential(c.Request.Context(), c.GetInt("id"), tokenID); err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "signature credentials could not be revoked"})
		return
	}
	actorRole := 0
	if actor, cacheErr := model.GetUserCache(c.GetInt("id")); cacheErr == nil {
		actorRole = actor.Role
	}
	model.RecordAuditLog(c, model.AuditLog{UserId: c.GetInt("id"), ActorRole: actorRole, Category: model.AuditCategorySecurity,
		Action: "token.aksk.revoke", Content: "API token signature credential revoked", AuthMethod: "api_key", Success: true,
		Other: model.AuditOther{Op: &model.AuditOperation{Action: "token.aksk.revoke", Params: model.AuditFields{"token_id": tokenID}}}})
	common.ApiSuccess(c, gin.H{"token_id": tokenID, "enabled": false})
}
