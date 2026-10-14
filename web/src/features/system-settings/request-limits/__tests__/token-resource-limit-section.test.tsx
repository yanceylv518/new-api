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
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { TokenResourceLimitSection } from '../token-resource-limit-section'

afterEach(() => {
  cleanup()
  document.body.replaceChildren()
  vi.restoreAllMocks()
})

function renderSettings(concurrency = 0, bodyBytes = 0) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const actions = document.createElement('div')
  const container = document.createElement('div')
  container.appendChild(actions)
  document.body.appendChild(container)
  render(
    <QueryClientProvider client={client}>
      <SettingsPageProvider actionsContainer={actions}>
        <TokenResourceLimitSection
          defaultValues={{
            TokenResourceLimitsEnabled: false,
            TokenResourceDefaultMaxRequestBodyBytes: bodyBytes,
            TokenResourceDefaultMaxConcurrentRequests: concurrency,
          }}
        />
      </SettingsPageProvider>
    </QueryClientProvider>,
    { container: container.appendChild(document.createElement('div')) }
  )
}

it('displays an unlimited default concurrency as blank', () => {
  renderSettings()
  expect(
    screen.getByLabelText('Default concurrent requests per key')
  ).toHaveValue(null)
})

it('clearing an existing default concurrency saves unlimited', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderSettings(3)
  await userEvent.clear(
    screen.getByLabelText('Default concurrent requests per key')
  )
  await userEvent.click(
    screen.getByRole('button', { name: 'Save key resource limits' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'TokenResourceDefaultMaxConcurrentRequests',
      value: 0,
    })
  )
})

it('shows existing bytes in MB and saves decimal MB as bytes', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderSettings(0, 1048576)
  const body = screen.getByLabelText('Default request body limit (MB)')
  expect(body).toHaveValue('1')
  await userEvent.clear(body)
  await userEvent.type(body, '0.5')
  await userEvent.click(
    screen.getByRole('button', { name: 'Save key resource limits' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/option/', {
      key: 'TokenResourceDefaultMaxRequestBodyBytes',
      value: 524288,
    })
  )
})

it('persists limits before enabling enforcement', async () => {
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderSettings()
  await userEvent.click(
    screen.getByRole('switch', { name: 'Enable key resource limits' })
  )
  const input = screen.getByLabelText('Default concurrent requests per key')
  await userEvent.clear(input)
  await userEvent.type(input, '3')
  await userEvent.click(
    screen.getByRole('button', { name: 'Save key resource limits' })
  )
  await waitFor(() =>
    expect(put).toHaveBeenLastCalledWith('/api/option/', {
      key: 'TokenResourceLimitsEnabled',
      value: true,
    })
  )
  expect(put).toHaveBeenNthCalledWith(1, '/api/option/', {
    key: 'TokenResourceDefaultMaxConcurrentRequests',
    value: 3,
  })
})

it('does not enable enforcement if a default limit fails to save', async () => {
  const put = vi.spyOn(api, 'put').mockResolvedValue({
    data: { success: false, message: 'Database unavailable' },
  })
  renderSettings()
  await userEvent.click(
    screen.getByRole('switch', { name: 'Enable key resource limits' })
  )
  const input = screen.getByLabelText('Default concurrent requests per key')
  await userEvent.clear(input)
  await userEvent.type(input, '3')
  await userEvent.click(
    screen.getByRole('button', { name: 'Save key resource limits' })
  )
  await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Save key resource limits' })
    ).toBeEnabled()
  )
  expect(put.mock.calls[0][1]).toEqual({
    key: 'TokenResourceDefaultMaxConcurrentRequests',
    value: 3,
  })
})
