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
import {
  discountPercentLiteral,
  generateDiscountExpression,
  inheritPermanentDiscount,
  isTimeDiscountConfig,
  parseDiscountExpression,
} from './user-discount-expression'

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
      count +
      (item.mode === 'layered' || item.mode === 'scheduled'
        ? (item.periods?.length ?? 0)
        : 1),
    0
  )
}

export function userModelPricingFormItem(
  item: UserModelPricingItem
): UserModelPricingFormValues['items'][number] {
  if (item.mode === 'layered') {
    return {
      model_name: item.model_name,
      mode: 'layered',
      discount_percent: item.permanent?.expression
        ? (parseDiscountExpression(item.permanent.expression)?.percent ?? null)
        : (item.permanent?.discount_bps ?? 10000) / 100,
      expression: item.permanent?.expression ?? '',
      periods: (item.periods ?? []).map((period) => ({
        discount_percent: period.discount_bps / 100,
        expression: period.expression ?? '',
        start_time: period.start_time,
        end_time: period.end_time,
        legacy_unbounded: period.legacy_unbounded,
      })),
    }
  }
  const periods =
    item.mode === 'scheduled'
      ? [...(item.periods ?? [])]
      : [
          {
            discount_bps: item.discount_bps,
            start_time: item.start_time,
            end_time: item.end_time,
            expression: item.expression,
          },
        ]
  const last = periods.at(-1)
  const permanent =
    last?.end_time == null &&
    (last?.start_time ?? 0) <= Math.floor(Date.now() / 1000)
      ? periods.pop()
      : undefined
  return {
    model_name: item.model_name,
    mode: 'layered',
    discount_percent: permanent?.expression
      ? (parseDiscountExpression(permanent.expression)?.percent ?? null)
      : (permanent?.discount_bps ?? 10000) / 100,
    expression: permanent?.expression ?? '',
    periods: periods.map((period) => ({
      discount_percent: period.discount_bps / 100,
      expression: period.expression ?? '',
      start_time: period.start_time,
      end_time: period.end_time,
      legacy_unbounded: period.end_time == null,
    })),
  }
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
export function userModelPricingDiscountPercentSchema(t: TFunction) {
  return z
    .number({ error: t('Discount percentage is required') })
    .finite()
    .min(0.01, t('Discount percentage must be greater than 0'))
    .max(100, t('Discount percentage cannot exceed 100'))
    .refine(
      (percent) => discountPercentLiteral(percent) !== null,
      t('Enter a percentage from 0.01 to 100, with up to two decimal places.')
    )
}

export function createUserModelPricingFormSchema(t: TFunction) {
  const timestamp = z
    .number({ error: t('Select a valid date and time') })
    .int()
    .min(0)
    .max(253402300799)
    .nullish()
  const discount = userModelPricingDiscountPercentSchema(t)
  const expression = z
    .string()
    .trim()
    .max(4096, t('Discount expression is too long'))
    .optional()
  const calculation_type = z.enum(['fixed', 'expression']).optional()
  const percentage = z.union([z.number(), z.nan()]).nullish()
  const period = z.object({
    discount_percent: percentage,
    expression,
    calculation_type,
    start_time: timestamp,
    end_time: timestamp,
    legacy_unbounded: z.boolean().optional(),
  })
  return z
    .object({
      items: z.array(
        z.object({
          model_name: z.string().trim().min(1, t('Model is required')).max(128),
          mode: z.enum(['single', 'scheduled', 'layered']).optional(),
          discount_percent: percentage,
          expression,
          calculation_type,
          start_time: timestamp,
          end_time: timestamp,
          periods: z.array(period).max(1000).optional(),
        })
      ),
    })
    .superRefine((value, context) => {
      const seen = new Set<string>()
      let total = 0
      const checkDiscount = (
        rule: {
          discount_percent?: number | null
          expression?: string
          calculation_type?: 'fixed' | 'expression'
        },
        path: (string | number)[]
      ) => {
        if (
          rule.calculation_type === 'expression' &&
          !rule.expression?.trim()
        ) {
          context.addIssue({
            code: 'custom',
            message: t('Discount expression is required'),
            path: [...path, 'expression'],
          })
          return
        }
        if (rule.expression?.trim()) return
        const result = discount.safeParse(rule.discount_percent)
        if (!result.success) {
          context.addIssue({
            code: 'custom',
            message: result.error.issues[0].message,
            path: [...path, 'discount_percent'],
          })
        }
      }
      value.items.forEach((item, index) => {
        const prefix = ['items', index]
        if (item.mode !== 'scheduled') checkDiscount(item, prefix)
        const name = normalizeUserModelPricingModelName(item.model_name)
        if (seen.has(name)) {
          context.addIssue({
            code: 'custom',
            message: t('Duplicate model pricing rule'),
            path: [...prefix, 'model_name'],
          })
        }
        seen.add(name)
        const periods =
          item.mode === 'layered' || item.mode === 'scheduled'
            ? (item.periods ?? [])
            : [item]
        total +=
          item.mode === 'layered' ? periods.length : Math.max(1, periods.length)
        const ordered = periods
          .map((rule, position) => ({
            rule,
            position,
            start: rule.start_time ?? Math.floor(Date.now() / 1000),
          }))
          .sort((a, b) => a.start - b.start)
        ordered.forEach(({ rule, position, start }, order) => {
          const path =
            item.mode === 'single' || !item.mode
              ? prefix
              : [...prefix, 'periods', position]
          checkDiscount(rule, path)
          if (
            item.mode === 'layered' &&
            rule.end_time == null &&
            !('legacy_unbounded' in rule && rule.legacy_unbounded)
          ) {
            context.addIssue({
              code: 'custom',
              message: t('Temporary discounts require an end time'),
              path: [...path, 'end_time'],
            })
          }
          if (rule.end_time != null && rule.end_time <= start) {
            context.addIssue({
              code: 'custom',
              message: t('End time must be after start time'),
              path: [...path, 'end_time'],
            })
          }
          const previous = ordered[order - 1]
          if (
            previous &&
            (previous.rule.end_time == null || previous.rule.end_time > start)
          ) {
            context.addIssue({
              code: 'custom',
              message: t('Time periods must not overlap'),
              path: [...prefix, 'periods', previous.position, 'end_time'],
            })
            context.addIssue({
              code: 'custom',
              message: t('Time periods must not overlap'),
              path: [...path, 'start_time'],
            })
          }
        })
      })
      const configured = value.items.filter(
        (item) =>
          item.expression?.trim() ||
          item.discount_percent !== 100 ||
          (item.periods?.length ?? 0) > 0
      ).length
      if (total > 1000 || configured > 1000) {
        context.addIssue({
          code: 'custom',
          message: t('At most 1000 model pricing rules are allowed'),
          path: ['items'],
        })
      }
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

export function getUserModelPricingFormPeriods(
  item: UserModelPricingFormValues['items'][number]
) {
  if (item.mode === 'layered' || item.mode === 'scheduled') {
    return item.periods ?? []
  }
  return [item]
}

type UserModelPricingFormPayload = Omit<
  UserModelPricingReplacePayload,
  'revision'
>

/** 将规范化模型行转换为后端规则集，原价不会产生专属计费规则。 */
export function buildUserModelPricingPayload(
  values: UserModelPricingFormValues,
  persistedItems: readonly UserModelPricingItem[] = []
): UserModelPricingFormPayload {
  const items = new Map(
    persistedItems
      .filter(
        (item) =>
          item.expression?.trim() ||
          item.permanent?.expression?.trim() ||
          (item.discount_bps > 0 &&
            item.discount_bps < FULL_PRICE_DISCOUNT_BPS) ||
          (item.permanent != null &&
            item.permanent.discount_bps < FULL_PRICE_DISCOUNT_BPS) ||
          (item.periods?.length ?? 0) > 0
      )
      .map((item) => [
        normalizeUserModelPricingModelName(item.model_name),
        item,
      ])
  )
  const now = Math.floor(Date.now() / 1000)
  for (const raw of values.items) {
    const item =
      raw.mode === 'layered'
        ? raw
        : userModelPricingFormItem({
            model_name: raw.model_name,
            mode: raw.mode,
            discount_bps: Math.round((raw.discount_percent ?? 100) * 100),
            start_time: raw.start_time,
            end_time: raw.end_time,
            expression: raw.expression,
            periods: raw.periods?.map((period) => ({
              discount_bps: Math.round((period.discount_percent ?? 100) * 100),
              expression: period.expression,
              start_time: period.start_time,
              end_time: period.end_time,
            })),
          })
    const name = normalizeUserModelPricingModelName(item.model_name)
    const bps = item.expression?.trim()
      ? 10000
      : Math.round((item.discount_percent ?? 100) * 100)
    const periods = (item.periods ?? []).map((period) => {
      let expression = period.expression?.trim() || undefined
      const visual = expression ? parseDiscountExpression(expression) : null
      if (
        !item.expression?.trim() &&
        visual &&
        isTimeDiscountConfig(visual) &&
        (!period.legacy_unbounded || period.end_time != null)
      ) {
        expression =
          generateDiscountExpression(inheritPermanentDiscount(visual)) ??
          expression
      }
      return {
        discount_bps: period.expression?.trim()
          ? 10000
          : Math.round((period.discount_percent ?? 100) * 100),
        expression,
        start_time: period.start_time ?? now,
        end_time: period.end_time ?? null,
        legacy_unbounded: period.legacy_unbounded,
      }
    })
    if (bps === 10000 && !item.expression?.trim() && periods.length === 0) {
      items.delete(name)
      continue
    }
    items.set(name, {
      model_name: name,
      mode: 'layered',
      discount_bps: bps,
      permanent: {
        discount_bps: item.expression?.trim() ? 10000 : bps,
        expression: item.expression?.trim() || undefined,
      },
      periods,
    })
  }
  return { items: [...items.values()] }
}
