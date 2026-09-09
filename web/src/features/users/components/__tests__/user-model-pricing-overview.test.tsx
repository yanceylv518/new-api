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

import { afterAll as after, afterEach, describe, test } from 'vitest'

// 使用 rc35 的 Vitest/jsdom 环境，避免混用 DOM 和运行器。
const { act, useState } = await import('react')
const { createRoot } = await import('react-dom/client')
const { QueryClient, QueryClientProvider, notifyManager } =
  await import('@tanstack/react-query')
const { api } = await import('@/lib/api')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { UserModelPricingMobileRow } =
  await import('../user-model-pricing-overview')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const item = {
  user: {
    id: 42,
    username: 'pricing-user',
    display_name: 'Pricing User',
    email: 'pricing@example.com',
    group: 'default',
    role: 1,
    status: 1,
  },
  rules: [
    { model_name: 'model-01', discount_bps: 8000 },
    { model_name: 'model-02', discount_bps: 8500 },
    { model_name: 'model-03', discount_bps: 9000 },
    { model_name: 'model-04', discount_bps: 9500 },
  ],
  rule_count: 4,
  min_discount_bps: 8000,
  max_discount_bps: 9500,
}
const originalAdapter = api.defaults.adapter
const client = new QueryClient({
  defaultOptions: { queries: { retry: false } },
})
let requestedPages: number[] = []

// 只替换网络边界，真实查询和组件负责展开后请求以及分页结果替换。
api.defaults.adapter = async (config) => {
  const page = Number(config.params.p)
  requestedPages.push(page)
  return {
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
    data: {
      success: true,
      data: {
        items:
          page === 1
            ? item.rules
            : [{ model_name: 'model-21', discount_bps: 7500 }],
        total: 21,
        page_size: 20,
      },
    },
  }
}
notifyManager.setScheduler(queueMicrotask)

let host: HTMLDivElement | null = null
let root: ReturnType<typeof createRoot> | null = null

// 用真实受控展开状态验证移动端行的展开行为，而不是只断言静态初始 DOM。
function TestRowHarness(props: { onEdit: () => void }) {
  const [expanded, setExpanded] = useState(false)
  return (
    <UserModelPricingMobileRow
      item={item}
      expanded={expanded}
      onToggle={() => setExpanded((current) => !current)}
      onEdit={props.onEdit}
    />
  )
}

afterEach(async () => {
  if (root) {
    await act(async () => root?.unmount())
    root = null
  }
  host?.remove()
  host = null
  document.body.replaceChildren()
  client.clear()
  requestedPages = []
})

after(() => {
  api.defaults.adapter = originalAdapter
  notifyManager.setScheduler((callback) => setTimeout(callback, 0))
})

describe('user model pricing overview row', () => {
  // 收起时不读取明细，展开后分页加载并保持编辑入口可用。
  test('loads rules only on expansion and replaces the current page', async () => {
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
    let edited = false

    await act(async () => {
      root?.render(
        <I18nextProvider i18n={i18n}>
          <QueryClientProvider client={client}>
            <TestRowHarness onEdit={() => (edited = true)} />
          </QueryClientProvider>
        </I18nextProvider>
      )
    })

    assert.equal(document.body.textContent?.includes('model-04'), false)
    assert.deepEqual(requestedPages, [])
    const expandButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Show discounts for pricing-user"]'
    )
    assert.ok(expandButton)
    await act(async () => expandButton.click())
    assert.equal(document.body.textContent?.includes('model-04'), true)
    assert.equal(document.body.textContent?.includes('95%'), true)
    assert.deepEqual(requestedPages, [1])
    const next = [...document.querySelectorAll('button')].find(
      (button) => button.textContent === 'Next page'
    )
    assert.ok(next)
    await act(async () => next.click())
    assert.deepEqual(requestedPages, [1, 2])
    assert.equal(document.body.textContent?.includes('model-21'), true)
    assert.equal(document.body.textContent?.includes('model-04'), false)

    const editButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Edit discounts for pricing-user"]'
    )
    assert.ok(editButton)
    await act(async () => editButton.click())
    assert.equal(edited, true)
  })
})
