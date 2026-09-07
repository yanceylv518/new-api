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
import type { ReactNode } from 'react'

import { cn } from '@/lib/utils'

export interface DiscountedPriceProps {
  effective: ReactNode
  original?: ReactNode
  discounted?: boolean
  className?: string
  originalClassName?: string
  effectiveClassName?: string
}

/** 将公开价划线，并在存在用户折扣时展示实际价格。 */
export function DiscountedPrice(props: DiscountedPriceProps) {
  const showOriginal = props.discounted && props.original != null

  if (!showOriginal) {
    return props.effective
  }

  return (
    <span
      className={cn(
        'inline-flex min-w-0 flex-wrap items-baseline gap-x-1.5 gap-y-0.5',
        props.className
      )}
    >
      <del
        className={cn(
          'text-muted-foreground/55 font-normal line-through decoration-1',
          props.originalClassName
        )}
      >
        {props.original}
      </del>
      <span className={props.effectiveClassName}>{props.effective}</span>
    </span>
  )
}
