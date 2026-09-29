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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { beforeEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { LogSettingsSection } from '../log-settings-section'

function Fixture(props: { enabled: boolean }) {
  const [container, setContainer] = useState<HTMLDivElement | null>(null)
  return (
    <>
      <div ref={setContainer} />
      <SettingsPageProvider actionsContainer={container}>
        <LogSettingsSection
          defaultEnabled
          defaultRequestSnapshotEnabled={props.enabled}
        />
      </SettingsPageProvider>
    </>
  )
}

beforeEach(() => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: null },
  })
  vi.spyOn(api, 'put').mockResolvedValue({ data: { success: true } })
})

test.each([true, false])(
  'saving the request body switch from %s persists its boolean value',
  async (enabled) => {
    const user = userEvent.setup()
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    })
    const result = render(
      <QueryClientProvider client={client}>
        <Fixture enabled={enabled} />
      </QueryClientProvider>
    )
    const toggle = screen.getByRole('switch', {
      name: 'Save video request bodies',
    })
    expect(toggle).toHaveAttribute('aria-checked', String(enabled))
    await user.click(toggle)
    await user.click(screen.getByRole('button', { name: 'Save log settings' }))
    await waitFor(() =>
      expect(api.put).toHaveBeenCalledWith('/api/option/', {
        key: 'TaskRequestSnapshotEnabled',
        value: !enabled,
      })
    )
    expect(api.put).toHaveBeenCalledTimes(1)
    result.rerender(
      <QueryClientProvider client={client}>
        <Fixture enabled={!enabled} />
      </QueryClientProvider>
    )
    expect(toggle).toHaveAttribute('aria-checked', String(!enabled))
    result.unmount()
    client.clear()
  }
)

test('saving unchanged switches does not write settings', async () => {
  const user = userEvent.setup()
  const client = new QueryClient()
  const result = render(
    <QueryClientProvider client={client}>
      <Fixture enabled />
    </QueryClientProvider>
  )
  await user.click(screen.getByRole('button', { name: 'Save log settings' }))
  expect(api.put).not.toHaveBeenCalled()
  result.unmount()
  client.clear()
})
