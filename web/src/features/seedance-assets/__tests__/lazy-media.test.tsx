/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { act, render } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import { AssetPreview } from '../components/asset-preview'

let latestCallback: IntersectionObserverCallback | null = null
let latestDisconnect: ReturnType<typeof vi.fn> | null = null
let observedTargets: Element[] = []

class MockIntersectionObserver implements IntersectionObserver {
  readonly root = null
  readonly rootMargin = ''
  readonly scrollMargin = ''
  readonly thresholds: ReadonlyArray<number> = []

  constructor(callback: IntersectionObserverCallback) {
    latestCallback = callback
    latestDisconnect = this.disconnect
  }

  disconnect = vi.fn()
  observe = vi.fn((target: Element) => {
    observedTargets.push(target)
  })
  takeRecords = () => []
  unobserve = vi.fn()
}

afterEach(() => {
  latestCallback = null
  latestDisconnect = null
  observedTargets = []
  vi.unstubAllGlobals()
})

test.each(['Video', 'Audio'] as const)(
  'defers %s metadata requests until the preview nears the viewport',
  (assetType) => {
    vi.stubGlobal('IntersectionObserver', MockIntersectionObserver)
    const { container } = render(
      <AssetPreview
        showAudioControls
        asset={{
          id: 1,
          asset_id: 'video-1',
          group_id: 'group-1',
          name: 'clip.mp4',
          asset_type: assetType,
          status: 'Active',
          preview_url: 'https://media.example/clip.mp4?signature=test',
        }}
      />
    )

    const media = container.querySelector(
      assetType === 'Video' ? 'video' : 'audio'
    )
    if (!media) throw new Error(`${assetType} preview did not render`)
    expect(media.getAttribute('src')).toBeNull()
    expect(media.preload).toBe('none')
    expect(observedTargets).toContain(media)

    const callback = latestCallback
    if (!callback) throw new Error('intersection observer was not created')
    const observer: IntersectionObserver = {
      root: null,
      rootMargin: '',
      scrollMargin: '',
      thresholds: [],
      disconnect: () => undefined,
      observe: () => undefined,
      takeRecords: () => [],
      unobserve: () => undefined,
    }
    const bounds = media.getBoundingClientRect()
    const entry: IntersectionObserverEntry = {
      boundingClientRect: bounds,
      intersectionRatio: 1,
      intersectionRect: bounds,
      isIntersecting: true,
      rootBounds: null,
      target: media,
      time: 0,
    }
    act(() => callback([entry], observer))

    expect(media.getAttribute('src')).toBe(
      'https://media.example/clip.mp4?signature=test'
    )
    expect(media.preload).toBe('metadata')
    expect(latestDisconnect).toHaveBeenCalled()
  }
)
