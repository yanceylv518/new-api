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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { UserTokenResourceDialog } from '../user-token-resource-dialog'

afterEach(() => vi.restoreAllMocks())

it('uses refreshed server limits when reopening a previously cached policy', async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const policy = {
    token_id: 42,
    name: 'Client',
    max_request_body_bytes: 1048576,
    max_concurrent_requests: 1,
  }
  client.setQueryData(['token-resource-policies', 7, 1], {
    total: 1,
    items: [policy],
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            ...policy,
            max_request_body_bytes: 2097152,
            max_concurrent_requests: 3,
          },
        ],
      },
    },
  })
  render(
    <QueryClientProvider client={client}>
      <UserTokenResourceDialog
        open
        onOpenChange={() => undefined}
        user={{ id: 7, username: 'Customer' }}
      />
    </QueryClientProvider>
  )
  await waitFor(() =>
    expect(screen.getByLabelText('Request body limit (MB)')).toHaveValue('2')
  )
  expect(screen.getByLabelText('Concurrent requests')).toHaveValue('3')
})

it('blocks editing cached limits when reopening until the fresh read completes', async () => {
  vi.spyOn(api, 'get')
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          total: 1,
          items: [
            {
              token_id: 42,
              name: 'Client',
              max_request_body_bytes: null,
              max_concurrent_requests: null,
            },
          ],
        },
      },
    })
    .mockImplementation(() => new Promise(() => {}))
  vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const dialog = (open: boolean) => (
    <QueryClientProvider client={client}>
      <UserTokenResourceDialog
        open={open}
        onOpenChange={() => undefined}
        user={{ id: 7, username: 'Customer' }}
      />
    </QueryClientProvider>
  )
  const view = render(dialog(true))
  await userEvent.type(
    await screen.findByLabelText('Request body limit (MB)'),
    '256'
  )
  await userEvent.type(screen.getByLabelText('Concurrent requests'), '2')
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  )
  view.rerender(dialog(false))
  view.rerender(dialog(true))
  expect(await screen.findByText('Loading...')).toBeVisible()
  expect(
    screen.queryByLabelText('Request body limit (MB)')
  ).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
})

function renderDialog(open = true) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <UserTokenResourceDialog
        open={open}
        onOpenChange={() => undefined}
        user={{ id: 7, username: 'Customer' }}
      />
    </QueryClientProvider>
  )
}

it('does not fetch private policies while the dialog is closed', () => {
  const get = vi.spyOn(api, 'get')
  renderDialog(false)
  expect(get).not.toHaveBeenCalled()
})

it.each([
  {
    enabled: true,
    max_request_body_bytes: 31457280,
    max_concurrent_requests: 5,
    body: '30 MB',
    concurrency: '5',
  },
  {
    enabled: true,
    max_request_body_bytes: 0,
    max_concurrent_requests: 0,
    body: 'Unlimited',
    concurrency: 'Unlimited',
  },
  {
    enabled: false,
    max_request_body_bytes: 524288,
    max_concurrent_requests: 3,
    body: '0.5 MB',
    concurrency: '3',
  },
])(
  'shows inherited defaults without populating overrides: $body / $concurrency, enabled=$enabled',
  async (defaults) => {
    vi.spyOn(api, 'get').mockResolvedValue({
      data: {
        success: true,
        data: {
          total: 1,
          defaults,
          items: [
            {
              token_id: 42,
              name: 'Client',
              max_request_body_bytes: null,
              max_concurrent_requests: null,
            },
          ],
        },
      },
    })
    renderDialog()
    const body = await screen.findByLabelText('Request body limit (MB)')
    expect(body).toHaveValue('')
    expect(body).toHaveAttribute(
      'placeholder',
      `Inherit default (${defaults.body})`
    )
    expect(screen.getByLabelText('Concurrent requests')).toHaveValue('')
    expect(screen.getByLabelText('Concurrent requests')).toHaveAttribute(
      'placeholder',
      `Inherit default (${defaults.concurrency})`
    )
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
    if (!defaults.enabled) {
      expect(screen.getByRole('alert')).toHaveTextContent(
        'Key resource limits are disabled. The saved limits below are not enforced.'
      )
    } else {
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    }
  }
)

it('does not mistake missing defaults from an older server for unlimited', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            token_id: 42,
            name: 'Client',
            max_request_body_bytes: null,
            max_concurrent_requests: null,
          },
        ],
      },
    },
  })
  renderDialog()
  expect(
    await screen.findByLabelText('Request body limit (MB)')
  ).toHaveAttribute(
    'placeholder',
    'Inherit default (current value unavailable)'
  )
  expect(screen.getByLabelText('Concurrent requests')).toHaveAttribute(
    'placeholder',
    'Inherit default (current value unavailable)'
  )
})

it('preserves an existing byte limit when only concurrency changes, then converts decimal MB', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            token_id: 42,
            name: 'Legacy client',
            max_request_body_bytes: 1,
            max_concurrent_requests: null,
          },
        ],
      },
    },
  })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderDialog()
  const body = await screen.findByLabelText('Request body limit (MB)')
  expect(body).toHaveValue('0.00000095367431640625')
  await userEvent.type(screen.getByLabelText('Concurrent requests'), '2')
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/user/7/token-resource-policies/42', {
      max_request_body_bytes: 1,
      max_concurrent_requests: 2,
    })
  )
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  )
  await userEvent.clear(body)
  await userEvent.type(body, '0.5')
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(put).toHaveBeenLastCalledWith(
      '/api/user/7/token-resource-policies/42',
      {
        max_request_body_bytes: 524288,
        max_concurrent_requests: 2,
      }
    )
  )
})

it('saves blank as inheritance and zero as unlimited, then disables the saved form', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            token_id: 42,
            name: 'Image client',
            max_request_body_bytes: 1024,
            max_concurrent_requests: 2,
          },
        ],
      },
    },
  })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  renderDialog()
  const form = await screen.findByRole('form', {
    name: 'Resource limits for key 42',
  })
  await userEvent.clear(within(form).getByLabelText('Request body limit (MB)'))
  const concurrency = within(form).getByLabelText('Concurrent requests')
  await userEvent.clear(concurrency)
  await userEvent.type(concurrency, '0')
  await userEvent.click(within(form).getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(put).toHaveBeenCalledWith('/api/user/7/token-resource-policies/42', {
      max_request_body_bytes: null,
      max_concurrent_requests: 0,
    })
  )
  await waitFor(() =>
    expect(within(form).getByRole('button', { name: 'Save' })).toBeDisabled()
  )
  expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled()
  expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled()
})

it('rejects negative and overflowing values without changing the server policy', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            token_id: 42,
            name: 'Text client',
            max_request_body_bytes: null,
            max_concurrent_requests: null,
          },
        ],
      },
    },
  })
  const put = vi.spyOn(api, 'put')
  renderDialog()
  await screen.findByRole('form')
  await userEvent.type(screen.getByLabelText('Request body limit (MB)'), '-1')
  await userEvent.type(
    screen.getByLabelText('Concurrent requests'),
    '2147483648'
  )
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(screen.getByLabelText('Concurrent requests')).toHaveAttribute(
      'aria-invalid',
      'true'
    )
  )
  expect(put).not.toHaveBeenCalled()
})

it('shows a retry on load failure and an empty state after a successful retry', async () => {
  vi.spyOn(api, 'get')
    .mockResolvedValueOnce({ data: { success: false, message: 'Unavailable' } })
    .mockResolvedValueOnce({
      data: { success: true, data: { total: 0, items: [] } },
    })
  renderDialog()
  await userEvent.click(await screen.findByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('No API keys')).toBeVisible()
})

it('keeps entered limits editable after a failed save', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            token_id: 42,
            name: 'Client',
            max_request_body_bytes: null,
            max_concurrent_requests: null,
          },
        ],
      },
    },
  })
  const put = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: false, message: 'Unavailable' } })
  renderDialog()
  const concurrency = await screen.findByLabelText('Concurrent requests')
  await userEvent.type(concurrency, '3')
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(put).toHaveBeenCalled())
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
  )
  expect(concurrency).toHaveValue('3')
})
