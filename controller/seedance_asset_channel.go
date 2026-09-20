package controller

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// selectSeedanceAssetGroupChannel 在建组时按模型选择上游；已有组始终沿用持久化
// 绑定。API Key 的资源读取按用户共享，但建组不能绕过模型或分组访问限制。
func selectSeedanceAssetGroupChannel(c *gin.Context, modelName string) (*model.Channel, error) {
	if modelName == "" {
		if c.GetInt("token_id") > 0 {
			return nil, errors.New("model is required when creating an asset group with an API key")
		}
		return service.FindSeedanceAssetChannel()
	}
	if c.GetBool("token_model_limit_enabled") {
		limits, ok := c.Get("token_model_limit")
		allowed, valid := limits.(map[string]bool)
		if !ok || !valid || !allowed[modelName] {
			return nil, errors.New("API key does not allow this model")
		}
	}
	constraints := service.GetChannelConstraints(c)
	// 用渠道的 Doubao 身份约束模型，允许管理员配置的模型别名。
	constraints.AddFilter(dto.ChannelFilter{Kind: dto.FilterTaskPluginIdentity, TaskPluginKey: "doubao", TaskPluginChannelTypes: []int{constant.ChannelTypeDoubaoVideo}})
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
	if err != nil || channel == nil {
		return nil, errors.New("no available Seedance asset channel for this model and group")
	}
	return channel, nil
}
