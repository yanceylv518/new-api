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
import { File, Film, Image, Music2 } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import type { SeedanceAsset } from '../api'
import { retainSeedanceAssetPreview } from '../lib/preview'
import { getSeedanceAssetTypeLabel } from '../lib/upload'

export function AssetPreview(props: {
  asset: SeedanceAsset
  className?: string
  showAudioControls?: boolean
}) {
  const { t } = useTranslation()
  const assetType = props.asset.asset_type.toLowerCase()
  const previewURL = props.asset.preview_url?.trim() ?? ''
  const [retainedPreview, setRetainedPreview] = useState(() => ({
    assetId: props.asset.id,
    url: previewURL,
  }))
  const [failedPreview, setFailedPreview] = useState<{
    assetId: number
    url: string
  } | null>(null)
  const failedPreviewURL =
    failedPreview?.assetId === props.asset.id ? failedPreview.url : null
  const displayedPreview = retainSeedanceAssetPreview(
    retainedPreview,
    props.asset.id,
    previewURL,
    failedPreviewURL
  )
  const displayedPreviewURL = displayedPreview.url
  const hasPreview =
    Boolean(displayedPreviewURL) && displayedPreviewURL !== failedPreviewURL

  // 媒体确认可用后再记住地址；后续签名轮换不会改变已渲染元素的 src。
  const retainLoadedPreview = () => {
    setRetainedPreview((current) =>
      current.assetId === displayedPreview.assetId &&
      current.url === displayedPreview.url
        ? current
        : displayedPreview
    )
  }

  if (hasPreview && assetType === 'image') {
    return (
      <img
        src={displayedPreviewURL}
        alt={props.asset.name || props.asset.asset_id}
        className={props.className}
        loading='lazy'
        decoding='async'
        referrerPolicy='no-referrer'
        onLoad={retainLoadedPreview}
        onError={() =>
          setFailedPreview({
            assetId: props.asset.id,
            url: displayedPreviewURL,
          })
        }
      />
    )
  }

  if (hasPreview && assetType === 'video') {
    return (
      <video
        src={displayedPreviewURL}
        className={props.className}
        controls
        muted
        playsInline
        preload='metadata'
        onLoadedMetadata={retainLoadedPreview}
        onError={() =>
          setFailedPreview({
            assetId: props.asset.id,
            url: displayedPreviewURL,
          })
        }
      />
    )
  }

  if (hasPreview && assetType === 'audio' && props.showAudioControls) {
    return (
      <div className='flex h-full w-full items-center justify-center px-4'>
        <audio
          src={displayedPreviewURL}
          className='w-full'
          controls
          preload='metadata'
          onLoadedMetadata={retainLoadedPreview}
          onError={() =>
            setFailedPreview({
              assetId: props.asset.id,
              url: displayedPreviewURL,
            })
          }
        />
      </div>
    )
  }

  // 审核中通常还没有临时预览地址，统一使用稳定的类型占位，避免出现破图。
  let Icon = File
  if (assetType === 'image') Icon = Image
  if (assetType === 'video') Icon = Film
  if (assetType === 'audio') Icon = Music2

  return (
    <div className='text-muted-foreground flex h-full w-full flex-col items-center justify-center gap-2'>
      <Icon className='size-8' aria-hidden='true' />
      <span className='text-xs'>
        {t(getSeedanceAssetTypeLabel(props.asset.asset_type))}
      </span>
    </div>
  )
}
