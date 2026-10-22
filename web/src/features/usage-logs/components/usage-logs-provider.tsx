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
/* eslint-disable react-refresh/only-export-components */
import { createContext, useContext, useState, type ReactNode } from 'react'

import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import type { UsageLog } from '../data/schema'
import type { ChannelAffinityInfo } from '../types'

export type LogsViewScope = 'all' | 'self'
export type LogsViewAccess = 'self' | 'admin' | 'root'

export function resolveLogsViewAccess(
  role: number,
  viewScope: LogsViewScope
): LogsViewAccess {
  if (viewScope !== 'all' || role < ROLE.ADMIN) return 'self'
  return role === ROLE.SUPER_ADMIN ? 'root' : 'admin'
}

interface UsageLogsContextValue {
  selectedLog: UsageLog | null
  detailsDialogOpen: boolean
  setDetailsDialogOpen: (open: boolean) => void
  openLogDetails: (log: UsageLog) => void
  /** null 表示打开后先输入请求 ID 查询。 */
  timelineRequestId: string | null
  timelineDialogOpen: boolean
  setTimelineDialogOpen: (open: boolean) => void
  openRequestTimeline: (requestId: string | null) => void
  selectedUserId: number | null
  setSelectedUserId: (userId: number | null) => void
  userInfoDialogOpen: boolean
  setUserInfoDialogOpen: (open: boolean) => void
  affinityTarget: ChannelAffinityInfo | null
  setAffinityTarget: (target: ChannelAffinityInfo | null) => void
  affinityDialogOpen: boolean
  setAffinityDialogOpen: (open: boolean) => void
  sensitiveVisible: boolean
  setSensitiveVisible: (visible: boolean) => void
  viewScope: LogsViewScope
  setViewScope: (scope: LogsViewScope) => void
}

const UsageLogsContext = createContext<UsageLogsContextValue | undefined>(
  undefined
)

export function UsageLogsProvider({ children }: { children: ReactNode }) {
  const [selectedLog, setSelectedLog] = useState<UsageLog | null>(null)
  const [detailsDialogOpen, setDetailsDialogOpen] = useState(false)
  const [timelineRequestId, setTimelineRequestId] = useState<string | null>(
    null
  )
  const [timelineDialogOpen, setTimelineDialogOpen] = useState(false)
  const [selectedUserId, setSelectedUserId] = useState<number | null>(null)
  const [userInfoDialogOpen, setUserInfoDialogOpen] = useState(false)
  const [affinityTarget, setAffinityTarget] =
    useState<ChannelAffinityInfo | null>(null)
  const [affinityDialogOpen, setAffinityDialogOpen] = useState(false)
  const [sensitiveVisible, setSensitiveVisible] = useState(true)
  const [viewScope, setViewScopeState] = useState<LogsViewScope>('all')
  const setViewScope = (scope: LogsViewScope) => {
    setDetailsDialogOpen(false)
    setSelectedLog(null)
    setTimelineDialogOpen(false)
    setViewScopeState(scope)
  }
  const openLogDetails = (log: UsageLog) => {
    setSelectedLog(log)
    setDetailsDialogOpen(true)
  }
  // 时间线是独立弹窗；从日志详情跳转时先关闭详情，避免两层弹窗叠加。
  const openRequestTimeline = (requestId: string | null) => {
    setDetailsDialogOpen(false)
    setTimelineRequestId(requestId)
    setTimelineDialogOpen(true)
  }

  return (
    <UsageLogsContext.Provider
      value={{
        selectedLog,
        detailsDialogOpen,
        setDetailsDialogOpen,
        openLogDetails,
        timelineRequestId,
        timelineDialogOpen,
        setTimelineDialogOpen,
        openRequestTimeline,
        selectedUserId,
        setSelectedUserId,
        userInfoDialogOpen,
        setUserInfoDialogOpen,
        affinityTarget,
        setAffinityTarget,
        affinityDialogOpen,
        setAffinityDialogOpen,
        sensitiveVisible,
        setSensitiveVisible,
        viewScope,
        setViewScope,
      }}
    >
      {children}
    </UsageLogsContext.Provider>
  )
}

export function useUsageLogsContext() {
  const context = useContext(UsageLogsContext)
  if (!context) {
    throw new Error('useUsageLogsContext must be used within UsageLogsProvider')
  }
  return context
}

/**
 * Resolves the effective admin scope for usage logs: whether the current
 * user is allowed to view all users' logs (`canManageScope`), and whether
 * their current view preference (`viewScope`) has that scope active
 * (`isAdminView`). Data fetching and admin-only UI should key off
 * `isAdminView` rather than raw role, so an admin who switches to "only
 * mine" is treated exactly like a regular user for that view.
 */
export function useLogsViewScope() {
  const role = useAuthStore((state) => state.auth.user?.role ?? ROLE.GUEST)
  const { viewScope, setViewScope } = useUsageLogsContext()
  const canManageScope = role >= ROLE.ADMIN
  const viewAccess = resolveLogsViewAccess(role, viewScope)
  const isAdminView = viewAccess !== 'self'
  const isRootView = viewAccess === 'root'

  return {
    canManageScope,
    viewScope,
    setViewScope,
    isAdminView,
    isRootView,
    viewAccess,
  }
}
