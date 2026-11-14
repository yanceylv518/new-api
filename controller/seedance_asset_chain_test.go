package controller

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeedanceLibraryCreationRespectsGroupBeforePriority(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	const name = "doubao-seedance-2-0-fast-260128"
	base := "https://fixture.example"
	for index, group := range []string{"foreign", "default"} {
		channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "fixture-key", BaseURL: &base, Models: name, Group: group, Priority: common.GetPointer(int64(100 - index))}
		configureSeedanceTestAssetURL(t, channel)
		require.NoError(t, db.Create(channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
	}
	for _, token := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set("group", "default")
		if token {
			c.Set("token_id", 1)
			c.Set("token_model_limit_enabled", true)
			c.Set("token_model_limit", map[string]bool{name: true})
			c.Set(seedanceAssetActionContextKey, "CreateAssetGroup")
		}
		selected, err := selectSeedanceAssetGroupChannel(c, "")
		require.NoError(t, err)
		assert.Equal(t, "default", selected.Group)
		candidates, err := selectSeedanceAssetValidationChannels(c, "")
		require.NoError(t, err)
		require.Len(t, candidates, 1)
		assert.Equal(t, selected.Id, candidates[0].Id)
		c.Set("group", "unavailable")
		selected, err = selectSeedanceAssetGroupChannel(c, "")
		if token {
			assert.Error(t, err)
		} else {
			require.NoError(t, err)
			assert.Equal(t, "default", selected.Group)
		}
	}
}

func TestSeedanceHumanRoutingPinsAccountAndEnabledKey(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	service.InitHttpClient()
	const name = "doubao-seedance-2-0-fast-260128"
	const path = "/doubao/api/v3/contents/generations/tasks"
	registry := pluginruntime.NewRegistry()
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	_, err = registry.RegisterFactory(source, pluginruntime.Options{})
	require.NoError(t, err)
	generation := registry.Generation()
	binding, ok := generation.LookupDeclaredRoute("POST", path)
	require.True(t, ok)
	base := "https://fixture.example"
	setting := `{"task_plugin_key":"doubao","seedance_asset_url":"https://fixture.example/v2/assets"}`
	original := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "verified-key", BaseURL: &base, Setting: &setting, Models: name, Group: "default"}
	require.NoError(t, db.Create(original).Error)
	client, err := service.NewSeedanceAssetClientForSelectedKey(original, "verified-key")
	require.NoError(t, err)
	legacyFingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte("https://fixture.example/v2/assets\x00\x00"+client.KeyFingerprint)))
	group := &model.SeedanceAssetGroup{UserID: 7, ChannelID: original.Id, GroupID: "verified-group", Name: "Verified", GroupType: "LivenessFace", Status: "Active", AccountFingerprint: legacyFingerprint, KeyFingerprint: client.KeyFingerprint}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{UserID: 7, ChannelID: original.Id, GroupID: group.GroupID, AssetID: "verified-portrait", Name: "portrait", AssetType: "Image", Status: "Active", KeyFingerprint: client.KeyFingerprint, AccountFingerprint: legacyFingerprint}
	require.NoError(t, db.Create(asset).Error)
	canonicalGroup := *group
	canonicalGroup.ID, canonicalGroup.GroupID, canonicalGroup.Name, canonicalGroup.AccountFingerprint = 0, "canonical-group", "Canonical", client.AccountFingerprint
	require.NoError(t, db.Create(&canonicalGroup).Error)
	canonicalAsset := *asset
	canonicalAsset.ID, canonicalAsset.GroupID, canonicalAsset.AssetID, canonicalAsset.AccountFingerprint = 0, canonicalGroup.GroupID, "canonical-portrait", client.AccountFingerprint
	require.NoError(t, db.Create(&canonicalAsset).Error)
	// 原渠道删除后，同账号别名仍可使用，不能按更高优先级选到其他账号。
	require.NoError(t, db.Delete(original).Error)
	aliasBase := base + "/doubao"
	alias := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "other-key\nverified-key", BaseURL: &aliasBase, Setting: &setting, Models: name, Group: "default", Priority: common.GetPointer(int64(1)), ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}}
	require.NoError(t, db.Create(alias).Error)
	require.NoError(t, alias.AddAbilities(nil))
	wrong := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "wrong-key", BaseURL: &base, Setting: &setting, Models: name, Group: "default", Priority: common.GetPointer(int64(100))}
	require.NoError(t, db.Create(wrong).Error)
	require.NoError(t, wrong.AddAbilities(nil))
	otherSetting := strings.ReplaceAll(setting, "/v2/assets", "/other/assets")
	otherAccount := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "verified-key", BaseURL: &base, Setting: &otherSetting, Models: name, Group: "default", Priority: common.GetPointer(int64(50))}
	require.NoError(t, db.Create(otherAccount).Error)
	require.NoError(t, otherAccount.AddAbilities(nil))
	otherGroup := *group
	otherGroup.ID, otherGroup.GroupID, otherGroup.Name = 0, "other-group", "Other"
	otherGroup.AccountFingerprint = model.SeedanceAssetAccountFingerprint(otherAccount, client.KeyFingerprint)
	require.NoError(t, db.Create(&otherGroup).Error)
	otherAsset := *asset
	otherAsset.ID, otherAsset.GroupID, otherAsset.AssetID, otherAsset.AccountFingerprint = 0, otherGroup.GroupID, "other-portrait", otherGroup.AccountFingerprint
	require.NoError(t, db.Create(&otherAsset).Error)
	for _, tc := range []struct {
		name     string
		user     int
		group    string
		asset    string
		extra    string
		pin      int
		status   int
		selected int
	}{
		{"same-account-alias", 7, "default", asset.AssetID, "", 0, 200, alias.Id},
		{"mixed-old-and-new-fingerprints", 7, "default", asset.AssetID, canonicalAsset.AssetID, 0, 200, alias.Id},
		{"mixed-accounts", 7, "default", asset.AssetID, otherAsset.AssetID, 0, 409, 0},
		{"foreign-user", 8, "default", asset.AssetID, "", 0, 400, 0},
		{"inaccessible-group", 7, "foreign", asset.AssetID, "", 0, 409, 0},
		{"forced-wrong-account", 7, "default", asset.AssetID, "", wrong.Id, 409, 0},
		{"forced-other-material-service", 7, "default", asset.AssetID, "", otherAccount.Id, 409, 0},
		{"forced-foreign-group", 7, "foreign", asset.AssetID, "", alias.Id, 409, 0},
		{"next-request-is-independent", 7, "default", "", "", 0, 200, wrong.Id},
	} {
		for _, cached := range []bool{false, true} {
			common.MemoryCacheEnabled = cached
			if cached {
				model.InitChannelCache()
			}
			t.Run(fmt.Sprintf("%s/cache=%t", tc.name, cached), func(t *testing.T) {
				require.NoError(t, db.Model(group).Update("account_fingerprint", legacyFingerprint).Error)
				require.NoError(t, db.Model(asset).Update("account_fingerprint", legacyFingerprint).Error)
				router := gin.New()
				router.Use(middleware.BodyStorageCleanup())
				router.POST(path, func(c *gin.Context) {
					c.Set("id", tc.user)
					c.Set("group", tc.group)
					common.SetContextKey(c, constant.ContextKeyUsingGroup, tc.group)
					c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: binding.Plugin, Route: binding.Route})
					if tc.pin > 0 {
						service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: tc.pin, Source: dto.PinSourceToken, Rank: dto.PinRankToken})
					}
				}, middleware.PrepareTaskPluginRoute(), middleware.Distribute(), func(c *gin.Context) {
					assert.Equal(t, tc.selected, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
					if tc.asset != "" {
						assert.Equal(t, "verified-key", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
					}
					c.JSON(200, gin.H{"ok": true})
				})
				body := fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"move slowly"}],"duration":4}`, name)
				if tc.asset != "" {
					body = fmt.Sprintf(`{"model":%q,"content":[{"type":"image_url","role":"reference_image","image_url":{"url":"asset://%s"}}],"duration":4}`, name, tc.asset)
					if tc.extra != "" {
						body = strings.Replace(body, `}],"duration"`, `},{"type":"image_url","role":"reference_image","image_url":{"url":"asset://`+tc.extra+`"}}],"duration"`, 1)
					}
				}
				req := httptest.NewRequest("POST", path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				assert.Equal(t, tc.status, w.Code, w.Body.String())
				if tc.status == http.StatusConflict {
					assert.Contains(t, w.Body.String(), "real-person assets are unavailable")
				}
			})
		}
	}
	common.MemoryCacheEnabled = false
	for _, key := range []string{"verified-key\nother-key", "other-key\nverified-key"} {
		alias.Key = key
		assert.NoError(t, db.Model(alias).Update("key", key).Error)
		filter := dto.ChannelFilter{Kind: dto.FilterSeedanceAssetAccount, SeedanceAssetAccountFingerprint: client.AccountFingerprint, SeedanceAssetKeyFingerprint: client.KeyFingerprint}
		allowed, _ := model.ChannelSatisfiesFilters(alias, name, []dto.ChannelFilter{filter})
		assert.True(t, allowed)
		selected, _, selectionErr := alias.GetEnabledKeyByFingerprint(client.KeyFingerprint)
		require.Nil(t, selectionErr)
		assert.Equal(t, "verified-key", selected)
		assert.Zero(t, alias.ChannelInfo.MultiKeyPollingIndex)
	}
	alias.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
	filter := dto.ChannelFilter{Kind: dto.FilterSeedanceAssetAccount, SeedanceAssetAccountFingerprint: client.AccountFingerprint, SeedanceAssetKeyFingerprint: client.KeyFingerprint}
	allowed, _ := model.ChannelSatisfiesFilters(alias, name, []dto.ChannelFilter{filter})
	assert.False(t, allowed)
}

func TestSeedanceSharedScopeRoutesAndKeepsValidationCredential(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	service.InitHttpClient()
	const modelName = "doubao-seedance-2-0-260128"
	const path = "/doubao/api/v3/contents/generations/tasks"
	const sourceKey, targetKey = "fixture-source-key", "fixture-target-key"
	var validationQueries atomic.Int32
	var copiedGroups, copiedAssets atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/contents/generations/tasks" {
			assert.Equal(t, "Bearer "+targetKey, r.Header.Get("Authorization"))
			var body struct {
				Content []struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"content"`
			}
			require.NoError(t, common.DecodeJson(r.Body, &body))
			require.Len(t, body.Content, 2)
			assert.Equal(t, "asset://human-a", body.Content[0].ImageURL.URL)
			assert.Equal(t, "asset://human-b", body.Content[1].ImageURL.URL)
			_, _ = io.WriteString(w, `{"id":"upstream-shared-task"}`)
			return
		}
		switch r.URL.Query().Get("Action") {
		case "CreateVisualValidateSession":
			assert.Equal(t, "Bearer "+sourceKey, r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"Result":{"BytedToken":"fixture-token","H5Link":"https://example.com/verify"}}`)
		case "GetVisualValidateResult":
			validationQueries.Add(1)
			assert.Equal(t, "Bearer "+sourceKey, r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"Result":{"GroupId":"validated-session-group"}}`)
		case "GetAsset":
			assert.Contains(t, []string{"Bearer " + targetKey, "Bearer fixture-wrong-key"}, r.Header.Get("Authorization"))
			_, _ = io.WriteString(w, `{"Result":{"Status":"Active"}}`)
		case "CreateAssetGroup":
			assert.Equal(t, "Bearer fixture-wrong-key", r.Header.Get("Authorization"))
			copiedGroups.Add(1)
			_, _ = io.WriteString(w, `{"Result":{"Id":"copied-aigc-group"}}`)
		case "CreateAsset":
			assert.Equal(t, "Bearer fixture-wrong-key", r.Header.Get("Authorization"))
			copiedAssets.Add(1)
			_, _ = io.WriteString(w, `{"Result":{"Id":"copied-aigc-asset"}}`)
		default:
			t.Errorf("unexpected asset action %q", r.URL.Query().Get("Action"))
		}
	}))
	defer upstream.Close()
	setting := `{"task_plugin_key":"doubao","seedance_asset_url":"` + upstream.URL + `","seedance_asset_share_group":"shared-fixture"}`
	otherSetting := strings.Replace(setting, "shared-fixture", "different-fixture", 1)
	makeChannel := func(key, settings, group string, priority int64) *model.Channel {
		channel := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: key,
			BaseURL: &upstream.URL, Setting: &settings, Models: modelName, Group: group, Priority: &priority}
		require.NoError(t, db.Create(channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
		return channel
	}
	source := makeChannel(sourceKey, setting, "default", 1)
	target := makeChannel(sourceKey+"\n"+targetKey, setting, "default", 10)
	target.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModeRandom, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}}
	require.NoError(t, db.Model(target).Update("channel_info", target.ChannelInfo).Error)
	wrongScope := makeChannel("fixture-wrong-key", otherSetting, "default", 100)
	foreignGroup := makeChannel("fixture-foreign-key", setting, "foreign", 200)
	sourceClient, err := service.NewSeedanceAssetClientForSelectedKey(source, sourceKey)
	require.NoError(t, err)
	targetClient, err := service.NewSeedanceAssetClientForSelectedKey(target, targetKey)
	require.NoError(t, err)
	assert.NotEqual(t, sourceClient.KeyFingerprint, targetClient.KeyFingerprint)
	assert.Equal(t, sourceClient.AccountFingerprint, targetClient.AccountFingerprint)
	assert.False(t, model.SeedanceAssetSharedAccountMatches(wrongScope, sourceClient.AccountFingerprint))
	ungrouped := *target
	ungroupedSetting := strings.Replace(setting, `,"seedance_asset_share_group":"shared-fixture"`, "", 1)
	ungrouped.Setting = &ungroupedSetting
	assert.False(t, model.SeedanceAssetSharedAccountMatches(&ungrouped, sourceClient.AccountFingerprint))
	assert.NotEqual(t, sourceClient.AccountFingerprint, model.SeedanceAssetAccountFingerprint(&ungrouped, sourceClient.KeyFingerprint))
	otherURL := *target
	otherURLSetting := strings.Replace(setting, upstream.URL, upstream.URL+"/other", 1)
	otherURL.Setting = &otherURLSetting
	assert.False(t, model.SeedanceAssetSharedAccountMatches(&otherURL, sourceClient.AccountFingerprint))
	otherProtocol := *target
	otherProtocolSetting := strings.Replace(setting, `"doubao"`, `"anyllm"`, 1)
	otherProtocol.Setting = &otherProtocolSetting
	assert.False(t, model.SeedanceAssetSharedAccountMatches(&otherProtocol, sourceClient.AccountFingerprint))

	started, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, source, service.SeedanceAssetValidationGroupDetails{Name: "Shared session"}, "", "https://example.com/callback")
	require.NoError(t, err)
	require.NoError(t, db.Model(source).Update("status", common.ChannelStatusManuallyDisabled).Error)
	pending, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusPending, pending.Status)
	assert.Zero(t, validationQueries.Load(), "an unfinished session must not use the other key")
	require.NoError(t, db.Model(source).Update("status", common.ChannelStatusEnabled).Error)
	completed, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusSucceeded, completed.Status)
	assert.EqualValues(t, 1, validationQueries.Load())
	require.NoError(t, db.Delete(source).Error)
	resolved, err := service.ResolveSeedanceAssetClient(t.Context(), source.Id, sourceClient.KeyFingerprint, sourceClient.AccountFingerprint)
	require.NoError(t, err)
	assert.Equal(t, target.Id, resolved.ChannelID)
	status, _, err := resolved.GetSeedanceAsset(t.Context(), "verified-image")
	require.NoError(t, err)
	assert.Equal(t, "Active", status)

	groups := []*model.SeedanceAssetGroup{
		{UserID: 7, ChannelID: source.Id, GroupID: "shared-human-a", Name: "Human A", GroupType: "LivenessFace", Status: "Active", AccountFingerprint: sourceClient.AccountFingerprint, KeyFingerprint: sourceClient.KeyFingerprint},
		{UserID: 7, ChannelID: target.Id, GroupID: "shared-human-b", Name: "Human B", GroupType: "LivenessFace", Status: "Active", AccountFingerprint: targetClient.AccountFingerprint, KeyFingerprint: targetClient.KeyFingerprint},
		{UserID: 7, ChannelID: source.Id, GroupID: "shared-aigc", Name: "AIGC", GroupType: "AIGC", Status: "Active", AccountFingerprint: sourceClient.AccountFingerprint, KeyFingerprint: sourceClient.KeyFingerprint},
	}
	assets := []*model.SeedanceAsset{
		{UserID: 7, ChannelID: source.Id, GroupID: groups[0].GroupID, AssetID: "human-a", Name: "Human A", AssetType: "Image", Status: "Active", AccountFingerprint: sourceClient.AccountFingerprint, KeyFingerprint: sourceClient.KeyFingerprint},
		{UserID: 7, ChannelID: target.Id, GroupID: groups[1].GroupID, AssetID: "human-b", Name: "Human B", AssetType: "Image", Status: "Active", AccountFingerprint: targetClient.AccountFingerprint, KeyFingerprint: targetClient.KeyFingerprint},
		{UserID: 7, ChannelID: source.Id, GroupID: groups[2].GroupID, AssetID: "aigc-a", Name: "AIGC", AssetType: "Image", SourceURL: "https://8.8.8.8/reference.jpg", Status: "Active", AccountFingerprint: sourceClient.AccountFingerprint, KeyFingerprint: sourceClient.KeyFingerprint},
	}
	for _, group := range groups {
		require.NoError(t, db.Create(group).Error)
	}
	for _, asset := range assets {
		require.NoError(t, db.Create(asset).Error)
	}
	mapped, err := service.MapSeedanceAssetsToAccount(t.Context(), 7, targetClient, []string{"aigc-a"})
	require.NoError(t, err)
	assert.Equal(t, "aigc-a", mapped["aigc-a"])
	var storedGroup model.SeedanceAssetGroup
	var storedAsset model.SeedanceAsset
	require.NoError(t, db.First(&storedGroup, groups[2].ID).Error)
	require.NoError(t, db.First(&storedAsset, assets[2].ID).Error)
	assert.Equal(t, source.Id, storedGroup.ChannelID)
	assert.Equal(t, sourceClient.KeyFingerprint, storedGroup.KeyFingerprint)
	assert.Equal(t, source.Id, storedAsset.ChannelID)
	assert.Equal(t, sourceClient.KeyFingerprint, storedAsset.KeyFingerprint)
	var replicas int64
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Count(&replicas).Error)
	assert.Zero(t, replicas, "same-scope AIGC must not be imported again")
	otherClient, err := service.NewSeedanceAssetClientForSelectedKey(wrongScope, "fixture-wrong-key")
	require.NoError(t, err)
	copied, err := service.MapSeedanceAssetsToAccount(t.Context(), 7, otherClient, []string{"aigc-a"})
	require.NoError(t, err)
	assert.Equal(t, "copied-aigc-asset", copied["aigc-a"])
	assert.EqualValues(t, 1, copiedGroups.Load())
	assert.EqualValues(t, 1, copiedAssets.Load())
	_, err = service.MapSeedanceAssetsToAccount(t.Context(), 7, otherClient, []string{"human-a"})
	assert.ErrorContains(t, err, "verified real-person assets require verification")
	assert.EqualValues(t, 1, copiedGroups.Load())
	assert.EqualValues(t, 1, copiedAssets.Load())

	registry := pluginruntime.NewRegistry()
	pluginSource, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	_, err = registry.RegisterFactory(pluginSource, pluginruntime.Options{})
	require.NoError(t, err)
	generation := registry.Generation()
	binding, ok := generation.LookupDeclaredRoute("POST", path)
	require.True(t, ok)
	for _, cached := range []bool{false, true} {
		common.MemoryCacheEnabled = cached
		if cached {
			model.InitChannelCache()
		}
		for _, tc := range []struct {
			name, group  string
			user, status int
		}{
			{"shared keys", "default", 7, http.StatusOK},
			{"different user", "default", 8, http.StatusBadRequest},
			{"different caller group", "unavailable", 7, http.StatusConflict},
		} {
			t.Run(fmt.Sprintf("%s/cache=%t", tc.name, cached), func(t *testing.T) {
				router := gin.New()
				router.Use(middleware.BodyStorageCleanup())
				router.POST(path, func(c *gin.Context) {
					c.Set("id", tc.user)
					c.Set("group", tc.group)
					common.SetContextKey(c, constant.ContextKeyUsingGroup, tc.group)
					c.Set(pluginruntime.ContextKeyPinnedRoute, pluginruntime.PinnedRoute{Generation: generation, Plugin: binding.Plugin, Route: binding.Route})
				}, middleware.PrepareTaskPluginRoute(), middleware.Distribute(), func(c *gin.Context) {
					assert.Equal(t, target.Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
					assert.Equal(t, targetKey, common.GetContextKeyString(c, constant.ContextKeyChannelKey))
					assert.NotEqual(t, foreignGroup.Id, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
					mapping, _ := common.GetContextKeyType[map[string]string](c, constant.ContextKeySeedanceAssetMapping)
					assert.Equal(t, "human-a", mapping["human-a"])
					assert.Equal(t, "human-b", mapping["human-b"])
					pinned, found := common.GetContextKeyType[pluginruntime.PinnedPlugin](c, pluginruntime.ContextKeyPinnedPlugin)
					require.True(t, found)
					info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: target.Type, ApiKey: targetKey, ChannelBaseUrl: upstream.URL, UpstreamModelName: modelName}, OriginModelName: modelName, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_shared_fixture"}}
					adaptor := taskplugin.New(pinned.Plugin)
					adaptor.Init(info)
					require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
					body, err := adaptor.BuildRequestBody(c, info)
					require.NoError(t, err)
					result, err := adaptor.DoRequest(c, info, body)
					require.NoError(t, err)
					defer result.Body.Close()
					parsed, taskErr := adaptor.ParseResponse(c, result, info)
					require.Nil(t, taskErr)
					assert.Equal(t, "upstream-shared-task", parsed.UpstreamTaskID)
					c.JSON(http.StatusOK, gin.H{"ok": true})
				})
				body := fmt.Sprintf(`{"model":%q,"content":[{"type":"image_url","role":"reference_image","image_url":{"url":"asset://human-a"}},{"type":"image_url","role":"reference_image","image_url":{"url":"asset://human-b"}}],"duration":4}`, modelName)
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				assert.Equal(t, tc.status, response.Code, response.Body.String())
			})
		}
	}
	common.MemoryCacheEnabled = false
	filter := dto.ChannelFilter{Kind: dto.FilterSeedanceAssetAccount, SeedanceAssetAccountFingerprint: sourceClient.AccountFingerprint, SeedanceAssetKeyFingerprint: sourceClient.KeyFingerprint}
	multi := *target
	multi.Key = sourceKey + "\n" + targetKey
	multi.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}}
	allowed, _ := model.ChannelSatisfiesFilters(&multi, modelName, []dto.ChannelFilter{filter})
	assert.True(t, allowed)
	multi.ChannelInfo.MultiKeyStatusList[1] = common.ChannelStatusManuallyDisabled
	allowed, _ = model.ChannelSatisfiesFilters(&multi, modelName, []dto.ChannelFilter{filter})
	assert.False(t, allowed)
}

func TestSeedanceValidationTemporaryFailureRecoversWithoutFailedSession(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	var queryStatus atomic.Int32
	queryStatus.Store(504)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, common.DecodeJson(r.Body, &body))
		switch body["Action"] {
		case "CreateVisualValidateSession":
			_, _ = io.WriteString(w, `{"Result":{"BytedToken":"fixture-retry","H5Link":"https://example.com/verify"}}`)
		case "GetVisualValidateResult":
			if status := int(queryStatus.Load()); status != 200 {
				w.WriteHeader(status)
				if status == http.StatusConflict {
					_, _ = io.WriteString(w, `{"error":{"code":"OperationConflict","message":"Real-person verification is not complete"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"error":{"code":"ServiceUnavailable","message":"internal timeout"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"Result":{"GroupId":"retry-human-group"}}`)
		case "GetAssetGroup":
			_, _ = io.WriteString(w, `{"Result":{"Id":"retry-human-group","GroupType":"LivenessFace","Status":"Active"}}`)
		default:
			t.Errorf("unexpected Action %s", body["Action"])
		}
	}))
	defer upstream.Close()
	setting := `{"task_plugin_key":"anyllm","seedance_asset_url":"` + upstream.URL + `"}`
	channel := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "fixture-key", BaseURL: &upstream.URL, Setting: &setting}
	require.NoError(t, db.Create(channel).Error)
	started, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, channel, service.SeedanceAssetValidationGroupDetails{Name: "Retry human"}, "", "https://example.com/callback")
	require.NoError(t, err)
	router := gin.New()
	router.POST("/doubao/v2/assets", func(c *gin.Context) { c.Set("id", 7) }, SeedanceAssetV2Action)
	for _, status := range []int{504, 409, 429, 503} {
		queryStatus.Store(int32(status))
		request := httptest.NewRequest("POST", "/doubao/v2/assets?Action=GetVisualValidateResult&Version=2024-01-01", strings.NewReader(`{"BytedToken":"`+started.BytedToken+`"}`))
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, request)
		if status == http.StatusConflict {
			assert.Equal(t, 409, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), `"Code":"ValidationPending"`)
		} else {
			assert.Equal(t, 503, w.Code, w.Body.String())
			assert.Equal(t, "5", w.Header().Get("Retry-After"))
			assert.Contains(t, w.Body.String(), `"Code":"ServiceUnavailable"`)
		}
		var stored model.SeedanceAssetValidationSession
		require.NoError(t, db.First(&stored, started.Session.ID).Error)
		view := service.BuildSeedanceAssetValidationSessionView(&stored)
		assert.Equal(t, model.SeedanceAssetValidationStatusPending, view.Status)
		assert.Equal(t, status != http.StatusConflict, view.StatusCheckRetrying)
		assert.Empty(t, view.LastError)
	}
	queryStatus.Store(403)
	denied, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	deniedView := service.BuildSeedanceAssetValidationSessionView(denied)
	assert.False(t, deniedView.StatusCheckRetrying)
	assert.NotEmpty(t, deniedView.LastError)
	queryStatus.Store(200)
	recovered, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	view := service.BuildSeedanceAssetValidationSessionView(recovered)
	assert.Equal(t, model.SeedanceAssetValidationStatusSucceeded, view.Status)
	assert.False(t, view.StatusCheckRetrying)
	assert.Empty(t, view.LastError)
	assert.Empty(t, recovered.BytedTokenCiphertext)
}
