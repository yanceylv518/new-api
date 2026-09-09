package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// LogTaskConsumption 记录任务消费日志和统计信息（仅记录，不涉及实际扣费）。
// 实际扣费已由 BillingSession（PreConsumeBilling + SettleBilling）完成。
func LogTaskConsumption(c *gin.Context, info *relaycommon.RelayInfo, task *model.Task) {
	tokenName := c.GetString("token_name")
	logContent := fmt.Sprintf("操作 %s", info.Action)
	// 支持任务仅按次计费
	if common.StringsContains(constant.TaskPricePatches, info.OriginModelName) {
		logContent = fmt.Sprintf("%s，按次计费", logContent)
	} else {
		var contents []string
		if otherRatios := info.PriceData.OtherRatios(); len(otherRatios) > 0 {
			for key, ra := range otherRatios {
				if 1.0 != ra {
					contents = append(contents, fmt.Sprintf("%s: %.2f", key, ra))
				}
			}
		}
		if snap := info.TieredBillingSnapshot; snap != nil {
			for key, value := range snap.UsageFacts {
				contents = append(contents, fmt.Sprintf("%s: %v", key, value))
			}
		}
		if len(contents) > 0 {
			logContent = fmt.Sprintf("%s, 计算参数：%s", logContent, strings.Join(contents, ", "))
		}
	}
	other := model.NewLogOther()
	other.SetPublic("is_task", true)
	other.SetPublic("request_path", c.Request.URL.Path)
	other.SetPublic("model_price", info.PriceData.ModelPrice)
	if info.PriceData.ModelRatio > 0 {
		other.SetPublic("model_ratio", info.PriceData.ModelRatio)
	}
	other.SetPublic("group_ratio", info.PriceData.GroupRatioInfo.GroupRatio)
	other.SetPublic(types.UserModelDiscountRatioKey, info.PriceData.UserModelDiscountMultiplier())
	info.PriceData.DiscountAmounts.AddToLog(other)
	if info.PriceData.DiscountAmounts != nil {
		other.SetPublic("discount_cost_scope", "task_total")
	}
	if info.PriceData.GroupRatioInfo.HasSpecialRatio {
		other.SetPublic("user_group_ratio", info.PriceData.GroupRatioInfo.GroupSpecialRatio)
	}
	if info.IsModelMapped {
		other.SetPublic("is_model_mapped", true)
		other.SetPublic("upstream_model_name", info.UpstreamModelName)
	}
	if snap := info.TieredBillingSnapshot; snap != nil {
		other.SetPublic("billing_mode", "tiered_expr")
		other.SetPublic("expr_b64", base64.StdEncoding.EncodeToString([]byte(snap.ExprString)))
		other.SetPublic("matched_tier", snap.EstimatedTier)
		if len(snap.UsageFacts) > 0 {
			other.SetPublic("usage_facts", snap.UsageFacts)
		}
	}
	appendTaskLogInfo(task, other)
	attachQuotaSaturation(c, info, other)
	model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{
		ChannelId: info.ChannelId,
		ModelName: info.OriginModelName,
		TokenName: tokenName,
		Quota:     info.PriceData.Quota,
		Content:   logContent,
		TokenId:   info.TokenId,
		Group:     info.UsingGroup,
		Other:     other,
	})
	model.UpdateUserUsedQuotaAndRequestCount(info.UserId, info.PriceData.Quota)
	model.UpdateChannelUsedQuota(info.ChannelId, info.PriceData.DiscountAmounts.ChannelQuota(info.PriceData.Quota))
}

// ---------------------------------------------------------------------------
// 异步任务计费辅助函数
// ---------------------------------------------------------------------------

// resolveTokenKey 通过 TokenId 运行时获取令牌 Key（用于 Redis 缓存操作）。
// 如果令牌已被删除或查询失败，返回空字符串。
func resolveTokenKey(ctx context.Context, tokenId int, taskID string) string {
	token, err := model.GetTokenById(tokenId)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("获取令牌 key 失败 (tokenId=%d, task=%s): %s", tokenId, taskID, err.Error()))
		return ""
	}
	return token.Key
}

// taskBillingOther 从 task 的 BillingContext 构建日志 Other 字段。
func taskBillingOther(task *model.Task) *model.LogOther {
	other := model.NewLogOther()
	if bc := task.PrivateData.BillingContext; bc != nil {
		other.SetPublic("model_price", bc.ModelPrice)
		if bc.ModelRatio > 0 {
			other.SetPublic("model_ratio", bc.ModelRatio)
		}
		other.SetPublic("group_ratio", bc.GroupRatio)
		if priceData := taskBillingContextPriceData(bc); priceData != nil {
			for k, v := range priceData.OtherRatios() {
				if !other.SetPublic(k, v) {
					common.SysError("task billing other ratio key rejected: " + k)
				}
			}
		}
		if snap := bc.TieredSnapshot; snap != nil {
			other.SetPublic("billing_mode", "tiered_expr")
			other.SetPublic("expr_b64", base64.StdEncoding.EncodeToString([]byte(snap.ExprString)))
			other.SetPublic("matched_tier", snap.EstimatedTier)
			if len(snap.UsageFacts) > 0 {
				other.SetPublic("usage_facts", snap.UsageFacts)
			}
		}
	}
	props := task.Properties
	if props.UpstreamModelName != "" && props.UpstreamModelName != props.OriginModelName {
		other.SetPublic("is_model_mapped", true)
		other.SetPublic("upstream_model_name", props.UpstreamModelName)
	}
	appendTaskLogInfo(task, other)
	return other
}

func appendTaskLogInfo(task *model.Task, other *model.LogOther) {
	if task == nil || other == nil {
		return
	}
	if task.TaskID != "" {
		other.SetPublic("task_id", task.TaskID)
	}
	if task.PrivateData.Execution != nil {
		AppendTaskPluginAuditInfo(other, task.PrivateData.Execution.TaskPlugin)
	}
	if task.PrivateData.UpstreamTaskID == "" && task.PrivateData.NodeName == "" {
		return
	}
	if task.PrivateData.UpstreamTaskID != "" {
		other.SetRoot("upstream_task_id", task.PrivateData.UpstreamTaskID)
	}
	if task.PrivateData.NodeName != "" {
		other.SetRoot("node_name", task.PrivateData.NodeName)
	}
}

func taskBillingContextPriceData(bc *model.TaskBillingContext) *types.PriceData {
	if bc == nil || len(bc.OtherRatios) == 0 {
		return nil
	}
	priceData := &types.PriceData{}
	if !priceData.ReplaceOtherRatios(bc.OtherRatios) {
		return nil
	}
	return priceData
}

// taskModelName 从 BillingContext 或 Properties 中获取模型名称。
func taskModelName(task *model.Task) string {
	if bc := task.PrivateData.BillingContext; bc != nil && bc.OriginModelName != "" {
		return bc.OriginModelName
	}
	return task.Properties.OriginModelName
}

// RefundTaskQuota 统一的任务失败退款逻辑。
// 当异步任务失败时，退还资金与令牌额度，并回减用户和渠道用量。
// 返回资金来源是否已成功退还；失败时保留 quota，供显式重试或人工对账。
func RefundTaskQuota(ctx context.Context, task *model.Task, reason string) bool {
	quota := task.Quota
	if quota == 0 {
		return true
	}

	// 资金、令牌、渠道及任务金额在同一主库事务内退款，重复请求由持久化任务金额去重。
	var amounts *types.DiscountAmounts
	if task.PrivateData.DiscountAmounts != nil {
		amounts = types.NewDiscountAmounts(0, 0)
	}
	delta, err := model.CommitTaskSettlement(ctx, task, 0, amounts)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("退还资金来源失败 task %s: %s", task.TaskID, err.Error()))
		return false
	}
	if delta == 0 {
		return true
	}
	quota = -delta

	// 4. 记录日志
	other := taskBillingOther(task)
	other.SetPublic("task_id", task.TaskID)
	other.SetPublic("reason", reason)
	if task.PrivateData.DiscountAmounts != nil {
		types.NewDiscountAmounts(0, 0).AddToLog(other)
		other.SetPublic("discount_cost_scope", "task_total")
	}
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   model.LogTypeRefund,
		Content:   "",
		ChannelId: task.ChannelId,
		ModelName: taskModelName(task),
		Quota:     quota,
		TokenId:   task.PrivateData.TokenId,
		Group:     task.Group,
		Other:     other,
	})

	return true
}

// RecalculateTaskQuota 通用的异步差额结算。
// actualQuota 是任务完成后的实际应扣额度，与预扣额度 (task.Quota) 做差额结算。
// reason 用于日志记录（例如 "token重算" 或 "adaptor调整"）。
// clamps 可选：若计算 actualQuota 时发生额度饱和，将其记入日志 admin_info（仅管理员可见）。
func RecalculateTaskQuota(ctx context.Context, task *model.Task, actualQuota int, reason string, clamps ...*common.QuotaClamp) {
	// 兼容没有折扣的旧调用方；有折扣却没有原始量时不能反推或保留过期金额。
	var amounts *types.DiscountAmounts
	priceData := taskBillingContextPriceData(task.PrivateData.BillingContext)
	if priceData == nil || priceData.UserModelDiscountMultiplier() == 1 {
		amounts = types.NewDiscountAmounts(actualQuota, actualQuota)
	}
	RecalculateTaskQuotaWithAmounts(ctx, task, actualQuota, amounts, reason, clamps...)
}

// RecalculateTaskQuotaWithAmounts 在资金调整成功后保存本次实际费用快照。
// 日志中的三项金额是任务总额，原 quota 字段仍表示本次补扣或退款差额。
func RecalculateTaskQuotaWithAmounts(ctx context.Context, task *model.Task, actualQuota int, amounts *types.DiscountAmounts, reason string, clamps ...*common.QuotaClamp) {
	settleTaskQuotaWithSnapshot(ctx, task, actualQuota, amounts, nil, reason, clamps...)
}

// settleTaskQuotaWithSnapshot 在同一结算中保存可选的最终用量；只有事务确实更新时才记日志。
func settleTaskQuotaWithSnapshot(ctx context.Context, task *model.Task, actualQuota int, amounts *types.DiscountAmounts, snapshot *billingexpr.BillingSnapshot, reason string, clamps ...*common.QuotaClamp) {
	if actualQuota < 0 {
		return
	}
	// 不允许不匹配的 JSON 金额快照进入资金调整，避免实扣、渠道与日志各记不同金额。
	if amounts != nil && !amounts.ValidFor(actualQuota) {
		logger.LogError(ctx, fmt.Sprintf("任务金额快照与实际扣款不一致 task %s", task.TaskID))
		return
	}
	result, err := model.CommitTaskSettlementWithSnapshot(ctx, task, actualQuota, amounts, snapshot)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("任务原子结算失败 task %s: %s", task.TaskID, err.Error()))
		return
	}
	if !result.Updated {
		return
	}
	quotaDelta := result.QuotaDelta
	preConsumedQuota := actualQuota - quotaDelta

	logger.LogInfo(ctx, fmt.Sprintf("任务 %s 差额结算：delta=%s（实际：%s，预扣：%s，%s）",
		task.TaskID,
		logger.LogQuota(quotaDelta),
		logger.LogQuota(actualQuota),
		logger.LogQuota(preConsumedQuota),
		reason,
	))

	var logType int
	var logQuota int
	if quotaDelta >= 0 {
		logType = model.LogTypeConsume
		logQuota = quotaDelta
	} else {
		logType = model.LogTypeRefund
		logQuota = -quotaDelta
	}
	other := taskBillingOther(task)
	other.SetPublic("task_id", task.TaskID)
	other.SetPublic("pre_consumed_quota", preConsumedQuota)
	other.SetPublic("actual_quota", actualQuota)
	if amounts != nil {
		amounts.AddToLog(other)
		other.SetPublic("discount_cost_scope", "task_total")
	}
	for _, clamp := range clamps {
		attachQuotaSaturationToOther(other, clamp)
	}
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   logType,
		Content:   reason,
		ChannelId: task.ChannelId,
		ModelName: taskModelName(task),
		Quota:     logQuota,
		TokenId:   task.PrivateData.TokenId,
		Group:     task.Group,
		Other:     other,
		NodeName:  task.PrivateData.NodeName,
		// 零差额只补齐最终费用或用量，不能再次计入导出的请求次数。
		MetadataOnly: quotaDelta == 0,
	})
}

// RecalculateTaskQuotaByTokens 根据实际 token 消耗重新计费（异步差额结算）。
// 当任务成功且返回了 totalTokens 时，根据模型倍率和分组倍率重新计算实际扣费额度，
// 与预扣费的差额进行补扣或退还。支持钱包和订阅计费来源。
func RecalculateTaskQuotaByTokens(ctx context.Context, task *model.Task, totalTokens int) bool {
	if totalTokens <= 0 {
		return false
	}

	modelName := taskModelName(task)

	var finalGroupRatio float64
	var modelRatio float64
	if billingContext := task.PrivateData.BillingContext; billingContext != nil && billingContext.OriginModelName != "" && billingContext.OtherRatios[types.UserModelDiscountRatioKey] > 0 {
		// 新任务必须使用提交时冻结的倍率，管理员后续修改配置不应改变已提交任务的账单。
		modelRatio = billingContext.ModelRatio
		finalGroupRatio = billingContext.GroupRatio
	} else {
		// 旧版 BillingContext 不保证保存 ModelRatio，只有显式折扣标记才证明存在完整新快照。
		var hasRatioSetting bool
		modelRatio, hasRatioSetting, _ = ratio_setting.GetModelRatio(modelName)
		if !hasRatioSetting || modelRatio <= 0 {
			return false
		}

		usingGroup := task.Group
		if usingGroup == "" {
			if user, err := model.GetUserById(task.UserId, false); err == nil {
				usingGroup = user.Group
			}
		}
		if usingGroup == "" {
			return false
		}

		finalGroupRatio = ratio_setting.GetGroupRatio(usingGroup)
		// 旧任务缺少提交时的用户组快照，保留 rc35 既有回退，不推测历史组关系。
		if userGroupRatio, ok := ratio_setting.GetGroupGroupRatio(usingGroup, usingGroup); ok {
			finalGroupRatio = userGroupRatio
		}
	}
	if modelRatio < 0 {
		return false
	}

	// 计算 OtherRatios 乘积（视频折扣、时长等）
	otherMultiplier := 1.0
	beforeMultiplier := 1.0
	priceData := taskBillingContextPriceData(task.PrivateData.BillingContext)
	if priceData != nil {
		otherMultiplier = priceData.OtherRatioMultiplier()
		beforeMultiplier = priceData.OtherRatioMultiplierBeforeDiscount()
	}

	// 计算实际应扣费额度: totalTokens * modelRatio * groupRatio * otherMultiplier（饱和转换，防止溢出成负数）
	actualQuotaValue := float64(totalTokens) * modelRatio * finalGroupRatio * otherMultiplier
	actualQuota, clamp := common.QuotaFromFloatChecked(actualQuotaValue)

	reason := fmt.Sprintf("token重算：tokens=%d, modelRatio=%.2f, groupRatio=%.2f, otherMultiplier=%.4f", totalTokens, modelRatio, finalGroupRatio, otherMultiplier)
	originalValue := float64(totalTokens) * modelRatio * finalGroupRatio * beforeMultiplier
	// 折前沿用任务既有计价；只将用户折扣改为十进制乘法，避免 200*0.29 截断成 57。
	// 先截断再走统一饱和转换器，保留任务的截断契约及非有限输入的审计行为。
	if priceData != nil && priceData.UserModelDiscountMultiplier() != 1 {
		actualQuota, clamp = common.QuotaDiscountChecked(originalValue, priceData.UserModelDiscountMultiplier(), true)
	}
	originalQuota, originalClamp := common.QuotaFromFloatChecked(originalValue)
	if originalQuota > 0 && actualQuota == 0 && otherMultiplier != beforeMultiplier {
		actualQuota = 1
	}
	if clamp == nil {
		clamp = originalClamp
	}
	RecalculateTaskQuotaWithAmounts(ctx, task, actualQuota, types.NewDiscountAmounts(originalQuota, actualQuota), reason, clamp)
	return true
}
