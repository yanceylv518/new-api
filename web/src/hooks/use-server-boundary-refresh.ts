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
import { useEffect } from 'react'

// 用服务端时间与响应接收时间计算延迟，避免依赖浏览器时钟与服务器对时。
export function useServerBoundaryRefresh(options: {
  enabled: boolean
  serverTime?: number
  nextChange?: number
  receivedAt: number
  refresh: () => unknown
}) {
  const { enabled, serverTime, nextChange, receivedAt, refresh } = options
  useEffect(() => {
    if (!enabled) return
    let timer: ReturnType<typeof setTimeout> | undefined
    const refreshVisible = () => {
      if (document.visibilityState === 'visible') void refresh()
    }
    const schedule = () => {
      if (!serverTime || !nextChange) return
      const remaining =
        (nextChange - serverTime) * 1000 - (Date.now() - receivedAt)
      timer = setTimeout(
        () => {
          if (remaining > 2147483647) schedule()
          else refreshVisible()
        },
        Math.max(100, Math.min(remaining + 100, 2147483647))
      )
    }
    schedule()
    document.addEventListener('visibilitychange', refreshVisible)
    return () => {
      if (timer !== undefined) clearTimeout(timer)
      document.removeEventListener('visibilitychange', refreshVisible)
    }
  }, [enabled, serverTime, nextChange, receivedAt, refresh])
}
