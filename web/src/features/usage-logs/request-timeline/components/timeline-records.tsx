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
import { memo, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import {
  StaticDataTable,
  type StaticDataTableColumn,
} from '@/components/data-table'
import { Button } from '@/components/ui/button'

import type { TraceEventRow } from '../lib/timeline-events'
import { formatTraceDuration } from '../lib/timeline-scale'

/** 长列表在表格内滚动，避免把弹窗正文持续撑高。 */
export const TimelineRecords = memo(function TimelineRecords(props: {
  rows: TraceEventRow[]
  events?: boolean
  onSelect: (id: string) => void
}) {
  const { t } = useTranslation()
  const onSelect = props.onSelect
  const events = props.events
  const columns = useMemo<StaticDataTableColumn<TraceEventRow>[]>(() => {
    const result: StaticDataTableColumn<TraceEventRow>[] = [
      {
        id: 'label',
        header: events ? t('Event') : t('Stage'),
        cell: (row) => {
          const nodeId = row.nodeId
          if (!nodeId) {
            return (
              <span className={row.failed ? 'text-destructive' : undefined}>
                {row.label}
              </span>
            )
          }
          return (
            <Button
              variant='link'
              className='h-auto max-w-full justify-start p-0 text-left whitespace-normal'
              onClick={() => onSelect(nodeId)}
            >
              {row.label}
            </Button>
          )
        },
      },
      {
        id: 'code',
        header: t('Source'),
        cell: (row) => (
          <code className='text-muted-foreground text-xs'>{row.code}</code>
        ),
      },
      {
        id: 'start',
        header: events ? t('Offset from entry') : t('Start'),
        cell: (row) => (
          <span className='font-mono whitespace-nowrap'>
            {formatTraceDuration(row.start)}
          </span>
        ),
      },
    ]
    if (!events) {
      result.push(
        {
          id: 'end',
          header: t('End'),
          cell: (row) => (
            <span className='font-mono whitespace-nowrap'>
              {row.end === null
                ? t('Not observed')
                : formatTraceDuration(row.end)}
            </span>
          ),
        },
        {
          id: 'duration',
          header: t('Duration'),
          cell: (row) => (
            <span className='font-mono whitespace-nowrap'>
              {row.end === null
                ? t('Unknown')
                : formatTraceDuration(row.end - row.start)}
            </span>
          ),
        }
      )
    }
    return result
  }, [t, events, onSelect])
  return (
    <div className='min-w-0 space-y-3'>
      <p className='text-muted-foreground text-xs'>
        {props.events
          ? t(
              'Events are ordered by observed offset. Equal timestamps do not establish an order between concurrent callbacks.'
            )
          : t(
              'All offsets start at gateway entry. Parent stages include their children; durations must not be added together.'
            )}
      </p>
      <StaticDataTable
        className='focus-visible:ring-ring/50 max-h-[min(50dvh,28rem)] overflow-auto overscroll-contain outline-none focus-visible:ring-[3px] [&>[data-slot=table-container]]:overflow-visible'
        containerProps={{
          role: 'region',
          'aria-label': events ? t('Event list') : t('Stage details'),
          tabIndex: 0,
        }}
        columns={columns}
        data={props.rows}
        getRowKey={(row) => row.id}
        getRowClassName={(row) => (row.failed ? 'bg-destructive/5' : undefined)}
        emptyContent={t('No data available')}
        tableClassName='min-w-[42rem] [&_thead]:sticky [&_thead]:top-0 [&_thead]:z-10 [&_thead]:bg-background'
      />
    </div>
  )
})
