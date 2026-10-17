package model

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/discountexpr"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 使用公历 9999 年末作为时间上界，兼容浏览器 Date 和各数据库 bigint。
const maxUserModelPricingTime int64 = 253402300799

// 控制批量写入的绑定参数数量，兼容 SQLite 的较低参数上限。
const userModelPricingWriteBatchSize = 100

type UserModelPricingPeriod struct {
	DiscountBPS     int    `json:"discount_bps"`
	StartTime       *int64 `json:"start_time"`
	EndTime         *int64 `json:"end_time"`
	Expression      string `json:"expression,omitempty"`
	LegacyUnbounded bool   `json:"legacy_unbounded,omitempty"`
}

type UserModelPricingItem struct {
	ModelName string `json:"model_name"`
	UserModelPricingPeriod
	Mode      string                   `json:"mode"`
	Periods   []UserModelPricingPeriod `json:"periods,omitempty"`
	Permanent *UserModelPricingPeriod  `json:"permanent,omitempty"`
}

func userModelPricingItem(name string, config hosttypes.UserModelDiscountConfig) UserModelPricingItem {
	if config.Mode != hosttypes.UserModelPricingLayered {
		// 展示时转换旧配置；未开始的无限期段仍保留起点，不提前提升为永久规则。
		if len(config.Periods) > 0 {
			last := config.Periods[len(config.Periods)-1]
			if last.EndTime == nil && last.StartTime <= time.Now().Unix() {
				last.StartTime = 0
				config.Permanent = &last
				config.Periods = config.Periods[:len(config.Periods)-1]
			}
		}
		config.Mode = hosttypes.UserModelPricingLayered
	}
	item := UserModelPricingItem{ModelName: name, Mode: config.Mode}
	if config.Mode == hosttypes.UserModelPricingLayered {
		item.DiscountBPS = fullPriceDiscountBPS
		if config.Permanent != nil {
			item.DiscountBPS = config.Permanent.DiscountBPS
			item.Permanent = &UserModelPricingPeriod{DiscountBPS: config.Permanent.DiscountBPS, Expression: config.Permanent.Expression}
		}
	}
	for _, period := range config.Periods {
		start := period.StartTime
		item.Periods = append(item.Periods, UserModelPricingPeriod{DiscountBPS: period.DiscountBPS, StartTime: &start, EndTime: period.EndTime, Expression: period.Expression, LegacyUnbounded: period.EndTime == nil})
	}
	return item
}

func readUserModelPricingSchedules(tx *gorm.DB, userID int) (map[string]hosttypes.UserModelDiscountConfig, error) {
	var rows []UserModelPricing
	if err := tx.Where("user_id = ?", userID).Order("model_key ASC, slot ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	configs := make(map[string]hosttypes.UserModelDiscountConfig)
	for _, row := range rows {
		config := configs[row.ModelName]
		config.Mode = row.Mode
		if config.Mode == "" {
			config.Mode = hosttypes.UserModelPricingSingle
		}
		window := hosttypes.UserModelDiscountWindow{DiscountBPS: row.DiscountBPS, StartTime: row.StartTime, EndTime: row.EndTime, Expression: row.Expression}
		if row.Mode == hosttypes.UserModelPricingLayered && row.Slot == -1 {
			config.Permanent = &window
		} else {
			config.Periods = append(config.Periods, window)
		}
		configs[row.ModelName] = config
	}
	return configs, nil
}

func normalizeUserModelPricingSchedules(items []UserModelPricingItem, previous map[string]hosttypes.UserModelDiscountConfig, now int64) (map[string]hosttypes.UserModelDiscountConfig, error) {
	if len(items) > maxUserModelPricingRules {
		return nil, ErrUserModelPricingInvalid
	}
	configs := make(map[string]hosttypes.UserModelDiscountConfig, len(items))
	seen := make(map[string]bool, len(items))
	total := 0
	expressionBytes := 0
	for _, item := range items {
		name := ratio_setting.FormatMatchingModelName(ResolveUserModelPricingName(strings.TrimSpace(item.ModelName)))
		if name == "" || len(name) > userModelPricingNameSize || seen[name] {
			return nil, ErrUserModelPricingInvalid
		}
		seen[name] = true
		old := previous[name]
		if item.Mode == hosttypes.UserModelPricingLayered {
			config, err := normalizeLayeredUserModelPricing(item, old, now)
			if err != nil {
				return nil, err
			}
			total += len(config.Periods)
			if total > maxUserModelPricingRules {
				return nil, ErrUserModelPricingInvalid
			}
			if config.Permanent != nil {
				expressionBytes += len(config.Permanent.Expression)
			}
			for _, period := range config.Periods {
				expressionBytes += len(period.Expression)
			}
			if expressionBytes > 2<<20 {
				return nil, fmt.Errorf("user discount expressions exceed 2 MiB")
			}
			if config.Permanent != nil || len(config.Periods) > 0 {
				configs[name] = config
			}
			continue
		}
		if old.Mode == hosttypes.UserModelPricingLayered {
			return nil, ErrUserModelPricingInvalid
		}
		legacy := item.Mode == "" && item.StartTime == nil && item.EndTime == nil && len(item.Periods) == 0
		if item.Mode == "" && !legacy {
			item.Mode = hosttypes.UserModelPricingSingle
		}
		if legacy {
			// 旧客户端不认识排期，不能用一条规则静默覆盖多个时间段。
			if old.Mode == hosttypes.UserModelPricingScheduled {
				return nil, ErrUserModelPricingInvalid
			}
			item.Mode = hosttypes.UserModelPricingSingle
			if len(old.Periods) == 1 {
				item.EndTime = old.Periods[0].EndTime
			}
		}
		periods := item.Periods
		if item.Mode == hosttypes.UserModelPricingSingle {
			if len(periods) > 0 {
				return nil, ErrUserModelPricingInvalid
			}
			if item.DiscountBPS == fullPriceDiscountBPS {
				continue
			}
			if item.StartTime == nil && len(old.Periods) == 1 {
				start := old.Periods[0].StartTime
				item.StartTime = &start
			}
			periods = []UserModelPricingPeriod{item.UserModelPricingPeriod}
		} else if item.Mode != hosttypes.UserModelPricingScheduled || len(periods) == 0 {
			return nil, ErrUserModelPricingInvalid
		}
		total += len(periods)
		if total > maxUserModelPricingRules {
			return nil, ErrUserModelPricingInvalid
		}
		config := hosttypes.UserModelDiscountConfig{Mode: item.Mode, Periods: make([]hosttypes.UserModelDiscountWindow, 0, len(periods))}
		for _, period := range periods {
			start := now
			if period.StartTime != nil {
				start = *period.StartTime
			}
			if period.DiscountBPS < 1 || period.DiscountBPS >= fullPriceDiscountBPS || start < 0 || start > maxUserModelPricingTime ||
				(period.EndTime != nil && (*period.EndTime <= start || *period.EndTime > maxUserModelPricingTime)) {
				return nil, ErrUserModelPricingInvalid
			}
			config.Periods = append(config.Periods, hosttypes.UserModelDiscountWindow{DiscountBPS: period.DiscountBPS, StartTime: start, EndTime: period.EndTime})
		}
		sort.Slice(config.Periods, func(i, j int) bool { return config.Periods[i].StartTime < config.Periods[j].StartTime })
		for i := 1; i < len(config.Periods); i++ {
			end := config.Periods[i-1].EndTime
			if end == nil || *end > config.Periods[i].StartTime {
				return nil, ErrUserModelPricingInvalid
			}
		}
		configs[name] = config
	}
	return configs, nil
}

func ReplaceUserModelPricingSchedules(ctx context.Context, userID int, items []UserModelPricingItem, revision int64, actorID int) (int64, error) {
	if revision <= 0 || actorID <= 0 {
		return 0, ErrUserModelPricingInvalid
	}
	return replaceUserModelPricingSchedules(ctx, userID, items, &revision, actorID)
}

func replaceUserModelPricingSchedules(ctx context.Context, userID int, items []UserModelPricingItem, expectedRevision *int64, actorID int) (int64, error) {
	if userID <= 0 || (expectedRevision != nil && *expectedRevision <= 0) {
		return 0, ErrUserModelPricingInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, userModelPricingQueryTimeout)
	defer cancel()
	var revision int64
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		version, err := lockUserModelPricing(tx, userID)
		if err != nil {
			return err
		}
		if expectedRevision != nil && version.Revision != *expectedRevision {
			return ErrUserModelPricingRevisionConflict
		}
		revision = version.Revision
		previous, err := readUserModelPricingSchedules(tx, userID)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		configs, err := normalizeUserModelPricingSchedules(items, previous, now)
		if err != nil {
			return err
		}
		if reflect.DeepEqual(previous, configs) {
			return nil
		}
		if revision >= 9007199254740991 {
			return ErrUserModelPricingInvalid
		}
		revision++
		var subject User
		if err := tx.Select("id", "role").First(&subject, userID).Error; err != nil {
			return err
		}
		if actorID > 0 {
			var actor User
			if err := tx.Select("id", "role", "status").First(&actor, actorID).Error; err != nil {
				return err
			}
			if actor.Status != common.UserStatusEnabled || actor.Role < common.RoleAdminUser || (actor.Role != common.RoleRootUser && actor.Role <= subject.Role) {
				return ErrUserModelPricingInvalid
			}
		}
		names := make(map[string]bool, len(previous)+len(configs))
		for name := range previous {
			names[name] = true
		}
		for name := range configs {
			names[name] = true
		}
		history := make([]UserModelPricingHistory, 0, len(names))
		upserts := make([]UserModelPricing, 0)
		for name := range names {
			before, had := previous[name]
			after, has := configs[name]
			if had && has && reflect.DeepEqual(before, after) {
				continue
			}
			action := "update"
			if !had {
				action = "create"
			} else if !has {
				action = "remove"
			}
			entry, err := newUserModelPricingHistory(userID, subject.Role, actorID, name, action, revision, now, before, after)
			if err != nil {
				return err
			}
			history = append(history, entry)
			key := userModelPricingModelKey(name)
			if after.Permanent != nil && (!had || !reflect.DeepEqual(before.Permanent, after.Permanent)) {
				upserts = append(upserts, UserModelPricing{UserId: userID, ModelName: name, ModelKey: key, Slot: -1, Mode: after.Mode, DiscountBPS: after.Permanent.DiscountBPS, Expression: after.Permanent.Expression})
			}
			for slot, period := range after.Periods {
				if had && before.Mode == after.Mode && slot < len(before.Periods) && reflect.DeepEqual(before.Periods[slot], period) {
					continue
				}
				row := UserModelPricing{UserId: userID, ModelName: name, ModelKey: key, Slot: slot, Mode: after.Mode, DiscountBPS: period.DiscountBPS, StartTime: period.StartTime, EndTime: period.EndTime, Expression: period.Expression}
				upserts = append(upserts, row)
			}
		}
		var stored []UserModelPricing
		if err := tx.Select("id", "model_name", "slot").Where("user_id = ?", userID).Find(&stored).Error; err != nil {
			return err
		}
		deleteIDs := make([]int, 0)
		for _, row := range stored {
			if (row.Slot == -1 && configs[row.ModelName].Permanent == nil) || row.Slot >= len(configs[row.ModelName].Periods) {
				deleteIDs = append(deleteIDs, row.Id)
			}
		}
		for start := 0; start < len(deleteIDs); start += userModelPricingWriteBatchSize {
			if err := tx.Where("id IN ?", deleteIDs[start:min(start+userModelPricingWriteBatchSize, len(deleteIDs))]).Delete(&UserModelPricing{}).Error; err != nil {
				return err
			}
		}
		if len(upserts) > 0 {
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "user_id"}, {Name: "model_key"}, {Name: "slot"}},
				DoUpdates: clause.AssignmentColumns([]string{"mode", "discount_bps", "start_time", "end_time", "expression"}),
			}).CreateInBatches(&upserts, userModelPricingWriteBatchSize).Error; err != nil {
				return err
			}
		}
		if err := tx.CreateInBatches(&history, userModelPricingWriteBatchSize).Error; err != nil {
			return err
		}
		if err := tx.Model(&UserModelPricingRevision{}).Where("user_id = ?", userID).UpdateColumn("revision", revision).Error; err != nil {
			return err
		}
		if common.RedisEnabled {
			return publishUserModelPricingVersion(ctx, userID, revision, true)
		}
		return nil
	})
	return revision, err
}

func normalizeLayeredUserModelPricing(item UserModelPricingItem, previous hosttypes.UserModelDiscountConfig, now int64) (hosttypes.UserModelDiscountConfig, error) {
	config := hosttypes.UserModelDiscountConfig{Mode: hosttypes.UserModelPricingLayered}
	validateRule := func(period UserModelPricingPeriod) (hosttypes.UserModelDiscountWindow, error) {
		window := hosttypes.UserModelDiscountWindow{DiscountBPS: period.DiscountBPS, Expression: strings.TrimSpace(period.Expression), EndTime: period.EndTime}
		if period.StartTime != nil {
			window.StartTime = *period.StartTime
		} else {
			window.StartTime = now
		}
		if window.Expression != "" {
			if err := discountexpr.Validate(window.Expression); err != nil {
				return window, err
			}
			window.DiscountBPS = fullPriceDiscountBPS
		} else if window.DiscountBPS < 1 || window.DiscountBPS > fullPriceDiscountBPS {
			return window, ErrUserModelPricingInvalid
		}
		return window, nil
	}
	if item.Permanent != nil {
		if item.Permanent.StartTime != nil || item.Permanent.EndTime != nil {
			return config, ErrUserModelPricingInvalid
		}
		permanent, err := validateRule(*item.Permanent)
		if err != nil {
			return config, err
		}
		permanent.StartTime = 0
		if permanent.Expression != "" && discountexpr.DependsOnPermanent(permanent.Expression) {
			return config, fmt.Errorf("permanent discount cannot reference itself")
		}
		if permanent.DiscountBPS != fullPriceDiscountBPS || permanent.Expression != "" {
			config.Permanent = &permanent
		}
	}
	if len(item.Periods) > maxUserModelPricingRules {
		return config, ErrUserModelPricingInvalid
	}
	for _, period := range item.Periods {
		window, err := validateRule(period)
		if err != nil {
			return config, err
		}
		if window.Expression != "" && discountexpr.DependsOnPermanent(window.Expression) && config.Permanent != nil && config.Permanent.Expression != "" {
			return config, fmt.Errorf("set a fixed permanent discount before using it as the temporary default")
		}
		if window.StartTime < 0 || window.StartTime > maxUserModelPricingTime || (window.EndTime != nil && (*window.EndTime <= window.StartTime || *window.EndTime > maxUserModelPricingTime)) {
			return config, ErrUserModelPricingInvalid
		}
		if window.EndTime == nil {
			// 旧无限期时间窗只能原样保留；新建或修改临时规则必须明确结束时间。
			kept := false
			for _, old := range previous.Periods {
				if reflect.DeepEqual(old, window) {
					kept = true
					break
				}
			}
			if !kept {
				return config, ErrUserModelPricingInvalid
			}
		}
		config.Periods = append(config.Periods, window)
	}
	sort.Slice(config.Periods, func(i, j int) bool { return config.Periods[i].StartTime < config.Periods[j].StartTime })
	for i := 1; i < len(config.Periods); i++ {
		end := config.Periods[i-1].EndTime
		if end == nil || *end > config.Periods[i].StartTime {
			return config, ErrUserModelPricingInvalid
		}
	}
	return config, nil
}

// migrateUserModelPricingScheduleIndex 先创建新唯一索引，再移除旧索引；已有行的 slot 为 0。
func migrateUserModelPricingScheduleIndex(db *gorm.DB) error {
	if !db.Migrator().HasIndex(&UserModelPricing{}, "idx_user_model_pricing_slot") {
		if err := db.Migrator().CreateIndex(&UserModelPricing{}, "idx_user_model_pricing_slot"); err != nil {
			return err
		}
	}
	if db.Migrator().HasIndex(&UserModelPricing{}, "idx_user_model_pricing_user_key") {
		return db.Migrator().DropIndex(&UserModelPricing{}, "idx_user_model_pricing_user_key")
	}
	return nil
}
