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
import { RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { StatusBadge, type StatusVariant } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

import type { RequestTimelineResult } from '../api'
import {
  formatTraceDuration,
  formatTraceTimestamp,
} from '../lib/timeline-scale'
import { TIMELINE_KIND_BG } from '../lib/timeline-style'
import { getOutcomeLabel } from '../lib/timeline-text'

type TraceSummary = NonNullable<RequestTimelineResult['summary']>

const OUTCOME_VARIANT: Record<string, StatusVariant> = {
  success: 'success',
  failed: 'danger',
  panic: 'danger',
  canceled: 'warning',
  timeout: 'warning',
}

export function TimelineHeader(props: {
  result: RequestTimelineResult
  summary: TraceSummary
  admin: boolean
  refreshing: boolean
  onRefresh: () => void
}) {
  const { t } = useTranslation()
  const summary = props.summary
  const detail = props.admin ? props.result.detail : null
  return (
    <div className='flex flex-wrap items-center gap-2'>
      <span className='min-w-0 truncate font-mono text-xs'>
        {summary.request_id}
      </span>
      <CopyButton value={summary.request_id} className='size-7' />
      <StatusBadge
        label={getOutcomeLabel(t, summary.outcome)}
        variant={OUTCOME_VARIANT[summary.outcome] ?? 'neutral'}
        copyable={false}
      />
      {summary.http_status !== null && (
        <StatusBadge label={`HTTP ${summary.http_status}`} copyable={false} />
      )}
      {summary.is_stream !== null && (
        <StatusBadge
          label={summary.is_stream ? t('Stream') : t('Non-stream')}
          copyable={false}
        />
      )}
      {detail?.timeline && (
        <StatusBadge
          label={`${detail.timeline.method} ${detail.timeline.route}`}
          className='font-mono'
          copyable={false}
        />
      )}
      {detail && detail.attempt_count > 1 && (
        <StatusBadge
          label={t('Retried {{count}} times', {
            count: detail.attempt_count - 1,
          })}
          variant='warning'
          copyable={false}
        />
      )}
      {summary.capture_status !== 'complete' && (
        <StatusBadge label={t('Some details are missing')} copyable={false} />
      )}
      <Button
        variant='ghost'
        size='icon'
        className='ml-auto size-7'
        aria-label={t('Refresh')}
        onClick={props.onRefresh}
        disabled={props.refreshing}
      >
        <RefreshCw
          className={cn('size-4', props.refreshing && 'animate-spin')}
        />
      </Button>
    </div>
  )
}

function Metric(props: {
  label: string
  value: number | null
  swatch?: string
}) {
  return (
    <div className='min-w-0 py-2.5 sm:px-4 sm:first:pl-0 sm:[&+&]:border-l'>
      <dt className='text-muted-foreground flex items-center gap-1.5 text-xs whitespace-nowrap'>
        {props.swatch && (
          <i className={cn('size-2 rounded-sm', props.swatch)} />
        )}
        {props.label}
      </dt>
      <dd className='overflow-x-auto font-mono text-lg font-semibold whitespace-nowrap tabular-nums'>
        {formatTraceDuration(props.value)}
      </dd>
    </div>
  )
}

export function TimelineMetrics(props: {
  result: RequestTimelineResult
  summary: TraceSummary
  admin: boolean
  channelStart: number | null
}) {
  const { t } = useTranslation()
  const detail = props.admin ? props.result.detail : null
  return (
    <>
      <dl
        className={cn(
          'grid grid-cols-2 border-y',
          detail ? 'sm:grid-cols-4' : 'sm:grid-cols-2'
        )}
      >
        <Metric
          label={t('Server processing time')}
          value={props.summary.duration_us}
        />
        <Metric
          label={t('First output')}
          value={props.summary.first_payload_write_offset_us}
          swatch={TIMELINE_KIND_BG.response}
        />
        {detail && (
          <>
            <Metric
              label={
                detail.attempt_count > 1
                  ? t('Final response received')
                  : t('First response received')
              }
              value={detail.final_attempt_first_byte_offset_us}
              swatch={TIMELINE_KIND_BG.upstream}
            />
            <Metric
              label={t('Prepare request')}
              value={props.channelStart}
              swatch={TIMELINE_KIND_BG.gateway}
            />
          </>
        )}
      </dl>
      <div className='text-muted-foreground grid min-w-0 gap-1 text-xs'>
        <p>
          {t('Request entry')} ·{' '}
          <span className='font-mono break-all'>
            {formatTraceTimestamp(props.summary.started_at_us)}
          </span>
        </p>
        <p>
          {t('Server processing ended')} ·{' '}
          <span className='font-mono break-all'>
            {formatTraceTimestamp(props.summary.finished_at_us)}
          </span>
        </p>
      </div>
    </>
  )
}

/** 普通用户只看到首次写出前后两段，不暴露渠道和网关内部阶段。 */
export function TimelineUserSplit(props: { summary: TraceSummary }) {
  const { t } = useTranslation()
  const total = Math.max(props.summary.duration_us, 1)
  const firstWrite = props.summary.first_payload_write_offset_us
  return (
    <div className='space-y-2'>
      {firstWrite !== null && (
        <>
          <div className='flex h-3 gap-0.5' aria-hidden='true'>
            <i
              className={cn('rounded-sm', TIMELINE_KIND_BG.upstream)}
              style={{ width: `${(firstWrite / total) * 100}%` }}
            />
            <i className={cn('flex-1 rounded-sm', TIMELINE_KIND_BG.response)} />
          </div>
          <div className='text-muted-foreground flex justify-between gap-3 text-xs'>
            <span>
              {t('Before first output: {{time}}', {
                time: formatTraceDuration(firstWrite),
              })}
            </span>
            <span>
              {t('After first output: {{time}}', {
                time: formatTraceDuration(
                  props.summary.duration_us - firstWrite
                ),
              })}
            </span>
          </div>
        </>
      )}
      <p className='text-muted-foreground text-sm'>
        {t('Times are recorded by the server.')}
      </p>
    </div>
  )
}
