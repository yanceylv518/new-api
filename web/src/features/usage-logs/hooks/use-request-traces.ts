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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

export type RequestTrace = {
  request_id: string
  attempt: number
  created_at: number
  trace: {
    ms: number
    req_bytes?: number
    resp_bytes: number
    status: number
    finish_reason?: string
    stage?: string
    client_error_type?: string
  }
  admin_info?: {
    channel_id?: number
    shape_incomplete?: boolean
    client: {
      ua?: string
      content_type?: string
      wire_bytes?: number
      anthropic_version?: string
    }
    shape?: {
      messages: number
      text_chars: number
      system: boolean
      images?: number
      videos?: number
      audios?: number
      files?: number
      tools?: number
      tool_choice?: string
      max_tokens?: number
      temperature?: number
      include_usage?: boolean
      keys?: string[]
    }
    upstream: {
      status?: number
      proto?: string
      req_bytes?: number
      header_ms: number
      request_id?: string
      resp_id?: string
      resp_model?: string
      object?: string
      created?: number
      fingerprint?: string
      service_tier?: string
      error_type?: string
      error_code?: string
    }
    resp: {
      error?: boolean
      content_type?: string
      content_chars: number
      reasoning_chars: number
      reasoning_tokens?: number
      reasoning_output_tokens?: number
      tool_calls?: number
      tool_calls_by_name?: Record<string, number>
      refused?: boolean
      chunks?: number
      last_chunk_ms?: number
      done: boolean
      end_marker?: string
      usage_chunk: boolean
      metadata_incomplete?: boolean
      keys?: Record<string, string[]>
    }
  }
}

export function useRequestTraces(requestId: string, enabled: boolean) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const queryClient = useQueryClient()
  const queryKey = useMemo(
    () => ['request-traces', user?.id, user?.role, requestId],
    [user?.id, user?.role, requestId]
  )
  useEffect(() => {
    if (!enabled) return
    return () => {
      void queryClient.cancelQueries({ queryKey, exact: true })
    }
  }, [enabled, queryClient, queryKey])
  return useQuery({
    queryKey,
    enabled: enabled && !!user && !!requestId,
    queryFn: async ({ signal }): Promise<RequestTrace[]> => {
      const response = await api.get<{
        success: boolean
        message?: string
        data?: RequestTrace[]
      }>(`/api/log/trace?request_id=${encodeURIComponent(requestId)}`, {
        signal,
      })
      if (!response.data.success || !response.data.data) {
        throw new Error(
          response.data.message || t('Failed to load request trace')
        )
      }
      return response.data.data
    },
    staleTime: 60_000,
    refetchOnWindowFocus: false,
  })
}

export function formatTraceTime(ms: number) {
  return ms < 1000 ? `${ms} ms` : `${(ms / 1000).toFixed(2)} s`
}
