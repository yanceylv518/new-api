package controller_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/andybalholm/brotli"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type zhipuLiveTransport struct {
	base             http.RoundTripper
	key              string
	calls            *atomic.Int32
	requestBody      *bytes.Buffer
	responseBody     *bytes.Buffer
	responseHeaders  *http.Header
	autoDecompressed *bool
	acceptEncoding   *string
	legacyAuthHeader *string
}

func (t zhipuLiveTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() == "open.bigmodel.cn" && request.URL.Path == "/api/paas/v4/web_search" {
		t.calls.Add(1)
		*t.acceptEncoding = request.Header.Get("Accept-Encoding")
		*t.legacyAuthHeader = request.Header.Get("mj-api-secret")
		request = request.Clone(request.Context())
		if request.Header.Get("Authorization") == "Bearer test-placeholder" {
			request.Header.Set("Authorization", "Bearer "+t.key)
		}
		request.Body = struct {
			io.Reader
			io.Closer
		}{Reader: io.TeeReader(request.Body, t.requestBody), Closer: request.Body}
	}
	response, err := t.base.RoundTrip(request)
	if err == nil && response.Body != nil {
		*t.responseHeaders = response.Header.Clone()
		*t.autoDecompressed = response.Uncompressed
		response.Body = struct {
			io.Reader
			io.Closer
		}{Reader: io.TeeReader(response.Body, t.responseBody), Closer: response.Body}
	}
	return response, err
}

func TestZhipuWebSearchLive(t *testing.T) {
	if os.Getenv("NEWAPI_RUN_ZHIPU_LIVE") != "1" {
		t.Skip("需要显式启用智谱真实接口测试")
	}
	baselineWorkers := gopool.WorkerCount()
	key := os.Getenv("NEWAPI_ZHIPU_LIVE_KEY")
	require.NotEmpty(t, key)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousPath := common.IsMasterNode, common.SQLitePath
	previousRedis, previousCache, previousBatch := common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled
	previousLogging, previousExport := common.LogConsumeEnabled, common.DataExportEnabled
	previousTimeout, previousRetries, previousQuotaUnit := common.RelayTimeout, common.RetryTimes, common.QuotaPerUnit
	previousCount, previousErrorLog := constant.CountToken, constant.ErrorLogEnabled
	previousPrice := ratio_setting.ModelPrice2JSONString()
	previousRate, previousMode := operation_setting.USDExchangeRate, gin.Mode()
	common.SQLitePath = "file:zhipu-web-search-live?mode=memory&cache=shared"
	common.IsMasterNode = false
	common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled = false, false, false
	common.LogConsumeEnabled, common.DataExportEnabled = true, false
	common.RelayTimeout, common.RetryTimes, common.QuotaPerUnit = 120, 0, 500_000
	constant.CountToken = false
	common.OptionMapRWMutex.Lock()
	operation_setting.USDExchangeRate = 7.3
	common.OptionMapRWMutex.Unlock()
	gin.SetMode(gin.TestMode)
	t.Setenv("SQL_DSN", "local")
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
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}, &model.Model{}, &model.Vendor{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}))
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
	service.InitHttpClient()
	upstreamClient, err := service.GetHttpClientWithProxySettings("", dto.ChannelSettings{})
	require.NoError(t, err)
	previousTransport := upstreamClient.Transport
	var upstreamCalls atomic.Int32
	var upstreamRequestBody bytes.Buffer
	var upstreamBody bytes.Buffer
	var upstreamHeaders http.Header
	var upstreamAutoDecompressed bool
	var upstreamAcceptEncoding string
	var upstreamLegacyAuthHeader string
	upstreamClient.Transport = zhipuLiveTransport{base: previousTransport, key: key, calls: &upstreamCalls, requestBody: &upstreamRequestBody, responseBody: &upstreamBody, responseHeaders: &upstreamHeaders, autoDecompressed: &upstreamAutoDecompressed, acceptEncoding: &upstreamAcceptEncoding, legacyAuthHeader: &upstreamLegacyAuthHeader}
	t.Cleanup(func() { upstreamClient.Transport = previousTransport })
	user := model.User{Id: 1, Username: "zhipu-search", Password: "unused", Status: common.UserStatusEnabled, Group: "default", Quota: 10_000_000, Setting: `{"billing_preference":"wallet_only"}`}
	token := model.Token{Id: 1, UserId: user.Id, Key: "zhipusearchtest", Name: "test", Status: common.TokenStatusEnabled, RemainQuota: 10_000_000, ExpiredTime: -1}
	channel := model.Channel{Id: 1, Name: "zhipu-search", Type: constant.ChannelTypeZhipuWebSearch, Status: common.ChannelStatusEnabled, Key: "test-placeholder", Models: "search_std,search_pro,search_pro_sogou,search_pro_quark", Group: "default", AutoBan: common.GetPointer(0)}
	require.NoError(t, database.Create(&user).Error)
	require.NoError(t, database.Create(&token).Error)
	require.NoError(t, database.Create(&model.UserModelPricing{UserId: user.Id, ModelName: dto.ZhipuSearchPro, DiscountBPS: 5000}).Error)
	require.NoError(t, database.Create(&model.SubscriptionPlan{Id: 1, Title: "zhipu-search", TotalAmount: 10_000_000}).Error)
	require.NoError(t, database.Create(&model.UserSubscription{Id: 1, UserId: user.Id, PlanId: 1, AmountTotal: 10_000_000, StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix(), Status: "active"}).Error)
	require.NoError(t, model.BatchInsertChannels([]model.Channel{channel}))
	engine := gin.New()
	completed := make(chan struct{}, 4)
	engine.Use(middleware.RequestId(), func(c *gin.Context) {
		c.Next()
		completed <- struct{}{}
	})
	router.SetApiRouter(engine)
	router.SetRelayRouter(engine)
	server := httptest.NewServer(engine)
	defer server.Close()
	client := &http.Client{Timeout: 150 * time.Second}
	type zhipuLiveCase struct {
		name             string
		engine           string
		quota            int
		path             string
		fullOptions      bool
		searchIntent     bool
		query            string
		subscription     bool
		upstreamError    bool
		responseLimitMiB int
		overrides        map[string]any
		omitSearchIntent bool
		omitSearchQuery  bool
		wantErrorCode    string
		duplicateFields  bool
		observeOutcome   bool
		headerOverride   string
		requestEncoding  string
		legacyAuth       bool
		method           string
		errorStatus      int
	}
	testCases := []zhipuLiveCase{
		{name: dto.ZhipuSearchStd, engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", responseLimitMiB: 8},
		{name: dto.ZhipuSearchPro, engine: dto.ZhipuSearchPro, quota: 1027, path: "/api/paas/v4/web_search", responseLimitMiB: 8},
		{name: dto.ZhipuSearchProSogou, engine: dto.ZhipuSearchProSogou, quota: 3425, path: "/v1/web_search", responseLimitMiB: 8},
		{name: dto.ZhipuSearchProQuark, engine: dto.ZhipuSearchProQuark, quota: 3425, path: "/v1/web_search", responseLimitMiB: 8},
		{name: "intent_std", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", searchIntent: true},
		{name: "intent_pro", engine: dto.ZhipuSearchPro, quota: 1027, path: "/api/paas/v4/web_search", searchIntent: true},
		{name: "intent_sogou", engine: dto.ZhipuSearchProSogou, quota: 3425, path: "/v1/web_search", searchIntent: true},
		{name: "intent_quark", engine: dto.ZhipuSearchProQuark, quota: 3425, path: "/v1/web_search", searchIntent: true},
		{name: "intent_greeting", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", searchIntent: true, query: "你好"},
		{name: "options_and_price_override", engine: dto.ZhipuSearchStd, quota: 1500, path: "/v1/web_search", fullOptions: true},
		{name: "subscription", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", subscription: true},
		{name: "upstream_auth_error", engine: dto.ZhipuSearchStd, path: "/v1/web_search", upstreamError: true},
		{name: "header_override_gzip_success", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", headerOverride: "gzip"},
		{name: "header_override_br_success", engine: dto.ZhipuSearchStd, quota: 685, path: "/api/paas/v4/web_search", headerOverride: "br"},
		{name: "header_override_gzip_error", engine: dto.ZhipuSearchStd, path: "/v1/web_search", headerOverride: "gzip", overrides: map[string]any{"search_query": ""}, wantErrorCode: "1214"},
		{name: "header_override_br_error", engine: dto.ZhipuSearchStd, path: "/api/paas/v4/web_search", headerOverride: "br", overrides: map[string]any{"search_query": ""}, wantErrorCode: "1214"},
		{name: "gzip_request_uppercase", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", requestEncoding: "GZip"},
		{name: "legacy_auth_wildcard", engine: dto.ZhipuSearchStd, quota: 685, path: "/api/paas/v4/web_search", legacyAuth: true},
		{name: "patch_business_error", engine: dto.ZhipuSearchStd, path: "/v1/web_search", method: http.MethodPatch, errorStatus: http.StatusOK, wantErrorCode: "500"},
		{name: "array_count_error", engine: dto.ZhipuSearchStd, path: "/api/paas/v4/web_search", overrides: map[string]any{"count": []int{1}}, wantErrorCode: "400"},
		{name: "passthrough_sogou_count", engine: dto.ZhipuSearchProSogou, quota: 3425, path: "/v1/web_search", overrides: map[string]any{"count": 11}},
		{name: "passthrough_quark_options", engine: dto.ZhipuSearchProQuark, quota: 3425, path: "/api/paas/v4/web_search", overrides: map[string]any{"count": 1, "search_domain_filter": "www.sohu.com"}},
		{name: "passthrough_numeric_query", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", overrides: map[string]any{"search_query": 123, "count": "5"}},
		{name: "passthrough_zero_count", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", overrides: map[string]any{"count": 0}},
		{name: "passthrough_fractional_count", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", overrides: map[string]any{"count": 1.5}},
		{name: "passthrough_pro_options", engine: dto.ZhipuSearchPro, quota: 1027, path: "/api/paas/v4/web_search", overrides: map[string]any{"count": 51, "content_size": "large"}},
		{name: "passthrough_duplicate_fields", engine: dto.ZhipuSearchPro, quota: 1027, path: "/v1/web_search", duplicateFields: true},
		{name: "passthrough_null_options", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", overrides: map[string]any{"search_intent": nil, "count": nil}},
		{name: "passthrough_long_query", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", overrides: map[string]any{"search_query": strings.Repeat("天", 71), "count": 51}},
		{name: "passthrough_unusual_options", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", overrides: map[string]any{"search_recency_filter": "yesterday", "content_size": "large"}},
		{name: "passthrough_omitted_intent", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", omitSearchIntent: true},
		{name: "upstream_rejects_null_query", engine: dto.ZhipuSearchStd, path: "/v1/web_search", overrides: map[string]any{"search_query": nil}, wantErrorCode: "1214"},
		{name: "upstream_rejects_missing_query", engine: dto.ZhipuSearchStd, path: "/v1/web_search", omitSearchQuery: true, wantErrorCode: "1214"},
		{name: "upstream_rejects_empty_query", engine: dto.ZhipuSearchStd, path: "/v1/web_search", overrides: map[string]any{"search_query": ""}, wantErrorCode: "1214"},
		{name: "upstream_rejects_blank_query", engine: dto.ZhipuSearchStd, path: "/api/paas/v4/web_search", overrides: map[string]any{"search_query": "   "}, wantErrorCode: "1213"},
		{name: "upstream_rejects_short_request_id", engine: dto.ZhipuSearchStd, path: "/api/paas/v4/web_search", overrides: map[string]any{"request_id": "abcde"}, wantErrorCode: "1214"},
		{name: "upstream_rejects_short_user_id", engine: dto.ZhipuSearchStd, path: "/v1/web_search", overrides: map[string]any{"user_id": "abcde"}, wantErrorCode: "1214"},
		{name: "upstream_rejects_object_intent", engine: dto.ZhipuSearchStd, path: "/v1/web_search", overrides: map[string]any{"search_intent": map[string]any{}}, wantErrorCode: "400"},
	}
	for _, engine := range []struct {
		name  string
		quota int
	}{
		{dto.ZhipuSearchStd, 685},
		{dto.ZhipuSearchPro, 1027},
		{dto.ZhipuSearchProSogou, 3425},
		{dto.ZhipuSearchProQuark, 3425},
	} {
		for _, scenario := range []struct {
			name      string
			query     string
			overrides map[string]any
		}{
			{"query_2048", strings.Repeat("\u6d4b", 2048), nil},
			{"mixed_emoji", "中文搜索 \U0001f600\U0001f680 emoji test", nil},
			{"count_1000", "", map[string]any{"count": 1000}},
			{"invalid_recency", "", map[string]any{"search_recency_filter": "invalid_enum_value"}},
			{"invalid_content_size", "", map[string]any{"content_size": "invalid_enum_value"}},
		} {
			testCases = append(testCases, zhipuLiveCase{name: scenario.name + "_" + engine.name, engine: engine.name, quota: engine.quota, path: "/v1/web_search", query: scenario.query, overrides: scenario.overrides, observeOutcome: true})
		}
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.legacyAuth {
				require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("header_override", `{"*":""}`).Error)
				t.Cleanup(func() {
					_ = database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("header_override", "").Error
				})
			}
			if tc.headerOverride != "" {
				headerOverride := fmt.Sprintf(`{"Accept-Encoding":%q}`, tc.headerOverride)
				require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("header_override", headerOverride).Error)
				t.Cleanup(func() {
					require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("header_override", "").Error)
				})
			}
			if tc.responseLimitMiB > 0 {
				setting := fmt.Sprintf(`{"max_response_body_mib":%d}`, tc.responseLimitMiB)
				require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("setting", setting).Error)
				t.Cleanup(func() {
					require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("setting", nil).Error)
				})
			}
			if tc.upstreamError {
				require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("key", "invalid-test-key").Error)
				constant.ErrorLogEnabled, common.RetryTimes = true, 2
				t.Cleanup(func() {
					constant.ErrorLogEnabled, common.RetryTimes = previousErrorLog, 0
					_ = database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("key", channel.Key).Error
				})
			}
			if tc.wantErrorCode != "" || tc.observeOutcome {
				previousErrorLog := constant.ErrorLogEnabled
				constant.ErrorLogEnabled = true
				t.Cleanup(func() { constant.ErrorLogEnabled = previousErrorLog })
			}
			if tc.fullOptions {
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"search_std":0.003}`))
				t.Cleanup(func() {
					_ = ratio_setting.UpdateModelPriceByJSONString(`{}`)
					model.InvalidatePricingCache()
				})
			}
			model.InvalidatePricingCache()
			expectedPrice, err := operation_setting.ZhipuSearchDefaultPrice(tc.engine)
			require.NoError(t, err)
			if tc.fullOptions {
				expectedPrice = 0.003
			}
			foundPrice := false
			for _, pricing := range model.GetPricing() {
				if pricing.ModelName != tc.engine {
					continue
				}
				foundPrice = true
				assert.Equal(t, 1, pricing.QuotaType)
				assert.Contains(t, pricing.SupportedEndpointTypes, constant.EndpointTypeZhipuWebSearch)
				assert.InDelta(t, expectedPrice, pricing.ModelPrice, 0.000000001)
			}
			require.True(t, foundPrice, "搜索引擎应出现在定价目录中")
			var subscriptionBefore model.UserSubscription
			if tc.subscription {
				require.NoError(t, database.Model(&model.User{}).Where("id = ?", user.Id).Update("setting", `{"billing_preference":"subscription_only"}`).Error)
				t.Cleanup(func() {
					_ = database.Model(&model.User{}).Where("id = ?", user.Id).Update("setting", user.Setting).Error
				})
				require.NoError(t, database.First(&subscriptionBefore, 1).Error)
			}
			var before model.User
			require.NoError(t, database.First(&before, user.Id).Error)
			var beforeToken model.Token
			require.NoError(t, database.First(&beforeToken, token.Id).Error)
			requestID := fmt.Sprintf("live-search-%s-%d", tc.engine, time.Now().UnixNano())
			query := tc.query
			if query == "" {
				query = "人工智能最新新闻"
			}
			payload := map[string]any{
				"search_engine": tc.engine, "search_query": query, "search_intent": tc.searchIntent,
				"search_recency_filter": "noLimit", "content_size": "high", "request_id": requestID, "user_id": "test-user-001",
			}
			if !tc.observeOutcome && (tc.engine == dto.ZhipuSearchStd || tc.engine == dto.ZhipuSearchPro) {
				payload["count"] = 3
			} else if !tc.observeOutcome && tc.engine == dto.ZhipuSearchProSogou {
				payload["count"] = 20
			}
			if !tc.observeOutcome && tc.engine != dto.ZhipuSearchProQuark {
				payload["search_domain_filter"] = "www.sohu.com"
			}
			if tc.fullOptions {
				payload["search_domain_filter"] = "www.gov.cn"
				payload["search_recency_filter"] = "oneYear"
				payload["content_size"] = "high"
			}
			for name, value := range tc.overrides {
				payload[name] = value
			}
			if tc.omitSearchIntent {
				delete(payload, "search_intent")
			}
			if tc.omitSearchQuery {
				delete(payload, "search_query")
			}
			encoded, err := common.Marshal(payload)
			require.NoError(t, err)
			if tc.duplicateFields {
				encoded = []byte(fmt.Sprintf(`{"search_engine":"search_std","search_engine":"search_pro","search_query":"first","search_query":"北京今日天气","SEARCH_QUERY":"ignored","search_intent":false,"count":1,"request_id":%q}`, requestID))
			}
			wireBody := encoded
			if tc.requestEncoding != "" {
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				_, err = writer.Write(encoded)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				wireBody = compressed.Bytes()
			}
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			request, err := http.NewRequest(method, server.URL+tc.path, bytes.NewReader(wireBody))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			if tc.requestEncoding != "" {
				request.Header.Set("Content-Encoding", tc.requestEncoding)
			}
			if tc.legacyAuth {
				request.Header.Set("mj-api-secret", "sk-"+token.Key)
			} else {
				request.Header.Set("Authorization", "Bearer sk-"+token.Key)
			}
			callsBefore := upstreamCalls.Load()
			upstreamRequestBody.Reset()
			upstreamBody.Reset()
			upstreamHeaders = nil
			upstreamAutoDecompressed = false
			upstreamAcceptEncoding = ""
			upstreamLegacyAuthHeader = ""
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("HTTP 请求失败: %s", strings.ReplaceAll(err.Error(), key, "[redacted]"))
			}
			data, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			require.NoError(t, err)
			select {
			case <-completed:
			case <-time.After(10 * time.Second):
				t.Fatal("搜索请求处理未完成")
			}
			assert.True(t, bytes.Equal(encoded, upstreamRequestBody.Bytes()), "上游请求应与客户端原文一致")
			upstreamData := upstreamBody.Bytes()
			encoding := strings.ToLower(strings.TrimSpace(upstreamHeaders.Get("Content-Encoding")))
			switch encoding {
			case "gzip":
				reader, err := gzip.NewReader(bytes.NewReader(upstreamData))
				require.NoError(t, err)
				upstreamData, err = io.ReadAll(reader)
				require.NoError(t, err)
				require.NoError(t, reader.Close())
			case "br":
				var err error
				upstreamData, err = io.ReadAll(brotli.NewReader(bytes.NewReader(upstreamData)))
				require.NoError(t, err)
			case "", "identity":
			default:
				t.Fatalf("未覆盖的上游响应编码: %s", encoding)
			}
			t.Logf("headers:id=%t retry=%t encoding=%q auto_decompressed=%t", upstreamHeaders.Get("X-Request-Id") != "", upstreamHeaders.Get("Retry-After") != "", encoding, upstreamAutoDecompressed)
			if value := upstreamHeaders.Get("Retry-After"); value != "" {
				assert.Equal(t, value, response.Header.Get("Retry-After"), "上游重试提示应被转发")
			}
			if tc.headerOverride != "" {
				assert.Equal(t, tc.headerOverride, upstreamAcceptEncoding)
			}
			if tc.legacyAuth {
				assert.Empty(t, upstreamLegacyAuthHeader)
			}
			if tc.upstreamError || tc.wantErrorCode != "" || (tc.observeOutcome && response.StatusCode >= http.StatusBadRequest) {
				if tc.errorStatus != 0 {
					require.Equal(t, tc.errorStatus, response.StatusCode)
				} else {
					require.GreaterOrEqual(t, response.StatusCode, http.StatusBadRequest)
				}
				if tc.wantErrorCode != "" || tc.observeOutcome {
					assert.True(t, bytes.Equal(upstreamData, data), "错误响应应与上游原文一致")
				}
				assert.EqualValues(t, 1, upstreamCalls.Load()-callsBefore)
				require.Eventually(t, func() bool {
					var currentUser model.User
					var currentToken model.Token
					return database.First(&currentUser, user.Id).Error == nil && database.First(&currentToken, token.Id).Error == nil &&
						currentUser.Quota == before.Quota && currentToken.RemainQuota == beforeToken.RemainQuota
				}, 5*time.Second, 10*time.Millisecond)
				code := gjson.GetBytes(data, "error.code").String()
				if !tc.observeOutcome {
					require.NotEmpty(t, code)
				}
				if tc.wantErrorCode != "" {
					assert.Equal(t, tc.wantErrorCode, code)
				}
				var logged model.Log
				require.NoError(t, database.Where("user_id = ? AND type = ?", user.Id, model.LogTypeError).Order("id DESC").First(&logged).Error)
				assert.Contains(t, logged.Content, code)
				if strings.Contains(logged.Content+logged.Other+string(data), key) {
					t.Error("错误响应或日志包含上游密钥")
				}
				t.Logf("upstream error status=%d code=%s attempts=%d; quota refunded", response.StatusCode, code, upstreamCalls.Load()-callsBefore)
				return
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("HTTP 状态 %d: %s", response.StatusCode, strings.ReplaceAll(string(data), key, "[redacted]"))
			}
			assert.True(t, gjson.ValidBytes(data))
			assert.True(t, bytes.Equal(upstreamData, data), "成功响应应与上游原文一致")
			if tc.observeOutcome {
				echoedQuery := gjson.GetBytes(data, "search_intent.0.query").String()
				t.Logf("obs=%d:%d:%d:%t:%d", response.StatusCode, utf8.RuneCountInString(query), utf8.RuneCountInString(echoedQuery), echoedQuery == query, len(gjson.GetBytes(data, "search_result").Array()))
			}
			if tc.duplicateFields {
				assert.Equal(t, "北京今日天气", gjson.GetBytes(data, "search_intent.0.query").String())
			}
			if tc.responseLimitMiB > 0 {
				assert.LessOrEqual(t, len(data), tc.responseLimitMiB<<20)
			}
			var logged model.Log
			require.NoError(t, database.Where("user_id = ?", user.Id).Order("id DESC").First(&logged).Error)
			assert.Equal(t, tc.engine, logged.ModelName)
			assert.Equal(t, tc.quota, logged.Quota)
			assert.False(t, gjson.Get(logged.Other, "frt").Exists())
			assert.False(t, gjson.Get(logged.Other, "admin_info.usage_billing_path").Exists())
			var after model.User
			require.NoError(t, database.First(&after, user.Id).Error)
			var afterToken model.Token
			require.NoError(t, database.First(&afterToken, token.Id).Error)
			if tc.subscription {
				var subscriptionAfter model.UserSubscription
				require.NoError(t, database.First(&subscriptionAfter, 1).Error)
				assert.Equal(t, before.Quota, after.Quota)
				assert.EqualValues(t, tc.quota, subscriptionAfter.AmountUsed-subscriptionBefore.AmountUsed)
			} else {
				assert.Equal(t, tc.quota, before.Quota-after.Quota)
			}
			assert.Equal(t, tc.quota, beforeToken.RemainQuota-afterToken.RemainQuota)
			assert.Equal(t, requestID, gjson.GetBytes(data, "request_id").String())
			if strings.Contains(logged.Content+logged.Other+string(data), key) {
				t.Error("响应或消费日志包含上游密钥")
			}
			t.Logf("engine=%s status=%d intents=%d results=%d quota=%d bytes=%d limit_mib=%d", tc.engine, response.StatusCode, len(gjson.GetBytes(data, "search_intent").Array()), len(gjson.GetBytes(data, "search_result").Array()), logged.Quota, len(data), tc.responseLimitMiB)
		})
	}
}
