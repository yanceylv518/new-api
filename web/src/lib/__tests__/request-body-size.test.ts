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
import { expect, it } from 'vitest'

import { formatRequestBodyMB, parseRequestBodyMB } from '../request-body-size'

it.each([0, 1, 30, 512, 1048576, Number.MAX_SAFE_INTEGER])(
  'round-trips an existing %i-byte policy without changing its limit',
  (bytes) => expect(parseRequestBodyMB(formatRequestBodyMB(bytes))).toBe(bytes)
)

it.each([
  ['30', 31457280],
  ['0.5', 524288],
  ['0.1', 104858],
  ['0', 0],
] as const)('converts %s MB into integer bytes', (value, expected) => {
  expect(parseRequestBodyMB(value)).toBe(expected)
})

it.each([
  '',
  '-1',
  'Infinity',
  'NaN',
  '1e3',
  '1,5',
  '9007199254740991',
  '0.0000001',
  `0.${'0'.repeat(330)}1`,
])('rejects invalid or unsafe MB value %s', (value) =>
  expect(parseRequestBodyMB(value)).toBeNull()
)
