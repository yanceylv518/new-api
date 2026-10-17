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
import {
  ArrowDown,
  ArrowUp,
  ChevronDown,
  ChevronRight,
  Clock3,
  Plus,
  SlidersHorizontal,
  Trash2,
} from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Field, FieldError, FieldLabel } from '@/components/ui/field'
import { Switch } from '@/components/ui/switch'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'

import {
  DISCOUNT_COMPARISONS,
  DISCOUNT_TIME_FIELDS,
  MAX_DISCOUNT_BRANCHES,
  MAX_DISCOUNT_CONDITIONS,
  discountConditionLiteral,
  discountPercentLiteral,
  generateDiscountExpression,
  type DiscountBranch,
  type DiscountVisualConfig,
} from '../lib/user-discount-expression'
import {
  describeTimeBranch,
  timeBranchDiagnostics,
  timeToMinute,
} from '../lib/user-discount-time'
import {
  ALL_DAY_EVERY_DAY,
  isSimpleTemporaryDiscount,
  temporaryDiscountVisual,
} from '../lib/user-temporary-discount'
import {
  DiscountChoice,
  DiscountPercentInput,
  DiscountTimeInput,
} from './user-discount-inputs'
import {
  TemporaryDiscountSimpleFields,
  TemporaryDiscountValidityFields,
  type TemporaryDiscountRule,
} from './user-temporary-discount-fields'

const WEEKDAYS = [
  'Sunday',
  'Monday',
  'Tuesday',
  'Wednesday',
  'Thursday',
  'Friday',
  'Saturday',
] as const
const COMPARISON_LABELS = {
  '==': '=',
  '!=': '≠',
  '>=': '≥',
  '>': '>',
  '<=': '≤',
  '<': '<',
} as const
/** 单个临时折扣的编辑器：默认按天设置，高级自定义提供星期、多条规则和条件。 */
export function UserDiscountRuleEditor(props: {
  value: TemporaryDiscountRule
  permanentPercent: number | null | undefined
  id: string
  onChange: (changes: Partial<TemporaryDiscountRule>) => void
  errors: Record<string, string>
  onValidityChange?: (valid: boolean) => void
}) {
  const { t } = useTranslation()
  const nextId = useRef(0)
  const [initial] = useState(() =>
    temporaryDiscountVisual(props.value, props.permanentPercent)
  )
  const [unsupported, setUnsupported] = useState(initial.unsupported)
  const [visual, setVisual] = useState<DiscountVisualConfig>(initial.visual)
  const [advanced, setAdvanced] = useState(
    initial.unsupported || !isSimpleTemporaryDiscount(initial.visual)
  )
  const [expandedBranch, setExpandedBranch] = useState<string | null>(
    initial.visual.branches[0]?.id ?? null
  )
  const diagnostics = useMemo(
    () => timeBranchDiagnostics(visual.branches),
    [visual.branches]
  )
  const visualValid =
    generateDiscountExpression(visual) !== null &&
    !diagnostics.some((item) => item.impossible || item.shadowed)
  // 简单表单必须填写折扣；高级模式沿用“没有规则即使用永久折扣”的原有语义。
  const valid =
    unsupported || (visualValid && (advanced || visual.branches.length > 0))
  useEffect(() => {
    props.onValidityChange?.(valid)
  }, [valid, props])
  const changeVisual = (config: DiscountVisualConfig) => {
    setVisual(config)
    const expression = generateDiscountExpression({
      ...config,
      inheritPermanent: true,
    })
    props.onChange({
      discount_percent: props.permanentPercent,
      calculation_type: 'expression',
      expression: expression ?? '',
    })
  }
  const updateBranch = (index: number, changes: Partial<DiscountBranch>) =>
    changeVisual({
      ...visual,
      branches: visual.branches.map((branch, position) =>
        position === index ? { ...branch, ...changes } : branch
      ),
    })
  const addBranch = (
    preset: 'weekdays' | 'weekends' | 'daytime' | 'night' | 'advanced'
  ) => {
    if (visual.branches.length >= MAX_DISCOUNT_BRANCHES) return
    const hour = DISCOUNT_TIME_FIELDS[0].value
    const conditions: DiscountBranch['conditions'] =
      preset === 'advanced'
        ? [
            {
              id: `draft-condition-${nextId.current++}`,
              field: hour,
              operator: '>=',
              value: '9',
            },
          ]
        : []
    let days = [0, 1, 2, 3, 4, 5, 6]
    if (preset === 'weekdays') days = [1, 2, 3, 4, 5]
    if (preset === 'weekends') days = [6, 0]
    const branchId = `draft-branch-${nextId.current++}`
    changeVisual({
      ...visual,
      branches: [
        ...visual.branches,
        {
          id: branchId,
          match: preset === 'weekends' ? 'any' : 'all',
          percent: props.permanentPercent ?? 100,
          conditions,
          ...(preset === 'advanced'
            ? {}
            : {
                schedule: {
                  days,
                  start: preset === 'night' ? '23:00' : '09:00',
                  end: preset === 'night' ? '07:00' : '18:00',
                  allDay: preset === 'weekends',
                },
              }),
        },
      ],
    })
    setExpandedBranch(branchId)
  }
  if (unsupported) {
    return (
      <div className='space-y-4'>
        <Alert>
          <AlertDescription className='space-y-3'>
            <p>
              {t(
                'This saved rule cannot be edited with time conditions. It stays unchanged until you choose to replace it.'
              )}
            </p>
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={() => {
                setUnsupported(false)
                changeVisual({
                  percent: props.permanentPercent ?? 100,
                  inheritPermanent: true,
                  branches: [],
                })
              }}
            >
              {t('Replace with time conditions')}
            </Button>
          </AlertDescription>
        </Alert>
        <TemporaryDiscountValidityFields
          id={props.id}
          value={props.value}
          errors={props.errors}
          onChange={props.onChange}
        />
      </div>
    )
  }
  if (!advanced) {
    return (
      <div className='space-y-4'>
        <TemporaryDiscountSimpleFields
          id={props.id}
          branch={visual.branches[0]}
          permanentPercent={props.permanentPercent}
          value={props.value}
          errors={props.errors}
          onChange={props.onChange}
          onBranchChange={(changes) =>
            changeVisual({
              ...visual,
              branches: [
                {
                  ...(visual.branches[0] ?? {
                    id: `draft-branch-${nextId.current++}`,
                    match: 'all',
                    percent: null,
                    conditions: [],
                    schedule: { ...ALL_DAY_EVERY_DAY },
                  }),
                  ...changes,
                },
              ],
            })
          }
        />
        <Button
          type='button'
          variant='ghost'
          size='sm'
          onClick={() => {
            setExpandedBranch(visual.branches[0]?.id ?? null)
            setAdvanced(true)
          }}
        >
          <SlidersHorizontal aria-hidden='true' />
          {t('Advanced customization')}
        </Button>
      </div>
    )
  }
  return (
    <div className='space-y-4'>
      <TemporaryDiscountValidityFields
        id={props.id}
        value={props.value}
        errors={props.errors}
        onChange={props.onChange}
      />
      <div className='space-y-3'>
        <div className='flex flex-wrap items-center justify-between gap-2'>
          <span className='flex items-center gap-2 text-sm font-medium'>
            <Clock3 className='size-4' aria-hidden='true' />
            {t('Time-based discounts')}
          </span>
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  disabled={visual.branches.length >= MAX_DISCOUNT_BRANCHES}
                />
              }
            >
              <Plus aria-hidden='true' />
              {t('Add time rule')}
              <ChevronDown aria-hidden='true' />
            </DropdownMenuTrigger>
            <DropdownMenuContent align='end' className='w-56'>
              <DropdownMenuGroup>
                {(
                  [
                    ['weekdays', 'Weekday daytime'],
                    ['weekends', 'Weekend all day'],
                    ['daytime', 'Daily 09:00–18:00'],
                    ['night', 'Daily overnight'],
                  ] as const
                ).map(([preset, label]) => (
                  <DropdownMenuItem
                    key={preset}
                    onClick={() => addBranch(preset)}
                  >
                    {t(label)}
                  </DropdownMenuItem>
                ))}
              </DropdownMenuGroup>
              <DropdownMenuSeparator />
              <DropdownMenuItem onClick={() => addBranch('advanced')}>
                <SlidersHorizontal aria-hidden='true' />
                {t('Custom conditions')}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
        {visual.branches.length === 0 && (
          <p className='text-muted-foreground rounded-lg border border-dashed p-3 text-sm'>
            {t('No time rules. The default discount applies at all times.')}
          </p>
        )}
        {visual.branches.length > 1 && (
          <p className='text-muted-foreground text-xs'>
            {t(
              'Rules are checked from top to bottom. Only the first matching rule applies; discounts are not combined.'
            )}
          </p>
        )}
      </div>
      {visual.branches.length > 0 && (
        <div className='divide-y rounded-lg border'>
          {visual.branches.map((branch, index) => {
            const schedule = branch.schedule
            const startMinute = timeToMinute(schedule?.start ?? '')
            const endMinute = timeToMinute(schedule?.end ?? '')
            const invalidTimes = Boolean(
              schedule &&
              !schedule.allDay &&
              (startMinute === null ||
                endMinute === null ||
                startMinute === endMinute)
            )
            let timeError: string | null = null
            if (invalidTimes) {
              timeError =
                startMinute === endMinute && startMinute !== null
                  ? t('Start and end times must differ')
                  : t('Enter a valid start and end time.')
            }
            let branchError: string | null = null
            if (discountPercentLiteral(branch.percent) === null) {
              branchError = t(
                'Enter a percentage from 0.01 to 100, with up to two decimal places.'
              )
            } else if (schedule && schedule.days.length === 0) {
              branchError = t('Select at least one weekday')
            } else if (timeError) {
              branchError = timeError
            } else if (
              branch.conditions.some(
                (condition) => discountConditionLiteral(condition) === null
              )
            ) {
              branchError = t(
                'Complete the time rules before applying this discount.'
              )
            } else if (diagnostics[index]?.impossible) {
              branchError = t(
                'These conditions can never match. Adjust the weekdays, time range, or condition relation.'
              )
            } else if (diagnostics[index]?.shadowed) {
              branchError = t(
                'Earlier rules cover this entire rule. Move it up or narrow the earlier rules.'
              )
            }
            const issueMessage =
              branchError ??
              (diagnostics[index]?.overlapping
                ? t(
                    'This rule overlaps earlier rules. Earlier matching rules take priority.'
                  )
                : null)
            const issueId = `${props.id}-${branch.id}-issue`
            return (
              <section
                key={branch.id}
                aria-labelledby={`${props.id}-${branch.id}-title`}
                data-invalid={Boolean(branchError)}
                className='data-[invalid=true]:bg-destructive/5 min-w-0'
              >
                <Collapsible
                  open={expandedBranch === branch.id}
                  onOpenChange={(open) =>
                    setExpandedBranch(open ? branch.id : null)
                  }
                >
                  <div className='flex items-center gap-1 pr-2'>
                    <h4
                      id={`${props.id}-${branch.id}-title`}
                      className='sr-only'
                    >
                      {t('Time rule {{n}}', { n: index + 1 })}
                    </h4>
                    <CollapsibleTrigger
                      aria-invalid={Boolean(branchError)}
                      aria-labelledby={`${props.id}-${branch.id}-title ${props.id}-${branch.id}-summary ${props.id}-${branch.id}-percent-summary`}
                      aria-describedby={issueMessage ? issueId : undefined}
                      className='group focus-visible:ring-ring/50 flex min-w-0 flex-1 items-center gap-2 rounded-md px-3 py-3 text-left outline-none focus-visible:ring-2'
                    >
                      <ChevronRight
                        aria-hidden='true'
                        className='text-muted-foreground size-4 shrink-0 transition-transform group-aria-expanded:rotate-90'
                      />
                      <span
                        aria-label={`${t('Priority')} ${index + 1}`}
                        className='bg-muted text-muted-foreground flex size-6 shrink-0 items-center justify-center rounded-md font-mono text-xs'
                      >
                        {index + 1}
                      </span>
                      <span
                        id={`${props.id}-${branch.id}-summary`}
                        className='min-w-0 flex-1 truncate text-sm'
                        title={describeTimeBranch(branch, t)}
                      >
                        {describeTimeBranch(branch, t)}
                      </span>
                      {branchError && (
                        <span className='text-destructive shrink-0 text-xs'>
                          {t('Needs correction')}
                        </span>
                      )}
                      <span
                        id={`${props.id}-${branch.id}-percent-summary`}
                        className='shrink-0 font-mono text-sm font-semibold tabular-nums'
                      >
                        {branch.percent == null ? '—' : `${branch.percent}%`}
                      </span>
                    </CollapsibleTrigger>
                    <div className='flex shrink-0 gap-0.5'>
                      {([-1, 1] as const).map((direction) => (
                        <Button
                          key={direction}
                          type='button'
                          variant='ghost'
                          size='icon-sm'
                          aria-label={`${direction < 0 ? t('Move up') : t('Move down')} · ${t('Time rule {{n}}', { n: index + 1 })}`}
                          disabled={
                            index + direction < 0 ||
                            index + direction >= visual.branches.length
                          }
                          onClick={() => {
                            const branches = [...visual.branches]
                            ;[branches[index], branches[index + direction]] = [
                              branches[index + direction],
                              branches[index],
                            ]
                            changeVisual({ ...visual, branches })
                          }}
                        >
                          {direction < 0 ? (
                            <ArrowUp aria-hidden='true' />
                          ) : (
                            <ArrowDown aria-hidden='true' />
                          )}
                        </Button>
                      ))}
                      <Button
                        type='button'
                        variant='ghost'
                        size='icon-sm'
                        aria-label={`${t('Remove time rule')} · ${t('Time rule {{n}}', { n: index + 1 })}`}
                        onClick={() => {
                          const branches = visual.branches.filter(
                            (_, position) => position !== index
                          )
                          changeVisual({
                            ...visual,
                            branches,
                          })
                          if (expandedBranch === branch.id) {
                            setExpandedBranch(
                              branches[Math.min(index, branches.length - 1)]
                                ?.id ?? null
                            )
                          }
                        }}
                      >
                        <Trash2 aria-hidden='true' />
                      </Button>
                    </div>
                  </div>
                  {issueMessage && (
                    <p
                      id={issueId}
                      role='status'
                      className={
                        branchError
                          ? 'text-destructive px-3 pb-3 text-xs'
                          : 'text-warning px-3 pb-3 text-xs'
                      }
                    >
                      {issueMessage}
                    </p>
                  )}
                  <CollapsibleContent
                    keepMounted
                    className='space-y-3 border-t p-3 sm:p-4'
                  >
                    {branch.schedule && (
                      <div className='space-y-3'>
                        <Field data-invalid={!branch.schedule.days.length}>
                          <div className='flex flex-wrap items-center justify-between gap-2'>
                            <FieldLabel>{t('Applicable weekdays')}</FieldLabel>
                            <div className='flex items-center gap-1'>
                              {(
                                [
                                  ['Every day', [0, 1, 2, 3, 4, 5, 6]],
                                  ['Weekdays', [1, 2, 3, 4, 5]],
                                  ['Weekends', [0, 6]],
                                ] as const
                              ).map(([label, days]) => (
                                <Button
                                  key={label}
                                  type='button'
                                  variant='outline'
                                  size='sm'
                                  className='aria-pressed:border-primary aria-pressed:bg-primary aria-pressed:text-primary-foreground'
                                  aria-pressed={
                                    branch.schedule?.days.length ===
                                      days.length &&
                                    days.every((day) =>
                                      branch.schedule?.days.includes(day)
                                    )
                                  }
                                  onClick={() => {
                                    if (
                                      branch.schedule &&
                                      !(
                                        branch.schedule.days.length ===
                                          days.length &&
                                        days.every((day) =>
                                          branch.schedule?.days.includes(day)
                                        )
                                      )
                                    ) {
                                      updateBranch(index, {
                                        schedule: {
                                          ...branch.schedule,
                                          days: [...days],
                                        },
                                      })
                                    }
                                  }}
                                >
                                  {t(label)}
                                </Button>
                              ))}
                            </div>
                          </div>
                          <ToggleGroup
                            multiple
                            value={branch.schedule.days.map(String)}
                            variant='outline'
                            spacing={1}
                            className='w-full flex-wrap'
                            aria-label={t('Applicable weekdays')}
                            aria-invalid={!branch.schedule.days.length}
                            aria-describedby={
                              branch.schedule.days.length === 0
                                ? `${props.id}-${branch.id}-weekday-error`
                                : undefined
                            }
                            onValueChange={(values) => {
                              if (branch.schedule) {
                                updateBranch(index, {
                                  schedule: {
                                    ...branch.schedule,
                                    days: values.map(Number),
                                  },
                                })
                              }
                            }}
                          >
                            {[1, 2, 3, 4, 5, 6, 0].map((day) => (
                              <ToggleGroupItem
                                key={day}
                                value={String(day)}
                                aria-label={t(WEEKDAYS[day])}
                                className='aria-pressed:border-primary aria-pressed:bg-primary aria-pressed:text-primary-foreground aria-pressed:hover:bg-primary/90 aria-pressed:hover:text-primary-foreground'
                              >
                                {t(WEEKDAYS[day])}
                              </ToggleGroupItem>
                            ))}
                          </ToggleGroup>
                          {!branch.schedule.days.length && (
                            <FieldError
                              id={`${props.id}-${branch.id}-weekday-error`}
                            >
                              {t('Select at least one weekday')}
                            </FieldError>
                          )}
                        </Field>
                        <Field>
                          <div className='flex items-center justify-between gap-3'>
                            <span className='text-sm font-medium'>
                              {t('Daily hours')}
                            </span>
                            <div className='flex items-center gap-2'>
                              <FieldLabel
                                htmlFor={`${props.id}-${index}-all-day`}
                              >
                                {t('All day')}
                              </FieldLabel>
                              <Switch
                                id={`${props.id}-${index}-all-day`}
                                checked={branch.schedule.allDay}
                                onCheckedChange={(allDay) => {
                                  if (branch.schedule) {
                                    updateBranch(index, {
                                      schedule: { ...branch.schedule, allDay },
                                    })
                                  }
                                }}
                              />
                            </div>
                          </div>
                          {!branch.schedule.allDay && (
                            <div className='grid grid-cols-2 gap-3'>
                              {(['start', 'end'] as const).map((field) => (
                                <Field
                                  key={field}
                                  data-invalid={
                                    field === 'start'
                                      ? startMinute === null ||
                                        startMinute === endMinute
                                      : endMinute === null ||
                                        startMinute === endMinute
                                  }
                                >
                                  <FieldLabel
                                    htmlFor={`${props.id}-${index}-${field}`}
                                  >
                                    {t(
                                      field === 'start'
                                        ? 'Start time'
                                        : 'End time'
                                    )}
                                    {field === 'end' &&
                                      (timeToMinute(
                                        branch.schedule?.start ?? ''
                                      ) ?? 0) >
                                        (timeToMinute(
                                          branch.schedule?.end ?? ''
                                        ) ?? 0) &&
                                      ` · ${t('Next day')}`}
                                  </FieldLabel>
                                  <DiscountTimeInput
                                    id={`${props.id}-${index}-${field}`}
                                    label={t(
                                      field === 'start'
                                        ? 'Start time'
                                        : 'End time'
                                    )}
                                    value={branch.schedule?.[field] ?? ''}
                                    invalid={
                                      field === 'start'
                                        ? startMinute === null ||
                                          startMinute === endMinute
                                        : endMinute === null ||
                                          startMinute === endMinute
                                    }
                                    aria-describedby={
                                      timeError
                                        ? `${props.id}-${branch.id}-time-error`
                                        : undefined
                                    }
                                    onChange={(value) => {
                                      if (branch.schedule) {
                                        updateBranch(index, {
                                          schedule: {
                                            ...branch.schedule,
                                            [field]: value,
                                          },
                                        })
                                      }
                                    }}
                                  />
                                </Field>
                              ))}
                            </div>
                          )}
                          {timeError && (
                            <FieldError
                              id={`${props.id}-${branch.id}-time-error`}
                            >
                              {timeError}
                            </FieldError>
                          )}
                          {!branch.schedule.allDay && (
                            <p className='text-muted-foreground text-xs'>
                              {t(
                                'Time ranges include the start and exclude the end. Overnight ranges belong to their starting weekday.'
                              )}
                            </p>
                          )}
                        </Field>
                      </div>
                    )}
                    {!branch.schedule && (
                      <Collapsible defaultOpen>
                        <CollapsibleTrigger
                          render={
                            <Button
                              type='button'
                              variant='ghost'
                              size='sm'
                              className='group'
                            />
                          }
                        >
                          <ChevronRight
                            aria-hidden='true'
                            className='size-4 transition-transform group-aria-expanded:rotate-90'
                          />
                          {t('Custom conditions')}
                        </CollapsibleTrigger>
                        <CollapsibleContent
                          keepMounted
                          className='space-y-3 pt-3'
                        >
                          {!branch.schedule && branch.conditions.length > 1 && (
                            <DiscountChoice
                              id={`${props.id}-${index}-match`}
                              label={t('Apply when')}
                              value={branch.match}
                              items={[
                                {
                                  value: 'all',
                                  label: t('All of the following match'),
                                },
                                {
                                  value: 'any',
                                  label: t('Any of the following matches'),
                                },
                              ]}
                              onChange={(match) => {
                                if (match === 'all' || match === 'any') {
                                  updateBranch(index, { match })
                                }
                              }}
                            />
                          )}
                          {branch.conditions.map((condition, position) => {
                            const definition = DISCOUNT_TIME_FIELDS.find(
                              (field) => field.value === condition.field
                            )
                            if (!definition) return null
                            const prefix = `${props.id}-${index}-${position}`
                            const updateCondition = (
                              changes: Partial<typeof condition>
                            ) =>
                              updateBranch(index, {
                                conditions: branch.conditions.map(
                                  (item, offset) =>
                                    offset === position
                                      ? { ...item, ...changes }
                                      : item
                                ),
                              })
                            const invalid =
                              discountConditionLiteral(condition) === null
                            const operators = DISCOUNT_COMPARISONS.filter(
                              (operator) =>
                                definition.value !==
                                  DISCOUNT_TIME_FIELDS[2].value ||
                                operator === '==' ||
                                operator === '!='
                            )
                            const values = Array.from(
                              { length: definition.max + 1 },
                              (_, value) => {
                                let label = String(value).padStart(2, '0')
                                if (
                                  definition.value ===
                                  DISCOUNT_TIME_FIELDS[0].value
                                ) {
                                  label += ':00'
                                }
                                if (
                                  definition.value ===
                                  DISCOUNT_TIME_FIELDS[2].value
                                ) {
                                  label = t(WEEKDAYS[value])
                                }
                                return { value: String(value), label }
                              }
                            )
                            return (
                              <div
                                key={condition.id}
                                className='grid items-end gap-2 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)_auto]'
                              >
                                <DiscountChoice
                                  id={`${prefix}-field`}
                                  label={t('Time condition')}
                                  value={condition.field}
                                  items={DISCOUNT_TIME_FIELDS.map((field) => ({
                                    value: field.value,
                                    label: t(field.label),
                                  }))}
                                  onChange={(field) => {
                                    if (field === condition.field) return
                                    const selected = DISCOUNT_TIME_FIELDS.find(
                                      (item) => item.value === field
                                    )
                                    if (selected) {
                                      updateCondition({
                                        field: selected.value,
                                        operator: '==',
                                        value:
                                          selected.value ===
                                          DISCOUNT_TIME_FIELDS[2].value
                                            ? '1'
                                            : '0',
                                      })
                                    }
                                  }}
                                />
                                <DiscountChoice
                                  id={`${prefix}-operator`}
                                  label={t('Comparison')}
                                  value={condition.operator}
                                  items={operators.map((operator) => ({
                                    value: operator,
                                    label: COMPARISON_LABELS[operator],
                                  }))}
                                  onChange={(operator) => {
                                    const selected = operators.find(
                                      (item) => item === operator
                                    )
                                    if (selected) {
                                      updateCondition({ operator: selected })
                                    }
                                  }}
                                />
                                <DiscountChoice
                                  id={`${prefix}-value`}
                                  label={t(definition.label)}
                                  value={condition.value}
                                  items={values}
                                  invalid={invalid}
                                  onChange={(value) =>
                                    updateCondition({ value })
                                  }
                                />
                                <Button
                                  type='button'
                                  variant='ghost'
                                  size='icon'
                                  aria-label={t('Remove condition')}
                                  disabled={branch.conditions.length === 1}
                                  onClick={() =>
                                    updateBranch(index, {
                                      conditions: branch.conditions.filter(
                                        (_, offset) => offset !== position
                                      ),
                                    })
                                  }
                                >
                                  <Trash2 aria-hidden='true' />
                                </Button>
                              </div>
                            )
                          })}
                          {!branch.schedule && (
                            <Button
                              type='button'
                              variant='ghost'
                              size='sm'
                              disabled={
                                branch.conditions.length >=
                                MAX_DISCOUNT_CONDITIONS
                              }
                              onClick={() =>
                                updateBranch(index, {
                                  conditions: [
                                    ...branch.conditions,
                                    {
                                      id: `draft-condition-${nextId.current++}`,
                                      field: DISCOUNT_TIME_FIELDS[2].value,
                                      operator: '==',
                                      value: '1',
                                    },
                                  ],
                                })
                              }
                            >
                              <Plus aria-hidden='true' />
                              {t('Add time condition')}
                            </Button>
                          )}
                        </CollapsibleContent>
                      </Collapsible>
                    )}
                    <DiscountPercentInput
                      id={`${props.id}-${index}-percent`}
                      label={t('Discount when matched')}
                      value={branch.percent}
                      onChange={(percent) => updateBranch(index, { percent })}
                    />
                  </CollapsibleContent>
                </Collapsible>
              </section>
            )
          })}
        </div>
      )}
      {visual.branches.length > 0 && (
        <p className='text-muted-foreground text-xs'>
          {t('Unmatched conditions use the permanent discount: {{percent}}%.', {
            percent: props.permanentPercent ?? 100,
          })}
        </p>
      )}
      {!visualValid && (
        <FieldError>
          {t(
            'Complete all conditions and enter discount percentages from 0.01 to 100 with at most two decimal places.'
          )}
        </FieldError>
      )}
      {(props.errors.expression || props.errors.discount_percent) && (
        <FieldError>
          {props.errors.expression
            ? t('Complete the time rules before applying this discount.')
            : props.errors.discount_percent}
        </FieldError>
      )}
      {isSimpleTemporaryDiscount(visual) && (
        <Button
          type='button'
          variant='ghost'
          size='sm'
          onClick={() => setAdvanced(false)}
        >
          {t('Back to simple settings')}
        </Button>
      )}
    </div>
  )
}
