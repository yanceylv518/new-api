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
import { Clock3 } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Field, FieldError, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from '@/components/ui/input-group'
import {
  Popover,
  PopoverContent,
  PopoverTitle,
  PopoverTrigger,
} from '@/components/ui/popover'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import { discountPercentLiteral } from '../lib/user-discount-expression'

const TIME_HOURS = Array.from({ length: 24 }, (_, hour) => {
  const value = String(hour).padStart(2, '0')
  return { value, label: value }
})
const TIME_MINUTES = Array.from({ length: 60 }, (_, minute) => {
  const value = String(minute).padStart(2, '0')
  return { value, label: value }
})

export function DiscountChoice(props: {
  id: string
  label: string
  value: string
  items: { value: string; label: string }[]
  invalid?: boolean
  onChange: (value: string) => void
}) {
  return (
    <Field data-invalid={props.invalid}>
      <FieldLabel htmlFor={props.id}>{props.label}</FieldLabel>
      <Select
        value={props.value}
        items={props.items}
        onValueChange={(value) => {
          if (
            value !== null &&
            props.items.some((item) => item.value === value)
          ) {
            props.onChange(value)
          }
        }}
      >
        <SelectTrigger
          id={props.id}
          className='w-full min-w-0'
          aria-invalid={props.invalid}
        >
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectGroup>
            {props.items.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectGroup>
        </SelectContent>
      </Select>
    </Field>
  )
}

export function DiscountTimeInput(props: {
  id: string
  label: string
  value: string
  invalid?: boolean
  'aria-describedby'?: string
  onChange: (value: string) => void
}) {
  const { t } = useTranslation()
  const [hour, minute] = props.value.split(':')

  return (
    <Popover>
      <InputGroup>
        <InputGroupInput
          id={props.id}
          type='time'
          step={60}
          value={props.value}
          aria-invalid={props.invalid}
          aria-describedby={props['aria-describedby']}
          onChange={(event) => props.onChange(event.target.value)}
          className='tabular-nums [&::-webkit-calendar-picker-indicator]:hidden [&::-webkit-calendar-picker-indicator]:appearance-none'
        />
        <InputGroupAddon align='inline-end'>
          <PopoverTrigger
            render={
              <InputGroupButton
                size='icon-xs'
                aria-label={`${props.label} (${t('Time')})`}
              />
            }
          >
            <Clock3 aria-hidden='true' />
          </PopoverTrigger>
        </InputGroupAddon>
      </InputGroup>
      <PopoverContent align='end' className='w-56'>
        <PopoverTitle>{props.label}</PopoverTitle>
        <div className='grid grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-end gap-2 tabular-nums'>
          <DiscountChoice
            id={`${props.id}-hour`}
            label={t('Hour')}
            value={hour ?? ''}
            items={TIME_HOURS}
            onChange={(value) => props.onChange(`${value}:${minute || '00'}`)}
          />
          <span aria-hidden='true' className='flex h-8 items-center'>
            :
          </span>
          <DiscountChoice
            id={`${props.id}-minute`}
            label={t('Minute')}
            value={minute ?? ''}
            items={TIME_MINUTES}
            onChange={(value) => props.onChange(`${hour || '00'}:${value}`)}
          />
        </div>
      </PopoverContent>
    </Popover>
  )
}

export function DiscountPercentInput(props: {
  id: string
  label: string
  hideLabel?: boolean
  // 尚未填写的新规则先不提示错误，避免表单一打开就报错。
  showError?: boolean
  value: number | null | undefined
  onChange: (value: number | null) => void
}) {
  const { t } = useTranslation()
  const invalid =
    props.showError !== false &&
    discountPercentLiteral(props.value ?? null) === null
  return (
    <Field data-invalid={invalid}>
      <FieldLabel
        htmlFor={props.id}
        className={props.hideLabel ? 'sr-only' : undefined}
      >
        {props.label}
      </FieldLabel>
      <div className='relative'>
        <Input
          id={props.id}
          type='number'
          min={0.01}
          max={100}
          step={0.01}
          className='pr-8 tabular-nums'
          aria-invalid={invalid}
          aria-describedby={invalid ? `${props.id}-error` : undefined}
          value={
            props.value != null && Number.isFinite(props.value)
              ? props.value
              : ''
          }
          onChange={(event) =>
            props.onChange(
              Number.isFinite(event.target.valueAsNumber)
                ? event.target.valueAsNumber
                : null
            )
          }
        />
        <span className='text-muted-foreground pointer-events-none absolute inset-y-0 right-3 flex items-center'>
          %
        </span>
      </div>
      {invalid && (
        <FieldError id={`${props.id}-error`}>
          {t(
            'Enter a percentage from 0.01 to 100, with up to two decimal places.'
          )}
        </FieldError>
      )}
    </Field>
  )
}
