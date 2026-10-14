package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestChannelResourcePolicySelection(t *testing.T) {
	for _, dialect := range []common.DatabaseType{common.DatabaseTypeSQLite, common.DatabaseTypeMySQL, common.DatabaseTypePostgreSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := openChannelResourcePolicyRuntimeTestDB(t, dialect)
			ctx := context.Background()
			small := Channel{Name: "small", Type: constant.ChannelTypeOpenAI, Key: "fixture", Models: "body-model", Group: "default", Priority: common.GetPointer(int64(100))}
			large := Channel{Name: "large", Type: constant.ChannelTypeOpenAI, Key: "fixture", Models: "body-model", Group: "default", Priority: common.GetPointer(int64(10))}
			require.NoError(t, small.Insert())
			require.NoError(t, large.Insert())
			require.NoError(t, SetChannelResourcePolicy(ctx, ChannelResourcePolicy{ChannelID: small.Id, MaxRequestBodyBytes: 512}))
			require.NoError(t, SetChannelResourcePolicy(ctx, ChannelResourcePolicy{ChannelID: large.Id, MaxRequestBodyBytes: 2048}))
			InitChannelCache()
			for _, memory := range []bool{true, false} {
				common.MemoryCacheEnabled = memory
				for _, tc := range []struct {
					name      string
					size      int64
					retry     int
					modelName string
					want      int
					tooLarge  bool
				}{
					{"boundary", 512, 0, "body-model", small.Id, false},
					{"large skips high priority", 513, 0, "body-model", large.Id, false},
					{"retry never returns oversized channel", 513, 4, "body-model", large.Id, false},
					{"all too small", 2049, 0, "body-model", 0, true},
					{"unknown model is not 413", 2049, 0, "unknown", 0, false},
				} {
					t.Run(fmt.Sprintf("memory=%v/%s", memory, tc.name), func(t *testing.T) {
						ch, err := GetRandomSatisfiedChannel("default", tc.modelName, tc.retry, []dto.ChannelFilter{{Kind: dto.FilterRequestBodySize, RequestBodyBytes: tc.size}})
						if tc.tooLarge {
							assert.ErrorIs(t, err, ErrChannelRequestBodyTooLarge)
						} else {
							require.NoError(t, err)
						}
						if tc.want == 0 {
							assert.Nil(t, ch)
						} else {
							require.NotNil(t, ch)
							assert.Equal(t, tc.want, ch.Id)
						}
					})
				}
				// An incompatible plugin must not become a body-limit error.
				ch, err := GetRandomSatisfiedChannel("default", "body-model", 0, []dto.ChannelFilter{
					{Kind: dto.FilterRequestBodySize, RequestBodyBytes: 4096},
					{Kind: dto.FilterTaskPluginIdentity, TaskPluginKey: "unrelated"},
				})
				assert.Nil(t, ch)
				assert.NoError(t, err)
			}
			// No optional table: direct database selection remains available.
			require.NoError(t, db.Migrator().DropTable(&ChannelResourcePolicy{}))
			ch, err := GetRandomSatisfiedChannel("default", "body-model", 0, []dto.ChannelFilter{{Kind: dto.FilterRequestBodySize, RequestBodyBytes: 4096}})
			require.NoError(t, err)
			require.NotNil(t, ch)
			assert.Equal(t, small.Id, ch.Id)
			common.MemoryCacheEnabled = true
			assert.Error(t, RefreshChannelResourcePolicyCache(ctx))
			_, err = GetRandomSatisfiedChannel("default", "body-model", 0, []dto.ChannelFilter{{Kind: dto.FilterRequestBodySize, RequestBodyBytes: 4096}})
			assert.ErrorIs(t, err, ErrChannelRequestBodyTooLarge, "failed refresh retains the valid snapshot")
		})
	}
}

func TestChannelResourcePolicyDatabase(t *testing.T) {
	for _, dialect := range []common.DatabaseType{common.DatabaseTypeSQLite, common.DatabaseTypeMySQL, common.DatabaseTypePostgreSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			t.Run("fresh", func(t *testing.T) {
				// Reuse the isolated, uniquely prefixed database fixture. Its cleanup
				// closes the connection after this test's channel tables are removed.
				db, recorder := openTokenResourcePolicyTestDB(t, dialect)
				t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(&ChannelResourcePolicy{})) })
				require.NoError(t, db.AutoMigrate(&ChannelResourcePolicy{}))
				zero := ChannelResourcePolicy{ChannelID: 1}
				require.NoError(t, db.Create(&zero).Error)
				for range 2 {
					recorder.reset()
					require.NoError(t, db.AutoMigrate(&ChannelResourcePolicy{}))
					assert.Empty(t, recorder.schemaMutations(), "restarting must not alter the policy table")
				}
				got, err := GetChannelResourcePolicy(context.Background(), zero.ChannelID)
				require.NoError(t, err)
				assert.Equal(t, &zero, got, "explicit zero also means unlimited")
				assert.Error(t, db.Create(&zero).Error, "a channel may have only one policy")
			})
			t.Run("upgrade_and_crud", func(t *testing.T) {
				db, recorder := openTokenResourcePolicyTestDB(t, dialect)
				t.Cleanup(func() {
					require.NoError(t, db.Migrator().DropTable(&ChannelResourcePolicy{}, &Ability{}, &Channel{}))
				})
				testChannelResourcePolicyUpgrade(t, db, recorder)
			})
		})
	}
}

func openChannelResourcePolicyRuntimeTestDB(t *testing.T, dialect common.DatabaseType) *gorm.DB {
	t.Helper()
	db, _ := openTokenResourcePolicyTestDB(t, dialect)
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}, &ChannelResourcePolicy{}))
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	channelResourcePolicyCache.Lock()
	previousPolicies := channelResourcePolicyCache.policies
	channelResourcePolicyCache.policies = nil
	channelResourcePolicyCache.Unlock()
	channelSyncLock.RLock()
	previousChannels, previousGroups, previousAdvanced := channelsIDM, group2model2channels, channel2advancedCustomConfig
	channelSyncLock.RUnlock()
	previousAlias := taskAliasViewPtr.Load()
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousMemoryCacheEnabled
		channelResourcePolicyCache.Lock()
		channelResourcePolicyCache.policies = previousPolicies
		channelResourcePolicyCache.Unlock()
		channelSyncLock.Lock()
		channelsIDM, group2model2channels, channel2advancedCustomConfig = previousChannels, previousGroups, previousAdvanced
		channelSyncLock.Unlock()
		taskAliasViewPtr.Store(previousAlias)
		require.NoError(t, db.Migrator().DropTable(&ChannelResourcePolicy{}, &Ability{}, &Channel{}))
	})
	return db
}

func TestChannelResourcePolicyCache(t *testing.T) {
	for _, dialect := range []common.DatabaseType{common.DatabaseTypeSQLite, common.DatabaseTypeMySQL, common.DatabaseTypePostgreSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			db := openChannelResourcePolicyRuntimeTestDB(t, dialect)
			ctx := context.Background()
			channel := Channel{Name: "cached", Key: "fixture", Models: "test-model", Group: "default"}
			require.NoError(t, channel.Insert())
			ids := []int{channel.Id}

			// An unavailable policy table must not prevent ordinary channel routing.
			require.NoError(t, db.Migrator().DropTable(&ChannelResourcePolicy{}))
			InitChannelCache()
			assert.Empty(t, CacheGetChannelResourcePolicies(ctx, ids))
			selected, err := GetRandomSatisfiedChannel("default", "test-model", 0, nil)
			require.NoError(t, err)
			require.NotNil(t, selected)
			assert.Equal(t, channel.Id, selected.Id)
			require.NoError(t, db.AutoMigrate(&ChannelResourcePolicy{}))
			policy := ChannelResourcePolicy{ChannelID: channel.Id, MaxRequestBodyBytes: 4096}
			require.NoError(t, db.Create(&policy).Error) // another instance saves
			InitChannelCache()
			assert.Equal(t, policy, CacheGetChannelResourcePolicies(ctx, ids)[channel.Id])
			copyOfPolicies := CacheGetChannelResourcePolicies(ctx, ids)
			delete(copyOfPolicies, channel.Id)
			assert.Equal(t, policy, CacheGetChannelResourcePolicies(ctx, ids)[channel.Id], "callers cannot modify the shared snapshot")

			policy.MaxRequestBodyBytes = 1024
			require.NoError(t, SetChannelResourcePolicy(ctx, policy))
			assert.Equal(t, policy, CacheGetChannelResourcePolicies(ctx, ids)[channel.Id], "local saves update the cache immediately")
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			assert.Error(t, SetChannelResourcePolicy(canceled, ChannelResourcePolicy{ChannelID: channel.Id}))
			assert.Equal(t, policy, CacheGetChannelResourcePolicies(ctx, ids)[channel.Id], "failed saves must retain the cached limit")
			require.NoError(t, db.Model(&policy).Update("max_request_body_bytes", 2048).Error)
			assert.Equal(t, int64(1024), CacheGetChannelResourcePolicies(ctx, ids)[channel.Id].MaxRequestBodyBytes)
			InitChannelCache()
			assert.Equal(t, int64(2048), CacheGetChannelResourcePolicies(ctx, ids)[channel.Id].MaxRequestBodyBytes)
			CacheUpdateChannel(&channel)
			assert.Equal(t, int64(2048), CacheGetChannelResourcePolicies(ctx, ids)[channel.Id].MaxRequestBodyBytes, "ordinary channel refreshes cannot erase policies")

			require.NoError(t, db.Model(&policy).Update("max_request_body_bytes", -1).Error)
			assert.ErrorIs(t, RefreshChannelResourcePolicyCache(ctx), ErrInvalidChannelResourcePolicy)
			assert.Equal(t, int64(2048), CacheGetChannelResourcePolicies(ctx, ids)[channel.Id].MaxRequestBodyBytes)
			require.NoError(t, db.Migrator().DropTable(&ChannelResourcePolicy{}))
			assert.Error(t, RefreshChannelResourcePolicyCache(ctx))
			assert.Equal(t, int64(2048), CacheGetChannelResourcePolicies(ctx, ids)[channel.Id].MaxRequestBodyBytes, "failed refreshes retain the last valid snapshot")
			_, err = GetChannelResourcePolicy(ctx, channel.Id)
			assert.Error(t, err, "admin reads must report failures rather than show unlimited")

			common.MemoryCacheEnabled = false
			assert.Empty(t, CacheGetChannelResourcePolicies(ctx, ids), "direct-query failures do not block relay selection")
			require.NoError(t, db.AutoMigrate(&ChannelResourcePolicy{}))
			policy.MaxRequestBodyBytes = 512
			require.NoError(t, SetChannelResourcePolicy(ctx, policy))
			assert.Equal(t, policy, CacheGetChannelResourcePolicies(ctx, ids)[channel.Id], "without memory caching, reads use current database values")
			common.MemoryCacheEnabled = true
			require.NoError(t, SetChannelResourcePolicy(ctx, ChannelResourcePolicy{ChannelID: channel.Id}))
			assert.Empty(t, CacheGetChannelResourcePolicies(ctx, ids), "canceling the limit clears it locally")
			require.NoError(t, SetChannelResourcePolicy(ctx, policy))
			require.NoError(t, db.Delete(&policy).Error) // another instance cancels
			require.NoError(t, RefreshChannelResourcePolicyCache(ctx))
			assert.Empty(t, CacheGetChannelResourcePolicies(ctx, ids), "successful refreshes remove canceled policies")
		})
	}
}

func TestChannelResourcePolicyDeletion(t *testing.T) {
	for _, dialect := range []common.DatabaseType{common.DatabaseTypeSQLite, common.DatabaseTypeMySQL, common.DatabaseTypePostgreSQL} {
		t.Run(string(dialect), func(t *testing.T) {
			for _, test := range []struct {
				name    string
				delete  func([]Channel) (int64, error)
				deleted []int
			}{
				{"single", func(channels []Channel) (int64, error) {
					err := channels[0].Delete()
					if err != nil {
						return 0, err
					}
					return 1, nil
				}, []int{0}},
				{"batch", func(channels []Channel) (int64, error) {
					return BatchDeleteChannels([]int{channels[0].Id, channels[1].Id})
				}, []int{0, 1}},
				{"disabled", func([]Channel) (int64, error) { return DeleteDisabledChannel() }, []int{1, 2}},
				{"by_status", func([]Channel) (int64, error) { return DeleteChannelByStatus(common.ChannelStatusAutoDisabled) }, []int{1}},
			} {
				t.Run(test.name, func(t *testing.T) {
					db := openChannelResourcePolicyRuntimeTestDB(t, dialect)
					ctx := context.Background()
					channels := []Channel{
						{Name: "enabled", Key: "fixture", Models: "test-model", Group: "default", Status: common.ChannelStatusEnabled},
						{Name: "auto-disabled", Key: "fixture", Models: "test-model", Group: "default", Status: common.ChannelStatusAutoDisabled},
						{Name: "manually-disabled", Key: "fixture", Models: "test-model", Group: "default", Status: common.ChannelStatusManuallyDisabled},
					}
					ids := make([]int, len(channels))
					for index := range channels {
						require.NoError(t, channels[index].Insert())
						ids[index] = channels[index].Id
						require.NoError(t, SetChannelResourcePolicy(ctx, ChannelResourcePolicy{ChannelID: ids[index], MaxRequestBodyBytes: 1024}))
					}
					// Fail after channels and abilities have been deleted in the transaction.
					failure := errors.New("policy cleanup unavailable")
					const callback = "channel-policy-delete-failure"
					require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "ChannelResourcePolicy" {
							tx.AddError(failure)
						}
					}))
					t.Cleanup(func() { _ = db.Callback().Delete().Remove(callback) })
					count, err := test.delete(channels)
					assert.ErrorIs(t, err, failure)
					assert.Zero(t, count)
					for _, table := range []any{&Channel{}, &Ability{}, &ChannelResourcePolicy{}} {
						var remaining int64
						require.NoError(t, db.Model(table).Count(&remaining).Error)
						assert.Equal(t, int64(3), remaining, "failed cleanup must roll back every related table")
					}
					assert.Len(t, CacheGetChannelResourcePolicies(ctx, ids), 3, "rollback must not invalidate cached policies")
					require.NoError(t, db.Callback().Delete().Remove(callback))
					count, err = test.delete(channels)
					require.NoError(t, err)
					assert.Equal(t, int64(len(test.deleted)), count)
					remainingPolicies := CacheGetChannelResourcePolicies(ctx, ids)
					assert.Len(t, remainingPolicies, len(channels)-len(test.deleted))
					for _, index := range test.deleted {
						var channel Channel
						assert.ErrorIs(t, db.First(&channel, ids[index]).Error, gorm.ErrRecordNotFound)
						policy, err := GetChannelResourcePolicy(ctx, ids[index])
						require.NoError(t, err)
						assert.Nil(t, policy)
						assert.NotContains(t, remainingPolicies, ids[index])
						var abilityCount int64
						require.NoError(t, db.Model(&Ability{}).Where("channel_id = ?", ids[index]).Count(&abilityCount).Error)
						assert.Zero(t, abilityCount)
						assert.ErrorIs(t, SetChannelResourcePolicy(ctx, ChannelResourcePolicy{ChannelID: ids[index], MaxRequestBodyBytes: 1}), gorm.ErrRecordNotFound)
					}
				})
			}
		})
	}
}

func TestChannelResourcePolicyErrorLogging(t *testing.T) {
	db := openChannelResourcePolicyRuntimeTestDB(t, common.DatabaseTypeSQLite)
	require.NoError(t, db.Migrator().DropTable(&ChannelResourcePolicy{}))
	var output bytes.Buffer
	previousDB := DB
	DB = db.Session(&gorm.Session{Logger: logger.New(log.New(&output, "", 0), logger.Config{LogLevel: logger.Error})})
	common.LogWriterMu.Lock()
	previousWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &output
	common.LogWriterMu.Unlock()
	channelResourcePolicyLogMu.Lock()
	previousLogTime := channelResourcePolicyLastErrorLog
	channelResourcePolicyLastErrorLog = time.Time{}
	channelResourcePolicyLogMu.Unlock()
	t.Cleanup(func() {
		DB = previousDB
		common.LogWriterMu.Lock()
		gin.DefaultErrorWriter = previousWriter
		common.LogWriterMu.Unlock()
		channelResourcePolicyLogMu.Lock()
		channelResourcePolicyLastErrorLog = previousLogTime
		channelResourcePolicyLogMu.Unlock()
	})
	for range 2 {
		require.Error(t, RefreshChannelResourcePolicyCache(context.Background()))
	}
	common.MemoryCacheEnabled = false
	for range 2 {
		assert.Empty(t, CacheGetChannelResourcePolicies(context.Background(), []int{1}))
	}
	assert.Equal(t, 1, strings.Count(output.String(), "channel resource policy read failed"), "repeated failures must not flood backend logs")
	assert.Equal(t, 1, strings.Count(output.String(), "no such table"), "ORM error logging must not bypass the log rate limit")
	assert.Contains(t, output.String(), "bypassing channel size filtering")
}

func testChannelResourcePolicyUpgrade(t *testing.T, db *gorm.DB, recorder *migrationSQLRecorder) {
	t.Helper()
	ctx := context.Background()
	// Channel and Ability retain the base version's schema. Create existing
	// data before adding the separate policy table, then exercise old writes.
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	channel := Channel{Name: "existing", Key: "test-channel-policy-key", Models: "test-model", Group: "default"}
	require.NoError(t, channel.Insert())
	var original Channel
	require.NoError(t, db.First(&original, channel.Id).Error)
	columnsBefore, err := db.Migrator().ColumnTypes(&Channel{})
	require.NoError(t, err)
	indexesBefore, err := db.Migrator().GetIndexes(&Channel{})
	require.NoError(t, err)
	var abilitiesBefore []Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilitiesBefore).Error)
	require.NotEmpty(t, abilitiesBefore)

	_, err = GetChannelResourcePolicy(ctx, channel.Id)
	require.Error(t, err, "an unavailable table must be distinguishable from an absent policy")
	_, err = GetChannelResourcePolicies(ctx, []int{channel.Id})
	require.Error(t, err)
	require.NoError(t, db.AutoMigrate(&ChannelResourcePolicy{}))
	got, err := GetChannelResourcePolicy(ctx, channel.Id)
	require.NoError(t, err)
	assert.Nil(t, got, "upgrading must not add limits to existing channels")

	policy := ChannelResourcePolicy{ChannelID: channel.Id, MaxRequestBodyBytes: int64(5) << 30}
	require.NoError(t, SetChannelResourcePolicy(ctx, policy))
	got, err = GetChannelResourcePolicy(ctx, channel.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "limits larger than 32 bits must round-trip")
	policy.MaxRequestBodyBytes = 1
	require.NoError(t, SetChannelResourcePolicy(ctx, policy))
	got, err = GetChannelResourcePolicy(ctx, channel.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "saving again must replace the previous limit")

	for range 2 {
		recorder.reset()
		require.NoError(t, db.AutoMigrate(&ChannelResourcePolicy{}))
		assert.Empty(t, recorder.schemaMutations())
	}
	got, err = GetChannelResourcePolicy(ctx, channel.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "repeated migrations must retain existing policies")
	var preserved Channel
	require.NoError(t, db.First(&preserved, channel.Id).Error)
	assert.Equal(t, original, preserved)
	columnsAfter, err := db.Migrator().ColumnTypes(&Channel{})
	require.NoError(t, err)
	require.Len(t, columnsAfter, len(columnsBefore))
	for index, column := range columnsBefore {
		assert.Equal(t, column.Name(), columnsAfter[index].Name())
		assert.Equal(t, column.DatabaseTypeName(), columnsAfter[index].DatabaseTypeName())
	}
	indexesAfter, err := db.Migrator().GetIndexes(&Channel{})
	require.NoError(t, err)
	assert.ElementsMatch(t, indexesBefore, indexesAfter, "index enumeration order is database-dependent")
	var abilitiesAfter []Ability
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&abilitiesAfter).Error)
	assert.Equal(t, abilitiesBefore, abilitiesAfter)

	channel.Name = "ordinary-edit"
	require.NoError(t, channel.Update())
	got, err = GetChannelResourcePolicy(ctx, channel.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "ordinary channel edits must not change the policy")
	clone := channel
	clone.Id = 0
	clone.Name = "copy-without-policy"
	require.NoError(t, clone.Insert())
	got, err = GetChannelResourcePolicy(ctx, clone.Id)
	require.NoError(t, err)
	assert.Nil(t, got, "copying a channel must not copy its policy")

	policies, err := GetChannelResourcePolicies(ctx, []int{channel.Id, clone.Id, channel.Id})
	require.NoError(t, err)
	assert.Equal(t, map[int]ChannelResourcePolicy{channel.Id: policy}, policies)
	policies, err = GetChannelResourcePolicies(ctx, []int{clone.Id})
	require.NoError(t, err)
	assert.Empty(t, policies, "candidate reads must not load policies for unrelated channels")
	policies, err = GetChannelResourcePolicies(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, policies, "an empty candidate set must not load every policy")

	for _, invalid := range []ChannelResourcePolicy{
		{ChannelID: 0}, {ChannelID: -1},
		{ChannelID: channel.Id, MaxRequestBodyBytes: -1},
	} {
		assert.ErrorIs(t, SetChannelResourcePolicy(ctx, invalid), ErrInvalidChannelResourcePolicy)
	}
	_, err = GetChannelResourcePolicy(ctx, 0)
	assert.ErrorIs(t, err, ErrInvalidChannelResourcePolicy)
	_, err = GetChannelResourcePolicies(ctx, []int{channel.Id, -1})
	assert.ErrorIs(t, err, ErrInvalidChannelResourcePolicy)
	assert.ErrorIs(t, SetChannelResourcePolicy(ctx, ChannelResourcePolicy{ChannelID: clone.Id + 1, MaxRequestBodyBytes: 1}), gorm.ErrRecordNotFound)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = GetChannelResourcePolicy(canceled, channel.Id)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = GetChannelResourcePolicies(canceled, []int{channel.Id})
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, SetChannelResourcePolicy(canceled, policy), context.Canceled)
	got, err = GetChannelResourcePolicy(ctx, channel.Id)
	require.NoError(t, err)
	assert.Equal(t, &policy, got, "invalid or canceled saves must leave the limit unchanged")

	// Invalid persisted data is a read failure, not a successfully loaded limit.
	require.NoError(t, db.Model(&ChannelResourcePolicy{}).Where("channel_id = ?", channel.Id).Update("max_request_body_bytes", -1).Error)
	_, err = GetChannelResourcePolicy(ctx, channel.Id)
	assert.ErrorIs(t, err, ErrInvalidChannelResourcePolicy)
	policies, err = GetChannelResourcePolicies(ctx, []int{channel.Id})
	assert.ErrorIs(t, err, ErrInvalidChannelResourcePolicy)
	assert.Nil(t, policies)
	require.NoError(t, SetChannelResourcePolicy(ctx, policy))
	for range 2 {
		require.NoError(t, SetChannelResourcePolicy(ctx, ChannelResourcePolicy{ChannelID: channel.Id}))
	}
	got, err = GetChannelResourcePolicy(ctx, channel.Id)
	require.NoError(t, err)
	assert.Nil(t, got, "saving zero cancels the channel limit")
	var count int64
	require.NoError(t, db.Model(&ChannelResourcePolicy{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, channel.Delete())
	assert.ErrorIs(t, SetChannelResourcePolicy(ctx, policy), gorm.ErrRecordNotFound)

	// Cross the query batch boundary explicitly. Both the first and last
	// candidate must be included, and later-batch errors must discard all data.
	ids := make([]int, 201)
	for index := range ids {
		ids[index] = 1000 + index
	}
	first := ChannelResourcePolicy{ChannelID: ids[0], MaxRequestBodyBytes: 1024}
	last := ChannelResourcePolicy{ChannelID: ids[len(ids)-1], MaxRequestBodyBytes: 2048}
	require.NoError(t, db.Create(&[]ChannelResourcePolicy{first, last}).Error)
	policies, err = GetChannelResourcePolicies(ctx, ids)
	require.NoError(t, err)
	assert.Equal(t, map[int]ChannelResourcePolicy{first.ChannelID: first, last.ChannelID: last}, policies)
	require.NoError(t, db.Model(&last).Update("max_request_body_bytes", -1).Error)
	policies, err = GetChannelResourcePolicies(ctx, ids)
	assert.ErrorIs(t, err, ErrInvalidChannelResourcePolicy)
	assert.Nil(t, policies, "a failed batch must not publish a partial policy snapshot")
}
