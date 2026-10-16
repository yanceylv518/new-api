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

import type { RequestTimelineData, RequestTimelineResult } from '../api'
import { getStageText } from './timeline-text'

export interface TraceEventRow {
  id: string
  label: string
  code: string
  start: number
  end: number | null
  nodeId?: string
  failed?: boolean
}

/** 只列出已有观测点，缺失的结束、取消或写出时间不补零、不推算。 */
export function buildTraceEvents(
  t: TFunction,
  data: RequestTimelineData,
  summary: NonNullable<RequestTimelineResult['summary']>
): TraceEventRow[] {
  const stageText = getStageText(t)
  const rows: TraceEventRow[] = [
    {
      id: 'entry',
      label: t('Request entry'),
      code: 'entry',
      start: 0,
      end: null,
    },
    {
      id: 'return',
      label: t('Server processing ended'),
      code: 'handler_return',
      start: summary.duration_us,
      end: null,
    },
  ]
  data.phases.forEach((phase, index) => {
    const label = stageText[phase.name]?.label ?? phase.name
    const nodeId = `phase-${index}`
    rows.push({
      id: `${nodeId}-start`,
      label: `${label} · ${t('Start')}`,
      code: phase.name,
      start: phase.start_us,
      end: null,
      nodeId,
    })
    if (phase.end_us !== null) {
      rows.push({
        id: `${nodeId}-end`,
        label: `${label} · ${t('End')}`,
        code: phase.name,
        start: phase.end_us,
        end: null,
        nodeId,
      })
    }
  })
  for (const attempt of data.attempts) {
    const nodeId = `attempt-${attempt.index}`
    const attemptLabel = t('Attempt {{index}} · Channel {{channel}}', {
      index: attempt.index,
      channel: `#${attempt.channel_id}`,
    })
    rows.push({
      id: `${nodeId}-start`,
      label: `${attemptLabel} · ${t('Start')}`,
      code: nodeId,
      start: attempt.start_us,
      end: null,
      nodeId,
    })
    if (attempt.end_us !== null) {
      rows.push({
        id: `${nodeId}-end`,
        label: `${attemptLabel} · ${t('End')}`,
        code: nodeId,
        start: attempt.end_us,
        end: null,
        nodeId,
        failed: attempt.outcome !== 'success',
      })
    }
    for (const exchange of attempt.exchanges ?? []) {
      const exchangeId = `exchange-${attempt.index}-${exchange.index}`
      const code = `${nodeId} / HTTP ${exchange.index}`
      rows.push({
        id: `${exchangeId}-start`,
        label: t('HTTP request started'),
        code,
        start: exchange.start_us,
        end: null,
        nodeId: exchangeId,
      })
      if (exchange.headers_us !== null) {
        rows.push({
          id: `${exchangeId}-headers`,
          label: t('Response headers observed'),
          code,
          start: exchange.headers_us,
          end: null,
          nodeId: exchangeId,
        })
      }
      if (exchange.end_us !== null) {
        rows.push({
          id: `${exchangeId}-end`,
          label: t('Response body ended'),
          code: `${code} / ${exchange.end_reason ?? ''}`,
          start: exchange.end_us,
          end: null,
          nodeId: exchangeId,
        })
      }
      for (const event of exchange.events ?? []) {
        rows.push({
          id: `${exchangeId}-event-${event.sequence}`,
          label: event.name,
          code: `${code} / #${event.sequence}`,
          start: event.offset_us,
          end: null,
          nodeId: exchangeId,
          failed: event.failed,
        })
      }
      if (
        exchange.first_byte_us !== null &&
        !exchange.events?.some(
          (event) =>
            event.name === 'first_byte' &&
            event.offset_us === exchange.first_byte_us
        )
      ) {
        rows.push({
          id: `${exchangeId}-first-byte`,
          label: t('Upstream first byte'),
          code: `${code} / first_byte_us`,
          start: exchange.first_byte_us,
          end: null,
          nodeId: exchangeId,
        })
      }
    }
    if (attempt.stream?.first_us != null) {
      rows.push({
        id: `${nodeId}-stream-first`,
        label: t('First observed data line'),
        code: nodeId,
        start: attempt.stream.first_us,
        end: null,
        nodeId,
      })
    }
    if (attempt.stream?.last_us != null) {
      rows.push({
        id: `${nodeId}-stream-last`,
        label: t('Last observed data line'),
        code: nodeId,
        start: attempt.stream.last_us,
        end: null,
        nodeId,
      })
    }
  }
  if (summary.first_payload_write_offset_us !== null) {
    rows.push({
      id: 'first-write',
      label: t('First payload write'),
      code: 'response / first_payload_write_offset_us',
      start: summary.first_payload_write_offset_us,
      end: null,
    })
  }
  if (data.response.last_write_us !== null) {
    rows.push({
      id: 'last-write',
      label: t('Last response write'),
      code: 'response / last_write_us',
      start: data.response.last_write_us,
      end: null,
    })
  }
  if (data.response.cancel_observed_us !== null) {
    rows.push({
      id: 'cancel',
      label: t('Cancellation observed'),
      code: 'response / cancel_observed_us',
      start: data.response.cancel_observed_us,
      end: null,
      failed: true,
    })
  }
  data.errors.forEach((error, index) => {
    rows.push({
      id: `error-${index}`,
      label: error.code,
      code: error.stage,
      start: error.offset_us,
      end: null,
      failed: true,
    })
  })
  return rows.sort((a, b) => a.start - b.start)
}
