import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import {
  act,
  fireEvent,
  render,
  renderHook,
  screen,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import type { ReactNode } from 'react'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import type { TaskLog } from '../../types'
import { RetryChainIndicator } from '../columns/column-helpers'
import { useTaskLogsColumns } from '../columns/task-logs-columns'
import { TaskDetailsDialog } from '../dialogs/task-details-dialog'
import { UsageLogsMobileList } from '../usage-logs-mobile-card'
import { UsageLogsProvider } from '../usage-logs-provider'

const i18n = createInstance()
let client: QueryClient
beforeEach(async () => {
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  await i18n.init({
    lng: 'en',
    resources: { en, zh },
    interpolation: { escapeValue: false },
  })
})
afterEach(() => client.clear())

function TestProviders(props: { children: ReactNode }) {
  return (
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <UsageLogsProvider>{props.children}</UsageLogsProvider>
      </QueryClientProvider>
    </I18nextProvider>
  )
}

function TaskChannel(props: {
  channels?: string[]
  isAdmin: boolean
  mobile?: boolean
}) {
  const task: TaskLog = {
    id: 1,
    user_id: 1,
    task_id: 'task_retry',
    platform: 'vidu',
    action: 'text_to_video',
    channel_id: 12,
    group: 'default',
    quota: 100,
    submit_time: 1,
    status: props.mobile ? 'IN_PROGRESS' : 'SUCCESS',
    admin_info: { use_channel: props.channels },
  }
  const table = useReactTable({
    data: [task],
    columns: useTaskLogsColumns(props.isAdmin, false),
    getCoreRowModel: getCoreRowModel(),
  })
  if (props.mobile) {
    return (
      <UsageLogsMobileList
        table={table}
        logCategory='task'
        isAdmin={props.isAdmin}
      />
    )
  }
  const cell = table
    .getRowModel()
    .rows[0].getAllCells()
    .find((item) => item.column.id === 'channel_id')
  return cell ? flexRender(cell.column.columnDef.cell, cell.getContext()) : null
}

function columnKey(column: { id?: string; accessorKey?: string }): string {
  return column.id ?? String(column.accessorKey)
}

describe('task log columns', () => {
  test('keeps one definition for each task log field in the user view', () => {
    const { result } = renderHook(() => useTaskLogsColumns(false, false))
    const keys = result.current.map(columnKey)

    expect(keys).toEqual([
      'submit_time',
      'task_id',
      'duration',
      'status',
      'progress',
      'artifacts',
      'fail_reason',
    ])
  })

  test('keeps one definition for each task log field in the admin view', () => {
    const { result } = renderHook(() => useTaskLogsColumns(true, true))
    const keys = result.current.map(columnKey)

    expect(new Set(keys).size).toBe(keys.length)
    expect(keys).toContain('task_id')
    expect(keys).toContain('artifacts')
    expect(keys).toContain('fail_reason')
  })

  test('shows the persisted retry chain to an administrator', async () => {
    render(
      <I18nextProvider i18n={i18n}>
        <TaskChannel channels={['8', '12']} isAdmin />
      </I18nextProvider>
    )
    fireEvent.click(screen.getByRole('button', { name: 'Retry Chain' }))
    expect(await screen.findByText('8 → 12')).toBeVisible()
  })

  test.each([{ channels: undefined }, { channels: [] }, { channels: ['12'] }])(
    'does not invent retries for a single attempt or historical task: %j',
    ({ channels }) => {
      render(
        <I18nextProvider i18n={i18n}>
          <TaskChannel channels={channels} isAdmin />
        </I18nextProvider>
      )
      expect(screen.getByText('#12')).toBeVisible()
      expect(screen.queryByRole('button', { name: 'Retry Chain' })).toBeNull()
    }
  )

  test('keeps retry channels out of the ordinary user view', () => {
    render(
      <I18nextProvider i18n={i18n}>
        <TaskChannel channels={['8', '12']} isAdmin={false} />
      </I18nextProvider>
    )
    expect(screen.queryByRole('button', { name: 'Retry Chain' })).toBeNull()
    expect(screen.queryByText('#12')).toBeNull()
  })

  test('preserves repeated-channel attempts and keeps the popover open during refresh', async () => {
    const view = (channels: string[]) => (
      <TaskChannel channels={channels} isAdmin />
    )
    const result = render(view(['12', '12']), { wrapper: TestProviders })
    fireEvent.click(screen.getByRole('button', { name: 'Retry Chain' }))
    expect(await screen.findByText('12 → 12')).toBeVisible()
    result.rerender(view(['12', '12']))
    expect(screen.getByText('12 → 12')).toBeVisible()
  })

  test('supports legacy numeric channel IDs and keyboard access without opening the row', async () => {
    const rowClick = vi.fn()
    render(
      <div onClick={rowClick}>
        <RetryChainIndicator channels={[8, 12]} />
      </div>,
      { wrapper: TestProviders }
    )
    const trigger = screen.getByRole('button', { name: 'Retry Chain' })
    trigger.focus()
    await userEvent.setup().keyboard('{Enter}')
    expect(await screen.findByText('8 → 12')).toBeVisible()
    expect(rowClick).not.toHaveBeenCalled()
  })

  test.each([true, false])(
    'mobile retry-chain visibility follows administrator access: %s',
    async (isAdmin) => {
      const get = vi.spyOn(api, 'get')
      render(<TaskChannel channels={['8', '12']} isAdmin={isAdmin} mobile />, {
        wrapper: TestProviders,
      })
      if (isAdmin) {
        fireEvent.click(screen.getByRole('button', { name: 'Retry Chain' }))
        expect(await screen.findByText('8 → 12')).toBeVisible()
      } else {
        expect(screen.queryByRole('button', { name: 'Retry Chain' })).toBeNull()
        expect(screen.queryByText('8 → 12')).toBeNull()
      }
      expect(get).not.toHaveBeenCalled()
    }
  )

  test.each([true, false])(
    'task details expose the retry chain only to administrators: %s',
    (isAdmin) => {
      const get = vi.spyOn(api, 'get')
      const task: TaskLog = {
        id: 1,
        user_id: 1,
        task_id: 'task_retry',
        platform: 'vidu',
        action: 'text_to_video',
        channel_id: 12,
        group: 'default',
        quota: 100,
        submit_time: 1,
        status: 'SUCCESS',
        admin_info: { use_channel: ['8', '12'] },
      }
      render(
        <TaskDetailsDialog
          log={task}
          isAdmin={isAdmin}
          isRoot={false}
          open
          onOpenChange={() => undefined}
        />,
        { wrapper: TestProviders }
      )
      if (isAdmin) expect(screen.getByText('8 → 12')).toBeVisible()
      else expect(screen.queryByText('8 → 12')).toBeNull()
      expect(get).not.toHaveBeenCalled()
    }
  )

  test('updates the retry label when the interface language changes', async () => {
    render(<TaskChannel channels={['8', '12']} isAdmin />, {
      wrapper: TestProviders,
    })
    await act(() => i18n.changeLanguage('zh'))
    expect(
      await screen.findByRole('button', { name: '重试链路' })
    ).toBeVisible()
  })
})
