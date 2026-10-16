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
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

import type { TimelineNode } from '../lib/timeline-model'
import { formatTraceBytes, formatTraceDuration } from '../lib/timeline-scale'
import {
  getBodyEndLabel,
  getOutcomeLabel,
  getStreamEndLabel,
} from '../lib/timeline-text'

export function TimelineInspector(props: {
  node: TimelineNode
  total: number
}) {
  const { t } = useTranslation()
  const node = props.node
  const duration = node.end === null ? null : node.end - node.start
  const facts: [string, string][] = [
    [t('Start'), `+${formatTraceDuration(node.start)}`],
    [
      t('End'),
      node.end === null
        ? t('Not observed')
        : `+${formatTraceDuration(node.end)}`,
    ],
    [
      t('Duration'),
      duration === null ? t('Unknown') : formatTraceDuration(duration),
    ],
    [
      t('Measurement'),
      node.derived ? t('Derived group span') : t('Observed boundary'),
    ],
  ]
  if (node.stage) facts.push([t('Stage code'), node.stage])
  const exchange = node.exchange
  if (exchange) {
    let connection = t('Not observed')
    if (exchange.reused !== null) {
      connection = exchange.reused ? t('Reused') : t('New')
    }
    facts.push(
      [t('Protocol'), exchange.protocol ?? '—'],
      [
        t('Status'),
        exchange.status ? String(exchange.status) : t('No response'),
      ],
      [t('Connection'), connection],
      [t('Proxy'), exchange.proxy ? t('Configured') : t('Not configured')],
      [t('Bytes read'), formatTraceBytes(exchange.bytes_read)],
      [
        t('Body end'),
        exchange.end_reason
          ? getBodyEndLabel(t, exchange.end_reason)
          : t('Not observed'),
      ]
    )
    for (const [key, label] of [
      ['dns', 'DNS'],
      ['tcp', 'TCP'],
      ['tls', 'TLS'],
    ] as const) {
      const span = node.network?.[key]
      if (span) facts.push([label, formatTraceDuration(span[1] - span[0])])
    }
  }
  const attempt = exchange ? undefined : node.attempt
  if (attempt) {
    facts.push(
      [t('Channel'), `#${attempt.channel_id}`],
      [t('Result'), getOutcomeLabel(t, attempt.outcome)],
      [t('Upstream requests'), String(attempt.exchanges?.length ?? 0)]
    )
    if (attempt.stream) {
      facts.push(
        [t('Observed data lines'), String(attempt.stream.count)],
        [
          t('Largest observed line gap'),
          formatTraceDuration(
            attempt.stream.count > 1 ? attempt.stream.max_gap_us : null
          ),
        ],
        [t('Stream end'), getStreamEndLabel(t, attempt.stream.end)]
      )
    }
  }

  return (
    <section
      className='space-y-3 rounded-lg border p-3 sm:p-4'
      aria-live='polite'
      aria-label={t('Selected stage')}
    >
      <div className='flex flex-wrap items-center gap-2'>
        <h3 className='text-sm font-semibold'>{node.label}</h3>
        {node.status && (
          <span
            className={cn(
              'rounded-full border px-1.5 text-xs',
              node.status.failed
                ? 'border-destructive/40 text-destructive'
                : 'border-success/40 text-success'
            )}
          >
            {node.status.label}
          </span>
        )}
      </div>
      <dl className='grid grid-cols-2 gap-x-4 gap-y-2 sm:grid-cols-4'>
        {facts.map(([label, value]) => (
          <div key={label} className='min-w-0'>
            <dt className='text-muted-foreground text-xs'>{label}</dt>
            <dd className='font-mono text-sm break-words'>{value}</dd>
          </div>
        ))}
      </dl>
      {node.description && (
        <p className='text-muted-foreground text-sm'>{node.description}</p>
      )}
      {exchange && (exchange.events?.length ?? 0) > 0 && (
        <ul className='flex flex-wrap gap-1.5' aria-label={t('Network events')}>
          {exchange.events?.map((event) => (
            <li
              key={event.sequence}
              className={cn(
                'bg-muted rounded px-1.5 font-mono text-xs',
                event.failed ? 'text-destructive' : 'text-muted-foreground'
              )}
            >
              {event.name} +{formatTraceDuration(event.offset_us)}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
