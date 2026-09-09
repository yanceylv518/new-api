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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import type { ColumnDef, Row } from '@tanstack/react-table'
import {
  BadgePercent,
  ChevronDown,
  Layers3,
  ListChecks,
  Loader2,
  Pencil,
  RefreshCw,
  TriangleAlert,
  Users,
} from 'lucide-react'
import {
  createContext,
  useContext,
  useCallback,
  useMemo,
  useState,
} from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { GroupBadge } from '@/components/group-badge'
import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { TableId } from '@/components/table-id'
import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import { TableCell, TableRow } from '@/components/ui/table'
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { useMediaQuery } from '@/hooks'
import { useTableUrlState } from '@/hooks/use-table-url-state'
import { cn } from '@/lib/utils'

import {
  getUserModelPricingOverview,
  getUserModelPricingRulePage,
} from '../api'
import { USER_ROLES, USER_STATUSES } from '../constants'
import type {
  UserModelPricingItem,
  UserModelPricingOverviewData,
  UserModelPricingOverviewItem,
  UserModelPricingOverviewUser,
} from '../types'
import { UserModelPricingDialog } from './dialogs/user-model-pricing-dialog'

const route = getRouteApi('/_authenticated/users/model-pricing')
// 搜索作用域随总览更新，展开区按作用域独立缓存，避免混入其他模型。
const PricingSearchContext = createContext('')

const EMPTY_OVERVIEW: UserModelPricingOverviewData = {
  items: [],
  total: 0,
  page: 1,
  page_size: 20,
  total_rules: 0,
  total_models: 0,
}

// 数字格式化集中在页面边界，确保基点值始终按“支付原价百分比”显示。
function formatDiscountNumber(discountBPS: number): string {
  return new Intl.NumberFormat(undefined, {
    maximumFractionDigits: 2,
  }).format(discountBPS / 100)
}

function formatDiscountRate(discountBPS: number): string {
  return `${formatDiscountNumber(discountBPS)}%`
}

function getUserInitials(user: UserModelPricingOverviewUser): string {
  const source = (user.display_name || user.username).trim()
  return source.slice(0, 2).toUpperCase()
}

// 摘要只展示折扣区间，收起状态不下载完整规则。
function DiscountSummary({ item }: { item: UserModelPricingOverviewItem }) {
  return (
    <span className='text-info font-mono font-semibold tabular-nums'>
      {formatDiscountRate(item.min_discount_bps ?? 10000)}
      {item.max_discount_bps !== item.min_discount_bps &&
        ` – ${formatDiscountRate(item.max_discount_bps ?? 10000)}`}
    </span>
  )
}

/** 展开才挂载查询，每页最多 20 个模型；切页替换而不累积 DOM 和结果。 */
function DiscountRulePage({ userId }: { userId: number }) {
  const keyword = useContext(PricingSearchContext)
  return (
    <DiscountRulePageContent
      key={`${userId}:${keyword}`}
      userId={userId}
      keyword={keyword}
    />
  )
}

function DiscountRulePageContent(props: { userId: number; keyword: string }) {
  const { t } = useTranslation()
  const [page, setPage] = useState(1)
  const query = useQuery({
    queryKey: ['user-model-pricing-rules', props.userId, props.keyword, page],
    queryFn: async ({ signal }) => {
      const response = await getUserModelPricingRulePage(
        props.userId,
        props.keyword,
        page,
        signal
      )
      if (!response.success || !response.data) {
        throw new Error(response.message || t('Failed to load user discounts'))
      }
      return response.data
    },
    staleTime: 0,
    gcTime: 0,
    placeholderData: (previous) => previous,
    retry: 1,
    refetchOnWindowFocus: false,
  })
  const totalPages = Math.max(1, Math.ceil((query.data?.total ?? 0) / 20))
  return (
    <div className='space-y-3 whitespace-normal' aria-busy={query.isFetching}>
      <div className='flex items-center justify-between gap-2'>
        <span className='text-muted-foreground text-xs'>
          {t('Page {{current}} of {{total}}', {
            current: page,
            total: totalPages,
          })}
        </span>
        <div className='flex gap-2'>
          <Button
            variant='outline'
            size='sm'
            disabled={page === 1 || query.isFetching}
            onClick={() => setPage((value) => value - 1)}
          >
            {t('Previous page')}
          </Button>
          <Button
            variant='outline'
            size='sm'
            disabled={!query.data || page >= totalPages || query.isFetching}
            onClick={() => setPage((value) => value + 1)}
          >
            {t('Next page')}
          </Button>
        </div>
      </div>
      {query.isPending ? (
        <LoadingState className='min-h-24' message={t('Loading')} />
      ) : null}
      {query.isError ? (
        <OverviewError
          onRetry={() => {
            void query.refetch()
          }}
        />
      ) : null}
      {query.data ? (
        <div className={query.isPlaceholderData ? 'opacity-60' : undefined}>
          <DiscountRuleGrid rules={query.data.items} />
        </div>
      ) : null}
      {query.data?.items.length === 0 ? (
        <p className='text-muted-foreground py-4 text-sm'>
          {t('No models matched your search.')}
        </p>
      ) : null}
    </div>
  )
}

function DiscountRuleGrid({ rules }: { rules: UserModelPricingItem[] }) {
  const { t } = useTranslation()

  return (
    <ul
      aria-label={t('Model discounts')}
      // 根据内容区宽度自动增减列数，窄屏退化为单列，长模型名在卡片内换行。
      className='grid min-w-0 grid-cols-[repeat(auto-fit,minmax(min(100%,18rem),1fr))] gap-3 whitespace-normal'
    >
      {rules.map((rule) => (
        <li
          key={rule.model_name}
          className='bg-background/80 flex min-w-0 items-center justify-between gap-3 rounded-lg border px-3.5 py-3'
        >
          <div className='min-w-0'>
            <p
              className='font-mono text-sm leading-relaxed [overflow-wrap:anywhere]'
              title={rule.model_name}
            >
              {rule.model_name}
            </p>
          </div>
          <div className='shrink-0 text-right'>
            <p className='text-info font-mono text-sm font-semibold tabular-nums'>
              {formatDiscountRate(rule.discount_bps)}
            </p>
          </div>
        </li>
      ))}
    </ul>
  )
}

function UserIdentity({ user }: { user: UserModelPricingOverviewUser }) {
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
          <TableId value={`#${user.id}`} className='shrink-0 text-xs' />
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

function UserStatus({ user }: { user: UserModelPricingOverviewUser }) {
  const { t } = useTranslation()
  const status = USER_STATUSES[user.status as keyof typeof USER_STATUSES]
  const role = USER_ROLES[user.role as keyof typeof USER_ROLES]

  return (
    <div className='flex min-w-0 flex-wrap items-center gap-1.5'>
      {status ? (
        <StatusBadge
          label={t(status.labelKey)}
          variant={status.variant}
          copyable={false}
        />
      ) : null}
      {role ? (
        <span className='text-muted-foreground inline-flex items-center gap-1 text-xs'>
          <role.icon aria-hidden='true' className='size-3.5' />
          {t(role.labelKey)}
        </span>
      ) : null}
    </div>
  )
}

function ExpandDiscountsButton({
  expanded,
  username,
  onClick,
}: {
  expanded: boolean
  username: string
  onClick: () => void
}) {
  const { t } = useTranslation()
  const label = expanded
    ? t('Hide discounts for {{username}}', { username })
    : t('Show discounts for {{username}}', { username })

  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant='ghost'
            size='icon-sm'
            onClick={onClick}
            aria-label={label}
            aria-expanded={expanded}
          />
        }
      >
        <ChevronDown
          aria-hidden='true'
          className={cn(
            'transition-transform duration-200 motion-reduce:transition-none',
            expanded && 'rotate-180'
          )}
        />
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  )
}

function EditDiscountsButton({
  username,
  onClick,
}: {
  username: string
  onClick: () => void
}) {
  const { t } = useTranslation()

  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            variant='ghost'
            size='icon-sm'
            onClick={onClick}
            aria-label={t('Edit discounts for {{username}}', { username })}
          />
        }
      >
        <Pencil aria-hidden='true' />
      </TooltipTrigger>
      <TooltipContent>{t('Edit discounts')}</TooltipContent>
    </Tooltip>
  )
}

function RuleCount({ count }: { count: number }) {
  const { t } = useTranslation()

  return (
    <div className='flex items-baseline gap-1.5 whitespace-nowrap'>
      <span className='font-mono text-base font-semibold tabular-nums'>
        {count.toLocaleString()}
      </span>
      <span className='text-muted-foreground text-xs'>{t('Models')}</span>
    </div>
  )
}

function UserModelPricingDesktopRow({
  item,
  expanded,
  onToggle,
  onEdit,
}: {
  item: UserModelPricingOverviewItem
  expanded: boolean
  onToggle: () => void
  onEdit: () => void
}) {
  return (
    // 每个用户独立成组，吸顶行在当前模型列表结束时被下一用户自然顶走。
    <tbody className='[clip-path:inset(0)] [&>tr]:animate-none'>
      <TableRow
        aria-expanded={expanded ? true : undefined}
        className={cn(
          'sticky top-10 z-[5] bg-background shadow-[0_1px_0_var(--border)] [&>td]:py-3',
          expanded && '[&>td]:bg-muted/40'
        )}
      >
        <TableCell className='w-12 px-2'>
          <ExpandDiscountsButton
            expanded={expanded}
            username={item.user.username}
            onClick={onToggle}
          />
        </TableCell>
        <TableCell className='min-w-[240px]'>
          <UserIdentity user={item.user} />
        </TableCell>
        <TableCell className='min-w-[130px]'>
          <GroupBadge group={item.user.group} />
        </TableCell>
        <TableCell className='min-w-[170px]'>
          <UserStatus user={item.user} />
        </TableCell>
        <TableCell className='min-w-[110px]'>
          <RuleCount count={item.rule_count} />
        </TableCell>
        <TableCell className='max-w-[520px] min-w-[300px]'>
          <DiscountSummary item={item} />
        </TableCell>
        <TableCell className='w-14 px-2 text-right'>
          <EditDiscountsButton username={item.user.username} onClick={onEdit} />
        </TableCell>
      </TableRow>
      {expanded && (
        <TableRow className='bg-muted/20 hover:bg-muted/20 !h-auto'>
          <TableCell colSpan={7} className='px-4 py-5'>
            {/* 用户行已展示规则数量，展开区直接列出模型，避免重复标题和嵌套卡片。 */}
            <DiscountRulePage userId={item.user.id} />
          </TableCell>
        </TableRow>
      )}
    </tbody>
  )
}

export function UserModelPricingMobileRow({
  item,
  expanded,
  onToggle,
  onEdit,
}: {
  item: UserModelPricingOverviewItem
  expanded: boolean
  onToggle: () => void
  onEdit: () => void
}) {
  return (
    <div
      className={cn(
        '[background-color:var(--data-table-card-bg,var(--table-row))] p-3',
        expanded && 'bg-muted/20'
      )}
    >
      {/* 手机端将用户信息作为组内吸顶区，外层组保留边界以实现自然交接。 */}
      <div className='bg-background sticky top-0 z-[5] -mx-3 -mt-3 px-3 py-3 shadow-[0_1px_0_var(--border)]'>
        <div className='flex min-w-0 items-start gap-2'>
          <ExpandDiscountsButton
            expanded={expanded}
            username={item.user.username}
            onClick={onToggle}
          />
          <div className='min-w-0 flex-1'>
            <UserIdentity user={item.user} />
          </div>
          <EditDiscountsButton username={item.user.username} onClick={onEdit} />
        </div>
        <div className='mt-3 flex flex-wrap items-center gap-2'>
          <GroupBadge group={item.user.group} />
          <UserStatus user={item.user} />
          <RuleCount count={item.rule_count} />
        </div>
      </div>
      {!expanded && (
        <div className='mt-3'>
          <DiscountSummary item={item} />
        </div>
      )}
      {expanded && (
        <div className='mt-4 border-t pt-4 pb-2'>
          {/* 移动端与桌面端保持一致，分隔线下直接展示模型列表。 */}
          <DiscountRulePage userId={item.user.id} />
        </div>
      )}
    </div>
  )
}

function OverviewEmpty({ hasSearch }: { hasSearch: boolean }) {
  const { t } = useTranslation()

  return (
    <EmptyState
      bordered
      icon={BadgePercent}
      title={t('No user discounts configured')}
      description={
        hasSearch
          ? t('No users or models matched your search.')
          : t('No model discounts have been configured yet.')
      }
    />
  )
}

function OverviewMobileList({
  items,
  isLoading,
  isFetching,
  hasSearch,
  expandedUserIds,
  onToggle,
  onEdit,
}: {
  items: UserModelPricingOverviewItem[]
  isLoading: boolean
  isFetching: boolean
  hasSearch: boolean
  expandedUserIds: Set<number>
  onToggle: (userId: number) => void
  onEdit: (user: UserModelPricingOverviewUser) => void
}) {
  if (isLoading) {
    return (
      <div className='divide-y overflow-hidden rounded-lg border'>
        {Array.from({ length: 5 }, (_, index) => (
          <div key={index} className='space-y-3 p-3'>
            <div className='flex items-center gap-2'>
              <div className='bg-muted size-7 animate-pulse rounded-md' />
              <div className='bg-muted h-4 w-36 animate-pulse rounded' />
            </div>
            <div className='bg-muted h-4 w-4/5 animate-pulse rounded' />
          </div>
        ))}
      </div>
    )
  }

  if (items.length === 0) {
    return <OverviewEmpty hasSearch={hasSearch} />
  }

  return (
    <div aria-busy={isFetching} className='divide-y rounded-lg border'>
      {items.map((item) => (
        <UserModelPricingMobileRow
          key={item.user.id}
          item={item}
          expanded={expandedUserIds.has(item.user.id)}
          onToggle={() => onToggle(item.user.id)}
          onEdit={() => onEdit(item.user)}
        />
      ))}
    </div>
  )
}

function OverviewStats({
  data,
  isLoading,
}: {
  data: UserModelPricingOverviewData
  isLoading: boolean
}) {
  const { t } = useTranslation()
  const stats = [
    {
      label: t('Users with discounts'),
      value: data.total,
      icon: Users,
      iconClassName: 'text-info bg-info/10',
    },
    {
      label: t('Discount rules'),
      value: data.total_rules,
      icon: ListChecks,
      iconClassName: 'text-warning bg-warning/10',
    },
    {
      label: t('Models covered'),
      value: data.total_models,
      icon: Layers3,
      iconClassName: 'text-success bg-success/10',
    },
  ] as const

  return (
    <div className='grid shrink-0 grid-cols-3 gap-2 sm:gap-3'>
      {stats.map((stat) => (
        <div
          key={stat.label}
          className='bg-card flex min-w-0 flex-col items-start gap-2 rounded-lg border px-3 py-3 sm:flex-row sm:items-center sm:gap-3 sm:px-3.5'
        >
          <div
            className={cn(
              'flex size-8 shrink-0 items-center justify-center rounded-md',
              stat.iconClassName
            )}
          >
            <stat.icon aria-hidden='true' className='size-4' />
          </div>
          <div className='min-w-0'>
            <p className='text-muted-foreground min-h-10 text-xs leading-relaxed font-medium sm:min-h-0'>
              {stat.label}
            </p>
            {isLoading ? (
              <div className='bg-muted mt-1 h-5 w-14 animate-pulse rounded' />
            ) : (
              <p className='mt-0.5 font-mono text-lg font-semibold tabular-nums'>
                {stat.value.toLocaleString()}
              </p>
            )}
          </div>
        </div>
      ))}
    </div>
  )
}

function OverviewError({ onRetry }: { onRetry: () => void }) {
  const { t } = useTranslation()

  return (
    <ErrorState
      title={t('Failed to load user discounts')}
      onRetry={onRetry}
      className='border-destructive/30 bg-destructive/5 min-h-56 rounded-lg border'
    />
  )
}

function OverviewRefreshError({ onRetry }: { onRetry: () => void }) {
  const { t } = useTranslation()

  return (
    <Alert variant='destructive' className='shrink-0 items-center'>
      <TriangleAlert aria-hidden='true' />
      <AlertDescription className='flex flex-wrap items-center justify-between gap-2'>
        <span>
          {t('User discounts could not be refreshed. Showing the last result.')}
        </span>
        <Button variant='outline' size='sm' onClick={onRetry}>
          <RefreshCw aria-hidden='true' />
          {t('Retry')}
        </Button>
      </AlertDescription>
    </Alert>
  )
}

/** 管理端用户折扣总览，负责分页查询、展开状态和已有编辑弹窗的联动。 */
export function UserModelPricingOverview() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const isMobile = useMediaQuery('(max-width: 640px)')
  const [expandedUserIds, setExpandedUserIds] = useState<Set<number>>(
    () => new Set()
  )
  const [editingUser, setEditingUser] =
    useState<UserModelPricingOverviewUser | null>(null)

  const {
    globalFilter,
    onGlobalFilterChange,
    columnFilters,
    onColumnFiltersChange,
    pagination,
    onPaginationChange,
    ensurePageInRange,
  } = useTableUrlState({
    search: route.useSearch(),
    navigate: route.useNavigate(),
    pagination: {
      defaultPage: 1,
      defaultPageSize: isMobile ? 10 : 20,
    },
    globalFilter: { enabled: true, key: 'filter' },
  })

  const query = useQuery({
    queryKey: [
      'user-model-pricing-overview',
      pagination.pageIndex + 1,
      pagination.pageSize,
      globalFilter,
    ],
    queryFn: async ({ signal }) => {
      const response = await getUserModelPricingOverview(
        {
          keyword: globalFilter,
          p: pagination.pageIndex + 1,
          page_size: pagination.pageSize,
        },
        signal
      )
      if (!response.success || !response.data) {
        throw new Error(response.message || t('Failed to load user discounts'))
      }
      return response.data
    },
    placeholderData: (previous) => previous,
    staleTime: 15_000,
  })

  const overview = query.data ?? EMPTY_OVERVIEW
  const items = overview.items
  const hasSearch = Boolean(globalFilter?.trim())

  const toggleUser = useCallback((userId: number) => {
    setExpandedUserIds((previous) => {
      const next = new Set(previous)
      if (next.has(userId)) next.delete(userId)
      else next.add(userId)
      return next
    })
  }, [])

  const editUser = useCallback((user: UserModelPricingOverviewUser) => {
    setEditingUser(user)
  }, [])

  const toggleAll = useCallback(() => {
    setExpandedUserIds((previous) => {
      const allExpanded =
        items.length > 0 && items.every((item) => previous.has(item.user.id))
      return allExpanded
        ? new Set()
        : new Set(items.map((item) => item.user.id))
    })
  }, [items])

  const allExpanded =
    items.length > 0 && items.every((item) => expandedUserIds.has(item.user.id))

  const refreshOverview = useCallback(() => {
    void query.refetch()
    void queryClient.invalidateQueries({
      queryKey: ['user-model-pricing-rules'],
    })
  }, [query, queryClient])

  const handleSaved = useCallback(async () => {
    await queryClient.invalidateQueries({
      queryKey: ['user-model-pricing-rules'],
    })
    await queryClient.invalidateQueries({
      queryKey: ['user-model-pricing-overview'],
    })
  }, [queryClient])

  const columns = useMemo<ColumnDef<UserModelPricingOverviewItem>[]>(
    () => [
      { id: 'expand', header: '', size: 48, enableHiding: false },
      {
        id: 'user',
        accessorFn: (item) => item.user.username,
        header: t('User'),
        size: 280,
        enableHiding: false,
      },
      {
        id: 'group',
        accessorFn: (item) => item.user.group,
        header: t('Group'),
        size: 150,
      },
      {
        id: 'status',
        accessorFn: (item) => item.user.status,
        header: t('Status'),
        size: 170,
      },
      {
        id: 'models',
        accessorKey: 'rule_count',
        header: t('Discounted models'),
        size: 140,
      },
      {
        id: 'discounts',
        accessorFn: (item) =>
          item.rules.map((rule) => rule.model_name).join(' '),
        header: t('Model discounts'),
        size: 380,
      },
      { id: 'actions', header: t('Actions'), size: 64, enableHiding: false },
    ],
    [t]
  )

  const renderDesktopRow = useCallback(
    (row: Row<UserModelPricingOverviewItem>) => {
      const item = row.original
      return (
        <UserModelPricingDesktopRow
          key={row.id}
          item={item}
          expanded={expandedUserIds.has(item.user.id)}
          onToggle={() => toggleUser(item.user.id)}
          onEdit={() => editUser(item.user)}
        />
      )
    },
    [editUser, expandedUserIds, toggleUser]
  )

  const { table } = useDataTable({
    data: items,
    columns,
    getRowId: (item) => String(item.user.id),
    pagination,
    onPaginationChange,
    manualPagination: true,
    manualFiltering: true,
    columnFilters,
    onColumnFiltersChange,
    globalFilter,
    onGlobalFilterChange,
    totalCount: overview.total,
    ensurePageInRange,
  })

  if (query.isError && !query.data) {
    return (
      <SectionPageLayout fixedContent>
        <SectionPageLayout.Breadcrumb>
          <OverviewBreadcrumb />
        </SectionPageLayout.Breadcrumb>
        <SectionPageLayout.Title>{t('User Discounts')}</SectionPageLayout.Title>
        <SectionPageLayout.Actions>
          <Button
            variant='outline'
            size='sm'
            onClick={refreshOverview}
            disabled={query.isFetching}
          >
            {query.isFetching ? (
              <Loader2 aria-hidden='true' className='animate-spin' />
            ) : (
              <RefreshCw aria-hidden='true' />
            )}
            {t('Refresh')}
          </Button>
        </SectionPageLayout.Actions>
        <SectionPageLayout.Content>
          <OverviewError onRetry={refreshOverview} />
        </SectionPageLayout.Content>
      </SectionPageLayout>
    )
  }

  return (
    <PricingSearchContext.Provider value={globalFilter ?? ''}>
      <SectionPageLayout fixedContent>
        <SectionPageLayout.Breadcrumb>
          <OverviewBreadcrumb />
        </SectionPageLayout.Breadcrumb>
        <SectionPageLayout.Title>{t('User Discounts')}</SectionPageLayout.Title>
        <SectionPageLayout.Actions>
          <Button
            variant='outline'
            size='sm'
            onClick={refreshOverview}
            disabled={query.isFetching}
          >
            {query.isFetching ? (
              <Loader2 aria-hidden='true' className='animate-spin' />
            ) : (
              <RefreshCw aria-hidden='true' />
            )}
            {t('Refresh')}
          </Button>
        </SectionPageLayout.Actions>
        <SectionPageLayout.Content>
          <div className='flex h-full min-h-0 flex-col gap-3 sm:gap-4'>
            <OverviewStats data={overview} isLoading={query.isLoading} />
            {query.isError && query.data ? (
              <OverviewRefreshError onRetry={refreshOverview} />
            ) : null}
            <div className='min-h-0 flex-1'>
              <DataTablePage
                table={table}
                columns={columns}
                isLoading={query.isLoading}
                // 同页刷新保留列表亮度与交互，仅切页占位数据期间锁定旧内容。
                isFetching={query.isFetching && query.isPlaceholderData}
                emptyTitle={t('No user discounts configured')}
                emptyDescription={
                  hasSearch
                    ? t('No users or models matched your search.')
                    : t('No model discounts have been configured yet.')
                }
                emptyIcon={<BadgePercent aria-hidden='true' />}
                skeletonKeyPrefix='user-model-pricing-overview-skeleton'
                applyHeaderSize
                renderRow={renderDesktopRow}
                renderRowGroups
                // 分离边框且清零间距，避免折叠边框在吸顶合成时露出下方文字。
                tableClassName='[&_table]:border-separate [&_table]:border-spacing-0'
                mobile={
                  <OverviewMobileList
                    items={items}
                    isLoading={query.isLoading}
                    isFetching={query.isFetching}
                    hasSearch={hasSearch}
                    expandedUserIds={expandedUserIds}
                    onToggle={toggleUser}
                    onEdit={editUser}
                  />
                }
                toolbarProps={{
                  searchPlaceholder: t('Search users or models...'),
                  searchDebounceMs: 400,
                  hideViewOptions: true,
                  preActions:
                    items.length > 0 ? (
                      <Button
                        variant='ghost'
                        size='sm'
                        onClick={toggleAll}
                        aria-label={
                          allExpanded ? t('Collapse all') : t('Expand all')
                        }
                      >
                        {allExpanded ? (
                          <ChevronDown
                            aria-hidden='true'
                            className='rotate-180'
                          />
                        ) : (
                          <ChevronDown aria-hidden='true' />
                        )}
                        <span>
                          {allExpanded ? t('Collapse all') : t('Expand all')}
                        </span>
                      </Button>
                    ) : undefined,
                }}
              />
            </div>
          </div>
        </SectionPageLayout.Content>
      </SectionPageLayout>

      {editingUser ? (
        <UserModelPricingDialog
          open
          onOpenChange={(open) => {
            if (!open) setEditingUser(null)
          }}
          user={{ id: editingUser.id, username: editingUser.username }}
          onSaved={handleSaved}
        />
      ) : null}
    </PricingSearchContext.Provider>
  )
}

function OverviewBreadcrumb() {
  const { t } = useTranslation()

  return (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbLink render={<Link to='/users' />}>
            {t('Users')}
          </BreadcrumbLink>
        </BreadcrumbItem>
        <BreadcrumbSeparator />
        <BreadcrumbItem>
          <BreadcrumbPage>{t('User Discounts')}</BreadcrumbPage>
        </BreadcrumbItem>
      </BreadcrumbList>
    </Breadcrumb>
  )
}
