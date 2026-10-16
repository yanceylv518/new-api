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

export type TimelineSegmentKey = 'whole'

export interface TimelineSegment {
  key: TimelineSegmentKey
  start: number
  end: number
  offset: number
  width: number
}

/** 全图使用同一线性刻度，聚焦只改变观察窗口。 */
export function buildTimelineSegments(
  total: number,
  window?: { start: number; end: number } | null
): TimelineSegment[] {
  const start = window?.start ?? 0
  const end = window?.end ?? total
  return [
    { key: 'whole', start, end: Math.max(end, start), offset: 0, width: 100 },
  ]
}

export function timelinePercent(
  segments: TimelineSegment[],
  value: number
): number {
  const segment = segments[0]
  const ratio =
    (value - segment.start) / Math.max(segment.end - segment.start, 1)
  return Math.min(Math.max(ratio, 0), 1) * 100
}

/** 整数商余数保留全部微秒，不经过浮点舍入。 */
export function formatTraceDuration(us: number | null): string {
  if (us === null || !Number.isSafeInteger(us) || us < 0) {
    return '—'
  }
  return `${Math.trunc(us / 1000)}.${String(us % 1000).padStart(3, '0')} ms`
}

export function formatTraceBytes(bytes: number): string {
  return `${bytes} B`
}

export function formatTraceTimestamp(us: number): string {
  const date = new Date(Math.trunc(us / 1000))
  return `${date.toISOString().slice(0, 19).replace('T', ' ')}.${String(us % 1_000_000).padStart(6, '0')} UTC`
}
