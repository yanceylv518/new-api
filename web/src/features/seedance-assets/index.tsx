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
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  DATA_TABLE_VIEW_MODES,
  useDataTableViewMode,
} from '@/components/data-table/hooks/use-data-table-view-mode'
import { Main } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { useDebounce } from '@/hooks/use-debounce'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import {
  createSeedanceAssetGroup,
  createSeedanceAssetValidationSession,
  batchDeleteSeedanceAssets,
  deleteSeedanceAsset,
  deleteSeedanceAssetGroup,
  getSeedanceAssetValidationSession,
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
import { CreateHumanValidationDialog } from './components/create-human-validation-dialog'
import { seedanceAssetLayoutClasses } from './layout'
import { updateSeedanceAssetInList } from './lib/cache'
import {
  getSeedanceAssetGroupCategory,
  type SeedanceAssetGroupCategory,
  type SeedanceAssetStatusFilter,
  type SeedanceAssetTypeFilter,
  type SeedanceAssetViewMode,
} from './types'

const SEEDANCE_ASSETS_VIEW_MODE_KEY = 'seedance-assets-view-mode'
const EMPTY_SEEDANCE_ASSETS: SeedanceAsset[] = []

// 私域素材库的查询与变更统一在页面层编排，子组件只负责具体交互和展示。
export function SeedanceAssets() {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const validationSessionStorageKey = userId
    ? `seedance-assets-human-session:${userId}`
    : ''
  const [groupCategory, setGroupCategory] =
    useState<SeedanceAssetGroupCategory>('AIGC')
  const [selectedGroupId, setSelectedGroupId] = useState('')
  const [search, setSearch] = useState('')
  const debouncedSearch = useDebounce(search, 300)
  const [page, setPage] = useState(1)
  const [typeFilter, setTypeFilter] = useState<SeedanceAssetTypeFilter>('all')
  const [statusFilter, setStatusFilter] =
    useState<SeedanceAssetStatusFilter>('all')
  const [editGroup, setEditGroup] = useState<SeedanceAssetGroup | null>(null)
  const [createValidationDialogOpen, setCreateValidationDialogOpen] =
    useState(false)
  const [validationSessionId, setValidationSessionId] = useState<number | null>(
    null
  )
  const [deleteGroup, setDeleteGroup] = useState<SeedanceAssetGroup | null>(
    null
  )
  const [deleteAsset, setDeleteAsset] = useState<SeedanceAsset | null>(null)
  const [deleteSelectedAssets, setDeleteSelectedAssets] = useState(false)
  const [selectedAssetIds, setSelectedAssetIds] = useState<Set<number>>(
    () => new Set()
  )
  const [storedViewMode, setStoredViewMode] = useDataTableViewMode({
    storageKey: SEEDANCE_ASSETS_VIEW_MODE_KEY,
    defaultMode: DATA_TABLE_VIEW_MODES.CARD,
  })

  useEffect(() => {
    if (!validationSessionStorageKey) {
      setValidationSessionId(null)
      return
    }
    try {
      const storedId = Number(
        window.localStorage.getItem(validationSessionStorageKey)
      )
      setValidationSessionId(
        Number.isSafeInteger(storedId) && storedId > 0 ? storedId : null
      )
    } catch {
      setValidationSessionId(null)
    }
  }, [validationSessionStorageKey])

  const groups = useQuery({
    queryKey: ['seedance-asset-groups', userId],
    queryFn: ({ signal }) => listSeedanceAssetGroups(signal),
    refetchInterval: (query) =>
      query.state.data?.data.some((group) => group.status === 'Deleting')
        ? 5000
        : false,
  })
  const validationSession = useQuery({
    queryKey: [
      'seedance-asset-validation-session',
      userId,
      validationSessionId,
    ],
    queryFn: () =>
      getSeedanceAssetValidationSession(validationSessionId as number),
    enabled: validationSessionId !== null,
    refetchInterval: (query) => {
      if (!createValidationDialogOpen) return false
      const status = query.state.data?.data.status
      return status === 'Creating' || status === 'Pending' ? 5000 : false
    },
  })
  const allGroups = groups.data?.data ?? []
  const visibleGroups = allGroups.filter(
    (group) => getSeedanceAssetGroupCategory(group.group_type) === groupCategory
  )
  const selectedGroup =
    visibleGroups.find((group) => group.group_id === selectedGroupId) ??
    visibleGroups[0]
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

  // 分组和筛选改变后清空选择；分页切换保留已选 ID，支持跨页批量操作。
  useEffect(() => {
    setSelectedAssetIds(new Set())
  }, [selectedId, debouncedSearch, typeFilter, statusFilter])

  const clearAssetSelection = () => setSelectedAssetIds(new Set())

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
      setGroupCategory('AIGC')
      setSelectedGroupId(response.data.group_id)
      setPage(1)
      void invalidate()
      toast.success(t('Created successfully'))
    },
    // mutation 的后续参数是业务变量和上下文，不能传给错误处理器作为提示文案。
    onError: (error) => handleServerError(error),
  })
  const createValidationMutation = useMutation({
    mutationFn: (details: {
      name: string
      description: string
      tags: string
    }) => createSeedanceAssetValidationSession(details),
    onSuccess: (response) => {
      setValidationSessionId(response.data.id)
      if (validationSessionStorageKey) {
        try {
          window.localStorage.setItem(
            validationSessionStorageKey,
            String(response.data.id)
          )
        } catch {
          // 服务端仍保存会话状态，本页内可继续完成流程。
        }
      }
      toast.success(t('Verification session created'))
    },
    onError: (error) => handleServerError(error),
  })
  const updateGroupMutation = useMutation({
    mutationFn: (payload: {
      id: number
      name: string
      description: string
      tags: string
    }) => updateSeedanceAssetGroup(payload.id, payload),
    onSuccess: (response) => {
      setSelectedGroupId(response.data.group_id)
      setEditGroup(null)
      void invalidate()
      toast.success(t('Saved successfully'))
    },
    onError: (error) => handleServerError(error),
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
    onError: (error) => handleServerError(error),
  })
  const deleteAssetMutation = useMutation({
    mutationFn: deleteSeedanceAsset,
    onSuccess: (_response, assetId) => {
      setSelectedAssetIds((current) => {
        if (!current.has(assetId)) return current
        const next = new Set(current)
        next.delete(assetId)
        return next
      })
      setDeleteAsset(null)
      void invalidate()
      toast.success(t('Deleted successfully'))
    },
    onError: (error) => {
      void invalidate()
      handleServerError(error)
    },
  })
  const batchDeleteAssetMutation = useMutation({
    mutationFn: batchDeleteSeedanceAssets,
    onSuccess: async (response) => {
      setDeleteSelectedAssets(false)
      setSelectedAssetIds(new Set(response.data.failed_ids))
      await invalidate()
      if (response.data.failed_ids.length > 0) {
        toast.warning(
          t('Deleted {{deleted}} assets; {{failed}} failed', {
            deleted: response.data.deleted_ids.length,
            failed: response.data.failed_ids.length,
          })
        )
      } else if (response.data.pending_ids.length > 0) {
        toast.success(
          t('Queued {{count}} assets for deletion', {
            count: response.data.pending_ids.length,
          })
        )
      } else {
        toast.success(
          t('Deleted {{count}} assets', {
            count: response.data.deleted_ids.length,
          })
        )
      }
    },
    onError: (error) => {
      setDeleteSelectedAssets(false)
      void invalidate()
      handleServerError(error)
    },
  })

  const validationStatus = validationSession.data?.data.status
  useEffect(() => {
    if (validationStatus === 'Succeeded') {
      setGroupCategory('LivenessFace')
      if (validationSession.data?.data.group_id) {
        setSelectedGroupId(validationSession.data.data.group_id)
      }
      void queryClient.invalidateQueries({
        queryKey: ['seedance-asset-groups', userId],
      })
      toast.success(
        t('Identity verified. Upload a matching portrait to finish setup.')
      )
    }
  }, [
    queryClient,
    t,
    userId,
    validationSession.data?.data.group_id,
    validationStatus,
  ])

  const allAssets = assets.data?.data ?? EMPTY_SEEDANCE_ASSETS
  // 服务端在完整素材集合上执行搜索和筛选，页面只渲染当前页。
  const rows = allAssets

  const viewMode: SeedanceAssetViewMode =
    storedViewMode === 'card' ? 'grid' : 'list'
  const setViewMode = (mode: SeedanceAssetViewMode) => {
    setStoredViewMode(mode === 'grid' ? 'card' : 'table')
  }
  const resumableSession = validationSession.data?.data
  let resumeMessage = t('Character setup is unfinished')
  let resumeAction = t('Continue verification')
  if (resumableSession?.status === 'Succeeded') {
    resumeMessage = t('Portrait upload is still needed')
    resumeAction = t('Continue portrait upload')
  } else if (
    resumableSession?.status === 'Failed' ||
    resumableSession?.status === 'Expired'
  ) {
    resumeAction = t('Review verification status')
  }

  const finishHumanPortraitUpload = async (asset: SeedanceAsset) => {
    setGroupCategory('LivenessFace')
    setSelectedGroupId(asset.group_id)
    setPage(1)
    clearAssetSelection()
    await invalidate()
    if (validationSessionStorageKey) {
      try {
        window.localStorage.removeItem(validationSessionStorageKey)
      } catch {
        // 会话已完成，存储异常不应阻止素材展示。
      }
    }
    setValidationSessionId(null)
    setCreateValidationDialogOpen(false)
    toast.success(t('Portrait uploaded for review'))
  }

  const clearValidationSession = () => {
    if (validationSessionStorageKey) {
      try {
        window.localStorage.removeItem(validationSessionStorageKey)
      } catch {
        // 存储不可用时仍清除页面内的会话状态。
      }
    }
    setValidationSessionId(null)
    queryClient.removeQueries({
      queryKey: ['seedance-asset-validation-session', userId],
    })
    createValidationMutation.reset()
  }

  const handleGroupCategoryChange = (category: SeedanceAssetGroupCategory) => {
    setGroupCategory(category)
    const nextGroup = allGroups.find(
      (group) => getSeedanceAssetGroupCategory(group.group_type) === category
    )
    setSelectedGroupId(nextGroup?.group_id ?? '')
    setSearch('')
    setTypeFilter('all')
    setStatusFilter('all')
    setPage(1)
    clearAssetSelection()
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

        {!createValidationDialogOpen && resumableSession ? (
          <div
            role='status'
            className='flex flex-wrap items-center justify-between gap-3 border-b px-5 py-3 sm:px-6'
          >
            <div className='min-w-0'>
              <p className='text-sm font-medium'>{resumeMessage}</p>
              <p className='text-muted-foreground truncate text-xs'>
                {resumableSession.name}
              </p>
            </div>
            <Button
              type='button'
              variant='outline'
              onClick={() => setCreateValidationDialogOpen(true)}
            >
              {resumeAction}
            </Button>
          </div>
        ) : null}

        <div className={seedanceAssetLayoutClasses.content}>
          <AssetGroupSidebar
            groups={allGroups}
            groupCategory={groupCategory}
            selectedId={selectedId}
            isLoading={groups.isPending}
            isError={groups.isError}
            isCreating={createGroupMutation.isPending}
            onRetry={() => void groups.refetch()}
            onGroupCategoryChange={handleGroupCategoryChange}
            onSelect={(group) => {
              clearAssetSelection()
              setGroupCategory(getSeedanceAssetGroupCategory(group.group_type))
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
            onCreateVerified={() => {
              setCreateValidationDialogOpen(true)
            }}
            onEdit={setEditGroup}
            onDelete={setDeleteGroup}
          />

          <div className={seedanceAssetLayoutClasses.workspace}>
            <AssetUploadPanel
              groupId={canUpload ? selectedId : ''}
              groupName={selectedGroup?.name}
              onUploaded={(asset) => {
                clearAssetSelection()
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
                  onPageChange={(nextPage) => {
                    setPage(nextPage)
                  }}
                  isLoading={Boolean(selectedId) && assets.isPending}
                  isError={Boolean(selectedId) && assets.isError}
                  isFetching={assets.isFetching}
                  search={search}
                  typeFilter={typeFilter}
                  statusFilter={statusFilter}
                  viewMode={viewMode}
                  onSearchChange={(value) => {
                    clearAssetSelection()
                    setSearch(value)
                    setPage(1)
                  }}
                  onTypeFilterChange={(value) => {
                    clearAssetSelection()
                    setTypeFilter(value)
                    setPage(1)
                  }}
                  onStatusFilterChange={(value) => {
                    clearAssetSelection()
                    setStatusFilter(value)
                    setPage(1)
                  }}
                  onViewModeChange={setViewMode}
                  onRetry={() => void assets.refetch()}
                  onRefresh={(asset) => refreshAssetMutation.mutate(asset)}
                  onDelete={setDeleteAsset}
                  selectedIds={selectedAssetIds}
                  onSelectionChange={(asset, selected) => {
                    setSelectedAssetIds((current) => {
                      const next = new Set(current)
                      if (selected) next.add(asset.id)
                      else next.delete(asset.id)
                      return next
                    })
                  }}
                  onSelectAll={(selected) => {
                    setSelectedAssetIds((current) => {
                      const next = new Set(current)
                      for (const asset of rows) {
                        if (selected) next.add(asset.id)
                        else next.delete(asset.id)
                      }
                      return next
                    })
                  }}
                  onClearSelection={clearAssetSelection}
                  onBatchDelete={() => setDeleteSelectedAssets(true)}
                  isBatchDeleting={batchDeleteAssetMutation.isPending}
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

      <CreateHumanValidationDialog
        open={createValidationDialogOpen}
        isPending={createValidationMutation.isPending}
        isChecking={validationSession.isFetching}
        session={validationSession.data?.data ?? null}
        onOpenChange={(open) => {
          setCreateValidationDialogOpen(open)
        }}
        onSubmit={(details) => createValidationMutation.mutate(details)}
        onCheckStatus={() => void validationSession.refetch()}
        onStartOver={clearValidationSession}
        onPortraitUploaded={finishHumanPortraitUpload}
      />
      <EditAssetGroupDialog
        group={editGroup}
        open={Boolean(editGroup)}
        isPending={updateGroupMutation.isPending}
        onOpenChange={(open) => {
          if (!open) setEditGroup(null)
        }}
        onSubmit={(details) => {
          if (editGroup) {
            updateGroupMutation.mutate({ id: editGroup.id, ...details })
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
      <ConfirmDialog
        open={deleteSelectedAssets}
        onOpenChange={(open) => {
          if (!batchDeleteAssetMutation.isPending) setDeleteSelectedAssets(open)
        }}
        title={t('Delete selected assets')}
        desc={t(
          'Delete {{count}} selected assets from this group? This action cannot be undone.',
          { count: selectedAssetIds.size }
        )}
        destructive
        isLoading={batchDeleteAssetMutation.isPending}
        confirmText={t('Delete')}
        handleConfirm={() => {
          if (selectedAssetIds.size > 0) {
            batchDeleteAssetMutation.mutate([...selectedAssetIds])
          }
        }}
      />
    </Main>
  )
}
