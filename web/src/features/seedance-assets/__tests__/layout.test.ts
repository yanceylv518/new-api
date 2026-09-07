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

import { seedanceAssetLayoutClasses } from '../layout.ts'

describe('Seedance asset layout', () => {
  test('keeps the page frame and grid from creating a second page scrollbar', () => {
    assert.match(seedanceAssetLayoutClasses.page, /overflow-hidden/)
    assert.match(seedanceAssetLayoutClasses.content, /overflow-hidden/)
    assert.match(
      seedanceAssetLayoutClasses.content,
      /grid-rows-\[minmax\(0,1fr\)_minmax\(0,1fr\)\]/
    )
  })

  test('keeps the merged workspace header outside the asset scroll area', () => {
    assert.match(seedanceAssetLayoutClasses.workspace, /min-h-0/)
    assert.match(seedanceAssetLayoutClasses.workspace, /overflow-hidden/)
    assert.match(seedanceAssetLayoutClasses.uploadPanel, /min-h-0/)
    assert.match(seedanceAssetLayoutClasses.uploadPanel, /overflow-hidden/)
    assert.match(seedanceAssetLayoutClasses.library, /min-h-0/)
    assert.match(seedanceAssetLayoutClasses.library, /overflow-hidden/)
    assert.match(seedanceAssetLayoutClasses.header, /shrink-0/)
    assert.doesNotMatch(seedanceAssetLayoutClasses.header, /overflow-y-auto/)
  })

  test('gives only the asset collection vertical scroll ownership', () => {
    assert.match(seedanceAssetLayoutClasses.assetScroll, /min-h-0/)
    assert.match(seedanceAssetLayoutClasses.assetScroll, /flex-1/)
    assert.match(seedanceAssetLayoutClasses.assetScroll, /overflow-y-auto/)
    assert.match(seedanceAssetLayoutClasses.assetScroll, /overscroll-contain/)
  })

  test('makes the group area fill its sidebar and keeps its list locally scrollable', () => {
    assert.match(seedanceAssetLayoutClasses.groupSidebar, /min-h-0/)
    assert.match(seedanceAssetLayoutClasses.groupSidebar, /overflow-hidden/)
    assert.match(seedanceAssetLayoutClasses.groupSidebarBody, /flex-1/)
    assert.match(seedanceAssetLayoutClasses.groupSidebarBody, /min-h-0/)
    assert.match(seedanceAssetLayoutClasses.groupSidebarScroll, /flex-1/)
    assert.match(
      seedanceAssetLayoutClasses.groupSidebarScroll,
      /overflow-y-auto/
    )
  })
})
