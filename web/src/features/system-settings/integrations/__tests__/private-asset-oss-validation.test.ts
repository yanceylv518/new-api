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

import { createPrivateAssetOSSSchema } from '../private-asset-oss-validation.ts'

const schema = createPrivateAssetOSSSchema((key) => key)

const validSettings = {
  region: 'cn-hangzhou',
  endpoint: 'https://oss-cn-hangzhou.aliyuncs.com',
  bucket: 'private-assets',
  prefix: 'private-assets/',
  accessKeyId: 'test-id',
  accessKeySecret: '',
}

describe('Private asset OSS settings validation', () => {
  test('accepts an HTTPS Alibaba Cloud OSS configuration', () => {
    assert.equal(schema.safeParse(validSettings).success, true)
  })

  test('rejects HTTP endpoints', () => {
    assert.equal(
      schema.safeParse({
        ...validSettings,
        endpoint: 'http://oss-cn-hangzhou.aliyuncs.com',
      }).success,
      false
    )
  })

  test('rejects invalid prefix path segments', () => {
    assert.equal(
      schema.safeParse({
        ...validSettings,
        prefix: 'private-assets/../other',
      }).success,
      false
    )
  })
})
