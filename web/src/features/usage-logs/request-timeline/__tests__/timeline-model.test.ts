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

import type { RequestTimelineData } from '../api'
import {
  buildTimelineGroups,
  findTimelineNode,
  flattenTimeline,
  getChannelStart,
  type TimelineNode,
} from '../lib/timeline-model'
import { retriedStreamResult } from './fixtures'

// 返回翻译键本身，断言与界面语言无关。
const t = ((key: string) => key) as unknown as TFunction

function retriedData(): RequestTimelineData {
  const data = retriedStreamResult().detail?.timeline
  if (!data) throw new Error('fixture must include a timeline')
  return data
}

const stagesOf = (node: TimelineNode | null) =>
  node?.children.map((child) => child.stage ?? child.id)

describe('request timeline grouping', () => {
  test('initial channel selection stays in gateway preparation and retry selection belongs to the next group', () => {
    const groups = buildTimelineGroups(t, retriedData(), 7_419_300)

    expect(groups.map((group) => group.id)).toEqual([
      'gateway',
      'attempt-1',
      'attempt-2',
    ])
    expect(stagesOf(groups[1])).toEqual(['channel_handler'])
    expect(groups[2].start).toBe(316_900)
    expect(groups[2].children[0].stage).toBe('channel_select')
  })

  test('stages contained in another stage are nested under it instead of listed side by side', () => {
    const groups = buildTimelineGroups(t, retriedData(), 7_419_300)

    expect(stagesOf(groups[0])).toEqual([
      'auth',
      'distribute',
      'precharge',
      'channel_select',
    ])
    const distribute = groups[0].children[1]
    expect(stagesOf(distribute)).toEqual(['request_body'])
    const handler = groups[2].children[1]
    expect(stagesOf(handler)).toEqual([
      'exchange-2-1',
      'response_handle',
      'settlement',
    ])
    const settlement = handler.children[2]
    expect(stagesOf(settlement)).toEqual(['consume_log'])
    expect(stagesOf(settlement.children[0])).toEqual(['log_insert'])
  })

  test('an upstream request is split into connect, send, wait and receive steps that follow each other', () => {
    const groups = buildTimelineGroups(t, retriedData(), 7_419_300)
    const exchange = findTimelineNode(groups, 'exchange-2-1')
    const steps = exchange?.children.map((step) => [step.start, step.end])

    expect(steps).toEqual([
      [322_450, 344_700],
      [344_700, 350_000],
      [350_000, 1_306_100],
      [1_306_100, 7_401_600],
    ])
    expect(exchange?.children[0].network).toEqual({
      dns: [322_600, 324_900],
      tcp: [325_000, 331_200],
      tls: [331_300, 344_600],
    })
  })

  test('a failed upstream status marks the request and its wait step as failed', () => {
    const groups = buildTimelineGroups(t, retriedData(), 7_419_300)
    const exchange = findTimelineNode(groups, 'exchange-1-1')

    expect(exchange?.status).toEqual({ label: 'Failed', failed: true })
    expect(findTimelineNode(groups, 'exchange-1-1-wait')?.kind).toBe('failed')
    expect(groups[1].status).toEqual({ label: 'Failed', failed: true })
  })

  test('an unfinished stage keeps a null end instead of borrowing the request duration', () => {
    const data = retriedData()
    data.phases[8] = { ...data.phases[8], end_us: null }
    const groups = buildTimelineGroups(t, data, 7_419_300)

    const responseHandle = findTimelineNode(groups, 'phase-8')
    expect(responseHandle?.end).toBeNull()
  })

  test('a request rejected before any attempt has one gateway processing group', () => {
    const data: RequestTimelineData = {
      ...retriedData(),
      phases: [{ name: 'auth', start_us: 100, end_us: 5800 }],
      attempts: [],
    }
    const groups = buildTimelineGroups(t, data, 8900)

    expect(groups.map((group) => group.label)).toEqual(['Process request'])
    expect(getChannelStart(data)).toBeNull()
  })
})

describe('request timeline rows', () => {
  test('collapsing a group hides its descendants but keeps the group row', () => {
    const groups = buildTimelineGroups(t, retriedData(), 7_419_300)
    const all = flattenTimeline(groups, new Set())
    const collapsed = flattenTimeline(groups, new Set(['attempt-2']))

    const ids = collapsed.map((row) => row.node.id)
    expect(ids).toContain('attempt-2')
    expect(ids).not.toContain('exchange-2-1')
    expect(collapsed.length).toBeLessThan(all.length)
    expect(all.find((row) => row.node.id === 'exchange-2-1-wait')?.depth).toBe(
      3
    )
  })

  test('the first attempt starts at its observed boundary after gateway preparation', () => {
    expect(getChannelStart(retriedData())).toBe(16_950)
  })
})
