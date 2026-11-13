package router

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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
	paths := make(map[string]bool)
	for _, route := range routes {
		assert.Equal(t, http.MethodPost, route.Method)
		paths[route.Path] = true
	}
	for _, path := range []string{"/doubao/v2/assets", "/doubao/api/ark/assets", "/doubao/v1/ark/asset", "/doubao/api/v3/asset", "/doubao", "/doubao/"} {
		assert.True(t, paths[path], path)
	}
}

func TestSeedanceCompatAsyncUploadMySQL(t *testing.T) {
	dsn := isolatedSeedanceCompatTestDSN(t, seedanceCompatTestDSN(t))
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	previousDB, previousLog := model.DB, model.LOG_DB
	previousMain, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousRedis, previousMemory := common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled
	previousOptions := common.OptionMap
	previousRatios, previousGroups := ratio_setting.GroupRatio2JSONString(), setting.UserUsableGroups2JSONString()
	common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = true, false, false
	common.OptionMap = make(map[string]string)
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	db := model.DB
	connection, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, connection.Close())
		model.DB, model.LOG_DB = previousDB, previousLog
		common.SetDatabaseTypes(previousMain, previousLogType)
		common.IsMasterNode, common.RedisEnabled, common.MemoryCacheEnabled = previousMaster, previousRedis, previousMemory
		common.OptionMap = previousOptions
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousGroups))
	})
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"vip":1,"aaa":1}`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"vip":"VIP","aaa":"AAA"}`))
	user := &model.User{Id: 1, Username: "async-compat-user", Password: "fixture", AffCode: "async-compat-user", Status: common.UserStatusEnabled, Group: "vip"}
	require.NoError(t, db.Create(user).Error)
	require.NoError(t, db.Create(&model.Token{UserId: user.Id, Key: "asynccompatvip", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "vip"}).Error)
	require.NoError(t, db.Create(&model.Token{UserId: user.Id, Key: "asynccompataaa", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "aaa"}).Error)
	require.NoError(t, db.Create(&model.Token{UserId: user.Id, Key: "asynccompatlimited", Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "vip", ModelLimitsEnabled: true, ModelLimits: "doubao-seedance-2-5-260628"}).Error)

	var imageData bytes.Buffer
	require.NoError(t, png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 300, 300))))
	var stored atomic.Value
	var uploadCount atomic.Int32
	ossServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			content, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			stored.Store(content)
			uploadCount.Add(1)
		}
		w.Header().Set("ETag", `"fixture"`)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			if content, ok := stored.Load().([]byte); ok {
				_, _ = w.Write(content)
			}
		}
	}))
	t.Cleanup(ossServer.Close)
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	seedanceTestRootsOnce.Do(func() {
		roots := x509.NewCertPool()
		roots.AddCert(ossServer.Certificate())
		x509.SetFallbackRoots(roots)
	})
	common.OptionMap = system_setting.PrivateAssetOSSOptionValues(system_setting.PrivateAssetOSSSettings{Region: "cn-hangzhou", Endpoint: ossServer.URL, Bucket: "fixture-assets", Prefix: "private-assets/", AccessKeyID: "fixtureid", AccessKeySecret: "fixturesecret"})
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imageData.Bytes())
	}))
	t.Cleanup(source.Close)
	fetchSetting := system_setting.GetFetchSetting()
	previousFetch := *fetchSetting
	for _, endpoint := range []string{ossServer.URL, source.URL} {
		parsed, parseErr := url.Parse(endpoint)
		require.NoError(t, parseErr)
		fetchSetting.AllowedPorts = append(fetchSetting.AllowedPorts, parsed.Port())
	}
	fetchSetting.AllowPrivateIp = true
	t.Cleanup(func() { *fetchSetting = previousFetch })
	service.InitHttpClient()

	var upstreamCreates atomic.Int32
	var upstreamGets atomic.Int32
	var invalidUpstreamSource atomic.Bool
	ossEndpoint, err := url.Parse(ossServer.URL)
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"group-%s"}}`, key)
		case "CreateAsset":
			var payload struct {
				URL string `json:"URL"`
			}
			if decodeErr := common.DecodeJson(r.Body, &payload); decodeErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			parsed, parseErr := url.Parse(payload.URL)
			if parseErr != nil || parsed.Hostname() != ossEndpoint.Hostname() || parsed.RawQuery == "" {
				invalidUpstreamSource.Store(true)
			}
			sequence := upstreamCreates.Add(1)
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"asset-%s-%d"}}`, key, sequence)
		case "GetAsset":
			upstreamGets.Add(1)
			status := "Active"
			if key == "async-high" {
				status = "Processing"
			}
			_, _ = fmt.Fprintf(w, `{"Result":{"Status":%q}}`, status)
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(upstream.Close)
	const modelName = "doubao-seedance-2-0-260128"
	settingValue := `{"seedance_asset_url":"` + upstream.URL + `"}`
	for _, item := range []struct {
		id       int
		key      string
		group    string
		priority int64
	}{{201, "async-high", "vip", 10}, {202, "async-low", "vip", 0}, {203, "async-other", "aaa", 20}} {
		channel := &model.Channel{Id: item.id, Type: constant.ChannelTypeDoubaoVideo, Name: item.key, Key: item.key, Status: common.ChannelStatusEnabled, Group: item.group, Models: modelName, BaseURL: &upstream.URL, Setting: &settingValue, Priority: &item.priority}
		require.NoError(t, channel.Insert())
	}
	channel, err := model.GetChannelById(202, true)
	require.NoError(t, err)
	fingerprint := fmt.Sprintf("%x", common.Sha256Raw([]byte(channel.Key)))
	group := &model.SeedanceAssetGroup{UserID: user.Id, ChannelID: channel.Id, GroupID: "group-async-compat", Name: "async", GroupType: "AIGC", Status: "Active", KeyFingerprint: fingerprint, AccountFingerprint: model.SeedanceAssetAccountFingerprint(channel, fingerprint)}
	require.NoError(t, db.Create(group).Error)
	router := gin.New()
	router.Use(middleware.BodyStorageCleanup())
	setSeedanceAssetAPIRoutes(router)
	setSeedanceCompatAssetRoutes(router)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	require.NoError(t, form.WriteField("group_id", group.GroupID))
	require.NoError(t, form.WriteField("mode", "library"))
	file, err := form.CreateFormFile("file", "picture.png")
	require.NoError(t, err)
	_, err = file.Write(imageData.Bytes())
	require.NoError(t, err)
	require.NoError(t, form.Close())
	request := httptest.NewRequest(http.MethodPost, "/doubao/api/assets/upload", &body)
	request.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	request.Header.Set("Content-Type", form.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var uploaded struct {
		Data struct {
			ID     string `json:"assetId"`
			Status string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &uploaded))
	require.NotEmpty(t, uploaded.Data.ID)
	require.Equal(t, "PROCESSING", uploaded.Data.Status)
	require.EqualValues(t, 1, uploadCount.Load())
	require.Zero(t, upstreamCreates.Load(), "上传应在后台调用素材服务")
	require.Equal(t, imageData.Bytes(), stored.Load())
	var asset model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", user.Id, uploaded.Data.ID).First(&asset).Error)
	require.True(t, asset.IsReplicaManaged())
	require.NotEmpty(t, asset.ObjectKey)
	replicas, err := model.FindSeedanceAssetReplicas(t.Context(), user.Id, asset.ID)
	require.NoError(t, err)
	require.Len(t, replicas, 2)
	for _, replica := range replicas {
		require.Contains(t, []int{201, 202}, replica.ChannelID)
	}
	_, err = service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 2, upstreamCreates.Load())
	query := httptest.NewRequest(http.MethodGet, "/doubao/api/assets/"+asset.AssetID, nil)
	query.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	queried := httptest.NewRecorder()
	router.ServeHTTP(queried, query)
	require.Equal(t, http.StatusOK, queried.Code, queried.Body.String())
	require.Contains(t, queried.Body.String(), `"status":"ACTIVE"`)
	queryGroup := func(key, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer sk-"+key)
		result := httptest.NewRecorder()
		router.ServeHTTP(result, req)
		return result
	}
	otherStatus := queryGroup("asynccompataaa", "/doubao/api/assets/"+asset.AssetID)
	require.Equal(t, http.StatusOK, otherStatus.Code, otherStatus.Body.String())
	require.Contains(t, otherStatus.Body.String(), `"status":"PROCESSING"`)
	otherList := queryGroup("asynccompataaa", "/doubao/api/assets/list?status=active")
	require.Equal(t, http.StatusOK, otherList.Code, otherList.Body.String())
	require.Contains(t, otherList.Body.String(), `"total":0`)
	clientList := queryGroup("asynccompataaa", "/doubao/api/assets/client-list")
	require.Equal(t, http.StatusOK, clientList.Code, clientList.Body.String())
	require.Contains(t, clientList.Body.String(), `"activeCount":0`)
	nativeStatus := queryGroup("asynccompataaa", fmt.Sprintf("/v1/seedance/assets/%d", asset.ID))
	require.Equal(t, http.StatusOK, nativeStatus.Code, nativeStatus.Body.String())
	require.Contains(t, nativeStatus.Body.String(), `"status":"Processing"`)
	nativeList := queryGroup("asynccompataaa", "/v1/seedance/assets?status=active")
	require.Equal(t, http.StatusOK, nativeList.Code, nativeList.Body.String())
	require.Contains(t, nativeList.Body.String(), `"total":0`)
	tree := queryGroup("asynccompataaa", "/doubao/api/assets/tree")
	require.Equal(t, http.StatusOK, tree.Code, tree.Body.String())
	require.Contains(t, tree.Body.String(), `"status":"PROCESSING"`)

	otherChannel, err := model.GetChannelById(203, true)
	require.NoError(t, err)
	otherKey := fmt.Sprintf("%x", common.Sha256Raw([]byte(otherChannel.Key)))
	otherScope := model.SeedanceAssetModelScopeFingerprint(otherChannel, modelName, otherKey)
	otherReplica, err := model.FindSeedanceAssetReplica(t.Context(), user.Id, asset.ID, otherScope)
	require.NoError(t, err)
	require.NotEmpty(t, otherReplica.SyncScope)
	otherStatus = queryGroup("asynccompataaa", "/doubao/api/assets/"+asset.AssetID)
	require.Contains(t, otherStatus.Body.String(), `"status":"PROCESSING"`)
	_, err = service.RunSeedanceAssetSynchronizationOnce(t.Context(), nil)
	require.NoError(t, err)
	otherStatus = queryGroup("asynccompataaa", "/doubao/api/assets/"+asset.AssetID)
	require.Contains(t, otherStatus.Body.String(), `"status":"ACTIVE"`)
	otherList = queryGroup("asynccompataaa", "/doubao/api/assets/list?status=active")
	require.Contains(t, otherList.Body.String(), `"total":1`)
	clientList = queryGroup("asynccompataaa", "/doubao/api/assets/client-list")
	require.Contains(t, clientList.Body.String(), `"activeCount":1`)
	require.EqualValues(t, 3, upstreamCreates.Load())
	optimizeRequest := httptest.NewRequest(http.MethodPost, "/doubao/api/assets/"+asset.AssetID+"/optimize", strings.NewReader(`{"model_family":"doubao-seedance-2-0"}`))
	optimizeRequest.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	optimizeRequest.Header.Set("Content-Type", "application/json")
	optimized := httptest.NewRecorder()
	router.ServeHTTP(optimized, optimizeRequest)
	require.Equal(t, http.StatusOK, optimized.Code, optimized.Body.String())
	require.Contains(t, optimized.Body.String(), `"status":"processing"`)
	humanGroup := &model.SeedanceAssetGroup{UserID: user.Id, ChannelID: channel.Id, GroupID: "group-async-human", Name: "human", GroupType: "LivenessFace", Status: "Active", KeyFingerprint: fingerprint, AccountFingerprint: group.AccountFingerprint}
	require.NoError(t, db.Create(humanGroup).Error)
	var humanBody bytes.Buffer
	humanForm := multipart.NewWriter(&humanBody)
	require.NoError(t, humanForm.WriteField("group_id", humanGroup.GroupID))
	require.NoError(t, humanForm.WriteField("mode", "real"))
	humanFile, err := humanForm.CreateFormFile("file", "person.png")
	require.NoError(t, err)
	_, err = humanFile.Write(imageData.Bytes())
	require.NoError(t, err)
	require.NoError(t, humanForm.Close())
	humanRequest := httptest.NewRequest(http.MethodPost, "/doubao/api/assets/upload", &humanBody)
	humanRequest.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	humanRequest.Header.Set("Content-Type", humanForm.FormDataContentType())
	humanResponse := httptest.NewRecorder()
	router.ServeHTTP(humanResponse, humanRequest)
	require.Equal(t, http.StatusOK, humanResponse.Code, humanResponse.Body.String())
	var humanAsset struct {
		Data struct {
			ID string `json:"assetId"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(humanResponse.Body.Bytes(), &humanAsset))
	var humanRecord model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", user.Id, humanAsset.Data.ID).First(&humanRecord).Error)
	require.False(t, humanRecord.IsReplicaManaged())
	require.Equal(t, channel.Id, humanRecord.ChannelID)
	require.NotEmpty(t, humanRecord.PrimarySyncScope)
	humanReplicas, err := model.FindSeedanceAssetReplicas(t.Context(), user.Id, humanRecord.ID)
	require.NoError(t, err)
	require.Empty(t, humanReplicas)
	require.EqualValues(t, 4, upstreamCreates.Load())
	require.False(t, invalidUpstreamSource.Load(), "审核应使用 OSS 签名地址")
	humanRecord.Status = "Active"
	require.NoError(t, db.Save(&humanRecord).Error)
	beforeRefresh := upstreamGets.Load()
	unauthorizedRefresh := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/seedance/assets/%d/refresh", humanRecord.ID), nil)
	unauthorizedRefresh.Header.Set("Authorization", "Bearer sk-asynccompataaa")
	unauthorizedRefreshResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedRefreshResponse, unauthorizedRefresh)
	require.Equal(t, http.StatusForbidden, unauthorizedRefreshResponse.Code, unauthorizedRefreshResponse.Body.String())
	require.Equal(t, beforeRefresh, upstreamGets.Load(), "分组外令牌不能查询绑定渠道")
	limitedRefresh := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/seedance/assets/%d/refresh", humanRecord.ID), nil)
	limitedRefresh.Header.Set("Authorization", "Bearer sk-asynccompatlimited")
	limitedRefreshResponse := httptest.NewRecorder()
	router.ServeHTTP(limitedRefreshResponse, limitedRefresh)
	require.Equal(t, http.StatusForbidden, limitedRefreshResponse.Code, limitedRefreshResponse.Body.String())
	require.Equal(t, beforeRefresh, upstreamGets.Load(), "模型限制内没有可用渠道时不能查询绑定渠道")
	var unauthorizedBody bytes.Buffer
	unauthorizedForm := multipart.NewWriter(&unauthorizedBody)
	require.NoError(t, unauthorizedForm.WriteField("group_id", humanGroup.GroupID))
	require.NoError(t, unauthorizedForm.WriteField("mode", "real"))
	unauthorizedFile, err := unauthorizedForm.CreateFormFile("file", "person.png")
	require.NoError(t, err)
	_, err = unauthorizedFile.Write(imageData.Bytes())
	require.NoError(t, err)
	require.NoError(t, unauthorizedForm.Close())
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, "/doubao/api/assets/upload", &unauthorizedBody)
	unauthorizedRequest.Header.Set("Authorization", "Bearer sk-asynccompataaa")
	unauthorizedRequest.Header.Set("Content-Type", unauthorizedForm.FormDataContentType())
	unauthorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedResponse, unauthorizedRequest)
	require.Equal(t, http.StatusForbidden, unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	require.EqualValues(t, 4, upstreamCreates.Load(), "分组外令牌不能调用绑定渠道上传")
	beforeUploads := uploadCount.Load()
	unauthorizedURLBody := fmt.Sprintf(`{"group_id":%q,"source_url":%q,"asset_type":"Image"}`, humanGroup.GroupID, source.URL+"/image.png")
	unauthorizedURL := httptest.NewRequest(http.MethodPost, "/v1/seedance/assets", strings.NewReader(unauthorizedURLBody))
	unauthorizedURL.Header.Set("Authorization", "Bearer sk-asynccompataaa")
	unauthorizedURL.Header.Set("Content-Type", "application/json")
	unauthorizedURLResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedURLResponse, unauthorizedURL)
	require.Equal(t, http.StatusForbidden, unauthorizedURLResponse.Code, unauthorizedURLResponse.Body.String())
	require.Equal(t, beforeUploads, uploadCount.Load(), "分组外令牌不能上传 OSS 素材")
	require.EqualValues(t, 4, upstreamCreates.Load(), "分组外令牌不能调用绑定渠道创建素材")
	require.NoError(t, db.Model(&humanRecord).UpdateColumn("primary_sync_scope", "").Error)
	authorizedRefresh := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/seedance/assets/%d/refresh", humanRecord.ID), nil)
	authorizedRefresh.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	authorizedRefreshResponse := httptest.NewRecorder()
	router.ServeHTTP(authorizedRefreshResponse, authorizedRefresh)
	require.Equal(t, http.StatusOK, authorizedRefreshResponse.Code, authorizedRefreshResponse.Body.String())
	require.Greater(t, upstreamGets.Load(), beforeRefresh)
	require.NoError(t, db.First(&humanRecord, humanRecord.ID).Error)
	require.NotEmpty(t, humanRecord.PrimarySyncScope, "授权刷新应恢复素材的后台审核范围")
	humanVIP := queryGroup("asynccompatvip", "/doubao/api/assets/"+humanRecord.AssetID)
	require.Contains(t, humanVIP.Body.String(), `"status":"ACTIVE"`)
	humanOther := queryGroup("asynccompataaa", "/doubao/api/assets/"+humanRecord.AssetID)
	require.Contains(t, humanOther.Body.String(), `"status":"FAILED"`)

	urlBody := fmt.Sprintf(`{"group_id":%q,"url":%q,"name":"remote"}`, group.GroupID, source.URL+"/image.png")
	urlRequest := httptest.NewRequest(http.MethodPost, "/doubao/api/assets", strings.NewReader(urlBody))
	urlRequest.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	urlRequest.Header.Set("Content-Type", "application/json")
	urlResponse := httptest.NewRecorder()
	router.ServeHTTP(urlResponse, urlRequest)
	require.Equal(t, http.StatusOK, urlResponse.Code, urlResponse.Body.String())
	require.Contains(t, urlResponse.Body.String(), `"status":"PROCESSING"`)
	require.EqualValues(t, 3, uploadCount.Load())
	require.EqualValues(t, 4, upstreamCreates.Load())
	limitedChannel := &model.Channel{Id: 204, Type: constant.ChannelTypeDoubaoVideo, Name: "async-limited", Key: "async-limited", Status: common.ChannelStatusEnabled, Group: "vip", Models: "doubao-seedance-2-5-260628", BaseURL: &upstream.URL, Setting: &settingValue}
	require.NoError(t, limitedChannel.Insert())
	limitedRequest := httptest.NewRequest(http.MethodPost, "/doubao/api/assets", strings.NewReader(urlBody))
	limitedRequest.Header.Set("Authorization", "Bearer sk-asynccompatlimited")
	limitedRequest.Header.Set("Content-Type", "application/json")
	limitedResponse := httptest.NewRecorder()
	router.ServeHTTP(limitedResponse, limitedRequest)
	require.Equal(t, http.StatusOK, limitedResponse.Code, limitedResponse.Body.String())
	var limitedAsset struct {
		Data struct {
			ID string `json:"assetId"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(limitedResponse.Body.Bytes(), &limitedAsset))
	var limitedRecord model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", user.Id, limitedAsset.Data.ID).First(&limitedRecord).Error)
	limitedReplicas, err := model.FindSeedanceAssetReplicas(t.Context(), user.Id, limitedRecord.ID)
	require.NoError(t, err)
	require.Len(t, limitedReplicas, 1)
	require.Equal(t, limitedChannel.Id, limitedReplicas[0].ChannelID)

	var linkBody bytes.Buffer
	linkForm := multipart.NewWriter(&linkBody)
	require.NoError(t, linkForm.WriteField("group_id", group.GroupID))
	require.NoError(t, linkForm.WriteField("mode", "link"))
	linkFile, err := linkForm.CreateFormFile("file", "link.png")
	require.NoError(t, err)
	_, err = linkFile.Write(imageData.Bytes())
	require.NoError(t, err)
	require.NoError(t, linkForm.Close())
	linkRequest := httptest.NewRequest(http.MethodPost, "/doubao/api/assets/upload", &linkBody)
	linkRequest.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	linkRequest.Header.Set("Content-Type", linkForm.FormDataContentType())
	linkResponse := httptest.NewRecorder()
	router.ServeHTTP(linkResponse, linkRequest)
	require.Equal(t, http.StatusOK, linkResponse.Code, linkResponse.Body.String())
	var linkAsset struct {
		Data struct {
			ID string `json:"assetId"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(linkResponse.Body.Bytes(), &linkAsset))
	require.NotEmpty(t, linkAsset.Data.ID)
	linkClient, err := service.NewSeedanceAssetClient(channel)
	require.NoError(t, err)
	var mapped map[string]string
	var mapErr error
	require.NotPanics(t, func() {
		mapped, mapErr = service.MapSeedanceAssetsToModelAccount(t.Context(), user.Id, linkClient, channel, modelName, []string{linkAsset.Data.ID})
	})
	require.NoError(t, mapErr)
	require.Contains(t, mapped[linkAsset.Data.ID], "private-assets/")
	linkOptimize := httptest.NewRequest(http.MethodPost, "/doubao/api/assets/"+linkAsset.Data.ID+"/optimize", strings.NewReader(`{"model_family":"doubao-seedance-2-0"}`))
	linkOptimize.Header.Set("Authorization", "Bearer sk-asynccompatvip")
	linkOptimize.Header.Set("Content-Type", "application/json")
	optimizedLink := httptest.NewRecorder()
	router.ServeHTTP(optimizedLink, linkOptimize)
	require.Equal(t, http.StatusOK, optimizedLink.Code, optimizedLink.Body.String())
	var promoted model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", user.Id, linkAsset.Data.ID).First(&promoted).Error)
	require.NotEmpty(t, promoted.PrimaryUpstreamID)
	promotedChannel, err := model.GetChannelById(promoted.ChannelID, true)
	require.NoError(t, err)
	promotedClient, err := service.NewSeedanceAssetClient(promotedChannel, promoted.KeyFingerprint)
	require.NoError(t, err)
	require.NotPanics(t, func() {
		mapped, mapErr = service.MapSeedanceAssetsToModelAccount(t.Context(), user.Id, promotedClient, promotedChannel, modelName, []string{linkAsset.Data.ID})
	})
	if mapErr != nil {
		require.ErrorIs(t, mapErr, service.ErrSeedanceAssetMappingPending)
	} else {
		require.NotEmpty(t, mapped[linkAsset.Data.ID])
	}
}

func TestSeedanceCompatRoutesCoexistWithExistingRoutes(t *testing.T) {
	router := gin.New()
	setSeedanceAssetAPIRoutes(router)
	setSeedanceAssetValidationActionRoute(router)
	setSeedanceCompatAssetRoutes(router)
	routes := make(map[string]bool)
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		"POST /doubao/v2/assets", "POST /v1/seedance/assets/upload",
		"POST /doubao/api/ark/assets", "POST /doubao/v1/ark/asset", "POST /doubao/api/v3/asset", "POST /doubao", "POST /doubao/",
		"GET /doubao/api/a/:id",
		"POST /doubao/api/assets", "POST /doubao/api/assets/upload", "GET /doubao/api/assets/list",
		"GET /doubao/api/assets/groups", "GET /doubao/api/assets/:id", "PUT /doubao/api/assets/:id",
		"GET /doubao/api/assets/:id/download", "GET /doubao/api/assets/:id/public-url",
		"POST /doubao/api/assets/:id/optimize", "GET /doubao/api/assets/liveness/callback",
	} {
		assert.True(t, routes[route], route)
	}
}

func TestSeedanceCompatMySQLAssetGroupAndCleanup(t *testing.T) {
	dsn := os.Getenv("SEEDANCE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("SEEDANCE_TEST_MYSQL_DSN is unset")
	}
	previousDB, previousLog := model.DB, model.LOG_DB
	previousRedis, previousMemory, previousMaster := common.RedisEnabled, common.MemoryCacheEnabled, common.IsMasterNode
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	common.RedisEnabled, common.MemoryCacheEnabled, common.IsMasterNode = false, false, false
	common.SetDatabaseTypes(common.DatabaseTypeMySQL, common.DatabaseTypeMySQL)
	bootstrap, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, bootstrap.AutoMigrate(&model.Option{}))
	bootstrapConnection, err := bootstrap.DB()
	require.NoError(t, err)
	require.NoError(t, bootstrapConnection.Close())
	t.Setenv("SQL_DSN", dsn)
	require.NoError(t, model.InitDB())
	db := model.DB
	model.LOG_DB = db
	connection, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLog
		common.RedisEnabled, common.MemoryCacheEnabled, common.IsMasterNode = previousRedis, previousMemory, previousMaster
		common.SetDatabaseTypes(previousMainType, previousLogType)
		assert.NoError(t, connection.Close())
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{},
		&model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAsset{}, &model.SeedanceAssetSize{},
		&model.SeedanceAssetReplica{}, &model.SeedanceAssetCleanupJob{}, &model.SeedanceAssetValidationSession{},
		&model.SystemTask{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.UserModelPricingHistory{}))
	user := &model.User{Id: 98101, Username: "compatowner", AffCode: "compatowner", Group: "default", Status: common.UserStatusEnabled}
	other := &model.User{Id: 98102, Username: "compatother", AffCode: "compatother", Group: "default", Status: common.UserStatusEnabled}
	require.NoError(t, db.Create([]*model.User{user, other}).Error)
	require.NoError(t, db.Create([]*model.Token{
		{UserId: user.Id, Key: "compatownerkey", UnlimitedQuota: true, ExpiredTime: -1},
		{UserId: other.Id, Key: "compatotherkey", UnlimitedQuota: true, ExpiredTime: -1},
		{UserId: user.Id, Key: "compatrestrictedkey", UnlimitedQuota: true, ExpiredTime: -1,
			ModelLimitsEnabled: true, ModelLimits: "doubao-seedance-2-0-mini-260615"},
		{UserId: user.Id, Key: "compatipkey", UnlimitedQuota: true, ExpiredTime: -1, AllowIps: common.GetPointer("10.0.0.0/8")},
	}).Error)
	var groupSequence atomic.Int32
	var assetSequence atomic.Int32
	var deletedSource atomic.Int32
	var deletedSourceReplica atomic.Int32
	var deletedOptimizedReplica atomic.Int32
	var validationSequence atomic.Int32
	var validationCallback atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("Action") {
		case "CreateAssetGroup":
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":"group-compat-%d"}}`, groupSequence.Add(1))
		case "CreateAsset":
			assetID := "asset-compat-original"
			if sequence := assetSequence.Add(1); sequence > 1 {
				assetID = fmt.Sprintf("asset-compat-%d", sequence)
			}
			_, _ = fmt.Fprintf(w, `{"Result":{"Id":%q}}`, assetID)
		case "GetAsset":
			_, _ = io.WriteString(w, `{"Result":{"Id":"asset-compat-original","Status":"Active"}}`)
		case "CreateVisualValidateSession":
			var body struct {
				CallbackURL string `json:"CallbackURL"`
			}
			assert.NoError(t, common.DecodeJson(r.Body, &body))
			validationCallback.Store(body.CallbackURL)
			if validationSequence.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"Result":{"BytedToken":"fixture-compat-token-ark","H5Link":"https://example.com/verify"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"Result":{"BytedToken":"fixture-compat-token","H5Link":"https://example.com/verify"}}`)
		case "GetVisualValidateResult":
			_, _ = io.WriteString(w, `{"Result":{"GroupId":"group-compat-human"}}`)
		case "GetAssetGroup":
			_, _ = io.WriteString(w, `{"Result":{"Id":"group-compat-human","GroupType":"LivenessFace","Status":"Active"}}`)
		case "DeleteAssetGroup":
			var body struct {
				ID string `json:"Id"`
			}
			assert.NoError(t, common.DecodeJson(r.Body, &body))
			if body.ID == "group-compat-1" {
				deletedSource.Add(1)
			}
			if body.ID == "group-compat-replica" {
				deletedSourceReplica.Add(1)
			}
			if body.ID == "group-compat-3" {
				deletedOptimizedReplica.Add(1)
			}
			_, _ = io.WriteString(w, `{"Result":{}}`)
		default:
			_, _ = io.WriteString(w, `{"Result":{}}`)
		}
	}))
	t.Cleanup(upstream.Close)
	setting := `{"seedance_asset_url":"` + upstream.URL + `"}`
	channel := &model.Channel{Id: 98101, Type: constant.ChannelTypeDoubaoVideo, Key: "fixtureupstream", BaseURL: &upstream.URL,
		Setting: &setting, Models: "doubao-seedance-2-0-fast-260128", Group: "default", Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, channel.AddAbilities(nil))
	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	router.Use(middleware.BodyStorageCleanup())
	setSeedanceAssetAPIRoutes(router)
	setSeedanceAssetValidationActionRoute(router)
	setSeedanceCompatAssetRoutes(router)
	router.NoRoute(seedanceCompatAssetReferenceDispatcher())
	request := func(key, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-"+key)
		req.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		router.ServeHTTP(result, req)
		return result
	}
	for _, name := range []string{"source", "target"} {
		response := request("compatownerkey", http.MethodPost, "/doubao/api/assets/groups", `{"name":"`+name+`","type":"aigc","bind_model":"doubao-seedance-2-0-fast"}`)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"group_type":"aigc"`)
		assert.Contains(t, response.Body.String(), `"bind_model":"doubao-seedance-2-0-fast"`)
	}
	deniedGroup := request("compatrestrictedkey", http.MethodPost, "/doubao/api/assets/groups", `{"name":"denied","bind_model":"doubao-seedance-2-0-fast"}`)
	assert.Equal(t, http.StatusForbidden, deniedGroup.Code)
	created := request("compatownerkey", http.MethodPost, "/doubao/api/assets", `{"group_id":"group-compat-1","url":"https://8.8.8.8/image.png","name":"picture"}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	assert.Contains(t, created.Body.String(), `"assetId":"asset-compat-original"`)
	// 已有记录升级后，新增兼容字段可能为 NULL。
	require.NoError(t, db.Model(&model.SeedanceAssetGroup{}).Where("user_id = ? AND group_id = ?", user.Id, "group-compat-2").
		UpdateColumns(map[string]any{"bind_model": nil, "compat_type": nil}).Error)
	require.NoError(t, db.Model(&model.SeedanceAsset{}).Where("user_id = ? AND asset_id = ?", user.Id, "asset-compat-original").
		UpdateColumns(map[string]any{"primary_upstream_id": nil, "compat_optimize_replica_id": nil}).Error)
	groups := request("compatownerkey", http.MethodGet, "/doubao/api/assets/groups", "")
	require.Equal(t, http.StatusOK, groups.Code, groups.Body.String())
	assert.Contains(t, groups.Body.String(), "group-compat-2")
	encodedID := url.PathEscape("asset://asset-compat-original")
	encodedAsset := request("compatownerkey", http.MethodGet, "/doubao/api/assets/"+encodedID, "")
	require.Equal(t, http.StatusOK, encodedAsset.Code, encodedAsset.Body.String())
	assert.Contains(t, encodedAsset.Body.String(), `"assetId":"asset-compat-original"`)
	assert.Equal(t, http.StatusNotFound, request("compatotherkey", http.MethodGet, "/doubao/api/assets/"+encodedID, "").Code)
	spoofedIP := httptest.NewRequest(http.MethodGet, "/doubao/api/assets/"+encodedID, nil)
	spoofedIP.Header.Set("Authorization", "Bearer sk-compatipkey")
	spoofedIP.Header.Set("X-Forwarded-For", "10.1.2.3")
	spoofedResponse := httptest.NewRecorder()
	router.ServeHTTP(spoofedResponse, spoofedIP)
	assert.Equal(t, http.StatusForbidden, spoofedResponse.Code)
	require.NoError(t, router.SetTrustedProxies([]string{"192.0.2.0/24"}))
	trustedResponse := httptest.NewRecorder()
	router.ServeHTTP(trustedResponse, spoofedIP)
	assert.Equal(t, http.StatusOK, trustedResponse.Code)
	require.NoError(t, router.SetTrustedProxies(nil))
	var sourceGroup model.SeedanceAssetGroup
	require.NoError(t, db.Where("user_id = ? AND group_id = ?", user.Id, "group-compat-1").First(&sourceGroup).Error)
	var originalAsset model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", user.Id, "asset-compat-original").First(&originalAsset).Error)
	copyClient, err := service.NewSeedanceAssetClient(channel)
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.SeedanceAssetGroupReplica{UserID: user.Id, LocalGroupID: sourceGroup.ID,
		ChannelID: channel.Id, AccountFingerprint: copyClient.AccountFingerprint, KeyFingerprint: copyClient.KeyFingerprint,
		UpstreamGroupID: "group-compat-replica", GroupName: sourceGroup.Name, Status: model.SeedanceAssetReplicaReady}).Error)
	require.NoError(t, db.Create(&model.SeedanceAssetReplica{UserID: user.Id, LocalGroupID: sourceGroup.ID,
		LocalAssetID: originalAsset.ID, ChannelID: channel.Id, AccountFingerprint: copyClient.AccountFingerprint,
		KeyFingerprint: copyClient.KeyFingerprint, UpstreamGroupID: "group-compat-replica",
		UpstreamAssetID: "asset-compat-replica", ProvisionStatus: model.SeedanceAssetReplicaReady, Status: "Active"}).Error)
	move := request("compatownerkey", http.MethodPut, "/doubao/api/assets/asset-compat-original", `{"group_id":"group-compat-2","name":"must-not-change"}`)
	assert.Equal(t, http.StatusBadRequest, move.Code, move.Body.String())
	require.NoError(t, db.First(&originalAsset, originalAsset.ID).Error)
	assert.Equal(t, "group-compat-1", originalAsset.GroupID)
	assert.Equal(t, "picture", originalAsset.Name)
	rename := request("compatownerkey", http.MethodPut, "/doubao/api/assets/asset-compat-original", `{"group_id":"group-compat-1","name":"renamed"}`)
	require.Equal(t, http.StatusOK, rename.Code, rename.Body.String())
	assert.Equal(t, http.StatusNotFound, request("compatotherkey", http.MethodGet, "/doubao/api/assets/asset-compat-original", "").Code)
	list := request("compatownerkey", http.MethodGet, "/doubao/api/assets/list?group_id=group-compat-1", "")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	assert.Contains(t, list.Body.String(), `"assetId":"asset-compat-original"`)
	assert.Contains(t, list.Body.String(), `"assetName":"renamed"`)
	tree := request("compatownerkey", http.MethodGet, "/doubao/api/assets/tree", "")
	require.Equal(t, http.StatusOK, tree.Code, tree.Body.String())
	assert.True(t, json.Valid(tree.Body.Bytes()), tree.Body.String())
	removed := request("compatownerkey", http.MethodDelete, "/doubao/api/assets/groups/group-compat-1", "")
	require.Equal(t, http.StatusOK, removed.Code, removed.Body.String())
	_, err = service.RunSeedanceAssetCleanupOnce(t.Context(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, deletedSource.Load())
	assert.EqualValues(t, 1, deletedSourceReplica.Load())
	assert.Equal(t, http.StatusNotFound, request("compatownerkey", http.MethodGet, "/doubao/api/assets/asset-compat-original", "").Code)
	var targetGroup model.SeedanceAssetGroup
	require.NoError(t, db.Where("user_id = ? AND group_id = ?", user.Id, "group-compat-2").First(&targetGroup).Error)
	previousCryptoSecret := common.CryptoSecret
	common.CryptoSecret = "fixture-compat-session-secret"
	t.Setenv("CRYPTO_SECRET", "fixture-compat-session-secret")
	t.Cleanup(func() { common.CryptoSecret = previousCryptoSecret })
	arkStarted := request("compatownerkey", http.MethodPost, "/doubao/api/ark/assets?Action=CreateVisualValidateSession", `{"CallbackURL":"https://example.com/callback"}`)
	require.Equal(t, http.StatusOK, arkStarted.Code, arkStarted.Body.String())
	assert.Contains(t, arkStarted.Body.String(), `"BytedToken":"fixture-compat-token-ark"`)
	started := request("compatownerkey", http.MethodPost, "/doubao/api/assets/liveness/session", `{"model":"doubao-seedance-2-0-fast"}`)
	require.Equal(t, http.StatusOK, started.Code, started.Body.String())
	require.Contains(t, started.Body.String(), `"byted_token":"fixture-compat-token"`)
	callbackURL, err := url.Parse(validationCallback.Load().(string))
	require.NoError(t, err)
	values := callbackURL.Query()
	values.Set("bytedToken", "wrong-token")
	badCallback := httptest.NewRequest(http.MethodGet, callbackURL.Path+"?"+values.Encode(), nil)
	badCallbackResponse := httptest.NewRecorder()
	router.ServeHTTP(badCallbackResponse, badCallback)
	assert.Equal(t, http.StatusNotFound, badCallbackResponse.Code)
	values.Set("bytedToken", "fixture-compat-token")
	values.Set("resultCode", "10000")
	callback := httptest.NewRequest(http.MethodGet, callbackURL.Path+"?"+values.Encode(), nil)
	callbackResponse := httptest.NewRecorder()
	router.ServeHTTP(callbackResponse, callback)
	require.Equal(t, http.StatusOK, callbackResponse.Code, callbackResponse.Body.String())
	arkResult := request("compatownerkey", http.MethodPost, "/doubao/api/ark/assets?Action=GetVisualValidateResult", `{"BytedToken":"fixture-compat-token"}`)
	require.Equal(t, http.StatusOK, arkResult.Code, arkResult.Body.String())
	assert.Contains(t, arkResult.Body.String(), `"GroupId":"group-compat-human"`)
	history := request("compatownerkey", http.MethodGet, "/doubao/api/assets/liveness/history?status=success", "")
	require.Equal(t, http.StatusOK, history.Code, history.Body.String())
	assert.Contains(t, history.Body.String(), `"group_id":"group-compat-human"`)
	var uploadedContent atomic.Value
	ossServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			content, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			uploadedContent.Store(content)
		}
		w.Header().Set("ETag", `"fixture"`)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			if content, ok := uploadedContent.Load().([]byte); ok {
				_, _ = w.Write(content)
			}
		}
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
	fetchSetting := system_setting.GetFetchSetting()
	previousFetchSetting := *fetchSetting
	parsedOSS, err := url.Parse(ossServer.URL)
	require.NoError(t, err)
	fetchSetting.AllowPrivateIp = true
	fetchSetting.AllowedPorts = append(append([]string{}, fetchSetting.AllowedPorts...), parsedOSS.Port())
	t.Cleanup(func() { *fetchSetting = previousFetchSetting })
	service.InitHttpClient()
	var imageBytes bytes.Buffer
	require.NoError(t, png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 300, 300))))
	upload := func(group, mode string, imageData []byte) *httptest.ResponseRecorder {
		var uploadBytes bytes.Buffer
		form := multipart.NewWriter(&uploadBytes)
		require.NoError(t, form.WriteField("group_id", group))
		require.NoError(t, form.WriteField("mode", mode))
		file, err := form.CreateFormFile("file", "picture.png")
		require.NoError(t, err)
		_, err = file.Write(imageData)
		require.NoError(t, err)
		require.NoError(t, form.Close())
		uploadRequest := httptest.NewRequest(http.MethodPost, "/doubao/api/assets/upload", &uploadBytes)
		uploadRequest.Header.Set("Authorization", "Bearer sk-compatownerkey")
		uploadRequest.Header.Set("Content-Type", form.FormDataContentType())
		uploadResponse := httptest.NewRecorder()
		router.ServeHTTP(uploadResponse, uploadRequest)
		return uploadResponse
	}
	uploadResponse := upload("group-compat-2", "link", imageBytes.Bytes())
	require.Equal(t, http.StatusOK, uploadResponse.Code, uploadResponse.Body.String())
	var uploaded struct {
		Data struct {
			ID     string `json:"assetId"`
			Status string `json:"status"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(uploadResponse.Body.Bytes(), &uploaded))
	assert.Equal(t, "ACTIVE", uploaded.Data.Status)
	require.NotEmpty(t, uploaded.Data.ID)
	publicURL := request("compatownerkey", http.MethodGet, "/doubao/api/assets/"+uploaded.Data.ID+"/public-url", "")
	require.Equal(t, http.StatusOK, publicURL.Code, publicURL.Body.String())
	assert.Contains(t, publicURL.Body.String(), `"expires_in":7200`)
	var publicResult struct {
		Data struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(publicURL.Body.Bytes(), &publicResult))
	signedURL, err := url.Parse(publicResult.Data.URL)
	require.NoError(t, err)
	assert.Equal(t, "/doubao/api/a/"+uploaded.Data.ID, signedURL.Path)
	assert.Len(t, signedURL.Query().Get("s"), 32)
	publicRequest := httptest.NewRequest(http.MethodGet, signedURL.RequestURI(), nil)
	publicResponse := httptest.NewRecorder()
	router.ServeHTTP(publicResponse, publicRequest)
	require.Equal(t, http.StatusOK, publicResponse.Code, publicResponse.Body.String())
	assert.Equal(t, imageBytes.Bytes(), publicResponse.Body.Bytes())
	tampered := signedURL.Query()
	tampered.Set("s", strings.Repeat("0", 32))
	tamperedRequest := httptest.NewRequest(http.MethodGet, signedURL.Path+"?"+tampered.Encode(), nil)
	tamperedResponse := httptest.NewRecorder()
	router.ServeHTTP(tamperedResponse, tamperedRequest)
	assert.Equal(t, http.StatusNotFound, tamperedResponse.Code)
	legacyAccess, err := service.IssueTaskArtifactAccess(uploaded.Data.ID, "asset-public:"+signedURL.Query().Get("e"))
	require.NoError(t, err)
	legacyQuery := url.Values{"e": {signedURL.Query().Get("e")}, "access": {legacyAccess}}
	legacyRequest := httptest.NewRequest(http.MethodGet, "/doubao/api/assets/public/"+uploaded.Data.ID+"?"+legacyQuery.Encode(), nil)
	legacyResponse := httptest.NewRecorder()
	router.ServeHTTP(legacyResponse, legacyRequest)
	assert.Equal(t, http.StatusOK, legacyResponse.Code, legacyResponse.Body.String())
	assert.Equal(t, imageBytes.Bytes(), legacyResponse.Body.Bytes())
	expired := signedURL.Query()
	expired.Set("e", "1")
	expiredRequest := httptest.NewRequest(http.MethodGet, signedURL.Path+"?"+expired.Encode(), nil)
	expiredResponse := httptest.NewRecorder()
	router.ServeHTTP(expiredResponse, expiredRequest)
	assert.Equal(t, http.StatusNotFound, expiredResponse.Code)
	mapping, err := service.MapSeedanceAssetsToAccount(t.Context(), user.Id, nil, []string{uploaded.Data.ID})
	require.NoError(t, err)
	assert.Contains(t, mapping[uploaded.Data.ID], "private-assets/")
	videoInputURL, err := url.Parse(mapping[uploaded.Data.ID])
	require.NoError(t, err)
	inputTTL, err := strconv.Atoi(videoInputURL.Query().Get("x-oss-expires"))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, inputTTL, 259200)
	_, err = service.MapSeedanceAssetsToAccount(t.Context(), other.Id, nil, []string{uploaded.Data.ID})
	assert.ErrorIs(t, err, model.ErrSeedanceAssetUnavailable)
	_, err = service.MapSeedanceAssetsToAccount(t.Context(), user.Id, nil, []string{"asset-compat-original"})
	assert.Error(t, err)
	otherSetting := `{"seedance_asset_url":"` + upstream.URL + `/other"}`
	otherChannel := &model.Channel{Id: 98103, Type: constant.ChannelTypeDoubaoVideo, Key: "fixture-other-upstream",
		BaseURL: &upstream.URL, Setting: &otherSetting, Models: channel.Models, Group: "default",
		Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(100))}
	require.NoError(t, db.Create(otherChannel).Error)
	require.NoError(t, otherChannel.AddAbilities(nil))
	deniedOptimize := request("compatrestrictedkey", http.MethodPost, "/doubao/api/assets/"+uploaded.Data.ID+"/optimize", `{"model_family":"doubao-seedance-2-0-fast"}`)
	assert.Equal(t, http.StatusForbidden, deniedOptimize.Code)
	optimized := request("compatownerkey", http.MethodPost, "/doubao/api/assets/"+uploaded.Data.ID+"/optimize", `{"model_family":"doubao-seedance-2-0-fast"}`)
	require.Equal(t, http.StatusOK, optimized.Code, optimized.Body.String())
	assert.Contains(t, optimized.Body.String(), `"status":"processing"`)
	_, err = service.RunSeedanceAssetPollingOnce(t.Context(), nil)
	require.NoError(t, err)
	optimizedAsset := request("compatownerkey", http.MethodGet, "/doubao/api/assets/"+uploaded.Data.ID, "")
	require.Equal(t, http.StatusOK, optimizedAsset.Code, optimizedAsset.Body.String())
	assert.Contains(t, optimizedAsset.Body.String(), `"status":"ACTIVE"`)
	var optimizedRecord model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", user.Id, uploaded.Data.ID).First(&optimizedRecord).Error)
	assert.Equal(t, targetGroup.GroupID, optimizedRecord.GroupID)
	assert.Equal(t, otherChannel.Id, optimizedRecord.ChannelID)
	var optimizedGroup model.SeedanceAssetGroupReplica
	require.NoError(t, db.Where("user_id = ? AND local_group_id = ? AND channel_id = ?", user.Id, targetGroup.ID, otherChannel.Id).First(&optimizedGroup).Error)
	assert.Equal(t, "group-compat-3", optimizedGroup.UpstreamGroupID)
	for _, item := range []struct{ group, mode string }{{"group-compat-2", "library"}, {"group-compat-human", "real"}, {"group-compat-human", ""}} {
		response := upload(item.group, item.mode, imageBytes.Bytes())
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"status":"PROCESSING"`)
	}
	defaultUpload := upload("group-compat-2", "", imageBytes.Bytes())
	require.Equal(t, http.StatusOK, defaultUpload.Code, defaultUpload.Body.String())
	assert.Contains(t, defaultUpload.Body.String(), `"status":"ACTIVE"`)
	assert.NotContains(t, defaultUpload.Body.String(), `"auto_mode"`)
	invalidUpload := upload("group-compat-2", "", []byte("invalid PNG"))
	assert.Equal(t, http.StatusBadRequest, invalidUpload.Code)
	removedTarget := request("compatownerkey", http.MethodDelete, "/doubao/api/assets/groups/group-compat-2", "")
	require.Equal(t, http.StatusOK, removedTarget.Code, removedTarget.Body.String())
	_, err = service.RunSeedanceAssetCleanupOnce(t.Context(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, deletedOptimizedReplica.Load())
	assert.Equal(t, http.StatusNotFound, request("compatownerkey", http.MethodGet, "/doubao/api/assets/"+uploaded.Data.ID, "").Code)

	for _, tc := range []struct{ path, body string }{
		{"/v1/seedance/asset-groups", `{"name":"same group","model":"doubao-seedance-2-0-fast-260128"}`},
		{"/doubao/api/assets/groups", `{"name":"same group","type":"aigc","bind_model":"doubao-seedance-2-0-fast"}`},
		{"/doubao/v2/assets?Action=CreateAssetGroup&Version=2024-01-01", `{"Name":"same group","model":"doubao-seedance-2-0-fast-260128"}`},
	} {
		response := request("compatownerkey", http.MethodPost, tc.path, tc.body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), `"success":false`)
		assert.NotContains(t, response.Body.String(), `"Error"`)
	}
	var sameNameGroups []model.SeedanceAssetGroup
	require.NoError(t, db.Where("user_id = ? AND name = ?", user.Id, "same group").Find(&sameNameGroups).Error)
	require.Len(t, sameNameGroups, 3)
	assert.NotEqual(t, sameNameGroups[0].GroupID, sameNameGroups[1].GroupID)
	assert.NotEqual(t, sameNameGroups[1].GroupID, sameNameGroups[2].GroupID)
	createdForRename := request("compatownerkey", http.MethodPost, "/v1/seedance/asset-groups", `{"name":"rename group","model":"doubao-seedance-2-0-fast-260128"}`)
	require.Equal(t, http.StatusOK, createdForRename.Code, createdForRename.Body.String())
	var renamed model.SeedanceAssetGroup
	require.NoError(t, db.Where("user_id = ? AND name = ?", user.Id, "rename group").First(&renamed).Error)
	response := request("compatownerkey", http.MethodPut, fmt.Sprintf("/v1/seedance/asset-groups/%d", renamed.ID), `{"name":"same group"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"success":true`)
	require.NoError(t, db.First(&renamed, renamed.ID).Error)
	assert.Equal(t, "same group", renamed.Name)
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
			tables := []any{&model.User{}, &model.Token{}, &model.Channel{}, &model.Ability{}, &model.SeedanceAssetGroup{}, &model.SeedanceAssetGroupReplica{}, &model.SeedanceAssetValidationSession{}, &model.SeedanceAsset{}, &model.SeedanceAssetSize{}, &model.SeedanceAssetReplica{}, &model.SeedanceAssetCleanupJob{}, &model.SystemTask{}, &model.UserModelPricing{}, &model.UserModelPricingRevision{}, &model.UserModelPricingHistory{}}
			require.NoError(t, db.AutoMigrate(tables...))
			t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(tables...)) })
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
			setting := `{"seedance_asset_url":"` + upstream.URL + `"}`
			channel.Setting = &setting
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
			var imageData bytes.Buffer
			require.NoError(t, png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 300, 300))))
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(imageData.Bytes())
			}))
			t.Cleanup(source.Close)
			fetchSetting := system_setting.GetFetchSetting()
			previousFetch := *fetchSetting
			for _, endpoint := range []string{source.URL, ossServer.URL} {
				parsed, parseErr := url.Parse(endpoint)
				require.NoError(t, parseErr)
				fetchSetting.AllowedPorts = append(fetchSetting.AllowedPorts, parsed.Port())
			}
			fetchSetting.AllowPrivateIp = true
			t.Cleanup(func() { *fetchSetting = previousFetch })
			service.InitHttpClient()
			registered := request("assetkeytwo", "POST", "/assets", fmt.Sprintf(`{"group_id":"group-api","source_url":%q,"asset_type":"Image","name":"registered"}`, source.URL+"/file.png"))
			require.Contains(t, registered.Body.String(), `"success":true`)
			var registeredAsset struct {
				Data struct {
					ID string `json:"asset_id"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(registered.Body.Bytes(), &registeredAsset))
			require.NotEmpty(t, registeredAsset.Data.ID)
			assert.Contains(t, request("assetkeyone", "GET", "/assets?search="+registeredAsset.Data.ID, "").Body.String(), registeredAsset.Data.ID)
			var uploadBody bytes.Buffer
			form := multipart.NewWriter(&uploadBody)
			require.NoError(t, form.WriteField("group_id", group.GroupID))
			file, err := form.CreateFormFile("file", "fixture.png")
			require.NoError(t, err)
			_, err = file.Write(imageData.Bytes())
			require.NoError(t, err)
			require.NoError(t, form.Close())
			uploadRequest := httptest.NewRequest("POST", "/v1/seedance/assets/upload", &uploadBody)
			uploadRequest.Header.Set("Authorization", "Bearer sk-assetkeytwo")
			uploadRequest.Header.Set("Content-Type", form.FormDataContentType())
			uploadResponse := httptest.NewRecorder()
			router.ServeHTTP(uploadResponse, uploadRequest)
			require.Equal(t, 202, uploadResponse.Code, uploadResponse.Body.String())
			assert.EqualValues(t, 2, uploaded.Load())
			var uploadedAsset struct {
				Data struct {
					ID string `json:"asset_id"`
				} `json:"data"`
			}
			require.NoError(t, common.Unmarshal(uploadResponse.Body.Bytes(), &uploadedAsset))
			require.NotEmpty(t, uploadedAsset.Data.ID)
			// 真人流程也经过生产 TokenAuth、会话持久化、组就绪检查和真实 OSS SDK。
			t.Setenv("CRYPTO_SECRET", "fixture-human-session-secret")
			previousCryptoSecret := common.CryptoSecret
			common.CryptoSecret = "fixture-human-session-secret"
			t.Cleanup(func() { common.CryptoSecret = previousCryptoSecret })
			var humanCallback atomic.Value
			var humanResultCalls atomic.Int32
			var humanGroupUnavailable atomic.Bool
			humanServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer fixture-human-key", r.Header.Get("Authorization"))
				var body map[string]string
				require.NoError(t, common.DecodeJson(r.Body, &body))
				w.Header().Set("Content-Type", "application/json")
				switch body["Action"] {
				case "CreateVisualValidateSession":
					humanCallback.Store(body["CallbackURL"])
					_, _ = io.WriteString(w, `{"Result":{"BytedToken":"fixture-human-token","H5Link":"https://example.com/verify"}}`)
				case "GetVisualValidateResult":
					humanResultCalls.Add(1)
					_, _ = io.WriteString(w, `{"Result":{"GroupId":"human-api-group"}}`)
				case "GetAssetGroup":
					if humanGroupUnavailable.Load() {
						_, _ = io.WriteString(w, `{"ResponseMetadata":{"Error":{"Code":"600502","Message":"RPC call failed"}}}`)
						return
					}
					_, _ = io.WriteString(w, `{"Result":{"Id":"human-api-group","GroupType":"LivenessFace","Status":"Active"}}`)
				case "CreateAsset":
					assert.Equal(t, "human-api-group", body["GroupId"])
					assert.Contains(t, body["URL"], "private-assets/")
					_, _ = io.WriteString(w, `{"Result":{"Id":"human-api-portrait"}}`)
				case "GetAsset":
					_, _ = io.WriteString(w, `{"Result":{"Id":"human-api-portrait","Status":"Active"}}`)
				default:
					t.Errorf("unexpected human Action %s", body["Action"])
				}
			}))
			t.Cleanup(humanServer.Close)
			humanSetting := `{"task_plugin_key":"anyllm","seedance_asset_url":"` + humanServer.URL + `"}`
			humanChannel := &model.Channel{Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, Key: "fixture-human-key", BaseURL: &humanServer.URL, Setting: &humanSetting, Models: channel.Models, Group: "default", Priority: common.GetPointer(int64(100))}
			require.NoError(t, db.Create(humanChannel).Error)
			require.NoError(t, humanChannel.AddAbilities(nil))
			started := request("assetkeytwo", "POST", "/validation-sessions", `{"name":"Human API group","model":"doubao-seedance-2-0-fast-260128","callback_url":"https://example.com/callback?state=client-state&existing=value"}`)
			require.Equal(t, 202, started.Code, started.Body.String())
			var session model.SeedanceAssetValidationSession
			require.NoError(t, db.Where("user_id = ?", users[0].Id).First(&session).Error)
			assert.NotContains(t, session.CompatCallbackURL, "example.com")
			callbackURL, err := url.Parse(humanCallback.Load().(string))
			require.NoError(t, err)
			callbackValues := callbackURL.Query()
			missingToken := httptest.NewRequest(http.MethodGet, callbackURL.Path+"?"+callbackValues.Encode(), nil)
			missingTokenResponse := httptest.NewRecorder()
			router.ServeHTTP(missingTokenResponse, missingToken)
			assert.Equal(t, http.StatusFound, missingTokenResponse.Code)
			assert.EqualValues(t, 1, humanResultCalls.Load())
			callbackValues.Set("bytedToken", "wrong-token")
			badCallback := httptest.NewRequest(http.MethodGet, callbackURL.Path+"?"+callbackValues.Encode(), nil)
			badCallbackResponse := httptest.NewRecorder()
			router.ServeHTTP(badCallbackResponse, badCallback)
			assert.Equal(t, http.StatusNotFound, badCallbackResponse.Code)
			assert.EqualValues(t, 1, humanResultCalls.Load())
			callbackValues.Set("bytedToken", "fixture-human-token")
			callbackValues.Set("resultCode", "10000")
			callbackValues.Set("algorithmBaseRespCode", "0")
			callbackValues.Set("reqMeasureInfoValue", "1")
			callbackValues.Set("verify_type", "real_time")
			callback := httptest.NewRequest(http.MethodGet, callbackURL.Path+"?"+callbackValues.Encode(), nil)
			callbackResponse := httptest.NewRecorder()
			router.ServeHTTP(callbackResponse, callback)
			require.Equal(t, http.StatusFound, callbackResponse.Code, callbackResponse.Body.String())
			redirectURL, err := url.Parse(callbackResponse.Header().Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, "example.com", redirectURL.Hostname())
			assert.Equal(t, "/callback", redirectURL.Path)
			assert.Equal(t, "client-state", redirectURL.Query().Get("state"))
			assert.Equal(t, "value", redirectURL.Query().Get("existing"))
			assert.Equal(t, "fixture-human-token", redirectURL.Query().Get("bytedToken"))
			assert.Equal(t, "10000", redirectURL.Query().Get("resultCode"))
			assert.Equal(t, "0", redirectURL.Query().Get("algorithmBaseRespCode"))
			assert.Equal(t, "1", redirectURL.Query().Get("reqMeasureInfoValue"))
			assert.Equal(t, "real_time", redirectURL.Query().Get("verify_type"))
			assert.Equal(t, "no-referrer", callbackResponse.Header().Get("Referrer-Policy"))
			var verifiedGroup model.SeedanceAssetGroup
			require.NoError(t, db.Where("user_id = ? AND group_id = ?", users[0].Id, "human-api-group").First(&verifiedGroup).Error)
			humanGroupUnavailable.Store(true)
			temporaryFailure := httptest.NewRecorder()
			router.ServeHTTP(temporaryFailure, callback)
			assert.Equal(t, http.StatusServiceUnavailable, temporaryFailure.Code)
			humanGroupUnavailable.Store(false)
			verified := request("assetkeyone", "GET", fmt.Sprintf("/validation-sessions/%d", session.ID), "")
			require.Contains(t, verified.Body.String(), `"status":"Succeeded"`)
			var portraitBody bytes.Buffer
			portraitForm := multipart.NewWriter(&portraitBody)
			require.NoError(t, portraitForm.WriteField("group_id", "human-api-group"))
			portraitPart, err := portraitForm.CreateFormFile("file", "portrait.png")
			require.NoError(t, err)
			_, err = portraitPart.Write(imageData.Bytes())
			require.NoError(t, err)
			require.NoError(t, portraitForm.Close())
			portraitRequest := httptest.NewRequest("POST", "/v1/seedance/assets/upload", &portraitBody)
			portraitRequest.Header.Set("Authorization", "Bearer sk-assetkeytwo")
			portraitRequest.Header.Set("Content-Type", portraitForm.FormDataContentType())
			portraitResponse := httptest.NewRecorder()
			router.ServeHTTP(portraitResponse, portraitRequest)
			require.Equal(t, 202, portraitResponse.Code, portraitResponse.Body.String())
			assert.EqualValues(t, 3, uploaded.Load())
			assert.Contains(t, request("assetkeyone", "GET", "/assets?search=human-api-portrait", "").Body.String(), "human-api-portrait")
			assert.NotContains(t, request("assetkeyother", "GET", "/assets?search=human-api-portrait", "").Body.String(), "human-api-portrait")
			// 后面的 Action 兼容用例继续使用原渠道。
			require.NoError(t, db.Model(humanChannel).Update("status", common.ChannelStatusManuallyDisabled).Error)
			assert.Contains(t, request("assetkeyone", "GET", "/assets?search="+uploadedAsset.Data.ID, "").Body.String(), uploadedAsset.Data.ID)
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
			verifySeedanceAssetActions(t, db, router, users[0].Id+20, users[1].Id, source.URL)
		})
	}
}

// verifySeedanceAssetActions 通过生产路由覆盖 Action 全链路，并与原 REST 接口交叉读取。
func verifySeedanceAssetActions(t *testing.T, db *gorm.DB, router *gin.Engine, owner, outsider int, sourceURL string) {
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
	compatRequest := func(key, action, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/doubao/api/ark/assets?Action="+action, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-"+key)
		req.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		router.ServeHTTP(result, req)
		return result
	}
	compatGroup := compatRequest("assetactionone", "GetAssetGroup", fmt.Sprintf(`{"Id":%q,"ProjectName":"default"}`, groupID))
	require.Equal(t, http.StatusOK, compatGroup.Code, compatGroup.Body.String())
	assert.Contains(t, compatGroup.Body.String(), groupID)
	assert.Equal(t, http.StatusNotFound, compatRequest("assetkeyother", "GetAssetGroup", groupBody).Code)
	assert.Contains(t, request("assetactionone", "GetAssetGroup", groupBody).Body.String(), `"Description":"references"`)
	assert.Contains(t, request("assetactionone", "ListAssetGroups", `{}`).Body.String(), groupID)
	updatedGroup := request("assetactiontwo", "UpdateAssetGroup", fmt.Sprintf(`{"Id":%q,"Description":"changed"}`, groupID))
	require.Equal(t, http.StatusOK, updatedGroup.Code, updatedGroup.Body.String())
	assert.Contains(t, request("assetactionone", "GetAssetGroup", groupBody).Body.String(), `"Description":"changed"`)
	registered := request("assetactiontwo", "CreateAsset", fmt.Sprintf(`{"GroupId":%q,"URL":%q,"AssetType":"Image","Name":"reference"}`, groupID, sourceURL+"/reference.png"))
	require.Equal(t, http.StatusOK, registered.Code, registered.Body.String())
	require.NoError(t, common.Unmarshal(registered.Body.Bytes(), &createdResponse))
	assetID := createdResponse.Result.ID
	require.NotEmpty(t, assetID)
	assetBody := fmt.Sprintf(`{"Id":%q}`, assetID)
	compatAsset := compatRequest("assetactionone", "GetAsset", fmt.Sprintf(`{"Id":%q,"ProjectName":"default"}`, assetID))
	require.Equal(t, http.StatusOK, compatAsset.Code, compatAsset.Body.String())
	assert.Contains(t, compatAsset.Body.String(), assetID)
	assert.Equal(t, http.StatusNotFound, compatRequest("assetkeyother", "GetAsset", assetBody).Code)
	pending := request("assetactionone", "GetAsset", assetBody)
	require.Equal(t, http.StatusOK, pending.Code, pending.Body.String())
	assert.Contains(t, pending.Body.String(), `"Status":"Processing"`)
	assert.Contains(t, pending.Body.String(), `"URL":""`)
	var asset model.SeedanceAsset
	require.NoError(t, db.Where("user_id = ? AND asset_id = ?", owner, assetID).First(&asset).Error)
	// 只模拟上游审核终态；读取仍经过平台真实查询、归属校验与响应转换。
	require.NoError(t, db.Model(&model.SeedanceAssetReplica{}).Where("user_id = ? AND local_asset_id = ?", owner, asset.ID).Updates(map[string]any{
		"provision_status": model.SeedanceAssetReplicaReady, "status": "Active", "upstream_asset_id": "asset-action-ready", "next_poll_at": 0,
	}).Error)
	require.NoError(t, model.RefreshReplicaManagedSeedanceAsset(t.Context(), owner, asset.ID))
	active := request("assetactionone", "GetAsset", assetBody)
	assert.Contains(t, active.Body.String(), "private-assets/")
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
		{"CreateAsset", fmt.Sprintf(`{"GroupId":%q,"URL":"file:///private","AssetType":"Image"}`, groupID)},
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
	// 素材副本、OSS 对象和素材组各有一条清理任务。
	assert.EqualValues(t, 3, jobs)
	var stored model.Token
	require.NoError(t, db.First(&stored, tokens[0].Id).Error)
	assert.Zero(t, stored.UsedQuota)

	compatCreated := compatRequest("assetactiontwo", "CreateAssetGroup", `{"Name":"Action compat","ProjectName":"default"}`)
	require.Equal(t, http.StatusOK, compatCreated.Code, compatCreated.Body.String())
	require.NoError(t, common.Unmarshal(compatCreated.Body.Bytes(), &createdResponse))
	compatGroupID := createdResponse.Result.ID
	require.NotEmpty(t, compatGroupID)
	compatUpdateGroup := compatRequest("assetactiontwo", "UpdateAssetGroup", fmt.Sprintf(`{"Id":%q,"Description":"compat description"}`, compatGroupID))
	require.Equal(t, http.StatusOK, compatUpdateGroup.Code, compatUpdateGroup.Body.String())
	compatRegistered := compatRequest("assetactiontwo", "CreateAsset", fmt.Sprintf(`{"GroupId":%q,"URL":%q,"AssetType":"Image","ProjectName":"default"}`, compatGroupID, sourceURL+"/compat.png"))
	require.Equal(t, http.StatusOK, compatRegistered.Code, compatRegistered.Body.String())
	require.NoError(t, common.Unmarshal(compatRegistered.Body.Bytes(), &createdResponse))
	compatAssetID := createdResponse.Result.ID
	require.NotEmpty(t, compatAssetID)
	compatRegisteredAgain := compatRequest("assetactiontwo", "CreateAsset", fmt.Sprintf(`{"GroupId":%q,"URL":%q,"AssetType":"Image"}`, compatGroupID, sourceURL+"/compat-2.png"))
	require.Equal(t, http.StatusOK, compatRegisteredAgain.Code, compatRegisteredAgain.Body.String())
	require.NoError(t, common.Unmarshal(compatRegisteredAgain.Body.Bytes(), &createdResponse))
	compatOtherAssetID := createdResponse.Result.ID
	require.NotEmpty(t, compatOtherAssetID)
	compatUpdate := compatRequest("assetactiontwo", "UpdateAsset", fmt.Sprintf(`{"Id":%q,"Name":"compat renamed"}`, compatAssetID))
	require.Equal(t, http.StatusOK, compatUpdate.Code, compatUpdate.Body.String())
	compatDelete := compatRequest("assetactiontwo", "DeleteAsset", fmt.Sprintf(`{"Id":%q}`, compatAssetID))
	require.Equal(t, http.StatusOK, compatDelete.Code, compatDelete.Body.String())
	assert.Equal(t, http.StatusNotFound, compatRequest("assetactiontwo", "GetAsset", fmt.Sprintf(`{"Id":%q}`, compatAssetID)).Code)
	compatDeleteGroup := compatRequest("assetactiontwo", "DeleteAssetGroup", fmt.Sprintf(`{"Id":%q}`, compatGroupID))
	require.Equal(t, http.StatusOK, compatDeleteGroup.Code, compatDeleteGroup.Body.String())
	assert.Equal(t, http.StatusNotFound, compatRequest("assetactiontwo", "GetAssetGroup", fmt.Sprintf(`{"Id":%q}`, compatGroupID)).Code)
	assert.Equal(t, http.StatusNotFound, compatRequest("assetactiontwo", "GetAsset", fmt.Sprintf(`{"Id":%q}`, compatOtherAssetID)).Code)
	assert.NotContains(t, compatRequest("assetactiontwo", "ListAssets", `{}`).Body.String(), compatOtherAssetID)
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
