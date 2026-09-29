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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import type { TaskLog } from '../../types'
import { TaskDetailsDialog } from '../dialogs/task-details-dialog'

const task: TaskLog = {
  id: 1,
  user_id: 7,
  platform: 'doubao',
  task_id: 'task-request',
  action: 'text_to_video',
  channel_id: 3,
  group: 'default',
  quota: 10,
  submit_time: 1,
  status: 'SUCCESS',
}
const snapshot = {
  task_id: task.task_id,
  platform: task.platform,
  model: 'video-model',
  body: { prompt: 'stored prompt', image: '[base64 content omitted]' },
  body_bytes: 78,
  base64_omitted: true,
  truncated: false,
  created_at: 1,
}
const clients: QueryClient[] = []
afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
})

function renderDetails(open = true, log = task) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const view = (isOpen: boolean, currentLog: TaskLog) => (
    <QueryClientProvider client={client}>
      <TaskDetailsDialog
        log={currentLog}
        isAdmin={false}
        isRoot={false}
        open={isOpen}
        onOpenChange={() => undefined}
      />
    </QueryClientProvider>
  )
  const result = render(view(open, log))
  return {
    ...result,
    update: (isOpen: boolean, currentLog = log) =>
      result.rerender(view(isOpen, currentLog)),
  }
}

test('request body is collapsed and fetched only after expansion', async () => {
  const user = userEvent.setup()
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: snapshot } })
  renderDetails()
  const trigger = screen.getByRole('button', { name: 'Request body' })
  expect(trigger).toHaveAttribute('aria-expanded', 'false')
  expect(get).not.toHaveBeenCalled()
  await user.click(trigger)
  expect(trigger).toHaveAttribute('aria-expanded', 'true')
  expect(
    await screen.findByRole('textbox', { name: 'Request body' })
  ).toHaveTextContent('stored prompt')
  expect(screen.getByText('Base64 content omitted')).toBeVisible()
  expect(get).toHaveBeenCalledWith(
    '/api/task/task-request/request',
    expect.objectContaining({ signal: expect.any(AbortSignal) })
  )
  await user.click(screen.getByRole('button', { name: 'Copy request body' }))
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Copied' })).toBeVisible()
  )
})

test('historical task without a snapshot shows an empty state', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: false,
      code: 'task_request_snapshot_not_found',
      message: 'task request snapshot not found',
    },
  })
  renderDetails()
  await user.click(screen.getByRole('button', { name: 'Request body' }))
  expect(
    await screen.findByText('No request body was saved for this task')
  ).toBeVisible()
  expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
})

test('failed request can be retried and truncated body is identified', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('temporary failure'))
    .mockResolvedValue({
      data: { success: true, data: { ...snapshot, truncated: true } },
    })
  renderDetails()
  await user.click(screen.getByRole('button', { name: 'Request body' }))
  expect(await screen.findByText('Failed to load request body')).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(
    await screen.findByRole('textbox', { name: 'Request body' })
  ).toBeVisible()
  expect(screen.getByText('Request body truncated')).toBeVisible()
})

test('closing the dialog cancels the read and reopening starts collapsed', async () => {
  const user = userEvent.setup()
  let signal: AbortSignal | undefined
  vi.spyOn(api, 'get').mockImplementation((_url, config) => {
    signal = config?.signal as AbortSignal
    return new Promise(() => undefined)
  })
  const result = renderDetails()
  await user.click(screen.getByRole('button', { name: 'Request body' }))
  expect(await screen.findByRole('status')).toHaveTextContent('Loading...')
  await waitFor(() => expect(signal).toBeDefined())
  result.update(false)
  await waitFor(() => expect(signal?.aborted).toBe(true))
  result.update(true)
  expect(
    await screen.findByRole('button', { name: 'Request body' })
  ).toHaveAttribute('aria-expanded', 'false')
})

test('switching tasks starts collapsed and does not show another task body', async () => {
  const user = userEvent.setup()
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValue({ data: { success: true, data: snapshot } })
  const result = renderDetails()
  await user.click(screen.getByRole('button', { name: 'Request body' }))
  await screen.findByRole('textbox', { name: 'Request body' })
  result.update(true, { ...task, task_id: 'task-other' })
  expect(screen.getByRole('button', { name: 'Request body' })).toHaveAttribute(
    'aria-expanded',
    'false'
  )
  expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
  expect(get).toHaveBeenCalledTimes(1)
})

test.each([
  { success: false, message: 'task not found' },
  { success: true, data: { ...snapshot, task_id: 'task-foreign' } },
])(
  'failed authorization or mismatched response cannot display a request body',
  async (payload) => {
    const user = userEvent.setup()
    vi.spyOn(api, 'get').mockResolvedValue({ data: payload })
    renderDetails()
    await user.click(screen.getByRole('button', { name: 'Request body' }))
    expect(await screen.findByText('Failed to load request body')).toBeVisible()
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Copy request body' })
    ).not.toBeInTheDocument()
  }
)
