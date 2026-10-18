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

import { Button } from '@/components/ui/button'

import type {
  TimelineObservation,
  TimelineObservationKind,
} from '../lib/timeline-model'
import { formatTraceDuration } from '../lib/timeline-scale'

export function TimelineDiagnostics(props: {
  observations: TimelineObservation[]
  onSelect: (id: string) => void
}) {
  const { t } = useTranslation()
  if (props.observations.length === 0) return null
  const text: Record<
    TimelineObservationKind,
    { label: string; description: string }
  > = {
    retry_window: {
      label: t('Earlier attempts and channel reselection'),
      description: t(
        'From the first attempt start to the final attempt start, including earlier attempts and the gaps between them.'
      ),
    },
    response_wait: {
      label: t('Longest observed response-byte wait'),
      description: t(
        'From one completed request write to its first response byte. Includes network round trips; upstream queueing cannot be isolated.'
      ),
    },
    read_gap: {
      label: t('Largest recorded data-line gap'),
      description: t(
        'The largest gap in the recorded attempts. Downstream backpressure can contribute; the exact gap location was not recorded.'
      ),
    },
    log_insert: {
      label: t('Longest recorded usage or error log insert'),
      description: t(
        'Includes ORM and database-client work and the database round trip; it is not database-server CPU time.'
      ),
    },
    write_tail: {
      label: t('After the last observed response write'),
      description: t(
        'From the last Write that returned bytes to synchronous handler completion. Includes any remaining flush or middleware work, excluding timeline persistence.'
      ),
    },
  }
  return (
    <div className='space-y-3 rounded-lg border p-3'>
      <h3 className='text-sm font-medium'>{t('Timing observations')}</h3>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Only complete observed boundaries are compared. These intervals may overlap and must not be added together; missing observations do not imply zero time.'
        )}
      </p>
      <dl className='grid gap-3 sm:grid-cols-2'>
        {props.observations.map((item) => (
          <div key={item.kind} className='min-w-0 space-y-1'>
            <dt className='text-sm'>
              {item.nodeId ? (
                <Button
                  type='button'
                  variant='link'
                  className='h-auto justify-start p-0 text-left whitespace-normal'
                  onClick={() => {
                    if (item.nodeId) props.onSelect(item.nodeId)
                  }}
                >
                  {text[item.kind].label}
                </Button>
              ) : (
                text[item.kind].label
              )}
            </dt>
            <dd className='font-mono text-sm break-all tabular-nums'>
              {formatTraceDuration(item.duration)}
            </dd>
            <dd className='text-muted-foreground text-xs'>
              {text[item.kind].description}
            </dd>
            {item.start !== undefined && item.end !== undefined && (
              <dd className='text-muted-foreground text-xs'>
                {t('Observed offsets: {{start}} → {{end}}', {
                  start: formatTraceDuration(item.start),
                  end: formatTraceDuration(item.end),
                })}
              </dd>
            )}
          </div>
        ))}
      </dl>
    </div>
  )
}
