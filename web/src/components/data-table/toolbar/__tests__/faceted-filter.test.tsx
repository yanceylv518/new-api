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
import {
  getCoreRowModel,
  getFacetedRowModel,
  getFacetedUniqueValues,
  getFilteredRowModel,
  useReactTable,
  type ColumnFiltersState,
} from '@tanstack/react-table'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'

import { DataTableToolbar } from '../toolbar'

const groupOptions = [
  { label: 'default', value: 'default' },
  { label: 'vip', value: 'vip' },
]

// 使用稳定的分组选项模拟用户折扣页，确保筛选值变化仍会刷新记忆化组件。
function FilterHarness() {
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([])
  const table = useReactTable({
    data: [{ group: 'vip' }],
    columns: [{ id: 'group', accessorKey: 'group' }],
    state: { columnFilters, globalFilter: '' },
    onColumnFiltersChange: setColumnFilters,
    getCoreRowModel: getCoreRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getFacetedRowModel: getFacetedRowModel(),
    getFacetedUniqueValues: getFacetedUniqueValues(),
  })

  return (
    <DataTableToolbar
      table={table}
      filters={[
        {
          columnId: 'group',
          title: 'Group',
          options: groupOptions,
          singleSelect: true,
        },
      ]}
    />
  )
}

describe('DataTableFacetedFilter', () => {
  it('updates the selected badge when a stable-options filter changes', async () => {
    const user = userEvent.setup()
    render(<FilterHarness />)

    const trigger = screen.getByRole('button', { name: 'Group' })
    await user.click(trigger)
    await user.click(screen.getByRole('option', { name: /^vip/ }))
    expect(within(trigger).getByText('vip')).toBeVisible()

    await user.click(screen.getByRole('option', { name: /^default/ }))
    expect(within(trigger).getByText('default')).toBeVisible()
    expect(within(trigger).queryByText('vip')).not.toBeInTheDocument()
  })
})
