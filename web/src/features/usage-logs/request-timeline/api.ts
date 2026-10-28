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
import { z } from 'zod'

import { api, type ApiRequestConfig } from '@/lib/api'

const timestamp = z.number().int().nonnegative().safe()
const offset = timestamp.nullable()
const event = z.object({
  sequence: z.number().int().positive(),
  name: z.string(),
  offset_us: timestamp,
  failed: z.boolean().optional(),
})
const phase = z.object({
  name: z.string(),
  attempt: z.number().optional(),
  start_us: timestamp,
  end_us: offset,
})
const exchange = z.object({
  index: z.number().int().positive(),
  kind: z.string(),
  start_us: timestamp,
  headers_us: offset,
  first_byte_us: offset,
  end_us: offset,
  status: z.number().optional(),
  protocol: z.string().optional(),
  event_stream: z.boolean().optional(),
  proxy: z.boolean(),
  reused: z.boolean().nullable(),
  bytes_read: z.number().nullable(),
  end_reason: z.string().optional(),
  events: z.array(event).nullable(),
})
const stream = z.object({
  first_us: offset,
  last_us: offset,
  count: z.number(),
  max_gap_us: z.number(),
  end: z.string(),
  failed: z.boolean(),
})
const timeline = z.object({
  measurement_version: z.number().int().positive().optional(),
  method: z.string(),
  route: z.string(),
  phases: z.array(phase),
  attempts: z.array(
    z.object({
      index: z.number(),
      channel_id: z.number(),
      start_us: timestamp,
      end_us: offset,
      outcome: z.string(),
      exchanges: z.array(exchange).nullable(),
      stream: stream.optional(),
    })
  ),
  response: z.object({
    bytes_written: z.number(),
    write_failed: z.boolean(),
    last_write_us: offset,
    cancel_observed_us: offset,
    hijacked: z.boolean(),
  }),
  capture: z.object({ truncated: z.boolean(), missing: z.array(z.string()) }),
  errors: z.array(
    z.object({ stage: z.string(), code: z.string(), offset_us: timestamp })
  ),
})
const queryResult = z.object({
  found: z.boolean(),
  enabled: z.boolean(),
  retention_days: z.number(),
  summary: z
    .object({
      request_id: z.string(),
      started_at_us: timestamp,
      finished_at_us: timestamp,
      duration_us: timestamp,
      first_payload_write_offset_us: offset,
      http_status: z.number().nullable(),
      outcome: z.string(),
      is_stream: z.boolean().nullable(),
      capture_status: z.string(),
      schema_version: z.number(),
    })
    .nullable(),
  detail: z
    .object({
      timeline: timeline.nullable(),
      attempt_count: z.number(),
      final_channel_id: z.number().nullable(),
      final_attempt_first_byte_offset_us: offset,
      node_id: z.string(),
      status: z.string(),
    })
    .nullable(),
})

export type RequestTimelineData = z.infer<typeof timeline>
export type RequestTimelineResult = z.infer<typeof queryResult>
export type TracePhase = z.infer<typeof phase>

const writerStatus = z.object({
  initialized: z.boolean(),
  enabled: z.boolean(),
  accepting: z.boolean(),
  node_id: z.string(),
  started_at_us: timestamp,
  queue_capacity: timestamp,
  sampled_out: timestamp.optional(),
  pending_payload_bytes: timestamp.optional(),
  payload_budget_bytes: timestamp.optional(),
  dropped_byte_limit: timestamp.optional(),
  accepted: timestamp,
  pending: timestamp,
  processing: timestamp,
  buffered: timestamp,
  writing: timestamp,
  outstanding: timestamp,
  oldest_outstanding_age_us: offset,
  written: timestamp,
  failed: timestamp,
  dropped: timestamp,
  retries: timestamp,
  dropped_encoding: timestamp,
  dropped_queue_full: timestamp,
  dropped_stopped: timestamp,
  dropped_shutdown: timestamp,
  last_written_at_us: offset,
  last_failed_at_us: offset,
  last_batch_size: timestamp,
  last_batch_duration_us: offset,
  retention_days: timestamp,
  cleanup_deleted: timestamp,
  cleanup_failed: timestamp,
  cleanup_pending: z.boolean(),
})

export type RequestTimelineWriterStatus = z.infer<typeof writerStatus>

export function isRequestId(value: string): boolean {
  return /^[A-Za-z0-9_-]{1,64}$/.test(value)
}

export async function getRequestTimeline(
  requestId: string,
  admin: boolean,
  signal: AbortSignal
): Promise<RequestTimelineResult> {
  const config = {
    signal,
    disableDuplicate: true,
    skipBusinessError: true,
    skipErrorHandler: true,
  } satisfies ApiRequestConfig
  const path = admin ? '/api/request-traces' : '/api/request-traces/self'
  const response = await api.get<unknown>(
    `${path}/${encodeURIComponent(requestId)}`,
    config
  )
  const envelope = z
    .object({ success: z.literal(true), data: queryResult })
    .parse(response.data)
  return envelope.data
}

export async function getRequestTimelineWriterStatus(
  signal: AbortSignal
): Promise<RequestTimelineWriterStatus> {
  const config = {
    signal,
    disableDuplicate: true,
    skipBusinessError: true,
    skipErrorHandler: true,
  } satisfies ApiRequestConfig
  const response = await api.get<unknown>('/api/request-traces/status', config)
  return z
    .object({ success: z.literal(true), data: writerStatus })
    .parse(response.data).data
}
