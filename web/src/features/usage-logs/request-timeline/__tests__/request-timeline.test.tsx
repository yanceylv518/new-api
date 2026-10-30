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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  act,
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import type { TimelineResponse } from '../api'
import { RequestTimeline } from '../request-timeline'

function recordedResponse(): TimelineResponse & {
  record: NonNullable<TimelineResponse['record']>
  timeline: NonNullable<TimelineResponse['timeline']>
} {
  return {
    found: true,
    enabled: true,
    status: 'available',
    record: {
      request_id: 'request-1',
      upstream_status: 200,
      result: 'failed',
      error_kind: 'upstream_business_error',
      upstream_request_body_bytes: 0,
    },
    timeline: {
      version: 1,
      started_at_ms: 1793232000000,
      request_sent_ms: 20,
      first_response_ms: 120,
      first_write_ms: 125,
      ended_ms: 200,
      timing_valid: true,
      request_model: 'example',
      channel_id: 1,
      first_chunk_type: 'role',
      end_marker_received: true,
      returned_usage: null,
      first_error: {
        offset_ms: 150,
        stage: 'upstream_read',
        kind: 'upstream_business_error',
        code: 'overloaded',
        message: '<script>unsafe()</script>',
        message_truncated: false,
      },
    },
  }
}

function mountTimeline() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const view = render(
    <QueryClientProvider client={client}>
      <RequestTimeline requestId='request-1' />
    </QueryClientProvider>
  )
  return { ...view, client }
}

beforeEach(() => {
  useAuthStore.getState().auth.setUser({ id: 7, username: 'admin', role: 10 })
})
afterEach(() => {
  cleanup()
  useAuthStore.getState().auth.reset()
  vi.useRealTimers()
})

describe('request timeline', () => {
  it('loads only after keyboard expansion and keeps HTTP 200 separate from a business failure', async () => {
    const get = vi
      .spyOn(api, 'get')
      .mockResolvedValue({ data: { data: recordedResponse() } })
    const user = userEvent.setup()
    mountTimeline()
    const trigger = screen.getByRole('button', { name: 'Request timeline' })
    expect(trigger).toHaveAttribute('aria-expanded', 'false')
    expect(get).not.toHaveBeenCalled()
    trigger.focus()
    await user.keyboard('{Enter}')
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    const timeline = await screen.findByRole('list', {
      name: 'Request timeline',
    })
    expect(within(timeline).getAllByRole('listitem')).toHaveLength(5)
    expect(screen.getByText('HTTP 200')).toBeVisible()
    expect(screen.getAllByText('Failed').length).toBeGreaterThan(0)
    expect(screen.queryByText('Completed')).not.toBeInTheDocument()
    expect(screen.getByText('0 B')).toBeVisible()
    expect(screen.getByText('Role information (not text)')).toBeVisible()
    expect(screen.getByText('Not returned by upstream')).toBeVisible()
    expect(screen.getByText('+150 ms')).toBeVisible()
    expect(screen.getByText('<script>unsafe()</script>')).toBeVisible()
    expect(document.querySelector('script')).toBeNull()
    await user.click(
      screen.getByRole('button', { name: 'View recorded fields' })
    )
    expect(
      screen.getByRole('button', { name: 'Copy recorded fields' })
    ).toBeVisible()
    expect(screen.getByText(/"request_id": "request-1"/)).toHaveClass(
      'whitespace-pre-wrap',
      'break-all',
      'max-h-64'
    )
  })

  it('does not display or request administrative data after the account loses admin access', async () => {
    const get = vi
      .spyOn(api, 'get')
      .mockResolvedValue({ data: { data: recordedResponse() } })
    const user = userEvent.setup()
    mountTimeline()
    await user.click(screen.getByRole('button', { name: 'Request timeline' }))
    await screen.findByRole('list')
    act(() =>
      useAuthStore
        .getState()
        .auth.setUser({ id: 8, username: 'reader', role: 1 })
    )
    expect(screen.queryByRole('list')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Request timeline' })
    ).not.toBeInTheDocument()
    expect(get).toHaveBeenCalledTimes(1)
  })

  it('shows unknown size and missing write time without inventing zero or normal completion', async () => {
    const response = recordedResponse()
    response.record.result = 'unknown'
    response.record.upstream_request_body_bytes = null
    response.timeline.first_write_ms = null
    response.timeline.first_error = null
    vi.spyOn(api, 'get').mockResolvedValue({ data: { data: response } })
    mountTimeline()
    await userEvent.click(
      screen.getByRole('button', { name: 'Request timeline' })
    )
    await screen.findByRole('list')
    expect(screen.getByText('Unknown')).toBeVisible()
    expect(screen.getAllByText('Unconfirmed end').length).toBeGreaterThan(0)
    const write = screen
      .getByText('First response write completed')
      .closest('li')
    if (!write) throw new Error('Missing first-write node')
    expect(within(write).getByText('Not recorded')).toBeVisible()
    expect(within(write).queryByText('+0 ms')).not.toBeInTheDocument()
  })

  it('cancels an in-flight query when collapsed and allows retry after a network error', async () => {
    let signal: AbortSignal | undefined
    const get = vi
      .spyOn(api, 'get')
      .mockImplementationOnce((_url, config) => {
        signal = config?.signal as AbortSignal
        return new Promise(() => {})
      })
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValue({ data: { data: recordedResponse() } })
    const user = userEvent.setup()
    mountTimeline()
    const trigger = screen.getByRole('button', { name: 'Request timeline' })
    await user.click(trigger)
    await waitFor(() => expect(get).toHaveBeenCalledTimes(1))
    await user.click(trigger)
    expect(signal?.aborted).toBe(true)
    await user.click(trigger)
    await user.click(await screen.findByRole('button', { name: 'Retry' }))
    expect(await screen.findByRole('list')).toBeVisible()
  })

  it('stops polling absent data after three follow-ups and offers manual refresh', async () => {
    vi.useFakeTimers()
    const missing: TimelineResponse = {
      found: false,
      enabled: true,
      status: 'missing',
      record: null,
      timeline: null,
    }
    const get = vi
      .spyOn(api, 'get')
      .mockResolvedValue({ data: { data: missing } })
    mountTimeline()
    await act(async () => {
      screen.getByRole('button', { name: 'Request timeline' }).click()
      await vi.advanceTimersByTimeAsync(0)
    })
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4100)
    })
    expect(get).toHaveBeenCalledTimes(4)
    expect(screen.getByText('No timeline data')).toBeVisible()
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000)
    })
    expect(get).toHaveBeenCalledTimes(4)
    await act(async () => {
      screen.getByRole('button', { name: 'Refresh' }).click()
      await vi.advanceTimersByTimeAsync(0)
    })
    expect(get).toHaveBeenCalledTimes(5)
  })

  it('rejects malformed API data and can load a valid record after retry', async () => {
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce({ data: { data: { status: 'available' } } })
      .mockResolvedValue({ data: { data: recordedResponse() } })
    mountTimeline()
    await userEvent.click(
      screen.getByRole('button', { name: 'Request timeline' })
    )
    await userEvent.click(await screen.findByRole('button', { name: 'Retry' }))
    expect(await screen.findByRole('list')).toBeVisible()
  })
})
