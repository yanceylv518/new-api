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
  fireEvent,
  render,
  screen,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18n from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api, type ApiRequestConfig } from '@/lib/api'

import type { RequestTimelineResult } from '../api'
import { RequestTimelineDialog } from '../request-timeline-dialog'
import { retriedStreamResult } from './fixtures'

const originalGet = api.get
let requestedUrls: string[] = []
let response: RequestTimelineResult
let requestError: Error | null = null
const clients = new Set<QueryClient>()

function renderDialog(props: { requestId: string | null; admin: boolean }) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.add(client)
  const dialog = (open: boolean) => (
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <RequestTimelineDialog
          open={open}
          onOpenChange={() => undefined}
          requestId={props.requestId}
          admin={props.admin}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  const view = render(dialog(true))
  return {
    ...view,
    client,
    setOpen: (open: boolean) => view.rerender(dialog(open)),
  }
}

function missingTimeline(enabled = true): RequestTimelineResult {
  return {
    found: false,
    enabled,
    retention_days: 7,
    summary: null,
    detail: null,
  }
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

beforeEach(async () => {
  await i18n.changeLanguage('en')
  requestedUrls = []
  response = retriedStreamResult()
  requestError = null
  // 只替换网络边界，返回与接口一致的追踪数据。
  api.get = (async (url: string) => {
    requestedUrls.push(url)
    if (requestError) throw requestError
    return { data: { success: true, data: response } }
  }) as typeof api.get
})
afterEach(() => {
  cleanup()
  for (const client of clients) client.clear()
  clients.clear()
  vi.useRealTimers()
  api.get = originalGet
})

describe('request timeline dialog', () => {
  test('searching an invalid request ID shows an error without fetching', async () => {
    renderDialog({ requestId: null, admin: true })

    await userEvent.type(screen.getByLabelText('Request ID'), 'not valid!')
    await userEvent.click(screen.getByRole('button', { name: 'Search' }))

    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(requestedUrls).toEqual([])
  })

  test('an administrator can collapse an attempt to hide its steps', async () => {
    renderDialog({ requestId: 'req_retry_1', admin: true })
    const stages = await screen.findByRole('list', { name: 'Request stages' })
    const toggle = within(stages).getByRole('button', {
      name: 'Collapse Attempt 2 · Channel #27',
    })
    expect(within(stages).getAllByText('Wait for response')).toHaveLength(1)

    await userEvent.click(toggle)

    expect(
      within(stages).getByRole('button', {
        name: 'Expand Attempt 2 · Channel #27',
      })
    ).toHaveAttribute('aria-expanded', 'false')
    expect(
      within(stages).queryByText('Wait for response')
    ).not.toBeInTheDocument()
    expect(requestedUrls).toEqual(['/api/request-traces/req_retry_1'])
  })

  test('selecting a stage shows its details in the inspector', async () => {
    renderDialog({ requestId: 'req_retry_1', admin: true })
    const stages = await screen.findByRole('list', { name: 'Request stages' })

    await userEvent.click(
      within(stages).getAllByRole('button', { name: 'Channel handling' })[0]
    )

    const inspector = screen.getByRole('region', { name: 'Selected stage' })
    expect(
      within(inspector).getByRole('heading', { name: 'Channel handling' })
    ).toBeInTheDocument()
    expect(
      within(stages).getAllByRole('button', { name: 'Channel handling' })[0]
    ).toHaveAttribute('aria-pressed', 'true')
  })

  test('a regular user sees the summary but no stage details', async () => {
    response = { ...retriedStreamResult(), detail: null }
    renderDialog({ requestId: 'req_retry_1', admin: false })

    expect(
      await screen.findByText('Server processing time')
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('list', { name: 'Request stages' })
    ).not.toBeInTheDocument()
    expect(requestedUrls).toEqual(['/api/request-traces/self/req_retry_1'])
  })

  test('a missing timeline is rechecked three times and then waits for manual refresh', async () => {
    vi.useFakeTimers()
    response = missingTimeline()
    renderDialog({ requestId: 'req_missing', admin: true })
    await advance(0)
    expect(requestedUrls).toHaveLength(1)
    for (const delay of [500, 1000, 2000]) await advance(delay)
    expect(requestedUrls).toHaveLength(4)
    expect(
      screen.getByText('Automatic rechecks finished. You can refresh manually.')
    ).toBeInTheDocument()
    await advance(20_000)
    expect(requestedUrls).toHaveLength(4)
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }))
    await advance(0)
    expect(requestedUrls).toHaveLength(5)
    await advance(500)
    expect(requestedUrls).toHaveLength(6)
  })

  test('a newly persisted timeline stops rechecks after it is found', async () => {
    vi.useFakeTimers()
    response = missingTimeline()
    renderDialog({ requestId: 'req_retry_1', admin: true })
    await advance(0)
    response = retriedStreamResult()
    await advance(500)
    expect(screen.getByText('Server processing time')).toBeInTheDocument()
    await advance(60_000)
    expect(requestedUrls).toHaveLength(2)
  })

  test('disabled collection leaves a missing result available for manual refresh only', async () => {
    vi.useFakeTimers()
    response = missingTimeline(false)
    renderDialog({ requestId: 'req_disabled', admin: true })
    await advance(0)
    await advance(20_000)
    expect(requestedUrls).toHaveLength(1)
    expect(
      screen.getByText('Request trace collection is currently disabled')
    ).toBeInTheDocument()
  })

  test('network errors stop automatic rechecks and allow an explicit retry', async () => {
    vi.useFakeTimers()
    requestError = new Error('controlled network failure')
    renderDialog({ requestId: 'req_retry_1', admin: true })
    await advance(0)
    await advance(20_000)
    expect(requestedUrls).toHaveLength(1)
    expect(
      screen.getByText('Unable to load request timeline')
    ).toBeInTheDocument()
    requestError = null
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await advance(0)
    expect(screen.getByText('Server processing time')).toBeInTheDocument()
    expect(requestedUrls).toHaveLength(2)
  })

  test('closing cancels a pending request and reopening starts a fresh request', async () => {
    vi.useFakeTimers()
    let stalled = true
    let observedSignal: ApiRequestConfig['signal']
    const networkGet = api.get
    api.get = (async (url: string, config: ApiRequestConfig = {}) => {
      if (!stalled) return networkGet(url, config)
      requestedUrls.push(url)
      observedSignal = config.signal
      return await new Promise((_, reject) => {
        config.signal?.addEventListener?.('abort', () =>
          reject(new Error('controlled abort'))
        )
      })
    }) as typeof api.get
    const view = renderDialog({ requestId: 'req_retry_1', admin: true })
    await advance(0)
    expect(observedSignal?.aborted).toBe(false)
    view.setOpen(false)
    await advance(0)
    expect(observedSignal?.aborted).toBe(true)
    await advance(5000)
    expect(requestedUrls).toHaveLength(1)
    stalled = false
    view.setOpen(true)
    await advance(0)
    expect(requestedUrls).toHaveLength(2)
    expect(screen.getByText('Server processing time')).toBeInTheDocument()
  })

  test('missing results are stale immediately when the dialog is reopened', async () => {
    vi.useFakeTimers()
    response = missingTimeline()
    const view = renderDialog({ requestId: 'req_missing', admin: true })
    await advance(0)
    view.setOpen(false)
    view.setOpen(true)
    await advance(0)
    expect(requestedUrls).toHaveLength(2)
  })

  test('completed results stay cached for thirty seconds across reopening', async () => {
    vi.useFakeTimers()
    const view = renderDialog({ requestId: 'req_retry_1', admin: true })
    await advance(0)
    view.setOpen(false)
    view.setOpen(true)
    await advance(0)
    expect(requestedUrls).toHaveLength(1)
    view.setOpen(false)
    await advance(30_001)
    view.setOpen(true)
    await advance(0)
    expect(requestedUrls).toHaveLength(2)
  })
})
