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
/**
 * 按耗时分配宽度：按比例不足 minWidth 的段固定为 minWidth，其余段按耗时比例分剩下的宽度。
 * 最小宽度不超过每段均分的宽度；minWidth 为 0 时就是纯比例。
 */
export function allocateWidths(
  durations: number[],
  width: number,
  minWidth: number
): number[] {
  if (durations.length === 0) return []
  const minimumWidth = Math.min(minWidth, width / durations.length)
  const fixed = durations.map(() => false)
  for (let pass = 0; pass <= durations.length; pass++) {
    const freeDuration = durations.reduce(
      (sum, duration, i) => (fixed[i] ? sum : sum + duration),
      0
    )
    const freeWidth = width - fixed.filter(Boolean).length * minimumWidth
    let changed = false
    durations.forEach((duration, i) => {
      if (fixed[i]) return
      if (
        freeDuration <= 0 ||
        (duration / freeDuration) * freeWidth < minimumWidth
      ) {
        fixed[i] = true
        changed = true
      }
    })
    if (!changed) {
      return durations.map((duration, i) =>
        fixed[i] ? minimumWidth : (duration / freeDuration) * freeWidth
      )
    }
  }
  return durations.map(() => minimumWidth)
}

/** 首尾相接的段在横轴上的位置，以及把时刻换算成横坐标的函数。 */
export function layoutSegments(
  spans: { start: number; end: number }[],
  width: number,
  minWidth: number
): {
  boxes: { x: number; width: number }[]
  toX: (time: number) => number
} {
  const widths = allocateWidths(
    spans.map((span) => Math.max(span.end - span.start, 0)),
    width,
    minWidth
  )
  let x = 0
  const boxes = widths.map((boxWidth) => {
    const box = { x, width: boxWidth }
    x += boxWidth
    return box
  })
  const right = x
  // 时刻按所在段的宽度线性换算，段边界与刻度严格对齐
  const toX = (time: number) => {
    const i = spans.findIndex((span) => time <= span.end)
    if (i === -1) return right
    const length = spans[i].end - spans[i].start
    const ratio = length > 0 ? (time - spans[i].start) / length : 0
    return boxes[i].x + Math.min(Math.max(ratio, 0), 1) * boxes[i].width
  }
  return { boxes, toX }
}

export interface LabelCandidate {
  /** 锚点横坐标 */
  x: number
  width: number
  /** 越小越先放 */
  rank: number
}

/**
 * 按优先级放标签：先在每一行试以锚点居中，都放不下才在各行试紧贴已有标签的两侧
 * （偏离锚点不超过 shift），尽量少画斜着的长引线。
 * 所有行都放不下的标签直接省略，对应数值在图例或表格里仍能读到。
 */
export function placeLabels<T extends LabelCandidate>(
  items: T[],
  width: number,
  options: { rows: number; gap: number; shift?: number }
): (T & { left: number; row: number })[] {
  const shift = options.shift ?? 0
  const rows: [number, number][][] = Array.from(
    { length: options.rows },
    () => []
  )
  const placed: (T & { left: number; row: number })[] = []
  for (const item of [...items].sort((a, b) => a.rank - b.rank)) {
    const ideal = Math.min(
      Math.max(item.x - item.width / 2, 0),
      Math.max(width - item.width, 0)
    )
    let spot: { left: number; row: number } | undefined
    for (const limit of shift > 0 ? [0, shift] : [0]) {
      for (const [row, spans] of rows.entries()) {
        const left = [
          ideal,
          ...spans.flatMap(([l, r]) => [
            r + options.gap,
            l - options.gap - item.width,
          ]),
        ]
          .filter(
            (candidate) =>
              candidate >= 0 &&
              candidate + item.width <= width + 0.5 &&
              Math.abs(candidate - ideal) <= limit
          )
          .sort((a, b) => Math.abs(a - ideal) - Math.abs(b - ideal))
          .find((candidate) =>
            spans.every(
              ([l, r]) =>
                candidate + item.width + options.gap <= l ||
                candidate >= r + options.gap
            )
          )
        if (left !== undefined) {
          spot = { left, row }
          break
        }
      }
      if (spot) break
    }
    if (!spot) continue
    rows[spot.row].push([spot.left, spot.left + item.width])
    placed.push({ ...item, ...spot })
  }
  return placed
}

/** 整数商余保留全部微秒，不经过浮点舍入。 */
export function formatTraceDuration(us: number | null): string {
  if (us === null || !Number.isSafeInteger(us) || us < 0) {
    return '—'
  }
  return `${Math.trunc(us / 1000)}.${String(us % 1000).padStart(3, '0')} ms`
}

/** 占总耗时的比例，保留一位小数；非零但不足 0.1% 时写作 <0.1%。 */
export function formatTraceShare(part: number, total: number): string {
  const percent = total > 0 ? (part / total) * 100 : 0
  if (percent > 0 && percent < 0.1) return '<0.1%'
  return `${percent.toFixed(1)}%`
}

export function formatTraceBytes(bytes: number | null): string {
  if (bytes === null) return '—'
  return `${bytes} B`
}

export function formatTraceTimestamp(us: number): string {
  const date = new Date(Math.trunc(us / 1000))
  return `${date.toISOString().slice(0, 19).replace('T', ' ')}.${String(us % 1_000_000).padStart(6, '0')} UTC`
}
