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
import type { Row } from '@tanstack/react-table'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { channelSchema, type Channel } from '../../../types'
import { ChannelsProvider } from '../../channels-provider'
import { DataTableRowActions } from '../../data-table-row-actions'
import { ChannelResourcePolicyDialog } from '../channel-resource-policy-dialog'

beforeEach(() => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'operator', role: 100 })
})
afterEach(() => {
  useAuthStore.getState().auth.setUser(null)
  vi.restoreAllMocks()
})

const policy = (bytes = 31457280, id = 7) => ({
  data: {
    success: true,
    data: { channel_id: id, max_request_body_bytes: bytes },
  },
})

function renderDialog() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const close = vi.fn()
  const element = (id = 7, open = true) => (
    <QueryClientProvider client={client}>
      <ChannelResourcePolicyDialog
        channel={{ id, name: 'Image provider' }}
        open={open}
        onOpenChange={close}
      />
    </QueryClientProvider>
  )
  const view = render(element())
  return { ...view, close, element }
}

it.each([
  ['1.5', 1572864],
  ['0', 0],
])(
  'saves %s MB as %s bytes and closes only after success',
  async (value, bytes) => {
    vi.spyOn(api, 'get').mockResolvedValue(policy())
    const put = vi.spyOn(api, 'put').mockResolvedValue(policy(bytes as number))
    const view = renderDialog()
    const input = await screen.findByLabelText('Request body limit (MB)')
    expect(input).toHaveValue('30')
    await userEvent.clear(input)
    await userEvent.type(input, String(value))
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith(
        '/api/channel/7/resource-policy',
        { max_request_body_bytes: bytes },
        expect.any(Object)
      )
    )
    await waitFor(() => expect(view.close).toHaveBeenCalledWith(false))
  }
)

it.each(['', '-1', 'NaN', '0.0000001', '999999999999999999'])(
  'rejects invalid size %s without sending a write',
  async (value) => {
    vi.spyOn(api, 'get').mockResolvedValue(policy())
    const put = vi.spyOn(api, 'put')
    renderDialog()
    const input = await screen.findByLabelText('Request body limit (MB)')
    await userEvent.clear(input)
    if (value) await userEvent.type(input, value)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(input).toHaveAttribute('aria-invalid', 'true'))
    expect(put).not.toHaveBeenCalled()
  }
)

it('shows a retryable read error instead of an unlimited editable default', async () => {
  vi.spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('unavailable'))
    .mockResolvedValue(policy(0))
  renderDialog()
  expect(
    await screen.findByText('Unable to load channel request body limit.')
  ).toBeVisible()
  expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByLabelText('Request body limit (MB)')).toHaveValue(
    '0'
  )
})

it('keeps the edited value after a failed save and allows retry', async () => {
  vi.spyOn(api, 'get').mockResolvedValue(policy())
  const put = vi
    .spyOn(api, 'put')
    .mockRejectedValueOnce(new Error('unavailable'))
    .mockResolvedValue(policy(1048576))
  const view = renderDialog()
  const input = await screen.findByLabelText('Request body limit (MB)')
  await userEvent.clear(input)
  await userEvent.type(input, '1')
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
  )
  expect(input).toHaveValue('1')
  expect(view.close).not.toHaveBeenCalled()
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() => expect(view.close).toHaveBeenCalledWith(false))
  expect(put).toHaveBeenCalledTimes(2)
})

it('disables editing and closing while a save is pending', async () => {
  vi.spyOn(api, 'get').mockResolvedValue(policy())
  let resolveSave!: (value: ReturnType<typeof policy>) => void
  vi.spyOn(api, 'put').mockImplementation(
    () =>
      new Promise((resolve) => {
        resolveSave = resolve
      })
  )
  const view = renderDialog()
  const input = await screen.findByLabelText('Request body limit (MB)')
  await userEvent.clear(input)
  await userEvent.type(input, '0')
  await userEvent.click(screen.getByRole('button', { name: 'Save' }))
  expect(
    await screen.findByRole('button', { name: 'Saving...' })
  ).toBeDisabled()
  expect(input).toBeDisabled()
  expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
  await userEvent.keyboard('{Escape}')
  expect(view.close).not.toHaveBeenCalled()
  resolveSave(policy(0))
  await waitFor(() => expect(view.close).toHaveBeenCalledWith(false))
})

it('fetches the selected channel again on reopening and discards the previous draft', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValueOnce(policy())
    .mockResolvedValueOnce(policy(2097152, 8))
    .mockResolvedValueOnce(policy(1048576, 8))
  const view = renderDialog()
  const input = await screen.findByLabelText('Request body limit (MB)')
  await userEvent.clear(input)
  await userEvent.type(input, '99')
  view.rerender(view.element(8))
  await waitFor(() =>
    expect(screen.getByLabelText('Request body limit (MB)')).toHaveValue('2')
  )
  view.rerender(view.element(8, false))
  view.rerender(view.element(8, true))
  await waitFor(() =>
    expect(screen.getByLabelText('Request body limit (MB)')).toHaveValue('1')
  )
  expect(get).toHaveBeenLastCalledWith(
    '/api/channel/8/resource-policy',
    expect.any(Object)
  )
})

it.each([
  [1, true, false],
  [10, false, false],
  [10, true, true],
  [100, false, true],
])(
  'exposes the action only to permitted administrators (role=%s, write=%s)',
  async (role, write, visible) => {
    useAuthStore
      .getState()
      .auth.setUser({
        id: 1,
        username: 'operator',
        role: Number(role),
        permissions: {
          admin_permissions: { channel: { read: true, write: Boolean(write) } },
        },
      })
    const get = vi.spyOn(api, 'get').mockResolvedValue(policy())
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const channel = channelSchema.parse({
      id: 7,
      name: 'Image provider',
      type: 1,
      key: '',
      status: 1,
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
    })
    render(
      <QueryClientProvider client={client}>
        <ChannelsProvider>
          <DataTableRowActions row={{ original: channel } as Row<Channel>} />
        </ChannelsProvider>
      </QueryClientProvider>
    )
    await userEvent.click(screen.getByRole('button', { name: 'Open menu' }))
    const action = screen.queryByRole('menuitem', {
      name: 'Channel request body limit',
    })
    if (visible) expect(action).toBeVisible()
    else expect(action).not.toBeInTheDocument()
    expect(get).not.toHaveBeenCalled()
  }
)

it('does not fetch policies when a common user tries to open the dialog', () => {
  useAuthStore.getState().auth.setUser({ id: 2, username: 'customer', role: 1 })
  const get = vi.spyOn(api, 'get')
  renderDialog()
  expect(get).not.toHaveBeenCalled()
  expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
})
