package controller

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupSeedancePriorityRegression(t *testing.T) *gorm.DB {
	t.Helper()
	db, dialect := openTaskDialectDatabase(t, &model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetValidationSession{}, &model.SeedanceAssetCleanupJob{}, &model.SystemTask{})
	oldDB, oldMemory := model.DB, common.MemoryCacheEnabled
	oldLogDB, oldMaster := model.LOG_DB, common.IsMasterNode
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	t.Setenv("CRYPTO_SECRET", "fixture-seedance-priority-secret")
	model.DB, common.MemoryCacheEnabled = db, false
	common.SetDatabaseTypes(dialect, dialect)
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode = false
	require.NoError(t, model.InitLogDB())
	t.Cleanup(func() {
		model.DB, common.MemoryCacheEnabled = oldDB, oldMemory
		model.LOG_DB = oldLogDB
		common.SetDatabaseTypes(oldMain, oldLog)
		_ = model.InitLogDB()
		common.IsMasterNode = oldMaster
	})
	return db
}

func configureSeedanceTestAssetURL(t *testing.T, channel *model.Channel) {
	t.Helper()
	settings := channel.GetSetting()
	settings.SeedanceAssetURL = channel.GetBaseURL()
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	value := string(encoded)
	channel.Setting = &value
}

func TestSeedanceAssetURLSettingOnlyAppliesToAssetPlugins(t *testing.T) {
	for _, tc := range []struct {
		channelType int
		plugin      string
		allowed     bool
	}{
		{constant.ChannelTypeTaskPlugin, "doubao", true},
		{constant.ChannelTypeTaskPlugin, "anyllm", true},
		{constant.ChannelTypeDoubaoVideo, "", true},
		{constant.ChannelTypeTaskPlugin, "vidu", false},
		{constant.ChannelTypeTaskPlugin, "hailuo", false},
		{constant.ChannelTypeNewAPI, "", false},
	} {
		encoded, err := common.Marshal(map[string]string{"task_plugin_key": tc.plugin, "seedance_asset_url": "https://provider.example/api/ark/assets", "seedance_asset_share_group": "shared-fixture"})
		require.NoError(t, err)
		value := string(encoded)
		err = (&model.Channel{Type: tc.channelType, Setting: &value}).ValidateSettings()
		if tc.allowed {
			assert.NoError(t, err)
		} else {
			assert.ErrorContains(t, err, "Seedance video plugins")
		}
	}
}

func TestSeedanceValidationNotFoundWaitsWithoutErrorAndExpiresLocally(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	var creates, queries atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "CreateVisualValidateSession" {
			_, _ = fmt.Fprintf(w, `{"Result":{"BytedToken":"fixture-wait-%d","H5Link":"https://example.com/verify"}}`, creates.Add(1))
			return
		}
		if queries.Add(1) == 1 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"ResponseMetadata":{"Error":{"Code":"NotFound","Message":"Ark rejected the request (NotFound)"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"Result":{"GroupId":"verified-after-wait"}}`)
	}))
	defer server.Close()
	channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "fixture-key", BaseURL: &server.URL}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, db.Create(channel).Error)
	started, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, channel, service.SeedanceAssetValidationGroupDetails{Name: "wait-then-verify"}, "", "https://example.com/callback")
	require.NoError(t, err)
	require.NoError(t, db.Model(started.Session).Update("last_error", "previous wrapped NotFound").Error)
	pending, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusPending, pending.Status)
	assert.Empty(t, pending.LastError)
	assert.Equal(t, started.Session.BytedTokenCiphertext, pending.BytedTokenCiphertext)
	var groups int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Count(&groups).Error)
	assert.Zero(t, groups)
	completed, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusSucceeded, completed.Status)
	expired, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, channel, service.SeedanceAssetValidationGroupDetails{Name: "expired-wait"}, "", "https://example.com/callback")
	require.NoError(t, err)
	require.NoError(t, db.Model(expired.Session).Update("expires_at", time.Now().Unix()-1).Error)
	ended, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, expired.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusExpired, ended.Status)
	assert.EqualValues(t, 2, queries.Load())
}

func TestSeedanceJSONValidationWaitsForReadyGroupAndGuardsOldUploads(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	require.NoError(t, db.AutoMigrate(&model.SeedanceAssetSize{}))
	groupStatus := "Processing"
	var creates, uploads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer fixture-json-key", r.Header.Get("Authorization"))
		var request map[string]string
		require.NoError(t, common.DecodeJson(r.Body, &request))
		switch request["Action"] {
		case "CreateVisualValidateSession":
			_, _ = fmt.Fprintf(w, `{"Result":{"BytedToken":"fixture-json-token-%d","H5Link":"https://example.com/verify"}}`, creates.Add(1))
		case "GetVisualValidateResult":
			_, _ = io.WriteString(w, `{"Result":{"GroupId":"json-human-group"}}`)
		case "GetAssetGroup":
			assert.Equal(t, "json-human-group", request["Id"])
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"json-human-group","GroupType":"LivenessFace","Status":%q}}`, groupStatus)
		case "CreateAsset":
			uploads.Add(1)
			_, _ = io.WriteString(w, `{"Result":{"Id":"unexpected-upload"}}`)
		default:
			t.Errorf("unexpected material Action %q", request["Action"])
		}
	}))
	defer upstream.Close()
	settings := `{"task_plugin_key":"anyllm","seedance_asset_url":"` + upstream.URL + `"}`
	channel := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "fixture-json-key", BaseURL: &upstream.URL, Setting: &settings}
	require.NoError(t, db.Create(channel).Error)
	started, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, channel, service.SeedanceAssetValidationGroupDetails{Name: "ready-group"}, "", "https://example.com/callback")
	require.NoError(t, err)
	pending, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusPending, pending.Status)
	assert.Equal(t, "json-human-group", pending.UpstreamGroupID)
	assert.NotEmpty(t, pending.BytedTokenCiphertext)
	assert.Empty(t, pending.LastError)
	var count int64
	require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Count(&count).Error)
	assert.Zero(t, count)
	groupStatus = "Active"
	completed, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, pending.ID)
	require.NoError(t, err)
	require.Equal(t, model.SeedanceAssetValidationStatusSucceeded, completed.Status)
	assert.Empty(t, completed.BytedTokenCiphertext)
	var group model.SeedanceAssetGroup
	require.NoError(t, db.First(&group, completed.LocalGroupID).Error)
	assert.Equal(t, "Active", group.Status)

	groupStatus = "Processing"
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("group_id", group.GroupID))
	part, err := writer.CreateFormFile("file", "portrait.jpg")
	require.NoError(t, err)
	_, err = part.Write([]byte("not decoded before the group is ready"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	router := gin.New()
	router.POST("/upload", func(c *gin.Context) { c.Set("id", 7) }, UploadSeedanceAsset)
	request := httptest.NewRequest(http.MethodPost, "/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusConflict, recorder.Code, recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "still processing")
	assert.Zero(t, uploads.Load())
	stored, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, completed.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusPending, stored.Status)
	require.NoError(t, db.First(&group, group.ID).Error)
	assert.Equal(t, "Processing", group.Status)
	groupStatus = "Active"
	staleGroup := group
	client, err := service.NewSeedanceAssetClient(channel)
	require.NoError(t, err)
	require.NoError(t, service.EnsureSeedanceAssetGroupReady(t.Context(), client, &group))
	require.NoError(t, service.EnsureSeedanceAssetGroupReady(t.Context(), client, &staleGroup))
	assert.Equal(t, "Active", staleGroup.Status)
	ready, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, completed.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusSucceeded, ready.Status)

	rejected, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, channel, service.SeedanceAssetValidationGroupDetails{Name: "rejected-group"}, "", "https://example.com/callback")
	require.NoError(t, err)
	groupStatus = "Failed"
	failed, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, rejected.Session.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusFailed, failed.Status)
	assert.Empty(t, failed.BytedTokenCiphertext)
	require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
}

func TestSeedanceGroupDeletionReportsFailureAndKeepsOwnerIsolation(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	group := &model.SeedanceAssetGroup{UserID: 7, ChannelID: 1, GroupID: "deleting-group", Name: "Waiting group", Status: "Deleting"}
	require.NoError(t, db.Create(group).Error)
	ownerJob := &model.SeedanceAssetCleanupJob{UserID: 7, LocalGroupID: group.ID, Kind: model.SeedanceAssetCleanupKindGroupDelete, DedupKey: "owned-delete", Status: model.SeedanceAssetCleanupStatusPending, LastError: "old account unavailable", NextAttemptAt: time.Now().Unix() + 60}
	foreignJob := &model.SeedanceAssetCleanupJob{UserID: 8, LocalGroupID: group.ID, Kind: model.SeedanceAssetCleanupKindGroupDelete, DedupKey: "foreign-delete", Status: model.SeedanceAssetCleanupStatusPending, LastError: "foreign account"}
	require.NoError(t, db.Create(ownerJob).Error)
	require.NoError(t, db.Create(foreignJob).Error)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("id", 7) })
	router.GET("/groups", ListSeedanceAssetGroups)
	router.GET("/groups/:id", GetSeedanceAssetGroup)
	for _, path := range []string{"/groups", fmt.Sprintf("/groups/%d", group.ID)} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Contains(t, recorder.Body.String(), `"deletion_error":"old account unavailable"`)
		assert.NotContains(t, recorder.Body.String(), "foreign account")
	}
	require.NoError(t, db.Model(ownerJob).Update("lease_until", time.Now().Unix()+30).Error)
	groups := []model.SeedanceAssetGroup{*group}
	require.NoError(t, model.LoadSeedanceAssetGroupDeletionStates(t.Context(), 7, groups))
	assert.Empty(t, groups[0].DeletionError)
	assert.Equal(t, ownerJob.NextAttemptAt, groups[0].DeletionRetryAt)
}

func TestSeedanceAssetPriorityPaging(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	baseURL, otherPlugin := "https://example.com", `{"task_plugin_key":"other"}`
	unsupported := make([]model.Channel, 101)
	priority := int64(1000)
	for index := range unsupported {
		unsupported[index] = model.Channel{Id: 1000 + index, Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "fixture", BaseURL: &baseURL, Setting: &otherPlugin, Priority: &priority}
	}
	require.NoError(t, db.Create(&unsupported).Error)
	create := func(id int, priority *int64) *model.Channel {
		channel := &model.Channel{Id: id, Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "fixture", BaseURL: &baseURL, Priority: priority, Models: "doubao-seedance-2-0-fast-260128", Group: "default"}
		configureSeedanceTestAssetURL(t, channel)
		require.NoError(t, db.Create(channel).Error)
		require.NoError(t, channel.AddAbilities(nil))
		return channel
	}
	high, low, negative := int64(200), int64(1), int64(-10)
	create(1, &low)
	create(200, &high)
	create(400, &high)
	create(2, nil)
	create(3, &negative)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 2).UpdateColumn("priority", nil).Error)
	disabledPriority := int64(900)
	disabled := create(1500, &disabledPriority)
	disabled.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeyStatusList: map[int]int{0: common.ChannelStatusAutoDisabled}}
	require.NoError(t, db.Model(disabled).Update("channel_info", disabled.ChannelInfo).Error)

	selected, err := service.FindSeedanceAssetChannel()
	require.NoError(t, err)
	assert.Equal(t, 200, selected.Id)
	channels, err := service.FindSeedanceAssetChannels()
	require.NoError(t, err)
	ids := make([]int, len(channels))
	for index, channel := range channels {
		ids[index] = channel.Id
	}
	assert.Equal(t, []int{200, 400, 1, 2, 3}, ids)
	for _, action := range []string{"", "CreateAssetGroup"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set(seedanceAssetActionContextKey, action)
		c.Set("group", "default")
		selected, err := selectSeedanceAssetGroupChannel(c, "")
		require.NoError(t, err)
		assert.Equal(t, 200, selected.Id, action)
	}
}

func TestSeedanceValidationPriorityPinsActualMultiKeyAccount(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		for _, mode := range []constant.MultiKeyMode{constant.MultiKeyModePolling, constant.MultiKeyModeRandom} {
			for _, memory := range []bool{false, true} {
				t.Run(fmt.Sprintf("fallback-%t/%s-memory-%t", fallback, mode, memory), func(t *testing.T) {
					db := setupSeedancePriorityRegression(t)
					common.MemoryCacheEnabled = memory
					t.Setenv("CRYPTO_SECRET", "fixture-validation-persistent-secret")
					var unsupportedCalls, unusedCalls atomic.Int32
					createdKeys, queriedKeys := make(chan string, 1), make(chan string, 1)
					unsupported := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						unsupportedCalls.Add(1)
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"code":"unsupported_action","message":"unsupported action"}`)
					}))
					defer unsupported.Close()
					selected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Query().Get("Action") {
						case "CreateVisualValidateSession":
							createdKeys <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
							_, _ = io.WriteString(w, `{"Result":{"BytedToken":"fixture-validation-token","H5Link":"https://example.com/verify"}}`)
						case "GetVisualValidateResult":
							queriedKeys <- strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
							_, _ = io.WriteString(w, `{"Result":{"GroupId":"fixture-verified-group"}}`)
						default:
							t.Errorf("unexpected action %q", r.URL.Query().Get("Action"))
						}
					}))
					defer selected.Close()
					unused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						unusedCalls.Add(1)
						w.WriteHeader(http.StatusInternalServerError)
					}))
					defer unused.Close()
					lowPriority, selectedPriority, highPriority := int64(0), int64(50), int64(100)
					channels := []*model.Channel{
						{Id: 1, Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "unused-key", BaseURL: &unused.URL, Priority: &lowPriority},
						{Id: 500, Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "selected-a\nselected-b", BaseURL: &selected.URL, Priority: &selectedPriority, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: mode}},
						{Id: 900, Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "unsupported-key", BaseURL: &unsupported.URL, Priority: &highPriority},
					}
					if !fallback {
						channels = channels[:2]
					}
					for _, channel := range channels {
						configureSeedanceTestAssetURL(t, channel)
					}
					require.NoError(t, db.Create(channels).Error)
					if memory {
						model.InitChannelCache()
					}
					started, err := service.StartSeedanceAssetValidationSessionWithFallback(t.Context(), 7, channels, service.SeedanceAssetValidationGroupDetails{Name: "verified-priority"}, "", "https://example.com/callback")
					require.NoError(t, err)
					require.Len(t, createdKeys, 1)
					createdKey := <-createdKeys
					require.NotEmpty(t, createdKey)
					assert.Equal(t, 500, started.Session.ChannelID)
					assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(createdKey))), started.Session.KeyFingerprint)
					var stored model.SeedanceAssetValidationSession
					require.NoError(t, db.First(&stored, started.Session.ID).Error)
					assert.Equal(t, started.Session.KeyFingerprint, stored.KeyFingerprint)
					assert.Equal(t, started.Session.AccountFingerprint, stored.AccountFingerprint)
					require.NoError(t, db.Model(channels[1]).UpdateColumn("key", "selected-b\nselected-a").Error)
					completed, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, stored.ID)
					require.NoError(t, err)
					assert.Equal(t, model.SeedanceAssetValidationStatusSucceeded, completed.Status)
					require.Len(t, queriedKeys, 1)
					assert.Equal(t, createdKey, <-queriedKeys)
					if fallback {
						assert.EqualValues(t, 1, unsupportedCalls.Load())
					} else {
						assert.Zero(t, unsupportedCalls.Load())
					}
					assert.Zero(t, unusedCalls.Load())
				})
			}
		}
	}
}

func TestSeedanceDeletedVerifiedGroupInvalidatesSessionAndAllowsRestart(t *testing.T) {
	db := setupSeedancePriorityRegression(t)
	t.Setenv("CRYPTO_SECRET", "fixture-validation-persistent-secret")
	var creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") == "CreateVisualValidateSession" {
			_, _ = fmt.Fprintf(w, `{"Result":{"BytedToken":"fixture-token-%d","H5Link":"https://example.com/verify"}}`, creates.Add(1))
			return
		}
		_, _ = io.WriteString(w, `{"Result":{"GroupId":"fixture-verified-group"}}`)
	}))
	defer upstream.Close()
	channel := &model.Channel{Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "fixture-key", BaseURL: &upstream.URL}
	configureSeedanceTestAssetURL(t, channel)
	require.NoError(t, db.Create(channel).Error)
	started, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, channel, service.SeedanceAssetValidationGroupDetails{Name: "original-group"}, "", "https://example.com/callback")
	require.NoError(t, err)
	completed, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, started.Session.ID)
	require.NoError(t, err)
	require.Equal(t, model.SeedanceAssetValidationStatusSucceeded, completed.Status)
	var group model.SeedanceAssetGroup
	require.NoError(t, db.First(&group, completed.LocalGroupID).Error)
	require.NoError(t, model.QueueSeedanceAssetGroupDeletion(t.Context(), service.NewSeedanceAssetGroupDeleteCleanupJob(&group)))
	cancelled, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, completed.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusCancelled, cancelled.Status)
	assert.Zero(t, cancelled.LocalGroupID)
	assert.Empty(t, cancelled.UpstreamGroupID)
	restarted, err := service.StartSeedanceAssetValidationSession(t.Context(), 7, channel, service.SeedanceAssetValidationGroupDetails{Name: "new-group"}, "", "https://example.com/callback")
	require.NoError(t, err)
	assert.NotEqual(t, completed.ID, restarted.Session.ID)
	assert.Equal(t, model.SeedanceAssetValidationStatusPending, restarted.Session.Status)

	orphan := &model.SeedanceAssetValidationSession{UserID: 7, ChannelID: channel.Id, Status: model.SeedanceAssetValidationStatusSucceeded, Name: "orphan-group", LocalGroupID: 99999, UpstreamGroupID: "removed-group", CallbackStateHash: "fixture-orphan-state"}
	require.NoError(t, db.Create(orphan).Error)
	recovered, err := service.RefreshSeedanceAssetValidationSession(t.Context(), 7, orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SeedanceAssetValidationStatusCancelled, recovered.Status)
	assert.Zero(t, recovered.LocalGroupID)
}
