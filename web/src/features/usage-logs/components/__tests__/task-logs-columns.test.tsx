import { renderHook } from '@testing-library/react'
import { describe, expect, test } from 'vitest'

import { useTaskLogsColumns } from '../columns/task-logs-columns'

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
})
