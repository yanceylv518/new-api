package controller_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type zhipuLiveTransport struct {
	base  http.RoundTripper
	key   string
	calls *atomic.Int32
}

func (t zhipuLiveTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() == "open.bigmodel.cn" && request.URL.Path == "/api/paas/v4/web_search" {
		t.calls.Add(1)
		if request.Header.Get("Authorization") == "Bearer test-placeholder" {
			request = request.Clone(request.Context())
			request.Header.Set("Authorization", "Bearer "+t.key)
		}
	}
	return t.base.RoundTrip(request)
}

func TestZhipuWebSearchLive(t *testing.T) {
	if os.Getenv("NEWAPI_RUN_ZHIPU_LIVE") != "1" {
		t.Skip("需要显式启用智谱真实接口测试")
	}
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
	upstreamClient.Transport = zhipuLiveTransport{base: previousTransport, key: key, calls: &upstreamCalls}
	t.Cleanup(func() { upstreamClient.Transport = previousTransport })
	user := model.User{Id: 1, Username: "zhipu-search", Password: "unused", Status: common.UserStatusEnabled, Group: "default", Quota: 10_000_000, Setting: `{"billing_preference":"wallet_only"}`}
	token := model.Token{Id: 1, UserId: user.Id, Key: "zhipusearchtest", Name: "test", Status: common.TokenStatusEnabled, RemainQuota: 10_000_000, ExpiredTime: -1}
	channel := model.Channel{Id: 1, Name: "zhipu-search", Type: constant.ChannelTypeZhipu_v4, Status: common.ChannelStatusEnabled, Key: "test-placeholder", Models: "search_std,search_pro,search_pro_sogou,search_pro_quark", Group: "default", AutoBan: common.GetPointer(0)}
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
	for _, tc := range []struct {
		name          string
		engine        string
		quota         int
		path          string
		fullOptions   bool
		subscription  bool
		upstreamError bool
	}{
		{name: dto.ZhipuSearchStd, engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search"},
		{name: dto.ZhipuSearchPro, engine: dto.ZhipuSearchPro, quota: 1027, path: "/api/paas/v4/web_search"},
		{name: dto.ZhipuSearchProSogou, engine: dto.ZhipuSearchProSogou, quota: 3425, path: "/v1/web_search"},
		{name: dto.ZhipuSearchProQuark, engine: dto.ZhipuSearchProQuark, quota: 3425, path: "/v1/web_search"},
		{name: "options_and_price_override", engine: dto.ZhipuSearchStd, quota: 1500, path: "/v1/web_search", fullOptions: true},
		{name: "subscription", engine: dto.ZhipuSearchStd, quota: 685, path: "/v1/web_search", subscription: true},
		{name: "upstream_auth_error", engine: dto.ZhipuSearchStd, path: "/v1/web_search", upstreamError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.upstreamError {
				require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("key", "invalid-test-key").Error)
				constant.ErrorLogEnabled, common.RetryTimes = true, 2
				t.Cleanup(func() {
					constant.ErrorLogEnabled, common.RetryTimes = previousErrorLog, 0
					_ = database.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("key", channel.Key).Error
				})
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
			payload := map[string]any{
				"search_engine": tc.engine, "search_query": "人工智能最新新闻", "search_intent": false,
				"search_recency_filter": "noLimit", "content_size": "high", "request_id": requestID, "user_id": "test-user-001",
			}
			if tc.engine == dto.ZhipuSearchStd || tc.engine == dto.ZhipuSearchPro {
				payload["count"] = 3
			} else if tc.engine == dto.ZhipuSearchProSogou {
				payload["count"] = 11
			} else if tc.engine == dto.ZhipuSearchProQuark {
				payload["count"] = 2
			}
			payload["search_domain_filter"] = "www.sohu.com"
			if tc.fullOptions {
				payload["search_domain_filter"] = "www.gov.cn"
				payload["search_recency_filter"] = "oneYear"
				payload["content_size"] = "high"
			}
			encoded, err := common.Marshal(payload)
			require.NoError(t, err)
			request, err := http.NewRequest(http.MethodPost, server.URL+tc.path, bytes.NewReader(encoded))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer sk-"+token.Key)
			callsBefore := upstreamCalls.Load()
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("HTTP 请求失败: %s", strings.ReplaceAll(err.Error(), key, "[redacted]"))
			}
			data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
			_ = response.Body.Close()
			require.NoError(t, err)
			if tc.upstreamError {
				require.GreaterOrEqual(t, response.StatusCode, http.StatusBadRequest)
				select {
				case <-completed:
				case <-time.After(10 * time.Second):
					t.Fatal("失败请求处理未完成")
				}
				assert.EqualValues(t, 1, upstreamCalls.Load()-callsBefore)
				require.Eventually(t, func() bool {
					var currentUser model.User
					var currentToken model.Token
					return database.First(&currentUser, user.Id).Error == nil && database.First(&currentToken, token.Id).Error == nil &&
						currentUser.Quota == before.Quota && currentToken.RemainQuota == beforeToken.RemainQuota
				}, 5*time.Second, 10*time.Millisecond)
				code := gjson.GetBytes(data, "error.code").String()
				require.NotEmpty(t, code)
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
			select {
			case <-completed:
			case <-time.After(10 * time.Second):
				t.Fatal("搜索请求处理未完成")
			}
			assert.True(t, gjson.ValidBytes(data))
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
			t.Logf("engine=%s status=%d results=%d quota=%d", tc.engine, response.StatusCode, len(gjson.GetBytes(data, "search_result").Array()), logged.Quota)
		})
	}
}
