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
import { ChevronRight, Plus, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DateTimePicker } from '@/components/datetime-picker'
import { Dialog } from '@/components/dialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { Field, FieldError, FieldGroup } from '@/components/ui/field'
import { Label } from '@/components/ui/label'
import dayjs from '@/lib/dayjs'
import { cn } from '@/lib/utils'

import {
  createUserModelPricingFormSchema,
  getPricingPeriodStatus,
  type UserModelPricingFormValues,
} from '../lib/user-model-pricing-form'
import {
  describeTemporaryDiscountRules,
  formatValidityRange,
  isLocalDayStart,
} from '../lib/user-temporary-discount'
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
    active: t('Within validity period'),
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

/** 一行摘要：有效期、生效时段和折扣比例，供编辑弹窗和模型列表共用。 */
export function TemporaryDiscountSummary(props: {
  period: PeriodDraft
  permanentPercent: number | null | undefined
  className?: string
}) {
  const { t } = useTranslation()
  const { unsupported, rules } = describeTemporaryDiscountRules(
    props.period,
    props.permanentPercent,
    t
  )
  let detail = t('{{n}} time rules', { n: rules.length })
  if (unsupported) {
    detail = t('Custom conditions')
  } else if (
    rules.length === 0 ||
    (rules.length === 1 && rules[0].percent == null)
  ) {
    detail = t('No discount set')
  } else if (rules.length === 1) {
    detail = `${rules[0].when} · ${rules[0].percent}%`
  }
  return (
    <span className={cn('min-w-0 truncate', props.className)}>
      {formatValidityRange(props.period, {
        start: t('On save'),
        end: t('Permanent'),
      })}
      {' · '}
      {detail}
    </span>
  )
}

// 新临时折扣默认从今天起按整天生效七天；已有临时折扣时接在最后一段之后。
function newTemporaryDiscount(
  periods: readonly PeriodDraft[],
  permanentPercent: number | null | undefined
): PeriodDraft {
  const lastEnd = Math.max(0, ...periods.map((period) => period.end_time ?? 0))
  let start = dayjs().startOf('day')
  if (lastEnd > start.unix()) {
    start = isLocalDayStart(lastEnd)
      ? dayjs.unix(lastEnd)
      : dayjs.unix(lastEnd).startOf('day').add(1, 'day')
  }
  return {
    discount_percent: permanentPercent ?? 100,
    expression: 'permanent_discount',
    calculation_type: 'expression',
    start_time: start.unix(),
    end_time: start.add(7, 'day').unix(),
  }
}

export function UserModelPricingScheduleDialog(props: {
  value: PricingDraft
  onApply: (value: PricingDraft) => string | null
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<PricingDraft>(() => {
    const periods = props.value.periods?.map((period) => ({ ...period })) ?? []
    // 首次配置时直接给出一条待填写的临时折扣，省去先点“添加”再填写的空步骤。
    if (
      periods.length === 0 &&
      !props.value.expression &&
      props.value.discount_percent != null
    ) {
      periods.push(newTemporaryDiscount([], props.value.discount_percent))
    }
    return { ...props.value, mode: 'layered', periods }
  })
  const [error, setError] = useState<string | null>(null)
  const [originalDraft] = useState(() => JSON.stringify(draft))
  const [discardOpen, setDiscardOpen] = useState(false)
  const [invalidEditors, setInvalidEditors] = useState<Record<number, boolean>>(
    {}
  )
  const requestClose = () => {
    if (JSON.stringify(draft) !== originalDraft) setDiscardOpen(true)
    else props.onClose()
  }
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
    (draft.periods ?? []).map((_, index) => index)
  )
  const [visited, setVisited] = useState(() => new Set(keys.slice(-1)))
  const nextKey = useRef(keys.length)
  const schema = useMemo(() => createUserModelPricingFormSchema(t), [t])
  const validation = schema.safeParse({ items: [draft] })
  const issues = validation.success ? [] : validation.error.issues
  const fieldErrors = (position: number) => {
    const errors: Record<string, string> = {}
    for (const issue of issues) {
      if (issue.path[2] === 'periods' && issue.path[3] === position) {
        errors[String(issue.path.at(-1))] = issue.message
      }
    }
    return errors
  }
  // 折扣比例和时间规则的错误在各自的输入框旁提示，这里只汇总有效期等其余问题。
  const summaryIssue = issues.find(
    (issue) =>
      issue.path[2] !== 'periods' ||
      !['expression', 'discount_percent'].includes(String(issue.path.at(-1)))
  )
  const update = (position: number, changes: Partial<PeriodDraft>) => {
    const key = keys[position]
    setVisited((previous) =>
      previous.has(key) ? previous : new Set([...previous, key])
    )
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
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open) requestClose()
        }}
        title={t('Temporary discounts')}
        description={
          <span className='flex flex-wrap items-center gap-x-2 gap-y-1'>
            <span className='font-mono' translate='no'>
              {draft.model_name}
            </span>
            <span>
              {t('Permanent discount')}:{' '}
              {draft.expression
                ? t('Conditional discount')
                : `${String(draft.discount_percent ?? 100)}%`}
            </span>
          </span>
        }
        contentClassName='sm:max-w-3xl'
        footer={
          <>
            <Button type='button' variant='outline' onClick={requestClose}>
              {t('Cancel')}
            </Button>
            <Button
              type='button'
              disabled={
                !validation.success ||
                Boolean(error) ||
                keys.some((key) => invalidEditors[key])
              }
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
              {t('Apply to draft')}
            </Button>
          </>
        }
      >
        <div className='flex flex-col gap-4'>
          <p className='text-muted-foreground text-xs'>
            {t(
              'Temporary discounts take priority when conditions match within their validity period. Otherwise, the permanent discount applies.'
            )}
          </p>
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
            const errors = fieldErrors(index)
            const invalid =
              Object.keys(errors).length > 0 || invalidEditors[keys[index]]
            return (
              <Collapsible
                key={keys[index]}
                className='rounded-lg border'
                defaultOpen={
                  index === periods.length - 1 || Object.keys(errors).length > 0
                }
                onOpenChange={(open) => {
                  if (open) {
                    setVisited(
                      (previous) => new Set([...previous, keys[index]])
                    )
                  }
                }}
              >
                <div className='flex items-center gap-2 px-3 py-2'>
                  <CollapsibleTrigger
                    aria-invalid={invalid}
                    className='group aria-invalid:text-destructive flex min-w-0 flex-1 items-center gap-2 text-left text-sm'
                  >
                    <ChevronRight
                      className='size-4 shrink-0 transition-transform group-data-[panel-open]:rotate-90'
                      aria-hidden='true'
                    />
                    <span className='sr-only'>
                      {t('Temporary discount {{n}}', { n: index + 1 })}
                    </span>{' '}
                    <TemporaryDiscountSummary
                      period={period}
                      permanentPercent={draft.discount_percent}
                    />
                  </CollapsibleTrigger>
                  <PricingPeriodStatusBadge period={period} />
                  <Button
                    type='button'
                    variant='ghost'
                    size='icon-sm'
                    aria-label={t('Remove temporary discount {{n}}', {
                      n: index + 1,
                    })}
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
                <CollapsibleContent
                  keepMounted={visited.has(keys[index])}
                  className='border-t p-3 sm:p-4'
                >
                  <fieldset
                    disabled={Boolean(draft.expression)}
                    className='min-w-0 disabled:opacity-60'
                  >
                    <UserDiscountRuleEditor
                      id={`temporary-discount-${keys[index]}`}
                      value={period}
                      permanentPercent={draft.discount_percent}
                      errors={errors}
                      onChange={(changes) => update(index, changes)}
                      onValidityChange={(valid) =>
                        setInvalidEditors((previous) =>
                          previous[keys[index]] === !valid
                            ? previous
                            : { ...previous, [keys[index]]: !valid }
                        )
                      }
                    />
                  </fieldset>
                </CollapsibleContent>
              </Collapsible>
            )
          })}
          <Button
            type='button'
            variant='outline'
            size='sm'
            className='self-start'
            disabled={
              periods.length >= 1000 ||
              Boolean(draft.expression) ||
              draft.discount_percent == null
            }
            onClick={() => {
              const key = nextKey.current++
              setDraft((previous) => ({
                ...previous,
                periods: [
                  ...(previous.periods ?? []),
                  newTemporaryDiscount(
                    previous.periods ?? [],
                    previous.discount_percent
                  ),
                ],
              }))
              setVisited((previous) => new Set([...previous, key]))
              setKeys((previous) => [...previous, key])
              setError(null)
            }}
          >
            <Plus aria-hidden='true' />
            {t('Add temporary discount')}
          </Button>
          {(error || summaryIssue) && (
            <Alert variant='destructive'>
              <AlertDescription>
                {error || summaryIssue?.message}
              </AlertDescription>
            </Alert>
          )}
        </div>
      </Dialog>
      <ConfirmDialog
        open={discardOpen}
        onOpenChange={setDiscardOpen}
        title={t('Discard unsaved changes?')}
        desc={t('These changes have not been applied to the draft.')}
        confirmText={t('Discard changes')}
        cancelBtnText={t('Keep editing')}
        destructive
        handleConfirm={props.onClose}
      />
    </>
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
