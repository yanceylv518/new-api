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
import { useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLabel,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

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
  '==': 'Equals',
  '!=': 'Does not equal',
  '>=': 'At or after',
  '>': 'Time is after',
  '<=': 'At or before',
  '<': 'Time is before',
} as const

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
}) {
  const { t } = useTranslation()
  const nextId = useRef(0)
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
  const visualValid = generateDiscountExpression(visual) !== null
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
    preset: 'custom' | 'weekdays' | 'weekends' | 'daytime'
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
    changeVisual({
      ...visual,
      branches: [
        ...visual.branches,
        {
          id: `draft-branch-${nextId.current++}`,
          match: preset === 'weekends' ? 'any' : 'all',
          percent: props.permanentPercent ?? 100,
          conditions,
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
      <Field>
        <FieldLabel htmlFor={`${props.id}-default`}>
          {t('Default discount')}
        </FieldLabel>
        <Input
          id={`${props.id}-default`}
          readOnly
          value={
            props.permanentPercent == null ? '—' : `${props.permanentPercent}%`
          }
        />
        <FieldDescription>
          {t(
            'Automatically follows the permanent discount when no time condition matches.'
          )}
        </FieldDescription>
      </Field>
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
            'Choose when a different discount applies. Rules use the request start time in Beijing time and repeat every week.'
          )}
        </p>
        <p className='text-muted-foreground text-xs'>
          {t(
            'Hour conditions cover the whole hour. For example, 09:00–18:00 includes 09:00 and excludes 18:00.'
          )}
        </p>
        <div className='flex flex-wrap items-center gap-2'>
          <span className='text-muted-foreground text-xs'>
            {t('Quick add')}
          </span>
          {(
            [
              ['weekdays', 'Weekdays'],
              ['weekends', 'Weekends'],
              ['daytime', 'Daily 09:00–18:00'],
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
          {branch.conditions.length > 1 && (
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
                    label: t(COMPARISON_LABELS[operator]),
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
          <DiscountPercentInput
            id={`${props.id}-${index}-percent`}
            label={t('Discount when matched')}
            value={branch.percent}
            onChange={(percent) => updateBranch(index, { percent })}
          />
          <p className='bg-muted/50 text-muted-foreground rounded-md p-2 text-xs'>
            {branch.conditions
              .map((condition) => {
                const field = DISCOUNT_TIME_FIELDS.find(
                  (item) => item.value === condition.field
                )
                let value = condition.value.padStart(2, '0')
                if (field?.value === DISCOUNT_TIME_FIELDS[0].value) {
                  value += ':00'
                }
                if (field?.value === DISCOUNT_TIME_FIELDS[2].value) {
                  value = t(WEEKDAYS[Number(condition.value)])
                }
                return `${t(field?.label ?? 'Time condition')} ${t(COMPARISON_LABELS[condition.operator])} ${value}`
              })
              .join(branch.match === 'all' ? t(' · and · ') : t(' · or · '))}
            {' → '}
            {t('Apply {{percent}}% discount', {
              percent: branch.percent ?? '—',
            })}
          </p>
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
