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
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { useAuthStore } from '@/stores/auth-store'

import {
  getRequestTimeline,
  isRequestId,
  type RequestTimelineData,
  type RequestTimelineResult,
} from './api'
import { TimelineChart } from './components/timeline-chart'
import { TimelineIssues } from './components/timeline-issues'
import {
  TimelineHeader,
  TimelineMetrics,
  TimelineUserSplit,
} from './components/timeline-summary'
import { getAttemptResponseOffset, getChannelStart } from './lib/timeline-model'
import { buildRequestSegments } from './lib/timeline-segments'

const EMPTY_TIMELINE_RECHECK_DELAYS = [500, 1000, 2000] as const

/** 按关键时刻切段，点选时高亮对应的时间块。 */
function TimelineStages(props: {
  result: RequestTimelineResult
  data: RequestTimelineData
  total: number
}) {
  const { t } = useTranslation()
  let firstByte =
    props.result.detail?.final_attempt_first_byte_offset_us ?? null
  if ((props.data.measurement_version ?? 1) >= 3) {
    firstByte = getAttemptResponseOffset(
      props.data.attempts.at(-1),
      props.data.capture.truncated,
      props.result.detail?.attempt_count
    )
  }
  const firstWrite = props.result.summary?.first_payload_write_offset_us ?? null
  const stream = props.result.summary?.is_stream === true
  const timeline = useMemo(
    () =>
      buildRequestSegments(t, props.data, {
        total: props.total,
        firstByte,
        firstWrite,
        stream,
      }),
    [t, props.data, props.total, firstByte, firstWrite, stream]
  )
  const [segmentKey, setSegmentKey] = useState<string | null>(null)
  const segment = timeline.segments.find((item) => item.key === segmentKey)
  const toggleSegment = useCallback(
    (key: string) => setSegmentKey((current) => (current === key ? null : key)),
    []
  )
  return (
    <TimelineChart
      segments={timeline.segments}
      moments={timeline.moments}
      total={props.total}
      selectedKey={segment?.key ?? null}
      onSelect={toggleSegment}
    />
  )
}

export function RequestTimeline(props: {
  requestId: string
  admin: boolean
  enabled: boolean
}) {
  const { t } = useTranslation()
  const viewerId = useAuthStore((state) => state.auth.user?.id)
  const recheckScope = `${props.requestId}:${props.admin}:${viewerId ?? ''}`
  const [automaticRecheck, setAutomaticRecheck] = useState(() => ({
    scope: recheckScope,
    count: 0,
  }))
  const recheckCount =
    automaticRecheck.scope === recheckScope ? automaticRecheck.count : 0
  const query = useQuery({
    queryKey: [
      'request-timeline',
      props.requestId,
      props.admin ? 'admin' : 'self',
      viewerId,
    ],
    queryFn: ({ signal }) =>
      getRequestTimeline(props.requestId, props.admin, signal),
    enabled: props.enabled && isRequestId(props.requestId),
    retry: false,
    staleTime: (current) => (current.state.data?.found ? 30_000 : 0),
    refetchOnWindowFocus: false,
  })
  const refetch = query.refetch
  const canAutoRecheck =
    props.enabled &&
    !query.isFetching &&
    !query.isError &&
    query.data?.found === false &&
    query.data.enabled &&
    recheckCount < EMPTY_TIMELINE_RECHECK_DELAYS.length
  useEffect(() => {
    if (!canAutoRecheck) return
    const delay = EMPTY_TIMELINE_RECHECK_DELAYS[recheckCount]
    if (delay === undefined) return
    const timer = window.setTimeout(() => {
      setAutomaticRecheck({ scope: recheckScope, count: recheckCount + 1 })
      void refetch()
    }, delay)
    return () => window.clearTimeout(timer)
  }, [canAutoRecheck, recheckCount, recheckScope, refetch])
  const refresh = useCallback(() => {
    setAutomaticRecheck({ scope: recheckScope, count: 0 })
    void refetch()
  }, [recheckScope, refetch])
  if (!isRequestId(props.requestId)) {
    return <EmptyState title={t('No request ID available')} />
  }
  if (query.isPending) {
    return <LoadingState message={t('Loading request timeline...')} />
  }
  if (query.isError) {
    return (
      <ErrorState
        title={t('Unable to load request timeline')}
        action={
          <Button type='button' disabled={query.isFetching} onClick={refresh}>
            {t('Retry')}
          </Button>
        }
      />
    )
  }
  const result = query.data
  const summary = result.summary
  if (!result.found || !summary) {
    return (
      <EmptyState
        title={t('No request trace found')}
        description={t(
          'The trace may not have been collected, may still be pending, or may have expired. Retention: {{days}} days.',
          { days: result.retention_days }
        )}
        action={
          <div className='space-y-3 text-center'>
            {(canAutoRecheck || query.isFetching) && (
              <p className='text-muted-foreground text-sm' role='status'>
                {t('Checking briefly for a newly written timeline...')}
              </p>
            )}
            {!query.isFetching &&
              recheckCount >= EMPTY_TIMELINE_RECHECK_DELAYS.length && (
                <p className='text-muted-foreground text-sm' role='status'>
                  {t('Automatic rechecks finished. You can refresh manually.')}
                </p>
              )}
            {!result.enabled && (
              <p className='text-muted-foreground text-sm'>
                {t('Request trace collection is currently disabled')}
              </p>
            )}
            <Button
              type='button'
              variant='outline'
              disabled={query.isFetching}
              onClick={refresh}
            >
              {t('Refresh')}
            </Button>
          </div>
        }
      />
    )
  }
  const data = props.admin ? result.detail?.timeline : null
  const channelStart = data ? getChannelStart(data) : null

  return (
    <div className='min-w-0 space-y-4'>
      <TimelineHeader
        result={result}
        summary={summary}
        admin={props.admin}
        refreshing={query.isFetching}
        onRefresh={refresh}
      />
      <TimelineMetrics
        result={result}
        summary={summary}
        admin={props.admin}
        channelStart={channelStart}
      />
      <TimelineIssues result={result} admin={props.admin} />
      {!props.admin && <TimelineUserSplit summary={summary} />}
      {props.admin && result.detail && !data && (
        <Alert>
          <AlertDescription>
            {t(
              'The summary is available, but this timeline format cannot be displayed.'
            )}
          </AlertDescription>
        </Alert>
      )}
      {data && (
        <TimelineStages
          key={summary.request_id}
          result={result}
          data={data}
          total={summary.duration_us}
        />
      )}
    </div>
  )
}
