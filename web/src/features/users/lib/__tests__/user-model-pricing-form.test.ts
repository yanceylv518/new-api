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

import type { TFunction } from 'i18next'
import { afterEach, describe, expect, test, vi } from 'vitest'

import {
  buildUserModelPricingPayload,
  buildUserModelPricingRows,
  createUserModelPricingFormSchema,
  getAvailableUserModelPricingOptions,
  normalizeUserModelPricingModelName,
} from '../user-model-pricing-form'

// 测试只需要恒等翻译，类型断言用于满足 i18next 的运行时类型约束。
const t = ((key: string) => key) as TFunction

afterEach(() => vi.useRealTimers())

describe('user model pricing form', () => {
  const schema = createUserModelPricingFormSchema(t)

  test.each([null, undefined, Number.NaN, 0, 101])(
    'accepts valid scheduled periods when the unused single discount is %s',
    (discount) => {
      const result = schema.safeParse({
        items: [
          {
            model_name: 'scheduled-model',
            mode: 'scheduled',
            discount_percent: discount,
            periods: [{ discount_percent: 80, start_time: 100, end_time: 200 }],
          },
        ],
      })
      expect(result.success).toBe(true)
    }
  )

  test('rejects an invalid scheduled period discount even when the single value is valid', () => {
    expect(
      schema.safeParse({
        items: [
          {
            model_name: 'scheduled-model',
            mode: 'scheduled',
            discount_percent: 80,
            periods: [
              { discount_percent: 100, start_time: 100, end_time: 200 },
            ],
          },
        ],
      }).success
    ).toBe(false)
  })

  test('fills missing starts once from browser time and preserves existing schedules', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-08T02:00:00Z'))
    const now = Math.floor(Date.now() / 1000)
    const payload = buildUserModelPricingPayload(
      {
        items: [
          { model_name: 'new-single', discount_percent: 80 },
          {
            model_name: 'new-scheduled',
            mode: 'scheduled',
            discount_percent: 100,
            periods: [{ discount_percent: 70, end_time: now + 600 }],
          },
          { model_name: 'existing', discount_percent: 60 },
        ],
      },
      [
        {
          model_name: 'existing',
          mode: 'single',
          discount_bps: 6000,
          start_time: 100,
          end_time: 200,
        },
      ]
    )
    expect(
      payload.items.find((item) => item.model_name === 'new-single')?.start_time
    ).toBe(now)
    expect(
      payload.items.find((item) => item.model_name === 'new-scheduled')
        ?.periods?.[0].start_time
    ).toBe(now)
    expect(
      payload.items.find((item) => item.model_name === 'existing')
    ).toMatchObject({ start_time: 100, end_time: 200 })
  })

  test('normalizes aliases with the same backend matching rules', () => {
    assert.equal(
      normalizeUserModelPricingModelName('gemini-2.5-pro-thinking-1024'),
      'gemini-2.5-pro-thinking-*'
    )
    assert.equal(
      normalizeUserModelPricingModelName('gpt-4o-gizmo-abc'),
      'gpt-4o-gizmo-*'
    )
  })

  test('accepts distinct model rules with valid discount percentages', () => {
    const result = schema.safeParse({
      items: [
        { model_name: 'gpt-4o', discount_percent: 80 },
        { model_name: 'claude-3-5-sonnet', discount_percent: 65.5 },
      ],
    })

    assert.equal(result.success, true)
  })

  test('rejects models with an empty discount', () => {
    const result = schema.safeParse({
      items: [
        { model_name: 'gpt-4o', discount_percent: undefined },
        { model_name: 'claude-3-5-sonnet', discount_percent: null },
      ],
    })

    assert.equal(result.success, false)
    if (!result.success) {
      assert.equal(
        result.error.issues.some(
          (issue) => issue.message === 'Discount percentage is required'
        ),
        true
      )
    }
  })

  test('rejects aliases with the same configured discount', () => {
    const result = schema.safeParse({
      items: [
        { model_name: 'gemini-2.5-pro-thinking-1024', discount_percent: 80 },
        { model_name: 'gemini-2.5-pro-thinking-2048', discount_percent: 80 },
      ],
    })

    assert.equal(result.success, false)
  })

  test('rejects duplicate model rules', () => {
    const result = schema.safeParse({
      items: [
        { model_name: 'gpt-4o', discount_percent: 80 },
        { model_name: 'gpt-4o', discount_percent: 70 },
      ],
    })

    assert.equal(result.success, false)
  })

  test('rejects duplicate thinking-budget aliases', () => {
    const result = schema.safeParse({
      items: [
        { model_name: 'gemini-2.5-pro-thinking-1024', discount_percent: 80 },
        { model_name: 'gemini-2.5-pro-thinking-2048', discount_percent: 70 },
      ],
    })

    assert.equal(result.success, false)
  })

  test('allows duplicate aliases when all rows use full price', () => {
    const result = schema.safeParse({
      items: [
        { model_name: 'gemini-2.5-pro-thinking-1024', discount_percent: 100 },
        { model_name: 'gemini-2.5-pro-thinking-2048', discount_percent: 100 },
      ],
    })

    assert.equal(result.success, true)
  })

  test('rejects discounts outside the supported range', () => {
    assert.equal(
      schema.safeParse({
        items: [{ model_name: 'gpt-4o', discount_percent: 0 }],
      }).success,
      false
    )
    assert.equal(
      schema.safeParse({
        items: [{ model_name: 'gpt-4o', discount_percent: 100.01 }],
      }).success,
      false
    )
  })

  test('builds a payload by filtering full-price rows and converting percentages', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-10-08T02:00:00Z'))
    const startTime = Math.floor(Date.now() / 1000)
    const payload = buildUserModelPricingPayload({
      items: [
        { model_name: ' gpt-4o ', discount_percent: 80 },
        {
          model_name: 'gemini-2.5-pro-thinking-1024',
          discount_percent: 100,
        },
        { model_name: 'gemini-2.5-pro-thinking-2048', discount_percent: 75.5 },
        { model_name: 'claude-3-5-sonnet', discount_percent: 100 },
      ],
    })

    assert.deepEqual(payload, {
      items: [
        {
          model_name: 'gpt-4o',
          discount_bps: 8000,
          mode: 'single',
          start_time: startTime,
          end_time: undefined,
          periods: undefined,
        },
        {
          model_name: 'gemini-2.5-pro-thinking-*',
          discount_bps: 7550,
          mode: 'single',
          start_time: startTime,
          end_time: undefined,
          periods: undefined,
        },
      ],
    })
  })

  test('groups enabled aliases into one editable rule', () => {
    const rows = buildUserModelPricingRows([
      { model_name: 'gemini-2.5-pro-thinking-1024' },
      { model_name: 'gemini-2.5-pro-thinking-2048' },
      { model_name: 'gpt-4o' },
    ])

    assert.deepEqual(rows, [
      {
        model_name: 'gemini-2.5-pro-thinking-*',
        aliases: [
          'gemini-2.5-pro-thinking-1024',
          'gemini-2.5-pro-thinking-2048',
        ],
      },
      { model_name: 'gpt-4o', aliases: [] },
    ])
  })

  // 未启用模型不在可编辑行中，保存其他模型时仍保留它已有的折扣。
  test('preserves hidden discounts while resetting visible models to full price', () => {
    const payload = buildUserModelPricingPayload(
      { items: [{ model_name: 'enabled-model', discount_percent: 100 }] },
      [
        { model_name: 'disabled-model', discount_bps: 8000 },
        { model_name: 'enabled-model', discount_bps: 7000 },
      ]
    )
    assert.deepEqual(payload.items, [
      { model_name: 'disabled-model', discount_bps: 8000 },
    ])
  })

  test('hides models selected by other rows while keeping the current row value', () => {
    const options = [
      { value: 'gpt-4o', label: 'gpt-4o' },
      { value: 'claude-3-5-sonnet', label: 'claude-3-5-sonnet' },
      { value: 'gemini-2.5-pro', label: 'gemini-2.5-pro' },
    ]

    assert.deepEqual(
      getAvailableUserModelPricingOptions(
        options,
        ['gpt-4o', 'claude-3-5-sonnet'],
        'gpt-4o'
      ),
      [options[0], options[2]]
    )
    assert.deepEqual(
      getAvailableUserModelPricingOptions(
        options,
        ['gpt-4o', 'claude-3-5-sonnet'],
        ''
      ),
      [options[2]]
    )
  })

  test('hides thinking-budget aliases selected by another row', () => {
    const options = [
      {
        value: 'gemini-2.5-pro-thinking-1024',
        label: 'gemini-2.5-pro-thinking-1024',
      },
      {
        value: 'gemini-2.5-pro-thinking-2048',
        label: 'gemini-2.5-pro-thinking-2048',
      },
      { value: 'gpt-4o', label: 'gpt-4o' },
    ]

    assert.deepEqual(
      getAvailableUserModelPricingOptions(
        options,
        ['gemini-2.5-pro-thinking-1024'],
        'gpt-4o'
      ).map((option) => option.value),
      ['gpt-4o']
    )
  })
})
