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
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, test } from 'vitest'

import type { UsageLog } from '../../data/schema'
import { useCommonLogsColumns } from '../columns/common-logs-columns'
import { DetailsDialog } from '../dialogs/details-dialog'
import { UsageLogsMobileList } from '../usage-logs-mobile-card'
import { UsageLogsProvider } from '../usage-logs-provider'

// 使用真实列、移动卡片和详情组件，防止任一入口泄露上游模型。
const log: UsageLog = {
  id: 1,
  user_id: 1,
  created_at: 1,
  type: 2,
  content: '',
  username: '',
  token_name: '',
  model_name: 'public-model',
  quota: 0,
  prompt_tokens: 0,
  completion_tokens: 0,
  use_time: 0,
  is_stream: false,
  channel: 0,
  channel_name: '',
  token_id: 1,
  group: '',
  ip: '',
  request_id: '',
  upstream_request_id: '',
  other: JSON.stringify({
    is_model_mapped: true,
    upstream_model_name: 'private-upstream',
  }),
}

// 仅取模型列，仍通过真实表格上下文驱动桌面与移动渲染。
function ModelPreview(props: { isAdmin: boolean; mobile: boolean }) {
  const columns = useCommonLogsColumns(props.isAdmin, false)
  const table = useReactTable({
    data: [log],
    columns: columns.filter(
      (column) => 'accessorKey' in column && column.accessorKey === 'model_name'
    ),
    getCoreRowModel: getCoreRowModel(),
  })
  if (props.mobile) {
    return <UsageLogsMobileList table={table} logCategory='common' />
  }
  const cell = table.getRowModel().rows[0].getAllCells()[0]
  return <>{flexRender(cell.column.columnDef.cell, cell.getContext())}</>
}

describe('model mapping visibility', () => {
  const clients: QueryClient[] = []
  afterEach(() => {
    clients.forEach((client) => client.clear())
    clients.length = 0
  })

  // 两种列表均保留请求模型，普通用户不能看到上游模型映射。
  test.each([false, true])(
    'hides mapping for regular users in mobile=%s',
    (mobile) => {
      const user = userEvent.setup()
      const view = render(<ModelPreview isAdmin={false} mobile={mobile} />)
      expect(screen.getByText('public-model')).toBeInTheDocument()
      expect(screen.queryByRole('button')).toBeNull()
      expect(screen.queryByText('private-upstream')).toBeNull()

      view.rerender(<ModelPreview isAdmin mobile={mobile} />)
      const mappingButton = screen.getByRole('button')
      expect(mappingButton).toHaveAttribute('aria-expanded', 'false')
      return user.click(mappingButton).then(async () => {
        expect(await screen.findByText('private-upstream')).toBeInTheDocument()
      })
    }
  )

  // 即使响应带映射字段，个人详情也不能展示；管理详情保留原信息。
  test.each([false, true])('gates details for isAdmin=%s', (isAdmin) => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    client.setQueryData(['status'], {}, { updatedAt: Date.now() + 60_000 })
    client.setQueryData(
      ['pricing'],
      { data: [], vendors: [] },
      { updatedAt: Date.now() + 60_000 }
    )
    render(
      <QueryClientProvider client={client}>
        <UsageLogsProvider>
          <DetailsDialog
            log={log}
            isAdmin={isAdmin}
            isRoot={false}
            open
            onOpenChange={() => undefined}
          />
        </UsageLogsProvider>
      </QueryClientProvider>
    )
    expect(screen.queryByText('private-upstream') !== null).toBe(isAdmin)
  })
})
