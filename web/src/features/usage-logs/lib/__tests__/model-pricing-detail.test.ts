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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { LogOtherData } from '../../types'
import { buildModelPricingDetailSegments } from '../model-pricing-detail'

const t = (key: string, options?: Record<string, unknown>) =>
  key.replace('{{percent}}', String(options?.percent ?? ''))

function primaryDetail(other: LogOtherData): string {
  return buildModelPricingDetailSegments(other, t)[0]?.text ?? ''
}

// 回归保护：详情列展示实际折扣比例和折后价格，同时保持纯文本输出。
describe('common log user-model pricing details', () => {
  test('shows the discount immediately before effective token prices', () => {
    assert.equal(
      primaryDetail({
        model_ratio: 0.07,
        completion_ratio: 2,
        user_model_discount: 0.8,
      }),
      'Discount 80% · $0.112 / $0.224/M'
    )
  })

  test('applies the frozen discount to fixed model prices', () => {
    assert.equal(
      primaryDetail({ model_price: 0.01, user_model_discount: 0.5 }),
      'Per-call · Discount 50% · $0.005'
    )
  })

  test('shows the discount when dynamic pricing has no matching tier', () => {
    assert.deepEqual(
      buildModelPricingDetailSegments(
        { billing_mode: 'tiered_expr', user_model_discount: 0.8 },
        t
      ),
      [
        { text: 'Discount 80%' },
        { text: 'Dynamic Pricing · No matching results', muted: true },
      ]
    )
  })

  test('keeps historical rows unchanged without a discount snapshot', () => {
    assert.equal(
      primaryDetail({ model_ratio: 0.07, completion_ratio: 2 }),
      'Standard · $0.14 / $0.28/M'
    )
  })
})
