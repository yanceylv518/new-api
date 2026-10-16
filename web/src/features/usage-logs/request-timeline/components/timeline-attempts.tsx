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
import { memo } from 'react'
import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

import type { TraceAttempt } from '../lib/timeline-model'
import { formatTraceBytes, formatTraceDuration } from '../lib/timeline-scale'
import { getOutcomeLabel } from '../lib/timeline-text'

export const TimelineAttempts = memo(function TimelineAttempts(props: {
  attempts: TraceAttempt[]
}) {
  const { t } = useTranslation()
  if (props.attempts.length === 0) return null
  return (
    <section className='min-w-0 space-y-2' aria-labelledby='timeline-attempts'>
      <h3 id='timeline-attempts' className='text-sm font-semibold'>
        {t('Channel attempts')}
      </h3>
      <div className='overflow-x-auto rounded-lg border'>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>#</TableHead>
              <TableHead>{t('Channel')}</TableHead>
              <TableHead>{t('Result')}</TableHead>
              <TableHead>{t('Status')}</TableHead>
              <TableHead>{t('Connection')}</TableHead>
              <TableHead>{t('First byte from entry')}</TableHead>
              <TableHead>{t('Bytes read')}</TableHead>
              <TableHead>{t('Observed data lines')}</TableHead>
              <TableHead>{t('Largest observed line gap')}</TableHead>
              <TableHead>{t('Duration')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {props.attempts.map((attempt) => {
              const exchange = attempt.exchanges?.at(-1)
              let connection = '—'
              if (exchange && exchange.reused !== null) {
                connection = exchange.reused ? t('Reused') : t('New')
              }
              return (
                <TableRow key={attempt.index}>
                  <TableCell>{attempt.index}</TableCell>
                  <TableCell className='font-mono'>
                    #{attempt.channel_id}
                  </TableCell>
                  <TableCell>
                    <StatusBadge
                      label={getOutcomeLabel(t, attempt.outcome)}
                      variant={
                        attempt.outcome === 'success' ? 'success' : 'danger'
                      }
                      copyable={false}
                    />
                  </TableCell>
                  <TableCell className='font-mono'>
                    {exchange?.status ?? '—'}
                  </TableCell>
                  <TableCell>{connection}</TableCell>
                  <TableCell className='font-mono'>
                    {formatTraceDuration(exchange?.first_byte_us ?? null)}
                  </TableCell>
                  <TableCell className='font-mono'>
                    {exchange ? formatTraceBytes(exchange.bytes_read) : '—'}
                  </TableCell>
                  <TableCell className='font-mono'>
                    {attempt.stream?.count ?? '—'}
                  </TableCell>
                  <TableCell className='font-mono'>
                    {formatTraceDuration(
                      attempt.stream && attempt.stream.count > 1
                        ? attempt.stream.max_gap_us
                        : null
                    )}
                  </TableCell>
                  <TableCell className='font-mono'>
                    {attempt.end_us === null
                      ? t('Not finished')
                      : formatTraceDuration(attempt.end_us - attempt.start_us)}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </div>
    </section>
  )
})
