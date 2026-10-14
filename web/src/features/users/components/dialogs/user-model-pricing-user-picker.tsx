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
import { useQuery } from '@tanstack/react-query'
import type {
  ColumnDef,
  OnChangeFn,
  PaginationState,
} from '@tanstack/react-table'
import { Loader2, Search, UserSearch } from 'lucide-react'
import { type ReactNode, useCallback, useState } from 'react'
import { useTranslation } from 'react-i18next'

import {
  DataTablePagination,
  DataTableView,
  useDataTable,
} from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { GroupBadge } from '@/components/group-badge'
import { LoadingState } from '@/components/loading-state'
import { TableId } from '@/components/table-id'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useMediaQuery } from '@/hooks'
import { useDebounce } from '@/hooks/use-debounce'
import { cn } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth-store'

import { getUsers, searchUsers } from '../../api'
import { USER_ROLE, isUserDeleted } from '../../constants'
import type { User } from '../../types'
import { UserIdentity, UserRole } from '../user-model-pricing-identity'

/** 从折扣总览添加折扣时先选择用户，再进入与用户管理页相同的模型折扣弹窗。 */
export function UserModelPricingUserPicker(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSelect: (user: Pick<User, 'id' | 'username'>) => void
  children?: ReactNode
}) {
  const { t } = useTranslation()
  const isMobile = useMediaQuery('(max-width: 640px)')
  const operatorRole = useAuthStore((state) => state.auth.user?.role ?? 0)
  const [keyword, setKeyword] = useState('')
  const [pagination, setPagination] = useState<PaginationState>(() => ({
    pageIndex: 0,
    pageSize: isMobile ? 10 : 20,
  }))
  const debouncedKeyword = useDebounce(keyword.trim(), 300)
  const isSearching = keyword.trim() !== debouncedKeyword
  const query = useQuery({
    queryKey: [
      'user-model-pricing-user-picker',
      debouncedKeyword,
      pagination.pageIndex + 1,
      pagination.pageSize,
    ],
    queryFn: async () => {
      const params = {
        p: pagination.pageIndex + 1,
        page_size: pagination.pageSize,
      }
      const result = debouncedKeyword
        ? await searchUsers({
            ...params,
            keyword: debouncedKeyword,
          })
        : await getUsers(params)
      if (!result.success || !result.data) {
        throw new Error(result.message || t('Failed to load users'))
      }
      return result.data
    },
    enabled: props.open && !isSearching,
    placeholderData: (previous) => previous,
    staleTime: 15_000,
  })
  const users = query.data?.items ?? []
  const isPageTransition = isSearching || query.isPlaceholderData
  const isBusy = isPageTransition || query.isFetching
  const SearchIcon = isBusy ? Loader2 : Search
  const onPaginationChange: OnChangeFn<PaginationState> = useCallback(
    (updater) => {
      if (isPageTransition) return
      setPagination((previous) => {
        const next = typeof updater === 'function' ? updater(previous) : updater
        return {
          ...next,
          pageIndex: next.pageSize === previous.pageSize ? next.pageIndex : 0,
        }
      })
    },
    [isPageTransition]
  )
  const ensurePageInRange = useCallback(
    (pageCount: number) => {
      if (!query.isSuccess || isPageTransition) return
      const lastPage = Math.max(0, pageCount - 1)
      setPagination((previous) =>
        previous.pageIndex > lastPage
          ? { ...previous, pageIndex: lastPage }
          : previous
      )
    },
    [query.isSuccess, isPageTransition]
  )
  // 与后端一致：超级管理员账号不设置折扣，普通管理员只能管理角色更低的用户。
  const canManage = (user: User) =>
    !isUserDeleted(user) &&
    user.role !== USER_ROLE.ROOT &&
    (operatorRole === USER_ROLE.ROOT || operatorRole > user.role)
  const columns: ColumnDef<User>[] = [
    {
      accessorKey: 'id',
      header: t('ID'),
      cell: ({ row }) => <TableId value={`#${row.original.id}`} />,
    },
    {
      accessorKey: 'username',
      header: t('Username'),
      cell: ({ row }) => <UserIdentity user={row.original} showId={false} />,
    },
    {
      accessorKey: 'group',
      header: t('Group'),
      cell: ({ row }) => <GroupBadge group={row.original.group} />,
    },
    {
      accessorKey: 'role',
      header: t('Role'),
      cell: ({ row }) => <UserRole user={row.original} />,
    },
    {
      id: 'actions',
      header: t('Actions'),
      cell: ({ row }) => (
        <Button
          type='button'
          variant='outline'
          size='sm'
          disabled={isPageTransition || !canManage(row.original)}
          aria-label={t('Set discounts for {{username}}', {
            username: row.original.username,
          })}
          onClick={() =>
            props.onSelect({
              id: row.original.id,
              username: row.original.username,
            })
          }
        >
          {t('Select')}
        </Button>
      ),
    },
  ]
  const { table } = useDataTable({
    data: users,
    columns,
    getRowId: (user) => String(user.id),
    enableSorting: false,
    pagination,
    onPaginationChange,
    manualPagination: true,
    manualFiltering: true,
    totalCount: query.data?.total ?? 0,
    ensurePageInRange,
  })
  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Add user discount')}
      description={t('Choose a user, then set discounts for their models.')}
      contentClassName='sm:max-w-2xl'
      contentHeight='min(50vh, 24rem)'
      bodyWrapperClassName='overflow-hidden'
      bodyClassName='flex h-full min-h-0 flex-col'
      footer={
        query.data ? (
          <fieldset
            disabled={isPageTransition}
            aria-busy={isBusy}
            className='w-full min-w-0'
          >
            <DataTablePagination table={table} compact />
          </fieldset>
        ) : undefined
      }
    >
      <div className='flex h-full min-h-0 flex-col gap-3'>
        <div className='relative shrink-0'>
          <SearchIcon
            aria-hidden='true'
            className={cn(
              'text-muted-foreground pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2',
              isBusy && 'animate-spin'
            )}
          />
          <Input
            autoFocus
            value={keyword}
            onChange={(event) => {
              setKeyword(event.target.value)
              setPagination((previous) => ({ ...previous, pageIndex: 0 }))
            }}
            placeholder={t('Search by username, ID, or email...')}
            aria-label={t('Search users')}
            className='pl-9'
          />
        </div>
        {(query.isPending || isSearching) && users.length === 0 && (
          <LoadingState className='min-h-0 flex-1' />
        )}
        {query.isError && !isSearching && (
          <ErrorState
            className='min-h-0 flex-1 overflow-y-auto py-2'
            title={t('Failed to load users')}
            onRetry={() => void query.refetch()}
          />
        )}
        {query.isSuccess && !isPageTransition && users.length === 0 && (
          <EmptyState
            icon={UserSearch}
            title={t('No users found')}
            description={t('Try a different username, ID, or email.')}
            className='min-h-0 flex-1 overflow-y-auto py-2'
          />
        )}
        {users.length > 0 && !query.isError && (
          <DataTableView
            table={table}
            containerProps={{
              role: 'region',
              'aria-label': t('Users'),
              'aria-busy': isBusy,
            }}
            containerClassName={cn(
              'min-h-0 flex-1 rounded-md transition-opacity',
              isPageTransition && 'pointer-events-none opacity-60'
            )}
            tableContainerClassName='h-full min-h-0'
            tableClassName='min-w-[36rem] table-fixed'
            tableHeaderClassName='[background-color:var(--table-header)]'
            bodyContainerClassName='[scrollbar-gutter:stable]'
            splitHeader
            getColumnClassName={(columnId, part) =>
              cn(
                columnId === 'id' && 'w-16',
                (columnId === 'group' || columnId === 'role') && 'w-28',
                columnId === 'actions' && 'w-32 text-right',
                part === 'cell' && 'overflow-hidden'
              )
            }
            getRowClassName={(row) =>
              canManage(row.original) ? undefined : 'opacity-50'
            }
          />
        )}
      </div>
      {props.children}
    </Dialog>
  )
}
