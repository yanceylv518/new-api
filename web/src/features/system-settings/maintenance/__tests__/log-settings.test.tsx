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
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { LogSettingsSection } from '../log-settings-section'

let toolbar: HTMLDivElement

beforeEach(() => {
  toolbar = document.createElement('div')
  document.body.appendChild(toolbar)
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: null },
  })
})

afterEach(() => {
  cleanup()
  toolbar.remove()
})

function mountLogSettings(enabled = false) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <SettingsPageProvider actionsContainer={toolbar}>
        <LogSettingsSection defaultEnabled defaultTimelineEnabled={enabled} />
      </SettingsPageProvider>
    </QueryClientProvider>
  )
}

describe('stream timeline system option', () => {
  it('defaults to off and saves only the timeline option after a keyboard toggle', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    mountLogSettings()
    const timeline = screen.getByRole('switch', {
      name: 'Record streaming request timeline',
    })
    expect(timeline).toHaveAttribute('aria-checked', 'false')
    expect(
      screen.getByRole('switch', { name: 'Record quota usage' })
    ).toHaveAttribute('aria-checked', 'true')
    timeline.focus()
    await user.keyboard(' ')
    expect(timeline).toHaveAttribute('aria-checked', 'true')
    await user.click(screen.getByRole('button', { name: 'Save log settings' }))
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/option/', {
        key: 'RequestTimelineEnabled',
        value: true,
      })
    )
    expect(put).toHaveBeenCalledTimes(1)
  })

  it('loads a saved enabled value and can disable collection', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    mountLogSettings(true)
    const timeline = screen.getByRole('switch', {
      name: 'Record streaming request timeline',
    })
    expect(timeline).toHaveAttribute('aria-checked', 'true')
    await user.click(timeline)
    await user.click(screen.getByRole('button', { name: 'Save log settings' }))
    await waitFor(() =>
      expect(put).toHaveBeenCalledWith('/api/option/', {
        key: 'RequestTimelineEnabled',
        value: false,
      })
    )
    expect(put).toHaveBeenCalledTimes(1)
  })

  it('keeps the changed value available for retry when saving fails', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockRejectedValueOnce(new Error('offline'))
      .mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    mountLogSettings()
    await user.click(
      screen.getByRole('switch', { name: 'Record streaming request timeline' })
    )
    await user.click(screen.getByRole('button', { name: 'Save log settings' }))
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
    const save = await screen.findByRole('button', {
      name: 'Save log settings',
    })
    await waitFor(() => expect(save).toBeEnabled())
    expect(
      screen.getByRole('switch', { name: 'Record streaming request timeline' })
    ).toHaveAttribute('aria-checked', 'true')
    await user.click(save)
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
  })
})
