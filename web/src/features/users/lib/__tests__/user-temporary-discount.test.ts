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
import { describe, expect, test } from 'vitest'

import {
  formatValidityRange,
  isSimpleTemporaryDiscount,
  discountDayEnd,
  discountDayStart,
  temporaryDiscountVisual,
  validityEndDate,
} from '../user-temporary-discount'

// 用明确的 UTC+8 构造期望时刻，不依赖测试进程的时区。
const timestamp = (...parts: [number, number, number, number?, number?]) =>
  (Date.UTC(...parts) - 8 * 3600 * 1000) / 1000
const labels = { start: 'on save', end: 'no end' }
const everyDay = [0, 1, 2, 3, 4, 5, 6]

describe('temporary discount validity', () => {
  test('a selected end date keeps the whole day inside the validity period', () => {
    const end = discountDayEnd(new Date('2030-10-14T15:30:00+08:00'))
    expect(end).toBe(timestamp(2030, 9, 15))
    expect(validityEndDate(end)).toEqual(new Date('2030-10-14T00:00:00+08:00'))
    expect(discountDayStart(new Date('2030-10-08T15:30:00+08:00'))).toBe(
      timestamp(2030, 9, 8)
    )
  })

  test('a minute-precision end stays on its own date', () => {
    expect(validityEndDate(timestamp(2030, 9, 15, 14, 17))).toEqual(
      new Date('2030-10-15T00:00:00+08:00')
    )
  })

  test.each([
    [
      'whole days show inclusive dates',
      { start_time: timestamp(2030, 9, 8), end_time: timestamp(2030, 9, 15) },
      '2030-10-08 → 2030-10-14',
    ],
    [
      'minute-precision boundaries keep their time',
      {
        start_time: timestamp(2030, 9, 8, 14, 17),
        end_time: timestamp(2030, 9, 15, 14, 17),
      },
      '2030-10-08 14:17 → 2030-10-15 14:17',
    ],
    [
      'missing boundaries use the caller labels',
      { start_time: null, end_time: null },
      'on save → no end',
    ],
  ])('%s', (_name, period, expected) => {
    expect(formatValidityRange(period, labels)).toBe(expected)
  })
})

describe('temporary discount editing mode', () => {
  test('a new temporary discount without rules opens in the simple form', () => {
    const { unsupported, visual } = temporaryDiscountVisual(
      { expression: 'permanent_discount' },
      80
    )
    expect(unsupported).toBe(false)
    expect(visual.branches).toEqual([])
    expect(isSimpleTemporaryDiscount(visual)).toBe(true)
  })

  test.each([
    ['a legacy fixed percentage', { discount_percent: 60 }, 60],
    ['a constant expression', { expression: '0.6' }, 60],
    [
      'the legacy all-day marker',
      { expression: '(hour("Asia/Shanghai") >= 0) ? 0.6 : permanent_discount' },
      60,
    ],
  ])('%s becomes one every-day all-day rule', (_name, rule, percent) => {
    const { unsupported, visual } = temporaryDiscountVisual(rule, 80)
    expect(unsupported).toBe(false)
    expect(visual.branches).toHaveLength(1)
    expect(visual.branches[0]).toMatchObject({
      percent,
      conditions: [],
      schedule: { days: everyDay, allDay: true },
    })
    expect(isSimpleTemporaryDiscount(visual)).toBe(true)
  })

  test.each([
    [
      'weekday-limited rules',
      '((weekday("Asia/Shanghai") == 0 || weekday("Asia/Shanghai") == 6) && (hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai")) >= 0) ? 0.6 : permanent_discount',
    ],
    [
      'multiple rules',
      '(hour("Asia/Shanghai") >= 0) ? 0.6 : (hour("Asia/Shanghai") >= 0) ? 0.5 : permanent_discount',
    ],
    [
      'raw time conditions',
      '(hour("Asia/Shanghai") >= 9) ? 0.6 : permanent_discount',
    ],
  ])('%s need advanced customization', (_name, expression) => {
    const { unsupported, visual } = temporaryDiscountVisual({ expression }, 80)
    expect(unsupported).toBe(false)
    expect(isSimpleTemporaryDiscount(visual)).toBe(false)
  })

  test('expressions outside the time editor stay untouched', () => {
    expect(
      temporaryDiscountVisual(
        { expression: 'min(0.8, max(0.4, output_tokens / 10000))' },
        80
      ).unsupported
    ).toBe(true)
  })
})
