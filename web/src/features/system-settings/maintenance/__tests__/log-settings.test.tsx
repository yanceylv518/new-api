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
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import {
  useSystemOptions,
  getOptionValue,
} from '../../hooks/use-system-options'
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

function mountLogSettings(enabled = false, liveOptions = false) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <SettingsPageProvider actionsContainer={toolbar}>
        {liveOptions ? (
          <LiveLogSettings />
        ) : (
          <LogSettingsSection defaultEnabled defaultTimelineEnabled={enabled} />
        )}
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

function LiveLogSettings() {
  const { data, isLoading } = useSystemOptions()
  const settings = getOptionValue(data?.data, {
    LogConsumeEnabled: true,
    RequestTimelineEnabled: false,
  })
  if (isLoading) return null
  return (
    <>
      <output aria-label='Server settings'>{`saved=${settings.LogConsumeEnabled}/${settings.RequestTimelineEnabled}`}</output>
      <LogSettingsSection
        defaultEnabled={settings.LogConsumeEnabled}
        defaultTimelineEnabled={settings.RequestTimelineEnabled}
      />
    </>
  )
}

describe('saving both log options with real query refreshes', () => {
  it('keeps the unsaved timeline choice after the first option saves and the second fails', async () => {
    const server = { LogConsumeEnabled: true, RequestTimelineEnabled: false }
    vi.mocked(api.get).mockImplementation(async (url) => ({
      data: {
        success: true,
        data:
          url === '/api/option/'
            ? Object.entries(server).map(([key, value]) => ({
                key,
                value: String(value),
              }))
            : null,
      },
    }))
    let failTimeline: (reason: Error) => void = () => undefined
    const pendingTimeline = new Promise<never>((_resolve, reject) => {
      failTimeline = reject
    })
    let failedOnce = false
    const put = vi.spyOn(api, 'put').mockImplementation(async (_url, data) => {
      const update = data as { key: keyof typeof server; value: boolean }
      if (update.key === 'RequestTimelineEnabled' && !failedOnce) {
        failedOnce = true
        return pendingTimeline
      }
      server[update.key] = update.value
      return { data: { success: true } }
    })
    const user = userEvent.setup()
    mountLogSettings(false, true)
    await screen.findByText('saved=true/false')
    await user.click(screen.getByRole('switch', { name: 'Record quota usage' }))
    await user.click(
      screen.getByRole('switch', { name: 'Record streaming request timeline' })
    )
    await user.click(screen.getByRole('button', { name: 'Save log settings' }))
    await screen.findByText('saved=false/false')
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    await act(async () => {
      failTimeline(new Error('timeline save failed'))
    })
    const save = await screen.findByRole('button', {
      name: 'Save log settings',
    })
    await waitFor(() => expect(save).toBeEnabled())
    expect(
      screen.getByRole('switch', { name: 'Record quota usage' })
    ).toHaveAttribute('aria-checked', 'false')
    expect(
      screen.getByRole('switch', { name: 'Record streaming request timeline' })
    ).toHaveAttribute('aria-checked', 'true')
    await user.click(save)
    await screen.findByText('saved=false/true')
    expect(put.mock.calls.map(([, body]) => body)).toEqual([
      { key: 'LogConsumeEnabled', value: false },
      { key: 'RequestTimelineEnabled', value: true },
      { key: 'RequestTimelineEnabled', value: true },
    ])
  })

  it('reflects both successful saves and does not resend acknowledged values', async () => {
    const server = { LogConsumeEnabled: true, RequestTimelineEnabled: false }
    vi.mocked(api.get).mockImplementation(async (url) => ({
      data: {
        success: true,
        data:
          url === '/api/option/'
            ? Object.entries(server).map(([key, value]) => ({
                key,
                value: String(value),
              }))
            : null,
      },
    }))
    const put = vi.spyOn(api, 'put').mockImplementation(async (_url, data) => {
      const update = data as { key: keyof typeof server; value: boolean }
      server[update.key] = update.value
      return { data: { success: true } }
    })
    const user = userEvent.setup()
    mountLogSettings(false, true)
    await screen.findByText('saved=true/false')
    await user.click(screen.getByRole('switch', { name: 'Record quota usage' }))
    await user.click(
      screen.getByRole('switch', { name: 'Record streaming request timeline' })
    )
    await user.click(screen.getByRole('button', { name: 'Save log settings' }))
    await screen.findByText('saved=false/true')
    const save = await screen.findByRole('button', {
      name: 'Save log settings',
    })
    await waitFor(() => expect(save).toBeEnabled())
    await user.click(save)
    expect(put).toHaveBeenCalledTimes(2)
    expect(
      screen.getByRole('switch', { name: 'Record quota usage' })
    ).toHaveAttribute('aria-checked', 'false')
    expect(
      screen.getByRole('switch', { name: 'Record streaming request timeline' })
    ).toHaveAttribute('aria-checked', 'true')
  })
})
