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
  historical: boolean
}

// 未配置专属折扣时按原价展示；提交时原价不会写入独立表。
const fullPriceDiscountPercent = 100
const fullPriceDiscountBPS = fullPriceDiscountPercent * 100

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
  return z
    .object({
      items: z.array(
        z.object({
          model_name: z
            .string()
            .trim()
            .min(1, t('Model is required'))
            .max(128, t('Model name is too long')),
          discount_percent: z
            .number({ error: t('Discount percentage is required') })
            .finite()
            .min(0.01, t('Discount percentage must be greater than 0'))
            .max(
              fullPriceDiscountPercent,
              t('Discount percentage cannot exceed 100')
            ),
        })
      ),
    })
    .superRefine((value, context) => {
      // 所有模型都显示有效折扣，原价规则会在构造请求时省略。
      const configuredItems = value.items.filter(
        (item) => item.discount_percent < fullPriceDiscountPercent
      )
      if (configuredItems.length > 1000) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: t('At most 1000 model pricing rules are allowed'),
          path: ['items'],
        })
      }

      // 后端按归一化模型名匹配折扣，任何重复匹配范围都必须拒绝。
      const seen = new Set<string>()
      value.items.forEach((item, index) => {
        if (item.discount_percent === fullPriceDiscountPercent) return
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
  models: readonly { model_name: string }[],
  persistedItems: readonly { model_name: string }[]
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
      historical: false,
    })
  }

  // 目录下线后的规则仍然可见，管理员可以明确清空它，而不会被保存操作静默删除。
  for (const item of persistedItems) {
    const modelName = normalizeUserModelPricingModelName(item.model_name)
    if (rows.has(modelName)) continue
    rows.set(modelName, {
      model_name: modelName,
      aliases: [],
      historical: true,
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
  values: UserModelPricingFormValues
): UserModelPricingFormPayload {
  const itemsByModel = new Map<string, UserModelPricingItem>()

  for (const item of values.items) {
    const modelName = normalizeUserModelPricingModelName(item.model_name)
    const discountBPS = Math.round(item.discount_percent * 100)
    if (discountBPS === fullPriceDiscountBPS) continue

    itemsByModel.set(modelName, {
      model_name: modelName,
      discount_bps: discountBPS,
    })
  }

  return { items: [...itemsByModel.values()] }
}
