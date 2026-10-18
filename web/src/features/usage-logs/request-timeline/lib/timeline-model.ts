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

import type { RequestTimelineData } from '../api'
import { getOutcomeLabel, getStageText } from './timeline-text'

export type TraceAttempt = RequestTimelineData['attempts'][number]
export type TraceExchange = NonNullable<TraceAttempt['exchanges']>[number]
export type TimelineKind =
  | 'gateway'
  | 'upstream'
  | 'response'
  | 'log'
  | 'failed'
export type TimelineSpan = [number, number]

export interface TimelineNode {
  id: string
  label: string
  description: string
  start: number
  /** null 表示没有观测到结束，不能当作零或推算。 */
  end: number | null
  kind: TimelineKind
  hatched?: boolean
  stage?: string
  derived?: boolean
  attempt?: TraceAttempt
  exchange?: TraceExchange
  network?: Record<'dns' | 'tcp' | 'tls', TimelineSpan | null>
  status?: { label: string; failed: boolean }
  children: TimelineNode[]
}

const STAGE_KIND: Record<string, TimelineKind> = {
  response_handle: 'response',
  settlement: 'log',
  billing_settle: 'log',
  usage_finalize: 'log',
  consume_log: 'log',
  error_log: 'log',
  log_insert: 'log',
}

function exchangeNode(
  t: TFunction,
  exchange: TraceExchange,
  attempt: TraceAttempt
): TimelineNode {
  const eventAt = (name: string) => {
    const matches = exchange.events?.filter((event) => event.name === name)
    return matches?.length === 1 ? matches[0].offset_us : null
  }
  const span = (from: string, to: string): TimelineSpan | null => {
    const start = eventAt(from)
    const end = eventAt(to)
    return start !== null && end !== null && end >= start ? [start, end] : null
  }
  const network = {
    dns: span('dns_start', 'dns_done'),
    tcp: span('connect_start', 'connect_done'),
    tls: span('tls_start', 'tls_done'),
  }
  const gotConn = eventAt('got_conn')
  const wrote = eventAt('wrote_request')
  const headers = exchange.headers_us
  const failed =
    (exchange.status ?? 0) >= 400 ||
    exchange.end_reason === 'request_error' ||
    exchange.end_reason === 'request_timeout' ||
    exchange.end_reason === 'request_canceled' ||
    exchange.end_reason === 'read_timeout' ||
    exchange.end_reason === 'read_canceled' ||
    exchange.end_reason === 'read_error'
  const id = `exchange-${attempt.index}-${exchange.index}`
  const base = { attempt, exchange, network, children: [] }
  let connectionLabel = t('Connection')
  if (exchange.reused === false) connectionLabel = t('Connect · new')
  if (exchange.reused === true) connectionLabel = t('Connect · reused')
  const steps: TimelineNode[] = [
    {
      ...base,
      id: `${id}-connect`,
      label: connectionLabel,
      description: t(
        'Obtains a connection. This may include pool or HTTP/2 stream queueing; through a proxy it describes the proxy connection.'
      ),
      start: eventAt('get_conn') ?? Number.NaN,
      end: gotConn,
      kind: 'upstream',
      hatched: true,
    },
    {
      ...base,
      id: `${id}-send`,
      label: t('Send request'),
      description: t(
        'Writes the request headers and body. Long contexts or images make this noticeably longer.'
      ),
      start: gotConn ?? Number.NaN,
      end: wrote,
      kind: 'upstream',
      hatched: true,
    },
    {
      ...base,
      id: `${id}-wait`,
      label: t('Wait for response'),
      description: t(
        'From the request being written to the response arriving, including network round trips and upstream queueing. The first byte is not the first token.'
      ),
      start: wrote ?? Number.NaN,
      end: headers,
      kind: failed ? 'failed' : 'upstream',
    },
    {
      ...base,
      id: `${id}-body`,
      label: exchange.event_stream
        ? t('Receive stream')
        : t('Receive response'),
      description:
        exchange.end_us === null
          ? t(
              'The response body was never seen closing, so its end is unknown. The dashed line only runs to the end of the request.'
            )
          : t(
              'From the response headers to the body being fully read or closed.'
            ),
      start: headers ?? Number.NaN,
      end: exchange.end_us,
      kind: failed ? 'failed' : 'response',
    },
  ]
  return {
    ...base,
    id,
    label: t('Upstream request · {{protocol}} · {{status}}', {
      protocol: exchange.protocol?.replace(/\.0$/, '') ?? 'HTTP',
      status: exchange.status ?? t('No response'),
    }),
    description: t(
      'One HTTP request sent upstream. Stages may overlap; repeated network events remain in the raw event list.'
    ),
    start: exchange.start_us,
    end: exchange.end_us,
    kind: failed ? 'failed' : 'upstream',
    status: failed ? { label: t('Failed'), failed: true } : undefined,
    // 重复回调无法唯一配对；提前响应也可能先于请求写完，不能据此合成负耗时区间。
    children: steps.filter(
      (step) =>
        Number.isFinite(step.start) &&
        (step.end === null || step.end >= step.start)
    ),
  }
}

/** 按包含关系把同一组内的节点组织成树；上游请求自带子步骤，不再接收其他节点。 */
function nestByContainment(nodes: TimelineNode[], total: number) {
  const endOf = (node: TimelineNode) => node.end ?? total
  const sorted = [...nodes].sort(
    (a, b) => a.start - b.start || endOf(b) - endOf(a)
  )
  const roots: TimelineNode[] = []
  const stack: TimelineNode[] = []
  for (const node of sorted) {
    if (node.end === null) {
      roots.push(node)
      continue
    }
    let parent = stack.at(-1)
    while (
      parent &&
      !(parent.start <= node.start && endOf(node) <= endOf(parent))
    ) {
      stack.pop()
      parent = stack.at(-1)
    }
    if (parent) parent.children.push(node)
    else roots.push(node)
    if (!node.exchange) stack.push(node)
  }
  return roots
}

/**
 * 把扁平的阶段、尝试和上游请求整理成「网关准备 / 尝试 n / 收尾」三类分组。
 * 重试前的渠道选择在上一次尝试结束后发生，attempt 为 0，归到紧随其后的尝试。
 */
export function buildTimelineGroups(
  t: TFunction,
  data: RequestTimelineData,
  total: number
): TimelineNode[] {
  const stageText = getStageText(t)
  const attempts = data.attempts
  const firstAttemptStart = attempts[0]?.start_us ?? null
  const before: TimelineNode[] = []
  const after: TimelineNode[] = []
  const perAttempt = new Map<number, TimelineNode[]>(
    attempts.map((attempt) => [attempt.index, []])
  )
  data.phases.forEach((phase, index) => {
    const node: TimelineNode = {
      id: `phase-${index}`,
      label: stageText[phase.name]?.label ?? phase.name,
      description: stageText[phase.name]?.description ?? '',
      start: phase.start_us,
      end: phase.end_us,
      kind: STAGE_KIND[phase.name] ?? 'gateway',
      stage: phase.name,
      children: [],
    }
    const owned = phase.attempt ? perAttempt.get(phase.attempt) : undefined
    if (owned) {
      owned.push(node)
      return
    }
    if (
      phase.name === 'channel_select' &&
      firstAttemptStart !== null &&
      phase.start_us >= firstAttemptStart
    ) {
      const next = attempts.find(
        (attempt) => attempt.start_us >= (phase.end_us ?? phase.start_us)
      )
      const nextNodes = next ? perAttempt.get(next.index) : undefined
      if (nextNodes) {
        nextNodes.push(node)
        return
      }
    }
    if (firstAttemptStart === null || phase.start_us < firstAttemptStart) {
      before.push(node)
    } else {
      after.push(node)
    }
  })

  const groups: TimelineNode[] = []
  if (before.length > 0) {
    groups.push({
      id: 'gateway',
      label:
        attempts.length > 0
          ? t('Gateway preparation')
          : t('Gateway processing'),
      description: t(
        'Gateway preparation before the first channel call. Nested stages overlap and must not be added together.'
      ),
      start: 0,
      end: before.some((node) => node.end === null)
        ? null
        : Math.max(...before.map((node) => node.end ?? 0)),
      kind: 'gateway',
      derived: true,
      children: nestByContainment(before, total),
    })
  }
  for (const attempt of attempts) {
    const nodes = [
      ...(perAttempt.get(attempt.index) ?? []),
      ...(attempt.exchanges ?? []).map((exchange) =>
        exchangeNode(t, exchange, attempt)
      ),
    ]
    const succeeded = attempt.outcome === 'success'
    groups.push({
      id: `attempt-${attempt.index}`,
      label: t('Attempt {{index}} · Channel {{channel}}', {
        index: attempt.index,
        channel: `#${attempt.channel_id}`,
      }),
      description: succeeded
        ? t('This attempt produced the final result.')
        : t(
            'This attempt failed. If another attempt follows, the gateway retried on a new channel.'
          ),
      start: Math.min(attempt.start_us, ...nodes.map((node) => node.start)),
      end: attempt.end_us,
      kind: succeeded ? 'gateway' : 'failed',
      derived: true,
      attempt,
      status: {
        label: getOutcomeLabel(t, attempt.outcome),
        failed: !succeeded,
      },
      children: nestByContainment(nodes, total),
    })
  }
  if (after.length > 0) {
    groups.push({
      id: 'wrap-up',
      label: t('Wrap-up'),
      description: t('Work done after every attempt ended.'),
      start: Math.min(...after.map((node) => node.start)),
      end: after.some((node) => node.end === null)
        ? null
        : Math.max(...after.map((node) => node.end ?? 0)),
      kind: 'log',
      derived: true,
      children: nestByContainment(after, total),
    })
  }
  return groups
}

/** 首次尝试的直接观测起点，首次选渠属于之前的网关准备。 */
export function getChannelStart(data: RequestTimelineData): number | null {
  return data.attempts[0]?.start_us ?? null
}

export function flattenTimeline(
  groups: TimelineNode[],
  collapsed: ReadonlySet<string>
): { node: TimelineNode; depth: number }[] {
  const rows: { node: TimelineNode; depth: number }[] = []
  const walk = (node: TimelineNode, depth: number) => {
    rows.push({ node, depth })
    if (collapsed.has(node.id)) return
    for (const child of node.children) walk(child, depth + 1)
  }
  for (const group of groups) walk(group, 0)
  return rows
}

export function findTimelineNode(
  nodes: TimelineNode[],
  id: string | null
): TimelineNode | null {
  for (const node of nodes) {
    if (node.id === id) return node
    const found = findTimelineNode(node.children, id)
    if (found) return found
  }
  return null
}
