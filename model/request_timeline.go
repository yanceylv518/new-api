package model

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RequestTimelineCaptureEnabled follows the persisted system option.
var RequestTimelineCaptureEnabled atomic.Bool

// RequestTimeline contains eight columns; payload evolution uses data.version.
type RequestTimeline struct {
	RequestID                string  `json:"request_id" gorm:"column:request_id;type:varchar(64);primaryKey;not null"`
	UserID                   int64   `json:"user_id" gorm:"column:user_id;type:bigint;not null"`
	CreatedAt                int64   `json:"created_at" gorm:"type:bigint;not null;index:idx_request_timelines_created_at"`
	UpstreamStatus           *int    `json:"upstream_status" gorm:"type:smallint"`
	Result                   string  `json:"result" gorm:"type:varchar(16);not null"`
	ErrorKind                *string `json:"error_kind" gorm:"type:varchar(32)"`
	UpstreamRequestBodyBytes *int64  `json:"upstream_request_body_bytes" gorm:"type:bigint"`
	Data                     string  `json:"-" gorm:"type:text;not null"`
}

func RequestTimelineStorageSupported() bool {
	return LOG_DB != nil && !common.UsingLogDatabase(common.DatabaseTypeClickHouse)
}

func MigrateRequestTimelines() error {
	if !RequestTimelineStorageSupported() {
		return errors.New("request timeline storage is unsupported")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := LOG_DB.WithContext(ctx)
	if common.UsingLogDatabase(common.DatabaseTypeMySQL) {
		db = db.Set("gorm:table_options", "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")
	}
	if !db.Migrator().HasTable(&RequestTimeline{}) {
		return db.Migrator().CreateTable(&RequestTimeline{})
	}
	// Do not silently alter a table left by a different experimental schema.
	columns, err := db.Migrator().ColumnTypes(&RequestTimeline{})
	if err != nil {
		return err
	}
	expected := map[string]bool{"request_id": true, "user_id": true, "created_at": true, "upstream_status": true, "result": true, "error_kind": true, "upstream_request_body_bytes": true, "data": true}
	for _, column := range columns {
		if expected[column.Name()] {
			delete(expected, column.Name())
		} else {
			nullable, known := column.Nullable()
			_, hasDefault := column.DefaultValue()
			autoIncrement, _ := column.AutoIncrement()
			if !(known && nullable) && !hasDefault && !autoIncrement {
				return errors.New("request_timelines has an extra required column without a default")
			}
		}
		primary, known := column.PrimaryKey()
		if known && primary != (column.Name() == "request_id") {
			return errors.New("request_timelines has an incompatible primary key")
		}
		if column.Name() == "request_id" {
			if size, ok := column.Length(); ok && size < 64 {
				return errors.New("request_timelines request_id is too short")
			}
		}
	}
	if len(expected) != 0 {
		return errors.New("request_timelines is missing required columns")
	}
	if common.UsingLogDatabase(common.DatabaseTypeMySQL) {
		var collation string
		if err := db.Raw("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?", "request_timelines", "request_id").Scan(&collation).Error; err != nil {
			return err
		}
		if !strings.HasSuffix(collation, "_bin") {
			return errors.New("request_timelines request_id requires a binary collation")
		}
	}
	if !db.Migrator().HasIndex(&RequestTimeline{}, "idx_request_timelines_created_at") {
		return db.Migrator().CreateIndex(&RequestTimeline{}, "idx_request_timelines_created_at")
	}
	return nil
}

func InsertRequestTimelines(ctx context.Context, entries []*RequestTimeline) error {
	if len(entries) == 0 {
		return nil
	}
	return LOG_DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&entries).Error
}

func FindRequestTimeline(ctx context.Context, requestID string) (*RequestTimeline, error) {
	var entry RequestTimeline
	// Explicit columns keep PostgreSQL cached result shapes stable when a
	// newer version adds optional columns while this process is still running.
	if err := LOG_DB.WithContext(ctx).
		Select("request_id", "user_id", "created_at", "upstream_status", "result", "error_kind", "upstream_request_body_bytes", "data").
		Where("request_id = ?", requestID).Take(&entry).Error; err != nil {
		return nil, err
	}
	if entry.RequestID != requestID {
		return nil, gorm.ErrRecordNotFound
	}
	return &entry, nil
}

func DeleteOldRequestTimelineBatch(ctx context.Context, cutoff int64, limit int) (int64, error) {
	var ids []string
	db := LOG_DB.WithContext(ctx)
	if err := db.Model(&RequestTimeline{}).Where("created_at < ?", cutoff).Order("created_at").Limit(min(500, max(1, limit))).Pluck("request_id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result := db.Where("request_id IN ? AND created_at < ?", ids, cutoff).Delete(&RequestTimeline{})
	return result.RowsAffected, result.Error
}
