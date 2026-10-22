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
import { useQuery } from '@tanstack/react-query'
import { ChevronDown, RefreshCw } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { cn } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth-store'

import { getRequestTimelineWriterStatus } from '../api'
import {
  formatTraceDuration,
  formatTraceTimestamp,
} from '../lib/timeline-scale'

function WriterStatusContent() {
  const { t } = useTranslation()
  const viewerId = useAuthStore((state) => state.auth.user?.id)
  const viewerRole = useAuthStore((state) => state.auth.user?.role)
  const query = useQuery({
    queryKey: ['request-timeline-writer-status', viewerId, viewerRole],
    queryFn: ({ signal }) => getRequestTimelineWriterStatus(signal),
    retry: false,
    staleTime: 0,
    refetchInterval: (current) => (current.state.error ? false : 5000),
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: false,
  })
  if (query.isPending) {
    return <LoadingState size='sm' className='min-h-0 py-6' />
  }
  if (query.isError) {
    return (
      <ErrorState
        className='min-h-0 py-6'
        title={t('Unable to load timeline writer status')}
        action={
          <Button
            type='button'
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t('Retry')}
          </Button>
        }
      />
    )
  }
  const status = query.data
  if (!status.initialized) {
    return (
      <EmptyState
        className='min-h-0 py-6'
        title={t('Timeline writer has not started')}
      />
    )
  }
  const stages = [
    { label: t('Waiting in queue'), value: String(status.pending) },
    { label: t('Preparing a timeline'), value: String(status.processing) },
    { label: t('Waiting in a batch'), value: String(status.buffered) },
    { label: t('Writing or retrying'), value: String(status.writing) },
    { label: t('Outstanding timelines'), value: String(status.outstanding) },
    { label: t('Queue capacity'), value: String(status.queue_capacity) },
    {
      label: t('Reserved payload bytes'),
      value:
        status.pending_payload_bytes === undefined
          ? '—'
          : String(status.pending_payload_bytes),
    },
    {
      label: t('Payload byte budget'),
      value:
        status.payload_budget_bytes === undefined
          ? '—'
          : String(status.payload_budget_bytes),
    },
    {
      label: t('Oldest outstanding wait'),
      value: formatTraceDuration(status.oldest_outstanding_age_us),
    },
    {
      label: t('Last batch duration'),
      value: formatTraceDuration(status.last_batch_duration_us),
    },
    {
      label: t('Last completed batch size'),
      value:
        status.last_batch_duration_us === null
          ? '—'
          : String(status.last_batch_size),
    },
  ]
  const counters = [
    { label: t('Accepted since startup'), value: status.accepted },
    {
      label: t('Excluded by success sampling'),
      value: status.sampled_out ?? '—',
    },
    { label: t('Written since startup'), value: status.written },
    { label: t('Failed writes since startup'), value: status.failed },
    { label: t('Dropped since startup'), value: status.dropped },
    { label: t('Write retries since startup'), value: status.retries },
    { label: t('Expired timelines deleted'), value: status.cleanup_deleted },
    { label: t('Failed cleanup tasks'), value: status.cleanup_failed },
  ]
  const drops = [
    { label: t('Encoding or size limit'), value: status.dropped_encoding },
    { label: t('Queue full'), value: status.dropped_queue_full },
    {
      label: t('Payload budget exceeded'),
      value: status.dropped_byte_limit ?? '—',
    },
    { label: t('Writer already stopped'), value: status.dropped_stopped },
    { label: t('Shutdown deadline'), value: status.dropped_shutdown },
  ]
  return (
    <div className='space-y-4 pt-4'>
      <div className='flex flex-wrap items-center gap-2'>
        <StatusBadge
          label={
            status.accepting
              ? t('Accepting timelines')
              : t('Not accepting timelines')
          }
          variant={status.accepting ? 'success' : 'neutral'}
          copyable={false}
        />
        <span className='text-muted-foreground min-w-0 text-xs break-all'>
          {t('Current node')}: {status.node_id || '—'}
        </span>
        <Button
          type='button'
          size='sm'
          variant='ghost'
          className='ml-auto'
          disabled={query.isFetching}
          onClick={() => void query.refetch()}
        >
          <RefreshCw
            className={cn('size-4', query.isFetching && 'animate-spin')}
            aria-hidden='true'
          />
          {t('Refresh')}
        </Button>
      </div>
      <p className='text-muted-foreground text-xs'>
        {t('Node started')}: {formatTraceTimestamp(status.started_at_us)}
      </p>
      <p className='text-muted-foreground text-xs'>
        {t(
          'This snapshot describes the responding node only. Counters restart with that node; refreshes may reach another node.'
        )}
      </p>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Updates every five seconds while expanded and visible. An error pauses automatic refresh.'
        )}
      </p>
      {(status.failed > 0 || status.dropped > 0) && (
        <Alert>
          <AlertDescription>
            {t(
              'Failures or drops have occurred since startup. Use the latest successful write time to assess the current state.'
            )}
          </AlertDescription>
        </Alert>
      )}
      <dl className='grid grid-cols-2 gap-3 sm:grid-cols-4'>
        {stages.map((item) => (
          <div key={item.label} className='min-w-0'>
            <dt className='text-muted-foreground text-xs'>{item.label}</dt>
            <dd className='font-mono text-sm break-all tabular-nums'>
              {item.value}
            </dd>
          </div>
        ))}
      </dl>
      <dl className='grid grid-cols-2 gap-x-4 gap-y-2 border-t pt-3 text-sm'>
        {counters.map((item) => (
          <div
            key={item.label}
            className='flex flex-wrap justify-between gap-2'
          >
            <dt className='text-muted-foreground'>{item.label}</dt>
            <dd className='font-mono tabular-nums'>{item.value}</dd>
          </div>
        ))}
      </dl>
      <div className='space-y-2 border-t pt-3'>
        <h4 className='text-sm font-medium'>
          {t('Drop reasons since startup')}
        </h4>
        <dl className='grid grid-cols-2 gap-x-4 gap-y-2 text-sm'>
          {drops.map((item) => (
            <div
              key={item.label}
              className='flex flex-wrap justify-between gap-2'
            >
              <dt className='text-muted-foreground'>{item.label}</dt>
              <dd className='font-mono tabular-nums'>{item.value}</dd>
            </div>
          ))}
        </dl>
      </div>
      <dl className='space-y-2 border-t pt-3 text-xs'>
        <div>
          <dt className='text-muted-foreground'>
            {t('Last successful timeline write')}
          </dt>
          <dd className='font-mono break-all'>
            {status.last_written_at_us === null
              ? '—'
              : formatTraceTimestamp(status.last_written_at_us)}
          </dd>
        </div>
        <div>
          <dt className='text-muted-foreground'>
            {t('Last failed timeline write')}
          </dt>
          <dd className='font-mono break-all'>
            {status.last_failed_at_us === null
              ? '—'
              : formatTraceTimestamp(status.last_failed_at_us)}
          </dd>
        </div>
      </dl>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Outstanding includes the queue, preparation, buffered batches, and writing or retrying. Wait starts at enqueue; batch duration includes retries.'
        )}
      </p>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Accepted, written, failed, and dropped totals count records. Failures are final batch failures; retries count repeated batch write calls.'
        )}
      </p>
      {status.cleanup_pending && (
        <p className='text-muted-foreground text-xs'>{t('Cleaning...')}</p>
      )}
    </div>
  )
}

export function RequestTimelineWriterStatusPanel(props: {
  embedded?: boolean
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  if (props.embedded) {
    return (
      <section
        className='border-t pt-4'
        aria-label={t('Timeline collection and storage status')}
      >
        <h4 className='text-sm font-medium'>
          {t('Timeline collection and storage status')}
        </h4>
        <WriterStatusContent />
      </section>
    )
  }
  return (
    <Collapsible open={open} onOpenChange={setOpen} className='border-y py-3'>
      <CollapsibleTrigger
        render={
          <Button
            type='button'
            variant='ghost'
            className='w-full justify-between'
          />
        }
      >
        {t('Timeline collection and storage status')}
        <ChevronDown
          className={cn('size-4', open && 'rotate-180')}
          aria-hidden='true'
        />
      </CollapsibleTrigger>
      <CollapsibleContent>{open && <WriterStatusContent />}</CollapsibleContent>
    </Collapsible>
  )
}
