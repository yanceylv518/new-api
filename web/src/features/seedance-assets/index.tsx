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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Archive } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  DATA_TABLE_VIEW_MODES,
  useDataTableViewMode,
} from '@/components/data-table/hooks/use-data-table-view-mode'
import { Main } from '@/components/layout'
import { useDebounce } from '@/hooks/use-debounce'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import {
  createSeedanceAssetGroup,
  deleteSeedanceAsset,
  deleteSeedanceAssetGroup,
  listSeedanceAssetGroups,
  listSeedanceAssets,
  refreshSeedanceAsset,
  updateSeedanceAssetGroup,
  type SeedanceAssetListResponse,
  type SeedanceAsset,
  type SeedanceAssetGroup,
} from './api'
import {
  DeleteAssetGroupDialog,
  EditAssetGroupDialog,
} from './components/asset-group-dialogs'
import { AssetGroupSidebar } from './components/asset-group-sidebar'
import { AssetLibraryView } from './components/asset-library-view'
import { AssetUploadPanel } from './components/asset-upload-panel'
import { seedanceAssetLayoutClasses } from './layout'
import { updateSeedanceAssetInList } from './lib/cache'
import type {
  SeedanceAssetStatusFilter,
  SeedanceAssetTypeFilter,
  SeedanceAssetViewMode,
} from './types'

const SEEDANCE_ASSETS_VIEW_MODE_KEY = 'seedance-assets-view-mode'
const EMPTY_SEEDANCE_ASSETS: SeedanceAsset[] = []

// 私域素材库的查询与变更统一在页面层编排，子组件只负责具体交互和展示。
export function SeedanceAssets() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const [selectedGroupId, setSelectedGroupId] = useState('')
  const [search, setSearch] = useState('')
  const debouncedSearch = useDebounce(search, 300)
  const [page, setPage] = useState(1)
  const [typeFilter, setTypeFilter] = useState<SeedanceAssetTypeFilter>('all')
  const [statusFilter, setStatusFilter] =
    useState<SeedanceAssetStatusFilter>('all')
  const [editGroup, setEditGroup] = useState<SeedanceAssetGroup | null>(null)
  const [deleteGroup, setDeleteGroup] = useState<SeedanceAssetGroup | null>(
    null
  )
  const [deleteAsset, setDeleteAsset] = useState<SeedanceAsset | null>(null)
  const [storedViewMode, setStoredViewMode] = useDataTableViewMode({
    storageKey: SEEDANCE_ASSETS_VIEW_MODE_KEY,
    defaultMode: DATA_TABLE_VIEW_MODES.CARD,
  })

  const groups = useQuery({
    queryKey: ['seedance-asset-groups', userId],
    queryFn: listSeedanceAssetGroups,
  })
  const selectedGroup =
    groups.data?.data.find((group) => group.group_id === selectedGroupId) ??
    groups.data?.data[0]
  const selectedId = selectedGroup?.group_id ?? ''
  const canUpload = Boolean(selectedId) && selectedGroup?.status !== 'Deleting'
  const filters = {
    p: page,
    page_size: 24,
    search: debouncedSearch,
    asset_type: typeFilter,
    status: statusFilter,
  }
  const assets = useQuery({
    queryKey: ['seedance-assets', userId, selectedId, filters],
    queryFn: ({ signal }) => listSeedanceAssets(selectedId, signal, filters),
    enabled: Boolean(selectedId),
    // React Query 管理计时器与卸载取消；全部进入终态后停止轮询。
    refetchInterval: (query) => (query.state.data?.has_pending ? 5000 : false),
  })

  // 先把变更接口返回的完整素材写入缓存，再重新请求列表，避免刷新结果被下一次渲染吞掉。
  const updateAssetCache = (updatedAsset: SeedanceAsset) => {
    queryClient.setQueriesData<SeedanceAssetListResponse>(
      { queryKey: ['seedance-assets', userId, updatedAsset.group_id] },
      (current) => updateSeedanceAssetInList(current, updatedAsset)
    )
  }

  const invalidate = async () => {
    // 先取消旧列表请求，避免旧响应在变更成功后覆盖最新缓存。
    await Promise.all([
      queryClient.cancelQueries({
        queryKey: ['seedance-asset-groups', userId],
      }),
      queryClient.cancelQueries({
        queryKey: ['seedance-assets', userId],
      }),
    ])
    await Promise.all([
      queryClient.invalidateQueries({
        queryKey: ['seedance-asset-groups', userId],
      }),
      queryClient.invalidateQueries({
        queryKey: ['seedance-assets', userId],
      }),
    ])
  }

  const createGroupMutation = useMutation({
    mutationFn: createSeedanceAssetGroup,
    onSuccess: (response) => {
      setSelectedGroupId(response.data.group_id)
      setPage(1)
      void invalidate()
      toast.success(t('Created successfully'))
    },
    onError: handleServerError,
  })
  const updateGroupMutation = useMutation({
    mutationFn: (payload: { id: number; name: string }) =>
      updateSeedanceAssetGroup(payload.id, payload.name),
    onSuccess: (response) => {
      setSelectedGroupId(response.data.group_id)
      setEditGroup(null)
      void invalidate()
      toast.success(t('Saved successfully'))
    },
    onError: handleServerError,
  })
  const deleteGroupMutation = useMutation({
    mutationFn: (group: SeedanceAssetGroup) =>
      deleteSeedanceAssetGroup(group.id),
    onSuccess: (_response, group) => {
      if (selectedId === group.group_id) setSelectedGroupId('')
      setPage(1)
      setDeleteGroup(null)
      void invalidate()
      toast.success(t('Deleted successfully'))
    },
    onError: (error) => {
      void invalidate()
      handleServerError(error)
    },
  })
  const refreshAssetMutation = useMutation({
    mutationFn: (asset: SeedanceAsset) => refreshSeedanceAsset(asset.id),
    onMutate: async (asset) => {
      await queryClient.cancelQueries({
        queryKey: ['seedance-assets', userId, asset.group_id],
      })
    },
    onSuccess: async (response) => {
      // 上游刷新期间可能启动新的定时列表请求，写回前再次取消旧快照。
      await queryClient.cancelQueries({
        queryKey: ['seedance-assets', userId, response.data.group_id],
      })
      updateAssetCache(response.data)
    },
    onError: handleServerError,
  })
  const deleteAssetMutation = useMutation({
    mutationFn: deleteSeedanceAsset,
    onSuccess: () => {
      setDeleteAsset(null)
      void invalidate()
      toast.success(t('Deleted successfully'))
    },
    onError: (error) => {
      void invalidate()
      handleServerError(error)
    },
  })

  const allAssets = assets.data?.data ?? EMPTY_SEEDANCE_ASSETS
  // 服务端在完整素材集合上执行搜索和筛选，页面只渲染当前页。
  const rows = allAssets

  const viewMode: SeedanceAssetViewMode =
    storedViewMode === 'card' ? 'grid' : 'list'
  const setViewMode = (mode: SeedanceAssetViewMode) => {
    setStoredViewMode(mode === 'grid' ? 'card' : 'table')
  }

  return (
    <Main>
      <div className={seedanceAssetLayoutClasses.page}>
        <header className='flex flex-wrap items-center justify-between gap-3 border-b px-5 py-4 sm:px-6'>
          <div className='flex min-w-0 items-center gap-3'>
            <span className='bg-primary/10 text-primary flex size-9 shrink-0 items-center justify-center rounded-lg'>
              <Archive className='size-5' aria-hidden='true' />
            </span>
            <div className='min-w-0'>
              <h1 className='truncate text-lg font-semibold'>
                {t('Private asset library')}
              </h1>
              <p className='text-muted-foreground truncate text-xs'>
                {t('Seedance media workspace')}
              </p>
            </div>
          </div>
          <span className='text-muted-foreground text-xs'>Seedance</span>
        </header>

        <div className={seedanceAssetLayoutClasses.content}>
          <AssetGroupSidebar
            groups={groups.data?.data ?? []}
            selectedId={selectedId}
            isLoading={groups.isPending}
            isError={groups.isError}
            isCreating={createGroupMutation.isPending}
            onRetry={() => void groups.refetch()}
            onSelect={(group) => {
              setSelectedGroupId(group.group_id)
              setSearch('')
              setTypeFilter('all')
              setStatusFilter('all')
              setPage(1)
            }}
            onCreate={async (name) => {
              try {
                await createGroupMutation.mutateAsync(name)
                return true
              } catch {
                return false
              }
            }}
            onEdit={setEditGroup}
            onDelete={setDeleteGroup}
          />

          <div className={seedanceAssetLayoutClasses.workspace}>
            <AssetUploadPanel
              key={selectedId}
              groupId={canUpload ? selectedId : ''}
              onUploaded={(asset) => {
                setPage(1)
                void queryClient
                  .cancelQueries({
                    queryKey: ['seedance-assets', userId, asset.group_id],
                  })
                  .then(() => {
                    return queryClient.invalidateQueries({
                      queryKey: ['seedance-assets', userId, asset.group_id],
                    })
                  })
              }}
            >
              {(browse) => (
                <AssetLibraryView
                  groupName={selectedGroup?.name}
                  onBrowse={browse}
                  rows={rows}
                  hasGroup={Boolean(selectedId)}
                  canUpload={canUpload}
                  hasAssets={allAssets.length > 0}
                  page={assets.data?.page ?? page}
                  total={assets.data?.total ?? 0}
                  pageSize={24}
                  onPageChange={setPage}
                  isLoading={Boolean(selectedId) && assets.isPending}
                  isError={Boolean(selectedId) && assets.isError}
                  isFetching={assets.isFetching}
                  search={search}
                  typeFilter={typeFilter}
                  statusFilter={statusFilter}
                  viewMode={viewMode}
                  onSearchChange={(value) => {
                    setSearch(value)
                    setPage(1)
                  }}
                  onTypeFilterChange={(value) => {
                    setTypeFilter(value)
                    setPage(1)
                  }}
                  onStatusFilterChange={(value) => {
                    setStatusFilter(value)
                    setPage(1)
                  }}
                  onViewModeChange={setViewMode}
                  onRetry={() => void assets.refetch()}
                  onRefresh={(asset) => refreshAssetMutation.mutate(asset)}
                  onDelete={setDeleteAsset}
                  refreshingId={
                    refreshAssetMutation.isPending &&
                    refreshAssetMutation.variables
                      ? refreshAssetMutation.variables.id
                      : undefined
                  }
                  deletingId={
                    deleteAssetMutation.isPending &&
                    typeof deleteAssetMutation.variables === 'number'
                      ? deleteAssetMutation.variables
                      : undefined
                  }
                />
              )}
            </AssetUploadPanel>
          </div>
        </div>
      </div>

      <EditAssetGroupDialog
        group={editGroup}
        open={Boolean(editGroup)}
        isPending={updateGroupMutation.isPending}
        onOpenChange={(open) => {
          if (!open) setEditGroup(null)
        }}
        onSubmit={(name) => {
          if (editGroup) {
            updateGroupMutation.mutate({ id: editGroup.id, name })
          }
        }}
      />
      <DeleteAssetGroupDialog
        group={deleteGroup}
        open={Boolean(deleteGroup)}
        isPending={deleteGroupMutation.isPending}
        onOpenChange={(open) => {
          if (!open) setDeleteGroup(null)
        }}
        onConfirm={() => {
          if (deleteGroup) deleteGroupMutation.mutate(deleteGroup)
        }}
      />
      <ConfirmDialog
        open={Boolean(deleteAsset)}
        onOpenChange={(open) => {
          if (!open) setDeleteAsset(null)
        }}
        title={t('Delete asset')}
        desc={t(
          'Delete "{{name}}" from this group? This action cannot be undone.',
          {
            name: deleteAsset?.name ?? deleteAsset?.asset_id ?? '',
          }
        )}
        destructive
        isLoading={deleteAssetMutation.isPending}
        confirmText={t('Delete')}
        handleConfirm={() => {
          if (deleteAsset) deleteAssetMutation.mutate(deleteAsset.id)
        }}
      />
    </Main>
  )
}
