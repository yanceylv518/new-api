package controller_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type zhipuSearchUpstream struct {
	mu        sync.Mutex
	status    int
	body      string
	truncated bool
	closeNext bool
	paths     []string
	bodies    []string
	auth      []string
}

func TestZhipuSearchValidationWithoutDistributor(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"valid", `{"search_engine":"search_std","search_query":"test","search_intent":false}`, true},
		{"duplicate engine", `{"search_engine":"search_std","search_engine":"search_pro","search_query":"test"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			defer common.CleanupBodyStorage(ctx)
			request, err := helper.GetAndValidateRequest(ctx, types.RelayFormatZhipuWebSearch)
			if !tc.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			search, ok := request.(*dto.ZhipuWebSearchRequest)
			require.True(t, ok)
			assert.Equal(t, tc.body, string(search.RawBody))
		})
	}
}

func (u *zhipuSearchUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "cannot read request", http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.bodies = append(u.bodies, string(body))
	u.auth = append(u.auth, r.Header.Get("Authorization"))
	status, response, truncated, closeNext := u.status, u.body, u.truncated, u.closeNext
	u.closeNext = false
	u.mu.Unlock()
	if closeNext {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if truncated {
		w.Header().Set("Content-Length", strconv.Itoa(len(response)+10))
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, response)
}

func (u *zhipuSearchUpstream) setResponse(status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.body = status, body
}

func (u *zhipuSearchUpstream) setTruncated(truncated bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.truncated = truncated
}

func (u *zhipuSearchUpstream) closeNextResponse() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closeNext = true
}

func (u *zhipuSearchUpstream) snapshot() ([]string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...), append([]string(nil), u.bodies...)
}

func (u *zhipuSearchUpstream) lastAuthorization() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.auth) == 0 {
		return ""
	}
	return u.auth[len(u.auth)-1]
}

func setupZhipuSearchContract(t *testing.T) (*gin.Engine, *gorm.DB, *zhipuSearchUpstream) {
	t.Helper()
	baselineWorkers := gopool.WorkerCount()
	require.NoError(t, i18n.Init())
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousPath := common.IsMasterNode, common.SQLitePath
	previousRedis, previousCache, previousBatch := common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled
	previousLogging, previousExport := common.LogConsumeEnabled, common.DataExportEnabled
	previousTimeout, previousRetries, previousQuotaUnit := common.RelayTimeout, common.RetryTimes, common.QuotaPerUnit
	previousCount, previousErrorLog := constant.CountToken, constant.ErrorLogEnabled
	previousPrice := ratio_setting.ModelPrice2JSONString()
	previousRate, previousMode := operation_setting.USDExchangeRate, gin.Mode()
	common.SQLitePath = "file:zhipu-web-search-contract?mode=memory&cache=shared"
	common.IsMasterNode = false
	common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled = false, false, false
	common.LogConsumeEnabled, common.DataExportEnabled = true, false
	common.RelayTimeout, common.RetryTimes, common.QuotaPerUnit = 10, 2, 500_000
	constant.CountToken, constant.ErrorLogEnabled = false, true
	common.OptionMapRWMutex.Lock()
	operation_setting.USDExchangeRate = 7.3
	common.OptionMapRWMutex.Unlock()
	gin.SetMode(gin.TestMode)
	dsn := os.Getenv("ZHIPU_SEARCH_TEST_DISPOSABLE_DSN")
	if dsn == "" {
		dsn = "local"
	}
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	database := model.DB
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		assert.Eventually(t, func() bool { return gopool.WorkerCount() <= baselineWorkers }, 5*time.Second, 10*time.Millisecond)
		_ = ratio_setting.UpdateModelPriceByJSONString(previousPrice)
		common.OptionMapRWMutex.Lock()
		operation_setting.USDExchangeRate = previousRate
		common.OptionMapRWMutex.Unlock()
		constant.CountToken, constant.ErrorLogEnabled = previousCount, previousErrorLog
		common.RelayTimeout, common.RetryTimes, common.QuotaPerUnit = previousTimeout, previousRetries, previousQuotaUnit
		common.LogConsumeEnabled, common.DataExportEnabled = previousLogging, previousExport
		common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled = previousRedis, previousCache, previousBatch
		common.IsMasterNode, common.SQLitePath = previousMaster, previousPath
		common.SetDatabaseTypes(previousMain, previousLog)
		model.DB, model.LOG_DB = previousDB, previousLogDB
		model.InvalidatePricingCache()
		gin.SetMode(previousMode)
		_ = sqlDB.Close()
	})
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}, &model.Model{}, &model.Vendor{}, &model.Option{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}))
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
	service.InitHttpClient()

	upstream := &zhipuSearchUpstream{status: http.StatusOK, body: `{"id":"upstream-id","search_result":[{"title":"result"}],"extra":{"keep":true}}`}
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	user := model.User{Id: 1, Username: "search-fixture", Status: common.UserStatusEnabled, Group: "default", Quota: 10_000_000, Setting: `{"billing_preference":"wallet_only"}`}
	token := model.Token{Id: 1, UserId: user.Id, Key: "fixturetoken", Name: "fixture", Status: common.TokenStatusEnabled, RemainQuota: 10_000_000, ExpiredTime: -1}
	limited := model.Token{Id: 2, UserId: user.Id, Key: "limitedtoken", Name: "limited", Status: common.TokenStatusEnabled, RemainQuota: 10_000_000, ExpiredTime: -1, ModelLimitsEnabled: true, ModelLimits: "search_std"}
	channel := model.Channel{Id: 1, Name: "search-fixture", Type: constant.ChannelTypeZhipuWebSearch, Status: common.ChannelStatusEnabled, Key: "upstream-placeholder", BaseURL: &upstreamServer.URL, Models: "search_std,search_pro,search_pro_sogou,search_pro_quark", Group: "default", AutoBan: common.GetPointer(0)}
	require.NoError(t, database.Create(&user).Error)
	require.NoError(t, database.Create(&token).Error)
	require.NoError(t, database.Create(&limited).Error)
	require.NoError(t, database.Create(&model.UserModelPricing{UserId: user.Id, ModelName: "search_pro", DiscountBPS: 5000}).Error)
	require.NoError(t, model.BatchInsertChannels([]model.Channel{channel}))
	engine := gin.New()
	engine.Use(middleware.RequestId())
	router.SetApiRouter(engine)
	router.SetRelayRouter(engine)
	return engine, database, upstream
}

func zhipuSearchRequest(engine *gin.Engine, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer sk-"+token)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	return response
}

func zhipuSearchBalances(t *testing.T, database *gorm.DB, tokenID int) (int, int) {
	t.Helper()
	var user model.User
	var token model.Token
	require.NoError(t, database.First(&user, 1).Error)
	require.NoError(t, database.First(&token, tokenID).Error)
	return user.Quota, token.RemainQuota
}

func TestZhipuSearchChannelIsolation(t *testing.T) {
	for _, cacheEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cache_%t", cacheEnabled), func(t *testing.T) {
			engine, database, upstream := setupZhipuSearchContract(t)
			common.RetryTimes = 0
			plan := "glm-coding-plan"
			ordinaryBase := "http://127.0.0.1:1"
			require.NoError(t, model.BatchInsertChannels([]model.Channel{
				{Id: 2, Name: "coding-plan", Type: constant.ChannelTypeZhipu_v4, Status: common.ChannelStatusEnabled, Key: "coding-placeholder", BaseURL: &plan, Models: "search_std,glm-4", Group: "default", Priority: common.GetPointer(int64(100))},
				{Id: 3, Name: "ordinary-zhipu", Type: constant.ChannelTypeZhipu_v4, Status: common.ChannelStatusEnabled, Key: "ordinary-placeholder", BaseURL: &ordinaryBase, Models: "search_std", Group: "default", Priority: common.GetPointer(int64(99))},
			}))
			common.MemoryCacheEnabled = cacheEnabled
			if cacheEnabled {
				model.InitChannelCache()
			}
			for _, path := range []string{"/v1/web_search", "/api/paas/v4/web_search"} {
				beforePaths, _ := upstream.snapshot()
				response := zhipuSearchRequest(engine, path, `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				afterPaths, _ := upstream.snapshot()
				assert.Len(t, afterPaths, len(beforePaths)+1)
				assert.Equal(t, "/api/paas/v4/web_search", afterPaths[len(afterPaths)-1])
				var logged model.Log
				require.NoError(t, database.Order("id desc").First(&logged).Error)
				assert.Equal(t, 1, logged.ChannelId)
			}

			require.True(t, model.UpdateChannelStatus(1, "", common.ChannelStatusManuallyDisabled, "isolation test"))
			beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
			beforePaths, _ := upstream.snapshot()
			for _, path := range []string{"/v1/web_search", "/api/paas/v4/web_search"} {
				response := zhipuSearchRequest(engine, path, `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
				assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			}
			afterPaths, _ := upstream.snapshot()
			assert.Equal(t, beforePaths, afterPaths)
			afterUser, afterToken := zhipuSearchBalances(t, database, 1)
			assert.Equal(t, beforeUser, afterUser)
			assert.Equal(t, beforeToken, afterToken)

			require.True(t, model.UpdateChannelStatus(2, "", common.ChannelStatusManuallyDisabled, "isolation test"))
			require.True(t, model.UpdateChannelStatus(3, "", common.ChannelStatusManuallyDisabled, "isolation test"))
			require.NoError(t, model.BatchInsertChannels([]model.Channel{
				{Id: 4, Name: "search-with-chat-model", Type: constant.ChannelTypeZhipuWebSearch, Status: common.ChannelStatusEnabled, Key: "search-placeholder", BaseURL: &ordinaryBase, Models: "glm-4", Group: "default", Priority: common.GetPointer(int64(100))},
			}))
			if cacheEnabled {
				model.InitChannelCache()
			}
			for _, tc := range []struct {
				path string
				body string
			}{
				{"/v1/chat/completions", `{"model":"glm-4","messages":[{"role":"user","content":"test"}]}`},
				{"/v1/messages", `{"model":"glm-4","max_tokens":16,"messages":[{"role":"user","content":"test"}]}`},
				{"/v1/responses/compact", `{"model":"glm-4","input":"test"}`},
			} {
				response := zhipuSearchRequest(engine, tc.path, tc.body, "fixturetoken")
				assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			}
			afterPaths, _ = upstream.snapshot()
			assert.Equal(t, beforePaths, afterPaths)
		})
	}
}

func TestZhipuSearchPreflightFallback(t *testing.T) {
	for _, cacheEnabled := range []bool{false, true} {
		for _, failure := range []string{"request URL", "header override", "HTTP client", "sent request"} {
			t.Run(fmt.Sprintf("cache_%t/%s", cacheEnabled, failure), func(t *testing.T) {
				engine, database, upstream := setupZhipuSearchContract(t)
				common.RetryTimes = 1
				var backup model.Channel
				require.NoError(t, database.First(&backup, 1).Error)
				primary := model.Channel{Id: 2, Name: "primary-search", Type: constant.ChannelTypeZhipuWebSearch, Status: common.ChannelStatusEnabled, Key: "primary-placeholder", BaseURL: backup.BaseURL, Models: "search_std", Group: "default", Priority: common.GetPointer(int64(100)), AutoBan: common.GetPointer(0)}
				switch failure {
				case "request URL":
					primary.BaseURL = common.GetPointer("http://[")
				case "header override":
					primary.HeaderOverride = common.GetPointer(`{"re:[":"*"}`)
				case "HTTP client":
					primary.Setting = common.GetPointer(`{"proxy":"ftp://127.0.0.1:1"}`)
				case "sent request":
					upstream.closeNextResponse()
				}
				require.NoError(t, model.BatchInsertChannels([]model.Channel{primary}))
				common.MemoryCacheEnabled = cacheEnabled
				if cacheEnabled {
					model.InitChannelCache()
				}
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
				paths, _ := upstream.snapshot()
				assert.Len(t, paths, 1)
				if failure == "sent request" {
					assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
					return
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var logged model.Log
				require.NoError(t, database.Where("type = ?", model.LogTypeConsume).Order("id desc").First(&logged).Error)
				assert.Equal(t, 1, logged.ChannelId)
				afterUser, afterToken := zhipuSearchBalances(t, database, 1)
				assert.Equal(t, 685, beforeUser-afterUser)
				assert.Equal(t, 685, beforeToken-afterToken)
			})
		}
	}
}

func TestZhipuSearchRequestBodyLimit(t *testing.T) {
	engine, database, upstream := setupZhipuSearchContract(t)
	previousLimit := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() { constant.MaxRequestBodyMB = previousLimit })
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	const limit = 1 << 20
	prefix := `{"search_engine":"search_std","search_query":"test","extension":"`
	suffix := `"}`
	for _, tc := range []struct {
		name       string
		size       int
		encoding   string
		chunked    bool
		wantStatus int
	}{
		{"exact limit", limit, "", false, http.StatusOK},
		{"chunked exact limit", limit, "", true, http.StatusOK},
		{"gzip exact limit", limit, "gzip", false, http.StatusOK},
		{"over limit", limit + 1, "", false, http.StatusBadRequest},
		{"chunked over limit", limit + 1, "", true, http.StatusBadRequest},
		{"gzip over limit", limit + 1, "gzip", false, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := prefix + strings.Repeat("x", tc.size-len(prefix)-len(suffix)) + suffix
			var body io.Reader = strings.NewReader(payload)
			if tc.encoding == "gzip" {
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				_, err := writer.Write([]byte(payload))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				body = bytes.NewReader(compressed.Bytes())
			}
			beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
			_, before := upstream.snapshot()
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/web_search", body)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer sk-fixturetoken")
			request.Header.Set("Content-Type", "application/json")
			if tc.encoding != "" {
				request.Header.Set("Content-Encoding", tc.encoding)
			}
			if tc.chunked {
				request.ContentLength = -1
			}
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			responseBody, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, response.StatusCode, string(responseBody))
			_, after := upstream.snapshot()
			afterUser, afterToken := zhipuSearchBalances(t, database, 1)
			if tc.wantStatus == http.StatusOK {
				assert.Len(t, after, len(before)+1)
				assert.Equal(t, 685, beforeUser-afterUser)
				assert.Equal(t, 685, beforeToken-afterToken)
			} else {
				assert.Len(t, after, len(before))
				assert.Equal(t, beforeUser, afterUser)
				assert.Equal(t, beforeToken, afterToken)
			}
		})
	}
}

func TestZhipuWebSearchContract(t *testing.T) {
	engine, database, upstream := setupZhipuSearchContract(t)
	t.Run("pricing settings expose effective default", func(t *testing.T) {
		snapshot, err := model.GetModelPricingSnapshot([]string{"search_std", "search_pro", "search_pro_sogou", "search_pro_quark"})
		require.NoError(t, err)
		expected := map[string]float64{"search_std": 10, "search_pro": 30, "search_pro_sogou": 50, "search_pro_quark": 50}
		require.Len(t, snapshot.Entries, len(expected))
		for _, entry := range snapshot.Entries {
			assert.NotContains(t, entry.Configured, "ModelPrice")
			assert.InDelta(t, expected[entry.ModelName]/7.3/1000, entry.Effective["ModelPrice"], 1e-12)
			assert.InDelta(t, expected[entry.ModelName]/7.3/1000, gjson.Get(snapshot.Options["ModelPrice"], entry.ModelName).Float(), 1e-12)
		}
		common.OptionMapRWMutex.Lock()
		operation_setting.USDExchangeRate = 8
		common.OptionMapRWMutex.Unlock()
		t.Cleanup(func() {
			common.OptionMapRWMutex.Lock()
			operation_setting.USDExchangeRate = 7.3
			common.OptionMapRWMutex.Unlock()
		})
		snapshot, err = model.GetModelPricingSnapshot([]string{"search_std"})
		require.NoError(t, err)
		assert.InDelta(t, 10.0/8/1000, snapshot.Entries[0].Effective["ModelPrice"], 1e-12)
		assert.NotContains(t, snapshot.Entries[0].Configured, "ModelPrice")
		require.NoError(t, database.Create(&model.Option{Key: "ModelPrice", Value: `{"search_std":0.003}`}).Error)
		t.Cleanup(func() { require.NoError(t, database.Delete(&model.Option{Key: "ModelPrice"}).Error) })
		snapshot, err = model.GetModelPricingSnapshot([]string{"search_std"})
		require.NoError(t, err)
		assert.InDelta(t, 0.003, snapshot.Entries[0].Configured["ModelPrice"], 1e-12)
		assert.InDelta(t, 0.003, snapshot.Entries[0].Effective["ModelPrice"], 1e-12)
		assert.InDelta(t, 0.003, gjson.Get(snapshot.Options["ModelPrice"], "search_std").Float(), 1e-12)
	})
	t.Run("model discovery retains the official search engine", func(t *testing.T) {
		model.InvalidatePricingCache()
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer sk-fixturetoken")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		item := gjson.Get(response.Body.String(), `data.#(id=="search_std")`)
		require.True(t, item.Exists(), response.Body.String())
		assert.Contains(t, item.Get("supported_endpoint_types").Raw, "zhipu-web-search")
	})
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		charge int
	}{
		{"standard", "/v1/web_search", `{"search_engine":"search_std","search_query":"test","search_intent":false,"count":10,"search_domain_filter":"www.example.com","search_recency_filter":"oneWeek","content_size":"high","request_id":"request-std-0001","user_id":"user-0001","extension":{"keep":true}}`, 685},
		{"search intent", "/v1/web_search", `{"search_engine":"search_std","search_query":"test","search_intent":true}`, 685},
		{"pro", "/api/paas/v4/web_search", `{"search_engine":"search_pro","search_query":"test","search_intent":false,"count":10}`, 1027},
		{"sogou", "/v1/web_search", `{"search_engine":"search_pro_sogou","search_query":"test","search_intent":false,"count":11}`, 3425},
		{"quark", "/v1/web_search", `{"search_engine":"search_pro_quark","search_query":"test","search_intent":false,"count":2,"search_domain_filter":"www.example.com"}`, 3425},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
			_, beforeBodies := upstream.snapshot()
			response := zhipuSearchRequest(engine, tc.path, tc.body, "fixturetoken")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, `{"id":"upstream-id","search_result":[{"title":"result"}],"extra":{"keep":true}}`, response.Body.String())
			assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
			paths, bodies := upstream.snapshot()
			require.Len(t, bodies, len(beforeBodies)+1)
			assert.Equal(t, "/api/paas/v4/web_search", paths[len(paths)-1])
			assert.Equal(t, tc.body, bodies[len(bodies)-1])
			afterUser, afterToken := zhipuSearchBalances(t, database, 1)
			assert.Equal(t, tc.charge, beforeUser-afterUser)
			assert.Equal(t, tc.charge, beforeToken-afterToken)
			var log model.Log
			require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeConsume).Order("id DESC").First(&log).Error)
			assert.Equal(t, tc.charge, log.Quota)
			assert.Equal(t, gjson.Get(tc.body, "search_engine").String(), log.ModelName)
		})
	}

	t.Run("invalid requests do not reach upstream", func(t *testing.T) {
		cases := map[string]string{
			"missing engine":        `{"search_query":"test"}`,
			"unknown engine":        `{"search_engine":"search_unknown","search_query":"test"}`,
			"duplicate engine":      `{"search_engine":"search_std","search_engine":"search_pro","search_query":"test"}`,
			"escaped duplicate":     `{"search_engine":"search_std","search_\u0065ngine":"search_pro","search_query":"test"}`,
			"case variant engine":   `{"search_engine":"search_pro","SEARCH_ENGINE":"search_std","search_query":"test"}`,
			"case variant only":     `{"SEARCH_ENGINE":"search_std","search_query":"test"}`,
			"unicode folded key":    `{"search_engine":"search_pro","\u017fearch_engine":"search_std","search_query":"test"}`,
			"empty query":           `{"search_engine":"search_std","search_query":" "}`,
			"long query":            fmt.Sprintf(`{"search_engine":"search_std","search_query":"%s"}`, strings.Repeat("x", 71)),
			"zero count":            `{"search_engine":"search_std","search_query":"test","count":0}`,
			"sogou count too large": `{"search_engine":"search_pro_sogou","search_query":"test","count":51}`,
			"quark count too large": `{"search_engine":"search_pro_quark","search_query":"test","count":51}`,
			"invalid recency":       `{"search_engine":"search_std","search_query":"test","search_recency_filter":"week"}`,
			"invalid content size":  `{"search_engine":"search_std","search_query":"test","content_size":"small"}`,
			"short request id":      `{"search_engine":"search_std","search_query":"test","request_id":"a"}`,
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				_, before := upstream.snapshot()
				response := zhipuSearchRequest(engine, "/v1/web_search", body, "fixturetoken")
				assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
				_, after := upstream.snapshot()
				assert.Len(t, after, len(before))
				afterUser, afterToken := zhipuSearchBalances(t, database, 1)
				assert.Equal(t, beforeUser, afterUser)
				assert.Equal(t, beforeToken, afterToken)
			})
		}
	})

	t.Run("both paths require a valid token", func(t *testing.T) {
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		_, before := upstream.snapshot()
		for _, path := range []string{"/v1/web_search", "/api/paas/v4/web_search"} {
			for _, authorization := range []string{"", "Bearer sk-invalidtoken"} {
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"search_engine":"search_std","search_query":"test"}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", authorization)
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				assert.Equal(t, http.StatusUnauthorized, response.Code, "%s: %s", path, response.Body.String())
			}
		}
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before))
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, beforeUser, afterUser)
		assert.Equal(t, beforeToken, afterToken)
	})

	t.Run("token model restrictions", func(t *testing.T) {
		_, before := upstream.snapshot()
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_pro","search_query":"test"}`, "limitedtoken")
		assert.Equal(t, http.StatusForbidden, response.Code)
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before))
	})

	t.Run("compatible New API upstream", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("type", constant.ChannelTypeNewAPI).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("type", constant.ChannelTypeZhipuWebSearch).Error)
		})
		body := `{"search_engine":"search_std","search_query":"test","search_intent":false}`
		response := zhipuSearchRequest(engine, "/api/paas/v4/web_search", body, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		paths, bodies := upstream.snapshot()
		assert.Equal(t, "/v1/web_search", paths[len(paths)-1])
		assert.Equal(t, body, bodies[len(bodies)-1])
	})

	t.Run("non-engine parameter override is forwarded", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", `{"count":5}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", "").Error)
		})
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test","count":3}`, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		_, bodies := upstream.snapshot()
		assert.EqualValues(t, 5, gjson.Get(bodies[len(bodies)-1], "count").Int())
		assert.Equal(t, "search_std", gjson.Get(bodies[len(bodies)-1], "search_engine").String())
	})

	t.Run("parameter override cannot remove the search query", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", `{"operations":[{"mode":"delete","path":"search_query"}]}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", "").Error)
		})
		_, before := upstream.snapshot()
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before))
	})

	t.Run("configured model price sets search price", func(t *testing.T) {
		previousPrice := ratio_setting.ModelPrice2JSONString()
		t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrice)) })
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"search_std":0.003}`))
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 1500, beforeUser-afterUser)
		assert.Equal(t, 1500, beforeToken-afterToken)
	})

	t.Run("unsupported token pricing is rejected before forwarding", func(t *testing.T) {
		previousRatio := ratio_setting.ModelRatio2JSONString()
		t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatio)) })
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"search_std":1}`))
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		_, before := upstream.snapshot()
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before))
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, beforeUser, afterUser)
		assert.Equal(t, beforeToken, afterToken)
	})

	t.Run("search price and discount use the engine model name", func(t *testing.T) {
		previousPrice := ratio_setting.ModelPrice2JSONString()
		t.Cleanup(func() {
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrice))
			model.InvalidatePricingCache()
		})
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"search_std":0.003}`))
		model.InvalidatePricingCache()
		foundSearch := false
		for _, pricing := range model.GetPricing() {
			if pricing.ModelName == "search_std" {
				foundSearch = true
				assert.Equal(t, 1, pricing.QuotaType)
				assert.InDelta(t, 0.003, pricing.ModelPrice, 1e-12)
			}
		}
		assert.True(t, foundSearch)
		assert.Equal(t, []int{1}, model.GetModelQuotaTypes("search_std"))
		assert.Contains(t, model.GetModelSupportEndpointTypes("search_std"), constant.EndpointTypeZhipuWebSearch)
		names, err := model.GetUserModelPricingModelNames(context.Background())
		require.NoError(t, err)
		assert.Contains(t, names, "search_std")
		user := model.User{Id: 3, Username: "engine-pricing", AffCode: "engine-pricing-fixture", Status: common.UserStatusEnabled, Group: "default", Quota: 10_000_000, Setting: `{"billing_preference":"wallet_only"}`}
		token := model.Token{Id: 4, UserId: user.Id, Key: "enginepricingtoken", Name: "engine-pricing", Status: common.TokenStatusEnabled, RemainQuota: 10_000_000, ExpiredTime: -1}
		require.NoError(t, database.Create(&user).Error)
		require.NoError(t, database.Create(&token).Error)
		require.NoError(t, database.Create(&model.UserModelPricing{UserId: user.Id, ModelName: "search_std", DiscountBPS: 5000}).Error)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "enginepricingtoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.NoError(t, database.First(&user, 3).Error)
		assert.Equal(t, 10_000_000-750, user.Quota)
	})

	t.Run("channel overrides cannot change the search engine", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			field string
			value string
		}{
			{"model mapping", "model_mapping", `{"search_std":"search_pro"}`},
			{"parameter override", "param_override", `{"search_engine":"search_pro"}`},
			{"case variant override", "param_override", `{"SEARCH_ENGINE":"search_pro"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update(tc.field, tc.value).Error)
				t.Cleanup(func() {
					require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update(tc.field, "").Error)
				})
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				_, before := upstream.snapshot()
				response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
				assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
				_, after := upstream.snapshot()
				assert.Len(t, after, len(before))
				require.Eventually(t, func() bool {
					user, token := zhipuSearchBalances(t, database, 1)
					return user == beforeUser && token == beforeToken
				}, 5*time.Second, 10*time.Millisecond)
			})
		}
	})

	t.Run("upstream failure refunds once without retry", func(t *testing.T) {
		upstream.setResponse(http.StatusUnauthorized, `{"error":{"code":"invalid_key","message":"upstream denied"}}`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		_, before := upstream.snapshot()
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
		assert.Equal(t, "invalid_key", gjson.Get(response.Body.String(), "error.code").String())
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before)+1)
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("upstream error preserves status and body", func(t *testing.T) {
		body := ` {"code":42901,"message":"rate limited","request_id":"upstream-request","detail":{"token":123,"sequence":9007199254740993,"extra":["kept",2]}} `
		upstream.setResponse(http.StatusTooManyRequests, body)
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("status_code_mapping", `{"429":"503"}`).Error)
		t.Cleanup(func() {
			upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("status_code_mapping", "").Error)
		})
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusTooManyRequests, response.Code)
		assert.Equal(t, body, response.Body.String())
		assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("upstream error masks known credentials without changing other fields", func(t *testing.T) {
		body := `{"error":{"message":"upstream-\u0070laceholder rejected","token":123,"metadata":{"detail":"kept"}},"request_id":"upstream-request"}`
		upstream.setResponse(http.StatusUnauthorized, body)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Equal(t, `{"error":{"message":"*** rejected","token":123,"metadata":{"detail":"kept"}},"request_id":"upstream-request"}`, response.Body.String())
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("upstream error masks credentials in nested JSON strings", func(t *testing.T) {
		body := `{"message":"{\"detail\":\"upstream-\\u0070laceholder\"}","request_id":"upstream-request"}`
		upstream.setResponse(http.StatusUnauthorized, body)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Equal(t, `{"detail":"***"}`, gjson.Get(response.Body.String(), "message").String())
		assert.Equal(t, "upstream-request", gjson.Get(response.Body.String(), "request_id").String())
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("upstream error masks individual cookie credentials", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", `{"Cookie":"sessionid=fixture-cookie-secret; theme=dark"}`).Error)
		upstream.setResponse(http.StatusUnauthorized, `{"message":"fixture-cookie-secret rejected","detail":"kept"}`)
		t.Cleanup(func() {
			upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", "").Error)
		})
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Equal(t, `{"message":"*** rejected","detail":"kept"}`, response.Body.String())
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("upstream error masks credential fields while preserving diagnostics", func(t *testing.T) {
		body := `{"error":{"message":"Cookie : sessionid=fixture-message-secret\nX-Trace: kept","token":123},"headers":{"Cookie":["sessionid=fixture-unknown-secret","other=fixture-other-secret"],"X-Trace":"kept"},"session_id":"fixture-session-secret","request_id":"upstream-request"}`
		upstream.setResponse(http.StatusUnauthorized, body)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.NotContains(t, response.Body.String(), "fixture-unknown-secret")
		assert.NotContains(t, response.Body.String(), "fixture-other-secret")
		assert.NotContains(t, response.Body.String(), "fixture-session-secret")
		assert.NotContains(t, response.Body.String(), "fixture-message-secret")
		assert.Equal(t, int64(123), gjson.Get(response.Body.String(), "error.token").Int())
		assert.Contains(t, gjson.Get(response.Body.String(), "error.message").String(), "X-Trace: kept")
		assert.Equal(t, "kept", gjson.Get(response.Body.String(), "headers.X-Trace").String())
		assert.Equal(t, "upstream-request", gjson.Get(response.Body.String(), "request_id").String())
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("upstream error masks API keys and access tokens in text", func(t *testing.T) {
		body := `{"error":{"code":"invalid_request","message":"request failed: api_key=fixture-api-secret access_token=fixture-access-secret","detail":"kept"},"request_id":"upstream-request"}`
		upstream.setResponse(http.StatusUnauthorized, body)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.True(t, gjson.Valid(response.Body.String()))
		assert.NotContains(t, response.Body.String(), "fixture-api-secret")
		assert.NotContains(t, response.Body.String(), "fixture-access-secret")
		assert.Equal(t, "invalid_request", gjson.Get(response.Body.String(), "error.code").String())
		assert.Equal(t, "request failed: api_key=*** access_token=***", gjson.Get(response.Body.String(), "error.message").String())
		assert.Equal(t, "kept", gjson.Get(response.Body.String(), "error.detail").String())
		assert.Equal(t, "upstream-request", gjson.Get(response.Body.String(), "request_id").String())
		var entry model.Log
		require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeError).Order("id DESC").First(&entry).Error)
		assert.NotContains(t, entry.Content, "fixture-api-secret")
		assert.NotContains(t, entry.Content, "fixture-access-secret")
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("truncated upstream error masks credentials in logs", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", `{"Cookie":"sessionid=fixture-cookie-secret; theme=dark"}`).Error)
		upstream.setResponse(http.StatusUnauthorized, `{"message":"fixture-cookie-secret rejected"}`)
		upstream.setTruncated(true)
		t.Cleanup(func() {
			upstream.setTruncated(false)
			upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", "").Error)
		})
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadGateway, response.Code)
		assert.NotContains(t, response.Body.String(), "fixture-cookie-secret")
		var entry model.Log
		require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeError).Order("id DESC").First(&entry).Error)
		assert.Contains(t, entry.Content, "partial body")
		assert.NotContains(t, entry.Content, "fixture-cookie-secret")
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("transport does not replay a search after connection failure", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", `{"Idempotency-Key":"fixture-id"}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", "").Error)
		})
		body := `{"search_engine":"search_std","search_query":"test"}`
		warmup := zhipuSearchRequest(engine, "/v1/web_search", body, "fixturetoken")
		require.Equal(t, http.StatusOK, warmup.Code, warmup.Body.String())
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		_, before := upstream.snapshot()
		upstream.closeNextResponse()
		response := zhipuSearchRequest(engine, "/v1/web_search", body, "fixturetoken")
		assert.GreaterOrEqual(t, response.Code, http.StatusBadRequest, response.Body.String())
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before)+1)
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("upstream errors do not expose channel credentials", func(t *testing.T) {
		upstream.setResponse(http.StatusUnauthorized, `{"error":{"code":"invalid_key","message":"upstream-placeholder rejected","metadata":{"authorization":"upstream-placeholder","detail":"kept"}}}`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "upstream-placeholder")
		assert.Equal(t, "invalid_key", gjson.Get(response.Body.String(), "error.code").String())
		assert.Equal(t, "kept", gjson.Get(response.Body.String(), "error.metadata.detail").String())
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("overridden credentials stay out of responses and public audit logs", func(t *testing.T) {
		previousDebug := common.DebugEnabled
		common.DebugEnabled = true
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", `{"operations":[{"mode":"set_header","path":"Authorization","value":"Bearer fixture-header-token"}]}`).Error)
		t.Cleanup(func() {
			common.DebugEnabled = previousDebug
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", "").Error)
		})
		upstream.setResponse(http.StatusOK, `{"search_result":[]}`)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, "Bearer fixture-header-token", upstream.lastAuthorization())
		var log model.Log
		require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeConsume).Order("id DESC").First(&log).Error)
		assert.NotContains(t, log.Other, "fixture-header-token")
		assert.Contains(t, log.Other, "set_header")

		upstream.setResponse(http.StatusUnauthorized, `{"error":{"code":"invalid_key","message":"rejected fixture-header-token"}}`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		response = zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "fixture-header-token")
		log = model.Log{}
		require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeError).Order("id DESC").First(&log).Error)
		assert.NotContains(t, log.Content, "fixture-header-token")
	})

	t.Run("session header credentials stay out of errors", func(t *testing.T) {
		const sessionCredential = "fixture-session-credential"
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", `{"X-Session-Id":"fixture-session-credential"}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", "").Error)
		})
		upstream.setResponse(http.StatusUnauthorized, `{"error":{"code":"invalid_session","message":"rejected fixture-session-credential"}}`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		require.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), sessionCredential)
		var log model.Log
		require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeError).Order("id DESC").First(&log).Error)
		assert.NotContains(t, log.Content, sessionCredential)
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("malformed upstream errors do not expose channel credentials in logs", func(t *testing.T) {
		upstream.setResponse(http.StatusUnauthorized, "upstream-placeholder rejected by upstream")
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		var captured bytes.Buffer
		common.LogWriterMu.Lock()
		previousWriter := gin.DefaultErrorWriter
		gin.DefaultErrorWriter = &captured
		common.LogWriterMu.Unlock()
		defer func() {
			common.LogWriterMu.Lock()
			gin.DefaultErrorWriter = previousWriter
			common.LogWriterMu.Unlock()
		}()
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusUnauthorized, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "upstream-placeholder")
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
		common.LogWriterMu.Lock()
		output := captured.String()
		common.LogWriterMu.Unlock()
		assert.NotContains(t, output, "upstream-placeholder")
		assert.Contains(t, output, "rejected by upstream")
		var entry model.Log
		require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeError).Order("id DESC").First(&entry).Error)
		assert.NotContains(t, entry.Content, "upstream-placeholder")
		assert.Contains(t, entry.Content, "rejected by upstream")
	})

	t.Run("truncated upstream response returns an error after charging", func(t *testing.T) {
		upstream.setResponse(http.StatusOK, `{"search_result":[`)
		upstream.setTruncated(true)
		defer func() {
			upstream.setTruncated(false)
			upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		}()
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		_, before := upstream.snapshot()
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
		assert.True(t, gjson.Valid(response.Body.String()), response.Body.String())
		assert.True(t, gjson.Get(response.Body.String(), "error").Exists())
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before)+1)
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 685, beforeUser-afterUser)
		assert.Equal(t, 685, beforeToken-afterToken)
	})

	t.Run("complete invalid JSON is rejected after charging", func(t *testing.T) {
		upstream.setResponse(http.StatusOK, `{"search_result":[`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
		assert.True(t, gjson.Valid(response.Body.String()))
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 685, beforeUser-afterUser)
		assert.Equal(t, 685, beforeToken-afterToken)
	})

	t.Run("client disconnect closes the upstream body after charging", func(t *testing.T) {
		ready := make(chan struct{})
		release := make(chan struct{})
		closed := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(ready)
			select {
			case <-r.Context().Done():
				close(closed)
			case <-release:
			}
		}))
		t.Cleanup(func() {
			close(release)
			server.Close()
		})
		var channel model.Channel
		require.NoError(t, database.First(&channel, 1).Error)
		originalBaseURL := channel.GetBaseURL()
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("base_url", server.URL).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("base_url", originalBaseURL).Error)
		})
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(`{"search_engine":"search_std","search_query":"test"}`)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer sk-fixturetoken")
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { engine.ServeHTTP(response, request); close(done) }()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("上游未收到请求")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("客户端断开后请求未结束")
		}
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("上游响应体未关闭")
		}
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 685, beforeUser-afterUser)
		assert.Equal(t, 685, beforeToken-afterToken)
	})

	t.Run("large upstream error preserves the full body", func(t *testing.T) {
		body := `{"error":{"code":"upstream_failure","message":"failed"},"detail":"` + strings.Repeat("x", (1<<20)+4096) + `"}`
		upstream.setResponse(http.StatusBadGateway, body)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
		var log model.Log
		require.NoError(t, database.Where("user_id = ? AND type = ?", 1, model.LogTypeError).Order("id DESC").First(&log).Error)
		assert.Contains(t, log.Content, "upstream_failure")
		assert.Contains(t, log.Content, "failed")
		assert.True(t, response.Body.String() == body, "上游错误响应不应按大小截断")
	})

	t.Run("search discount count ignores extra request fields", func(t *testing.T) {
		user := model.User{Id: 2, Username: "search-count-fixture", AffCode: "count-fixture", Status: common.UserStatusEnabled, Group: "default", Quota: 10_000_000, Setting: `{"billing_preference":"wallet_only"}`}
		token := model.Token{Id: 3, UserId: 2, Key: "countfixture", Name: "count", Status: common.TokenStatusEnabled, RemainQuota: 10_000_000, ExpiredTime: -1}
		rule := model.UserModelPricing{UserId: 2, ModelName: "search_std", DiscountBPS: 10000, Expression: "count >= 10 ? 0.5 : 1"}
		require.NoError(t, database.Create(&user).Error)
		require.NoError(t, database.Create(&token).Error)
		require.NoError(t, database.Create(&rule).Error)
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", `{"operations":[{"mode":"delete","path":"n"}]}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", "").Error)
		})
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test","n":10}`, "countfixture")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		_, bodies := upstream.snapshot()
		assert.False(t, gjson.Get(bodies[len(bodies)-1], "n").Exists())
		require.NoError(t, database.First(&user, 2).Error)
		assert.Equal(t, 10_000_000-685, user.Quota)
	})

	t.Run("search pricing rejects token and expression configurations", func(t *testing.T) {
		assert.Error(t, model.ValidateModelPricing("search_std", model.PricingValues{"ModelRatio": float64(1)}))
		assert.Error(t, model.ValidateModelPricing("search_pro", model.PricingValues{"billing_setting.billing_mode": "tiered_expr", "billing_setting.billing_expr": `tier("base", p)`}))
		assert.NoError(t, model.ValidateModelPricing("search_std", model.PricingValues{"ModelPrice": float64(0.002)}))
	})

	t.Run("subscription success and failure", func(t *testing.T) {
		sqlDB, err := database.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(10)
		t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
		require.NoError(t, database.Create(&model.SubscriptionPlan{Id: 1, Title: "search-fixture", TotalAmount: 10_000_000}).Error)
		require.NoError(t, database.Create(&model.UserSubscription{Id: 1, UserId: 1, PlanId: 1, AmountTotal: 10_000_000, StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix(), Status: "active"}).Error)
		require.NoError(t, database.Model(&model.User{}).Where("id = ?", 1).Update("setting", `{"billing_preference":"subscription_only"}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.User{}).Where("id = ?", 1).Update("setting", `{"billing_preference":"wallet_only"}`).Error)
		})
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var subscription model.UserSubscription
		require.NoError(t, database.First(&subscription, 1).Error)
		assert.EqualValues(t, 685, subscription.AmountUsed)
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, beforeUser, afterUser)
		assert.Equal(t, 685, beforeToken-afterToken)

		upstream.setResponse(http.StatusBadGateway, `{"error":{"code":"upstream_failure","message":"failed"}}`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		response = zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
		require.Eventually(t, func() bool {
			var current model.UserSubscription
			if database.First(&current, 1).Error != nil {
				return false
			}
			user, token := zhipuSearchBalances(t, database, 1)
			return current.AmountUsed == 685 && user == afterUser && token == afterToken
		}, 5*time.Second, 10*time.Millisecond)
		var refunded model.SubscriptionPreConsumeRecord
		require.NoError(t, database.Where("user_subscription_id = ? AND status = ?", 1, "refunded").First(&refunded).Error)
		require.NoError(t, model.RefundSubscriptionPreConsume(refunded.RequestId))
		require.NoError(t, database.First(&subscription, 1).Error)
		assert.EqualValues(t, 685, subscription.AmountUsed)
	})

	t.Run("concurrent calls respect available wallet quota", func(t *testing.T) {
		const callers, affordable = 8, 3
		const remaining = 1
		originalUser, originalToken := zhipuSearchBalances(t, database, 1)
		require.NoError(t, database.Model(&model.User{}).Where("id = ?", 1).Updates(map[string]any{
			"quota": affordable*685 + remaining, "setting": `{"billing_preference":"wallet_only","quota_warning_threshold":1}`,
		}).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.User{}).Where("id = ?", 1).Updates(map[string]any{
				"quota": originalUser, "setting": `{"billing_preference":"wallet_only"}`,
			}).Error)
		})
		_, before := upstream.snapshot()
		statuses := make(chan int, callers)
		var wait sync.WaitGroup
		for i := 0; i < callers; i++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
				statuses <- response.Code
			}()
		}
		wait.Wait()
		close(statuses)
		success, rejected := 0, 0
		for status := range statuses {
			switch status {
			case http.StatusOK:
				success++
			case http.StatusForbidden:
				rejected++
			default:
				t.Errorf("unexpected status: %d", status)
			}
		}
		assert.Equal(t, affordable, success)
		assert.Equal(t, callers-affordable, rejected)
		_, after := upstream.snapshot()
		assert.Len(t, after, len(before)+affordable)
		user, token := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, remaining, user)
		assert.Equal(t, affordable*685, originalToken-token)
	})

	t.Run("configured price overrides default", func(t *testing.T) {
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"search_std":0.003}`))
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 1500, beforeUser-afterUser)
		assert.Equal(t, 1500, beforeToken-afterToken)
	})
}
