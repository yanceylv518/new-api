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

import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterAll as after, afterEach, describe, test } from 'vitest'

Object.assign(globalThis, { IS_REACT_ACT_ENVIRONMENT: true })

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
  defaultOptions: { queries: { retry: false, retryDelay: 0 } },
})
let requestedPages: number[] = []
let requestedOverviewParams: Record<string, unknown> = {}
let partialPageTest = false
let failNextPage = false
let nextPageAttempts = 0
// 覆盖抽屉第一页返回的规则，用于验证永久与临时折扣的展示。
let detailRules: Record<string, unknown>[] | null = null

// 只替换网络边界，真实查询和组件负责展开后请求以及分页结果替换。
api.defaults.adapter = async (config) => {
  const page = Number(config.params.p)
  requestedPages.push(page)
  requestedOverviewParams = config.params
  if (partialPageTest && page === 2) {
    nextPageAttempts++
    if (failNextPage) throw new Error('temporary next-page failure')
  }
  return {
    status: 200,
    statusText: 'OK',
    headers: {},
    config,
    data: config.url?.endsWith('/model-pricing/history')
      ? {
          success: true,
          data: {
            items: [
              {
                id: 1,
                user_id: 42,
                actor_id: 1,
                model_name: 'history-permanent-only',
                action: 'create',
                revision: 2,
                created_at: 100,
                before: null,
                after: {
                  mode: 'layered',
                  permanent: { discount_bps: 8000 },
                  periods: null,
                },
              },
            ],
            total: 1,
            page_size: 20,
          },
        }
      : {
          success: true,
          data: {
            items:
              page === 1
                ? (detailRules ?? item.rules)
                : [{ model_name: 'model-21', discount_bps: 7500 }],
            total:
              detailRules?.length ?? (partialPageTest && page === 2 ? 5 : 21),
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
  partialPageTest = false
  failNextPage = false
  nextPageAttempts = 0
  detailRules = null
})

after(() => {
  api.defaults.adapter = originalAdapter
  notifyManager.setScheduler((callback) => setTimeout(callback, 0))
})

describe('user model pricing overview row', () => {
  test('shows permanent-only history when the stored periods are null', async () => {
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
    await act(async () => {
      root?.render(
        <I18nextProvider i18n={i18n}>
          <QueryClientProvider client={client}>
            <TestRowHarness onEdit={() => undefined} />
          </QueryClientProvider>
        </I18nextProvider>
      )
    })
    await userEvent.click(
      screen.getByRole('button', { name: 'Show discounts for pricing-user' })
    )
    await userEvent.click(
      screen.getByRole('button', { name: 'Discount history' })
    )
    await waitFor(() => assert.ok(screen.getByText('history-permanent-only')))
    const article = screen
      .getByText('history-permanent-only')
      .closest('article')
    assert.ok(article)
    assert.ok(article.textContent?.includes('Permanent discount: 80%'))
  })
  test('keeps loaded rules and stops automatic paging after failure until retry', async () => {
    partialPageTest = true
    failNextPage = true
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
    await act(async () => {
      root?.render(
        <I18nextProvider i18n={i18n}>
          <QueryClientProvider client={client}>
            <TestRowHarness onEdit={() => undefined} />
          </QueryClientProvider>
        </I18nextProvider>
      )
    })
    await userEvent.click(
      screen.getByRole('button', { name: 'Show discounts for pricing-user' })
    )
    await waitFor(() =>
      assert.ok(document.body.textContent?.includes('model-04'))
    )
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
    await waitFor(() =>
      assert.ok(screen.getByRole('button', { name: 'Retry' }))
    )
    assert.equal(nextPageAttempts, 2)
    assert.ok(document.body.textContent?.includes('model-04'))
    await act(async () => {
      viewport.dispatchEvent(new Event('scroll'))
    })
    assert.equal(nextPageAttempts, 2)
    failNextPage = false
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() =>
      assert.ok(document.body.textContent?.includes('model-21'))
    )
    assert.equal(nextPageAttempts, 3)
    assert.equal(screen.queryByRole('button', { name: 'Retry' }), null)
  })
  test('defaults detail requests to active and resets pagination when switching status', async () => {
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
    await act(async () => {
      root?.render(
        <I18nextProvider i18n={i18n}>
          <QueryClientProvider client={client}>
            <TestRowHarness onEdit={() => undefined} />
          </QueryClientProvider>
        </I18nextProvider>
      )
    })
    await userEvent.click(
      screen.getByRole('button', { name: 'Show discounts for pricing-user' })
    )
    await waitFor(() => assert.equal(requestedOverviewParams.status, 'active'))
    for (const [label, status] of [
      ['Not started', 'pending'],
      ['Expired', 'expired'],
      ['All statuses', undefined],
    ] as const) {
      await userEvent.click(
        screen.getByRole('combobox', { name: 'Discount status' })
      )
      await userEvent.click(screen.getByRole('option', { name: label }))
      await waitFor(() => assert.equal(requestedOverviewParams.status, status))
      assert.equal(requestedOverviewParams.p, 1)
      assert.equal(
        screen.getByRole('combobox', { name: 'Discount status' }).textContent,
        label
      )
    }
    await userEvent.click(
      screen.getByRole('combobox', { name: 'Discount status' })
    )
    await userEvent.click(
      screen.getByRole('option', { name: 'Within validity period' })
    )
    assert.equal(
      screen.getByRole('combobox', { name: 'Discount status' }).textContent,
      'Within validity period'
    )
  })
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

  // 抽屉直接说明每条折扣的类型、有效期和生效时段，不需要再点开百分比。
  test('lists permanent and temporary discounts with their effective time', async () => {
    const start = new Date(2030, 9, 8).getTime() / 1000
    const end = new Date(2030, 9, 15).getTime() / 1000
    detailRules = [
      { model_name: 'model-01', mode: 'layered', slot: -1, discount_bps: 4500 },
      {
        model_name: 'model-02',
        mode: 'layered',
        slot: 0,
        discount_bps: 10000,
        start_time: start,
        end_time: end,
        expression:
          '((weekday("Asia/Shanghai") >= 0 && (hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai")) >= 1380) || (weekday("Asia/Shanghai") >= 0 && (hour("Asia/Shanghai") * 60 + minute("Asia/Shanghai")) < 420)) ? 0.68 : permanent_discount',
      },
    ]
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
    await act(async () => {
      root?.render(
        <I18nextProvider i18n={i18n}>
          <QueryClientProvider client={client}>
            <TestRowHarness onEdit={() => undefined} />
          </QueryClientProvider>
        </I18nextProvider>
      )
    })
    await userEvent.click(
      screen.getByRole('button', { name: 'Show discounts for pricing-user' })
    )
    const permanent = await screen.findByRole('row', { name: /model-01/ })
    assert.equal(permanent.textContent?.includes('Permanent'), true)
    assert.equal(permanent.textContent?.includes('45%'), true)
    assert.equal(permanent.textContent?.includes('Always'), true)
    const temporary = screen.getByRole('row', { name: /model-02/ })
    assert.equal(temporary.textContent?.includes('Temporary'), true)
    assert.equal(temporary.textContent?.includes('68%'), true)
    assert.equal(
      temporary.textContent?.includes('2030-10-08 → 2030-10-14'),
      true
    )
    assert.equal(
      temporary.textContent?.includes('Every day · 23:00–Next day 07:00'),
      true
    )
  })

  // 主表预览用图标区分临时折扣，避免和永久折扣混在一起。
  test('marks temporary discounts in the model preview', async () => {
    host = document.createElement('div')
    document.body.append(host)
    root = createRoot(host)
    const layeredItem = {
      ...item,
      rules: [
        {
          model_name: 'model-01',
          mode: 'layered' as const,
          slot: -1,
          discount_bps: 4500,
        },
        {
          model_name: 'model-02',
          mode: 'layered' as const,
          slot: 0,
          discount_bps: 6800,
          start_time: 100,
          end_time: 200,
        },
      ],
    }

    await act(async () => {
      root?.render(
        <I18nextProvider i18n={i18n}>
          <UserModelPricingMobileRow
            item={layeredItem}
            selected={false}
            onOpen={() => undefined}
            onEdit={() => undefined}
          />
        </I18nextProvider>
      )
    })

    assert.equal(screen.getAllByRole('img', { name: 'Temporary' }).length, 1)
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
