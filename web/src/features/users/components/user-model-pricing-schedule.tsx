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
import { Plus, Trash2 } from 'lucide-react'
import { useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DateTimePicker } from '@/components/datetime-picker'
import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Field,
  FieldError,
  FieldGroup,
  FieldLabel,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import dayjs from '@/lib/dayjs'

import {
  createUserModelPricingFormSchema,
  getPricingPeriodStatus,
  type UserModelPricingFormValues,
} from '../lib/user-model-pricing-form'

type PricingDraft = UserModelPricingFormValues['items'][number]
type PeriodDraft = NonNullable<PricingDraft['periods']>[number]
type PeriodField = 'discount_percent' | 'start_time' | 'end_time'

export function PricingPeriodStatusBadge(props: {
  period: { start_time?: number | null; end_time?: number | null }
  now?: number
}) {
  const { t } = useTranslation()
  const status = getPricingPeriodStatus(
    props.period,
    props.now ?? Math.floor(Date.now() / 1000)
  )
  const labels = {
    active: t('Active'),
    pending: t('Not started'),
    expired: t('Expired'),
  }
  const variants = {
    active: 'outline',
    pending: 'warning',
    expired: 'destructive',
  } as const
  return (
    <Badge
      variant={variants[status]}
      className={
        status === 'active'
          ? 'border-success/40 bg-success/10 text-success'
          : undefined
      }
    >
      {labels[status]}
    </Badge>
  )
}

export function PricingPeriodSummary(props: {
  period: { start_time?: number | null; end_time?: number | null }
  now?: number
}) {
  const { t } = useTranslation()
  let startLabel = t('On save')
  if (props.period.start_time === 0) startLabel = t('Existing rule')
  else if (props.period.start_time) {
    startLabel = dayjs.unix(props.period.start_time).format('YYYY-MM-DD HH:mm')
  }
  return (
    <span className='flex flex-wrap items-center gap-1 text-xs'>
      <PricingPeriodStatusBadge period={props.period} now={props.now} />
      <span className='text-muted-foreground'>
        {startLabel}
        {' → '}
        {props.period.end_time == null
          ? t('Permanent')
          : dayjs.unix(props.period.end_time).format('YYYY-MM-DD HH:mm')}
      </span>
    </span>
  )
}

export function UserModelPricingScheduleDialog(props: {
  value: PricingDraft
  onApply: (value: PricingDraft) => string | null
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<PricingDraft>(() =>
    props.value.mode === 'scheduled'
      ? {
          ...props.value,
          periods: props.value.periods?.map((period) => ({
            ...period,
            start_time: period.start_time ?? Math.floor(Date.now() / 1000),
          })),
        }
      : {
          ...props.value,
          start_time: props.value.start_time ?? Math.floor(Date.now() / 1000),
        }
  )
  const [error, setError] = useState<string | null>(null)
  const [confirmSingle, setConfirmSingle] = useState(false)
  const [keepIndex, setKeepIndex] = useState('0')
  // 删除中间时间段时保留其余编辑器身份，避免复用被删段的日期选择器状态。
  const [periodKeys, setPeriodKeys] = useState(() =>
    Array.from(
      {
        length:
          props.value.mode === 'scheduled'
            ? (props.value.periods?.length ?? 0)
            : 1,
      },
      (_, index) => index
    )
  )
  const nextPeriodKey = useRef(periodKeys.length)
  const schema = useMemo(() => createUserModelPricingFormSchema(t), [t])
  const mode = draft.mode ?? 'single'
  const periods: PeriodDraft[] =
    draft.mode === 'scheduled' ? (draft.periods ?? []) : [draft]
  const validation = useMemo(
    () => schema.safeParse({ items: [draft] }),
    [schema, draft]
  )
  const fieldErrors = new Map<number, Partial<Record<PeriodField, string>>>()
  if (!validation.success) {
    for (const issue of validation.error.issues) {
      const periodIndex = issue.path[2] === 'periods' ? issue.path[3] : 0
      const field = issue.path.at(-1)
      if (
        typeof periodIndex !== 'number' ||
        (field !== 'discount_percent' &&
          field !== 'start_time' &&
          field !== 'end_time')
      ) {
        continue
      }
      const errors = fieldErrors.get(periodIndex) ?? {}
      errors[field] ??= issue.message
      fieldErrors.set(periodIndex, errors)
    }
  }
  const orderedPeriods = periods
    .map((period, index) => ({
      index,
      start: period.start_time ?? Math.floor(Date.now() / 1000),
    }))
    .filter((period) => Number.isFinite(period.start))
    .sort((a, b) => a.start - b.start)
  const nextStarts = new Map<number, number>()
  orderedPeriods.forEach((period, index) => {
    const next = orderedPeriods[index + 1]
    if (next && next.start > period.start) {
      nextStarts.set(period.index, next.start)
    }
  })

  const updatePeriod = (index: number, update: Partial<PeriodDraft>) => {
    setDraft((previous) =>
      previous.mode === 'scheduled'
        ? {
            ...previous,
            periods: previous.periods?.map((period, position) =>
              position === index ? { ...period, ...update } : period
            ),
          }
        : { ...previous, ...update }
    )
    setError(null)
  }

  const selectSinglePeriod = (index: number) => {
    const period = periods[index]
    const key = periodKeys[index]
    if (!period || key === undefined) return
    setDraft({
      model_name: draft.model_name,
      mode: 'single',
      ...period,
      periods: undefined,
    })
    setPeriodKeys([key])
    setConfirmSingle(false)
    setError(null)
  }

  const apply = () => {
    const result = schema.safeParse({ items: [draft] })
    if (!result.success) {
      setError(result.error.issues[0].message)
      return
    }
    const applyError = props.onApply(result.data.items[0])
    if (applyError) {
      setError(applyError)
      return
    }
    props.onClose()
  }

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Discount validity')}
      description={draft.model_name}
      contentClassName='sm:max-w-2xl'
      footer={
        <>
          <Button variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            disabled={!validation.success || Boolean(error)}
            onClick={apply}
          >
            {t('Apply')}
          </Button>
        </>
      }
    >
      <div className='flex flex-col gap-4'>
        <Tabs
          value={mode}
          onValueChange={(value) => {
            if (value === 'scheduled') {
              setDraft({
                ...draft,
                mode: 'scheduled',
                periods: [
                  {
                    discount_percent: draft.discount_percent ?? Number.NaN,
                    start_time: draft.start_time,
                    end_time: draft.end_time,
                  },
                ],
              })
            } else if (value === 'single') {
              if (periods.length > 1) {
                setKeepIndex('0')
                setConfirmSingle(true)
              } else selectSinglePeriod(0)
            }
            setError(null)
          }}
        >
          <TabsList>
            <TabsTrigger value='single'>{t('Single period')}</TabsTrigger>
            <TabsTrigger value='scheduled'>{t('Multiple periods')}</TabsTrigger>
          </TabsList>
        </Tabs>
        <p className='text-muted-foreground text-xs'>
          {t('Blank end time means permanent.')}
        </p>
        {mode === 'single' &&
          draft.start_time != null &&
          draft.start_time > Math.floor(Date.now() / 1000) && (
            <Alert>
              <AlertDescription>
                {t(
                  'Until this period starts, the model is charged at its normal price.'
                )}
              </AlertDescription>
            </Alert>
          )}
        {periods.map((period, index) => {
          const errors = fieldErrors.get(index) ?? {}
          const fieldId = `pricing-period-${periodKeys[index]}`
          return (
            <div
              key={periodKeys[index]}
              className='flex flex-col gap-3 rounded-lg border p-3'
            >
              <div className='flex items-center justify-between gap-2'>
                <span className='text-sm font-medium'>
                  {t('Period {{n}}', { n: index + 1 })}
                </span>
                {mode === 'scheduled' && (
                  <Button
                    type='button'
                    variant='ghost'
                    size='icon-sm'
                    aria-label={t('Remove period')}
                    disabled={periods.length === 1}
                    onClick={() => {
                      setDraft({
                        ...draft,
                        mode: 'scheduled',
                        periods: periods.filter(
                          (_, position) => position !== index
                        ),
                      })
                      setPeriodKeys((keys) =>
                        keys.filter((_, position) => position !== index)
                      )
                      setError(null)
                    }}
                  >
                    <Trash2 aria-hidden='true' />
                  </Button>
                )}
              </div>
              <FieldGroup className='gap-3'>
                <Field data-invalid={Boolean(errors.discount_percent)}>
                  <FieldLabel htmlFor={`pricing-period-${index}`}>
                    {t('Discount percentage')}
                  </FieldLabel>
                  <div className='relative'>
                    <Input
                      id={`pricing-period-${index}`}
                      type='number'
                      min='0.01'
                      max={mode === 'scheduled' ? '99.99' : '100'}
                      step='0.01'
                      aria-invalid={Boolean(errors.discount_percent)}
                      aria-describedby={
                        errors.discount_percent
                          ? `${fieldId}-discount-error`
                          : undefined
                      }
                      className='pr-7 font-mono'
                      value={
                        Number.isFinite(period.discount_percent)
                          ? period.discount_percent
                          : ''
                      }
                      onChange={(event) =>
                        updatePeriod(index, {
                          discount_percent: event.target.valueAsNumber,
                        })
                      }
                    />
                    <span
                      className='text-muted-foreground pointer-events-none absolute top-1/2 right-3 -translate-y-1/2 text-sm'
                      aria-hidden='true'
                    >
                      %
                    </span>
                  </div>
                  {errors.discount_percent && (
                    <FieldError id={`${fieldId}-discount-error`}>
                      {errors.discount_percent}
                    </FieldError>
                  )}
                </Field>
                <Field data-invalid={Boolean(errors.start_time)}>
                  <FieldLabel htmlFor={`${fieldId}-start`}>
                    {t('Start time')}
                  </FieldLabel>
                  <DateTimePicker
                    id={`${fieldId}-start`}
                    hideTimeWhenEmpty
                    aria-label={t('Start time')}
                    aria-invalid={Boolean(errors.start_time)}
                    aria-describedby={
                      errors.start_time ? `${fieldId}-start-error` : undefined
                    }
                    value={
                      period.start_time != null && period.start_time !== 0
                        ? new Date(period.start_time * 1000)
                        : undefined
                    }
                    clearable={false}
                    placeholder={
                      period.start_time === 0
                        ? t('Existing rule')
                        : t('On save')
                    }
                    onChange={(value) => {
                      if (value) {
                        updatePeriod(index, {
                          start_time: Math.floor(value.getTime() / 1000),
                        })
                      }
                    }}
                  />
                  {errors.start_time && (
                    <FieldError id={`${fieldId}-start-error`}>
                      {errors.start_time}
                    </FieldError>
                  )}
                </Field>
                <Field data-invalid={Boolean(errors.end_time)}>
                  <FieldLabel htmlFor={`${fieldId}-end`}>
                    {t('End time')}
                  </FieldLabel>
                  <DateTimePicker
                    id={`${fieldId}-end`}
                    aria-label={t('End time')}
                    aria-invalid={Boolean(errors.end_time)}
                    aria-describedby={
                      errors.end_time ? `${fieldId}-end-error` : undefined
                    }
                    hideTimeWhenEmpty
                    value={
                      period.end_time != null
                        ? new Date(period.end_time * 1000)
                        : undefined
                    }
                    placeholder={t('Permanent')}
                    onChange={(value) =>
                      updatePeriod(index, {
                        end_time: value
                          ? Math.floor(value.getTime() / 1000)
                          : null,
                      })
                    }
                  />
                  {errors.end_time && (
                    <FieldError id={`${fieldId}-end-error`}>
                      {errors.end_time}
                    </FieldError>
                  )}
                  {errors.end_time && nextStarts.has(index) && (
                    <Button
                      type='button'
                      size='sm'
                      variant='ghost'
                      className='self-start'
                      onClick={() =>
                        updatePeriod(index, { end_time: nextStarts.get(index) })
                      }
                    >
                      {t('End at next start')}
                    </Button>
                  )}
                </Field>
              </FieldGroup>
              {Object.keys(errors).length === 0 && (
                <PricingPeriodSummary period={period} />
              )}
            </div>
          )
        })}
        {mode === 'scheduled' && (
          <Button
            variant='outline'
            disabled={periods.length >= 1000}
            onClick={() => {
              const key = nextPeriodKey.current++
              setPeriodKeys((keys) => [...keys, key])
              setDraft({
                ...draft,
                mode: 'scheduled',
                periods: [
                  ...periods,
                  {
                    discount_percent: periods.at(-1)?.discount_percent ?? 99,
                    start_time:
                      periods.at(-1)?.end_time ??
                      Math.max(
                        Math.floor(Date.now() / 1000),
                        (periods.at(-1)?.start_time ??
                          Math.floor(Date.now() / 1000)) + 60
                      ),
                    end_time: null,
                  },
                ],
              })
              setError(null)
            }}
          >
            <Plus aria-hidden='true' />
            {t('Add period')}
          </Button>
        )}
        {error && (
          <Alert variant='destructive'>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        {!validation.success && fieldErrors.size === 0 && (
          <Alert variant='destructive'>
            <AlertDescription>
              {validation.error.issues[0].message}
            </AlertDescription>
          </Alert>
        )}
      </div>
      <ConfirmDialog
        open={confirmSingle}
        onOpenChange={setConfirmSingle}
        title={t('Switch to a single period')}
        desc={t(
          'Only the selected period will remain in the draft. Other periods are removed when you save.'
        )}
        confirmText={t('Keep selected period')}
        handleConfirm={() => selectSinglePeriod(Number(keepIndex))}
      >
        <Select
          value={keepIndex}
          onValueChange={(value) => {
            if (value !== null) setKeepIndex(value)
          }}
        >
          <SelectTrigger aria-label={t('Period to keep')} className='w-full'>
            <SelectValue>
              {t('Period {{n}}', { n: Number(keepIndex) + 1 })}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectGroup>
              {periods.map((period, index) => (
                <SelectItem key={periodKeys[index]} value={String(index)}>
                  {index + 1}: {period.discount_percent}% ·{' '}
                  {period.start_time
                    ? dayjs.unix(period.start_time).format('YYYY-MM-DD HH:mm')
                    : t('On save')}
                </SelectItem>
              ))}
            </SelectGroup>
          </SelectContent>
        </Select>
      </ConfirmDialog>
    </Dialog>
  )
}

export function UserModelPricingBatchTimeDialog(props: {
  onClose: () => void
  onApply: (changes: {
    start_time?: number
    end_time?: number | null
  }) => string | null
}) {
  const { t } = useTranslation()
  const [changeStart, setChangeStart] = useState(false)
  const [changeEnd, setChangeEnd] = useState(false)
  const [start, setStart] = useState<Date | undefined>(() => new Date())
  const [end, setEnd] = useState<Date>()
  const [error, setError] = useState<string | null>(null)
  const invalidStart =
    changeStart && (!start || !Number.isFinite(start.getTime()))
  const invalidEnd =
    changeEnd && end !== undefined && !Number.isFinite(end.getTime())
  const invalidRange =
    changeStart &&
    changeEnd &&
    start !== undefined &&
    end !== undefined &&
    end <= start
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Batch validity')}
      description={t(
        'Only checked fields are applied to every selected period. Discounts are preserved.'
      )}
      footer={
        <>
          <Button variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button
            disabled={
              (!changeStart && !changeEnd) ||
              invalidStart ||
              invalidEnd ||
              invalidRange ||
              Boolean(error)
            }
            onClick={() => {
              if (changeStart && !start) {
                setError(t('Select a start time'))
                return
              }
              const result = props.onApply({
                ...(changeStart && start
                  ? { start_time: Math.floor(start.getTime() / 1000) }
                  : {}),
                ...(changeEnd
                  ? { end_time: end ? Math.floor(end.getTime() / 1000) : null }
                  : {}),
              })
              if (result) {
                setError(result)
                return
              }
              props.onClose()
            }}
          >
            {t('Apply')}
          </Button>
        </>
      }
    >
      <div className='flex flex-col gap-4'>
        <div className='flex items-center gap-2'>
          <Checkbox
            id='batch-change-start'
            checked={changeStart}
            onCheckedChange={(checked) => {
              setChangeStart(checked)
              setError(null)
            }}
          />
          <Label htmlFor='batch-change-start'>{t('Change start time')}</Label>
        </div>
        <DateTimePicker
          aria-label={t('Start time')}
          aria-invalid={invalidStart}
          disabled={!changeStart}
          value={start}
          clearable={false}
          onChange={(value) => {
            setStart(value)
            setError(null)
          }}
        />
        <div className='flex items-center gap-2'>
          <Checkbox
            id='batch-change-end'
            checked={changeEnd}
            onCheckedChange={(checked) => {
              setChangeEnd(checked)
              setError(null)
            }}
          />
          <Label htmlFor='batch-change-end'>{t('Change end time')}</Label>
        </div>
        <DateTimePicker
          aria-label={t('End time')}
          aria-invalid={invalidEnd || invalidRange}
          hideTimeWhenEmpty
          disabled={!changeEnd}
          value={end}
          placeholder={t('Permanent')}
          onChange={(value) => {
            setEnd(value)
            setError(null)
          }}
        />
        {invalidRange && (
          <FieldError>{t('End time must be after start time')}</FieldError>
        )}
        {error && (
          <Alert variant='destructive'>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
      </div>
    </Dialog>
  )
}
