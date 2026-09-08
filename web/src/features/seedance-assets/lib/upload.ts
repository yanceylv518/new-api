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
export const SEEDANCE_ASSET_ACCEPT = 'image/*,video/*,audio/*'

export const SEEDANCE_ASSET_MAX_BYTES = {
  Image: 30 * 1024 * 1024,
  Video: 50 * 1024 * 1024,
  Audio: 15 * 1024 * 1024,
} as const

export type SeedanceAssetType = keyof typeof SEEDANCE_ASSET_MAX_BYTES

const extensionTypeMap: Record<string, SeedanceAssetType> = {
  aac: 'Audio',
  avi: 'Video',
  avif: 'Image',
  bmp: 'Image',
  flac: 'Audio',
  gif: 'Image',
  heic: 'Image',
  heif: 'Image',
  jpeg: 'Image',
  jpg: 'Image',
  m4a: 'Audio',
  m4v: 'Video',
  mkv: 'Video',
  mov: 'Video',
  mp3: 'Audio',
  mp4: 'Video',
  oga: 'Audio',
  ogg: 'Audio',
  png: 'Image',
  wav: 'Audio',
  webm: 'Video',
  webp: 'Image',
}

export type SeedanceAssetFileValidation =
  | { valid: true; assetType: SeedanceAssetType }
  | {
      valid: false
      errorKey:
        | 'Unsupported file type'
        | 'File is empty'
        | 'File exceeds {{limit}}'
      limit?: string
    }

// 文件类型优先取浏览器 MIME，MIME 缺失时回退到扩展名以兼容拖入的本地文件。
export function getSeedanceAssetType(file: File): SeedanceAssetType | null {
  const mimeType = file.type.toLowerCase()
  if (mimeType.startsWith('image/')) return 'Image'
  if (mimeType.startsWith('video/')) return 'Video'
  if (mimeType.startsWith('audio/')) return 'Audio'

  const extension = file.name.toLowerCase().split('.').pop() ?? ''
  return extensionTypeMap[extension] ?? null
}

// 上传前在浏览器侧提前阻止不支持或超过上游限制的文件，减少无效网络请求。
export function validateSeedanceAssetFile(
  file: File
): SeedanceAssetFileValidation {
  const assetType = getSeedanceAssetType(file)
  if (!assetType) {
    return { valid: false, errorKey: 'Unsupported file type' }
  }
  if (file.size === 0) {
    return { valid: false, errorKey: 'File is empty' }
  }

  const limit = SEEDANCE_ASSET_MAX_BYTES[assetType]
  if (file.size > limit) {
    return {
      valid: false,
      errorKey: 'File exceeds {{limit}}',
      limit: formatSeedanceFileSize(limit),
    }
  }
  return { valid: true, assetType }
}

export function formatSeedanceFileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

export function getSeedanceAssetTypeLabel(assetType: string): string {
  switch (assetType.trim().toLowerCase()) {
    case 'image':
      return 'Image'
    case 'video':
      return 'Video'
    case 'audio':
      return 'Audio'
    default:
      return 'File'
  }
}
