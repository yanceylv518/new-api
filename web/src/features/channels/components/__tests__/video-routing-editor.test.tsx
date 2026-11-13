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
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { expect, test, vi } from 'vitest'

import en from '@/i18n/locales/en.json'

import { VideoRoutingEditorDialog } from '../dialogs/video-routing-editor-dialog'

async function renderEditor() {
  const i18n = createInstance()
  await i18n.init({
    lng: 'en',
    fallbackLng: 'en',
    resources: { en },
    keySeparator: false,
    interpolation: { escapeValue: false },
  })
  const onChange = vi.fn()
  const onOpenChange = vi.fn()
  const rule = {
    model: 'doubao-seedance-2-0-260128',
    mode: 'deny' as const,
    resolutions: ['4k' as const],
  }
  render(
    <I18nextProvider i18n={i18n}>
      <VideoRoutingEditorDialog
        models={[rule.model]}
        value={[rule]}
        onChange={onChange}
        onOpenChange={onOpenChange}
      />
    </I18nextProvider>
  )
  return { user: userEvent.setup(), onChange, onOpenChange, rule }
}

test('cancel discards draft edits without changing saved rules', async () => {
  const { user, onChange, onOpenChange, rule } = await renderEditor()
  await user.click(screen.getByRole('checkbox', { name: '720p' }))
  await user.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(onChange).not.toHaveBeenCalled()
  expect(onOpenChange).toHaveBeenCalledWith(false)
  expect(rule.resolutions).toEqual(['4k'])
})

test('apply requires a resolution and unique models, then returns only rule fields', async () => {
  const { user, onChange, rule } = await renderEditor()
  await user.click(screen.getByRole('checkbox', { name: '4K' }))
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(screen.getByRole('alert')).toBeVisible()
  expect(onChange).not.toHaveBeenCalled()
  await user.click(screen.getByRole('checkbox', { name: '4K' }))
  await user.click(screen.getByRole('button', { name: 'Add rule' }))
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(screen.getByRole('alert')).toBeVisible()
  expect(onChange).not.toHaveBeenCalled()
  await user.click(screen.getAllByRole('button', { name: 'Remove' })[1])
  await user.click(screen.getByRole('button', { name: 'Apply' }))
  expect(onChange).toHaveBeenCalledWith([rule])
})
