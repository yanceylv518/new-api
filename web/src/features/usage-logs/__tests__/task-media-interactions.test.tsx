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
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { AxiosAdapter } from 'axios'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { DetailsDialog } from '../components/dialogs/details-dialog'
import { TaskArtifactsCell } from '../components/task-artifacts'
import { usageLogSchema } from '../data/schema'
import type { TaskLog } from '../types'

const originalAdapter = api.defaults.adapter
const clients: QueryClient[] = []
afterEach(() => {
  api.defaults.adapter = originalAdapter
  for (const client of clients) client.clear()
  clients.length = 0
})

const task: TaskLog = {
  id: 1,
  user_id: 7,
  platform: 'seedance',
  task_id: 'task-video',
  action: 'GENERATE',
  channel_id: 3,
  group: 'default',
  quota: 10,
  submit_time: 1,
  status: 'SUCCESS',
  legacy_video_available: true,
}
const videoUrl = `https://media.example/v1/tasks/task-video/artifacts/video/content?access=${'A'.repeat(43)}`

// 真实渲染任务入口，网络适配器只返回服务端授权投影；剪贴板使用浏览器边界 mock。
function renderTask(log = task) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  return render(
    <QueryClientProvider client={client}>
      <TaskArtifactsCell log={log} />
    </QueryClientProvider>
  )
}

test('copies a single video beside the list preview using the authorized projection only on click', async () => {
  const user = userEvent.setup()
  const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
  const adapter = vi.fn<AxiosAdapter>(async (config) => ({
    data: {
      success: true,
      data: {
        artifacts: [{ key: 'video', type: 'video', content_url: videoUrl }],
      },
    },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  }))
  api.defaults.adapter = adapter
  renderTask()
  expect(screen.getByRole('button', { name: 'Preview video' })).toBeVisible()
  expect(adapter).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Copy link' }))
  await waitFor(() => expect(write).toHaveBeenCalledWith(videoUrl))
  expect(adapter.mock.calls.map(([config]) => config.url)).toEqual([
    '/api/task/task-video/artifacts',
  ])
  expect(screen.queryByRole('dialog')).toBeNull()
})

test('multiple videos open the artifact chooser without copying an arbitrary URL', async () => {
  const user = userEvent.setup()
  const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
  api.defaults.adapter = async (config) => ({
    data: {
      success: true,
      data: {
        artifacts: [
          { key: 'video', type: 'video', content_url: videoUrl },
          {
            key: 'alternate',
            type: 'video',
            content_url: videoUrl.replace(
              '/video/content',
              '/alternate/content'
            ),
          },
        ],
      },
    },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  })
  renderTask({
    ...task,
    admin_info: { task_plugin: { key: 'seedance', name: 'Seedance' } },
  })
  await user.click(screen.getByRole('button', { name: 'Copy link' }))
  await screen.findByRole('dialog', { name: 'Artifacts' })
  expect(write).not.toHaveBeenCalled()
  expect(screen.getAllByRole('button', { name: 'Download' })).toHaveLength(2)
})

test('legacy projection previews and copies its authorized URL and offers retry after media failure', async () => {
  const user = userEvent.setup()
  const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
  api.defaults.adapter = async (config) => ({
    data: {
      success: true,
      data: { artifacts: [], legacy_content_url: videoUrl },
    },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  })
  renderTask()
  await user.click(screen.getByRole('button', { name: 'Copy link' }))
  await waitFor(() => expect(write).toHaveBeenCalledWith(videoUrl))
  await user.click(screen.getByRole('button', { name: 'Preview video' }))
  const dialog = await screen.findByRole('dialog')
  await waitFor(() =>
    expect(dialog.querySelector('video')).toHaveAttribute('src', videoUrl)
  )
  const video = dialog.querySelector('video')
  if (!video) throw new Error('Missing video preview')
  fireEvent.error(video)
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(dialog.querySelector('video')).toHaveAttribute('src', videoUrl)
})

test('a failed artifact request copies nothing and can be retried from the list', async () => {
  const user = userEvent.setup()
  const write = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
  let attempts = 0
  api.defaults.adapter = async (config) => ({
    data:
      ++attempts === 1
        ? { success: false, message: 'projection unavailable' }
        : {
            success: true,
            data: { artifacts: [], legacy_content_url: videoUrl },
          },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  })
  renderTask()
  const copy = screen.getByRole('button', { name: 'Copy link' })
  await user.click(copy)
  await waitFor(() => expect(copy).toBeEnabled())
  expect(write).not.toHaveBeenCalled()
  await user.click(copy)
  await waitFor(() => expect(write).toHaveBeenCalledWith(videoUrl))
})

test('refund details read public settlement fields while preserving ordinary failure refunds', async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  api.defaults.adapter = async (config) => ({
    data: { success: true, data: {} },
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
  })
  const log = usageLogSchema.parse({
    id: 1,
    user_id: 7,
    created_at: 1,
    type: 6,
    quota: 141100,
    content: 'task settled',
    other: JSON.stringify({
      task_id: 'task-video',
      pre_consumed_quota: 250000,
      actual_quota: 108900,
    }),
  })
  const view = render(
    <QueryClientProvider client={client}>
      <DetailsDialog
        log={log}
        isAdmin={false}
        isRoot={false}
        open
        onOpenChange={() => {}}
      />
    </QueryClientProvider>
  )
  expect(await screen.findByText('Task settlement refund')).toBeVisible()
  expect(screen.getByText('Pre-consumed')).toBeVisible()
  expect(screen.getByText('Actual cost')).toBeVisible()
  expect(screen.getByText('Refund amount')).toBeVisible()
  view.rerender(
    <QueryClientProvider client={client}>
      <DetailsDialog
        log={{
          ...log,
          other: JSON.stringify({
            task_id: 'task-video',
            reason: 'upstream failed',
          }),
        }}
        isAdmin={false}
        isRoot={false}
        open
        onOpenChange={() => {}}
      />
    </QueryClientProvider>
  )
  expect(screen.getByText('Refund Details')).toBeVisible()
  expect(screen.queryByText('Actual cost')).toBeNull()
  expect(screen.getByText('upstream failed')).toBeVisible()
})
