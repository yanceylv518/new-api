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

import { describe, test } from 'vitest'

import { retainSeedanceAssetPreview } from '../lib/preview'

describe('Seedance asset preview URL retention', () => {
  // OSS 每次签名都会改变查询参数，已加载的同一素材不应因此重新下载。
  test('keeps the displayed URL when the same asset receives a renewed signature', () => {
    const current = {
      assetId: 1,
      url: 'https://cdn.example/image.png?signature=old',
    }

    const result = retainSeedanceAssetPreview(
      current,
      1,
      'https://cdn.example/image.png?signature=new',
      null
    )

    assert.strictEqual(result, current)
  })

  // 当前签名确实加载失败后，新的签名必须能够恢复预览。
  test('adopts a renewed URL after the displayed URL fails', () => {
    const failedURL = 'https://cdn.example/image.png?signature=expired'

    const result = retainSeedanceAssetPreview(
      { assetId: 1, url: failedURL },
      1,
      'https://cdn.example/image.png?signature=fresh',
      failedURL
    )

    assert.deepEqual(result, {
      assetId: 1,
      url: 'https://cdn.example/image.png?signature=fresh',
    })
  })
})
