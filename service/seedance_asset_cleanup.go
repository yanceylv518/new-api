package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	seedanceAssetCleanupBatchSize          = 50
	seedanceAssetCleanupWorkerSize         = 4
	seedanceAssetCleanupLease              = 5 * time.Minute
	seedanceAssetCleanupOperationTimeout   = 2 * time.Minute
	seedanceAssetCleanupBaseDelay          = 30 * time.Second
	seedanceAssetCleanupMaxDelay           = time.Hour
	seedanceAssetCleanupMaxBackoffAttempts = 20
)

// SeedanceAssetCleanupSummary 记录一次补偿清理批次的结果，便于系统任务审计。
type SeedanceAssetCleanupSummary struct {
	Candidates     int `json:"candidates"`
	Claimed        int `json:"claimed"`
	Completed      int `json:"completed"`
	RetryScheduled int `json:"retry_scheduled"`
	Skipped        int `json:"skipped"`
	Errors         int `json:"errors"`
}

type seedanceAssetCleanupResult struct {
	claimed        bool
	completed      bool
	retryScheduled bool
	skipped        bool
	err            error
}

// NewSeedanceAssetDeleteCleanupJob 把本地素材快照转换为后台删除任务，确保外部删除完成后才移除映射。
func NewSeedanceAssetDeleteCleanupJob(asset *model.SeedanceAsset) model.SeedanceAssetCleanupJob {
	if asset == nil {
		return model.SeedanceAssetCleanupJob{}
	}
	return model.SeedanceAssetCleanupJob{
		Kind:           model.SeedanceAssetCleanupKindAssetDelete,
		UserID:         asset.UserID,
		LocalAssetID:   asset.ID,
		ChannelID:      asset.ChannelID,
		KeyFingerprint: asset.KeyFingerprint,
		UpstreamID:     asset.AssetID,
		UpstreamDoneAt: asset.UpstreamDeletedAt,
		ObjectKey:      asset.ObjectKey,
		Storage:        asset.Storage,
		DedupKey: seedanceAssetCleanupDedupKey(
			model.SeedanceAssetCleanupKindAssetDelete,
			fmt.Sprintf("%d", asset.UserID),
			fmt.Sprintf("%d", asset.ID),
		),
		Status:        model.SeedanceAssetCleanupStatusPending,
		NextAttemptAt: common.GetTimestamp(),
	}
}

// queueSeedanceAssetUpstreamCleanup 记录上游资源清理失败，客户端恢复后由后台任务继续处理。
func queueSeedanceAssetUpstreamCleanup(kind string, client *SeedanceAssetClient, upstreamID string) error {
	if client == nil || strings.TrimSpace(upstreamID) == "" {
		return errors.New("invalid Seedance upstream cleanup target")
	}
	job := &model.SeedanceAssetCleanupJob{
		Kind:           kind,
		ChannelID:      client.ChannelID,
		KeyFingerprint: client.KeyFingerprint,
		UpstreamID:     upstreamID,
		DedupKey:       seedanceAssetCleanupDedupKey(kind, fmt.Sprintf("%d", client.ChannelID), client.KeyFingerprint, upstreamID),
		Status:         model.SeedanceAssetCleanupStatusPending,
		NextAttemptAt:  common.GetTimestamp(),
	}
	if err := model.CreateSeedanceAssetCleanupJob(job); err != nil {
		return err
	}
	enqueueSeedanceAssetCleanupTask()
	return nil
}

// queueSeedanceAssetObjectCleanup 记录 OSS 对象清理失败，并固定当时的存储位置以支持配置轮换。
func queueSeedanceAssetObjectCleanup(objectKey string, storage model.SeedanceAssetStorage) error {
	if strings.TrimSpace(objectKey) == "" {
		return nil
	}
	job := &model.SeedanceAssetCleanupJob{
		Kind:          model.SeedanceAssetCleanupKindOSSObject,
		ObjectKey:     objectKey,
		Storage:       storage,
		DedupKey:      seedanceAssetCleanupDedupKey(model.SeedanceAssetCleanupKindOSSObject, storage.Bucket, storage.Region, storage.Endpoint, objectKey),
		Status:        model.SeedanceAssetCleanupStatusPending,
		NextAttemptAt: common.GetTimestamp(),
	}
	if err := model.CreateSeedanceAssetCleanupJob(job); err != nil {
		return err
	}
	enqueueSeedanceAssetCleanupTask()
	return nil
}

// enqueueSeedanceAssetCleanupTask 只负责唤醒去重的系统任务，调度器仍会兜底扫描未处理记录。
func enqueueSeedanceAssetCleanupTask() {
	if _, _, err := EnqueueSystemTask(model.SystemTaskTypeSeedanceAssetCleanup, nil); err != nil {
		common.SysError("failed to enqueue Seedance asset cleanup: " + err.Error())
	}
}

// EnqueueSeedanceAssetCleanup 唤醒清理 runner；即使唤醒失败，周期调度也会扫描待处理记录。
func EnqueueSeedanceAssetCleanup() {
	enqueueSeedanceAssetCleanupTask()
}

// seedanceAssetCleanupDedupKey 对清理目标做稳定摘要，避免把对象路径和凭证摘要暴露在唯一键之外。
func seedanceAssetCleanupDedupKey(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// RunSeedanceAssetCleanupOnce 并发处理到期的清理任务，同时用数据库租约保证多节点不重复执行。
func RunSeedanceAssetCleanupOnce(ctx context.Context, report func(processed, total int)) (SeedanceAssetCleanupSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SeedanceAssetCleanupSummary{}, err
	}
	if model.DB == nil {
		return SeedanceAssetCleanupSummary{}, errors.New("database is not initialized")
	}
	jobs, err := model.ListDueSeedanceAssetCleanupJobs(common.GetTimestamp(), seedanceAssetCleanupBatchSize)
	if err != nil {
		return SeedanceAssetCleanupSummary{}, err
	}
	summary := SeedanceAssetCleanupSummary{Candidates: len(jobs)}
	if report != nil {
		report(0, len(jobs))
	}
	if len(jobs) == 0 {
		return summary, nil
	}

	jobsCh := make(chan *model.SeedanceAssetCleanupJob, len(jobs))
	results := make(chan seedanceAssetCleanupResult, len(jobs))
	for _, job := range jobs {
		jobsCh <- job
	}
	close(jobsCh)
	workerSize := min(seedanceAssetCleanupWorkerSize, len(jobs))
	var workers sync.WaitGroup
	workers.Add(workerSize)
	for worker := 0; worker < workerSize; worker++ {
		go func() {
			defer workers.Done()
			for job := range jobsCh {
				result := seedanceAssetCleanupResult{skipped: true}
				if ctx.Err() == nil {
					result = processSeedanceAssetCleanupJob(ctx, job)
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
		if result.completed {
			summary.Completed++
		}
		if result.retryScheduled {
			summary.RetryScheduled++
		}
		if result.skipped {
			summary.Skipped++
		}
		if result.err != nil {
			summary.Errors++
			logger.LogWarn(ctx, fmt.Sprintf("Seedance asset cleanup failed: %v", result.err))
		}
		if report != nil {
			report(processed, len(jobs))
		}
	}
	return summary, ctx.Err()
}

// processSeedanceAssetCleanupJob 先完成外部资源清理，再删除本地删除任务对应的素材映射。
func processSeedanceAssetCleanupJob(ctx context.Context, job *model.SeedanceAssetCleanupJob) seedanceAssetCleanupResult {
	now := common.GetTimestamp()
	leaseUntil := now + int64(seedanceAssetCleanupLease/time.Second)
	claimed, err := model.ClaimSeedanceAssetCleanupJob(job.ID, job.UpdatedAt, now, leaseUntil)
	if err != nil {
		return seedanceAssetCleanupResult{err: err}
	}
	if !claimed {
		return seedanceAssetCleanupResult{skipped: true}
	}
	result := seedanceAssetCleanupResult{claimed: true}
	job.LeaseUntil = leaseUntil
	if err := executeSeedanceAssetCleanup(ctx, job); err != nil {
		if ctx.Err() != nil {
			result.err = ctx.Err()
			return result
		}
		return scheduleSeedanceAssetCleanupRetry(result, job, leaseUntil, err)
	}
	completed, err := model.CompleteSeedanceAssetCleanupJob(job.ID, leaseUntil)
	if err != nil {
		result.err = err
		return result
	}
	if !completed {
		result.skipped = true
		return result
	}
	result.completed = true
	return result
}

// executeSeedanceAssetCleanup 执行单个清理目标；每次外部操作都有独立的有界超时。
func executeSeedanceAssetCleanup(ctx context.Context, job *model.SeedanceAssetCleanupJob) error {
	if job == nil {
		return errors.New("Seedance asset cleanup job is nil")
	}
	operationCtx, cancel := context.WithTimeout(ctx, seedanceAssetCleanupOperationTimeout)
	defer cancel()

	switch job.Kind {
	case model.SeedanceAssetCleanupKindUpstreamAsset, model.SeedanceAssetCleanupKindUpstreamGroup, model.SeedanceAssetCleanupKindAssetDelete:
		channel, err := model.GetChannelById(job.ChannelID, true)
		if err != nil {
			return err
		}
		client, err := NewSeedanceAssetClient(channel, job.KeyFingerprint)
		if err != nil {
			return err
		}
		if strings.TrimSpace(job.UpstreamID) != "" && (job.Kind != model.SeedanceAssetCleanupKindAssetDelete || job.UpstreamDoneAt == 0) {
			switch job.Kind {
			case model.SeedanceAssetCleanupKindUpstreamGroup:
				if err := client.DeleteSeedanceAssetGroup(operationCtx, job.UpstreamID); err != nil {
					return err
				}
			default:
				if err := client.DeleteSeedanceAsset(operationCtx, job.UpstreamID); err != nil {
					return err
				}
			}
			if job.Kind == model.SeedanceAssetCleanupKindAssetDelete {
				updated, err := model.MarkSeedanceAssetCleanupUpstreamDone(job.ID, job.LeaseUntil, common.GetTimestamp())
				if err != nil {
					return err
				}
				if !updated {
					return errors.New("Seedance asset cleanup lease lost")
				}
			}
		}
		if job.Kind == model.SeedanceAssetCleanupKindAssetDelete && strings.TrimSpace(job.ObjectKey) != "" {
			if err := RemoveSeedanceAssetObject(operationCtx, job.ObjectKey, job.Storage); err != nil {
				return err
			}
		}
		if job.Kind == model.SeedanceAssetCleanupKindAssetDelete && job.LocalAssetID > 0 {
			result := model.DB.WithContext(operationCtx).
				Where("id = ? AND user_id = ? AND status = ?", job.LocalAssetID, job.UserID, "Deleting").
				Delete(&model.SeedanceAsset{})
			if result.Error != nil {
				return result.Error
			}
		}
		return nil
	case model.SeedanceAssetCleanupKindOSSObject:
		return RemoveSeedanceAssetObject(operationCtx, job.ObjectKey, job.Storage)
	default:
		return fmt.Errorf("unsupported Seedance asset cleanup kind: %s", job.Kind)
	}
}

// scheduleSeedanceAssetCleanupRetry 使用有上限的指数退避，避免故障时持续打满上游和 OSS。
func scheduleSeedanceAssetCleanupRetry(result seedanceAssetCleanupResult, job *model.SeedanceAssetCleanupJob, leaseUntil int64, cleanupErr error) seedanceAssetCleanupResult {
	attempts := min(max(job.Attempts+1, 1), seedanceAssetCleanupMaxBackoffAttempts)
	delay := seedanceAssetCleanupBaseDelay
	for step := 1; step < attempts && delay < seedanceAssetCleanupMaxDelay; step++ {
		if delay > seedanceAssetCleanupMaxDelay/2 {
			delay = seedanceAssetCleanupMaxDelay
			break
		}
		delay *= 2
	}
	nextAttemptAt := common.GetTimestamp() + int64(delay/time.Second)
	updated, err := model.RescheduleSeedanceAssetCleanupJob(
		job.ID,
		leaseUntil,
		attempts,
		nextAttemptAt,
		truncateSeedanceAssetCleanupError(cleanupErr),
	)
	if err != nil {
		result.err = fmt.Errorf("%v; retry schedule update failed: %w", cleanupErr, err)
		return result
	}
	if !updated {
		result.skipped = true
		return result
	}
	result.retryScheduled = true
	result.err = cleanupErr
	return result
}

// truncateSeedanceAssetCleanupError 防止上游异常响应无限膨胀清理任务记录。
func truncateSeedanceAssetCleanupError(err error) string {
	if err == nil {
		return ""
	}
	message := []rune(err.Error())
	if len(message) > 4096 {
		message = message[:4096]
	}
	return string(message)
}
