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
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AxiosAdapter } from 'axios'
import { useState } from 'react'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { PrivateAssetOSSSettingsSection } from '../private-asset-oss-settings-section'

const originalAdapter = api.defaults.adapter
const clients: QueryClient[] = []
afterEach(() => {
  api.defaults.adapter = originalAdapter
  for (const client of clients) client.clear()
  clients.length = 0
})

// 使用真正的设置动作 Portal 和路由离开保护，确保提交从页面入口经过字段校验。
function OSSSettingsFixture() {
  const [actionsContainer, setActionsContainer] =
    useState<HTMLDivElement | null>(null)
  return (
    <SettingsPageProvider actionsContainer={actionsContainer}>
      <div ref={setActionsContainer} />
      <PrivateAssetOSSSettingsSection
        secretConfigured
        defaultValues={{
          region: 'cn-hangzhou',
          endpoint: '',
          bucket: 'private-assets',
          prefix: 'private-assets/',
          accessKeyId: 'test-id',
          accessKeySecret: '',
        }}
      />
    </SettingsPageProvider>
  )
}

async function renderSettings() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const root = createRootRoute({ component: OSSSettingsFixture })
  const router = createRouter({
    routeTree: root,
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  return render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

test('saves OSS settings atomically and omits an unchanged configured secret', async () => {
  const adapter = vi.fn<AxiosAdapter>(async (config) => ({
    data: { success: true },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  }))
  api.defaults.adapter = adapter
  await renderSettings()
  const prefix = await screen.findByRole('textbox', { name: 'Object Prefix' })
  await userEvent.clear(prefix)
  await userEvent.type(prefix, 'next-prefix')
  await userEvent.click(
    screen.getByRole('button', { name: 'Save OSS settings' })
  )
  await waitFor(() => expect(adapter).toHaveBeenCalledTimes(1))
  expect(adapter.mock.calls[0][0].url).toBe('/api/option/private-asset-oss')
  expect(JSON.parse(adapter.mock.calls[0][0].data)).toEqual({
    region: 'cn-hangzhou',
    endpoint: '',
    bucket: 'private-assets',
    prefix: 'next-prefix/',
    access_key_id: 'test-id',
  })
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Save OSS settings' })
    ).toBeDisabled()
  )
})

test('business failure retains form input for correction without logging credential requests', async () => {
  const log = vi.spyOn(console, 'log')
  const adapter = vi.fn<AxiosAdapter>(async (config) => ({
    data: { success: false, message: 'OSS configuration rejected' },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  }))
  api.defaults.adapter = adapter
  await renderSettings()
  const secret = await screen.findByLabelText('AccessKey Secret', {
    selector: 'input',
  })
  await userEvent.type(secret, 'test-replacement')
  await userEvent.click(
    screen.getByRole('button', { name: 'Save OSS settings' })
  )
  await waitFor(() => expect(adapter).toHaveBeenCalledTimes(1))
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Save OSS settings' })
    ).toBeEnabled()
  )
  expect(secret).toHaveValue('test-replacement')
  expect(log).not.toHaveBeenCalled()
})
