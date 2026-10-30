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

import { api } from '@/lib/api'

const milliseconds = z.number().int().nullable()
const timelineSchema = z.object({
  version: z.literal(1),
  started_at_ms: z.number().int().positive(),
  request_sent_ms: milliseconds,
  first_response_ms: milliseconds,
  first_write_ms: milliseconds,
  ended_ms: milliseconds,
  timing_valid: z.boolean(),
  request_model: z.string(),
  channel_id: z.number().int(),
  upstream_request_id: z.string().optional(),
  response_id: z.string().optional(),
  response_model: z.string().optional(),
  first_chunk_type: z.string().optional(),
  finish_reason: z.string().optional(),
  end_reason: z.string().optional(),
  end_marker_received: z.boolean(),
  upstream_usage: z.record(z.string(), z.unknown()).optional(),
  upstream_usage_capture: z.enum(['captured', 'truncated']).optional(),
  upstream_usage_preview: z.string().optional(),
  upstream_usage_bytes: z.number().int().nonnegative().optional(),
  returned_usage: z
    .object({
      input_tokens: z.number(),
      output_tokens: z.number(),
      total_tokens: z.number(),
    })
    .nullable(),
  first_error: z
    .object({
      offset_ms: z.number().int(),
      stage: z.string(),
      kind: z.string(),
      code: z.string(),
      message: z.string(),
      message_truncated: z.boolean(),
    })
    .nullable(),
})

const responseSchema = z.object({
  found: z.boolean(),
  enabled: z.boolean(),
  status: z.enum([
    'available',
    'missing',
    'unavailable',
    'invalid_data',
    'unsupported_version',
  ]),
  record: z
    .object({
      request_id: z.string(),
      upstream_status: z.number().int().nullable(),
      result: z.enum(['success', 'failed', 'interrupted', 'unknown']),
      error_kind: z.string().nullable(),
      upstream_request_body_bytes: z.number().int().nonnegative().nullable(),
    })
    .nullable(),
  timeline: timelineSchema.nullable(),
})

export type TimelineResponse = z.infer<typeof responseSchema>
export type TimelineData = z.infer<typeof timelineSchema>

export async function getRequestTimeline(
  requestId: string,
  signal: AbortSignal
): Promise<TimelineResponse> {
  const response = await api.get(
    `/api/request-timelines/${encodeURIComponent(requestId)}`,
    {
      signal,
      disableDuplicate: true,
      skipErrorHandler: true,
    }
  )
  return responseSchema.parse(response.data.data)
}
