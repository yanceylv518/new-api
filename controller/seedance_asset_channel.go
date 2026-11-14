package controller

import (
	"errors"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

type seedanceAssetChannelChoice struct {
	channel      *model.Channel
	model        string
	requestModel string
}

var errSeedanceAssetModelNotPermitted = errors.New("API key does not allow this model")
var errSeedanceAssetNoChannel = errors.New("no available Seedance asset channel for the current group and model permissions")

// selectSeedanceAssetValidationChannels 按优先级降序筛选允许访问的素材渠道，供认证回退和官方建组共用。
func selectSeedanceAssetValidationChannels(c *gin.Context, modelName string) ([]*model.Channel, error) {
	choices, err := selectSeedanceAssetValidationCandidates(c, modelName, false, true)
	if err != nil {
		return nil, err
	}
	channels := make([]*model.Channel, 0, len(choices))
	for _, choice := range choices {
		channels = append(channels, choice.channel)
	}
	return channels, nil
}

func selectSeedanceAssetValidationChoices(c *gin.Context, modelName string, matchFamily bool) ([]seedanceAssetChannelChoice, error) {
	return selectSeedanceAssetValidationCandidates(c, modelName, matchFamily, false)
}

func selectSeedanceAssetValidationCandidates(c *gin.Context, modelName string, matchFamily, includeFallback bool) ([]seedanceAssetChannelChoice, error) {
	modelLimitsEnabled := c.GetBool("token_model_limit_enabled")
	allowed := map[string]bool{}
	var allowedModels []string
	if modelLimitsEnabled {
		limits, ok := c.Get("token_model_limit")
		var valid bool
		allowed, valid = limits.(map[string]bool)
		if !ok || !valid {
			return nil, errors.New("API key model limits are invalid")
		}
		allowedModels = seedanceAssetSynchronizationModels(c)
		slices.Sort(allowedModels)
		if modelName != "" {
			permitted := middleware.TokenModelLimitAllows(allowed, modelName)
			for candidate, enabled := range allowed {
				if enabled && matchFamily && strings.HasPrefix(ratio_setting.RoutingMatchModelName(candidate), modelName+"-") {
					permitted = true
					break
				}
			}
			if !permitted {
				return nil, errSeedanceAssetModelNotPermitted
			}
		}
	}
	constraints := service.GetChannelConstraints(c)
	constraints.AddFilter(dto.ChannelFilter{Kind: dto.FilterTaskPluginIdentity, TaskPluginKey: "doubao", TaskPluginKeys: []string{"anyllm", "originocean"}, TaskPluginChannelTypes: []int{constant.ChannelTypeDoubaoVideo}})
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if group == "" {
		group = c.GetString("group")
	}
	if group == "" {
		group = c.GetString("user_group")
	}
	if group == "" {
		group = "default"
	}
	groups := []string{group}
	if group == "auto" {
		groups = service.GetRequestAutoGroups(c, c.GetString("user_group"))
	} else if c.GetInt("token_id") == 0 {
		userGroup := c.GetString("user_group")
		if userGroup == "" {
			userGroup = c.GetString("group")
		}
		for candidate := range service.GetUserUsableGroups(userGroup) {
			if candidate == "" || candidate == "auto" || candidate == group || !ratio_setting.ContainsGroupRatio(candidate) {
				continue
			}
			groups = append(groups, candidate)
		}
	}
	channels, err := service.FindSeedanceAssetChannels()
	if err != nil {
		if errors.Is(err, service.ErrSeedanceAssetChannelUnavailable) {
			return nil, errSeedanceAssetNoChannel
		}
		return nil, err
	}
	pin, pinned, _ := constraints.ResolvedPin()
	filtered := make([]seedanceAssetChannelChoice, 0, len(channels))
	fallback := make([]seedanceAssetChannelChoice, 0, len(channels))
	candidates := make([]seedanceAssetChannelChoice, 0, len(channels))
	for _, channel := range channels {
		if pinned && channel.Id != pin.ChannelId {
			continue
		}
		var selected seedanceAssetChannelChoice
		for _, candidateModel := range channel.GetModels() {
			if modelName != "" && candidateModel != modelName && candidateModel != ratio_setting.RoutingMatchModelName(modelName) && !(matchFamily && strings.HasPrefix(candidateModel, modelName+"-")) {
				continue
			}
			if modelLimitsEnabled && !service.SeedanceAssetModelAllowed(candidateModel, allowedModels) {
				continue
			}
			requestModel := candidateModel
			if modelName != "" && (candidateModel == modelName || candidateModel == ratio_setting.RoutingMatchModelName(modelName)) &&
				(!modelLimitsEnabled || middleware.TokenModelLimitAllows(allowed, modelName)) {
				requestModel = modelName
			} else if modelLimitsEnabled && !middleware.TokenModelLimitAllows(allowed, requestModel) {
				requestModel = ""
				for _, allowedModel := range allowedModels {
					if ratio_setting.RoutingMatchModelName(allowedModel) == candidateModel {
						requestModel = allowedModel
						break
					}
				}
				if requestModel == "" {
					continue
				}
			}
			if ok, _ := model.ChannelSatisfiesFilters(channel, candidateModel, constraints.Filters); !ok {
				continue
			}
			for _, candidateGroup := range groups {
				if candidateGroup == "" || candidateGroup == "auto" {
					continue
				}
				if model.IsChannelEnabledForGroupModel(candidateGroup, candidateModel, channel.Id) {
					if selected.channel == nil || candidateModel == modelName {
						selected = seedanceAssetChannelChoice{channel: channel, model: candidateModel, requestModel: requestModel}
					}
					break
				}
			}
			if selected.channel != nil && (modelName == "" || candidateModel == modelName) {
				break
			}
		}
		if selected.channel != nil {
			candidates = append(candidates, selected)
			if modelName == "" || selected.model == modelName {
				filtered = append(filtered, selected)
			} else {
				fallback = append(fallback, selected)
			}
		}
	}
	if includeFallback {
		filtered = candidates
	} else if len(filtered) == 0 {
		filtered = fallback
	}
	if len(filtered) == 0 {
		return nil, errSeedanceAssetNoChannel
	}
	return filtered, nil
}

// selectSeedanceAssetGroupChannel 在建组时按模型选择上游；已有组始终沿用持久化
// 绑定。API Key 的资源读取按用户共享，但建组不能绕过模型或分组访问限制。
func selectSeedanceAssetGroupChannel(c *gin.Context, modelName string) (*model.Channel, error) {
	if modelName == "" {
		if c.GetInt("token_id") > 0 && c.GetString(seedanceAssetActionContextKey) != "CreateAssetGroup" {
			return nil, errors.New("model is required when creating an asset group with an API key")
		}
		channels, err := selectSeedanceAssetValidationChannels(c, "")
		if err != nil {
			return nil, err
		}
		return channels[0], nil
	}
	if c.GetBool("token_model_limit_enabled") {
		limits, ok := c.Get("token_model_limit")
		allowed, valid := limits.(map[string]bool)
		if !ok || !valid || !middleware.TokenModelLimitAllows(allowed, modelName) {
			return nil, errors.New("API key does not allow this model")
		}
	}
	constraints := service.GetChannelConstraints(c)
	// 用素材插件身份约束模型，允许管理员配置的模型别名。
	constraints.AddFilter(dto.ChannelFilter{Kind: dto.FilterTaskPluginIdentity, TaskPluginKey: "doubao", TaskPluginKeys: []string{"anyllm", "originocean"}, TaskPluginChannelTypes: []int{constant.ChannelTypeDoubaoVideo}})
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	if group == "" {
		group = c.GetString("group")
	}
	if pin, found, _ := constraints.ResolvedPin(); found {
		channel, err := model.CacheGetChannel(pin.ChannelId)
		if err != nil {
			return nil, errors.New("selected asset channel is unavailable")
		}
		groups := []string{group}
		if group == "auto" {
			groups = service.GetRequestAutoGroups(c, c.GetString("user_group"))
		}
		for _, candidate := range groups {
			if ok, _ := model.ChannelSatisfiesFilters(channel, modelName, constraints.Filters); ok && model.IsChannelEnabledForGroupModel(candidate, modelName, channel.Id) {
				return channel, nil
			}
		}
		return nil, errors.New("selected asset channel does not serve this model in an allowed group")
	}
	channel, _, err := service.CacheGetRandomSatisfiedChannel(&service.RetryParam{Ctx: c, ModelName: modelName, TokenGroup: group})
	if err == nil && channel == nil && ratio_setting.RoutingMatchModelName(modelName) != modelName {
		choices, choiceErr := selectSeedanceAssetValidationChoices(c, modelName, false)
		if choiceErr == nil && len(choices) > 0 {
			return choices[0].channel, nil
		}
	}
	if err != nil || channel == nil {
		return nil, errors.New("no available Seedance asset channel for this model and group")
	}
	return channel, nil
}
