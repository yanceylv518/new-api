package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type seedanceCompatValidationStartData struct {
	BytedToken    string `json:"byted_token"`
	H5Link        string `json:"h5_link"`
	HistoryID     uint   `json:"history_id"`
	SuggestModel  string `json:"suggest_model"`
	LivenessGroup string `json:"liveness_group"`
}

type seedanceCompatValidationHistoryData struct {
	HistoryID  uint   `json:"history_id"`
	GroupID    string `json:"group_id"`
	Status     string `json:"status"`
	ResultCode int    `json:"result_code,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func CreateSeedanceCompatValidationSession(c *gin.Context) {
	var request struct {
		CallbackURL string `json:"callback_url"`
		Model       string `json:"model"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		seedanceCompatError(c, http.StatusBadRequest, "invalid validation request")
		return
	}
	request.CallbackURL = strings.TrimSpace(request.CallbackURL)
	request.Model = strings.TrimSpace(request.Model)
	if request.CallbackURL != "" {
		if err := service.ValidateSeedanceAssetValidationCallbackURL(request.CallbackURL); err != nil {
			seedanceCompatError(c, http.StatusBadRequest, "invalid callback_url")
			return
		}
	}
	sealedCallback, err := common.SealTaskSecret(request.CallbackURL)
	if err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "validation session creation failed")
		return
	}
	choices, err := selectSeedanceAssetValidationChoices(c, request.Model, true)
	if err != nil {
		if errors.Is(err, errSeedanceAssetModelNotPermitted) || errors.Is(err, errSeedanceAssetNoChannel) {
			c.JSON(http.StatusForbidden, struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
				Message string `json:"message"`
			}{Success: false, Error: "model_no_permission", Message: "no available channel for this model"})
		} else {
			common.SysError("seedance validation channel selection failed: " + err.Error())
			seedanceCompatError(c, http.StatusServiceUnavailable, "validation service is temporarily unavailable")
		}
		return
	}
	channels := make([]*model.Channel, 0, len(choices))
	for _, choice := range choices {
		channels = append(channels, choice.channel)
	}
	base := strings.TrimSpace(system_setting.TaskPublicAddress)
	if base == "" {
		base = strings.TrimSpace(system_setting.ServerAddress)
	}
	if err := service.ValidateTaskArtifactBaseURL(base); err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "public address is not configured")
		return
	}
	state, err := common.GenerateRandomCharsKey(48)
	if err != nil {
		seedanceCompatError(c, http.StatusInternalServerError, "validation session creation failed")
		return
	}
	stateHash, err := service.HashSeedanceAssetValidationState(state)
	if err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "validation session creation failed")
		return
	}
	callback, err := url.Parse(base)
	if err != nil {
		seedanceCompatError(c, http.StatusServiceUnavailable, "public address is not configured")
		return
	}
	callback.Path = strings.TrimRight(callback.Path, "/") + "/doubao/api/assets/liveness/callback"
	callback.RawQuery = url.Values{"state": {state}}.Encode()
	started, err := service.StartSeedanceAssetValidationSessionWithFallback(c.Request.Context(), c.GetInt("id"), channels,
		service.SeedanceAssetValidationGroupDetails{}, "", callback.String())
	if err != nil {
		seedanceCompatError(c, http.StatusBadGateway, "validation session creation failed")
		return
	}
	suggestedModel := request.Model
	if suggestedModel == "" {
		for _, choice := range choices {
			if choice.channel.Id == started.Session.ChannelID {
				suggestedModel = choice.model
				break
			}
		}
	}
	if err := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetValidationSession{}).
		Where("id = ? AND user_id = ?", started.Session.ID, c.GetInt("id")).
		Updates(map[string]any{"callback_state_hash": stateHash, "compat_callback_url": sealedCallback, "compat_model": suggestedModel}).Error; err != nil {
		seedanceCompatError(c, http.StatusInternalServerError, "validation session creation failed")
		return
	}
	c.JSON(http.StatusOK, seedanceCompatSuccessResponse{Success: true, Data: seedanceCompatValidationStartData{
		BytedToken: started.BytedToken, H5Link: started.LaunchURL, HistoryID: started.Session.ID,
		SuggestModel: suggestedModel, LivenessGroup: "real",
	}})
}

func ListSeedanceCompatValidationHistory(c *gin.Context) {
	page, size := seedanceCompatPage(c)
	query := model.DB.WithContext(c.Request.Context()).Model(&model.SeedanceAssetValidationSession{}).Where("user_id = ?", c.GetInt("id"))
	if groupID := strings.TrimSpace(c.Query("group_id")); groupID != "" {
		query = query.Where("upstream_group_id = ?", groupID)
	}
	if status := strings.ToLower(strings.TrimSpace(c.Query("status"))); status != "" {
		switch status {
		case "pending":
			query = query.Where("status IN ?", []string{model.SeedanceAssetValidationStatusCreating, model.SeedanceAssetValidationStatusPending})
		case "success":
			query = query.Where("status = ?", model.SeedanceAssetValidationStatusSucceeded)
		case "failed":
			query = query.Where("status IN ?", []string{model.SeedanceAssetValidationStatusFailed, model.SeedanceAssetValidationStatusExpired, model.SeedanceAssetValidationStatusCancelled})
		default:
			seedanceCompatError(c, http.StatusBadRequest, "invalid status")
			return
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	sessions := make([]model.SeedanceAssetValidationSession, 0)
	if err := query.Order("id desc").Offset((page - 1) * size).Limit(size).Find(&sessions).Error; err != nil {
		seedanceCompatLookupError(c, err)
		return
	}
	data := make([]seedanceCompatValidationHistoryData, 0, len(sessions))
	for _, session := range sessions {
		status := "pending"
		switch session.Status {
		case model.SeedanceAssetValidationStatusSucceeded:
			status = "success"
		case model.SeedanceAssetValidationStatusFailed, model.SeedanceAssetValidationStatusExpired, model.SeedanceAssetValidationStatusCancelled:
			status = "failed"
		}
		item := seedanceCompatValidationHistoryData{HistoryID: session.ID, GroupID: session.UpstreamGroupID, Status: status,
			CreatedAt: session.CreatedAt.In(seedanceCompatTimeZone).Format(time.RFC3339),
			UpdatedAt: session.UpdatedAt.In(seedanceCompatTimeZone).Format(time.RFC3339)}
		if status == "success" {
			item.ResultCode = 10000
		}
		data = append(data, item)
	}
	c.JSON(http.StatusOK, seedanceCompatSuccessPageResponse{Success: true, Data: data, Total: total, Page: page, PageSize: size})
}

func SeedanceCompatValidationCallback(c *gin.Context) {
	query, stateHash, err := parseSeedanceValidationCallback(c)
	if err != nil {
		seedanceCompatError(c, http.StatusNotFound, "validation session not found")
		return
	}
	var session model.SeedanceAssetValidationSession
	if err := model.DB.WithContext(c.Request.Context()).Where("callback_state_hash = ?", stateHash).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			seedanceCompatError(c, http.StatusNotFound, "validation session not found")
		} else {
			common.SysError("seedance validation callback lookup failed: " + common.MaskSensitiveInfo(err.Error()))
			seedanceCompatValidationWait(c, 0)
		}
		return
	}
	if !seedanceValidationCallbackTokenMatches(query, &session) {
		seedanceCompatError(c, http.StatusNotFound, "validation session not found")
		return
	}
	refreshed, err := service.RefreshSeedanceAssetValidationSession(c.Request.Context(), session.UserID, session.ID)
	if err != nil {
		if session.ExpiresAt > time.Now().Unix() {
			seedanceCompatValidationWait(c, session.ExpiresAt)
			return
		}
		seedanceCompatError(c, http.StatusBadGateway, "validation status is temporarily unavailable")
		return
	}
	if refreshed.Status == model.SeedanceAssetValidationStatusPending || refreshed.Status == model.SeedanceAssetValidationStatusCreating {
		seedanceCompatValidationWait(c, session.ExpiresAt)
		return
	}
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'")
	if session.CompatCallbackURL != "" {
		callbackURL, err := common.OpenTaskSecret(session.CompatCallbackURL)
		if err != nil || service.ValidateSeedanceAssetValidationCallbackURL(callbackURL) != nil {
			seedanceCompatError(c, http.StatusBadGateway, "validation callback is invalid")
			return
		}
		redirect, err := url.Parse(callbackURL)
		if err != nil {
			seedanceCompatError(c, http.StatusBadGateway, "validation callback is invalid")
			return
		}
		values := redirect.Query()
		for _, key := range []string{"bytedToken", "resultCode", "algorithmBaseRespCode", "reqMeasureInfoValue", "verify_type"} {
			if value := query.Get(key); value != "" {
				values.Set(key, value)
			}
		}
		setSeedanceValidationCallbackResult(values, refreshed)
		redirect.RawQuery = values.Encode()
		if c.GetHeader("X-Seedance-Validation-Poll") == "1" {
			c.JSON(http.StatusOK, gin.H{"status": "complete", "redirect_url": redirect.String()})
			return
		}
		c.Redirect(http.StatusFound, redirect.String())
		return
	}
	message := "Validation completed"
	if refreshed.Status != model.SeedanceAssetValidationStatusSucceeded {
		message = "Validation was not completed"
	}
	if c.GetHeader("X-Seedance-Validation-Poll") == "1" {
		c.JSON(http.StatusOK, gin.H{"status": "complete", "message": message})
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte("<!doctype html><html><body><p>"+message+"</p><script>window.close()</script></body></html>"))
}

func seedanceCompatValidationWait(c *gin.Context, expiresAt int64) {
	if c.GetHeader("X-Seedance-Validation-Poll") == "1" {
		c.JSON(http.StatusOK, gin.H{"status": "pending"})
		return
	}
	if expiresAt <= 0 {
		expiresAt = time.Now().Add(30 * time.Minute).Unix()
	}
	interval := int64(10)
	limit := common.GetManagementRateLimits().Sensitive
	if limit.Enabled && limit.Requests > 0 && limit.WindowSeconds > 0 {
		interval = max(interval, (limit.WindowSeconds+int64(limit.Requests)-1)/int64(limit.Requests)+1)
	}
	c.Header("Content-Security-Policy", "default-src 'none'; connect-src 'self'; script-src 'unsafe-inline'")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(fmt.Sprintf(`<!doctype html><html><body><p id="status">认证处理中，请稍候</p><script>
const interval=%d*1000, deadline=%d*1000;
async function poll(){
  if(Date.now()>deadline+60000){document.getElementById('status').textContent='认证结果暂时不可用，请稍后查询认证历史';return;}
  try{
    const r=await fetch(location.href,{headers:{'X-Seedance-Validation-Poll':'1'},cache:'no-store',credentials:'omit'});
    if(r.status===404){document.getElementById('status').textContent='认证会话已失效，请重新发起认证';return;}
    if(r.ok){const data=await r.json();if(data.status==='complete'){if(data.redirect_url)location.replace(data.redirect_url);else{document.getElementById('status').textContent=data.message;window.close();}return;}}
  }catch(e){}
  setTimeout(poll,interval);
}
setTimeout(poll,5000);
</script></body></html>`, interval, expiresAt)))
}
