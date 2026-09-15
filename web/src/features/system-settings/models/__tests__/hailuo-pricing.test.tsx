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
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { expect, it, vi } from 'vitest'

import {
  getTaskEnumCombinations,
  tryParseTaskMatrixConfig,
  generateTaskExprFromConfig,
  taskMatrixToTiers,
  evaluateTaskVisualConfig,
} from '@/features/pricing/lib/task-expr'
import { getTaskPricingDisplayTiers } from '@/features/pricing/lib/task-matrix-display'
import type { BillingUsageSchema } from '@/features/pricing/types'

import { TaskUsagePricingEditor } from '../task-usage-pricing-editor'

// 固定官方支持的三种操作，复现分辨率全组合产生无效再生成价格的问题。
const schema: BillingUsageSchema = {
  operation: {
    enum: ['generation', 'regeneration', 'context_ir'],
    description: 'Operation',
    enumLabels: { context_ir: { en: 'H3-Context-IR', zh: 'H3-Context-IR' } },
  },
  resolution: {
    enum: ['768P', '2K'],
    description: 'Resolution',
    when: [
      { field: 'operation', values: ['generation'] },
      { field: 'operation', values: ['regeneration'], enum: ['2K'] },
    ],
  },
  seconds: {
    type: 'number',
    unit: 'second',
    displayOrder: 10,
    description: 'Video unit price',
    when: [{ field: 'operation', values: ['generation', 'regeneration'] }],
  },
  input_images: {
    type: 'number',
    unit: 'count',
    displayOrder: 40,
    description: 'Input image unit price',
    when: [{ field: 'operation', values: ['generation', 'regeneration'] }],
  },
  input_video_seconds: {
    type: 'number',
    unit: 'second',
    displayOrder: 50,
    description: 'Input video unit price',
    when: [{ field: 'operation', values: ['generation', 'regeneration'] }],
  },
  prompt_tokens: {
    type: 'number',
    unit: 'token',
    displayOrder: 20,
    description: 'H3-Context-IR input token unit price',
    when: [{ field: 'operation', values: ['context_ir'] }],
  },
  completion_tokens: {
    type: 'number',
    unit: 'token',
    displayOrder: 30,
    description: 'H3-Context-IR output token unit price',
    when: [{ field: 'operation', values: ['context_ir'] }],
  },
}
const legacyExpression =
  'u("operation") == "context_ir" ? tier("ir", u("tokens") * 2 / 1000000) : u("operation") == "regeneration" ? tier("regen", u("seconds") * 0.2) : tier("generation", u("seconds") * 0.1)'
const expression =
  'u("operation") == "context_ir" ? tier("ir", u("prompt_tokens") * 2 / 1000000 + u("completion_tokens") * 8 / 1000000) : u("operation") == "regeneration" ? tier("regen", u("seconds") * 0.2) : tier("generation", u("seconds") * 0.1)'

// 模拟后端按键名序列化 schema；表头必须仍按视频、输入/输出 Token、素材、加收费排列。
it('keeps H3 pricing columns in business order after schema serialization', () => {
  const onChange = vi.fn()
  render(
    <TaskUsagePricingEditor
      billingExpr={expression}
      requestRuleExpr=''
      usageSchema={Object.fromEntries(
        Object.entries(schema).sort(([left], [right]) =>
          left.localeCompare(right)
        )
      )}
      onBillingExprChange={onChange}
      onRequestRuleExprChange={vi.fn()}
    />
  )
  const headings = within(screen.getByRole('table')).getAllByRole(
    'columnheader'
  )
  const expected = [
    'Operation',
    'Resolution',
    'Video unit price',
    'H3-Context-IR input token unit price',
    'H3-Context-IR output token unit price',
    'Input image unit price',
    'Input video unit price',
    'Additional charge',
    'Status',
  ]
  expect(headings).toHaveLength(expected.length)
  expected.forEach((label, index) =>
    expect(headings[index]).toHaveTextContent(label)
  )
  expect(onChange).not.toHaveBeenCalled()
})

// 管理员可分别修改生成、再生成额度；重新打开编辑器时设置及金额都必须保留。
it('edits and restores free image quantities separately for generation and regeneration', async () => {
  let saved = expression
  function PricingForm() {
    const [value, setValue] = useState(saved)
    return (
      <TaskUsagePricingEditor
        billingExpr={value}
        requestRuleExpr=''
        usageSchema={schema}
        onBillingExprChange={(next) => {
          saved = next
          setValue(next)
        }}
        onRequestRuleExprChange={vi.fn()}
      />
    )
  }
  const view = render(<PricingForm />)
  const user = userEvent.setup()
  const generationLabel =
    /Free quantity per task: Input image unit price:.*generation.*768P/
  const regenerationLabel =
    /Free quantity per task: Input image unit price:.*regeneration/
  const generation = screen.getByRole('spinbutton', { name: generationLabel })
  const regeneration = screen.getByRole('spinbutton', {
    name: regenerationLabel,
  })
  await user.clear(generation)
  await user.type(generation, '5')
  await user.clear(regeneration)
  await user.type(regeneration, '7')
  expect(saved).toContain('max(u("input_images") - 5, 0)')
  expect(saved).toContain('max(u("input_images") - 7, 0)')
  view.unmount()
  render(<PricingForm />)
  expect(screen.getByRole('spinbutton', { name: generationLabel })).toHaveValue(
    5
  )
  expect(
    screen.getByRole('spinbutton', { name: regenerationLabel })
  ).toHaveValue(7)
  const generationAgain = screen.getByRole('spinbutton', {
    name: generationLabel,
  })
  await user.clear(generationAgain)
  await user.type(generationAgain, '0')
  expect(saved).not.toContain('max(u("input_images") - 5, 0)')
  expect(
    screen.queryByRole('spinbutton', {
      name: /Free quantity per task:.*H3-Context-IR/,
    })
  ).not.toBeInTheDocument()
})

it('only offers valid H3 operation and output-resolution combinations', () => {
  expect(getTaskEnumCombinations(schema)).toEqual([
    { operation: 'generation', resolution: '768P' },
    { operation: 'generation', resolution: '2K' },
    { operation: 'regeneration', resolution: '2K' },
    { operation: 'context_ir' },
  ])
  const tiers = getTaskPricingDisplayTiers(expression, schema)
  expect(tiers).toHaveLength(4)
  expect(tiers[2].conditions).toEqual([
    { field: 'operation', value: 'regeneration' },
    { field: 'resolution', value: '2K' },
  ])
  expect(tiers[3].conditions).toEqual([
    { field: 'operation', value: 'context_ir' },
  ])
})

it('preserves valid existing prices through a visual pricing round trip', () => {
  const matrix = tryParseTaskMatrixConfig(expression, schema)
  expect(matrix).not.toBeNull()
  if (!matrix) return
  const config = { tiers: taskMatrixToTiers(matrix, schema) }
  const generated = generateTaskExprFromConfig(config, schema)
  const restored = tryParseTaskMatrixConfig(generated, schema)
  expect(restored?.rows).toEqual(matrix.rows)
  expect(
    evaluateTaskVisualConfig(
      config,
      { operation: 'regeneration', resolution: '2K', seconds: 5, tokens: 0 },
      schema
    )?.total
  ).toBe(1)
  expect(
    evaluateTaskVisualConfig(
      config,
      {
        operation: 'context_ir',
        seconds: 0,
        tokens: 9090,
        prompt_tokens: 5664,
        completion_tokens: 3426,
      },
      schema
    )?.total
  ).toBeCloseTo(0.038736)
})

// 已移除的总量价格不能被可视化编辑器误解析为有效的新定价。
it('rejects total-token pricing after the field is removed', () => {
  expect(tryParseTaskMatrixConfig(legacyExpression, schema)).toBeNull()
})

// 实际界面只允许编辑适用于该操作的费用项，计算器切换操作后也必须收敛选项。
it('hides invalid matrix prices and narrows the calculator when operation changes', async () => {
  const onChange = vi.fn()
  render(
    <TaskUsagePricingEditor
      billingExpr={expression}
      requestRuleExpr=''
      usageSchema={schema}
      onBillingExprChange={onChange}
      onRequestRuleExprChange={vi.fn()}
    />
  )
  const table = screen.getByRole('table')
  const rows = within(table).getAllByRole('row')
  expect(rows).toHaveLength(5)
  const regeneration = rows.find((row) =>
    within(row).queryByText('regeneration')
  )
  expect(regeneration).toBeDefined()
  if (!regeneration) return
  expect(within(regeneration).queryByText('768P')).not.toBeInTheDocument()
  expect(within(regeneration).getByText('2K')).toBeVisible()
  expect(
    within(regeneration).queryByRole('textbox', {
      name: /H3-Context-IR .* token unit price/,
    })
  ).not.toBeInTheDocument()
  const contextRow = rows.find((row) =>
    within(row).queryByText('H3-Context-IR')
  )
  expect(contextRow).toBeDefined()
  if (!contextRow) return
  expect(
    within(contextRow).queryByRole('textbox', { name: /Video unit price/ })
  ).not.toBeInTheDocument()
  expect(
    within(contextRow).getByRole('textbox', {
      name: /H3-Context-IR input token unit price/,
    })
  ).toBeVisible()
  expect(
    within(contextRow).getByRole('textbox', {
      name: /H3-Context-IR output token unit price/,
    })
  ).toBeVisible()
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Operation' }))
  await user.click(screen.getByRole('option', { name: 'regeneration' }))
  await user.click(screen.getByRole('combobox', { name: 'Resolution' }))
  expect(screen.queryByRole('option', { name: '768P' })).not.toBeInTheDocument()
  await user.keyboard('{Escape}')
  await user.click(screen.getByRole('combobox', { name: 'Operation' }))
  await user.click(screen.getByRole('option', { name: 'H3-Context-IR' }))
  expect(
    screen.queryByRole('combobox', { name: 'Resolution' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('spinbutton', { name: 'Usage · Video unit price' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('spinbutton', {
      name: 'Usage · H3-Context-IR input token unit price',
    })
  ).toBeVisible()
  expect(
    screen.getByRole('spinbutton', {
      name: 'Usage · H3-Context-IR output token unit price',
    })
  ).toBeVisible()
  expect(onChange).not.toHaveBeenCalled()
})

// 真实受控编辑器分别修改两项单价，生成的表达式必须按各自用量计算且不叠加总量。
it('edits input and output prices independently without adding total-token charges', async () => {
  const onChange = vi.fn()
  function PricingForm() {
    const [value, setValue] = useState(expression)
    return (
      <TaskUsagePricingEditor
        billingExpr={value}
        requestRuleExpr=''
        usageSchema={schema}
        onBillingExprChange={(next) => {
          setValue(next)
          onChange(next)
        }}
        onRequestRuleExprChange={vi.fn()}
      />
    )
  }
  render(<PricingForm />)
  const user = userEvent.setup()
  const input = screen.getByRole('textbox', {
    name: /H3-Context-IR input token unit price:/,
  })
  const output = screen.getByRole('textbox', {
    name: /H3-Context-IR output token unit price:/,
  })
  await user.clear(input)
  await user.type(input, '3')
  await user.tab()
  expect(output).toHaveValue('8')
  await user.clear(output)
  await user.type(output, '9')
  await user.tab()
  expect(input).toHaveValue('3')
  expect(
    screen.queryByRole('textbox', {
      name: /H3-Context-IR total token unit price:/,
    })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('columnheader', {
      name: /H3-Context-IR total token unit price/,
    })
  ).not.toBeInTheDocument()
  expect(onChange.mock.lastCall?.[0]).not.toContain('u("tokens")')
  const matrix = tryParseTaskMatrixConfig(
    onChange.mock.lastCall?.[0] ?? '',
    schema
  )
  expect(matrix).not.toBeNull()
  if (!matrix) return
  expect(
    evaluateTaskVisualConfig(
      { tiers: taskMatrixToTiers(matrix, schema) },
      {
        operation: 'context_ir',
        tokens: 9090,
        prompt_tokens: 5664,
        completion_tokens: 3426,
      },
      schema
    )?.total
  ).toBeCloseTo(0.047826)
})
