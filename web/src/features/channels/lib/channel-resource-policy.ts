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
import type { TFunction } from 'i18next'
import { z } from 'zod'

import { hasPermission } from '@/lib/admin-permissions'
import { parseRequestBodyMB } from '@/lib/request-body-size'
import { ROLE } from '@/lib/roles'
import type { AuthUser } from '@/stores/auth-store'

export function canManageChannelResourcePolicy(
  user: AuthUser | null | undefined
) {
  return (
    !!user &&
    user.role >= ROLE.ADMIN &&
    hasPermission(user, 'channel', 'read') &&
    hasPermission(user, 'channel', 'write')
  )
}

export function channelResourcePolicySchema(t: TFunction) {
  return z.object({
    limitMB: z
      .string()
      .refine(
        (value) => parseRequestBodyMB(value) !== null,
        t(
          'Enter a valid non-negative size in MB (at least 1 byte when positive).'
        )
      ),
  })
}
