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
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import axios from 'axios'
import i18next from 'i18next'
import { useState } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { ManagementRateLimitSection } from '../management-rate-limit-section'

const defaults = {
  global_api: { enabled: true, requests: 360, window_seconds: 180 },
  login_ip: { enabled: true, requests: 20, window_seconds: 1200 },
  login_account: { enabled: true, requests: 20, window_seconds: 1200 },
  session_refresh: { enabled: true, requests: 60, window_seconds: 60 },
  token_query: { enabled: true, requests: 60, window_seconds: 60 },
  sensitive: { enabled: true, requests: 20, window_seconds: 1200 },
}

function Fixture({ value = JSON.stringify(defaults) }: { value?: string }) {
  const [actions, setActions] = useState<HTMLDivElement | null>(null)
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: { retry: false },
          mutations: { retry: false },
        },
      })
  )
  return (
    <QueryClientProvider client={client}>
      <SettingsPageProvider
        actionsContainer={actions}
        suppressSectionHeader={false}
      >
        <div ref={setActions} />
        <ManagementRateLimitSection value={value} />
      </SettingsPageProvider>
    </QueryClientProvider>
  )
}

afterEach(async () => {
  await i18next.changeLanguage('en')
  vi.restoreAllMocks()
})

describe('management rate limit settings', () => {
  test('loads each independent limit and saves one complete configuration', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    render(<Fixture />)
    expect(screen.getAllByRole('switch')).toHaveLength(6)
    const save = screen.getByRole('button', { name: 'Save Changes' })
    expect(save).toBeDisabled()
    const query = within(
      screen.getByRole('group', { name: 'Token usage and logs (per account)' })
    )
    fireEvent.change(
      query.getByRole('spinbutton', { name: 'Max requests per period' }),
      { target: { value: '5' } }
    )
    await userEvent.click(save)
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
    const [, request] = put.mock.calls[0]
    expect(request).toEqual({
      key: 'ManagementRateLimits',
      expected_value: JSON.stringify(defaults),
      value: JSON.stringify({
        ...defaults,
        token_query: { ...defaults.token_query, requests: 5 },
      }),
    })
    await waitFor(() => expect(save).toBeDisabled())
  })

  test.each(['0', '2147483648', '1.5', ''])(
    'an invalid period %s is rejected without sending an update',
    async (value) => {
      const put = vi
        .spyOn(api, 'put')
        .mockResolvedValue({ data: { success: true } })
      render(<Fixture />)
      const query = within(
        screen.getByRole('group', {
          name: 'Token usage and logs (per account)',
        })
      )
      fireEvent.change(
        query.getByRole('spinbutton', { name: 'Window (seconds)' }),
        { target: { value } }
      )
      await userEvent.click(
        screen.getByRole('button', { name: 'Save Changes' })
      )
      expect(
        await query.findByText('Enter a whole number between 1 and 2147483647')
      ).toBeVisible()
      expect(put).not.toHaveBeenCalled()
    }
  )

  test('a failed save preserves edits and can be retried', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockRejectedValueOnce(new Error('settings request failed'))
      .mockResolvedValueOnce({ data: { success: true } })
    render(<Fixture />)
    const query = within(
      screen.getByRole('group', { name: 'Token usage and logs (per account)' })
    )
    const count = query.getByRole('spinbutton', {
      name: 'Max requests per period',
    })
    fireEvent.change(count, { target: { value: '7' } })
    const save = screen.getByRole('button', { name: 'Save Changes' })
    await userEvent.click(save)
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(save).toBeEnabled())
    expect(count).toHaveValue(7)
    await userEvent.click(save)
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    expect(put.mock.calls[1][1]).toEqual(put.mock.calls[0][1])
    await waitFor(() => expect(save).toBeDisabled())
  })

  test('disabling one policy preserves its values and other policies', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    render(<Fixture />)
    const query = within(
      screen.getByRole('group', { name: 'Token usage and logs (per account)' })
    )
    await userEvent.click(query.getByRole('switch'))
    expect(
      query.getByRole('spinbutton', { name: 'Max requests per period' })
    ).toBeDisabled()
    const login = within(
      screen.getByRole('group', {
        name: 'Password login attempts (per account)',
      })
    )
    expect(
      login.getByRole('spinbutton', { name: 'Max requests per period' })
    ).toBeEnabled()
    await userEvent.click(screen.getByRole('button', { name: 'Save Changes' }))
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/option/', {
        key: 'ManagementRateLimits',
        expected_value: JSON.stringify(defaults),
        value: JSON.stringify({
          ...defaults,
          token_query: { ...defaults.token_query, enabled: false },
        }),
      })
    )
  })

  test('invalid server configuration shows an error instead of editable defaults', () => {
    render(<Fixture value='{}' />)
    expect(
      screen.getByText('Unable to load management rate limits')
    ).toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Save Changes' })
    ).not.toBeInTheDocument()
  })

  test('Chinese labels are available for the controls and validation', async () => {
    i18next.addResourceBundle('zh', 'translation', zh.translation)
    await i18next.changeLanguage('zh')
    render(<Fixture />)
    expect(
      screen.getByRole('heading', { name: '管理接口限流' })
    ).toBeInTheDocument()
    const query = within(
      screen.getByRole('group', { name: '令牌用量与日志查询（按账号）' })
    )
    expect(
      query.getByRole('spinbutton', { name: '限流周期（秒）' })
    ).toHaveValue(60)
    fireEvent.change(
      query.getByRole('spinbutton', { name: '限流周期（秒）' }),
      {
        target: { value: '0' },
      }
    )
    expect(
      await query.findByText('请输入 1 到 2147483647 之间的整数')
    ).toBeVisible()
  })

  test('a stale save preserves edits and reloads the conflicting configuration before retrying', async () => {
    const changed = {
      ...defaults,
      login_ip: { ...defaults.login_ip, requests: 40 },
    }
    const conflictError = new axios.AxiosError(
      'conflict',
      undefined,
      undefined,
      undefined,
      {
        status: 409,
        statusText: 'Conflict',
        config: { headers: new axios.AxiosHeaders() },
        headers: {},
        data: {
          success: false,
          code: 'MANAGEMENT_RATE_LIMITS_CONFLICT',
          data: { value: JSON.stringify(changed) },
        },
      }
    )
    const put = vi
      .spyOn(api, 'put')
      .mockRejectedValueOnce(conflictError)
      .mockResolvedValueOnce({ data: { success: true } })
    const view = render(<Fixture />)
    const query = within(
      screen.getByRole('group', { name: 'Token usage and logs (per account)' })
    )
    const count = query.getByRole('spinbutton', {
      name: 'Max requests per period',
    })
    fireEvent.change(count, { target: { value: '7' } })
    view.rerender(<Fixture value={JSON.stringify(changed)} />)
    expect(count).toHaveValue(7)
    const save = screen.getByRole('button', { name: 'Save Changes' })
    await userEvent.click(save)
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Rate limits changed elsewhere.'
    )
    expect(count).toHaveValue(7)
    expect(save).toBeDisabled()
    expect(put.mock.calls[0][1]).toMatchObject({
      expected_value: JSON.stringify(defaults),
    })
    await userEvent.click(
      screen.getByRole('button', { name: 'Reload saved limits' })
    )
    expect(count).toHaveValue(60)
    const login = within(
      screen.getByRole('group', { name: 'Login attempts (per IP)' })
    )
    expect(
      login.getByRole('spinbutton', { name: 'Max requests per period' })
    ).toHaveValue(40)
    fireEvent.change(count, { target: { value: '7' } })
    await userEvent.click(save)
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    expect(put.mock.calls[1][1]).toEqual({
      key: 'ManagementRateLimits',
      expected_value: JSON.stringify(changed),
      value: JSON.stringify({
        ...changed,
        token_query: { ...changed.token_query, requests: 7 },
      }),
    })
  })

  test('legacy one-hour windows can be loaded and preserved when saving another policy', async () => {
    const legacy = {
      ...defaults,
      global_api: {
        ...defaults.global_api,
        requests: 2_000_000,
        window_seconds: 3600,
      },
    }
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    render(<Fixture value={JSON.stringify(legacy)} />)
    const global = within(
      screen.getByRole('group', { name: 'Global API requests (per IP)' })
    )
    expect(
      global.getByRole('spinbutton', { name: 'Window (seconds)' })
    ).toHaveValue(3600)
    const query = within(
      screen.getByRole('group', { name: 'Token usage and logs (per account)' })
    )
    fireEvent.change(
      query.getByRole('spinbutton', { name: 'Max requests per period' }),
      { target: { value: '5' } }
    )
    await userEvent.click(screen.getByRole('button', { name: 'Save Changes' }))
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
    expect(put.mock.calls[0][1]).toEqual({
      key: 'ManagementRateLimits',
      expected_value: JSON.stringify(legacy),
      value: JSON.stringify({
        ...legacy,
        token_query: { ...legacy.token_query, requests: 5 },
      }),
    })
  })
})
