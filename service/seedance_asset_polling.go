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
	seedanceAssetPollMaxAttempts = 6
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

// RunSeedanceAssetPollingOnce 批量同步到期素材，限制批量规模和并发数以避免放大上游请求。
func RunSeedanceAssetPollingOnce(ctx context.Context, report func(processed, total int)) (SeedanceAssetPollSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if model.DB == nil {
		return SeedanceAssetPollSummary{}, errors.New("database is not initialized")
	}
	assets, err := model.ListDueSeedanceAssets(common.GetTimestamp(), seedanceAssetPollBatchSize)
	if err != nil {
		return SeedanceAssetPollSummary{}, err
	}
	summary := SeedanceAssetPollSummary{Candidates: len(assets)}
	if report != nil {
		report(0, len(assets))
	}
	if len(assets) == 0 {
		return summary, nil
	}

	jobs := make(chan *model.SeedanceAsset, len(assets))
	results := make(chan seedanceAssetPollResult, len(assets))
	for _, asset := range assets {
		jobs <- asset
	}
	close(jobs)

	workerSize := seedanceAssetPollWorkerSize
	if workerSize > len(assets) {
		workerSize = len(assets)
	}
	var workers sync.WaitGroup
	workers.Add(workerSize)
	for worker := 0; worker < workerSize; worker++ {
		go func() {
			defer workers.Done()
			for asset := range jobs {
				result := seedanceAssetPollResult{skipped: true}
				if ctx.Err() == nil {
					result = pollSeedanceAsset(ctx, asset)
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
			report(processed, len(assets))
		}
	}
	return summary, nil
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
	channel, err := model.GetChannelById(asset.ChannelID, true)
	if err != nil {
		return scheduleSeedanceAssetRetry(result, asset, leaseUntil, err)
	}
	status, previewURL, err := NewSeedanceAssetClient(channel).GetSeedanceAsset(ctx, asset.AssetID)
	if err != nil {
		if ctx.Err() != nil {
			return result
		}
		return scheduleSeedanceAssetRetry(result, asset, leaseUntil, err)
	}
	if ctx.Err() != nil {
		return result
	}
	attempts, nextPollAt := SeedanceAssetPollSchedule(status, asset.PollAttempts, common.GetTimestamp())
	updates := map[string]any{
		"status":           status,
		"poll_attempts":    attempts,
		"next_poll_at":     nextPollAt,
		"poll_lease_until": int64(0),
	}
	if previewURL != "" {
		updates["preview_url"] = previewURL
	}
	updated, err := model.UpdateSeedanceAssetPollState(asset.ID, leaseUntil, updates)
	if err != nil {
		return seedanceAssetPollResult{claimed: true, err: err}
	}
	if !updated {
		return seedanceAssetPollResult{claimed: true, skipped: true}
	}
	return seedanceAssetPollResult{claimed: true, updated: true}
}

// scheduleSeedanceAssetRetry 只推进失败素材的退避时间，不伪造审核失败状态。
func scheduleSeedanceAssetRetry(result seedanceAssetPollResult, asset *model.SeedanceAsset, leaseUntil int64, pollErr error) seedanceAssetPollResult {
	attempts, nextPollAt := SeedanceAssetPollSchedule(asset.Status, asset.PollAttempts, common.GetTimestamp())
	updated, err := model.UpdateSeedanceAssetPollState(asset.ID, leaseUntil, map[string]any{
		"poll_attempts":    attempts,
		"next_poll_at":     nextPollAt,
		"poll_lease_until": int64(0),
	})
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
	return strings.ToLower(strings.TrimSpace(status))
}
