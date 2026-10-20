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

import { EmptyState } from '@/components/empty-state'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

import { findTimelineNode, type TimelineNode } from '../lib/timeline-model'
import {
  formatTraceDuration,
  timelinePercent,
  type TimelineSegment,
} from '../lib/timeline-scale'
import { HATCH, TIMELINE_KIND_BG } from '../lib/timeline-style'

export interface TimelineMarker {
  value: number
  label: string
  className: string
}

interface TimelineChartProps {
  groups: TimelineNode[]
  segments: TimelineSegment[]
  total: number
  markers: TimelineMarker[]
  selectedId: string | null
  onSelect: (id: string) => void
}

export function TimelineChart(props: TimelineChartProps) {
  const { t } = useTranslation()
  const selectedGroup = useMemo(
    () =>
      props.selectedId === null
        ? null
        : (props.groups.find((group) =>
            findTimelineNode([group], props.selectedId)
          )?.id ?? null),
    [props.groups, props.selectedId]
  )
  if (props.groups.length === 0) {
    return <EmptyState title={t('No data available')} />
  }

  return (
    <section className='min-w-0 space-y-3' aria-label={t('Request timeline')}>
      <div className='text-muted-foreground flex justify-between gap-3 font-mono text-xs'>
        <span>{formatTraceDuration(0)}</span>
        <span>{formatTraceDuration(props.total)}</span>
      </div>
      <div
        className='bg-muted relative h-8 overflow-hidden rounded-md'
        aria-hidden='true'
      >
        {props.groups.map((node) => {
          const left = timelinePercent(props.segments, node.start)
          const width = Math.max(
            timelinePercent(props.segments, node.end ?? props.total) - left,
            0
          )
          return (
            <span
              key={node.id}
              className={cn(
                'absolute inset-y-0 border-r border-background',
                TIMELINE_KIND_BG[node.kind],
                node.end === null && HATCH,
                selectedGroup !== null &&
                  selectedGroup !== node.id &&
                  'opacity-40'
              )}
              style={{ left: `${left}%`, width: `${width}%` }}
            />
          )
        })}
        {props.markers
          .filter((marker) => marker.value >= 0 && marker.value <= props.total)
          .map((marker) => (
            <span
              key={marker.label}
              className={cn(
                'absolute inset-y-0 border-l-2 border-dashed ring-1 ring-background',
                marker.className
              )}
              title={`${marker.label} · ${formatTraceDuration(marker.value)}`}
              style={{
                left: `${timelinePercent(props.segments, marker.value)}%`,
              }}
            />
          ))}
      </div>
      <ol
        className='flex flex-wrap gap-x-2 gap-y-1'
        aria-label={t('Request stages')}
      >
        {props.groups.map((node) => (
          <li key={node.id} className='min-w-0'>
            <Button
              type='button'
              variant='ghost'
              size='sm'
              aria-pressed={selectedGroup === node.id}
              aria-controls={
                selectedGroup === node.id ? 'timeline-selection' : undefined
              }
              className={cn(
                'h-auto max-w-full justify-start gap-2 px-2 py-1.5 text-left whitespace-normal',
                selectedGroup === node.id && 'bg-accent'
              )}
              onClick={() => props.onSelect(node.id)}
            >
              <span
                className={cn(
                  'size-2 shrink-0 rounded-sm',
                  TIMELINE_KIND_BG[node.kind]
                )}
                aria-hidden='true'
              />
              <span>{node.label}</span>
              {node.status?.failed && (
                <span className='text-destructive'>{node.status.label}</span>
              )}
              <span className='text-muted-foreground font-mono text-xs whitespace-nowrap'>
                {node.end === null
                  ? t('Not recorded')
                  : formatTraceDuration(node.end - node.start)}
              </span>
            </Button>
          </li>
        ))}
      </ol>
    </section>
  )
}
