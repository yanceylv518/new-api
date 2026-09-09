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
import type { LogOtherData } from '../types'

/** 根据结构化额度快照识别结算退差额，避免把任务失败退款误标为成功结算。 */
export function getTaskSettlementRefund(
  logType: number,
  other: LogOtherData | null
): { preConsumedQuota: number; actualQuota: number } | null {
  const preConsumedQuota = other?.pre_consumed_quota
  const actualQuota = other?.actual_quota
  if (
    logType !== 6 ||
    typeof other?.task_id !== 'string' ||
    !other.task_id.trim() ||
    typeof preConsumedQuota !== 'number' ||
    !Number.isSafeInteger(preConsumedQuota) ||
    typeof actualQuota !== 'number' ||
    !Number.isSafeInteger(actualQuota) ||
    actualQuota < 0 ||
    preConsumedQuota <= actualQuota
  ) {
    return null
  }
  return { preConsumedQuota, actualQuota }
}
