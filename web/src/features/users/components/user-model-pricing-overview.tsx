import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
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
import type {
  ColumnDef,
  ColumnFiltersState,
  OnChangeFn,
  PaginationState,
  Row,
} from '@tanstack/react-table'
import {
  BadgePercent,
  Layers3,
  List,
  ListChecks,
  Loader2,
  Pencil,
  RefreshCw,
  TriangleAlert,
  Users,
} from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTablePage, useDataTable } from '@/components/data-table'
import {
  sideDrawerContentClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { GroupBadge } from '@/components/group-badge'
import { SectionPageLayout } from '@/components/layout'
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
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import { ScrollArea } from '@/components/ui/scroll-area'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
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
  getGroups,
  getUserModelPricingOverview,
  getUserModelPricingRulePage,
} from '../api'
import { getUserRoleOptions, USER_ROLES } from '../constants'
import type {
  UserModelPricingItem,
  UserModelPricingOverviewData,
  UserModelPricingOverviewItem,
  UserModelPricingOverviewUser,
} from '../types'
import { UserModelPricingDialog } from './dialogs/user-model-pricing-dialog'

const route = getRouteApi('/_authenticated/users/model-pricing')

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

// 抽屉头部展示当前用户的折扣区间，主表使用模型预览避免信息过于抽象。
function DiscountSummary({ item }: { item: UserModelPricingOverviewItem }) {
  return (
    <span className='text-info font-mono font-semibold tabular-nums'>
      {formatDiscountRate(item.min_discount_bps ?? 10000)}
      {item.max_discount_bps !== item.min_discount_bps &&
        ` – ${formatDiscountRate(item.max_discount_bps ?? 10000)}`}
    </span>
  )
}

// 主表只展示前三个模型，剩余数量通过计数提示；具体规则在抽屉中完整查看。
function DiscountModelPreview({
  item,
}: {
  item: UserModelPricingOverviewItem
}) {
  const previewRules = (item.preview_rules ?? item.rules).slice(0, 3)

  if (previewRules.length === 0) {
    return <span className='text-muted-foreground'>—</span>
  }

  return (
    <div className='flex min-w-0 flex-wrap items-center gap-1.5'>
      {previewRules.map((rule) => (
        <span
          key={rule.model_name}
          className='border-border/70 bg-muted/40 inline-flex max-w-full min-w-0 items-center gap-1.5 rounded-md border px-2 py-1 text-xs'
          title={`${rule.model_name} - ${formatDiscountRate(rule.discount_bps)}`}
        >
          <span className='min-w-0 truncate font-mono' translate='no'>
            {rule.model_name}
          </span>
          <span className='text-info shrink-0 font-mono font-semibold tabular-nums'>
            {formatDiscountRate(rule.discount_bps)}
          </span>
        </span>
      ))}
      {item.rule_count > previewRules.length ? (
        <span className='text-muted-foreground shrink-0 font-mono text-xs tabular-nums'>
          +{item.rule_count - previewRules.length}
        </span>
      ) : null}
    </div>
  )
}

type UserModelPricingRulePage = {
  items: UserModelPricingItem[]
  total: number
  page_size: number
}

/** 抽屉内按滚动位置加载模型，页面本身只保留用户分页。 */
function DiscountRuleList(props: { userId: number; keyword: string }) {
  const { t } = useTranslation()
  const scrollAreaRef = useRef<HTMLDivElement>(null)
  const loadMoreRef = useRef<HTMLDivElement>(null)
  const query = useInfiniteQuery<UserModelPricingRulePage>({
    queryKey: ['user-model-pricing-rules', props.userId, props.keyword],
    initialPageParam: 1,
    queryFn: async ({ pageParam, signal }) => {
      const response = await getUserModelPricingRulePage(
        props.userId,
        props.keyword,
        Number(pageParam),
        signal
      )
      if (!response.success || !response.data) {
        throw new Error(response.message || t('Failed to load user discounts'))
      }
      return response.data
    },
    getNextPageParam: (lastPage, allPages) => {
      const loaded = allPages.reduce(
        (count, page) => count + page.items.length,
        0
      )
      if (lastPage.items.length === 0 || loaded >= lastPage.total) {
        return undefined
      }
      return allPages.length + 1
    },
    staleTime: 15_000,
    gcTime: 5 * 60_000,
    retry: 1,
    refetchOnWindowFocus: false,
  })
  const { fetchNextPage, hasNextPage, isFetchingNextPage } = query
  const fetchNextPageInFlightRef = useRef(false)

  // 统一保护自动加载请求，避免滚动事件和观察器在同一帧重复请求同一页。
  const loadNextPage = useCallback(() => {
    if (
      !hasNextPage ||
      isFetchingNextPage ||
      fetchNextPageInFlightRef.current
    ) {
      return
    }

    fetchNextPageInFlightRef.current = true
    void fetchNextPage().then(
      () => {
        fetchNextPageInFlightRef.current = false
      },
      () => {
        fetchNextPageInFlightRef.current = false
      }
    )
  }, [fetchNextPage, hasNextPage, isFetchingNextPage])

  // 同时监听真实滚动距离和底部哨兵，兼容不同浏览器对自定义滚动根节点的处理。
  useEffect(() => {
    const viewport = scrollAreaRef.current?.querySelector<HTMLElement>(
      '[data-slot="scroll-area-viewport"]'
    )
    const target = loadMoreRef.current
    if (!viewport || !target) {
      return
    }

    const loadWhenNearBottom = () => {
      // 抽屉动画或测试环境尚未完成布局时，零尺寸不能代表已经到达底部。
      if (viewport.clientHeight <= 0 || viewport.scrollHeight <= 0) {
        return
      }
      const distanceToBottom =
        viewport.scrollHeight - viewport.scrollTop - viewport.clientHeight
      if (distanceToBottom <= 240) {
        loadNextPage()
      }
    }

    const observer =
      typeof IntersectionObserver === 'undefined'
        ? null
        : new IntersectionObserver(
            (entries) => {
              if (entries[0]?.isIntersecting) {
                loadNextPage()
              }
            },
            { root: viewport, rootMargin: '240px 0px' }
          )
    const resizeObserver =
      typeof ResizeObserver === 'undefined'
        ? null
        : new ResizeObserver(loadWhenNearBottom)
    observer?.observe(target)
    resizeObserver?.observe(viewport)
    resizeObserver?.observe(target)
    viewport.addEventListener('scroll', loadWhenNearBottom, { passive: true })

    // 页面首批数据不足以撑满抽屉时，立即继续加载，避免必须手动滚动才能触发。
    const frameId = requestAnimationFrame(loadWhenNearBottom)
    return () => {
      cancelAnimationFrame(frameId)
      observer?.disconnect()
      resizeObserver?.disconnect()
      viewport.removeEventListener('scroll', loadWhenNearBottom)
    }
  }, [loadNextPage])

  const rules = query.data?.pages.flatMap((page) => page.items) ?? []
  return (
    <div
      ref={scrollAreaRef}
      className='flex min-h-0 flex-1 flex-col'
      aria-busy={query.isFetching}
      aria-live='polite'
    >
      {query.isPending ? (
        <div className='flex min-h-24 items-center justify-center'>
          <Loader2 className='animate-spin' aria-label={t('Loading')} />
        </div>
      ) : null}
      {query.isError && !query.data ? (
        <OverviewError
          onRetry={() => {
            void query.refetch()
          }}
        />
      ) : null}
      {query.data && rules.length > 0 ? (
        <ScrollArea className='min-h-0 flex-1 pr-2'>
          <DiscountRuleGrid rules={rules} />
          <div
            ref={loadMoreRef}
            data-slot='load-more-sentinel'
            className='flex min-h-8 items-center justify-center'
            aria-hidden='true'
          >
            {isFetchingNextPage ? (
              <Loader2
                className='text-muted-foreground size-4 animate-spin'
                aria-label={t('Loading')}
              />
            ) : null}
          </div>
        </ScrollArea>
      ) : null}
      {query.data && rules.length === 0 ? (
        <p className='text-muted-foreground py-4 text-sm'>
          {t('No models matched your search.')}
        </p>
      ) : null}
    </div>
  )
}

/** 在抽屉中展示单个用户的规则，避免主表出现嵌套明细和第二套分页。 */
export function UserModelPricingDetailsSheet(props: {
  item: UserModelPricingOverviewItem
  keyword: string
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useTranslation()

  return (
    <Sheet open={props.open} onOpenChange={props.onOpenChange}>
      <SheetContent className={sideDrawerContentClassName('sm:max-w-2xl')}>
        <SheetHeader className={sideDrawerHeaderClassName()}>
          <div className='min-w-0 pr-8'>
            <SheetTitle className='flex min-w-0 items-center gap-2'>
              <span
                className='min-w-0 truncate'
                title={props.item.user.username}
              >
                {props.item.user.username}
              </span>
              <TableId
                value={`#${props.item.user.id}`}
                className='shrink-0 text-xs'
              />
            </SheetTitle>
            <SheetDescription className='mt-1'>
              {t('Model discounts')}
            </SheetDescription>
          </div>
          <div className='flex flex-wrap items-center gap-2'>
            <GroupBadge group={props.item.user.group} />
            <UserRole user={props.item.user} />
            <RuleCount count={props.item.rule_count} />
            <DiscountSummary item={props.item} />
          </div>
        </SheetHeader>
        <div
          className={sideDrawerFormClassName('min-h-0 gap-3 overflow-hidden')}
        >
          <DiscountRuleList
            userId={props.item.user.id}
            keyword={props.keyword}
          />
        </div>
      </SheetContent>
    </Sheet>
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
              translate='no'
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

// 用户折扣总览仅展示角色，账号启用状态不属于该页面的筛选和比较维度。
function UserRole({ user }: { user: UserModelPricingOverviewUser }) {
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

function ViewDiscountsButton({
  open,
  username,
  onClick,
}: {
  open: boolean
  username: string
  onClick: () => void
}) {
  const { t } = useTranslation()
  const label = open
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
            aria-expanded={open}
          />
        }
      >
        <List aria-hidden='true' />
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
  selected,
  onOpen,
  onEdit,
}: {
  item: UserModelPricingOverviewItem
  selected: boolean
  onOpen: () => void
  onEdit: () => void
}) {
  return (
    <TableRow
      aria-selected={selected}
      className={cn('[&>td]:py-3', selected && 'bg-muted/40 hover:bg-muted/40')}
    >
      <TableCell className='min-w-[240px]'>
        <UserIdentity user={item.user} />
      </TableCell>
      <TableCell className='min-w-[130px]'>
        <GroupBadge group={item.user.group} />
      </TableCell>
      <TableCell className='min-w-[170px]'>
        <UserRole user={item.user} />
      </TableCell>
      <TableCell className='min-w-[110px]'>
        <RuleCount count={item.rule_count} />
      </TableCell>
      <TableCell className='max-w-[520px] min-w-[300px]'>
        <DiscountModelPreview item={item} />
      </TableCell>
      <TableCell className='w-24 px-2 text-right'>
        <div className='flex items-center justify-end gap-1'>
          <ViewDiscountsButton
            open={selected}
            username={item.user.username}
            onClick={onOpen}
          />
          <EditDiscountsButton username={item.user.username} onClick={onEdit} />
        </div>
      </TableCell>
    </TableRow>
  )
}

export function UserModelPricingMobileRow({
  item,
  selected,
  onOpen,
  onEdit,
}: {
  item: UserModelPricingOverviewItem
  selected: boolean
  onOpen: () => void
  onEdit: () => void
}) {
  return (
    <div
      className={cn(
        '[background-color:var(--data-table-card-bg,var(--table-row))] p-3',
        selected && 'bg-muted/20'
      )}
    >
      <div className='flex min-w-0 items-start gap-2'>
        <div className='min-w-0 flex-1'>
          <UserIdentity user={item.user} />
        </div>
        <div className='flex shrink-0 items-center gap-1'>
          <ViewDiscountsButton
            open={selected}
            username={item.user.username}
            onClick={onOpen}
          />
          <EditDiscountsButton username={item.user.username} onClick={onEdit} />
        </div>
      </div>
      <div className='mt-3 flex flex-wrap items-center gap-2'>
        <GroupBadge group={item.user.group} />
        <UserRole user={item.user} />
        <RuleCount count={item.rule_count} />
        <DiscountModelPreview item={item} />
      </div>
    </div>
  )
}

function OverviewEmpty({ hasSearch }: { hasSearch: boolean }) {
  const { t } = useTranslation()

  return (
    <div className='rounded-lg border p-6'>
      <Empty className='border-none p-0'>
        <EmptyHeader>
          <EmptyMedia variant='icon'>
            <BadgePercent aria-hidden='true' className='size-5' />
          </EmptyMedia>
          <EmptyTitle>{t('No user discounts configured')}</EmptyTitle>
          <EmptyDescription>
            {hasSearch
              ? t('No users or models matched your search.')
              : t('No model discounts have been configured yet.')}
          </EmptyDescription>
        </EmptyHeader>
      </Empty>
    </div>
  )
}

function OverviewMobileList({
  items,
  isLoading,
  isFetching,
  hasSearch,
  selectedUserId,
  onOpen,
  onEdit,
}: {
  items: UserModelPricingOverviewItem[]
  isLoading: boolean
  isFetching: boolean
  hasSearch: boolean
  selectedUserId: number | null
  onOpen: (item: UserModelPricingOverviewItem) => void
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
          selected={selectedUserId === item.user.id}
          onOpen={() => onOpen(item)}
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
      label: t('Configured models'),
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
    <div className='border-destructive/30 bg-destructive/5 flex min-h-56 flex-col items-center justify-center gap-3 rounded-lg border px-4 text-center'>
      <TriangleAlert aria-hidden='true' className='text-destructive size-5' />
      <p className='text-sm font-medium'>
        {t('Failed to load user discounts')}
      </p>
      <Button variant='outline' onClick={onRetry}>
        <RefreshCw aria-hidden='true' />
        {t('Retry')}
      </Button>
    </div>
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

/** 管理端用户折扣总览，负责用户分页、详情抽屉和已有编辑弹窗的联动。 */
export function UserModelPricingOverview() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const isMobile = useMediaQuery('(max-width: 640px)')
  const [selectedUserId, setSelectedUserId] = useState<number | null>(null)
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
    columnFilters: [
      { columnId: 'group', searchKey: 'group', type: 'array' },
      { columnId: 'role', searchKey: 'role', type: 'array' },
    ],
  })

  const groupFilter =
    (columnFilters.find((filter) => filter.id === 'group')?.value as
      | string[]
      | undefined) ?? []
  const roleFilter =
    (columnFilters.find((filter) => filter.id === 'role')?.value as
      | string[]
      | undefined) ?? []

  const { data: groupsData } = useQuery({
    queryKey: ['groups'],
    queryFn: getGroups,
  })
  const groupOptions = useMemo(
    () =>
      (groupsData?.data || []).map((group) => ({
        label: group,
        value: group,
      })),
    [groupsData]
  )

  const query = useQuery({
    queryKey: [
      'user-model-pricing-overview',
      pagination.pageIndex + 1,
      pagination.pageSize,
      globalFilter,
      groupFilter,
      roleFilter,
    ],
    queryFn: async ({ signal }) => {
      const response = await getUserModelPricingOverview(
        {
          keyword: globalFilter,
          group: groupFilter.length > 0 ? groupFilter[0] : undefined,
          role: roleFilter.length > 0 ? roleFilter[0] : undefined,
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
  const hasSearch = Boolean(
    globalFilter?.trim() || groupFilter.length > 0 || roleFilter.length > 0
  )
  const selectedUser =
    items.find((item) => item.user.id === selectedUserId) ?? null

  const openUser = useCallback((item: UserModelPricingOverviewItem) => {
    setSelectedUserId((previous) =>
      previous === item.user.id ? null : item.user.id
    )
  }, [])

  const editUser = useCallback((user: UserModelPricingOverviewUser) => {
    setSelectedUserId(null)
    setEditingUser(user)
  }, [])

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

  // 用户筛选或翻页会改变主列表上下文，先关闭详情抽屉再提交 URL 状态。
  const handleGlobalFilterChange: OnChangeFn<string> = useCallback(
    (updater) => {
      setSelectedUserId(null)
      onGlobalFilterChange?.(updater)
    },
    [onGlobalFilterChange]
  )
  const handlePaginationChange: OnChangeFn<PaginationState> = useCallback(
    (updater) => {
      setSelectedUserId(null)
      onPaginationChange(updater)
    },
    [onPaginationChange]
  )
  const handleColumnFiltersChange: OnChangeFn<ColumnFiltersState> = useCallback(
    (updater) => {
      setSelectedUserId(null)
      onColumnFiltersChange(updater)
    },
    [onColumnFiltersChange]
  )

  const columns = useMemo<ColumnDef<UserModelPricingOverviewItem>[]>(
    () => [
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
        id: 'role',
        accessorFn: (item) => item.user.role,
        header: t('Role'),
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
          (item.preview_rules ?? item.rules)
            .map((rule) => rule.model_name)
            .join(' '),
        header: t('Model discounts'),
        size: 380,
      },
      { id: 'actions', header: t('Actions'), size: 96, enableHiding: false },
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
          selected={selectedUserId === item.user.id}
          onOpen={() => openUser(item)}
          onEdit={() => editUser(item.user)}
        />
      )
    },
    [editUser, openUser, selectedUserId]
  )

  const { table } = useDataTable({
    data: items,
    columns,
    getRowId: (item) => String(item.user.id),
    pagination,
    onPaginationChange: handlePaginationChange,
    manualPagination: true,
    manualFiltering: true,
    columnFilters,
    onColumnFiltersChange: handleColumnFiltersChange,
    globalFilter,
    onGlobalFilterChange: handleGlobalFilterChange,
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
    <>
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
                mobile={
                  <OverviewMobileList
                    items={items}
                    isLoading={query.isLoading}
                    isFetching={query.isFetching}
                    hasSearch={hasSearch}
                    selectedUserId={selectedUserId}
                    onOpen={openUser}
                    onEdit={editUser}
                  />
                }
                toolbarProps={{
                  searchPlaceholder: t('Search users or models...'),
                  searchDebounceMs: 400,
                  hideViewOptions: true,
                  filters: [
                    {
                      columnId: 'group',
                      title: t('Group'),
                      options: groupOptions,
                      singleSelect: true,
                    },
                    {
                      columnId: 'role',
                      title: t('Role'),
                      options: getUserRoleOptions(t),
                      singleSelect: true,
                    },
                  ],
                }}
              />
            </div>
          </div>
        </SectionPageLayout.Content>
      </SectionPageLayout>

      {selectedUser ? (
        <UserModelPricingDetailsSheet
          open
          item={selectedUser}
          keyword={globalFilter ?? ''}
          onOpenChange={(open) => {
            if (!open) setSelectedUserId(null)
          }}
        />
      ) : null}

      {editingUser ? (
        <UserModelPricingDialog
          open
          configuredOnly
          onOpenChange={(open) => {
            if (!open) setEditingUser(null)
          }}
          user={{ id: editingUser.id, username: editingUser.username }}
          onSaved={handleSaved}
        />
      ) : null}
    </>
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
