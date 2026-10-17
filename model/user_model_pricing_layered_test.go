package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserModelPricingPermanentFallbackValidation(t *testing.T) {
	start, end := int64(100), int64(200)
	item := UserModelPricingItem{ModelName: "model", Mode: "layered", Permanent: &UserModelPricingPeriod{DiscountBPS: 7500}, Periods: []UserModelPricingPeriod{{DiscountBPS: 10000, StartTime: &start, EndTime: &end, Expression: `(hour("Asia/Shanghai") < 9) ? 0.6 : permanent_discount`}}}
	config, err := normalizeLayeredUserModelPricing(item, hosttypes.UserModelDiscountConfig{}, 100)
	require.NoError(t, err)
	assert.Equal(t, 7500, config.Permanent.DiscountBPS)
	assert.Equal(t, item.Periods[0].Expression, config.Periods[0].Expression)
	item.Permanent.Expression = "permanent_discount"
	_, err = normalizeLayeredUserModelPricing(item, hosttypes.UserModelDiscountConfig{}, 100)
	require.Error(t, err)
	item.Permanent.Expression = "0.8"
	_, err = normalizeLayeredUserModelPricing(item, hosttypes.UserModelDiscountConfig{}, 100)
	require.Error(t, err)
	item.Permanent = nil
	_, err = normalizeLayeredUserModelPricing(item, hosttypes.UserModelDiscountConfig{}, 100)
	require.NoError(t, err)
}

func TestUserModelPricingLayeredPersistence(t *testing.T) {
	setupUserModelPricingCacheTest(t)
	useUserCacheMiniRedis(t)
	assertUserModelPricingLayeredPersistence(t)
}

func assertUserModelPricingLayeredPersistence(t *testing.T) {
	t.Helper()
	user := User{Username: "layered-persistence", AffCode: "layered-persistence", Role: common.RoleCommonUser, Status: common.UserStatusEnabled}
	actor := User{Username: "layered-actor", AffCode: "layered-actor", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
	require.NoError(t, DB.Create(&actor).Error)
	defer DB.Unscoped().Delete(&User{}, actor.Id)
	require.NoError(t, DB.Create(&user).Error)
	defer DB.Unscoped().Delete(&User{}, user.Id)
	defer DB.Where("user_id = ?", user.Id).Delete(&UserModelPricing{})
	defer DB.Where("user_id = ?", user.Id).Delete(&UserModelPricingRevision{})
	defer DB.Where("user_id = ?", user.Id).Delete(&UserModelPricingHistory{})
	now := time.Now().Unix()
	start, end, next, nextEnd := now-10, now+10, now+20, now+30
	items := []UserModelPricingItem{{ModelName: "layered-model", Mode: "layered", Permanent: &UserModelPricingPeriod{DiscountBPS: 8000}, Periods: []UserModelPricingPeriod{{DiscountBPS: 5000, StartTime: &start, EndTime: &end}, {DiscountBPS: 10000, StartTime: &next, EndTime: &nextEnd}}}}
	revision, err := ReplaceUserModelPricingSchedules(t.Context(), user.Id, items, 1, actor.Id)
	require.NoError(t, err)
	frozen, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	for _, tc := range []struct {
		at  int64
		bps int
	}{{start - 1, 8000}, {start, 5000}, {end - 1, 5000}, {end, 8000}, {next, 10000}, {nextEnd, 8000}} {
		assert.Equal(t, tc.bps, frozen.DiscountBPSAt("layered-model", tc.at))
	}
	require.NoError(t, DB.AutoMigrate(&Ability{}))
	ability := Ability{Group: "default", Model: "layered-model", ChannelId: 987654, Enabled: true}
	require.NoError(t, DB.Create(&ability).Error)
	defer DB.Where("channel_id = ?", ability.ChannelId).Delete(&Ability{})
	for _, tc := range []struct {
		status string
		total  int64
		slot   int
	}{{"active", 1, 0}, {"pending", 1, 1}, {"expired", 0, 0}, {"", 3, -1}} {
		rules, total, err := GetUserModelPricingRulePage(t.Context(), user.Id, common.RoleRootUser, "", 1, tc.status)
		require.NoError(t, err)
		assert.Equal(t, tc.total, total)
		if len(rules) > 0 {
			assert.Equal(t, tc.slot, rules[0].Slot)
		}
	}
	var rows []UserModelPricing
	require.NoError(t, DB.Where("user_id = ?", user.Id).Order("slot").Find(&rows).Error)
	require.Len(t, rows, 3)
	assert.Equal(t, -1, rows[0].Slot)
	items[0].Permanent.Expression = "output_tokens >= 500 ? 0.6 : 0.8"
	items[0].Periods[0].Expression = "0.5"
	revision, err = ReplaceUserModelPricingSchedules(t.Context(), user.Id, items, revision, actor.Id)
	require.NoError(t, err)
	fresh, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	rule, _ := fresh.RuleAt("layered-model", end)
	assert.Equal(t, items[0].Permanent.Expression, rule.Expression)
	assert.Equal(t, 8000, frozen.DiscountBPSAt("layered-model", end))
	got, gotRevision, err := GetUserModelPricingRulesContext(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, revision, gotRevision)
	require.Len(t, got, 1)
	assert.Equal(t, "layered", got[0].Mode)
	assert.Equal(t, items[0].Permanent.Expression, got[0].Permanent.Expression)
	_, err = ReplaceUserModelPricingSchedules(t.Context(), user.Id, items, revision-1, actor.Id)
	require.ErrorIs(t, err, ErrUserModelPricingRevisionConflict)
	invalid := []UserModelPricingItem{{ModelName: "layered-model", Mode: "layered", Permanent: items[0].Permanent, Periods: []UserModelPricingPeriod{{DiscountBPS: 5000, StartTime: &start}}}}
	_, err = ReplaceUserModelPricingSchedules(t.Context(), user.Id, invalid, revision, actor.Id)
	require.Error(t, err)
	_, sameRevision, err := GetUserModelPricingRulesContext(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, revision, sameRevision)
	_, err = ReplaceUserModelPricingSchedules(t.Context(), user.Id, nil, revision, actor.Id)
	require.NoError(t, err)
	cleared, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Zero(t, cleared.DiscountBPSAt("layered-model", 100))
	history, total, err := GetUserModelPricingHistory(t.Context(), user.Id, common.RoleRootUser, 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total)
	require.Len(t, history, 3)
}
