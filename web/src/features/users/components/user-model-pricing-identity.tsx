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
import { useTranslation } from 'react-i18next'

import { TableId } from '@/components/table-id'

import { USER_ROLES } from '../constants'

// 折扣总览和用户选择共用的最小用户信息，不依赖完整账号字段。
interface DiscountUser {
  id: number
  username: string
  display_name?: string
  email?: string
  role: number
}

function getUserInitials(user: DiscountUser): string {
  const source = (user.display_name || user.username).trim()
  return source.slice(0, 2).toUpperCase()
}

export function UserIdentity({
  user,
  showId = true,
}: {
  user: DiscountUser
  showId?: boolean
}) {
  return (
    <div className='flex min-w-0 items-center gap-2.5'>
      <div
        aria-hidden='true'
        className='bg-primary/10 text-primary flex size-8 shrink-0 items-center justify-center rounded-lg text-xs font-semibold uppercase'
      >
        {getUserInitials(user)}
      </div>
      <div className='min-w-0'>
        <div className='flex min-w-0 items-center gap-2'>
          <span className='min-w-0 truncate font-medium' title={user.username}>
            {user.username}
          </span>
          {showId && (
            <TableId value={`#${user.id}`} className='shrink-0 text-xs' />
          )}
        </div>
        {(user.display_name && user.display_name !== user.username) ||
        user.email ? (
          <p className='text-muted-foreground mt-0.5 max-w-[260px] truncate text-xs'>
            {user.display_name && user.display_name !== user.username
              ? user.display_name
              : user.email}
          </p>
        ) : null}
      </div>
    </div>
  )
}

// 用户折扣总览仅展示角色，账号启用状态不属于该页面的筛选和比较维度。
export function UserRole({ user }: { user: DiscountUser }) {
  const { t } = useTranslation()
  const role = USER_ROLES[user.role as keyof typeof USER_ROLES]

  if (!role) return null

  return (
    <span className='text-muted-foreground inline-flex items-center gap-1 text-xs'>
      <role.icon aria-hidden='true' className='size-3.5' />
      {t(role.labelKey)}
    </span>
  )
}
