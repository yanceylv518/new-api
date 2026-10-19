package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RequestTrace struct {
	Id        int64  `gorm:"primaryKey"`
	RequestId string `gorm:"type:varchar(64);uniqueIndex:idx_request_trace_attempt,priority:1"`
	Attempt   int    `gorm:"uniqueIndex:idx_request_trace_attempt,priority:2"`
	UserId    int
	CreatedAt int64  `gorm:"index:idx_request_traces_created_at"`
	Data      string `gorm:"type:text"`
}

func (RequestTrace) TableName() string {
	return "request_trace_metadata"
}

type RequestTraceView struct {
	RequestId string `json:"request_id"`
	Attempt   int    `json:"attempt"`
	CreatedAt int64  `json:"created_at"`
	common.RequestTraceData
}

func RecordRequestTraces(ctx context.Context, rows []RequestTrace) (err error) {
	var compressor common.RequestTraceCompressor
	defer func() { err = errors.Join(err, compressor.Close()) }()
	return recordRequestTraces(ctx, rows, &compressor)
}

func recordRequestTraces(ctx context.Context, rows []RequestTrace, compressor *common.RequestTraceCompressor) error {
	if len(rows) == 0 {
		return nil
	}
	tx := LOG_DB.WithContext(ctx)
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return tx.Omit("Id").Create(&rows).Error
	}
	stored := append([]RequestTrace(nil), rows...)
	for index := range stored {
		if err := ctx.Err(); err != nil {
			return err
		}
		encoded, err := compressor.Encode(stored[index].Data)
		if err != nil {
			return fmt.Errorf("encode request trace: %w", err)
		}
		stored[index].Data = encoded
	}
	if len(rows) == 1 {
		// 单行 INSERT 自身具有原子性；多行尝试仍保留 GORM 事务。
		tx = tx.Session(&gorm.Session{SkipDefaultTransaction: true})
	}
	err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "request_id"}, {Name: "attempt"}}, DoNothing: true}).Create(&stored).Error
	for index := range rows {
		rows[index].Id = stored[index].Id
	}
	return err
}

func GetRequestTraces(ctx context.Context, requestId string, userId int, admin bool) ([]RequestTraceView, error) {
	tx := LOG_DB.WithContext(ctx).Model(&RequestTrace{}).Where("request_id = ?", requestId)
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		tx = LOG_DB.WithContext(ctx).Table("request_trace_metadata FINAL").Where("request_id = ?", requestId)
	}
	if !admin {
		tx = tx.Where("user_id = ?", userId)
	}
	var rows []RequestTrace
	if admin {
		tx = tx.Order("attempt asc")
	} else {
		tx = tx.Order("attempt desc").Limit(1)
	}
	if err := tx.Find(&rows).Error; err != nil {
		return nil, err
	}
	views := make([]RequestTraceView, 0, len(rows))
	for _, row := range rows {
		decoded, err := common.DecodeRequestTraceStorage(row.Data)
		if err != nil {
			return nil, fmt.Errorf("decode request trace: %w", err)
		}
		var data common.RequestTraceData
		if err := common.UnmarshalJsonStr(decoded, &data); err != nil {
			return nil, fmt.Errorf("decode request trace: %w", err)
		}
		if !admin {
			data.AdminInfo = nil
		}
		views = append(views, RequestTraceView{RequestId: row.RequestId, Attempt: row.Attempt, CreatedAt: row.CreatedAt, RequestTraceData: data})
	}
	return views, nil
}

func DeleteOldRequestTraces(ctx context.Context, timestamp int64, limit int) (int64, error) {
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		var count int64
		if err := LOG_DB.WithContext(ctx).Model(&RequestTrace{}).Where("created_at < ?", timestamp).Count(&count).Error; err != nil {
			return 0, err
		}
		if count == 0 {
			return 0, nil
		}
		return count, LOG_DB.WithContext(ctx).Exec("ALTER TABLE request_trace_metadata DELETE WHERE created_at < ? SETTINGS mutations_sync = 1", timestamp).Error
	}
	var ids []int64
	if err := LOG_DB.WithContext(ctx).Model(&RequestTrace{}).Where("created_at < ?", timestamp).Order("created_at").Order("id").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result := LOG_DB.WithContext(ctx).Where("id IN ?", ids).Delete(&RequestTrace{})
	return result.RowsAffected, result.Error
}

func CountOldRequestTraces(ctx context.Context, timestamp int64) (int64, error) {
	var count int64
	err := LOG_DB.WithContext(ctx).Model(&RequestTrace{}).Where("created_at < ?", timestamp).Count(&count).Error
	return count, err
}

func GetLatestRequestTraceCleanup(ctx context.Context) (*SystemTask, error) {
	var task SystemTask
	result := DB.WithContext(ctx).Where("type = ?", SystemTaskTypeRequestTraceCleanup).Order("id desc").Limit(1).Find(&task)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &task, nil
}

func migrateRequestTraceTableName(db *gorm.DB) error {
	migrator := db.Migrator()
	if !migrator.HasTable("request_traces") {
		return nil
	}
	if migrator.HasTable(&RequestTrace{}) {
		return fmt.Errorf("both request_traces and request_trace_metadata exist; resolve the table conflict before migration")
	}
	var err error
	if db.Dialector.Name() == "clickhouse" {
		err = db.Exec("RENAME TABLE request_traces TO request_trace_metadata").Error
	} else {
		err = migrator.RenameTable("request_traces", &RequestTrace{})
	}
	if err != nil {
		return fmt.Errorf("rename request_traces to request_trace_metadata: %w", err)
	}
	return nil
}

func migrateRequestTraces() error {
	if err := migrateRequestTraceTableName(LOG_DB); err != nil {
		return err
	}
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return LOG_DB.AutoMigrate(&RequestTrace{})
	}
	ttlDays := clickHouseLogTTLDays()
	statement := `CREATE TABLE IF NOT EXISTS request_trace_metadata (
		id Int64 DEFAULT 0, request_id String, attempt Int32, user_id Int32,
		created_at UInt64, data String
	) ENGINE = ReplacingMergeTree(created_at) ORDER BY (request_id, attempt)` + clickHouseLogTTLClause(ttlDays)
	if err := LOG_DB.Exec(statement).Error; err != nil {
		return err
	}
	if ttlDays > 0 {
		return LOG_DB.Exec("ALTER TABLE request_trace_metadata MODIFY TTL " + clickHouseLogTTLExpression(ttlDays)).Error
	}
	var definition string
	if err := LOG_DB.Raw("SHOW CREATE TABLE request_trace_metadata").Scan(&definition).Error; err != nil {
		return err
	}
	if strings.Contains(definition, "TTL ") {
		return LOG_DB.Exec("ALTER TABLE request_trace_metadata REMOVE TTL").Error
	}
	return nil
}
