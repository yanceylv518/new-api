package model

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/go-redis/redis/v8"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestHardDeleteUserFailsClosedWhenAuthFenceCannotPublish(t *testing.T) {
	truncateTables(t)

	user := User{Username: "hard-delete-user", Password: "password", TelegramId: "hard-delete-telegram"}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		return ClaimExternalIdentityWithTx(tx, ExternalIdentityProviderTelegram, user.TelegramId, user.Id)
	}))
	require.NoError(t, DB.Create(&Token{UserId: user.Id, Key: "hard-delete-token"}).Error)
	require.NoError(t, DB.Create(&UserAccessToken{UserId: user.Id, TokenHash: AccessTokenFingerprint("nap_hard-delete-token")}).Error)
	require.NoError(t, DB.Create(&TwoFA{UserId: user.Id, Secret: "secret", IsEnabled: true}).Error)
	require.NoError(t, DB.Create(&TwoFABackupCode{UserId: user.Id, CodeHash: "hash"}).Error)
	require.NoError(t, DB.Create(&PasskeyCredential{UserID: user.Id, CredentialID: "credential", PublicKey: "public-key"}).Error)
	require.NoError(t, DB.Create(&UserOAuthBinding{UserId: user.Id, ProviderId: 1, ProviderUserId: "provider-user"}).Error)
	require.NoError(t, DB.Create(&UserSession{
		SID: "hard-delete-session", UserID: user.Id, Version: 1, UserAuthVersion: 1,
		Status: UserSessionStatusActive, RefreshHash: "refresh-hash", LoginMethod: "password",
		LastActiveAt: 1, ExpiresAt: 2,
	}).Error)
	require.NoError(t, DB.Create(&AuthFlow{
		TokenHash: "hard-delete-auth-flow", Purpose: AuthFlowPurposeTwoFALogin,
		UserId: user.Id, ExpiresAt: time.Now().Add(time.Minute),
	}).Error)

	oldRedisEnabled, oldRDB := common.RedisEnabled, common.RDB
	common.RedisEnabled = true
	common.RDB = redis.NewClient(&redis.Options{
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("forced redis failure")
		},
		MaxRetries: -1,
	})
	t.Cleanup(func() {
		_ = common.RDB.Close()
		common.RedisEnabled, common.RDB = oldRedisEnabled, oldRDB
	})

	_, err := HardDeleteUserById(user.Id)
	require.Error(t, err)

	var count int64
	require.NoError(t, DB.Unscoped().Model(&User{}).Where("id = ?", user.Id).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	for _, record := range []any{
		&Token{},
		&TwoFA{},
		&TwoFABackupCode{},
		&PasskeyCredential{},
		&UserOAuthBinding{},
		&UserSession{},
		&AuthFlow{},
		&ExternalIdentityClaim{},
		&UserAccessToken{},
	} {
		require.NoError(t, DB.Unscoped().Model(record).Where("user_id = ?", user.Id).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	}
}

func TestHardDeleteUserPublishesTombstoneAndPurgesAuthenticationData(t *testing.T) {
	truncateTables(t)
	server := useUserCacheMiniRedis(t)

	user := User{
		Username: "hard-delete-success", Password: "password", AuthVersion: 1,
		TelegramId: "hard-delete-success-telegram",
	}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Transaction(func(tx *gorm.DB) error {
		return ClaimExternalIdentityWithTx(tx, ExternalIdentityProviderTelegram, user.TelegramId, user.Id)
	}))
	require.NoError(t, DB.Create(&Token{UserId: user.Id, Key: "hard-delete-success-token"}).Error)
	require.NoError(t, DB.Create(&UserAccessToken{UserId: user.Id, TokenHash: AccessTokenFingerprint("nap_hard-delete-success-token")}).Error)
	require.NoError(t, DB.Create(&TwoFA{UserId: user.Id, Secret: "secret", IsEnabled: true}).Error)
	require.NoError(t, DB.Create(&TwoFABackupCode{UserId: user.Id, CodeHash: "hash"}).Error)
	require.NoError(t, DB.Create(&PasskeyCredential{UserID: user.Id, CredentialID: "credential-success", PublicKey: "public-key"}).Error)
	require.NoError(t, DB.Create(&UserOAuthBinding{UserId: user.Id, ProviderId: 1, ProviderUserId: "provider-user-success"}).Error)
	require.NoError(t, DB.Create(&UserSession{
		SID: "hard-delete-success-session", UserID: user.Id, Version: 1, UserAuthVersion: 1,
		Status: UserSessionStatusActive, RefreshHash: "refresh-hash", LoginMethod: "password",
		LastActiveAt: 1, ExpiresAt: 2,
	}).Error)
	require.NoError(t, DB.Create(&AuthFlow{
		TokenHash: "hard-delete-success-flow", Purpose: AuthFlowPurposeTwoFALogin,
		UserId: user.Id, ExpiresAt: time.Now().Add(time.Minute),
	}).Error)
	require.NoError(t, populateUserCache(user))
	// 硬删除应同时移除独立折扣和共享版本，删除后不能从旧快照继续取价。
	_, err := ReplaceUserModelPricing(user.Id, map[string]int{"deleted-model": 8000}, 1)
	require.NoError(t, err)
	_, err = GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	require.NoError(t, err)
	// Administrative hard deletion commonly targets an already soft-deleted
	// user; the shared version increment must therefore query unscoped.
	require.NoError(t, DB.Delete(&user).Error)

	revokedAccessTokens, err := HardDeleteUserById(user.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 1, revokedAccessTokens)

	var count int64
	require.NoError(t, DB.Unscoped().Model(&User{}).Where("id = ?", user.Id).Count(&count).Error)
	assert.Zero(t, count)
	for _, record := range []any{
		&Token{},
		&TwoFA{},
		&TwoFABackupCode{},
		&PasskeyCredential{},
		&UserOAuthBinding{},
		&UserSession{},
		&AuthFlow{},
		&ExternalIdentityClaim{},
		&UserAccessToken{},
	} {
		require.NoError(t, DB.Unscoped().Model(record).Where("user_id = ?", user.Id).Count(&count).Error)
		assert.Zero(t, count)
	}
	assert.False(t, server.Exists(getUserAuthFenceKey(user.Id)))
	committed, err := common.RDB.Get(t.Context(), getUserAuthVersionKey(user.Id)).Result()
	require.NoError(t, err)
	assert.Equal(t, "2", committed)
	assert.False(t, server.Exists(getUserCacheKey(user.Id)))
	for _, key := range userModelPricingVersionKeys(user.Id) {
		assert.False(t, server.Exists(key))
	}
	_, err = GetUserModelDiscountSnapshotContext(t.Context(), user.Id)
	assert.Error(t, err)
}

func TestUserDeletionRejectsPendingTaskAccounting(t *testing.T) {
	for _, scenario := range []string{"task", "initial accounting", "terminal success", "terminal failure", "refund", "refund cache"} {
		t.Run(scenario, func(t *testing.T) {
			truncateTables(t)
			user := User{Username: "pending-owner", AffCode: "pending-owner", Status: common.UserStatusEnabled, AuthVersion: 1, Quota: 900}
			require.NoError(t, DB.Create(&user).Error)
			access := UserAccessToken{UserId: user.Id, TokenHash: AccessTokenFingerprint("pending-owner-test")}
			require.NoError(t, DB.Create(&access).Error)
			var task Task
			var refund BillingRefund
			switch scenario {
			case "task":
				task = Task{TaskID: "pending-task", UserId: user.Id, Status: TaskStatusQueued}
				require.NoError(t, StageBillingTask(t.Context(), &task, 0))
			case "initial accounting":
				task = Task{TaskID: "pending-accounting", UserId: user.Id, Status: TaskStatusSuccess}
				require.NoError(t, DB.Create(&task).Error)
				require.NoError(t, DB.Create(&VideoTaskPendingLog{TaskID: -task.ID}).Error)
			case "terminal success", "terminal failure":
				task = Task{TaskID: "terminal-accounting", UserId: user.Id, Status: TaskStatusUnknown,
					PrivateData: TaskPrivateData{SubmissionPending: true, InitialAccountingPending: true}}
				require.NoError(t, StageBillingTask(t.Context(), &task, 0))
				task.Status = TaskStatusSuccess
				if scenario == "terminal failure" {
					task.Status = TaskStatusFailure
				}
				task.PrivateData.SubmissionPending = false
				won, err := PersistTaskSubmission(t.Context(), &task, true)
				require.NoError(t, err)
				require.True(t, won)
			case "refund", "refund cache":
				refund = BillingRefund{RequestID: "pending-refund", UserID: user.Id, Amount: 100, Completed: scenario == "refund cache", CachePending: scenario == "refund cache"}
				require.NoError(t, QueueBillingRefund(t.Context(), &refund))
			}

			_, err := user.Delete()
			require.ErrorIs(t, err, ErrUserTaskBillingPending)
			_, err = user.HardDelete()
			require.ErrorIs(t, err, ErrUserTaskBillingPending)
			var stored User
			require.NoError(t, DB.First(&stored, user.Id).Error)
			assert.EqualValues(t, 1, stored.AuthVersion)
			assert.Equal(t, 900, stored.Quota)
			var accessCount int64
			require.NoError(t, DB.Model(&UserAccessToken{}).Where("id = ?", access.Id).Count(&accessCount).Error)
			assert.EqualValues(t, 1, accessCount)
			other := User{Username: "unrelated-owner", AffCode: "unrelated-owner", Status: common.UserStatusEnabled, AuthVersion: 1}
			require.NoError(t, DB.Create(&other).Error)
			_, err = other.Delete()
			require.NoError(t, err)
			_, err = other.HardDelete()
			require.NoError(t, err)

			switch scenario {
			case "task":
				require.NoError(t, DB.Model(&task).UpdateColumn("status", TaskStatusSuccess).Error)
			case "initial accounting":
				require.NoError(t, DB.Where("task_id = ?", -task.ID).Delete(&VideoTaskPendingLog{}).Error)
			case "terminal success", "terminal failure":
				task.PrivateData.InitialAccountingPending = false
				require.NoError(t, DB.Model(&task).Update("private_data", task.PrivateData).Error)
			case "refund", "refund cache":
				require.NoError(t, DB.Model(&refund).Updates(map[string]any{"completed": true, "cache_pending": false}).Error)
			}
			_, err = user.Delete()
			require.NoError(t, err)
			_, err = user.HardDelete()
			require.NoError(t, err)
		})
	}
}

func TestTaskSubmissionAndUserDeletionSerialize(t *testing.T) {
	if !common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		t.Skip("requires MySQL row locking")
	}
	for _, submissionFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("submission_first=%t", submissionFirst), func(t *testing.T) {
			truncateTables(t)
			connection, err := DB.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(4)
			t.Cleanup(func() { connection.SetMaxOpenConns(1) })
			user := User{Username: "concurrent-owner", AffCode: "concurrent-owner", Status: common.UserStatusEnabled, AuthVersion: 1}
			require.NoError(t, DB.Create(&user).Error)
			task := Task{TaskID: "concurrent-submission", UserId: user.Id, Status: TaskStatusQueued}
			locked, release, attempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce, lockedOnce, attemptedOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			callback := "test:user-task-lock"
			if submissionFirst {
				require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "tasks" {
						lockedOnce.Do(func() { close(locked) })
						<-release
					}
				}))
				t.Cleanup(func() {
					releaseOnce.Do(func() { close(release) })
					assert.NoError(t, DB.Callback().Create().Remove(callback))
				})
			} else {
				require.NoError(t, DB.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "users" {
						lockedOnce.Do(func() { close(locked) })
						<-release
					}
				}))
				t.Cleanup(func() {
					releaseOnce.Do(func() { close(release) })
					assert.NoError(t, DB.Callback().Delete().Remove(callback))
				})
			}
			submissionResult, deletionResult := make(chan error, 1), make(chan error, 1)
			submit := func() { submissionResult <- StageBillingTask(t.Context(), &task, 0) }
			deleteUser := func() { _, deleteErr := user.Delete(); deletionResult <- deleteErr }
			if submissionFirst {
				go submit()
			} else {
				go deleteUser()
			}
			select {
			case <-locked:
			case <-time.After(5 * time.Second):
				t.Fatal("first transaction did not reach its locked write")
			}
			require.NoError(t, DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "users" {
					attemptedOnce.Do(func() { close(attempted) })
				}
			}))
			t.Cleanup(func() { assert.NoError(t, DB.Callback().Query().Remove(callback)) })
			if submissionFirst {
				go deleteUser()
			} else {
				go submit()
			}
			select {
			case <-attempted:
			case <-time.After(5 * time.Second):
				t.Fatal("second transaction did not attempt the user lock")
			}
			select {
			case err := <-submissionResult:
				t.Fatalf("submission completed before the first transaction released its lock: %v", err)
			case err := <-deletionResult:
				t.Fatalf("deletion completed before the first transaction released its lock: %v", err)
			case <-time.After(200 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			var submissionErr, deletionErr error
			select {
			case submissionErr = <-submissionResult:
			case <-time.After(5 * time.Second):
				t.Fatal("submission did not finish after releasing the lock")
			}
			select {
			case deletionErr = <-deletionResult:
			case <-time.After(5 * time.Second):
				t.Fatal("deletion did not finish after releasing the lock")
			}
			if submissionFirst {
				assert.NoError(t, submissionErr)
				assert.ErrorIs(t, deletionErr, ErrUserTaskBillingPending)
			} else {
				assert.NoError(t, deletionErr)
				assert.ErrorIs(t, submissionErr, gorm.ErrRecordNotFound)
				var count int64
				require.NoError(t, DB.Model(&Task{}).Where("user_id = ?", user.Id).Count(&count).Error)
				assert.Zero(t, count)
			}
		})
	}
}

func TestIncrementFailedAttemptsCountsConcurrentFailures(t *testing.T) {
	truncateTables(t)

	user := User{Username: "twofa-cas-user", Password: "password"}
	require.NoError(t, DB.Create(&user).Error)
	twoFA := TwoFA{UserId: user.Id, Secret: "secret", IsEnabled: true}
	require.NoError(t, DB.Create(&twoFA).Error)

	const attempts = 4
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			errs <- (&TwoFA{Id: twoFA.Id}).IncrementFailedAttempts()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var reloaded TwoFA
	require.NoError(t, DB.First(&reloaded, twoFA.Id).Error)
	assert.Equal(t, attempts, reloaded.FailedAttempts)
}

func TestValidateBackupCodeCanOnlySucceedOnce(t *testing.T) {
	truncateTables(t)

	const code = "ABCD-1234"
	user := User{Id: 123, Username: "backup-code-user", Password: "password", AuthVersion: 1}
	require.NoError(t, DB.Create(&user).Error)
	require.NoError(t, DB.Create(&TwoFA{UserId: user.Id, Secret: "secret", IsEnabled: false}).Error)
	hashedCode, err := common.HashBackupCode(code)
	require.NoError(t, err)
	require.NoError(t, DB.Create(&TwoFABackupCode{UserId: user.Id, CodeHash: hashedCode}).Error)

	const attempts = 2
	results := make(chan bool, attempts)
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			valid, err := ValidateBackupCode(123, code)
			results <- valid
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	wins := 0
	for valid := range results {
		if valid {
			wins++
		}
	}
	assert.Equal(t, 1, wins)

	remaining, err := GetUnusedBackupCodeCount(123)
	require.NoError(t, err)
	assert.Zero(t, remaining)
}

func TestPendingTwoFASetupAPIsRejectEnabledFactor(t *testing.T) {
	truncateTables(t)

	user := User{Username: "enabled-twofa-guard", Password: "password", Status: common.UserStatusEnabled, AuthVersion: 1}
	require.NoError(t, DB.Create(&user).Error)
	twoFA := TwoFA{UserId: user.Id, Secret: "secret", IsEnabled: true}
	require.NoError(t, DB.Create(&twoFA).Error)

	session := UserSession{SID: "enabled-factor-session", UserID: user.Id, Version: 1, UserAuthVersion: 1, Status: UserSessionStatusActive, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, DB.Create(&session).Error)
	identity := AuthSessionIdentity{UserID: user.Id, SessionID: session.SID, UserAuthVersion: 1, SessionVersion: 1}
	authorization := &AuthFlowAuthorization{AuthSessionIdentity: identity, ProofID: 1, Scope: "2fa.setup", ContextHash: "setup-context", Method: "password"}
	_, err := CreateTwoFAEnrollment(identity, authorization, "replacement-secret", []string{"ABCD-1234"}, time.Now().Add(time.Minute))
	require.ErrorIs(t, err, ErrTwoFAAlreadyEnabled)

	var stored TwoFA
	require.NoError(t, DB.First(&stored, twoFA.Id).Error)
	assert.True(t, stored.IsEnabled)
	var backupCodeCount int64
	require.NoError(t, DB.Model(&TwoFABackupCode{}).Where("user_id = ?", user.Id).Count(&backupCodeCount).Error)
	assert.Zero(t, backupCodeCount)
}

func TestSecurityFactorMutationsAdvanceUserAuthVersion(t *testing.T) {
	truncateTables(t)

	user := User{
		Username:    "security-factor-version-user",
		Password:    "password",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
	}
	require.NoError(t, DB.Create(&user).Error)
	session := UserSession{SID: "factor-version-session", UserID: user.Id, Version: 1, UserAuthVersion: 1, Status: UserSessionStatusActive, ExpiresAt: time.Now().Add(time.Hour).Unix()}
	require.NoError(t, DB.Create(&session).Error)
	identity := AuthSessionIdentity{UserID: user.Id, SessionID: session.SID, UserAuthVersion: 1, SessionVersion: 1}
	authorization := &AuthFlowAuthorization{AuthSessionIdentity: identity, ProofID: 1, Scope: "2fa.setup", ContextHash: "setup-context", Method: "password"}
	token, err := CreateTwoFAEnrollment(identity, authorization, "JBSWY3DPEHPK3PXP", []string{"ABCD-1234"}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	code, err := totp.GenerateCode("JBSWY3DPEHPK3PXP", time.Now())
	require.NoError(t, err)
	require.NoError(t, EnableTwoFAEnrollment(identity, token, code))
	assertUserAuthVersion(t, user.Id, 2)
	assert.ErrorIs(t, EnableTwoFAEnrollment(identity, token, code), ErrTwoFASetupInvalid)
	assertUserAuthVersion(t, user.Id, 2)
	require.NoError(t, ReplaceBackupCodesWithAuthVersion(user.Id, []string{"ABCD-1234"}))
	assertUserAuthVersion(t, user.Id, 3)
	require.NoError(t, DisableTwoFAWithAuthVersion(user.Id))
	assertUserAuthVersion(t, user.Id, 4)

	credential := &PasskeyCredential{UserID: user.Id, CredentialID: "credential-id", PublicKey: "public-key"}
	require.NoError(t, UpsertPasskeyCredentialWithAuthVersion(credential))
	assertUserAuthVersion(t, user.Id, 5)
	require.NoError(t, DeletePasskeyByUserIDWithAuthVersion(user.Id))
	assertUserAuthVersion(t, user.Id, 6)
}

func TestUpdatePasskeyAssertionStateCannotRewriteRegistrationIdentity(t *testing.T) {
	truncateTables(t)
	require.NoError(t, DB.AutoMigrate(&Option{}))
	previousSettings := *system_setting.GetPasskeySettings()
	domainKeys := map[string]any{"key": []string{"ServerAddress", "passkey.rp_id", "passkey.legacy_rp_ids", "passkey.origins"}}
	var previousOptions []Option
	require.NoError(t, DB.Where(domainKeys).Find(&previousOptions).Error)
	t.Cleanup(func() {
		*system_setting.GetPasskeySettings() = previousSettings
		require.NoError(t, DB.Where(domainKeys).Delete(&Option{}).Error)
		if len(previousOptions) > 0 {
			require.NoError(t, DB.Create(&previousOptions).Error)
		}
	})
	*system_setting.GetPasskeySettings() = system_setting.PasskeySettings{RPID: "example.com", Origins: "https://example.com"}
	for _, option := range []Option{{Key: "passkey.rp_id", Value: "example.com"}, {Key: "passkey.legacy_rp_ids", Value: ""}, {Key: "passkey.origins", Value: "https://example.com"}} {
		require.NoError(t, DB.Save(&option).Error)
	}

	user := User{Username: "passkey-assertion-state", Password: "password", AuthVersion: 1}
	require.NoError(t, DB.Create(&user).Error)
	credentialID := []byte("stable-credential-id")
	stored := PasskeyCredential{
		UserID:          user.Id,
		CredentialID:    base64.StdEncoding.EncodeToString(credentialID),
		PublicKey:       "original-public-key",
		AttestationType: "packed",
		AAGUID:          "original-aaguid",
		SignCount:       1,
		Transports:      `["usb"]`,
		Attachment:      "platform",
	}
	require.NoError(t, DB.Create(&stored).Error)
	usedAt := time.Now().UTC().Truncate(time.Second)
	validated := &webauthn.Credential{
		ID:              credentialID,
		PublicKey:       []byte("replacement-public-key"),
		AttestationType: "none",
		Flags: webauthn.CredentialFlags{
			UserPresent:    true,
			UserVerified:   true,
			BackupEligible: true,
			BackupState:    true,
		},
		Authenticator: webauthn.Authenticator{
			AAGUID:       []byte("replacement-aaguid"),
			SignCount:    8,
			CloneWarning: true,
		},
	}
	require.NoError(t, UpdatePasskeyAssertionState(user.Id, validated, usedAt, "example.com"))

	var updated PasskeyCredential
	require.NoError(t, DB.First(&updated, stored.ID).Error)
	assert.Equal(t, stored.CredentialID, updated.CredentialID)
	assert.Equal(t, stored.PublicKey, updated.PublicKey)
	assert.Equal(t, stored.AttestationType, updated.AttestationType)
	assert.Equal(t, stored.AAGUID, updated.AAGUID)
	assert.Equal(t, stored.Transports, updated.Transports)
	assert.Equal(t, stored.Attachment, updated.Attachment)
	assert.EqualValues(t, 8, updated.SignCount)
	assert.True(t, updated.CloneWarning)
	assert.True(t, updated.UserPresent)
	assert.True(t, updated.UserVerified)
	assert.True(t, updated.BackupEligible)
	assert.True(t, updated.BackupState)
	require.NotNil(t, updated.LastUsedAt)
	assert.Equal(t, usedAt.Unix(), updated.LastUsedAt.Unix())

	validated.ID = []byte("another-credential")
	assert.ErrorIs(t, UpdatePasskeyAssertionState(user.Id, validated, usedAt, "example.com"), ErrPasskeyNotFound)
}

func assertUserAuthVersion(t *testing.T, userID int, expected int64) {
	t.Helper()
	var version int64
	require.NoError(t, DB.Model(&User{}).Where("id = ?", userID).Select("auth_version").Scan(&version).Error)
	assert.Equal(t, expected, version)
}
