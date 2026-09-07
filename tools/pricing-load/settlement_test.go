package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const settlementInitialQuota = 1000000

// 上游仅控制用量、失败和返回时机，鉴权、转发、预扣、结算、退款和日志均使用生产代码。
type settlementUpstream struct {
	status     int
	prompt     int
	completion int
	entered    chan struct{}
	release    chan struct{}
}

type settlementFixture struct {
	db       *gorm.DB
	server   *httptest.Server
	scenario atomic.Pointer[settlementUpstream]
	channel  model.Channel
}

// 仅使用现有隔离 Compose 的固定端口；非空数据库拒绝接管，所有资源在用例结束时清理。
func newSettlementFixture(t *testing.T, engine string) *settlementFixture {
	t.Helper()
	dsn := "root@tcp(127.0.0.1:13316)/pricing_review?charset=utf8mb4&parseTime=true&timeout=5s"
	redisDB := 8
	if engine == "postgres" {
		dsn = "postgres://postgres@127.0.0.1:15417/pricing_review?sslmode=disable&connect_timeout=5"
		redisDB = 9
	}
	t.Setenv("SQL_DSN", dsn)
	common.IsMasterNode = false
	require.NoError(t, model.InitDB())
	f := &settlementFixture{db: model.DB}
	f.db.Logger = logger.Default.LogMode(logger.Silent)
	model.LOG_DB = f.db
	sqlDB, err := f.db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(32)
	sqlDB.SetMaxIdleConns(32)
	if f.db.Migrator().HasTable(&model.User{}) {
		var count int64
		require.NoError(t, f.db.Model(&model.User{}).Count(&count).Error)
		require.Zero(t, count, "isolated database already in use")
	}
	common.RedisEnabled, common.MemoryCacheEnabled = true, true
	common.BatchUpdateEnabled, common.DataExportEnabled = false, false
	common.LogConsumeEnabled = true
	common.RetryTimes = 0
	common.PreConsumedQuota = 500
	common.SyncFrequency = 60
	constant.StreamingTimeout = 10
	constant.CountToken = false
	common.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:16379", DB: redisDB})
	t.Cleanup(func() { _ = common.RDB.Close() })
	require.NoError(t, common.RDB.Ping(t.Context()).Err())
	require.NoError(t, common.RDB.FlushDB(t.Context()).Err())
	schema := []any{&model.User{}, &model.Token{}, &model.UserModelPricing{}, &model.Channel{}, &model.Ability{}, &model.Log{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}}
	require.NoError(t, f.db.AutoMigrate(schema...))
	t.Cleanup(func() {
		assert.NoError(t, f.db.Migrator().DropTable(schema...))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, common.RDB.FlushDB(ctx).Err())
	})
	require.NoError(t, model.InitializeUserModelPricingKeys())
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"billing-ratio":2,"billing-fixed":2,"billing-tiered":2}`))
	require.NoError(t, ratio_setting.UpdateCompletionRatioByJSONString(`{"billing-ratio":2,"billing-fixed":2,"billing-tiered":2}`))
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"billing-fixed":0.01}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"billing-tiered":"tiered_expr"}`,
		"billing_setting.billing_expr": `{"billing-tiered":"tier(\"base\", p * 6 + c * 10)"}`,
	}))
	service.InitHttpClient()
	service.InitTokenEncoders()
	f.scenario.Store(&settlementUpstream{status: 200, prompt: 1000, completion: 500})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := common.DecodeJson(r.Body, &input); err != nil {
			http.Error(w, "invalid test request", 400)
			return
		}
		scenario := f.scenario.Load()
		if scenario.entered != nil {
			select {
			case scenario.entered <- struct{}{}:
			case <-r.Context().Done():
				return
			}
			select {
			case <-scenario.release:
			case <-r.Context().Done():
				return
			}
		}
		if scenario.status != 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(scenario.status)
			_, _ = io.WriteString(w, `{"error":{"message":"controlled upstream failure","type":"server_error","code":"test_failure"}}`)
			return
		}
		usage := map[string]int{"prompt_tokens": scenario.prompt, "completion_tokens": scenario.completion, "total_tokens": scenario.prompt + scenario.completion}
		if input.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk, _ := common.Marshal(gin.H{"id": "test-stream", "object": "chat.completion.chunk", "model": input.Model, "choices": []any{gin.H{"index": 0, "delta": gin.H{"content": "ok"}, "finish_reason": "stop"}}, "usage": usage})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := common.Marshal(gin.H{"id": "test-completion", "object": "chat.completion", "model": input.Model, "choices": []any{gin.H{"index": 0, "message": gin.H{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}, "usage": usage})
		_, _ = w.Write(body)
	}))
	t.Cleanup(upstream.Close)
	f.channel = model.Channel{Type: constant.ChannelTypeOpenAI, Name: "settlement-loopback", Key: rand.Text(), BaseURL: common.GetPointer(upstream.URL), Models: "billing-ratio,billing-fixed,billing-tiered", Group: "default", Status: common.ChannelStatusEnabled, AutoBan: common.GetPointer(0)}
	require.NoError(t, f.channel.Insert())
	model.InitChannelCache()
	gin.SetMode(gin.ReleaseMode)
	app := gin.New()
	app.Use(gin.Recovery(), middleware.RequestId())
	router.SetRelayRouter(app)
	f.server = httptest.NewServer(app)
	t.Cleanup(f.server.Close)
	return f
}

// 为每种场景创建独立账户和有限额令牌，余额低于信任旁路阈值以确保确实执行预扣。
func (f *settlementFixture) user(t *testing.T, modelName string, discount int) (model.User, model.Token) {
	t.Helper()
	user := model.User{Username: rand.Text()[:16], AffCode: rand.Text()[:16], Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default", Quota: settlementInitialQuota, AuthVersion: 1, ModelPricingVersion: 1}
	user.SetSetting(dto.UserSetting{BillingPreference: "wallet_only"})
	require.NoError(t, f.db.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: rand.Text(), Name: "settlement-test", Status: common.TokenStatusEnabled, RemainQuota: settlementInitialQuota, ExpiredTime: -1}
	require.NoError(t, f.db.Create(&token).Error)
	_, err := model.ReplaceUserModelPricingContext(t.Context(), user.Id, map[string]int{modelName: discount}, 1)
	require.NoError(t, err)
	return user, token
}

// 返回真实路由响应，调用者等待账本而非依赖响应写出时机判断异步退款是否完成。
func (f *settlementFixture) request(ctx context.Context, token model.Token, modelName string, maxTokens int, stream bool) (int, string, error) {
	data, err := common.Marshal(gin.H{"model": modelName, "messages": []any{gin.H{"role": "user", "content": "test"}}, "max_tokens": maxTokens, "stream": stream})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/v1/chat/completions", strings.NewReader(string(data)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-"+token.Key)
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	return response.StatusCode, string(body), err
}

// 对账必须同时满足用户余额、令牌余额、累计用量和成功日志，防止只写日志却没实际扣款。
func (f *settlementFixture) ledger(t *testing.T, user model.User, token model.Token, charged, requests int, expectedLogs ...int) {
	t.Helper()
	walletCharged := charged
	if user.GetSetting().BillingPreference == "subscription_only" {
		walletCharged = 0
	}
	if len(expectedLogs) == 0 && requests > 0 {
		for range requests {
			expectedLogs = append(expectedLogs, charged/requests)
		}
	}
	require.EventuallyWithT(t, func(check *assert.CollectT) {
		var gotUser model.User
		var gotToken model.Token
		var logs []model.Log
		if !assert.NoError(check, f.db.First(&gotUser, user.Id).Error) || !assert.NoError(check, f.db.First(&gotToken, token.Id).Error) || !assert.NoError(check, f.db.Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Find(&logs).Error) {
			return
		}
		assert.Equal(check, user.Quota-walletCharged, gotUser.Quota)
		assert.Equal(check, charged, gotUser.UsedQuota)
		assert.Equal(check, requests, gotUser.RequestCount)
		assert.Equal(check, token.RemainQuota-charged, gotToken.RemainQuota)
		assert.Equal(check, charged, gotToken.UsedQuota)
		assert.Len(check, logs, requests)
		sum := 0
		actualLogs := make([]int, 0, len(logs))
		for _, entry := range logs {
			sum += entry.Quota
			actualLogs = append(actualLogs, entry.Quota)
		}
		assert.Equal(check, charged, sum)
		assert.ElementsMatch(check, expectedLogs, actualLogs)
		// 核对生产读取入口看到的余额，避免数据库已正确但异步缓存仍错误。
		quota, err := model.GetUserQuota(user.Id, false)
		if assert.NoError(check, err) {
			assert.Equal(check, user.Quota-walletCharged, quota)
		}
		cachedToken, err := model.GetTokenByKey(token.Key, false)
		if assert.NoError(check, err) {
			assert.Equal(check, token.RemainQuota-charged, cachedToken.RemainQuota)
		}
	}, 5*time.Second, 10*time.Millisecond)
}

// 外部数据库测试显式开启；两个引擎串行，使用同一套真实 HTTP 和账本断言。
func TestFinalBillingHTTP(t *testing.T) {
	if os.Getenv("PRICING_EXTERNAL_TESTS") != "1" {
		t.Skip("requires isolated Compose and PRICING_EXTERNAL_TESTS=1")
	}
	for _, engine := range []string{"mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			f := newSettlementFixture(t, engine)
			for _, test := range []struct {
				name      string
				maxTokens int
				stream    bool
				model     string
				discount  int
				expected  int
			}{
				{"top_up", 100, false, "billing-ratio", 8000, 3200},
				{"partial_refund", 5000, false, "billing-ratio", 8000, 3200},
				{"exact_reservation", 1500, false, "billing-ratio", 8000, 3200},
				{"stream", 100, true, "billing-ratio", 8000, 3200},
				{"full_price", 100, false, "billing-ratio", 10000, 4000},
				{"fixed_price", 100, false, "billing-fixed", 8000, 4000},
				{"tiered", 100, false, "billing-tiered", 8000, 4400},
				{"minimum_discount", 100, false, "billing-ratio", 1, 1},
			} {
				t.Run(test.name, func(t *testing.T) {
					user, token := f.user(t, test.model, test.discount)
					status, body, err := f.request(t.Context(), token, test.model, test.maxTokens, test.stream)
					require.NoError(t, err)
					require.Equal(t, 200, status, body)
					f.ledger(t, user, token, test.expected, 1)
				})
			}
			// 失败响应必须退还预扣，且不产生成功消费日志。
			t.Run("upstream_failure_refund", func(t *testing.T) {
				f.scenario.Store(&settlementUpstream{status: 503})
				defer f.scenario.Store(&settlementUpstream{status: 200, prompt: 1000, completion: 500})
				user, token := f.user(t, "billing-ratio", 8000)
				status, body, err := f.request(t.Context(), token, "billing-ratio", 100, false)
				require.NoError(t, err)
				require.Equal(t, 503, status, body)
				f.ledger(t, user, token, 0, 0)
			})
			// 钱包或令牌余额不足时不得留下部分预扣或成功消费记录。
			for _, source := range []string{"wallet", "token"} {
				t.Run("insufficient_"+source, func(t *testing.T) {
					user, token := f.user(t, "billing-ratio", 8000)
					if source == "wallet" {
						user.Quota = 500
						require.NoError(t, f.db.Model(&user).Update("quota", user.Quota).Error)
					} else {
						token.RemainQuota = 500
						require.NoError(t, f.db.Model(&token).Update("remain_quota", token.RemainQuota).Error)
					}
					status, body, err := f.request(t.Context(), token, "billing-ratio", 100, false)
					require.NoError(t, err)
					require.Equal(t, 403, status, body)
					f.ledger(t, user, token, 0, 0)
				})
			}
			// 订阅只扣订阅额度和令牌额度；钱包不受影响，失败则两者都恢复。
			for _, test := range []struct {
				name                      string
				maxTokens, status, charge int
			}{
				{"subscription_top_up", 100, 200, 3200},
				{"subscription_partial_refund", 5000, 200, 3200},
				{"subscription_failure_refund", 100, 503, 0},
			} {
				t.Run(test.name, func(t *testing.T) {
					user, token := f.user(t, "billing-ratio", 8000)
					user.SetSetting(dto.UserSetting{BillingPreference: "subscription_only"})
					require.NoError(t, f.db.Model(&user).Update("setting", user.Setting).Error)
					plan := model.SubscriptionPlan{Title: "billing-test", Enabled: true, TotalAmount: settlementInitialQuota, QuotaResetPeriod: "never"}
					require.NoError(t, f.db.Create(&plan).Error)
					sub := model.UserSubscription{UserId: user.Id, PlanId: plan.Id, AmountTotal: settlementInitialQuota, Status: "active", StartTime: time.Now().Add(-time.Hour).Unix(), EndTime: time.Now().Add(time.Hour).Unix()}
					require.NoError(t, f.db.Create(&sub).Error)
					f.scenario.Store(&settlementUpstream{status: test.status, prompt: 1000, completion: 500})
					defer f.scenario.Store(&settlementUpstream{status: 200, prompt: 1000, completion: 500})
					status, body, err := f.request(t.Context(), token, "billing-ratio", test.maxTokens, false)
					require.NoError(t, err)
					require.Equal(t, test.status, status, body)
					requests := 1
					if test.status != 200 {
						requests = 0
					}
					f.ledger(t, user, token, test.charge, requests)
					var got model.UserSubscription
					require.NoError(t, f.db.First(&got, sub.Id).Error)
					assert.EqualValues(t, test.charge, got.AmountUsed)
				})
			}
			// 实际上游挂起后改价，普通及阶梯路径的旧请求均按 80% 结算，新请求按 50% 结算。
			for _, test := range []struct {
				model                     string
				pre, oldCharge, newCharge int
			}{
				{"billing-ratio", 960, 3200, 2000},
				{"billing-tiered", 400, 4400, 2750},
			} {
				t.Run("inflight_price_change_"+test.model, func(t *testing.T) {
					user, token := f.user(t, test.model, 8000)
					state := &settlementUpstream{status: 200, prompt: 1000, completion: 500, entered: make(chan struct{}, 1), release: make(chan struct{})}
					f.scenario.Store(state)
					defer f.scenario.Store(&settlementUpstream{status: 200, prompt: 1000, completion: 500})
					var release sync.Once
					defer release.Do(func() { close(state.release) })
					finished := make(chan error, 1)
					go func() {
						status, body, err := f.request(t.Context(), token, test.model, 100, false)
						if err == nil && status != 200 {
							err = fmt.Errorf("HTTP %d: %s", status, body)
						}
						finished <- err
					}()
					select {
					case <-state.entered:
					case <-time.After(10 * time.Second):
						t.Fatal("upstream was not reached")
					}
					var reserved model.User
					require.NoError(t, f.db.First(&reserved, user.Id).Error)
					assert.Equal(t, settlementInitialQuota-test.pre, reserved.Quota)
					_, err := model.ReplaceUserModelPricingContext(t.Context(), user.Id, map[string]int{test.model: 5000}, 2)
					require.NoError(t, err)
					release.Do(func() { close(state.release) })
					require.NoError(t, <-finished)
					f.ledger(t, user, token, test.oldCharge, 1)
					f.scenario.Store(&settlementUpstream{status: 200, prompt: 1000, completion: 500})
					status, body, err := f.request(t.Context(), token, test.model, 100, false)
					require.NoError(t, err)
					require.Equal(t, 200, status, body)
					f.ledger(t, user, token, test.oldCharge+test.newCharge, 2, test.oldCharge, test.newCharge)
				})
			}
			// 明确的并发请求数用于验证原子累计，不以随机次数或仅打印结果代替对账。
			t.Run("concurrent_wallet_settlement", func(t *testing.T) {
				user, token := f.user(t, "billing-ratio", 8000)
				var wg sync.WaitGroup
				errors := make(chan error, 32)
				for range 32 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						status, body, err := f.request(t.Context(), token, "billing-ratio", 100, false)
						if err == nil && status != 200 {
							err = fmt.Errorf("HTTP %d: %s", status, body)
						}
						errors <- err
					}()
				}
				wg.Wait()
				close(errors)
				for err := range errors {
					require.NoError(t, err)
				}
				f.ledger(t, user, token, 32*3200, 32)
			})
			// 100 个用户各 10 次真实请求，按用户分别核对折扣和账本，再核对渠道总账。
			t.Run("hundred_users_final_accounting", func(t *testing.T) {
				users := make([]model.User, 100)
				tokens := make([]model.Token, 100)
				for index := range users {
					users[index], tokens[index] = f.user(t, "billing-ratio", 4000+index*50)
				}
				jobs := make(chan int, 32)
				failures := make(chan error, 1000)
				var workers sync.WaitGroup
				for range 32 {
					workers.Add(1)
					go func() {
						defer workers.Done()
						for index := range jobs {
							status, body, err := f.request(t.Context(), tokens[index], "billing-ratio", 100, false)
							if err == nil && status != 200 {
								err = fmt.Errorf("HTTP %d: %s", status, body)
							}
							failures <- err
						}
					}()
				}
				for request := range 1000 {
					jobs <- request % 100
				}
				close(jobs)
				workers.Wait()
				close(failures)
				for err := range failures {
					require.NoError(t, err)
				}
				for index := range users {
					f.ledger(t, users[index], tokens[index], 10*(1600+index*20), 10)
				}
				var channel model.Channel
				var logs []model.Log
				require.NoError(t, f.db.First(&channel, f.channel.Id).Error)
				require.NoError(t, f.db.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
				var total int64
				for _, entry := range logs {
					total += int64(entry.Quota)
				}
				assert.Equal(t, total, channel.UsedQuota)
				t.Logf("100 users, 1000 requests: database, token/cache balances, per-request logs and channel total reconciled")
			})
		})
	}
}
