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

import type {
  DiscountBranch,
  DiscountCondition,
} from './user-discount-expression'

export const MINUTE_OF_DAY =
  '(hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai"))'
const WEEKDAY = 'weekday("Asia/Shanghai")'
const HOUR = 'hour("Asia/Shanghai")'
const MINUTE = 'minute("Asia/Shanghai")'
export const WEEKDAY_NAMES = [
  'Sunday',
  'Monday',
  'Tuesday',
  'Wednesday',
  'Thursday',
  'Friday',
  'Saturday',
] as const

export interface WeeklyDiscountSchedule {
  days: number[]
  start: string
  end: string
  allDay: boolean
}

export type DiscountPredicate =
  | { condition: DiscountCondition }
  | { match: 'all' | 'any'; parts: DiscountPredicate[] }

export function timeToMinute(value: string): number | null {
  if (!/^\d{2}:\d{2}$/.test(value)) return null
  const [hour, minute] = value.split(':').map(Number)
  return hour < 24 && minute < 60 ? hour * 60 + minute : null
}

function minuteToTime(value: number): string {
  return `${String(Math.floor(value / 60)).padStart(2, '0')}:${String(value % 60).padStart(2, '0')}`
}

export function weeklySchedulePredicate(
  schedule: WeeklyDiscountSchedule
): string | null {
  const days = [...new Set(schedule.days)].sort((a, b) => a - b)
  if (
    !days.length ||
    days.some((day) => !Number.isInteger(day) || day < 0 || day > 6)
  ) {
    return null
  }
  const dayExpression = (selected: number[]) => {
    if (selected.length === 7) return `${WEEKDAY} >= 0`
    const excluded = Array.from({ length: 7 }, (_, day) => day).filter(
      (day) => !selected.includes(day)
    )
    const complement = excluded.length < selected.length
    const values = complement ? excluded : selected
    return `(${values.map((day) => `${WEEKDAY} ${complement ? '!=' : '=='} ${day}`).join(complement ? ' && ' : ' || ')})`
  }
  if (schedule.allDay) {
    return `(${dayExpression(days)} && ${MINUTE_OF_DAY} >= 0)`
  }
  const start = timeToMinute(schedule.start)
  const end = timeToMinute(schedule.end)
  if (start === null || end === null || start === end) return null
  if (start < end) {
    return `(${dayExpression(days)} && ${MINUTE_OF_DAY} >= ${start} && ${MINUTE_OF_DAY} < ${end})`
  }
  const nextDays = days.map((day) => (day + 1) % 7).sort((a, b) => a - b)
  return `((${dayExpression(days)} && ${MINUTE_OF_DAY} >= ${start}) || (${dayExpression(nextDays)} && ${MINUTE_OF_DAY} < ${end}))`
}

function compare(left: number, operator: string, right: number): boolean {
  switch (operator) {
    case '>=':
      return left >= right
    case '>':
      return left > right
    case '<=':
      return left <= right
    case '<':
      return left < right
    case '==':
      return left === right
    case '!=':
      return left !== right
    default:
      return false
  }
}

function weekdayOnly(predicate: DiscountPredicate): boolean {
  return 'condition' in predicate
    ? predicate.condition.field === WEEKDAY
    : predicate.parts.every(weekdayOnly)
}

function weekdayMatches(predicate: DiscountPredicate, day: number): boolean {
  if ('condition' in predicate) {
    return compare(
      day,
      predicate.condition.operator,
      Number(predicate.condition.value)
    )
  }
  const matches = predicate.parts.map((part) => weekdayMatches(part, day))
  return predicate.match === 'all'
    ? matches.every(Boolean)
    : matches.some(Boolean)
}

function splitSchedule(predicate: DiscountPredicate) {
  const parts =
    'match' in predicate && predicate.match === 'all'
      ? predicate.parts
      : [predicate]
  const dayParts = parts.filter(weekdayOnly)
  const timeParts = parts.filter((part) => !weekdayOnly(part))
  if (
    timeParts.some(
      (part) => !('condition' in part) || part.condition.field !== MINUTE_OF_DAY
    )
  ) {
    return null
  }
  const days = Array.from({ length: 7 }, (_, day) => day).filter((day) =>
    dayParts.every((part) => weekdayMatches(part, day))
  )
  if (!days.length) return null
  return {
    days,
    conditions: timeParts.flatMap((part) =>
      'condition' in part ? [part.condition] : []
    ),
  }
}

export function predicateWeeklySchedule(
  predicate: DiscountPredicate
): WeeklyDiscountSchedule | null {
  const parts =
    'match' in predicate && predicate.match === 'any' && !weekdayOnly(predicate)
      ? predicate.parts
      : [predicate]
  const parsed = parts.map(splitSchedule)
  if (parsed.some((part) => part === null)) return null
  const first = parsed[0]
  if (!first || !first.days.length) return null
  if (
    parsed.length === 1 &&
    first.conditions.length === 1 &&
    first.conditions[0].operator === '>=' &&
    first.conditions[0].value === '0'
  ) {
    return { days: first.days, start: '09:00', end: '18:00', allDay: true }
  }
  const comparisons = parsed.flatMap((part) => part?.conditions ?? [])
  const start = comparisons.find((condition) => condition.operator === '>=')
  const end = comparisons.find((condition) => condition.operator === '<')
  if (comparisons.length !== 2 || !start || !end) return null
  const startMinute = Number(start.value)
  const endMinute = Number(end.value)
  if (
    !Number.isInteger(startMinute) ||
    !Number.isInteger(endMinute) ||
    startMinute < 0 ||
    startMinute >= 1440 ||
    endMinute < 0 ||
    endMinute >= 1440 ||
    startMinute === endMinute
  ) {
    return null
  }
  if (parsed.length === 1 && startMinute > endMinute) return null
  if (parsed.length === 2) {
    const second = parsed[1]
    if (
      startMinute < endMinute ||
      first.conditions.length !== 1 ||
      first.conditions[0] !== start ||
      !second ||
      second.conditions.length !== 1 ||
      second.conditions[0] !== end
    ) {
      return null
    }
    const nextDays = first.days
      .map((day) => (day + 1) % 7)
      .sort((a, b) => a - b)
    if (nextDays.join(',') !== second.days.join(',')) return null
  }
  if (parsed.length > 2) return null
  return {
    days: first.days,
    start: minuteToTime(startMinute),
    end: minuteToTime(endMinute),
    allDay: false,
  }
}

export function matchesTimeBranch(
  branch: DiscountBranch,
  day: number,
  minute: number
): boolean {
  if (branch.schedule) {
    const schedule = branch.schedule
    if (schedule.allDay) return schedule.days.includes(day)
    const start = timeToMinute(schedule.start)
    const end = timeToMinute(schedule.end)
    if (start === null || end === null || start === end) return false
    if (start < end) {
      return schedule.days.includes(day) && minute >= start && minute < end
    }
    return (
      (schedule.days.includes(day) && minute >= start) ||
      (schedule.days.includes((day + 6) % 7) && minute < end)
    )
  }
  const match = (condition: DiscountCondition) => {
    let value: number
    switch (condition.field) {
      case WEEKDAY:
        value = day
        break
      case HOUR:
        value = Math.floor(minute / 60)
        break
      case MINUTE:
        value = minute % 60
        break
      default:
        return false
    }
    return compare(value, condition.operator, Number(condition.value))
  }
  return branch.match === 'all'
    ? branch.conditions.every(match)
    : branch.conditions.some(match)
}

export function timeBranchDiagnostics(branches: DiscountBranch[]) {
  const covered = new Uint8Array(7 * 1440)
  return branches.map((branch) => {
    let hits = 0,
      available = 0
    for (let minute = 0; minute < covered.length; minute++) {
      if (
        !matchesTimeBranch(branch, Math.floor(minute / 1440), minute % 1440)
      ) {
        continue
      }
      hits++
      if (!covered[minute]) available++
      covered[minute] = 1
    }
    return {
      impossible: hits === 0,
      shadowed: hits > 0 && available === 0,
      overlapping: available > 0 && available < hits,
    }
  })
}

const beijingClock = new Intl.DateTimeFormat('en-US', {
  timeZone: 'Asia/Shanghai',
  weekday: 'long',
  hour: '2-digit',
  minute: '2-digit',
  hourCycle: 'h23',
})
export function beijingTime(at: number) {
  const parts = beijingClock.formatToParts(at * 1000)
  const value = (name: string) =>
    parts.find((part) => part.type === name)?.value ?? ''
  return {
    day: (WEEKDAY_NAMES as readonly string[]).indexOf(value('weekday')),
    minute: Number(value('hour')) * 60 + Number(value('minute')),
  }
}

export function describeTimeBranch(
  branch: DiscountBranch,
  t: TFunction
): string {
  if (branch.schedule) {
    const schedule = branch.schedule
    const days =
      schedule.days.length === 7
        ? t('Every day')
        : [1, 2, 3, 4, 5, 6, 0]
            .filter((day) => schedule.days.includes(day))
            .map((day) => t(WEEKDAY_NAMES[day]))
            .join('、')
    const overnight =
      !schedule.allDay &&
      (timeToMinute(schedule.start) ?? 0) > (timeToMinute(schedule.end) ?? 0)
    return `${days} · ${schedule.allDay ? t('All day') : `${schedule.start}–${overnight ? `${t('Next day')} ` : ''}${schedule.end}`}`
  }
  const symbols = {
    '>=': '≥',
    '>': '>',
    '<=': '≤',
    '<': '<',
    '==': '=',
    '!=': '≠',
  }
  return branch.conditions
    .map((condition) => {
      let label = t('Day of week'),
        value = t(WEEKDAY_NAMES[Number(condition.value)])
      if (condition.field === HOUR) {
        label = t('Daily hour')
        value = `${condition.value.padStart(2, '0')}:00`
      }
      if (condition.field === MINUTE) {
        label = t('Minute within each hour')
        value = condition.value.padStart(2, '0')
      }
      return `${label} ${symbols[condition.operator]} ${value}`
    })
    .join(branch.match === 'all' ? t(' · and · ') : t(' · or · '))
}
