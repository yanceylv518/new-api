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
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosHeaders, type GenericAbortSignal } from 'axios'
import i18n from 'i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import type { UsageLog } from '../../data/schema'
import { parseUpstreamBusinessError } from '../../lib/format'
import { useCommonLogsColumns } from '../columns/common-logs-columns'
import { UsageLogsMobileList } from '../usage-logs-mobile-card'
import { UsageLogsProvider, useUsageLogsContext } from '../usage-logs-provider'
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

function BusinessErrorLogCell(props: { log: UsageLog }) {
  const columns = useCommonLogsColumns(true)
  const { selectedLog, detailsDialogOpen, setDetailsDialogOpen } =
    useUsageLogsContext()
  const table = useReactTable({
    data: [props.log],
    columns,
    getCoreRowModel: getCoreRowModel(),
  })
  const cell = table
    .getRowModel()
    .rows[0].getAllCells()
    .find((item) => item.column.id === 'content')
  return (
    <>
      {cell && flexRender(cell.column.columnDef.cell, cell.getContext())}
      {selectedLog && (
        <DetailsDialog
          log={selectedLog}
          open={detailsDialogOpen}
          isAdmin
          isRoot
          onOpenChange={setDetailsDialogOpen}
        />
      )}
    </>
  )
}

function BusinessErrorStatusCell(props: { log: UsageLog; mobile: boolean }) {
  const columns = useCommonLogsColumns(true)
  const table = useReactTable({
    data: [props.log],
    columns,
    getCoreRowModel: getCoreRowModel(),
  })
  if (props.mobile) {
    return <UsageLogsMobileList table={table} logCategory='common' />
  }
  const cell = table
    .getRowModel()
    .rows[0].getAllCells()
    .find((item) => item.column.id === 'is_stream')
  return cell ? flexRender(cell.column.columnDef.cell, cell.getContext()) : null
}

function renderBusinessErrorStatus(logValue: UsageLog, mobile = false) {
  return render(
    <QueryClientProvider client={queryClient()}>
      <UsageLogsProvider>
        <BusinessErrorStatusCell log={logValue} mobile={mobile} />
      </UsageLogsProvider>
    </QueryClientProvider>
  )
}

describe('HTTP 200 upstream business error logs', () => {
  const failure = {
    http_status: 200,
    event: 'response.failed',
    reported_status_code: 429,
    error: { code: 'upstream_timeout', message: '<script>failed</script>' },
    note: 'existing billing note',
    truncated: true,
  }

  test.each([
    [false, false, false],
    [false, false, true],
    [false, true, false],
    [false, true, true],
    [true, false, false],
    [true, false, true],
    [true, true, false],
    [true, true, true],
  ])(
    'shows business error status for mobile=%s stream=%s legacy=%s',
    async (mobile, isStream, legacy) => {
      const user = userEvent.setup()
      renderBusinessErrorStatus(
        {
          ...log,
          type: isStream ? 5 : 2,
          is_stream: isStream,
          content: JSON.stringify(
            legacy ? failure : { ...failure, kind: 'upstream_business_error' }
          ),
          other: JSON.stringify({
            stream_status: { status: 'ok', end_reason: 'done' },
          }),
        },
        mobile
      )
      const indicator = screen.getByRole('button', {
        name: 'Upstream business error (HTTP 200)',
      })
      await user.hover(indicator)
      const tooltip = await screen.findByRole('tooltip')
      expect(tooltip).toHaveTextContent('upstream_business_error')
      expect(tooltip).toHaveTextContent('upstream_timeout')
      if (isStream) expect(tooltip).toHaveTextContent('done')
    }
  )

  test.each([false, true])(
    'shows business failure in details without requiring trace metadata, stream=%s',
    async (isStream) => {
      vi.spyOn(api, 'get').mockResolvedValue(response([]))
      renderDetails({
        log: {
          ...log,
          is_stream: isStream,
          content: JSON.stringify({
            ...failure,
            kind: 'upstream_business_error',
          }),
        },
      })
      expect(
        await screen.findByText('Upstream business error (HTTP 200)')
      ).toBeInTheDocument()
      expect(screen.getByText('upstream_business_error')).toBeInTheDocument()
    }
  )

  test('retains client_gone and supports keyboard access to the error hint', async () => {
    const user = userEvent.setup()
    renderBusinessErrorStatus({
      ...log,
      is_stream: true,
      other: JSON.stringify({
        stream_status: { status: 'error', end_reason: 'client_gone' },
      }),
    })
    const indicator = screen.getByRole('button', { name: 'Stream Status' })
    await user.tab()
    expect(indicator).toHaveFocus()
    expect(await screen.findByRole('tooltip')).toHaveTextContent('client_gone')
  })

  test.each([
    ['', 2],
    ['ordinary failure text', 5],
    ['{"kind":"upstream_business_error"}', 2],
    [JSON.stringify({ ...failure, kind: 'other_error' }), 2],
    [
      JSON.stringify({
        ...failure,
        kind: 'upstream_business_error',
        http_status: 500,
      }),
      2,
    ],
    [JSON.stringify({ ...failure, kind: 'upstream_business_error' }), 6],
  ])('does not mark unrelated or invalid content %#', (content, type) => {
    renderBusinessErrorStatus({
      ...log,
      content: String(content),
      type: Number(type),
    })
    expect(
      screen.queryByRole('button', {
        name: 'Upstream business error (HTTP 200)',
      })
    ).not.toBeInTheDocument()
  })

  test.each([2, 5])(
    'shows formatted copyable JSON for log type %s',
    async (type) => {
      const user = userEvent.setup()
      const copy = vi
        .spyOn(navigator.clipboard, 'writeText')
        .mockResolvedValue()
      vi.spyOn(api, 'get').mockResolvedValue(response([]))
      renderDetails({ log: { ...log, type, content: JSON.stringify(failure) } })
      expect(await screen.findByText('Upstream error JSON')).toBeInTheDocument()
      const content = screen.getByText(/"http_status": 200/)
      expect(content.textContent).toBe(JSON.stringify(failure, null, 2))
      expect(content.querySelector('script')).toBeNull()
      expect(
        screen.getByText('Error details were shortened.')
      ).toBeInTheDocument()
      const section = content.parentElement
      expect(section).not.toBeNull()
      const button = section?.querySelector('button')
      expect(button).not.toBeNull()
      if (button) await user.click(button)
      expect(copy).toHaveBeenCalledWith(JSON.stringify(failure, null, 2))
    }
  )

  test.each([1, 3, 6, 7])(
    'keeps structured-looking text unchanged for other log type %s',
    async (type) => {
      vi.spyOn(api, 'get').mockResolvedValue(response([]))
      const content = JSON.stringify(failure)
      renderDetails({ log: { ...log, type, content } })
      expect(await screen.findByText(content)).toBeInTheDocument()
      expect(screen.queryByText('Upstream error JSON')).not.toBeInTheDocument()
      expect(
        screen.queryByText('Error details were shortened.')
      ).not.toBeInTheDocument()
    }
  )

  test.each([
    'legacy error text',
    '{invalid JSON',
    '{"error":{"message":"legacy"}}',
  ])('preserves historical content %s', async (content) => {
    vi.spyOn(api, 'get').mockResolvedValue(response([]))
    renderDetails({ log: { ...log, content } })
    expect(await screen.findByText(content)).toBeInTheDocument()
    expect(screen.queryByText('Upstream error JSON')).not.toBeInTheDocument()
  })

  test.each([2, 5])(
    'prioritizes business failure over billing and admin summaries for type %s',
    async (type) => {
      vi.spyOn(api, 'get').mockResolvedValue(response([]))
      render(
        <QueryClientProvider client={queryClient()}>
          <UsageLogsProvider>
            <BusinessErrorLogCell
              log={{
                ...log,
                type,
                content: JSON.stringify(failure),
                other: JSON.stringify({
                  model_ratio: 1,
                  admin_info: { quota_saturation: { field: 'quota' } },
                }),
              }}
            />
          </UsageLogsProvider>
        </QueryClientProvider>
      )
      const preview = screen.getByRole('button', {
        name: /Upstream failed · HTTP 200 · upstream_timeout/,
      })
      expect(preview).not.toHaveTextContent('Quota clamped')
      await userEvent.click(preview)
      expect(await screen.findByText('Upstream error JSON')).toBeInTheDocument()
    }
  )

  test.each([
    { ...failure, kind: 'other_error' },
    { ...failure, kind: null },
    { ...failure, http_status: 502 },
    { ...failure, event: 'response.completed' },
    { ...failure, reported_status_code: '429' },
    { ...failure, reported_status_code: 600 },
    { ...failure, error: { message: [] } },
    { ...failure, error: null },
  ])('rejects content outside the business-error contract %#', (content) => {
    expect(parseUpstreamBusinessError(JSON.stringify(content))).toBeNull()
  })

  test('retains numeric zero codes and missing fields without inventing values', () => {
    expect(
      parseUpstreamBusinessError(
        '{"http_status":200,"event":"error","error":{"code":0}}'
      )
    ).toEqual({ http_status: 200, event: 'error', error: { code: 0 } })
    expect(
      parseUpstreamBusinessError(
        '{"http_status":200,"event":"response.failed","error":{}}'
      )
    ).toEqual({ http_status: 200, event: 'response.failed', error: {} })
  })
})

describe('request trace log details', () => {
  const formattedUsage =
    '{\n  "zero": 0,\n  "wide": 9007199254740993,\n  "nullable": null\n}'
  const usageRecord = {
    ...detailed,
    admin_info: {
      ...detailed.admin_info,
      upstream: {
        ...detailed.admin_info.upstream,
        usage: {
          format: 'openai',
          parts: [{ path: 'usage', raw_value: formattedUsage }],
        },
      },
    },
  }

  test('shows formatted usage directly with a copy control and preserves numeric precision', async () => {
    const user = userEvent.setup()
    const writeText = vi
      .spyOn(navigator.clipboard, 'writeText')
      .mockResolvedValue()
    vi.spyOn(api, 'get').mockResolvedValue(response([usageRecord]))
    renderDetails({ isAdmin: true })
    const content = await screen.findByText(
      (_text, element) =>
        element?.tagName === 'P' && element.textContent === formattedUsage
    )
    expect(content).toBeVisible()
    expect(content).toHaveClass('font-mono', 'whitespace-pre-wrap')
    expect(
      screen.queryByRole('button', { name: 'View usage' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('textbox', { name: 'json' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Copy to clipboard' }))
    expect(writeText).toHaveBeenCalledWith(formattedUsage)
  })

  test('updates directly displayed usage when switching between cached request details', async () => {
    const client = queryClient()
    const nextUsage = formattedUsage.replace('"zero": 0', '"zero": 1')
    client.setQueryDefaults(['request-traces'], { gcTime: Infinity })
    vi.spyOn(api, 'get').mockResolvedValue(response([]))
    for (const id of ['trace-first', 'trace-second']) {
      client.setQueryData(['request-traces', 11, 1, id], {
        records: [
          {
            ...usageRecord,
            request_id: id,
            admin_info: {
              ...usageRecord.admin_info,
              upstream: {
                ...usageRecord.admin_info.upstream,
                usage: {
                  parts: [
                    {
                      path: 'usage',
                      raw_value:
                        id === 'trace-first' ? formattedUsage : nextUsage,
                    },
                  ],
                },
              },
            },
          },
        ],
        pending: false,
      })
    }
    const view = render(
      <QueryClientProvider client={client}>
        <DetailsDialog
          log={{ ...log, request_id: 'trace-first' }}
          open
          isAdmin
          isRoot={false}
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )
    expect(
      await screen.findByText(
        (_text, element) =>
          element?.tagName === 'P' && element.textContent === formattedUsage
      )
    ).toBeVisible()
    view.rerender(
      <QueryClientProvider client={client}>
        <DetailsDialog
          log={{ ...log, request_id: 'trace-second' }}
          open
          isAdmin
          isRoot={false}
          onOpenChange={() => undefined}
        />
      </QueryClientProvider>
    )
    expect(
      await screen.findByText(
        (_text, element) =>
          element?.tagName === 'P' && element.textContent === nextUsage
      )
    ).toBeVisible()
    expect(
      screen.queryByText(
        (_text, element) =>
          element?.tagName === 'P' && element.textContent === formattedUsage
      )
    ).not.toBeInTheDocument()
  })

  test('does not expose upstream usage to ordinary users', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(response([usageRecord]))
    renderDetails()
    await screen.findByText('Input Tokens')
    expect(screen.queryByText('Upstream usage')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'View usage' })
    ).not.toBeInTheDocument()
  })

  test.each([false, true])(
    'distinguishes missing from incomplete usage, incomplete=%s',
    async (incomplete) => {
      vi.spyOn(api, 'get').mockResolvedValue(
        response([
          {
            ...usageRecord,
            admin_info: {
              ...usageRecord.admin_info,
              upstream: {
                ...usageRecord.admin_info.upstream,
                usage: { parts: [], incomplete },
              },
            },
          },
        ])
      )
      renderDetails({ isAdmin: true })
      const message = incomplete
        ? 'Upstream usage was not captured completely.'
        : 'Upstream usage was not reported.'
      expect(await screen.findByText(message)).toBeVisible()
      expect(
        screen.queryByRole('button', { name: 'View usage' })
      ).not.toBeInTheDocument()
    }
  )

  test('shows usage from retry attempts directly in separate sections', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([usageRecord, { ...usageRecord, attempt: 1 }])
    )
    renderDetails({ isAdmin: true })
    const values = await screen.findAllByText(
      (_text, element) =>
        element?.tagName === 'P' && element.textContent === formattedUsage
    )
    expect(values).toHaveLength(2)
    expect(screen.getByText('Attempt 1')).toBeVisible()
    expect(screen.getByText('Attempt 2')).toBeVisible()
    expect(values[0]).toBeVisible()
    expect(values[1]).toBeVisible()
    expect(
      screen.queryByRole('button', { name: 'View usage' })
    ).not.toBeInTheDocument()
  })

  test('hides uncollected counts for summary metadata while preserving billing tokens', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          admin_info: {
            ...detailed.admin_info,
            metadata_level: 'summary',
            resp: {
              ...detailed.admin_info.resp,
              tool_calls: 2,
              tool_calls_by_name: { search: 2 },
              metadata_incomplete: true,
            },
          },
        },
      ])
    )
    renderDetails({ isAdmin: true, isRoot: true })
    await screen.findByText('Finish reason')
    expect(screen.queryByText('Body: 61 characters')).not.toBeInTheDocument()
    expect(
      screen.queryByText('Reasoning: 318 characters')
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('region', { name: 'Tool calls' })
    ).not.toBeInTheDocument()
    expect(screen.queryByText('33 characters')).not.toBeInTheDocument()
    expect(screen.getByText('Input Tokens')).toBeInTheDocument()
    expect(screen.getByText('Output Tokens')).toBeInTheDocument()
  })

  test('hides partial tool counts after a late capture upgrade', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(
      response([
        {
          ...detailed,
          admin_info: {
            ...detailed.admin_info,
            metadata_level: 'full',
            resp: {
              ...detailed.admin_info.resp,
              tool_calls: 2,
              tool_calls_by_name: { search: 2 },
              metadata_incomplete: true,
              incomplete_reasons: ['capture_upgraded'],
            },
          },
        },
      ])
    )
    renderDetails({ isAdmin: true, isRoot: true })
    await screen.findByText('Finish reason')
    expect(
      screen.queryByRole('region', { name: 'Tool calls' })
    ).not.toBeInTheDocument()
    expect(screen.queryByText('Body: 61 characters')).not.toBeInTheDocument()
  })

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
    await screen.findByText('Finish reason')
    expect(screen.getAllByText('Response Time')).toHaveLength(1)
    expect(screen.getByText('15 ms')).toBeInTheDocument()
    expect(screen.queryByText('Request trace')).not.toBeInTheDocument()
    expect(screen.queryByText('Request metadata')).not.toBeInTheDocument()
    const headings = ['Request Conversion', 'Request', 'Upstream', 'Response']
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

  test('hides removed diagnostics in legacy records and preserves billing tokens', async () => {
    vi.spyOn(api, 'get').mockResolvedValue(response([detailed]))
    renderDetails({ isAdmin: true, isRoot: true })
    await screen.findByText('Finish reason')
    for (const label of [
      'Top-level fields',
      'Response body',
      'Output',
      'Streaming',
      'Field-name fingerprint',
    ]) {
      expect(screen.queryByText(label, { exact: true })).not.toBeInTheDocument()
    }
    expect(screen.queryByText('Body: 61 characters')).not.toBeInTheDocument()
    expect(
      screen.queryByText('Reasoning: 318 characters')
    ).not.toBeInTheDocument()
    expect(screen.getByText('provider-response')).toBeInTheDocument()
    expect(screen.getByText('Input Tokens')).toBeInTheDocument()
    expect(screen.getByText('Output Tokens')).toBeInTheDocument()
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

  test('shows truncation and the missing end marker without removed statistics', async () => {
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
    expect(screen.queryByText('Body: 0 characters')).not.toBeInTheDocument()
    expect(
      screen.getByText('Output was truncated at the configured limit.')
    ).toBeInTheDocument()
    expect(screen.queryByText(/First: 2.81 s/)).not.toBeInTheDocument()
    expect(screen.queryByText(/Last: 15.12 s/)).not.toBeInTheDocument()
    expect(screen.getByText('End marker missing')).toHaveClass(
      'text-destructive'
    )
    expect(screen.queryByText(/Usage event missing/)).not.toBeInTheDocument()
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

  test('shows only the final attempt for administrators', async () => {
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
    await screen.findByText('Finish reason')
    expect(
      screen.queryByRole('button', { name: 'Previous upstream attempts (1)' })
    ).not.toBeInTheDocument()
    expect(screen.queryByText('first-attempt-id')).not.toBeInTheDocument()
    expect(screen.queryByText('overloaded')).not.toBeInTheDocument()
  })

  test.each([
    ['failed', 'failed', true, 'required', false, 'Failed'],
    ['PROVIDER_UNKNOWN', 'unknown', false, 'required', false, 'Unknown'],
    ['SAFETY', 'success', false, 'not_applicable', false, 'Refused'],
    ['IMAGE_SAFETY', 'success', false, 'not_applicable', false, 'Refused'],
    ['refusal', 'success', false, 'required', true, 'Refused'],
    [
      'incomplete:content_filter',
      'success',
      false,
      'required',
      true,
      'Refused',
    ],
    ['incomplete', 'success', false, 'required', true, 'Incomplete'],
    [
      'incomplete:max_output_tokens',
      'success',
      false,
      'required',
      true,
      'Truncated',
    ],
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
      if (label === 'Truncated') {
        expect(
          screen.getByText('Output was truncated at the configured limit.')
        ).toBeInTheDocument()
      } else {
        expect(
          screen.queryByText('Output was truncated at the configured limit.')
        ).not.toBeInTheDocument()
      }
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

  test('shows request elapsed time without per-attempt timing rows', async () => {
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
    expect(await screen.findByText('5.00 s')).toBeVisible()
    expect(screen.queryByText('22 ms')).not.toBeInTheDocument()
    expect(screen.queryByText('11 ms')).not.toBeInTheDocument()
    expect(screen.queryByText('Attempt duration')).not.toBeInTheDocument()
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
