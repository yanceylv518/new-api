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

import dayjs from '@/lib/dayjs'

import {
  DISCOUNT_TIME_FIELDS,
  inheritPermanentDiscount,
  isTimeDiscountConfig,
  parseDiscountExpression,
  type DiscountBranch,
  type DiscountVisualConfig,
} from './user-discount-expression'
import {
  describeTimeBranch,
  type WeeklyDiscountSchedule,
} from './user-discount-time'

interface TemporaryDiscountRule {
  expression?: string
  discount_percent?: number | null
}

interface TemporaryDiscountValidity {
  start_time?: number | null
  end_time?: number | null
}

export const ALL_DAY_EVERY_DAY: WeeklyDiscountSchedule = {
  days: [0, 1, 2, 3, 4, 5, 6],
  start: '09:00',
  end: '18:00',
  allDay: true,
}

// 旧固定比例和纯常量表达式用 hour >= 0 表示全天，展示时统一成可读的全天时段。
function withAllDaySchedule(branch: DiscountBranch): DiscountBranch {
  const [condition] = branch.conditions
  if (
    branch.schedule ||
    branch.conditions.length !== 1 ||
    condition.field !== DISCOUNT_TIME_FIELDS[0].value ||
    condition.operator !== '>=' ||
    Number(condition.value) !== 0
  ) {
    return branch
  }
  return {
    ...branch,
    match: 'all',
    conditions: [],
    schedule: { ...ALL_DAY_EVERY_DAY },
  }
}

/** 将已保存的临时折扣转换为时间规则；无法用时间规则表达的表达式保持原样。 */
export function temporaryDiscountVisual(
  rule: TemporaryDiscountRule,
  permanentPercent: number | null | undefined
): { unsupported: boolean; visual: DiscountVisualConfig } {
  const parsed = parseDiscountExpression(rule.expression ?? '')
  const fallback = permanentPercent ?? 100
  if (parsed && isTimeDiscountConfig(parsed)) {
    const visual = inheritPermanentDiscount({
      ...parsed,
      branches: parsed.branches.map((branch) => ({
        ...branch,
        conditions: branch.conditions.map((condition) => ({
          ...condition,
          value: String(Number(condition.value)),
        })),
      })),
    })
    return {
      unsupported: false,
      visual: { ...visual, branches: visual.branches.map(withAllDaySchedule) },
    }
  }
  if (rule.expression?.trim()) {
    return {
      unsupported: true,
      visual: { percent: fallback, inheritPermanent: true, branches: [] },
    }
  }
  return {
    unsupported: false,
    visual: {
      percent: fallback,
      inheritPermanent: true,
      branches: [
        {
          id: 'existing-fixed',
          match: 'all',
          percent: rule.discount_percent ?? 100,
          conditions: [],
          schedule: { ...ALL_DAY_EVERY_DAY },
        },
      ],
    },
  }
}

/** 没有规则或只有一条每天生效的时段规则时，可以用简单表单编辑。 */
export function isSimpleTemporaryDiscount(
  visual: DiscountVisualConfig
): boolean {
  if (visual.branches.length === 0) return true
  if (visual.branches.length !== 1) return false
  const schedule = visual.branches[0].schedule
  return Boolean(schedule && new Set(schedule.days).size === 7)
}

export function isLocalDayStart(timestamp: number): boolean {
  return dayjs.unix(timestamp).startOf('day').unix() === timestamp
}

export function localDayStart(date: Date): number {
  return dayjs(date).startOf('day').unix()
}

/** 有效期不包含结束时刻，所选结束日期的整天都在有效期内。 */
export function localDayEnd(date: Date): number {
  return dayjs(date).startOf('day').add(1, 'day').unix()
}

/** 结束于零点时展示前一天，否则展示结束时刻所在日期。 */
export function validityEndDate(end: number): Date {
  const time = dayjs.unix(end)
  return (
    isLocalDayStart(end) ? time.subtract(1, 'day') : time.startOf('day')
  ).toDate()
}

/** 按整天设置的有效期只显示日期，其余情况保留到分钟。 */
export function formatValidityRange(
  period: TemporaryDiscountValidity,
  labels: { start: string; end: string }
): string {
  let start = labels.start
  if (period.start_time) {
    start = dayjs
      .unix(period.start_time)
      .format(
        isLocalDayStart(period.start_time) ? 'YYYY-MM-DD' : 'YYYY-MM-DD HH:mm'
      )
  }
  let end = labels.end
  if (period.end_time != null) {
    end = isLocalDayStart(period.end_time)
      ? dayjs(validityEndDate(period.end_time)).format('YYYY-MM-DD')
      : dayjs.unix(period.end_time).format('YYYY-MM-DD HH:mm')
  }
  return `${start} → ${end}`
}

/** 列表和摘要中展示的时间规则；全天规则不需要额外说明时段。 */
export function describeTemporaryDiscountRules(
  rule: TemporaryDiscountRule,
  permanentPercent: number | null | undefined,
  t: TFunction
): { unsupported: boolean; rules: { when: string; percent: number | null }[] } {
  const { unsupported, visual } = temporaryDiscountVisual(
    rule,
    permanentPercent
  )
  return {
    unsupported,
    rules: visual.branches.map((branch) => ({
      when: describeTimeBranch(branch, t),
      percent: branch.percent,
    })),
  }
}
