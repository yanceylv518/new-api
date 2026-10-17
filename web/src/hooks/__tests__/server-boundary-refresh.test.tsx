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
import { act, cleanup, renderHook } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { useServerBoundaryRefresh } from '../use-server-boundary-refresh'

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-08T00:00:00Z'))
  vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible')
})

afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('discount boundary refresh', () => {
  it('refreshes at the server boundary even when the browser clock differs', () => {
    const refresh = vi.fn()
    renderHook(() =>
      useServerBoundaryRefresh({
        enabled: true,
        serverTime: 1000,
        nextChange: 1010,
        receivedAt: Date.now() - 2000,
        refresh,
      })
    )
    act(() => vi.advanceTimersByTime(8099))
    expect(refresh).not.toHaveBeenCalled()
    act(() => vi.advanceTimersByTime(1))
    expect(refresh).toHaveBeenCalledOnce()
  })

  it('replaces the previous boundary when a new response arrives', () => {
    const refresh = vi.fn()
    const options = {
      enabled: true,
      serverTime: 1000,
      nextChange: 1001,
      receivedAt: Date.now(),
      refresh,
    }
    const { rerender } = renderHook(useServerBoundaryRefresh, {
      initialProps: options,
    })
    rerender({ ...options, nextChange: 1005 })
    act(() => vi.advanceTimersByTime(1100))
    expect(refresh).not.toHaveBeenCalled()
    act(() => vi.advanceTimersByTime(4000))
    expect(refresh).toHaveBeenCalledOnce()
  })

  it('waits for page visibility after a boundary passes in the background', () => {
    const visibility = vi
      .spyOn(document, 'visibilityState', 'get')
      .mockReturnValue('hidden')
    const refresh = vi.fn()
    renderHook(() =>
      useServerBoundaryRefresh({
        enabled: true,
        serverTime: 1000,
        nextChange: 1001,
        receivedAt: Date.now(),
        refresh,
      })
    )
    act(() => vi.advanceTimersByTime(1100))
    expect(refresh).not.toHaveBeenCalled()
    visibility.mockReturnValue('visible')
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    expect(refresh).toHaveBeenCalledOnce()
  })

  it('cancels both timed and visibility refresh when disabled or unmounted', () => {
    const refresh = vi.fn()
    const options = {
      enabled: true,
      serverTime: 1000,
      nextChange: 1001,
      receivedAt: Date.now(),
      refresh,
    }
    const { rerender, unmount } = renderHook(useServerBoundaryRefresh, {
      initialProps: options,
    })
    rerender({ ...options, enabled: false })
    act(() => {
      vi.advanceTimersByTime(1100)
      document.dispatchEvent(new Event('visibilitychange'))
    })
    expect(refresh).not.toHaveBeenCalled()
    rerender({ ...options, nextChange: 1005 })
    unmount()
    act(() => {
      vi.advanceTimersByTime(5000)
      document.dispatchEvent(new Event('visibilitychange'))
    })
    expect(refresh).not.toHaveBeenCalled()
  })

  it('keeps distant schedules pending beyond the browser timer limit', () => {
    const refresh = vi.fn()
    renderHook(() =>
      useServerBoundaryRefresh({
        enabled: true,
        serverTime: 1000,
        nextChange: 1000 + 3000000,
        receivedAt: Date.now(),
        refresh,
      })
    )
    act(() => vi.advanceTimersByTime(2147483647))
    expect(refresh).not.toHaveBeenCalled()
    act(() => vi.advanceTimersByTime(3000000100 - 2147483647))
    expect(refresh).toHaveBeenCalledOnce()
  })
})
