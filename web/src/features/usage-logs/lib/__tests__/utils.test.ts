import { afterEach, describe, expect, test, vi } from 'vitest'

import { getDefaultTimeRange } from '../utils'

describe('getDefaultTimeRange', () => {
  afterEach(() => {
    // 还原时间环境，避免影响同一进程中的其他日志筛选测试。
    vi.useRealTimers()
  })

  test('returns one hour before now and one hour after now', () => {
    const now = new Date('2026-09-09T04:00:00.000Z')
    vi.useFakeTimers()
    vi.setSystemTime(now)

    const range = getDefaultTimeRange()

    expect(range.start).toEqual(new Date('2026-09-09T03:00:00.000Z'))
    expect(range.end).toEqual(new Date('2026-09-09T05:00:00.000Z'))
  })
})
