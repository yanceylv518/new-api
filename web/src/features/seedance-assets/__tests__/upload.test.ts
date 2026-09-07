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

import {
  SEEDANCE_ASSET_MAX_BYTES,
  getSeedanceAssetType,
  validateSeedanceAssetFile,
} from '../lib/upload.ts'

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
      size: SEEDANCE_ASSET_MAX_BYTES.Video + 1,
    } as File

    assert.deepEqual(validateSeedanceAssetFile(oversized), {
      valid: false,
      errorKey: 'File exceeds {{limit}}',
      limit: '50.0 MB',
    })
  })
})
