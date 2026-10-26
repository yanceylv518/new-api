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
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupSeedancePriorityRegression(t *testing.T) *gorm.DB {
	t.Helper()
	db, dialect := openTaskDialectDatabase(t, &model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetValidationSession{}, &model.SeedanceAssetCleanupJob{})
	oldDB, oldMemory := model.DB, common.MemoryCacheEnabled
	oldMain, oldLog := common.MainDatabaseType(), common.LogDatabaseType()
	model.DB, common.MemoryCacheEnabled = db, false
	common.SetDatabaseTypes(dialect, dialect)
	t.Cleanup(func() {
		model.DB, common.MemoryCacheEnabled = oldDB, oldMemory
		common.SetDatabaseTypes(oldMain, oldLog)
	})
	return db
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
		channel := &model.Channel{Id: id, Type: constant.ChannelTypeDoubaoVideo, Status: common.ChannelStatusEnabled, Key: "fixture", BaseURL: &baseURL, Priority: priority}
		require.NoError(t, db.Create(channel).Error)
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
