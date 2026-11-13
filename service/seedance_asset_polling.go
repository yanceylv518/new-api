package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

const (
	seedanceAssetPollBatchSize   = 20
	seedanceAssetPollWorkerSize  = 4
	seedanceAssetPollLease       = 90 * time.Second
	seedanceAssetPollBaseDelay   = 5 * time.Second
	seedanceAssetPollMaxDelay    = 60 * time.Second
	seedanceAssetPollMaxAttempts = model.SeedanceAssetSyncMaxAttempts
	seedanceAssetReviewTimeout   = 15 * time.Minute
)

// SeedanceAssetPollSummary 记录一次后台素材状态同步的结果，避免把单个上游失败扩大为整批失败。
type SeedanceAssetPollSummary struct {
	Candidates     int `json:"candidates"`
	Claimed        int `json:"claimed"`
	Updated        int `json:"updated"`
	RetryScheduled int `json:"retry_scheduled"`
	Skipped        int `json:"skipped"`
	Errors         int `json:"errors"`
}

type seedanceAssetPollResult struct {
	claimed        bool
	updated        bool
	retryScheduled bool
	skipped        bool
	err            error
}

type seedanceAssetPollJob struct {
	asset       *model.SeedanceAsset
	replica     *model.SeedanceAssetReplica
	validation  *model.SeedanceAssetValidationSession
	synchronize *seedanceAssetSyncJob
}

// SeedanceAssetPollSchedule 根据状态和已有次数计算下一次轮询时间，终态素材直接停止调度。
func SeedanceAssetPollSchedule(status string, previousAttempts int, now int64) (int, int64) {
	normalizedStatus := normalizeSeedanceAssetStatus(status)
	if normalizedStatus != "processing" && normalizedStatus != "pending" {
		return 0, 0
	}
	if previousAttempts < 0 {
		previousAttempts = 0
	}
	attempts := previousAttempts + 1
	if attempts > seedanceAssetPollMaxAttempts {
		attempts = seedanceAssetPollMaxAttempts
	}
	delay := seedanceAssetPollBaseDelay
	for step := 1; step < attempts && delay < seedanceAssetPollMaxDelay; step++ {
		delay *= 2
		if delay > seedanceAssetPollMaxDelay {
			delay = seedanceAssetPollMaxDelay
		}
	}
	if now <= 0 {
		now = common.GetTimestamp()
	}
	return attempts, now + int64(delay/time.Second)
}

func seedanceAssetReviewPollSchedule(status string, previousAttempts int, createdAt time.Time, now int64) (int, int64, bool) {
	attempts, nextPollAt := SeedanceAssetPollSchedule(status, previousAttempts, now)
	if now <= 0 {
		now = common.GetTimestamp()
	}
	timedOut := nextPollAt > 0 && attempts >= seedanceAssetPollMaxAttempts && !createdAt.IsZero() && now-createdAt.Unix() >= int64(seedanceAssetReviewTimeout/time.Second)
	if timedOut {
		nextPollAt = now + int64(seedanceAssetSyncRecoveryDelay/time.Second)
	}
	return attempts, nextPollAt, timedOut
}

// SeedanceAssetPrimaryReviewUpdates 区分本站等待超时和上游明确拒绝，并保留后台恢复。
func SeedanceAssetPrimaryReviewUpdates(asset *model.SeedanceAsset, details SeedanceAssetDetails, now int64) map[string]any {
	attempts, nextPollAt, timedOut := seedanceAssetReviewPollSchedule(details.Status, asset.PollAttempts, asset.CreatedAt, now)
	updates := details.UpdateValues(true)
	if timedOut {
		updates["status"] = "Failed"
		updates["failure_code"] = model.SeedanceAssetReviewTimeoutCode
	} else if asset.FailureCode == model.SeedanceAssetReviewTimeoutCode {
		updates["failure_code"] = ""
	}
	updates["poll_attempts"], updates["next_poll_at"], updates["poll_lease_until"] = attempts, nextPollAt, int64(0)
	return updates
}

func seedanceAssetReplicaReviewUpdates(replica *model.SeedanceAssetReplica, details SeedanceAssetDetails, now int64) map[string]any {
	attempts, nextPollAt, timedOut := seedanceAssetReviewPollSchedule(details.Status, replica.PollAttempts, replica.CreatedAt, now)
	updates := details.UpdateValues(false)
	updates["poll_attempts"], updates["next_poll_at"], updates["poll_lease_until"] = attempts, nextPollAt, int64(0)
	updates["last_error"] = ""
	if timedOut {
		updates["last_error"] = model.SeedanceAssetReviewTimeoutCode
	}
	return updates
}

// RunSeedanceAssetPollingOnce 批量同步到期素材，限制批量规模和并发数以避免放大上游请求。
func RunSeedanceAssetPollingOnce(ctx context.Context, report func(processed, total int)) (SeedanceAssetPollSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 取消必须成为任务失败结果，不能记录成一次成功的空轮询。
	if err := ctx.Err(); err != nil {
		return SeedanceAssetPollSummary{}, err
	}
	if model.DB == nil {
		return SeedanceAssetPollSummary{}, errors.New("database is not initialized")
	}
	assets, err := model.ListDueSeedanceAssets(common.GetTimestamp(), seedanceAssetPollBatchSize)
	if err != nil {
		return SeedanceAssetPollSummary{}, err
	}
	replicas, err := model.ListDueSeedanceAssetReplicas(common.GetTimestamp(), seedanceAssetPollBatchSize)
	if err != nil {
		return SeedanceAssetPollSummary{}, err
	}
	jobsList := make([]seedanceAssetPollJob, 0, len(assets)+len(replicas))
	for _, asset := range assets {
		jobsList = append(jobsList, seedanceAssetPollJob{asset: asset})
	}
	for _, replica := range replicas {
		jobsList = append(jobsList, seedanceAssetPollJob{replica: replica})
	}
	return runSeedanceAssetPollJobs(ctx, jobsList, report)
}

// RunSeedanceAssetValidationPollingOnce 同步需要后台检查的认证会话。
func RunSeedanceAssetValidationPollingOnce(ctx context.Context, report func(processed, total int)) (SeedanceAssetPollSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SeedanceAssetPollSummary{}, err
	}
	sessions, err := model.ListDueSeedanceAssetValidationSessions(ctx, common.GetTimestamp(), seedanceAssetPollBatchSize)
	if err != nil {
		return SeedanceAssetPollSummary{}, err
	}
	jobs := make([]seedanceAssetPollJob, 0, len(sessions))
	for _, session := range sessions {
		jobs = append(jobs, seedanceAssetPollJob{validation: session})
	}
	return runSeedanceAssetPollJobs(ctx, jobs, report)
}

func runSeedanceAssetPollJobs(ctx context.Context, jobsList []seedanceAssetPollJob, report func(processed, total int)) (SeedanceAssetPollSummary, error) {
	summary := SeedanceAssetPollSummary{Candidates: len(jobsList)}
	if report != nil {
		report(0, len(jobsList))
	}
	if len(jobsList) == 0 {
		return summary, nil
	}

	jobs := make(chan seedanceAssetPollJob, len(jobsList))
	results := make(chan seedanceAssetPollResult, len(jobsList))
	for _, job := range jobsList {
		jobs <- job
	}
	close(jobs)

	workerSize := seedanceAssetPollWorkerSize
	if workerSize > len(jobsList) {
		workerSize = len(jobsList)
	}
	var workers sync.WaitGroup
	workers.Add(workerSize)
	for worker := 0; worker < workerSize; worker++ {
		go func() {
			defer workers.Done()
			for job := range jobs {
				result := seedanceAssetPollResult{skipped: true}
				if ctx.Err() == nil {
					if job.synchronize != nil {
						result = synchronizeSeedanceAssetReplica(ctx, job.synchronize)
					} else if job.asset != nil {
						result = pollSeedanceAsset(ctx, job.asset)
					} else if job.replica != nil {
						result = pollSeedanceAssetReplica(ctx, job.replica)
					} else if job.validation != nil {
						result = pollSeedanceAssetValidation(ctx, job.validation)
					}
				}
				results <- result
			}
		}()
	}
	workers.Wait()
	close(results)

	processed := 0
	for result := range results {
		processed++
		if result.claimed {
			summary.Claimed++
		}
		if result.updated {
			summary.Updated++
		}
		if result.retryScheduled {
			summary.RetryScheduled++
		}
		if result.skipped {
			summary.Skipped++
		}
		if result.err != nil {
			summary.Errors++
			logger.LogWarn(ctx, fmt.Sprintf("Seedance asset polling failed: %v", result.err))
		}
		if report != nil {
			report(processed, len(jobsList))
		}
	}
	return summary, ctx.Err()
}

func pollSeedanceAssetValidation(ctx context.Context, session *model.SeedanceAssetValidationSession) seedanceAssetPollResult {
	current, err := refreshSeedanceAssetValidationSession(ctx, session.UserID, session.ID, "", session.UpdatedAt)
	if err != nil {
		return seedanceAssetPollResult{err: err}
	}
	if current.UpdatedAt.Equal(session.UpdatedAt) && current.Status == session.Status {
		return seedanceAssetPollResult{skipped: true}
	}
	result := seedanceAssetPollResult{claimed: true}
	if current.Status == model.SeedanceAssetValidationStatusPending {
		result.retryScheduled = true
	} else {
		result.updated = true
	}
	return result
}

// pollSeedanceAssetReplica 用与原素材相同的租约协议同步目标账号副本。
func pollSeedanceAssetReplica(ctx context.Context, replica *model.SeedanceAssetReplica) seedanceAssetPollResult {
	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetPollLease/time.Second)
	claimed, err := model.ClaimSeedanceAssetReplicaForPolling(replica.ID, replica.UpdatedAt, now, leaseUntil)
	if err != nil {
		return seedanceAssetPollResult{err: err}
	}
	if !claimed {
		return seedanceAssetPollResult{skipped: true}
	}
	result := seedanceAssetPollResult{claimed: true}
	client, err := resolveSeedanceAssetSyncClient(ctx, replica)
	if err != nil {
		return scheduleSeedanceAssetReplicaRetry(ctx, result, replica, leaseUntil, err)
	}
	details, err := client.GetCheckedSeedanceAssetDetails(ctx, replica.UpstreamAssetID)
	if err != nil {
		if ctx.Err() != nil {
			return result
		}
		return scheduleSeedanceAssetReplicaRetry(ctx, result, replica, leaseUntil, err)
	}
	if ctx.Err() != nil {
		return result
	}
	updates := seedanceAssetReplicaReviewUpdates(replica, details, common.GetTimestamp())
	updated, err := model.UpdateSeedanceAssetReplicaPollState(replica.ID, leaseUntil, updates)
	if err != nil {
		return seedanceAssetPollResult{claimed: true, err: err}
	}
	if !updated {
		return seedanceAssetPollResult{claimed: true, skipped: true}
	}
	if err := model.RefreshReplicaManagedSeedanceAsset(ctx, replica.UserID, replica.LocalAssetID); err != nil {
		return seedanceAssetPollResult{claimed: true, err: err}
	}
	return seedanceAssetPollResult{claimed: true, updated: true}
}

func scheduleSeedanceAssetReplicaRetry(ctx context.Context, result seedanceAssetPollResult, replica *model.SeedanceAssetReplica, leaseUntil int64, pollErr error) seedanceAssetPollResult {
	previousAttempts := replica.PollAttempts
	if replica.LastError == "" {
		previousAttempts = 0
	}
	now := common.GetTimestamp()
	attempts, nextPollAt, timedOut := seedanceAssetReviewPollSchedule(replica.Status, previousAttempts, replica.CreatedAt, now)
	message := truncateSeedanceAssetMappingError(pollErr.Error())
	if timedOut {
		message = model.SeedanceAssetReviewTimeoutCode
	}
	updates := map[string]any{
		"poll_attempts": attempts, "next_poll_at": nextPollAt,
		"poll_lease_until": int64(0), "last_error": message,
	}
	if attempts >= seedanceAssetPollMaxAttempts {
		nextPollAt = now + int64(seedanceAssetSyncRecoveryDelay/time.Second)
		updates["next_poll_at"] = nextPollAt
	}
	updated, err := model.UpdateSeedanceAssetReplicaPollState(replica.ID, leaseUntil, updates)
	if err != nil {
		result.err = errors.Join(pollErr, fmt.Errorf("retry schedule update failed: %w", err))
		return result
	}
	if !updated {
		result.skipped = true
		return result
	}
	result.retryScheduled = nextPollAt > 0
	if refreshErr := model.RefreshReplicaManagedSeedanceAsset(ctx, replica.UserID, replica.LocalAssetID); refreshErr != nil {
		pollErr = errors.Join(pollErr, refreshErr)
	}
	result.err = pollErr
	return result
}

// pollSeedanceAsset 抢占单个素材后查询上游，并用租约条件提交结果。
func pollSeedanceAsset(ctx context.Context, asset *model.SeedanceAsset) seedanceAssetPollResult {
	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetPollLease/time.Second)
	claimed, err := model.ClaimSeedanceAssetForPolling(asset.ID, asset.UpdatedAt, now, leaseUntil)
	if err != nil {
		return seedanceAssetPollResult{err: err}
	}
	if !claimed {
		return seedanceAssetPollResult{skipped: true}
	}
	result := seedanceAssetPollResult{claimed: true}
	modelScoped := asset.LocalOnly != nil && !*asset.LocalOnly && asset.PrimaryUpstreamID != ""
	client, err := ResolveSeedanceAssetClientWithinScope(ctx, asset.UserID, asset.ChannelID, asset.KeyFingerprint, asset.AccountFingerprint, asset.PrimarySyncScope, modelScoped)
	if err != nil {
		return scheduleSeedanceAssetRetry(result, asset, leaseUntil, err)
	}
	var details SeedanceAssetDetails
	if asset.LocalOnly != nil && !*asset.LocalOnly {
		details, err = client.GetCheckedSeedanceAssetDetails(ctx, asset.UpstreamAssetID())
	} else {
		details, err = client.GetSeedanceAssetDetails(ctx, asset.UpstreamAssetID())
	}
	if err != nil {
		if ctx.Err() != nil {
			return result
		}
		return scheduleSeedanceAssetRetry(result, asset, leaseUntil, err)
	}
	if ctx.Err() != nil {
		return result
	}
	updates := SeedanceAssetPrimaryReviewUpdates(asset, details, common.GetTimestamp())
	updated, err := model.UpdateSeedanceAssetPollState(asset.ID, leaseUntil, updates)
	if err != nil {
		return seedanceAssetPollResult{claimed: true, err: err}
	}
	if !updated {
		return seedanceAssetPollResult{claimed: true, skipped: true}
	}
	return seedanceAssetPollResult{claimed: true, updated: true}
}

// scheduleSeedanceAssetRetry 在查询超时后保留低频恢复，不将其当作上游审核拒绝。
func scheduleSeedanceAssetRetry(result seedanceAssetPollResult, asset *model.SeedanceAsset, leaseUntil int64, pollErr error) seedanceAssetPollResult {
	status := asset.Status
	if asset.FailureCode == model.SeedanceAssetReviewTimeoutCode {
		status = "Processing"
	}
	attempts, nextPollAt, timedOut := seedanceAssetReviewPollSchedule(status, asset.PollAttempts, asset.CreatedAt, common.GetTimestamp())
	updates := map[string]any{
		"poll_attempts":    attempts,
		"next_poll_at":     nextPollAt,
		"poll_lease_until": int64(0),
	}
	if timedOut {
		updates["status"] = "Failed"
		updates["failure_code"] = model.SeedanceAssetReviewTimeoutCode
	}
	updated, err := model.UpdateSeedanceAssetPollState(asset.ID, leaseUntil, updates)
	if err != nil {
		result.err = fmt.Errorf("%v; retry schedule update failed: %w", pollErr, err)
		return result
	}
	if !updated {
		result.skipped = true
		return result
	}
	result.retryScheduled = true
	result.err = pollErr
	return result
}

// normalizeSeedanceAssetStatus 统一处理上游状态大小写和空白，确保退避判断稳定。
func normalizeSeedanceAssetStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "success" || status == "succeeded" {
		return "active"
	}
	return status
}
