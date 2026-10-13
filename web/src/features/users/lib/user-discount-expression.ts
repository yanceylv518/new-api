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
import {
  MINUTE_OF_DAY,
  predicateWeeklySchedule,
  weeklySchedulePredicate,
  type DiscountPredicate,
  type WeeklyDiscountSchedule,
} from './user-discount-time'

export const DISCOUNT_TIME_FIELDS = [
  { value: 'hour("Asia/Shanghai")', label: 'Daily hour', max: 23 },
  {
    value: 'minute("Asia/Shanghai")',
    label: 'Minute within each hour',
    max: 59,
  },
  { value: 'weekday("Asia/Shanghai")', label: 'Day of week', max: 6 },
] as const

export const DISCOUNT_FIELDS = [
  ...[
    'input_tokens',
    'output_tokens',
    'task_tokens',
    'cache_read_tokens',
    'seconds',
    'count',
    'units',
  ].map((name) => ({ value: name, type: 'number' as const })),
  ...DISCOUNT_TIME_FIELDS.map((field) => ({
    value: field.value,
    type: 'number' as const,
  })),
  ...['model', 'service_tier', 'quality', 'resolution', 'size'].map((name) => ({
    value: `param("${name}")`,
    type: 'string' as const,
  })),
  ...['duration', 'seconds', 'n', 'max_tokens', 'max_completion_tokens'].map(
    (name) => ({ value: `param("${name}")`, type: 'number' as const })
  ),
  ...['generate_audio', 'stream'].map((name) => ({
    value: `param("${name}")`,
    type: 'boolean' as const,
  })),
]

export const DISCOUNT_COMPARISONS = ['>=', '>', '<=', '<', '==', '!='] as const
export const MAX_DISCOUNT_BRANCHES = 4
export const MAX_DISCOUNT_CONDITIONS = 4

export interface DiscountCondition {
  id: string
  field: string
  operator: (typeof DISCOUNT_COMPARISONS)[number]
  value: string
}

export interface DiscountBranch {
  id: string
  conditions: DiscountCondition[]
  match: 'all' | 'any'
  percent: number | null
  schedule?: WeeklyDiscountSchedule
}

export interface DiscountVisualConfig {
  percent: number | null
  inheritPermanent?: boolean
  branches: DiscountBranch[]
}

export function isTimeDiscountConfig(config: DiscountVisualConfig): boolean {
  return config.branches.every((branch) =>
    branch.schedule
      ? weeklySchedulePredicate(branch.schedule) !== null
      : branch.conditions.every((condition) => {
          const field = DISCOUNT_TIME_FIELDS.find(
            (item) => item.value === condition.field
          )
          return Boolean(
            field &&
            discountConditionLiteral(condition) !== null &&
            (field.value !== DISCOUNT_TIME_FIELDS[2].value ||
              condition.operator === '==' ||
              condition.operator === '!=')
          )
        })
  )
}

export function inheritPermanentDiscount(
  config: DiscountVisualConfig
): DiscountVisualConfig {
  return {
    ...config,
    inheritPermanent: true,
    branches:
      config.branches.length || config.inheritPermanent
        ? config.branches
        : [
            {
              id: 'existing-fixed',
              match: 'all',
              percent: config.percent,
              conditions: [
                {
                  id: 'existing-all-day',
                  field: DISCOUNT_TIME_FIELDS[0].value,
                  operator: '>=',
                  value: '0',
                },
              ],
            },
          ],
  }
}

export function discountPercentLiteral(percent: number | null): string | null {
  if (
    percent == null ||
    !Number.isFinite(percent) ||
    percent < 0.01 ||
    percent > 100
  ) {
    return null
  }
  const bps = Math.round(percent * 100)
  if (Math.abs(bps - percent * 100) > 1e-8) return null
  return String(bps / 10000)
}

export function discountConditionLiteral(
  condition: DiscountCondition
): string | null {
  const field = DISCOUNT_FIELDS.find((item) => item.value === condition.field)
  if (!field || !DISCOUNT_COMPARISONS.includes(condition.operator)) return null
  if (
    field.type !== 'number' &&
    condition.operator !== '==' &&
    condition.operator !== '!='
  ) {
    return null
  }
  if (field.type === 'string') return JSON.stringify(condition.value)
  if (field.type === 'boolean') {
    return condition.value === 'true' || condition.value === 'false'
      ? condition.value
      : null
  }
  if (!condition.value.trim()) return null
  const value = Number(condition.value)
  const timeField = DISCOUNT_TIME_FIELDS.find(
    (item) => item.value === condition.field
  )
  if (
    timeField &&
    (!Number.isInteger(value) || value < 0 || value > timeField.max)
  ) {
    return null
  }
  return Number.isFinite(value) && value >= 0 && value <= 2147483647
    ? String(value)
    : null
}

export function generateDiscountExpression(
  config: DiscountVisualConfig
): string | null {
  let source = config.inheritPermanent
    ? 'permanent_discount'
    : discountPercentLiteral(config.percent)
  if (source === null || config.branches.length > MAX_DISCOUNT_BRANCHES) {
    return null
  }
  for (const branch of [...config.branches].reverse()) {
    const ratio = discountPercentLiteral(branch.percent)
    if (branch.schedule) {
      const predicate = weeklySchedulePredicate(branch.schedule)
      if (ratio === null || predicate === null) return null
      source = `(${predicate}) ? ${ratio} : ${source}`
      continue
    }
    if (
      ratio === null ||
      branch.conditions.length === 0 ||
      branch.conditions.length > MAX_DISCOUNT_CONDITIONS
    ) {
      return null
    }
    const conditions: string[] = []
    for (const condition of branch.conditions) {
      const value = discountConditionLiteral(condition)
      if (value === null) return null
      const comparison = `${condition.field} ${condition.operator} ${value}`
      conditions.push(
        condition.field.startsWith('param(')
          ? `(${condition.field} != nil && ${comparison})`
          : comparison
      )
    }
    source = `(${conditions.join(branch.match === 'all' ? ' && ' : ' || ')}) ? ${ratio} : ${source}`
  }
  return source.length <= 4096 ? source : null
}

// 仅解析可视化编辑器支持的标量条件子集，复杂表达式保留原文，不执行或猜测转换。
export function parseDiscountExpression(
  expression: string
): DiscountVisualConfig | null {
  const source = expression.trim()
  if (!source || source.length > 4096) return null
  const tokenizer =
    /\s*("(?:\\.|[^"\\])*"|\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|[A-Za-z_]\w*|>=|<=|==|!=|&&|\|\||[()?:,<>*+])/gy
  const tokens: string[] = []
  let offset = 0
  while (offset < source.length) {
    tokenizer.lastIndex = offset
    const match = tokenizer.exec(source)
    if (!match) return null
    tokens.push(match[1])
    offset = tokenizer.lastIndex
  }
  let position = 0
  const readPercent = () => {
    const token = tokens[position++]
    if (!token || !/^\d/.test(token)) return null
    const percent = Number(token) * 100
    return discountPercentLiteral(percent) === null
      ? null
      : Math.round(percent * 100) / 100
  }
  const readField = (): string | null => {
    if (
      tokens[position] === '(' &&
      tokens[position + 1] === 'hour' &&
      tokens[position + 5] === '*'
    ) {
      const expected = [
        '(',
        'hour',
        '(',
        '"Asia/Shanghai"',
        ')',
        '*',
        '60',
        '+',
        'minute',
        '(',
        '"Asia/Shanghai"',
        ')',
        ')',
      ]
      if (expected.some((token, index) => tokens[position + index] !== token)) {
        return null
      }
      position += expected.length
      return MINUTE_OF_DAY
    }
    let field = tokens[position++]
    if (!field) return null
    if (tokens[position] === '(') {
      position++
      const argument = tokens[position++]
      if (!argument?.startsWith('"') || tokens[position++] !== ')') return null
      try {
        field = `${field}(${JSON.stringify(JSON.parse(argument))})`
      } catch {
        return null
      }
    }
    return DISCOUNT_FIELDS.some((item) => item.value === field) ? field : null
  }
  const readCondition = (): DiscountCondition | null => {
    const id = `condition-${position}`
    const guarded = tokens[position] === '(' && tokens[position + 1] === 'param'
    if (guarded) position++
    const field = readField()
    if (!field) return null
    if (
      guarded &&
      (!field.startsWith('param(') ||
        tokens[position++] !== '!=' ||
        tokens[position++] !== 'nil' ||
        tokens[position++] !== '&&' ||
        readField() !== field)
    ) {
      return null
    }
    const operator = tokens[position++]
    const token = tokens[position++]
    if (!DISCOUNT_COMPARISONS.some((item) => item === operator) || !token) {
      return null
    }
    const comparison = DISCOUNT_COMPARISONS.find((item) => item === operator)
    if (!comparison) return null
    let value = token
    if (token.startsWith('"')) {
      try {
        const decoded: unknown = JSON.parse(token)
        if (typeof decoded !== 'string') return null
        value = decoded
      } catch {
        return null
      }
    }
    const condition = { id, field, operator: comparison, value }
    const definition =
      field === MINUTE_OF_DAY
        ? { type: 'number' as const }
        : DISCOUNT_FIELDS.find((item) => item.value === field)
    if (
      !definition ||
      (definition.type === 'string' && !token.startsWith('"')) ||
      (definition.type !== 'string' && token.startsWith('"'))
    ) {
      return null
    }
    if (guarded && tokens[position++] !== ')') return null
    if (field === MINUTE_OF_DAY) {
      return /^\d+$/.test(value) && Number(value) <= 1440 ? condition : null
    }
    return discountConditionLiteral(condition) === null ? null : condition
  }
  const readPrimary = (depth: number): DiscountPredicate | null => {
    if (depth > 20) return null
    if (
      tokens[position] === '(' &&
      tokens[position + 1] !== 'param' &&
      !(tokens[position + 1] === 'hour' && tokens[position + 5] === '*')
    ) {
      position++
      const predicate = readOr(depth + 1)
      return predicate && tokens[position++] === ')' ? predicate : null
    }
    const condition = readCondition()
    return condition ? { condition } : null
  }
  const readAnd = (depth: number): DiscountPredicate | null => {
    const first = readPrimary(depth)
    if (!first) return null
    const parts = [first]
    while (tokens[position] === '&&') {
      position++
      const next = readPrimary(depth)
      if (!next) return null
      parts.push(next)
    }
    return parts.length === 1 ? first : { match: 'all', parts }
  }
  const readOr = (depth: number): DiscountPredicate | null => {
    const first = readAnd(depth)
    if (!first) return null
    const parts = [first]
    while (tokens[position] === '||') {
      position++
      const next = readAnd(depth)
      if (!next) return null
      parts.push(next)
    }
    return parts.length === 1 ? first : { match: 'any', parts }
  }
  const branches: DiscountBranch[] = []
  while (
    position < tokens.length &&
    !/^\d/.test(tokens[position]) &&
    tokens[position] !== 'permanent_discount'
  ) {
    if (branches.length >= MAX_DISCOUNT_BRANCHES) return null
    const predicate = readOr(0)
    if (!predicate) return null
    const schedule = predicateWeeklySchedule(predicate)
    const match = 'match' in predicate ? predicate.match : 'all'
    const parts = 'parts' in predicate ? predicate.parts : [predicate]
    const conditions = parts.flatMap((part) =>
      'condition' in part ? [part.condition] : []
    )
    if (
      !schedule &&
      (conditions.length !== parts.length ||
        conditions.length > MAX_DISCOUNT_CONDITIONS)
    ) {
      return null
    }
    if (tokens[position++] !== '?') return null
    const percent = readPercent()
    if (percent === null || tokens[position++] !== ':') return null
    branches.push({
      id: `branch-${branches.length}`,
      conditions,
      match,
      percent,
      ...(schedule ? { schedule, conditions: [] } : {}),
    })
  }
  if (tokens[position] === 'permanent_discount') {
    return position + 1 === tokens.length
      ? { percent: null, branches, inheritPermanent: true }
      : null
  }
  const percent = readPercent()
  return percent !== null && position === tokens.length
    ? { percent, branches }
    : null
}
