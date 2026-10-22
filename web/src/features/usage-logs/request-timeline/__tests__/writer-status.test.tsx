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
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react'
import i18n from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import type { RequestTimelineWriterStatus } from '../api'
import { RequestTimelineWriterStatusPanel } from '../components/writer-status'
import { writerStatusResult } from './fixtures'

const originalGet = api.get
const clients = new Set<QueryClient>()
let response: RequestTimelineWriterStatus
let failure: Error | null
let requests: number

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.add(client)
  return render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <RequestTimelineWriterStatusPanel />
      </I18nextProvider>
    </QueryClientProvider>
  )
}

async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

beforeEach(async () => {
  await i18n.changeLanguage('en')
  response = writerStatusResult()
  failure = null
  requests = 0
  api.get = (async () => {
    requests++
    if (failure) throw failure
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

describe('timeline writer status', () => {
  test('a collapsed panel makes no request and expanding shows node, stage and drop counts', async () => {
    renderPanel()
    expect(requests).toBe(0)
    fireEvent.click(
      screen.getByRole('button', {
        name: 'Timeline collection and storage status',
      })
    )
    expect(
      await screen.findByText('fixture-node', { exact: false })
    ).toBeInTheDocument()
    expect(screen.getByText('250.000 ms')).toBeInTheDocument()
    expect(screen.getByText('200.123 ms')).toBeInTheDocument()
    expect(screen.getByText('Drop reasons since startup')).toBeInTheDocument()
    expect(requests).toBe(1)
  })

  test('automatic refresh runs while expanded and stops after collapse', async () => {
    vi.useFakeTimers()
    renderPanel()
    fireEvent.click(
      screen.getByRole('button', {
        name: 'Timeline collection and storage status',
      })
    )
    await advance(0)
    await advance(5000)
    expect(requests).toBe(2)
    fireEvent.click(
      screen.getByRole('button', {
        name: 'Timeline collection and storage status',
      })
    )
    await advance(20_000)
    expect(requests).toBe(2)
  })

  test('a failed status query pauses automatic refresh and permits manual retry', async () => {
    vi.useFakeTimers()
    failure = new Error('controlled status error')
    renderPanel()
    fireEvent.click(
      screen.getByRole('button', {
        name: 'Timeline collection and storage status',
      })
    )
    await advance(0)
    await advance(20_000)
    expect(requests).toBe(1)
    expect(
      screen.getByText('Unable to load timeline writer status')
    ).toBeInTheDocument()
    failure = null
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await advance(0)
    expect(requests).toBe(2)
    expect(
      screen.getByText('fixture-node', { exact: false })
    ).toBeInTheDocument()
  })

  test('an uninitialized writer reports its state without inventing startup or batch times', async () => {
    response = {
      ...writerStatusResult(),
      initialized: false,
      started_at_us: 0,
      last_batch_duration_us: null,
    }
    renderPanel()
    fireEvent.click(
      screen.getByRole('button', {
        name: 'Timeline collection and storage status',
      })
    )
    expect(
      await screen.findByText('Timeline writer has not started')
    ).toBeInTheDocument()
    expect(screen.queryByText('Node started')).not.toBeInTheDocument()
  })

  test('unknown times render as missing while observed zero batch duration remains exact', async () => {
    response = {
      ...writerStatusResult(),
      outstanding: 0,
      pending: 0,
      writing: 0,
      accepted: 7,
      failed: 0,
      oldest_outstanding_age_us: null,
      last_failed_at_us: null,
      last_batch_duration_us: 0,
    }
    renderPanel()
    fireEvent.click(
      screen.getByRole('button', {
        name: 'Timeline collection and storage status',
      })
    )
    expect(await screen.findByText('0.000 ms')).toBeInTheDocument()
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(2)
  })
})
