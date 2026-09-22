package middleware

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type seedanceAssetAffinityLoadCase struct {
	userID          int
	body            string
	wantSuccess     bool
	wantChannelID   int
	wantFingerprint string
}

type seedanceAssetAffinityLoadResult struct {
	err             error
	channelID       int
	keyFingerprint  string
	wantSuccess     bool
	wantChannelID   int
	wantFingerprint string
}

// TestSeedanceAssetAffinityConcurrentLoad 在 200 用户、64 并发下验证素材账号绑定不串号。
// 仅显式开启，避免普通单元测试因为压测数据扩大运行时间。
func TestSeedanceAssetAffinityConcurrentLoad(t *testing.T) {
	if os.Getenv("SEEDANCE_ASSET_AFFINITY_LOAD") != "1" {
		t.Skip("explicit Seedance asset affinity load test only")
	}
	gin.SetMode(gin.TestMode)
	previousDB := model.DB
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "seedance-affinity-load.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(64)
	require.NoError(t, db.AutoMigrate(&model.SeedanceAsset{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		_ = sqlDB.Close()
	})

	const userCount = 200
	assets := make([]model.SeedanceAsset, 0, userCount*4)
	cases := make([]seedanceAssetAffinityLoadCase, 0, userCount*10)
	for userID := 1; userID <= userCount; userID++ {
		channelA := 10000 + userID
		channelB := 20000 + userID
		fingerprintA := fmt.Sprintf("account-a-%03d", userID)
		fingerprintB := fmt.Sprintf("account-b-%03d", userID)
		for _, asset := range []struct {
			id          string
			channelID   int
			fingerprint string
		}{
			{fmt.Sprintf("load-%03d-a1", userID), channelA, fingerprintA},
			{fmt.Sprintf("load-%03d-a2", userID), channelA, fingerprintA},
			{fmt.Sprintf("load-%03d-b1", userID), channelB, fingerprintB},
			{fmt.Sprintf("load-%03d-b2", userID), channelB, fingerprintB},
		} {
			assets = append(assets, model.SeedanceAsset{
				UserID: userID, ChannelID: asset.channelID, GroupID: fmt.Sprintf("group-%03d", userID),
				AssetID: asset.id, Name: asset.id, AssetType: "Image", Status: "Active",
				KeyFingerprint: asset.fingerprint,
			})
		}
		for range 5 {
			cases = append(cases, seedanceAssetAffinityLoadCase{
				userID: userID, body: fmt.Sprintf(`{"content":[{"type":"image_url","image_url":{"url":"asset://load-%03d-a1"}}]}`, userID),
				wantSuccess: true, wantChannelID: channelA, wantFingerprint: fingerprintA,
			})
		}
		for range 3 {
			cases = append(cases, seedanceAssetAffinityLoadCase{
				userID: userID, body: fmt.Sprintf(`{"content":[{"type":"image_url","image_url":{"url":"asset://load-%03d-a1"}},{"type":"image_url","image_url":{"url":"asset://load-%03d-a2"}}]}`, userID, userID),
				wantSuccess: true, wantChannelID: channelA, wantFingerprint: fingerprintA,
			})
		}
		for range 2 {
			cases = append(cases, seedanceAssetAffinityLoadCase{
				userID: userID, body: fmt.Sprintf(`{"content":[{"type":"image_url","image_url":{"url":"asset://load-%03d-a1"}},{"type":"image_url","image_url":{"url":"asset://load-%03d-b1"}}]}`, userID, userID),
				wantSuccess: false,
			})
		}
	}
	require.NoError(t, db.CreateInBatches(&assets, 100).Error)

	jobs := make(chan seedanceAssetAffinityLoadCase, len(cases))
	results := make(chan seedanceAssetAffinityLoadResult, len(cases))
	for _, testCase := range cases {
		jobs <- testCase
	}
	close(jobs)
	var workers sync.WaitGroup
	workers.Add(64)
	for range 64 {
		go func() {
			defer workers.Done()
			for testCase := range jobs {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest("POST", "/v1/video/generations", strings.NewReader(testCase.body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Set("id", testCase.userID)
				err := applySeedanceAssetAffinity(c)
				result := seedanceAssetAffinityLoadResult{
					err:             err,
					wantSuccess:     testCase.wantSuccess,
					wantChannelID:   testCase.wantChannelID,
					wantFingerprint: testCase.wantFingerprint,
				}
				if err == nil {
					result.channelID = common.GetContextKeyInt(c, constant.ContextKeySeedanceAssetChannelId)
					result.keyFingerprint = common.GetContextKeyString(c, constant.ContextKeySeedanceAssetKeyFingerprint)
				}
				common.CleanupBodyStorage(c)
				results <- result
			}
		}()
	}
	workers.Wait()
	close(results)

	successCount := 0
	errorCount := 0
	for result := range results {
		if result.wantSuccess {
			require.NoError(t, result.err)
			assert.Equal(t, result.wantChannelID, result.channelID)
			assert.Equal(t, result.wantFingerprint, result.keyFingerprint)
			successCount++
			continue
		}
		require.Error(t, result.err)
		assert.Contains(t, result.err.Error(), "different upstream accounts")
		errorCount++
	}
	assert.Equal(t, userCount*8, successCount)
	assert.Equal(t, userCount*2, errorCount)
}
