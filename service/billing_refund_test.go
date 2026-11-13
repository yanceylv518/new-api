package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func requireBillingRefundMySQL(t *testing.T) {
	t.Helper()
	if !common.UsingMainDatabase(common.DatabaseTypeMySQL) {
		t.Skip("requires isolated MySQL")
	}
}

func billingTestRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("BILLING_REFUND_TEST_REDIS_ADDR")
	if addr == "" {
		addr = miniredis.RunT(t).Addr()
	} else {
		host, _, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1", host)
	}
	return redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1})
}

func TestBillingRefundKeepsBalancesAtomicOnFailure(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", 941101).Delete(&model.BillingRefund{}).Error) })
	const id = 941101
	seedUser(t, id, 1000)
	seedToken(t, id, id, "refund-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "refund-fixture", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))

	failed := make(chan struct{}, 1)
	var shouldFail atomic.Bool
	shouldFail.Store(true)
	var attempts atomic.Int32
	const failureCallback = "refund-test:fail-wallet"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(failureCallback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" && shouldFail.Load() {
			tx.AddError(errors.New("injected wallet write failure"))
			if attempts.Add(1) == 3 {
				failed <- struct{}{}
			}
		}
	}))
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Callback().Update().Remove(failureCallback))
	})
	session.Refund(c)
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("refund attempts did not finish")
	}
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
	assert.False(t, refund.Completed)
	shouldFail.Store(false)
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	session.Refund(c)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
	var token model.Token
	require.NoError(t, model.DB.First(&token, id).Error)
	assert.Zero(t, token.UsedQuota)
}

func TestBillingRefundRollsBackWalletWhenTokenWriteFails(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941117
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 900)
	seedToken(t, id, id, "refund-token-failure-fixture", 900)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	refund := &model.BillingRefund{RequestID: "refund-token-failure", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	const callback = "refund-test:fail-token"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" {
			tx.AddError(errors.New("injected token write failure"))
		}
	}))
	require.Error(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	require.NoError(t, model.DB.Callback().Update().Remove(callback))
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
	require.NoError(t, model.DB.First(refund, refund.ID).Error)
	assert.False(t, refund.Completed)
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingRefundRecoversWhenQueueWriteFails(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941102
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 1000)
	seedToken(t, id, id, "refund-queue-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "refund-queue-fixture", RequestId: "refund-queue-failure", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Nil(t, session.preConsume(c, 100))
	const callback = "refund-test:fail-queue"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "billing_refunds" {
			tx.AddError(errors.New("injected refund queue failure"))
		}
	}))
	session.Refund(c)
	require.NoError(t, model.DB.Callback().Create().Remove(callback))
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingReservationRollbackOnFailure(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941103
	seedUser(t, id, 50)
	seedToken(t, id, id, "refund-reservation-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 50, TokenId: id, TokenKey: "refund-reservation-fixture", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	var tokenWrites atomic.Int32
	const callback = "refund-test:fail-compensation"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "tokens" && tokenWrites.Add(1) > 1 {
			tx.AddError(errors.New("injected token compensation failure"))
		}
	}))
	t.Cleanup(func() { assert.NoError(t, model.DB.Callback().Update().Remove(callback)) })
	require.NotNil(t, session.preConsume(c, 100))
	assert.Equal(t, 50, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingTaskRefundSurvivesQueueFailure(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941104
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 1000)
	seedToken(t, id, id, "task-refund-queue-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "task-refund-queue-fixture", RequestId: "task-refund-queue-failure", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_refund_queue_failure"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	require.NoError(t, session.ReserveBeforeTaskSubmit(150))
	const callback = "refund-test:fail-task-queue"
	const walletCallback = "refund-test:fail-task-wallet"
	failed := make(chan struct{}, 1)
	var attempts atomic.Int32
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "billing_refunds" {
			tx.AddError(errors.New("injected refund queue failure"))
		}
	}))
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(walletCallback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("injected wallet write failure"))
			if attempts.Add(1) == 3 {
				failed <- struct{}{}
			}
		}
	}))
	session.Refund(c)
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("refund attempts did not finish")
	}
	require.NoError(t, model.DB.Callback().Create().Remove(callback))
	require.NoError(t, model.DB.Callback().Update().Remove(walletCallback))
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
	assert.Equal(t, 150, refund.Amount)
	assert.Equal(t, 150, refund.TokenAmount)
	require.NoError(t, model.DB.Model(&refund).Update("next_attempt_at", 0).Error)
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingTaskRefundDoesNotRecreateExpiredReservation(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941116
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 1000)
	seedToken(t, id, id, "task-expired-refund-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "task-expired-refund-fixture", RequestId: "task-expired-refund", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_expired_refund"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
	require.NoError(t, model.MarkBillingRefundReady(t.Context(), refund.ID, id))
	require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	require.NoError(t, model.DB.Model(&refund).Update("completed_at", time.Now().Add(-8*24*time.Hour).Unix()).Error)
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	session.Refund(c)
	var duplicate model.BillingRefund
	err := model.DB.Where("request_id = ?", info.RequestId).First(&duplicate).Error
	if err == nil {
		require.NoError(t, model.CompleteBillingRefund(t.Context(), duplicate.ID))
	}
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingTaskReservationHandoffAndSettlement(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}, &model.VideoTaskPendingLog{}))
	const id = 941105
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 1000)
	seedToken(t, id, id, "task-handoff-fixture", 1000)
	seedChannel(t, id)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "task-handoff-fixture", RequestId: "task-handoff-request", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_handoff_request"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	require.NoError(t, session.ReserveBeforeTaskSubmit(150))
	task := &model.Task{TaskID: info.PublicTaskID, UserId: id, ChannelId: id, Quota: 150, Status: model.TaskStatusUnknown, PrivateData: model.TaskPrivateData{TokenId: id, BillingSource: BillingSourceWallet, SubmissionPending: true, InitialAccountingPending: true}}
	const callback = "refund-test:fail-intent"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "tasks" {
			tx.AddError(errors.New("injected task intent failure"))
		}
	}))
	require.Error(t, session.StageTaskSubmission(t.Context(), task))
	require.NoError(t, model.DB.Callback().Create().Remove(callback))
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
	assert.Equal(t, 150, refund.Amount)
	require.NoError(t, session.StageTaskSubmission(t.Context(), task))
	require.NoError(t, session.ReserveBeforeTaskSubmit(175))
	task.Status = model.TaskStatusSuccess
	task.Quota = 120
	task.PrivateData.SubmissionPending = false
	won, err := model.PersistTaskSubmission(t.Context(), task, true, model.TaskSubmissionFunding{ReservedQuota: 175, UpdateToken: true})
	require.NoError(t, err)
	require.True(t, won)
	session.CompleteTaskSubmission(120)
	require.NoError(t, session.Settle(120))
	require.Equal(t, 880, getUserQuota(t, id))
	require.Equal(t, 880, getTokenRemainQuota(t, id))
	won, err = model.CommitTaskInitialAccounting(t.Context(), task)
	require.NoError(t, err)
	require.True(t, won)
	var user model.User
	require.NoError(t, model.DB.First(&user, id).Error)
	assert.Equal(t, 120, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	var token model.Token
	require.NoError(t, model.DB.First(&token, id).Error)
	assert.Equal(t, 120, token.UsedQuota)
	won, err = model.CommitTaskInitialAccounting(t.Context(), task)
	require.NoError(t, err)
	assert.False(t, won)
}

func TestBillingTaskReservationRejectsDuplicateRequest(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941106
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 1000)
	seedToken(t, id, id, "task-duplicate-reservation-fixture", 1000)
	reservation := func() model.BillingQuotaReservation {
		return model.BillingQuotaReservation{UserID: id, TokenID: id, TokenKey: "task-duplicate-reservation-fixture", Amount: 100, UpdateToken: true, CheckWallet: true, Refund: &model.BillingRefund{RequestID: "task-duplicate-reservation"}}
	}
	_, err := model.ReserveBillingQuota(t.Context(), reservation())
	require.NoError(t, err)
	_, err = model.ReserveBillingQuota(t.Context(), reservation())
	assert.Error(t, err)
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
}

func TestBillingTaskPollingPreservesConcurrentReservation(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941110
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 1000)
	seedToken(t, id, id, "task-poll-reservation-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "task-poll-reservation-fixture", RequestId: "task-poll-reservation", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_poll_reservation"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	task := &model.Task{TaskID: info.PublicTaskID, UserId: id, Quota: 100, Status: model.TaskStatusUnknown, PrivateData: model.TaskPrivateData{TokenId: id, BillingSource: BillingSourceWallet, SubmissionPending: true, InitialAccountingPending: true}}
	require.NoError(t, session.StageTaskSubmission(t.Context(), task))
	var stale model.Task
	require.NoError(t, model.DB.First(&stale, task.ID).Error)
	require.NoError(t, session.ReserveBeforeTaskSubmit(150))
	stale.PrivateData.ReconciliationRequired = true
	stale.PrivateData.ReconciliationReason = "task polling is awaiting confirmation"
	_, err := stale.UpdateWithStatus(model.TaskStatusUnknown)
	require.NoError(t, err)
	var stored model.Task
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	require.NotNil(t, stored.PrivateData.SubmissionFunding)
	assert.Equal(t, 150, stored.PrivateData.SubmissionFunding.ReservedQuota)
	assert.Equal(t, 850, getUserQuota(t, id))
	assert.Equal(t, 850, getTokenRemainQuota(t, id))
}

func TestBillingRefundConcurrentCompletion(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941107
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 900)
	seedToken(t, id, id, "refund-concurrent-fixture", 900)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	refund := &model.BillingRefund{RequestID: "refund-concurrent", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			results <- model.CompleteBillingRefund(t.Context(), refund.ID)
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
	var token model.Token
	require.NoError(t, model.DB.First(&token, id).Error)
	assert.Zero(t, token.UsedQuota)
}

func TestBillingSubscriptionRefundRespectsResetPeriod(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}, &model.SubscriptionPreConsumeRecord{}))
	const id = 941108
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error)
		assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.SubscriptionPreConsumeRecord{}).Error)
	})
	seedUser(t, id, 1000)
	seedToken(t, id, id, "refund-subscription-fixture", 900)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	now := common.GetTimestamp()
	sub := &model.UserSubscription{Id: id, UserId: id, AmountTotal: 1000, AmountUsed: 200, Status: "active", StartTime: now - 7200, EndTime: now + 3600, LastResetTime: now - 3600}
	require.NoError(t, model.DB.Create(sub).Error)
	record := &model.SubscriptionPreConsumeRecord{RequestId: "refund-previous-period", UserId: id, UserSubscriptionId: id, PreConsumed: 100, Status: "consumed"}
	require.NoError(t, model.DB.Create(record).Error)
	refund := &model.BillingRefund{RequestID: record.RequestId, UserID: id, TokenID: id, Amount: 100, TokenAmount: 100, SubscriptionID: id, PeriodStart: now - 7200}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	require.NoError(t, model.DB.First(sub, id).Error)
	assert.EqualValues(t, 200, sub.AmountUsed)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
	require.NoError(t, model.DB.First(record, record.Id).Error)
	assert.Equal(t, "refunded", record.Status)
}

func TestBillingSubscriptionReservationRejectsDuplicateRequest(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPreConsumeRecord{}))
	const id = 941111
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.SubscriptionPreConsumeRecord{}).Error)
		assert.NoError(t, model.DB.Delete(&model.SubscriptionPlan{}, id).Error)
	})
	seedUser(t, id, 1000)
	seedToken(t, id, id, "subscription-duplicate-fixture", 1000)
	plan := &model.SubscriptionPlan{Id: id, Title: "reservation fixture", TotalAmount: 1000}
	require.NoError(t, model.DB.Create(plan).Error)
	now := common.GetTimestamp()
	sub := &model.UserSubscription{Id: id, UserId: id, PlanId: id, AmountTotal: 1000, Status: "active", StartTime: now - 60, EndTime: now + 3600}
	require.NoError(t, model.DB.Create(sub).Error)
	reservation := model.BillingQuotaReservation{UserID: id, TokenID: id, TokenKey: "subscription-duplicate-fixture", Amount: 100, UpdateToken: true, SubscriptionRequestID: "subscription-duplicate-request"}
	_, err := model.ReserveBillingQuota(t.Context(), reservation)
	require.NoError(t, err)
	_, err = model.ReserveBillingQuota(t.Context(), reservation)
	assert.Error(t, err)
	require.NoError(t, model.DB.First(sub, id).Error)
	assert.EqualValues(t, 100, sub.AmountUsed)
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
}

func TestBillingRefundPendingBlocksUserDeletion(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}, &model.UserAccessToken{}, &model.UserSession{}))
	const id = 941112
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 900)
	seedToken(t, id, id, "refund-delete-fixture", 900)
	refund := &model.BillingRefund{RequestID: "refund-delete", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	_, err := (&model.User{Id: id}).Delete()
	assert.Error(t, err)
	if err == nil {
		assert.Error(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	}
}

func TestBillingRefundSchedulerIgnoresCacheOnlyWorkWhenRedisDisabled(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941113
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	refund := &model.BillingRefund{RequestID: "refund-cache-disabled", UserID: id, Amount: 100, Completed: true, CachePending: true}
	require.NoError(t, model.DB.Create(refund).Error)
	require.False(t, common.RedisEnabled)
	assert.False(t, model.HasPendingBillingRefunds())
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	assert.False(t, model.HasPendingBillingRefunds())
}

type refundFinishFailureHook struct {
	scripts atomic.Int32
}

func (h *refundFinishFailureHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if cmd.Name() == "evalsha" && h.scripts.Add(1) == 2 {
		return ctx, errors.New("injected refund cache finish failure")
	}
	return ctx, nil
}

func (*refundFinishFailureHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (*refundFinishFailureHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*refundFinishFailureHook) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}

func TestBillingRefundCacheRecovery(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.BillingRefund{}))
	const id = 941109
	t.Cleanup(func() { assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.BillingRefund{}).Error) })
	seedUser(t, id, 900)
	seedToken(t, id, id, "refund-cache-fixture", 900)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	tokenCacheKey := "token:" + common.GenerateHMAC("refund-cache-fixture")
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 900, "UsedQuota", 100).Err())
	refund := &model.BillingRefund{RequestID: "refund-cache", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	require.NoError(t, model.DB.Create(&model.Task{ID: refund.ID, TaskID: "same-refund-id", UserId: id, Quota: 100, Status: model.TaskStatusQueued}).Error)
	hook := &refundFinishFailureHook{}
	client.AddHook(hook)
	require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	require.EqualValues(t, 2, hook.scripts.Load())
	require.NoError(t, model.DB.First(refund, refund.ID).Error)
	assert.True(t, refund.Completed)
	assert.True(t, refund.CachePending)
	user, err := model.GetUserCache(id)
	require.NoError(t, err)
	token, err := model.GetTokenByKey("refund-cache-fixture", false)
	require.NoError(t, err)
	assert.Equal(t, 1000, user.Quota)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	require.NoError(t, model.DB.First(refund, refund.ID).Error)
	assert.False(t, refund.CachePending)
}

func TestBillingReservationRestoresCacheOnInsufficientWallet(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941115
	seedUser(t, id, 50)
	seedToken(t, id, id, "reservation-cache-fixture", 1000)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	tokenCacheKey := "token:" + common.GenerateHMAC("reservation-cache-fixture")
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 1000, "UsedQuota", 0).Err())
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 50, TokenId: id, TokenKey: "reservation-cache-fixture", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.NotNil(t, session.preConsume(c, 100))
	user, err := model.GetUserCache(id)
	require.NoError(t, err)
	token, err := model.GetTokenByKey("reservation-cache-fixture", false)
	require.NoError(t, err)
	assert.Equal(t, 50, user.Quota)
	assert.Equal(t, 1000, token.RemainQuota)
	assert.Zero(t, token.UsedQuota)
	assert.Equal(t, 50, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingOrdinaryReservationSettlement(t *testing.T) {
	requireBillingRefundMySQL(t)
	for _, tc := range []struct {
		name       string
		reserved   int
		actual     int
		playground bool
	}{
		{name: "same", reserved: 100, actual: 100},
		{name: "refund difference", reserved: 100, actual: 50},
		{name: "additional charge", reserved: 100, actual: 150},
		{name: "zero reserve", reserved: 0, actual: 50},
		{name: "playground", reserved: 100, actual: 150, playground: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			const id = 941201
			seedUser(t, id, 1000)
			seedToken(t, id, id, "ordinary-settlement-fixture", 1000)
			info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "ordinary-settlement-fixture", ForcePreConsume: true, IsPlayground: tc.playground}
			session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			require.Nil(t, session.preConsume(c, tc.reserved))
			var refund model.BillingRefund
			require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
			assert.Equal(t, model.BillingRefundInFlight, refund.NextAttemptAt)
			require.ErrorIs(t, model.CompleteBillingRefund(t.Context(), refund.ID), model.ErrBillingRefundStateConflict)
			require.NoError(t, model.RetryBillingRefunds(t.Context()))
			assert.Equal(t, 1000-tc.reserved, getUserQuota(t, id))
			require.NoError(t, session.Settle(tc.actual))
			require.NoError(t, session.Settle(tc.actual))
			require.ErrorIs(t, model.MarkBillingRefundReady(t.Context(), refund.ID, id), model.ErrBillingRefundStateConflict)
			require.NoError(t, model.RetryBillingRefunds(t.Context()))
			require.NoError(t, model.DB.First(&refund, refund.ID).Error)
			assert.True(t, refund.Completed)
			assert.True(t, refund.Settled)
			assert.Equal(t, tc.actual, refund.SettledAmount)
			assert.Equal(t, 1000-tc.actual, getUserQuota(t, id))
			wantToken := 1000 - tc.actual
			if tc.playground {
				wantToken = 1000
			}
			assert.Equal(t, wantToken, getTokenRemainQuota(t, id))
		})
	}
}

func TestBillingSettlementRecoversAfterDatabaseFailure(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941202
	seedUser(t, id, 1000)
	seedToken(t, id, id, "settlement-recovery-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "settlement-recovery-fixture", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Nil(t, session.preConsume(c, 100))
	const callback = "refund-test:fail-settlement"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			tx.AddError(errors.New("injected settlement failure"))
		}
	}))
	require.Error(t, session.Settle(50))
	require.NoError(t, model.DB.Callback().Update().Remove(callback))
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
	assert.True(t, refund.SettlementPending)
	assert.False(t, refund.Completed)
	require.ErrorIs(t, model.MarkBillingRefundReady(t.Context(), refund.ID, id), model.ErrBillingRefundStateConflict)
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	require.NoError(t, model.SettleBillingReservation(t.Context(), refund.ID, id, 100, 50))
	assert.Equal(t, 950, getUserQuota(t, id))
	assert.Equal(t, 950, getTokenRemainQuota(t, id))
}

func TestBillingLockedSettlementDoesNotBlockRefunds(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const refundUser, settlementUser = 941220, 941221
	for _, id := range []int{refundUser, settlementUser} {
		require.NoError(t, model.DB.Create(&model.User{Id: id, Username: fmt.Sprintf("retry_%d", id), AffCode: fmt.Sprintf("retry_%d", id), Status: common.UserStatusEnabled, Quota: 900}).Error)
		seedToken(t, id, id, fmt.Sprintf("locked-settlement-token-%d", id), 900)
		require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	}
	refund := &model.BillingRefund{RequestID: "unblocked-refund", UserID: refundUser, TokenID: refundUser, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	settlement := &model.BillingRefund{RequestID: "locked-settlement", UserID: settlementUser, TokenID: settlementUser, Amount: 100, TokenAmount: 100, NextAttemptAt: model.BillingRefundInFlight}
	require.NoError(t, model.QueueBillingRefund(t.Context(), settlement))
	require.NoError(t, model.MarkBillingSettlementReady(t.Context(), settlement.ID, settlementUser, 100, 50))
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- model.DB.Transaction(func(tx *gorm.DB) error {
			var row model.BillingRefund
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, settlement.ID).Error; err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	defer func() { close(release); assert.NoError(t, <-done) }()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("settlement row was not locked")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.Error(t, model.RetryBillingRefunds(ctx))
	assert.Equal(t, 1000, getUserQuota(t, refundUser))
	assert.Equal(t, 1000, getTokenRemainQuota(t, refundUser))
}

func TestBillingLockedRefundDoesNotBlockLaterRefund(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941222
	seedUser(t, id, 800)
	seedToken(t, id, id, "locked-refund-fixture", 800)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 200).Error)
	lockedRefund := &model.BillingRefund{RequestID: "locked-refund", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
	otherRefund := &model.BillingRefund{RequestID: "unblocked-later-refund", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), lockedRefund))
	require.NoError(t, model.QueueBillingRefund(t.Context(), otherRefund))
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- model.DB.Transaction(func(tx *gorm.DB) error {
			var row model.BillingRefund
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, lockedRefund.ID).Error; err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	defer func() { close(release); assert.NoError(t, <-done) }()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("refund row was not locked")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.Error(t, model.RetryBillingRefunds(ctx))
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
	require.NoError(t, model.DB.First(otherRefund, otherRefund.ID).Error)
	assert.True(t, otherRefund.Completed)
}

func TestBillingRetryCursorMovesPastLockedRecords(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941223
	seedUser(t, id, 600)
	seedToken(t, id, id, "locked-refund-cursor-fixture", 600)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 400).Error)
	var lockedIDs []int64
	for i := range 3 {
		refund := &model.BillingRefund{RequestID: fmt.Sprintf("locked-cursor-%d", i), UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
		require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
		lockedIDs = append(lockedIDs, refund.ID)
	}
	otherRefund := &model.BillingRefund{RequestID: "unblocked-cursor-refund", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100}
	require.NoError(t, model.QueueBillingRefund(t.Context(), otherRefund))
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- model.DB.Transaction(func(tx *gorm.DB) error {
			var rows []model.BillingRefund
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id IN ?", lockedIDs).Find(&rows).Error; err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	defer func() { close(release); assert.NoError(t, <-done) }()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("refund rows were not locked")
	}
	for range 3 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		retryErr := model.RetryBillingRefunds(ctx)
		cancel()
		require.NoError(t, model.DB.First(otherRefund, otherRefund.ID).Error)
		if otherRefund.Completed {
			break
		}
		require.Error(t, retryErr)
	}
	assert.True(t, otherRefund.Completed)
	assert.Equal(t, 700, getUserQuota(t, id))
	assert.Equal(t, 700, getTokenRemainQuota(t, id))
}

func TestBillingFailedSettlementDecisionDoesNotRefund(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941209
	seedUser(t, id, 1000)
	seedToken(t, id, id, "settlement-decision-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "settlement-decision-fixture", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Nil(t, session.preConsume(c, 100))
	const callback = "refund-test:fail-settlement-decision"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "billing_refunds" {
			tx.AddError(errors.New("injected settlement decision failure"))
		}
	}))
	require.Error(t, session.Settle(50))
	require.NoError(t, model.DB.Callback().Update().Remove(callback))
	assert.False(t, session.NeedsRefund())
	session.Refund(c)
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
	assert.Equal(t, model.BillingRefundInFlight, refund.NextAttemptAt)
	assert.False(t, refund.DecisionKnown)
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
}

func TestBillingZeroReservationFailureClosesRecord(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941210
	seedUser(t, id, 1000)
	seedToken(t, id, id, "zero-reservation-refund-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "zero-reservation-refund-fixture", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Nil(t, session.preConsume(c, 0))
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("user_id = ?", id).First(&refund).Error)
	session.Refund(c)
	require.Eventually(t, func() bool {
		return model.DB.First(&refund, refund.ID).Error == nil && refund.Completed
	}, 5*time.Second, 20*time.Millisecond)
	assert.True(t, refund.Completed)
	assert.False(t, refund.Settled)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingZeroReservationRejectsWrongTokenOwner(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941213
	seedUser(t, id, 1000)
	seedToken(t, id, id+1, "zero-other-owner-fixture", 1000)
	refund := &model.BillingRefund{RequestID: "zero-other-owner", NextAttemptAt: model.BillingRefundInFlight}
	_, err := model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, Amount: 0, UpdateToken: true, Refund: refund})
	require.Error(t, err)
	var count int64
	require.NoError(t, model.DB.Model(&model.BillingRefund{}).Where("user_id = ?", id).Count(&count).Error)
	assert.Zero(t, count)
	assert.Equal(t, 1000, getUserQuota(t, id))
}

func TestBillingRefundWithDeletedTokenRestoresWallet(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941214
	seedUser(t, id, 1000)
	seedToken(t, id, id, "deleted-refund-token-fixture", 1000)
	refund := &model.BillingRefund{RequestID: "deleted-refund-token", NextAttemptAt: model.BillingRefundInFlight}
	_, err := model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, Amount: 100, UpdateToken: true, CheckWallet: true, Refund: refund})
	require.NoError(t, err)
	require.NoError(t, model.DB.Delete(&model.Token{}, id).Error)
	require.NoError(t, model.MarkBillingRefundReady(t.Context(), refund.ID, id))
	require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
	assert.Equal(t, 1000, getUserQuota(t, id))
	var token model.Token
	require.NoError(t, model.DB.Unscoped().First(&token, id).Error)
	assert.Equal(t, 900, token.RemainQuota)
	assert.Equal(t, 100, token.UsedQuota)
}

func TestBillingRefundAndSettlementDecisionsAreExclusive(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941211
	seedUser(t, id, 1000)
	seedToken(t, id, id, "billing-decision-race-fixture", 1000)
	refund := &model.BillingRefund{RequestID: "billing-decision-race", NextAttemptAt: model.BillingRefundInFlight}
	_, err := model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, Amount: 100, UpdateToken: true, CheckWallet: true, Refund: refund})
	require.NoError(t, err)
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- model.MarkBillingRefundReady(t.Context(), refund.ID, id) }()
	go func() { <-start; results <- model.MarkBillingSettlementReady(t.Context(), refund.ID, id, 100, 50) }()
	close(start)
	first, second := <-results, <-results
	require.True(t, first == nil && errors.Is(second, model.ErrBillingRefundStateConflict) || second == nil && errors.Is(first, model.ErrBillingRefundStateConflict))
	require.NoError(t, model.DB.First(refund, refund.ID).Error)
	if refund.SettlementPending {
		require.NoError(t, model.SettleBillingReservation(t.Context(), refund.ID, id, 100, 50))
		assert.Equal(t, 950, getUserQuota(t, id))
		assert.Equal(t, 950, getTokenRemainQuota(t, id))
	} else {
		require.NoError(t, model.CompleteBillingRefund(t.Context(), refund.ID))
		assert.Equal(t, 1000, getUserQuota(t, id))
		assert.Equal(t, 1000, getTokenRemainQuota(t, id))
	}
}

func TestBillingSettlementRespectsSubscriptionReset(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941212
	seedUser(t, id, 1000)
	seedToken(t, id, id, "settlement-subscription-fixture", 900)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	now := common.GetTimestamp()
	sub := &model.UserSubscription{Id: id, UserId: id, AmountTotal: 1000, AmountUsed: 200, Status: "active", StartTime: now - 7200, EndTime: now + 3600, LastResetTime: now - 3600}
	require.NoError(t, model.DB.Create(sub).Error)
	refund := &model.BillingRefund{RequestID: "settlement-previous-period", UserID: id, TokenID: id, Amount: 100, TokenAmount: 100, SubscriptionID: id, PeriodStart: now - 7200, NextAttemptAt: model.BillingRefundInFlight}
	require.NoError(t, model.QueueBillingRefund(t.Context(), refund))
	require.NoError(t, model.MarkBillingSettlementReady(t.Context(), refund.ID, id, 100, 50))
	require.NoError(t, model.SettleBillingReservation(t.Context(), refund.ID, id, 100, 50))
	require.NoError(t, model.DB.First(sub, id).Error)
	assert.EqualValues(t, 200, sub.AmountUsed)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 950, getTokenRemainQuota(t, id))
}

func TestBillingReservationRollsBackWhenRecordInsertFails(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941203
	seedUser(t, id, 1000)
	seedToken(t, id, id, "reservation-insert-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "reservation-insert-fixture", ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	const callback = "refund-test:fail-reservation-insert"
	require.NoError(t, model.DB.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "billing_refunds" {
			tx.AddError(errors.New("injected reservation insert failure"))
		}
	}))
	require.NotNil(t, session.preConsume(c, 100))
	require.NoError(t, model.DB.Callback().Create().Remove(callback))
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
	var count int64
	require.NoError(t, model.DB.Model(&model.BillingRefund{}).Where("user_id = ?", id).Count(&count).Error)
	assert.Zero(t, count)
}

func TestBillingReservationRejectsChangedIdentity(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941204
	seedUser(t, id, 1000)
	seedToken(t, id, id, "reservation-identity-fixture", 1000)
	seedToken(t, id+1, id, "reservation-other-token", 1000)
	refund := &model.BillingRefund{RequestID: "reservation-identity", NextAttemptAt: model.BillingRefundInFlight}
	_, err := model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, TokenKey: "reservation-identity-fixture", Amount: 100, UpdateToken: true, CheckWallet: true, Refund: refund})
	require.NoError(t, err)
	for _, changed := range []model.BillingQuotaReservation{
		{UserID: id, TokenID: id + 1, TokenKey: "reservation-other-token", Amount: 50, UpdateToken: true, Refund: &model.BillingRefund{ID: refund.ID}},
		{UserID: id, TokenID: id, Amount: 50, UpdateToken: false, Refund: &model.BillingRefund{ID: refund.ID}},
		{UserID: id, TokenID: id, Amount: 50, UpdateToken: true, SubscriptionID: 1, SubscriptionPeriodStart: 1, Refund: &model.BillingRefund{ID: refund.ID}},
	} {
		_, err = model.ReserveBillingQuota(t.Context(), changed)
		require.Error(t, err)
	}
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id+1))
}

type reservationCacheFault struct {
	mode   string
	armed  atomic.Bool
	ready  atomic.Bool
	failed atomic.Bool
}

func reservationCacheCommand(cmd redis.Cmder) bool {
	for _, arg := range cmd.Args() {
		if value, ok := arg.(string); ok && (strings.HasPrefix(value, "reserve_refund:") || strings.HasPrefix(value, "reserve_task:")) {
			return true
		}
	}
	return false
}

func (h *reservationCacheFault) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	if h.mode == "finish" && h.armed.Load() && h.ready.Load() && reservationCacheCommand(cmd) && h.failed.CompareAndSwap(false, true) {
		return ctx, errors.New("injected cache finish failure")
	}
	return ctx, nil
}

func (h *reservationCacheFault) AfterProcess(_ context.Context, cmd redis.Cmder) error {
	if !h.armed.Load() || cmd.Err() != nil || !reservationCacheCommand(cmd) {
		return nil
	}
	if h.mode == "prepare" && h.failed.CompareAndSwap(false, true) {
		return errors.New("injected lost cache prepare response")
	}
	h.ready.Store(true)
	return nil
}

func (*reservationCacheFault) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (*reservationCacheFault) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestBillingReservationCacheRecovery(t *testing.T) {
	requireBillingRefundMySQL(t)
	for _, mode := range []string{"prepare", "finish"} {
		t.Run(mode, func(t *testing.T) {
			truncate(t)
			const id = 941205
			const tokenKey = "reservation-recovery-fixture"
			seedUser(t, id, 1000)
			seedToken(t, id, id, tokenKey, 1000)
			client := billingTestRedisClient(t)
			previousClient, previousRedis := common.RDB, common.RedisEnabled
			common.RDB, common.RedisEnabled = client, true
			t.Cleanup(func() {
				common.RDB, common.RedisEnabled = previousClient, previousRedis
				assert.NoError(t, client.Close())
			})
			_, err := model.GetUserCache(id)
			require.NoError(t, err)
			tokenCacheKey := "token:" + common.GenerateHMAC(tokenKey)
			require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 1000, "UsedQuota", 0).Err())
			fault := &reservationCacheFault{mode: mode}
			fault.armed.Store(true)
			client.AddHook(fault)
			refund := &model.BillingRefund{RequestID: "reservation-cache-" + mode, NextAttemptAt: model.BillingRefundInFlight}
			_, err = model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, TokenKey: tokenKey, Amount: 100, UpdateToken: true, CheckWallet: true, Refund: refund})
			if mode == "prepare" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.True(t, fault.failed.Load())
			user, err := model.GetUserCache(id)
			require.NoError(t, err)
			token, err := model.GetTokenByKey(tokenKey, false)
			require.NoError(t, err)
			want := 1000
			if mode == "finish" {
				want = 900
			}
			assert.Equal(t, want, user.Quota)
			assert.Equal(t, want, token.RemainQuota)
			assert.Equal(t, want, getUserQuota(t, id))
			assert.Equal(t, want, getTokenRemainQuota(t, id))
		})
	}
}

func TestBillingTaskEvidenceAndZeroDeltaRecoverPendingReservation(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941218
	const tokenKey = "task-evidence-cache-fixture"
	seedUser(t, id, 1000)
	seedToken(t, id, id, tokenKey, 1000)
	seedChannel(t, id)
	client := billingTestRedisClient(t)
	previousClient, previousRedis, previousConsume := common.RDB, common.RedisEnabled, common.LogConsumeEnabled
	common.RDB, common.RedisEnabled, common.LogConsumeEnabled = client, true, false
	userKey := fmt.Sprintf("user:%d", id)
	tokenCacheKey := "token:" + common.GenerateHMAC(tokenKey)
	keys := []string{userKey, tokenCacheKey, fmt.Sprintf("quota:video:user:%d", id), "quota:video:" + tokenCacheKey}
	t.Cleanup(func() {
		assert.NoError(t, client.Del(context.Background(), keys...).Err())
		common.RDB, common.RedisEnabled, common.LogConsumeEnabled = previousClient, previousRedis, previousConsume
		assert.NoError(t, client.Close())
	})
	require.NoError(t, client.Del(t.Context(), keys...).Err())
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 1000, "UsedQuota", 0).Err())
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: tokenKey, RequestId: "task-evidence-cache", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_evidence_cache"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	task := &model.Task{TaskID: info.PublicTaskID, UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown, PrivateData: model.TaskPrivateData{TokenId: id, BillingSource: BillingSourceWallet, SubmissionPending: true, InitialAccountingPending: true}}
	require.NoError(t, session.StageTaskSubmission(t.Context(), task))
	fault := &reservationCacheFault{mode: "finish"}
	fault.armed.Store(true)
	client.AddHook(fault)
	require.NoError(t, session.ReserveBeforeTaskSubmit(150))
	require.True(t, fault.failed.Load())
	assert.Equal(t, "900", client.HGet(t.Context(), userKey, "Quota").Val())
	assert.True(t, strings.HasPrefix(client.HGet(t.Context(), keys[2], "Kind").Val(), "reserve_task:"))

	unavailable := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	common.RDB = unavailable
	task.PrivateData.SubmissionResponse = []byte(`{"parsed":{"taskId":"accepted"}}`)
	task.PrivateData.SubmissionQuote = &model.TaskSubmissionQuote{Quota: 150}
	task.PrivateData.ReconciliationReason = "submission outcome is uncertain"
	won, err := model.PersistTaskSubmission(t.Context(), task, false)
	common.RDB = client
	assert.NoError(t, unavailable.Close())
	require.NoError(t, err)
	require.True(t, won)
	var stored model.Task
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.Contains(t, string(stored.PrivateData.SubmissionResponse), "accepted")
	require.NotNil(t, stored.PrivateData.SubmissionFunding)
	assert.Equal(t, 150, stored.PrivateData.SubmissionFunding.ReservedQuota)
	require.NoError(t, RecoverTaskInitialAccounting(t.Context(), &stored))
	stored.Status = model.TaskStatusSuccess
	stored.PrivateData.SubmissionPending = false
	won, err = model.FinalizeVideoTask(t.Context(), &stored, model.TaskStatusUnknown, 150)
	require.NoError(t, err)
	require.True(t, won)
	assert.Equal(t, "850", client.HGet(t.Context(), userKey, "Quota").Val())
	assert.Equal(t, "850", client.HGet(t.Context(), tokenCacheKey, "RemainQuota").Val())
	assert.Equal(t, "150", client.HGet(t.Context(), tokenCacheKey, "UsedQuota").Val())
	assert.Equal(t, 850, getUserQuota(t, id))
	assert.Equal(t, 850, getTokenRemainQuota(t, id))
}

func TestBillingSubscriptionSettlementDoesNotDeadlockReservation(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941220
	seedUser(t, id, 1000)
	seedToken(t, id, id, "subscription-lock-order", 850)
	seedChannel(t, id)
	seedSubscription(t, id, id, 1000, 150)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", id).Update("used_quota", 100).Error)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 150).Error)
	task := &model.Task{TaskID: "subscription-lock-order", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown, PrivateData: model.TaskPrivateData{TokenId: id, BillingSource: BillingSourceSubscription, SubscriptionId: id, SubmissionFunding: &model.TaskSubmissionFunding{ReservedQuota: 150, UpdateToken: true}}}
	require.NoError(t, model.DB.Create(task).Error)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	connection, err := model.DB.DB()
	require.NoError(t, err)
	connection.SetMaxOpenConns(4)
	t.Cleanup(func() { connection.SetMaxOpenConns(1) })
	locked, attempted := make(chan struct{}), make(chan struct{})
	var signal sync.Once
	notify := func() { signal.Do(func() { close(attempted) }) }
	defer notify()
	results := make(chan error, 2)
	go func() {
		results <- model.DB.Transaction(func(tx *gorm.DB) error {
			var subscription model.UserSubscription
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&subscription, id).Error; err != nil {
				return err
			}
			close(locked)
			<-attempted
			return tx.Model(&model.User{}).Where("id = ?", id).UpdateColumn("id", gorm.Expr("id")).Error
		})
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription row was not locked")
	}
	const before = "refund-test:subscription-lock-before"
	const after = "refund-test:subscription-lock-after"
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(before, func(tx *gorm.DB) {
		if tx.Statement.Table == "user_subscriptions" {
			notify()
		}
	}))
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(after, func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			notify()
		}
	}))
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Callback().Query().Remove(before))
		assert.NoError(t, model.DB.Callback().Query().Remove(after))
	})
	go func() {
		task.Status = model.TaskStatusSuccess
		won, err := model.FinalizeVideoTask(t.Context(), task, model.TaskStatusUnknown, 50)
		if err == nil && !won {
			err = errors.New("settlement did not win")
		}
		results <- err
	}()
	for range 2 {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("subscription reservation and settlement did not complete")
		}
	}
	assert.EqualValues(t, 50, getSubscriptionUsed(t, id))
	assert.Equal(t, 950, getTokenRemainQuota(t, id))
}

type commitFailureConnector struct {
	driver.Connector
	fail          atomic.Bool
	rollback      bool
	afterRollback func() error
	afterCommit   func()
}

func (c *commitFailureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &commitFailureConn{Conn: conn, connector: c}, nil
}

type commitFailureConn struct {
	driver.Conn
	connector *commitFailureConnector
}

func (c *commitFailureConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &commitFailureTx{Tx: tx, connector: c.connector}, nil
}

func (c *commitFailureConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("test MySQL connection does not support BeginTx")
	}
	tx, err := beginner.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &commitFailureTx{Tx: tx, connector: c.connector}, nil
}

type commitFailureTx struct {
	driver.Tx
	connector *commitFailureConnector
}

func (tx *commitFailureTx) Commit() error {
	if tx.connector.rollback && tx.connector.fail.CompareAndSwap(true, false) {
		if err := tx.Tx.Rollback(); err != nil {
			return err
		}
		if tx.connector.afterRollback != nil {
			if err := tx.connector.afterRollback(); err != nil {
				return err
			}
		}
		return errors.New("injected failed MySQL commit")
	}
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.connector.fail.CompareAndSwap(true, false) {
		if tx.connector.afterCommit != nil {
			tx.connector.afterCommit()
		}
		return errors.New("injected lost MySQL commit response")
	}
	return nil
}

func TestBillingReservationRecoversLostCommitResponse(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941206
	const tokenKey = "reservation-commit-fixture"
	seedUser(t, id, 1000)
	seedToken(t, id, id, tokenKey, 1000)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	tokenCacheKey := "token:" + common.GenerateHMAC(tokenKey)
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 1000, "UsedQuota", 0).Err())
	config, err := mysqldriver.ParseDSN(os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN"))
	require.NoError(t, err)
	connector, err := mysqldriver.NewConnector(config)
	require.NoError(t, err)
	fault := &commitFailureConnector{Connector: connector}
	connection := sql.OpenDB(fault)
	connection.SetMaxOpenConns(2)
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{})
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; assert.NoError(t, connection.Close()) })
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: tokenKey, ForcePreConsume: true}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fault.fail.Store(true)
	require.Nil(t, session.preConsume(c, 100))
	assert.False(t, fault.fail.Load())
	fault.fail.Store(true)
	require.NoError(t, session.Reserve(150))
	assert.False(t, fault.fail.Load())
	user, err := model.GetUserCache(id)
	require.NoError(t, err)
	token, err := model.GetTokenByKey(tokenKey, false)
	require.NoError(t, err)
	assert.Equal(t, 850, user.Quota)
	assert.Equal(t, 850, token.RemainQuota)
	assert.Equal(t, 850, getUserQuota(t, id))
	assert.Equal(t, 850, getTokenRemainQuota(t, id))
	require.NoError(t, session.Settle(150))
}

func TestBillingReservationDoesNotMistakeAnotherCommitForItsOwn(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941215
	seedUser(t, id, 1000)
	seedToken(t, id, id, "reservation-other-commit-fixture", 1000)
	refund := &model.BillingRefund{RequestID: "reservation-other-commit", NextAttemptAt: model.BillingRefundInFlight}
	_, err := model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, Amount: 100, UpdateToken: true, CheckWallet: true, Refund: refund})
	require.NoError(t, err)
	config, err := mysqldriver.ParseDSN(os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN"))
	require.NoError(t, err)
	connector, err := mysqldriver.NewConnector(config)
	require.NoError(t, err)
	fault := &commitFailureConnector{Connector: connector, rollback: true}
	connection := sql.OpenDB(fault)
	connection.SetMaxOpenConns(2)
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{})
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; assert.NoError(t, connection.Close()) })
	fault.afterRollback = func() error {
		_, err := model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, Amount: 50, UpdateToken: true, CheckWallet: true, Refund: &model.BillingRefund{ID: refund.ID}})
		return err
	}
	fault.fail.Store(true)
	_, err = model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, Amount: 50, UpdateToken: true, CheckWallet: true, Refund: &model.BillingRefund{ID: refund.ID}})
	require.ErrorContains(t, err, "injected failed MySQL commit")
	assert.Equal(t, 850, getUserQuota(t, id))
	assert.Equal(t, 850, getTokenRemainQuota(t, id))
}

func TestBillingTaskStageRecoversLostCommitResponse(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941216
	seedUser(t, id, 1000)
	seedToken(t, id, id, "task-stage-commit-fixture", 1000)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "task-stage-commit-fixture", RequestId: "task-stage-commit", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_stage_commit"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	config, err := mysqldriver.ParseDSN(os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN"))
	require.NoError(t, err)
	connector, err := mysqldriver.NewConnector(config)
	require.NoError(t, err)
	fault := &commitFailureConnector{Connector: connector}
	connection := sql.OpenDB(fault)
	connection.SetMaxOpenConns(2)
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{})
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; assert.NoError(t, connection.Close()) })
	task := &model.Task{TaskID: info.PublicTaskID, UserId: id, Quota: 100, Status: model.TaskStatusUnknown, PrivateData: model.TaskPrivateData{TokenId: id, BillingSource: BillingSourceWallet, SubmissionPending: true}}
	fault.fail.Store(true)
	require.NoError(t, session.StageTaskSubmission(t.Context(), task))
	assert.False(t, fault.fail.Load())
	require.NoError(t, session.ReserveBeforeTaskSubmit(150))
	var stored model.Task
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	require.NotNil(t, stored.PrivateData.SubmissionFunding)
	assert.Equal(t, 150, stored.PrivateData.SubmissionFunding.ReservedQuota)
	assert.Equal(t, 850, getUserQuota(t, id))
	assert.Equal(t, 850, getTokenRemainQuota(t, id))
}

func TestBillingTaskStageCommitAndConfirmationFailureRecoversUnsent(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941236
	seedUser(t, id, 1000)
	seedToken(t, id, id, "task-stage-read-failure", 1000)
	seedChannel(t, id)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: "task-stage-read-failure", RequestId: "task-stage-read-failure", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_stage_read_failure"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	config, err := mysqldriver.ParseDSN(os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN"))
	require.NoError(t, err)
	connector, err := mysqldriver.NewConnector(config)
	require.NoError(t, err)
	var failRead atomic.Bool
	fault := &commitFailureConnector{Connector: connector, afterCommit: func() { failRead.Store(true) }}
	connection := sql.OpenDB(fault)
	connection.SetMaxOpenConns(2)
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{})
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; assert.NoError(t, connection.Close()) })
	const callback = "refund-test:stage-confirmation-unavailable"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "tasks" && failRead.CompareAndSwap(true, false) {
			tx.AddError(errors.New("injected confirmation read failure"))
		}
	}))
	t.Cleanup(func() { assert.NoError(t, db.Callback().Query().Remove(callback)) })
	task := &model.Task{TaskID: info.PublicTaskID, Platform: "plugin", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown, PrivateData: model.TaskPrivateData{TokenId: id, BillingSource: BillingSourceWallet, SubmissionPending: true, SubmissionPrepared: true, InitialAccountingPending: true, SubmissionAttemptID: "prepared"}}
	fault.fail.Store(true)
	require.ErrorContains(t, session.StageTaskSubmission(t.Context(), task), "injected lost MySQL commit response")
	assert.False(t, failRead.Load())
	session.Refund(c)
	var stored model.Task
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.True(t, stored.PrivateData.SubmissionPending)
	assert.True(t, stored.PrivateData.SubmissionPrepared)
	assert.Empty(t, stored.PrivateData.SubmissionResponse)
	var refunds int64
	require.NoError(t, model.DB.Model(&model.BillingRefund{}).Where("user_id = ?", id).Count(&refunds).Error)
	assert.Zero(t, refunds)
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
	previousFactory, previousLimit, previousTimeout := GetTaskAdaptorFunc, constant.TaskQueryLimit, constant.TaskTimeoutMinutes
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		t.Error("an unsent task must not contact the upstream")
		return nil
	}
	constant.TaskQueryLimit, constant.TaskTimeoutMinutes = 100, 0
	t.Cleanup(func() {
		GetTaskAdaptorFunc, constant.TaskQueryLimit, constant.TaskTimeoutMinutes = previousFactory, previousLimit, previousTimeout
	})
	RunTaskPollingOnce(t.Context(), nil)
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), stored.Status)
	assert.False(t, stored.PrivateData.SubmissionPrepared)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
	RunTaskPollingOnce(t.Context(), nil)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

func TestBillingCacheRecoveryAvoidsReverseRowLock(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941217
	const tokenKey = "reservation-lock-order-fixture"
	seedUser(t, id, 1000)
	seedToken(t, id, id, tokenKey, 1000)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	tokenCacheKey := "token:" + common.GenerateHMAC(tokenKey)
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 1000, "UsedQuota", 0).Err())
	fault := &reservationCacheFault{mode: "finish"}
	fault.armed.Store(true)
	client.AddHook(fault)
	refund := &model.BillingRefund{RequestID: "reservation-lock-order", NextAttemptAt: model.BillingRefundInFlight}
	_, err = model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, TokenKey: tokenKey, Amount: 100, UpdateToken: true, CheckWallet: true, Refund: refund})
	require.NoError(t, err)
	require.True(t, fault.failed.Load())
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
	locked, proceed, attempted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	result := make(chan error, 2)
	go func() {
		result <- model.DB.Transaction(func(tx *gorm.DB) error {
			var row model.BillingRefund
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, refund.ID).Error; err != nil {
				return err
			}
			close(locked)
			<-proceed
			close(attempted)
			return tx.Model(&model.User{}).Where("id = ?", id).Update("id", gorm.Expr("id")).Error
		})
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("refund row was not locked")
	}
	const callback = "refund-test:cache-lock-order"
	var once sync.Once
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "billing_refunds" {
			once.Do(func() { close(proceed); <-attempted })
		}
	}))
	go func() {
		_, err := model.GetUserCache(id)
		result <- err
	}()
	for range 2 {
		select {
		case err := <-result:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("cache recovery and reservation did not finish")
		}
	}
	require.NoError(t, model.DB.Callback().Query().Remove(callback))
	user, err := model.GetUserCache(id)
	require.NoError(t, err)
	assert.Equal(t, 900, user.Quota)
}

func TestBillingSubscriptionRecoveryReadsCommittedReservation(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	require.NoError(t, model.DB.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPreConsumeRecord{}))
	const id = 941218
	const tokenKey = "subscription-snapshot-fixture"
	t.Cleanup(func() {
		assert.NoError(t, model.DB.Where("user_id = ?", id).Delete(&model.SubscriptionPreConsumeRecord{}).Error)
		assert.NoError(t, model.DB.Delete(&model.SubscriptionPlan{}, id).Error)
	})
	seedUser(t, id, 1000)
	seedToken(t, id, id, tokenKey, 1000)
	now := common.GetTimestamp()
	require.NoError(t, model.DB.Create(&model.SubscriptionPlan{Id: id, Title: "snapshot fixture", TotalAmount: 1000}).Error)
	require.NoError(t, model.DB.Create(&model.UserSubscription{Id: id, UserId: id, PlanId: id, AmountTotal: 1000, Status: "active", StartTime: now - 60, EndTime: now + 3600}).Error)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	tokenCacheKey := "token:" + common.GenerateHMAC(tokenKey)
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 1000, "UsedQuota", 0).Err())
	fault := &reservationCacheFault{mode: "finish"}
	client.AddHook(fault)
	sqlDB, err := model.DB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { sqlDB.SetMaxOpenConns(1) })
	readSnapshot, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	defer resumeOnce.Do(func() { close(resume) })
	const callback = "refund-test:subscription-snapshot"
	var once sync.Once
	require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "subscription_pre_consume_records" {
			once.Do(func() { close(readSnapshot); <-resume })
		}
	}))
	result := make(chan error, 1)
	go func() {
		refund := &model.BillingRefund{RequestID: "subscription-snapshot", NextAttemptAt: model.BillingRefundInFlight}
		_, err := model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, TokenKey: tokenKey, Amount: 50, UpdateToken: true, SubscriptionRequestID: "subscription-snapshot", Refund: refund})
		result <- err
	}()
	select {
	case <-readSnapshot:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription transaction did not read its initial snapshot")
	}
	fault.armed.Store(true)
	refund := &model.BillingRefund{RequestID: "wallet-before-subscription", NextAttemptAt: model.BillingRefundInFlight}
	_, err = model.ReserveBillingQuota(t.Context(), model.BillingQuotaReservation{UserID: id, TokenID: id, TokenKey: tokenKey, Amount: 100, UpdateToken: true, CheckWallet: true, Refund: refund})
	require.NoError(t, err)
	require.True(t, fault.failed.Load())
	resumeOnce.Do(func() { close(resume) })
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("subscription reservation did not finish")
	}
	require.NoError(t, model.DB.Callback().Query().Remove(callback))
	user, err := model.GetUserCache(id)
	require.NoError(t, err)
	token, err := model.GetTokenByKey(tokenKey, false)
	require.NoError(t, err)
	assert.Equal(t, 900, user.Quota)
	assert.Equal(t, 850, token.RemainQuota)
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 850, getTokenRemainQuota(t, id))
}

func TestBillingTaskHandoffRecoversReservationCache(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941207
	const tokenKey = "task-reservation-cache-fixture"
	seedUser(t, id, 1000)
	seedToken(t, id, id, tokenKey, 1000)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	tokenCacheKey := "token:" + common.GenerateHMAC(tokenKey)
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 1000, "UsedQuota", 0).Err())
	fault := &reservationCacheFault{mode: "finish"}
	client.AddHook(fault)
	info := &relaycommon.RelayInfo{UserId: id, UserQuota: 1000, TokenId: id, TokenKey: tokenKey, RequestId: "task-reservation-cache", ForcePreConsume: true, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: "task_reservation_cache"}}
	session := &BillingSession{relayInfo: info, funding: &WalletFunding{userId: id}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	require.Nil(t, session.preConsume(c, 100))
	task := &model.Task{TaskID: info.PublicTaskID, UserId: id, Quota: 100, Status: model.TaskStatusUnknown, PrivateData: model.TaskPrivateData{TokenId: id, BillingSource: BillingSourceWallet, SubmissionPending: true, InitialAccountingPending: true}}
	require.NoError(t, session.StageTaskSubmission(t.Context(), task))
	fault.armed.Store(true)
	require.NoError(t, session.ReserveBeforeTaskSubmit(150))
	require.True(t, fault.failed.Load())
	task.Quota, task.Status, task.PrivateData.SubmissionPending = 150, model.TaskStatusQueued, false
	won, err := model.PersistTaskSubmission(t.Context(), task, true, model.TaskSubmissionFunding{ReservedQuota: 150, UpdateToken: true})
	require.NoError(t, err)
	require.True(t, won)
	user, err := model.GetUserCache(id)
	require.NoError(t, err)
	token, err := model.GetTokenByKey(tokenKey, false)
	require.NoError(t, err)
	assert.Equal(t, 850, user.Quota)
	assert.Equal(t, 850, token.RemainQuota)
	assert.Equal(t, 850, getUserQuota(t, id))
	assert.Equal(t, 850, getTokenRemainQuota(t, id))
}

func TestBillingRefundMigrationRetainsLegacyLogs(t *testing.T) {
	requireBillingRefundMySQL(t)
	dsn := os.Getenv("BILLING_REFUND_MIGRATION_DSN")
	if dsn == "" {
		t.Skip("requires a dedicated empty MySQL migration database")
	}
	config, err := mysqldriver.ParseDSN(dsn)
	require.NoError(t, err)
	host, _, err := net.SplitHostPort(config.Addr)
	require.NoError(t, err)
	require.Equal(t, "tcp", config.Net)
	require.Equal(t, "127.0.0.1", host)
	require.True(t, strings.HasPrefix(config.DBName, "billing_refund_upgrade"))
	legacy, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	legacySQL, err := legacy.DB()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, legacySQL.Close()) })
	var tables int64
	require.NoError(t, legacy.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&tables).Error)
	require.Zero(t, tables)
	require.NoError(t, legacy.Exec("CREATE TABLE video_task_pending_logs (task_id bigint NOT NULL PRIMARY KEY, payload text NOT NULL, claim_id varchar(64), claim_until bigint)").Error)
	require.NoError(t, legacy.Exec("CREATE TABLE billing_refunds (id bigint NOT NULL AUTO_INCREMENT PRIMARY KEY, request_id varchar(64), user_id bigint, token_id bigint, amount bigint, token_amount bigint, subscription_id bigint, period_start bigint, completed boolean, cache_pending boolean, next_attempt_at bigint NOT NULL DEFAULT 0, attempts bigint, created_at bigint, UNIQUE KEY idx_billing_refunds_request_id (request_id))").Error)
	require.NoError(t, legacy.Exec("INSERT INTO billing_refunds (request_id, user_id, token_id, amount, token_amount, subscription_id, period_start, completed, cache_pending, next_attempt_at, attempts, created_at) VALUES (?, ?, ?, 100, 100, 0, 0, false, false, 0, 0, ?)", "legacy-unknown", 941208, 941208, common.GetTimestamp()).Error)
	entry := &model.Log{UserId: 941114, CreatedAt: common.GetTimestamp(), RequestId: "refund-upgrade-log", Type: model.LogTypeRefund, Quota: 100}
	payload, err := common.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, legacy.Table("video_task_pending_logs").Create(map[string]any{"task_id": 941114, "payload": string(payload), "claim_until": 0}).Error)

	previousDB, previousLog := model.DB, model.LOG_DB
	previousMainType, previousLogType, previousMaster := common.MainDatabaseType(), common.LogDatabaseType(), common.IsMasterNode
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode = true
	var opened []*gorm.DB
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLog
		common.SetDatabaseTypes(previousMainType, previousLogType)
		common.IsMasterNode = previousMaster
		for _, db := range opened {
			connection, err := db.DB()
			if assert.NoError(t, err) {
				assert.NoError(t, connection.Close())
			}
		}
	})
	for range 2 {
		require.NoError(t, model.InitDB())
		opened = append(opened, model.DB)
		model.LOG_DB = model.DB
		assert.True(t, model.HasPendingVideoTaskLogs())
	}
	seedUser(t, 941208, 900)
	seedToken(t, 941208, 941208, "legacy-refund-fixture", 900)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 941208).Update("used_quota", 100).Error)
	var refund model.BillingRefund
	require.NoError(t, model.DB.Where("request_id = ?", "legacy-unknown").First(&refund).Error)
	assert.False(t, refund.DecisionKnown)
	assert.False(t, model.HasPendingBillingRefunds())
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	assert.Equal(t, 900, getUserQuota(t, 941208))
	require.NoError(t, model.MarkBillingRefundReady(t.Context(), refund.ID, 941208))
	require.NoError(t, model.RetryBillingRefunds(t.Context()))
	assert.Equal(t, 1000, getUserQuota(t, 941208))
	assert.Equal(t, 1000, getTokenRemainQuota(t, 941208))
	require.NoError(t, model.RetryVideoTaskLogs(t.Context()))
	var count int64
	require.NoError(t, model.DB.Model(&model.Log{}).Where("request_id = ?", entry.RequestId).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	assert.False(t, model.HasPendingVideoTaskLogs())
}

func TestBillingRetryDoesNotSettleAReplacedReconciliation(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id, nextChannel = 941231, 941232
	seedUser(t, id, 900)
	seedToken(t, id, id, "retry-reconcile-fixture", 900)
	seedChannel(t, id)
	seedChannel(t, nextChannel)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	previous := &model.Task{TaskID: "retry-reconcile", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown,
		PrivateData: model.TaskPrivateData{TokenId: id, SubmissionPending: true, InitialAccountingPending: true, ReconciliationRequired: true}}
	require.NoError(t, model.DB.Create(previous).Error)
	won, err := model.CommitTaskInitialAccounting(t.Context(), previous)
	require.NoError(t, err)
	require.True(t, won)
	stale := *previous
	next := *previous
	next.ChannelId = nextChannel
	next.PrivateData.InitialAccountingPending = true
	next.PrivateData.ReconciliationRequired = false
	next.PrivateData.SubmissionPrepared = true
	next.PrivateData.SubmissionAttemptID = "next"
	if err := model.ReplaceTaskSubmissionIntent(t.Context(), previous, &next); err != nil {
		assert.Equal(t, 900, getUserQuota(t, id))
		return
	}
	next.PrivateData.ReconciliationReason = "submission outcome is uncertain"
	won, err = model.PersistTaskSubmission(t.Context(), &next, false)
	require.NoError(t, err)
	require.True(t, won)
	won, err = model.CommitTaskInitialAccounting(t.Context(), &next)
	require.NoError(t, err)
	require.True(t, won)
	used, count := getUserUsageAccounting(t, id)
	assert.Equal(t, 100, used, "retry must not count the initial reservation twice")
	assert.Equal(t, 1, count, "retry must not count the request twice")
	stale.Status = model.TaskStatusFailure
	stale.PrivateData.SubmissionPending = false
	won, err = model.FinalizeVideoTask(t.Context(), &stale, model.TaskStatusUnknown, 0)
	require.NoError(t, err)
	assert.False(t, won, "an old reconciliation must not refund a newer submission")
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
}

func TestBillingRetryConsumptionUsesTheStagedChannel(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id, nextChannel = 941233, 941234
	seedUser(t, id, 900)
	seedToken(t, id, id, "retry-channel-fixture", 900)
	seedChannel(t, id)
	seedChannel(t, nextChannel)
	task := &model.Task{TaskID: "retry-channel", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown,
		Properties:  model.Properties{OriginModelName: "fixture-model"},
		PrivateData: model.TaskPrivateData{TokenId: id, SubmissionPending: true, InitialAccountingPending: true}}
	require.NoError(t, model.DB.Create(task).Error)
	info := &relaycommon.RelayInfo{UserId: id, TokenId: id, OriginModelName: "fixture-model", ChannelMeta: &relaycommon.ChannelMeta{ChannelId: nextChannel}, TaskRelayInfo: &relaycommon.TaskRelayInfo{}}
	info.PriceData.Quota = task.Quota
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", nil)
	LogTaskConsumption(c, info, task)
	var log model.Log
	require.NoError(t, model.DB.Where("user_id = ? AND type = ?", id, model.LogTypeConsume).First(&log).Error)
	assert.Equal(t, id, log.ChannelId)
	assert.EqualValues(t, 100, getChannelUsedQuota(t, id))
	assert.Zero(t, getChannelUsedQuota(t, nextChannel))
}

func TestBillingRetryRejectsStaleAttemptSnapshots(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941235
	seedUser(t, id, 900)
	seedToken(t, id, id, "retry-snapshot-fixture", 900)
	seedChannel(t, id)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	first := &model.Task{TaskID: "retry-snapshot", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown,
		PrivateData: model.TaskPrivateData{TokenId: id, SubmissionPending: true, InitialAccountingPending: true, NextPollAt: time.Now().Add(time.Minute).Unix()}}
	require.NoError(t, common.Unmarshal([]byte(`{"submission_attempt_id":"first"}`), &first.PrivateData))
	require.NoError(t, model.DB.Create(first).Error)
	stale := *first
	next := *first
	require.NoError(t, common.Unmarshal([]byte(`{"submission_attempt_id":"second"}`), &next.PrivateData))
	next.PrivateData.SubmissionPrepared = true
	require.NoError(t, model.ReplaceTaskSubmissionIntent(t.Context(), first, &next))
	require.NoError(t, model.MarkTaskSubmissionDispatched(t.Context(), &next))
	stale.PrivateData.ReconciliationRequired = true
	won, err := stale.UpdateWithStatus(model.TaskStatusUnknown)
	require.NoError(t, err)
	assert.False(t, won, "old polling state must not overwrite the new attempt")
	won, err = model.PersistTaskSubmission(t.Context(), &stale, false)
	require.NoError(t, err)
	assert.False(t, won, "old response evidence must not overwrite the new attempt")
	won, err = model.CommitTaskInitialAccounting(t.Context(), &next)
	require.NoError(t, err)
	require.True(t, won)
	stale.Status = model.TaskStatusFailure
	won, err = model.FinalizeVideoTask(t.Context(), &stale, model.TaskStatusUnknown, 0)
	require.NoError(t, err)
	assert.False(t, won, "old terminal state must not refund the new attempt")
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
}

func TestBillingPreparedRecoveryAndDispatchAreExclusive(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941238
	seedUser(t, id, 900)
	seedToken(t, id, id, "prepared-dispatch-fixture", 900)
	seedChannel(t, id)
	task := &model.Task{TaskID: "prepared-dispatch", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown,
		PrivateData: model.TaskPrivateData{TokenId: id, SubmissionPending: true, SubmissionPrepared: true, InitialAccountingPending: true, SubmissionAttemptID: "attempt", NextPollAt: time.Now().Add(time.Minute).Unix()}}
	require.NoError(t, model.DB.Create(task).Error)
	stale := *task
	require.NoError(t, model.MarkTaskSubmissionDispatched(t.Context(), task))
	_, err := model.CommitTaskInitialAccounting(t.Context(), &stale)
	require.ErrorContains(t, err, "task submission attempt changed")
	won, err := stale.UpdateWithStatus(model.TaskStatusUnknown)
	require.NoError(t, err)
	assert.False(t, won)
	won, err = model.PersistTaskSubmission(t.Context(), &stale, false)
	require.NoError(t, err)
	assert.False(t, won)
	stale.Status = model.TaskStatusFailure
	won, err = model.FinalizeVideoTask(t.Context(), &stale, model.TaskStatusUnknown, 0)
	require.NoError(t, err)
	assert.False(t, won)
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
}

func TestBillingRecoveredPreparedIntentBlocksDispatch(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941240
	seedUser(t, id, 900)
	seedToken(t, id, id, "prepared-accounting-fixture", 900)
	seedChannel(t, id)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	task := &model.Task{TaskID: "prepared-accounting", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown,
		PrivateData: model.TaskPrivateData{TokenId: id, SubmissionPending: true, SubmissionPrepared: true, InitialAccountingPending: true, SubmissionAttemptID: "attempt", NextPollAt: time.Now().Add(time.Minute).Unix()}}
	require.NoError(t, model.DB.Create(task).Error)
	staleDispatch := *task
	won, err := model.CommitTaskInitialAccounting(t.Context(), task)
	require.NoError(t, err)
	require.True(t, won)
	require.ErrorContains(t, model.MarkTaskSubmissionDispatched(t.Context(), &staleDispatch), "no longer dispatchable")
	task.PrivateData.NextPollAt = 0
	require.NoError(t, model.DB.Model(task).Update("private_data", task.PrivateData).Error)
	task.Status, task.PrivateData.SubmissionPending = model.TaskStatusFailure, false
	won, err = model.FinalizeVideoTask(t.Context(), task, model.TaskStatusUnknown, 0)
	require.NoError(t, err)
	require.True(t, won)
	won, err = model.FinalizeVideoTask(t.Context(), task, model.TaskStatusUnknown, 0)
	require.NoError(t, err)
	assert.False(t, won)
	used, count := getUserUsageAccounting(t, id)
	assert.Zero(t, used)
	assert.Equal(t, 1, count)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
}

type preparedRedisFault struct{ enabled atomic.Bool }

func (h *preparedRedisFault) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	if h.enabled.Load() {
		return ctx, errors.New("injected Redis unavailable")
	}
	return ctx, nil
}
func (*preparedRedisFault) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (*preparedRedisFault) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (*preparedRedisFault) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestBillingPreparedRecoveryRetriesAfterRedisFailure(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941241
	const tokenKey = "prepared-redis-fixture"
	seedUser(t, id, 900)
	seedToken(t, id, id, tokenKey, 900)
	seedChannel(t, id)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", id).Update("used_quota", 100).Error)
	client := billingTestRedisClient(t)
	previousClient, previousRedis := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousRedis
		assert.NoError(t, client.Close())
	})
	_, err := model.GetUserCache(id)
	require.NoError(t, err)
	tokenCacheKey := "token:" + common.GenerateHMAC(tokenKey)
	require.NoError(t, client.HSet(t.Context(), tokenCacheKey, "Id", id, "UserId", id, "RemainQuota", 900, "UsedQuota", 100).Err())
	fault := &preparedRedisFault{}
	client.AddHook(fault)
	task := &model.Task{TaskID: "prepared-redis", Platform: "plugin", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown,
		PrivateData: model.TaskPrivateData{TokenId: id, SubmissionPending: true, SubmissionPrepared: true, InitialAccountingPending: true, SubmissionAttemptID: "attempt", SubmissionFunding: &model.TaskSubmissionFunding{ReservedQuota: 100, UpdateToken: true}}}
	require.NoError(t, model.DB.Create(task).Error)
	previousFactory, previousLimit, previousTimeout := GetTaskAdaptorFunc, constant.TaskQueryLimit, constant.TaskTimeoutMinutes
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		t.Error("an unsent task must not contact the upstream")
		return nil
	}
	constant.TaskQueryLimit, constant.TaskTimeoutMinutes = 100, 0
	t.Cleanup(func() {
		GetTaskAdaptorFunc, constant.TaskQueryLimit, constant.TaskTimeoutMinutes = previousFactory, previousLimit, previousTimeout
	})
	fault.enabled.Store(true)
	RunTaskPollingOnce(t.Context(), nil)
	var stored model.Task
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusUnknown), stored.Status)
	assert.True(t, stored.PrivateData.SubmissionPrepared)
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
	fault.enabled.Store(false)
	RunTaskPollingOnce(t.Context(), nil)
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.Equal(t, model.TaskStatus(model.TaskStatusFailure), stored.Status)
	assert.Equal(t, 1000, getUserQuota(t, id))
	assert.Equal(t, 1000, getTokenRemainQuota(t, id))
	cachedUser, err := model.GetUserCache(id)
	require.NoError(t, err)
	cachedToken, err := model.GetTokenByKey(tokenKey, false)
	require.NoError(t, err)
	assert.Equal(t, 1000, cachedUser.Quota)
	assert.Equal(t, 1000, cachedToken.RemainQuota)
	used, count := getUserUsageAccounting(t, id)
	assert.Zero(t, used)
	assert.Equal(t, 1, count)
}

func TestBillingDispatchCommitUncertaintyKeepsReserve(t *testing.T) {
	requireBillingRefundMySQL(t)
	truncate(t)
	const id = 941239
	seedUser(t, id, 900)
	seedToken(t, id, id, "dispatch-commit-fixture", 900)
	seedChannel(t, id)
	task := &model.Task{TaskID: "dispatch-commit", UserId: id, ChannelId: id, Quota: 100, Status: model.TaskStatusUnknown,
		PrivateData: model.TaskPrivateData{TokenId: id, SubmissionPending: true, SubmissionPrepared: true, InitialAccountingPending: true, SubmissionAttemptID: "attempt", NextPollAt: time.Now().Add(time.Minute).Unix()}}
	require.NoError(t, model.DB.Create(task).Error)
	config, err := mysqldriver.ParseDSN(os.Getenv("TASK_PLUGIN_TEST_MYSQL_DSN"))
	require.NoError(t, err)
	connector, err := mysqldriver.NewConnector(config)
	require.NoError(t, err)
	fault := &commitFailureConnector{Connector: connector}
	connection := sql.OpenDB(fault)
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: connection, SkipInitializeWithVersion: true}), &gorm.Config{})
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = previousDB; assert.NoError(t, connection.Close()) })
	fault.fail.Store(true)
	require.ErrorContains(t, model.MarkTaskSubmissionDispatched(t.Context(), task), "injected lost MySQL commit response")
	var stored model.Task
	require.NoError(t, model.DB.First(&stored, task.ID).Error)
	assert.False(t, stored.PrivateData.SubmissionPrepared)
	won, err := model.PersistTaskSubmission(t.Context(), task, false)
	require.NoError(t, err)
	assert.False(t, won)
	task.Status = model.TaskStatusFailure
	won, err = model.FinalizeVideoTask(t.Context(), task, model.TaskStatusUnknown, 0)
	require.NoError(t, err)
	assert.False(t, won)
	assert.Equal(t, 900, getUserQuota(t, id))
	assert.Equal(t, 900, getTokenRemainQuota(t, id))
}
