package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 目录属于用户管理能力，必须独立于操作者的消费分组，并继续校验目标用户管理权限。
func TestUserModelPricingIncludesModelsOutsideOperatorGroup(t *testing.T) {
	db := setupManageUserTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.UserModelPricing{}, &model.Channel{}, &model.Ability{}, &model.Model{}, &model.Vendor{}))
	user := model.User{Id: 42, Username: "pricing-target", Role: common.RoleCommonUser, Group: "vip"}
	require.NoError(t, db.Create(&user).Error)
	channel := model.Channel{Id: 1, Key: "fixture-key", Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled}
	require.NoError(t, db.Create(&channel).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "vip", Model: "vip-model", ChannelId: channel.Id, Enabled: true}).Error)
	require.NoError(t, db.Create(&model.Ability{Group: "disabled", Model: "disabled-channel-model", ChannelId: channel.Id, Enabled: false}).Error)
	require.NoError(t, db.Create(&model.Model{ModelName: "catalog-only-model", NameRule: model.NameRuleExact}).Error)
	for _, role := range []int{common.RoleRootUser, common.RoleCommonUser} {
		response := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(response)
		c.Set("id", 1)
		c.Set("role", role)
		c.Set("group", "default")
		c.Params = gin.Params{{Key: "id", Value: "42"}}
		c.Request = httptest.NewRequest(http.MethodGet, "/api/user/42/model-pricing", nil)
		GetUserModelPricing(c)
		var payload struct {
			Success bool `json:"success"`
			Data    struct {
				ModelNames []string `json:"model_names"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &payload))
		if role == common.RoleRootUser {
			assert.True(t, payload.Success)
			assert.Contains(t, payload.Data.ModelNames, "vip-model")
			assert.NotContains(t, payload.Data.ModelNames, "disabled-channel-model")
			assert.NotContains(t, payload.Data.ModelNames, "catalog-only-model")
		} else {
			assert.False(t, payload.Success)
			assert.Empty(t, payload.Data.ModelNames)
		}
	}
}
