package middleware

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type ModelRequest struct {
	Model string `json:"model"`
	Group string `json:"group,omitempty"`
}

const seedanceAssetMappingRetryAfterSeconds = "5"

func newSeedanceAssetMappingError(c *gin.Context, err error) *types.NewAPIError {
	statusCode := http.StatusBadGateway
	code := types.ErrorCode("private_asset_mapping_failed")
	if errors.Is(err, service.ErrSeedanceAssetMappingPending) {
		statusCode = http.StatusConflict
		code = types.ErrorCode("private_asset_mapping_pending")
		c.Header("Retry-After", seedanceAssetMappingRetryAfterSeconds)
	} else if errors.Is(err, errSeedanceAssetAccountUnavailable) {
		statusCode = http.StatusConflict
		code = types.ErrorCode("private_asset_account_unavailable")
	} else if errors.Is(err, errSeedanceAssetChannelOutsideGroup) {
		statusCode = http.StatusForbidden
		code = types.ErrorCode("private_asset_channel_unavailable")
	} else if errors.Is(err, errSeedanceAssetSynchronizationFailed) {
		statusCode = http.StatusServiceUnavailable
		code = types.ErrorCode("private_asset_sync_failed")
	} else if errors.Is(err, service.ErrSeedanceAssetServiceTemporary) {
		statusCode = http.StatusServiceUnavailable
		code = types.ErrorCode("private_asset_service_unavailable")
		c.Header("Retry-After", seedanceAssetMappingRetryAfterSeconds)
	} else if errors.Is(err, ErrInvalidSeedanceAssetRequest) || errors.Is(err, model.ErrSeedanceAssetUnavailable) {
		statusCode = http.StatusBadRequest
		code = types.ErrorCode("private_asset_invalid")
	}
	return types.NewError(err, code, types.ErrOptionWithStatusCode(statusCode), types.ErrOptionWithSkipRetry())
}

func Distribute() func(c *gin.Context) {
	return func(c *gin.Context) {
		var channel *model.Channel
		defer func() {
			if c.Writer.Status() >= 400 {
				service.RecordRequestPolicyTermination(c, types.NewErrorWithStatusCode(errors.New("request rejected"), types.ErrorCodeInvalidRequest, c.Writer.Status(), types.ErrOptionWithSkipRetry()))
			}
		}()
		constraints := service.GetChannelConstraints(c)
		constraints.AddFilter(taskdto.ChannelFilter{
			Kind:        taskdto.FilterRequestPath,
			RequestPath: c.Request.URL.Path,
		})
		service.AppendTaskPluginIdentityFilter(c, c.GetString("expected_task_plugin_key"))
		modelRequest, shouldSelectChannel, err := getModelRequest(c)
		if err != nil {
			abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidRequest, map[string]any{"Error": err.Error()}))
			return
		}
		pin, pinned, _ := constraints.ResolvedPin()
		if !pinned || pin.Source == taskdto.PinSourceOriginTask {
			if message := tokenModelLimitDenial(c, modelRequest.Model); message != "" {
				abortWithOpenAiMessage(c, http.StatusForbidden, message)
				return
			}
		}
		if !pinned {
			// Select a channel for the user
			if shouldSelectChannel {
				if modelRequest.Model == "" {
					abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorModelNameRequired))
					return
				}
				// check path is /pg/chat/completions
				if strings.HasPrefix(c.Request.URL.Path, "/pg/chat/completions") {
					usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
					playgroundRequest := &dto.PlayGroundRequest{}
					err = common.UnmarshalBodyReusable(c, playgroundRequest)
					if err != nil {
						abortWithOpenAiMessage(c, http.StatusBadRequest, i18n.T(c, i18n.MsgDistributorInvalidPlayground, map[string]any{"Error": err.Error()}))
						return
					}
					if playgroundRequest.Group != "" {
						if !service.GroupInUserUsableGroups(usingGroup, playgroundRequest.Group) && playgroundRequest.Group != usingGroup {
							abortWithOpenAiMessage(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorGroupAccessDenied))
							return
						}
						common.SetContextKey(c, constant.ContextKeyUsingGroup, playgroundRequest.Group)
					}
				}
			}
		}
		if pinned || shouldSelectChannel {
			if err := prepareVideoRouting(c, modelRequest.Model); err != nil {
				abortWithOpenAiMessage(c, http.StatusBadRequest, err.Error(), types.ErrorCodeInvalidRequest)
				return
			}
			if routingErr := prepareSeedanceAssetRouting(c, modelRequest.Model); routingErr != nil {
				assetErr := newSeedanceAssetMappingError(c, routingErr)
				abortWithOpenAiMessage(c, assetErr.StatusCode, assetErr.Error(), assetErr.GetErrorCode())
				return
			}
			usingGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
			var selectErr *service.ChannelSelectError
			channel, _, selectErr = service.SelectChannelForRequest(c, modelRequest.Model, &service.RetryParam{
				Ctx:         c,
				ModelName:   modelRequest.Model,
				TokenGroup:  usingGroup,
				RequestPath: c.Request.URL.Path,
				Retry:       common.GetPointer(0),
			})
			if selectErr != nil {
				if scope, ok := c.Get(seedanceReadyScopeKey); ok && (selectErr.NoAvailableChannel || selectErr.FilterKind == taskdto.FilterSeedanceAssetReady) {
					if ready, valid := scope.(*seedanceReadyScope); valid {
						mappingErr := error(service.ErrSeedanceAssetMappingPending)
						if !ready.pending && ready.failed {
							mappingErr = errSeedanceAssetSynchronizationFailed
						}
						assetErr := newSeedanceAssetMappingError(c, mappingErr)
						abortWithOpenAiMessage(c, assetErr.StatusCode, assetErr.Error(), assetErr.GetErrorCode())
						return
					}
				}
				if selectErr.FilterKind == taskdto.FilterVideoRouting {
					abortWithOpenAiMessage(c, http.StatusServiceUnavailable, "No available channel supports the requested video resolution")
					return
				}
				if selectErr.NoAvailableChannel || selectErr.FilterKind == taskdto.FilterTaskPluginIdentity {
					if detail := rejectedTaskPluginError(c, modelRequest.Model, usingGroup); detail != "" {
						abortWithOpenAiMessage(c, http.StatusBadRequest, detail, types.ErrorCodeInvalidRequest)
						return
					}
				}
				if (selectErr.NoAvailableChannel || selectErr.FilterKind == taskdto.FilterSeedanceAssetAccount) && common.GetContextKeyString(c, constant.ContextKeySeedanceAssetAccountFingerprint) != "" {
					assetErr := newSeedanceAssetMappingError(c, errSeedanceAssetAccountUnavailable)
					abortWithOpenAiMessage(c, assetErr.StatusCode, assetErr.Error(), assetErr.GetErrorCode())
					return
				}
				if selectErr.FilterKind == taskdto.FilterTaskPluginIdentity {
					logTaskPluginChannelDecision(c, selectErr.Channel, modelRequest.Model, "channel_rejected", "identity_mismatch")
				}
				message := selectErr.Message
				if selectErr.NoAvailableChannel {
					message = noAvailableChannelMessage(c, usingGroup, modelRequest.Model)
				} else if selectErr.MessageID != "" {
					message = i18n.T(c, selectErr.MessageID, selectErr.Params)
				}
				abortWithOpenAiMessage(c, selectErr.StatusCode, message, selectErr.Code)
				return
			}
		}
		common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
		if channel != nil {
			if setupErr := SetupContextForSelectedChannel(c, channel, modelRequest.Model); setupErr != nil {
				statusCode := setupErr.StatusCode
				if statusCode < http.StatusBadRequest {
					statusCode = http.StatusServiceUnavailable
				}
				abortWithOpenAiMessage(c, statusCode, setupErr.Error(), setupErr.GetErrorCode())
				return
			}
		}
		c.Next()
		if channel != nil && c.Writer != nil && c.Writer.Status() < http.StatusBadRequest {
			service.RecordChannelAffinity(c, channel.Id)
		}
	}
}

// 仅在路由失败时检查被拒绝的插件是否有满足其他约束的渠道。
func rejectedTaskPluginError(c *gin.Context, modelName, usingGroup string) string {
	value, exists := c.Get(contextKeyTaskPluginDecodeFailures)
	rejected, ok := value.([]taskPluginDecodeFailure)
	if !exists || !ok || len(rejected) == 0 {
		return ""
	}
	pinnedValue, exists := c.Get(jsplugin.ContextKeyPinnedPlugin)
	pinned, ok := pinnedValue.(jsplugin.PinnedPlugin)
	if !exists || !ok || pinned.Generation == nil {
		return ""
	}
	filter := taskdto.ChannelFilter{Kind: taskdto.FilterTaskPluginIdentity, TaskPluginKey: rejected[0].plugin.Meta.Key, TaskPluginChannelIDs: make(map[int]struct{})}
	restricted := true
	for _, failure := range rejected {
		filter.TaskPluginKeys = append(filter.TaskPluginKeys, failure.plugin.Meta.Key)
		if len(failure.channelIDs) == 0 {
			restricted = false
		} else {
			for _, id := range failure.channelIDs {
				filter.TaskPluginChannelIDs[id] = struct{}{}
			}
		}
		for _, channelType := range failure.plugin.Meta.ChannelTypes {
			if plugin, found := pinned.Generation.GetByChannelType(channelType); found && plugin == failure.plugin {
				filter.TaskPluginChannelTypes = append(filter.TaskPluginChannelTypes, channelType)
			}
		}
	}
	if !restricted {
		filter.TaskPluginChannelIDs = nil
	}
	constraints := service.GetChannelConstraints(c)
	filters := make([]taskdto.ChannelFilter, 0, len(constraints.Filters))
	for _, existing := range constraints.Filters {
		if existing.Kind != taskdto.FilterTaskPluginIdentity {
			filters = append(filters, existing)
		}
	}
	filters = append(filters, filter)
	var channel *model.Channel
	if pin, found, _ := constraints.ResolvedPin(); found {
		candidate, err := model.CacheGetChannel(pin.ChannelId)
		if err == nil && candidate != nil && candidate.Status == common.ChannelStatusEnabled {
			if satisfied, _ := model.ChannelSatisfiesFilters(candidate, modelName, filters); satisfied {
				channel = candidate
			}
		}
	} else {
		groups := []string{usingGroup}
		if usingGroup == "auto" {
			groups = service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
		}
		for _, group := range groups {
			candidate, err := model.GetRandomSatisfiedChannel(group, modelName, 0, filters)
			if err != nil {
				return ""
			}
			if candidate != nil && candidate.Status == common.ChannelStatusEnabled {
				channel = candidate
				break
			}
		}
	}
	if channel == nil {
		return ""
	}
	for _, failure := range rejected {
		if channel.Type == constant.ChannelTypeTaskPlugin || channel.Type == constant.ChannelTypeNewAPI {
			if channel.GetSetting().BindsTaskPlugin(failure.plugin.Meta.Key) {
				return failure.detail
			}
		} else if plugin, found := pinned.Generation.GetByChannelType(channel.Type); found && plugin == failure.plugin {
			return failure.detail
		}
	}
	return ""
}

// noAvailableChannelMessage explains a 503 for a task-plugin-claimed model.
// The response tells the caller the model is plugin-claimed without naming the
// plugin; the candidate plugin keys go to the server log under the request id.
func noAvailableChannelMessage(c *gin.Context, group, modelName string) string {
	value, exists := c.Get(jsplugin.ContextKeyPinnedPlugin)
	pinned, ok := value.(jsplugin.PinnedPlugin)
	if exists && ok && pinned.Plugin != nil {
		keys := []string{pinned.Plugin.Meta.Key}
		if value, exists := c.Get(jsplugin.ContextKeyPinnedEndpoint); exists {
			if endpoint, ok := value.(jsplugin.PinnedEndpoint); ok && len(endpoint.Candidates) > 0 {
				keys = nil
				for _, candidate := range endpoint.Candidates {
					if candidate.Plugin != nil {
						keys = append(keys, candidate.Plugin.Meta.Key)
					}
				}
			}
		}
		logger.LogWarn(c, "task_plugin subsystem=distribution event=no_available_channel group=%q model=%q plugins=%q reason=no_eligible_channel", group, modelName, strings.Join(keys, ","))
		return i18n.T(c, i18n.MsgDistributorNoAvailableChannelTaskPlugin, map[string]any{"Group": group, "Model": modelName})
	}
	return i18n.T(c, i18n.MsgDistributorNoAvailableChannel, map[string]any{"Group": group, "Model": modelName})
}

func channelMatchesExpectedTaskPlugin(c *gin.Context, channel *model.Channel, expected string) bool {
	if channel == nil {
		return false
	}
	if c != nil {
		if _, matched := pinnedEndpointCandidateForChannel(c, channel, expected); matched {
			return true
		}
		if _, matched := pinnedNativeCandidateForChannel(c, channel, expected); matched {
			return true
		}
	}
	if expected == "" {
		return channel.Type != constant.ChannelTypeTaskPlugin
	}
	if channel.Type == constant.ChannelTypeTaskPlugin || channel.Type == constant.ChannelTypeNewAPI {
		return channel.GetSetting().BindsTaskPlugin(expected)
	}

	if c == nil {
		return false
	}
	value, exists := c.Get(jsplugin.ContextKeyPinnedPlugin)
	pinned, ok := value.(jsplugin.PinnedPlugin)
	if !exists || !ok || pinned.Generation == nil || pinned.Plugin == nil || pinned.Plugin.Meta.Key != expected {
		return false
	}
	plugin, ok := pinned.Generation.GetByChannelType(channel.Type)
	return ok && plugin == pinned.Plugin
}

func pinnedEndpointCandidateForChannel(c *gin.Context, channel *model.Channel, expected string) (jsplugin.ProtocolBinding, bool) {
	if c == nil || channel == nil || expected == "" {
		return jsplugin.ProtocolBinding{}, false
	}
	value, exists := c.Get(jsplugin.ContextKeyPinnedEndpoint)
	pinned, ok := value.(jsplugin.PinnedEndpoint)
	if !exists || !ok || pinned.Generation == nil || pinned.Plugin == nil {
		return jsplugin.ProtocolBinding{}, false
	}
	candidates := pinned.Candidates
	if len(candidates) == 0 {
		candidates = []jsplugin.ProtocolBinding{{Plugin: pinned.Plugin, Protocol: pinned.Protocol, Operation: pinned.Operation, Model: pinned.Model}}
	}
	expectedOwned := false
	selected := jsplugin.ProtocolBinding{}
	setting := channel.GetSetting()
	for _, candidate := range candidates {
		if candidate.Plugin == nil {
			continue
		}
		if candidate.Plugin.Meta.Key == expected {
			expectedOwned = true
		}
		if len(candidate.ChannelIDs) > 0 && !slices.Contains(candidate.ChannelIDs, channel.Id) {
			continue
		}
		if channel.Type == constant.ChannelTypeTaskPlugin || channel.Type == constant.ChannelTypeNewAPI {
			// A New API channel may bind several candidates. The first bound
			// candidate in generation order executes, so the billing provider
			// depends only on the channel and the request, never on which
			// channels an earlier retry attempt happened to try.
			if selected.Plugin == nil && setting.BindsTaskPlugin(candidate.Plugin.Meta.Key) {
				selected = candidate
			}
			continue
		}
		plugin, indexed := pinned.Generation.GetByChannelType(channel.Type)
		if indexed && plugin == candidate.Plugin {
			selected = candidate
		}
	}
	return selected, expectedOwned && selected.Plugin != nil
}

func pinnedNativeCandidateForChannel(c *gin.Context, channel *model.Channel, expected string) (jsplugin.RouteBinding, bool) {
	value, exists := c.Get(jsplugin.ContextKeyPinnedRoute)
	pinned, ok := value.(jsplugin.PinnedRoute)
	if !exists || !ok || pinned.Generation == nil || len(pinned.Candidates) == 0 {
		return jsplugin.RouteBinding{}, false
	}
	var selected jsplugin.RouteBinding
	expectedOwned := false
	for _, candidate := range pinned.Candidates {
		if candidate.Plugin == nil {
			continue
		}
		expectedOwned = expectedOwned || candidate.Plugin.Meta.Key == expected
		if len(candidate.ChannelIDs) > 0 && !slices.Contains(candidate.ChannelIDs, channel.Id) {
			continue
		}
		if channel.Type == constant.ChannelTypeTaskPlugin || channel.Type == constant.ChannelTypeNewAPI {
			if selected.Plugin == nil && channel.GetSetting().BindsTaskPlugin(candidate.Plugin.Meta.Key) {
				selected = candidate
			}
		} else if plugin, found := pinned.Generation.GetByChannelType(channel.Type); found && plugin == candidate.Plugin {
			selected = candidate
		}
	}
	return selected, expectedOwned && selected.Plugin != nil
}

// getModelFromRequest 从请求中读取模型信息
// 根据 Content-Type 自动处理：
// - application/json
// - application/x-www-form-urlencoded
// - multipart/form-data
func getModelFromRequest(c *gin.Context) (*ModelRequest, error) {
	if cached, exists := c.Get(contextKeyTaskPluginEndpointModel); exists {
		if modelRequest, ok := cached.(ModelRequest); ok {
			cachedRequest := modelRequest
			return &cachedRequest, nil
		}
	}
	if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "application/json") {
		modelRequest, err := getModelFromJSONBody(c)
		if err != nil {
			return nil, errors.New(i18n.T(c, i18n.MsgDistributorInvalidRequest, map[string]any{"Error": err.Error()}))
		}
		return modelRequest, nil
	}

	var modelRequest ModelRequest
	err := common.UnmarshalBodyReusable(c, &modelRequest)
	if err != nil {
		return nil, errors.New(i18n.T(c, i18n.MsgDistributorInvalidRequest, map[string]any{"Error": err.Error()}))
	}
	return &modelRequest, nil
}

func getModelFromJSONBody(c *gin.Context) (*ModelRequest, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	requestBody, err := storage.Bytes()
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(requestBody) {
		return nil, errors.New("invalid JSON request body")
	}
	if countTopLevelJSONKey(requestBody, "model") > 1 {
		return nil, errors.New("model must be provided once")
	}

	values := gjson.GetManyBytes(requestBody, "model", "group")
	model, err := getJSONStringValue(values[0], "model")
	if err != nil {
		return nil, err
	}
	group, err := getJSONStringValue(values[1], "group")
	if err != nil {
		return nil, err
	}

	if _, seekErr := storage.Seek(0, io.SeekStart); seekErr != nil {
		return nil, seekErr
	}
	c.Request.Body = io.NopCloser(storage)

	return &ModelRequest{
		Model: model,
		Group: group,
	}, nil
}

func countTopLevelJSONKey(data []byte, target string) int {
	depth := 0
	inString := false
	escaped := false
	stringStart := 0
	expectingKey := false
	count := 0
	for index, current := range data {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' {
				escaped = true
				continue
			}
			if current != '"' {
				continue
			}
			inString = false
			if depth == 1 && expectingKey {
				key := string(data[stringStart:index])
				var decodedKey string
				if common.Unmarshal(data[stringStart-1:index+1], &decodedKey) == nil {
					key = decodedKey
				}
				cursor := index + 1
				for cursor < len(data) && (data[cursor] == ' ' || data[cursor] == '\t' || data[cursor] == '\r' || data[cursor] == '\n') {
					cursor++
				}
				if cursor < len(data) && data[cursor] == ':' && key == target {
					count++
				}
				expectingKey = false
			}
			continue
		}
		switch current {
		case '"':
			inString = true
			stringStart = index + 1
		case '{':
			depth++
			if depth == 1 {
				expectingKey = true
			}
		case '}':
			depth--
		case ',':
			if depth == 1 {
				expectingKey = true
			}
		}
	}
	return count
}

func getJSONStringValue(result gjson.Result, field string) (string, error) {
	if !result.Exists() || result.Type == gjson.Null {
		return "", nil
	}
	if result.Type != gjson.String {
		return "", fmt.Errorf("field %s must be a string", field)
	}
	return result.String(), nil
}

func getModelRequest(c *gin.Context) (*ModelRequest, bool, error) {
	var modelRequest ModelRequest
	shouldSelectChannel := true
	var err error
	if modelName := c.GetString("resolved_task_model"); modelName != "" {
		modelRequest.Model = modelName
	} else if strings.Contains(c.Request.URL.Path, "/mj/") {
		relayMode := relayconstant.Path2RelayModeMidjourney(c.Request.URL.Path)
		if relayMode == relayconstant.RelayModeMidjourneyTaskFetch ||
			relayMode == relayconstant.RelayModeMidjourneyTaskFetchByCondition ||
			relayMode == relayconstant.RelayModeMidjourneyNotify ||
			relayMode == relayconstant.RelayModeMidjourneyTaskImageSeed {
			shouldSelectChannel = false
		} else {
			midjourneyRequest := taskdto.MidjourneyRequest{}
			err = common.UnmarshalBodyReusable(c, &midjourneyRequest)
			if err != nil {
				return nil, false, errors.New(i18n.T(c, i18n.MsgDistributorInvalidMidjourney, map[string]any{"Error": err.Error()}))
			}
			midjourneyModel, mjErr, success := service.GetMjRequestModel(relayMode, &midjourneyRequest)
			if mjErr != nil {
				return nil, false, fmt.Errorf("%s", mjErr.Description)
			}
			if midjourneyModel == "" {
				if !success {
					return nil, false, fmt.Errorf("%s", i18n.T(c, i18n.MsgDistributorInvalidParseModel))
				} else {
					// task fetch, task fetch by condition, notify
					shouldSelectChannel = false
				}
			}
			modelRequest.Model = midjourneyModel
		}
		c.Set("relay_mode", relayMode)
	} else if strings.Contains(c.Request.URL.Path, "/v1/videos/") && strings.HasSuffix(c.Request.URL.Path, "/remix") {
		relayMode := relayconstant.RelayModeVideoSubmit
		c.Set("relay_mode", relayMode)
		modelRequest.Model = getTaskOriginModelName(c)
		shouldSelectChannel = false
	} else if strings.Contains(c.Request.URL.Path, "/v1/videos") {
		//curl https://api.openai.com/v1/videos \
		//  -H "Authorization: Bearer $OPENAI_API_KEY" \
		//  -F "model=sora-2" \
		//  -F "prompt=A calico cat playing a piano on stage"
		//	-F input_reference="@image.jpg"
		relayMode := relayconstant.RelayModeUnknown
		if c.Request.Method == http.MethodPost {
			relayMode = relayconstant.RelayModeVideoSubmit
			req, err := getModelFromRequest(c)
			if err != nil {
				return nil, false, err
			}
			if req != nil {
				modelRequest.Model = req.Model
			}
		} else if c.Request.Method == http.MethodGet {
			relayMode = relayconstant.RelayModeVideoFetchByID
			shouldSelectChannel = false
			modelRequest.Model = getTaskOriginModelName(c)
		}
		c.Set("relay_mode", relayMode)
	} else if strings.Contains(c.Request.URL.Path, "/v1/video/generations") {
		relayMode := relayconstant.RelayModeUnknown
		if c.Request.Method == http.MethodPost {
			req, err := getModelFromRequest(c)
			if err != nil {
				return nil, false, err
			}
			modelRequest.Model = req.Model
			relayMode = relayconstant.RelayModeVideoSubmit
		} else if c.Request.Method == http.MethodGet {
			relayMode = relayconstant.RelayModeVideoFetchByID
			shouldSelectChannel = false
			modelRequest.Model = getTaskOriginModelName(c)
		}
		if _, ok := c.Get("relay_mode"); !ok {
			c.Set("relay_mode", relayMode)
		}
	} else if strings.HasPrefix(c.Request.URL.Path, "/v1beta/models/") || strings.HasPrefix(c.Request.URL.Path, "/v1/models/") {
		// Gemini API 路径处理: /v1beta/models/gemini-2.0-flash:generateContent
		relayMode := relayconstant.RelayModeGemini
		modelName := extractModelNameFromGeminiPath(c.Request.URL.Path)
		if modelName != "" {
			modelRequest.Model = modelName
		}
		c.Set("relay_mode", relayMode)
	} else if !strings.HasPrefix(c.Request.URL.Path, "/v1/audio/transcriptions") && !strings.Contains(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
		req, err := getModelFromRequest(c)
		if err != nil {
			return nil, false, err
		}
		modelRequest.Model = req.Model
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/realtime") {
		//wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview-2024-10-01
		modelRequest.Model = c.Query("model")
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/moderations") {
		if modelRequest.Model == "" {
			modelRequest.Model = "text-moderation-stable"
		}
	}
	if strings.HasSuffix(c.Request.URL.Path, "embeddings") {
		if modelRequest.Model == "" {
			modelRequest.Model = c.Param("model")
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/images/generations") {
		modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "dall-e")
	} else if strings.HasPrefix(c.Request.URL.Path, "/v1/images/edits") {
		//modelRequest.Model = common.GetStringIfEmpty(c.PostForm("model"), "gpt-image-1")
		contentType := c.ContentType()
		if slices.Contains([]string{gin.MIMEPOSTForm, gin.MIMEMultipartPOSTForm}, contentType) {
			req, err := getModelFromRequest(c)
			if err == nil && req.Model != "" {
				modelRequest.Model = req.Model
			}
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/audio") {
		relayMode := relayconstant.RelayModeAudioSpeech
		if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/speech") {

			modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "tts-1")
		} else if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/translations") {
			// 先尝试从请求读取
			if req, err := getModelFromRequest(c); err == nil && req.Model != "" {
				modelRequest.Model = req.Model
			}
			modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "whisper-1")
			relayMode = relayconstant.RelayModeAudioTranslation
		} else if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/transcriptions") {
			// 先尝试从请求读取
			if req, err := getModelFromRequest(c); err == nil && req.Model != "" {
				modelRequest.Model = req.Model
			}
			modelRequest.Model = common.GetStringIfEmpty(modelRequest.Model, "whisper-1")
			relayMode = relayconstant.RelayModeAudioTranscription
		}
		c.Set("relay_mode", relayMode)
	}
	if strings.HasPrefix(c.Request.URL.Path, "/pg/chat/completions") {
		// playground chat completions
		req, err := getModelFromRequest(c)
		if err != nil {
			return nil, false, err
		}
		modelRequest.Model = req.Model
		modelRequest.Group = req.Group
		common.SetContextKey(c, constant.ContextKeyTokenGroup, modelRequest.Group)
	}

	return &modelRequest, shouldSelectChannel, nil
}

// TokenModelLimitAllows reports whether a token model-limit map authorizes
// model. Exact name, wildcard-normalized name, and routing-normalized name
// (modifiers and legacy aliases stripped) are all accepted. The Responses
// WebSocket relay shares this rule so both transports admit the same names.
func TokenModelLimitAllows(limit map[string]bool, model string) bool {
	if limit[model] {
		return true
	}
	if formatted := ratio_setting.FormatMatchingModelName(model); limit[formatted] {
		return true
	}
	return limit[ratio_setting.RoutingMatchModelName(model)]
}

// TokenModelLimitDenial 返回令牌模型限制的拒绝原因。
func TokenModelLimitDenial(c *gin.Context, modelName string) string {
	return tokenModelLimitDenial(c, modelName)
}

func tokenModelLimitDenial(c *gin.Context, modelName string) string {
	if !common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		return ""
	}
	value, exists := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
	if !exists {
		return i18n.T(c, i18n.MsgDistributorTokenNoModelAccess)
	}
	limit, ok := value.(map[string]bool)
	if !ok {
		limit = map[string]bool{}
	}
	if !TokenModelLimitAllows(limit, modelName) {
		return i18n.T(c, i18n.MsgDistributorTokenModelForbidden, map[string]any{"Model": modelName})
	}
	return ""
}

// 修复 #4834: GET /v1/video/generations/:task_id && /v1/video/:task_id 此前不解析 model，
// 当 token 启用「可用模型限制」时，下游 modelLimitEnable 校验会因
// modelRequest.Model 为空而误报 "This token has no access to model"。
// 任务查询和 remix 从用户自己的源任务恢复模型名，兼容历史模型字段。
func getTaskOriginModelName(c *gin.Context) string {
	if !common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		return ""
	}

	taskId := c.Param("task_id")
	if taskId == "" {
		taskId = c.Param("video_id")
	}
	if taskId == "" {
		return ""
	}

	userId := c.GetInt("id")
	if task, exist, err := model.GetByTaskId(userId, taskId); err == nil && exist && task != nil {
		if task.Properties.OriginModelName != "" {
			return task.Properties.OriginModelName
		}
		if task.Properties.UpstreamModelName != "" {
			return task.Properties.UpstreamModelName
		}
		var data struct {
			Model string `json:"model"`
		}
		if task.GetData(&data) == nil {
			return data.Model
		}
	}
	return ""
}

func SetupContextForSelectedChannel(c *gin.Context, channel *model.Channel, modelName string) *types.NewAPIError {
	c.Set("original_model", modelName) // for retry
	expectedPlugin := c.GetString("expected_task_plugin_key")
	if channel == nil {
		logTaskPluginChannelDecision(c, nil, modelName, "channel_rejected", "nil_channel")
		return types.NewError(errors.New("channel is nil"), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if err := channel.ValidateConfigurationEncoding(); err != nil {
		return types.NewError(errors.New("selected channel settings are invalid"), types.ErrorCodeGetChannelFailed, types.ErrOptionWithSkipRetry())
	}
	if expectedPlugin != "" && !channelMatchesExpectedTaskPlugin(c, channel, expectedPlugin) {
		logTaskPluginChannelDecision(c, channel, modelName, "channel_rejected", "identity_mismatch")
		return types.NewError(
			errors.New("selected channel does not match the pinned task plugin"),
			types.ErrorCodeGetChannelFailed,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if candidate, matched := pinnedEndpointCandidateForChannel(c, channel, expectedPlugin); matched {
		if value, exists := c.Get(jsplugin.ContextKeyPinnedEndpoint); exists {
			if pinned, ok := value.(jsplugin.PinnedEndpoint); ok && candidate.Plugin != nil && (candidate.Plugin != pinned.Plugin || (len(candidate.ChannelIDs) > 0 && pinned.MappedModel != candidate.Model)) {
				previousPlugin := pinned.Plugin.Meta.Key
				pinned.Plugin = candidate.Plugin
				pinned.Protocol = candidate.Protocol
				pinned.Operation = candidate.Operation
				if len(candidate.ChannelIDs) > 0 {
					pinned.MappedModel = candidate.Model
					if protocolValue, exists := c.Get(jsplugin.ContextKeyProtocolRequest); exists {
						if protocolContext, valid := protocolValue.(jsplugin.ProtocolRequestContext); valid {
							protocolContext.UpstreamModel = candidate.Model
							c.Set(jsplugin.ContextKeyProtocolRequest, protocolContext)
						}
					}
				}
				c.Set(jsplugin.ContextKeyPinnedEndpoint, pinned)
				c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: pinned.Generation, Plugin: candidate.Plugin})
				c.Set("expected_task_plugin_key", candidate.Plugin.Meta.Key)
				c.Set("task_plugin_key", candidate.Plugin.Meta.Key)
				c.Set("platform", candidate.Plugin.Meta.Key)
				logger.LogDebug(
					c,
					"task_plugin subsystem=endpoint event=provider_selected generation=%d previous_plugin=%q plugin=%q model=%q channel_id=%d channel_type=%d",
					pinned.Generation.Number,
					previousPlugin,
					candidate.Plugin.Meta.Key,
					modelName,
					channel.Id,
					channel.Type,
				)
			}
		}
	}
	if candidate, matched := pinnedNativeCandidateForChannel(c, channel, expectedPlugin); matched {
		pinned, _ := common.GetContextKeyType[jsplugin.PinnedRoute](c, jsplugin.ContextKeyPinnedRoute)
		pinned.Plugin, pinned.Route, pinned.DeclaredModel = candidate.Plugin, candidate.Route, candidate.DeclaredModel
		c.Set(jsplugin.ContextKeyPinnedRoute, pinned)
		c.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: pinned.Generation, Plugin: candidate.Plugin})
		c.Set("expected_task_plugin_key", candidate.Plugin.Meta.Key)
		c.Set("task_plugin_key", candidate.Plugin.Meta.Key)
		c.Set("platform", candidate.Plugin.Meta.Key)
	}
	if task, ok := common.GetContextKeyType[*model.Task](c, jsplugin.ContextKeyOriginResourceTask); ok && task != nil {
		resourceChannel, err := model.GetResourceTaskChannel(task, channel)
		if err != nil {
			return types.NewError(err, types.ErrorCodeInvalidRequest, types.ErrOptionWithStatusCode(http.StatusForbidden), types.ErrOptionWithSkipRetry())
		}
		channel = resourceChannel
	}
	common.SetContextKey(c, constant.ContextKeyChannelId, channel.Id)
	common.SetContextKey(c, constant.ContextKeyChannelName, channel.Name)
	common.SetContextKey(c, constant.ContextKeyChannelType, channel.Type)
	common.SetContextKey(c, constant.ContextKeyChannelCreateTime, channel.CreatedTime)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, channel.GetSetting())
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, channel.GetOtherSettings())
	switch channel.Type {
	case constant.ChannelTypeTaskPlugin:
		c.Set("task_plugin_key", channel.GetSetting().TaskPluginKey)
	case constant.ChannelTypeNewAPI:
		// The bound plugin verified above executes; ordinary requests through
		// the same gateway channel carry no pinned plugin.
		if executing := c.GetString("expected_task_plugin_key"); executing != "" {
			c.Set("task_plugin_key", executing)
		}
	}
	logTaskPluginChannelDecision(c, channel, modelName, "channel_selected", "")
	paramOverride := channel.GetParamOverride()
	headerOverride := channel.GetHeaderOverride()
	if mergedParam, applied := service.ApplyChannelAffinityOverrideTemplate(c, paramOverride); applied {
		paramOverride = mergedParam
	}
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, paramOverride)
	common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, headerOverride)
	if nil != channel.OpenAIOrganization && *channel.OpenAIOrganization != "" {
		common.SetContextKey(c, constant.ContextKeyChannelOrganization, *channel.OpenAIOrganization)
	}
	common.SetContextKey(c, constant.ContextKeyChannelAutoBan, channel.GetAutoBan())
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, channel.GetModelMapping())
	common.SetContextKey(c, constant.ContextKeyChannelStatusCodeMapping, channel.GetStatusCodeMapping())

	var key string
	var index int
	var newAPIError *types.NewAPIError
	if fingerprint := common.GetContextKeyString(c, constant.ContextKeySeedanceAssetKeyFingerprint); fingerprint != "" {
		accountFingerprint := common.GetContextKeyString(c, constant.ContextKeySeedanceAssetAccountFingerprint)
		if !model.SeedanceAssetAccountFingerprintMatches(channel, fingerprint, accountFingerprint) {
			return newSeedanceAssetMappingError(c, errSeedanceAssetAccountUnavailable)
		}
		if model.SeedanceAssetSharedAccountMatches(channel, accountFingerprint) {
			key, index, newAPIError = channel.GetNextEnabledKey()
			if newAPIError == nil && strings.TrimSpace(key) == "" {
				for candidateIndex, candidate := range channel.GetKeys() {
					if strings.TrimSpace(candidate) == "" {
						continue
					}
					if status, exists := channel.ChannelInfo.MultiKeyStatusList[candidateIndex]; exists && status != common.ChannelStatusEnabled {
						continue
					}
					key, index = candidate, candidateIndex
					break
				}
				if strings.TrimSpace(key) == "" {
					return newSeedanceAssetMappingError(c, errSeedanceAssetAccountUnavailable)
				}
			}
		} else {
			key, index, newAPIError = channel.GetEnabledKeyByFingerprint(fingerprint)
		}
		if newAPIError != nil {
			return newSeedanceAssetMappingError(c, errSeedanceAssetAccountUnavailable)
		}
	} else {
		key, index, newAPIError = channel.GetNextEnabledKey()
	}
	if newAPIError != nil {
		return newAPIError
	}
	if value, exists := c.Get(seedanceReadyScopeKey); exists {
		if scope, valid := value.(*seedanceReadyScope); valid {
			selectedGroup := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
			if selectedGroup == "auto" {
				selectedGroup = common.GetContextKeyString(c, constant.ContextKeyAutoGroup)
			}
			models := []string{common.GetContextKeyString(c, constant.ContextKeySelectedAbility)}
			if models[0] == "" {
				models[0] = modelName
				if normalized := ratio_setting.RoutingMatchModelName(modelName); normalized != modelName {
					models = append(models, normalized)
				}
			}
			var allowed map[string]struct{}
			scope.selectedModel = ""
			for _, candidate := range models {
				if selectedGroup != "" {
					if _, exists := scope.groups[channel.Id][candidate][selectedGroup]; !exists {
						continue
					}
				}
				if keys := scope.keys[channel.Id][candidate]; len(keys) > 0 {
					allowed = keys
					scope.selectedModel = candidate
					break
				}
			}
			if len(allowed) == 0 {
				return newSeedanceAssetMappingError(c, service.ErrSeedanceAssetMappingPending)
			}
			fingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte(key)))
			if _, found := allowed[fingerprint]; !found {
				key = ""
				for _, candidate := range channel.GetKeys() {
					candidateFingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte(candidate)))
					if _, found := allowed[candidateFingerprint]; !found {
						continue
					}
					selected, selectedIndex, selectedErr := channel.GetEnabledKeyByFingerprint(candidateFingerprint)
					if selectedErr == nil {
						key, index = selected, selectedIndex
						break
					}
				}
				if key == "" {
					return newSeedanceAssetMappingError(c, service.ErrSeedanceAssetMappingPending)
				}
			}
			selectedFingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte(key)))
			expectedAccount := scope.accounts[seedanceReadyIdentity{channel.Id, scope.selectedModel, selectedFingerprint}]
			if expectedAccount == "" || expectedAccount != model.SeedanceAssetModelScopeFingerprint(channel, scope.selectedModel, selectedFingerprint) {
				return newSeedanceAssetMappingError(c, service.ErrSeedanceAssetMappingPending)
			}
		}
	}
	if channel.ChannelInfo.IsMultiKey {
		common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, true)
		common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, index)
	} else {
		// 必须设置为 false，否则在重试到单个 key 的时候会导致日志显示错误
		common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, false)
	}
	// c.Request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", key))
	common.SetContextKey(c, constant.ContextKeyChannelKey, key)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, channel.GetBaseURL())

	common.SetContextKey(c, constant.ContextKeySystemPromptOverride, false)

	// TODO: api_version统一
	switch channel.Type {
	case constant.ChannelTypeAzure:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeVertexAi:
		c.Set("region", channel.Other)
	case constant.ChannelTypeXunfei:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeGemini:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeAli:
		c.Set("plugin", channel.Other)
	case constant.ChannelCloudflare:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeMokaAI:
		c.Set("api_version", channel.Other)
	case constant.ChannelTypeCoze:
		c.Set("bot_id", channel.Other)
	}
	if err := prepareSeedanceAssetRequest(c, channel, key, modelName); err != nil {
		return newSeedanceAssetMappingError(c, err)
	}
	return nil
}

// extractModelNameFromGeminiPath 从 Gemini API URL 路径中提取模型名
// 输入格式: /v1beta/models/gemini-2.0-flash:generateContent
// 输出: gemini-2.0-flash
func extractModelNameFromGeminiPath(path string) string {
	// 查找 "/models/" 的位置
	modelsPrefix := "/models/"
	modelsIndex := strings.Index(path, modelsPrefix)
	if modelsIndex == -1 {
		return ""
	}

	// 从 "/models/" 之后开始提取
	startIndex := modelsIndex + len(modelsPrefix)
	if startIndex >= len(path) {
		return ""
	}

	// 查找 ":" 的位置，模型名在 ":" 之前
	colonIndex := strings.Index(path[startIndex:], ":")
	if colonIndex == -1 {
		// 如果没有找到 ":"，返回从 "/models/" 到路径结尾的部分
		return path[startIndex:]
	}

	// 返回模型名部分
	return path[startIndex : startIndex+colonIndex]
}
