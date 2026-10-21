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
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

import { useElementWidth } from '../hooks/use-element-width'
import { getLabelFonts, measureTextWidth } from '../lib/text-width'
import {
  formatTraceBytes,
  formatTraceDuration,
  formatTraceShare,
} from '../lib/timeline-scale'
import type { RequestSegment, SegmentStep } from '../lib/timeline-segments'
import { SEGMENT_FILL, SEGMENT_INK } from '../lib/timeline-style'
import {
  getBodyEndLabel,
  getOutcomeLabel,
  getStreamEndLabel,
} from '../lib/timeline-text'
import { SegmentStatus, SegmentSwatch, type LabelFit } from './timeline-chart'

/** 放大镜：按选中段自己的刻度展开内部步骤，并列出这段的关键数据。 */
export function TimelineLens(props: {
  segment: RequestSegment
  steps: SegmentStep[]
  total: number
  stream: boolean
}) {
  const { t } = useTranslation()
  const [laneRef, width] = useElementWidth<HTMLDivElement>()
  const segment = props.segment
  const end = segment.end ?? props.total
  const span = Math.max(end - segment.start, 1)
  const fits = useMemo(() => {
    const fonts = width > 0 ? getLabelFonts() : null
    return props.steps.map((step): LabelFit => {
      if (!fonts) return 'none'
      const room = ((step.end - step.start) / span) * width - 18
      const name = measureTextWidth(step.label, fonts.strong)
      const time = measureTextWidth(
        formatTraceDuration(step.end - step.start),
        fonts.mono
      )
      if (room >= name + 6 + time) return 'full'
      return room >= name ? 'name' : 'none'
    })
  }, [width, props.steps, span])

  // 步骤之间可能有间隙；按开始时间合并重叠后求实际覆盖的时长，余下的单独列出
  let covered = 0
  let reach = segment.start
  for (const step of props.steps) {
    const from = Math.max(step.start, reach)
    if (step.end > from) covered += step.end - from
    reach = Math.max(reach, step.end)
  }
  const uncovered =
    segment.end === null || props.steps.length === 0
      ? 0
      : segment.end - segment.start - covered

  const attempt = segment.attempt
  const exchange = attempt?.exchanges?.at(-1)
  const facts: [string, string][] = []
  if (attempt && (segment.kind === 'failed' || segment.kind === 'wait')) {
    facts.push([t('Channel'), `#${attempt.channel_id}`])
    if (segment.kind === 'failed') {
      facts.push([t('Result'), getOutcomeLabel(t, attempt.outcome)])
    }
    if (exchange) {
      let connection = t('Not recorded')
      if (exchange.reused !== null) {
        connection = exchange.reused ? t('Reused') : t('New')
      }
      facts.push(
        [t('Connection'), connection],
        [t('Protocol'), exchange.protocol ?? '—']
      )
    }
  }
  if (attempt && segment.kind === 'flow') {
    if (props.stream && attempt.stream) {
      facts.push(
        [t('Received data entries'), String(attempt.stream.count)],
        [
          t('Longest response pause'),
          formatTraceDuration(
            attempt.stream.count > 1 ? attempt.stream.max_gap_us : null
          ),
        ],
        [t('End reason'), getStreamEndLabel(t, attempt.stream.end)]
      )
    } else if (exchange) {
      facts.push(
        [t('Response size'), formatTraceBytes(exchange.bytes_read)],
        [
          t('End reason'),
          exchange.end_reason
            ? getBodyEndLabel(t, exchange.end_reason)
            : t('Not recorded'),
        ]
      )
    }
  }

  return (
    <section
      id='timeline-selection'
      className='space-y-3 rounded-lg border p-3 sm:p-4'
      aria-live='polite'
      aria-label={t('Selected stage')}
    >
      <div className='flex flex-wrap items-center gap-x-3 gap-y-1'>
        <h3 className='flex items-center gap-2 text-sm font-semibold'>
          <SegmentSwatch kind={segment.kind} />
          {segment.label}
        </h3>
        <SegmentStatus segment={segment} />
        <span className='font-mono text-sm'>
          {segment.end === null
            ? t('Not recorded')
            : formatTraceDuration(segment.end - segment.start)}
        </span>
        {segment.end !== null && (
          <span className='text-muted-foreground text-xs'>
            {formatTraceShare(segment.end - segment.start, props.total)}
          </span>
        )}
      </div>
      <div ref={laneRef} className='space-y-2'>
        {props.steps.length > 0 && (
          <>
            <div
              className='bg-muted relative h-6 rounded-[4px]'
              aria-hidden='true'
            >
              {props.steps.map((step, i) => (
                <span
                  key={step.id}
                  className={cn(
                    'absolute inset-y-0 rounded-[3px] bg-(--segment)',
                    SEGMENT_FILL[step.kind]
                  )}
                  style={{
                    left: `${((step.start - segment.start) / span) * 100}%`,
                    width: `max(3px, calc(${((step.end - step.start) / span) * 100}% - 2px))`,
                  }}
                  title={`${step.label} · ${formatTraceDuration(step.end - step.start)}`}
                >
                  {fits[i] !== 'none' && (
                    <span
                      className={cn(
                        'absolute inset-0 flex items-center justify-center gap-1.5 px-2 text-xs font-semibold whitespace-nowrap',
                        SEGMENT_INK
                      )}
                    >
                      {step.label}
                      {fits[i] === 'full' && (
                        <span className='font-mono font-normal'>
                          {formatTraceDuration(step.end - step.start)}
                        </span>
                      )}
                    </span>
                  )}
                </span>
              ))}
            </div>
            <div
              className='text-muted-foreground flex justify-between gap-3 font-mono text-xs'
              aria-hidden='true'
            >
              <span>+{formatTraceDuration(segment.start)}</span>
              <span>+{formatTraceDuration(end)}</span>
            </div>
            <ul
              className='flex flex-wrap gap-x-4 gap-y-1 text-xs'
              aria-label={t('Stage details')}
            >
              {props.steps.map((step) => (
                <li key={step.id} className='flex items-center gap-1.5'>
                  <SegmentSwatch kind={step.kind} />
                  {step.label}
                  <span className='text-muted-foreground font-mono'>
                    {formatTraceDuration(step.end - step.start)}
                  </span>
                </li>
              ))}
              {uncovered > 0 && (
                <li className='text-muted-foreground flex items-center gap-1.5'>
                  <span
                    className='bg-muted size-2.5 shrink-0 rounded-[3px] border'
                    aria-hidden='true'
                  />
                  {t('Not covered')}
                  <span className='font-mono'>
                    {formatTraceDuration(uncovered)}
                  </span>
                </li>
              )}
            </ul>
          </>
        )}
        {props.steps.length === 0 && segment.kind === 'think' && (
          <p className='text-muted-foreground text-sm'>
            {t('No further breakdown is available for this stage.')}
          </p>
        )}
      </div>
      {facts.length > 0 && (
        <dl className='grid grid-cols-2 gap-x-4 gap-y-2 sm:grid-cols-4'>
          {facts.map(([label, value]) => (
            <div key={label} className='min-w-0'>
              <dt className='text-muted-foreground text-xs'>{label}</dt>
              <dd className='font-mono text-sm break-words'>{value}</dd>
            </div>
          ))}
        </dl>
      )}
    </section>
  )
}
