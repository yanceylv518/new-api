package requesttracetest

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var schemaSequence atomic.Uint64

// MySQLDSN 为显式启用的集成测试创建独立 schema，清理仅作用于本次生成的名称。
func MySQLDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("REQUEST_TRACE_TEST_DSN")
	if dsn == "" {
		t.Skip("REQUEST_TRACE_TEST_DSN is required for MySQL request trace integration tests")
	}
	isolated, cleanup, err := SuiteMySQLDSN(dsn, t.Name())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cleanup()) })
	return isolated
}

// SuiteMySQLDSN 支持 TestMain，共用相同的测试库隔离与清理边界。
func SuiteMySQLDSN(dsn, label string) (string, func() error, error) {
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", nil, err
	}
	if !strings.HasPrefix(config.DBName, "request_trace_") {
		return "", nil, errors.New("integration target must use a request_trace_ database")
	}
	config.DBName = ""
	config.ParseTime = true
	config.Timeout, config.ReadTimeout, config.WriteTimeout = 5*time.Second, 5*time.Second, 5*time.Second
	admin, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return "", nil, err
	}
	admin.SetMaxOpenConns(1)
	digest := sha256.Sum256(fmt.Appendf(nil, "%d:%s:%d", time.Now().UnixNano(), label, schemaSequence.Add(1)))
	schema := fmt.Sprintf("request_trace_test_%d_%x", os.Getpid(), digest[:8])
	_, err = admin.Exec("CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	if err != nil {
		return "", nil, errors.Join(err, admin.Close())
	}
	cleanup := func() error {
		_, dropErr := admin.Exec("DROP DATABASE `" + schema + "`")
		return errors.Join(dropErr, admin.Close())
	}
	config.DBName = schema
	return config.FormatDSN(), cleanup, nil
}
