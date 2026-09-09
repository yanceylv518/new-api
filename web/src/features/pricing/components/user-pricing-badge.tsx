/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { BadgePercent } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

import {
  getUserModelDiscountPercent,
  hasUserModelDiscount,
} from '../lib/model-helpers'
import type { PricingModel } from '../types'

export interface UserPricingBadgeProps {
  model: PricingModel
  className?: string
}

function formatDiscountPercent(value: number): string {
  return Number.isInteger(value)
    ? String(value)
    : value.toFixed(2).replace(/0+$/, '').replace(/\.$/, '')
}

/** 标记模型广场中已应用当前用户专属折扣的价格。 */
export function UserPricingBadge(props: UserPricingBadgeProps) {
  const { t } = useTranslation()
  if (!hasUserModelDiscount(props.model)) return null

  const percent = formatDiscountPercent(
    Math.max(0, Math.min(100, getUserModelDiscountPercent(props.model)))
  )

  return (
    <Badge
      variant='secondary'
      className={cn(
        'border-primary/20 bg-primary/8 text-primary gap-1 px-1.5 text-[10px] leading-4',
        props.className
      )}
      title={t('Model discount')}
    >
      <BadgePercent aria-hidden='true' />
      {t('Discount {{percent}}%', { percent })}
    </Badge>
  )
}
