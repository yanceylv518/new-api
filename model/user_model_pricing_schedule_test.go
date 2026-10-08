package model

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestUserModelPricingScheduleHistoryFailureRollsBack(t *testing.T) {
	user := setupUserModelPricingCacheTest(t)
	useUserCacheMiniRedis(t)
	actor := User{Username: "schedule-operator", AffCode: "schedule-operator", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
	require.NoError(t, DB.Create(&actor).Error)
	start, boundary := int64(100), int64(200)
	items := []UserModelPricingItem{{ModelName: "scheduled-model", Mode: "scheduled", Periods: []UserModelPricingPeriod{
		{DiscountBPS: 8000, StartTime: &start, EndTime: &boundary},
		{DiscountBPS: 6000, StartTime: &boundary},
	}}}
	revision, err := ReplaceUserModelPricingSchedules(t.Context(), user.Id, items, 1, actor.Id)
	require.NoError(t, err)
	var original []UserModelPricing
	require.NoError(t, DB.Where("user_id = ?", user.Id).Order("slot").Find(&original).Error)
	const callback = "test:pricing-history-write-failure"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "user_model_pricing_histories" {
			tx.AddError(errors.New("history write failed"))
		}
	}))
	t.Cleanup(func() { _ = DB.Callback().Create().Remove(callback) })
	items[0].Periods[1].DiscountBPS = 5000
	_, err = ReplaceUserModelPricingSchedules(t.Context(), user.Id, items, revision, actor.Id)
	require.ErrorContains(t, err, "history write failed")
	require.NoError(t, DB.Callback().Create().Remove(callback))
	var actual []UserModelPricing
	require.NoError(t, DB.Where("user_id = ?", user.Id).Order("slot").Find(&actual).Error)
	assert.Equal(t, original, actual)
	_, committedRevision, err := GetUserModelPricingRulesContext(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, revision, committedRevision)
	snapshot, err := GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	assert.Equal(t, 6000, snapshot.DiscountBPSAt("scheduled-model", 200))
	history, total, err := GetUserModelPricingHistory(t.Context(), user.Id, common.RoleRootUser, 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	assert.Nil(t, history[0].Before)
	assert.Equal(t, 6000, history[0].After.Periods[1].DiscountBPS)

	revision, err = ReplaceUserModelPricingSchedules(t.Context(), user.Id, items, revision, actor.Id)
	require.NoError(t, err)
	require.NoError(t, DB.Where("user_id = ?", user.Id).Order("slot").Find(&actual).Error)
	assert.Equal(t, original[0].Id, actual[0].Id)
	assert.Equal(t, original[1].Id, actual[1].Id)
	assert.Equal(t, 5000, actual[1].DiscountBPS)
	history, total, err = GetUserModelPricingHistory(t.Context(), user.Id, common.RoleRootUser, 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 2, total)
	assert.Equal(t, actor.Id, history[0].ActorId)
	assert.Equal(t, revision, history[0].Revision)
	assert.Equal(t, 6000, history[0].Before.Periods[1].DiscountBPS)
	assert.Equal(t, 5000, history[0].After.Periods[1].DiscountBPS)
	assert.Equal(t, 6000, snapshot.DiscountBPSAt("scheduled-model", 200))
}

func TestUserModelPricingScheduleRejectsInvalidTimelines(t *testing.T) {
	start, boundary, later := int64(100), int64(200), int64(300)
	negative, upper := int64(-1), maxUserModelPricingTime+1
	valid := UserModelPricingPeriod{DiscountBPS: 8000, StartTime: &start, EndTime: &boundary}
	cases := []struct {
		name    string
		periods []UserModelPricingPeriod
	}{
		{"empty", nil},
		{"zero discount", []UserModelPricingPeriod{{DiscountBPS: 0, StartTime: &start}}},
		{"full price", []UserModelPricingPeriod{{DiscountBPS: 10000, StartTime: &start}}},
		{"negative start", []UserModelPricingPeriod{{DiscountBPS: 8000, StartTime: &negative}}},
		{"start above bound", []UserModelPricingPeriod{{DiscountBPS: 8000, StartTime: &upper}}},
		{"end above bound", []UserModelPricingPeriod{{DiscountBPS: 8000, StartTime: &start, EndTime: &upper}}},
		{"empty interval", []UserModelPricingPeriod{{DiscountBPS: 8000, StartTime: &start, EndTime: &start}}},
		{"overlap", []UserModelPricingPeriod{valid, {DiscountBPS: 6000, StartTime: &start, EndTime: &later}}},
		{"permanent before later", []UserModelPricingPeriod{{DiscountBPS: 8000, StartTime: &start}, {DiscountBPS: 6000, StartTime: &boundary}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeUserModelPricingSchedules([]UserModelPricingItem{{ModelName: "model", Mode: "scheduled", Periods: tc.periods}}, nil, 100)
			require.ErrorIs(t, err, ErrUserModelPricingInvalid)
		})
	}
	configs, err := normalizeUserModelPricingSchedules([]UserModelPricingItem{{ModelName: "model", Mode: "scheduled", Periods: []UserModelPricingPeriod{
		{DiscountBPS: 6000, StartTime: &boundary}, valid,
	}}}, nil, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(100), configs["model"].Periods[0].StartTime)
	assert.Equal(t, int64(200), configs["model"].Periods[1].StartTime)
	_, err = normalizeUserModelPricingSchedules([]UserModelPricingItem{{ModelName: "model", UserModelPricingPeriod: UserModelPricingPeriod{DiscountBPS: 7000}}}, configs, 100)
	require.ErrorIs(t, err, ErrUserModelPricingInvalid)
}
