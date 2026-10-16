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
import type { TFunction } from 'i18next'

export interface TraceText {
  label: string
  description: string
}

/** 追踪阶段代码到展示名称和说明，未知代码按原样展示。 */
export function getStageText(t: TFunction): Record<string, TraceText> {
  return {
    auth: {
      label: t('Authentication'),
      description: t('Looks up the token and checks the user and group.'),
    },
    rate_limit: {
      label: t('Rate limit'),
      description: t('Checks the model request rate limit.'),
    },
    distribute: {
      label: t('Distribution'),
      description: t(
        'Resolves the model and picks the first channel. Reading the request body happens inside this step.'
      ),
    },
    request_body: {
      label: t('Request body read'),
      description: t(
        'Reads the request body from the client. A slow client upload makes this longer without the gateway being slow.'
      ),
    },
    validate: {
      label: t('Request validation'),
      description: t('Parses and validates the request parameters.'),
    },
    pricing_prepare: {
      label: t('Pricing preparation'),
      description: t(
        'Runs content checks, estimates tokens and calculates the price.'
      ),
    },
    precharge: {
      label: t('Precharge'),
      description: t(
        'Reserves quota through Redis or the database. This is the preparation step most likely to slow down.'
      ),
    },
    channel_select: {
      label: t('Channel selection'),
      description: t(
        'Picks a channel by group and affinity. A retry selects again after the previous attempt ends.'
      ),
    },
    channel_handler: {
      label: t('Channel handling'),
      description: t(
        'The whole handling of this attempt, including every step below. It is not the upstream time.'
      ),
    },
    request_prepare: {
      label: t('Request preparation'),
      description: t(
        'Converts the request to the upstream format and serializes it.'
      ),
    },
    response_handle: {
      label: t('Response processing'),
      description: t(
        'Reads the upstream response, converts it and writes it to the client. For streams it runs alongside receiving the response.'
      ),
    },
    settlement: {
      label: t('Settlement and usage log (legacy)'),
      description: t(
        'Legacy inclusive stage: quota calculation, settlement and usage logging.'
      ),
    },
    usage_finalize: {
      label: t('Usage finalization'),
      description: t(
        'Calculates usage and includes the settlement and usage log sub-stages. Do not add their durations together.'
      ),
    },
    billing_settle: {
      label: t('Settlement'),
      description: t(
        'Charges or refunds the difference from the precharge based on actual usage.'
      ),
    },
    consume_log: {
      label: t('Usage log recording'),
      description: t('Builds and writes the usage log.'),
    },
    error_log: {
      label: t('Error log recording'),
      description: t('Writes the error log.'),
    },
    log_insert: {
      label: t('Log database insert'),
      description: t('Time spent on the log INSERT itself.'),
    },
    websocket_handshake: {
      label: t('WebSocket handshake'),
      description: t('Opens the WebSocket connection to the upstream.'),
    },
  }
}

export function getOutcomeLabel(t: TFunction, outcome: string): string {
  const labels: Record<string, string> = {
    success: t('Success'),
    failed: t('Failed'),
    canceled: t('Canceled'),
    timeout: t('Timeout'),
    panic: t('Panic'),
  }
  return labels[outcome] ?? outcome
}

/** capture.missing 原因代码的说明；缺失不代表耗时为零。 */
export function getMissingText(t: TFunction, reason: string): string {
  const reasons: Record<string, string> = {
    upstream_body_not_closed: t(
      'The upstream response body was not closed, so the end of receiving the response is unknown.'
    ),
    unfinished_phase: t(
      'Some stages never finished, so their durations are unknown (dashed in the chart).'
    ),
    unfinished_attempt: t(
      'An attempt end was not observed. Its end and duration remain unknown.'
    ),
    cancel_time_not_observed: t(
      'The request context was canceled, but the cancellation callback time was not observed.'
    ),
    stream_events: t('No stream events were captured.'),
    upstream_transport: t(
      'This channel does not use the shared HTTP client, so network steps are missing.'
    ),
    websocket_network_events: t(
      'Network steps inside the WebSocket session are not captured.'
    ),
    hijacked_payload_writes: t(
      'The connection was taken over (WebSocket), so written bytes are incomplete.'
    ),
    metadata_truncated: t('The model or group name was too long and was cut.'),
  }
  return reasons[reason] ?? reason
}

export function getStreamEndLabel(t: TFunction, end: string): string {
  const labels: Record<string, string> = {
    done: t('Finished normally'),
    client_gone: t('Client disconnected'),
    timeout: t('Timeout'),
    eof: t('Upstream closed'),
    scanner_error: t('Read error'),
    handler_stop: t('Stopped by the handler'),
    panic: t('Panic'),
  }
  return labels[end] ?? end
}

/** 上游响应体的结束方式。 */
export function getBodyEndLabel(t: TFunction, reason: string): string {
  const labels: Record<string, string> = {
    eof: t('Read to the end'),
    closed: t('Closed early'),
    read_error: t('Read error'),
    read_timeout: t('Timeout'),
    read_canceled: t('Canceled'),
    close_error: t('Close error'),
    request_error: t('Request error'),
    request_timeout: t('Timeout'),
    request_canceled: t('Canceled'),
    empty_body: t('Empty body'),
  }
  return labels[reason] ?? reason
}

/** errors[] 里的阶段与代码；目前只有请求级失败会写入。 */
export function getFailureText(
  t: TFunction,
  stage: string,
  code: string
): string {
  const codes: Record<string, string> = {
    panic: t('The handler panicked.'),
    upgrade_failed: t('The WebSocket upgrade failed.'),
    request_failed: t('The request failed after all attempts.'),
  }
  return codes[code] ?? `${getStageText(t)[stage]?.label ?? stage}: ${code}`
}
