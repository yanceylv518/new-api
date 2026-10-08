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
import type { TFunction } from 'i18next'
import { z } from 'zod'

import type {
  UserModelPricingItem,
  UserModelPricingReplacePayload,
} from '../types'

export interface UserModelPricingOption {
  value: string
  label: string
}

export interface UserModelPricingDisplayRow {
  model_name: string
  aliases: string[]
}

// 未配置专属折扣时按原价展示；提交时原价不会写入独立表。
export const FULL_PRICE_DISCOUNT_BPS = 10000
const fullPriceDiscountPercent = FULL_PRICE_DISCOUNT_BPS / 100

export function getUserModelPricingStatusOptions(t: TFunction) {
  return [
    { value: 'all', label: t('All statuses') },
    { value: 'active', label: t('Active') },
    { value: 'pending', label: t('Not started') },
    { value: 'expired', label: t('Expired') },
  ]
}

export function getPricingPeriodStatus(
  period: { start_time?: number | null; end_time?: number | null },
  now: number
): 'pending' | 'active' | 'expired' {
  if (period.end_time != null && now >= period.end_time) return 'expired'
  if (period.start_time != null && now < period.start_time) return 'pending'
  return 'active'
}

export function countUserModelPricingPeriods(
  items: readonly UserModelPricingItem[]
): number {
  return items.reduce(
    (count, item) =>
      count + (item.mode === 'scheduled' ? (item.periods?.length ?? 0) : 1),
    0
  )
}

export function userModelPricingFormItem(
  item: UserModelPricingItem
): UserModelPricingFormValues['items'][number] {
  const common = {
    model_name: item.model_name,
    discount_percent: item.discount_bps / 100,
    start_time: item.start_time,
    end_time: item.end_time,
  }
  return item.mode === 'scheduled'
    ? {
        ...common,
        mode: 'scheduled',
        periods: item.periods?.map((period) => ({
          discount_percent: period.discount_bps / 100,
          start_time: period.start_time,
          end_time: period.end_time,
        })),
      }
    : { ...common, mode: 'single' }
}

/** 与后端计费匹配规则保持一致，避免别名模型被重复定价。 */
export function normalizeUserModelPricingModelName(modelName: string): string {
  const name = modelName.trim()
  if (name.startsWith('gemini-2.5-flash-lite') && name.includes('-thinking-')) {
    return 'gemini-2.5-flash-lite-thinking-*'
  }
  if (name.startsWith('gemini-2.5-flash') && name.includes('-thinking-')) {
    return 'gemini-2.5-flash-thinking-*'
  }
  if (name.startsWith('gemini-2.5-pro') && name.includes('-thinking-')) {
    return 'gemini-2.5-pro-thinking-*'
  }
  if (name.startsWith('gpt-4-gizmo')) return 'gpt-4-gizmo-*'
  if (name.startsWith('gpt-4o-gizmo')) return 'gpt-4o-gizmo-*'
  return name
}

/** 隐藏其他行已定价的模型，同时保留当前行正在编辑的模型。 */
export function getAvailableUserModelPricingOptions(
  options: readonly UserModelPricingOption[],
  selectedModelNames: readonly string[],
  currentModelName: string
): UserModelPricingOption[] {
  const currentName = normalizeUserModelPricingModelName(currentModelName)
  const selectedNames = new Set(
    selectedModelNames.map(normalizeUserModelPricingModelName).filter(Boolean)
  )

  return options.filter(
    (option) =>
      normalizeUserModelPricingModelName(option.value) === currentName ||
      !selectedNames.has(normalizeUserModelPricingModelName(option.value))
  )
}

/** 让前端校验与后端规则数量及折扣范围保持一致。 */
export function createUserModelPricingFormSchema(t: TFunction) {
  const pricingTimestamp = z
    .number({ error: t('Select a valid date and time') })
    .int(t('Select a valid date and time'))
    .min(0, t('Select a valid date and time'))
    .max(253402300799, t('Select a valid date and time'))
    .nullable()
    .optional()
  const discountPercent = z
    .number({ error: t('Discount percentage is required') })
    .finite()
    .min(0.01, t('Discount percentage must be greater than 0'))
  const commonFields = {
    model_name: z
      .string()
      .trim()
      .min(1, t('Model is required'))
      .max(128, t('Model name is too long')),
    start_time: pricingTimestamp,
    end_time: pricingTimestamp,
  }
  return z
    .object({
      items: z.array(
        z.discriminatedUnion('mode', [
          z.object({
            ...commonFields,
            mode: z.literal('single').optional(),
            discount_percent: discountPercent.max(
              fullPriceDiscountPercent,
              t('Discount percentage cannot exceed 100')
            ),
            periods: z.undefined().optional(),
          }),
          z.object({
            ...commonFields,
            mode: z.literal('scheduled'),
            // 多段模式仅使用各段折扣，保留切换前的字段不能阻止排期保存。
            discount_percent: z.union([z.number(), z.nan()]).nullish(),
            periods: z
              .array(
                z.object({
                  discount_percent: discountPercent.max(
                    99.99,
                    t('Scheduled discounts must be below 100%')
                  ),
                  start_time: pricingTimestamp,
                  end_time: pricingTimestamp,
                })
              )
              .max(1000, t('At most 1000 model pricing rules are allowed'))
              .optional(),
          }),
        ])
      ),
    })
    .superRefine((value, context) => {
      const now = Math.floor(Date.now() / 1000)
      // 所有模型都显示有效折扣，原价规则会在构造请求时省略。
      const periodCount = value.items.reduce(
        (count, item) =>
          count +
          (item.mode === 'scheduled'
            ? (item.periods?.length ?? 0)
            : Number(item.discount_percent < fullPriceDiscountPercent)),
        0
      )
      if (periodCount > 1000) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: t('At most 1000 model pricing rules are allowed'),
          path: ['items'],
        })
      }

      // 后端按归一化模型名匹配折扣，任何重复匹配范围都必须拒绝。
      const seen = new Set<string>()
      value.items.forEach((item, index) => {
        if (
          item.mode !== 'scheduled' &&
          item.discount_percent === fullPriceDiscountPercent
        ) {
          return
        }
        const periods =
          item.mode === 'scheduled' ? (item.periods ?? []) : [item]
        if (periods.length === 0) {
          context.addIssue({
            code: 'custom',
            message: t('Add at least one time period'),
            path: ['items', index, 'periods'],
          })
        }
        const sorted = periods
          .map((period, periodIndex) => ({
            ...period,
            periodIndex,
            start: period.start_time ?? now,
          }))
          .sort((a, b) => a.start - b.start)
        sorted.forEach((period, position) => {
          const prefix =
            item.mode === 'scheduled'
              ? ['items', index, 'periods', period.periodIndex]
              : ['items', index]
          if (period.end_time != null && period.end_time <= period.start) {
            context.addIssue({
              code: 'custom',
              message: t('End time must be after start time'),
              path: [...prefix, 'end_time'],
            })
          }
          const previous = sorted[position - 1]
          const previousEnd = previous?.end_time
          if (
            position > 0 &&
            previous &&
            (previousEnd == null || previousEnd > period.start)
          ) {
            const previousPrefix =
              item.mode === 'scheduled'
                ? ['items', index, 'periods', previous.periodIndex]
                : ['items', index]
            context.addIssue({
              code: 'custom',
              message:
                previousEnd == null
                  ? t('Only the last time period can be permanent')
                  : t('Time periods must not overlap'),
              path: [...previousPrefix, 'end_time'],
            })
            if (previousEnd != null) {
              context.addIssue({
                code: 'custom',
                message: t('Time periods must not overlap'),
                path: [...prefix, 'start_time'],
              })
            }
          }
        })
        const modelName = normalizeUserModelPricingModelName(item.model_name)
        if (!seen.has(modelName)) {
          seen.add(modelName)
          return
        }
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: t('Duplicate model pricing rule'),
          path: ['items', index, 'model_name'],
        })
      })
    })
}

/** 将实际模型别名聚合为一个可编辑的规范化定价规则。 */
export function buildUserModelPricingRows(
  models: readonly { model_name: string }[]
): UserModelPricingDisplayRow[] {
  const rows = new Map<string, UserModelPricingDisplayRow>()
  for (const model of models) {
    const modelName = normalizeUserModelPricingModelName(model.model_name)
    const row = rows.get(modelName)
    if (row) {
      if (
        model.model_name !== row.model_name &&
        !row.aliases.includes(model.model_name)
      ) {
        row.aliases.push(model.model_name)
      }
      continue
    }
    rows.set(modelName, {
      model_name: modelName,
      aliases: model.model_name === modelName ? [] : [model.model_name],
    })
  }

  return [...rows.values()]
}

export type UserModelPricingFormValues = z.infer<
  ReturnType<typeof createUserModelPricingFormSchema>
>

type UserModelPricingFormPayload = Omit<
  UserModelPricingReplacePayload,
  'revision'
>

/** 将规范化模型行转换为后端规则集，原价不会产生专属计费规则。 */
export function buildUserModelPricingPayload(
  values: UserModelPricingFormValues,
  persistedItems: readonly UserModelPricingItem[] = []
): UserModelPricingFormPayload {
  const itemsByModel = new Map<string, UserModelPricingItem>()
  const now = Math.floor(Date.now() / 1000)

  // 弹窗仅展示启用模型，完整替换时必须保留不可见模型已有的折扣。
  for (const item of persistedItems) {
    if (
      item.mode !== 'scheduled' &&
      (item.discount_bps <= 0 || item.discount_bps >= FULL_PRICE_DISCOUNT_BPS)
    ) {
      continue
    }
    const modelName = normalizeUserModelPricingModelName(item.model_name)
    itemsByModel.set(modelName, { ...item, model_name: modelName })
  }

  for (const item of values.items) {
    const modelName = normalizeUserModelPricingModelName(item.model_name)
    const discountBPS =
      item.mode === 'scheduled'
        ? Math.round((item.periods?.[0]?.discount_percent ?? 100) * 100)
        : Math.round(item.discount_percent * 100)
    if (item.mode !== 'scheduled' && discountBPS === FULL_PRICE_DISCOUNT_BPS) {
      itemsByModel.delete(modelName)
      continue
    }

    const previous = itemsByModel.get(modelName)
    itemsByModel.set(modelName, {
      model_name: modelName,
      discount_bps: discountBPS,
      mode: item.mode ?? previous?.mode ?? 'single',
      start_time:
        (item.start_time === undefined
          ? previous?.start_time
          : item.start_time) ?? now,
      end_time:
        item.end_time === undefined ? previous?.end_time : item.end_time,
      periods:
        item.mode === 'scheduled'
          ? item.periods?.map((period) => ({
              discount_bps: Math.round(period.discount_percent * 100),
              start_time: period.start_time ?? now,
              end_time: period.end_time,
            }))
          : undefined,
    })
  }

  return { items: [...itemsByModel.values()] }
}
