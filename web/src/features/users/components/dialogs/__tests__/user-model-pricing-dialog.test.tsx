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
import { after, afterEach, describe, test } from 'node:test'

import { Window } from 'happy-dom'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLButtonElement',
  'HTMLInputElement',
  'HTMLFormElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
  'KeyboardEvent',
  'PointerEvent',
  'MouseEvent',
  'FocusEvent',
  'CustomEvent',
  'MutationObserver',
  'ResizeObserver',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'getComputedStyle',
] as const

for (const key of domGlobals) {
  Object.defineProperty(globalThis, key, {
    configurable: true,
    value: domWindow[key],
  })
}

const { act } = await import('react')
const { createRoot } = await import('react-dom/client')
const { QueryClient, QueryClientProvider } =
  await import('@tanstack/react-query')
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { api } = await import('@/lib/api')
const { UserModelPricingDialog } = await import('../user-model-pricing-dialog')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: { en: { translation: {} } },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

type ApiMethod = (url: string, data?: unknown) => Promise<{ data: unknown }>
type MockableApi = {
  get: ApiMethod
}
type RenderedDialog = {
  host: HTMLDivElement
  queryClient: InstanceType<typeof QueryClient>
  root: ReturnType<typeof createRoot>
}

const models = Array.from({ length: 30 }, (_, index) => ({
  id: index + 1,
  model_name: `model-${String(index + 1).padStart(2, '0')}`,
  quota_type: 0,
  model_ratio: 1,
  completion_ratio: 1,
  enable_groups: ['default'],
}))

const apiClient = api as unknown as MockableApi
const originalGet = apiClient.get
let renderedDialog: RenderedDialog | null = null

// 为组件测试提供稳定的定价、状态和已有折扣数据，避免依赖真实后端。
function installApiFixtures(pricingModels = models) {
  apiClient.get = async (url) => {
    switch (url) {
      case '/api/status':
        return { data: { data: { price: 1, usd_exchange_rate: 1 } } }
      case '/api/pricing':
        return {
          data: {
            success: true,
            data: pricingModels,
            vendors: [],
            group_ratio: { default: 1 },
            usable_group: { default: { desc: 'Default', ratio: 1 } },
            supported_endpoint: {},
            auto_groups: [],
          },
        }
      case '/api/user/42/model-pricing':
        return {
          data: {
            success: true,
            data: {
              user_id: 42,
              items: [{ model_name: 'model-03', discount_bps: 8000 }],
              revision: 1,
            },
          },
        }
      default:
        throw new Error(`Unexpected GET ${url}`)
    }
  }
}

async function waitForCondition(
  condition: () => boolean,
  failureMessage: string
): Promise<void> {
  if (condition()) return

  await new Promise<void>((resolve, reject) => {
    const observer = new MutationObserver(() => {
      if (!condition()) return
      clearTimeout(timeoutId)
      observer.disconnect()
      resolve()
    })
    const timeoutId = setTimeout(() => {
      observer.disconnect()
      reject(new Error(`${failureMessage}: ${document.body.textContent}`))
    }, 1500)

    observer.observe(document, {
      attributes: true,
      childList: true,
      characterData: true,
      subtree: true,
    })
  })
}

async function renderDialog(expectedInputCount = 25) {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  queryClient.setQueryData(
    ['status'],
    { price: 1, usd_exchange_rate: 1 },
    { updatedAt: Date.now() + 60_000 }
  )
  renderedDialog = { host, queryClient, root }

  await act(async () =>
    root.render(
      <QueryClientProvider client={queryClient}>
        <I18nextProvider i18n={i18n}>
          <UserModelPricingDialog
            open
            onOpenChange={() => undefined}
            user={{ id: 42, username: 'pricing-user' }}
          />
        </I18nextProvider>
      </QueryClientProvider>
    )
  )
  await act(
    async () =>
      await waitForCondition(
        () =>
          document.querySelectorAll('input[type="number"]').length ===
          expectedInputCount,
        'model pricing dialog did not render the first page'
      )
  )
}

function visibleModelNames(): string[] {
  return [...document.querySelectorAll<HTMLElement>('span[title]')].map(
    (item) => item.title
  )
}

function getModelInput(modelName: string): HTMLInputElement {
  const model = [...document.querySelectorAll<HTMLElement>('span[title]')].find(
    (item) => item.title === modelName
  )
  assert.ok(model, `Expected visible model ${modelName}`)
  const row = model.closest('div.grid')
  assert.ok(row)
  const input = row.querySelector<HTMLInputElement>('input[type="number"]')
  assert.ok(input)
  return input
}

async function changeInput(input: HTMLInputElement, value: string) {
  await act(async () => {
    const valueSetter = Object.getOwnPropertyDescriptor(
      domWindow.HTMLInputElement.prototype,
      'value'
    )?.set
    assert.ok(valueSetter)
    valueSetter.call(input, value)
    input.dispatchEvent(
      new domWindow.Event('input', { bubbles: true }) as unknown as Event
    )
  })
}

// 通过用户可见的下拉选项切换折扣配置状态，覆盖筛选控件的真实交互路径。
async function selectDiscountFilter(label: string) {
  const trigger = document.querySelector<HTMLButtonElement>(
    '[data-slot="select-trigger"]'
  )
  assert.ok(trigger)

  await act(async () => trigger.click())
  await act(async () =>
    waitForCondition(
      () =>
        [
          ...document.querySelectorAll<HTMLElement>(
            '[data-slot="select-item"]'
          ),
        ].some((item) => item.textContent?.trim() === label),
      `discount filter option was not rendered: ${label}`
    )
  )

  const item = [
    ...document.querySelectorAll<HTMLElement>('[data-slot="select-item"]'),
  ].find((option) => option.textContent?.trim() === label)
  assert.ok(item)
  await act(async () => item.click())
}

afterEach(async () => {
  apiClient.get = originalGet
  if (renderedDialog) {
    await act(async () => renderedDialog?.root.unmount())
    renderedDialog.queryClient.clear()
    renderedDialog.host.remove()
    renderedDialog = null
  }
  document.body.replaceChildren()
})

after(() => {
  domWindow.close()
})

describe('user model pricing dialog', () => {
  test('lists enabled models, preserves configured values, searches, and paginates', async () => {
    installApiFixtures()
    await renderDialog()

    assert.equal(visibleModelNames().length, 25)
    // 已保存折扣的模型优先显示，其余模型继续保持原始顺序。
    assert.deepEqual(visibleModelNames().slice(0, 4), [
      'model-03',
      'model-01',
      'model-02',
      'model-04',
    ])
    assert.equal(visibleModelNames().includes('model-25'), true)
    assert.equal(visibleModelNames().includes('model-26'), false)
    assert.equal(getModelInput('model-01').value, '100')
    assert.equal(getModelInput('model-03').value, '80')

    await changeInput(getModelInput('model-03'), '')
    assert.equal(getModelInput('model-03').value, '')

    const nextPageButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Next page"]'
    )
    assert.ok(nextPageButton)
    const modelPricingScroll = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-scroll"]'
    )
    assert.ok(modelPricingScroll)
    const modelPricingPagination = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-pagination"]'
    )
    assert.ok(modelPricingPagination)
    // 分页是模型列表的独立兄弟层，不随模型行滚动。
    assert.equal(modelPricingScroll.contains(nextPageButton), false)
    assert.equal(modelPricingPagination.contains(nextPageButton), true)
    const modelPricingList = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-list"]'
    )
    assert.ok(modelPricingList)
    assert.equal(modelPricingList.contains(modelPricingPagination), false)
    assert.equal(
      modelPricingList.parentElement?.contains(modelPricingPagination),
      true
    )

    // 高度约束必须从弹窗主体连续传递到模型滚动区，避免分页溢出并覆盖底部操作栏。
    const dialogBody = document.querySelector<HTMLElement>(
      '[data-slot="dialog-body"]'
    )
    assert.ok(dialogBody)
    assert.equal(dialogBody.classList.contains('flex'), true)
    assert.equal(dialogBody.classList.contains('overflow-y-hidden'), true)

    const dialogBodyContent = dialogBody.firstElementChild
    assert.ok(dialogBodyContent)
    assert.equal(dialogBodyContent.classList.contains('flex'), true)
    assert.equal(dialogBodyContent.classList.contains('flex-1'), true)

    const form = document.querySelector<HTMLFormElement>(
      '#user-model-pricing-form'
    )
    assert.ok(form)
    assert.equal(form.classList.contains('flex-1'), true)
    assert.equal(form.classList.contains('overflow-hidden'), true)
    assert.equal(modelPricingList.classList.contains('flex'), true)
    assert.equal(modelPricingList.classList.contains('flex-1'), false)
    assert.equal(modelPricingScroll.classList.contains('flex-1'), true)

    await act(async () => nextPageButton.click())
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().includes('model-26'),
        'next page was not rendered'
      )
    )
    assert.equal(visibleModelNames().length, 5)

    const searchInput = document.querySelector<HTMLInputElement>(
      'input[aria-label="Search models"]'
    )
    assert.ok(searchInput)
    await changeInput(searchInput, 'model-30')
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().length === 1,
        'model search did not filter the list'
      )
    )
    assert.deepEqual(visibleModelNames(), ['model-30'])
  })

  test('sizes a sparse model list to its content without pagination', async () => {
    installApiFixtures(models.slice(0, 3))
    await renderDialog(3)

    assert.equal(visibleModelNames().length, 3)
    assert.equal(document.querySelector('button[aria-label="Next page"]'), null)

    const dialogContent = document.querySelector<HTMLElement>(
      '[data-slot="dialog-content"]'
    )
    assert.ok(dialogContent)
    assert.match(
      dialogContent.getAttribute('style') ?? '',
      /--dialog-content-height:\s*auto/
    )

    const modelList = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-list"]'
    )
    assert.ok(modelList)
    assert.equal(modelList.classList.contains('flex-1'), false)

    const dialogBody = document.querySelector<HTMLElement>(
      '[data-slot="dialog-body"]'
    )
    assert.ok(dialogBody)
    assert.equal(dialogBody.classList.contains('overflow-y-hidden'), true)
  })

  test('filters models by configured discount status', async () => {
    installApiFixtures()
    await renderDialog()

    const discountFilterTrigger = document.querySelector<HTMLElement>(
      '[data-slot="select-trigger"]'
    )
    assert.ok(discountFilterTrigger)
    assert.equal(discountFilterTrigger.textContent?.includes('All'), true)

    await selectDiscountFilter('Configured discounts')
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().length === 1,
        'configured discount filter did not narrow the list'
      )
    )
    assert.deepEqual(visibleModelNames(), ['model-03'])

    await selectDiscountFilter('Unconfigured discounts')
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().length === 25,
        'unconfigured discount filter did not render the first page'
      )
    )
    assert.equal(visibleModelNames().includes('model-03'), false)
  })

  // 提交分页外的空折扣时，页面必须回到错误行并显示字段错误。
  test('reveals an invalid discount after submitting from another page', async () => {
    installApiFixtures()
    await renderDialog()

    const nextPageButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Next page"]'
    )
    assert.ok(nextPageButton)
    await act(async () => nextPageButton.click())
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().includes('model-30'),
        'second page was not rendered'
      )
    )

    await changeInput(getModelInput('model-30'), '')
    const previousPageButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Previous page"]'
    )
    assert.ok(previousPageButton)
    await act(async () => previousPageButton.click())
    await act(async () =>
      waitForCondition(
        () =>
          visibleModelNames().includes('model-01') &&
          !visibleModelNames().includes('model-30'),
        'first page was not rendered'
      )
    )

    const saveButton = document.querySelector<HTMLButtonElement>(
      'button[type="submit"][form="user-model-pricing-form"]'
    )
    assert.ok(saveButton)
    const form = document.querySelector<HTMLFormElement>(
      '#user-model-pricing-form'
    )
    assert.ok(form)
    // happy-dom 不支持弹窗外 form 属性按钮的原生提交，这里直接触发表单提交事件。
    await act(async () =>
      form.dispatchEvent(
        new domWindow.Event('submit', {
          bubbles: true,
          cancelable: true,
        }) as unknown as Event
      )
    )
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().includes('model-30'),
        'invalid model was not revealed'
      )
    )

    await act(async () =>
      waitForCondition(
        () =>
          Boolean(
            getModelInput('model-30')
              .closest('[data-slot="form-item"]')
              ?.querySelector('[aria-invalid="true"]')
          ),
        'invalid model did not receive an error state'
      )
    )
    const invalidInput = getModelInput('model-30')
    assert.equal(invalidInput.value, '')
    assert.equal(
      invalidInput
        .closest('[data-slot="form-item"]')
        ?.textContent?.includes('Discount percentage is required'),
      true
    )
  })
})
