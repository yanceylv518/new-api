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
import { ArrowDown, ArrowUp, Clock3, Plus, Trash2 } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
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
  inheritPermanentDiscount,
  isTimeDiscountConfig,
  parseDiscountExpression,
  type DiscountVisualConfig,
} from '../lib/user-discount-expression'
import {
  beijingTime,
  describeTimeBranch,
  matchesTimeBranch,
  timeBranchDiagnostics,
  timeToMinute,
} from '../lib/user-discount-time'
import type { UserModelPricingFormValues } from '../lib/user-model-pricing-form'

type DiscountRule = NonNullable<
  UserModelPricingFormValues['items'][number]['periods']
>[number]

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
const TIME_HOURS = Array.from({ length: 24 }, (_, hour) => {
  const value = String(hour).padStart(2, '0')
  return { value, label: value }
})
const TIME_MINUTES = Array.from({ length: 60 }, (_, minute) => {
  const value = String(minute).padStart(2, '0')
  return { value, label: value }
})

function DiscountChoice(props: {
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

function DiscountTimeInput(props: {
  id: string
  label: string
  value: string
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
  value: number | null | undefined
  onChange: (value: number | null) => void
}) {
  const { t } = useTranslation()
  const invalid = discountPercentLiteral(props.value ?? null) === null
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

export function UserDiscountRuleEditor(props: {
  value: DiscountRule
  permanentPercent: number | null | undefined
  id: string
  onChange: (changes: Partial<DiscountRule>) => void
  errors: Record<string, string>
  onValidityChange?: (valid: boolean) => void
}) {
  const { t } = useTranslation()
  const nextId = useRef(0)
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000))
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout>
    const tick = () => {
      setNow(Math.floor(Date.now() / 1000))
      timer = setTimeout(tick, 60000 - (Date.now() % 60000))
    }
    timer = setTimeout(tick, 60000 - (Date.now() % 60000))
    return () => clearTimeout(timer)
  }, [])
  const [initial] = useState(() => {
    const parsed = parseDiscountExpression(props.value.expression ?? '')
    const supported = parsed && isTimeDiscountConfig(parsed)
    return {
      unsupported: Boolean(props.value.expression?.trim() && !supported),
      visual:
        supported && parsed
          ? {
              ...parsed,
              branches: parsed.branches.map((branch) => ({
                ...branch,
                conditions: branch.conditions.map((condition) => ({
                  ...condition,
                  value: String(Number(condition.value)),
                })),
              })),
            }
          : {
              percent: props.permanentPercent ?? 100,
              branches: props.value.expression
                ? []
                : [
                    {
                      id: 'existing-fixed',
                      match: 'all' as const,
                      percent: props.value.discount_percent ?? 100,
                      conditions: [
                        {
                          id: 'existing-all-day',
                          field: DISCOUNT_TIME_FIELDS[0].value,
                          operator: '>=' as const,
                          value: '0',
                        },
                      ],
                    },
                  ],
            },
    }
  })
  const [unsupported, setUnsupported] = useState(initial.unsupported)
  const [visual, setVisual] = useState<DiscountVisualConfig>(
    inheritPermanentDiscount(initial.visual)
  )
  const diagnostics = useMemo(
    () => timeBranchDiagnostics(visual.branches),
    [visual.branches]
  )
  const visualValid =
    generateDiscountExpression(visual) !== null &&
    !diagnostics.some((item) => item.impossible || item.shadowed)
  const clock = beijingTime(now)
  const currentBranch = visual.branches.findIndex((branch) =>
    matchesTimeBranch(branch, clock.day, clock.minute)
  )
  const withinValidity =
    (props.value.start_time == null || now >= props.value.start_time) &&
    (props.value.end_time == null || now < props.value.end_time)
  useEffect(() => {
    props.onValidityChange?.(unsupported || visualValid)
  }, [unsupported, visualValid, props])
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
  const updateBranch = (
    index: number,
    changes: Partial<DiscountVisualConfig['branches'][number]>
  ) =>
    changeVisual({
      ...visual,
      branches: visual.branches.map((branch, position) =>
        position === index ? { ...branch, ...changes } : branch
      ),
    })
  const addBranch = (
    preset:
      | 'custom'
      | 'weekdays'
      | 'weekends'
      | 'daytime'
      | 'night'
      | 'advanced'
  ) => {
    if (visual.branches.length >= MAX_DISCOUNT_BRANCHES) return
    const weekday = DISCOUNT_TIME_FIELDS[2].value
    const hour = DISCOUNT_TIME_FIELDS[0].value
    let conditions: DiscountVisualConfig['branches'][number]['conditions'] = [
      {
        id: `draft-condition-${nextId.current++}`,
        field: hour,
        operator: '>=',
        value: '9',
      },
    ]
    if (preset === 'weekdays' || preset === 'weekends') {
      conditions = ['0', '6'].map((value) => ({
        id: `draft-condition-${nextId.current++}`,
        field: weekday,
        operator: preset === 'weekdays' ? '!=' : '==',
        value,
      }))
    } else if (preset === 'daytime') {
      conditions.push({
        id: `draft-condition-${nextId.current++}`,
        field: hour,
        operator: '<',
        value: '18',
      })
    }
    let days = [0, 1, 2, 3, 4, 5, 6]
    if (preset === 'weekdays') days = [1, 2, 3, 4, 5]
    if (preset === 'weekends') days = [6, 0]
    changeVisual({
      ...visual,
      branches: [
        ...visual.branches,
        {
          id: `draft-branch-${nextId.current++}`,
          match: preset === 'weekends' ? 'any' : 'all',
          percent: props.permanentPercent ?? 100,
          conditions: preset === 'advanced' ? conditions : [],
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
  }
  if (unsupported) {
    return (
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
    )
  }
  return (
    <div className='space-y-4'>
      <Field orientation='horizontal'>
        <FieldLabel htmlFor={`${props.id}-default`}>
          {t('Default discount')}
        </FieldLabel>
        <output
          id={`${props.id}-default`}
          className='font-mono text-lg font-semibold'
        >
          {props.permanentPercent == null ? '—' : `${props.permanentPercent}%`}
        </output>
      </Field>
      <p className='bg-muted/50 rounded-md px-3 py-2 text-sm' role='status'>
        {t('Current billing')}:{' '}
        <span className='font-mono font-semibold'>
          {withinValidity && currentBranch >= 0
            ? `${visual.branches[currentBranch].percent ?? '—'}%`
            : `${props.permanentPercent ?? '—'}%`}
        </span>
        <span className='text-muted-foreground ml-2 text-xs'>
          {t(
            withinValidity && currentBranch >= 0
              ? 'Time rule {{n}}'
              : 'Permanent discount',
            { n: currentBranch + 1 }
          )}
        </span>
      </p>
      <div className='space-y-3 border-t pt-4'>
        <div className='flex flex-wrap items-center justify-between gap-2'>
          <span className='flex items-center gap-2 text-sm font-medium'>
            <Clock3 className='size-4' aria-hidden='true' />
            {t('Time-based discounts')}
          </span>
          <Button
            type='button'
            variant='outline'
            size='sm'
            disabled={visual.branches.length >= MAX_DISCOUNT_BRANCHES}
            onClick={() => addBranch('custom')}
          >
            <Plus aria-hidden='true' />
            {t('Add time rule')}
          </Button>
        </div>
        <p className='text-muted-foreground text-xs'>
          {t(
            'Time ranges include the start and exclude the end. Overnight ranges belong to their starting weekday.'
          )}
        </p>
        <div className='flex flex-wrap items-center gap-2'>
          <span className='text-muted-foreground text-xs'>
            {t('Quick add')}
          </span>
          {(
            [
              ['weekdays', 'Weekday daytime'],
              ['weekends', 'Weekend all day'],
              ['daytime', 'Daily 09:00–18:00'],
              ['night', 'Daily overnight'],
            ] as const
          ).map(([preset, label]) => (
            <Button
              key={preset}
              type='button'
              variant='secondary'
              size='sm'
              disabled={visual.branches.length >= MAX_DISCOUNT_BRANCHES}
              onClick={() => addBranch(preset)}
            >
              {t(label)}
            </Button>
          ))}
          <Button
            type='button'
            variant='ghost'
            size='sm'
            disabled={visual.branches.length >= MAX_DISCOUNT_BRANCHES}
            onClick={() => addBranch('advanced')}
          >
            {t('Advanced conditions')}
          </Button>
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
      {visual.branches.map((branch, index) => (
        <section
          key={branch.id}
          aria-labelledby={`${props.id}-${branch.id}-title`}
          className='space-y-3 rounded-lg border p-3 sm:p-4'
        >
          <div className='flex items-center justify-between gap-2'>
            <h4
              id={`${props.id}-${branch.id}-title`}
              className='text-sm font-medium'
            >
              {t('Time rule {{n}}', { n: index + 1 })}
            </h4>
            <div className='flex gap-1'>
              {([-1, 1] as const).map((direction) => (
                <Button
                  key={direction}
                  type='button'
                  variant='ghost'
                  size='icon-sm'
                  aria-label={direction < 0 ? t('Move up') : t('Move down')}
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
                aria-label={t('Remove time rule')}
                onClick={() =>
                  changeVisual({
                    ...visual,
                    branches: visual.branches.filter(
                      (_, position) => position !== index
                    ),
                  })
                }
              >
                <Trash2 aria-hidden='true' />
              </Button>
            </div>
          </div>
          {branch.schedule && (
            <div className='space-y-3'>
              <Field data-invalid={!branch.schedule.days.length}>
                <FieldLabel>{t('Applicable weekdays')}</FieldLabel>
                <ToggleGroup
                  multiple
                  value={branch.schedule.days.map(String)}
                  variant='outline'
                  spacing={1}
                  className='w-full flex-wrap'
                  aria-label={t('Applicable weekdays')}
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
                  <FieldError>{t('Select at least one weekday')}</FieldError>
                )}
              </Field>
              <Field orientation='horizontal'>
                <FieldLabel htmlFor={`${props.id}-${index}-all-day`}>
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
              </Field>
              {!branch.schedule.allDay && (
                <div className='grid grid-cols-2 gap-3'>
                  {(['start', 'end'] as const).map((field) => (
                    <Field key={field}>
                      <FieldLabel htmlFor={`${props.id}-${index}-${field}`}>
                        {t(field === 'start' ? 'Start time' : 'End time')}
                        {field === 'end' &&
                          (timeToMinute(branch.schedule?.start ?? '') ?? 0) >
                            (timeToMinute(branch.schedule?.end ?? '') ?? 0) &&
                          ` · ${t('Next day')}`}
                      </FieldLabel>
                      <DiscountTimeInput
                        id={`${props.id}-${index}-${field}`}
                        label={t(field === 'start' ? 'Start time' : 'End time')}
                        value={branch.schedule?.[field] ?? ''}
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
              {!branch.schedule.allDay &&
                branch.schedule.start === branch.schedule.end && (
                  <FieldError>
                    {t('Start and end times must differ')}
                  </FieldError>
                )}
              <p className='text-muted-foreground text-xs'>
                {t('Beijing time')}
              </p>
            </div>
          )}
          {!branch.schedule && branch.conditions.length > 1 && (
            <DiscountChoice
              id={`${props.id}-${index}-match`}
              label={t('Apply when')}
              value={branch.match}
              items={[
                { value: 'all', label: t('All of the following match') },
                { value: 'any', label: t('Any of the following matches') },
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
            const updateCondition = (changes: Partial<typeof condition>) =>
              updateBranch(index, {
                conditions: branch.conditions.map((item, offset) =>
                  offset === position ? { ...item, ...changes } : item
                ),
              })
            const invalid = discountConditionLiteral(condition) === null
            const operators = DISCOUNT_COMPARISONS.filter(
              (operator) =>
                definition.value !== DISCOUNT_TIME_FIELDS[2].value ||
                operator === '==' ||
                operator === '!='
            )
            const values = Array.from(
              { length: definition.max + 1 },
              (_, value) => {
                let label = String(value).padStart(2, '0')
                if (definition.value === DISCOUNT_TIME_FIELDS[0].value) {
                  label += ':00'
                }
                if (definition.value === DISCOUNT_TIME_FIELDS[2].value) {
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
                          selected.value === DISCOUNT_TIME_FIELDS[2].value
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
                    const selected = operators.find((item) => item === operator)
                    if (selected) updateCondition({ operator: selected })
                  }}
                />
                <DiscountChoice
                  id={`${prefix}-value`}
                  label={t(definition.label)}
                  value={condition.value}
                  items={values}
                  invalid={invalid}
                  onChange={(value) => updateCondition({ value })}
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
              disabled={branch.conditions.length >= MAX_DISCOUNT_CONDITIONS}
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
          <DiscountPercentInput
            id={`${props.id}-${index}-percent`}
            label={t('Discount when matched')}
            value={branch.percent}
            onChange={(percent) => updateBranch(index, { percent })}
          />
          <p className='bg-muted/50 text-muted-foreground rounded-md p-2 text-xs'>
            {describeTimeBranch(branch, t)}
            {' → '}
            {t('Apply {{percent}}% discount', {
              percent: branch.percent ?? '—',
            })}
          </p>
          {(diagnostics[index]?.impossible ||
            diagnostics[index]?.shadowed ||
            diagnostics[index]?.overlapping) && (
            <p
              role='status'
              className={
                diagnostics[index]?.overlapping
                  ? 'text-warning text-xs'
                  : 'text-destructive text-xs'
              }
            >
              {diagnostics[index]?.impossible &&
                t(
                  'These conditions can never match. Adjust the weekdays, time range, or condition relation.'
                )}
              {diagnostics[index]?.shadowed &&
                t(
                  'Earlier rules cover this entire rule. Move it up or narrow the earlier rules.'
                )}
              {diagnostics[index]?.overlapping &&
                t(
                  'This rule overlaps earlier rules. Earlier matching rules take priority.'
                )}
            </p>
          )}
        </section>
      ))}
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
    </div>
  )
}
