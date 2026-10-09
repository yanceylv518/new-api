package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestExtractSeedanceAssetIDsFromJSONOnlyReadsMediaFields(t *testing.T) {
	raw := []byte(`{"model":"doubao-seedance-2-0-fast-260128","prompt":"asset://ignore-this-text","content":[{"type":"image_url","image_url":{"url":"asset://asset-image"}},{"type":"video_url","video_url":{"url":"asset://asset-video"}}],"metadata":{"content":[{"image_url":{"url":"asset://asset-image"}}]}}`)
	ids, err := extractSeedanceAssetIDsFromJSON(raw)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"asset-image", "asset-video"}, ids)
}

func TestExtractSeedanceAssetIDsFromJSONHandlesEscapedSlashes(t *testing.T) {
	raw := []byte(`{"content":[{"image_url":{"url":"asset:\/\/asset-image"}}]}`)
	ids, err := extractSeedanceAssetIDsFromJSON(raw)
	require.NoError(t, err)
	assert.Equal(t, []string{"asset-image"}, ids)

	rewritten, changed, err := rewriteSeedanceJSON(raw, nil, map[string]string{"asset-image": "remote-image"})
	require.NoError(t, err)
	require.True(t, changed)
	assert.Contains(t, string(rewritten), "asset://remote-image")
}

func TestSeedanceAssetEscapingCannotBypassOwnershipMapping(t *testing.T) {
	for _, reference := range []string{`asset://victim`, `\u0061sset://victim`, `ASSET://victim`, `\u0061\u0073\u0073\u0065\u0074\u003a\/\/victim`} {
		t.Run(reference, func(t *testing.T) {
			raw := []byte(`{"content":[{"image_url":{"url":"` + reference + `"}}]}`)
			ids, err := extractSeedanceAssetIDsFromJSON(raw)
			require.NoError(t, err)
			assert.Equal(t, []string{"victim"}, ids)
			_, _, err = rewriteSeedanceJSON(raw, nil, map[string]string{})
			assert.ErrorIs(t, err, ErrInvalidSeedanceAssetRequest)
			rewritten, changed, err := rewriteSeedanceJSON(raw, nil, map[string]string{"victim": "authorized"})
			require.NoError(t, err)
			assert.True(t, changed)
			assert.Contains(t, string(rewritten), "asset://authorized")
		})
	}
}

func TestRewriteSeedanceJSONRemapsMediaAndPreservesUnrelatedValues(t *testing.T) {
	raw := []byte(`{"model":"doubao-seedance-2-0-fast-260128","seed":18446744073709551615,"prompt":"keep asset://remote-old as text","content":[{"type":"image_url","image_url":{"url":"asset://remote-old"}}]}`)
	previous := map[string]string{"local-image": "remote-old"}
	selected := map[string]string{"local-image": "remote-new"}

	rewritten, changed, err := rewriteSeedanceJSON(raw, previous, selected)
	require.NoError(t, err)
	require.True(t, changed)
	var result struct {
		Seed    json.Number `json:"seed"`
		Prompt  string      `json:"prompt"`
		Content []struct {
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	require.NoError(t, common.Unmarshal(rewritten, &result))
	assert.Equal(t, json.Number("18446744073709551615"), result.Seed)
	assert.Equal(t, "keep asset://remote-old as text", result.Prompt)
	require.Len(t, result.Content, 1)
	assert.Equal(t, "asset://remote-new", result.Content[0].ImageURL.URL)
}

func TestRewriteSeedanceJSONRejectsUnknownReference(t *testing.T) {
	_, _, err := rewriteSeedanceJSON([]byte(`{"content":[{"image_url":{"url":"asset://unknown"}}]}`), nil, map[string]string{})
	assert.ErrorIs(t, err, ErrInvalidSeedanceAssetRequest)
}

func TestSeedancePluginSnapshotsSubmitSelectedAccountAsset(t *testing.T) {
	service.InitHttpClient()
	source, err := builtinplugins.Source("doubao")
	require.NoError(t, err)
	plugin, err := pluginruntime.NewRegistry().RegisterFactory(source, pluginruntime.Options{Key: "doubao"})
	require.NoError(t, err)
	const modelName = "doubao-seedance-2-0-260128"
	const prompt = "keep asset://local-asset as text"
	content := `[{"type":"text","text":"` + prompt + `"},{"type":"image_url","role":"reference_image","image_url":{"url":"asset://local-asset"}}]`
	for _, surface := range []string{"native", "video_json", "video_multipart"} {
		t.Run(surface, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			path := "/v1/videos"
			if surface == "native" {
				path = "/doubao/api/v3/contents/generations/tasks"
			}
			if surface == "video_multipart" {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				for key, value := range map[string]string{"model": modelName, "content": content, "duration": "4", "resolution": "480p", "generate_audio": "false"} {
					require.NoError(t, writer.WriteField(key, value))
				}
				require.NoError(t, writer.Close())
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body.Bytes()))
				c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			} else {
				body := `{"model":"` + modelName + `","content":` + content + `,"duration":4,"resolution":"480p","generate_audio":false}`
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
			}
			route, err := buildTaskPluginRouteRequest(c)
			require.NoError(t, err)
			var resolved any
			if surface == "native" {
				resolved, err = plugin.Engine.CallPath(t.Context(), "native", []string{"createTask"}, route.JSValue())
			} else {
				protocol := pluginruntime.ProtocolRequestContext{RouteRequestContext: route, Protocol: "openai_video", Operation: "create", Model: modelName}
				resolved, err = plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, protocol.JSValue())
				c.Set(pluginruntime.ContextKeyProtocolRequest, protocol)
				c.Set(pluginruntime.ContextKeyPinnedEndpoint, pluginruntime.PinnedEndpoint{Plugin: plugin, Protocol: "openai_video", Model: modelName})
			}
			require.NoError(t, err)
			route.RequestBody = resolved.(map[string]any)["requestBody"]
			c.Set(pluginruntime.ContextKeyRouteRequest, route)
			c.Set("task_request", route.RequestBody)
			c.Set("resolved_task_model", modelName)
			var previous map[string]string
			for _, account := range []string{"a", "b"} {
				key := "key-" + account
				remoteID := "remote-" + account
				var received atomic.Bool
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
					var request struct {
						Content []struct {
							Text     string `json:"text"`
							ImageURL struct {
								URL string `json:"url"`
							} `json:"image_url"`
						} `json:"content"`
						Duration      int  `json:"duration"`
						GenerateAudio bool `json:"generate_audio"`
					}
					if !assert.NoError(t, common.DecodeJson(r.Body, &request)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if !assert.Len(t, request.Content, 2) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					received.Store(true)
					assert.Equal(t, prompt, request.Content[0].Text)
					assert.Equal(t, 4, request.Duration)
					assert.False(t, request.GenerateAudio)
					if !assert.Equal(t, "asset://"+remoteID, request.Content[1].ImageURL.URL) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"accepted"}`)
				}))
				t.Cleanup(upstream.Close)
				mapping := map[string]string{"local-asset": remoteID}
				require.NoError(t, rewriteSeedanceRequestBody(c, previous, mapping))
				info := &relaycommon.RelayInfo{
					ChannelMeta:     &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeDoubaoVideo, ChannelBaseUrl: upstream.URL, ApiKey: key, UpstreamModelName: modelName},
					OriginModelName: modelName, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task-test"},
				}
				adaptor := taskplugin.New(plugin)
				adaptor.Init(info)
				require.Nil(t, adaptor.ValidateRequestAndSetAction(c, info))
				body, err := adaptor.BuildRequestBody(c, info)
				require.NoError(t, err)
				response, err := adaptor.DoRequest(c, info, body)
				require.NoError(t, err)
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				assert.Equal(t, http.StatusOK, response.StatusCode)
				assert.True(t, received.Load())
				previous = mapping
			}
			original, err := common.Marshal(route.RequestBody)
			require.NoError(t, err)
			assert.Contains(t, string(original), `"url":"asset://local-asset"`, "request snapshots must not mutate shared decoder inputs")
		})
	}
}

func TestSeedanceSnapshotRewriteFailurePreservesRequest(t *testing.T) {
	for _, invalidSnapshot := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid_snapshot_%t", invalidSnapshot), func(t *testing.T) {
			rawID, snapshotID := "unknown", "local-asset"
			if invalidSnapshot {
				rawID, snapshotID = snapshotID, rawID
			}
			original := `{"image_url":"asset://` + rawID + `"}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(original))
			c.Request.Header.Set("Content-Type", "application/json")
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			request := map[string]any{"image_url": "asset://" + snapshotID, "seed": json.Number("18446744073709551615"), "generate_audio": false}
			c.Set("task_request", request)
			err := rewriteSeedanceRequestBody(c, nil, map[string]string{"local-asset": "remote"})
			require.ErrorIs(t, err, ErrInvalidSeedanceAssetRequest)
			stored, exists := c.Get("task_request")
			require.True(t, exists)
			assert.Equal(t, request, stored)
			storage, err := common.GetBodyStorage(c)
			require.NoError(t, err)
			body, err := storage.Bytes()
			require.NoError(t, err)
			assert.Equal(t, original, string(body))
		})
	}
}

func TestPrepareSeedanceAssetRequestRejectsUnsupportedChannel(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
	common.SetContextKey(c, constant.ContextKeySeedanceAssetReferences, []string{"asset-a"})

	err := prepareSeedanceAssetRequest(c, &model.Channel{Type: constant.ChannelTypeOpenAI}, "key")
	assert.ErrorIs(t, err, ErrInvalidSeedanceAssetRequest)
}

func TestSeedanceAssetMappingPendingSetsRetryAfter(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	response := newSeedanceAssetMappingError(c, service.ErrSeedanceAssetMappingPending)

	require.Equal(t, http.StatusConflict, response.StatusCode)
	assert.Equal(t, seedanceAssetMappingRetryAfterSeconds, recorder.Header().Get("Retry-After"))

	invalidRecorder := httptest.NewRecorder()
	invalidContext, _ := gin.CreateTestContext(invalidRecorder)
	newSeedanceAssetMappingError(invalidContext, ErrInvalidSeedanceAssetRequest)
	assert.Empty(t, invalidRecorder.Header().Get("Retry-After"))
}

func TestRewriteSeedanceMultipartPreservesFilesAndPrompt(t *testing.T) {
	var original bytes.Buffer
	writer := multipart.NewWriter(&original)
	require.NoError(t, writer.WriteField("prompt", "keep asset://prompt-only as text"))
	require.NoError(t, writer.WriteField("content", `[{"type":"image_url","image_url":{"url":"asset://local-image"}}]`))
	filePart, err := writer.CreateFormFile("file", "source.bin")
	require.NoError(t, err)
	_, err = io.WriteString(filePart, "original-file-content")
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/videos", bytes.NewReader(original.Bytes()))
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	t.Cleanup(func() { common.CleanupBodyStorage(c) })

	err = rewriteSeedanceRequestBody(c, nil, map[string]string{"local-image": "remote-image"})
	require.NoError(t, err)
	assert.Contains(t, c.Request.Header.Get("Content-Type"), "multipart/form-data; boundary=")

	form, err := common.ParseMultipartFormReusable(c)
	require.NoError(t, err)
	defer form.RemoveAll()
	assert.Equal(t, []string{"keep asset://prompt-only as text"}, form.Value["prompt"])
	var content []struct {
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	require.NoError(t, common.Unmarshal([]byte(form.Value["content"][0]), &content))
	require.Len(t, content, 1)
	assert.Equal(t, "asset://remote-image", content[0].ImageURL.URL)
	require.Len(t, form.File["file"], 1)
	file, err := form.File["file"][0].Open()
	require.NoError(t, err)
	defer file.Close()
	fileContents, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, "original-file-content", string(fileContents))
}

func TestPrepareSeedanceAssetRequestLeavesSelectedChannelAndTokenPinUntouched(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/videos", strings.NewReader(`{"model":"doubao-seedance-2-0-fast-260128","content":[]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	constraints := &dto.ChannelConstraints{}
	constraints.AddPin(dto.ChannelPin{ChannelId: 22, Source: dto.PinSourceToken, Rank: dto.PinRankToken})
	common.SetContextKey(c, constant.ContextKeyChannelConstraints, constraints)

	channel := &model.Channel{
		Id:     22,
		Type:   constant.ChannelTypeDoubaoVideo,
		Key:    "selected-key",
		Status: common.ChannelStatusEnabled,
	}
	setupErr := SetupContextForSelectedChannel(c, channel, "doubao-seedance-2-0-fast-260128")
	require.Nil(t, setupErr)
	assert.Equal(t, "selected-key", common.GetContextKeyString(c, constant.ContextKeyChannelKey))
	assert.Equal(t, 22, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
	_, hasMapping := common.GetContextKey(c, constant.ContextKeySeedanceAssetMapping)
	assert.False(t, hasMapping)

	resolved, found, overridden := constraints.ResolvedPin()
	require.True(t, found)
	assert.Equal(t, dto.PinSourceToken, resolved.Source)
	assert.Empty(t, overridden)
}

func TestPrepareSeedanceAssetRequestRemapsAfterChannelReselection(t *testing.T) {
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(
		&model.SystemTask{},
		&model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{},
		&model.SeedanceAsset{}, &model.SeedanceAssetReplica{},
	))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		_ = sqlDB.Close()
	})

	group := &model.SeedanceAssetGroup{
		UserID: 9, ChannelID: 11, GroupID: "local-group", Name: "Local group",
		Status: "Active", KeyFingerprint: "source-key", AccountFingerprint: "source-account",
	}
	require.NoError(t, db.Create(group).Error)
	asset := &model.SeedanceAsset{
		UserID: 9, ChannelID: 11, GroupID: group.GroupID, AssetID: "local-asset",
		Name: "source.png", AssetType: "Image", Status: "Active",
		SourceURL: "https://8.8.8.8/source.png", KeyFingerprint: group.KeyFingerprint,
		AccountFingerprint: group.AccountFingerprint,
	}
	require.NoError(t, db.Create(asset).Error)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"group-%s"}}`, key)
		case "CreateAsset":
			_, _ = fmt.Fprintf(writer, `{"Result":{"Id":"asset-%s"}}`, key)
		case "GetAsset":
			_, _ = writer.Write([]byte(`{"Result":{"Status":"Active"}}`))
		default:
			http.Error(writer, "unexpected Seedance action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	channel := func(id int, key string) *model.Channel {
		return &model.Channel{
			Id: id, Type: constant.ChannelTypeDoubaoVideo, Key: key,
			Status: common.ChannelStatusEnabled, BaseURL: &upstream.URL,
		}
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("id", group.UserID)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(
		`{"model":"doubao-seedance-2-0-fast-260128","content":[{"type":"image_url","image_url":{"url":"asset://local-asset"}}]}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })

	require.NoError(t, prepareSeedanceAssetRequest(c, channel(21, "key-a"), "key-a"))
	require.NoError(t, prepareSeedanceAssetRequest(c, channel(22, "key-b"), "key-b"))
	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	rewritten, err := storage.Bytes()
	require.NoError(t, err)
	assert.Contains(t, string(rewritten), "asset://asset-key-b")
	assert.NotContains(t, string(rewritten), "asset://asset-key-a")
	assert.NotContains(t, string(rewritten), "asset://local-asset")

	mapping, _ := common.GetContextKeyType[map[string]string](c, constant.ContextKeySeedanceAssetMapping)
	assert.Equal(t, "asset-key-b", mapping[asset.AssetID])
}

func TestTokenModelLimitDenial(t *testing.T) {
	require.NoError(t, i18n.Init())
	newContext := func(modelLimit map[string]bool) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/videos", nil)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
		if modelLimit != nil {
			common.SetContextKey(c, constant.ContextKeyTokenModelLimit, modelLimit)
		}
		return c
	}

	const modelName = "doubao-seedance-test"
	assert.NotEmpty(t, tokenModelLimitDenial(newContext(nil), modelName), "missing model limits must deny access")
	assert.NotEmpty(t, tokenModelLimitDenial(newContext(map[string]bool{"other-model": true}), modelName))
	assert.Empty(t, tokenModelLimitDenial(newContext(map[string]bool{modelName: true}), modelName))
}
