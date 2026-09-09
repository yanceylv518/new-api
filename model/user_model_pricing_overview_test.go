package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 摘要与分页明细必须使用相同搜索权限，且大于一页时不漏项、不混入其他用户。
func TestUserModelPricingSummaryAndRulePages(t *testing.T) {
	setupUserUpdateTestState(t)
	user := User{Id: 71, Username: "paged-user", AffCode: "paged", Role: common.RoleCommonUser}
	require.NoError(t, DB.Create(&user).Error)
	for i := 0; i < 25; i++ {
		require.NoError(t, DB.Create(&UserModelPricing{UserId: user.Id, ModelName: fmt.Sprintf("match-%02d", i), DiscountBPS: 5000 + i}).Error)
	}
	require.NoError(t, DB.Create(&UserModelPricing{UserId: user.Id, ModelName: "excluded", DiscountBPS: 8000}).Error)
	result, err := GetUserModelPricingOverview(t.Context(), "match-", common.RoleRootUser, 0, 20, true)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Empty(t, result.Items[0].Rules)
	assert.Equal(t, 25, result.Items[0].RuleCount)
	assert.Equal(t, 5000, result.Items[0].MinDiscountBPS)
	assert.Equal(t, 5024, result.Items[0].MaxDiscountBPS)
	first, total, err := GetUserModelPricingRulePage(t.Context(), user.Id, common.RoleAdminUser, "match-", 1)
	require.NoError(t, err)
	assert.Equal(t, int64(25), total)
	require.Len(t, first, 20)
	second, _, err := GetUserModelPricingRulePage(t.Context(), user.Id, common.RoleAdminUser, "match-", 2)
	require.NoError(t, err)
	require.Len(t, second, 5)
	seen := map[string]bool{}
	for _, rule := range append(first, second...) {
		assert.NotEqual(t, "excluded", rule.ModelName)
		assert.False(t, seen[rule.ModelName])
		seen[rule.ModelName] = true
	}
	denied, count, err := GetUserModelPricingRulePage(t.Context(), user.Id, common.RoleCommonUser, "", 1)
	require.NoError(t, err)
	assert.Empty(t, denied)
	assert.Zero(t, count)
	_, _, err = GetUserModelPricingRulePage(t.Context(), user.Id, common.RoleRootUser, "", -1)
	assert.Error(t, err)
}

// 使用固定的 10 万条规则比较同一数据库下完整响应与摘要响应，包含 JSON 序列化成本。
func BenchmarkUserModelPricingOverview(b *testing.B) {
	require.NoError(b, DB.Exec("DELETE FROM user_model_pricings").Error)
	require.NoError(b, DB.Exec("DELETE FROM users").Error)
	b.Cleanup(func() {
		DB.Exec("DELETE FROM user_model_pricings")
		DB.Exec("DELETE FROM users")
	})
	for u := 1; u <= 100; u++ {
		user := User{Id: u, Username: fmt.Sprintf("bench-%d", u), AffCode: fmt.Sprintf("bench-%d", u), Role: common.RoleCommonUser}
		require.NoError(b, DB.Create(&user).Error)
		rules := make([]UserModelPricing, 1000)
		for i := range rules {
			rules[i] = UserModelPricing{UserId: u, ModelName: fmt.Sprintf("model-%04d", i), DiscountBPS: 7500}
		}
		require.NoError(b, DB.CreateInBatches(&rules, 100).Error)
	}
	for _, summary := range []bool{false, true} {
		b.Run(fmt.Sprintf("summary=%v", summary), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				result, err := GetUserModelPricingOverview(b.Context(), "", common.RoleRootUser, 0, 20, summary)
				require.NoError(b, err)
				require.Equal(b, int64(100000), result.TotalRules)
				encoded, err := common.Marshal(result)
				require.NoError(b, err)
				b.ReportMetric(float64(len(encoded)), "response-bytes")
			}
		})
	}
	b.Run("detail-page", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			items, total, err := GetUserModelPricingRulePage(b.Context(), 1, common.RoleRootUser, "", 1)
			require.NoError(b, err)
			require.Equal(b, int64(1000), total)
			require.Len(b, items, 20)
		}
	})
}

// 总览必须按用户聚合规则，并只返回当前管理员能够管理的未删除账号。
func TestGetUserModelPricingOverviewGroupsRulesAndAppliesRoleBoundary(t *testing.T) {
	setupUserUpdateTestState(t)

	visibleUser := User{
		Id: 1, Username: "visible-discount-user", DisplayName: "Visible User",
		Email: "visible@example.com", Group: "default", AffCode: "overview-visible",
		Role:   common.RoleCommonUser,
		Status: common.UserStatusEnabled,
	}
	adminUser := User{
		Id: 2, Username: "admin-discount-user", Group: "default", AffCode: "overview-admin",
		Role: common.RoleAdminUser, Status: common.UserStatusEnabled,
	}
	deletedUser := User{
		Id: 3, Username: "deleted-discount-user", Group: "default", AffCode: "overview-deleted",
		Role: common.RoleCommonUser, Status: common.UserStatusEnabled,
	}
	for _, user := range []*User{&visibleUser, &adminUser, &deletedUser} {
		require.NoError(t, DB.Create(user).Error)
	}
	for _, rule := range []UserModelPricing{
		{UserId: visibleUser.Id, ModelName: "gpt-4o", DiscountBPS: 8000},
		{UserId: visibleUser.Id, ModelName: "doubao-video", DiscountBPS: 6500},
		{UserId: adminUser.Id, ModelName: "admin-only-model", DiscountBPS: 7000},
		{UserId: deletedUser.Id, ModelName: "deleted-only-model", DiscountBPS: 7000},
	} {
		require.NoError(t, DB.Create(&rule).Error)
	}
	require.NoError(t, DB.Delete(&deletedUser).Error)

	result, err := GetUserModelPricingOverview(t.Context(), "", common.RoleAdminUser, 0, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.TotalUsers)
	assert.Equal(t, int64(2), result.TotalRules)
	assert.Equal(t, int64(2), result.TotalModels)
	require.Len(t, result.Items, 1)
	assert.Equal(t, visibleUser.Id, result.Items[0].User.Id)
	assert.Equal(t, []UserModelPricingOverviewRule{
		{ModelName: "doubao-video", DiscountBPS: 6500},
		{ModelName: "gpt-4o", DiscountBPS: 8000},
	}, result.Items[0].Rules)
	assert.Equal(t, 2, result.Items[0].RuleCount)
}

// 搜索模型时仍按匹配用户分页，并保持统计值只覆盖当前搜索结果。
func TestGetUserModelPricingOverviewSearchesUsersAndModels(t *testing.T) {
	setupUserUpdateTestState(t)

	users := []*User{
		{Id: 11, Username: "alpha-user", Group: "default", AffCode: "overview-alpha", Role: common.RoleCommonUser, Status: common.UserStatusEnabled},
		{Id: 12, Username: "beta-user", Group: "default", AffCode: "overview-beta", Role: common.RoleCommonUser, Status: common.UserStatusDisabled},
	}
	for _, user := range users {
		require.NoError(t, DB.Create(user).Error)
	}
	rules := []UserModelPricing{
		{UserId: users[0].Id, ModelName: "gpt-4o", DiscountBPS: 8000},
		{UserId: users[0].Id, ModelName: "claude-3-7", DiscountBPS: 9000},
		{UserId: users[1].Id, ModelName: "gpt-4o", DiscountBPS: 7000},
	}
	for _, rule := range rules {
		require.NoError(t, DB.Create(&rule).Error)
	}

	result, err := GetUserModelPricingOverview(t.Context(), "claude", common.RoleRootUser, 0, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.TotalUsers)
	assert.Equal(t, int64(1), result.TotalRules)
	assert.Equal(t, int64(1), result.TotalModels)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "alpha-user", result.Items[0].User.Username)
	assert.Equal(t, []UserModelPricingOverviewRule{
		{ModelName: "claude-3-7", DiscountBPS: 9000},
	}, result.Items[0].Rules)

	// 同名模型属于不同用户时不能跨用户去重，且分页统计只计算匹配规则。
	result, err = GetUserModelPricingOverview(t.Context(), "gpt-4o", common.RoleRootUser, 0, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.TotalUsers)
	assert.Equal(t, int64(2), result.TotalRules)
	assert.Equal(t, int64(1), result.TotalModels)
	require.Len(t, result.Items, 2)
	for i, discount := range []int{8000, 7000} {
		assert.Equal(t, users[i].Id, result.Items[i].User.Id)
		assert.Equal(t, []UserModelPricingOverviewRule{{ModelName: "gpt-4o", DiscountBPS: discount}}, result.Items[i].Rules)
		assert.Equal(t, 1, result.Items[i].RuleCount)
	}
	page, err := GetUserModelPricingOverview(t.Context(), "gpt-4o", common.RoleRootUser, 1, 1)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, result.Items[1], page.Items[0])
	assert.Equal(t, int64(2), page.TotalUsers)

	// 用户搜索仍可查看该用户的所有规则；无匹配时返回空集合。
	result, err = GetUserModelPricingOverview(t.Context(), "alpha-user", common.RoleRootUser, 0, 20)
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Len(t, result.Items[0].Rules, 2)
	result, err = GetUserModelPricingOverview(t.Context(), "missing-model", common.RoleRootUser, 0, 20)
	require.NoError(t, err)
	assert.Empty(t, result.Items)
	assert.Zero(t, result.TotalUsers)
	assert.Zero(t, result.TotalRules)
	assert.Zero(t, result.TotalModels)
}
