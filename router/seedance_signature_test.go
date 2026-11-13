package router

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	appI18n "github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/gin-gonic/gin"
	driver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// signSeedanceClientRequest 以客户端视角构造火山 V4 请求，输入仅为隔离测试随机凭据。
func signSeedanceClientRequest(t *testing.T, request *http.Request, body []byte, accessKey, secret string, timestamp time.Time, includeContentHash bool) {
	t.Helper()
	date := timestamp.UTC().Format("20060102T150405Z")
	digest := sha256.Sum256(body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Date", date)
	headerNames := []string{"content-type", "host", "x-date"}
	if includeContentHash {
		request.Header.Set("X-Content-Sha256", hex.EncodeToString(digest[:]))
		headerNames = []string{"content-type", "host", "x-content-sha256", "x-date"}
	}
	if request.Header.Get("Content-Encoding") != "" {
		headerNames = append([]string{"content-encoding"}, headerNames...)
	}
	var headers strings.Builder
	for _, name := range headerNames {
		value := request.Header.Get(name)
		if name == "host" {
			value = request.Host
		}
		headers.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	path := strings.ReplaceAll(request.URL.EscapedPath(), "asset:", "asset%3A")
	query := strings.ReplaceAll(request.URL.Query().Encode(), "+", "%20")
	canonical := strings.Join([]string{request.Method, path, query, headers.String(), strings.Join(headerNames, ";"), hex.EncodeToString(digest[:])}, "\n")
	canonicalHash := sha256.Sum256([]byte(canonical))
	scope := timestamp.UTC().Format("20060102") + "/cn-beijing/ark/request"
	toSign := "HMAC-SHA256\n" + date + "\n" + scope + "\n" + hex.EncodeToString(canonicalHash[:])
	mac := func(key []byte, value string) []byte {
		h := hmac.New(sha256.New, key)
		_, err := h.Write([]byte(value))
		require.NoError(t, err)
		return h.Sum(nil)
	}
	key := mac([]byte(secret), timestamp.UTC().Format("20060102"))
	key = mac(key, "cn-beijing")
	key = mac(key, "ark")
	key = mac(key, "request")
	request.Header.Set("Authorization", "HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+strings.Join(headerNames, ";")+", Signature="+hex.EncodeToString(mac(key, toSign)))
}

func TestSeedanceSignatureAuthenticationMySQL(t *testing.T) {
	dsn := os.Getenv("AKSK_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AKSK_TEST_MYSQL_DSN is not configured")
	}
	config, err := driver.ParseDSN(dsn)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(config.DBName, "codex_aksk_"), "test requires an isolated database")
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	database, err := db.DB()
	require.NoError(t, err)
	previousDB, previousLog := model.DB, model.LOG_DB
	previousMain, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousMemory, previousSecret := common.RedisEnabled, common.MemoryCacheEnabled, common.CryptoSecret
	var secretBytes [32]byte
	_, err = rand.Read(secretBytes[:])
	require.NoError(t, err)
	common.CryptoSecret = hex.EncodeToString(secretBytes[:])
	common.RedisEnabled, common.MemoryCacheEnabled = false, false
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
	model.DB, model.LOG_DB = db, db
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLog
		common.RedisEnabled, common.MemoryCacheEnabled, common.CryptoSecret = previousRedis, previousMemory, previousSecret
		common.SetDatabaseTypes(previousMain, previousLogType)
		assert.NoError(t, database.Close())
	})
	require.NoError(t, appI18n.Init())
	// 先创建旧结构和令牌，再重复升级凭据表，验证现有数据与迁移幂等性。
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.UserModelPricingHistory{}, &model.UserAccessToken{}, &model.AuditLog{}, &model.SeedanceAssetGroup{}, &model.SeedanceAsset{}, &model.SeedanceAssetReplica{}, &model.Task{}, &model.VideoTaskPendingLog{}, &model.BillingRefund{}, &model.ExternalIdentityClaim{}, &model.TwoFABackupCode{}, &model.TwoFA{}, &model.UserSession{}, &model.AuthFlow{}, &model.PasskeyCredential{}, &model.UserOAuthBinding{}))
	user := model.User{Username: "signature-user", Password: "fixture", AffCode: "signature-user", Group: "default", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Quota: 1000000}
	require.NoError(t, db.Create(&user).Error)
	var tokenBytes [24]byte
	_, err = rand.Read(tokenBytes[:])
	require.NoError(t, err)
	token := model.Token{UserId: user.Id, Key: hex.EncodeToString(tokenBytes[:]), Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, ModelLimitsEnabled: true, ModelLimits: "doubao-seedance-2-0-260128"}
	require.NoError(t, db.Create(&token).Error)
	require.NoError(t, db.AutoMigrate(&model.TokenSignatureCredential{}))
	require.NoError(t, db.AutoMigrate(&model.TokenSignatureCredential{}))
	var original model.Token
	require.NoError(t, db.First(&original, token.Id).Error)
	assert.Equal(t, token.Key, original.Key)

	router := gin.New()
	SetApiRouter(router)
	SetRelayRouter(router)
	const videoPath = "/doubao/api/v3/contents/generations/tasks"
	router.POST(videoPath, middleware.TokenAuth(), func(c *gin.Context) {
		body, readErr := io.ReadAll(c.Request.Body)
		if readErr != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.JSON(http.StatusOK, gin.H{"user_id": c.GetInt("id"), "token_id": c.GetInt("token_id"), "model_limited": c.GetBool("token_model_limit_enabled"), "body": string(body)})
	})
	router.NoRoute(seedanceCompatAssetReferenceDispatcher())

	call := func(request *http.Request) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}
	issue := func() (string, string) {
		request := httptest.NewRequest(http.MethodPost, "/api/token/aksk", strings.NewReader(fmt.Sprintf(`{"token_id":%d}`, token.Id)))
		request.Header.Set("Authorization", "Bearer sk-"+token.Key)
		request.Header.Set("Content-Type", "application/json")
		response := call(request)
		require.Equal(t, http.StatusOK, response.Code)
		var payload struct {
			Success bool `json:"success"`
			Data    struct {
				AccessKey string `json:"access_key"`
				SecretKey string `json:"secret_key"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
		require.True(t, payload.Success)
		require.NotEmpty(t, payload.Data.AccessKey)
		require.NotEmpty(t, payload.Data.SecretKey)
		return payload.Data.AccessKey, payload.Data.SecretKey
	}
	accessKey, secretKey := issue()
	credential, err := model.GetTokenSignatureCredential(t.Context(), token.Id)
	require.NoError(t, err)
	assert.NotContains(t, credential.SecretCiphertext, secretKey)
	statusRequest := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/token/aksk?token_id=%d", token.Id), nil)
	statusRequest.Header.Set("Authorization", "Bearer sk-"+token.Key)
	status := call(statusRequest)
	require.Equal(t, http.StatusOK, status.Code)
	assert.NotContains(t, status.Body.String(), "secret_key")
	foreignRequest := httptest.NewRequest(http.MethodPost, "/api/token/aksk", strings.NewReader(fmt.Sprintf(`{"token_id":%d}`, token.Id+1)))
	foreignRequest.Header.Set("Authorization", "Bearer sk-"+token.Key)
	foreignRequest.Header.Set("Content-Type", "application/json")
	assert.Equal(t, http.StatusForbidden, call(foreignRequest).Code)
	oversized := httptest.NewRequest(http.MethodPost, "/api/token/aksk", strings.NewReader(`{}`+strings.Repeat(" ", 4096)))
	oversized.Header.Set("Authorization", "Bearer sk-"+token.Key)
	oversized.Header.Set("Content-Type", "application/json")
	assert.Equal(t, http.StatusRequestEntityTooLarge, call(oversized).Code)

	body := []byte(`{"model":"doubao-seedance-2-0-260128","prompt":"test"}`)
	signed := func(method, target string, input []byte, access, secret string, at time.Time) *http.Request {
		request := httptest.NewRequest(method, target, bytes.NewReader(input))
		signSeedanceClientRequest(t, request, input, access, secret, at, true)
		return request
	}
	response := call(signed(http.MethodPost, "https://example.test"+videoPath+"?z=a%20b&a=1", body, accessKey, secretKey, time.Now()))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var video struct {
		UserID       int    `json:"user_id"`
		TokenID      int    `json:"token_id"`
		ModelLimited bool   `json:"model_limited"`
		Body         string `json:"body"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &video))
	assert.Equal(t, user.Id, video.UserID)
	assert.Equal(t, token.Id, video.TokenID)
	assert.True(t, video.ModelLimited)
	assert.Equal(t, string(body), video.Body)
	bearerRequest := httptest.NewRequest(http.MethodPost, videoPath, bytes.NewReader(body))
	bearerRequest.Header.Set("Authorization", "Bearer sk-"+token.Key)
	assert.Equal(t, http.StatusOK, call(bearerRequest).Code)
	withoutHash := httptest.NewRequest(http.MethodPost, "https://example.test"+videoPath, bytes.NewReader(body))
	signSeedanceClientRequest(t, withoutHash, body, accessKey, secretKey, time.Now(), false)
	assert.Equal(t, http.StatusOK, call(withoutHash).Code)

	productionRouter := gin.New()
	SetRelayRouter(productionRouter)
	registry := jsplugin.NewRegistry()
	source, err := plugins.Source("doubao")
	require.NoError(t, err)
	loaded, err := jsplugin.CompilePlugin(source, jsplugin.Options{Key: "doubao"})
	require.NoError(t, err)
	require.NoError(t, registry.ReplaceOverrides([]*jsplugin.LoadedPlugin{loaded}))
	builder := newPluginGenerationBuilder(productionRouter.Routes(), nil, productionPluginRouteHandlers)
	require.NoError(t, registry.SetGenerationPreparer(builder.prepare))
	productionRouter.NoRoute(seedanceCompatAssetReferenceDispatcher(), (&pluginRouteDispatcher{registry: registry}).dispatch)
	productionQuery := signed(http.MethodGet, "https://example.test"+videoPath+"/task_missing", nil, accessKey, secretKey, time.Now())
	productionResult := httptest.NewRecorder()
	productionRouter.ServeHTTP(productionResult, productionQuery)
	assert.Equal(t, http.StatusNotFound, productionResult.Code, productionResult.Body.String())
	assert.NotEqual(t, "404 page not found", productionResult.Body.String())
	for _, alias := range []string{"/doubao/contents/generations/tasks", "/doubao/v1/contents/generations/tasks"} {
		aliasQuery := signed(http.MethodGet, "https://example.test"+alias+"/task_missing", nil, accessKey, secretKey, time.Now())
		aliasResult := httptest.NewRecorder()
		productionRouter.ServeHTTP(aliasResult, aliasQuery)
		assert.Equal(t, http.StatusNotFound, aliasResult.Code, aliasResult.Body.String())
		assert.NotEqual(t, "404 page not found", aliasResult.Body.String())
		invalid := signed(http.MethodGet, "https://example.test"+alias+"/task_missing", nil, accessKey, secretKey, time.Now())
		invalid.URL.RawQuery = "changed=1"
		invalid.RequestURI = invalid.URL.RequestURI()
		invalidResult := httptest.NewRecorder()
		productionRouter.ServeHTTP(invalidResult, invalid)
		assert.Equal(t, http.StatusUnauthorized, invalidResult.Code)
		invalidBody := []byte(`{"prompt":"test"}`)
		submit := signed(http.MethodPost, "https://example.test"+alias, invalidBody, accessKey, secretKey, time.Now())
		submitResult := httptest.NewRecorder()
		productionRouter.ServeHTTP(submitResult, submit)
		assert.Equal(t, http.StatusBadRequest, submitResult.Code, submitResult.Body.String())
	}

	groups := []model.SeedanceAssetGroup{
		{UserID: user.Id, ChannelID: 1, GroupID: "group-signature-a", Name: "signature-a", GroupType: "AIGC", Status: "Active"},
		{UserID: user.Id, ChannelID: 1, GroupID: "group-signature-b", Name: "signature-b", GroupType: "LivenessFace", Status: "Active"},
	}
	require.NoError(t, db.Create(&groups).Error)
	assets := []model.SeedanceAsset{
		{UserID: user.Id, ChannelID: 1, GroupID: groups[0].GroupID, AssetID: "asset-signature-a", Name: "signature-a", AssetType: "Image", Status: "Active"},
		{UserID: user.Id, ChannelID: 1, GroupID: groups[1].GroupID, AssetID: "asset-signature-b", Name: "signature-b", AssetType: "Image", Status: "Active"},
	}
	require.NoError(t, db.Create(&assets).Error)
	var firstPage struct {
		Result struct {
			Items     []map[string]any `json:"Items"`
			NextToken string           `json:"NextToken"`
		} `json:"Result"`
	}
	for _, path := range []string{"/doubao/api/ark/assets", "/doubao/v1/ark/asset", "/doubao/api/v3/asset", "/doubao", "/doubao/"} {
		input := []byte(`{"MaxResults":1,"ProjectName":"default"}`)
		request := signed(http.MethodPost, "https://example.test"+path+"?Action=ListAssetGroups", input, accessKey, secretKey, time.Now())
		result := call(request)
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		require.NoError(t, common.Unmarshal(result.Body.Bytes(), &firstPage))
		assert.Len(t, firstPage.Result.Items, 1)
		assert.NotEmpty(t, firstPage.Result.NextToken)
		tampered := signed(http.MethodPost, "https://example.test"+path+"?Action=ListAssetGroups", input, accessKey, secretKey, time.Now())
		tampered.URL.RawQuery = "Action=DeleteAssetGroup"
		tampered.RequestURI = tampered.URL.RequestURI()
		assert.Equal(t, http.StatusUnauthorized, call(tampered).Code)
	}
	nextBody, err := common.Marshal(gin.H{"MaxResults": 1, "NextToken": firstPage.Result.NextToken})
	require.NoError(t, err)
	nextResult := call(signed(http.MethodPost, "https://example.test/doubao/api/ark/assets?Action=ListAssetGroups", nextBody, accessKey, secretKey, time.Now()))
	require.Equal(t, http.StatusOK, nextResult.Code, nextResult.Body.String())
	var nextPage struct {
		Result struct {
			Items     []map[string]any `json:"Items"`
			NextToken string           `json:"NextToken"`
		} `json:"Result"`
	}
	require.NoError(t, common.Unmarshal(nextResult.Body.Bytes(), &nextPage))
	require.Len(t, nextPage.Result.Items, 1)
	assert.NotEqual(t, firstPage.Result.Items[0]["Id"], nextPage.Result.Items[0]["Id"])
	assert.Empty(t, nextPage.Result.NextToken)
	filteredBody := []byte(`{"Filter":{"GroupType":"LivenessFace","GroupIds":["group-signature-b"]}}`)
	filtered := call(signed(http.MethodPost, "https://example.test/doubao/api/ark/assets?Action=ListAssets", filteredBody, accessKey, secretKey, time.Now()))
	require.Equal(t, http.StatusOK, filtered.Code, filtered.Body.String())
	assert.Contains(t, filtered.Body.String(), "asset-signature-b")
	assert.NotContains(t, filtered.Body.String(), "asset-signature-a")
	invalidToken := []byte(`{"NextToken":"invalid"}`)
	assert.Equal(t, http.StatusBadRequest, call(signed(http.MethodPost, "https://example.test/doubao/api/ark/assets?Action=ListAssetGroups", invalidToken, accessKey, secretKey, time.Now())).Code)
	otherProject := []byte(`{"ProjectName":"other"}`)
	assert.Equal(t, http.StatusBadRequest, call(signed(http.MethodPost, "https://example.test/doubao/api/ark/assets?Action=ListAssets", otherProject, accessKey, secretKey, time.Now())).Code)

	assetResponse := call(signed(http.MethodGet, "https://example.test/doubao/api/assets/list", nil, accessKey, secretKey, time.Now()))
	assert.Equal(t, http.StatusOK, assetResponse.Code, assetResponse.Body.String())
	missingAsset := call(signed(http.MethodGet, "https://example.test/doubao/api/assets/asset://asset-missing", nil, accessKey, secretKey, time.Now()))
	assert.Equal(t, http.StatusNotFound, missingAsset.Code)
	assert.Contains(t, missingAsset.Body.String(), "asset resource not found")

	badBody := signed(http.MethodPost, "https://example.test"+videoPath, body, accessKey, secretKey, time.Now())
	badBody.Body = io.NopCloser(strings.NewReader(`{"model":"tampered"}`))
	assert.Equal(t, http.StatusUnauthorized, call(badBody).Code)
	badPath := signed(http.MethodPost, "https://example.test"+videoPath+"?a=1", body, accessKey, secretKey, time.Now())
	badPath.URL.RawQuery = "a=2"
	badPath.RequestURI = badPath.URL.RequestURI()
	assert.Equal(t, http.StatusUnauthorized, call(badPath).Code)
	assert.Equal(t, http.StatusUnauthorized, call(signed(http.MethodPost, "https://example.test"+videoPath, body, accessKey, secretKey, time.Now().Add(-16*time.Minute))).Code)
	assert.Equal(t, http.StatusUnauthorized, call(signed(http.MethodPost, "https://example.test"+videoPath, body, accessKey, secretKey, time.Now().Add(16*time.Minute))).Code)
	assert.Equal(t, http.StatusUnauthorized, call(signed(http.MethodPost, "https://example.test"+videoPath, body, strings.ToUpper(accessKey), secretKey, time.Now())).Code)
	wrongHash := signed(http.MethodPost, "https://example.test"+videoPath, body, accessKey, secretKey, time.Now())
	wrongHash.Header.Set("X-Content-Sha256", strings.Repeat("0", 64))
	assert.Equal(t, http.StatusUnauthorized, call(wrongHash).Code)

	var compressed bytes.Buffer
	compressor := gzip.NewWriter(&compressed)
	_, err = compressor.Write(body)
	require.NoError(t, err)
	require.NoError(t, compressor.Close())
	compressedRequest := httptest.NewRequest(http.MethodPost, "https://example.test"+videoPath, bytes.NewReader(compressed.Bytes()))
	compressedRequest.Header.Set("Content-Encoding", "gzip")
	signSeedanceClientRequest(t, compressedRequest, compressed.Bytes(), accessKey, secretKey, time.Now(), true)
	compressedResponse := call(compressedRequest)
	require.Equal(t, http.StatusOK, compressedResponse.Code, compressedResponse.Body.String())
	require.NoError(t, common.Unmarshal(compressedResponse.Body.Bytes(), &video))
	assert.Equal(t, string(body), video.Body)

	newAccess, newSecret := issue()
	assert.NotEqual(t, accessKey, newAccess)
	assert.Equal(t, http.StatusUnauthorized, call(signed(http.MethodPost, "https://example.test"+videoPath, body, accessKey, secretKey, time.Now())).Code)
	assert.Equal(t, http.StatusOK, call(signed(http.MethodPost, "https://example.test"+videoPath, body, newAccess, newSecret, time.Now())).Code)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusDisabled).Error)
	assert.Equal(t, http.StatusUnauthorized, call(signed(http.MethodPost, "https://example.test"+videoPath, body, newAccess, newSecret, time.Now())).Code)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("status", common.TokenStatusEnabled).Error)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("allow_ips", "203.0.113.1").Error)
	assert.Equal(t, http.StatusForbidden, call(signed(http.MethodPost, "https://example.test"+videoPath, body, newAccess, newSecret, time.Now())).Code)
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", token.Id).Update("allow_ips", "").Error)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error)
	assert.Equal(t, http.StatusForbidden, call(signed(http.MethodPost, "https://example.test"+videoPath, body, newAccess, newSecret, time.Now())).Code)
	require.NoError(t, db.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusEnabled).Error)

	revoke := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/token/aksk?token_id=%d", token.Id), nil)
	revoke.Header.Set("Authorization", "Bearer sk-"+token.Key)
	assert.Equal(t, http.StatusOK, call(revoke).Code)
	assert.Equal(t, http.StatusUnauthorized, call(signed(http.MethodPost, "https://example.test"+videoPath, body, newAccess, newSecret, time.Now())).Code)
	finalAccess, _ := issue()
	require.NotEmpty(t, finalAccess)
	require.NoError(t, token.Delete())
	_, err = model.GetTokenSignatureCredentialByAccessKey(t.Context(), finalAccess)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)

	deletedUser := model.User{Username: "signature-deleted-user", Password: "fixture", AffCode: "signature-deleted-user", Group: "default", Status: common.UserStatusEnabled, Role: common.RoleCommonUser}
	require.NoError(t, db.Create(&deletedUser).Error)
	deletedToken := model.Token{UserId: deletedUser.Id, Key: "deleted-user-fixture", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1}
	require.NoError(t, db.Create(&deletedToken).Error)
	deletedCredential, _, err := model.IssueTokenSignatureCredential(t.Context(), deletedUser.Id, deletedToken.Id)
	require.NoError(t, err)
	_, err = deletedUser.HardDeleteWithActor(user.Id)
	require.NoError(t, err)
	_, err = model.GetTokenSignatureCredentialByAccessKey(t.Context(), deletedCredential.AccessKey)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
