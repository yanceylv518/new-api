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
import * as z from 'zod'

export type PrivateAssetOSSFormValues = {
  region: string
  endpoint: string
  bucket: string
  prefix: string
  accessKeyId: string
  accessKeySecret: string
}

// 前后端使用相同边界，页面先给出字段级提示，服务端仍执行权威校验。
export function createPrivateAssetOSSSchema(t: (key: string) => string) {
  return z.object({
    region: z
      .string()
      .trim()
      .min(1, t('Region is required'))
      .regex(/^[a-z0-9][a-z0-9-]{0,62}$/, t('Region is invalid')),
    endpoint: z
      .string()
      .trim()
      .refine((value) => {
        if (!value) return true
        try {
          const parsed = new URL(value)
          return (
            parsed.protocol === 'https:' &&
            parsed.username === '' &&
            parsed.password === '' &&
            (parsed.pathname === '' || parsed.pathname === '/') &&
            parsed.search === '' &&
            parsed.hash === ''
          )
        } catch {
          return false
        }
      }, t('Provide a valid HTTPS endpoint')),
    bucket: z
      .string()
      .trim()
      .regex(/^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/, t('Bucket name is invalid')),
    prefix: z
      .string()
      .trim()
      .max(512, t('Object prefix is invalid'))
      .refine((value) => {
        if (!value) return true
        const segments = value.replaceAll(/^\/+|\/+$/g, '').split('/')
        return (
          !value.includes('\\') &&
          !segments.some(
            (segment) => segment === '' || segment === '.' || segment === '..'
          ) &&
          !/\p{Cc}/u.test(value)
        )
      }, t('Object prefix is invalid')),
    accessKeyId: z
      .string()
      .trim()
      .min(1, t('AccessKey ID is required'))
      .max(128, t('AccessKey ID is invalid')),
    accessKeySecret: z
      .string()
      .trim()
      .max(256, t('AccessKey Secret is invalid')),
  })
}
