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
import type { SeedanceAsset, SeedanceAssetListResponse } from '../api'

// 手动刷新只替换已有条目，不能把其他页的素材插入当前页或改变兄弟条目的引用。
export function updateSeedanceAssetInList(
  current: SeedanceAssetListResponse | undefined,
  updatedAsset: SeedanceAsset
): SeedanceAssetListResponse | undefined {
  if (!current) return current
  const hasAsset = current.data.some((asset) => asset.id === updatedAsset.id)
  if (!hasAsset) return current
  const data = current.data.map((asset) =>
    asset.id === updatedAsset.id ? updatedAsset : asset
  )
  return { ...current, data }
}
