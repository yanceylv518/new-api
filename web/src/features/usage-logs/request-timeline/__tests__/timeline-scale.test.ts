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
  allocateWidths,
  formatTraceDuration,
  formatTraceShare,
  layoutSegments,
  placeLabels,
} from '../lib/timeline-scale'

describe('request timeline widths', () => {
  test('widths stay proportional when every segment is wide enough', () => {
    expect(allocateWidths([100, 300], 400, 6)).toEqual([100, 300])
  })

  test('a short segment gets the minimum width and the others share the rest in proportion', () => {
    const widths = allocateWidths([1, 600, 399], 1000, 10)

    expect(widths[0]).toBe(10)
    expect(widths[1] / widths[2]).toBeCloseTo(600 / 399)
    expect(widths.reduce((sum, width) => sum + width, 0)).toBeCloseTo(1000)
  })

  test('without a minimum width the layout is purely proportional', () => {
    expect(allocateWidths([1, 99], 100, 0)).toEqual([1, 99])
  })
})

describe('request timeline positions', () => {
  test('a moment on a segment boundary lands exactly on the box edge', () => {
    const { boxes, toX } = layoutSegments(
      [
        { start: 0, end: 10 },
        { start: 10, end: 1000 },
      ],
      500,
      20
    )

    expect(toX(10)).toBe(boxes[1].x)
    expect(toX(505)).toBeCloseTo(boxes[1].x + boxes[1].width / 2)
    expect(toX(1000)).toBe(500)
  })

  test('moments outside the request are clamped to the track', () => {
    const { toX } = layoutSegments([{ start: 0, end: 100 }], 300, 0)

    expect(toX(-5)).toBe(0)
    expect(toX(250)).toBe(300)
  })
})

describe('request timeline labels', () => {
  test('a colliding label moves to the next row and one that fits nowhere is left out', () => {
    const placed = placeLabels(
      [
        { id: 'a', x: 50, width: 80, rank: 0 },
        { id: 'b', x: 60, width: 80, rank: 1 },
        { id: 'c', x: 70, width: 80, rank: 2 },
      ],
      400,
      { rows: 2, gap: 8 }
    )

    expect(placed.map((label) => [label.id, label.row])).toEqual([
      ['a', 0],
      ['b', 1],
    ])
  })

  test('the label with the higher priority keeps the contested spot', () => {
    const placed = placeLabels(
      [
        { id: 'minor', x: 50, width: 80, rank: 5 },
        { id: 'major', x: 60, width: 80, rank: 0 },
      ],
      400,
      { rows: 1, gap: 8 }
    )

    expect(placed.map((label) => label.id)).toEqual(['major'])
  })

  test('a label near the end of the track stays inside it', () => {
    const [label] = placeLabels([{ x: 395, width: 80, rank: 0 }], 400, {
      rows: 1,
      gap: 8,
    })

    expect(label.left).toBe(320)
  })

  test('a label keeps its anchor position on the next row before shifting along the first row', () => {
    const placed = placeLabels(
      [
        { id: 'a', x: 40, width: 80, rank: 0 },
        { id: 'b', x: 60, width: 80, rank: 1 },
      ],
      400,
      { rows: 2, gap: 8, shift: 100 }
    )

    expect(placed.find((label) => label.id === 'b')).toMatchObject({
      row: 1,
      left: 20,
    })
  })

  test('when no row has room at its anchor a label shifts beside its neighbour', () => {
    const placed = placeLabels(
      [
        { id: 'a', x: 40, width: 80, rank: 0 },
        { id: 'b', x: 60, width: 80, rank: 1 },
      ],
      400,
      { rows: 1, gap: 8, shift: 100 }
    )

    expect(placed.find((label) => label.id === 'b')).toMatchObject({
      row: 0,
      left: 88,
    })
  })
})

describe('request timeline formatting', () => {
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

  test.each([
    [0, 1000, '0.0%'],
    [1, 10_000, '<0.1%'],
    [374, 1000, '37.4%'],
    [1000, 1000, '100.0%'],
  ])('formats %s of %s as %s', (part, total, expected) => {
    expect(formatTraceShare(part, total)).toBe(expected)
  })
})
