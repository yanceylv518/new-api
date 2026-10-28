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
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosHeaders } from 'axios'
import i18n from 'i18next'
import { useState } from 'react'
import { beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { LogSettingsSection } from '../log-settings-section'

function response(data: unknown) {
  return {
    data,
    status: 200,
    statusText: 'OK',
    headers: new AxiosHeaders(),
    config: { headers: new AxiosHeaders() },
  }
}

const config = {
  level: 'errors',
  pre_channel_errors_enabled: true,
  anonymous_errors_enabled: false,
  upstream_request_id_headers: ['X-Existing-Id'],
  compression_level: 'fast',
  success_sample_rate: 100,
  retention_days: 0,
  diagnostic_channel_ids: [],
  diagnostic_expires_at: 0,
}

function SettingsFixture(props: { config?: string }) {
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: { retry: false },
          mutations: { retry: false },
        },
      })
  )
  const [actions, setActions] = useState<HTMLDivElement | null>(null)
  return (
    <QueryClientProvider client={client}>
      <div ref={setActions} />
      <SettingsPageProvider actionsContainer={actions}>
        <LogSettingsSection defaultEnabled defaultTraceConfig={props.config} />
      </SettingsPageProvider>
    </QueryClientProvider>
  )
}

async function renderTraceSettings(element: Parameters<typeof render>[0]) {
  const view = render(element)
  await userEvent.click(
    screen.getByRole('button', { name: 'Request trace settings' })
  )
  return view
}

beforeEach(async () => {
  await i18n.changeLanguage('en')
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/performance/logs') {
      return response({
        success: true,
        data: { enabled: false, file_count: 0, total_size: 0, log_dir: '' },
      })
    }
    if (url === '/api/system-task/current') {
      return response({ success: true, data: null })
    }
    if (url === '/api/log/trace/status') {
      return response({
        success: true,
        data: {
          node: 'trace-test-instance',
          running: true,
          queued_rows: 3,
          queued_bytes: 1024,
          queue_row_limit: 8192,
          queue_byte_limit: 16777216,
          in_flight_rows: 1,
          persisted_rows: 57,
          failed_rows: 2,
          fallback_rows: 0,
          dropped_rows: 40,
          dropped_priority_rows: 17,
          dropped_regular_rows: 23,
          retries: 1,
          collected_requests: 80,
          sampled_out_requests: 20,
          partial_requests: 2,
          average_write_ms: 1.5,
          last_write_ms: 2.3,
          cleanup_error: 'synthetic cleanup read error',
        },
      })
    }
    throw new Error(`Unexpected GET: ${url}`)
  })
  vi.spyOn(api, 'put').mockResolvedValue(response({ success: true }))
})

describe('request trace log settings', () => {
  test('keeps settings collapsed and writer polling unmounted until opened', async () => {
    const previous = useAuthStore.getState().auth.user
    useAuthStore
      .getState()
      .auth.setUser({ id: 11, username: 'trace-root', role: 100 })
    try {
      render(<SettingsFixture config={JSON.stringify(config)} />)
      const accordion = screen.getByRole('button', {
        name: 'Request trace settings',
      })
      expect(accordion).toHaveAttribute('aria-expanded', 'false')
      expect(
        screen.queryByRole('textbox', {
          name: 'Upstream request ID response headers',
        })
      ).not.toBeInTheDocument()
      expect(
        vi
          .mocked(api.get)
          .mock.calls.some(([url]) => url === '/api/log/trace/status')
      ).toBe(false)
      await userEvent.click(accordion)
      await screen.findByRole('region', { name: 'Asynchronous trace writer' })
      expect(accordion).toHaveAttribute('aria-expanded', 'true')
      await userEvent.click(accordion)
      expect(accordion).toHaveAttribute('aria-expanded', 'false')
      await waitFor(() =>
        expect(
          screen.queryByRole('region', { name: 'Asynchronous trace writer' })
        ).not.toBeInTheDocument()
      )
    } finally {
      useAuthStore.getState().auth.setUser(previous)
    }
  })

  test('retains the selected detail level when toggled and saves off', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify({ ...config, level: 'all' })} />
    )
    const toggle = screen.getByRole('switch', {
      name: 'Enable request tracing',
    })
    await userEvent.click(toggle)
    await userEvent.click(toggle)
    expect(
      screen.getByRole('combobox', { name: 'Request trace level' })
    ).toHaveTextContent('All requests in detail')
    await userEvent.click(toggle)
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() =>
      expect(api.put).toHaveBeenLastCalledWith('/api/option/', {
        key: 'RequestTraceConfig',
        value: JSON.stringify({ ...config, level: 'off' }),
      })
    )
  })

  test.each(['high', 'best'])(
    'maps legacy %s compression to standard',
    async (compression) => {
      await renderTraceSettings(
        <SettingsFixture
          config={JSON.stringify({ ...config, compression_level: compression })}
        />
      )
      const select = screen.getByRole('combobox', { name: 'Compression level' })
      expect(select).toHaveTextContent('Standard compression')
      await userEvent.click(select)
      expect(screen.getAllByRole('option')).toHaveLength(2)
    }
  )

  test.each([
    ['off', 'Detailed failures only'],
    ['errors', 'Detailed failures only'],
    ['all', 'All requests in detail'],
  ])(
    'shows the label for saved level %s before opening the dropdown',
    async (level, label) => {
      await renderTraceSettings(
        <SettingsFixture config={JSON.stringify({ ...config, level })} />
      )
      expect(
        screen.getByRole('combobox', { name: 'Request trace level' })
      ).toHaveTextContent(label)
      expect(
        screen.getByRole('combobox', { name: 'Request trace level' })
      ).not.toHaveTextContent(new RegExp(`^${level}$`))
    }
  )

  test('supports old configs and updates inputs when settings are reloaded', async () => {
    const view = await renderTraceSettings(
      <SettingsFixture config='{"level":"errors","pre_channel_errors_enabled":true,"anonymous_errors_enabled":false}' />
    )
    expect(
      screen.getByRole('textbox', {
        name: 'Upstream request ID response headers',
      })
    ).toHaveValue('')
    view.rerender(<SettingsFixture config={JSON.stringify(config)} />)
    await waitFor(() =>
      expect(
        screen.getByRole('textbox', {
          name: 'Upstream request ID response headers',
        })
      ).toHaveValue('X-Existing-Id')
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    expect(api.put).not.toHaveBeenCalled()
  })

  test('saves custom priority and preserves unrelated trace options', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    const input = screen.getByRole('textbox', {
      name: 'Upstream request ID response headers',
    })
    await userEvent.clear(input)
    await userEvent.type(input, 'X-First-Id\nNvcf-Reqid\nx-first-id')
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith('/api/option/', {
        key: 'RequestTraceConfig',
        value: JSON.stringify({
          ...config,
          upstream_request_id_headers: ['x-first-id', 'Nvcf-Reqid'],
        }),
      })
    )
  })

  test('keeps header configuration editable when request tracing is off', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify({ ...config, level: 'off' })} />
    )
    const input = screen.getByRole('textbox', {
      name: 'Upstream request ID response headers',
    })
    expect(input).toBeEnabled()
    expect(
      screen.getByRole('switch', {
        name: 'Record failures before upstream dispatch',
      })
    ).toHaveAttribute('aria-disabled', 'true')
    await userEvent.clear(input)
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith('/api/option/', {
        key: 'RequestTraceConfig',
        value: JSON.stringify({
          ...config,
          level: 'off',
          upstream_request_id_headers: [],
        }),
      })
    )
  })

  test.each([
    ['invalid header token', 'X Bad-Id'],
    ['overlong name', 'a'.repeat(129)],
    [
      'too many names',
      Array.from({ length: 17 }, (_, index) => `X-Id-${index}`).join('\n'),
    ],
  ])('blocks saving %s before any setting mutation', async (_name, value) => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    const input = screen.getByRole('textbox', {
      name: 'Upstream request ID response headers',
    })
    await userEvent.click(
      screen.getByRole('switch', { name: 'Record quota usage' })
    )
    await userEvent.clear(input)
    await userEvent.type(input, value)
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await screen.findByText(
      'Enter up to 16 valid response header names, each at most 128 characters.'
    )
    expect(input).toHaveFocus()
    expect(input).toHaveAttribute('aria-invalid', 'true')
    expect(api.put).not.toHaveBeenCalled()
  })

  test('uses translated dropdown options after changing the level', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    await userEvent.click(
      screen.getByRole('combobox', { name: 'Request trace level' })
    )
    await userEvent.click(
      screen.getByRole('option', { name: 'All requests in detail' })
    )
    expect(
      screen.getByRole('combobox', { name: 'Request trace level' })
    ).toHaveTextContent('All requests in detail')
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith('/api/option/', {
        key: 'RequestTraceConfig',
        value: JSON.stringify({ ...config, level: 'all' }),
      })
    )
  })

  test('keeps user input after a failed save and allows a retry', async () => {
    vi.mocked(api.put).mockRejectedValueOnce(new Error('Save unavailable'))
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    const input = screen.getByRole('textbox', {
      name: 'Upstream request ID response headers',
    })
    await userEvent.clear(input)
    await userEvent.type(input, 'Nvcf-Reqid')
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() => expect(api.put).toHaveBeenCalledTimes(1))
    expect(input).toHaveValue('Nvcf-Reqid')
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Save log settings' })
      ).toBeEnabled()
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() => expect(api.put).toHaveBeenCalledTimes(2))
  })

  test('locks controls while saving and requires a new change after success', async () => {
    let complete: ((value: ReturnType<typeof response>) => void) | undefined
    vi.mocked(api.put).mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          complete = resolve
        })
    )
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    const input = screen.getByRole('textbox', {
      name: 'Upstream request ID response headers',
    })
    await userEvent.clear(input)
    await userEvent.type(input, 'Nvcf-Reqid')
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Saving...' })).toBeDisabled()
    )
    expect(
      screen.getByRole('combobox', { name: 'Request trace level' })
    ).toBeDisabled()
    await act(async () => complete?.(response({ success: true })))
    await waitFor(() =>
      expect(
        screen.getByRole('combobox', { name: 'Request trace level' })
      ).toBeEnabled()
    )
    expect(
      screen.getByRole('button', { name: 'Save log settings' })
    ).toBeDisabled()
    await userEvent.type(input, '\nX-New-Id')
    expect(
      screen.getByRole('button', { name: 'Save log settings' })
    ).toBeEnabled()
  })

  test('saves all metadata and timeline switches to their own options', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify({ ...config, level: 'all' })} />
    )
    const input = screen.getByRole('textbox', {
      name: 'Upstream request ID response headers',
    })
    await userEvent.clear(input)
    await userEvent.type(input, 'X-Combined-Id')
    await userEvent.click(
      screen.getByRole('switch', { name: 'Collect request timelines' })
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() => expect(api.put).toHaveBeenCalledTimes(2))
    expect(api.put).toHaveBeenNthCalledWith(1, '/api/option/', {
      key: 'RequestTraceConfig',
      value: JSON.stringify({
        ...config,
        level: 'all',
        upstream_request_id_headers: ['X-Combined-Id'],
      }),
    })
    expect(api.put).toHaveBeenNthCalledWith(2, '/api/option/', {
      key: 'request_trace.enabled',
      value: false,
    })
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Save log settings' })
      ).toBeDisabled()
    )
  })

  test.each([0, 35, 100])(
    'saves the successful request sample rate %s',
    async (rate) => {
      await renderTraceSettings(
        <SettingsFixture
          config={JSON.stringify({
            ...config,
            success_sample_rate: rate === 100 ? 35 : 100,
          })}
        />
      )
      const input = screen.getByRole('spinbutton', {
        name: 'Successful request sample rate (%)',
      })
      await userEvent.clear(input)
      await userEvent.type(input, String(rate))
      await userEvent.click(
        screen.getByRole('button', { name: 'Save log settings' })
      )
      await waitFor(() =>
        expect(api.put).toHaveBeenCalledWith('/api/option/', {
          key: 'RequestTraceConfig',
          value: JSON.stringify({ ...config, success_sample_rate: rate }),
        })
      )
    }
  )

  test('saves independent retention and leaves consumption logging enabled', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    const input = screen.getByRole('spinbutton', {
      name: 'Request trace retention (days)',
    })
    await userEvent.clear(input)
    await userEvent.type(input, '7')
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith('/api/option/', {
        key: 'RequestTraceConfig',
        value: JSON.stringify({ ...config, retention_days: 7 }),
      })
    )
    expect(
      screen.getByRole('switch', { name: 'Record quota usage' })
    ).toHaveAttribute('aria-checked', 'true')
  })

  test.each([
    ['Successful request sample rate (%)', '101'],
    ['Request trace retention (days)', '3651'],
  ])(
    'rejects out-of-range %s before issuing a mutation',
    async (name, value) => {
      await renderTraceSettings(
        <SettingsFixture config={JSON.stringify(config)} />
      )
      const input = screen.getByRole('spinbutton', { name })
      await userEvent.clear(input)
      await userEvent.type(input, value)
      await userEvent.click(
        screen.getByRole('button', { name: 'Save log settings' })
      )
      expect(api.put).not.toHaveBeenCalled()
    }
  )

  test('deduplicates diagnostic channel IDs and preserves a valid expiry', async () => {
    const expires = Math.floor(Date.now() / 1000) + 3600
    await renderTraceSettings(
      <SettingsFixture
        config={JSON.stringify({ ...config, diagnostic_expires_at: expires })}
      />
    )
    await userEvent.type(
      screen.getByRole('textbox', { name: 'Temporary diagnostic channel IDs' }),
      '7, 8, 7'
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith('/api/option/', {
        key: 'RequestTraceConfig',
        value: JSON.stringify({
          ...config,
          diagnostic_channel_ids: [7, 8],
          diagnostic_expires_at: expires,
        }),
      })
    )
  })

  test('rejects a newly configured channel without a future expiry', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    await userEvent.type(
      screen.getByRole('textbox', { name: 'Temporary diagnostic channel IDs' }),
      '7'
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    expect(
      await screen.findByText(
        'Choose a future diagnostic expiry within 24 hours.'
      )
    ).toBeInTheDocument()
    expect(api.put).not.toHaveBeenCalled()
  })

  test('rejects malformed diagnostic channels before issuing a mutation', async () => {
    await renderTraceSettings(
      <SettingsFixture config={JSON.stringify(config)} />
    )
    await userEvent.type(
      screen.getByRole('textbox', { name: 'Temporary diagnostic channel IDs' }),
      '7, invalid'
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Save log settings' })
    )
    expect(
      await screen.findByText('Enter up to 16 positive integer channel IDs.')
    ).toBeInTheDocument()
    expect(api.put).not.toHaveBeenCalled()
  })

  test('shows instance writer counters even when cleanup history is unavailable', async () => {
    const previous = useAuthStore.getState().auth.user
    useAuthStore
      .getState()
      .auth.setUser({ id: 11, username: 'trace-root', role: 100 })
    try {
      await renderTraceSettings(
        <SettingsFixture config={JSON.stringify(config)} />
      )
      const region = await screen.findByRole('region', {
        name: 'Asynchronous trace writer',
      })
      await within(region).findByText(
        'Responding instance: trace-test-instance'
      )
      expect(within(region).getByText('57')).toBeInTheDocument()
      expect(
        within(region).getByText(
          'Trace rows dropped for failed or diagnostic requests'
        )
      ).toBeInTheDocument()
      expect(
        within(region).getByText('Trace rows dropped for regular requests')
      ).toBeInTheDocument()
      expect(within(region).getByText('17')).toBeInTheDocument()
      expect(within(region).getByText('23')).toBeInTheDocument()
      expect(within(region).getByText('3 / 8,192')).toBeInTheDocument()
      expect(
        within(region).getByText(
          'Request trace cleanup: synthetic cleanup read error'
        )
      ).toBeInTheDocument()
    } finally {
      useAuthStore.getState().auth.setUser(previous)
    }
  })
})
