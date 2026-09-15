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
import { expect, it } from 'vitest'

import { parseTaskTiersFromExpr } from '../lib/billing-expr'
import { evaluateBillingExpression } from '../lib/billing-expression/runtime'
import {
  evaluateTaskVisualConfig,
  generateTaskExprFromConfig,
  taskMatrixToTiers,
  tryParseTaskMatrixConfig,
} from '../lib/task-expr'
import { getTaskPricingDisplayTiers } from '../lib/task-matrix-display'
import type { BillingUsageSchema } from '../types'

const schema: BillingUsageSchema = {
  operation: { enum: ['generation', 'regeneration'] },
  input_images: { type: 'number', unit: 'count' },
}

// 不同操作的额度即使单价相同也必须分别保存，编辑往返、预览和表达式计算保持一致。
it('preserves independent free quantities through matrix editing and public pricing', () => {
  const expression =
    'u("operation") == "generation" ? tier("gen", max(u("input_images") - 5, 0) * 0.2) : tier("regen", max(u("input_images") - 7, 0) * 0.2)'
  const matrix = tryParseTaskMatrixConfig(expression, schema)
  expect(matrix).not.toBeNull()
  if (!matrix) return
  const config = { tiers: taskMatrixToTiers(matrix, schema) }
  expect(config.tiers).toHaveLength(2)
  const generated = generateTaskExprFromConfig(config, schema)
  expect(tryParseTaskMatrixConfig(generated, schema)?.rows).toEqual(matrix.rows)
  expect(
    getTaskPricingDisplayTiers(generated, schema).map(
      (tier) => tier.freeAllowances?.input_images
    )
  ).toEqual([5, 7])
  for (const [operation, quantity, amount] of [
    ['generation', 0, 0],
    ['generation', 5, 0],
    ['generation', 6, 0.2],
    ['generation', 9, 0.8],
    ['regeneration', 6, 0],
    ['regeneration', 7, 0],
    ['regeneration', 9, 0.4],
  ] as const) {
    const sample = { operation, input_images: quantity }
    expect(evaluateTaskVisualConfig(config, sample, schema)?.total).toBeCloseTo(
      amount
    )
    expect(
      evaluateBillingExpression(generated, { usage: sample })
    ).toMatchObject({ status: 'success', cost: amount })
  }
})

// 零单价时设置的免费数量也要保存，后续改单价不能丢失额度；零额度恢复全部计费。
it('retains free quantities at zero price and accepts an administrator choosing zero', () => {
  const matrix = tryParseTaskMatrixConfig(
    'tier("free", max(u("input_images") - 5, 0) * 0)',
    schema
  )
  expect(matrix).not.toBeNull()
  if (!matrix) return
  matrix.rows.forEach((row) => {
    row.unitPrices.input_images = 0.2
    row.freeAllowances = { input_images: 0 }
  })
  const expression = generateTaskExprFromConfig(
    { tiers: taskMatrixToTiers(matrix, schema) },
    schema
  )
  expect(expression).not.toContain('max(')
  expect(
    evaluateBillingExpression(expression, {
      usage: { operation: 'generation', input_images: 5 },
    })
  ).toMatchObject({ status: 'success', cost: 1 })
})

// 不支持的减免形式留在原始编辑器，不能静默转成不同费用。
it.each([
  'max(u("input_images") - -1, 0)',
  'max(u("input_images") - 1.5, 0)',
  'max(u("input_images") - 9007199254740992, 0)',
  'max(u("input_images") - 5, 1)',
  'max(u("input_images") - 5, 0, 1)',
  'u("input_images") - 5',
])(
  'keeps invalid free quantity expression %s out of the visual editor',
  (quantity) => {
    expect(
      parseTaskTiersFromExpr(`tier("bad", ${quantity} * 0.2)`, schema)
    ).toEqual([])
  }
)
