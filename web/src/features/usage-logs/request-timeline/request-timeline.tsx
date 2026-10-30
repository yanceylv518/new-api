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
import dayjs from 'dayjs'
import { useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { useAuthStore } from '@/stores/auth-store'

import {
  DetailRow,
  DetailSection,
} from '../components/dialogs/log-detail-layout'
import {
  getRequestTimeline,
  type TimelineData,
  type TimelineResponse,
} from './api'

export function RequestTimeline(props: { requestId: string }) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState(false)
  const user = useAuthStore((state) => state.auth.user)
  if (!user || user.role < 10 || !props.requestId) return null

  return (
    <Collapsible
      open={expanded}
      onOpenChange={setExpanded}
      className='min-w-0 space-y-2'
    >
      <CollapsibleTrigger
        render={<Button variant='outline' className='w-full justify-between' />}
      >
        {t('Request timeline')}
        <span aria-hidden='true'>{expanded ? '−' : '+'}</span>
      </CollapsibleTrigger>
      <CollapsibleContent>
        {expanded && (
          <TimelineQuery
            requestId={props.requestId}
            userId={user.id}
            role={user.role}
          />
        )}
      </CollapsibleContent>
    </Collapsible>
  )
}

function TimelineQuery(props: {
  requestId: string
  userId: number
  role: number
}) {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: ['request-timeline', props.userId, props.role, props.requestId],
    queryFn: ({ signal }) => getRequestTimeline(props.requestId, signal),
    retry: false,
    gcTime: 0,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    // The diagnostic write can lag behind the consumption log. Retry only
    // three times, then leave an explicit missing state and manual refresh.
    refetchInterval: (state) => {
      const data = state.state.data
      if (
        !state.state.error &&
        data?.enabled &&
        data.status === 'missing' &&
        state.state.dataUpdateCount < 4
      ) {
        return 1000
      }
      return false
    },
  })
  if (query.isPending) return <LoadingState size='sm' className='min-h-24' />
  if (query.isError) {
    return (
      <ErrorState
        className='min-h-24'
        title={t('Unable to load request timeline')}
        onRetry={() => void query.refetch()}
      />
    )
  }
  const data = query.data
  if (data.status !== 'available' || !data.timeline || !data.record) {
    let description = t(
      'Timeline unavailable. It may not have been collected, may have expired, or may have been dropped when storage was busy.'
    )
    if (data.status === 'unavailable') {
      description = t(
        'Timeline storage is unavailable. Existing request logs are unaffected.'
      )
    }
    if (
      data.status === 'invalid_data' ||
      data.status === 'unsupported_version'
    ) {
      description = t('This timeline cannot be displayed by this version.')
    }
    return (
      <EmptyState
        className='min-h-24'
        title={t('No timeline data')}
        description={description}
        action={
          <Button
            size='sm'
            variant='outline'
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t('Refresh')}
          </Button>
        }
      />
    )
  }
  return <TimelineDetails timeline={data.timeline} record={data.record} />
}

function TimelineNode(props: {
  label: string
  offset: number | null
  previous?: number | null
  start: number
  valid: boolean
  children: ReactNode
}) {
  const { t } = useTranslation()
  let elapsed = t('Not recorded')
  if (props.offset != null && props.valid) {
    elapsed = `+${props.offset.toLocaleString()} ms`
  }
  let gap: string | null = null
  if (props.valid && props.offset != null && props.previous != null) {
    gap = t('Since previous point: {{ms}} ms', {
      ms: (props.offset - props.previous).toLocaleString(),
    })
  }
  return (
    <li className='border-border relative min-w-0 border-l-2 pb-5 pl-4 last:pb-0'>
      <span
        aria-hidden='true'
        className='bg-primary absolute top-1 -left-1.5 size-2.5 rounded-full'
      />
      <div className='mb-2 flex flex-wrap items-baseline justify-between gap-1 text-xs'>
        <span className='font-semibold'>{props.label}</span>
        <span
          className='font-mono tabular-nums'
          title={
            props.offset == null
              ? undefined
              : dayjs(props.start + props.offset).format(
                  'YYYY-MM-DD HH:mm:ss.SSS'
                )
          }
        >
          {elapsed}
        </span>
      </div>
      {gap && <p className='text-muted-foreground mb-2 text-xs'>{gap}</p>}
      <div className='min-w-0 space-y-1'>{props.children}</div>
    </li>
  )
}

function TimelineDetails(props: {
  timeline: TimelineData
  record: NonNullable<TimelineResponse['record']>
}) {
  const { t } = useTranslation()
  const data = props.timeline
  const record = props.record
  const absent = t('Not recorded')
  const point = { start: data.started_at_ms, valid: data.timing_valid }
  let result = t('Unconfirmed end')
  let variant: 'green' | 'red' | 'amber' | 'neutral' = 'neutral'
  if (record.result === 'success') {
    result = t('Completed')
    variant = 'green'
  }
  if (record.result === 'failed') {
    result = t('Failed')
    variant = 'red'
  }
  if (record.result === 'interrupted') {
    result = t('Interrupted')
    variant = 'amber'
  }
  let chunkType = data.first_chunk_type || absent
  if (chunkType === 'role') chunkType = t('Role information (not text)')
  if (chunkType === 'text') chunkType = t('Text')
  if (chunkType === 'tool_call') chunkType = t('Tool call')
  if (chunkType === 'reasoning') chunkType = t('Reasoning')
  if (chunkType === 'error') chunkType = t('Error')
  if (chunkType === 'metadata') chunkType = t('Response metadata')

  const duration = (from: number | null, to: number | null): string => {
    if (!data.timing_valid || from == null || to == null || to < from) {
      return absent
    }
    return `${(to - from).toLocaleString()} ms`
  }
  const selected = JSON.stringify({ ...record, ...data }, null, 2)
  return (
    <div className='min-w-0 space-y-4 rounded-md border p-3'>
      <div className='flex flex-wrap items-center gap-2'>
        <StatusBadge label={result} variant={variant} copyable={false} />
        {record.upstream_status != null && (
          <StatusBadge
            label={`HTTP ${record.upstream_status}`}
            variant='neutral'
            copyable={false}
          />
        )}
      </div>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Recorded inside this service. A completed write does not confirm receipt by the client.'
        )}
      </p>
      {!data.timing_valid && (
        <p role='status' className='text-destructive text-xs'>
          {t(
            'Timing order is invalid; durations are hidden. Use the recorded fields to investigate.'
          )}
        </p>
      )}
      <DetailSection label={t('First-response breakdown')}>
        <DetailRow
          label={t('Before sending')}
          value={duration(0, data.request_sent_ms)}
          mono
        />
        <DetailRow
          label={t('Waiting for upstream')}
          value={duration(data.request_sent_ms, data.first_response_ms)}
          mono
        />
        <DetailRow
          label={t('Forwarding first response')}
          value={duration(data.first_response_ms, data.first_write_ms)}
          mono
        />
        <DetailRow
          label={t('Existing first-response time')}
          value={duration(0, data.first_response_ms)}
          mono
        />
        <DetailRow
          label={t('Total output time')}
          value={duration(0, data.ended_ms)}
          mono
        />
      </DetailSection>
      <ol aria-label={t('Request timeline')} className='ml-1 min-w-0'>
        <TimelineNode {...point} label={t('Processing started')} offset={0}>
          <DetailRow
            label={t('Time')}
            value={dayjs(data.started_at_ms).format('YYYY-MM-DD HH:mm:ss.SSS')}
            mono
          />
          <DetailRow
            label={t('Request ID')}
            value={
              <span className='inline-flex max-w-full items-start gap-1'>
                <span className='min-w-0 break-all'>{record.request_id}</span>
                <CopyButton value={record.request_id} className='size-5' />
              </span>
            }
          />
          <DetailRow label={t('Model')} value={data.request_model || absent} />
        </TimelineNode>
        <TimelineNode
          {...point}
          label={t('Request sent to upstream')}
          offset={data.request_sent_ms}
          previous={0}
        >
          <DetailRow
            label={t('Request body size')}
            value={
              record.upstream_request_body_bytes == null
                ? t('Unknown')
                : `${record.upstream_request_body_bytes.toLocaleString()} B`
            }
            mono
          />
          <p className='text-muted-foreground text-xs'>
            {t('Size of the final upstream request body, excluding headers.')}
          </p>
        </TimelineNode>
        <TimelineNode
          {...point}
          label={t('First upstream response received')}
          offset={data.first_response_ms}
          previous={data.request_sent_ms}
        >
          <DetailRow label={t('First response type')} value={chunkType} />
          <DetailRow
            label={t('Upstream Request ID')}
            value={data.upstream_request_id || absent}
            mono
          />
          <DetailRow
            label={t('Response ID')}
            value={data.response_id || absent}
            mono
          />
          <DetailRow
            label={t('Returned model')}
            value={data.response_model || absent}
          />
          <p className='text-muted-foreground text-xs'>
            {t(
              'Uses the existing first-response endpoint; the first response may contain role information rather than text.'
            )}
          </p>
        </TimelineNode>
        <TimelineNode
          {...point}
          label={t('First response write completed')}
          offset={data.first_write_ms}
          previous={data.first_response_ms}
        >
          <p className='text-muted-foreground text-xs'>
            {t(
              'First payload write and flush completed. Heartbeats are excluded.'
            )}
          </p>
        </TimelineNode>
        <TimelineNode
          {...point}
          label={t('Output ended')}
          offset={data.ended_ms}
          previous={data.first_write_ms}
        >
          <DetailRow label={t('Status')} value={result} />
          <DetailRow
            label={t('End Reason')}
            value={data.end_reason || absent}
          />
          <DetailRow
            label={t('Finish reason')}
            value={data.finish_reason || absent}
          />
          <DetailRow
            label={t('Upstream end marker')}
            value={data.end_marker_received ? t('End marker received') : t('Not received')}
          />
          <DetailRow
            label={t('Returned token usage')}
            value={
              data.returned_usage
                ? t('Input {{input}} · Output {{output}} · Total {{total}}', {
                    input: data.returned_usage.input_tokens,
                    output: data.returned_usage.output_tokens,
                    total: data.returned_usage.total_tokens,
                  })
                : t('Not returned by upstream')
            }
          />
        </TimelineNode>
      </ol>
      {data.first_error && (
        <DetailSection variant='danger' label={t('First observed error')}>
          <DetailRow
            label={t('Time')}
            value={
              data.timing_valid
                ? `+${data.first_error.offset_ms.toLocaleString()} ms`
                : absent
            }
            mono
          />
          <DetailRow label={t('Stage')} value={data.first_error.stage} />
          <DetailRow label={t('Type')} value={data.first_error.kind} />
          <DetailRow
            label={t('Error Code')}
            value={data.first_error.code || absent}
            mono
          />
          <p className='min-w-0 text-xs break-all whitespace-pre-wrap'>
            {data.first_error.message}
          </p>
          {data.first_error.message_truncated && (
            <p className='text-muted-foreground text-xs'>
              {t('Error message was truncated.')}
            </p>
          )}
        </DetailSection>
      )}
      <Collapsible className='min-w-0'>
        <CollapsibleTrigger render={<Button size='sm' variant='ghost' />}>
          {t('View recorded fields')}
        </CollapsibleTrigger>
        <CollapsibleContent>
          <div className='flex justify-end'>
            <CopyButton
              value={selected}
              aria-label={t('Copy recorded fields')}
            />
          </div>
          <pre className='bg-muted max-h-64 min-w-0 overflow-auto rounded p-2 text-xs break-all whitespace-pre-wrap'>
            {selected}
          </pre>
        </CollapsibleContent>
      </Collapsible>
    </div>
  )
}
