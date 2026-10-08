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
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18n from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import {
  PricingPeriodStatusBadge,
  UserModelPricingScheduleDialog,
} from '../user-model-pricing-schedule'

const firstStart = new Date('2030-10-08T10:00:00+08:00').getTime() / 1000
const secondStart = firstStart + 3600

beforeEach(async () => {
  await i18n.changeLanguage('en')
})
afterEach(cleanup)

describe('discount schedule editing', () => {
  test('accepts one permanent period without an end-time input', () => {
    render(
      <I18nextProvider i18n={i18n}>
        <UserModelPricingScheduleDialog
          value={{
            model_name: 'test-model',
            mode: 'single',
            discount_percent: 80,
            start_time: firstStart,
            end_time: null,
          }}
          onApply={() => null}
          onClose={() => undefined}
        />
      </I18nextProvider>
    )
    expect(screen.getByRole('button', { name: 'Apply' })).toBeEnabled()
    expect(screen.queryByLabelText('End time (Time)')).not.toBeInTheDocument()
    expect(
      screen.getByText('Blank end time means permanent.')
    ).toBeInTheDocument()
  })

  test('blocks two permanent periods and aligns the first end only after explicit action', async () => {
    const onApply = vi.fn(() => null)
    render(
      <I18nextProvider i18n={i18n}>
        <UserModelPricingScheduleDialog
          value={{
            model_name: 'test-model',
            mode: 'scheduled',
            discount_percent: 80,
            periods: [
              { discount_percent: 80, start_time: firstStart, end_time: null },
              { discount_percent: 60, start_time: secondStart, end_time: null },
            ],
          }}
          onApply={onApply}
          onClose={() => undefined}
        />
      </I18nextProvider>
    )
    const ends = screen.getAllByRole('button', { name: 'End time' })
    expect(ends[0]).toHaveAttribute('aria-invalid', 'true')
    expect(ends[1]).toHaveAttribute('aria-invalid', 'false')
    expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
    expect(onApply).not.toHaveBeenCalled()
    await userEvent.click(
      screen.getByRole('button', { name: 'End at next start' })
    )
    expect(ends[0]).toHaveAttribute('aria-invalid', 'false')
    expect(screen.getByRole('button', { name: 'Apply' })).toBeEnabled()
    await userEvent.click(screen.getByRole('button', { name: 'Apply' }))
    expect(onApply).toHaveBeenCalledWith(
      expect.objectContaining({
        periods: [
          {
            discount_percent: 80,
            start_time: firstStart,
            end_time: secondStart,
          },
          { discount_percent: 60, start_time: secondStart, end_time: null },
        ],
      })
    )
  })

  test('blocks apply when a previously valid start-time input is cleared and permits correction', () => {
    render(
      <I18nextProvider i18n={i18n}>
        <UserModelPricingScheduleDialog
          value={{
            model_name: 'test-model',
            mode: 'single',
            discount_percent: 80,
            start_time: firstStart,
            end_time: null,
          }}
          onApply={() => null}
          onClose={() => undefined}
        />
      </I18nextProvider>
    )
    const time = screen.getByLabelText('Start time (Time)')
    fireEvent.change(time, { target: { value: '' } })
    expect(screen.getByRole('button', { name: 'Start time' })).toHaveAttribute(
      'aria-invalid',
      'true'
    )
    expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
    fireEvent.change(time, { target: { value: '12:00' } })
    expect(screen.getByRole('button', { name: 'Apply' })).toBeEnabled()
  })

  test('allows adding a second period without silently shortening the permanent first period', async () => {
    render(
      <I18nextProvider i18n={i18n}>
        <UserModelPricingScheduleDialog
          value={{
            model_name: 'test-model',
            mode: 'scheduled',
            discount_percent: 80,
            periods: [
              { discount_percent: 80, start_time: firstStart, end_time: null },
            ],
          }}
          onApply={() => null}
          onClose={() => undefined}
        />
      </I18nextProvider>
    )
    await userEvent.click(screen.getByRole('button', { name: 'Add period' }))
    expect(
      screen.getAllByRole('button', { name: 'End time' })[0]
    ).toHaveAttribute('aria-invalid', 'true')
    expect(screen.getByRole('button', { name: 'Apply' })).toBeDisabled()
    expect(screen.queryAllByLabelText('End time (Time)')).toHaveLength(0)
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
  expect(screen.getByText('Active')).toHaveClass('text-success')
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
