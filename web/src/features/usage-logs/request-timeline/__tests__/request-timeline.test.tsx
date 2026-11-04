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
      response_id: 'response-1',
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
  it('reports omitted intermediate attempts and truncated fields without hiding the final failure', async () => {
    const response = recordedResponse()
    const span = { ...response.timeline, details_truncated: true }
    response.timeline.details_truncated = true
    response.timeline.channel_id = 0
    response.timeline.attempt_count = 20
    response.timeline.attempts_omitted = 18
    response.timeline.attempts = [
      {
        ...response.record,
        number: 1,
        retry_index: 0,
        started_ms: 0,
        data: span,
      },
      {
        ...response.record,
        number: 20,
        retry_index: 19,
        started_ms: 100,
        upstream_status: null,
        data: { ...span, channel_id: 0 },
      },
    ]
    vi.spyOn(api, 'get').mockResolvedValue({ data: { data: response } })
    mountTimeline()
    await userEvent.click(
      screen.getByRole('button', { name: 'Request timeline' })
    )
    expect(await screen.findByLabelText('Final result')).toHaveTextContent(
      'Failed'
    )
    expect(
      screen.getByText(
        '18 intermediate attempts omitted to keep this timeline within its recording limit.'
      )
    ).toBeVisible()
    expect(
      screen.getByText(
        'Some recorded fields were truncated to keep this timeline within its recording limit.'
      )
    ).toBeVisible()
    expect(
      screen.getByRole('button', { name: /Attempt 20 · No channel selected/ })
    ).toHaveAttribute('aria-expanded', 'true')
  })

  it('shows a successful fallback separately from the failed first attempt and uses attempt-relative durations', async () => {
    const response = recordedResponse()
    const successful = {
      ...response.timeline,
      channel_id: 2,
      request_sent_ms: 220,
      first_response_ms: 320,
      first_write_ms: 325,
      ended_ms: 400,
      first_error: null,
      upstream_usage: {
        input_tokens: 2,
        output_tokens: 3,
        vendor_note: 'complete final usage',
      },
    }
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        data: {
          ...response,
          record: { ...response.record, result: 'success', error_kind: null },
          timeline: {
            ...successful,
            attempt_count: 2,
            attempts: [
              {
                number: 1,
                retry_index: 0,
                started_ms: 0,
                ...response.record,
                upstream_status: 502,
                data: response.timeline,
              },
              {
                number: 2,
                retry_index: 1,
                started_ms: 200,
                ...response.record,
                result: 'success',
                error_kind: null,
                data: {
                  ...successful,
                  upstream_usage: undefined,
                  upstream_usage_capture: 'truncated',
                  upstream_usage_preview: 'saved-prefix',
                },
              },
            ],
          },
        },
      },
    })
    const user = userEvent.setup()
    mountTimeline()
    await user.click(screen.getByRole('button', { name: 'Request timeline' }))
    const first = await screen.findByRole('button', {
      name: /Attempt 1 · Channel 1/,
    })
    const last = screen.getByRole('button', { name: /Attempt 2 · Channel 2/ })
    expect(first).toHaveAttribute('aria-expanded', 'false')
    expect(last).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByLabelText('Final result')).toHaveTextContent('Completed')
    expect(screen.getByText('400 ms')).toBeVisible()
    const finalPanel = screen.getByRole('region', {
      name: /Attempt 2 · Channel 2/,
    })
    const beforeSend =
      within(finalPanel).getByText('Before sending').parentElement
    if (!beforeSend) throw new Error('Missing before-send timing row')
    expect(within(beforeSend).getByText('20 ms')).toBeVisible()
    await user.click(
      within(finalPanel).getByRole('button', {
        name: 'Completion recorded fields',
      })
    )
    expect(
      within(finalPanel).getByText(/"vendor_note": "complete final usage"/)
    ).toBeVisible()
    expect(
      screen.queryByText('<script>unsafe()</script>')
    ).not.toBeInTheDocument()
    first.focus()
    await user.keyboard('{Enter}')
    const firstPanel = screen.getByRole('region', {
      name: /Attempt 1 · Channel 1/,
    })
    expect(
      within(firstPanel).getByText('<script>unsafe()</script>')
    ).toBeVisible()
    expect(document.querySelector('script')).toBeNull()
    expect(within(firstPanel).getByText('HTTP 502')).toBeVisible()
    expect(screen.getByLabelText('Final result')).toHaveTextContent('Completed')
  })

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
    expect(screen.getByText('No upstream usage was recorded')).toBeVisible()
    expect(screen.getByText('+150 ms')).toBeVisible()
    expect(screen.getByText('<script>unsafe()</script>')).toBeVisible()
    expect(document.querySelector('script')).toBeNull()
    await user.click(
      screen.getByRole('button', { name: 'Upstream response recorded fields' })
    )
    expect(
      screen.getByRole('button', { name: 'Copy recorded fields' })
    ).toBeVisible()
    expect(screen.getByText(/"response_id": "response-1"/)).toHaveAttribute(
      'tabindex',
      '0'
    )
    await user.tab()
    await user.tab()
    expect(screen.getByText(/"response_id": "response-1"/)).toHaveFocus()
    await user.click(
      screen.getByRole('button', { name: 'Copy recorded fields' })
    )
    expect(await navigator.clipboard.readText()).toContain(
      '"response_id": "response-1"'
    )
    expect(await navigator.clipboard.readText()).not.toContain('usage_summary')
    expect(screen.getByText(/"response_id": "response-1"/)).toHaveClass(
      'whitespace-pre',
      'overflow-auto',
      'max-h-40'
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

describe('upstream usage and per-point fields', () => {
  it.each([
    {
      delta: { output_tokens: 3 },
      expected: 'Input 2 · Output 3 · Total Not recorded',
    },
    {
      delta: { input_tokens: 0, output_tokens: 0 },
      expected: 'Input 0 · Output 0 · Total Not recorded',
    },
  ])(
    'combines Claude usage events without inventing omitted fields: $expected',
    async ({ delta, expected }) => {
      const response = recordedResponse()
      response.timeline.upstream_usage = {
        message_start: {
          input_tokens: 2,
          output_tokens: 1,
          cache_read_input_tokens: 5,
        },
        message_delta: delta,
      }
      response.timeline.returned_usage = {
        input_tokens: 7,
        output_tokens: 3,
        total_tokens: 10,
      }
      vi.spyOn(api, 'get').mockResolvedValue({ data: { data: response } })
      mountTimeline()
      await userEvent.click(
        screen.getByRole('button', { name: 'Request timeline' })
      )
      expect(await screen.findByText(expected)).toBeVisible()
      expect(
        screen.getByText(
          'Usage objects received in message_start and message_delta events from the directly connected upstream.'
        )
      ).toBeVisible()
      await userEvent.click(
        screen.getByRole('button', { name: 'Completion recorded fields' })
      )
      expect(screen.getByText(/"cache_read_input_tokens": 5/)).toBeVisible()
      expect(screen.getByText(/"message_start"/)).toBeVisible()
      expect(screen.getByText(/"message_delta"/)).toBeVisible()
    }
  )

  it('shows only actually returned token values and exposes the complete object at the completion point', async () => {
    const response = recordedResponse()
    response.timeline.upstream_usage = {
      prompt_tokens: 0,
      total_tokens: 9,
      prompt_tokens_details: { cached_tokens: 4 },
      vendor: { enabled: true, note: 'provider text', values: [1, null] },
    }
    response.timeline.returned_usage = {
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 9,
    }
    vi.spyOn(api, 'get').mockResolvedValue({ data: { data: response } })
    mountTimeline()
    await userEvent.click(
      screen.getByRole('button', { name: 'Request timeline' })
    )
    await screen.findByRole('list')
    expect(
      screen.getByText('Input 0 · Output Not recorded · Total 9')
    ).toBeVisible()
    expect(
      screen.getByText(
        'Complete usage object from the last parsed usage frame received from the directly connected upstream.'
      )
    ).toBeVisible()
    await userEvent.click(
      screen.getByRole('button', { name: 'Completion recorded fields' })
    )
    expect(screen.getByText(/"cached_tokens": 4/)).toBeVisible()
    expect(screen.getByText(/"note": "provider text"/)).toBeVisible()
    expect(screen.getByText(/"enabled": true/)).toBeVisible()
    expect(screen.queryByText(/"completion_tokens": 0/)).not.toBeInTheDocument()
  })

  it('labels older records as summaries when original field presence was not saved', async () => {
    const response = recordedResponse()
    response.timeline.returned_usage = {
      input_tokens: 3,
      output_tokens: 0,
      total_tokens: 7,
    }
    vi.spyOn(api, 'get').mockResolvedValue({ data: { data: response } })
    mountTimeline()
    await userEvent.click(
      screen.getByRole('button', { name: 'Request timeline' })
    )
    expect(
      await screen.findByText(
        'Earlier records contain only a summary; upstream field presence was not retained.'
      )
    ).toBeVisible()
    expect(screen.getByText('Input 3 · Output Unknown · Total 7')).toBeVisible()
    await userEvent.click(
      screen.getByRole('button', { name: 'Completion recorded fields' })
    )
    expect(screen.getByText(/"usage_summary"/)).toBeVisible()
  })
})

describe('conflicting upstream usage aliases', () => {
  it('preserves both reported values instead of silently choosing one', async () => {
    const response = recordedResponse()
    response.timeline.upstream_usage_capture = 'captured'
    response.timeline.upstream_usage = {
      prompt_tokens: 3,
      input_tokens: 7,
      completion_tokens: 2,
      output_tokens: 2,
      total_tokens: 9,
    }
    vi.spyOn(api, 'get').mockResolvedValue({ data: { data: response } })
    mountTimeline()
    await userEvent.click(
      screen.getByRole('button', { name: 'Request timeline' })
    )
    expect(
      await screen.findByText(
        'Input prompt_tokens: 3; input_tokens: 7 · Output 2 · Total 9'
      )
    ).toBeVisible()
  })
})

describe('truncated upstream usage', () => {
  it('displays and copies the saved prefix as text without inventing a complete object', async () => {
    const response = recordedResponse()
    const prefix =
      '{"prompt_tokens":7,"vendor":"<script>unsafe()</script>未完成'
    response.timeline.upstream_usage_capture = 'truncated'
    response.timeline.upstream_usage_preview = prefix
    response.timeline.upstream_usage_bytes = 9000
    response.timeline.returned_usage = {
      input_tokens: 7,
      output_tokens: 0,
      total_tokens: 7,
    }
    vi.spyOn(api, 'get').mockResolvedValue({ data: { data: response } })
    const user = userEvent.setup()
    mountTimeline()
    await user.click(screen.getByRole('button', { name: 'Request timeline' }))
    expect(
      await screen.findByText(
        'Upstream usage was truncated (original size: 9,000 B). The saved prefix is text, not complete JSON.'
      )
    ).toBeVisible()
    expect(
      screen.queryByText('Input 7 · Output Unknown · Total 7')
    ).not.toBeInTheDocument()
    await user.click(
      screen.getByRole('button', { name: 'Truncated upstream usage' })
    )
    expect(screen.getByText(prefix)).toBeVisible()
    expect(document.querySelector('script')).toBeNull()
    await user.click(
      screen.getByRole('button', { name: 'Copy recorded fields' })
    )
    expect(await navigator.clipboard.readText()).toBe(prefix)
  })
})
