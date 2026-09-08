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
import { Grid2X2, List } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'

import type { SeedanceAssetViewMode } from '../types'

// 视图切换保持为紧凑的分段控件，便于在桌面和移动端快速切换。
export function AssetViewToggle(props: {
  value: SeedanceAssetViewMode
  onChange: (value: SeedanceAssetViewMode) => void
}) {
  const { t } = useTranslation()
  const options: Array<{
    value: SeedanceAssetViewMode
    label: string
    icon: typeof Grid2X2
  }> = [
    { value: 'grid', label: t('Card view'), icon: Grid2X2 },
    { value: 'list', label: t('List view'), icon: List },
  ]

  return (
    <div
      role='group'
      aria-label={t('View mode')}
      className='bg-muted/60 inline-flex h-8 items-center rounded-lg border p-0.5'
    >
      {options.map((option) => {
        const Icon = option.icon
        const active = option.value === props.value
        return (
          <Tooltip key={option.value}>
            <TooltipTrigger
              render={
                <button
                  type='button'
                  aria-label={option.label}
                  aria-pressed={active}
                  className={cn(
                    'inline-flex h-full w-7 items-center justify-center rounded-md text-xs transition-all',
                    active
                      ? 'bg-primary text-primary-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'
                  )}
                  onClick={() => props.onChange(option.value)}
                />
              }
            >
              <Icon className='size-3.5' aria-hidden='true' />
            </TooltipTrigger>
            <TooltipContent side='bottom' className='text-xs'>
              {option.label}
            </TooltipContent>
          </Tooltip>
        )
      })}
    </div>
  )
}
