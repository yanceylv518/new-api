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

// jsdom 未实现动画查询，Base UI 滚动区的异步清理需要这个浏览器 API。
if (typeof HTMLElement.prototype.getAnimations !== 'function') {
  Object.defineProperty(HTMLElement.prototype, 'getAnimations', {
    configurable: true,
    value: () => [] as Animation[],
  })
}

// 使用 rc35 的 Vitest/jsdom 环境，避免混用 DOM 和运行器。
const { act, useState } = await import('react')
const { createRoot } = await import('react-dom/client')
const { QueryClient, QueryClientProvider, notifyManager } =
  await import('@tanstack/react-query')
const { api } = await import('@/lib/api')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { getUserModelPricingOverview } = await import('../../api')
const { UserModelPricingDetailsSheet, UserModelPricingMobileRow } =
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
let requestedOverviewParams: Record<string, unknown> = {}

// 只替换网络边界，真实查询和组件负责展开后请求以及分页结果替换。
api.defaults.adapter = async (config) => {
  const page = Number(config.params.p)
  requestedPages.push(page)
  requestedOverviewParams = config.params
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

// 用真实受控状态验证用户行打开抽屉，并覆盖抽屉关闭后的清理。
function TestRowHarness(props: { onEdit: () => void }) {
  const [selected, setSelected] = useState(false)
  return (
    <>
      <UserModelPricingMobileRow
        item={item}
        selected={selected}
        onOpen={() => setSelected((current) => !current)}
        onEdit={props.onEdit}
      />
      {selected ? (
        <UserModelPricingDetailsSheet
          open
          item={item}
          keyword=''
          onOpenChange={(open) => {
            if (!open) setSelected(false)
          }}
        />
      ) : null}
    </>
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
  requestedOverviewParams = {}
})

after(() => {
  api.defaults.adapter = originalAdapter
  notifyManager.setScheduler((callback) => setTimeout(callback, 0))
})

describe('user model pricing overview row', () => {
  // 筛选器选择的值必须进入总览请求，后端才能同步过滤用户和统计数据。
  test('sends group and role filters to the overview request', async () => {
    await getUserModelPricingOverview({
      keyword: 'pricing',
      group: 'team-a',
      role: '10',
      p: 2,
      page_size: 20,
    })

    assert.equal(requestedOverviewParams.keyword, 'pricing')
    assert.equal(requestedOverviewParams.group, 'team-a')
    assert.equal(requestedOverviewParams.role, '10')
    assert.equal(requestedOverviewParams.p, 2)
  })

  // 移动端用户摘要应展示角色，不能把账号启用状态混入折扣总览。
  test('shows the user role without showing the account status', async () => {
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
    const adminItem = {
      ...item,
      user: { ...item.user, display_name: 'Pricing', role: 10 },
    }

    await act(async () => {
      root?.render(
        <I18nextProvider i18n={i18n}>
          <UserModelPricingMobileRow
            item={adminItem}
            selected={false}
            onOpen={() => undefined}
            onEdit={() => undefined}
          />
        </I18nextProvider>
      )
    })

    const content = document.body.textContent ?? ''
    assert.equal(content.includes('Admin'), true)
    assert.equal(content.includes('Enabled'), false)
  })

  // 收起时不读取明细，打开抽屉后自动追加下一批并保持主表编辑入口可用。
  test('opens a detail drawer and loads the next rule page near the bottom', async () => {
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

    assert.equal(document.body.textContent?.includes('model-01'), true)
    assert.equal(document.body.textContent?.includes('model-03'), true)
    assert.equal(document.body.textContent?.includes('model-04'), false)
    assert.deepEqual(requestedPages, [])
    const viewButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Show discounts for pricing-user"]'
    )
    assert.ok(viewButton)
    await act(async () => viewButton.click())
    assert.equal(document.body.textContent?.includes('model-04'), true)
    assert.equal(document.body.textContent?.includes('95%'), true)
    assert.equal(requestedPages[0], 1)
    assert.equal(document.body.textContent?.includes('Next page'), false)
    assert.equal(document.body.textContent?.includes('Page 1 of'), false)

    const viewport = document.querySelector<HTMLElement>(
      '[data-slot="scroll-area-viewport"]'
    )
    assert.ok(viewport)
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, value: 480 },
      scrollHeight: { configurable: true, value: 900 },
      scrollTop: { configurable: true, value: 440 },
    })
    await act(async () => {
      viewport.dispatchEvent(new Event('scroll'))
    })
    assert.equal(
      requestedPages.some((page) => page === 2),
      true
    )
    assert.equal(document.body.textContent?.includes('model-21'), true)

    assert.equal(
      document.querySelectorAll(
        'button[aria-label="Edit discounts for pricing-user"]'
      ).length,
      1
    )

    const closeDrawerButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Hide discounts for pricing-user"]'
    )
    assert.ok(closeDrawerButton)
    await act(async () => closeDrawerButton.click())
    assert.equal(document.body.textContent?.includes('model-04'), false)

    const editButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Edit discounts for pricing-user"]'
    )
    assert.ok(editButton)
    await act(async () => editButton.click())
    assert.equal(edited, true)
  })
})
