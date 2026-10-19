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
import * as z from 'zod'

const compressionLevelSchema = z.enum(['fast', 'standard', 'high', 'best'])

export const requestTraceConfigSchema = z.object({
  level: z.enum(['off', 'errors', 'all']),
  pre_channel_errors_enabled: z.boolean(),
  anonymous_errors_enabled: z.boolean(),
  upstream_request_id_headers: z.array(z.string()).optional().default([]),
  compression_level: compressionLevelSchema.optional().default('fast'),
  success_sample_rate: z.number().int().min(0).max(100).optional().default(100),
  retention_days: z.number().int().min(0).max(3650).optional().default(0),
  diagnostic_channel_ids: z
    .array(z.number().int().positive())
    .max(16)
    .optional()
    .default([]),
  diagnostic_expires_at: z.number().int().nonnegative().optional().default(0),
})

export const defaultRequestTraceConfig: z.infer<
  typeof requestTraceConfigSchema
> = {
  level: 'errors',
  pre_channel_errors_enabled: true,
  anonymous_errors_enabled: false,
  upstream_request_id_headers: [],
  compression_level: 'fast',
  success_sample_rate: 100,
  retention_days: 0,
  diagnostic_channel_ids: [],
  diagnostic_expires_at: 0,
}

export const logSettingsSchema = z.object({
  LogConsumeEnabled: z.boolean(),
  RequestTraceEnabled: z.boolean(),
  RequestTraceLevel: z.enum(['errors', 'all']),
  RequestTracePreChannelErrorsEnabled: z.boolean(),
  RequestTraceAnonymousErrorsEnabled: z.boolean(),
  RequestTraceUpstreamRequestIdHeaders: z.string(),
  RequestTraceCompressionLevel: compressionLevelSchema,
  RequestTraceSuccessSampleRate: z.number().int().min(0).max(100),
  RequestTraceRetentionDays: z.number().int().min(0).max(3650),
  RequestTraceDiagnosticChannels: z.string(),
  RequestTraceDiagnosticExpiresAt: z.number().int().nonnegative(),
})

export type LogSettingsFormValues = z.infer<typeof logSettingsSchema>
