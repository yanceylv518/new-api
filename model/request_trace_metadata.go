package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RequestTraceMetadata struct {
	Id        int64  `gorm:"primaryKey"`
	RequestId string `gorm:"type:varchar(64);uniqueIndex:idx_request_trace_attempt,priority:1"`
	Attempt   int    `gorm:"uniqueIndex:idx_request_trace_attempt,priority:2"`
	UserId    int
	CreatedAt int64  `gorm:"index:idx_request_traces_created_at"`
	Data      string `gorm:"type:text"`
}

func (RequestTraceMetadata) TableName() string {
	return "request_trace_metadata"
}

type RequestTraceView struct {
	RequestId string `json:"request_id"`
	Attempt   int    `json:"attempt"`
	CreatedAt int64  `json:"created_at"`
	common.RequestTraceData
}

func RecordRequestTraces(ctx context.Context, rows []RequestTraceMetadata) (err error) {
	var compressor common.RequestTraceCompressor
	defer func() { err = errors.Join(err, compressor.Close()) }()
	return recordRequestTraces(ctx, rows, &compressor)
}

func recordRequestTraces(ctx context.Context, rows []RequestTraceMetadata, compressor *common.RequestTraceCompressor) error {
	if len(rows) == 0 {
		return nil
	}
	tx := LOG_DB.WithContext(ctx)
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return tx.Omit("Id").Create(&rows).Error
	}
	stored := append([]RequestTraceMetadata(nil), rows...)
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
	tx := LOG_DB.WithContext(ctx).Model(&RequestTraceMetadata{}).Where("request_id = ?", requestId)
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		tx = LOG_DB.WithContext(ctx).Table("request_trace_metadata FINAL").Where("request_id = ?", requestId)
	}
	if !admin {
		tx = tx.Where("user_id = ?", userId)
	}
	var rows []RequestTraceMetadata
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
		if err := LOG_DB.WithContext(ctx).Model(&RequestTraceMetadata{}).Where("created_at < ?", timestamp).Count(&count).Error; err != nil {
			return 0, err
		}
		if count == 0 {
			return 0, nil
		}
		return count, LOG_DB.WithContext(ctx).Exec("ALTER TABLE request_trace_metadata DELETE WHERE created_at < ? SETTINGS mutations_sync = 1", timestamp).Error
	}
	var ids []int64
	if err := LOG_DB.WithContext(ctx).Model(&RequestTraceMetadata{}).Where("created_at < ?", timestamp).Order("created_at").Order("id").Limit(limit).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result := LOG_DB.WithContext(ctx).Where("id IN ?", ids).Delete(&RequestTraceMetadata{})
	return result.RowsAffected, result.Error
}

func CountOldRequestTraces(ctx context.Context, timestamp int64) (int64, error) {
	var count int64
	err := LOG_DB.WithContext(ctx).Model(&RequestTraceMetadata{}).Where("created_at < ?", timestamp).Count(&count).Error
	return count, err
}

func GetLatestRequestTraceCleanup(ctx context.Context, taskType string) (*SystemTask, error) {
	var task SystemTask
	result := DB.WithContext(ctx).Where("type = ?", taskType).Order("id desc").Limit(1).Find(&task)
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
	columns, err := migrator.ColumnTypes("request_traces")
	if err != nil {
		return fmt.Errorf("inspect legacy request_traces: %w", err)
	}
	metadataColumns := []string{"id", "request_id", "attempt", "user_id", "created_at", "data"}
	var target string
	var expected []string
	switch len(columns) {
	case len(metadataColumns):
		target, expected = "request_trace_metadata", metadataColumns
	case len(requestTraceColumns):
		target, expected = requestTraceTableName, requestTraceColumns[:]
	default:
		return errors.New("unknown legacy request_traces layout; resolve the schema before migration")
	}
	actual := make(map[string]bool, len(columns))
	for _, column := range columns {
		actual[column.Name()] = true
	}
	for _, name := range expected {
		if !actual[name] {
			return errors.New("unknown legacy request_traces layout; resolve the schema before migration")
		}
	}
	if migrator.HasTable(target) {
		return fmt.Errorf("both request_traces and %s exist; resolve the table conflict before migration", target)
	}
	if db.Dialector.Name() == "clickhouse" {
		err = db.Exec("RENAME TABLE ? TO ?", clause.Table{Name: "request_traces"}, clause.Table{Name: target}).Error
	} else {
		err = migrator.RenameTable("request_traces", target)
	}
	if err != nil {
		return fmt.Errorf("rename request_traces to %s: %w", target, err)
	}
	return nil
}

func migrateRequestTraces() error {
	if err := migrateRequestTraceTableName(LOG_DB); err != nil {
		return err
	}
	if !common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return LOG_DB.AutoMigrate(&RequestTraceMetadata{})
	}
	statement := `CREATE TABLE IF NOT EXISTS request_trace_metadata (
		id Int64 DEFAULT 0, request_id String, attempt Int32, user_id Int32,
		created_at UInt64, data String
	) ENGINE = ReplacingMergeTree(created_at) ORDER BY (request_id, attempt)`
	if err := LOG_DB.Exec(statement).Error; err != nil {
		return err
	}
	return removeRequestTraceTTL(context.Background(), "request_trace_metadata")
}
