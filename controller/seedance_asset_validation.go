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
	"errors"
	"fmt"
	"net/http"
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
	if request.Name != "" {
		duplicated, err := model.IsSeedanceAssetGroupNameDuplicated(0, c.GetInt("id"), request.Name)
		if err != nil {
			if official {
				writeSeedanceAssetOfficialError(c, http.StatusInternalServerError, "CreateVisualValidateSession", err.Error())
			} else {
				common.ApiError(c, err)
			}
			return
		}
		if duplicated {
			if official {
				writeSeedanceAssetOfficialError(c, http.StatusConflict, "CreateVisualValidateSession", "validation group name already exists")
			} else {
				common.ApiErrorMsg(c, "asset group name already exists")
			}
			return
		}
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
		},
		projectName,
		callbackURL,
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
		c.JSON(http.StatusOK, seedanceAssetOfficialResponse("CreateVisualValidateSession", gin.H{
			"BytedToken": start.BytedToken,
			"H5Link":     start.LaunchURL,
		}))
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
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "validation session not found"})
		return
	}
	stateHash, err := service.HashSeedanceAssetValidationState(state)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "validation session not found"})
		return
	}
	session, err := model.FindSeedanceAssetValidationSessionByCallbackState(c.Request.Context(), id, stateHash)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": "validation session not found"})
		return
	}
	refreshed, err := service.RefreshSeedanceAssetValidationSession(c.Request.Context(), session.UserID, session.ID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": service.BuildSeedanceAssetValidationSessionView(refreshed)})
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
	c.JSON(status, gin.H{
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

// SeedanceAssetV2Action exposes only the two official validation Actions that New API can bind safely.
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
			message := session.LastError
			if message == "" {
				message = service.ErrSeedanceAssetValidationPending.Error()
			}
			if session.Status == model.SeedanceAssetValidationStatusExpired {
				writeSeedanceAssetOfficialError(c, http.StatusBadRequest, "GetVisualValidateResult", message)
				return
			}
			writeSeedanceAssetOfficialUpstreamError(c, http.StatusBadGateway, "GetVisualValidateResult", message)
			return
		}
		c.JSON(http.StatusOK, seedanceAssetOfficialResponse("GetVisualValidateResult", gin.H{"GroupId": session.UpstreamGroupID}))
	default:
		writeSeedanceAssetOfficialError(c, http.StatusBadRequest, c.Query("Action"), "unsupported asset Action")
	}
}
