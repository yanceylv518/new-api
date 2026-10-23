package model

import (
	"errors"
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var managementRateLimitOptionMutex sync.Mutex

type ManagementRateLimitConflictError struct {
	CurrentValue string
}

func (*ManagementRateLimitConflictError) Error() string {
	return "management rate limits changed; reload saved settings"
}

func canonicalManagementRateLimits(raw string) (string, error) {
	settings, err := common.ParseManagementRateLimits(raw)
	if err != nil {
		return "", err
	}
	encoded, err := common.Marshal(settings)
	return string(encoded), err
}

func publishManagementRateLimits(value string) error {
	common.OptionMapRWMutex.Lock()
	defer common.OptionMapRWMutex.Unlock()
	if err := common.LoadManagementRateLimits(value); err != nil {
		return err
	}
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMap[common.ManagementRateLimitsOptionKey] = value
	return nil
}

// RefreshManagementRateLimits 在保存共用的锁内重新读取数据库，不应用批量同步的旧副本。
func RefreshManagementRateLimits() error {
	managementRateLimitOptionMutex.Lock()
	defer managementRateLimitOptionMutex.Unlock()
	var option Option
	err := DB.Where(&Option{Key: common.ManagementRateLimitsOptionKey}).First(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		encoded, encodeErr := common.Marshal(common.DefaultManagementRateLimits())
		if encodeErr != nil {
			return encodeErr
		}
		option.Value = string(encoded)
	} else if err != nil {
		return err
	}
	value, err := canonicalManagementRateLimits(option.Value)
	if err != nil {
		return err
	}
	return publishManagementRateLimits(value)
}

// UpdateManagementRateLimits 以页面读取值校验过期保存，行锁同时保护不同节点的写入。
// expected 为 nil 仅供服务端直接更新；管理接口必须提供读取时的配置。
func UpdateManagementRateLimits(raw string, expected *string) (string, error) {
	value, err := canonicalManagementRateLimits(raw)
	if err != nil {
		return "", err
	}
	var expectedValue string
	if expected != nil {
		expectedValue, err = canonicalManagementRateLimits(*expected)
		if err != nil {
			return "", fmt.Errorf("invalid saved rate limit baseline: %w", err)
		}
	}
	defaults, err := common.Marshal(common.DefaultManagementRateLimits())
	if err != nil {
		return "", err
	}
	managementRateLimitOptionMutex.Lock()
	defer managementRateLimitOptionMutex.Unlock()
	err = DB.Transaction(func(tx *gorm.DB) error {
		option := Option{Key: common.ManagementRateLimitsOptionKey, Value: string(defaults)}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&option).Error; err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where(&Option{Key: option.Key}).First(&option).Error; err != nil {
			return err
		}
		current, err := canonicalManagementRateLimits(option.Value)
		if err != nil {
			return err
		}
		if expected != nil && current != expectedValue {
			return &ManagementRateLimitConflictError{CurrentValue: current}
		}
		return tx.Model(&Option{}).Where(&Option{Key: option.Key}).Update("value", value).Error
	})
	if err != nil {
		var conflict *ManagementRateLimitConflictError
		if errors.As(err, &conflict) {
			if publishErr := publishManagementRateLimits(conflict.CurrentValue); publishErr != nil {
				return "", publishErr
			}
		}
		return "", err
	}
	if err := publishManagementRateLimits(value); err != nil {
		return "", err
	}
	return value, nil
}
