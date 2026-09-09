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
  Archive,
  ChevronLeft,
  ChevronRight,
  CloudUpload,
  FolderOpen,
  Loader2,
  RefreshCw,
  Search,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { DataTableViewModeToggle } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import type { SeedanceAsset } from '../api'
import { seedanceAssetLayoutClasses } from '../layout'
import type {
  SeedanceAssetStatusFilter,
  SeedanceAssetTypeFilter,
  SeedanceAssetViewMode,
} from '../types'
import { AssetItem } from './asset-item'

// 素材库把素材组和操作合并为固定头部，素材集合独立滚动。
export function AssetLibraryView(props: {
  groupName?: string
  rows: SeedanceAsset[]
  hasGroup: boolean
  canUpload: boolean
  hasAssets: boolean
  isLoading: boolean
  isError: boolean
  isFetching: boolean
  page: number
  total: number
  pageSize: number
  onPageChange: (page: number) => void
  search: string
  typeFilter: SeedanceAssetTypeFilter
  statusFilter: SeedanceAssetStatusFilter
  viewMode: SeedanceAssetViewMode
  onSearchChange: (value: string) => void
  onTypeFilterChange: (value: SeedanceAssetTypeFilter) => void
  onStatusFilterChange: (value: SeedanceAssetStatusFilter) => void
  onViewModeChange: (value: SeedanceAssetViewMode) => void
  onRetry: () => void
  onBrowse: () => void
  onRefresh: (asset: SeedanceAsset) => void
  onDelete: (asset: SeedanceAsset) => void
  refreshingId?: number
  deletingId?: number
}) {
  const { t } = useTranslation()
  const totalPages = Math.max(1, Math.ceil(props.total / props.pageSize))
  const hasFilters = Boolean(
    props.search.trim() ||
    props.typeFilter !== 'all' ||
    props.statusFilter !== 'all'
  )
  let emptyTitle = 'Drop your first asset'
  let emptyDescription = 'Drop media here or browse'
  if (!props.hasGroup) {
    emptyTitle = 'Select an asset group'
    emptyDescription = 'Select an asset group first'
  } else if (hasFilters) {
    emptyTitle = 'No matching assets'
    emptyDescription = 'Try a different search or filter.'
  } else if (props.hasAssets) {
    emptyTitle = 'No assets in this group'
  }

  return (
    <section
      className={seedanceAssetLayoutClasses.library}
      aria-label={t('Assets')}
    >
      <header className={seedanceAssetLayoutClasses.header}>
        <div className='flex min-w-0 flex-col gap-3 xl:flex-row xl:items-center'>
          <div className='flex min-w-0 items-center gap-2 xl:w-64 xl:shrink-0'>
            <FolderOpen
              className='text-primary size-5 shrink-0'
              aria-hidden='true'
            />
            <h2 className='min-w-0 truncate text-sm font-semibold'>
              {props.groupName ?? t('Select an asset group')}
            </h2>
          </div>

          <div className='flex min-w-0 flex-1 flex-wrap items-center gap-2'>
            <div className='relative w-full min-w-0 sm:w-auto sm:flex-1'>
              <Search
                className='text-muted-foreground pointer-events-none absolute top-2.5 left-3 size-4'
                aria-hidden='true'
              />
              <Input
                className='pl-9'
                aria-label={t('Search assets')}
                placeholder={t('Search assets')}
                value={props.search}
                onChange={(event) => props.onSearchChange(event.target.value)}
              />
            </div>
            <Select
              value={props.typeFilter}
              onValueChange={(value) =>
                props.onTypeFilterChange(value as SeedanceAssetTypeFilter)
              }
            >
              <SelectTrigger aria-label={t('Filter by type')} className='w-32'>
                <SelectValue>
                  {t(
                    props.typeFilter === 'all' ? 'All types' : props.typeFilter
                  )}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='all'>{t('All types')}</SelectItem>
                <SelectItem value='Image'>{t('Image')}</SelectItem>
                <SelectItem value='Video'>{t('Video')}</SelectItem>
                <SelectItem value='Audio'>{t('Audio')}</SelectItem>
              </SelectContent>
            </Select>
            <Select
              value={props.statusFilter}
              onValueChange={(value) =>
                props.onStatusFilterChange(value as SeedanceAssetStatusFilter)
              }
            >
              <SelectTrigger
                aria-label={t('Filter by status')}
                className='w-36'
              >
                <SelectValue>
                  {t(
                    props.statusFilter === 'all'
                      ? 'All statuses'
                      : props.statusFilter
                  )}
                </SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectItem value='all'>{t('All statuses')}</SelectItem>
                <SelectItem value='Processing'>{t('Processing')}</SelectItem>
                <SelectItem value='Active'>{t('Active')}</SelectItem>
                <SelectItem value='Failed'>{t('Failed')}</SelectItem>
              </SelectContent>
            </Select>
            <Button
              type='button'
              size='icon'
              variant='outline'
              title={t('Refresh')}
              aria-label={t('Refresh')}
              disabled={props.isFetching}
              onClick={props.onRetry}
            >
              <RefreshCw className={props.isFetching ? 'animate-spin' : ''} />
            </Button>
            <Button
              type='button'
              size='icon'
              variant='outline'
              title={t('Upload assets')}
              aria-label={t('Upload assets')}
              disabled={!props.canUpload}
              onClick={props.onBrowse}
            >
              <CloudUpload aria-hidden='true' />
            </Button>
            <DataTableViewModeToggle
              value={props.viewMode === 'grid' ? 'card' : 'table'}
              onChange={(mode) =>
                props.onViewModeChange(mode === 'card' ? 'grid' : 'list')
              }
            />
          </div>
        </div>
      </header>

      <div className={seedanceAssetLayoutClasses.assetScroll}>
        <div className='flex items-center justify-between gap-3 py-4'>
          <span className='text-muted-foreground text-xs'>
            {t('{{count}} assets', { count: props.total })}
          </span>
          {props.isFetching && !props.isLoading ? (
            <span
              role='status'
              className='text-muted-foreground flex items-center gap-2 text-xs'
            >
              <Loader2 className='size-3.5 animate-spin' />
              {t('Updating')}
            </span>
          ) : null}
        </div>

        {props.isLoading ? (
          <div role='status'>
            <LoadingState className='min-h-56' message={t('Loading')} />
          </div>
        ) : null}
        {props.isError ? (
          <ErrorState
            className='min-h-56'
            title={t('Failed to load assets')}
            onRetry={props.onRetry}
          />
        ) : null}
        {!props.isLoading && !props.isError && props.rows.length === 0 ? (
          <EmptyState
            className='min-h-56 border-dashed'
            bordered
            icon={Archive}
            title={t(emptyTitle)}
            description={t(emptyDescription)}
            action={
              props.canUpload ? (
                <Button
                  type='button'
                  variant='outline'
                  onClick={props.onBrowse}
                >
                  <CloudUpload aria-hidden='true' />
                  {t('Upload assets')}
                </Button>
              ) : null
            }
          />
        ) : null}
        {!props.isLoading &&
        !props.isError &&
        props.rows.length > 0 &&
        props.viewMode === 'grid' ? (
          <div className='grid min-w-0 grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3'>
            {props.rows.map((asset) => (
              <AssetItem
                key={asset.id}
                asset={asset}
                mode='grid'
                onRefresh={props.onRefresh}
                onDelete={props.onDelete}
                isRefreshing={props.refreshingId === asset.id}
                isDeleting={props.deletingId === asset.id}
              />
            ))}
          </div>
        ) : null}
        {!props.isLoading &&
        !props.isError &&
        props.rows.length > 0 &&
        props.viewMode === 'list' ? (
          <div className='min-w-0'>
            {props.rows.map((asset) => (
              <AssetItem
                key={asset.id}
                asset={asset}
                mode='list'
                onRefresh={props.onRefresh}
                onDelete={props.onDelete}
                isRefreshing={props.refreshingId === asset.id}
                isDeleting={props.deletingId === asset.id}
              />
            ))}
          </div>
        ) : null}
      </div>
      {/* 分页固定在素材滚动区之外，长列表不会把导航按钮挤出可视区域。 */}
      {props.hasGroup && totalPages > 1 ? (
        <nav
          className='flex shrink-0 items-center justify-between gap-3 border-t px-4 py-2 sm:px-6'
          aria-label={t('Assets')}
        >
          <span className='text-muted-foreground text-xs'>
            {t('Page {{current}} of {{total}}', {
              current: props.page,
              total: totalPages,
            })}
          </span>
          <div className='flex items-center gap-1'>
            <Button
              variant='outline'
              size='icon-sm'
              title={t('Previous page')}
              aria-label={t('Previous page')}
              disabled={props.isFetching || props.page <= 1}
              onClick={() => props.onPageChange(props.page - 1)}
            >
              <ChevronLeft />
            </Button>
            <Button
              variant='outline'
              size='icon-sm'
              title={t('Next page')}
              aria-label={t('Next page')}
              disabled={props.isFetching || props.page >= totalPages}
              onClick={() => props.onPageChange(props.page + 1)}
            >
              <ChevronRight />
            </Button>
          </div>
        </nav>
      ) : null}
    </section>
  )
}
