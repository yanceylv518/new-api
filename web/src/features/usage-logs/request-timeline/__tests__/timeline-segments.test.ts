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
import { describe, expect, test } from 'vitest'

import type { RequestTimelineData, RequestTimelineResult } from '../api'
import { buildTimelineGroups, flattenTimeline } from '../lib/timeline-model'
import {
  buildRequestSegments,
  buildSegmentSteps,
  type RequestSegment,
} from '../lib/timeline-segments'
import { retriedStreamResult, singleStreamResult } from './fixtures'

// 返回翻译键本身，断言与界面语言无关。
const t = ((key: string) => key) as unknown as TFunction

type SegmentInput = Parameters<typeof buildRequestSegments>[2]

function timelineOf(result: RequestTimelineResult): RequestTimelineData {
  const data = result.detail?.timeline
  if (!data) throw new Error('fixture must include a timeline')
  return data
}

function cut(
  result: RequestTimelineResult,
  overrides: Partial<SegmentInput> = {}
) {
  const summary = result.summary
  if (!summary || !result.detail) {
    throw new Error('fixture must include details')
  }
  return buildRequestSegments(t, timelineOf(result), {
    total: summary.duration_us,
    firstByte: result.detail.final_attempt_first_byte_offset_us,
    firstWrite: summary.first_payload_write_offset_us,
    stream: summary.is_stream === true,
    ...overrides,
  })
}

const spans = (segments: RequestSegment[]) =>
  segments.map((segment) => [segment.key, segment.start, segment.end])

function stepsOf(result: RequestTimelineResult, key: string) {
  const data = timelineOf(result)
  const total = result.summary?.duration_us ?? 0
  const groups = buildTimelineGroups(t, data, total)
  const index = new Map(
    flattenTimeline(groups, new Set()).map(({ node }) => [node.id, node])
  )
  const segment = cut(result).segments.find((item) => item.key === key)
  if (!segment) throw new Error(`fixture has no ${key} segment`)
  return buildSegmentSteps(segment, data, index, total)
}

describe('request timeline segments', () => {
  test('a streamed request is cut at the call start, first response, first output and last write', () => {
    const { segments } = cut(singleStreamResult())

    expect(spans(segments)).toEqual([
      ['prepare', 0, 14_592],
      ['wait', 14_592, 1_774_477],
      ['think', 1_774_477, 2_927_720],
      ['output', 2_927_720, 4_688_913],
      ['finish', 4_688_913, 4_701_926],
    ])
  })

  test('a failed attempt runs until the next attempt starts so channel reselection stays inside it', () => {
    const { segments, moments } = cut(retriedStreamResult())

    expect(spans(segments)).toEqual([
      ['prepare', 0, 16_950],
      ['attempt-1', 16_950, 320_700],
      ['wait', 320_700, 1_305_800],
      ['think', 1_305_800, 1_322_700],
      ['output', 1_322_700, 7_402_100],
      ['finish', 7_402_100, 7_419_300],
    ])
    expect(segments[1].kind).toBe('failed')
    expect(
      moments.find((moment) => moment.key === 'attempt-1-end')
    ).toMatchObject({ at: 316_600, failed: true })
    expect(moments.find((moment) => moment.key === 'first-byte')?.label).toBe(
      'Final response received'
    )
  })

  test('a non-stream response is received until its last write and only the work after it is finishing', () => {
    const { segments } = cut(singleStreamResult(), { stream: false })

    expect(spans(segments)).toEqual([
      ['prepare', 0, 14_592],
      ['wait', 14_592, 1_774_477],
      ['receive', 1_774_477, 4_688_913],
      ['finish', 4_688_913, 4_701_926],
    ])
  })

  test('when the last attempt also fails it ends at its observed end and the rest is finishing work', () => {
    const result = retriedStreamResult()
    const data = timelineOf(result)
    data.attempts[1] = {
      ...data.attempts[1],
      outcome: 'failed',
      end_us: 2_000_000,
    }

    expect(spans(cut(result).segments)).toEqual([
      ['prepare', 0, 16_950],
      ['attempt-1', 16_950, 320_700],
      ['attempt-2', 320_700, 2_000_000],
      ['finish', 2_000_000, 7_419_300],
    ])
  })

  test('an attempt without an observed end runs to the end of the request and nothing is guessed after it', () => {
    const result = retriedStreamResult()
    const data = timelineOf(result)
    data.attempts[1] = {
      ...data.attempts[1],
      outcome: 'canceled',
      end_us: null,
    }

    const { segments } = cut(result)

    expect(segments.at(-1)).toMatchObject({
      key: 'attempt-2',
      start: 320_700,
      end: null,
    })
    expect(segments.map((segment) => segment.key)).not.toContain('finish')
  })

  test('without an upstream first response the wait for response and for output form one segment', () => {
    const { segments, moments } = cut(singleStreamResult(), { firstByte: null })

    expect(spans(segments)).toEqual([
      ['prepare', 0, 14_592],
      ['wait', 14_592, 2_927_720],
      ['output', 2_927_720, 4_688_913],
      ['finish', 4_688_913, 4_701_926],
    ])
    expect(segments[1].label).toBe('Wait for output')
    expect(moments.map((moment) => moment.key)).not.toContain('first-byte')
  })

  test('a request rejected before any attempt is a single processing segment', () => {
    const result = singleStreamResult()
    timelineOf(result).attempts = []

    const { segments } = cut(result, { firstByte: null, firstWrite: 900 })

    expect(spans(segments)).toEqual([['process', 0, 4_701_926]])
  })
})

describe('request timeline segment steps', () => {
  test('preparation expands into the outermost gateway stages', () => {
    const steps = stepsOf(singleStreamResult(), 'prepare')

    expect(steps.map((step) => step.label)).toEqual([
      'Verify identity',
      'Check limits',
      'Choose channel',
      'Check request',
      'Calculate cost',
      'Reserve quota',
    ])
  })

  test('waiting for the response expands into request conversion and the upstream connection, sending and processing', () => {
    const steps = stepsOf(singleStreamResult(), 'wait')

    expect(steps.map((step) => [step.label, step.start, step.end])).toEqual([
      ['Convert request', 14_700, 15_820],
      ['Existing connection', 15_950, 16_080],
      ['Send request', 16_080, 22_700],
      ['Upstream processing', 22_700, 1_774_477],
    ])
  })

  test('a failed attempt expands into its upstream steps and the channel reselection after it', () => {
    const steps = stepsOf(retriedStreamResult(), 'attempt-1')

    expect(steps.map((step) => step.label)).toEqual([
      'Existing connection',
      'Send request',
      'Upstream processing',
      'Receive response',
      'Choose channel',
    ])
    expect(
      steps.find((step) => step.label === 'Upstream processing')?.kind
    ).toBe('failed')
  })

  test('finishing work drills into the only stage that fills it', () => {
    const steps = stepsOf(singleStreamResult(), 'finish')

    expect(steps.map((step) => step.label)).toEqual([
      'Finish billing',
      'Save usage log',
    ])
  })

  test('waiting for output happens inside the upstream and has no finer steps', () => {
    expect(stepsOf(singleStreamResult(), 'think')).toEqual([])
  })
})
