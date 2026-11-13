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

import {
  getSeedanceAssetType,
  validateSeedanceAssetFile,
  validateSeedancePortraitFile,
} from '../lib/upload'

const limits = { Image: 30, Video: 50, Audio: 15 }

describe('Seedance asset upload validation', () => {
  test('detects media type from MIME and extension fallback', () => {
    assert.equal(
      getSeedanceAssetType(
        new File(['image'], 'cover.png', { type: 'image/png' })
      ),
      'Image'
    )
    assert.equal(
      getSeedanceAssetType(new File(['video'], 'clip.mp4', { type: '' })),
      'Video'
    )
  })

  test('rejects empty and unsupported files before upload', () => {
    assert.deepEqual(
      validateSeedanceAssetFile(
        new File([], 'empty.mp4', { type: 'video/mp4' })
      ),
      { valid: false, errorKey: 'File is empty' }
    )
    assert.deepEqual(
      validateSeedanceAssetFile(
        new File(['text'], 'notes.txt', { type: 'text/plain' })
      ),
      { valid: false, errorKey: 'Unsupported file type' }
    )
  })

  test('rejects files above the per-type upstream limit', () => {
    const oversized = {
      name: 'clip.mp4',
      type: 'video/mp4',
      size: limits.Video * 1024 * 1024 + 1,
    } as File

    assert.deepEqual(validateSeedanceAssetFile(oversized, limits), {
      valid: false,
      errorKey: 'File exceeds {{limit}}',
      limit: '50.0 MB',
    })
  })

  test('accepts the verified portrait formats and rejects other images', () => {
    for (const extension of ['jpg', 'jpeg', 'png', 'webp', 'gif', 'heic']) {
      assert.deepEqual(
        validateSeedancePortraitFile(
          new File(['image'], `portrait.${extension}`, {
            type: extension === 'heic' ? 'image/heic' : `image/${extension}`,
          })
        ),
        { valid: true, assetType: 'Image' }
      )
    }
    assert.deepEqual(
      validateSeedancePortraitFile(
        new File(['image'], 'portrait.bmp', { type: 'image/bmp' })
      ),
      { valid: false, errorKey: 'Unsupported file type' }
    )
  })

  test('enforces the same 30 MB image limit for verified portraits', () => {
    const oversized = {
      name: 'portrait.jpg',
      type: 'image/jpeg',
      size: limits.Image * 1024 * 1024 + 1,
    } as File

    assert.deepEqual(validateSeedancePortraitFile(oversized, limits), {
      valid: false,
      errorKey: 'File exceeds {{limit}}',
      limit: '30.0 MB',
    })
  })

  test('uses the updated image limit and leaves size validation to the server when unavailable', () => {
    const image = {
      name: 'portrait.jpg',
      type: 'image/jpeg',
      size: 40 * 1024 * 1024,
    } as File
    assert.equal(validateSeedancePortraitFile(image, limits).valid, false)
    assert.equal(
      validateSeedancePortraitFile(image, { ...limits, Image: 60 }).valid,
      true
    )
    assert.equal(validateSeedancePortraitFile(image, null).valid, true)
  })
})
