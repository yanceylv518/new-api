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
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useAuthStore } from '@/stores/auth-store'

import {
  getRequestTimeline,
  isRequestId,
  type RequestTimelineData,
  type RequestTimelineResult,
} from './api'
import { TimelineAttempts } from './components/timeline-attempts'
import { TimelineChart } from './components/timeline-chart'
import { TimelineInspector } from './components/timeline-inspector'
import { TimelineIssues } from './components/timeline-issues'
import { TimelineRecords } from './components/timeline-records'
import {
  TimelineHeader,
  TimelineMetrics,
  TimelineUserSplit,
} from './components/timeline-summary'
import { buildTraceEvents, type TraceEventRow } from './lib/timeline-events'
import {
  buildTimelineGroups,
  flattenTimeline,
  getChannelStart,
} from './lib/timeline-model'
import { buildTimelineSegments } from './lib/timeline-scale'

const EMPTY_TIMELINE_RECHECK_DELAYS = [500, 1000, 2000] as const

function TimelineEvents(props: {
  result: RequestTimelineResult
  data: RequestTimelineData
  onSelect: (id: string) => void
}) {
  const { t } = useTranslation()
  const rows = useMemo(
    () =>
      props.result.summary
        ? buildTraceEvents(t, props.data, props.result.summary)
        : [],
    [t, props.data, props.result.summary]
  )
  return <TimelineRecords rows={rows} events onSelect={props.onSelect} />
}

/** 所有视图共用阶段模型和索引，只有事件视图挂载时才投影事件列表。 */
function TimelineStages(props: {
  result: RequestTimelineResult
  data: RequestTimelineData
  total: number
}) {
  const { t } = useTranslation()
  const model = useMemo(() => {
    const groups = buildTimelineGroups(t, props.data, props.total)
    const rows = flattenTimeline(groups, new Set())
    const index = new Map(rows.map(({ node }) => [node.id, node]))
    const stages: TraceEventRow[] = rows
      .filter(({ node }) => !node.derived)
      .map(({ node }) => ({
        id: node.id,
        label: node.label,
        code: node.stage ?? node.id,
        start: node.start,
        end: node.end,
        nodeId: node.id,
      }))
    return { groups, index, stages }
  }, [t, props.data, props.total])
  const finalAttempt = props.data.attempts.at(-1)
  const finalExchange = finalAttempt?.exchanges?.at(-1)
  const waitId = [
    'exchange',
    finalAttempt?.index,
    finalExchange?.index,
    'wait',
  ].join('-')
  const defaultId = model.index.get(waitId)?.id ?? model.groups[0]?.id ?? null
  const [selectedId, setSelectedId] = useState<string | null>(defaultId)
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(
    () =>
      new Set(
        model.groups
          .filter((group) => group.attempt && group.attempt !== finalAttempt)
          .map((group) => group.id)
      )
  )
  const [view, setView] = useState('waterfall')
  const [window, setWindow] = useState<{ start: number; end: number } | null>(
    null
  )
  const selected =
    model.index.get(selectedId ?? '') ?? model.index.get(defaultId ?? '')
  const segments = useMemo(
    () => buildTimelineSegments(props.total, window),
    [props.total, window]
  )
  const firstByte =
    props.result.detail?.final_attempt_first_byte_offset_us ?? null
  const firstWrite = props.result.summary?.first_payload_write_offset_us ?? null
  const markers = useMemo(
    () =>
      [
        { value: firstByte, className: 'border-warning' },
        { value: firstWrite, className: 'border-success' },
      ].filter((marker): marker is { value: number; className: string } =>
        Number.isFinite(marker.value)
      ),
    [firstByte, firstWrite]
  )
  const toggle = useCallback((id: string) => {
    setCollapsed((previous) => {
      const next = new Set(previous)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])
  return (
    <>
      <Tabs
        value={view}
        onValueChange={(value) => {
          if (typeof value === 'string') {
            setView(value)
          }
        }}
      >
        <TabsList
          aria-label={t('Timeline view')}
          className='max-w-full flex-wrap group-data-horizontal/tabs:h-auto'
        >
          <TabsTrigger value='waterfall'>{t('Waterfall')}</TabsTrigger>
          <TabsTrigger value='stages'>{t('Stage details')}</TabsTrigger>
          <TabsTrigger value='events'>{t('Event list')}</TabsTrigger>
        </TabsList>
        <TabsContent value='waterfall'>
          {view === 'waterfall' && (
            <TimelineChart
              groups={model.groups}
              segments={segments}
              total={props.total}
              markers={markers}
              selectedId={selected?.id ?? null}
              collapsed={collapsed}
              onSelect={setSelectedId}
              onToggle={toggle}
              focused={window !== null}
              canFocus={selected?.end != null && selected.end > selected.start}
              onFocus={() => {
                if (selected?.end != null && selected.end > selected.start) {
                  setWindow({ start: selected.start, end: selected.end })
                }
              }}
              onReset={() => setWindow(null)}
            />
          )}
        </TabsContent>
        <TabsContent value='stages'>
          {view === 'stages' && (
            <TimelineRecords rows={model.stages} onSelect={setSelectedId} />
          )}
        </TabsContent>
        <TabsContent value='events'>
          {view === 'events' && (
            <TimelineEvents
              result={props.result}
              data={props.data}
              onSelect={setSelectedId}
            />
          )}
        </TabsContent>
      </Tabs>
      {selected && <TimelineInspector node={selected} total={props.total} />}
      <TimelineAttempts attempts={props.data.attempts} />
    </>
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
