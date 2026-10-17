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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { DateTimePicker } from '@/components/datetime-picker'
import { Button } from '@/components/ui/button'
import {
  Field,
  FieldDescription,
  FieldError,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Switch } from '@/components/ui/switch'
import { shanghaiTime } from '@/lib/dayjs'

import type { DiscountBranch } from '../lib/user-discount-expression'
import { timeToMinute } from '../lib/user-discount-time'
import type { UserModelPricingFormValues } from '../lib/user-model-pricing-form'
import {
  ALL_DAY_EVERY_DAY,
  discountDayEnd,
  discountDayStart,
  isDiscountDayStart,
  validityEndDate,
} from '../lib/user-temporary-discount'
import { DiscountPercentInput, DiscountTimeInput } from './user-discount-inputs'

export type TemporaryDiscountRule = NonNullable<
  UserModelPricingFormValues['items'][number]['periods']
>[number]

/** 默认表单：折扣比例、按天的有效期和每天的生效时段。 */
export function TemporaryDiscountSimpleFields(props: {
  id: string
  branch: DiscountBranch | undefined
  permanentPercent: number | null | undefined
  value: TemporaryDiscountRule
  errors: Record<string, string>
  onBranchChange: (
    changes: Partial<Pick<DiscountBranch, 'percent' | 'schedule'>>
  ) => void
  onChange: (changes: Partial<TemporaryDiscountRule>) => void
}) {
  const { t } = useTranslation()
  const [percentTouched, setPercentTouched] = useState(false)
  const schedule = props.branch?.schedule ?? ALL_DAY_EVERY_DAY
  const percent = props.branch?.percent ?? null
  const start = props.value.start_time
  const end = props.value.end_time
  const startMinute = timeToMinute(schedule.start)
  const endMinute = timeToMinute(schedule.end)
  const overnight = (startMinute ?? 0) > (endMinute ?? 0)
  const invalidStartTime =
    !schedule.allDay && (startMinute === null || startMinute === endMinute)
  const invalidEndTime =
    !schedule.allDay && (endMinute === null || startMinute === endMinute)
  let timeError: string | null = null
  if (!schedule.allDay) {
    if (startMinute === null || endMinute === null) {
      timeError = t('Complete the time rules before applying this discount.')
    } else if (startMinute === endMinute) {
      timeError = t('Start and end times must differ')
    }
  }
  const preciseValidity =
    (start != null && !isDiscountDayStart(start)) ||
    (end != null && !isDiscountDayStart(end))
  const dateError = props.errors.start_time || props.errors.end_time

  return (
    <FieldGroup className='gap-4'>
      <div onBlur={() => setPercentTouched(true)}>
        <DiscountPercentInput
          id={`${props.id}-percent`}
          label={t('Temporary discount percentage')}
          showError={Boolean(props.branch) || percentTouched}
          value={percent}
          onChange={(value) => props.onBranchChange({ percent: value })}
        />
        {percent != null && percent === props.permanentPercent && (
          <FieldDescription className='text-warning mt-2'>
            {t(
              'Same as the permanent discount, so this temporary discount changes nothing.'
            )}
          </FieldDescription>
        )}
      </div>
      <Field data-invalid={Boolean(dateError)}>
        <FieldLabel htmlFor={`${props.id}-start`}>
          {t('Effective dates')}
        </FieldLabel>
        <div className='flex flex-wrap items-center gap-2'>
          <DateTimePicker
            timeZone='Asia/Shanghai'
            id={`${props.id}-start`}
            className='w-36'
            aria-label={t('Start date')}
            aria-invalid={Boolean(props.errors.start_time)}
            hideTime
            clearable={false}
            value={
              start != null
                ? shanghaiTime(start * 1000)
                    .startOf('day')
                    .toDate()
                : undefined
            }
            placeholder={t('On save')}
            onChange={(value) =>
              props.onChange({
                start_time: value ? discountDayStart(value) : null,
              })
            }
          />
          <span aria-hidden='true' className='text-muted-foreground'>
            →
          </span>
          <DateTimePicker
            timeZone='Asia/Shanghai'
            id={`${props.id}-end`}
            className='w-36'
            aria-label={t('End date')}
            aria-invalid={Boolean(props.errors.end_time)}
            hideTime
            clearable={false}
            value={end != null ? validityEndDate(end) : undefined}
            onChange={(value) =>
              props.onChange({
                end_time: value ? discountDayEnd(value) : null,
                legacy_unbounded: false,
              })
            }
          />
          {([7, 30] as const).map((days) => (
            <Button
              key={days}
              type='button'
              size='sm'
              variant='outline'
              onClick={() =>
                props.onChange({
                  end_time: shanghaiTime(
                    (start ?? Math.floor(Date.now() / 1000)) * 1000
                  )
                    .startOf('day')
                    .add(days, 'day')
                    .unix(),
                  legacy_unbounded: false,
                })
              }
            >
              {t('For {{days}} days', { days })}
            </Button>
          ))}
        </div>
        {dateError && <FieldError>{dateError}</FieldError>}
        {preciseValidity && (
          <FieldDescription>
            {t(
              'This discount starts or ends at a specific time. Picking a date switches that side to whole days.'
            )}
          </FieldDescription>
        )}
        {props.value.legacy_unbounded && (
          <FieldDescription>
            {t(
              'This existing rule has no end time. Set an end time before changing it.'
            )}
          </FieldDescription>
        )}
      </Field>
      <Field>
        <div className='flex items-center justify-between gap-3'>
          <span className='text-sm font-medium'>{t('Daily hours')}</span>
          <span className='flex items-center gap-2'>
            <FieldLabel
              htmlFor={`${props.id}-all-day`}
              className='text-muted-foreground font-normal'
            >
              {t('All day')}
            </FieldLabel>
            <Switch
              id={`${props.id}-all-day`}
              checked={schedule.allDay}
              onCheckedChange={(allDay) =>
                props.onBranchChange({ schedule: { ...schedule, allDay } })
              }
            />
          </span>
        </div>
        {!schedule.allDay && (
          <div className='grid grid-cols-2 gap-3'>
            {(['start', 'end'] as const).map((field) => (
              <Field
                key={field}
                data-invalid={
                  field === 'start' ? invalidStartTime : invalidEndTime
                }
              >
                <FieldLabel htmlFor={`${props.id}-daily-${field}`}>
                  {t(field === 'start' ? 'Start time' : 'End time')}
                  {field === 'end' && overnight && ` · ${t('Next day')}`}
                </FieldLabel>
                <DiscountTimeInput
                  id={`${props.id}-daily-${field}`}
                  label={t(field === 'start' ? 'Start time' : 'End time')}
                  value={schedule[field]}
                  invalid={
                    field === 'start' ? invalidStartTime : invalidEndTime
                  }
                  aria-describedby={
                    timeError ? `${props.id}-daily-error` : undefined
                  }
                  onChange={(value) =>
                    props.onBranchChange({
                      schedule: { ...schedule, [field]: value },
                    })
                  }
                />
              </Field>
            ))}
          </div>
        )}
        {timeError && (
          <FieldError id={`${props.id}-daily-error`}>{timeError}</FieldError>
        )}
      </Field>
    </FieldGroup>
  )
}

/** 高级模式下精确到分钟的有效期。 */
export function TemporaryDiscountValidityFields(props: {
  id: string
  value: TemporaryDiscountRule
  errors: Record<string, string>
  onChange: (changes: Partial<TemporaryDiscountRule>) => void
}) {
  const { t } = useTranslation()
  const start = props.value.start_time
  const end = props.value.end_time

  return (
    <div className='flex flex-col gap-3 border-b pb-4'>
      <div className='flex flex-wrap justify-end gap-2'>
        {([7, 30] as const).map((days) => (
          <Button
            type='button'
            key={days}
            size='sm'
            variant='outline'
            onClick={() =>
              props.onChange({
                end_time:
                  (start ?? Math.floor(Date.now() / 1000)) + days * 86400,
                legacy_unbounded: false,
              })
            }
          >
            {t('For {{days}} days', { days })}
          </Button>
        ))}
      </div>
      <FieldGroup className='grid gap-3 md:grid-cols-2'>
        <Field data-invalid={Boolean(props.errors.start_time)}>
          <FieldLabel htmlFor={`${props.id}-start`}>
            {t('Validity starts')}
          </FieldLabel>
          <DateTimePicker
            timeZone='Asia/Shanghai'
            id={`${props.id}-start`}
            aria-label={t('Validity starts')}
            aria-invalid={Boolean(props.errors.start_time)}
            aria-describedby={
              props.errors.start_time
                ? `${props.id}-validity-start-error`
                : undefined
            }
            clearable={false}
            value={start != null ? new Date(start * 1000) : undefined}
            placeholder={t('On save')}
            onChange={(value) =>
              props.onChange({
                start_time: value ? Math.floor(value.getTime() / 1000) : null,
              })
            }
          />
          {props.errors.start_time && (
            <FieldError id={`${props.id}-validity-start-error`}>
              {props.errors.start_time}
            </FieldError>
          )}
        </Field>
        <Field data-invalid={Boolean(props.errors.end_time)}>
          <FieldLabel htmlFor={`${props.id}-end`}>
            {t('Validity ends')}
          </FieldLabel>
          <DateTimePicker
            timeZone='Asia/Shanghai'
            id={`${props.id}-end`}
            aria-label={t('Validity ends')}
            aria-invalid={Boolean(props.errors.end_time)}
            aria-describedby={
              props.errors.end_time
                ? `${props.id}-validity-end-error`
                : undefined
            }
            hideTimeWhenEmpty
            value={end != null ? new Date(end * 1000) : undefined}
            placeholder={t('Select an end time')}
            onChange={(value) =>
              props.onChange({
                end_time: value ? Math.floor(value.getTime() / 1000) : null,
                legacy_unbounded: false,
              })
            }
          />
          {props.errors.end_time && (
            <FieldError id={`${props.id}-validity-end-error`}>
              {props.errors.end_time}
            </FieldError>
          )}
        </Field>
      </FieldGroup>
      {props.value.legacy_unbounded && (
        <p className='text-muted-foreground text-xs'>
          {t(
            'This existing rule has no end time. Set an end time before changing it.'
          )}
        </p>
      )}
    </div>
  )
}
