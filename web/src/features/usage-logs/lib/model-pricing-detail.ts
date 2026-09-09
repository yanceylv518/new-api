/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { formatBillingCurrencyFromUSD } from '@/lib/currency'

import type { LogOtherData } from '../types'
import {
  getTieredBillingSummary,
  getUserModelDiscountFactor,
  hasAnyCacheTokens,
} from './format'
import { isPerCallBilling } from './utils'

export interface ModelPricingDetailSegment {
  text: string
  muted?: boolean
}

/** 只接受完整且自洽的账务快照，历史记录及不合法数据不显示推测金额。 */
export function getRecordedDiscountAmounts(other: LogOtherData | null) {
  const before = other?.quota_before_discount
  const after = other?.quota_after_discount
  const savings = other?.discount_quota
  if (
    typeof before !== 'number' ||
    !Number.isSafeInteger(before) ||
    typeof after !== 'number' ||
    !Number.isSafeInteger(after) ||
    typeof savings !== 'number' ||
    !Number.isSafeInteger(savings) ||
    before < 0 ||
    after < 0 ||
    savings < 0 ||
    before - after !== savings
  ) {
    return null
  }
  return { before, after, savings }
}

function formatPercentCompact(percent: number): string {
  return percent.toFixed(2).replace(/\.?0+$/, '')
}

/** 根据日志中的计费快照构建详情列的纯文本价格摘要。 */
export function buildModelPricingDetailSegments(
  other: LogOtherData,
  t: (key: string, opts?: Record<string, unknown>) => string
): ModelPricingDetailSegment[] {
  const segments: ModelPricingDetailSegment[] = []
  const priceOpts = { digitsLarge: 4, digitsSmall: 6, abbreviate: false }
  const formatPrice = (price: number) =>
    `${formatBillingCurrencyFromUSD(price, priceOpts)}/M`
  const formatPriceCompact = (price: number) =>
    formatBillingCurrencyFromUSD(price, priceOpts)
  const formatPriceList = (prices: string[], showUnit: boolean) => {
    const text = prices.join(' / ')
    return showUnit ? `${text}/M` : text
  }

  // 折扣必须使用日志快照，避免管理员后续改价影响历史用量记录。
  const discountFactor = getUserModelDiscountFactor(other)
  const hasDiscount = discountFactor < 1
  const discountLabel = hasDiscount
    ? t('Discount {{percent}}%', {
        percent: formatPercentCompact(discountFactor * 100),
      })
    : null
  const withDiscountLabel = (label: string) =>
    discountLabel ? `${label} · ${discountLabel}` : label

  const isTieredExpr = other.billing_mode === 'tiered_expr'
  const tieredSummary = getTieredBillingSummary(other)
  if (isTieredExpr) {
    if (tieredSummary) {
      const effectiveEntries = tieredSummary.priceEntries
        .filter((entry) => ['inputPrice', 'outputPrice'].includes(entry.field))
        .map((entry) => formatPriceCompact(entry.price * discountFactor))
      if (effectiveEntries.length > 0) {
        const tierLabel = tieredSummary.tier.label || t('Default')
        segments.push({
          text: `${withDiscountLabel(tierLabel)} · ${formatPriceList(effectiveEntries, true)}`,
        })
      }

      const effectiveCacheEntries = tieredSummary.priceEntries
        .filter((entry) =>
          ['cacheReadPrice', 'cacheCreatePrice', 'cacheCreate1hPrice'].includes(
            entry.field
          )
        )
        .map((entry) => formatPriceCompact(entry.price * discountFactor))
      if (effectiveCacheEntries.length > 0) {
        segments.push({
          text: `${t('Cache')} ${formatPriceList(effectiveCacheEntries, false)}`,
          muted: true,
        })
      }

      const effectiveOtherEntries = tieredSummary.priceEntries
        .filter(
          (entry) =>
            ![
              'inputPrice',
              'outputPrice',
              'cacheReadPrice',
              'cacheCreatePrice',
              'cacheCreate1hPrice',
            ].includes(entry.field)
        )
        .map(
          (entry) =>
            `${t(entry.shortLabel)} ${formatPrice(entry.price * discountFactor)}`
        )
      if (effectiveOtherEntries.length > 0) {
        segments.push({
          text: effectiveOtherEntries.join(' · '),
          muted: true,
        })
      }
    } else {
      segments.push({
        text: `${t('Dynamic Pricing')} · ${t('No matching results')}`,
        muted: true,
      })
    }
    // 动态价格无法解析或没有主价格时，仍要显示本次请求的折扣快照。
    if (
      discountLabel &&
      !segments.some((segment) => segment.text.includes(discountLabel))
    ) {
      segments.unshift({ text: discountLabel })
    }
    return segments
  }

  const modelPrice = other.model_price
  if (isPerCallBilling(modelPrice) && modelPrice != null) {
    segments.push({
      text: `${withDiscountLabel(t('Per-call'))} · ${formatBillingCurrencyFromUSD(modelPrice * discountFactor, priceOpts)}`,
    })
    return segments
  }

  if (other.model_ratio != null) {
    const inputPriceUSD = other.model_ratio * 2.0
    const baseEntries = [formatPriceCompact(inputPriceUSD * discountFactor)]
    if (other.completion_ratio != null) {
      baseEntries.push(
        formatPriceCompact(
          inputPriceUSD * other.completion_ratio * discountFactor
        )
      )
    }
    segments.push({
      text: `${discountLabel ?? t('Standard')} · ${formatPriceList(baseEntries, true)}`,
    })

    if (hasAnyCacheTokens(other)) {
      const cacheEntries = [
        other.cache_ratio != null && other.cache_ratio !== 1
          ? formatPriceCompact(
              inputPriceUSD * other.cache_ratio * discountFactor
            )
          : null,
        other.cache_creation_ratio != null && other.cache_creation_ratio !== 1
          ? formatPriceCompact(
              inputPriceUSD * other.cache_creation_ratio * discountFactor
            )
          : null,
        other.cache_creation_ratio_1h != null &&
        other.cache_creation_ratio_1h !== 0
          ? formatPriceCompact(
              inputPriceUSD * other.cache_creation_ratio_1h * discountFactor
            )
          : null,
      ].filter(Boolean) as string[]

      if (cacheEntries.length > 0) {
        segments.push({
          text: `${t('Cache')} ${formatPriceList(cacheEntries, false)}`,
          muted: true,
        })
      }
    }
    return segments
  }

  const userGroupRatio = other.user_group_ratio
  const groupRatio = other.group_ratio
  const isUserGroup =
    userGroupRatio != null &&
    Number.isFinite(userGroupRatio) &&
    userGroupRatio !== -1
  const effectiveRatio = isUserGroup ? userGroupRatio : groupRatio
  const ratioLabel = isUserGroup ? t('User Exclusive Ratio') : t('Group Ratio')

  if (effectiveRatio != null && Number.isFinite(effectiveRatio)) {
    const compactRatio =
      effectiveRatio % 1 === 0
        ? String(effectiveRatio)
        : effectiveRatio.toFixed(4).replace(/\.?0+$/, '')
    segments.push({ text: `${ratioLabel} ${compactRatio}x` })
  }
  // 即使日志缺少模型价格，也要保留折扣快照，避免详情列丢失计费比例。
  if (discountLabel) {
    segments.unshift({ text: discountLabel })
  }
  return segments
}
