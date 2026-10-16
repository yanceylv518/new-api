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
import { cleanup, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18n from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test } from 'vitest'

import { api } from '@/lib/api'

import type { RequestTimelineResult } from '../api'
import { RequestTimelineDialog } from '../request-timeline-dialog'
import { retriedStreamResult } from './fixtures'

const originalGet = api.get
let requestedUrls: string[] = []
let response: RequestTimelineResult

function renderDialog(props: { requestId: string | null; admin: boolean }) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <RequestTimelineDialog
          open
          onOpenChange={() => undefined}
          requestId={props.requestId}
          admin={props.admin}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
}

beforeEach(async () => {
  await i18n.changeLanguage('en')
  requestedUrls = []
  response = retriedStreamResult()
  // 只替换网络边界，返回与接口一致的追踪数据。
  api.get = (async (url: string) => {
    requestedUrls.push(url)
    return { data: { success: true, data: response } }
  }) as typeof api.get
})
afterEach(() => {
  cleanup()
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
})
