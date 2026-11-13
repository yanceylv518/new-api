package middleware

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func prepareVideoRouting(c *gin.Context, publicModel string) error {
	pinned, ok := common.GetContextKeyType[jsplugin.PinnedPlugin](c, jsplugin.ContextKeyPinnedPlugin)
	if !ok || pinned.Generation == nil {
		return nil
	}
	// 共享别名可能被其他插件接受；只对本次解码通过的 Seedance 候选启用规则。
	supported := pinned.Plugin != nil && (pinned.Plugin.Meta.Key == "doubao" || pinned.Plugin.Meta.Key == "anyllm" || pinned.Plugin.Meta.Key == "originocean")
	if endpoint, exists := common.GetContextKeyType[jsplugin.PinnedEndpoint](c, jsplugin.ContextKeyPinnedEndpoint); exists && len(endpoint.Candidates) > 0 {
		supported = false
		for _, candidate := range endpoint.Candidates {
			if candidate.Plugin != nil && (candidate.Plugin.Meta.Key == "doubao" || candidate.Plugin.Meta.Key == "anyllm" || candidate.Plugin.Meta.Key == "originocean") {
				supported = true
				break
			}
		}
	}
	if route, exists := common.GetContextKeyType[jsplugin.PinnedRoute](c, jsplugin.ContextKeyPinnedRoute); exists {
		supported = false
		for _, candidate := range route.Bindings() {
			if candidate.Plugin != nil && (candidate.Plugin.Meta.Key == "doubao" || candidate.Plugin.Meta.Key == "anyllm" || candidate.Plugin.Meta.Key == "originocean") {
				supported = true
				break
			}
		}
	}
	if !supported {
		return nil
	}
	logical, _ := pinned.Generation.CanonicalModel(publicModel)
	if strings.EqualFold(logical, "maas-doubao-seedance-2.0") {
		logical = "doubao-seedance-2-0-260128"
	}
	groups := []string{common.GetContextKeyString(c, constant.ContextKeyUsingGroup)}
	if groups[0] == "auto" {
		groups = service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	channelModels := make(map[int]string)
	video := strings.HasPrefix(strings.ToLower(logical), "doubao-seedance-")
	for _, target := range model.ResolveTaskModelAliasesForGroups(pinned.Generation, publicModel, groups) {
		if target.PluginKey == "originocean" && strings.EqualFold(target.Declared, "maas-doubao-seedance-2.0") {
			channelModels[target.ChannelID] = "doubao-seedance-2-0-260128"
			video = true
			continue
		}
		if (target.PluginKey == "doubao" || target.PluginKey == "anyllm" || target.PluginKey == "originocean") && strings.HasPrefix(strings.ToLower(target.Declared), "doubao-seedance-") {
			channelModels[target.ChannelID] = target.Declared
			video = true
		}
	}
	if !video {
		return nil
	}
	request, exists := common.GetContextKeyType[jsplugin.RouteRequestContext](c, jsplugin.ContextKeyRouteRequest)
	if !exists {
		return nil
	}
	body, valid := request.Body.(map[string]any)
	if !valid {
		return fmt.Errorf("video request body is unavailable")
	}
	var fields map[string]any
	switch body["kind"] {
	case string(jsplugin.BodyJSON):
		fields, valid = body["value"].(map[string]any)
		if !valid {
			return fmt.Errorf("video request body must be an object")
		}
	case string(jsplugin.BodyMultipart), string(jsplugin.BodyForm):
		values, valid := body["fields"].(map[string][]string)
		if !valid {
			return fmt.Errorf("video request fields are unavailable")
		}
		fields = make(map[string]any)
		for _, key := range []string{"resolution", "size", "metadata"} {
			if value, exists := values[key]; exists {
				if len(value) != 1 {
					return fmt.Errorf("%s must be provided once", key)
				}
				fields[key] = value[0]
			}
		}
		if raw, exists := fields["metadata"].(string); exists {
			var metadata map[string]any
			if err := common.UnmarshalJsonStr(raw, &metadata); err != nil || metadata == nil {
				return fmt.Errorf("metadata must be a JSON object")
			}
			fields["metadata"] = metadata
		}
	default:
		return nil
	}
	metadata, _ := fields["metadata"].(map[string]any)
	var resolution any
	for _, key := range []string{"resolution", "size"} {
		if value, exists := fields[key]; exists {
			resolution = value
			break
		}
		if value, exists := metadata[key]; exists {
			resolution = value
			break
		}
	}
	// Responses 会用 size 补齐空的 metadata.resolution，沿用插件解码结果，避免误选普通渠道。
	if endpoint, exists := common.GetContextKeyType[jsplugin.PinnedEndpoint](c, jsplugin.ContextKeyPinnedEndpoint); exists && endpoint.Protocol == "openai_responses" {
		if _, explicit := fields["resolution"]; !explicit {
			prepared, _ := request.RequestBody.(map[string]any)
			options, _ := prepared["metadata"].(map[string]any)
			if value, present := options["resolution"]; present {
				resolution = value
			}
		}
	}
	normalized, err := videoRoutingResolution(resolution)
	if err != nil {
		return err
	}
	service.GetChannelConstraints(c).AddFilter(dto.ChannelFilter{
		Kind: dto.FilterVideoRouting, VideoRoutingResolution: normalized,
		VideoRoutingModel: logical, VideoRoutingChannelModels: channelModels,
	})
	return nil
}

func videoRoutingResolution(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("resolution or size must be a string")
	}
	text = strings.ToLower(strings.TrimSpace(text))
	switch text {
	case "", "480p", "720p", "1080p", "4k":
		return text, nil
	}
	width, height, found := strings.Cut(strings.ReplaceAll(text, "*", "x"), "x")
	if found {
		w, widthErr := strconv.ParseUint(width, 10, 32)
		h, heightErr := strconv.ParseUint(height, 10, 32)
		if widthErr == nil && heightErr == nil && w > 0 && h > 0 {
			switch largest := max(w, h); {
			case largest >= 3840:
				return "4k", nil
			case largest >= 1920:
				return "1080p", nil
			case largest >= 1280:
				return "720p", nil
			default:
				return "480p", nil
			}
		}
	}
	return "", fmt.Errorf("unsupported video resolution")
}
