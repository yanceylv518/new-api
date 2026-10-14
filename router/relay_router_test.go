package router

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelBodyLimitRouting(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.ChannelResourcePolicy{}))
	previousMemory := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemory })
	small := model.Channel{Name: "small", Type: constant.ChannelTypeOpenAI, Key: "fixture", Models: "body-model", Group: "default", Priority: common.GetPointer(int64(100))}
	large := model.Channel{Name: "large", Type: constant.ChannelTypeOpenAI, Key: "fixture", Models: "body-model", Group: "default", Priority: common.GetPointer(int64(10))}
	require.NoError(t, small.Insert())
	require.NoError(t, large.Insert())
	require.NoError(t, model.SetChannelResourcePolicy(context.Background(), model.ChannelResourcePolicy{ChannelID: small.Id, MaxRequestBodyBytes: 512}))
	require.NoError(t, model.SetChannelResourcePolicy(context.Background(), model.ChannelResourcePolicy{ChannelID: large.Id, MaxRequestBodyBytes: 2048}))
	affinity := operation_setting.GetChannelAffinitySetting()
	previousAffinity := *affinity
	*affinity = operation_setting.ChannelAffinitySetting{Enabled: true, SwitchOnSuccess: true, DefaultTTLSeconds: 60, Rules: []operation_setting.ChannelAffinityRule{{
		Name: t.Name(), ModelRegex: []string{"^body-model$"}, PathRegex: []string{"/v1/chat/completions"}, IncludeRuleName: true,
		SkipRetryOnFailure: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Body-Affinity"}},
	}}}
	t.Cleanup(func() { *affinity = previousAffinity })
	engine := gin.New()
	engine.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup())
	engine.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		if c.GetHeader("X-Pin") != "" {
			service.GetChannelConstraints(c).AddPin(dto.ChannelPin{ChannelId: small.Id, Source: dto.PinSourceToken})
		}
		if c.GetHeader("X-Rewrite") != "" {
			_, err := common.GetBodyStorage(c)
			require.NoError(t, err)
			common.CleanupBodyStorage(c)
			storage, err := common.CreateBodyStorage([]byte(`{"model":"body-model"}`))
			require.NoError(t, err)
			c.Set(common.KeyBodyStorage, storage)
		}
	})
	engine.Use(middleware.Distribute())
	engine.Any("/*path", func(c *gin.Context) {
		c.Header("X-Selected-Channel", fmt.Sprint(c.GetInt("channel_id")))
		if c.GetHeader("X-Check-Retry") != "" {
			ch, _, err := service.CacheGetRandomSatisfiedChannel(&service.RetryParam{Ctx: c, TokenGroup: "default", ModelName: "body-model", Retry: common.GetPointer(1)})
			require.NoError(t, err)
			require.NotNil(t, ch)
			assert.Equal(t, large.Id, ch.Id)
			assert.False(t, service.ShouldSkipRetryAfterChannelAffinityFailure(c))
		}
		c.Status(http.StatusNoContent)
	})
	body := `{"model":"body-model"}`
	for _, tc := range []struct {
		name            string
		size            int
		encoding        string
		pin, rewrite    bool
		status, channel int
	}{
		{"boundary", 512, "", false, false, 204, small.Id},
		{"large uses fallback", 513, "", false, false, 204, large.Id},
		{"chunked gzip uses decoded size", 513, "gzip", false, false, 204, large.Id},
		{"all candidates too small", 2049, "", false, false, 413, 0},
		{"explicit channel cannot fall back", 513, "", true, false, 413, 0},
		{"model rewrite preserves original size", 2049, "", false, true, 413, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := body + strings.Repeat(" ", tc.size-len(body))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encodeResourceBody(t, payload, tc.encoding)))
			req.Header.Set("Content-Type", "application/json")
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
				req.ContentLength = -1
			}
			if tc.pin {
				req.Header.Set("X-Pin", "yes")
			}
			if tc.rewrite {
				req.Header.Set("X-Rewrite", "yes")
			}
			rec := httptest.NewRecorder()
			engine.ServeHTTP(rec, req)
			assert.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.channel != 0 {
				assert.Equal(t, fmt.Sprint(tc.channel), rec.Header().Get("X-Selected-Channel"))
			} else {
				assert.Contains(t, rec.Body.String(), string(service.ChannelBodyTooLargeCode))
			}
		})
	}
	// The large request must neither delete nor replace the original affinity.
	for _, size := range []int{512, 513, 512} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body+strings.Repeat(" ", size-len(body))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Body-Affinity", t.Name())
		want := small.Id
		if size > 512 {
			want = large.Id
			req.Header.Set("X-Check-Retry", "yes")
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, fmt.Sprint(want), rec.Header().Get("X-Selected-Channel"))
	}
	// Fetches and websocket handshakes do not acquire an HTTP body constraint.
	for _, path := range []string{"/v1/videos/task-id", "/v1/realtime?model=body-model"} {
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(strings.Repeat("x", 2049)))
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}
	// Multipart overhead belongs to the incoming size, even for disk storage.
	common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: true, ThresholdMB: 0, MaxSizeMB: 10, Path: t.TempDir()})
	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	require.NoError(t, writer.WriteField("model", "body-model"))
	part, err := writer.CreateFormFile("image", "input.png")
	require.NoError(t, err)
	_, err = part.Write(bytes.Repeat([]byte("x"), 400))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	for _, path := range []string{"/v1/images/edits", "/v1beta/models/body-model:generateContent"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(multipartBody.Bytes()))
		req.Header.Set("Content-Type", writer.FormDataContentType())
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		assert.Equal(t, fmt.Sprint(large.Id), rec.Header().Get("X-Selected-Channel"))
	}
	entries, err := os.ReadDir(common.GetDiskCacheDir())
	require.NoError(t, err)
	assert.Empty(t, entries)
	// Channel rejection must release the key's concurrency slot.
	setupTokenConcurrencyRedis(t)
	noBodyLimit, one := int64(0), 1
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id, MaxRequestBodyBytes: &noBodyLimit, MaxConcurrentRequests: &one}))
	limited := gin.New()
	limited.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	}, middleware.TokenResourceLimits(), middleware.Distribute())
	limited.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	for _, size := range []int{2049, 512} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body+strings.Repeat(" ", size-len(body))))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		limited.ServeHTTP(rec, req)
		want := http.StatusNoContent
		if size == 2049 {
			want = http.StatusRequestEntityTooLarge
		}
		assert.Equal(t, want, rec.Code, rec.Body.String())
	}
	// Auto groups continue past a size rejection and return 413 only when exhausted.
	previousGroups := setting.UserUsableGroups2JSONString()
	previousRatios := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
	})
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2}`))
	require.NoError(t, model.DB.Model(&small).Update("group", "vip").Error)
	require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", small.Id).Update("group", "vip").Error)
	for _, size := range []int64{513, 2049} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
		service.GetChannelConstraints(c).AddFilter(dto.ChannelFilter{Kind: dto.FilterRequestBodySize, RequestBodyBytes: size})
		ch, group, err := service.CacheGetRandomSatisfiedChannel(&service.RetryParam{Ctx: c, TokenGroup: "auto", ModelName: "body-model"})
		if size == 513 {
			require.NoError(t, err)
			require.NotNil(t, ch)
			assert.Equal(t, large.Id, ch.Id)
			assert.Equal(t, "default", group)
		} else {
			assert.Nil(t, ch)
			assert.ErrorIs(t, err, model.ErrChannelRequestBodyTooLarge)
		}
	}
}

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}))

	t.Cleanup(func() {
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}

func setupTokenResourceBodyTest(t *testing.T) model.Token {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	t.Cleanup(func() { model.DB, model.LOG_DB = previousDB, previousLogDB })
	setupRelayRouterTestDB(t)
	require.NoError(t, model.DB.AutoMigrate(&model.TokenResourcePolicy{}, &model.Option{}))
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = map[string]string{
		setting.TokenResourceLimitsEnabledKey:              "true",
		setting.TokenResourceDefaultMaxRequestBodyBytesKey: "64",
	}
	common.OptionMapRWMutex.Unlock()
	previousRateLimit, previousMaxBody := setting.ModelRequestRateLimitEnabled, constant.MaxRequestBodyMB
	previousDisk := common.GetDiskCacheConfig()
	setting.ModelRequestRateLimitEnabled = false
	constant.MaxRequestBodyMB = 1
	common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: false})
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
		setting.ModelRequestRateLimitEnabled = previousRateLimit
		constant.MaxRequestBodyMB = previousMaxBody
		common.SetDiskCacheConfig(previousDisk)
	})
	user := model.User{Username: "resource-user", Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "resourcebodytestkey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, model.DB.Create(&token).Error)
	return token
}

func encodeResourceBody(t *testing.T, body, encoding string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&buffer)
	case "br":
		writer = brotli.NewWriter(&buffer)
	case "zstd":
		var err error
		writer, err = zstd.NewWriter(&buffer)
		require.NoError(t, err)
	default:
		return []byte(body)
	}
	_, err := io.WriteString(writer, body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func TestTokenResourceBodyBoundariesAndReplay(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	body := `{"input":"` + strings.Repeat("x", 52) + `"}`
	require.Len(t, body, 64)
	for _, test := range []struct {
		name, body, encoding string
		contentLength        int64
		wantStatus           int
	}{
		{name: "below", body: strings.Replace(body, "xx", "x", 1), wantStatus: 200},
		{name: "exact", body: body, wantStatus: 200},
		{name: "one_byte_over", body: body + " ", wantStatus: 413},
		{name: "chunked_exact", body: body, contentLength: -1, wantStatus: 200},
		{name: "chunked_over", body: body + " ", contentLength: -1, wantStatus: 413},
		{name: "understated_length", body: body + " ", contentLength: 1, wantStatus: 413},
		{name: "overstated_length", body: body, contentLength: 4096, wantStatus: 200},
		{name: "gzip_exact", body: body, encoding: "gzip", wantStatus: 200},
		{name: "gzip_expansion", body: body + strings.Repeat(" ", 256), encoding: "gzip", wantStatus: 413},
		{name: "brotli_exact", body: body, encoding: "br", wantStatus: 200},
		{name: "brotli_expansion", body: body + strings.Repeat(" ", 256), encoding: "br", wantStatus: 413},
		{name: "zstd_exact", body: body, encoding: "zstd", wantStatus: 200},
		{name: "zstd_expansion", body: body + strings.Repeat(" ", 256), encoding: "zstd", wantStatus: 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := gin.New()
			engine.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.TokenResourceLimits())
			called := false
			var storage common.BodyStorage
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				called = true
				var err error
				storage, err = common.GetBodyStorage(c)
				require.NoError(t, err)
				data, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				assert.Equal(t, test.body, string(data))
				for range 2 {
					var payload map[string]string
					require.NoError(t, common.UnmarshalBodyReusable(c, &payload))
					assert.NotEmpty(t, payload["input"])
					cached, err := common.GetBodyStorage(c)
					require.NoError(t, err)
					assert.Same(t, storage, cached, "downstream parsers must reuse the existing storage")
				}
				c.Status(http.StatusOK)
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encodeResourceBody(t, test.body, test.encoding)))
			request.Header.Set("Authorization", "Bearer "+token.Key)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Content-Encoding", test.encoding)
			if test.contentLength != 0 {
				request.ContentLength = test.contentLength
			}
			if test.contentLength == -1 {
				request.TransferEncoding = []string{"chunked"}
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			assert.Equal(t, test.wantStatus, recorder.Code, recorder.Body.String())
			assert.Equal(t, test.wantStatus == 200, called)
			if called {
				_, err := storage.Bytes()
				assert.ErrorIs(t, err, common.ErrStorageClosed)
			} else {
				assert.Contains(t, recorder.Body.String(), "request_body_too_large")
				assert.NotContains(t, recorder.Body.String(), token.Key)
				assert.NotContains(t, recorder.Body.String(), "64")
			}
		})
	}
}

func TestTokenResourcePolicyAdminAPI(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.AuditLog{}))
	credentials := map[int]string{}
	users := map[int]model.User{}
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
		credential := fmt.Sprintf("resource-policy-dashboard-%d", role)
		user := model.User{Username: fmt.Sprintf("policy-actor-%d", role), AffCode: fmt.Sprintf("policy%d", role), Role: role, Status: common.UserStatusEnabled, AccessToken: &credential}
		require.NoError(t, model.DB.Create(&user).Error)
		credentials[role], users[role] = credential, user
	}
	engine := gin.New()
	SetApiRouter(engine)
	base := fmt.Sprintf("/api/user/%d/token-resource-policies", token.UserId)
	path := fmt.Sprintf("%s/%d", base, token.Id)
	request := func(method, path, credential, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	payload := `{"max_request_body_bytes":1024,"max_concurrent_requests":2}`
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		url := base
		if method == http.MethodPut {
			url = path
		}
		assert.Equal(t, http.StatusUnauthorized, request(method, url, "", payload).Code)
		assert.Equal(t, http.StatusForbidden, request(method, url, credentials[common.RoleCommonUser], payload).Code)
	}
	admin := credentials[common.RoleAdminUser]
	rec := request(http.MethodPut, path, admin, payload)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	policy, err := model.GetTokenResourcePolicy(context.Background(), token.Id)
	require.NoError(t, err)
	require.NotNil(t, policy)
	assert.Equal(t, int64(1024), *policy.MaxRequestBodyBytes)
	assert.Equal(t, 2, *policy.MaxConcurrentRequests)
	rec = request(http.MethodGet, base+"?p=1&page_size=1", admin, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
	assert.Contains(t, rec.Body.String(), `"max_concurrent_requests":2`)
	assert.NotContains(t, rec.Body.String(), token.Key)
	assert.NotContains(t, rec.Body.String(), `"key"`)
	// Admin defaults are a single configured snapshot, including when disabled.
	for _, enabled := range []string{"true", "false"} {
		common.OptionMapRWMutex.Lock()
		common.OptionMap[setting.TokenResourceLimitsEnabledKey] = enabled
		common.OptionMap[setting.TokenResourceDefaultMaxRequestBodyBytesKey] = "31457280"
		common.OptionMap[setting.TokenResourceDefaultMaxConcurrentRequestsKey] = "0"
		common.OptionMapRWMutex.Unlock()
		rec = request(http.MethodGet, base, admin, "")
		require.Equal(t, http.StatusOK, rec.Code)
		var response struct {
			Data struct {
				Total    int                                 `json:"total"`
				Defaults *setting.TokenResourceLimitSettings `json:"defaults"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &response))
		require.NotNil(t, response.Data.Defaults)
		assert.Equal(t, enabled == "true", response.Data.Defaults.Enabled)
		assert.Equal(t, int64(31457280), response.Data.Defaults.DefaultMaxRequestBodyBytes)
		assert.Zero(t, response.Data.Defaults.DefaultMaxConcurrentRequests)
		assert.Equal(t, 1, response.Data.Total)
	}
	common.OptionMapRWMutex.Lock()
	common.OptionMap[setting.TokenResourceLimitsEnabledKey] = "true"
	common.OptionMapRWMutex.Unlock()
	// Existing user-facing endpoints must never include the private policy.
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", token.UserId).Updates(map[string]any{"role": common.RoleCommonUser, "access_token": "resource-owner-pat"}).Error)
	rec = request(http.MethodGet, fmt.Sprintf("/api/token/%d", token.Id), "resource-owner-pat", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "max_request_body_bytes")
	assert.NotContains(t, rec.Body.String(), "max_concurrent_requests")
	for _, body := range []string{
		`{}`, `{"max_request_body_bytes":1}`,
		`{"max_request_body_bytes":-1,"max_concurrent_requests":2}`,
		`{"max_request_body_bytes":1,"max_concurrent_requests":2147483648}`,
		`{"max_request_body_bytes":1.5,"max_concurrent_requests":2}`,
		payload + "{}",
		`{"token_id":999,"max_request_body_bytes":1,"max_concurrent_requests":2}`,
	} {
		assert.Equal(t, http.StatusBadRequest, request(http.MethodPut, path, admin, body).Code, body)
	}
	otherToken := model.Token{UserId: users[common.RoleRootUser].Id, Key: "private-root-api-key"}
	require.NoError(t, model.DB.Create(&otherToken).Error)
	assert.Equal(t, http.StatusNotFound, request(http.MethodPut, fmt.Sprintf("%s/%d", base, otherToken.Id), admin, payload).Code)
	for _, role := range []int{common.RoleAdminUser, common.RoleRootUser} {
		rec = request(http.MethodGet, fmt.Sprintf("/api/user/%d/token-resource-policies", users[role].Id), admin, "")
		assert.Contains(t, rec.Body.String(), `"success":false`, "admin cannot manage same/higher role")
	}
	rootPath := fmt.Sprintf("/api/user/%d/token-resource-policies/%d", otherToken.UserId, otherToken.Id)
	assert.Equal(t, http.StatusOK, request(http.MethodPut, rootPath, credentials[common.RoleRootUser], payload).Code)
	assert.Equal(t, http.StatusOK, request(http.MethodPut, path, admin, `{"max_request_body_bytes":null,"max_concurrent_requests":0}`).Code)
	policy, err = model.GetTokenResourcePolicy(context.Background(), token.Id)
	require.NoError(t, err)
	require.NotNil(t, policy)
	assert.Nil(t, policy.MaxRequestBodyBytes)
	assert.Equal(t, 0, *policy.MaxConcurrentRequests)
	assert.Equal(t, http.StatusOK, request(http.MethodPut, path, admin, `{"max_request_body_bytes":null,"max_concurrent_requests":null}`).Code)
	policy, err = model.GetTokenResourcePolicy(context.Background(), token.Id)
	require.NoError(t, err)
	assert.Nil(t, policy)
	// Global configuration retains its root-only boundary.
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser} {
		assert.Equal(t, http.StatusForbidden, request(http.MethodPut, "/api/option/", credentials[role], `{"key":"TokenResourceDefaultMaxConcurrentRequests","value":"3"}`).Code)
	}
	var auditCount int64
	require.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).Where("user_id = ? AND category = ?", users[common.RoleAdminUser].Id, "operation").Count(&auditCount).Error)
	assert.Positive(t, auditCount, "admin writes must leave an audit trail")
	assert.Equal(t, http.StatusBadRequest, request(http.MethodGet, base+"?page_size=-1", admin, "").Code)
	// Confirm a successful root option write persists, and a later storage
	// failure does not silently alter the instance's in-memory policy.
	rec = request(http.MethodPut, "/api/option/", credentials[common.RoleRootUser], `{"key":"TokenResourceDefaultMaxConcurrentRequests","value":"3"}`)
	require.Contains(t, rec.Body.String(), `"success":true`)
	var saved model.Option
	require.NoError(t, model.DB.Where("key = ?", setting.TokenResourceDefaultMaxConcurrentRequestsKey).Take(&saved).Error)
	assert.Equal(t, "3", saved.Value)
	require.NoError(t, model.DB.Migrator().DropTable(&model.Option{}))
	assert.Error(t, model.UpdateOption(setting.TokenResourceDefaultMaxConcurrentRequestsKey, "9"))
	settings, err := setting.GetTokenResourceLimitSettings()
	require.NoError(t, err)
	assert.Equal(t, 3, settings.DefaultMaxConcurrentRequests)
}

func TestTokenResourceBodyPoliciesAndFailures(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	engine := gin.New()
	engine.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.TokenResourceLimits())
	engine.POST("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token.Key)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		return recorder.Code
	}
	body := strings.Repeat("x", 65)
	assert.Equal(t, 413, request(body), "missing policy inherits the default")
	limit := int64(100)
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id, MaxRequestBodyBytes: &limit}))
	assert.Equal(t, 204, request(body), "a key override replaces the default")
	limit = 1
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id, MaxRequestBodyBytes: &limit}))
	assert.Equal(t, 413, request(body), "policy updates are visible on the next request")
	limit = 0
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id, MaxRequestBodyBytes: &limit}))
	assert.Equal(t, 204, request(body), "explicit zero disables the per-key body cap")
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id}))
	assert.Equal(t, 413, request(body), "reset restores inheritance")
	assert.Equal(t, 204, request(""), "empty bodies can proceed to existing request validation")

	for _, value := range []string{"-1", "1.5", "9223372036854775808"} {
		assert.Error(t, model.UpdateOption(setting.TokenResourceDefaultMaxRequestBodyBytesKey, value))
	}
	assert.Error(t, model.UpdateOption(setting.TokenResourceLimitsEnabledKey, "invalid"))
	require.NoError(t, model.UpdateOption(setting.TokenResourceDefaultMaxRequestBodyBytesKey, "100"))
	assert.Equal(t, 204, request(body), "updated defaults take effect without a restart")

	require.NoError(t, model.DB.Migrator().DropTable(&model.TokenResourcePolicy{}))
	assert.Equal(t, 503, request(body), "an unavailable policy table must not silently allow a request")
	require.NoError(t, model.UpdateOption(setting.TokenResourceLimitsEnabledKey, "false"))
	assert.Equal(t, 204, request(body), "disabled mode must not query the missing policy table")
	common.OptionMapRWMutex.Lock()
	delete(common.OptionMap, setting.TokenResourceLimitsEnabledKey)
	common.OptionMapRWMutex.Unlock()
	assert.Equal(t, 204, request(body), "an old configuration defaults to disabled")
}

func TestTokenResourceBodyRouteCoverage(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	engine := gin.New()
	SetRelayRouter(engine)
	SetTaskPluginProtocolRouter(engine)
	for _, path := range []string{
		"/v1/chat/completions", "/v1/completions", "/v1/messages",
		"/v1/responses", "/v1/responses/compact", "/v1/alpha/search",
		"/v1/images/generations", "/v1/images/edits", "/v1/edits",
		"/v1/embeddings", "/v1/audio/transcriptions", "/v1/audio/translations", "/v1/audio/speech",
		"/v1/rerank", "/v1/moderations", "/v1/engines/test/embeddings",
		"/v1/models/test:generateContent", "/v1beta/models/test:generateContent",
		"/v1beta/models/test:streamGenerateContent",
	} {
		t.Run(path, func(t *testing.T) {
			// Invalid JSON would otherwise fail in the parser: a 413 proves
			// admission ran before plugin preparation and channel selection.
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", 65)))
			req.Header.Set("Content-Type", "application/json")
			switch {
			case path == "/v1/messages":
				req.Header.Set("x-api-key", token.Key)
			case strings.Contains(path, "/models/"):
				req.Header.Set("x-goog-api-key", token.Key)
			default:
				req.Header.Set("Authorization", "Bearer "+token.Key)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, req)
			assert.Equal(t, 413, recorder.Code, recorder.Body.String())
			assert.Contains(t, recorder.Body.String(), "request_body_too_large")
		})
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1beta/models/test:generateContent"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", 65)))
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code, "authentication must precede resource admission: %s", path)
	}
	var user model.User
	require.NoError(t, model.DB.First(&user, token.UserId).Error)
	assert.Equal(t, 100, user.Quota)
	assert.Zero(t, user.UsedQuota)
	var preserved model.Token
	require.NoError(t, model.DB.First(&preserved, token.Id).Error)
	assert.Zero(t, preserved.UsedQuota, "rejected bodies must not incur charges")
}

type observedResourceBody struct {
	io.Reader
	read   int
	closed bool
}

func (body *observedResourceBody) Read(data []byte) (int, error) {
	n, err := body.Reader.Read(data)
	body.read += n
	return n, err
}

func (body *observedResourceBody) Close() error {
	body.closed = true
	return nil
}

func TestTokenResourceBodyBoundedReadAndCleanup(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	for _, disk := range []bool{false, true} {
		t.Run(fmt.Sprintf("disk_%t", disk), func(t *testing.T) {
			common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: disk, ThresholdMB: 0, MaxSizeMB: 10, Path: t.TempDir()})
			engine := gin.New()
			engine.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.TokenResourceLimits())
			called := false
			engine.POST("/test", func(c *gin.Context) { called = true })
			body := &observedResourceBody{Reader: strings.NewReader(strings.Repeat("x", 1024))}
			req := httptest.NewRequest(http.MethodPost, "/test", body)
			req.ContentLength = 1024
			req.Header.Set("Authorization", "Bearer "+token.Key)
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, req)
			assert.Equal(t, 413, recorder.Code)
			assert.False(t, called)
			assert.Equal(t, 65, body.read, "read no more than the limit plus one byte")
			assert.True(t, body.closed)
			if disk {
				entries, err := os.ReadDir(common.GetDiskCacheDir())
				require.NoError(t, err)
				assert.Empty(t, entries, "failed reads must remove temporary files")
			}
		})
	}
}

func TestTokenResourceBodyMultipartReplayAndSystemCeiling(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	common.SetDiskCacheConfig(common.DiskCacheConfig{Enabled: true, ThresholdMB: 0, MaxSizeMB: 10, Path: t.TempDir()})
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	require.NoError(t, writer.WriteField("model", "test-model"))
	part, err := writer.CreateFormFile("image", "test.png")
	require.NoError(t, err)
	_, err = part.Write([]byte("image-bytes"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	limit := int64(buffer.Len())
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id, MaxRequestBodyBytes: &limit}))
	engine := gin.New()
	engine.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.TokenResourceLimits())
	called := false
	engine.POST("/test", func(c *gin.Context) {
		called = true
		storage, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		assert.True(t, storage.IsDisk())
		require.NoError(t, c.Request.ParseMultipartForm(1<<20))
		defer c.Request.MultipartForm.RemoveAll()
		assert.Equal(t, "test-model", c.Request.FormValue("model"))
		file, _, err := c.Request.FormFile("image")
		require.NoError(t, err)
		defer file.Close()
		data, err := io.ReadAll(file)
		require.NoError(t, err)
		assert.Equal(t, "image-bytes", string(data))
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader(buffer.Bytes()))
	req.Header.Set("Authorization", "Bearer "+token.Key)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	assert.Equal(t, 204, recorder.Code, recorder.Body.String())
	assert.True(t, called)
	entries, err := os.ReadDir(common.GetDiskCacheDir())
	require.NoError(t, err)
	assert.Empty(t, entries, "successful disk storage must also be cleaned up")

	limit = 2 << 20
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id, MaxRequestBodyBytes: &limit}))
	called = false
	req = httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	req.Header.Set("Authorization", "Bearer "+token.Key)
	recorder = httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	assert.Equal(t, 413, recorder.Code, "a key override must not raise the system ceiling")
	assert.False(t, called)
}

func setupTokenConcurrencyRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
	previousClient, previousEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousEnabled
		_ = client.Close()
	})
	return server
}

func TestTokenConcurrencyMiddlewareLifecycle(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	setupTokenConcurrencyRedis(t)
	require.NoError(t, model.UpdateOption(setting.TokenResourceDefaultMaxConcurrentRequestsKey, "1"))
	entered, canceled, exit, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var exitOnce sync.Once
	finish := func() { exitOnce.Do(func() { close(exit) }) }
	t.Cleanup(finish)
	first := gin.New()
	first.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.TokenResourceLimits())
	first.POST("/test", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: started\n\n")
		c.Writer.Flush()
		close(entered)
		<-c.Request.Context().Done()
		close(canceled)
		<-exit // Simulate upstream/billing cleanup still using the slot.
	})
	second := gin.New()
	second.Use(middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.TokenResourceLimits())
	second.POST("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := func(engine *gin.Engine, key string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/test", body)
		req.Header.Set("Authorization", "Bearer "+key)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		return recorder
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader("{}")).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token.Key)
	go func() { defer close(done); first.ServeHTTP(httptest.NewRecorder(), req) }()
	t.Cleanup(func() {
		cancel()
		finish()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("first request did not exit")
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not start")
	}
	body := &observedResourceBody{Reader: strings.NewReader(strings.Repeat("x", 65))}
	response := request(second, token.Key, body)
	assert.Equal(t, 429, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "concurrency_limit_exceeded")
	assert.Zero(t, body.read, "reject concurrency before reading a large body")
	other := model.Token{UserId: token.UserId, Key: "otherconcurrencykey", Status: common.TokenStatusEnabled, ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, model.DB.Create(&other).Error)
	assert.Equal(t, 204, request(second, other.Key, strings.NewReader("{}")).Code, "another key of the same user has independent capacity")
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not receive cancellation")
	}
	assert.Equal(t, 429, request(second, token.Key, strings.NewReader("{}")).Code, "cancellation alone must not release a busy slot")
	finish()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request cleanup did not finish")
	}
	assert.Equal(t, 204, request(second, token.Key, strings.NewReader("{}")).Code)
}

func TestTokenConcurrencyEarlyExitAndConfiguration(t *testing.T) {
	token := setupTokenResourceBodyTest(t)
	setupTokenConcurrencyRedis(t)
	require.NoError(t, model.UpdateOption(setting.TokenResourceDefaultMaxConcurrentRequestsKey, "1"))
	engine := gin.New()
	engine.Use(gin.RecoveryWithWriter(io.Discard), middleware.DecompressRequestMiddleware(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.TokenResourceLimits())
	engine.POST("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	engine.POST("/panic", func(c *gin.Context) { panic("test handler panic") })
	request := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token.Key)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)
		return recorder
	}
	assert.Equal(t, 413, request("/test", strings.Repeat("x", 65)).Code)
	assert.Equal(t, 204, request("/test", "{}").Code, "body rejection releases the lease")
	assert.Equal(t, 500, request("/panic", "{}").Code)
	assert.Equal(t, 204, request("/test", "{}").Code, "panic unwinding releases the lease")
	common.RedisEnabled = false
	assert.Equal(t, 503, request("/test", "{}").Code, "a required distributed cap cannot fall back to process-local counting")
	zero := 0
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id, MaxConcurrentRequests: &zero}))
	assert.Equal(t, 204, request("/test", "{}").Code, "explicit zero bypasses Redis concurrency")
	require.NoError(t, model.SetTokenResourcePolicy(context.Background(), model.TokenResourcePolicy{TokenID: token.Id}))
	assert.Equal(t, 503, request("/test", "{}").Code, "reset inherits the default cap again")
	for _, value := range []string{"-1", "1.5", "2147483648"} {
		assert.Error(t, model.UpdateOption(setting.TokenResourceDefaultMaxConcurrentRequestsKey, value))
	}
	require.NoError(t, model.UpdateOption(setting.TokenResourceDefaultMaxConcurrentRequestsKey, "0"))
	assert.Equal(t, 204, request("/test", "{}").Code, "body-only policies do not require Redis")
}
