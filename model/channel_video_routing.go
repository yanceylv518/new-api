package model

import (
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
)

func (channel *Channel) matchesVideoRouting(publicModel string, filter dto.ChannelFilter) bool {
	settings := channel.GetSetting()
	if channel.Type != constant.ChannelTypeDoubaoVideo &&
		!((channel.Type == constant.ChannelTypeTaskPlugin || channel.Type == constant.ChannelTypeNewAPI) &&
			(settings.BindsTaskPlugin("doubao") || settings.BindsTaskPlugin("anyllm") || settings.BindsTaskPlugin("originocean"))) {
		return true
	}
	if len(settings.VideoRoutingRules) == 0 {
		return true
	}
	if err := settings.ValidateVideoRoutingRules(); err != nil {
		return false
	}
	logicalModel := filter.VideoRoutingModel
	if target := filter.VideoRoutingChannelModels[channel.Id]; target != "" {
		logicalModel = target
	}
	// 规则只作用于视频，避免同一插件内的 Seedream 图片请求被筛掉。
	if !strings.HasPrefix(strings.ToLower(logicalModel), "doubao-seedance-") {
		return true
	}
	// 显式别名规则优先，未配置别名时继承该渠道对应原模型的路由条件。
	for _, name := range []string{publicModel, logicalModel} {
		for _, rule := range settings.VideoRoutingRules {
			if !strings.EqualFold(rule.Model, name) {
				continue
			}
			matched := filter.VideoRoutingResolution != "" && slices.Contains(rule.Resolutions, filter.VideoRoutingResolution)
			if rule.Mode == "allow" {
				return matched
			}
			return !matched
		}
	}
	return true
}
