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
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18n from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { parseDiscountExpression } from '../../lib/user-discount-expression'
import {
  PricingPeriodStatusBadge,
  UserModelPricingScheduleDialog,
} from '../user-model-pricing-schedule'

const firstStart = new Date('2030-10-08T10:00:00+08:00').getTime() / 1000
const secondStart = firstStart + 3600

beforeEach(async () => {
  await i18n.changeLanguage('en')
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

function renderSchedule(
  value: Parameters<typeof UserModelPricingScheduleDialog>[0]['value'] = {
    model_name: 'test-model',
    mode: 'layered',
    discount_percent: 80,
    periods: [],
  }
) {
  const onApply = vi.fn<
    Parameters<typeof UserModelPricingScheduleDialog>[0]['onApply']
  >(() => null)
  const onClose = vi.fn()
  render(
    <I18nextProvider i18n={i18n}>
      <UserModelPricingScheduleDialog
        value={value}
        onApply={onApply}
        onClose={onClose}
      />
    </I18nextProvider>
  )
  return { onApply, onClose }
}

describe('permanent and temporary discount editors', () => {
  test('blocks applying when the external permanent percentage has excess precision', () => {
    const { onApply } = renderSchedule({
      model_name: 'test-model',
      mode: 'layered',
      discount_percent: 80.001,
      periods: [],
    })
    expect(
      screen.getByRole('button', { name: 'Apply to draft' })
    ).toBeDisabled()
    expect(onApply).not.toHaveBeenCalled()
  })
  test('updates draft status and fallback summary at natural time boundaries', () => {
    vi.useFakeTimers()
    vi.setSystemTime(firstStart * 1000 - 1000)
    renderSchedule({
      model_name: 'model',
      mode: 'layered',
      discount_percent: 80,
      periods: [
        {
          discount_percent: 60,
          start_time: firstStart,
          end_time: firstStart + 1,
        },
      ],
    })
    expect(screen.getByText('Not started')).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(1100)
    })
    expect(screen.getByText('Within validity period')).toBeInTheDocument()
    expect(
      screen.getByText(
        'Temporary period active. Unmatched conditions use the permanent discount: 80%.'
      )
    ).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(screen.getByText('Expired')).toBeInTheDocument()
    expect(screen.getByText('Permanent discount: 80%.')).toBeInTheDocument()
  })
  test('requires an end time after modifying the start of a legacy unbounded period', async () => {
    renderSchedule({
      model_name: 'model',
      mode: 'layered',
      discount_percent: 80,
      periods: [
        {
          discount_percent: 60,
          start_time: firstStart,
          end_time: null,
          legacy_unbounded: true,
        },
      ],
    })
    expect(screen.getByRole('button', { name: 'Apply to draft' })).toBeEnabled()
    const timeInput = screen.getByLabelText('Start time (Time)')
    fireEvent.change(timeInput, { target: { value: '11:00' } })
    expect(
      screen.getByRole('button', { name: 'Apply to draft' })
    ).toBeDisabled()
    expect(
      screen.getAllByText('Temporary discounts require an end time').length
    ).toBeGreaterThan(0)
  })
  async function temporary(value?: Parameters<typeof renderSchedule>[0]) {
    const result = renderSchedule(
      value ?? {
        model_name: 'test-model',
        mode: 'layered',
        discount_percent: 80,
        periods: [
          {
            expression: 'permanent_discount',
            start_time: firstStart,
            end_time: secondStart,
          },
        ],
      }
    )
    return result
  }

  test('opens temporary settings without tabs or a permanent editor', () => {
    renderSchedule()
    expect(screen.queryByRole('tab')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('spinbutton', { name: 'Permanent discount' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Advanced conditions' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('tab', { name: 'Expression editor' })
    ).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Apply to draft' })).toBeEnabled()
  })

  test('preserves matching temporary discounts after the permanent value changes externally', async () => {
    const { onApply } = await temporary()
    expect(screen.getByLabelText('Default discount')).toHaveTextContent('80%')
    expect(screen.getByLabelText('Default discount')).toHaveProperty(
      'tagName',
      'OUTPUT'
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Weekday daytime' })
    )
    fireEvent.change(
      screen.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '60' } }
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    const applied = onApply.mock.calls.at(-1)?.[0]
    if (!applied) throw new Error('Temporary discounts were not applied')
    cleanup()
    const reopened = renderSchedule({ ...applied, discount_percent: 75 })
    expect(screen.getByLabelText('Default discount')).toHaveTextContent('75%')
    expect(
      screen.getByRole('spinbutton', { name: 'Discount when matched' })
    ).toHaveValue(60)
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(reopened.onApply).toHaveBeenCalledWith(
      expect.objectContaining({
        discount_percent: 75,
        periods: [
          expect.objectContaining({
            expression:
              '(((weekday("Asia/Shanghai") != 0 && weekday("Asia/Shanghai") != 6) && (hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai")) >= 540 && (hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai")) < 1080)) ? 0.6 : permanent_discount',
          }),
        ],
      })
    )
  })

  test('keeps an incomplete conditional percentage and blocks apply until corrected', async () => {
    await temporary()
    await userEvent.click(
      screen.getByRole('button', { name: 'Advanced conditions' })
    )
    fireEvent.change(
      screen.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '' } }
    )
    expect(
      screen.getByRole('button', { name: 'Apply to draft' })
    ).toBeDisabled()
    expect(
      screen.getByRole('button', { name: 'Apply to draft' })
    ).toBeDisabled()
    expect(
      screen.getByRole('spinbutton', { name: 'Discount when matched' })
    ).toHaveValue(null)
    fireEvent.change(
      screen.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '60' } }
    )
    expect(screen.getByRole('button', { name: 'Apply to draft' })).toBeEnabled()
  })

  test('uses only readable time fields and weekday equality comparisons', async () => {
    await temporary()
    expect(
      screen.queryByRole('textbox', { name: 'Discount expression' })
    ).not.toBeInTheDocument()
    await userEvent.click(
      screen.getByRole('button', { name: 'Advanced conditions' })
    )
    await userEvent.click(
      screen.getByRole('combobox', { name: 'Time condition' })
    )
    expect(
      screen.getAllByRole('option').map((option) => option.textContent)
    ).toEqual(['Daily hour', 'Minute within each hour', 'Day of week'])
    await userEvent.click(screen.getByRole('option', { name: 'Day of week' }))
    await userEvent.click(screen.getByRole('combobox', { name: 'Day of week' }))
    expect(screen.getAllByRole('option')).toHaveLength(7)
    await userEvent.click(screen.getByRole('option', { name: 'Friday' }))
    await userEvent.click(screen.getByRole('combobox', { name: 'Comparison' }))
    expect(
      screen.getAllByRole('option').map((option) => option.textContent)
    ).toEqual(['=', '≠'])
    await userEvent.click(screen.getByRole('option', { name: '=' }))
  })

  test('reselecting the current time field preserves comparison and value', async () => {
    const { onApply } = await temporary()
    await userEvent.click(
      screen.getByRole('button', { name: 'Advanced conditions' })
    )
    await userEvent.click(
      screen.getAllByRole('combobox', { name: 'Time condition' })[0]
    )
    await userEvent.click(screen.getByRole('option', { name: 'Daily hour' }))
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenCalledWith(
      expect.objectContaining({
        periods: [
          expect.objectContaining({
            expression:
              '(hour("Asia/Shanghai") >= 9) ? 0.8 : permanent_discount',
          }),
        ],
      })
    )
  })

  test('edits a minute condition and preserves it when applied', async () => {
    const { onApply } = await temporary()
    await userEvent.click(
      screen.getByRole('button', { name: 'Advanced conditions' })
    )
    await userEvent.click(
      screen.getByRole('combobox', { name: 'Time condition' })
    )
    await userEvent.click(
      screen.getByRole('option', { name: 'Minute within each hour' })
    )
    await userEvent.click(screen.getByRole('combobox', { name: 'Comparison' }))
    await userEvent.click(screen.getByRole('option', { name: '<' }))
    await userEvent.click(
      screen.getByRole('combobox', { name: 'Minute within each hour' })
    )
    await userEvent.click(screen.getByRole('option', { name: '30' }))
    fireEvent.change(
      screen.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '29' } }
    )
    expect(
      screen.getByRole('combobox', { name: 'Minute within each hour' })
    ).toHaveTextContent('30')
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenCalledWith(
      expect.objectContaining({
        periods: [
          expect.objectContaining({
            expression:
              '(minute("Asia/Shanghai") < 30) ? 0.29 : permanent_discount',
          }),
        ],
      })
    )
  })

  test('preserves existing permanent conditions without exposing a permanent editor', async () => {
    const source = '(hour("Asia/Shanghai") >= 9) ? 0.6 : 0.8'
    const { onApply } = renderSchedule({
      model_name: 'test-model',
      mode: 'layered',
      discount_percent: 80,
      expression: source,
      periods: [],
    })
    expect(
      screen.queryByRole('spinbutton', { name: 'Permanent discount' })
    ).not.toBeInTheDocument()
    expect(
      screen.getByRole('button', { name: 'Add temporary discount' })
    ).toBeDisabled()
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenLastCalledWith(
      expect.objectContaining({ expression: source, discount_percent: 80 })
    )
  })

  test('preserves unsupported temporary source until explicitly replaced', async () => {
    const source = 'min(0.8, max(0.4, output_tokens / 10000))'
    const { onApply } = await temporary({
      model_name: 'test-model',
      mode: 'layered',
      discount_percent: 80,
      periods: [
        { expression: source, start_time: firstStart, end_time: secondStart },
      ],
    })
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenLastCalledWith(
      expect.objectContaining({
        periods: [expect.objectContaining({ expression: source })],
      })
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Replace with time conditions' })
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenLastCalledWith(
      expect.objectContaining({
        periods: [
          expect.objectContaining({ expression: 'permanent_discount' }),
        ],
      })
    )
  })

  test.each([
    [
      'Weekday daytime',
      { days: [1, 2, 3, 4, 5], start: '09:00', end: '18:00', allDay: false },
    ],
    [
      'Weekend all day',
      { days: [0, 6], start: '09:00', end: '18:00', allDay: true },
    ],
    [
      'Daily 09:00–18:00',
      {
        days: [0, 1, 2, 3, 4, 5, 6],
        start: '09:00',
        end: '18:00',
        allDay: false,
      },
    ],
  ])(
    'saves the %s preset with an inherited fallback',
    async (preset, schedule) => {
      const { onApply } = await temporary()
      await userEvent.click(screen.getByRole('button', { name: preset }))
      fireEvent.change(
        screen.getByRole('spinbutton', { name: 'Discount when matched' }),
        { target: { value: '60' } }
      )
      await userEvent.click(
        screen.getByRole('button', { name: 'Apply to draft' })
      )
      const expression = onApply.mock.calls.at(-1)?.[0].periods?.[0].expression
      const parsed = parseDiscountExpression(expression ?? '')
      expect(parsed?.inheritPermanent).toBe(true)
      expect(parsed?.branches[0]).toMatchObject({ percent: 60, schedule })
    }
  )

  test('reorders and removes rules without changing their matching discounts', async () => {
    const { onApply } = await temporary()
    await userEvent.click(
      screen.getByRole('button', { name: 'Weekday daytime' })
    )
    fireEvent.change(
      screen.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '60' } }
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Daily 09:00–18:00' })
    )
    const second = within(screen.getByRole('region', { name: 'Time rule 2' }))
    fireEvent.change(
      second.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '50' } }
    )
    await userEvent.click(second.getByRole('button', { name: 'Move up' }))
    expect(
      within(screen.getByRole('region', { name: 'Time rule 1' })).getByRole(
        'spinbutton',
        { name: 'Discount when matched' }
      )
    ).toHaveValue(50)
    await userEvent.click(
      within(screen.getByRole('region', { name: 'Time rule 2' })).getByRole(
        'button',
        { name: 'Remove time rule' }
      )
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenCalledWith(
      expect.objectContaining({
        periods: [
          expect.objectContaining({
            expression:
              '((weekday("Asia/Shanghai") >= 0 && (hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai")) >= 540 && (hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai")) < 1080)) ? 0.5 : permanent_discount',
          }),
        ],
      })
    )
  })

  test('enforces rule and condition limits and returns to permanent after removing all conditions', async () => {
    const { onApply } = await temporary()
    for (let i = 0; i < 4; i++) {
      await userEvent.click(
        screen.getByRole('button', { name: 'Advanced conditions' })
      )
    }
    expect(
      screen.getByRole('button', { name: 'Advanced conditions' })
    ).toBeDisabled()
    const first = within(screen.getByRole('region', { name: 'Time rule 1' }))
    for (let i = 1; i < 4; i++) {
      await userEvent.click(
        first.getByRole('button', { name: 'Add time condition' })
      )
    }
    expect(
      first.getByRole('button', { name: 'Add time condition' })
    ).toBeDisabled()
    await userEvent.click(
      first.getAllByRole('button', { name: 'Remove condition' })[0]
    )
    expect(
      first.getByRole('button', { name: 'Add time condition' })
    ).toBeEnabled()
    for (let i = 0; i < 4; i++) {
      await userEvent.click(
        screen.getAllByRole('button', { name: 'Remove time rule' })[0]
      )
    }
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenCalledWith(
      expect.objectContaining({
        periods: [
          expect.objectContaining({ expression: 'permanent_discount' }),
        ],
      })
    )
  })

  test('overlaps and missing ends block applying temporary discounts', async () => {
    renderSchedule({
      model_name: 'test-model',
      mode: 'layered',
      discount_percent: 80,
      periods: [
        {
          discount_percent: 50,
          start_time: firstStart,
          end_time: secondStart + 1,
        },
        { discount_percent: 60, start_time: secondStart, end_time: null },
      ],
    })
    expect(
      screen.getByRole('button', { name: 'Apply to draft' })
    ).toBeDisabled()
    expect(
      screen.getAllByRole('button', { name: 'End time' })[0]
    ).toHaveAttribute('aria-invalid', 'true')
    expect(
      screen.getAllByRole('button', { name: 'End time' })[1]
    ).toHaveAttribute('aria-invalid', 'true')
    await userEvent.click(
      screen.getAllByRole('button', { name: 'Remove period' })[1]
    )
    expect(screen.getByRole('button', { name: 'Apply to draft' })).toBeEnabled()
  })

  test('retains a fixed full-price temporary override', async () => {
    const { onApply } = await temporary({
      model_name: 'test-model',
      mode: 'layered',
      discount_percent: 80,
      periods: [
        {
          discount_percent: 100,
          start_time: firstStart,
          end_time: secondStart,
        },
      ],
    })
    expect(
      screen.getByRole('spinbutton', { name: 'Discount when matched' })
    ).toHaveValue(100)
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply).toHaveBeenCalledWith(
      expect.objectContaining({
        periods: [expect.objectContaining({ discount_percent: 100 })],
      })
    )
  })

  test('sets weekdays and a minute-precision range without boolean condition controls', async () => {
    const { onApply } = await temporary()
    await userEvent.click(
      screen.getByRole('button', { name: 'Weekday daytime' })
    )
    const first = within(screen.getByRole('region', { name: 'Time rule 1' }))
    expect(first.queryByRole('combobox', { name: 'Comparison' })).toBeNull()
    fireEvent.change(first.getByLabelText('Start time'), {
      target: { value: '09:30' },
    })
    fireEvent.change(first.getByLabelText('End time'), {
      target: { value: '23:00' },
    })
    fireEvent.change(
      first.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '70' } }
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Weekend all day' })
    )
    const second = within(screen.getByRole('region', { name: 'Time rule 2' }))
    fireEvent.change(
      second.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '60' } }
    )
    expect(screen.getByRole('button', { name: 'Apply to draft' })).toBeEnabled()
    await userEvent.click(
      screen.getByRole('button', { name: 'Apply to draft' })
    )
    expect(onApply.mock.calls.at(-1)?.[0].periods?.[0].expression).toContain(
      '>= 570'
    )
    expect(onApply.mock.calls.at(-1)?.[0].periods?.[0].expression).toContain(
      '? 0.7 :'
    )
    expect(onApply.mock.calls.at(-1)?.[0].periods?.[0].expression).toContain(
      '? 0.6 : permanent_discount'
    )
  })

  test('keeps incomplete edits when a period is collapsed and confirms discarding them', async () => {
    const { onClose } = await temporary()
    await userEvent.click(screen.getByRole('button', { name: 'Add time rule' }))
    fireEvent.change(
      screen.getByRole('spinbutton', { name: 'Discount when matched' }),
      { target: { value: '' } }
    )
    await userEvent.click(screen.getByRole('button', { name: 'Period 1' }))
    await userEvent.click(screen.getByRole('button', { name: 'Period 1' }))
    expect(
      screen.getByRole('spinbutton', { name: 'Discount when matched' })
    ).toHaveValue(null)
    expect(
      screen.getByRole('button', { name: 'Apply to draft' })
    ).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(onClose).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Keep editing' }))
    expect(onClose).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    await userEvent.click(
      screen.getByRole('button', { name: 'Discard changes' })
    )
    expect(onClose).toHaveBeenCalledOnce()
  })

  test('supports keyboard cancellation without applying', async () => {
    const { onApply, onClose } = renderSchedule()
    screen.getByRole('button', { name: 'Cancel' }).focus()
    await userEvent.keyboard('{Enter}')
    expect(onApply).not.toHaveBeenCalled()
    expect(onClose).toHaveBeenCalledOnce()
  })
})

test('status switches at inclusive start and exclusive end with the requested colors', () => {
  const period = { start_time: 100, end_time: 200 }
  const { rerender } = render(
    <I18nextProvider i18n={i18n}>
      <PricingPeriodStatusBadge period={period} now={99} />
    </I18nextProvider>
  )
  expect(screen.getByText('Not started')).toHaveAttribute(
    'data-variant',
    'warning'
  )
  rerender(
    <I18nextProvider i18n={i18n}>
      <PricingPeriodStatusBadge period={period} now={100} />
    </I18nextProvider>
  )
  expect(screen.getByText('Within validity period')).toHaveClass('text-success')
  rerender(
    <I18nextProvider i18n={i18n}>
      <PricingPeriodStatusBadge period={period} now={200} />
    </I18nextProvider>
  )
  expect(screen.getByText('Expired')).toHaveAttribute(
    'data-variant',
    'destructive'
  )
})
