/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

package controller

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

type seedanceAssetValidationSessionRequest struct {
	Name                string `json:"name" binding:"max=64"`
	Description         string `json:"description" binding:"max=1024"`
	Tags                string `json:"tags" binding:"max=512"`
	Model               string `json:"model" binding:"max=191"`
	CallbackURL         string `json:"callback_url"`
	OfficialCallbackURL string `json:"CallbackURL"`
	ProjectName         string `json:"project_name" binding:"max=191"`
	OfficialProjectName string `json:"ProjectName" binding:"max=191"`
}

type seedanceAssetValidationResultRequest struct {
	BytedToken          string `json:"BytedToken" binding:"required"`
	OfficialProjectName string `json:"ProjectName" binding:"max=191"`
}

func (request seedanceAssetValidationSessionRequest) callbackURL() string {
	if strings.TrimSpace(request.CallbackURL) != "" {
		return strings.TrimSpace(request.CallbackURL)
	}
	return strings.TrimSpace(request.OfficialCallbackURL)
}

func (request seedanceAssetValidationSessionRequest) projectName() string {
	if strings.TrimSpace(request.ProjectName) != "" {
		return strings.TrimSpace(request.ProjectName)
	}
	return strings.TrimSpace(request.OfficialProjectName)
}

func parseSeedanceValidationSessionID(c *gin.Context) (uint, error) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		return 0, errors.New("invalid validation session id")
	}
	return uint(id), nil
}

func createSeedanceAssetValidationSession(c *gin.Context, official bool) {
	var request seedanceAssetValidationSessionRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		if official {
			writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "CreateVisualValidateSession", "Invalid validation session request")
		} else {
			common.ApiErrorI18n(c, "invalid_params")
		}
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Description = strings.TrimSpace(request.Description)
	request.Tags = strings.TrimSpace(request.Tags)
	request.Model = strings.TrimSpace(request.Model)
	callbackURL := request.callbackURL()
	projectName := request.projectName()
	if official && callbackURL == "" {
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "CreateVisualValidateSession", "CallbackURL is required")
		return
	}
	channels, err := selectSeedanceAssetValidationChannels(c, request.Model)
	if err != nil {
		if official {
			writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "CreateVisualValidateSession", err.Error())
		} else {
			common.ApiError(c, err)
		}
		return
	}
	start, err := service.StartSeedanceAssetValidationSessionWithFallback(
		c.Request.Context(),
		c.GetInt("id"),
		channels,
		service.SeedanceAssetValidationGroupDetails{
			Name:        request.Name,
			Description: request.Description,
			Tags:        request.Tags,
			CallbackURL: callbackURL,
		},
		projectName,
		"",
	)
	if err != nil {
		if official {
			writeSeedanceAssetOfficialUpstreamError(c, http.StatusBadGateway, "CreateVisualValidateSession", err.Error())
		} else {
			common.ApiError(c, err)
		}
		return
	}
	if official {
		result := gin.H{
			"BytedToken": start.BytedToken,
			"H5Link":     start.LaunchURL,
		}
		if c.GetBool("seedance_compat_ark_action") {
			result["CallbackURL"] = callbackURL
		}
		writeSeedanceAssetOfficialResponse(c, http.StatusOK, seedanceAssetOfficialResponse("CreateVisualValidateSession", result))
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": "",
		"data":    service.BuildSeedanceAssetValidationSessionView(start.Session),
	})
}

// CreateSeedanceAssetValidationSession starts the user-facing H5 verification flow.
func CreateSeedanceAssetValidationSession(c *gin.Context) {
	createSeedanceAssetValidationSession(c, false)
}

// GetSeedanceAssetValidationSession refreshes one user-owned validation session.
func GetSeedanceAssetValidationSession(c *gin.Context) {
	id, err := parseSeedanceValidationSessionID(c)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	session, err := service.RefreshSeedanceAssetValidationSession(c.Request.Context(), c.GetInt("id"), id)
	if err != nil {
		if errors.Is(err, model.ErrSeedanceAssetValidationSessionNotFound) {
			common.ApiError(c, err)
			return
		}
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": service.BuildSeedanceAssetValidationSessionView(session)})
}

// SeedanceAssetValidationCallback is a capability URL used as the upstream redirect target.
// It contains no API key; the random state is checked before reading or refreshing the session.
func SeedanceAssetValidationCallback(c *gin.Context) {
	id, err := parseSeedanceValidationSessionID(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "validation session not found"})
		return
	}
	query, stateHash, err := parseSeedanceValidationCallback(c)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "validation session not found"})
		return
	}
	session, err := model.FindSeedanceAssetValidationSessionByCallbackState(c.Request.Context(), id, stateHash)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "validation session not found"})
		return
	}
	if !seedanceValidationCallbackTokenMatches(query, session) {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "validation session not found"})
		return
	}
	var redirect *url.URL
	if session.CompatCallbackURL != "" {
		callbackURL, openErr := common.OpenTaskSecret(session.CompatCallbackURL)
		if openErr != nil || service.ValidateSeedanceAssetValidationCallbackURL(callbackURL) != nil {
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "validation callback is invalid"})
			return
		}
		redirect, err = url.Parse(callbackURL)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "validation callback is invalid"})
			return
		}
	}
	refreshed, err := service.RefreshSeedanceAssetValidationSession(c.Request.Context(), session.UserID, session.ID)
	if redirect != nil {
		if err != nil {
			common.SysError(fmt.Sprintf("Seedance validation callback refresh failed for session %d: %v", session.ID, err))
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "validation status is temporarily unavailable"})
			return
		}
		if refreshed.Status == model.SeedanceAssetValidationStatusPending || refreshed.Status == model.SeedanceAssetValidationStatusCreating {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "message": "validation is still pending"})
			return
		}
		values := redirect.Query()
		for key, incoming := range query {
			if key == "state" {
				continue
			}
			values.Del(key)
			for _, value := range incoming {
				values.Add(key, value)
			}
		}
		setSeedanceValidationCallbackResult(values, refreshed)
		redirect.RawQuery = values.Encode()
		c.Header("Referrer-Policy", "no-referrer")
		c.Redirect(http.StatusFound, redirect.String())
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": service.BuildSeedanceAssetValidationSessionView(refreshed)})
}

func parseSeedanceValidationCallback(c *gin.Context) (url.Values, string, error) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil || len(query["state"]) != 1 || strings.TrimSpace(query.Get("state")) == "" {
		return nil, "", model.ErrSeedanceAssetValidationSessionNotFound
	}
	stateHash, err := service.HashSeedanceAssetValidationState(query.Get("state"))
	return query, stateHash, err
}

func seedanceValidationCallbackTokenMatches(query url.Values, session *model.SeedanceAssetValidationSession) bool {
	// 部分渠道只回传 state；认证结果仍须向会话绑定的账号查询，不能信任回跳中的成功标记。
	tokens, supplied := query["bytedToken"]
	if !supplied {
		return true
	}
	if len(tokens) != 1 || strings.TrimSpace(tokens[0]) == "" || session.BytedTokenHash == nil {
		return false
	}
	tokenHash, err := service.HashSeedanceAssetValidationToken(tokens[0])
	return err == nil && hmac.Equal([]byte(tokenHash), []byte(*session.BytedTokenHash))
}

func setSeedanceValidationCallbackResult(values url.Values, session *model.SeedanceAssetValidationSession) {
	values.Set("session_id", fmt.Sprint(session.ID))
	values.Del("GroupId")
	values.Set("status", "failed")
	if values.Get("resultCode") == "10000" {
		values.Del("resultCode")
	}
	if session.Status == model.SeedanceAssetValidationStatusSucceeded {
		values.Set("resultCode", "10000")
		values.Set("status", "success")
		values.Set("GroupId", session.UpstreamGroupID)
	}
}

func seedanceAssetOfficialResponse(action string, result gin.H) gin.H {
	return gin.H{
		"ResponseMetadata": gin.H{
			"RequestId": common.NewRequestId(),
			"Action":    action,
			"Version":   "2024-01-01",
			"Service":   "ark",
			"Region":    "cn-beijing",
		},
		"Result": result,
	}
}

func writeSeedanceAssetOfficialError(c *gin.Context, status int, action, message string) {
	writeSeedanceAssetOfficialErrorWithCode(c, status, action, "ValidationError", message)
}

func writeSeedanceAssetOfficialUpstreamError(c *gin.Context, status int, action, message string) {
	writeSeedanceAssetOfficialErrorWithCode(c, status, action, "UpstreamError", message)
}

func writeSeedanceAssetOfficialErrorWithCode(c *gin.Context, status int, action, code, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream validation operation failed"
	}
	writeSeedanceAssetOfficialResponse(c, status, gin.H{
		"ResponseMetadata": gin.H{
			"RequestId": common.NewRequestId(),
			"Action":    action,
			"Version":   "2024-01-01",
			"Service":   "ark",
			"Region":    "cn-beijing",
			"Error": gin.H{
				"Code":    code,
				"Message": message,
			},
		},
	})
}

// SeedanceCompatArkAssetAction 保留客户入口的可选 Version 约定。
func SeedanceCompatArkAssetAction(c *gin.Context) {
	c.Set("seedance_compat_ark_action", true)
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "", "invalid query string")
		return
	}
	if len(query["Action"]) != 1 || len(query["Version"]) > 1 {
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "", "Action must appear once; Version must appear at most once")
		return
	}
	if !query.Has("Version") {
		query.Set("Version", "2024-01-01")
	}
	c.Request.URL.RawQuery = query.Encode()
	SeedanceAssetV2Action(c)
}

// SeedanceAssetV2Action 在同一入口分发素材管理和真人认证，资源归属由平台校验。
func SeedanceAssetV2Action(c *gin.Context) {
	action := strings.TrimSpace(c.Query("Action"))
	version := strings.TrimSpace(c.Query("Version"))
	if version != "2024-01-01" {
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, action, fmt.Sprintf("unsupported Version %q", version))
		return
	}
	if action == "" {
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, action, "Action is required")
		return
	}
	switch action {
	case "CreateVisualValidateSession":
		createSeedanceAssetValidationSession(c, true)
	case "GetVisualValidateResult":
		var request seedanceAssetValidationResultRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "GetVisualValidateResult", "BytedToken is required")
			return
		}
		session, err := service.RefreshSeedanceAssetValidationSessionByToken(c.Request.Context(), c.GetInt("id"), request.BytedToken, request.OfficialProjectName)
		if err != nil {
			if errors.Is(err, model.ErrSeedanceAssetValidationSessionNotFound) {
				writeSeedanceAssetOfficialError(c, http.StatusNotFound, "GetVisualValidateResult", "validation token is invalid")
				return
			}
			if errors.Is(err, service.ErrSeedanceAssetValidationProject) {
				writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "GetVisualValidateResult", err.Error())
				return
			}
			writeSeedanceAssetOfficialUpstreamError(c, http.StatusBadGateway, "GetVisualValidateResult", err.Error())
			return
		}
		if session.Status != model.SeedanceAssetValidationStatusSucceeded || session.UpstreamGroupID == "" {
			view := service.BuildSeedanceAssetValidationSessionView(session)
			if view.StatusCheckRetrying {
				c.Header("Retry-After", "5")
				writeSeedanceAssetOfficialErrorWithCode(c, http.StatusServiceUnavailable, "GetVisualValidateResult", "ServiceUnavailable", service.ErrSeedanceAssetServiceTemporary.Error())
				return
			}
			if (session.Status == model.SeedanceAssetValidationStatusPending || session.Status == model.SeedanceAssetValidationStatusCreating) && view.LastError == "" {
				c.Header("Retry-After", "5")
				writeSeedanceAssetOfficialErrorWithCode(c, http.StatusConflict, "GetVisualValidateResult", "ValidationPending", service.ErrSeedanceAssetValidationPending.Error())
				return
			}
			message := view.LastError
			if message == "" {
				message = service.ErrSeedanceAssetValidationPending.Error()
			}
			if session.Status == model.SeedanceAssetValidationStatusExpired {
				writeSeedanceAssetOfficialErrorWithCode(c, http.StatusBadRequest, "GetVisualValidateResult", "ValidationExpired", service.ErrSeedanceAssetValidationExpired.Error())
				return
			}
			if session.Status == model.SeedanceAssetValidationStatusFailed {
				if view.LastError == "" {
					message = service.ErrSeedanceAssetValidationFailed.Error()
				}
				writeSeedanceAssetOfficialErrorWithCode(c, http.StatusBadRequest, "GetVisualValidateResult", "ValidationFailed", message)
				return
			}
			if session.Status == model.SeedanceAssetValidationStatusCancelled {
				writeSeedanceAssetOfficialErrorWithCode(c, http.StatusBadRequest, "GetVisualValidateResult", "ValidationCancelled", "This verification session can no longer be continued.")
				return
			}
			writeSeedanceAssetOfficialUpstreamError(c, http.StatusBadGateway, "GetVisualValidateResult", message)
			return
		}
		writeSeedanceAssetOfficialResponse(c, http.StatusOK, seedanceAssetOfficialResponse("GetVisualValidateResult", gin.H{"GroupId": session.UpstreamGroupID}))
	default:
		handleSeedanceAssetLibraryAction(c, action)
	}
}
