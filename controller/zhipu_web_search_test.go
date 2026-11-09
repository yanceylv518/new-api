package controller_test

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/andybalholm/brotli"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

type zhipuSearchUpstream struct {
	mu               sync.Mutex
	status           int
	body             string
	truncated        bool
	closeNext        bool
	knownLength      bool
	compressed       bool
	brotliIfAccepted bool
	forceBrotli      bool
	headers          http.Header
	paths            []string
	bodies           []string
	auth             []string
	requestHeaders   []http.Header
}

func TestZhipuSearchValidationWithoutDistributor(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"valid", `{"search_engine":"search_std","search_query":"test","search_intent":false}`, true},
		{"duplicate engine", `{"search_engine":"search_std","search_engine":"search_pro","search_query":"test"}`, true},
		{"invalid final engine", `{"search_engine":"search_std","search_engine":"unknown","search_query":"test"}`, false},
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
	u.paths = append(u.paths, r.URL.RequestURI())
	u.bodies = append(u.bodies, string(body))
	u.auth = append(u.auth, r.Header.Get("Authorization"))
	u.requestHeaders = append(u.requestHeaders, r.Header.Clone())
	status, response, truncated, closeNext := u.status, u.body, u.truncated, u.closeNext
	knownLength, compressed, brotliIfAccepted, forceBrotli, headers := u.knownLength, u.compressed, u.brotliIfAccepted, u.forceBrotli, u.headers.Clone()
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
	for name, values := range headers {
		w.Header()[name] = values
	}
	if compressed {
		var encoded bytes.Buffer
		writer := gzip.NewWriter(&encoded)
		_, _ = writer.Write([]byte(response))
		_ = writer.Close()
		response = encoded.String()
		w.Header().Set("Content-Encoding", "gzip")
	}
	if forceBrotli || (brotliIfAccepted && strings.Contains(r.Header.Get("Accept-Encoding"), "br")) {
		var encoded bytes.Buffer
		writer := brotli.NewWriter(&encoded)
		_, _ = writer.Write([]byte(response))
		_ = writer.Close()
		response = encoded.String()
		w.Header().Set("Content-Encoding", "br")
	}
	if truncated {
		w.Header().Set("Content-Length", strconv.Itoa(len(response)+10))
	} else if knownLength {
		w.Header().Set("Content-Length", strconv.Itoa(len(response)))
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, response)
}

func (u *zhipuSearchUpstream) setResponse(status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status, u.body = status, body
}

func (u *zhipuSearchUpstream) setResponseHeaders(headers http.Header) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.headers = headers.Clone()
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
	return setupZhipuSearchContractForUser(t, 1)
}

func setupZhipuSearchContractForUser(t *testing.T, userID int) (*gin.Engine, *gorm.DB, *zhipuSearchUpstream) {
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
	schema := []any{&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Log{}, &model.Model{}, &model.Vendor{}, &model.Option{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}}
	if os.Getenv("ZHIPU_SEARCH_TEST_DISPOSABLE_DSN") != "" {
		// 外部隔离测试库不会随连接关闭清空，避免不同用例共享夹具数据。
		require.NoError(t, database.Migrator().DropTable(schema...))
	}
	require.NoError(t, database.AutoMigrate(schema...))
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
	service.InitHttpClient()

	upstream := &zhipuSearchUpstream{status: http.StatusOK, body: `{"id":"upstream-id","search_result":[{"title":"result"}],"extra":{"keep":true}}`}
	upstreamServer := httptest.NewServer(upstream)
	t.Cleanup(upstreamServer.Close)
	user := model.User{Id: userID, Username: "search-fixture", Status: common.UserStatusEnabled, Group: "default", Quota: 10_000_000, Setting: `{"billing_preference":"wallet_only"}`}
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
	require.NoError(t, database.First(&token, tokenID).Error)
	require.NoError(t, database.First(&user, token.UserId).Error)
	return user.Quota, token.RemainQuota
}

func TestZhipuSearchRootCauseFixes(t *testing.T) {
	const payload = `{"search_engine":"search_std","search_query":"test"}`
	t.Run("response error fields cannot be overwritten", func(t *testing.T) {
		engine, database, upstream := setupZhipuSearchContract(t)
		for _, tc := range []struct{ name, body string }{
			{"alias after error", `{"error":{"code":"invalid_key","message":"rejected upstream-placeholder"},"ERROR":null}`},
			{"alias before error", `{"ERROR":null,"error":{"code":"invalid_key","message":"rejected upstream-placeholder"}}`},
			{"duplicate null after error", `{"error":{"code":"invalid_key","message":"rejected upstream-placeholder"},"error":null}`},
			{"duplicate null before error", `{"error":null,"error":{"code":"invalid_key","message":"rejected upstream-placeholder"}}`},
			{"escaped duplicate null", `{"error":{"code":"invalid_key","message":"rejected upstream-placeholder"},"err\u006fr":null}`},
			{"alias only error", `{"ERROR":{"code":"invalid_key","message":"rejected upstream-placeholder"}}`},
			{"false cannot clear error", `{"error":{"code":"invalid_key","message":"rejected upstream-placeholder"},"error":false}`},
			{"empty string cannot clear error", `{"error":{"code":"invalid_key","message":"rejected upstream-placeholder"},"error":""}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				upstream.setResponse(http.StatusOK, tc.body)
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
				require.Equal(t, http.StatusOK, response.Code)
				assert.Equal(t, strings.ReplaceAll(tc.body, "upstream-placeholder", "***"), response.Body.String())
				assert.Eventually(t, func() bool {
					user, token := zhipuSearchBalances(t, database, 1)
					return user == beforeUser && token == beforeToken
				}, 5*time.Second, 10*time.Millisecond)
				var entry model.Log
				require.NoError(t, database.Where("request_id = ? AND type = ?", response.Header().Get(common.RequestIdKey), model.LogTypeError).First(&entry).Error)
				assert.Equal(t, "invalid_key", gjson.Get(entry.Other, "error_code").String())
				assert.NotContains(t, entry.Content+entry.Other, "upstream-placeholder")
			})
		}
	})

	t.Run("malformed credential responses", func(t *testing.T) {
		engine, database, upstream := setupZhipuSearchContract(t)
		for _, tc := range []struct{ name, body, want string }{
			{"unclosed unicode string", `{"error":{"message":"\u0075pstream-placeholder`, `{"error":{"message":"***"`},
			{"dangling escape", `{"error":{"message":"\u0075pstream-placeholder\`, `{"error":{"message":"***"`},
			{"invalid closed string", `{"error":{"message":"\q\u0075pstream-placeholder"},"trace":"kept"}`, `{"error":{"message":"***"},"trace":"kept"}`},
			{"nested unclosed string", `{"error":{"message":"nested: {\"message\":\"\\u0075pstream-placeholder"}}`, `{"error":{"message":"nested: {\"message\":\"***\""}}`},
		} {
			for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
				t.Run(fmt.Sprintf("%s/%d", tc.name, status), func(t *testing.T) {
					upstream.setResponse(status, tc.body)
					response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
					require.Equal(t, status, response.Code)
					assert.Equal(t, tc.want, response.Body.String())
					if status == http.StatusBadRequest || gjson.Valid(tc.body) {
						var entry model.Log
						require.NoError(t, database.Where("type = ?", model.LogTypeError).Order("id DESC").First(&entry).Error)
						assert.NotContains(t, entry.Content+entry.Other, "upstream-placeholder")
						assert.NotContains(t, entry.Content+entry.Other, `\u0075pstream-placeholder`)
					}
				})
			}
		}
	})

	for _, cacheEnabled := range []bool{false, true} {
		for _, wallet := range []int{700, 1500} {
			t.Run(fmt.Sprintf("cross group reservation/cache_%t/wallet_%d", cacheEnabled, wallet), func(t *testing.T) {
				engine, database, upstream := setupZhipuSearchContract(t)
				previousAuto, previousUsable := setting.AutoGroups2JsonString(), setting.UserUsableGroups2JSONString()
				previousRatios, previousSpecial := ratio_setting.GroupRatio2JSONString(), ratio_setting.GroupGroupRatio2JSONString()
				t.Cleanup(func() {
					require.NoError(t, setting.UpdateAutoGroupsByJsonString(previousAuto))
					require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousUsable))
					require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
					require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(previousSpecial))
				})
				require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["default","vip"]`))
				require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"auto":"Auto","default":"Default","vip":"VIP"}`))
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2}`))
				require.NoError(t, ratio_setting.UpdateGroupGroupRatioByJSONString(`{}`))
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"search_std":0.00137}`))
				require.NoError(t, database.Model(&model.User{}).Where("id = ?", 1).Updates(map[string]any{
					"quota": wallet, "setting": `{"billing_preference":"wallet_only","quota_warning_threshold":1}`,
				}).Error)
				require.NoError(t, database.Model(&model.Token{}).Where("id = ?", 1).Updates(map[string]any{
					"group": "auto", "auto_groups": `["default","vip"]`, "cross_group_retry": true,
				}).Error)
				var primary model.Channel
				require.NoError(t, database.First(&primary, 1).Error)
				require.NoError(t, database.Model(&primary).Update("header_override", `{"re:[":""}`).Error)
				backup := model.Channel{Id: 2, Name: "vip-search", Type: constant.ChannelTypeZhipuWebSearch, Status: common.ChannelStatusEnabled,
					Key: "vip-placeholder", BaseURL: primary.BaseURL, Models: "search_std", Group: "vip", AutoBan: common.GetPointer(0)}
				require.NoError(t, model.BatchInsertChannels([]model.Channel{backup}))
				common.RetryTimes, common.MemoryCacheEnabled = 1, cacheEnabled
				if cacheEnabled {
					model.InitChannelCache()
				}
				_, beforeToken := zhipuSearchBalances(t, database, 1)
				response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
				paths, _ := upstream.snapshot()
				if wallet == 700 {
					require.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
					assert.Equal(t, string(types.ErrorCodeInsufficientUserQuota), gjson.Get(response.Body.String(), "error.code").String())
					assert.Empty(t, paths)
					require.Eventually(t, func() bool {
						user, token := zhipuSearchBalances(t, database, 1)
						return user == wallet && token == beforeToken
					}, 5*time.Second, 10*time.Millisecond)
					return
				}
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				assert.Len(t, paths, 1)
				user, token := zhipuSearchBalances(t, database, 1)
				assert.Equal(t, wallet-1370, user)
				assert.Equal(t, beforeToken-1370, token)
				var entry model.Log
				require.NoError(t, database.Where("type = ?", model.LogTypeConsume).First(&entry).Error)
				assert.Equal(t, "vip", entry.Group)
				assert.Equal(t, 1370, entry.Quota)
			})
		}
	}

	for _, sendHeaders := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancellation/headers_%t", sendHeaders), func(t *testing.T) {
			engine, database, _ := setupZhipuSearchContract(t)
			previousDisable, previousCodes := common.AutomaticDisableChannelEnabled, operation_setting.AutomaticDisableStatusCodesToString()
			common.AutomaticDisableChannelEnabled = true
			require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString("408,500,502"))
			t.Cleanup(func() {
				common.AutomaticDisableChannelEnabled = previousDisable
				require.NoError(t, operation_setting.AutomaticDisableStatusCodesFromString(previousCodes))
			})
			ready, closed, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				calls.Add(1)
				if sendHeaders {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(ready)
				select {
				case <-r.Context().Done():
					close(closed)
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release); server.Close() })
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{"base_url": server.URL, "auto_ban": 1}).Error)
			beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
			baselineWorkers := gopool.WorkerCount()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(payload)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer sk-fixturetoken")
			response, done := httptest.NewRecorder(), make(chan struct{})
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
				t.Fatal("取消后网关未结束请求")
			}
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("取消后上游连接未关闭")
			}
			assert.Equal(t, http.StatusRequestTimeout, response.Code)
			assert.Equal(t, string(types.ErrorCodeRequestCancelled), gjson.Get(response.Body.String(), "error.code").String())
			assert.EqualValues(t, 1, calls.Load())
			require.Eventually(t, func() bool {
				user, token := zhipuSearchBalances(t, database, 1)
				return user == beforeUser && token == beforeToken
			}, 5*time.Second, 10*time.Millisecond)
			require.Eventually(t, func() bool { return gopool.WorkerCount() <= baselineWorkers }, 5*time.Second, 10*time.Millisecond)
			var channel model.Channel
			require.NoError(t, database.First(&channel, 1).Error)
			assert.Equal(t, common.ChannelStatusEnabled, channel.Status)
			var count int64
			require.NoError(t, database.Model(&model.Log{}).Where("type IN ?", []int{model.LogTypeError, model.LogTypeConsume}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}

	t.Run("upstream cancellation code remains an upstream error", func(t *testing.T) {
		engine, database, upstream := setupZhipuSearchContract(t)
		upstream.setResponse(http.StatusUnauthorized, `{"error":{"code":"request_cancelled","message":"upstream rejected"}}`)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		require.Equal(t, http.StatusUnauthorized, response.Code)
		var entry model.Log
		require.NoError(t, database.Where("type = ?", model.LogTypeError).First(&entry).Error)
		assert.Equal(t, "request_cancelled", gjson.Get(entry.Other, "error_code").String())
	})
}

func TestZhipuSearchReportedIssues(t *testing.T) {
	engine, database, upstream := setupZhipuSearchContract(t)
	const payload = `{"search_engine":"search_std","search_query":"test"}`
	const success = `{"id":"upstream-id","search_result":[]}`

	t.Run("websocket authentication is not forwarded", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-fixturetoken")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Empty(t, response.Header().Get("Retry-After"))
		upstream.mu.Lock()
		headers := upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
		upstream.mu.Unlock()
		assert.Empty(t, headers.Get("Sec-WebSocket-Protocol"))
		assert.Equal(t, "Bearer upstream-placeholder", headers.Get("Authorization"))
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", `{"*":""}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", "").Error)
		})
		request = httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.sk-fixturetoken")
		response = httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		upstream.mu.Lock()
		headers = upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
		upstream.mu.Unlock()
		assert.Empty(t, headers.Get("Sec-WebSocket-Protocol"))
	})

	t.Run("compressed business error refunds", func(t *testing.T) {
		upstream.setResponse(http.StatusOK, `{"error":{"code":"400","message":"upstream-placeholder rejected"}}`)
		upstream.mu.Lock()
		upstream.forceBrotli = true
		upstream.mu.Unlock()
		defer func() {
			upstream.setResponse(http.StatusOK, success)
			upstream.mu.Lock()
			upstream.forceBrotli = false
			upstream.mu.Unlock()
		}()
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		request := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer sk-fixturetoken")
		request.Header.Set("Accept-Encoding", "br")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Empty(t, response.Header().Get("Content-Encoding"))
		assert.Equal(t, "400", gjson.Get(response.Body.String(), "error.code").String())
		assert.NotContains(t, response.Body.String(), "upstream-placeholder")
		upstream.mu.Lock()
		headers := upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
		upstream.mu.Unlock()
		assert.NotContains(t, headers.Get("Accept-Encoding"), "br")
		assert.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 3*time.Second, 10*time.Millisecond)
	})

	t.Run("compression header overrides handle responses", func(t *testing.T) {
		for _, channel := range []struct {
			name        string
			channelType int
		}{{"zhipu", constant.ChannelTypeZhipuWebSearch}, {"newapi", constant.ChannelTypeNewAPI}} {
			for _, encoding := range []string{"gzip", "br"} {
				t.Run(channel.name+"_"+encoding, func(t *testing.T) {
					override := fmt.Sprintf(`{"Accept-Encoding":%q}`, encoding)
					require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{"type": channel.channelType, "header_override": override}).Error)
					t.Cleanup(func() {
						require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{"type": constant.ChannelTypeZhipuWebSearch, "header_override": ""}).Error)
					})
					upstream.mu.Lock()
					upstream.compressed = encoding == "gzip"
					upstream.brotliIfAccepted = encoding == "br"
					upstream.mu.Unlock()
					defer func() {
						upstream.setResponse(http.StatusOK, success)
						upstream.mu.Lock()
						upstream.compressed = false
						upstream.brotliIfAccepted = false
						upstream.mu.Unlock()
					}()
					upstream.setResponse(http.StatusOK, `{"error":{"code":"400","message":"upstream-placeholder rejected"}}`)
					beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
					response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
					assert.Equal(t, http.StatusOK, response.Code)
					assert.Equal(t, "400", gjson.Get(response.Body.String(), "error.code").String())
					assert.NotContains(t, response.Body.String(), "upstream-placeholder")
					upstream.mu.Lock()
					requestHeaders := upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
					upstream.mu.Unlock()
					assert.Equal(t, encoding, requestHeaders.Get("Accept-Encoding"))
					assert.Eventually(t, func() bool {
						user, token := zhipuSearchBalances(t, database, 1)
						return user == beforeUser && token == beforeToken
					}, 3*time.Second, 10*time.Millisecond)

					upstream.setResponse(http.StatusOK, success)
					response = zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
					assert.Equal(t, http.StatusOK, response.Code)
					assert.Equal(t, success, response.Body.String())
					afterUser, afterToken := zhipuSearchBalances(t, database, 1)
					assert.Equal(t, 685, beforeUser-afterUser)
					assert.Equal(t, 685, beforeToken-afterToken)
				})
			}
		}
	})

	t.Run("gzip request routes and succeeds", func(t *testing.T) {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		_, err := writer.Write([]byte(payload))
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		for _, encoding := range []string{"gzip", "GZip"} {
			t.Run(encoding, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodPost, "/v1/web_search", bytes.NewReader(compressed.Bytes()))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Content-Encoding", encoding)
				request.Header.Set("Authorization", "Bearer sk-fixturetoken")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
				_, bodies := upstream.snapshot()
				require.NotEmpty(t, bodies)
				assert.Equal(t, payload, bodies[len(bodies)-1])
				upstream.mu.Lock()
				headers := upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
				upstream.mu.Unlock()
				assert.Empty(t, headers.Get("Content-Encoding"))
			})
		}
	})

	t.Run("legacy token is not forwarded by wildcard", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", `{"*":""}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", "").Error)
		})
		request := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("mj-api-secret", "sk-fixturetoken")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		upstream.mu.Lock()
		headers := upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
		upstream.mu.Unlock()
		assert.Empty(t, headers.Get("mj-api-secret"))
		assert.Equal(t, "Bearer upstream-placeholder", headers.Get("Authorization"))

		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("header_override", `{"mj-api-secret":"upstream-legacy-key"}`).Error)
		response = zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		upstream.mu.Lock()
		headers = upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
		upstream.mu.Unlock()
		assert.Equal(t, "upstream-legacy-key", headers.Get("mj-api-secret"))
	})

	t.Run("string business error is redacted and refunded", func(t *testing.T) {
		upstream.setResponse(http.StatusOK, `{"error":"upstream-placeholder rejected"}`)
		defer upstream.setResponse(http.StatusOK, success)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		assert.Equal(t, http.StatusOK, response.Code)
		assert.Equal(t, `{"error":"*** rejected"}`, response.Body.String())
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("empty error markers remain successful searches", func(t *testing.T) {
		defer upstream.setResponse(http.StatusOK, success)
		for _, tc := range []struct{ name, marker string }{{"null", "null"}, {"false", "false"}, {"empty string", `""`}, {"blank string", `"  "`}} {
			t.Run(tc.name, func(t *testing.T) {
				body := `{"error":` + tc.marker + `,"search_result":[{"title":"kept"}]}`
				upstream.setResponse(http.StatusOK, body)
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
				require.Equal(t, http.StatusOK, response.Code)
				assert.Equal(t, body, response.Body.String())
				var entry model.Log
				require.NoError(t, database.Where("request_id = ? AND type = ?", response.Header().Get(common.RequestIdKey), model.LogTypeConsume).First(&entry).Error)
				assert.Equal(t, 685, entry.Quota)
				afterUser, afterToken := zhipuSearchBalances(t, database, 1)
				assert.Equal(t, 685, beforeUser-afterUser)
				assert.Equal(t, 685, beforeToken-afterToken)
			})
		}
	})

	t.Run("empty 205 preserves gzip metadata", func(t *testing.T) {
		upstream.setResponse(http.StatusResetContent, "")
		upstream.setResponseHeaders(http.Header{"Content-Encoding": {"gzip"}, "Content-Length": {"0"}})
		defer upstream.setResponse(http.StatusOK, success)
		defer upstream.setResponseHeaders(nil)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		require.Equal(t, http.StatusResetContent, response.Code, response.Body.String())
		assert.Empty(t, response.Body.String())
		assert.Equal(t, "gzip", response.Header().Get("Content-Encoding"))
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 685, beforeUser-afterUser)
		assert.Equal(t, 685, beforeToken-afterToken)
	})

	t.Run("unsupported upstream encoding is not billed", func(t *testing.T) {
		upstream.setResponse(http.StatusOK, `{"error":"upstream-placeholder rejected"}`)
		upstream.setResponseHeaders(http.Header{"Content-Encoding": {"unknown"}})
		defer upstream.setResponse(http.StatusOK, success)
		defer upstream.setResponseHeaders(nil)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		assert.Equal(t, http.StatusBadGateway, response.Code)
		assert.NotContains(t, response.Body.String(), "upstream-placeholder")
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("required upstream response headers are preserved", func(t *testing.T) {
		defer upstream.setResponse(http.StatusOK, success)
		defer upstream.setResponseHeaders(nil)
		for _, tc := range []struct {
			method string
			status int
			name   string
			value  string
		}{
			{http.MethodPost, http.StatusMethodNotAllowed, "Allow", "POST"},
			{http.MethodPost, http.StatusFound, "Location", "/search-redirect"},
			{http.MethodHead, http.StatusOK, "Content-Encoding", "gzip"},
			{http.MethodGet, http.StatusNotModified, "Content-Encoding", "gzip"},
		} {
			upstream.setResponse(tc.status, `{"error":{"message":"rejected"}}`)
			upstream.setResponseHeaders(http.Header{tc.name: {tc.value}})
			request := httptest.NewRequest(tc.method, "/v1/web_search", strings.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer sk-fixturetoken")
			request.Header.Set("Origin", "https://client.example")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code)
			assert.Equal(t, tc.value, response.Header().Get(tc.name))
			assert.Contains(t, strings.Split(response.Header().Get("Access-Control-Expose-Headers"), ","), tc.name)
		}
	})

	t.Run("redacted error does not retain upstream digest", func(t *testing.T) {
		upstream.setResponse(http.StatusBadRequest, `{"error":{"code":"400","message":"upstream-placeholder rejected"}}`)
		upstream.setResponseHeaders(http.Header{"Content-Digest": {"sha-256=:fixture:"}})
		defer upstream.setResponse(http.StatusOK, success)
		defer upstream.setResponseHeaders(nil)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.NotContains(t, response.Body.String(), "upstream-placeholder")
		assert.Empty(t, response.Header().Get("Content-Digest"))
	})

	t.Run("search authentication preflight is allowed", func(t *testing.T) {
		for _, path := range []string{"/v1/web_search", "/api/paas/v4/web_search"} {
			for _, method := range []string{http.MethodPost, http.MethodPatch} {
				request := httptest.NewRequest(http.MethodOptions, path, nil)
				request.Header.Set("Origin", "https://client.example")
				request.Header.Set("Access-Control-Request-Method", method)
				request.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, request)
				assert.Contains(t, strings.Split(response.Header().Get("Access-Control-Allow-Methods"), ","), method)
				assert.Contains(t, strings.Split(response.Header().Get("Access-Control-Allow-Headers"), ","), "Authorization")
			}
		}
		request := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		request.Header.Set("Origin", "https://client.example")
		request.Header.Set("Access-Control-Request-Method", http.MethodPatch)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		assert.NotContains(t, strings.Split(response.Header().Get("Access-Control-Allow-Methods"), ","), http.MethodPatch)
		assert.NotContains(t, strings.Split(response.Header().Get("Access-Control-Allow-Headers"), ","), "Authorization")
	})

	t.Run("upstream cannot clear gateway site data", func(t *testing.T) {
		upstream.setResponseHeaders(http.Header{"Clear-Site-Data": {`"cookies"`}})
		defer upstream.setResponseHeaders(nil)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Empty(t, response.Header().Get("Clear-Site-Data"))
	})

	t.Run("content type does not expose upstream credentials", func(t *testing.T) {
		upstream.setResponseHeaders(http.Header{"Content-Type": {"application/json; token=upstream-placeholder"}})
		defer upstream.setResponseHeaders(nil)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.NotContains(t, response.Header().Get("Content-Type"), "upstream-placeholder")
		assert.Contains(t, response.Header().Get("Content-Type"), "***")
	})
}

func TestZhipuSearchZstdWindowLimit(t *testing.T) {
	const payload = `{"search_engine":"search_std","search_query":"test"}`
	const body = `{"search_result":[{"title":"kept"}]}`
	t.Run("stacked windows share the channel budget", func(t *testing.T) {
		engine, database, upstream := setupZhipuSearchContract(t)
		data := []byte(body)
		for range 2 {
			var buf bytes.Buffer
			writer, err := zstd.NewWriter(&buf, zstd.WithWindowSize(1<<20))
			require.NoError(t, err)
			_, err = writer.Write(data)
			require.NoError(t, err)
			require.NoError(t, writer.Flush())
			require.NoError(t, writer.Close())
			data = buf.Bytes()
			var header zstd.Header
			require.NoError(t, header.Decode(data))
			require.EqualValues(t, 1<<20, header.WindowSize)
		}
		upstream.setResponse(http.StatusOK, string(data))
		upstream.setResponseHeaders(http.Header{"Content-Encoding": {"zstd, zstd"}})
		for _, limit := range []int{1, 2} {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).
				Update("setting", fmt.Sprintf(`{"max_response_body_mib":%d}`, limit)).Error)
			before, beforeToken := zhipuSearchBalances(t, database, 1)
			response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
			if limit == 1 {
				require.Equal(t, http.StatusBadGateway, response.Code)
				require.Eventually(t, func() bool {
					user, token := zhipuSearchBalances(t, database, 1)
					return user == before && token == beforeToken
				}, time.Second*5, 10*time.Millisecond)
			} else {
				require.Equal(t, http.StatusOK, response.Code)
				require.Equal(t, body, response.Body.String())
			}
		}
	})
	for _, tc := range []struct {
		name        string
		channelType int
		limitMiB    int
		windowMiB   uint64
		windowByte  byte
		wantStatus  int
	}{
		{"window at limit", constant.ChannelTypeZhipuWebSearch, 1, 1, 0x50, http.StatusOK},
		{"window exceeds limit", constant.ChannelTypeZhipuWebSearch, 1, 8, 0x68, http.StatusBadGateway},
		{"larger configured limit", constant.ChannelTypeZhipuWebSearch, 8, 8, 0x68, http.StatusOK},
		{"unlimited", constant.ChannelTypeZhipuWebSearch, 0, 8, 0x68, http.StatusOK},
		{"New API is unaffected", constant.ChannelTypeNewAPI, 1, 8, 0x68, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, database, upstream := setupZhipuSearchContract(t)
			setting := fmt.Sprintf(`{"max_response_body_mib":%d}`, tc.limitMiB)
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{"type": tc.channelType, "setting": setting}).Error)
			// 使用无校验和的原始 Zstandard 块，让很小的正文声明独立的解码窗口。
			blockHeader := uint32(len(body))<<3 | 1
			frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0, tc.windowByte, byte(blockHeader), byte(blockHeader >> 8), byte(blockHeader >> 16)}
			frame = append(frame, body...)
			var header zstd.Header
			require.NoError(t, header.Decode(frame))
			require.Equal(t, tc.windowMiB<<20, header.WindowSize)
			upstream.setResponse(http.StatusOK, string(frame))
			upstream.setResponseHeaders(http.Header{"Content-Encoding": {"zstd"}})
			beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
			response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
			require.Equal(t, tc.wantStatus, response.Code, response.Body.String())
			if tc.wantStatus == http.StatusOK {
				assert.Equal(t, body, response.Body.String())
				assert.Empty(t, response.Header().Get("Content-Encoding"))
				user, token := zhipuSearchBalances(t, database, 1)
				assert.Equal(t, 685, beforeUser-user)
				assert.Equal(t, 685, beforeToken-token)
				return
			}
			assert.Equal(t, string(types.ErrorCodeReadResponseBodyFailed), gjson.Get(response.Body.String(), "error.code").String())
			require.Eventually(t, func() bool {
				user, token := zhipuSearchBalances(t, database, 1)
				return user == beforeUser && token == beforeToken
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestZhipuSearchCompressedResponses(t *testing.T) {
	engine, _, upstream := setupZhipuSearchContract(t)
	const payload = `{"search_engine":"search_std","search_query":"test"}`
	const body = `{"search_result":[{"title":"result"}]}`
	for _, tc := range []struct {
		name      string
		encodings []string
	}{
		{"gzip", []string{"GZip"}},
		{"brotli", []string{"br"}},
		{"deflate", []string{"deflate"}},
		{"zstd", []string{"zstd"}},
		{"multiple encodings", []string{"gzip", "br"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := []byte(body)
			for _, encoding := range tc.encodings {
				var compressed bytes.Buffer
				var writer io.WriteCloser
				switch strings.ToLower(encoding) {
				case "gzip":
					writer = gzip.NewWriter(&compressed)
				case "br":
					writer = brotli.NewWriter(&compressed)
				case "deflate":
					writer = zlib.NewWriter(&compressed)
				case "zstd":
					var err error
					writer, err = zstd.NewWriter(&compressed)
					require.NoError(t, err)
				}
				_, err := writer.Write(encoded)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				encoded = compressed.Bytes()
			}
			upstream.setResponse(http.StatusOK, string(encoded))
			upstream.setResponseHeaders(http.Header{"Content-Encoding": tc.encodings})
			response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, body, response.Body.String())
			assert.Empty(t, response.Header().Get("Content-Encoding"))
		})
	}
}

func TestZhipuSearchRedirects(t *testing.T) {
	const payload = `{"search_engine":"search_std","search_query":"test"}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		charge int
	}{
		{"resource created", http.StatusCreated, `{"search_result":[{"title":"weather"}]}`, 685},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"retry tomorrow"}}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, db, upstream := setupZhipuSearchContract(t)
			headers := http.Header{"Location": {"https://catalog.example/resources/123"}}
			if tc.status == http.StatusTooManyRequests {
				headers.Set("Retry-After", "12")
			}
			upstream.setResponse(tc.status, tc.body)
			upstream.setResponseHeaders(headers)
			before, beforeToken := zhipuSearchBalances(t, db, 1)
			res := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
			require.Equal(t, tc.status, res.Code, res.Body.String())
			assert.Equal(t, tc.body, res.Body.String())
			assert.Empty(t, res.Header().Get("Location"))
			assert.Equal(t, headers.Get("Retry-After"), res.Header().Get("Retry-After"))
			require.Eventually(t, func() bool {
				user, token := zhipuSearchBalances(t, db, 1)
				return before-user == tc.charge && beforeToken-token == tc.charge
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
	t.Run("client credentials stay at gateway", func(t *testing.T) {
		engine, database, upstream := setupZhipuSearchContract(t)
		var targetCalls atomic.Int32
		seenHeaders := make(chan http.Header, 1)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			targetCalls.Add(1)
			seenHeaders <- r.Header.Clone()
			_, _ = io.WriteString(w, `{"target":"untrusted"}`)
		}))
		t.Cleanup(target.Close)
		upstream.setResponse(http.StatusFound, `{"error":{"message":"redirect"}}`)
		upstream.setResponseHeaders(http.Header{"Location": {target.URL + "/collect"}})
		gateway := httptest.NewServer(engine)
		t.Cleanup(gateway.Close)
		request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/web_search", strings.NewReader(payload))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer sk-fixturetoken")
		request.Header.Set("X-Client-Credential", "client-placeholder")
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		client := gateway.Client()
		client.Timeout = 10 * time.Second
		response, err := client.Do(request)
		require.NoError(t, err)
		defer response.Body.Close()
		_, err = io.ReadAll(response.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadGateway, response.StatusCode)
		assert.Empty(t, response.Header.Get("Location"))
		assert.Zero(t, targetCalls.Load())
		select {
		case headers := <-seenHeaders:
			assert.Empty(t, headers.Get("Authorization"))
			assert.Empty(t, headers.Get("X-Client-Credential"))
		default:
		}
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("redirect target policy", func(t *testing.T) {
		engine, database, upstream := setupZhipuSearchContract(t)
		var channel model.Channel
		require.NoError(t, database.First(&channel, 1).Error)
		base := channel.GetBaseURL()
		for _, tc := range []struct {
			name      string
			locations []string
			want      string
			status    int
		}{
			{"relative path", []string{"/search-redirect"}, "/search-redirect", http.StatusFound},
			{"relative reference", []string{"next?trace=kept"}, "/api/paas/v4/next?trace=kept", http.StatusFound},
			{"same origin absolute", []string{base + "/v1/web_search?trace=one%2Ftwo#part"}, "/v1/web_search?trace=one%2Ftwo#part", http.StatusFound},
			{"encoded credential name", []string{base + "/search-redirect?access%5ftoken=fixture-redirect-secret&trace=kept"}, "/search-redirect?access%5ftoken=***&trace=kept", http.StatusFound},
			{"cross origin", []string{"https://untrusted.invalid/collect"}, "", http.StatusBadGateway},
			{"scheme relative cross origin", []string{"//untrusted.invalid/collect"}, "", http.StatusBadGateway},
			{"different port", []string{"http://127.0.0.1:1/collect"}, "", http.StatusBadGateway},
			{"different scheme", []string{strings.Replace(base, "http://", "https://", 1) + "/collect"}, "", http.StatusBadGateway},
			{"userinfo", []string{strings.Replace(base, "http://", "http://user@", 1) + "/collect"}, "", http.StatusBadGateway},
			{"backslash authority", []string{`\\untrusted.invalid\collect`}, "", http.StatusBadGateway},
			{"double slash path", []string{base + "//untrusted.invalid/collect"}, "", http.StatusBadGateway},
			{"encoded double slash path", []string{"/%2funtrusted.invalid/collect"}, "", http.StatusBadGateway},
			{"non HTTP scheme", []string{"javascript:alert(1)"}, "", http.StatusBadGateway},
			{"mixed locations", []string{"/search-redirect", "https://untrusted.invalid/collect"}, "", http.StatusBadGateway},
		} {
			t.Run(tc.name, func(t *testing.T) {
				upstream.setResponse(http.StatusFound, `{"error":{"message":"redirect"}}`)
				upstream.setResponseHeaders(http.Header{"Location": tc.locations})
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
				assert.Equal(t, tc.status, response.Code, response.Body.String())
				assert.Equal(t, tc.want, response.Header().Get("Location"))
				assert.NotContains(t, response.Header().Get("Location"), "fixture-redirect-secret")
				require.Eventually(t, func() bool {
					user, token := zhipuSearchBalances(t, database, 1)
					return user == beforeUser && token == beforeToken
				}, 5*time.Second, 10*time.Millisecond)
			})
		}
	})
}

var zhipuRateLimitTestUser atomic.Int64

func TestZhipuSearchMemorySuccessLimit(t *testing.T) {
	// 限流状态跨请求及测试路由实例共享，为每轮测试分配独立用户。
	engine, database, responses := setupZhipuSearchContractForUser(t, 100_000+int(zhipuRateLimitTestUser.Add(1)))
	previousEnabled, previousTotal, previousSuccess := setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount
	previousDuration := setting.ModelRequestRateLimitDurationMinutes
	setting.ModelRequestRateLimitEnabled = true
	setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount, setting.ModelRequestRateLimitDurationMinutes = 0, 1, 1
	t.Cleanup(func() {
		setting.ModelRequestRateLimitEnabled = previousEnabled
		setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount = previousTotal, previousSuccess
		setting.ModelRequestRateLimitDurationMinutes = previousDuration
	})
	ready, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			close(ready)
			<-release
			_, _ = io.WriteString(w, `{"error":{"message":"rejected"}}`)
			return
		}
		responses.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); upstream.Close() })
	require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("base_url", upstream.URL).Error)
	const payload = `{"search_engine":"search_std","search_query":"test"}`
	beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken") }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("上游未收到首个请求")
	}
	blocked := zhipuSearchRequest(engine, "/api/paas/v4/web_search", payload, "fixturetoken")
	assert.Equal(t, http.StatusTooManyRequests, blocked.Code)
	assert.EqualValues(t, 1, calls.Load())
	releaseOnce.Do(func() { close(release) })
	select {
	case first := <-firstDone:
		require.Equal(t, http.StatusOK, first.Code)
		require.True(t, gjson.Get(first.Body.String(), "error").Exists())
	case <-time.After(5 * time.Second):
		t.Fatal("首个失败请求未结束")
	}
	require.Eventually(t, func() bool {
		user, token := zhipuSearchBalances(t, database, 1)
		return user == beforeUser && token == beforeToken
	}, 5*time.Second, 10*time.Millisecond)
	for _, tc := range []struct {
		name     string
		status   int
		location string
		want     int
	}{
		{"upstream rejection", http.StatusBadRequest, "", http.StatusBadRequest},
		{"safe redirect", http.StatusTemporaryRedirect, "/search-redirect", http.StatusTemporaryRedirect},
		{"unsafe redirect", http.StatusFound, "https://untrusted.invalid/collect", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses.setResponse(tc.status, `{"error":{"message":"rejected"}}`)
			responses.setResponseHeaders(http.Header{"Location": {tc.location}})
			response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
			require.Equal(t, tc.want, response.Code, response.Body.String())
			require.Eventually(t, func() bool {
				user, token := zhipuSearchBalances(t, database, 1)
				return user == beforeUser && token == beforeToken
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
	responses.setResponseHeaders(nil)
	responses.setResponse(http.StatusOK, `{"error":false,"search_result":[{"title":"kept"}]}`)
	succeeded := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
	require.Equal(t, http.StatusOK, succeeded.Code, succeeded.Body.String())
	assert.EqualValues(t, 5, calls.Load())
	blocked = zhipuSearchRequest(engine, "/api/paas/v4/web_search", payload, "fixturetoken")
	assert.Equal(t, http.StatusTooManyRequests, blocked.Code)
	assert.EqualValues(t, 5, calls.Load())
	setting.ModelRequestRateLimitSuccessCount = 0
	for range 2 {
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}
	assert.EqualValues(t, 7, calls.Load())
	setting.ModelRequestRateLimitCount = 1
	responses.setResponse(http.StatusBadRequest, `{"error":{"message":"rejected"}}`)
	response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	responses.setResponse(http.StatusOK, `{"search_result":[]}`)
	blocked = zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
	assert.Equal(t, http.StatusTooManyRequests, blocked.Code)
	assert.EqualValues(t, 8, calls.Load())
}

func TestZhipuSearchBusinessErrorsDoNotCountAsSuccess(t *testing.T) {
	t.Run("channel affinity", func(t *testing.T) {
		engine, _, upstream := setupZhipuSearchContract(t)
		affinity := operation_setting.GetChannelAffinitySetting()
		previous := *affinity
		affinity.Enabled = true
		affinity.Rules = []operation_setting.ChannelAffinityRule{{
			Name: "zhipu-business-error", ModelRegex: []string{"^search_std$"}, PathRegex: []string{"^/v1/web_search$"},
			KeySources:      []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Affinity-Test"}},
			IncludeRuleName: true, IncludeModelName: true, IncludeUsingGroup: true,
		}}
		t.Cleanup(func() {
			_, _ = service.ClearChannelAffinityCacheByRuleName("zhipu-business-error")
			*affinity = previous
		})
		request := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(`{"search_engine":"search_std","search_query":"test"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-fixturetoken")
			req.Header.Set("X-Affinity-Test", "search-failure")
			result := httptest.NewRecorder()
			engine.ServeHTTP(result, req)
			return result
		}
		probe := func() (int, bool) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/web_search", nil)
			ctx.Request.Header.Set("X-Affinity-Test", "search-failure")
			return service.GetPreferredChannelByAffinity(ctx, "search_std", "default")
		}
		upstream.setResponse(http.StatusOK, `{"error":{"message":"rejected"}}`)
		response := request()
		require.Equal(t, http.StatusOK, response.Code)
		_, found := probe()
		assert.False(t, found)
		upstream.setResponse(http.StatusOK, `{"error":"","search_result":[]}`)
		response = request()
		require.Equal(t, http.StatusOK, response.Code)
		channel, found := probe()
		assert.True(t, found)
		assert.Equal(t, 1, channel)
	})

	t.Run("redis success limit", func(t *testing.T) {
		engine, database, upstream := setupZhipuSearchContract(t)
		server := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: server.Addr()})
		previousRedis, previousClient := common.RedisEnabled, common.RDB
		previousEnabled, previousTotal, previousSuccess := setting.ModelRequestRateLimitEnabled, setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount
		affinity := operation_setting.GetChannelAffinitySetting()
		previousAffinity := affinity.Enabled
		common.RedisEnabled, common.RDB = true, client
		setting.ModelRequestRateLimitEnabled = true
		setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount = 0, 1
		affinity.Enabled = false
		t.Cleanup(func() {
			common.RedisEnabled, common.RDB = previousRedis, previousClient
			setting.ModelRequestRateLimitEnabled = previousEnabled
			setting.ModelRequestRateLimitCount, setting.ModelRequestRateLimitSuccessCount = previousTotal, previousSuccess
			affinity.Enabled = previousAffinity
			_ = client.Close()
		})
		const payload = `{"search_engine":"search_std","search_query":"test"}`
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		upstream.setResponse(http.StatusOK, `{"error":{"message":"rejected"}}`)
		response := zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code)
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
		count, err := client.LLen(context.Background(), "rateLimit:MRRLS:1").Result()
		require.NoError(t, err)
		assert.Zero(t, count)
		upstream.setResponse(http.StatusOK, `{"error":false,"search_result":[]}`)
		response = zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code)
		count, err = client.LLen(context.Background(), "rateLimit:MRRLS:1").Result()
		require.NoError(t, err)
		assert.EqualValues(t, 1, count)
		response = zhipuSearchRequest(engine, "/v1/web_search", payload, "fixturetoken")
		assert.Equal(t, http.StatusTooManyRequests, response.Code)
	})
}

func TestZhipuSearchErrorResponses(t *testing.T) {
	engine, database, upstream := setupZhipuSearchContract(t)
	gateway := httptest.NewServer(engine)
	t.Cleanup(gateway.Close)
	client := gateway.Client()
	client.Timeout = 10 * time.Second

	for _, tc := range []struct {
		name         string
		path         string
		channelType  int
		upstreamPath string
		status       int
		contentType  string
		body         string
		wantBody     string
		compressed   bool
	}{
		{"plain error", "/v1/web_search", constant.ChannelTypeZhipuWebSearch, "/api/paas/v4/web_search", http.StatusTooManyRequests, "text/plain; charset=utf-8", "rate limit exceeded\n", "rate limit exceeded\n", false},
		{"inch mark in text", "/v1/web_search", constant.ChannelTypeZhipuWebSearch, "/api/paas/v4/web_search", http.StatusBadRequest, "text/plain", `The 24" display is unavailable; retry tomorrow.`, `The 24" display is unavailable; retry tomorrow.`, false},
		{"diagnostic path and credential", "/v1/web_search", constant.ChannelTypeZhipuWebSearch, "/api/paas/v4/web_search", http.StatusBadRequest, "text/plain", `The 24" display at C:\temp\wide.icc is unavailable. Authorization: Bearer upstream-placeholder; retry later.`, `The 24" display at C:\temp\wide.icc is unavailable. Authorization: ***; retry later.`, false},
		{"inch mark in JSON", "/v1/web_search", constant.ChannelTypeNewAPI, "/v1/web_search", http.StatusBadRequest, "application/json", `{"error":{"message":"The 24\" display is unavailable; retry tomorrow."}}`, `{"error":{"message":"The 24\" display is unavailable; retry tomorrow."}}`, false},
		{"Windows path in JSON message", "/v1/web_search", constant.ChannelTypeNewAPI, "/v1/web_search", http.StatusBadRequest, "application/json", `{"error":{"message":"Could not read \"C:\\Profiles\\wide.icc\". Retry after installing it."}}`, `{"error":{"message":"Could not read \"C:\\Profiles\\wide.icc\". Retry after installing it."}}`, false},
		{"parser diagnostic with a quote", "/v1/web_search", constant.ChannelTypeZhipuWebSearch, "/api/paas/v4/web_search", http.StatusBadRequest, "text/plain", `Unexpected delimiter: " at column 18. Expected a number.`, `Unexpected delimiter: " at column 18. Expected a number.`, false},
		{"quoted path as plain body", "/v1/web_search", constant.ChannelTypeZhipuWebSearch, "/api/paas/v4/web_search", http.StatusBadRequest, "text/plain; charset=utf-8", `"C:\Profiles\wide.icc"`, `"C:\Profiles\wide.icc"`, false},
		{"quoted path as JSON field", "/v1/web_search", constant.ChannelTypeZhipuWebSearch, "/api/paas/v4/web_search", http.StatusBadRequest, "application/json", `{"error":{"message":"\"C:\\Profiles\\wide.icc\"" }}`, `{"error":{"message":"\"C:\\Profiles\\wide.icc\"" }}`, false},
		{"plain error with credential", "/api/paas/v4/web_search", constant.ChannelTypeNewAPI, "/v1/web_search", http.StatusUnauthorized, "text/plain; charset=utf-8", "upstream-placeholder rejected", "*** rejected", true},
		{"JSON error without content type", "/v1/web_search", constant.ChannelTypeNewAPI, "/v1/web_search", http.StatusTooManyRequests, "", `{"error":{"code":"rate_limit","message":"limited"}}`, `{"error":{"code":"rate_limit","message":"limited"}}`, false},
		{"success", "/api/paas/v4/web_search", constant.ChannelTypeZhipuWebSearch, "/api/paas/v4/web_search", http.StatusOK, "application/json", `{"id":"result","search_result":[]}`, `{"id":"result","search_result":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("type", tc.channelType).Error)
			upstream.setResponse(tc.status, tc.body)
			upstream.mu.Lock()
			upstream.compressed = tc.compressed
			upstream.mu.Unlock()
			t.Cleanup(func() {
				upstream.mu.Lock()
				upstream.compressed = false
				upstream.mu.Unlock()
			})
			contentTypes := []string{tc.contentType}
			if tc.contentType == "" {
				contentTypes = nil
			}
			retryAfter := "17"
			if tc.name == "plain error with credential" {
				retryAfter = "Sun, 06 Nov 1994 08:49:37 GMT"
			}
			responseHeaders := http.Header{
				"Content-Type":                 contentTypes,
				"Retry-After":                  {retryAfter},
				"X-Request-Id":                 {"upstream-request"},
				"X-Result-Info":                {"first", "second"},
				"X-Ratelimit-Remaining-Tokens": {"42"},
				"Idempotency-Key":              {"fixture-id"},
				"X-Debug":                      {"rejected upstream-placeholder"},
				"X-Oneapi-Request-Id":          {"upstream-gateway-request"},
				"Set-Cookie":                   {"sessionid=fixture-secret"},
				"X-Api-Key":                    {"fixture-secret"},
				"Connection":                   {"X-Hop-Header"},
				"X-Hop-Header":                 {"hop-only"},
				"Access-Control-Allow-Origin":  {"https://upstream.example"},
			}
			if tc.contentType == "" {
				responseHeaders["Transfer-Encoding"] = []string{"chunked"}
			}
			upstream.setResponseHeaders(responseHeaders)
			beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
			_, beforeBodies := upstream.snapshot()
			req, err := http.NewRequest(http.MethodPost, gateway.URL+tc.path, strings.NewReader(`{"search_engine":"search_std","search_query":"test"}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sk-fixturetoken")
			req.Header.Set("Origin", "https://client.example")
			if tc.compressed {
				req.Header.Set("Accept-Encoding", "gzip")
			}
			response, err := client.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Empty(t, response.Header.Get("Content-Encoding"))
			assert.Equal(t, tc.status, response.StatusCode)
			assert.Equal(t, tc.wantBody, string(body))
			assert.Equal(t, tc.contentType, response.Header.Get("Content-Type"))
			if tc.contentType == "" {
				assert.NotContains(t, response.Header, "Content-Type")
				assert.NotContains(t, response.Header, "Content-Length")
			}
			assert.Equal(t, retryAfter, response.Header.Get("Retry-After"))
			assert.Empty(t, response.Header.Get("X-Request-Id"))
			assert.Empty(t, response.Header.Values("X-Result-Info"))
			assert.Empty(t, response.Header.Get("X-Ratelimit-Remaining-Tokens"))
			assert.Empty(t, response.Header.Get("Idempotency-Key"))
			assert.Empty(t, response.Header.Get("X-Debug"))
			assert.NotEqual(t, "upstream-gateway-request", response.Header.Get(common.RequestIdKey))
			assert.Empty(t, response.Header.Values("Set-Cookie"))
			assert.Empty(t, response.Header.Get("X-Api-Key"))
			assert.Empty(t, response.Header.Get("X-Hop-Header"))
			assert.Equal(t, "*", response.Header.Get("Access-Control-Allow-Origin"))
			assert.Contains(t, strings.Split(response.Header.Get("Access-Control-Expose-Headers"), ","), "Retry-After")
			afterPaths, afterBodies := upstream.snapshot()
			require.Len(t, afterBodies, len(beforeBodies)+1)
			assert.Equal(t, tc.upstreamPath, afterPaths[len(afterPaths)-1])
			charge := 0
			if tc.status == http.StatusOK {
				charge = 685
			}
			require.Eventually(t, func() bool {
				user, token := zhipuSearchBalances(t, database, 1)
				return beforeUser-user == charge && beforeToken-token == charge
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
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
		for _, failure := range []string{"request URL", "request URL without scheme", "request URL without host", "header override", "invalid header name", "invalid header value", "HTTP client", "sent request"} {
			t.Run(fmt.Sprintf("cache_%t/%s", cacheEnabled, failure), func(t *testing.T) {
				engine, database, upstream := setupZhipuSearchContract(t)
				common.RetryTimes = 1
				var backup model.Channel
				require.NoError(t, database.First(&backup, 1).Error)
				primary := model.Channel{Id: 2, Name: "primary-search", Type: constant.ChannelTypeZhipuWebSearch, Status: common.ChannelStatusEnabled, Key: "primary-placeholder", BaseURL: backup.BaseURL, Models: "search_std", Group: "default", Priority: common.GetPointer(int64(100)), AutoBan: common.GetPointer(0)}
				switch failure {
				case "request URL":
					primary.BaseURL = common.GetPointer("http://[")
				case "request URL without scheme":
					primary.BaseURL = common.GetPointer("open.bigmodel.cn")
				case "request URL without host":
					primary.BaseURL = common.GetPointer("https://")
				case "header override":
					primary.HeaderOverride = common.GetPointer(`{"re:[":"*"}`)
				case "invalid header name":
					primary.HeaderOverride = common.GetPointer(`{"Invalid Header":"value"}`)
				case "invalid header value":
					primary.HeaderOverride = common.GetPointer(`{"X-Test":"invalid\nvalue"}`)
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

func TestZhipuSearchResponseBodyLimit(t *testing.T) {
	const limit = 1 << 20
	for _, tc := range []struct {
		name           string
		limitMiB       int
		size           int
		upstreamStatus int
		knownLength    bool
		compressed     bool
		wantStatus     int
		charge         int
	}{
		{"unlimited", 0, limit + 1, http.StatusOK, false, false, http.StatusOK, 685},
		{"exact known", 1, limit, http.StatusOK, true, false, http.StatusOK, 685},
		{"exact unknown", 1, limit, http.StatusOK, false, false, http.StatusOK, 685},
		{"oversized known", 1, limit + 1, http.StatusOK, true, false, http.StatusBadGateway, 0},
		{"oversized unknown", 1, limit + 1, http.StatusOK, false, false, http.StatusBadGateway, 0},
		{"oversized decompressed", 1, limit + 1, http.StatusOK, true, true, http.StatusBadGateway, 0},
		{"larger configured limit", 2, limit + 1, http.StatusOK, false, false, http.StatusOK, 685},
		{"oversized error", 1, limit + 1, http.StatusUnauthorized, false, false, http.StatusBadGateway, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, database, upstream := setupZhipuSearchContract(t)
			setting := fmt.Sprintf(`{"max_response_body_mib":%d}`, tc.limitMiB)
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("setting", setting).Error)
			prefix, suffix := `{"search_result":[],"detail":"`, `"}`
			body := prefix + strings.Repeat("x", tc.size-len(prefix)-len(suffix)) + suffix
			upstream.setResponse(tc.upstreamStatus, body)
			upstream.mu.Lock()
			upstream.knownLength, upstream.compressed = tc.knownLength, tc.compressed
			upstream.mu.Unlock()
			for _, path := range []string{"/v1/web_search", "/api/paas/v4/web_search"} {
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				_, before := upstream.snapshot()
				response := zhipuSearchRequest(engine, path, `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
				require.Equal(t, tc.wantStatus, response.Code, response.Body.String())
				_, after := upstream.snapshot()
				assert.Len(t, after, len(before)+1)
				if tc.wantStatus == http.StatusOK {
					assert.Equal(t, body, response.Body.String())
				} else {
					assert.Contains(t, response.Body.String(), "configured size limit")
					assert.NotContains(t, response.Body.String(), "\"detail\"")
				}
				require.Eventually(t, func() bool {
					afterUser, afterToken := zhipuSearchBalances(t, database, 1)
					return beforeUser-afterUser == tc.charge && beforeToken-afterToken == tc.charge
				}, 5*time.Second, 10*time.Millisecond)
			}
		})
	}
}

func TestZhipuSearchResponseBodyLimitSettings(t *testing.T) {
	_, database, _ := setupZhipuSearchContract(t)
	for _, limit := range []int{8, 0} {
		body := fmt.Sprintf(`{"id":1,"type":62,"setting":"{\"max_response_body_mib\":%d}"}`, limit)
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel", strings.NewReader(body))
		ctx.Set("id", 1)
		ctx.Set("role", common.RoleRootUser)
		controller.UpdateChannel(ctx)
		require.True(t, gjson.Get(response.Body.String(), "success").Bool(), response.Body.String())
		var saved model.Channel
		require.NoError(t, database.First(&saved, 1).Error)
		assert.EqualValues(t, limit, saved.GetSetting().MaxResponseBodyMiB)
		assert.Equal(t, "upstream-placeholder", saved.Key)
		assert.Equal(t, "search_std,search_pro,search_pro_sogou,search_pro_quark", saved.Models)
		assert.Equal(t, "default", saved.Group)
		assert.Equal(t, common.ChannelStatusEnabled, saved.Status)
	}
}

func TestZhipuSearchResponseBodyLimitSubscription(t *testing.T) {
	engine, database, upstream := setupZhipuSearchContract(t)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(10)
	require.NoError(t, database.Create(&model.SubscriptionPlan{Id: 1, Title: "response-limit", TotalAmount: 10_000_000}).Error)
	require.NoError(t, database.Create(&model.UserSubscription{Id: 1, UserId: 1, PlanId: 1, AmountTotal: 10_000_000, StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix(), Status: "active"}).Error)
	require.NoError(t, database.Model(&model.User{}).Where("id = ?", 1).Update("setting", `{"billing_preference":"subscription_only"}`).Error)
	require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("setting", `{"max_response_body_mib":1}`).Error)
	upstream.setResponse(http.StatusOK, `{"detail":"`+strings.Repeat("x", 1<<20)+`"}`)
	beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
	response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
	require.Equal(t, http.StatusBadGateway, response.Code, response.Body.String())
	require.Eventually(t, func() bool {
		var subscription model.UserSubscription
		require.NoError(t, database.First(&subscription, 1).Error)
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		return subscription.AmountUsed == 0 && afterUser == beforeUser && afterToken == beforeToken
	}, 5*time.Second, 10*time.Millisecond)
	paths, _ := upstream.snapshot()
	assert.Len(t, paths, 1)
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
		{"search intent", "/v1/web_search", `{"search_engine":" SEARCH_STD ","search_query":"test","search_intent":true}`, 685},
		{"pro", "/api/paas/v4/web_search", `{"search_engine":"search_pro","search_query":"test","search_intent":false,"count":10}`, 1027},
		{"sogou", "/v1/web_search", `{"search_engine":"search_pro_sogou","search_query":"test","search_intent":false,"count":20}`, 3425},
		{"quark", "/v1/web_search", `{"search_engine":"search_pro_quark","search_query":"test","search_intent":false,"search_recency_filter":"noLimit","content_size":"medium"}`, 3425},
		{"sogou nonstandard count", "/v1/web_search", `{"search_engine":"search_pro_sogou","search_query":"test","count":11}`, 3425},
		{"quark count and domain", "/api/paas/v4/web_search", `{"search_engine":"search_pro_quark","search_query":"test","count":1,"search_domain_filter":"www.sohu.com"}`, 3425},
		{"numeric query and string count", "/v1/web_search", `{"search_engine":"search_std","search_query":123,"search_intent":"false","count":"5"}`, 685},
		{"null and unusual parameters", "/v1/web_search", `{"search_engine":"search_std","search_query":true,"count":null,"search_intent":null,"search_recency_filter":"yesterday","content_size":"large","request_id":null,"user_id":null}`, 685},
		{"long query and count", "/v1/web_search", `{"search_engine":"search_std","search_query":"` + strings.Repeat("x", 71) + `","count":51}`, 685},
		{"last engine and query", "/v1/web_search", `{"search_engine":"search_std","search_engine":"search_pro","search_query":"first","search_query":"last","SEARCH_QUERY":"ignored"}`, 1027},
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
			parsed, err := dto.ParseZhipuWebSearchRequest([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, parsed.SearchEngine, log.ModelName)
		})
	}

	t.Run("omitted search intent is forwarded unchanged", func(t *testing.T) {
		for _, path := range []string{"/v1/web_search", "/api/paas/v4/web_search"} {
			request := httptest.NewRequest(http.MethodPost, path+"?trace=one%2Ftwo", strings.NewReader(`{"search_engine":"search_std","search_query":"test"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer sk-fixturetoken")
			request.Header.Set("X-Client-Trace", "fixture-trace")
			request.Header.Set("Idempotency-Key", "fixture-id")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			paths, bodies := upstream.snapshot()
			assert.Equal(t, "/api/paas/v4/web_search?trace=one%2Ftwo", paths[len(paths)-1])
			assert.Equal(t, `{"search_engine":"search_std","search_query":"test"}`, bodies[len(bodies)-1])
			upstream.mu.Lock()
			headers := upstream.requestHeaders[len(upstream.requestHeaders)-1].Clone()
			upstream.mu.Unlock()
			assert.Empty(t, headers.Get("X-Client-Trace"))
			assert.Empty(t, headers.Get("Idempotency-Key"))
			assert.Equal(t, "Bearer upstream-placeholder", headers.Get("Authorization"))
		}
	})

	t.Run("passthrough with token counting enabled", func(t *testing.T) {
		previous := constant.CountToken
		constant.CountToken = true
		t.Cleanup(func() { constant.CountToken = previous })
		body := `{"search_engine":"search_std","search_query":123,"count":"5","search_intent":null}`
		response := zhipuSearchRequest(engine, "/v1/web_search", body, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		_, bodies := upstream.snapshot()
		assert.Equal(t, body, bodies[len(bodies)-1])
	})

	t.Run("invalid requests pass through to upstream", func(t *testing.T) {
		errorBody := `{"error":{"code":"1214","message":"invalid search engine"}}`
		upstream.setResponse(http.StatusBadRequest, errorBody)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[{"title":"result"}],"extra":{"keep":true}}`)
		cases := map[string]string{
			"missing engine":       `{"search_query":"test"}`,
			"unknown engine":       `{"search_engine":"search_unknown","search_query":"test"}`,
			"invalid final engine": `{"search_engine":"search_std","search_engine":"search_unknown","search_query":"test"}`,
			"case variant only":    `{"SEARCH_ENGINE":"search_std","search_query":"test"}`,
			"invalid JSON":         `{"search_engine":`,
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
				_, before := upstream.snapshot()
				response := zhipuSearchRequest(engine, "/v1/web_search", body, "fixturetoken")
				assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
				assert.Equal(t, errorBody, response.Body.String())
				_, after := upstream.snapshot()
				require.Len(t, after, len(before)+1)
				assert.Equal(t, body, after[len(after)-1])
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
		for _, body := range []string{
			`{"search_engine":"search_std","search_query":"test","search_intent":false}`,
			`{"search_engine":"search_std","search_query":123,"search_intent":null,"count":"5","content_size":"large"}`,
		} {
			response := zhipuSearchRequest(engine, "/api/paas/v4/web_search", body, "fixturetoken")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			paths, bodies := upstream.snapshot()
			assert.Equal(t, "/v1/web_search", paths[len(paths)-1])
			assert.Equal(t, body, bodies[len(bodies)-1])
		}
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

	t.Run("upstream validates a search query removed by parameter override", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", `{"operations":[{"mode":"delete","path":"search_query"}]}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", "").Error)
		})
		errorBody := `{"error":{"code":"1214","message":"invalid search query"}}`
		upstream.setResponse(http.StatusBadRequest, errorBody)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		_, before := upstream.snapshot()
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		assert.Equal(t, errorBody, response.Body.String())
		_, after := upstream.snapshot()
		require.Len(t, after, len(before)+1)
		assert.False(t, gjson.Get(after[len(after)-1], "search_query").Exists())
		require.Eventually(t, func() bool {
			user, token := zhipuSearchBalances(t, database, 1)
			return user == beforeUser && token == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
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

	t.Run("case variant channel parameter is forwarded", func(t *testing.T) {
		require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", `{"SEARCH_ENGINE":"search_pro"}`).Error)
		t.Cleanup(func() {
			require.NoError(t, database.Model(&model.Channel{}).Where("id = ?", 1).Update("param_override", "").Error)
		})
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		_, bodies := upstream.snapshot()
		assert.Equal(t, "search_std", gjson.Get(bodies[len(bodies)-1], "search_engine").String())
		assert.Equal(t, "search_pro", gjson.Get(bodies[len(bodies)-1], "SEARCH_ENGINE").String())
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 685, beforeUser-afterUser)
		assert.Equal(t, 685, beforeToken-afterToken)
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

	t.Run("HTTP 200 upstream errors do not expose channel credentials or charge", func(t *testing.T) {
		upstream.setResponse(http.StatusOK, `{"error":{"code":"invalid_key","message":"upstream-placeholder rejected","metadata":{"authorization":"upstream-placeholder","detail":"kept"}}}`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
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

	t.Run("truncated upstream response returns an error and refunds", func(t *testing.T) {
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
		require.Eventually(t, func() bool {
			afterUser, afterToken := zhipuSearchBalances(t, database, 1)
			return afterUser == beforeUser && afterToken == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("complete invalid JSON is forwarded after charging", func(t *testing.T) {
		upstream.setResponse(http.StatusOK, `{"search_result":[`)
		defer upstream.setResponse(http.StatusOK, `{"id":"upstream-id","search_result":[]}`)
		beforeUser, beforeToken := zhipuSearchBalances(t, database, 1)
		response := zhipuSearchRequest(engine, "/v1/web_search", `{"search_engine":"search_std","search_query":"test"}`, "fixturetoken")
		assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, `{"search_result":[`, response.Body.String())
		afterUser, afterToken := zhipuSearchBalances(t, database, 1)
		assert.Equal(t, 685, beforeUser-afterUser)
		assert.Equal(t, 685, beforeToken-afterToken)
	})

	t.Run("client disconnect closes the upstream body and refunds", func(t *testing.T) {
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
		require.Eventually(t, func() bool {
			afterUser, afterToken := zhipuSearchBalances(t, database, 1)
			return afterUser == beforeUser && afterToken == beforeToken
		}, 5*time.Second, 10*time.Millisecond)
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
