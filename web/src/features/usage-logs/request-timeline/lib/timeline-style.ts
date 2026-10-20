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
import type { TimelineKind } from './timeline-model'
import type { SegmentKind } from './timeline-segments'

/**
 * 段颜色写进 --segment，色块、色标和段内文字共用同一个值。
 * 等待输出用固定紫色：主题预设会改写 chart 色，无法保证与相邻的橙、绿区分。
 */
export const SEGMENT_FILL: Record<SegmentKind, string> = {
  gateway: '[--segment:var(--info)]',
  wait: '[--segment:var(--warning)]',
  think:
    '[--segment:var(--color-violet-600)] dark:[--segment:var(--color-violet-400)]',
  flow: '[--segment:var(--success)]',
  failed: '[--segment:var(--muted-foreground)]',
}

/** 段内文字按底色亮度取黑或白（CSS 相对颜色），各主题预设下都保持可读。 */
export const SEGMENT_INK =
  'text-[color:oklch(from_var(--segment)_clamp(0,(0.56_-_l)*1000,1)_0_0)]'

export const TIMELINE_KIND_BG: Record<TimelineKind, string> = {
  gateway: 'bg-info',
  upstream: 'bg-warning',
  response: 'bg-success',
  log: 'bg-muted-foreground/40',
  failed: 'bg-destructive',
}

export const HATCH =
  'bg-[image:repeating-linear-gradient(135deg,transparent_0_3px,rgb(255_255_255/0.45)_3px_5px)]'
