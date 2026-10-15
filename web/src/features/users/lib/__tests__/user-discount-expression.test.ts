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
import {
  beijingTime,
  matchesTimeBranch,
  timeBranchDiagnostics,
  type WeeklyDiscountSchedule,
} from '../user-discount-time'

describe('weekly time range discounts', () => {
  test('matches the independent weekly boundary expectations after parsing editor expressions', () => {
    const permanentPercent = 80
    const branch = (
      id: string,
      percent: number,
      schedule: WeeklyDiscountSchedule
    ) => ({ id, percent, match: 'all' as const, conditions: [], schedule })
    const fixtures = [
      {
        name: 'weekdays and weekend',
        branches: [
          branch('weekday', 70, {
            days: [1, 2, 3, 4, 5],
            start: '09:00',
            end: '23:00',
            allDay: false,
          }),
          branch('weekend', 60, {
            days: [0, 6],
            start: '09:00',
            end: '18:00',
            allDay: true,
          }),
        ],
        cases: [
          ['2026-10-12T08:59:59+08:00', 8000],
          ['2026-10-12T09:00:00+08:00', 7000],
          ['2026-10-16T22:59:59+08:00', 7000],
          ['2026-10-16T23:00:00+08:00', 8000],
          ['2026-10-17T00:00:00+08:00', 6000],
          ['2026-10-18T23:59:59+08:00', 6000],
        ],
      },
      {
        name: 'Friday overnight',
        branches: [
          branch('night', 60, {
            days: [5],
            start: '23:00',
            end: '07:00',
            allDay: false,
          }),
        ],
        cases: [
          ['2026-10-16T06:59:59+08:00', 8000],
          ['2026-10-16T22:59:59+08:00', 8000],
          ['2026-10-16T23:00:00+08:00', 6000],
          ['2026-10-17T06:59:59+08:00', 6000],
          ['2026-10-17T07:00:00+08:00', 8000],
        ],
      },
      {
        name: 'minute precision',
        branches: [
          branch('minute', 29, {
            days: [0, 1, 2, 3, 4, 5, 6],
            start: '09:30',
            end: '20:15',
            allDay: false,
          }),
        ],
        cases: [
          ['2026-10-12T09:29:59+08:00', 8000],
          ['2026-10-12T09:30:00+08:00', 2900],
          ['2026-10-12T10:00:00+08:00', 2900],
          ['2026-10-12T20:14:59+08:00', 2900],
          ['2026-10-12T20:15:00+08:00', 8000],
        ],
      },
      {
        name: 'four complex overnight branches',
        branches: Array.from({ length: 4 }, (_, index) =>
          branch(`night-${index}`, 60 + index, {
            days: [index, (index + 2) % 7, (index + 4) % 7],
            start: `${20 + index}:15`,
            end: `0${4 + index}:45`,
            allDay: false,
          })
        ),
        cases: [['2026-10-12T12:00:00+08:00', 8000]],
      },
      {
        name: 'all day',
        branches: [
          branch('all', 60, {
            days: [0, 1, 2, 3, 4, 5, 6],
            start: '09:00',
            end: '18:00',
            allDay: true,
          }),
        ],
        cases: [
          ['2026-10-12T00:00:00+08:00', 6000],
          ['2026-10-12T23:59:59+08:00', 6000],
        ],
      },
    ].map((fixture) => {
      const source = generateDiscountExpression({
        percent: permanentPercent,
        inheritPermanent: true,
        branches: fixture.branches,
      })
      assert.ok(source)
      assert.ok(parseDiscountExpression(source))
      return {
        name: fixture.name,
        source,
        cases: fixture.cases.map(([date, bps]) => ({
          at: Date.parse(String(date)) / 1000,
          bps,
        })),
      }
    })
    for (const fixture of fixtures) {
      const parsed = parseDiscountExpression(fixture.source)
      assert.ok(parsed)
      for (const boundary of fixture.cases) {
        const { day, minute } = beijingTime(boundary.at)
        const matched: DiscountVisualConfig['branches'][number] | undefined =
          parsed.branches.find((candidate) =>
            matchesTimeBranch(candidate, day, minute)
          )
        const percent: number | null =
          matched?.percent ??
          (parsed.inheritPermanent ? permanentPercent : parsed.percent)
        assert.ok(percent !== null, fixture.name)
        expect(percent * 100, `${fixture.name} at ${boundary.at}`).toBe(
          boundary.bps
        )
      }
    }
  })
  test.each([
    { days: [1, 2, 3, 4, 5], start: '09:00', end: '23:00', allDay: false },
    { days: [0, 6], start: '09:00', end: '18:00', allDay: true },
    { days: [5], start: '23:00', end: '07:00', allDay: false },
    { days: [0], start: '23:15', end: '00:30', allDay: false },
    {
      days: [0, 1, 2, 3, 4, 5, 6],
      start: '09:30',
      end: '20:15',
      allDay: false,
    },
    { days: [1, 3, 5], start: '23:59', end: '00:01', allDay: false },
  ] satisfies WeeklyDiscountSchedule[])(
    'preserves $days $start–$end after saving and reopening',
    (schedule) => {
      const branch = {
        id: 'weekly',
        percent: 70,
        match: 'all' as const,
        conditions: [],
        schedule,
      }
      const expression = generateDiscountExpression({
        percent: 80,
        inheritPermanent: true,
        branches: [branch],
      })
      assert.ok(expression)
      const parsed = parseDiscountExpression(expression)
      assert.ok(parsed)
      expect(generateDiscountExpression(parsed)).toBe(expression)
      for (let day = 0; day < 7; day++) {
        for (let minute = 0; minute < 1440; minute++) {
          expect(matchesTimeBranch(parsed.branches[0], day, minute)).toBe(
            matchesTimeBranch(branch, day, minute)
          )
        }
      }
    }
  )
  test('includes the starting weekday and the following morning for overnight ranges', () => {
    const branch = {
      id: 'night',
      percent: 60,
      match: 'all' as const,
      conditions: [],
      schedule: { days: [5], start: '23:00', end: '07:00', allDay: false },
    }
    expect(matchesTimeBranch(branch, 5, 1380)).toBe(true)
    expect(matchesTimeBranch(branch, 6, 419)).toBe(true)
    expect(matchesTimeBranch(branch, 6, 420)).toBe(false)
    expect(matchesTimeBranch(branch, 5, 419)).toBe(false)
  })
  test('detects contradictions, complete shadowing and partial priority overlap', () => {
    const condition = (operator: '>=' | '<', value: string) => ({
      id: value,
      field: 'hour("Asia/Shanghai")',
      operator,
      value,
    })
    const impossible = {
      id: 'impossible',
      percent: 70,
      match: 'all' as const,
      conditions: [condition('>=', '23'), condition('<', '7')],
    }
    const wide = {
      id: 'wide',
      percent: 70,
      match: 'all' as const,
      conditions: [condition('>=', '9')],
    }
    const narrower = {
      ...wide,
      id: 'narrower',
      conditions: [condition('>=', '12')],
    }
    const partial = {
      ...wide,
      id: 'partial',
      conditions: [condition('<', '12')],
    }
    expect(
      timeBranchDiagnostics([impossible, wide, narrower, partial])
    ).toEqual([
      { impossible: true, shadowed: false, overlapping: false },
      { impossible: false, shadowed: false, overlapping: false },
      { impossible: false, shadowed: true, overlapping: false },
      { impossible: false, shadowed: false, overlapping: true },
    ])
  })
  test('interprets request time in Beijing regardless of the browser timezone', () => {
    expect(beijingTime(Date.parse('2026-10-12T01:30:00Z') / 1000)).toEqual({
      day: 1,
      minute: 570,
    })
  })
  test.each([
    { days: [], start: '09:00', end: '18:00', allDay: false },
    { days: [1], start: '09:00', end: '09:00', allDay: false },
    { days: [1], start: '24:00', end: '09:00', allDay: false },
  ])('rejects incomplete or empty weekly ranges', (schedule) => {
    expect(
      generateDiscountExpression({
        percent: 80,
        branches: [
          {
            id: 'invalid',
            percent: 70,
            match: 'all',
            conditions: [],
            schedule,
          },
        ],
      })
    ).toBeNull()
  })
})

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
