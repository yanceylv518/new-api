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
import { RefreshCw, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardFooter } from '@/components/ui/card'

import type { SeedanceAsset } from '../api'
import { getSeedanceAssetTypeLabel } from '../lib/upload'
import { AssetPreview } from './asset-preview'
import { AssetStatusBadge } from './asset-status-badge'

type AssetItemProps = {
  asset: SeedanceAsset
  mode: 'grid' | 'list'
  onRefresh: (asset: SeedanceAsset) => void
  onDelete: (asset: SeedanceAsset) => void
  isRefreshing?: boolean
  isDeleting?: boolean
}

// 操作按钮在列表和卡片视图共用，避免不同视图产生不一致的行为。
function AssetActions(props: AssetItemProps) {
  const { t } = useTranslation()
  return (
    <div className='flex shrink-0 items-center gap-1'>
      <CopyButton
        value={props.asset.asset_id}
        size='icon-sm'
        tooltip={t('Copy to clipboard')}
      />
      <Button
        size='icon-sm'
        variant='ghost'
        title={t('Refresh')}
        aria-label={t('Refresh')}
        disabled={
          props.isRefreshing ||
          props.isDeleting ||
          props.asset.status === 'Deleting'
        }
        onClick={() => props.onRefresh(props.asset)}
      >
        <RefreshCw className={props.isRefreshing ? 'animate-spin' : ''} />
      </Button>
      <Button
        size='icon-sm'
        variant='ghost'
        title={t('Delete')}
        aria-label={t('Delete')}
        disabled={props.isDeleting || props.isRefreshing}
        onClick={() => props.onDelete(props.asset)}
      >
        <Trash2 />
      </Button>
    </div>
  )
}

// 素材项根据视图模式复用相同的预览、状态和操作语义。
export function AssetItem(props: AssetItemProps) {
  const { t } = useTranslation()
  const typeLabel = t(getSeedanceAssetTypeLabel(props.asset.asset_type))

  if (props.mode === 'list') {
    return (
      <article className='group flex min-w-0 items-center gap-3 border-b py-3 last:border-b-0 sm:gap-4'>
        <div className='bg-muted flex aspect-video size-16 shrink-0 items-center justify-center overflow-hidden rounded-lg sm:size-20'>
          <AssetPreview
            asset={props.asset}
            className='h-full w-full object-cover'
            showAudioControls={false}
          />
        </div>
        <div className='min-w-0 flex-1'>
          <div className='flex min-w-0 flex-wrap items-center gap-2'>
            <h3
              className='min-w-0 truncate text-sm font-medium'
              title={props.asset.name || props.asset.asset_id}
            >
              {props.asset.name || props.asset.asset_id}
            </h3>
            <StatusBadge variant='neutral' copyable={false}>
              {typeLabel}
            </StatusBadge>
          </div>
          <div className='mt-1 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1'>
            <p
              className='text-muted-foreground max-w-full min-w-0 truncate font-mono text-xs'
              title={props.asset.asset_id}
            >
              {props.asset.asset_id}
            </p>
            <AssetStatusBadge asset={props.asset} />
          </div>
        </div>
        <AssetActions {...props} />
      </article>
    )
  }

  return (
    <Card className='min-w-0 gap-0 py-0'>
      <div className='bg-muted aspect-video w-full overflow-hidden'>
        <AssetPreview
          asset={props.asset}
          className='h-full w-full object-cover'
          showAudioControls
        />
      </div>
      <CardContent className='min-w-0 space-y-3 p-4'>
        <div className='flex min-w-0 items-start justify-between gap-3'>
          <div className='min-w-0'>
            <h3
              className='truncate text-sm font-medium'
              title={props.asset.name || props.asset.asset_id}
            >
              {props.asset.name || props.asset.asset_id}
            </h3>
            <p
              className='text-muted-foreground mt-1 truncate font-mono text-xs'
              title={props.asset.asset_id}
            >
              {props.asset.asset_id}
            </p>
          </div>
          <AssetStatusBadge asset={props.asset} />
        </div>
        <div className='text-muted-foreground flex items-center gap-2 text-xs'>
          <span className='truncate'>{typeLabel}</span>
          {props.asset.preview_url ? <span aria-hidden='true'>·</span> : null}
          {props.asset.preview_url ? (
            <span>{t('Preview available')}</span>
          ) : null}
        </div>
      </CardContent>
      <CardFooter className='justify-end gap-1 px-4 py-3'>
        <AssetActions {...props} />
      </CardFooter>
    </Card>
  )
}
