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
import { Loader2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { StatusBadge, type StatusVariant } from '@/components/status-badge'

import type { SeedanceAsset } from '../api'

function getStatusVariant(status: string): StatusVariant {
  switch (status.toLowerCase()) {
    case 'active':
    case 'success':
    case 'succeeded':
      return 'success'
    case 'failed':
      return 'danger'
    case 'processing':
    case 'pending':
      return 'warning'
    default:
      return 'neutral'
  }
}

// 将上游可能返回的大小写变体统一为前端已有的生命周期文案。
function getStatusLabel(status: string): string {
  switch (status.trim().toLowerCase()) {
    case 'active':
    case 'success':
    case 'succeeded':
      return 'Active'
    case 'failed':
      return 'Failed'
    case 'processing':
    case 'pending':
      return 'Processing'
    default:
      return status
  }
}

// 状态颜色与审核生命周期保持一致，Processing 使用持续旋转图标避免脉冲闪烁。
export function AssetStatusBadge(props: { asset: SeedanceAsset }) {
  const { t } = useTranslation()
  const statusLabel = getStatusLabel(props.asset.status)
  const isProcessing = statusLabel === 'Processing'
  return (
    <StatusBadge
      variant={getStatusVariant(props.asset.status)}
      copyable={false}
      aria-live='polite'
    >
      {isProcessing ? (
        <Loader2
          className='size-3.5 shrink-0 animate-spin'
          aria-hidden='true'
        />
      ) : null}
      {t(statusLabel)}
    </StatusBadge>
  )
}
