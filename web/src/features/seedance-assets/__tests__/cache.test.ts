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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import type { SeedanceAsset } from '../api.ts'
import { updateSeedanceAssetInList } from '../lib/cache.ts'

describe('Seedance asset query cache', () => {
  test('replaces a manually refreshed asset and preserves sibling assets', () => {
    const current = {
      success: true,
      total: 2,
      page: 1,
      page_size: 24,
      has_pending: true,
      data: [
        {
          id: 1,
          asset_id: 'asset-1',
          group_id: 'group-1',
          name: 'cover.png',
          asset_type: 'Image',
          status: 'Processing',
        },
        {
          id: 2,
          asset_id: 'asset-2',
          group_id: 'group-1',
          name: 'clip.mp4',
          asset_type: 'Video',
          status: 'Active',
        },
      ] satisfies SeedanceAsset[],
    }
    const updated: SeedanceAsset = {
      ...current.data[0],
      status: 'Active',
      preview_url: 'https://cdn.example/cover.png',
    }

    const result = updateSeedanceAssetInList(current, updated)

    assert.equal(result?.data[0].status, 'Active')
    assert.equal(result?.data[0].preview_url, 'https://cdn.example/cover.png')
    assert.equal(result?.data[1].asset_id, 'asset-2')
    assert.strictEqual(result?.data[1], current.data[1])
  })

  test('does not insert an asset from another page into a cached page', () => {
    const asset: SeedanceAsset = {
      id: 3,
      asset_id: 'asset-3',
      group_id: 'group-1',
      name: 'new.png',
      asset_type: 'Image',
      status: 'Processing',
    }

    const current = {
      success: true,
      data: [],
      total: 25,
      page: 2,
      page_size: 24,
      has_pending: false,
    }
    const result = updateSeedanceAssetInList(current, asset)

    assert.strictEqual(result, current)
  })
})
