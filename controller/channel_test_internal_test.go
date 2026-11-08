package controller

import (
	"bytes"
	"context"
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
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel/zhipu_web_search"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZhipuWebSearchChannelHealth(t *testing.T) {
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousCache := common.RedisEnabled, common.MemoryCacheEnabled
	previousLogging, previousQuota := common.LogConsumeEnabled, common.QuotaPerUnit
	previousPrice, previousRate := ratio_setting.ModelPrice2JSONString(), operation_setting.USDExchangeRate
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousCache
		common.LogConsumeEnabled, common.QuotaPerUnit = previousLogging, previousQuota
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrice))
		operation_setting.USDExchangeRate = previousRate
		model.InvalidatePricingCache()
	})
	database := setupModelListControllerTestDB(t)
	require.NoError(t, database.AutoMigrate(&model.Log{}, &model.Option{}))
	common.MemoryCacheEnabled, common.LogConsumeEnabled, common.QuotaPerUnit = false, true, 500_000
	operation_setting.USDExchangeRate = 7.3
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{}`))
	require.NoError(t, database.Create(&model.User{Id: 1, Username: "search-health", Status: common.UserStatusEnabled, Group: "default", Quota: 10_000_000}).Error)
	service.InitHttpClient()
	type capturedRequest struct {
		path string
		body []byte
	}
	requests := make(chan capturedRequest, 1)
	var status atomic.Int32
	status.Store(http.StatusOK)
	var responseBody atomic.Value
	responseBody.Store(`{"id":"search-health","search_result":[]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- capturedRequest{path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, responseBody.Load().(string))
	}))
	t.Cleanup(server.Close)
	channel := &model.Channel{Id: 1, Type: constant.ChannelTypeZhipuWebSearch, Status: common.ChannelStatusEnabled, Key: "health-placeholder", BaseURL: &server.URL, Models: "search_std", Group: "default", AutoBan: common.GetPointer(0)}
	require.NoError(t, database.Create(channel).Error)
	for _, tc := range []struct {
		engine string
		quota  int
	}{
		{dto.ZhipuSearchStd, 685},
		{dto.ZhipuSearchPro, 2055},
		{dto.ZhipuSearchProSogou, 3425},
		{dto.ZhipuSearchProQuark, 3425},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			result := testChannel(context.Background(), channel, 1, tc.engine, "", true)
			require.NoError(t, result.localErr)
			assert.Nil(t, result.newAPIError)
			request := <-requests
			assert.Equal(t, "/api/paas/v4/web_search", request.path)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(request.body, &payload))
			assert.Equal(t, tc.engine, payload["search_engine"])
			assert.Equal(t, "人工智能", payload["search_query"])
			assert.NotContains(t, payload, "messages")
			assert.NotContains(t, payload, "stream")
			var logged model.Log
			require.NoError(t, database.Order("id desc").First(&logged).Error)
			assert.Equal(t, tc.quota, logged.Quota)
		})
	}
	require.Equal(t, channelTestSummary{Tested: 1, Succeeded: 1}, testChannelForHealthCheck(context.Background(), channel, 1, true, 0))
	<-requests
	status.Store(http.StatusUnauthorized)
	require.Equal(t, channelTestSummary{Tested: 1, Failed: 1}, testChannelForHealthCheck(context.Background(), channel, 1, false, 0))
	<-requests
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"oversized success", http.StatusOK, `{"detail":"` + strings.Repeat("x", 1<<20) + `"}`},
		{"oversized error", http.StatusUnauthorized, `{"detail":"` + strings.Repeat("x", 1<<20) + `"}`},
		{"json error", http.StatusUnauthorized, `{"error":{"message":"rejected health-placeholder"},"api_key":"health-placeholder"}`},
		{"non-json error", http.StatusBadGateway, `upstream rejected health-placeholder`},
		{"error in successful HTTP response", http.StatusOK, `{"error":{"message":"rejected health-placeholder"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel.Setting = common.GetPointer(`{"max_response_body_mib":1}`)
			status.Store(int32(tc.status))
			responseBody.Store(tc.body)
			result := testChannel(context.Background(), channel, 1, dto.ZhipuSearchStd, "", false)
			require.Error(t, result.localErr)
			assert.NotContains(t, result.localErr.Error(), "health-placeholder")
			require.NotNil(t, result.newAPIError)
			assert.NotContains(t, result.newAPIError.Error(), "health-placeholder")
			assert.NotContains(t, result.newAPIError.LogErrorWithStatusCode(), "health-placeholder")
			<-requests
		})
	}
}

func TestZhipuResponseBodyLimitReader(t *testing.T) {
	const limit = 1 << 20
	for _, tc := range []struct {
		name        string
		channelType int
		limitMiB    int64
		size        int
		length      int64
		wantRead    int
		tooLarge    bool
	}{
		{"known exact limit", constant.ChannelTypeZhipuWebSearch, 1, limit, limit, limit, false},
		{"known oversized", constant.ChannelTypeZhipuWebSearch, 1, limit + 1, limit + 1, 0, true},
		{"unknown exact limit", constant.ChannelTypeZhipuWebSearch, 1, limit, -1, limit, false},
		{"unknown oversized", constant.ChannelTypeZhipuWebSearch, 1, limit + 10, -1, limit + 1, true},
		{"unlimited", constant.ChannelTypeZhipuWebSearch, 0, limit + 10, -1, limit + 10, false},
		{"larger configured limit", constant.ChannelTypeZhipuWebSearch, 2, limit + 10, -1, limit + 10, false},
		{"New API is unaffected", constant.ChannelTypeNewAPI, 1, limit + 10, -1, limit + 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := strings.Repeat("x", tc.size)
			var observed bytes.Buffer
			resp := &http.Response{ContentLength: tc.length, Body: io.NopCloser(io.TeeReader(strings.NewReader(payload), &observed))}
			defer resp.Body.Close()
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: tc.channelType, ChannelSetting: dto.ChannelSettings{MaxResponseBodyMiB: tc.limitMiB}}}
			body, err := zhipu_web_search.ReadResponseBody(resp, info)
			if tc.tooLarge {
				require.ErrorIs(t, err, zhipu_web_search.ErrResponseBodyTooLarge)
				assert.Nil(t, body)
			} else {
				require.NoError(t, err)
				assert.Equal(t, payload, string(body))
			}
			assert.Equal(t, tc.wantRead, observed.Len())
		})
	}
}

func TestValidateChannelResponseBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		setting string
		valid   bool
	}{
		{`{"max_response_body_mib":0}`, true},
		{`{"max_response_body_mib":8}`, true},
		{`{"max_response_body_mib":-1}`, false},
		{`{"max_response_body_mib":1.5}`, false},
		{`{"max_response_body_mib":"8"}`, false},
		{`{"max_response_body_mib":9223372036854775807}`, false},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			channel := &model.Channel{Type: constant.ChannelTypeZhipuWebSearch, Setting: &tc.setting}
			assert.Equal(t, tc.valid, validateChannel(channel, false) == nil)
		})
	}
}

func TestZhipuWebSearchAdaptorIsolation(t *testing.T) {
	searchAPIType, ok := common.ChannelType2APIType(constant.ChannelTypeZhipuWebSearch)
	require.True(t, ok)
	searchAdaptor := relay.GetAdaptor(searchAPIType)
	searchInfo := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeZhipuWebSearch, ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeZhipuWebSearch}}
	url, err := searchAdaptor.GetRequestURL(searchInfo)
	require.NoError(t, err)
	assert.Equal(t, "https://open.bigmodel.cn/api/paas/v4/web_search", url)
	searchInfo.RelayMode = relayconstant.RelayModeChatCompletions
	_, err = searchAdaptor.GetRequestURL(searchInfo)
	assert.Error(t, err)

	codingAPIType, ok := common.ChannelType2APIType(constant.ChannelTypeZhipu_v4)
	require.True(t, ok)
	codingAdaptor := relay.GetAdaptor(codingAPIType)
	for _, plan := range []string{"glm-coding-plan", "glm-coding-plan-international"} {
		info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeZhipu_v4, ChannelBaseUrl: plan}}
		url, err := codingAdaptor.GetRequestURL(info)
		require.NoError(t, err)
		assert.Equal(t, constant.ChannelSpecialBases[plan].OpenAIBaseURL+"/chat/completions", url)
		assert.NotContains(t, codingAdaptor.GetModelList(), dto.ZhipuSearchStd)
	}
}

func TestValidateChannelProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   string
		wantErr bool
	}{
		{name: "empty"},
		{name: "http", proxy: "http://proxy.example:8080"},
		{name: "https", proxy: "https://proxy.example:8443"},
		{name: "socks5", proxy: "socks5://proxy.example"},
		{name: "socks5h", proxy: "socks5h://proxy.example:1080/"},
		{name: "unsupported", proxy: "ftp://proxy.example", wantErr: true},
		{name: "path", proxy: "socks5://proxy.example:1080/path", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setting, err := common.Marshal(dto.ChannelSettings{Proxy: test.proxy})
			require.NoError(t, err)
			channel := &model.Channel{
				Type:    constant.ChannelTypeOpenAI,
				Setting: common.GetPointer(string(setting)),
			}

			err = validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "invalid channel proxy")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateChannelRequiresNewAPIBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL *string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "blank", baseURL: common.GetPointer("  "), wantErr: true},
		{name: "configured", baseURL: common.GetPointer("https://new-api.example")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:    constant.ChannelTypeNewAPI,
				BaseURL: test.baseURL,
			}

			err := validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "New API channel base URL cannot be empty")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewAPIChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeNewAPI)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeNewAPI, apiType)
	assert.Equal(t, "New API", constant.GetChannelTypeName(constant.ChannelTypeNewAPI))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeNewAPI)
	assert.Empty(t, constant.ChannelBaseURLs[constant.ChannelTypeNewAPI])
}

func TestResponsesCompactChannelSupport(t *testing.T) {
	tests := []struct {
		name        string
		channelType int
		apiType     int
		want        bool
	}{
		{name: "OpenAI", channelType: constant.ChannelTypeOpenAI, apiType: constant.APITypeOpenAI, want: true},
		{name: "Azure", channelType: constant.ChannelTypeAzure, apiType: constant.APITypeOpenAI, want: true},
		{name: "Codex", channelType: constant.ChannelTypeCodex, apiType: constant.APITypeCodex, want: true},
		{name: "Advanced Custom", channelType: constant.ChannelTypeAdvancedCustom, apiType: constant.APITypeAdvancedCustom, want: true},
		{name: "Sub2API", channelType: constant.ChannelTypeSub2API, apiType: constant.APITypeSub2API, want: true},
		{name: "New API", channelType: constant.ChannelTypeNewAPI, apiType: constant.APITypeNewAPI, want: true},
		{name: "Anthropic", channelType: constant.ChannelTypeAnthropic, apiType: constant.APITypeAnthropic, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, common.SupportsResponsesCompact(test.channelType, test.apiType))
		})
	}
}

func TestMultiprotocolGatewayEndpointTypes(t *testing.T) {
	want := []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeOpenAIResponseCompact,
		constant.EndpointTypeAnthropic,
		constant.EndpointTypeGemini,
		constant.EndpointTypeOpenAIAlphaSearch,
	}

	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeNewAPI, "gpt-5"))
	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeSub2API, "gpt-5"))
}

func TestCopyChannelRejectsInvalidLegacyProxySettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	settingBytes, err := common.Marshal(dto.ChannelSettings{
		Proxy: "socks5://proxy.example/legacy-path",
	})
	require.NoError(t, err)
	setting := string(settingBytes)
	origin := &model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "legacy proxy channel",
		Key:     "test-key",
		Models:  "gpt-test",
		Group:   "default",
		Setting: &setting,
	}
	require.NoError(t, db.Create(origin).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", origin.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)

	CopyChannel(ctx)

	assert.Contains(t, recorder.Body.String(), "invalid channel settings")
	var channelCount int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCount).Error)
	assert.Equal(t, int64(1), channelCount)
}

func TestDeleteChannelResetsProxyCacheWhenPreReadFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	service.ResetProxyClientCache()
	t.Cleanup(service.ResetProxyClientCache)

	proxyURL := "http://proxy.example:8080"
	beforeDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/999999", nil)

	DeleteChannel(ctx)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	afterDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)
	assert.NotSame(t, beforeDelete, afterDelete)
}

func TestDeleteChannelBatchReportsAndAuditsActualDeletedCount(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	channel := &model.Channel{Name: "existing", Key: "test-key"}
	require.NoError(t, db.Create(channel).Error)

	requestBody, err := common.Marshal(ChannelBatch{Ids: []int{channel.Id, 999999}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/batch", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")

	DeleteChannelBatch(ctx)

	var response struct {
		Success bool  `json:"success"`
		Data    int64 `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, int64(1), response.Data)

	var auditLog model.AuditLog
	require.NoError(t, db.Order("id desc").First(&auditLog).Error)
	var auditData struct {
		Operation struct {
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	encodedAudit, err := common.Marshal(auditLog.Other)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(encodedAudit, &auditData))
	assert.Equal(t, float64(1), auditData.Operation.Params["count"])
}

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	requestRules := []billingexpr.RequestRuleTrace{{
		Cond:       `param("service_tier") == "fast"`,
		Multiplier: 2,
		Matched:    true,
	}}
	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier:  "base",
		RequestRules: requestRules,
	})

	fields := other.Snapshot()
	require.Equal(t, "tiered_expr", fields["billing_mode"])
	require.Equal(t, "base", fields["matched_tier"])
	require.Equal(t, requestRules, fields["request_rules"])
	require.NotEmpty(t, fields["expr_b64"])
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestScheduledSkipsManualDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 2, selected[1].Id)
}

func TestSelectChannelsForAutomaticTestAutoBanOnlyUsesEligibleChannels(t *testing.T) {
	autoBanEnabled := 1
	autoBanDisabled := 0
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled, AutoBan: &autoBanEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled, AutoBan: &autoBanDisabled},
		{Id: 3, Status: common.ChannelStatusAutoDisabled, AutoBan: &autoBanEnabled},
		{Id: 4, Status: common.ChannelStatusManuallyDisabled, AutoBan: &autoBanEnabled},
		{Id: 5, Status: common.ChannelStatusEnabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeAutoBanOnly)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 3, selected[1].Id)
}

func TestRunChannelTestWorkersHonorsConfiguredConcurrency(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	release := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32
	progress := make([]int, 0, len(channels)+1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			context.Background(),
			channels,
			2,
			func(_ context.Context, _ *model.Channel) channelTestSummary {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					observed := maxActive.Load()
					if current <= observed || maxActive.CompareAndSwap(observed, current) {
						break
					}
				}
				started <- struct{}{}
				<-release
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	select {
	case <-started:
		t.Fatal("started more channel tests than the configured concurrency")
	default:
	}
	close(release)

	summary := <-summaryResult

	assert.Equal(t, int32(2), maxActive.Load())
	assert.Equal(t, channelTestSummary{Tested: 4, Succeeded: 4}, summary)
	assert.Equal(t, []int{0, 1, 2, 3, 4}, progress)
}

func TestRunChannelTestWorkersStopsAfterCancellation(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	ctx, cancel := context.WithCancel(context.Background())
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	progress := make([]int, 0, 1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			ctx,
			channels,
			2,
			func(ctx context.Context, _ *model.Channel) channelTestSummary {
				started <- struct{}{}
				<-ctx.Done()
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	cancel()

	summary := <-summaryResult

	select {
	case <-started:
		t.Fatal("started another channel test after cancellation")
	default:
	}
	assert.Equal(t, channelTestSummary{Tested: 2, Succeeded: 2}, summary)
	assert.Equal(t, []int{0}, progress)
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有通道测试任务正在运行或等待中")
}
