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

import { CopyButton } from '@/components/copy-button'
import { IconBadge, type IconBadgeTone } from '@/components/ui/icon-badge'
import { Label } from '@/components/ui/label'
import { ScrollArea } from '@/components/ui/scroll-area'
import { cn } from '@/lib/utils'

export function DetailContent(props: {
  value: string
  mono?: boolean
  scrollLabel?: string
}) {
  const content = (
    <p
      className={cn(
        'min-w-0 pr-6 text-xs leading-relaxed break-all whitespace-pre-wrap sm:wrap-break-word',
        props.mono && 'font-mono'
      )}
    >
      {props.value}
    </p>
  )
  return (
    <div className='bg-muted/30 relative min-w-0 overflow-hidden rounded-md border p-2.5'>
      <CopyButton
        value={props.value}
        size='icon'
        className='absolute top-1.5 right-1.5 h-5 w-5 p-0'
        iconClassName='size-3'
      />
      {props.scrollLabel ? (
        <ScrollArea
          role='region'
          aria-label={props.scrollLabel}
          className='w-full [&_[data-slot=scroll-area-viewport]]:max-h-[min(20rem,50dvh)] [&_[data-slot=scroll-area-viewport]]:overscroll-contain'
        >
          {content}
        </ScrollArea>
      ) : (
        content
      )}
    </div>
  )
}

export function DetailRow(props: {
  label: ReactNode
  value: ReactNode
  mono?: boolean
  muted?: boolean
}) {
  return (
    <div className='grid min-w-0 grid-cols-[5.25rem_minmax(0,1fr)] gap-2 text-sm sm:grid-cols-[7rem_minmax(0,1fr)] sm:gap-3'>
      <span className='text-muted-foreground min-w-0 text-xs'>
        {props.label}
      </span>
      <span
        className={cn(
          'max-w-full min-w-0 text-xs break-all sm:wrap-break-word',
          props.mono && 'font-mono',
          props.muted && 'text-muted-foreground'
        )}
      >
        {props.value}
      </span>
    </div>
  )
}

export function DetailSection(props: {
  icon?: ReactNode
  iconTone?: IconBadgeTone
  label: string
  variant?: 'default' | 'danger'
  children: ReactNode
}) {
  const isDanger = props.variant === 'danger'
  const iconTone = isDanger ? 'destructive' : props.iconTone
  return (
    <div className='min-w-0 space-y-1.5'>
      <Label
        className={cn(
          'flex items-center gap-1.5 text-xs font-semibold',
          isDanger && 'text-red-500'
        )}
      >
        {props.icon && (
          <IconBadge tone={iconTone} size='xs'>
            {props.icon}
          </IconBadge>
        )}
        {props.label}
      </Label>
      <div
        className={cn(
          'min-w-0 space-y-1 overflow-hidden rounded-md border p-2.5 max-sm:p-2',
          isDanger
            ? 'border-red-200 bg-red-50 dark:border-red-900 dark:bg-red-950/20'
            : 'bg-muted/30'
        )}
      >
        {props.children}
      </div>
    </div>
  )
}
