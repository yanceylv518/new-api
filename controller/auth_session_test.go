package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestManagementRateLimitDatabase(t *testing.T) {
	// 会话和审计写入使用固定表名，必须连接独立的临时数据库。
	if os.Getenv("TEST_RATE_LIMIT_DEDICATED_DB") != "1" {
		t.Skip("requires an isolated MySQL or PostgreSQL test database")
	}
	require.Contains(t, []string{"mysql", "postgres"}, os.Getenv("TEST_TASK_DB_DIALECT"))
	db, dialect := openTaskDialectDatabase(t, &model.User{}, &model.UserSession{}, &model.Option{}, &model.Token{}, &model.Log{})
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousClient := common.RedisEnabled, common.RDB
	previousMemory, previousSecret := common.MemoryCacheEnabled, common.SessionSecret
	previousLogin, previousEncryption := common.PasswordLoginEnabled, common.PasswordLoginEncryptionEnabled
	previousMaster := common.IsMasterNode
	previousSettings, err := common.Marshal(common.GetManagementRateLimits())
	require.NoError(t, err)
	common.OptionMapRWMutex.Lock()
	previousOptions := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dialect, dialect)
	common.RedisEnabled, common.RDB, common.MemoryCacheEnabled = true, client, false
	common.SessionSecret = "management-rate-limit-test-session-signing"
	common.PasswordLoginEnabled, common.PasswordLoginEncryptionEnabled = true, false
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled, common.RDB, common.MemoryCacheEnabled, common.SessionSecret = previousRedis, previousClient, previousMemory, previousSecret
		common.PasswordLoginEnabled, common.PasswordLoginEncryptionEnabled = previousLogin, previousEncryption
		common.IsMasterNode = previousMaster
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptions
		common.OptionMapRWMutex.Unlock()
		require.NoError(t, common.LoadManagementRateLimits(string(previousSettings)))
		require.NoError(t, client.Close())
	})
	common.IsMasterNode = false
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	require.NoError(t, db.Table("audit_logs").AutoMigrate(&model.AuditLog{}))
	t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable("audit_logs")) })
	initialJSON, err := common.Marshal(common.DefaultManagementRateLimits())
	require.NoError(t, err)
	initialValue := string(initialJSON)
	_, err = model.UpdateManagementRateLimits(initialValue, &initialValue)
	require.NoError(t, err)
	settings := common.DefaultManagementRateLimits()
	settings.GlobalAPI.Enabled = false
	settings.LoginIP = common.ManagementRateLimit{Enabled: true, Requests: 10, WindowSeconds: 60}
	settings.LoginAccount = common.ManagementRateLimit{Enabled: true, Requests: 2, WindowSeconds: 60}
	settings.SessionRefresh = common.ManagementRateLimit{Enabled: true, Requests: 1, WindowSeconds: 60}
	settings.TokenQuery = common.ManagementRateLimit{Enabled: true, Requests: 2, WindowSeconds: 60}
	encoded, err := common.Marshal(settings)
	require.NoError(t, err)
	_, err = model.UpdateManagementRateLimits(string(encoded), nil)
	require.NoError(t, err)
	originalSettingsValue := string(encoded)
	password, err := common.Password2Hash("management-fixture-password")
	require.NoError(t, err)
	users := []model.User{
		{Username: "rate-user-a", Email: "rate-a@example.test", AffCode: "rate-a", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AuthVersion: 1, Password: password},
		{Username: "rate-user-b", Email: "rate-b@example.test", AffCode: "rate-b", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AuthVersion: 1, Password: password},
		{Username: "rate-root", AffCode: "rate-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AuthVersion: 1, Password: password},
	}
	for index := range users {
		require.NoError(t, db.Create(&users[index]).Error)
	}
	rootSession, err := service.CreateLoginSession(users[2].Id, "password", "192.0.2.1", "test")
	require.NoError(t, err)
	userSession, err := service.CreateLoginSession(users[0].Id, "password", "192.0.2.1", "test")
	require.NoError(t, err)
	otherSession, err := service.CreateLoginSession(users[1].Id, "password", "192.0.2.1", "test")
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(middleware.GlobalAPIRateLimit())
	router.POST("/api/user/login", middleware.CriticalRateLimit("login"), Login)
	router.POST("/api/user/auth/refresh", RefreshAuth)
	router.PUT("/api/option/", middleware.RootAuth(), UpdateOption)
	router.GET("/api/option/", middleware.RootAuth(), GetOptions)
	router.GET("/api/usage/token/", middleware.TokenAuthReadOnly(), middleware.TokenQueryRateLimit(), GetTokenUsage)
	request := func(method, path, address string, payload any, authorization, refresh string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := common.Marshal(payload)
		require.NoError(t, err)
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.RemoteAddr = address
		r.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			r.Header.Set("Authorization", "Bearer "+authorization)
		}
		if refresh != "" {
			r.AddCookie(&http.Cookie{Name: service.RefreshCookieName, Value: refresh})
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	t.Run("username and email share the account limit across IPs", func(t *testing.T) {
		for _, identifier := range []string{users[0].Username, users[0].Email} {
			response := request(http.MethodPost, "/api/user/login", "192.0.2.10:1000", LoginRequest{Username: identifier, Password: "wrong-password"}, "", "")
			require.Equal(t, 200, response.Code)
			assert.Contains(t, response.Body.String(), `"success":false`)
		}
		response := request(http.MethodPost, "/api/user/login", "192.0.2.11:1000", LoginRequest{Username: users[0].Username, Password: "wrong-password"}, "", "")
		require.Equal(t, 429, response.Code)
		assert.Equal(t, "60", response.Header().Get("Retry-After"))
		assert.Contains(t, response.Body.String(), "AUTH_RATE_LIMITED")
		response = request(http.MethodPost, "/api/user/login", "192.0.2.10:1000", LoginRequest{Username: users[1].Username, Password: "wrong-password"}, "", "")
		assert.Equal(t, 200, response.Code)
	})
	t.Run("API keys share the owner's query budget", func(t *testing.T) {
		for index, owner := range []int{users[0].Id, users[0].Id, users[1].Id} {
			key := strings.Repeat(string(rune('a'+index)), 32)
			token := model.Token{UserId: owner, Key: key, Status: common.TokenStatusEnabled, ExpiredTime: -1}
			require.NoError(t, db.Create(&token).Error)
			response := request(http.MethodGet, "/api/usage/token/", "192.0.2.10:1000", nil, "sk-"+key, "")
			assert.Equal(t, 200, response.Code)
			assert.Contains(t, response.Body.String(), `"object":"token_usage"`)
		}
		response := request(http.MethodGet, "/api/usage/token/", "192.0.2.12:1000", nil, "sk-"+strings.Repeat("b", 32), "")
		assert.Equal(t, 429, response.Code)
	})
	t.Run("only root can save and invalid updates preserve persisted settings", func(t *testing.T) {
		settings.TokenQuery.Requests = 10
		encoded, err := common.Marshal(settings)
		require.NoError(t, err)
		expectedValue := originalSettingsValue
		payload := OptionUpdateRequest{Key: common.ManagementRateLimitsOptionKey, Value: string(encoded), ExpectedValue: &expectedValue}
		assert.Equal(t, 401, request(http.MethodPut, "/api/option/", "192.0.2.1:1000", payload, "", "").Code)
		assert.Equal(t, 403, request(http.MethodPut, "/api/option/", "192.0.2.1:1000", payload, userSession.AccessToken, "").Code)
		missingBaseline := payload
		missingBaseline.ExpectedValue = nil
		assert.Equal(t, 400, request(http.MethodPut, "/api/option/", "192.0.2.1:1000", missingBaseline, rootSession.AccessToken, "").Code)
		response := request(http.MethodPut, "/api/option/", "192.0.2.1:1000", payload, rootSession.AccessToken, "")
		require.Equal(t, 200, response.Code)
		assert.Contains(t, response.Body.String(), `"success":true`)
		assert.Equal(t, settings, common.GetManagementRateLimits())
		var stored model.Option
		require.NoError(t, db.Where(&model.Option{Key: payload.Key}).First(&stored).Error)
		assert.Equal(t, string(encoded), stored.Value)
		expectedValue = string(encoded)
		response = request(http.MethodGet, "/api/usage/token/", "192.0.2.12:1000", nil, "sk-"+strings.Repeat("b", 32), "")
		assert.Equal(t, 200, response.Code)
		response = request(http.MethodGet, "/api/option/", "192.0.2.1:1000", nil, rootSession.AccessToken, "")
		assert.Equal(t, 200, response.Code)
		assert.Contains(t, response.Body.String(), common.ManagementRateLimitsOptionKey)
		invalid := settings
		invalid.TokenQuery.Requests = 0
		badJSON, err := common.Marshal(invalid)
		require.NoError(t, err)
		payload.Value = string(badJSON)
		response = request(http.MethodPut, "/api/option/", "192.0.2.1:1000", payload, rootSession.AccessToken, "")
		assert.Contains(t, response.Body.String(), `"success":false`)
		require.NoError(t, db.Where(&model.Option{Key: payload.Key}).First(&stored).Error)
		assert.Equal(t, string(encoded), stored.Value)
		assert.Equal(t, settings, common.GetManagementRateLimits())
		var invalidFields map[string]map[string]any
		require.NoError(t, common.Unmarshal(encoded, &invalidFields))
		invalidFields["login_ip"]["enabled"] = nil
		nullEnabled, err := common.Marshal(invalidFields)
		require.NoError(t, err)
		payload.Value = string(nullEnabled)
		response = request(http.MethodPut, "/api/option/", "192.0.2.1:1000", payload, rootSession.AccessToken, "")
		assert.Contains(t, response.Body.String(), `"success":false`)
		assert.Equal(t, settings, common.GetManagementRateLimits())
		stale := settings
		stale.LoginIP.Requests = 5
		staleJSON, err := common.Marshal(stale)
		require.NoError(t, err)
		staleBaseline := originalSettingsValue
		payload.Value, payload.ExpectedValue = string(staleJSON), &staleBaseline
		response = request(http.MethodPut, "/api/option/", "192.0.2.1:1000", payload, rootSession.AccessToken, "")
		require.Equal(t, 409, response.Code)
		assert.Contains(t, response.Body.String(), "MANAGEMENT_RATE_LIMITS_CONFLICT")
		require.NoError(t, db.Where(&model.Option{Key: payload.Key}).First(&stored).Error)
		assert.Equal(t, string(encoded), stored.Value)
		assert.Equal(t, settings, common.GetManagementRateLimits())
	})
	t.Run("concurrent saves from one baseline accept exactly one writer", func(t *testing.T) {
		current := common.GetManagementRateLimits()
		baseJSON, err := common.Marshal(current)
		require.NoError(t, err)
		baseline := string(baseJSON)
		responses := make(chan *httptest.ResponseRecorder, 2)
		var workers sync.WaitGroup
		for _, count := range []int{11, 12} {
			candidate := current
			candidate.TokenQuery.Requests = count
			value, err := common.Marshal(candidate)
			require.NoError(t, err)
			payload := OptionUpdateRequest{Key: common.ManagementRateLimitsOptionKey, Value: string(value), ExpectedValue: &baseline}
			workers.Go(func() {
				responses <- request(http.MethodPut, "/api/option/", "192.0.2.1:1000", payload, rootSession.AccessToken, "")
			})
		}
		workers.Wait()
		close(responses)
		statuses := make(map[int]int)
		for response := range responses {
			statuses[response.Code]++
		}
		assert.Equal(t, map[int]int{200: 1, 409: 1}, statuses)
		var stored model.Option
		require.NoError(t, db.Where(&model.Option{Key: common.ManagementRateLimitsOptionKey}).First(&stored).Error)
		parsed, err := common.ParseManagementRateLimits(stored.Value)
		require.NoError(t, err)
		assert.Equal(t, parsed, common.GetManagementRateLimits())
	})
	t.Run("a sync that read old options cannot overwrite a later save", func(t *testing.T) {
		oldRead, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var released sync.Once
		var paused atomic.Bool
		release := func() { released.Do(func() { close(resume) }) }
		t.Cleanup(release)
		const callbackName = "test:pause-management-options-read"
		require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
			if _, ok := tx.Statement.Dest.(*[]*model.Option); ok && paused.CompareAndSwap(false, true) {
				close(oldRead)
				select {
				case <-resume:
				case <-t.Context().Done():
				}
			}
		}))
		t.Cleanup(func() {
			release()
			<-finished
			require.NoError(t, db.Callback().Query().Remove(callbackName))
		})
		go func() {
			model.SyncOptionsOnce()
			close(finished)
		}()
		select {
		case <-oldRead:
		case <-t.Context().Done():
			t.Fatal("option sync did not reach its query")
		}
		current := common.GetManagementRateLimits()
		baseJSON, err := common.Marshal(current)
		require.NoError(t, err)
		baseline := string(baseJSON)
		current.TokenQuery.Requests++
		value, err := common.Marshal(current)
		require.NoError(t, err)
		response := request(http.MethodPut, "/api/option/", "192.0.2.1:1000", OptionUpdateRequest{Key: common.ManagementRateLimitsOptionKey, Value: string(value), ExpectedValue: &baseline}, rootSession.AccessToken, "")
		require.Equal(t, 200, response.Code)
		release()
		select {
		case <-finished:
		case <-t.Context().Done():
			t.Fatal("option sync did not complete")
		}
		assert.Equal(t, current, common.GetManagementRateLimits())
	})
	t.Run("refresh limits preserve rotation and replay protection", func(t *testing.T) {
		for _, invalidCookie := range []string{"", userSession.Session.SID + ".unrecognized-secret"} {
			response := request(http.MethodPost, "/api/user/auth/refresh", "192.0.2.20:1000", nil, "", invalidCookie)
			assert.Equal(t, 401, response.Code)
		}
		response := request(http.MethodPost, "/api/user/auth/refresh", "192.0.2.20:1000", nil, "", userSession.RefreshToken)
		require.Equal(t, 200, response.Code)
		var rotatedCookie string
		for _, cookie := range response.Result().Cookies() {
			if cookie.Name == service.RefreshCookieName {
				rotatedCookie = cookie.Value
			}
		}
		require.NotEmpty(t, rotatedCookie)
		require.NotEqual(t, userSession.RefreshToken, rotatedCookie)
		before, err := model.GetUserSessionBySID(userSession.Session.SID)
		require.NoError(t, err)
		response = request(http.MethodPost, "/api/user/auth/refresh", "192.0.2.21:1000", nil, "", rotatedCookie)
		require.Equal(t, 429, response.Code)
		assert.Equal(t, "60", response.Header().Get("Retry-After"))
		assert.Empty(t, response.Header().Values("Set-Cookie"))
		after, err := model.GetUserSessionBySID(userSession.Session.SID)
		require.NoError(t, err)
		assert.Equal(t, before.RefreshHash, after.RefreshHash)
		assert.Equal(t, model.UserSessionStatusActive, after.Status)
		response = request(http.MethodPost, "/api/user/auth/refresh", "192.0.2.21:1000", nil, "", userSession.Session.SID+".unrecognized-secret")
		assert.Equal(t, 401, response.Code)
		response = request(http.MethodPost, "/api/user/auth/refresh", "192.0.2.20:1000", nil, "", otherSession.RefreshToken)
		assert.Equal(t, 200, response.Code)
		server.FastForward(time.Minute)
		response = request(http.MethodPost, "/api/user/auth/refresh", "192.0.2.21:1000", nil, "", rotatedCookie)
		require.Equal(t, 200, response.Code)
		require.NoError(t, db.Model(&model.UserSession{}).Where("sid = ?", userSession.Session.SID).Update("previous_valid_until", time.Now().Unix()-1).Error)
		response = request(http.MethodPost, "/api/user/auth/refresh", "192.0.2.21:1000", nil, "", rotatedCookie)
		assert.Equal(t, 401, response.Code)
		after, err = model.GetUserSessionBySID(userSession.Session.SID)
		require.NoError(t, err)
		assert.Equal(t, model.UserSessionStatusRevoked, after.Status)
	})
}

func TestAuthLogoutRejectsRefreshCookieSessionMismatch(t *testing.T) {
	previousDB := model.DB
	previousRedis := common.RedisEnabled
	previousSecret := common.SessionSecret
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}))
	model.DB = db
	common.RedisEnabled = false
	common.SessionSecret = "auth-logout-mismatch-test-secret"
	t.Cleanup(func() {
		model.DB = previousDB
		common.RedisEnabled = previousRedis
		common.SessionSecret = previousSecret
	})

	user := &model.User{
		Username: "logout-mismatch-user", Password: "unused", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1,
	}
	require.NoError(t, db.Create(user).Error)
	sessionA, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", "agent-a")
	require.NoError(t, err)
	sessionB, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", "agent-b")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/auth/logout", nil)
	c.Request.Header.Set("Authorization", "Bearer "+sessionA.AccessToken)
	c.Request.Header.Set("X-Auth-Session", sessionA.Session.SID)
	c.Request.AddCookie(&http.Cookie{Name: service.RefreshCookieName, Value: sessionB.RefreshToken})

	AuthLogout(c)

	assert.Equal(t, http.StatusConflict, recorder.Code)
	var response struct {
		Success bool   `json:"success"`
		Code    string `json:"code"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
	assert.Equal(t, "AUTH_SESSION_MISMATCH", response.Code)
	for _, sid := range []string{sessionA.Session.SID, sessionB.Session.SID} {
		stored, err := model.GetUserSessionBySID(sid)
		require.NoError(t, err)
		assert.Equal(t, model.UserSessionStatusActive, stored.Status)
	}
}

func TestWriteAuthSessionErrorMapsSessionGrowthLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name           string
		err            error
		expectedStatus int
		expectedCode   string
	}{
		{
			name:           "active session limit",
			err:            model.ErrUserSessionLimit,
			expectedStatus: http.StatusConflict,
			expectedCode:   "AUTH_SESSION_LIMIT",
		},
		{
			name:           "issuance limit",
			err:            model.ErrUserSessionIssuanceLimit,
			expectedStatus: http.StatusTooManyRequests,
			expectedCode:   "AUTH_SESSION_ISSUANCE_LIMIT",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			writeAuthSessionError(c, test.err)

			assert.Equal(t, test.expectedStatus, recorder.Code)
			var response struct {
				Success bool   `json:"success"`
				Code    string `json:"code"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.False(t, response.Success)
			assert.Equal(t, test.expectedCode, response.Code)
		})
	}
}

func TestSessionLimitDoesNotRecordRejectedLoginAsSuccessful(t *testing.T) {
	previousDB := model.DB
	previousRedis := common.RedisEnabled
	previousActiveLimit := common.UserSessionActiveLimit
	previousIssuanceLimit := common.UserSessionIssuanceLimit
	previousIssuanceWindow := common.UserSessionIssuanceWindowSeconds
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.TwoFA{}, &model.PasskeyCredential{}))
	model.DB = db
	common.RedisEnabled = false
	common.UserSessionActiveLimit = 1
	common.UserSessionIssuanceLimit = 100
	common.UserSessionIssuanceWindowSeconds = int64(common.DefaultUserSessionIssuanceWindowSeconds)
	t.Cleanup(func() {
		model.DB = previousDB
		common.RedisEnabled = previousRedis
		common.UserSessionActiveLimit = previousActiveLimit
		common.UserSessionIssuanceLimit = previousIssuanceLimit
		common.UserSessionIssuanceWindowSeconds = previousIssuanceWindow
	})

	const previousLastLoginAt = int64(123)
	user := &model.User{
		Username: "rejected-login-audit-user", Password: "unused", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, LastLoginAt: previousLastLoginAt,
	}
	require.NoError(t, db.Create(user).Error)
	now := time.Now().Unix()
	require.NoError(t, db.Create(&model.UserSession{
		SID: "existing-active-session", UserID: user.Id, Version: 1, UserAuthVersion: user.AuthVersion,
		Status: model.UserSessionStatusActive, RefreshHash: "hash", LoginMethod: "password",
		CreatedAt: now, LastActiveAt: now, ExpiresAt: now + 3600,
	}).Error)

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/login", nil)
	setupLogin(user, nil, c)

	assert.Equal(t, http.StatusConflict, recorder.Code)
	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	assert.Equal(t, previousLastLoginAt, stored.LastLoginAt)
}
