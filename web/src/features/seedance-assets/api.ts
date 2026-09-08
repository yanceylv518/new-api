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
import { api } from '@/lib/api'

// HTTP 200 也可能携带业务失败，统一抛出错误以保留表单并显示失败反馈。
function checked<T extends { success: boolean; message?: string }>(
  response: T
): T {
  if (!response.success) throw new Error(response.message || 'Request failed')
  return response
}

export type SeedanceAssetGroup = {
  id: number
  group_id: string
  name: string
  status?: string
}
export type SeedanceAsset = {
  id: number
  asset_id: string
  group_id: string
  name: string
  asset_type: string
  status: string
  preview_url?: string
}

export type SeedanceAssetListResponse = {
  success: boolean
  message?: string
  data: SeedanceAsset[]
  total: number
  page: number
  page_size: number
  has_pending: boolean
}

export type SeedanceAssetResponse<T> = {
  success: boolean
  message?: string
  data: T
}

export async function listSeedanceAssetGroups() {
  return checked(
    (
      await api.get<{ success: boolean; data: SeedanceAssetGroup[] }>(
        '/api/user/seedance/asset-groups'
      )
    ).data
  )
}
export async function createSeedanceAssetGroup(name: string) {
  return checked(
    (
      await api.post<{
        success: boolean
        message?: string
        data: SeedanceAssetGroup
      }>('/api/user/seedance/asset-groups', { name })
    ).data
  )
}
export async function updateSeedanceAssetGroup(id: number, name: string) {
  return checked(
    (
      await api.put<SeedanceAssetResponse<SeedanceAssetGroup>>(
        `/api/user/seedance/asset-groups/${id}`,
        { name }
      )
    ).data
  )
}
export async function deleteSeedanceAssetGroup(id: number) {
  return checked(
    (
      await api.delete<SeedanceAssetResponse<null>>(
        `/api/user/seedance/asset-groups/${id}`
      )
    ).data
  )
}
export async function listSeedanceAssets(
  groupId?: string,
  signal?: AbortSignal,
  filters?: {
    p: number
    page_size: number
    search: string
    asset_type: string
    status: string
  }
) {
  // 列表请求只读取本地状态，审核同步由服务端有界后台任务负责。
  const response = checked(
    (
      await api.get<SeedanceAssetListResponse>('/api/user/seedance/assets', {
        params: { ...filters, ...(groupId ? { group_id: groupId } : {}) },
        signal,
        disableDuplicate: true,
      })
    ).data
  )
  response.data ??= []
  return response
}
export async function createSeedanceAsset(payload: {
  group_id: string
  source_url: string
  asset_type: string
  name: string
}) {
  return checked(
    (
      await api.post<SeedanceAssetResponse<SeedanceAsset>>(
        '/api/user/seedance/assets',
        payload
      )
    ).data
  )
}
export async function uploadSeedanceAsset(payload: {
  groupId: string
  file: File
  name: string
}) {
  const formData = new FormData()
  formData.append('group_id', payload.groupId)
  formData.append('file', payload.file, payload.file.name)
  if (payload.name.trim()) formData.append('name', payload.name.trim())

  return checked(
    (
      await api.post<SeedanceAssetResponse<SeedanceAsset>>(
        '/api/user/seedance/assets/upload',
        formData
      )
    ).data
  )
}
export async function refreshSeedanceAsset(id: number, signal?: AbortSignal) {
  return checked(
    (
      await api.post<SeedanceAssetResponse<SeedanceAsset>>(
        `/api/user/seedance/assets/${id}/refresh`,
        undefined,
        { signal, timeout: 35000 }
      )
    ).data
  )
}
export async function deleteSeedanceAsset(id: number) {
  return checked(
    (
      await api.delete<SeedanceAssetResponse<null>>(
        `/api/user/seedance/assets/${id}`
      )
    ).data
  )
}
