package model

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// 时间线表只迁移已声明的列，扩展详情放在版本化 JSON，不使用 AutoMigrate 推断结构。
func migrateTraceRelationalSchema(db *gorm.DB, value any, payload string) error {
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(value); err != nil {
		return err
	}
	table := statement.Schema.Table
	if !db.Migrator().HasTable(value) {
		create := db
		if db.Dialector.Name() == "mysql" {
			create = db.Set("gorm:table_options", "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")
		}
		if err := create.Migrator().CreateTable(value); err != nil && !db.Migrator().HasTable(value) {
			return err
		}
	}
	columns, err := db.Migrator().ColumnTypes(value)
	if err != nil {
		return err
	}
	if len(columns) != len(statement.Schema.DBNames) {
		return fmt.Errorf("%s column contract mismatch", table)
	}
	actual := make(map[string]gorm.ColumnType, len(columns))
	for _, column := range columns {
		actual[column.Name()] = column
	}
	requestIDCollation, payloadCharset := "", ""
	if db.Dialector.Name() == "mysql" {
		if err := db.Raw("SELECT COLLATION_NAME FROM information_schema.columns WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = 'request_id'", table).Scan(&requestIDCollation).Error; err != nil {
			return err
		}
		if err := db.Raw("SELECT CHARACTER_SET_NAME FROM information_schema.columns WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?", table, payload).Scan(&payloadCharset).Error; err != nil {
			return err
		}
	}
	var changes []*schema.Field
	for _, name := range statement.Schema.DBNames {
		column, exists := actual[name]
		if !exists {
			return fmt.Errorf("%s is missing required column %s", table, name)
		}
		field := statement.Schema.FieldsByDBName[name]
		if field.PrimaryKey {
			if primary, known := column.PrimaryKey(); known && !primary {
				return fmt.Errorf("%s.%s is not a primary key; resolve the schema before migration", table, name)
			}
		}
		target := traceColumnType(db, field, payload)
		current := normalizeTraceColumnType(column.DatabaseTypeName())
		matches := current == normalizeTraceColumnType(target)
		if strings.HasPrefix(target, "varchar(") {
			length, known := column.Length()
			matches = current == "varchar" && known && length == int64(field.Size)
		}
		if db.Dialector.Name() == "sqlite" {
			matches = current == "integer" || current == "int" || current == "bigint"
			if field.DataType == schema.String || field.DataType == "text" || strings.HasPrefix(target, "varchar") {
				matches = current == "text" || current == "varchar"
			}
			if field.DataType == schema.Bool {
				matches = matches || current == "boolean" || current == "numeric"
			}
		}
		if nullable, known := column.Nullable(); known && !field.PrimaryKey && nullable != !field.NotNull {
			matches = false
		}
		if name == "request_id" && db.Dialector.Name() == "mysql" && requestIDCollation != "utf8mb4_bin" {
			matches = false
		}
		if name == payload && db.Dialector.Name() == "mysql" && payloadCharset != "utf8mb4" {
			matches = false
		}
		if fullType, known := column.ColumnType(); known && strings.Contains(strings.ToLower(fullType), "unsigned") {
			matches = false
		}
		if !matches {
			textType := current == "varchar" || current == "text" || current == "mediumtext" || current == "longtext"
			numericType := current == "tinyint" || current == "smallint" || current == "mediumint" || current == "int" || current == "bigint" || current == "boolean" || current == "numeric"
			textTarget := target == "text" || target == "longtext" || strings.HasPrefix(target, "varchar(")
			if textTarget && !textType || !textTarget && !numericType {
				return fmt.Errorf("%s.%s has unsupported type %s; resolve the schema before migration", table, name, column.DatabaseTypeName())
			}
			if err := checkTraceColumnMigration(db, table, field, target); err != nil {
				return err
			}
			changes = append(changes, field)
		}
	}
	if db.Dialector.Name() == "sqlite" && len(changes) > 0 {
		if err := alterTraceSQLiteColumns(db, value, table, changes); err != nil {
			return err
		}
		changes = nil
	}
	for _, field := range changes {
		target := traceColumnType(db, field, payload)
		switch {
		case db.Dialector.Name() == "mysql" && field.DBName == payload:
			err = db.Exec("ALTER TABLE ? MODIFY COLUMN ? LONGTEXT CHARACTER SET utf8mb4 NOT NULL", clause.Table{Name: table}, clause.Column{Name: payload}).Error
		case db.Dialector.Name() == "mysql" && field.DBName == "request_id":
			err = db.Exec("ALTER TABLE ? MODIFY COLUMN ? VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL", clause.Table{Name: table}, clause.Column{Name: field.DBName}).Error
		default:
			err = db.Migrator().AlterColumn(value, field.Name)
		}
		if err != nil {
			return fmt.Errorf("migrate %s.%s to %s: %w", table, field.DBName, target, err)
		}
	}
	indexes, err := db.Migrator().GetIndexes(value)
	if err != nil {
		return err
	}
	for name, expected := range statement.Schema.ParseIndexes() {
		fields := make([]string, 0, len(expected.Fields))
		for _, field := range expected.Fields {
			fields = append(fields, field.DBName)
		}
		found := false
		for _, index := range indexes {
			if index.Name() != name {
				continue
			}
			unique, known := index.Unique()
			if slices.Equal(index.Columns(), fields) && known && unique == (expected.Class == "UNIQUE") {
				found = true
				break
			}
			if err := db.Migrator().DropIndex(value, name); err != nil {
				return err
			}
			break
		}
		if !found {
			if err := db.Migrator().CreateIndex(value, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// SQLite 的列迁移会重建表；同时保留自定义索引、触发器和已使用的自增序号。
func alterTraceSQLiteColumns(db *gorm.DB, value any, table string, fields []*schema.Field) error {
	var definitions []struct {
		Name string
		SQL  string
	}
	if err := db.Raw("SELECT name, sql FROM sqlite_master WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL", table).Scan(&definitions).Error; err != nil {
		return err
	}
	var sequence int64
	if err := db.Raw("SELECT seq FROM sqlite_sequence WHERE name = ?", table).Scan(&sequence).Error; err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, field := range fields {
			if err := tx.Migrator().AlterColumn(value, field.Name); err != nil {
				return err
			}
		}
		for _, definition := range definitions {
			var exists int
			if err := tx.Raw("SELECT 1 FROM sqlite_master WHERE tbl_name = ? AND name = ?", table, definition.Name).Scan(&exists).Error; err != nil {
				return err
			}
			if exists == 0 {
				if err := tx.Exec(definition.SQL).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Exec("UPDATE sqlite_sequence SET seq = ? WHERE name = ? AND seq < ?", sequence, table, sequence).Error; err != nil {
			return err
		}
		return tx.Exec("INSERT INTO sqlite_sequence(name, seq) SELECT ?, ? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = ?)", table, sequence, table).Error
	})
}

func traceColumnType(db *gorm.DB, field *schema.Field, payload string) string {
	if field.PrimaryKey {
		return "bigint"
	}
	if db.Dialector.Name() == "mysql" && field.DBName == payload {
		return "longtext"
	}
	if field.DataType == schema.Bool {
		if db.Dialector.Name() == "mysql" {
			return "tinyint"
		}
		return "boolean"
	}
	return strings.ToLower(field.TagSettings["TYPE"])
}

func normalizeTraceColumnType(value string) string {
	value = strings.ToLower(value)
	if index := strings.IndexByte(value, '('); index >= 0 {
		value = value[:index]
	}
	switch value {
	case "int8":
		return "bigint"
	case "int4", "integer":
		return "int"
	case "bool":
		return "boolean"
	case "character varying":
		return "varchar"
	}
	return value
}

func checkTraceColumnMigration(db *gorm.DB, table string, field *schema.Field, target string) error {
	var invalid int
	query := db.Table(table).Select("1").Limit(1)
	if field.NotNull || field.PrimaryKey {
		if err := query.Where(clause.Eq{Column: field.DBName, Value: nil}).Scan(&invalid).Error; err != nil {
			return err
		}
		if invalid != 0 {
			return fmt.Errorf("%s.%s contains NULL; resolve historical rows before migration", table, field.DBName)
		}
	}
	invalid = 0
	query = db.Table(table).Select("1").Limit(1)
	if strings.HasPrefix(target, "varchar(") {
		lengthFunction := "CHAR_LENGTH"
		if db.Dialector.Name() == "sqlite" {
			lengthFunction = "LENGTH"
		}
		query = query.Where(lengthFunction+"(?) > ?", clause.Column{Name: field.DBName}, field.Size)
	} else if target == "boolean" || target == "tinyint" {
		query = query.Where("? NOT IN (0, 1)", clause.Column{Name: field.DBName})
	} else if target == "integer" {
		query = query.Where("? < ? OR ? > ?", clause.Column{Name: field.DBName}, math.MinInt32, clause.Column{Name: field.DBName}, math.MaxInt32)
	} else if target == "bigint" {
		query = query.Where("? > ?", clause.Column{Name: field.DBName}, int64(math.MaxInt64))
	} else {
		return nil
	}
	if err := query.Scan(&invalid).Error; err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("%s.%s exceeds the target type; resolve historical rows before migration", table, field.DBName)
	}
	return nil
}

func migrateTraceClickHouseColumns(db *gorm.DB, value any) error {
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(value); err != nil {
		return err
	}
	table := statement.Schema.Table
	expected := make(map[string]string, len(statement.Schema.DBNames))
	for _, name := range statement.Schema.DBNames {
		field := statement.Schema.FieldsByDBName[name]
		target := "String"
		switch {
		case field.PrimaryKey || field.TagSettings["TYPE"] == "bigint":
			target = "Int64"
		case field.TagSettings["TYPE"] == "integer":
			target = "Int32"
		case field.DataType == schema.Bool:
			target = "UInt8"
		}
		if !field.NotNull && !field.PrimaryKey {
			target = "Nullable(" + target + ")"
		}
		expected[name] = target
	}
	var columns []struct {
		Name string
		Type string
	}
	if err := db.Raw("SELECT name, type FROM system.columns WHERE database = currentDatabase() AND table = ?", table).Scan(&columns).Error; err != nil {
		return err
	}
	if len(columns) != len(expected) {
		return fmt.Errorf("%s column contract mismatch", table)
	}
	for _, column := range columns {
		target, exists := expected[column.Name]
		if !exists {
			return fmt.Errorf("%s has unexpected column %s", table, column.Name)
		}
		if column.Type == target {
			continue
		}
		if !(column.Type == "Int32" && target == "Int64") && !(column.Type == "UInt64" && target == "Int64") {
			return fmt.Errorf("%s.%s has unsupported type %s; expected %s", table, column.Name, column.Type, target)
		}
		if column.Type == "UInt64" && target == "Int64" {
			var invalid uint64
			if err := db.Table(table).Select("count()").Where("? > ?", clause.Column{Name: column.Name}, uint64(math.MaxInt64)).Scan(&invalid).Error; err != nil {
				return err
			}
			if invalid > 0 {
				return fmt.Errorf("%s.%s exceeds Int64", table, column.Name)
			}
		}
		if err := db.Exec("ALTER TABLE ? MODIFY COLUMN ? "+target, clause.Table{Name: table}, clause.Column{Name: column.Name}).Error; err != nil {
			return err
		}
	}
	return nil
}
