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
  buildTimelineSegments,
  formatTraceDuration,
  timelinePercent,
} from '../lib/timeline-scale'

describe('request timeline scale', () => {
  test('a streamed request uses a single linear time axis', () => {
    const segments = buildTimelineSegments(9_800_000)

    expect(
      segments.map((s) => [s.key, s.start, s.end, s.offset, s.width])
    ).toEqual([['whole', 0, 9_800_000, 0, 100]])
  })

  test('short stages preserve their real proportion on the linear axis', () => {
    const segments = buildTimelineSegments(9_800_000)

    expect(timelinePercent(segments, 8000)).toBe((8000 / 9_800_000) * 100)
    expect(timelinePercent(segments, 16_000)).toBe((16_000 / 9_800_000) * 100)
    expect(timelinePercent(segments, 9_800_000)).toBe(100)
  })

  test('a focused stage uses one linear scale within its observed bounds', () => {
    const segments = buildTimelineSegments(300_000, {
      start: 16_000,
      end: 24_000,
    })

    expect(segments.map((s) => [s.key, s.width])).toEqual([['whole', 100]])
  })

  test('a request without any attempt uses one linear section', () => {
    const segments = buildTimelineSegments(8900)

    expect(segments.map((s) => s.key)).toEqual(['whole'])
    expect(timelinePercent(segments, 4450)).toBe(50)
  })

  test('values outside the request are clamped to the track', () => {
    const segments = buildTimelineSegments(1000)

    expect(timelinePercent(segments, -5)).toBe(0)
    expect(timelinePercent(segments, 5000)).toBe(100)
  })

  test.each([
    [null, '—'],
    [400, '0.400 ms'],
    [3_400, '3.400 ms'],
    [412_900, '412.900 ms'],
    [9_815_600, '9815.600 ms'],
    [18_420_000, '18420.000 ms'],
  ])('formats %s µs as %s', (value, expected) => {
    expect(formatTraceDuration(value)).toBe(expected)
  })
})
