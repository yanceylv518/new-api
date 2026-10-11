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
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

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
import { Label } from '@/components/ui/label'
import dayjs from '@/lib/dayjs'

import {
  createUserModelPricingFormSchema,
  getPricingPeriodStatus,
  type UserModelPricingFormValues,
} from '../lib/user-model-pricing-form'
import { UserDiscountRuleEditor } from './user-discount-rule-editor'

type PricingDraft = UserModelPricingFormValues['items'][number]
type PeriodDraft = NonNullable<PricingDraft['periods']>[number]

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
  return (
    <span className='flex flex-wrap items-center gap-1 text-xs'>
      <PricingPeriodStatusBadge {...props} />
      <span className='text-muted-foreground'>
        {props.period.start_time
          ? dayjs.unix(props.period.start_time).format('YYYY-MM-DD HH:mm')
          : t('Permanent discount')}
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
  const [draft, setDraft] = useState<PricingDraft>(() => ({
    ...props.value,
    mode: 'layered',
    periods: props.value.periods?.map((period) => ({ ...period })) ?? [],
  }))
  const [error, setError] = useState<string | null>(null)
  const [clockTick, setClockTick] = useState(0)
  useEffect(() => {
    const now = Date.now() / 1000
    const boundaries = (draft.periods ?? [])
      .flatMap((period) => [period.start_time, period.end_time])
      .filter(
        (value): value is number => typeof value === 'number' && value > now
      )
    const refresh = () => setClockTick((tick) => tick + 1)
    const timer = boundaries.length
      ? setTimeout(
          refresh,
          Math.min(2147483647, (Math.min(...boundaries) - now) * 1000 + 100)
        )
      : undefined
    const refreshVisible = () => {
      if (document.visibilityState === 'visible') refresh()
    }
    document.addEventListener('visibilitychange', refreshVisible)
    return () => {
      if (timer !== undefined) clearTimeout(timer)
      document.removeEventListener('visibilitychange', refreshVisible)
    }
  }, [draft.periods, clockTick])
  const [keys, setKeys] = useState(() =>
    (props.value.periods ?? []).map((_, index) => index)
  )
  const nextKey = useRef(keys.length)
  const schema = useMemo(() => createUserModelPricingFormSchema(t), [t])
  const validation = schema.safeParse({ items: [draft] })
  const issues = validation.success ? [] : validation.error.issues
  const fieldErrors = (position?: number) => {
    const errors: Record<string, string> = {}
    for (const issue of issues) {
      const isPeriod = issue.path[2] === 'periods'
      if (
        (position === undefined && !isPeriod) ||
        (position !== undefined && isPeriod && issue.path[3] === position)
      ) {
        errors[String(issue.path.at(-1))] = issue.message
      }
    }
    return errors
  }
  const update = (position: number, changes: Partial<PeriodDraft>) => {
    setDraft((previous) => ({
      ...previous,
      periods: previous.periods?.map((period, index) =>
        index === position
          ? {
              ...period,
              ...changes,
              legacy_unbounded:
                changes.legacy_unbounded ??
                ((changes.expression !== undefined &&
                  changes.expression !== period.expression) ||
                (changes.start_time !== undefined &&
                  changes.start_time !== period.start_time) ||
                (changes.discount_percent !== undefined &&
                  changes.discount_percent !== period.discount_percent)
                  ? false
                  : period.legacy_unbounded),
            }
          : period
      ),
    }))
    setError(null)
  }
  const periods = draft.periods ?? []
  const now = Math.floor(Date.now() / 1000)
  const active = periods.find(
    (period) => getPricingPeriodStatus(period, now) === 'active'
  )
  const permanentLabel = draft.expression
    ? t('Conditional discount')
    : `${String(draft.discount_percent ?? 100)}%`
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Temporary discounts')}
      description={draft.model_name}
      contentClassName='sm:max-w-3xl'
      footer={
        <>
          <Button type='button' variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            disabled={!validation.success || Boolean(error)}
            onClick={() => {
              if (!validation.success) return
              const result = props.onApply(validation.data.items[0])
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
      <div className='flex flex-col gap-5'>
        <section className='flex flex-col gap-3'>
          <div className='flex items-center justify-between gap-2'>
            <div>
              <p className='text-muted-foreground text-xs'>
                {t(
                  'Temporary discounts take priority when conditions match within their validity period. Otherwise, the permanent discount applies.'
                )}
              </p>
            </div>
            <Button
              type='button'
              variant='outline'
              size='sm'
              disabled={
                periods.length >= 1000 ||
                Boolean(draft.expression) ||
                draft.discount_percent == null
              }
              onClick={() => {
                const start = Math.max(
                  now,
                  ...periods.map((period) => period.end_time ?? now)
                )
                setDraft((previous) => ({
                  ...previous,
                  periods: [
                    ...(previous.periods ?? []),
                    {
                      discount_percent: previous.discount_percent ?? 100,
                      expression: 'permanent_discount',
                      calculation_type: 'expression',
                      start_time: start,
                      end_time: start + 86400,
                    },
                  ],
                }))
                setKeys((previous) => [...previous, nextKey.current++])
                setError(null)
              }}
            >
              <Plus aria-hidden='true' />
              {t('Add temporary discount')}
            </Button>
          </div>
          {draft.expression && (
            <Alert>
              <AlertDescription>
                {t(
                  'Set a fixed permanent discount before editing temporary discounts.'
                )}
              </AlertDescription>
            </Alert>
          )}
          {periods.length === 0 && (
            <p className='text-muted-foreground rounded-lg border border-dashed p-5 text-center text-sm'>
              {t('No temporary discounts. The permanent discount applies.')}
            </p>
          )}
          {periods.map((period, index) => {
            const id = `temporary-discount-${keys[index]}`
            const errors = fieldErrors(index)
            return (
              <section
                key={keys[index]}
                className='flex flex-col gap-3 rounded-lg border p-4'
              >
                <div className='flex items-center justify-between gap-2'>
                  <span className='flex items-center gap-2 text-sm font-medium'>
                    {t('Period {{n}}', { n: index + 1 })}
                    <PricingPeriodStatusBadge period={period} />
                  </span>
                  <Button
                    type='button'
                    variant='ghost'
                    size='icon-sm'
                    aria-label={t('Remove period')}
                    onClick={() => {
                      setDraft((previous) => ({
                        ...previous,
                        periods: previous.periods?.filter(
                          (_, position) => position !== index
                        ),
                      }))
                      setKeys((previous) =>
                        previous.filter((_, position) => position !== index)
                      )
                      setError(null)
                    }}
                  >
                    <Trash2 aria-hidden='true' />
                  </Button>
                </div>
                <fieldset
                  disabled={Boolean(draft.expression)}
                  className='min-w-0 disabled:opacity-60'
                >
                  <UserDiscountRuleEditor
                    id={id}
                    value={period}
                    permanentPercent={draft.discount_percent}
                    errors={errors}
                    onChange={(changes) => update(index, changes)}
                  />
                </fieldset>
                <FieldGroup className='gap-3'>
                  <Field data-invalid={Boolean(errors.start_time)}>
                    <FieldLabel htmlFor={`${id}-start`}>
                      {t('Start time')}
                    </FieldLabel>
                    <DateTimePicker
                      id={`${id}-start`}
                      aria-label={t('Start time')}
                      aria-invalid={Boolean(errors.start_time)}
                      clearable={false}
                      value={
                        period.start_time != null
                          ? new Date(period.start_time * 1000)
                          : undefined
                      }
                      placeholder={t('On save')}
                      onChange={(value) =>
                        update(index, {
                          start_time: value
                            ? Math.floor(value.getTime() / 1000)
                            : null,
                        })
                      }
                    />
                    {errors.start_time && (
                      <FieldError>{errors.start_time}</FieldError>
                    )}
                  </Field>
                  <Field data-invalid={Boolean(errors.end_time)}>
                    <FieldLabel htmlFor={`${id}-end`}>
                      {t('End time')}
                    </FieldLabel>
                    <DateTimePicker
                      id={`${id}-end`}
                      aria-label={t('End time')}
                      aria-invalid={Boolean(errors.end_time)}
                      hideTimeWhenEmpty
                      value={
                        period.end_time != null
                          ? new Date(period.end_time * 1000)
                          : undefined
                      }
                      placeholder={t('Select an end time')}
                      onChange={(value) =>
                        update(index, {
                          end_time: value
                            ? Math.floor(value.getTime() / 1000)
                            : null,
                          legacy_unbounded: false,
                        })
                      }
                    />
                    {errors.end_time && (
                      <FieldError>{errors.end_time}</FieldError>
                    )}
                  </Field>
                </FieldGroup>
                {period.legacy_unbounded && (
                  <p className='text-muted-foreground text-xs'>
                    {t(
                      'This existing rule has no end time. Set an end time before changing it.'
                    )}
                  </p>
                )}
              </section>
            )
          })}
        </section>
        <Alert>
          <AlertDescription>
            {active
              ? t(
                  'Temporary period active. Unmatched conditions use the permanent discount: {{discount}}.',
                  { discount: permanentLabel }
                )
              : t('Permanent discount: {{discount}}.', {
                  discount: permanentLabel,
                })}
          </AlertDescription>
        </Alert>
        {(error || (!validation.success && issues.length > 0)) && (
          <Alert variant='destructive'>
            <AlertDescription>{error || issues[0]?.message}</AlertDescription>
          </Alert>
        )}
      </div>
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
  const invalid =
    (changeStart && (!start || !Number.isFinite(start.getTime()))) ||
    (changeEnd && (!end || !Number.isFinite(end.getTime()))) ||
    (changeStart && changeEnd && Boolean(start && end && end <= start))
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Batch validity')}
      description={t(
        'Only temporary discounts are changed. Permanent discounts are preserved.'
      )}
      footer={
        <>
          <Button type='button' variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button
            type='button'
            disabled={(!changeStart && !changeEnd) || invalid}
            onClick={() => {
              const result = props.onApply({
                ...(changeStart && start
                  ? { start_time: Math.floor(start.getTime() / 1000) }
                  : {}),
                ...(changeEnd && end
                  ? { end_time: Math.floor(end.getTime() / 1000) }
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
      <FieldGroup>
        <Field>
          <div className='flex items-center gap-2'>
            <Checkbox
              id='batch-start'
              checked={changeStart}
              onCheckedChange={setChangeStart}
            />
            <Label htmlFor='batch-start'>{t('Change start time')}</Label>
          </div>
          <DateTimePicker
            aria-label={t('Start time')}
            disabled={!changeStart}
            clearable={false}
            value={start}
            onChange={setStart}
          />
        </Field>
        <Field>
          <div className='flex items-center gap-2'>
            <Checkbox
              id='batch-end'
              checked={changeEnd}
              onCheckedChange={setChangeEnd}
            />
            <Label htmlFor='batch-end'>{t('Change end time')}</Label>
          </div>
          <DateTimePicker
            aria-label={t('End time')}
            disabled={!changeEnd}
            clearable={false}
            value={end}
            onChange={setEnd}
            placeholder={t('Select an end time')}
          />
        </Field>
        {invalid && (
          <FieldError>{t('End time must be after start time')}</FieldError>
        )}
        {error && (
          <Alert variant='destructive'>
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
      </FieldGroup>
    </Dialog>
  )
}
