package router

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 测试专用 CA 只在显式 fallback 模式启用，仍执行证书验证，不修改系统信任库。
var seedanceTestRootsOnce sync.Once

func TestSeedanceAssetValidationActionUsesDoubaoNamespace(t *testing.T) {
	router := gin.New()
	setSeedanceAssetValidationActionRoute(router)

	routes := router.Routes()
	require.Len(t, routes, 1)
	assert.Equal(t, http.MethodPost, routes[0].Method)
	assert.Equal(t, "/doubao/v2/assets", routes[0].Path)
}

// TestSeedanceAssetAPIKeysSharedLibrary 通过真实 TokenAuth 和生产路由验证用户级
// 共享、跨用户隔离、失效凭证及建组模型权限；仅替换付费上游，不调用真实云服务。
func TestSeedanceAssetAPIKeysSharedLibrary(t *testing.T) {
	for engineIndex, engine := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			var dialect gorm.Dialector
			dbType := common.DatabaseTypeSQLite
			switch engine {
			case "mysql":
				if os.Getenv("SEEDANCE_TEST_MYSQL_DSN") == "" {
					t.Skip("SEEDANCE_TEST_MYSQL_DSN is unset")
				}
				dialect, dbType = mysql.Open(os.Getenv("SEEDANCE_TEST_MYSQL_DSN")), common.DatabaseTypeMySQL
			case "postgres":
				if os.Getenv("SEEDANCE_TEST_POSTGRES_DSN") == "" {
					t.Skip("SEEDANCE_TEST_POSTGRES_DSN is unset")
				}
				dialect, dbType = postgres.Open(os.Getenv("SEEDANCE_TEST_POSTGRES_DSN")), common.DatabaseTypePostgreSQL
			default:
				dialect = sqlite.Open(filepath.Join(t.TempDir(), "assets.db"))
			}
			db, err := gorm.Open(dialect, &gorm.Config{})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(1)
			// 记录真实引擎版本，便于交付时区分驱动版本和数据库兼容性证据。
			versionSQL := "SELECT version()"
			if engine == "sqlite" {
				versionSQL = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
			t.Logf("database engine: %s %s", engine, version)
			previousDB, previousLog := model.DB, model.LOG_DB
			previousRedis, previousMemory, previousMaster := common.RedisEnabled, common.MemoryCacheEnabled, common.IsMasterNode
			previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
			model.DB, common.RedisEnabled, common.MemoryCacheEnabled, common.IsMasterNode = db, false, false, false
			common.SetMainDatabaseType(dbType)
			t.Setenv("LOG_SQL_DSN", "")
			require.NoError(t, model.InitLogDB())
			t.Cleanup(func() {
				model.DB, model.LOG_DB = previousDB, previousLog
				common.RedisEnabled, common.MemoryCacheEnabled = previousRedis, previousMemory
				common.SetDatabaseTypes(previousMainType, previousLogType)
				_ = model.InitLogDB()
				common.IsMasterNode = previousMaster
				assert.NoError(t, connection.Close())
			})
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetValidationSession{}, &model.SeedanceAsset{}, &model.SeedanceAssetSize{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetCleanupJob{}, &model.SystemTask{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}))
			users := []model.User{{Id: 85001, Username: "assetsowner", AffCode: "assetsowner", Group: "default", Status: common.UserStatusEnabled}, {Id: 85002, Username: "assetsother", AffCode: "assetsother", Group: "default", Status: common.UserStatusEnabled}}
			// 不同引擎使用不同用户，避免进程级限流跨测试累计。
			for i := range users {
				users[i].Id += engineIndex * 100
			}
			require.NoError(t, db.Create(&users).Error)
			tokens := []model.Token{
				{UserId: users[0].Id, Key: "assetkeyone", UnlimitedQuota: true, ExpiredTime: -1},
				{UserId: users[0].Id, Key: "assetkeytwo", UnlimitedQuota: true, ExpiredTime: -1, ModelLimitsEnabled: true, ModelLimits: "doubao-seedance-2-0-fast-260128"},
				{UserId: users[1].Id, Key: "assetkeyother", UnlimitedQuota: true, ExpiredTime: -1},
				{UserId: users[0].Id, Key: "assetkeyexpired", UnlimitedQuota: true, ExpiredTime: 1},
				{UserId: users[0].Id, Key: "assetkeydisabled", Status: common.TokenStatusDisabled, UnlimitedQuota: true, ExpiredTime: -1},
				{UserId: users[0].Id, Key: "assetkeyip", UnlimitedQuota: true, ExpiredTime: -1, AllowIps: common.GetPointer("10.0.0.0/8")},
				{UserId: users[0].Id, Key: "assetkeydeleted", UnlimitedQuota: true, ExpiredTime: -1},
			}
			require.NoError(t, db.Create(&tokens).Error)
			require.NoError(t, db.Delete(&tokens[6]).Error)
			var calls atomic.Int32
			var assetSequence atomic.Int32
			var groupSequence atomic.Int32
			var createdGroupType string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "Bearer fixtureupstream", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("Action") == "CreateAssetGroup" {
					var body struct {
						GroupType string `json:"GroupType"`
					}
					assert.NoError(t, common.DecodeJson(r.Body, &body))
					createdGroupType = body.GroupType
					sequence := groupSequence.Add(1)
					if sequence > 1 {
						_, _ = fmt.Fprintf(w, `{"Result":{"Id":"group-api-%d"}}`, sequence)
						return
					}
				}
				if r.URL.Query().Get("Action") == "CreateAsset" {
					_, _ = fmt.Fprintf(w, `{"Result":{"Id":"asset-api-%d"}}`, assetSequence.Add(1))
					return
				}
				_, _ = w.Write([]byte(`{"Result":{"Id":"group-api","Status":"Active"}}`))
			}))
			t.Cleanup(upstream.Close)
			channel := &model.Channel{Id: 85001, Type: constant.ChannelTypeDoubaoVideo, Key: "fixtureupstream", BaseURL: &upstream.URL, Models: "doubao-seedance-2-0-fast-260128", Group: "default", Status: common.ChannelStatusEnabled}
			require.NoError(t, db.Create(channel).Error)
			require.NoError(t, channel.AddAbilities(nil))
			router := gin.New()
			router.Use(middleware.BodyStorageCleanup())
			setSeedanceAssetAPIRoutes(router)
			setSeedanceAssetValidationActionRoute(router)
			// 请求助手保留真实路由和鉴权，仅负责构造测试 HTTP 消息。
			request := func(key, method, path, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, "/v1/seedance"+path, strings.NewReader(body))
				if key != "" {
					req.Header.Set("Authorization", "Bearer sk-"+key)
				}
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				return w
			}
			for _, key := range []string{"", "invalid", "assetkeyexpired", "assetkeydisabled", "assetkeydeleted"} {
				assert.Equal(t, 401, request(key, "GET", "/asset-groups", "").Code)
			}
			assert.Equal(t, 403, request("assetkeyip", "GET", "/asset-groups", "").Code)
			created := request("assetkeytwo", "POST", "/asset-groups", `{"name":"API素材组","model":"doubao-seedance-2-0-fast-260128","user_id":85002}`)
			require.Contains(t, created.Body.String(), `"success":true`)
			assert.Equal(t, "AIGC", createdGroupType)
			var group model.SeedanceAssetGroup
			require.NoError(t, db.First(&group).Error)
			assert.Equal(t, users[0].Id, group.UserID)
			assert.Equal(t, channel.Id, group.ChannelID)
			assert.NotEmpty(t, group.KeyFingerprint)
			for _, key := range []string{"assetkeyone", "assetkeytwo"} {
				listed := request(key, "GET", "/asset-groups", "")
				assert.Contains(t, listed.Body.String(), "group-api")
				assert.NotContains(t, listed.Body.String(), "fixtureupstream")
				assert.Contains(t, listed.Header().Get("Cache-Control"), "no-store")
			}
			assert.NotContains(t, request("assetkeyother", "GET", "/asset-groups", "").Body.String(), "group-api")
			before := calls.Load()
			for _, body := range []string{`{"name":"missingmodel"}`, `{"name":"deniedmodel","model":"doubao-seedance-2-0-260128"}`} {
				assert.Contains(t, request("assetkeytwo", "POST", "/asset-groups", body).Body.String(), `"success":false`)
			}
			asset := &model.SeedanceAsset{UserID: group.UserID, ChannelID: channel.Id, GroupID: group.GroupID, AssetID: "asset-existing", Name: "existing", AssetType: "Image", Status: "Active", KeyFingerprint: group.KeyFingerprint}
			require.NoError(t, db.Create(asset).Error)
			for _, key := range []string{"assetkeyone", "assetkeytwo"} {
				assert.Contains(t, request(key, "GET", "/assets", "").Body.String(), "asset-existing")
			}
			assert.NotContains(t, request("assetkeyother", "GET", "/assets", "").Body.String(), "asset-existing")
			for _, operation := range []struct{ method, path, body string }{
				{"PUT", fmt.Sprintf("/asset-groups/%d", group.ID), `{"name":"stolen"}`},
				{"DELETE", fmt.Sprintf("/asset-groups/%d", group.ID), ""},
				{"POST", fmt.Sprintf("/assets/%d/refresh", asset.ID), ""},
				{"DELETE", fmt.Sprintf("/assets/%d", asset.ID), ""},
			} {
				assert.Contains(t, request("assetkeyother", operation.method, operation.path, operation.body).Body.String(), `"success":false`)
			}
			assert.Equal(t, before, calls.Load(), "unauthorized requests must not contact upstream")
			assert.Contains(t, request("assetkeyother", "POST", "/assets/batch-delete", fmt.Sprintf(`{"ids":[%d]}`, asset.ID)).Body.String(), `"success":false`)
			// 使用数值公网地址避免测试依赖 DNS；替身只检查登记请求，不抓取资源。
			registered := request("assetkeytwo", "POST", "/assets", `{"group_id":"group-api","source_url":"https://8.8.8.8/file.png","asset_type":"Image","name":"registered"}`)
			require.Contains(t, registered.Body.String(), `"success":true`)
			assert.Contains(t, request("assetkeyone", "GET", "/assets?search=asset-api-1", "").Body.String(), "asset-api-1")
			// 真实 OSS SDK 通过回环 HTTP 服务上传；签名和 multipart 均使用生产实现。
			var uploaded atomic.Int32
			ossServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					uploaded.Add(1)
				}
				w.Header().Set("ETag", `"fixture"`)
				w.WriteHeader(200)
			}))
			t.Cleanup(ossServer.Close)
			t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
			seedanceTestRootsOnce.Do(func() {
				roots := x509.NewCertPool()
				roots.AddCert(ossServer.Certificate())
				x509.SetFallbackRoots(roots)
			})
			common.OptionMapRWMutex.Lock()
			previousOptions := common.OptionMap
			common.OptionMap = system_setting.PrivateAssetOSSOptionValues(system_setting.PrivateAssetOSSSettings{Region: "cn-hangzhou", Endpoint: ossServer.URL, Bucket: "fixture-assets", Prefix: "private-assets/", AccessKeyID: "fixtureid", AccessKeySecret: "fixturesecret"})
			common.OptionMapRWMutex.Unlock()
			t.Cleanup(func() {
				common.OptionMapRWMutex.Lock()
				common.OptionMap = previousOptions
				common.OptionMapRWMutex.Unlock()
			})
			var uploadBody bytes.Buffer
			form := multipart.NewWriter(&uploadBody)
			require.NoError(t, form.WriteField("group_id", group.GroupID))
			file, err := form.CreateFormFile("file", "fixture.png")
			require.NoError(t, err)
			_, err = file.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
			require.NoError(t, err)
			require.NoError(t, form.Close())
			uploadRequest := httptest.NewRequest("POST", "/v1/seedance/assets/upload", &uploadBody)
			uploadRequest.Header.Set("Authorization", "Bearer sk-assetkeytwo")
			uploadRequest.Header.Set("Content-Type", form.FormDataContentType())
			uploadResponse := httptest.NewRecorder()
			router.ServeHTTP(uploadResponse, uploadRequest)
			require.Equal(t, 202, uploadResponse.Code, uploadResponse.Body.String())
			assert.EqualValues(t, 1, uploaded.Load())
			assert.Contains(t, request("assetkeyone", "GET", "/assets?search=asset-api-2", "").Body.String(), "asset-api-2")
			assert.Contains(t, request("assetkeytwo", "POST", fmt.Sprintf("/assets/%d/refresh", asset.ID), "").Body.String(), `"success":true`)
			assert.Contains(t, request("assetkeyone", "PUT", fmt.Sprintf("/asset-groups/%d", group.ID), `{"name":"renamed"}`).Body.String(), `"success":true`)
			assert.Contains(t, request("assetkeytwo", "DELETE", fmt.Sprintf("/assets/%d", asset.ID), "").Body.String(), `"success":true`)
			assert.Contains(t, request("assetkeyone", "DELETE", fmt.Sprintf("/asset-groups/%d", group.ID), "").Body.String(), `"success":true`)
			// API Key 管理素材不进入计费链，用户和令牌的使用额度保持不变。
			var stored model.Token
			require.NoError(t, db.First(&stored, tokens[0].Id).Error)
			assert.Zero(t, stored.UsedQuota)
			assert.False(t, bytes.Contains(created.Body.Bytes(), []byte(group.KeyFingerprint)))
			beforeQueries := calls.Load()
			verifySeedanceLibraryQueries(t, db, router, users[0].Id+10, users[1].Id)
			assert.Equal(t, beforeQueries, calls.Load(), "local queries must not call upstream")
			verifySeedanceAssetActions(t, db, router, users[0].Id+20, users[1].Id)
		})
	}
}

// verifySeedanceAssetActions 通过生产路由覆盖 Action 全链路，并与原 REST 接口交叉读取。
func verifySeedanceAssetActions(t *testing.T, db *gorm.DB, router *gin.Engine, owner, outsider int) {
	t.Helper()
	require.NoError(t, db.Create(&model.User{Id: owner, Username: "actionowner", AffCode: "actionowner", Group: "default", Status: common.UserStatusEnabled}).Error)
	tokens := []model.Token{
		{UserId: owner, Key: "assetactionone", UnlimitedQuota: true, ExpiredTime: -1},
		{UserId: owner, Key: "assetactiontwo", UnlimitedQuota: true, ExpiredTime: -1, ModelLimitsEnabled: true, ModelLimits: "doubao-seedance-2-0-fast-260128"},
		{UserId: owner, Key: "assetactiondenied", UnlimitedQuota: true, ExpiredTime: -1, ModelLimitsEnabled: true, ModelLimits: "unavailable-model"},
	}
	require.NoError(t, db.Create(&tokens).Error)
	request := func(key, action, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/doubao/v2/assets?Action="+action+"&Version=2024-01-01", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-"+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	created := request("assetactiontwo", "CreateAssetGroup", `{"Name":"Action library","Description":"references","GroupType":"AIGC"}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var createdResponse struct {
		Result struct {
			ID string `json:"Id"`
		}
	}
	require.NoError(t, common.Unmarshal(created.Body.Bytes(), &createdResponse))
	groupID := createdResponse.Result.ID
	require.NotEmpty(t, groupID)
	assert.NotContains(t, created.Body.String(), "channel_id")
	assert.NotContains(t, created.Body.String(), "user_id")
	assert.NotContains(t, created.Body.String(), "fixtureupstream")
	assert.Equal(t, http.StatusBadGateway, request("assetactiondenied", "CreateAssetGroup", `{"Name":"Denied"}`).Code)
	assert.Equal(t, http.StatusBadRequest, request("assetactionone", "CreateAssetGroup", `{"Name":"Portrait","GroupType":"LivenessFace"}`).Code)
	groupBody := fmt.Sprintf(`{"Id":%q}`, groupID)
	assert.Contains(t, request("assetactionone", "GetAssetGroup", groupBody).Body.String(), `"Description":"references"`)
	assert.Contains(t, request("assetactionone", "ListAssetGroups", `{}`).Body.String(), groupID)
	updatedGroup := request("assetactiontwo", "UpdateAssetGroup", fmt.Sprintf(`{"Id":%q,"Description":"changed"}`, groupID))
	require.Equal(t, http.StatusOK, updatedGroup.Code, updatedGroup.Body.String())
	assert.Contains(t, request("assetactionone", "GetAssetGroup", groupBody).Body.String(), `"Description":"changed"`)
	registered := request("assetactiontwo", "CreateAsset", fmt.Sprintf(`{"GroupId":%q,"URL":"https://8.8.8.8/reference.png","AssetType":"Image","Name":"reference"}`, groupID))
	require.Equal(t, http.StatusOK, registered.Code, registered.Body.String())
	require.NoError(t, common.Unmarshal(registered.Body.Bytes(), &createdResponse))
	assetID := createdResponse.Result.ID
	require.NotEmpty(t, assetID)
	assetBody := fmt.Sprintf(`{"Id":%q}`, assetID)
	pending := request("assetactionone", "GetAsset", assetBody)
	require.Equal(t, http.StatusOK, pending.Code, pending.Body.String())
	assert.Contains(t, pending.Body.String(), `"Status":"Processing"`)
	assert.Contains(t, pending.Body.String(), `"URL":""`)
	var asset model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", owner, assetID).First(&asset).Error)
	// 只模拟上游审核终态；读取仍经过平台真实查询、归属校验与响应转换。
	require.NoError(t, db.Model(&asset).Update("status", "Active").Error)
	active := request("assetactionone", "GetAsset", assetBody)
	assert.Contains(t, active.Body.String(), `"URL":"https://8.8.8.8/reference.png"`)
	updated := request("assetactiontwo", "UpdateAsset", fmt.Sprintf(`{"Id":%q,"Name":"reference renamed"}`, assetID))
	require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
	assert.Contains(t, request("assetactionone", "GetAsset", assetBody).Body.String(), `"Name":"reference renamed"`)
	for _, tc := range []struct {
		action, body string
		total, count int
	}{
		{"ListAssetGroups", `{"PageNumber":1,"PageSize":1}`, 1, 1},
		{"ListAssetGroups", `{"PageNumber":99,"PageSize":1}`, 1, 0},
		{"ListAssetGroups", `{"Filter":{"GroupType":"LivenessFace"}}`, 0, 0},
		{"ListAssetGroups", `{"Filter":{"Name":"Action"}}`, 1, 1},
		{"ListAssets", `{"Filter":{"Statuses":["Active"],"Name":"renamed","AssetType":"Image"}}`, 1, 1},
		{"ListAssets", `{"Filter":{"Statuses":["Failed"]}}`, 0, 0},
		{"ListAssets", `{"PageNumber":99,"PageSize":1}`, 1, 0},
	} {
		w := request("assetactiontwo", tc.action, tc.body)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var response struct {
			ResponseMetadata struct{ Action string }
			Result           struct {
				TotalCount int
				Items      []map[string]any
			}
		}
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &response))
		assert.Equal(t, tc.action, response.ResponseMetadata.Action)
		assert.Equal(t, tc.total, response.Result.TotalCount, tc.body)
		assert.Len(t, response.Result.Items, tc.count, tc.body)
	}
	for _, tc := range []struct{ action, body string }{
		{"GetAsset", assetBody}, {"UpdateAsset", fmt.Sprintf(`{"Id":%q,"Name":"stolen"}`, assetID)}, {"DeleteAsset", assetBody},
		{"GetAssetGroup", groupBody}, {"UpdateAssetGroup", fmt.Sprintf(`{"Id":%q,"Name":"stolen"}`, groupID)}, {"DeleteAssetGroup", groupBody},
		{"CreateAsset", fmt.Sprintf(`{"GroupId":%q,"URL":"https://8.8.8.8/reference.png","AssetType":"Image"}`, groupID)},
	} {
		w := request("assetkeyother", tc.action, tc.body)
		assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"Code":"ResourceNotFound"`)
	}
	for _, tc := range []struct{ action, body string }{
		{"ListAssets", `{"PageNumber":0}`}, {"ListAssets", `{"PageSize":101}`},
		{"ListAssets", `{"PageNumber":null}`}, {"ListAssets", `{"Filter":[]}`},
		{"ListAssets", `{"Filter":{"UserId":1}}`}, {"ListAssets", `{"Filter":{"Statuses":[null]}}`},
		{"ListAssets", `{"Filter":{"AssetType":"PDF"}}`}, {"ListAssets", `{"SortBy":"name; DROP TABLE users"}`},
		{"ListAssets", `{"user_id":1}`}, {"GetAsset", `{}`},
		{"UpdateAsset", fmt.Sprintf(`{"Id":%q,"URL":"https://8.8.8.8/replaced.png"}`, assetID)},
		{"CreateAsset", fmt.Sprintf(`{"GroupId":%q,"URL":"http://127.0.0.1/private","AssetType":"Image"}`, groupID)},
	} {
		w := request("assetactionone", tc.action, tc.body)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), `"ResponseMetadata"`)
	}
	// 新旧接口读取同一条记录，数字 ID 只用于 REST，Action 使用公开字符串 ID。
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/seedance/assets/%d", asset.ID), nil)
	req.Header.Set("Authorization", "Bearer sk-assetactiontwo")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Contains(t, w.Body.String(), `"name":"reference renamed"`)
	deleted := request("assetactiontwo", "DeleteAsset", assetBody)
	require.Equal(t, http.StatusAccepted, deleted.Code, deleted.Body.String())
	assert.Contains(t, deleted.Body.String(), `"Status":"Deleting"`)
	assert.Contains(t, request("assetactionone", "GetAsset", assetBody).Body.String(), `"Status":"Deleting"`)
	deletedGroup := request("assetactiontwo", "DeleteAssetGroup", groupBody)
	require.Equal(t, http.StatusAccepted, deletedGroup.Code, deletedGroup.Body.String())
	assert.Contains(t, deletedGroup.Body.String(), `"Status":"Deleting"`)
	var jobs int64
	require.NoError(t, db.Model(&model.SeedanceAssetCleanupJob{}).Where("user_id = ?", owner).Count(&jobs).Error)
	assert.EqualValues(t, 2, jobs)
	var stored model.Token
	require.NoError(t, db.First(&stored, tokens[0].Id).Error)
	assert.Zero(t, stored.UsedQuota)
}

// verifySeedanceLibraryQueries 在同一真实数据库矩阵内验证授权、组合筛选和稳定分页。
// 独立用户隔离本组的限流预算；查询始终通过真实 TokenAuth 和生产路由。
func verifySeedanceLibraryQueries(t *testing.T, db *gorm.DB, router *gin.Engine, owner, outsider int) {
	t.Helper()
	require.NoError(t, db.Create(&model.User{Id: owner, Username: "queryowner", AffCode: "queryowner", Group: "default", Status: common.UserStatusEnabled}).Error)
	require.NoError(t, db.Create(&model.Token{UserId: owner, Key: "assetquery", UnlimitedQuota: true, ExpiredTime: -1}).Error)
	start := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	groups := []model.SeedanceAssetGroup{
		{UserID: owner, GroupID: "group-a", Name: "Group %_#", CreatedAt: start},
		{UserID: owner, GroupID: "group-b", Name: "Second", Status: "Deleting", CreatedAt: start.Add(time.Hour)},
		{UserID: outsider, GroupID: "group-secret", Name: "Secret", CreatedAt: start},
	}
	require.NoError(t, db.Create(&groups).Error)
	assets := []model.SeedanceAsset{
		{UserID: owner, GroupID: "group-a", AssetID: "query-a", Name: "same", AssetType: "Image", Status: "Success", CreatedAt: start, UpdatedAt: start},
		{UserID: owner, GroupID: "group-a", AssetID: "query-b", Name: "same", AssetType: "Video", Status: "Pending", CreatedAt: start, UpdatedAt: start.Add(time.Hour)},
		{UserID: owner, GroupID: "group-b", AssetID: "query-c", Name: "literal %_#", AssetType: "Audio", Status: "Failed", CreatedAt: start.Add(time.Hour), UpdatedAt: start},
		{UserID: outsider, GroupID: "group-secret", AssetID: "query-secret", Name: "Secret", AssetType: "Image", Status: "Active", CreatedAt: start},
	}
	require.NoError(t, db.Create(&assets).Error)
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/seedance"+path, nil)
		req.Header.Set("Authorization", "Bearer sk-assetquery")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct {
		query   string
		want    []string
		total   int64
		pending bool
	}{
		{"", []string{"query-c", "query-b", "query-a"}, 3, true},
		{"?group_ids=group-a,group-b&statuses=Active,Processing&asset_types=image,video&sort_by=created_at&sort_order=asc&page_size=1&p=2", []string{"query-b"}, 2, true},
		{"?group_ids=group-b&group_ids=group-a&statuses=Failed&statuses=Active&sort_by=name&sort_order=asc", []string{"query-c", "query-a"}, 2, true},
		{"?group_id=group-b&statuses=active", []string{}, 0, false},
		{"?group_id=group-a&status=active&search=same", []string{"query-a"}, 1, true},
		{"?asset_id=query-a", []string{"query-a"}, 1, true},
		{"?asset_ids=query-secret,query-a,query-a", []string{"query-a"}, 1, true},
		{fmt.Sprintf("?ids=%d,%d", assets[0].ID, assets[3].ID), []string{"query-a"}, 1, true},
		{"?created_after=2026-09-20T08:00:00%2B08:00&created_before=2026-09-20T01:00:00Z&sort_order=asc", []string{"query-a", "query-b"}, 2, true},
		{"?sort_by=updated_at&sort_order=desc", []string{"query-b", "query-c", "query-a"}, 3, true},
		{"?search=%25_%23", []string{"query-c"}, 1, true},
		{"?group_ids=group-secret", []string{}, 0, false},
		{"?sort_by=created_at&sort_order=asc&p=999&page_size=2", []string{"query-c"}, 3, true},
	} {
		t.Run("assets"+tc.query, func(t *testing.T) {
			w := request("/assets" + tc.query)
			require.Equal(t, 200, w.Code, w.Body.String())
			var response struct {
				Success    bool
				Data       []model.SeedanceAsset
				Total      int64
				HasPending bool `json:"has_pending"`
			}
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &response))
			require.True(t, response.Success)
			ids := make([]string, 0, len(response.Data))
			for _, asset := range response.Data {
				ids = append(ids, asset.AssetID)
				assert.Equal(t, owner, asset.UserID)
			}
			assert.Equal(t, tc.want, ids)
			assert.Equal(t, tc.total, response.Total)
			assert.Equal(t, tc.pending, response.HasPending)
		})
	}
	for _, tc := range []struct {
		path  string
		want  []string
		total int
	}{
		{"/asset-groups", []string{"group-b", "group-a"}, 0},
		{"/asset-groups?search=%25_%23", []string{"group-a"}, 0},
		{"/asset-groups?p=1&page_size=1&sort_by=created_at&sort_order=asc", []string{"group-a"}, 2},
		{"/asset-groups?status=deleting&group_ids=group-a,group-b", []string{"group-b"}, 0},
		{"/asset-groups?group_id=group-secret&p=1", []string{}, 0},
	} {
		w := request(tc.path)
		require.Equal(t, 200, w.Code, w.Body.String())
		var response struct {
			Success bool
			Data    []model.SeedanceAssetGroup
			Total   int
		}
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &response))
		ids := make([]string, 0, len(response.Data))
		for _, group := range response.Data {
			ids = append(ids, group.GroupID)
		}
		assert.Equal(t, tc.want, ids, tc.path)
		assert.Equal(t, tc.total, response.Total, tc.path)
	}
	for _, path := range []string{
		"/assets?ids=0", "/assets?ids=9223372036854775808", "/assets?asset_ids=one,", "/assets?asset_ids=" + strings.Repeat("a,", 100) + "a",
		"/assets?status=active&statuses=failed", "/assets?statuses=all,active", "/assets?statuses=unknown", "/assets?asset_types=all,image", "/assets?asset_types=pdf",
		"/assets?group_id=a&group_ids=b", "/assets?group_id=a&group_id=b", "/assets?asset_id=a&asset_ids=b",
		"/assets?sort_by=id%20desc;DROP", "/assets?sort_order=sideways", "/assets?p=0", "/assets?p=2147483648", "/assets?page_size=-1",
		"/assets?created_after=yesterday", "/assets?created_after=2026-09-21T00:00:00Z&created_before=2026-09-20T00:00:00Z",
		"/asset-groups?status=failed", "/asset-groups?asset_type=image", "/assets/nope", "/asset-groups/0",
	} {
		assert.Equal(t, 400, request(path).Code, path)
	}
	for _, tc := range []struct {
		path     string
		code     int
		contains string
	}{
		{fmt.Sprintf("/assets/%d", assets[0].ID), 200, "query-a"},
		{fmt.Sprintf("/asset-groups/%d", groups[0].ID), 200, "group-a"},
		{fmt.Sprintf("/assets/%d", assets[3].ID), 404, "resource not found"},
		{fmt.Sprintf("/asset-groups/%d", groups[2].ID), 404, "resource not found"},
		{"/assets/9223372036854775807", 404, "resource not found"},
	} {
		w := request(tc.path)
		assert.Equal(t, tc.code, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), tc.contains)
		assert.NotContains(t, w.Body.String(), "key_fingerprint")
	}
}
