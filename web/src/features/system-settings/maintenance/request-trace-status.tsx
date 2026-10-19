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
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Progress } from '@/components/ui/progress'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { SettingsControlGroup } from '../components/settings-form-layout'
import type { LogCleanupTask } from '../types'

type TraceStatus = {
  node: string
  running: boolean
  queued_rows: number
  queued_bytes: number
  queue_row_limit: number
  queue_byte_limit: number
  in_flight_rows: number
  persisted_rows: number
  failed_rows: number
  dropped_rows?: number
  dropped_priority_rows?: number
  dropped_regular_rows?: number
  retries: number
  collected_requests: number
  sampled_out_requests: number
  partial_requests: number
  average_write_ms: number
  last_write_ms: number
  cleanup?: LogCleanupTask
  cleanup_error?: string
}

export function RequestTraceStatus() {
  const { t } = useTranslation()
  const userID = useAuthStore((state) => state.auth.user?.id)
  const query = useQuery({
    queryKey: ['request-trace-status', userID],
    enabled: !!userID,
    queryFn: async ({ signal }): Promise<TraceStatus> => {
      const response = await api.get<{
        success: boolean
        data?: TraceStatus
        message?: string
      }>('/api/log/trace/status', { signal })
      if (
        !response.data.success ||
        !response.data.data ||
        typeof response.data.data.persisted_rows !== 'number'
      ) {
        throw new Error(
          response.data.message || t('Failed to load trace writer status')
        )
      }
      return response.data.data
    },
    refetchInterval: 5000,
    refetchOnWindowFocus: false,
  })
  const status = query.data
  const cleanup = status?.cleanup
  const cleanupActive =
    cleanup?.status === 'pending' || cleanup?.status === 'running'
  const cleanupStatus = {
    pending: t('Pending'),
    running: t('Running'),
    succeeded: t('Succeeded'),
    failed: t('Failed'),
  }
  const metrics = status
    ? [
        [
          t('Pending trace rows'),
          `${status.queued_rows.toLocaleString()} / ${status.queue_row_limit.toLocaleString()}`,
        ],
        [
          t('Pending metadata memory'),
          `${(status.queued_bytes / 1048576).toFixed(1)} / ${status.queue_byte_limit / 1048576} MiB`,
        ],
        [t('Trace rows being written'), status.in_flight_rows.toLocaleString()],
        [t('Persisted trace rows'), status.persisted_rows.toLocaleString()],
        [t('Failed trace rows'), status.failed_rows.toLocaleString()],
        [t('Dropped trace rows'), status.dropped_rows?.toLocaleString() ?? '—'],
        [
          t('Trace rows dropped for failed or diagnostic requests'),
          status.dropped_priority_rows?.toLocaleString() ?? '—',
        ],
        [
          t('Trace rows dropped for regular requests'),
          status.dropped_regular_rows?.toLocaleString() ?? '—',
        ],
        [t('Trace write retries'), status.retries.toLocaleString()],
        [
          t('Sampled-out successful requests'),
          status.sampled_out_requests.toLocaleString(),
        ],
        [
          t('Requests with partial metadata'),
          status.partial_requests.toLocaleString(),
        ],
        [
          t('Average batch write time'),
          `${status.average_write_ms.toFixed(1)} ms`,
        ],
        [t('Last batch write time'), `${status.last_write_ms.toFixed(1)} ms`],
      ]
    : []
  return (
    <SettingsControlGroup
      role='region'
      aria-label={t('Asynchronous trace writer')}
    >
      <div className='flex flex-wrap items-center gap-2'>
        <h4 className='text-sm font-medium'>
          {t('Asynchronous trace writer')}
        </h4>
        {status && (
          <StatusBadge
            label={status.running ? t('Running') : t('Stopped')}
            variant={status.running ? 'green' : 'neutral'}
            copyable={false}
          />
        )}
        {status?.node && (
          <span className='text-muted-foreground text-xs'>
            {t('Responding instance')}: {status.node}
          </span>
        )}
      </div>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Counters are local to this instance and reset on restart. Pending rows include writes in flight. A full queue drops new trace metadata without blocking requests. Pending in-memory traces may be lost if the process crashes.'
        )}
      </p>
      {query.isPending && <LoadingState className='min-h-0 py-3' />}
      <p className='text-muted-foreground text-xs'>
        {t(
          'Some queue capacity is reserved for failed or diagnostic requests. Priority traces can still be dropped when available capacity is exhausted.'
        )}
      </p>
      {query.isError && (
        <ErrorState
          title={t('Failed to load trace writer status')}
          className='min-h-0 py-3'
          onRetry={() => void query.refetch()}
        />
      )}
      {status && (
        <dl className='grid gap-x-6 gap-y-3 sm:grid-cols-2 lg:grid-cols-3'>
          {metrics.map(([label, value]) => (
            <div key={label}>
              <dt className='text-muted-foreground text-xs'>{label}</dt>
              <dd className='text-sm tabular-nums'>{value}</dd>
            </div>
          ))}
        </dl>
      )}
      {cleanup && (
        <div className='flex flex-col gap-2'>
          <p className='text-muted-foreground text-xs'>
            {t('Request trace cleanup')}: {cleanupStatus[cleanup.status]} ·{' '}
            {t('{{count}} trace records removed.', {
              count:
                cleanup.state?.trace_processed ??
                cleanup.result?.trace_deleted_count ??
                0,
            })}
          </p>
          {cleanupActive && (
            <Progress
              value={Math.min(100, Math.max(0, cleanup.state?.progress ?? 0))}
            />
          )}
          {cleanup.error && (
            <p className='text-destructive text-xs'>{cleanup.error}</p>
          )}
        </div>
      )}
      {status?.cleanup_error && (
        <p className='text-destructive text-xs'>
          {t('Request trace cleanup')}: {status.cleanup_error}
        </p>
      )}
    </SettingsControlGroup>
  )
}
