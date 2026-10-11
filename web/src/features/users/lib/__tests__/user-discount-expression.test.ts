import assert from 'node:assert/strict'

import { describe, expect, test } from 'vitest'

import {
  DISCOUNT_FIELDS,
  DISCOUNT_TIME_FIELDS,
  discountConditionLiteral,
  discountPercentLiteral,
  generateDiscountExpression,
  parseDiscountExpression,
  type DiscountVisualConfig,
} from '../user-discount-expression'

describe('discount expression visual conversion', () => {
  test('round trips a permanent fallback without baking in a percentage', () => {
    const source =
      '(hour("Asia/Shanghai") >= 9 && hour("Asia/Shanghai") < 18) ? 0.6 : permanent_discount'
    const config = parseDiscountExpression(source)
    assert.ok(config)
    expect(config.inheritPermanent).toBe(true)
    expect(generateDiscountExpression(config)).toBe(source)
    expect(generateDiscountExpression({ ...config, percent: 75 })).toBe(source)
    expect(parseDiscountExpression('permanent_discount')).toEqual({
      percent: null,
      branches: [],
      inheritPermanent: true,
    })
  })
  test.each([
    ['hour', '23', true],
    ['hour', '24', false],
    ['hour', '1.5', false],
    ['minute', '59', true],
    ['minute', '60', false],
    ['minute', '', false],
    ['weekday', '0', true],
    ['weekday', '6', true],
    ['weekday', '7', false],
    ['weekday', '-1', false],
  ])('validates %s value %s against its time bounds', (field, value, valid) => {
    const literal = discountConditionLiteral({
      id: 'time',
      field: `${field}("Asia/Shanghai")`,
      operator: '==',
      value,
    })
    expect(literal !== null).toBe(valid)
  })
  test.each([0.01, 29, 68.5, 100])(
    'round trips fixed percentage %s with basis point precision',
    (percent) => {
      const source = generateDiscountExpression({ percent, branches: [] })
      assert.ok(source)
      expect(parseDiscountExpression(source)?.percent).toBe(percent)
    }
  )
  test.each([null, Number.NaN, 0, 100.01, 0.009, 80.123])(
    'rejects invalid or overprecise percentage %s',
    (percent) => {
      expect(discountPercentLiteral(percent)).toBeNull()
    }
  )
  test('round trips every supported field, guarded missing params, boolean and escaped string values', () => {
    for (const field of DISCOUNT_FIELDS) {
      let value = 'hello "quote" ? : \\ newline\n'
      if (field.type === 'number') value = '1.25'
      if (DISCOUNT_TIME_FIELDS.some((item) => item.value === field.value)) {
        value = '1'
      }
      if (field.type === 'boolean') value = 'true'
      const config: DiscountVisualConfig = {
        percent: 80,
        branches: [
          {
            id: 'branch',
            percent: 60,
            match: 'all',
            conditions: [
              {
                id: 'condition',
                field: field.value,
                operator: field.type === 'number' ? '>=' : '==',
                value,
              },
            ],
          },
        ],
      }
      const expression = generateDiscountExpression(config)
      assert.ok(expression)
      const parsed = parseDiscountExpression(expression)
      assert.ok(parsed, field.value)
      expect(generateDiscountExpression(parsed)).toBe(expression)
      if (field.value.startsWith('param(')) {
        expect(expression).toContain('!= nil &&')
      }
    }
  })
  test('preserves branch order and both all and any condition matching', () => {
    const source =
      '(output_tokens >= 10000 || input_tokens >= 20000) ? 0.6 : (count > 2 && seconds <= 5) ? 0.7 : 0.8'
    const parsed = parseDiscountExpression(source)
    assert.ok(parsed)
    expect(generateDiscountExpression(parsed)).toBe(source)
  })
  test.each([
    'min(0.5, 0.8)',
    'output_tokens / 10000',
    'output_tokens >= 1 ? 0.5 : bad',
    '(output_tokens >= 1 && count > 2 || seconds > 3) ? 0.5 : 0.8',
    '0.8 trailing',
    'param("secret") == "x" ? 0.5 : 1',
    '0.333333',
  ])('does not convert unsupported source %s', (source) => {
    expect(parseDiscountExpression(source)).toBeNull()
  })
  test('rejects missing values and oversized conditions without manufacturing a valid fallback', () => {
    const condition = {
      id: 'c',
      field: 'output_tokens',
      operator: '>=' as const,
      value: '',
    }
    expect(
      generateDiscountExpression({
        percent: 80,
        branches: [
          { id: 'b', percent: 60, match: 'all', conditions: [condition] },
        ],
      })
    ).toBeNull()
    expect(
      generateDiscountExpression({
        percent: 80,
        branches: Array.from({ length: 5 }, (_, i) => ({
          id: String(i),
          percent: 60,
          match: 'all',
          conditions: [{ ...condition, value: '100' }],
        })),
      })
    ).toBeNull()
  })
})
