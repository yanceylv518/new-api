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
import { X } from 'lucide-react'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

import { useElementWidth } from '../hooks/use-element-width'
import { getLabelFonts, measureTextWidth } from '../lib/text-width'
import {
  formatTraceDuration,
  formatTraceShare,
  layoutSegments,
  placeLabels,
} from '../lib/timeline-scale'
import type {
  RequestMoment,
  RequestSegment,
  SegmentKind,
} from '../lib/timeline-segments'
import { HATCH, SEGMENT_FILL, SEGMENT_INK } from '../lib/timeline-style'

/** 段内标签放得下多少：名称加耗时、只有名称、都放不下。 */
export type LabelFit = 'full' | 'name' | 'none'

/** 极短的段至少画这么宽（含 2px 间隔），否则看不见也点不到。 */
const MIN_SEGMENT_PX = 6
/** 窄于这个宽度时引线会层层交叉，放不下文字的段只在图例里列出。 */
const CALLOUT_MIN_WIDTH = 480
const CALLOUT_ROW_PX = 20
const TICK_ROW_PX = 18

export function SegmentSwatch(props: { kind: SegmentKind }) {
  return (
    <span
      className={cn(
        'size-2.5 shrink-0 rounded-[3px] bg-(--segment)',
        SEGMENT_FILL[props.kind]
      )}
      aria-hidden='true'
    />
  )
}

/** 失败调用最后一次上游请求的状态码，没有响应时写明。 */
export function SegmentStatus(props: { segment: RequestSegment }) {
  const { t } = useTranslation()
  if (props.segment.kind !== 'failed') return null
  const status = props.segment.attempt?.exchanges?.at(-1)?.status
  return (
    <span className='text-destructive inline-flex items-center gap-0.5 text-xs font-medium whitespace-nowrap'>
      <X className='size-3' aria-hidden='true' />
      {status ?? t('No response')}
    </span>
  )
}

interface TimelineChartProps {
  segments: RequestSegment[]
  moments: RequestMoment[]
  total: number
  selectedKey: string | null
  onSelect: (key: string) => void
}

/**
 * 按关键时刻切段的单条时间线：真实比例的色条，放得下就在段内写名称和耗时，
 * 过短的段用引线标在上方，关键时刻写在下方；图例按先后列出每段耗时和占比。
 */
export function TimelineChart(props: TimelineChartProps) {
  const { t } = useTranslation()
  const [trackRef, width] = useElementWidth<HTMLDivElement>()
  const chart = useMemo(() => {
    const duration = (segment: RequestSegment) =>
      segment.end === null
        ? t('Not recorded')
        : formatTraceDuration(segment.end - segment.start)
    const measured = width > 0
    // 没测到宽度（首帧或测试环境）时退回纯比例布局，不画依赖像素的标签
    const scale = measured ? width : 100
    const { boxes, toX } = layoutSegments(
      props.segments.map((segment) => ({
        start: segment.start,
        end: segment.end ?? props.total,
      })),
      scale,
      measured ? MIN_SEGMENT_PX : 0
    )
    const fonts = measured ? getLabelFonts() : null
    const fits = props.segments.map((segment, i): LabelFit => {
      if (!fonts) return 'none'
      // 段内左右各留 8px，名称和耗时之间 6px，扣掉 2px 段间隔
      const room = boxes[i].width - 18
      const name = measureTextWidth(segment.label, fonts.strong)
      const time = measureTextWidth(duration(segment), fonts.mono)
      if (room >= name + 6 + time) return 'full'
      return room >= name ? 'name' : 'none'
    })
    const callouts =
      fonts && width >= CALLOUT_MIN_WIDTH
        ? placeLabels(
            props.segments.flatMap((segment, i) =>
              fits[i] === 'none'
                ? [
                    {
                      segment,
                      duration: duration(segment),
                      anchor: boxes[i].x + Math.max(boxes[i].width - 2, 1) / 2,
                      x: boxes[i].x + boxes[i].width / 2,
                      width:
                        14 +
                        measureTextWidth(segment.label, fonts.strong) +
                        measureTextWidth(duration(segment), fonts.mono),
                      rank: 0,
                    },
                  ]
                : []
            ),
            width,
            { rows: 2, gap: 12, shift: 160 }
          )
        : []
    const ticks = fonts
      ? placeLabels(
          props.moments.map((moment) => {
            let labelWidth = measureTextWidth(
              formatTraceDuration(moment.at),
              fonts.mono
            )
            if (moment.major) {
              labelWidth += measureTextWidth(moment.label, fonts.text) + 4
            }
            if (moment.failed) labelWidth += 16
            return {
              moment,
              x: toX(moment.at),
              width: labelWidth,
              rank: moment.rank,
            }
          }),
          width,
          { rows: 2, gap: 10 }
        )
      : []
    return {
      scale,
      boxes,
      fits,
      callouts,
      ticks,
      durations: props.segments.map(duration),
    }
  }, [width, props.segments, props.moments, props.total, t])

  if (props.segments.length === 0) {
    return <EmptyState title={t('No data available')} />
  }
  const percent = (px: number) => `${(px / chart.scale) * 100}%`
  const calloutRows =
    chart.callouts.length > 0
      ? Math.max(...chart.callouts.map((callout) => callout.row)) + 1
      : 0
  const calloutHeight = calloutRows > 0 ? calloutRows * CALLOUT_ROW_PX + 8 : 0
  const tickRows =
    chart.ticks.length > 0
      ? Math.max(...chart.ticks.map((tick) => tick.row)) + 1
      : 0

  return (
    <section className='min-w-0 space-y-2' aria-label={t('Request timeline')}>
      {calloutHeight > 0 && (
        <div
          className='relative'
          style={{ height: calloutHeight }}
          aria-hidden='true'
        >
          <svg className='absolute inset-0 size-full overflow-visible'>
            {chart.callouts.map((callout) => {
              const top = (calloutRows - 1 - callout.row) * CALLOUT_ROW_PX
              const from = Math.min(
                Math.max(callout.anchor, callout.left + 4),
                callout.left + callout.width - 4
              )
              return (
                <path
                  key={callout.segment.key}
                  d={`M${from},${top + 18} L${from},${top + 22} L${callout.anchor},${calloutHeight}`}
                  className='stroke-muted-foreground'
                  fill='none'
                  strokeWidth={1}
                />
              )
            })}
          </svg>
          {chart.callouts.map((callout) => (
            <span
              key={callout.segment.key}
              className='bg-popover absolute flex items-baseline gap-1.5 px-1 text-xs leading-[18px] whitespace-nowrap'
              style={{
                left: callout.left,
                top: (calloutRows - 1 - callout.row) * CALLOUT_ROW_PX,
              }}
            >
              <span className='font-semibold'>{callout.segment.label}</span>
              <span className='text-muted-foreground font-mono'>
                {callout.duration}
              </span>
            </span>
          ))}
        </div>
      )}
      <div
        ref={trackRef}
        className='bg-muted relative h-6 rounded-[4px]'
        aria-hidden='true'
      >
        {props.segments.map((segment, i) => {
          const box = chart.boxes[i]
          const last = i === props.segments.length - 1
          const selected = props.selectedKey === segment.key
          const fit = chart.fits[i]
          return (
            // 色条只给指针操作；键盘和读屏使用下方图例里的同一组按钮
            <span
              key={segment.key}
              className={cn(
                'absolute inset-y-0 cursor-pointer bg-(--segment) transition-opacity hover:brightness-110',
                SEGMENT_FILL[segment.kind],
                i === 0 && 'rounded-l-[4px]',
                last && 'rounded-r-[4px]',
                segment.end === null && HATCH,
                props.selectedKey !== null && !selected && 'opacity-40',
                selected &&
                  'ring-foreground ring-offset-popover z-10 ring-2 ring-offset-2'
              )}
              style={{
                left: percent(box.x),
                width: last
                  ? percent(box.width)
                  : `calc(${percent(box.width)} - 2px)`,
              }}
              title={`${segment.label} · ${chart.durations[i]}`}
              onClick={() => props.onSelect(segment.key)}
            >
              {fit !== 'none' && (
                <span
                  className={cn(
                    'absolute inset-0 flex items-center justify-center gap-1.5 px-2 text-xs font-semibold whitespace-nowrap',
                    SEGMENT_INK
                  )}
                >
                  {segment.label}
                  {fit === 'full' && (
                    <span className='font-mono font-normal'>
                      {chart.durations[i]}
                    </span>
                  )}
                </span>
              )}
            </span>
          )
        })}
      </div>
      {tickRows > 0 && (
        <div
          className='relative'
          style={{ height: tickRows * TICK_ROW_PX + 6 }}
          aria-hidden='true'
        >
          {/* 先画全部刻度线，再画带底色的标签，下一行的刻度线不会压住上一行的文字 */}
          {chart.ticks.map((tick) => (
            <span
              key={tick.moment.key}
              className='bg-muted-foreground/50 absolute top-0 w-px'
              style={{
                left: Math.min(tick.x, width - 1),
                height: tick.row * TICK_ROW_PX + 5,
              }}
            />
          ))}
          {chart.ticks.map((tick) => (
            <span
              key={tick.moment.key}
              className={cn(
                'bg-popover absolute flex items-center gap-1 text-xs leading-4 whitespace-nowrap',
                tick.moment.major ? 'text-foreground' : 'text-muted-foreground'
              )}
              style={{ left: tick.left, top: tick.row * TICK_ROW_PX + 5 }}
            >
              {tick.moment.failed && (
                <X className='text-destructive size-3' aria-hidden='true' />
              )}
              {tick.moment.major && <span>{tick.moment.label}</span>}
              <span className='font-mono'>
                {formatTraceDuration(tick.moment.at)}
              </span>
            </span>
          ))}
        </div>
      )}
      <ol
        className='flex flex-wrap gap-x-1 gap-y-1 pt-1'
        aria-label={t('Request stages')}
      >
        {props.segments.map((segment, i) => {
          const selected = props.selectedKey === segment.key
          return (
            <li key={segment.key} className='min-w-0'>
              <Button
                type='button'
                variant='ghost'
                size='sm'
                aria-pressed={selected}
                className='aria-pressed:bg-foreground/10 h-auto max-w-full justify-start gap-2 px-2 py-1.5 text-left whitespace-normal'
                onClick={() => props.onSelect(segment.key)}
              >
                <SegmentSwatch kind={segment.kind} />
                <span>{segment.label}</span>
                <SegmentStatus segment={segment} />
                <span className='text-muted-foreground font-mono text-xs whitespace-nowrap'>
                  {chart.durations[i]}
                </span>
                {segment.end !== null && (
                  <span className='text-muted-foreground text-xs whitespace-nowrap'>
                    {formatTraceShare(segment.end - segment.start, props.total)}
                  </span>
                )}
              </Button>
            </li>
          )
        })}
      </ol>
    </section>
  )
}
