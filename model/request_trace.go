package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const requestTraceTableName = "request_trace_timeline"

// RequestTrace 调整列时须显式迁移并保留历史数据；TraceData 按版本兼容旧格式。
type RequestTrace struct {
	Id                            int64   `json:"id" gorm:"primaryKey;autoIncrement;index:idx_request_traces_started,priority:2"`
	RequestId                     string  `json:"request_id" gorm:"type:varchar(64);not null;uniqueIndex:idx_request_traces_request_id"`
	UserId                        *int64  `json:"user_id" gorm:"type:bigint"`
	TokenId                       *int64  `json:"token_id" gorm:"type:bigint"`
	ModelName                     *string `json:"model_name" gorm:"type:text"`
	GroupName                     *string `json:"group_name" gorm:"type:text"`
	FinalChannelId                *int64  `json:"final_channel_id" gorm:"type:bigint"`
	RequestKind                   string  `json:"request_kind" gorm:"type:varchar(32);not null"`
	IsStream                      *bool   `json:"is_stream"`
	HTTPStatus                    *int    `json:"http_status" gorm:"column:http_status;type:integer"`
	Outcome                       string  `json:"outcome" gorm:"type:varchar(32);not null"`
	StartedAtUS                   int64   `json:"started_at_us" gorm:"column:started_at_us;type:bigint;not null;index:idx_request_traces_started,priority:1"`
	FinishedAtUS                  int64   `json:"finished_at_us" gorm:"column:finished_at_us;type:bigint;not null"`
	DurationUS                    int64   `json:"duration_us" gorm:"column:duration_us;type:bigint;not null"`
	FinalAttemptFirstByteOffsetUS *int64  `json:"final_attempt_first_byte_offset_us" gorm:"column:final_attempt_first_byte_offset_us;type:bigint"`
	FirstPayloadWriteOffsetUS     *int64  `json:"first_payload_write_offset_us" gorm:"column:first_payload_write_offset_us;type:bigint"`
	AttemptCount                  int     `json:"attempt_count" gorm:"type:integer;not null"`
	CaptureStatus                 string  `json:"capture_status" gorm:"type:varchar(32);not null"`
	NodeId                        string  `json:"node_id" gorm:"type:text;not null"`
	SchemaVersion                 int     `json:"schema_version" gorm:"type:integer;not null"`
	TraceData                     string  `json:"-" gorm:"not null"`
}

func (RequestTrace) TableName() string {
	return requestTraceTableName
}

var requestTraceColumns = [...]string{
	"id", "request_id", "user_id", "token_id", "model_name", "group_name", "final_channel_id",
	"request_kind", "is_stream", "http_status", "outcome", "started_at_us", "finished_at_us", "duration_us",
	"final_attempt_first_byte_offset_us", "first_payload_write_offset_us", "attempt_count", "capture_status",
	"node_id", "schema_version", "trace_data",
}

func RequestTraceRetentionDays() int {
	return system_setting.GetRequestTraceRetentionDays()
}

// 删除前读取持久化策略，避免多节点选项缓存尚未同步时沿用较短的保留期。
func GetSavedRequestTraceRetentionDays(ctx context.Context, timeline bool) (int, error) {
	key := "RequestTraceConfig"
	days := common.DefaultRequestTraceRetentionDays
	if timeline {
		key = system_setting.RequestTraceRetentionOptionKey
		days = system_setting.DefaultRequestTraceRetentionDays()
	}
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var option Option
	result := DB.WithContext(queryCtx).Where(&Option{Key: key}).Limit(1).Find(&option)
	if result.Error != nil {
		return 0, result.Error
	}
	if result.RowsAffected == 0 {
		return days, nil
	}
	if timeline {
		if err := system_setting.ValidateRequestTraceRetentionDays(option.Value); err != nil {
			return 0, err
		}
		return strconv.Atoi(option.Value)
	}
	config, err := common.ParseRequestTraceConfig(option.Value)
	return config.RetentionDays, err
}

func removeRequestTraceTTL(ctx context.Context, table string) error {
	var definition string
	db := LOG_DB.WithContext(ctx)
	if err := db.Raw("SHOW CREATE TABLE ?", clause.Table{Name: table}).Scan(&definition).Error; err != nil {
		return err
	}
	if strings.Contains(definition, "TTL ") {
		// 清理由系统任务统一执行，旧 TTL 不得绕过管理员延长或关闭的保留策略。
		return db.Exec("ALTER TABLE ? REMOVE TTL", clause.Table{Name: table}).Error
	}
	return nil
}

func MigrateRequestTraces() error {
	if err := migrateRequestTraceTableName(LOG_DB); err != nil {
		return err
	}
	statement := &gorm.Statement{DB: LOG_DB}
	if err := statement.Parse(&RequestTrace{}); err != nil {
		return err
	}
	if len(statement.Schema.DBNames) != len(requestTraceColumns) {
		return errors.New("request_trace_timeline model column contract mismatch")
	}
	modelColumns := make(map[string]bool, len(requestTraceColumns))
	for _, name := range statement.Schema.DBNames {
		modelColumns[name] = true
	}
	for _, name := range requestTraceColumns {
		if !modelColumns[name] {
			return fmt.Errorf("request_trace_timeline model is missing required column %s", name)
		}
	}
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		if err := LOG_DB.Exec(`CREATE TABLE IF NOT EXISTS request_trace_timeline (
			id Int64 DEFAULT 0, request_id String, user_id Nullable(Int64), token_id Nullable(Int64),
			model_name Nullable(String), group_name Nullable(String), final_channel_id Nullable(Int64),
			request_kind String, is_stream Nullable(UInt8), http_status Nullable(Int32), outcome String,
			started_at_us Int64, finished_at_us Int64, duration_us Int64,
			final_attempt_first_byte_offset_us Nullable(Int64), first_payload_write_offset_us Nullable(Int64),
			attempt_count Int32, capture_status String, node_id String, schema_version Int32, trace_data String CODEC(ZSTD(1))
		) ENGINE = ReplacingMergeTree()
		PARTITION BY toYYYYMM(toDateTime(intDiv(started_at_us, 1000000)))
		ORDER BY (request_id)`).Error; err != nil {
			return err
		}
		if err := removeRequestTraceTTL(context.Background(), requestTraceTableName); err != nil {
			return err
		}
	} else if !LOG_DB.Migrator().HasTable(&RequestTrace{}) {
		db := LOG_DB
		if common.UsingLogDatabase(common.DatabaseTypeMySQL) {
			db = db.Set("gorm:table_options", "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")
		}
		if err := db.Migrator().CreateTable(&RequestTrace{}); err != nil && !LOG_DB.Migrator().HasTable(&RequestTrace{}) {
			return err
		}
	}
	// 禁止 AutoMigrate 随模型变化自动增加或删除列。
	columns, err := LOG_DB.Migrator().ColumnTypes(&RequestTrace{})
	if err != nil {
		return err
	}
	actual := make(map[string]bool, len(columns))
	for _, column := range columns {
		actual[column.Name()] = true
	}
	if len(actual) != len(requestTraceColumns) {
		return errors.New("request_trace_timeline column contract mismatch")
	}
	for _, name := range requestTraceColumns {
		if !actual[name] {
			return fmt.Errorf("request_trace_timeline is missing required column %s", name)
		}
	}
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		var compressionCodec string
		if err := LOG_DB.Raw("SELECT compression_codec FROM system.columns WHERE database = currentDatabase() AND table = ? AND name = 'trace_data'", requestTraceTableName).Row().Scan(&compressionCodec); err != nil {
			return fmt.Errorf("read request_trace_timeline compression codec: %w", err)
		}
		if compressionCodec != "CODEC(ZSTD(1))" {
			// 只修改列压缩元数据，已有数据块随正常合并采用新 codec。
			if err := LOG_DB.Exec("ALTER TABLE request_trace_timeline MODIFY COLUMN trace_data CODEC(ZSTD(1))").Error; err != nil {
				return fmt.Errorf("configure request_trace_timeline compression codec: %w", err)
			}
		}
	} else {
		for _, index := range []string{"idx_request_traces_request_id", "idx_request_traces_started"} {
			if !LOG_DB.Migrator().HasIndex(&RequestTrace{}, index) {
				if err := LOG_DB.Migrator().CreateIndex(&RequestTrace{}, index); err != nil && !LOG_DB.Migrator().HasIndex(&RequestTrace{}, index) {
					return err
				}
			}
		}
		const obsoleteIndex = "idx_request_traces_user_started"
		if LOG_DB.Migrator().HasIndex(&RequestTrace{}, obsoleteIndex) {
			if err := LOG_DB.Migrator().DropIndex(&RequestTrace{}, obsoleteIndex); err != nil && LOG_DB.Migrator().HasIndex(&RequestTrace{}, obsoleteIndex) {
				return fmt.Errorf("remove unused timeline index: %w", err)
			}
		}
	}
	return nil
}

func InsertRequestTraces(ctx context.Context, entries []*RequestTrace) error {
	if len(entries) == 0 {
		return nil
	}
	db := LOG_DB.WithContext(ctx)
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		// 原生批量驱动要求可空数值的指针类型与列类型一致。
		type clickHouseTrace struct {
			RequestTrace `gorm:"embedded"`
			IsStream     *uint8 `gorm:"column:is_stream"`
			HTTPStatus   *int32 `gorm:"column:http_status"`
		}
		rows := make([]clickHouseTrace, len(entries))
		for i, entry := range entries {
			rows[i].RequestTrace = *entry
			if entry.IsStream != nil {
				var stream uint8
				if *entry.IsStream {
					stream = 1
				}
				rows[i].IsStream = &stream
			}
			if entry.HTTPStatus != nil {
				status := int32(*entry.HTTPStatus)
				rows[i].HTTPStatus = &status
			}
		}
		return db.Table(requestTraceTableName).Create(&rows).Error
	}
	if common.UsingLogDatabase(common.DatabaseTypeMySQL) {
		// 无关联写入，显式保证整批为单条 INSERT，由 InnoDB 保证语句原子性。
		db = db.Session(&gorm.Session{SkipDefaultTransaction: true, CreateBatchSize: len(entries)})
	}
	db = db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "request_id"}}, DoNothing: true})
	return db.Create(&entries).Error
}

func FindRequestTrace(ctx context.Context, requestID string, userID int) (*RequestTrace, error) {
	return findRequestTrace(ctx, requestID, userID, true)
}

func FindRequestTraceSummary(ctx context.Context, requestID string, userID int) (*RequestTrace, error) {
	return findRequestTrace(ctx, requestID, userID, false)
}

func findRequestTrace(ctx context.Context, requestID string, userID int, includeDetail bool) (*RequestTrace, error) {
	query := LOG_DB.WithContext(ctx).Model(&RequestTrace{})
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		// 显式别名避免 GORM 把 FINAL 识别为表别名。
		query = query.Table(requestTraceTableName + " AS request_trace FINAL")
	}
	query = query.Where("request_id = ?", requestID)
	if userID > 0 {
		query = query.Where("user_id = ?", userID)
	}
	if !includeDetail {
		query = query.Select("request_id", "started_at_us", "finished_at_us", "duration_us", "first_payload_write_offset_us", "http_status", "outcome", "is_stream", "capture_status", "schema_version")
	}
	var entry RequestTrace
	if err := query.Order("started_at_us DESC").Take(&entry).Error; err != nil {
		return nil, err
	}
	if entry.RequestId != requestID {
		return nil, gorm.ErrRecordNotFound
	}
	return &entry, nil
}

func CountOldRequestTimelines(ctx context.Context, cutoff int64) (int64, error) {
	var count int64
	err := LOG_DB.WithContext(ctx).Model(&RequestTrace{}).Where("started_at_us < ?", cutoff).Count(&count).Error
	return count, err
}

func DeleteOldRequestTimelines(ctx context.Context, cutoff int64, batchSize int) (int64, error) {
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		count, err := CountOldRequestTimelines(ctx, cutoff)
		if err != nil || count == 0 {
			return 0, err
		}
		return count, LOG_DB.WithContext(ctx).Exec("ALTER TABLE request_trace_timeline DELETE WHERE started_at_us < ? SETTINGS mutations_sync = 1", cutoff).Error
	}
	var ids []int64
	if err := LOG_DB.WithContext(ctx).Model(&RequestTrace{}).Where("started_at_us < ?", cutoff).Order("started_at_us").Order("id").Limit(batchSize).Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result := LOG_DB.WithContext(ctx).Where("id IN ? AND started_at_us < ?", ids, cutoff).Delete(&RequestTrace{})
	return result.RowsAffected, result.Error
}
