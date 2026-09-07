// pricing-load 对隔离数据库执行真实 HTTP 鉴权和定价压测，不访问模型供应商或生产数据库。
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const userCount = 100

// 输出只包含聚合指标，不记录数据库连接信息、令牌或请求头。
type report struct {
	Engine           string  `json:"engine"`
	Version          string  `json:"version"`
	GoVersion        string  `json:"go_version"`
	CPUs             int     `json:"cpus"`
	RulesPerUser     int     `json:"rules_per_user"`
	TargetRPS        int     `json:"target_rps"`
	DurationSeconds  int     `json:"duration_seconds"`
	ElapsedSeconds   float64 `json:"elapsed_seconds"`
	Scheduled        int     `json:"scheduled"`
	Dropped          int     `json:"dropped"`
	Success          int64   `json:"success"`
	Failures         int64   `json:"failures"`
	Mismatches       int64   `json:"mismatches"`
	AchievedRPS      float64 `json:"achieved_rps"`
	P50MS            float64 `json:"p50_ms"`
	P95MS            float64 `json:"p95_ms"`
	P99MS            float64 `json:"p99_ms"`
	MaxLatencyMS     float64 `json:"max_latency_ms"`
	MaxDispatchLagMS float64 `json:"max_dispatch_lag_ms"`
	RuleQueries      int64   `json:"rule_queries"`
	SQLWaitCount     int64   `json:"sql_wait_count"`
	SQLWaitMS        float64 `json:"sql_wait_ms"`
	HeapMB           float64 `json:"heap_mb"`
	GCCount          uint32  `json:"gc_count"`
	UpdateSucceeded  bool    `json:"update_succeeded"`
	FirstError       string  `json:"first_error,omitempty"`
}

// 仅接受专用 Compose 的两种数据库目标，避免将负载或清理操作指向任意部署。
func main() {
	engine := flag.String("engine", "", "mysql or postgres (isolated Compose only)")
	rate := flag.Int("rate", 1200, "scheduled requests per second")
	seconds := flag.Int("seconds", 60, "measurement seconds, 1-180")
	rules := flag.Int("rules", 1000, "pricing rules per user, 1-1000")
	output := flag.String("output", "", "optional aggregate JSON report")
	flag.Parse()
	if err := run(*engine, *rate, *seconds, *rules, *output); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run 拥有测试表、Redis 专用逻辑库、HTTP 服务、负载协程和定时器，返回前全部关闭和清理。
func run(engine string, rate, seconds, rules int, output string) error {
	if rate < 1 || rate > 10000 || seconds < 1 || seconds > 180 || rate*seconds > 1000000 || rules < 1 || rules > 1000 {
		return fmt.Errorf("invalid bounded workload")
	}
	var dsn string
	var dbType common.DatabaseType
	redisDB := 10
	switch engine {
	case "mysql":
		dsn = "root@tcp(127.0.0.1:13316)/pricing_review?charset=utf8mb4&parseTime=true&timeout=5s"
		dbType = common.DatabaseTypeMySQL
	case "postgres":
		dsn = "postgres://postgres@127.0.0.1:15417/pricing_review?sslmode=disable&connect_timeout=5"
		dbType, redisDB = common.DatabaseTypePostgreSQL, 11
	default:
		return fmt.Errorf("select mysql or postgres")
	}
	// 使用正式连接初始化设置方言相关列名；测试自行管理最小表集，不启动应用后台任务。
	if err := os.Setenv("SQL_DSN", dsn); err != nil {
		return err
	}
	common.IsMasterNode = false
	if err := model.InitDB(); err != nil {
		return fmt.Errorf("test database connection failed: %w", err)
	}
	db := model.DB
	db.Logger = logger.Default.LogMode(logger.Silent)
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(32)
	sqlDB.SetMaxIdleConns(32)
	// 非空用户表意味着可能有其他验证正在运行，拒绝覆盖其数据。
	if db.Migrator().HasTable(&model.User{}) {
		var count int64
		if err := db.Model(&model.User{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("isolated database is not empty; refusing to overwrite users")
		}
	}
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dbType, dbType)
	common.RedisEnabled, common.MemoryCacheEnabled = true, true
	common.SyncFrequency = 60
	common.RDB = redis.NewClient(&redis.Options{Addr: "127.0.0.1:16379", DB: redisDB, PoolSize: 128})
	defer common.RDB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+120)*time.Second)
	defer cancel()
	if err := common.RDB.Ping(ctx).Err(); err != nil {
		return err
	}
	if err := common.RDB.FlushDB(ctx).Err(); err != nil {
		return err
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserModelPricing{}); err != nil {
		return err
	}
	defer func() {
		if err := db.Migrator().DropTable(&model.UserModelPricing{}, &model.Token{}, &model.User{}); err != nil {
			fmt.Fprintln(os.Stderr, "test table cleanup failed:", err)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := common.RDB.FlushDB(cleanupCtx).Err(); err != nil {
			fmt.Fprintln(os.Stderr, "test Redis cleanup failed:", err)
		}
	}()
	if err := model.InitializeUserModelPricingKeys(); err != nil {
		return err
	}
	result := report{Engine: engine, GoVersion: runtime.Version(), CPUs: runtime.GOMAXPROCS(0), RulesPerUser: rules, TargetRPS: rate, DurationSeconds: seconds}
	if err := db.Raw("SELECT VERSION()").Scan(&result.Version).Error; err != nil {
		return err
	}
	users := make([]model.User, userCount)
	keys := make([]string, userCount)
	ratios := make(map[string]float64, rules)
	for index := range rules {
		ratios[fmt.Sprintf("pricing-load-%04d", index)] = 2
	}
	ratioJSON, err := common.Marshal(ratios)
	if err != nil {
		return err
	}
	if err := ratio_setting.UpdateModelRatioByJSONString(string(ratioJSON)); err != nil {
		return err
	}
	if err := ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`); err != nil {
		return err
	}
	for index := range users {
		users[index] = model.User{Username: fmt.Sprintf("pricing-load-%03d", index), AffCode: fmt.Sprintf("pricing-load-%03d", index), Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default", Quota: 1000000000, AuthVersion: 1, ModelPricingVersion: 1}
		if err := db.Create(&users[index]).Error; err != nil {
			return err
		}
		keys[index] = rand.Text()
		token := model.Token{UserId: users[index].Id, Key: keys[index], Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1}
		if err := db.Create(&token).Error; err != nil {
			return err
		}
		discounts := make(map[string]int, rules)
		for name := range ratios {
			discounts[name] = 4000 + index*50
		}
		if _, err := model.ReplaceUserModelPricingContext(ctx, users[index].Id, discounts, 1); err != nil {
			return err
		}
	}
	// 被测 HTTP 路由调用生产鉴权和预扣估算函数；响应为实际计算结果，逐请求校验用户与价格。
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery(), middleware.TokenAuth())
	router.POST("/pricing-load", func(c *gin.Context) {
		var input struct {
			Model string `json:"model"`
		}
		if err := c.ShouldBindJSON(&input); err != nil {
			c.AbortWithStatus(400)
			return
		}
		value, _ := c.Get(string(constant.ContextKeyUserModelDiscounts))
		discounts, _ := value.(map[string]int)
		info := &relaycommon.RelayInfo{UserId: c.GetInt("id"), OriginModelName: input.Model, UserGroup: "default", UsingGroup: "default", UserModelDiscountBPS: discounts}
		price, err := helper.ModelPriceHelper(c, info, 1000, &types.TokenCountMeta{})
		if err != nil {
			c.AbortWithStatus(500)
			return
		}
		c.JSON(200, gin.H{"user_id": info.UserId, "quota": price.QuotaToPreConsume})
	})
	server := httptest.NewServer(router)
	defer server.Close()
	transport := &http.Transport{MaxIdleConns: 128, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 128}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var updated atomic.Bool
	request := func(index, modelIndex int, requireUpdated bool) error {
		payload := fmt.Sprintf(`{"model":"pricing-load-%04d"}`, modelIndex)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/pricing-load", strings.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer sk-"+keys[index])
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			return fmt.Errorf("HTTP %d", response.StatusCode)
		}
		var actual struct {
			UserID int `json:"user_id"`
			Quota  int `json:"quota"`
		}
		if err := common.DecodeJson(io.LimitReader(response.Body, 4096), &actual); err != nil {
			return err
		}
		expected := 800 + index*10
		// 改价中已开始的请求允许使用冻结旧价，提交后新开始的请求必须使用新价。
		if index == 0 && (requireUpdated || actual.Quota == 600) {
			expected = 600
		}
		if actual.UserID != users[index].Id || actual.Quota != expected {
			return fmt.Errorf("pricing mismatch: user=%d quota=%d expected=%d", actual.UserID, actual.Quota, expected)
		}
		return nil
	}
	for index := range users {
		if err := request(index, 0, false); err != nil {
			return fmt.Errorf("warmup: %w", err)
		}
	}
	var queries, success, failures, mismatches atomic.Int64
	if err := db.Callback().Query().Before("gorm:query").Register("pricing-load:rules", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_model_pricings" {
			queries.Add(1)
		}
	}); err != nil {
		return err
	}
	beforeSQL := sqlDB.Stats()
	var beforeMem runtime.MemStats
	runtime.ReadMemStats(&beforeMem)
	type job struct {
		sequence  int
		scheduled time.Time
	}
	jobs := make(chan job, 128)
	durations := make([]time.Duration, rate*seconds)
	var wg sync.WaitGroup
	var firstError sync.Once
	for range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				err := request(item.sequence%userCount, item.sequence%rules, updated.Load())
				durations[item.sequence] = time.Since(item.scheduled)
				if err != nil {
					failures.Add(1)
					if strings.HasPrefix(err.Error(), "pricing mismatch") {
						mismatches.Add(1)
					}
					firstError.Do(func() { result.FirstError = err.Error() })
				} else {
					success.Add(1)
				}
			}
		}()
	}
	started := time.Now()
	updateDone := make(chan error, 1)
	go func() {
		timer := time.NewTimer(time.Duration(seconds) * time.Second / 2)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			updateDone <- ctx.Err()
			return
		case <-timer.C:
		}
		discounts := make(map[string]int, rules)
		for name := range ratios {
			discounts[name] = 3000
		}
		_, err := model.ReplaceUserModelPricingContext(ctx, users[0].Id, discounts, 2)
		if err == nil {
			updated.Store(true)
		}
		updateDone <- err
	}()
	// 开环按计划到达时间计量延迟；队列有界，过载丢弃单独统计，避免隐藏协调遗漏。
	ticker := time.NewTicker(10 * time.Millisecond)
	for result.Scheduled < len(durations) {
		select {
		case <-ctx.Done():
			ticker.Stop()
			close(jobs)
			wg.Wait()
			<-updateDone
			return ctx.Err()
		case <-ticker.C:
		}
		due := min(len(durations), int(time.Since(started).Seconds()*float64(rate)))
		// 记录最早待发送请求的调度滞后，辅助识别负载发生器自身的暂停或突发补发。
		dispatchLag := time.Since(started) - time.Duration(float64(result.Scheduled)/float64(rate)*float64(time.Second))
		result.MaxDispatchLagMS = max(result.MaxDispatchLagMS, float64(dispatchLag)/float64(time.Millisecond))
		for result.Scheduled < due {
			item := job{result.Scheduled, started.Add(time.Duration(float64(result.Scheduled) / float64(rate) * float64(time.Second)))}
			select {
			case jobs <- item:
			default:
				result.Dropped++
			}
			result.Scheduled++
		}
	}
	ticker.Stop()
	close(jobs)
	wg.Wait()
	updateErr := <-updateDone
	result.UpdateSucceeded = updateErr == nil
	result.ElapsedSeconds = time.Since(started).Seconds()
	result.Success, result.Failures, result.Mismatches = success.Load(), failures.Load(), mismatches.Load()
	result.AchievedRPS = float64(result.Success) / result.ElapsedSeconds
	result.RuleQueries = queries.Load()
	afterSQL := sqlDB.Stats()
	result.SQLWaitCount, result.SQLWaitMS = afterSQL.WaitCount-beforeSQL.WaitCount, float64(afterSQL.WaitDuration-beforeSQL.WaitDuration)/float64(time.Millisecond)
	var afterMem runtime.MemStats
	runtime.ReadMemStats(&afterMem)
	result.HeapMB, result.GCCount = float64(afterMem.HeapAlloc)/(1024*1024), afterMem.NumGC-beforeMem.NumGC
	valid := durations[:0]
	for _, duration := range durations {
		if duration > 0 {
			valid = append(valid, duration)
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i] < valid[j] })
	if len(valid) > 0 {
		result.P50MS = float64(valid[(len(valid)-1)*50/100]) / float64(time.Millisecond)
		result.P95MS = float64(valid[(len(valid)-1)*95/100]) / float64(time.Millisecond)
		result.P99MS = float64(valid[(len(valid)-1)*99/100]) / float64(time.Millisecond)
		result.MaxLatencyMS = float64(valid[len(valid)-1]) / float64(time.Millisecond)
	}
	encoded, err := common.Marshal(result)
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	if output != "" {
		if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(output, append(encoded, '\n'), 0644); err != nil {
			return err
		}
	}
	if updateErr != nil {
		return fmt.Errorf("concurrent update failed: %w", updateErr)
	}
	if result.Failures != 0 || result.Dropped != 0 {
		return fmt.Errorf("workload failed acceptance: failures=%d dropped=%d", result.Failures, result.Dropped)
	}
	return nil
}
