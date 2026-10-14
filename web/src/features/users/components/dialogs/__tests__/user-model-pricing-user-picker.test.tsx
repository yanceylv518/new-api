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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18n from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { UserModelPricingUserPicker } from '../user-model-pricing-user-picker'

const user = (id: number, username: string, role: number) => ({
  id,
  username,
  display_name: username,
  email: `${username}@example.com`,
  quota: 0,
  used_quota: 0,
  request_count: 0,
  group: 'default',
  status: 1,
  role,
})
const recentUsers = [
  user(2, 'alice', 1),
  user(3, 'other-admin', 10),
  user(1, 'root', 100),
]
const originalGet = api.get
let requestedUrls: string[] = []

function renderPicker() {
  const onSelect = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <UserModelPricingUserPicker
          open
          onOpenChange={() => undefined}
          onSelect={onSelect}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  return { onSelect }
}

beforeEach(async () => {
  await i18n.changeLanguage('en')
  requestedUrls = []
  useAuthStore.getState().auth.setUser({ id: 9, username: 'admin', role: 10 })
  // 只替换网络边界：无关键字返回最近用户，搜索返回匹配用户。
  api.get = (async (url: string) => {
    requestedUrls.push(url)
    const items = url.startsWith('/api/user/search')
      ? [user(7, 'bob', 1)]
      : recentUsers
    return { data: { success: true, data: { items, total: items.length } } }
  }) as typeof api.get
})
afterEach(() => {
  cleanup()
  api.get = originalGet
  useAuthStore.getState().auth.setUser(null)
})

describe('user picker for adding discounts', () => {
  test('lists recent users and returns the selected user', async () => {
    const { onSelect } = renderPicker()
    await userEvent.click(
      await screen.findByRole('button', { name: 'Set discounts for alice' })
    )
    expect(onSelect).toHaveBeenCalledWith({ id: 2, username: 'alice' })
  })

  test('disables users the operator is not allowed to manage', async () => {
    renderPicker()
    expect(
      await screen.findByRole('button', { name: 'Set discounts for alice' })
    ).toBeEnabled()
    expect(
      screen.getByRole('button', { name: 'Set discounts for other-admin' })
    ).toBeDisabled()
    expect(
      screen.getByRole('button', { name: 'Set discounts for root' })
    ).toBeDisabled()
  })

  test('lets a root operator manage admins but not root accounts', async () => {
    useAuthStore.getState().auth.setUser({ id: 1, username: 'root', role: 100 })
    renderPicker()
    expect(
      await screen.findByRole('button', {
        name: 'Set discounts for other-admin',
      })
    ).toBeEnabled()
    expect(
      screen.getByRole('button', { name: 'Set discounts for root' })
    ).toBeDisabled()
  })

  test('searches by keyword and replaces the candidates', async () => {
    renderPicker()
    await screen.findByRole('button', { name: 'Set discounts for alice' })
    await userEvent.type(
      screen.getByRole('textbox', { name: 'Search users' }),
      'bob'
    )
    expect(
      await screen.findByRole('button', { name: 'Set discounts for bob' })
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Set discounts for alice' })
    ).not.toBeInTheDocument()
    expect(requestedUrls.at(-1)).toContain('keyword=bob')
  })
})
