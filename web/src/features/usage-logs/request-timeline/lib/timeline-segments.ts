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
import { getAttemptResponseOffset, type TraceAttempt } from './timeline-model'
import { getOutcomeLabel } from './timeline-text'

export type SegmentKind = 'gateway' | 'wait' | 'think' | 'flow' | 'failed'

/** 按关键时刻切出的一段；各段首尾相接，合计等于服务端处理耗时。 */
export interface RequestSegment {
  key: string
  kind: SegmentKind
  label: string
  start: number
  /** null 表示结束没有观测到：这段画到请求结束，不推算耗时，之后也不再切段。 */
  end: number | null
  attempt?: TraceAttempt
}

/** 刻度上的关键时刻；空间不够时按 rank 从小到大保留标签。 */
export interface RequestMoment {
  key: string
  label: string
  at: number
  rank: number
  major: boolean
  failed: boolean
}

interface SegmentInput {
  total: number
  firstByte: number | null
  firstWrite: number | null
  stream: boolean
}

function failedAttemptLabel(t: TFunction, attempt: TraceAttempt): string {
  return t('Attempt {{index}}: {{result}}', {
    index: attempt.index,
    result: getOutcomeLabel(t, attempt.outcome),
  })
}

/**
 * 用已采集的关键时刻把请求切成首尾相接的几段：准备请求、失败的调用、等待响应、
 * 等待输出、输出和完成处理。缺失的时刻不补零，相邻两段合并成一段。
 */
export function buildRequestSegments(
  t: TFunction,
  data: RequestTimelineData,
  input: SegmentInput
): { segments: RequestSegment[]; moments: RequestMoment[] } {
  const attempts = data.attempts
  const final = attempts.at(-1)
  const succeeded = final?.outcome === 'success'
  const fourPoints = (data.measurement_version ?? 1) >= 3
  const segments: RequestSegment[] = []
  let cursor: number | null = 0
  const push = (segment: Omit<RequestSegment, 'start'>) => {
    if (cursor === null) return
    const start = cursor
    if (segment.end === null) {
      segments.push({ ...segment, start, end: null })
      cursor = null
      return
    }
    const end = Math.min(Math.max(segment.end, start), input.total)
    if (end > start) segments.push({ ...segment, start, end })
    cursor = end
  }
  const failed = (attempt: TraceAttempt, end: number | null) =>
    push({
      key: `attempt-${attempt.index}`,
      kind: 'failed',
      label: failedAttemptLabel(t, attempt),
      end,
      attempt,
    })

  if (!final) {
    push({
      key: 'process',
      kind: 'gateway',
      label: t('Process request'),
      end:
        fourPoints &&
        data.capture.missing.includes('forwarding_end_not_observed')
          ? null
          : input.total,
    })
  } else {
    push({
      key: 'prepare',
      kind: 'gateway',
      label: t('Prepare request'),
      end: attempts[0].start_us,
    })
    // 失败的调用连同之后重新选择渠道的时间，一直算到下一次调用开始
    attempts.slice(0, -1).forEach((attempt, index) => {
      failed(attempt, attempts[index + 1].start_us)
    })
    if (succeeded) {
      const firstByte = input.firstByte
      // 首次输出属于整条请求，不能用前一次调用的输出划分最后一次调用。
      const firstWrite =
        input.firstWrite !== null &&
        input.firstWrite >= final.start_us &&
        input.firstWrite <= (final.end_us ?? input.total) &&
        (firstByte === null || input.firstWrite >= firstByte)
          ? input.firstWrite
          : null
      if (firstByte !== null) {
        push({
          key: 'wait',
          kind: 'wait',
          label: t('Wait for response'),
          end: firstByte,
          attempt: final,
        })
      }
      if (firstWrite === null) {
        // 缺少首次写出记录时，四点口径合并接收和转发，旧口径保留接收阶段。
        if (firstByte === null) {
          push({
            key: 'call',
            kind: 'wait',
            label: t('Call model'),
            end: final.end_us,
            attempt: final,
          })
        } else {
          push({
            key: 'receive',
            kind: 'flow',
            label: fourPoints ? t('Process response') : t('Receive response'),
            end: final.end_us,
            attempt: final,
          })
        }
      } else if (input.stream) {
        // 没有首次响应时，等待响应和等待输出分不开，合成一段
        const merged = firstByte === null
        push({
          key: merged ? 'wait' : 'think',
          kind: merged ? 'wait' : 'think',
          label: t('Wait for output'),
          end: firstWrite,
          attempt: final,
        })
        push({
          key: 'output',
          kind: 'flow',
          label: t('Stream output'),
          end: data.response.last_write_us ?? final.end_us,
          attempt: final,
        })
      } else {
        if (firstByte === null) {
          push({
            key: 'wait',
            kind: 'wait',
            label: t('Wait for output'),
            end: firstWrite,
            attempt: final,
          })
        }
        // 非流式一次写回完整结果：读取、转换和写出都算到最后一次写出，之后才是完成处理
        push({
          key: 'receive',
          kind: 'flow',
          label: t('Receive response'),
          end: data.response.last_write_us ?? firstWrite,
          attempt: final,
        })
      }
    } else {
      failed(final, final.end_us)
    }
    push({
      key: 'finish',
      kind: 'gateway',
      label: t('Finish request'),
      end: input.total,
    })
  }

  const moments: RequestMoment[] = []
  attempts.forEach((attempt, index) => {
    moments.push({
      key: `attempt-${attempt.index}-start`,
      label:
        index === 0
          ? t('Channel call started')
          : t('Attempt {{index}} started', { index: attempt.index }),
      at: attempt.start_us,
      rank: index === 0 ? 3 : 4,
      major: false,
      failed: false,
    })
    if (attempt.outcome !== 'success' && attempt.end_us !== null) {
      moments.push({
        key: `attempt-${attempt.index}-end`,
        label: failedAttemptLabel(t, attempt),
        at: attempt.end_us,
        rank: 4,
        major: false,
        failed: true,
      })
    }
  })
  if (succeeded && input.firstByte !== null) {
    moments.push({
      key: 'first-byte',
      label:
        attempts.length > 1
          ? t('Final response received')
          : t('First response received'),
      at: input.firstByte,
      rank: 1,
      major: true,
      failed: false,
    })
  }
  if (input.firstWrite !== null) {
    moments.push({
      key: 'first-write',
      label: t('First output'),
      at: input.firstWrite,
      rank: succeeded ? 0 : 2,
      major: true,
      failed: false,
    })
  }
  const lastWrite = data.response.last_write_us
  if (succeeded && lastWrite !== null) {
    moments.push({
      key: 'last-write',
      label: t('Output finished'),
      at: lastWrite,
      rank: 5,
      major: false,
      failed: false,
    })
  }
  moments.push({
    key: 'end',
    label: fourPoints ? t('Output finished') : t('Server processing ended'),
    at: input.total,
    rank: 2,
    major: true,
    failed: false,
  })
  if (fourPoints) {
    moments.splice(0, moments.length, {
      key: 'entry',
      label: t('Request entry'),
      at: 0,
      rank: 0,
      major: true,
      failed: false,
    })
    for (const attempt of attempts) {
      moments.push({
        key: `attempt-${attempt.index}-start`,
        label: t('Send request'),
        at: attempt.start_us,
        rank: 1,
        major: true,
        failed: false,
      })
      const response = getAttemptResponseOffset(attempt, data.capture.truncated)
      if (response !== null) {
        moments.push({
          key: `attempt-${attempt.index}-response`,
          label: t('First response received'),
          at: response,
          rank: 2,
          major: true,
          failed: false,
        })
      }
    }
    if (!data.capture.missing.includes('forwarding_end_not_observed')) {
      moments.push({
        key: 'end',
        label: t('Output finished'),
        at: input.total,
        rank: 3,
        major: true,
        failed: false,
      })
    }
  }
  return {
    segments,
    moments: moments
      .filter((moment) => moment.at >= 0 && moment.at <= input.total)
      .sort((a, b) => a.at - b.at),
  }
}
