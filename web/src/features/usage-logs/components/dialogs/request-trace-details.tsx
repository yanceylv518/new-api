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

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge, type StatusBadgeProps } from '@/components/status-badge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { cn } from '@/lib/utils'

import type { UsageLog } from '../../data/schema'
import type {
  RequestTrace,
  useRequestTraces,
} from '../../hooks/use-request-traces'
import { parseLogOther } from '../../lib/format'
import { DetailContent, DetailRow, DetailSection } from './log-detail-layout'

function UpstreamUsageDetails(props: {
  record: RequestTrace
  showAttempt: boolean
}) {
  const { t } = useTranslation()
  const usage = props.record.admin_info?.upstream.usage
  if (!usage) return null
  const visibleParts = usage.parts.filter(
    (part) => part.raw_value.trim() !== 'null'
  )
  return (
    <div className='min-w-0 space-y-1.5'>
      <Label className='text-xs font-semibold'>{t('Upstream usage')}</Label>
      {props.showAttempt && (
        <p className='text-muted-foreground text-xs'>
          {t('Attempt {{number}}', { number: props.record.attempt + 1 })}
        </p>
      )}
      {usage.incomplete && (
        <StatusBadge
          label={t('Upstream usage was not captured completely.')}
          variant='orange'
          copyable={false}
        />
      )}
      {visibleParts.length === 0 && !usage.incomplete && (
        <p className='text-muted-foreground text-xs'>
          {t('Upstream usage was not reported.')}
        </p>
      )}
      {visibleParts.map((part) => (
        <DetailContent
          key={`${part.event ?? ''}:${part.path}:${part.raw_value}`}
          value={part.raw_value}
          mono
          scrollLabel={t('Upstream usage')}
        />
      ))}
    </div>
  )
}

function formatTraceBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${Number((bytes / 1024).toFixed(1))} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

interface TraceAttemptDetailsProps {
  record?: RequestTrace
  log: UsageLog
  isAdmin: boolean
}

const TraceAttemptDetails = memo(function TraceAttemptDetails(
  props: TraceAttemptDetailsProps
) {
  const { t } = useTranslation()
  const summary = props.record?.trace
  const admin = props.isAdmin ? props.record?.admin_info : undefined
  const summaryMetadata = admin?.metadata_level === 'summary'
  const shape = summaryMetadata ? undefined : admin?.shape
  const upstream = admin?.upstream
  const response = admin?.resp
  let endMarkerLabel = t('End marker missing')
  if (response?.end_marker) {
    endMarkerLabel = response.end_marker
  } else if (response?.end_marker_policy === 'not_applicable') {
    endMarkerLabel = t('This protocol has no separate end marker')
  } else if (response?.done) {
    endMarkerLabel = t('End marker received (type not recorded)')
  } else if (response?.end_marker_policy !== 'required') {
    endMarkerLabel = t('End marker not reported')
  }
  const toolCallsByName = useMemo(
    () =>
      Object.entries(response?.tool_calls_by_name ?? {}).sort(
        ([left], [right]) => left.localeCompare(right)
      ),
    [response?.tool_calls_by_name]
  )
  const unnamedToolCalls = Math.max(
    0,
    (response?.tool_calls ?? 0) -
      toolCallsByName.reduce((total, [, count]) => total + count, 0)
  )
  const client = admin?.client
  const other = useMemo(() => parseLogOther(props.log.other), [props.log.other])
  const failed =
    props.log.type === 5 ||
    summary?.outcome === 'failed' ||
    summary?.outcome === 'cancelled' ||
    (summary?.status ?? 0) >= 400 ||
    !!response?.error ||
    !!upstream?.error_type ||
    ['failed', 'error', 'ERROR'].includes(summary?.finish_reason ?? '')
  const truncated = [
    'length',
    'max_tokens',
    'MAX_TOKENS',
    'incomplete:max_output_tokens',
  ].includes(summary?.finish_reason ?? '')
  const toolFinish = ['tool_calls', 'tool_use'].includes(
    summary?.finish_reason ?? ''
  )
  let finishLabel = t('Unknown')
  let finishVariant: StatusBadgeProps['variant'] = 'neutral'
  if (summary?.outcome === 'cancelled') {
    finishLabel = t('Client disconnected')
    finishVariant = 'orange'
  } else if (failed) {
    finishLabel = t('Failed')
    finishVariant = 'red'
  } else if (
    response?.refused ||
    [
      'content_filter',
      'refusal',
      'incomplete:content_filter',
      'SAFETY',
      'IMAGE_SAFETY',
      'BLOCKLIST',
      'PROHIBITED_CONTENT',
      'SPII',
      'RECITATION',
    ].includes(summary?.finish_reason ?? '')
  ) {
    finishLabel = t('Refused')
    finishVariant = 'orange'
  } else if (summary?.finish_reason === '') {
    finishLabel = t('Missing')
    finishVariant = 'red'
  } else if (truncated) {
    finishLabel = t('Truncated')
    finishVariant = 'orange'
  } else if (
    summary?.finish_reason === 'incomplete' ||
    summary?.finish_reason?.startsWith('incomplete:')
  ) {
    finishLabel = t('Incomplete')
    finishVariant = 'orange'
  } else if (toolFinish) {
    finishLabel = t('Tool call')
    finishVariant = 'blue'
  } else if (
    ['stop', 'end_turn', 'stop_sequence', 'STOP', 'completed'].includes(
      summary?.finish_reason ?? ''
    )
  ) {
    finishLabel = t('Normal completion')
  }
  const incompleteReasonLabels: Record<string, string> = {
    size_limit: t('Capture size limit reached'),
    body_unavailable: t('Request body unavailable for metadata'),
    invalid_json: t('Metadata payload is not valid JSON'),
    unsupported_format: t('Response format is not recognized'),
    tool_limit: t('Tool-call capture limit reached'),
    invalid_tool_name: t('Tool name is invalid or too long'),
    invalid_tool_index: t('Tool-call index is invalid'),
    usage_size_limit: t('Upstream usage was not captured completely.'),
    usage_invalid_json: t('Upstream usage was not captured completely.'),
    capture_upgraded: t(
      'Detailed capture started after earlier events; earlier text and tool statistics are unavailable.'
    ),
    client_delivery_incomplete: t(
      'The client did not receive the complete response.'
    ),
    upstream_read_error: t(
      'The upstream response could not be read completely.'
    ),
  }
  const parameters = [
    shape?.max_tokens !== undefined ? `max_tokens=${shape.max_tokens}` : '',
    shape?.temperature !== undefined ? `temperature=${shape.temperature}` : '',
    shape?.include_usage ? 'include_usage' : '',
  ].filter(Boolean)
  const media = [
    shape?.images ? `${t('Images')} ${shape.images}` : '',
    shape?.videos ? `${t('Videos')} ${shape.videos}` : '',
    shape?.audios ? `${t('Audio')} ${shape.audios}` : '',
    shape?.files ? `${t('Files')} ${shape.files}` : '',
  ].filter(Boolean)
  if (
    !summary &&
    !(
      failed &&
      (other?.status_code != null || other?.error_code || other?.error_type)
    )
  ) {
    return null
  }
  return (
    <div className='min-w-0 space-y-3'>
      {failed &&
        (summary ||
          other?.status_code != null ||
          other?.error_code ||
          other?.error_type) && (
          <DetailSection label={t('Error Information')} variant='danger'>
            {summary?.outcome && (
              <DetailRow label={t('Completion status')} value={finishLabel} />
            )}
            {summary?.stage && (
              <DetailRow
                label={t('Failure stage')}
                value={t(`Request trace stage: ${summary.stage}`)}
              />
            )}
            {(summary?.status ?? other?.status_code ?? 0) > 0 && (
              <DetailRow
                label={t('Status Code')}
                value={summary?.status ?? other?.status_code}
              />
            )}
            {other?.error_code && (
              <DetailRow
                label={t('Error Code')}
                value={other.error_code}
                mono
              />
            )}
            {(other?.error_type || summary?.client_error_type) && (
              <DetailRow
                label={t('Error Type')}
                value={
                  <>
                    {other?.error_type}
                    {summary?.client_error_type && (
                      <span className='text-muted-foreground'>
                        {other?.error_type ? ' · ' : ''}
                        {t('Client received')}: {summary.client_error_type}
                      </span>
                    )}
                  </>
                }
                mono
              />
            )}
            {upstream?.error_type && (
              <DetailRow
                label={t('Upstream error type')}
                value={upstream.error_type}
                mono
              />
            )}
            {upstream?.error_code &&
              upstream.error_code !== other?.error_code && (
                <DetailRow
                  label={t('Upstream error code')}
                  value={upstream.error_code}
                  mono
                />
              )}
          </DetailSection>
        )}
      {summary && (summary.req_bytes !== undefined || admin) && (
        <DetailSection label={t('Request')}>
          {(summary.req_bytes !== undefined ||
            client?.wire_bytes !== undefined ||
            client?.content_type) && (
            <DetailRow
              label={t('Request body')}
              value={
                <>
                  {summary.req_bytes !== undefined &&
                    formatTraceBytes(summary.req_bytes)}
                  {(client?.wire_bytes !== undefined ||
                    client?.content_type) && (
                    <span className='text-muted-foreground'>
                      {summary.req_bytes !== undefined ? ' · ' : ''}
                      {[
                        client?.wire_bytes !== undefined
                          ? t('On wire: {{size}}', {
                              size: formatTraceBytes(client.wire_bytes),
                            })
                          : '',
                        client?.content_type,
                      ]
                        .filter(Boolean)
                        .join(' · ')}
                    </span>
                  )}
                </>
              }
            />
          )}
          {shape && (
            <>
              <DetailRow
                label={t('Messages')}
                value={[
                  t('{{count}} messages', { count: shape.messages }),
                  t('Text: {{count}} characters', { count: shape.text_chars }),
                  shape.system ? t('Includes system prompt') : '',
                ]
                  .filter(Boolean)
                  .join(' · ')}
              />
              {media.length > 0 && (
                <DetailRow label={t('Multimedia')} value={media.join(' · ')} />
              )}
              {(shape.tools || shape.tool_choice) && (
                <DetailRow
                  label={t('Tool definitions')}
                  value={[
                    shape.tools
                      ? t('{{count}} tools', { count: shape.tools })
                      : '',
                    shape.tool_choice ? `tool_choice=${shape.tool_choice}` : '',
                  ]
                    .filter(Boolean)
                    .join(' · ')}
                />
              )}
              {parameters.length > 0 && (
                <DetailRow
                  label={t('Parameters')}
                  value={parameters.join(' · ')}
                  mono
                />
              )}
            </>
          )}
          {admin?.shape_incomplete && (
            <p className='text-muted-foreground text-xs'>
              {admin.shape_incomplete_reason
                ? (incompleteReasonLabels[admin.shape_incomplete_reason] ??
                  admin.shape_incomplete_reason)
                : t(
                    'Request metadata was unavailable or exceeded the capture limit.'
                  )}
            </p>
          )}
        </DetailSection>
      )}
      {upstream &&
        (upstream.status !== undefined ||
          upstream.proto ||
          upstream.resp_id ||
          upstream.request_id ||
          upstream.error_type) && (
          <DetailSection label={t('Upstream')}>
            <DetailRow
              label={t('Status')}
              value={[upstream.status, upstream.proto]
                .filter((value) => value !== undefined && value !== '')
                .join(' · ')}
            />
            {upstream.req_bytes !== undefined && (
              <DetailRow
                label={t('Sent size')}
                value={formatTraceBytes(upstream.req_bytes)}
              />
            )}
            {upstream.resp_id && (
              <DetailRow
                label={t('Response ID')}
                value={upstream.resp_id}
                mono
              />
            )}
            {upstream.resp_model && (
              <DetailRow
                label={t('Actual model')}
                value={
                  <span className='inline-flex flex-wrap items-center gap-1.5'>
                    {upstream.resp_model}
                    {upstream.resp_model !== props.log.model_name && (
                      <StatusBadge
                        label={t('Mapped')}
                        variant='blue'
                        className='text-[11px]'
                        copyable={false}
                      />
                    )}
                  </span>
                }
                mono
              />
            )}
            {upstream.fingerprint && (
              <DetailRow
                label={t('Fingerprint')}
                value={upstream.fingerprint}
                mono
              />
            )}
            {upstream.service_tier && (
              <DetailRow
                label={t('Service tier')}
                value={upstream.service_tier}
                mono
              />
            )}
          </DetailSection>
        )}
      {summary && (
        <DetailSection label={t('Response')}>
          {summary.finish_reason !== undefined && (
            <DetailRow
              label={t('Finish reason')}
              value={
                <span className='inline-flex flex-wrap items-center gap-1.5'>
                  <span className='font-mono'>
                    {summary.finish_reason || '""'}
                  </span>
                  <StatusBadge
                    label={finishLabel}
                    variant={finishVariant}
                    className='text-[11px]'
                    copyable={false}
                  />
                </span>
              }
            />
          )}
          {!summaryMetadata &&
            !response?.incomplete_reasons?.includes('capture_upgraded') &&
            !!response?.tool_calls && (
              <DetailRow
                label={
                  <span className='block leading-5'>{t('Tool calls')}</span>
                }
                value={
                  <div className='flex min-w-0 flex-col gap-1.5'>
                    <ScrollArea
                      role='region'
                      aria-label={t('Tool calls')}
                      className='w-full [&_[data-slot=scroll-area-viewport]]:max-h-32'
                    >
                      <ul className='m-0 flex min-w-0 list-none flex-wrap items-start gap-x-3 gap-y-1.5 p-0 pr-3'>
                        {toolCallsByName.map(([name, count]) => (
                          <li
                            key={name}
                            className='flex max-w-full min-w-0 items-start gap-1.5'
                          >
                            <span className='min-w-0 font-mono leading-5 break-all'>
                              {name}
                            </span>
                            <Badge
                              variant='secondary'
                              className='min-w-6 tabular-nums'
                            >
                              {count.toLocaleString()}
                            </Badge>
                          </li>
                        ))}
                        {unnamedToolCalls > 0 && (
                          <li
                            className='text-muted-foreground flex max-w-full min-w-0 items-start gap-1.5'
                            title={t(
                              '{{count}} calls without a recorded name',
                              {
                                count: unnamedToolCalls,
                              }
                            )}
                          >
                            <span className='min-w-0 leading-5'>
                              {t('Not recorded')}
                            </span>
                            <Badge
                              variant='secondary'
                              className='min-w-6 tabular-nums'
                            >
                              {unnamedToolCalls.toLocaleString()}
                            </Badge>
                          </li>
                        )}
                      </ul>
                    </ScrollArea>
                  </div>
                }
              />
            )}
          {response?.refused && (
            <DetailRow label={t('Refused')} value={t('Yes')} />
          )}
          {response &&
            (response.end_marker ||
              response.end_marker_policy ||
              props.log.is_stream) && (
              <DetailRow
                label={t('End marker')}
                value={
                  <span
                    className={cn(
                      response.end_marker && 'font-mono',
                      !response.done &&
                        response.end_marker_policy === 'required' &&
                        'text-destructive'
                    )}
                  >
                    {endMarkerLabel}
                  </span>
                }
              />
            )}
          {response?.metadata_incomplete &&
            (!summaryMetadata || !!response.incomplete_reasons?.length) && (
              <p className='text-muted-foreground text-xs'>
                {response.incomplete_reasons?.length
                  ? response.incomplete_reasons
                      .map((reason) => incompleteReasonLabels[reason] ?? reason)
                      .join(' · ')
                  : t(
                      'Response metadata is partial because an event was unavailable or exceeded the capture limit.'
                    )}
              </p>
            )}
          {truncated && (
            <Alert className='border-warning/30 bg-warning/10 text-warning mt-1 py-1.5'>
              <AlertDescription className='text-warning text-xs'>
                {t('Output was truncated at the configured limit.')}
              </AlertDescription>
            </Alert>
          )}
        </DetailSection>
      )}
    </div>
  )
})

export function RequestTraceDetails(props: {
  query: ReturnType<typeof useRequestTraces>
  log: UsageLog
  isAdmin: boolean
}) {
  const { t } = useTranslation()
  const records = props.query.data ?? []
  const finalRecord = records.at(-1)
  return (
    <>
      <TraceAttemptDetails
        record={finalRecord}
        log={props.log}
        isAdmin={props.isAdmin}
      />
      {props.isAdmin &&
        records.map((record) => (
          <UpstreamUsageDetails
            key={`${record.request_id}:${record.attempt}`}
            record={record}
            showAttempt={records.length > 1}
          />
        ))}
      {(props.query.isPending || props.query.isTracePending) &&
        props.log.request_id && (
          <div role='status' aria-live='polite'>
            <LoadingState
              className='min-h-0 py-4'
              message={
                props.query.isTracePending
                  ? t('Trace metadata is waiting to be written')
                  : undefined
              }
            />
          </div>
        )}
      {props.query.isError && (
        <ErrorState
          title={t('Failed to load request trace')}
          onRetry={() => void props.query.refetch()}
          className='min-h-0 py-4'
        />
      )}
      {!props.query.isPending &&
        !props.query.isTracePending &&
        !props.query.isError &&
        records.length === 0 &&
        props.log.request_id && (
          <div className='text-muted-foreground flex flex-wrap items-center gap-2 text-xs'>
            <span>{t('No trace metadata is available for this request.')}</span>
            <Button
              type='button'
              size='sm'
              variant='ghost'
              disabled={props.query.isFetching}
              onClick={() => void props.query.refetch()}
            >
              {t('Refresh trace')}
            </Button>
          </div>
        )}
    </>
  )
}
