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

import {
  getUserModelDiscountMultiplier,
  getUserModelDiscountPercent,
  hasUserModelDiscount,
} from '../lib/model-helpers'
import type { PricingModel } from '../types'

function model(discountBPS?: number): PricingModel {
  return {
    id: 1,
    model_name: 'gpt-test',
    quota_type: 0,
    model_ratio: 1,
    completion_ratio: 2,
    enable_groups: ['default'],
    user_model_discount_bps: discountBPS,
  }
}

describe('user model discount helpers', () => {
  test('converts a basis-point discount into catalog multipliers', () => {
    assert.equal(getUserModelDiscountMultiplier(model(8000)), 0.8)
    assert.equal(getUserModelDiscountPercent(model(8000)), 80)
    assert.equal(hasUserModelDiscount(model(8000)), true)
  })

  test('falls back to public price for missing or invalid discounts', () => {
    for (const value of [undefined, 0, 10000, 12000]) {
      assert.equal(getUserModelDiscountMultiplier(model(value)), 1)
      assert.equal(hasUserModelDiscount(model(value)), false)
    }
  })
})
