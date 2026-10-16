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
import {
  getCoreRowModel,
  getPaginationRowModel,
  useReactTable,
  type ColumnDef,
} from '@tanstack/react-table'
import { ChevronDown } from 'lucide-react'
import { memo, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePagination } from '@/components/data-table'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

import { flattenTimeline, type TimelineNode } from '../lib/timeline-model'
import {
  formatTraceDuration,
  timelinePercent,
  type TimelineSegment,
} from '../lib/timeline-scale'
import {
  HATCH,
  TIMELINE_KIND_BG,
  TIMELINE_KIND_TEXT,
} from '../lib/timeline-style'

const ROW_GRID =
  'grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 px-2 sm:grid-cols-[minmax(9rem,14rem)_minmax(0,1fr)_minmax(7rem,10rem)] sm:items-center'
export interface TimelineMarker {
  value: number
  className: string
}
interface TimelineChartProps {
  groups: TimelineNode[]
  segments: TimelineSegment[]
  total: number
  markers: TimelineMarker[]
  selectedId: string | null
  collapsed: ReadonlySet<string>
  onSelect: (id: string) => void
  onToggle: (id: string) => void
  focused: boolean
  canFocus: boolean
  onFocus: () => void
  onReset: () => void
}

function TrackGuides(props: {
  segments: TimelineSegment[]
  markers: TimelineMarker[]
}) {
  const window = props.segments[0]
  return (
    <>
      {[25, 50, 75].map((position) => (
        <span
          key={position}
          className='border-border absolute inset-y-0 border-l border-dashed'
          style={{ left: `${position}%` }}
        />
      ))}
      {props.markers
        .filter(
          (marker) => marker.value >= window.start && marker.value <= window.end
        )
        .map((marker) => (
          <span
            key={marker.className}
            className={cn(
              'absolute inset-y-0 border-l-[1.5px] border-dotted',
              marker.className
            )}
            style={{
              left: `${timelinePercent(props.segments, marker.value)}%`,
            }}
          />
        ))}
    </>
  )
}

function TimelineBar(props: {
  node: TimelineNode
  depth: number
  segments: TimelineSegment[]
  total: number
}) {
  const node = props.node
  const window = props.segments[0]
  if (
    node.start > window.end ||
    (node.end !== null && node.end < window.start)
  ) {
    return null
  }
  const left = timelinePercent(props.segments, node.start)
  const width = Math.max(
    timelinePercent(props.segments, node.end ?? props.total) - left,
    0
  )
  if (node.end === null) {
    return (
      <span
        className={cn(
          'absolute top-1/2 border-t-2 border-dashed border-current',
          TIMELINE_KIND_TEXT[node.kind]
        )}
        style={{ left: `${left}%`, width: `${width}%` }}
      />
    )
  }
  const dividers =
    node.hatched && node.network ? Object.entries(node.network) : []
  return (
    <span
      className={cn(
        'absolute top-1/2 -translate-y-1/2 overflow-hidden rounded-sm',
        props.depth === 0 ? 'h-1 opacity-60' : 'h-2.5',
        TIMELINE_KIND_BG[node.kind],
        node.hatched && HATCH
      )}
      style={{ left: `${left}%`, width: `${width}%` }}
    >
      {dividers.map(([key, span]) =>
        span ? (
          <span
            key={key}
            className='border-background absolute inset-y-0 border-r'
            style={{
              left: `${((timelinePercent(props.segments, span[0]) - left) / Math.max(width, 0.01)) * 100}%`,
              width: `${((timelinePercent(props.segments, span[1]) - timelinePercent(props.segments, span[0])) / Math.max(width, 0.01)) * 100}%`,
            }}
          />
        ) : null
      )}
    </span>
  )
}

type ChartRow = { node: TimelineNode; depth: number }
const CHART_COLUMNS: ColumnDef<ChartRow>[] = [
  { id: 'stage', accessorFn: (row) => row.node.label },
]

const TimelineChartRow = memo(function TimelineChartRow(props: {
  node: TimelineNode
  depth: number
  segments: TimelineSegment[]
  total: number
  markers: TimelineMarker[]
  selected: boolean
  expanded: boolean
  onSelect: (id: string) => void
  onToggle: (id: string) => void
}) {
  const { t } = useTranslation()
  const node = props.node
  return (
    <li
      className={cn(
        ROW_GRID,
        'py-1 sm:py-0',
        props.depth === 0 && 'border-t first:border-t-0',
        props.selected ? 'bg-accent' : 'hover:bg-muted/60'
      )}
    >
      <span
        className='flex min-h-8 min-w-0 items-center gap-1'
        style={{ paddingLeft: props.depth * 14 }}
      >
        {node.children.length > 0 ? (
          <Button
            type='button'
            variant='ghost'
            size='icon'
            className='size-5 shrink-0'
            aria-expanded={props.expanded}
            aria-label={
              props.expanded
                ? t('Collapse {{name}}', { name: node.label })
                : t('Expand {{name}}', { name: node.label })
            }
            onClick={() => props.onToggle(node.id)}
          >
            <ChevronDown
              className={cn('size-3.5', !props.expanded && '-rotate-90')}
            />
          </Button>
        ) : (
          <span className='size-5 shrink-0' />
        )}
        <Button
          type='button'
          variant='ghost'
          aria-pressed={props.selected}
          title={node.description}
          className={cn(
            'h-auto min-w-0 justify-start px-1 py-1 text-left text-sm whitespace-normal',
            props.depth === 0 && 'font-semibold'
          )}
          onClick={() => props.onSelect(node.id)}
        >
          {node.label}
        </Button>
        {node.status && (
          <span
            className={cn(
              'shrink-0 rounded-full border px-1.5 text-xs',
              node.status.failed
                ? 'border-destructive/40 text-destructive'
                : 'border-success/40 text-success'
            )}
          >
            {node.status.label}
          </span>
        )}
      </span>
      <span className='text-muted-foreground self-center text-right font-mono text-xs whitespace-nowrap sm:order-last'>
        {node.end === null
          ? t('Not observed')
          : formatTraceDuration(node.end - node.start)}
      </span>
      <span
        className='relative col-span-2 h-4 overflow-hidden sm:col-span-1 sm:h-8'
        aria-hidden='true'
      >
        <TrackGuides segments={props.segments} markers={props.markers} />
        <TimelineBar
          node={node}
          depth={props.depth}
          segments={props.segments}
          total={props.total}
        />
      </span>
    </li>
  )
})

export function TimelineChart(props: TimelineChartProps) {
  const { t } = useTranslation()
  const rows = useMemo(
    () => flattenTimeline(props.groups, props.collapsed),
    [props.groups, props.collapsed]
  )
  const table = useReactTable({
    data: rows,
    columns: CHART_COLUMNS,
    getRowId: (row) => row.node.id,
    getCoreRowModel: getCoreRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    initialState: { pagination: { pageSize: 50 } },
  })
  const window = props.segments[0]
  const ticks = [
    ...new Set([
      window.start,
      window.start + Math.trunc((window.end - window.start) / 2),
      window.end,
    ]),
  ]
  return (
    <section className='min-w-0 space-y-3' aria-labelledby='timeline-stages'>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <h3 id='timeline-stages' className='text-sm font-semibold'>
          {t('Linear time axis')}
        </h3>
        <div className='flex gap-2'>
          <Button
            variant='outline'
            size='sm'
            onClick={props.onReset}
            disabled={!props.focused}
          >
            {t('Whole request')}
          </Button>
          <Button
            variant='outline'
            size='sm'
            onClick={props.onFocus}
            disabled={!props.canFocus}
          >
            {t('Focus selected stage')}
          </Button>
        </div>
      </div>
      <div className='text-muted-foreground flex flex-wrap gap-x-4 gap-y-1 text-xs'>
        <span className='inline-flex items-center gap-1.5'>
          <i className={cn('h-2 w-3 rounded-sm', TIMELINE_KIND_BG.gateway)} />
          {t('Gateway')}
        </span>
        <span className='inline-flex items-center gap-1.5'>
          <i className={cn('h-2 w-3 rounded-sm', TIMELINE_KIND_BG.upstream)} />
          {t('Upstream')}
        </span>
        <span className='inline-flex items-center gap-1.5'>
          <i className={cn('h-2 w-3 rounded-sm', TIMELINE_KIND_BG.response)} />
          {t('Response')}
        </span>
        <span className='inline-flex items-center gap-1.5'>
          <i className={cn('h-2 w-3 rounded-sm', TIMELINE_KIND_BG.log)} />
          {t('Settlement and logs')}
        </span>
        <span className='text-warning'>┊ {t('Upstream first byte')}</span>
        <span className='text-success'>┊ {t('First payload write')}</span>
      </div>
      <div className='overflow-hidden rounded-lg border'>
        <div
          className={cn(ROW_GRID, 'bg-muted/60 py-2 text-xs')}
          aria-hidden='true'
        >
          <span className='text-muted-foreground hidden sm:block'>
            {t('Stage')}
          </span>
          <span className='relative col-span-2 h-5 sm:col-span-1'>
            {ticks.map((value, index) => (
              <span
                key={value}
                className='absolute font-mono whitespace-nowrap'
                style={{
                  left: `${timelinePercent(props.segments, value)}%`,
                  transform: `translateX(-${(index / Math.max(ticks.length - 1, 1)) * 100}%)`,
                }}
              >
                {formatTraceDuration(value)}
              </span>
            ))}
          </span>
          <span className='text-muted-foreground hidden text-right sm:block'>
            {t('Duration')}
          </span>
        </div>
        <ul aria-label={t('Request stages')}>
          {table.getRowModel().rows.map(({ original }) => (
            <TimelineChartRow
              key={original.node.id}
              node={original.node}
              depth={original.depth}
              segments={props.segments}
              total={props.total}
              markers={props.markers}
              selected={props.selectedId === original.node.id}
              expanded={!props.collapsed.has(original.node.id)}
              onSelect={props.onSelect}
              onToggle={props.onToggle}
            />
          ))}
        </ul>
        <p className='text-muted-foreground border-t px-3 py-2 text-xs'>
          {t(
            'One linear scale applies to every row. Select a short stage and focus it to inspect details. Dashed lines have no observed end; parent spans include their children.'
          )}
        </p>
      </div>
      <DataTablePagination table={table} compact />
    </section>
  )
}
