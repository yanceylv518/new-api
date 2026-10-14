package model

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestTokenResourcePolicyResolve(t *testing.T) {
	body, concurrent, zeroBody, zeroConcurrent := int64(1024), 2, int64(0), 0
	defaults := TokenResourceLimits{MaxRequestBodyBytes: 4096, MaxConcurrentRequests: 8}
	for _, test := range []struct {
		name   string
		policy *TokenResourcePolicy
		want   TokenResourceLimits
	}{
		{"missing", nil, defaults},
		{"inherit_both", &TokenResourcePolicy{}, defaults},
		{"override_body", &TokenResourcePolicy{MaxRequestBodyBytes: &body}, TokenResourceLimits{1024, 8}},
		{"override_concurrency", &TokenResourcePolicy{MaxConcurrentRequests: &concurrent}, TokenResourceLimits{4096, 2}},
		{"unlimited_body", &TokenResourcePolicy{MaxRequestBodyBytes: &zeroBody}, TokenResourceLimits{0, 8}},
		{"unlimited_concurrency", &TokenResourcePolicy{MaxConcurrentRequests: &zeroConcurrent}, TokenResourceLimits{4096, 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, test.policy.Resolve(defaults))
		})
	}
}

// Each fixture uses its own table prefix, including on externally configured
// test databases. Never migrate or drop the application's existing tables.
func openTokenResourcePolicyTestDB(t *testing.T, dialect common.DatabaseType) (*gorm.DB, *migrationSQLRecorder) {
	t.Helper()
	var dialector gorm.Dialector
	versionQuery := "SELECT sqlite_version()"
	switch dialect {
	case common.DatabaseTypeSQLite:
		dialector = sqlite.Open(filepath.Join(t.TempDir(), "policy.db"))
	case common.DatabaseTypeMySQL:
		dsn := strings.TrimSpace(os.Getenv("TEST_MYSQL_DSN"))
		if dsn == "" {
			t.Skip("TEST_MYSQL_DSN is not configured")
		}
		dialector = mysqlMigrationDialector{mysql.Dialector{Config: &mysql.Config{DSN: dsn}}}
		versionQuery = "SELECT version()"
	case common.DatabaseTypePostgreSQL:
		dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		dialector = postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{DSN: dsn, PreferSimpleProtocol: true}}}
		versionQuery = "SELECT version()"
	default:
		t.Fatalf("unsupported test dialect: %s", dialect)
	}
	recorder := &migrationSQLRecorder{}
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger:         recorder,
		NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("trp_%d_", time.Now().UnixNano())},
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	previousDB, previousType := DB, common.MainDatabaseType()
	previousGroup, previousKey, previousTrue, previousFalse := commonGroupCol, commonKeyCol, commonTrueVal, commonFalseVal
	previousLogGroup, previousLogKey := logGroupCol, logKeyCol
	DB = db
	common.SetMainDatabaseType(dialect)
	initCol()
	t.Cleanup(func() {
		DB = previousDB
		common.SetMainDatabaseType(previousType)
		commonGroupCol, commonKeyCol, commonTrueVal, commonFalseVal = previousGroup, previousKey, previousTrue, previousFalse
		logGroupCol, logKeyCol = previousLogGroup, previousLogKey
		defer sqlDB.Close()
		require.NoError(t, db.Migrator().DropTable(&TokenResourcePolicy{}, &Token{}))
	})
	var version string
	require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database version: %s", version)
	return db, recorder
}

func TestTokenResourcePolicyDatabase(t *testing.T) {
	for _, dialect := range []common.DatabaseType{common.DatabaseTypeSQLite, common.DatabaseTypeMySQL, common.DatabaseTypePostgreSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			t.Run("fresh", func(t *testing.T) {
				db, recorder := openTokenResourcePolicyTestDB(t, dialect)
				require.NoError(t, db.AutoMigrate(&TokenResourcePolicy{}))
				zeroBody, zeroConcurrent := int64(0), 0
				policy := TokenResourcePolicy{TokenID: 42, MaxRequestBodyBytes: &zeroBody, MaxConcurrentRequests: &zeroConcurrent}
				require.NoError(t, db.Create(&policy).Error)
				for range 2 {
					recorder.reset()
					require.NoError(t, db.AutoMigrate(&TokenResourcePolicy{}))
					assert.Empty(t, recorder.schemaMutations(), "restarting must not alter an already migrated policy table")
				}
				got, err := GetTokenResourcePolicy(context.Background(), policy.TokenID)
				require.NoError(t, err)
				assert.Equal(t, &policy, got)
				assert.Error(t, db.Create(&policy).Error, "one policy per token must remain enforced after migration")
			})
			t.Run("upgrade_and_crud", func(t *testing.T) {
				db, recorder := openTokenResourcePolicyTestDB(t, dialect)
				testTokenResourcePolicyUpgrade(t, db, recorder)
			})
		})
	}
}

func testTokenResourcePolicyUpgrade(t *testing.T, db *gorm.DB, recorder *migrationSQLRecorder) {
	t.Helper()
	ctx := context.Background()
	// Token is unchanged from the base branch: create the pre-feature schema
	// and data before adding the new table, then exercise the old Token API.
	require.NoError(t, db.AutoMigrate(&Token{}))
	token := Token{UserId: 123, Key: "test-resource-policy-key", Name: "existing", RemainQuota: 456}
	require.NoError(t, db.Create(&token).Error)
	var original Token
	require.NoError(t, db.First(&original, token.Id).Error)
	columnsBefore, err := db.Migrator().ColumnTypes(&Token{})
	require.NoError(t, err)
	_, err = GetTokenResourcePolicy(ctx, token.Id)
	require.Error(t, err, "missing schema must not be treated as an absent policy")
	require.NoError(t, db.AutoMigrate(&TokenResourcePolicy{}))
	for range 2 {
		recorder.reset()
		require.NoError(t, db.AutoMigrate(&TokenResourcePolicy{}))
		assert.Empty(t, recorder.schemaMutations())
	}
	got, err := GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Nil(t, got)

	body, concurrent := int64(5)<<30, 3 // Verify BIGINT storage above 32 bits.
	policy := TokenResourcePolicy{TokenID: token.Id, MaxRequestBodyBytes: &body, MaxConcurrentRequests: &concurrent}
	require.NoError(t, SetTokenResourcePolicy(ctx, policy))
	got, err = GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got)
	other := Token{UserId: token.UserId, Key: "test-resource-policy-other"}
	require.NoError(t, db.Create(&other).Error)
	missing, err := GetTokenResourcePolicy(ctx, other.Id)
	require.NoError(t, err)
	assert.Nil(t, missing, "policies belong to a key, not its user")
	items, err := GetUserTokenResourcePolicies(ctx, token.UserId, 0, 1)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, other.Id, items[0].TokenID)
	assert.Nil(t, items[0].MaxRequestBodyBytes)
	items, err = GetUserTokenResourcePolicies(ctx, token.UserId, 1, 1)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, policy, items[0].TokenResourcePolicy)
	assert.Equal(t, token.Name, items[0].Name)
	items, err = GetUserTokenResourcePolicies(ctx, token.UserId+1, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, items, "admin policy pages must remain scoped to their target user")

	zeroBody, zeroConcurrent := int64(0), 0
	policy.MaxRequestBodyBytes, policy.MaxConcurrentRequests = &zeroBody, &zeroConcurrent
	require.NoError(t, SetTokenResourcePolicy(ctx, policy))
	got, err = GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "updates must persist explicit zero values")
	policy.MaxRequestBodyBytes = nil
	require.NoError(t, SetTokenResourcePolicy(ctx, policy))
	got, err = GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "updates must clear an override to NULL")
	policy.MaxRequestBodyBytes, policy.MaxConcurrentRequests = &body, nil
	require.NoError(t, SetTokenResourcePolicy(ctx, policy))
	got, err = GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "concurrency can inherit while the body limit remains overridden")

	negativeBody, negativeConcurrent := int64(-1), -1
	invalid := []TokenResourcePolicy{
		{TokenID: 0}, {TokenID: -1},
		{TokenID: token.Id, MaxRequestBodyBytes: &negativeBody},
		{TokenID: token.Id, MaxConcurrentRequests: &negativeConcurrent},
	}
	if strconv.IntSize > 32 {
		overflow := int64(math.MaxInt32) + 1
		concurrentOverflow := int(overflow)
		invalid = append(invalid, TokenResourcePolicy{TokenID: token.Id, MaxConcurrentRequests: &concurrentOverflow})
	}
	for _, candidate := range invalid {
		assert.ErrorIs(t, SetTokenResourcePolicy(ctx, candidate), ErrInvalidTokenResourcePolicy)
	}
	got, err = GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "rejected writes must preserve the policy")
	assert.ErrorIs(t, SetTokenResourcePolicy(ctx, TokenResourcePolicy{TokenID: other.Id + 1, MaxRequestBodyBytes: &body}), gorm.ErrRecordNotFound)
	_, err = GetTokenResourcePolicy(ctx, 0)
	assert.ErrorIs(t, err, ErrInvalidTokenResourcePolicy)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = GetTokenResourcePolicy(canceled, token.Id)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, SetTokenResourcePolicy(canceled, policy), context.Canceled)

	// Verify the pre-feature token row, columns and key uniqueness survived.
	var preserved Token
	require.NoError(t, db.First(&preserved, token.Id).Error)
	assert.Equal(t, original, preserved)
	columnsAfter, err := db.Migrator().ColumnTypes(&Token{})
	require.NoError(t, err)
	require.Len(t, columnsAfter, len(columnsBefore))
	for index, column := range columnsBefore {
		assert.Equal(t, column.Name(), columnsAfter[index].Name())
		assert.Equal(t, column.DatabaseTypeName(), columnsAfter[index].DatabaseTypeName())
	}
	assert.Error(t, db.Create(&Token{Key: token.Key}).Error)
	require.NoError(t, db.Model(&Token{}).Where("id = ?", token.Id).Update("name", "old-version-update").Error)
	got, err = GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "old token writes must leave the policy unchanged")

	for range 2 {
		require.NoError(t, SetTokenResourcePolicy(ctx, TokenResourcePolicy{TokenID: token.Id}))
	}
	got, err = GetTokenResourcePolicy(ctx, token.Id)
	require.NoError(t, err)
	assert.Nil(t, got, "reset restores inheritance by deleting the override")
	require.NoError(t, db.Delete(&token).Error)
	items, err = GetUserTokenResourcePolicies(ctx, token.UserId, 0, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, other.Id, items[0].TokenID, "soft-deleted keys must not appear in policy management")
	assert.ErrorIs(t, SetTokenResourcePolicy(ctx, policy), gorm.ErrRecordNotFound)
	var count int64
	require.NoError(t, db.Model(&TokenResourcePolicy{}).Count(&count).Error)
	assert.Zero(t, count, "a deleted token must not acquire a policy")
}
