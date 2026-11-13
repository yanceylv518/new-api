package middleware

import (
	"cmp"
	"fmt"
	"io"
	"mime"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

func validateTaskAliasModelFields(request jsplugin.RouteRequestContext) error {
	if len(request.BodyText) == 0 {
		return nil
	}
	for _, field := range []string{"model", "model_name"} {
		if countTopLevelJSONKey(request.BodyText, field) > 1 {
			return fmt.Errorf("%s must be provided once", field)
		}
	}
	return nil
}

func taskPluginAliasesForRequest(c *gin.Context, generation *jsplugin.RoutingGeneration, name string) []model.TaskChannelAlias {
	if generation == nil || name == "" {
		return nil
	}
	if _, declared := generation.CanonicalModel(name); declared {
		return nil
	}
	usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	groups := []string{usingGroup}
	if usingGroup == "auto" {
		groups = service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	return model.ResolveTaskModelAliasesForGroups(generation, name, groups)
}

func taskPluginNativeAliasBindings(c *gin.Context, pinned jsplugin.PinnedRoute, name string) (string, []jsplugin.RouteBinding) {
	targets := taskPluginAliasesForRequest(c, pinned.Generation, name)
	var bindings []jsplugin.RouteBinding
	for _, candidate := range pinned.Bindings() {
		if candidate.Plugin == nil {
			continue
		}
		for _, target := range targets {
			if target.PluginKey != candidate.Plugin.Meta.Key || !slices.Contains(candidate.Plugin.Meta.Models, target.Declared) ||
				(len(candidate.Route.Models) > 0 && !slices.Contains(candidate.Route.Models, target.Declared)) {
				continue
			}
			index := slices.IndexFunc(bindings, func(binding jsplugin.RouteBinding) bool {
				return binding.Plugin == candidate.Plugin && binding.DeclaredModel == target.Declared
			})
			if index < 0 {
				candidate.DeclaredModel = target.Declared
				candidate.ChannelIDs = []int{target.ChannelID}
				bindings = append(bindings, candidate)
			} else {
				bindings[index].ChannelIDs = append(bindings[index].ChannelIDs, target.ChannelID)
			}
		}
	}
	if len(bindings) == 0 {
		return "", nil
	}
	return targets[0].Alias, bindings
}

func taskPluginProtocolAliasBindings(c *gin.Context, generation *jsplugin.RoutingGeneration, name string) (string, []jsplugin.ProtocolBinding) {
	targets := taskPluginAliasesForRequest(c, generation, name)
	var bindings []jsplugin.ProtocolBinding
	for _, target := range targets {
		for _, candidate := range generation.LookupEndpointCandidates(c.Request.Method, c.Request.URL.Path, target.Declared) {
			if candidate.Plugin == nil || candidate.Plugin.Meta.Key != target.PluginKey {
				continue
			}
			index := slices.IndexFunc(bindings, func(binding jsplugin.ProtocolBinding) bool {
				return binding.Plugin == candidate.Plugin && binding.Model == target.Declared
			})
			if index < 0 {
				candidate.ChannelIDs = []int{target.ChannelID}
				bindings = append(bindings, candidate)
			} else {
				bindings[index].ChannelIDs = append(bindings[index].ChannelIDs, target.ChannelID)
			}
		}
	}
	if len(bindings) == 0 {
		return "", nil
	}
	slices.SortStableFunc(bindings, func(left, right jsplugin.ProtocolBinding) int {
		if order := cmp.Compare(left.Plugin.Meta.Key, right.Plugin.Meta.Key); order != 0 {
			return order
		}
		return cmp.Compare(left.Model, right.Model)
	})
	return targets[0].Alias, bindings
}

// rewriteTaskPluginJSONModel patches the top-level JSON "model" field and
// replaces BodyStorage. Non-JSON bodies are left untouched.
func rewriteTaskPluginJSONModel(c *gin.Context, spelling string) error {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		return nil
	}
	if mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return nil
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return err
	}
	raw, err := storage.Bytes()
	if err != nil {
		return err
	}
	patched, err := sjson.SetBytes(raw, "model", spelling)
	if err != nil {
		return err
	}
	newStorage, err := common.CreateBodyStorage(patched)
	if err != nil {
		return err
	}
	_ = storage.Close()
	c.Set(common.KeyBodyStorage, newStorage)
	c.Set(common.KeyRequestBody, nil)
	c.Request.Body = io.NopCloser(newStorage)
	c.Request.ContentLength = int64(len(patched))
	return nil
}
