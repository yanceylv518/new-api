package router

import (
	"bytes"
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
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 真实HTTP服务和生产认证/分发/预扣/轮询/日志串联，仅上游供应商由可控HTTP服务替代。
func TestVideoHTTPBillingChainLoad(t *testing.T) {
	if os.Getenv("VIDEO_DEEP_AUDIT") != "1" {
		t.Skip("explicit audit only")
	}
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	previousLimit := constant.TaskQueryLimit
	constant.TaskQueryLimit = 400
	t.Cleanup(func() { constant.TaskQueryLimit = previousLimit })
	for _, key := range []string{"hailuo", "doubao"} {
		t.Run(key, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Error)})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(1)
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.Task{}, &model.Log{}, &model.Option{}, &model.UserSubscription{}, &model.SubscriptionPlan{}, &model.SubscriptionPreConsumeRecord{}, &model.QuotaData{}, &model.Model{}, &model.Vendor{}))
			oldDB, oldLog := model.DB, model.LOG_DB
			oldOptions := common.OptionMap
			common.OptionMap = make(map[string]string)
			oldRedis, oldMemory, oldBatch, oldConsume := common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled
			oldFactory := service.GetTaskAdaptorFunc
			oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
			oldModes, _ := common.Marshal(billing_setting.GetBillingModeCopy())
			oldExpressions, _ := common.Marshal(billing_setting.GetBillingExprCopy())
			model.DB, model.LOG_DB = db, db
			common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false, false, true
			common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
			// 复用启动入口初始化方言列名，避免测试绕过启动后出现空列名。
			previousMaster := common.IsMasterNode
			common.IsMasterNode = false
			t.Setenv("LOG_SQL_DSN", "")
			require.NoError(t, model.InitLogDB())
			common.IsMasterNode = previousMaster
			service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
			t.Cleanup(func() {
				assert.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_mode": string(oldModes), "billing_setting.billing_expr": string(oldExpressions)}))
				common.OptionMap = oldOptions
				model.DB, model.LOG_DB = oldDB, oldLog
				common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = oldRedis, oldMemory, oldBatch, oldConsume
				common.SetDatabaseTypes(oldMainType, oldLogType)
				service.GetTaskAdaptorFunc = oldFactory
				assert.NoError(t, connection.Close())
			})
			modelName, route := "MiniMax-H3", "/hailuo/v2/video_generation"
			expression := `tier("audit",u("seconds")*0.0002+max(u("input_images")-5,0)*0.00002)`
			finalQuota := 510
			if key == "doubao" {
				modelName, route, expression = "doubao-seedance-2-0-fast-260128", "/doubao/api/v3/contents/generations/tasks", `tier("audit",u("tokens")/1000000)`
				finalQuota = 500
			}
			modes, _ := common.Marshal(map[string]string{modelName: "tiered_expr"})
			expressions, _ := common.Marshal(map[string]string{modelName: expression})
			require.NoError(t, model.UpdateOptionsBulk(map[string]string{"billing_setting.billing_mode": string(modes), "billing_setting.billing_expr": string(expressions)}))
			var accepted atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					id := fmt.Sprintf("audit-upstream-%d", accepted.Add(1))
					if key == "hailuo" {
						fmt.Fprintf(w, `{"task_id":%q}`, id)
					} else {
						fmt.Fprintf(w, `{"id":%q}`, id)
					}
					return
				}
				id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				if r.Header.Get("Authorization") != "Bearer audit-local-only" {
					http.Error(w, "wrong upstream account", http.StatusNotFound)
					return
				}
				if key == "hailuo" {
					fmt.Fprintf(w, `{"task":{"task_id":%q,"model":"MiniMax-H3","status":"succeeded","task_type":"video_generation","duration":5,"resolution":"768P","usage":{"output_seconds":5,"input_image_count":6,"input_seconds":0},"content":{"url":"https://example.com/audit.mp4"}}}`, id)
				} else {
					fmt.Fprintf(w, `{"id":%q,"model":%q,"status":"succeeded","duration":5,"resolution":"720p","usage":{"total_tokens":1000,"completion_tokens":1000},"content":{"video_url":"https://example.com/audit.mp4"}}`, id, modelName)
				}
			}))
			defer upstream.Close()
			plugin, ok := jsplugin.DefaultRegistry.Get(key)
			require.True(t, ok)
			channel := model.Channel{Id: 1, Type: plugin.Meta.ChannelTypes[0], Name: "isolated-http-audit", Key: "audit-local-only", BaseURL: &upstream.URL, Status: common.ChannelStatusEnabled, Models: modelName, Group: "default", OtherSettings: `{"disable_task_polling_sleep":true}`}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, db.Create(&model.Ability{Group: "default", Model: modelName, ChannelId: 1, Enabled: true, Weight: 1}).Error)
			const initial = 1000000000
			for index := range 200 {
				id := index + 1
				require.NoError(t, db.Create(&model.User{Id: id, Username: fmt.Sprint(id), AffCode: fmt.Sprint(id), Group: "default", Quota: initial, Status: common.UserStatusEnabled}).Error)
				require.NoError(t, db.Create(&model.Token{Id: id, UserId: id, Key: fmt.Sprintf("auditvideokey%036d", id), RemainQuota: initial, Status: common.TokenStatusEnabled, ExpiredTime: -1, Group: "default"}).Error)
			}
			outer, registry := newPluginRouterTest(t, []*jsplugin.LoadedPlugin{plugin}, productionPluginRouteHandlers)
			outer.NoRoute((&pluginRouteDispatcher{registry: registry}).dispatch)
			server := httptest.NewServer(outer)
			defer server.Close()
			client := &http.Client{Timeout: 30 * time.Second}
			resolution := "768P"
			if key == "doubao" {
				resolution = "720p"
			}
			content := []map[string]any{{"type": "text", "text": "isolated audit"}}
			if key == "hailuo" {
				for range 6 {
					content = append(content, map[string]any{"type": "image_url", "role": "reference_image", "image_url": map[string]any{"url": "https://example.com/audit.png"}})
				}
			}
			payloadBytes, err := common.Marshal(map[string]any{"model": modelName, "resolution": resolution, "duration": 10, "content": content})
			require.NoError(t, err)
			payload := string(payloadBytes)
			jobs, results := make(chan int, 128), make(chan error, 400)
			var workers sync.WaitGroup
			for range 64 {
				workers.Go(func() {
					for index := range jobs {
						request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+route, bytes.NewBufferString(payload))
						if err != nil {
							results <- err
							continue
						}
						request.Header.Set("Authorization", fmt.Sprintf("Bearer sk-auditvideokey%036d", index%200+1))
						request.Header.Set("Content-Type", "application/json")
						response, err := client.Do(request)
						if err != nil {
							results <- err
							continue
						}
						body, err := io.ReadAll(response.Body)
						response.Body.Close()
						if response.StatusCode != 200 {
							err = fmt.Errorf("submit status=%d body=%s", response.StatusCode, body)
						}
						results <- err
					}
				})
			}
			for index := range 400 {
				jobs <- index
			}
			close(jobs)
			workers.Wait()
			close(results)
			for err := range results {
				require.NoError(t, err)
			}
			require.EqualValues(t, 400, accepted.Load())
			// 通过生产轮询读取受控上游，提交后的表达式变更不能影响已冻结的结算价格。
			changedExpression := `tier("changed",max(u("input_images")-10,0))`
			if key == "doubao" {
				changedExpression = `tier("changed",u("tokens")*999)`
			}
			changed, _ := common.Marshal(map[string]string{modelName: changedExpression})
			require.NoError(t, model.UpdateOption("billing_setting.billing_expr", string(changed)))
			// 管理员重排账号密钥后，在途任务必须仍查询提交时所属账号。
			if os.Getenv("VIDEO_AUDIT_ROTATE_KEYS") == "1" {
				channel.Key = "audit-new-account\naudit-local-only"
				channel.ChannelInfo.IsMultiKey = true
				require.NoError(t, db.Model(&channel).Select("key", "channel_info").Updates(&channel).Error)
			}
			for range 8 {
				if service.RunTaskPollingOnce(t.Context(), nil).UnfinishedTasks == 0 {
					break
				}
			}
			var tasks []model.Task
			require.NoError(t, db.Find(&tasks).Error)
			require.Len(t, tasks, 400)
			for _, task := range tasks {
				require.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status)
				require.Equal(t, finalQuota, task.Quota)
			}
			// 使用真实查询路由验证任务可读性、跨用户隔离和冻结凭证不进入公共响应。
			queryRoute := route + "/" + tasks[0].TaskID
			if key == "hailuo" {
				queryRoute = "/hailuo/v2/query/video_generation/" + tasks[0].TaskID
			}
			for _, owner := range []int{tasks[0].UserId, tasks[0].UserId%200 + 1} {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+queryRoute, nil)
				require.NoError(t, err)
				request.Header.Set("Authorization", fmt.Sprintf("Bearer sk-auditvideokey%036d", owner))
				response, err := client.Do(request)
				require.NoError(t, err)
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				require.NoError(t, err)
				if owner == tasks[0].UserId {
					assert.Equal(t, http.StatusOK, response.StatusCode)
				} else {
					assert.NotEqual(t, http.StatusOK, response.StatusCode)
					assert.NotContains(t, string(body), "audit-upstream-")
				}
				assert.NotContains(t, string(body), "audit-local-only")
				assert.NotContains(t, string(body), "audit-new-account")
			}
			var users []model.User
			var tokens []model.Token
			require.NoError(t, db.Find(&users).Error)
			require.NoError(t, db.Find(&tokens).Error)
			for _, user := range users {
				assert.EqualValues(t, initial-2*finalQuota, user.Quota)
				assert.EqualValues(t, 2*finalQuota, user.UsedQuota)
			}
			for _, token := range tokens {
				assert.Equal(t, initial-2*finalQuota, token.RemainQuota)
				assert.Equal(t, 2*finalQuota, token.UsedQuota)
			}
			var logs []model.Log
			require.NoError(t, db.Find(&logs).Error)
			signed := 0
			for _, entry := range logs {
				if entry.Type == model.LogTypeRefund {
					signed -= entry.Quota
				} else if entry.Type == model.LogTypeConsume {
					signed += entry.Quota
				}
			}
			assert.Equal(t, 400*finalQuota, signed)
			t.Logf("HTTP_CHAIN plugin=%s users=200 workers=64 tasks=400 logs=%d final_quota=%d", key, len(logs), signed)
		})
	}
}
