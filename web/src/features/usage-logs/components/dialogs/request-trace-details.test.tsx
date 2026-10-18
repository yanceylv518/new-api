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
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosHeaders, type GenericAbortSignal } from 'axios'
import i18n from 'i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import type { UsageLog } from '../../data/schema'
import { DetailsDialog } from './details-dialog'

const log: UsageLog = {
  id: 1,
  user_id: 11,
  created_at: 1,
  type: 2,
  content: '',
  username: 'owner',
  token_name: 'token',
  model_name: 'gpt-4o-mini',
  quota: 0,
  prompt_tokens: 1,
  completion_tokens: 1,
  use_time: 1,
  is_stream: false,
  channel: 7,
  channel_name: '',
  token_id: 1,
  group: 'default',
  ip: '',
  other: '{}',
  request_id: 'trace-owned',
  upstream_request_id: '',
}
const summary = {
  request_id: 'trace-owned',
  attempt: 0,
  created_at: 1,
  trace: {
    ms: 15,
    status: 200,
    req_bytes: 120,
    resp_bytes: 240,
    finish_reason: 'stop',
  },
}

function response(data: unknown) {
  return {
    data: { success: true, data },
    status: 200,
    statusText: 'OK',
    headers: new AxiosHeaders(),
    config: { headers: new AxiosHeaders() },
  }
}
function queryClient() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
  client.setQueryData(['status'], {}, { updatedAt: Date.now() + 60_000 })
  client.setQueryData(
    ['pricing'],
    { data: [], vendors: [] },
    { updatedAt: Date.now() + 60_000 }
  )
  return client
}

beforeEach(async () => {
  await i18n.changeLanguage('en')
  useAuthStore.getState().auth.setUser({ id: 11, username: 'owner', role: 1 })
})
afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  useAuthStore.getState().auth.setUser(null)
})

function renderDetails(
  overrides: Partial<Parameters<typeof DetailsDialog>[0]> = {}
) {
  return render(
    <QueryClientProvider client={queryClient()}>
      <DetailsDialog
        log={log}
        open
        isAdmin={false}
        isRoot={false}
        onOpenChange={() => undefined}
        {...overrides}
      />
    </QueryClientProvider>
  )
}

const detailed = {
  ...summary,
  admin_info: {
    channel_id: 7,
    client: {
      ua: 'trace-client',
      content_type: 'application/json',
      wire_bytes: 100,
    },
    shape: {
      messages: 2,
      text_chars: 33,
      system: true,
      tools: 1,
      tool_choice: 'auto',
      max_tokens: 300,
      temperature: 0,
      keys: ['model', 'mesages'],
    },
    upstream: {
      status: 200,
      proto: 'HTTP/2.0',
      header_ms: 3,
      req_bytes: 1024,
      resp_id: 'provider-response',
      resp_model: 'actual-model',
      object: 'chat.completion',
      created: 1,
    },
    resp: {
      content_type: 'application/json',
      content_chars: 61,
      reasoning_chars: 318,
      reasoning_tokens: 71,
      done: true,
      usage_chunk: false,
      keys: { top: ['id', 'choices'], message: ['content', 'role'] },
    },
  },
}

describe('request trace log details', () => {
  test('fetches only when a consume or error detail is open', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue(response([summary]))
    const client = queryClient()
    const view = render(
      <QueryClientProvider client={client}>
        <DetailsDialog
          log={log}
          open={false}
          isAdmin={false}
          isRoot={false}
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )
    expect(get).not.toHaveBeenCalled()
    view.rerender(
      <QueryClientProvider client={client}>
        <DetailsDialog
          log={log}
          open
          isAdmin={false}
          isRoot={false}
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )
    await screen.findByText('Finish reason')
    expect(get).toHaveBeenCalledTimes(1)
    expect(get.mock.calls[0]?.[0]).toBe('/api/log/trace?request_id=trace-owned')
  })

  test.each([1, 3, 6, 7])(
    'does not fetch traces for unrelated log type %s',
    async (type) => {
      const get = vi.spyOn(api, 'get').mockResolvedValue(response([]))
      renderDetails({ log: { ...log, type } })
      await screen.findByText('Log Details')
      expect(get).not.toHaveBeenCalled()
    }
  )

  test('keeps historical details and their timing when no trace exists', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue(response([]))
    renderDetails({ log: { ...log, use_time: 3 } })
    await waitFor(() => expect(get).toHaveResolved())
    expect(screen.getByText('Log Details')).toBeInTheDocument()
    expect(screen.getByText('Response Time')).toBeInTheDocument()
    expect(screen.queryByText('Request')).not.toBeInTheDocument()
    expect(screen.queryByText('Response')).not.toBeInTheDocument()
  })

  test('shows a recoverable failure and retries', async () => {
    const get = vi
      .spyOn(api, 'get')
      .mockRejectedValueOnce(new Error('unavailable'))
      .mockResolvedValue(response([summary]))
    renderDetails()
    await screen.findByText('Failed to load request trace')
    await userEvent.click(
      screen.getByRole('button', { name: /retry|try again/i })
    )
    await screen.findByText('Finish reason')
    expect(get).toHaveBeenCalledTimes(2)
  })

  test('cancels the request when the dialog closes', async () => {
    let signal: GenericAbortSignal | undefined
    vi.spyOn(api, 'get').mockImplementation((_url, config) => {
      signal = config?.signal
      return new Promise(() => undefined)
    })
    const client = queryClient()
    const view = render(
      <QueryClientProvider client={client}>
        <DetailsDialog
          log={log}
          open
          isAdmin={false}
          isRoot={false}
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )
    await waitFor(() => expect(signal).toBeDefined())
    view.rerender(
      <QueryClientProvider client={client}>
        <DetailsDialog
          log={log}
          open={false}
          isAdmin={false}
          isRoot={false}
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )
    await waitFor(() => expect(signal?.aborted).toBe(true))
  })

  test('does not show partial character statistics as zero', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          admin_info: {
            ...detailed.admin_info,
            resp: {
              content_chars: 0,
              reasoning_chars: 0,
              done: false,
              end_marker_policy: 'required',
              usage_chunk: false,
              metadata_incomplete: true,
            },
          },
        },
      ])
    )
    renderDetails({ isAdmin: true })
    await screen.findByText(/Response metadata is partial/)
    expect(screen.queryByText('Body: 0 characters')).not.toBeInTheDocument()
    expect(screen.queryByText('Output')).not.toBeInTheDocument()
  })

  test('separates cached data for each authenticated identity', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue(response([summary]))
    renderDetails()
    await screen.findByText('Finish reason')
    act(() =>
      useAuthStore
        .getState()
        .auth.setUser({ id: 12, username: 'other', role: 10 })
    )
    await waitFor(() => expect(get).toHaveBeenCalledTimes(2))
  })

  test('puts millisecond timing in the overview and follows the prototype section order', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(response([detailed]))
    renderDetails({
      isAdmin: true,
      log: {
        ...log,
        use_time: 9,
        other: JSON.stringify({
          request_conversion: ['OpenAI Compatible'],
          request_path: '/v1/chat/completions',
        }),
      },
    })
    await screen.findByText('Field-name fingerprint')
    expect(screen.getAllByText('Response Time')).toHaveLength(1)
    expect(screen.getByText('15 ms')).toBeInTheDocument()
    expect(screen.queryByText('Request trace')).not.toBeInTheDocument()
    expect(screen.queryByText('Request metadata')).not.toBeInTheDocument()
    const headings = [
      'Request Conversion',
      'Request',
      'Upstream',
      'Response',
      'Field-name fingerprint',
    ]
    const nodes = headings.map((heading) =>
      screen.getByText(heading, { exact: true })
    )
    for (let index = 1; index < nodes.length; index++) {
      const previous = nodes[index - 1]
      const current = nodes[index]
      if (!previous || !current) throw new Error('Missing section')
      expect(previous.compareDocumentPosition(current)).toBe(
        Node.DOCUMENT_POSITION_FOLLOWING
      )
    }
  })

  test('combines request statistics and preserves explicit zero parameters', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(response([detailed]))
    renderDetails({ isAdmin: true })
    await screen.findByText('Parameters')
    expect(
      screen.getByText('max_tokens=300 · temperature=0')
    ).toBeInTheDocument()
    expect(
      screen.getByText(
        '2 messages · Text: 33 characters · Includes system prompt'
      )
    ).toBeInTheDocument()
    expect(screen.getByText('1 tools · tool_choice=auto')).toBeInTheDocument()
    expect(
      screen.getByText(/On wire: 100 B · application\/json/)
    ).toBeInTheDocument()
    expect(screen.getByText('1 KB')).toBeInTheDocument()
    expect(screen.getByText('Mapped')).toBeInTheDocument()
    expect(screen.queryByText('Streaming')).not.toBeInTheDocument()
  })

  test('keeps field fingerprints collapsed and supports keyboard expansion', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(response([detailed]))
    renderDetails({ isAdmin: true })
    const trigger = await screen.findByRole('button', {
      name: 'Field-name fingerprint',
    })
    expect(trigger).toHaveAttribute('aria-expanded', 'false')
    expect(screen.getByText('3 layers')).toBeInTheDocument()
    expect(screen.queryByText('choices')).not.toBeInTheDocument()
    trigger.focus()
    await userEvent.keyboard('{Enter}')
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    expect(screen.getByText('choices')).toBeVisible()
    await userEvent.keyboard('{Enter}')
    expect(trigger).toHaveAttribute('aria-expanded', 'false')
  })

  test('keeps ordinary users to request and response summaries even if extra metadata is returned', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(response([detailed]))
    renderDetails()
    await screen.findByText('Finish reason')
    expect(screen.getByText('Request')).toBeInTheDocument()
    expect(screen.getByText('Response')).toBeInTheDocument()
    for (const hidden of [
      'Upstream',
      'Client',
      'Parameters',
      'Output',
      'Field-name fingerprint',
      'provider-response',
    ]) {
      expect(screen.queryByText(hidden)).not.toBeInTheDocument()
    }
  })

  test('shows truncation and a reasoning-only warning without inventing body text', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          trace: { ...summary.trace, finish_reason: 'length' },
          admin_info: {
            ...detailed.admin_info,
            resp: {
              ...detailed.admin_info.resp,
              content_chars: 0,
              chunks: 93,
              last_chunk_ms: 15120,
              done: false,
              end_marker_policy: 'required',
              usage_chunk: false,
            },
          },
        },
      ])
    )
    renderDetails({
      isAdmin: true,
      log: { ...log, is_stream: true, other: '{"frt":2806}' },
    })
    await screen.findByText('Truncated')
    expect(screen.getByText('length')).toBeInTheDocument()
    expect(screen.getByText('Body: 0 characters')).toHaveClass(
      'text-destructive'
    )
    expect(
      screen.getByText(
        'The output limit was used entirely for reasoning; no body text was returned.'
      )
    ).toBeInTheDocument()
    expect(screen.getByText(/First: 2.81 s/)).toBeInTheDocument()
    expect(screen.getByText(/Last: 15.12 s/)).toBeInTheDocument()
    expect(screen.getByText('End marker missing')).toHaveClass(
      'text-destructive'
    )
    expect(screen.getByText(/Usage event missing/)).toHaveClass(
      'text-destructive'
    )
  })

  test('keeps error information ahead of request data and preserves legacy error details', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          trace: {
            ...summary.trace,
            status: 429,
            stage: 'upstream',
            client_error_type: 'upstream_error',
          },
          admin_info: {
            ...detailed.admin_info,
            upstream: {
              ...detailed.admin_info.upstream,
              status: 429,
              error_type: 'tokens',
            },
          },
        },
      ])
    )
    renderDetails({
      isAdmin: true,
      log: {
        ...log,
        type: 5,
        content: 'existing-error-message',
        other:
          '{"status_code":429,"error_code":"rate_limit_exceeded","error_type":"openai_error"}',
      },
    })
    const stage = await screen.findByText('Failure stage')
    expect(
      screen.getByText('Error Information').parentElement
    ).toHaveTextContent('rate_limit_exceeded')
    expect(
      screen.getByText('Error Information').parentElement
    ).toHaveTextContent('429')
    expect(stage).toBeInTheDocument()
    expect(screen.getByText('tokens')).toBeInTheDocument()
    expect(screen.getByText('existing-error-message')).toBeInTheDocument()
    expect(
      screen
        .getByText('Error Information')
        .compareDocumentPosition(screen.getByText('Request'))
    ).toBe(Node.DOCUMENT_POSITION_FOLLOWING)
  })

  test('keeps legacy error fields when the new trace table has no row', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(response([]))
    renderDetails({
      log: {
        ...log,
        type: 5,
        other:
          '{"status_code":400,"error_code":"invalid_request","error_type":"new_api_error"}',
      },
    })
    await screen.findByText('invalid_request')
    expect(screen.getByText('new_api_error')).toBeInTheDocument()
    expect(screen.queryByText('Upstream')).not.toBeInTheDocument()
  })

  test('shows final attempt first and leaves earlier attempts collapsed for administrators', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          trace: { ...summary.trace, status: 502 },
          admin_info: {
            ...detailed.admin_info,
            upstream: {
              ...detailed.admin_info.upstream,
              request_id: 'first-attempt-id',
              error_type: 'overloaded',
            },
          },
        },
        { ...detailed, attempt: 1 },
      ])
    )
    renderDetails({ isAdmin: true })
    const trigger = await screen.findByRole('button', {
      name: 'Previous upstream attempts (1)',
    })
    expect(screen.queryByText('first-attempt-id')).not.toBeInTheDocument()
    await userEvent.click(trigger)
    expect(screen.getByText('first-attempt-id')).toBeVisible()
    expect(screen.getByText('overloaded')).toBeVisible()
  })

  test.each([
    ['failed', 'failed', true, 'required', false, 'Failed'],
    ['PROVIDER_UNKNOWN', 'unknown', false, 'required', false, 'Unknown'],
    ['SAFETY', 'success', false, 'not_applicable', false, 'Refused'],
    ['STOP', 'success', false, 'not_applicable', false, 'Normal completion'],
  ] as const)(
    'classifies %s without inventing successful completion',
    async (reason, outcome, error, policy, done, label) => {
      vi.spyOn(api, 'get').mockResolvedValue(
        response([
          {
            ...detailed,
            trace: { ...summary.trace, finish_reason: reason, outcome },
            admin_info: {
              ...detailed.admin_info,
              resp: {
                ...detailed.admin_info.resp,
                chunks: 2,
                error,
                done,
                end_marker_policy: policy,
              },
            },
          },
        ])
      )
      renderDetails({ isAdmin: true, log: { ...log, is_stream: true } })
      await screen.findByText(reason)
      expect(screen.getAllByText(label).length).toBeGreaterThan(0)
      if (label !== 'Normal completion') {
        expect(screen.queryByText('Normal completion')).not.toBeInTheDocument()
      }
      if (policy === 'not_applicable') {
        expect(
          screen.getByText('This protocol has no separate end marker')
        ).not.toHaveClass('text-destructive')
        expect(screen.queryByText('[DONE]')).not.toBeInTheDocument()
      }
    }
  )

  test('shows an actual end marker and keeps legacy received markers distinct', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          admin_info: {
            ...detailed.admin_info,
            resp: {
              ...detailed.admin_info.resp,
              chunks: 3,
              done: true,
              end_marker: '[DONE]',
              end_marker_policy: 'required',
            },
          },
        },
      ])
    )
    renderDetails({ isAdmin: true, log: { ...log, is_stream: true } })
    expect(await screen.findByText('[DONE]')).toBeInTheDocument()
    expect(
      screen.queryByText('End marker received (type not recorded)')
    ).not.toBeInTheDocument()
  })

  test('uses per-attempt durations instead of duplicating request elapsed time', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          trace: { ...summary.trace, ms: 5000, status: 502 },
          admin_info: {
            ...detailed.admin_info,
            upstream: { ...detailed.admin_info.upstream, ms: 11 },
          },
        },
        {
          ...detailed,
          attempt: 1,
          trace: { ...summary.trace, ms: 5000 },
          admin_info: {
            ...detailed.admin_info,
            upstream: { ...detailed.admin_info.upstream, ms: 22 },
          },
        },
      ])
    )
    renderDetails({ isAdmin: true })
    expect(await screen.findByText('22 ms')).toBeVisible()
    expect(screen.getByText('5.00 s')).toBeVisible()
    expect(screen.queryByText('11 ms')).not.toBeInTheDocument()
    await userEvent.click(
      screen.getByRole('button', { name: 'Previous upstream attempts (1)' })
    )
    expect(screen.getByText('11 ms')).toBeVisible()
    expect(screen.getAllByText('Attempt duration')).toHaveLength(2)
  })

  test('explains partial capture using the recorded reason codes', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          admin_info: {
            ...detailed.admin_info,
            shape: undefined,
            shape_incomplete: true,
            shape_incomplete_reason: 'body_unavailable',
            resp: {
              ...detailed.admin_info.resp,
              metadata_incomplete: true,
              incomplete_reasons: ['invalid_json', 'tool_limit'],
            },
          },
        },
      ])
    )
    renderDetails({ isAdmin: true })
    expect(
      await screen.findByText('Request body unavailable for metadata')
    ).toBeInTheDocument()
    expect(
      screen.getByText(/Metadata payload is not valid JSON/)
    ).toHaveTextContent('Tool-call capture limit reached')
  })

  test('polls pending asynchronous metadata until the committed row is available', async () => {
    vi.spyOn(api, 'get')
      .mockResolvedValueOnce({
        ...response([]),
        data: { success: true, data: [], pending: true },
      })
      .mockResolvedValueOnce(response([detailed]))
    renderDetails({ isAdmin: true })
    expect(
      await screen.findByText('Trace metadata is waiting to be written')
    ).toBeInTheDocument()
    await waitFor(
      () =>
        expect(
          screen.queryByText('Trace metadata is waiting to be written')
        ).not.toBeInTheDocument(),
      { timeout: 3000 }
    )
    expect(screen.getByText('15 ms')).toBeInTheDocument()
    expect(api.get).toHaveBeenCalledTimes(2)
  })

  test('stops asynchronous metadata polling when the detail unmounts', async () => {
    vi.spyOn(api, 'get').mockResolvedValue({
      ...response([]),
      data: { success: true, data: [], pending: true },
    })
    const view = renderDetails({ isAdmin: true })
    await screen.findByText('Trace metadata is waiting to be written')
    vi.useFakeTimers()
    try {
      view.unmount()
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1500)
      })
      expect(api.get).toHaveBeenCalledTimes(1)
    } finally {
      vi.useRealTimers()
    }
  })
})
