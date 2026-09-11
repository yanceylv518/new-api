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
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AxiosAdapter, AxiosResponse } from 'axios'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { listSeedanceAssets } from '../api'
import { AssetPreview } from '../components/asset-preview'
import { AssetUploadPanel } from '../components/asset-upload-panel'
import { SeedanceAssets } from '../index'

const originalAdapter = api.defaults.adapter
const clients: QueryClient[] = []
afterEach(() => {
  api.defaults.adapter = originalAdapter
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.getState().auth.reset()
  localStorage.clear()
})

// 使用真实页面和 HTTP 客户端，只替换网络边界以验证两层缓存与界面联动。
function renderLibrary() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  useAuthStore
    .getState()
    .auth.setUser({ id: 7, username: 'asset-owner', role: 1 })
  return render(
    <QueryClientProvider client={client}>
      <SeedanceAssets />
    </QueryClientProvider>
  )
}

const firstAsset = {
  id: 1,
  asset_id: 'asset-1',
  group_id: 'group-1',
  name: 'cover.png',
  asset_type: 'Image',
  status: 'Active',
  preview_url: 'https://media.example/cover.png?signature=old',
}
const secondAsset = {
  ...firstAsset,
  id: 2,
  asset_id: 'asset-2',
  name: 'second.png',
  preview_url: 'https://media.example/second.png?signature=old',
}

// 分组增删改通过实际菜单和确认框完成，失败响应不会伪装成删除成功。
test('creates, renames, and deletes a group through the library controls', async () => {
  let groups: Array<{ id: number; group_id: string; name: string }> = []
  api.defaults.adapter = async (config) => {
    if (config.url?.includes('/asset-groups')) {
      if (config.method === 'post') {
        groups = [
          { id: 1, group_id: 'group-1', name: JSON.parse(config.data).name },
        ]
      }
      if (config.method === 'put') {
        groups = [{ ...groups[0], name: JSON.parse(config.data).name }]
      }
      if (config.method === 'delete') groups = []
      return {
        data: {
          success: true,
          data: config.method === 'get' ? groups : groups[0],
        },
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      }
    }
    return {
      data: {
        success: true,
        data: [],
        total: 0,
        page: 1,
        page_size: 24,
        has_pending: false,
      },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    }
  }
  renderLibrary()
  await screen.findByText('No asset groups yet')
  expect(screen.getByRole('button', { name: 'Upload assets' })).toBeDisabled()
  await userEvent.type(
    screen.getByRole('textbox', { name: 'New group name' }),
    'References{Enter}'
  )
  const group = await screen.findByRole('button', { name: 'References' })
  expect(group).toHaveAttribute('aria-current', 'page')
  await userEvent.click(screen.getByRole('button', { name: 'Group actions' }))
  await userEvent.click(screen.getByRole('menuitem', { name: 'Rename' }))
  const name = screen.getByRole('textbox', { name: 'Group name' })
  await userEvent.clear(name)
  await userEvent.type(name, 'Renamed{Enter}')
  await screen.findByRole('button', { name: 'Renamed' })
  await userEvent.click(screen.getByRole('button', { name: 'Group actions' }))
  await userEvent.click(screen.getByRole('menuitem', { name: 'Delete' }))
  const confirmation = await screen.findByRole('alertdialog')
  expect(confirmation).toHaveTextContent('Renamed')
  await userEvent.click(
    within(confirmation).getByRole('button', { name: 'Delete' })
  )
  await screen.findByText('No asset groups yet')
  expect(screen.getByRole('button', { name: 'Upload assets' })).toBeDisabled()
})

test('refreshes only the selected asset and keeps sibling preview URLs stable', async () => {
  const requests: string[] = []
  api.defaults.adapter = async (config) => {
    requests.push(config.url ?? '')
    let data: unknown
    if (config.url?.endsWith('asset-groups')) {
      data = {
        success: true,
        data: [{ id: 1, group_id: 'group-1', name: 'Video references' }],
      }
    } else if (config.url?.endsWith('/1/refresh')) {
      data = {
        success: true,
        data: {
          ...firstAsset,
          preview_url: 'https://media.example/cover.png?signature=new',
        },
      }
    } else {
      data = {
        success: true,
        data: [firstAsset, secondAsset],
        total: 2,
        page: 1,
        page_size: 24,
        has_pending: false,
      }
    }
    return { data, status: 200, statusText: 'OK', headers: {}, config }
  }
  renderLibrary()
  const image = await screen.findByRole('img', { name: 'cover.png' })
  fireEvent.load(image)
  const card = image.closest('[data-slot="card"]')
  expect(card).not.toBeNull()
  if (!card) throw new Error('Missing asset card')
  await userEvent.click(
    within(card as HTMLElement).getByRole('button', { name: 'Refresh' })
  )
  await waitFor(() =>
    expect(requests).toContain('/api/user/seedance/assets/1/refresh')
  )
  expect(image).toHaveAttribute('src', firstAsset.preview_url)
  expect(screen.getByRole('img', { name: 'second.png' })).toHaveAttribute(
    'src',
    secondAsset.preview_url
  )
  expect(requests.filter((url) => url.endsWith('/assets'))).toHaveLength(1)
  expect(requests.some((url) => url.endsWith('/2/refresh'))).toBe(false)
})

test('selects the visible assets and deletes them with one batch request', async () => {
  const batchRequests: number[][] = []
  api.defaults.adapter = async (config) => {
    let data: unknown
    if (config.url?.endsWith('asset-groups')) {
      data = {
        success: true,
        data: [{ id: 1, group_id: 'group-1', name: 'References' }],
      }
    } else if (config.url?.endsWith('/batch-delete')) {
      batchRequests.push(JSON.parse(String(config.data)).ids)
      data = {
        success: true,
        data: { deleted_ids: [], failed_ids: [], pending_ids: [1, 2] },
      }
    } else {
      data = {
        success: true,
        data: [firstAsset, secondAsset],
        total: 2,
        page: 1,
        page_size: 24,
        has_pending: false,
      }
    }
    return { data, status: 200, statusText: 'OK', headers: {}, config }
  }

  renderLibrary()
  await screen.findByRole('img', { name: 'cover.png' })
  await userEvent.click(screen.getByRole('checkbox', { name: 'Select all' }))
  expect(screen.getByText('2 selected')).toBeInTheDocument()
  await userEvent.click(
    screen.getByRole('button', { name: 'Delete selected assets' })
  )
  const confirmation = await screen.findByRole('alertdialog')
  expect(confirmation).toHaveTextContent('Delete 2 selected assets')
  await userEvent.click(
    within(confirmation).getByRole('button', { name: 'Delete' })
  )

  await waitFor(() => expect(batchRequests).toEqual([[1, 2]]))
  await waitFor(() =>
    expect(screen.queryByText('2 selected')).not.toBeInTheDocument()
  )
})

test('keeps failed assets selected after a partial batch deletion', async () => {
  api.defaults.adapter = async (config) => {
    let data: unknown
    if (config.url?.endsWith('asset-groups')) {
      data = {
        success: true,
        data: [{ id: 1, group_id: 'group-1', name: 'References' }],
      }
    } else if (config.url?.endsWith('/batch-delete')) {
      data = {
        success: false,
        message: '1 asset failed to delete',
        data: { deleted_ids: [1], failed_ids: [2] },
      }
    } else {
      data = {
        success: true,
        data: [firstAsset, secondAsset],
        total: 2,
        page: 1,
        page_size: 24,
        has_pending: false,
      }
    }
    return { data, status: 200, statusText: 'OK', headers: {}, config }
  }

  renderLibrary()
  await screen.findByRole('img', { name: 'cover.png' })
  await userEvent.click(
    screen.getByRole('checkbox', { name: 'Select cover.png' })
  )
  await userEvent.click(
    screen.getByRole('checkbox', { name: 'Select second.png' })
  )
  await userEvent.click(
    screen.getByRole('button', { name: 'Delete selected assets' })
  )
  const confirmation = await screen.findByRole('alertdialog')
  await userEvent.click(
    within(confirmation).getByRole('button', { name: 'Delete' })
  )

  await waitFor(() =>
    expect(
      screen.getByRole('checkbox', { name: 'Select second.png' })
    ).toHaveAttribute('aria-checked', 'true')
  )
  expect(
    screen.getByRole('checkbox', { name: 'Select cover.png' })
  ).toHaveAttribute('aria-checked', 'false')
  expect(screen.getByText('1 selected')).toBeInTheDocument()
})

test('reloads the list after uploading and supports switching view with keyboard', async () => {
  let uploaded = false
  const listRequests: string[] = []
  api.defaults.adapter = async (config) => {
    let data: unknown
    if (config.url?.endsWith('asset-groups')) {
      data = {
        success: true,
        data: [{ id: 1, group_id: 'group-1', name: 'References' }],
      }
    } else if (config.url?.endsWith('/upload')) {
      uploaded = true
      expect(config.data).toBeInstanceOf(FormData)
      data = { success: true, data: firstAsset }
    } else {
      listRequests.push(String(config.headers.get('Cache-Control')))
      data = {
        success: true,
        data: uploaded ? [firstAsset] : [],
        total: uploaded ? 1 : 0,
        page: 1,
        page_size: 24,
        has_pending: false,
      }
    }
    return { data, status: 200, statusText: 'OK', headers: {}, config }
  }
  renderLibrary()
  await screen.findByText('Drop your first asset')
  const input = screen.getByLabelText('Upload assets', { selector: 'input' })
  await userEvent.upload(
    input,
    new File(['image'], 'cover.png', { type: 'image/png' })
  )
  await screen.findByRole('img', { name: 'cover.png' })
  expect(listRequests).toEqual(['no-cache, no-store', 'no-cache, no-store'])
  const tableButton = screen.getByRole('button', { name: 'Table view' })
  tableButton.focus()
  await userEvent.keyboard('{Enter}')
  expect(tableButton).toHaveAttribute('aria-pressed', 'true')
  expect(screen.getByRole('article')).toHaveTextContent('cover.png')
})

test('opens the floating upload queue and shows each transfer status', async () => {
  let resolveUpload: ((response: AxiosResponse) => void) | undefined
  let activeResponse: AxiosResponse | undefined
  const adapter = vi.fn<AxiosAdapter>(
    (config) =>
      new Promise((resolve) => {
        resolveUpload = resolve
        activeResponse = {
          data: { success: true, data: firstAsset },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      })
  )
  api.defaults.adapter = adapter
  const client = new QueryClient()
  clients.push(client)
  const view = render(
    <QueryClientProvider client={client}>
      <AssetUploadPanel groupId='group-1' onUploaded={() => undefined}>
        {() => null}
      </AssetUploadPanel>
    </QueryClientProvider>
  )

  await userEvent.upload(
    screen.getByLabelText('Upload assets', { selector: 'input' }),
    new File(['image'], 'cover.png', { type: 'image/png' })
  )
  const queueButton = screen.getByRole('button', { name: 'Upload queue' })
  await userEvent.click(queueButton)
  const queue = screen.getByRole('region', { name: 'Upload queue' })
  await waitFor(() => expect(queue).toHaveTextContent('Uploading'))

  await act(async () => {
    if (resolveUpload && activeResponse) resolveUpload(activeResponse)
  })
  await waitFor(() => expect(queue).toHaveTextContent('Uploaded'))
  expect(queueButton).toHaveAttribute('aria-expanded', 'true')
  await userEvent.click(screen.getByRole('button', { name: 'Close' }))
  expect(queueButton).toHaveAttribute('aria-expanded', 'false')
  view.unmount()
})

test('shows failed transfers and retries them from the floating queue', async () => {
  let attempts = 0
  const adapter = vi.fn<AxiosAdapter>(async (config) => {
    attempts++
    if (attempts === 1) throw new Error('temporary upload failure')
    return {
      data: { success: true, data: firstAsset },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    }
  })
  api.defaults.adapter = adapter
  const client = new QueryClient()
  clients.push(client)
  const view = render(
    <QueryClientProvider client={client}>
      <AssetUploadPanel groupId='group-1' onUploaded={() => undefined}>
        {() => null}
      </AssetUploadPanel>
    </QueryClientProvider>
  )

  await userEvent.upload(
    screen.getByLabelText('Upload assets', { selector: 'input' }),
    new File(['image'], 'cover.png', { type: 'image/png' })
  )
  await userEvent.click(screen.getByRole('button', { name: 'Upload queue' }))
  const queue = screen.getByRole('region', { name: 'Upload queue' })
  await waitFor(() => expect(queue).toHaveTextContent('Upload failed'))
  await userEvent.click(
    screen.getByRole('button', { name: 'Retry failed uploads' })
  )
  await waitFor(() => expect(queue).toHaveTextContent('Uploaded'))
  expect(attempts).toBe(2)
  view.unmount()
})

test('keeps an active upload visible when switching asset groups', async () => {
  let resolveUpload: ((response: AxiosResponse) => void) | undefined
  let activeResponse: AxiosResponse | undefined
  const adapter = vi.fn<AxiosAdapter>(
    (config) =>
      new Promise((resolve) => {
        resolveUpload = resolve
        activeResponse = {
          data: { success: true, data: firstAsset },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      })
  )
  api.defaults.adapter = adapter
  const client = new QueryClient()
  clients.push(client)
  const renderPanel = (groupId: string, groupName: string) => (
    <QueryClientProvider client={client}>
      <AssetUploadPanel
        groupId={groupId}
        groupName={groupName}
        onUploaded={() => undefined}
      >
        {() => null}
      </AssetUploadPanel>
    </QueryClientProvider>
  )
  const view = render(renderPanel('group-1', 'References'))

  await userEvent.upload(
    screen.getByLabelText('Upload assets', { selector: 'input' }),
    new File(['image'], 'cover.png', { type: 'image/png' })
  )
  await userEvent.click(screen.getByRole('button', { name: 'Upload queue' }))
  const queue = screen.getByRole('region', { name: 'Upload queue' })
  await waitFor(() => expect(queue).toHaveTextContent('Uploading'))
  expect(queue).toHaveTextContent('0%')

  view.rerender(renderPanel('group-2', 'Generated'))
  expect(screen.getByRole('region', { name: 'Upload queue' })).toHaveTextContent(
    'References'
  )

  await act(async () => {
    if (resolveUpload && activeResponse) resolveUpload(activeResponse)
  })
  await waitFor(() => expect(queue).toHaveTextContent('Uploaded'))
  view.unmount()
})

test('renews only a failed preview and bounds automatic recovery', () => {
  const refresh = vi.fn()
  const view = render(
    <AssetPreview asset={firstAsset} onUnavailable={refresh} />
  )
  fireEvent.error(screen.getByRole('img'))
  expect(refresh).toHaveBeenCalledTimes(1)
  expect(screen.queryByRole('img')).toBeNull()
  view.rerender(
    <AssetPreview
      asset={{
        ...firstAsset,
        preview_url: 'https://media.example/renewed.png',
      }}
      onUnavailable={refresh}
    />
  )
  fireEvent.error(screen.getByRole('img'))
  expect(refresh).toHaveBeenCalledTimes(1)
})

test('cancels an in-flight upload on unmount and never starts the remaining queue', async () => {
  let resolveUpload: ((response: AxiosResponse) => void) | undefined
  let activeResponse: AxiosResponse | undefined
  const adapter = vi.fn<AxiosAdapter>(
    (config) =>
      new Promise((resolve) => {
        resolveUpload = resolve
        activeResponse = {
          data: { success: true, data: firstAsset },
          status: 200,
          statusText: 'OK',
          headers: {},
          config,
        }
      })
  )
  api.defaults.adapter = adapter
  const client = new QueryClient()
  clients.push(client)
  const uploaded = vi.fn()
  const view = render(
    <QueryClientProvider client={client}>
      <AssetUploadPanel groupId='group-1' onUploaded={uploaded}>
        {() => null}
      </AssetUploadPanel>
    </QueryClientProvider>
  )
  await userEvent.upload(
    screen.getByLabelText('Upload assets', { selector: 'input' }),
    [
      new File(['a'], 'a.png', { type: 'image/png' }),
      new File(['b'], 'b.png', { type: 'image/png' }),
    ]
  )
  await waitFor(() => expect(adapter).toHaveBeenCalledTimes(1))
  view.unmount()
  expect(activeResponse?.config.signal?.aborted).toBe(true)
  await act(async () => {
    if (resolveUpload && activeResponse) resolveUpload(activeResponse)
  })
  expect(adapter).toHaveBeenCalledTimes(1)
  expect(uploaded).not.toHaveBeenCalled()
})

test('concurrent list requests have independent cancellation and reject business failures', async () => {
  const adapter = vi.fn<AxiosAdapter>(async (config) => ({
    data: { success: false, message: 'asset access denied' },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  }))
  api.defaults.adapter = adapter
  const requests = await Promise.allSettled([
    listSeedanceAssets('group-1'),
    listSeedanceAssets('group-1'),
  ])
  expect(adapter).toHaveBeenCalledTimes(2)
  for (const result of requests) {
    expect(result.status).toBe('rejected')
    if (result.status === 'rejected') {
      expect(result.reason.message).toBe('asset access denied')
    }
  }
})
