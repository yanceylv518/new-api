package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelResourcePolicyAdminAPI(t *testing.T) {
	previousDB := model.DB
	setupTokenResourceBodyTest(t)
	previousMemory := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemory })
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.ChannelResourcePolicy{}, &model.CasbinRule{}, &model.AuthzRole{}))
	common.IsMasterNode = true
	require.NoError(t, authz.Init(model.DB))
	t.Cleanup(func() {
		if previousDB != nil && previousDB.Migrator().HasTable(&model.CasbinRule{}) {
			require.NoError(t, authz.Init(previousDB))
		}
	})
	channel := model.Channel{Name: "managed", Key: "must-not-leak", Status: common.ChannelStatusEnabled}
	require.NoError(t, model.DB.Create(&channel).Error)
	credentials := map[int]string{}
	actors := map[int]model.User{}
	for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser, common.RoleRootUser} {
		credential := fmt.Sprintf("channel-policy-actor-%d", role)
		user := model.User{Username: credential, AffCode: credential, Role: role, Status: common.UserStatusEnabled, AccessToken: &credential}
		require.NoError(t, model.DB.Create(&user).Error)
		credentials[role], actors[role] = credential, user
	}
	engine := gin.New()
	SetApiRouter(engine)
	path := fmt.Sprintf("/api/channel/%d/resource-policy", channel.Id)
	request := func(method, url, credential, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		return rec
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		assert.Equal(t, 401, request(method, path, "", `{"max_request_body_bytes":1024}`).Code)
		assert.Equal(t, 403, request(method, path, credentials[common.RoleCommonUser], `{"max_request_body_bytes":1024}`).Code)
	}
	admin := credentials[common.RoleAdminUser]
	initial := request(http.MethodGet, path, admin, "")
	require.Equal(t, 200, initial.Code, initial.Body.String())
	assert.Contains(t, initial.Body.String(), `"max_request_body_bytes":0`)
	assert.Equal(t, "no-store", initial.Header().Get("Cache-Control"))
	assert.NotContains(t, initial.Body.String(), channel.Key)
	for _, role := range []int{common.RoleAdminUser, common.RoleRootUser} {
		rec := request(http.MethodPut, path, credentials[role], `{"max_request_body_bytes":1572864}`)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		policy, err := model.GetChannelResourcePolicy(context.Background(), channel.Id)
		require.NoError(t, err)
		require.NotNil(t, policy)
		assert.Equal(t, int64(1572864), policy.MaxRequestBodyBytes)
	}
	require.NoError(t, authz.SetUserPermissions(actors[common.RoleAdminUser].Id, authz.PermissionsMap{"channel": {"read": true, "write": false}}))
	assert.Equal(t, 200, request(http.MethodGet, path, admin, "").Code)
	assert.Equal(t, 403, request(http.MethodPut, path, admin, `{"max_request_body_bytes":0}`).Code)
	require.NoError(t, authz.SetUserPermissions(actors[common.RoleAdminUser].Id, authz.PermissionsMap{"channel": {"read": false, "write": false}}))
	assert.Equal(t, 403, request(http.MethodGet, path, admin, "").Code)
	root := credentials[common.RoleRootUser]
	for _, body := range []string{`{}`, `null`, `{"max_request_body_bytes":null}`, `{"max_request_body_bytes":-1}`, `{"max_request_body_bytes":1.5}`, `{"max_request_body_bytes":"30"}`, `{"max_request_body_bytes":9007199254740992}`, `{"max_request_body_bytes":0,"channel_id":1}`, `{"max_request_body_bytes":0}{}`, strings.Repeat("x", 4097)} {
		assert.Equal(t, 400, request(http.MethodPut, path, root, body).Code, body[:min(len(body), 100)])
	}
	for _, id := range []string{"0", "-1", "invalid"} {
		assert.Equal(t, 400, request(http.MethodGet, "/api/channel/"+id+"/resource-policy", root, "").Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		assert.Equal(t, 404, request(method, "/api/channel/99999/resource-policy", root, `{"max_request_body_bytes":0}`).Code)
	}
	assert.Equal(t, 200, request(http.MethodPut, path, root, `{"max_request_body_bytes":0}`).Code)
	policy, err := model.GetChannelResourcePolicy(context.Background(), channel.Id)
	require.NoError(t, err)
	assert.Nil(t, policy)
	// Mixed-version/migration failure must not appear as a successful unlimited setting.
	require.NoError(t, model.DB.Migrator().DropTable(&model.ChannelResourcePolicy{}))
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		rec := request(method, path, root, `{"max_request_body_bytes":1024}`)
		assert.Equal(t, 503, rec.Code)
		assert.NotContains(t, rec.Body.String(), `"max_request_body_bytes":0`)
	}
}

func TestChannelStatusRoutesUseOperatePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodPost, "/:id/status", authz.ChannelOperate, controller.UpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPost, "/status/batch", authz.ChannelOperate, controller.BatchUpdateChannelStatus)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
}

func TestChannelDeleteRoutesUseSensitiveWritePermission(t *testing.T) {
	assertChannelRoutePermission(t, http.MethodDelete, "/:id", authz.ChannelSensitiveWrite, controller.DeleteChannel)
	assertChannelRoutePermission(t, http.MethodPost, "/batch", authz.ChannelSensitiveWrite, controller.DeleteChannelBatch)
	assertChannelRoutePermission(t, http.MethodDelete, "/disabled", authz.ChannelSensitiveWrite, controller.DeleteDisabledChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/", authz.ChannelWrite, controller.UpdateChannel)
	assertChannelRoutePermission(t, http.MethodPut, "/tag", authz.ChannelWrite, controller.EditTagChannels)
	assertChannelRoutePermission(t, http.MethodPost, "/batch/tag", authz.ChannelWrite, controller.BatchSetChannelTag)
}

func TestChannelStatusRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")

	require.NotPanics(t, func() {
		registerChannelRoutes(api)
	})
}

func assertChannelRoutePermission(t *testing.T, method string, path string, permission authz.Permission, handler any) {
	t.Helper()
	for _, route := range channelPermissionRoutes {
		if route.method == method && route.path == path {
			assert.Equal(t, permission, route.permission)
			assert.Equal(t, reflect.ValueOf(handler).Pointer(), reflect.ValueOf(route.handler).Pointer())
			return
		}
	}
	t.Fatalf("route %s %s not found", method, path)
}
