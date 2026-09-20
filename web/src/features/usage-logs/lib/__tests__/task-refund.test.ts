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

import { describe, test } from 'vitest'

import type { LogOtherData } from '../../types'
import { getTaskSettlementAdjustment } from '../task-refund'

// 保护历史账务记录的展示分类，不依赖 token 重算等可变原因文案。
describe('task settlement adjustments', () => {
  // 补扣必须有有效任务及完整快照，不能从普通消费推测结算信息。
  test('recognizes surcharges and rejects invalid charge snapshots', () => {
    for (const pre of [0, 710955]) {
      assert.deepEqual(
        getTaskSettlementAdjustment(2, {
          task_id: 'task_video',
          pre_consumed_quota: pre,
          actual_quota: 750989,
        }),
        { preConsumedQuota: pre, actualQuota: 750989 }
      )
    }
    for (const [pre, actual] of [
      [-1, 10],
      [10, 10],
      [20, 10],
      [0.5, 10],
      [0, Infinity],
      [Number.NaN, 10],
    ]) {
      assert.equal(
        getTaskSettlementAdjustment(2, {
          task_id: 'task_video',
          pre_consumed_quota: pre,
          actual_quota: actual,
        }),
        null
      )
    }
    assert.equal(
      getTaskSettlementAdjustment(2, { task_id: 'task_video' }),
      null
    )
    assert.equal(
      getTaskSettlementAdjustment(2, {
        pre_consumed_quota: 0,
        actual_quota: 10,
      }),
      null
    )
  })
  test('recognizes a historical refund with pre-consumed and actual quotas', () => {
    assert.deepEqual(
      getTaskSettlementAdjustment(6, {
        task_id: 'task_video',
        pre_consumed_quota: 250000,
        actual_quota: 108900,
      }),
      { preConsumedQuota: 250000, actualQuota: 108900 }
    )
  })

  test('recognizes an explicit zero-cost settlement', () => {
    assert.deepEqual(
      getTaskSettlementAdjustment(6, {
        task_id: 'task_video',
        pre_consumed_quota: 250000,
        actual_quota: 0,
      }),
      { preConsumedQuota: 250000, actualQuota: 0 }
    )
  })

  test('keeps failed and incomplete task refunds under the generic label', () => {
    const records: (LogOtherData | null)[] = [
      null,
      { task_id: 'task_video', reason: 'upstream task failed' },
      { task_id: 'task_video', pre_consumed_quota: 250000 },
      { task_id: 'task_video', actual_quota: 108900 },
      { pre_consumed_quota: 250000, actual_quota: 108900 },
    ]
    for (const other of records) {
      assert.equal(getTaskSettlementAdjustment(6, other), null)
    }
  })

  test('does not label charges or invalid quota snapshots as settlement refunds', () => {
    assert.equal(
      getTaskSettlementAdjustment(2, {
        task_id: 'task_video',
        pre_consumed_quota: 250000,
        actual_quota: 108900,
      }),
      null
    )
    for (const [pre, actual] of [
      [108900, 250000],
      [250000, 250000],
      [250000, -1],
      [Number.NaN, 108900],
      [250000, Infinity],
      [250000, 0.5],
    ]) {
      assert.equal(
        getTaskSettlementAdjustment(6, {
          task_id: 'task_video',
          pre_consumed_quota: pre,
          actual_quota: actual,
        }),
        null
      )
    }
  })
})
