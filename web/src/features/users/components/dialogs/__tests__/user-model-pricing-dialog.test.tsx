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

import { afterEach, describe, test } from 'vitest'

// 使用 Vitest 的 jsdom，避免混用 DOM 实现导致节点归属判断失真。
const domWindow = window

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
  put: ApiMethod
}
type RenderedDialog = {
  host: HTMLDivElement
  queryClient: InstanceType<typeof QueryClient>
  root: ReturnType<typeof createRoot>
  renderOpen: (open: boolean) => Promise<void>
}

const models = Array.from({ length: 30 }, (_, index) => ({
  id: index + 1,
  model_name: `model-${String(index + 1).padStart(2, '0')}`,
  quota_type: 0,
  model_ratio: 1,
  completion_ratio: 1,
  enable_groups: ['default'],
}))

// 生成多页测试所需的完整已配置规则集，保持弹窗只展示有专属折扣的模型。
function configuredItemsFor(pricingModels: readonly { model_name: string }[]) {
  return pricingModels.map(({ model_name }) => ({
    model_name,
    discount_bps: 8000,
  }))
}

const apiClient = api as unknown as MockableApi
const originalGet = apiClient.get
const originalPut = apiClient.put
let renderedDialog: RenderedDialog | null = null

// 为组件测试提供稳定的定价、状态和已有折扣数据，避免依赖真实后端。
function installApiFixtures(
  pricingModels = models,
  pricingItems = [{ model_name: 'model-03', discount_bps: 8000 }]
) {
  apiClient.get = async (url) => {
    switch (url) {
      case '/api/status':
        return { data: { data: { price: 1, usd_exchange_rate: 1 } } }
      case '/api/user/42/model-pricing':
        return {
          data: {
            success: true,
            data: {
              user_id: 42,
              items: pricingItems,
              revision: 1,
              model_names: pricingModels.map((model) => model.model_name),
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

async function renderDialog(expectedInputCount = 25, configuredOnly = false) {
  const host = document.createElement('div')
  document.body.append(host)
  const root = createRoot(host)
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { gcTime: 0 } },
  })
  queryClient.setQueryData(
    ['status'],
    { price: 1, usd_exchange_rate: 1 },
    { updatedAt: Date.now() + 60_000 }
  )
  // 保留同一组件实例，验证受控关闭与重新打开时草稿会话的生命周期。
  const renderOpen = async (open: boolean) => {
    await act(async () =>
      root.render(
        <QueryClientProvider client={queryClient}>
          <I18nextProvider i18n={i18n}>
            <UserModelPricingDialog
              open={open}
              onOpenChange={() => undefined}
              user={{ id: 42, username: 'pricing-user' }}
              configuredOnly={configuredOnly}
            />
          </I18nextProvider>
        </QueryClientProvider>
      )
    )
  }
  renderedDialog = { host, queryClient, root, renderOpen }
  await renderOpen(true)
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
  return [
    ...document.querySelectorAll<HTMLElement>(
      '[data-slot="model-pricing-scroll"] span[title]'
    ),
  ].map((item) => item.title)
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

afterEach(async () => {
  apiClient.get = originalGet
  apiClient.put = originalPut
  if (renderedDialog) {
    await act(async () => renderedDialog?.root.unmount())
    renderedDialog.queryClient.clear()
    renderedDialog.host.remove()
    renderedDialog = null
  }
  document.body.replaceChildren()
})

describe('user model pricing dialog', () => {
  // 全选覆盖分页外的模型，超出规则上限时整批拒绝，原价重置仍然可用。
  test('selects across all pages and rejects batches exceeding the rule limit', async () => {
    const bulkModels = Array.from({ length: 1001 }, (_, index) => ({
      ...models[0],
      id: index + 1,
      model_name: `bulk-${index}`,
    }))
    installApiFixtures(bulkModels, configuredItemsFor(bulkModels))
    await renderDialog(25)
    const selectAll = document.querySelector<HTMLElement>(
      '[aria-label="Select all (filtered)"]'
    )
    assert.ok(selectAll)
    await act(async () => selectAll.click())
    assert.ok(document.body.textContent?.includes('1001 model(s) selected'))
    const batchToolbar = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-batch"]'
    )
    assert.ok(batchToolbar)
    assert.equal(batchToolbar.getAttribute('role'), 'toolbar')
    assert.equal(batchToolbar.dataset.placement, 'floating')
    const batch = document.querySelector<HTMLInputElement>(
      '[aria-label="Batch discount percentage"]'
    )
    const apply = [
      ...document.querySelectorAll<HTMLButtonElement>('button'),
    ].find((button) => button.textContent === 'Apply discount')
    assert.ok(batch)
    assert.ok(apply)
    await changeInput(batch, '80')
    await act(async () => apply.click())
    assert.ok(
      document.body.textContent?.includes(
        'At most 1000 model pricing rules are allowed'
      )
    )
    assert.equal(getModelInput('bulk-0').value, '80')
    await changeInput(batch, '100')
    await act(async () => apply.click())
    assert.equal(batch.getAttribute('aria-invalid'), 'false')
  })

  // 表头和模型行必须共用三列模板，保证全选框、模型名和输入框在同一竖线上。
  test('keeps the model header and rows on the same column template', async () => {
    installApiFixtures(models, configuredItemsFor(models))
    await renderDialog(25)

    const header = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-header"]'
    )
    const row = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-row"]'
    )
    assert.ok(header)
    assert.ok(row)

    for (const columnClass of [
      'grid-cols-[2rem_minmax(0,1fr)_7rem]',
      'sm:grid-cols-[2rem_minmax(0,1fr)_9rem]',
    ]) {
      assert.equal(header.classList.contains(columnClass), true)
      assert.equal(row.classList.contains(columnClass), true)
    }
  })

  // 跨页与筛选保留选择，批量操作只写草稿，提交仍携带原 revision。
  test('applies a batch discount across pages without changing unselected models', async () => {
    installApiFixtures(models, configuredItemsFor(models))
    await renderDialog(25)
    const selectFirst = document.querySelector<HTMLElement>(
      '[aria-label="Select model model-01"]'
    )
    assert.ok(selectFirst)
    await act(async () => selectFirst.click())
    const next = document.querySelector<HTMLButtonElement>(
      '[aria-label="Next page"]'
    )
    assert.ok(next)
    await act(async () => next.click())
    const selectLast = document.querySelector<HTMLElement>(
      '[aria-label="Select model model-30"]'
    )
    assert.ok(selectLast)
    await act(async () => selectLast.click())
    const batch = document.querySelector<HTMLInputElement>(
      '[aria-label="Batch discount percentage"]'
    )
    assert.ok(batch)
    await changeInput(batch, '65.25')
    const apply = [
      ...document.querySelectorAll<HTMLButtonElement>('button'),
    ].find((button) => button.textContent === 'Apply discount')
    assert.ok(apply)
    let submitted: unknown
    apiClient.put = async (_url, payload) => {
      submitted = payload
      return { data: { success: true } }
    }
    await act(async () => apply.click())
    assert.equal(submitted, undefined)
    assert.equal(getModelInput('model-30').value, '65.25')
    assert.equal(getModelInput('model-29').value, '80')
    const form = document.querySelector<HTMLFormElement>(
      '#user-model-pricing-form'
    )
    assert.ok(form)
    await act(async () =>
      form.dispatchEvent(
        new domWindow.Event('submit', {
          bubbles: true,
          cancelable: true,
        }) as unknown as Event
      )
    )
    assert.ok(submitted)
    const submittedPayload = submitted as {
      revision: number
      items: { model_name: string; discount_bps: number }[]
    }
    assert.equal(submittedPayload.revision, 1)
    assert.equal(submittedPayload.items.length, models.length)
    assert.deepEqual(
      submittedPayload.items.find((item) => item.model_name === 'model-01'),
      { model_name: 'model-01', discount_bps: 6525 }
    )
    assert.deepEqual(
      submittedPayload.items.find((item) => item.model_name === 'model-30'),
      { model_name: 'model-30', discount_bps: 6525 }
    )
    assert.deepEqual(
      submittedPayload.items.find((item) => item.model_name === 'model-29'),
      { model_name: 'model-29', discount_bps: 8000 }
    )
  })

  // 全选只覆盖当前已配置模型；原价批量设置清除专属折扣，关闭后不能残留选择。
  test('selects configured models and resets selection when reopened', async () => {
    installApiFixtures()
    await renderDialog(1, true)
    const selectAll = document.querySelector<HTMLElement>(
      '[aria-label="Select all (filtered)"]'
    )
    assert.ok(selectAll)
    await act(async () => selectAll.click())
    assert.ok(document.body.textContent?.includes('1 model(s) selected'))
    const apply = [
      ...document.querySelectorAll<HTMLButtonElement>('button'),
    ].find((button) => button.textContent === 'Apply discount')
    assert.ok(apply)
    await act(async () => apply.click())
    assert.equal(getModelInput('model-03').value, '100')
    assert.ok(renderedDialog)
    await renderedDialog.renderOpen(false)
    await renderedDialog.renderOpen(true)
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().length === 1,
        'reopened models missing'
      )
    )
    assert.equal(
      document.querySelector('[aria-label="Batch discount percentage"]'),
      null
    )
    assert.equal(getModelInput('model-03').value, '80')
  })

  // 空值及越界值不能写入所选模型，也不能让未应用的批量输入阻止保存草稿。
  test('rejects invalid batch values and clears the selection explicitly', async () => {
    installApiFixtures(
      models.slice(0, 3),
      configuredItemsFor(models.slice(0, 3))
    )
    await renderDialog(3)
    const selectAll = document.querySelector<HTMLElement>(
      '[aria-label="Select all (filtered)"]'
    )
    assert.ok(selectAll)
    await act(async () => selectAll.click())
    const batch = document.querySelector<HTMLInputElement>(
      '[aria-label="Batch discount percentage"]'
    )
    const apply = [
      ...document.querySelectorAll<HTMLButtonElement>('button'),
    ].find((button) => button.textContent === 'Apply discount')
    assert.ok(batch)
    assert.ok(apply)
    for (const value of ['', '0', '101']) {
      await changeInput(batch, value)
      await act(async () => apply.click())
      assert.equal(batch.getAttribute('aria-invalid'), 'true')
      assert.equal(getModelInput('model-03').value, '80')
      assert.equal(getModelInput('model-01').value, '80')
    }
    const clear = document.querySelector<HTMLButtonElement>(
      '[aria-label="Clear selection"]'
    )
    assert.ok(clear)
    await act(async () => clear.click())
    assert.equal(
      document.querySelector('[aria-label="Batch discount percentage"]'),
      null
    )
    assert.equal(selectAll.getAttribute('aria-checked'), 'false')
  })

  // 停用模型不显示，但完整替换提交仍须保留其历史折扣。
  test('shows only enabled models and preserves hidden discounts on save', async () => {
    installApiFixtures(models.slice(0, 3))
    const fixtureGet = apiClient.get
    apiClient.get = async (url) => {
      if (url === '/api/user/42/model-pricing') {
        return {
          data: {
            success: true,
            data: {
              user_id: 42,
              revision: 1,
              model_names: models.slice(0, 3).map((model) => model.model_name),
              items: [
                { model_name: 'disabled-model', discount_bps: 6000 },
                { model_name: 'model-03', discount_bps: 8000 },
              ],
            },
          },
        }
      }
      return fixtureGet(url)
    }
    await renderDialog(1, true)
    assert.equal(visibleModelNames().includes('disabled-model'), false)
    await changeInput(getModelInput('model-03'), '100')
    let submitted: unknown
    apiClient.put = async (_url, payload) => {
      submitted = payload
      return { data: { success: true } }
    }
    const form = document.querySelector<HTMLFormElement>(
      '#user-model-pricing-form'
    )
    assert.ok(form)
    await act(async () => {
      form.dispatchEvent(
        new domWindow.Event('submit', {
          bubbles: true,
          cancelable: true,
        }) as unknown as Event
      )
    })
    assert.deepEqual(submitted, {
      revision: 1,
      items: [{ model_name: 'disabled-model', discount_bps: 6000 }],
    })
  })

  // 重新打开失败时不能把旧缓存当成最新会话，重试成功后再恢复编辑。
  test('blocks editing cached rules when reopening fails and recovers on retry', async () => {
    installApiFixtures(models.slice(0, 3))
    await renderDialog(1, true)
    assert.ok(renderedDialog)
    await renderedDialog.renderOpen(false)
    const fixtureGet = apiClient.get
    apiClient.get = async (url) => {
      if (url === '/api/user/42/model-pricing') throw new Error('unavailable')
      return fixtureGet(url)
    }
    await renderedDialog.renderOpen(true)
    await act(async () =>
      waitForCondition(
        () =>
          document.body.textContent?.includes('Failed to load model pricing') ??
          false,
        'load failure was not shown'
      )
    )
    const save = document.querySelector<HTMLButtonElement>(
      'button[type="submit"][form="user-model-pricing-form"]'
    )
    assert.ok(save)
    assert.equal(save.disabled, true)
    assert.equal(document.querySelector('input[type="number"]'), null)
    apiClient.get = fixtureGet
    const retry = [
      ...document.querySelectorAll<HTMLButtonElement>('button'),
    ].find((button) => button.textContent === 'Retry')
    assert.ok(retry)
    await act(async () => retry.click())
    await act(async () =>
      waitForCondition(
        () => visibleModelNames().length === 1,
        'retry did not restore editing'
      )
    )
    assert.equal(getModelInput('model-03').value, '80')
    assert.equal(save.disabled, false)
  })

  // 后台规则和模型目录变化不能覆盖草稿，也不能让旧草稿携带新 revision 绕过冲突检查。
  test('keeps draft and original revision through refetch and a save conflict', async () => {
    installApiFixtures()
    await renderDialog(1, true)
    assert.ok(renderedDialog)
    await changeInput(getModelInput('model-03'), '55')
    const initialNames = visibleModelNames()
    const fixtureGet = apiClient.get
    apiClient.get = async (url) => {
      if (url === '/api/user/42/model-pricing') {
        return {
          data: {
            success: true,
            data: {
              user_id: 42,
              revision: 2,
              model_names: [models[0], ...models.slice(2)].map(
                (model) => model.model_name
              ),
              items: [{ model_name: 'model-01', discount_bps: 7000 }],
            },
          },
        }
      }
      return fixtureGet(url)
    }
    await act(async () => {
      await renderedDialog?.queryClient.refetchQueries({
        queryKey: ['user-model-pricing', 42],
      })
    })
    assert.equal(getModelInput('model-03').value, '55')
    assert.deepEqual(visibleModelNames(), initialNames)
    let submitted: unknown
    const { AxiosError } = await import('axios')
    apiClient.put = async (_url, payload) => {
      submitted = payload
      const error = new AxiosError('conflict')
      error.response = { status: 409 } as NonNullable<typeof error.response>
      throw error
    }
    const form = document.querySelector<HTMLFormElement>(
      '#user-model-pricing-form'
    )
    assert.ok(form)
    await act(async () => {
      form.dispatchEvent(
        new domWindow.Event('submit', {
          bubbles: true,
          cancelable: true,
        }) as unknown as Event
      )
    })
    assert.deepEqual(submitted, {
      revision: 1,
      items: [{ model_name: 'model-03', discount_bps: 5500 }],
    })
    assert.equal(getModelInput('model-03').value, '55')
    // 重新打开才加载最新完整快照，避免后台自动合并造成其他管理员的规则被覆盖。
    await renderedDialog.renderOpen(false)
    await renderedDialog.renderOpen(true)
    await act(async () =>
      waitForCondition(
        () => visibleModelNames()[0] === 'model-01',
        'latest rules were not loaded'
      )
    )
    assert.equal(getModelInput('model-01').value, '70')
    assert.deepEqual(visibleModelNames(), ['model-01'])
  })

  test('lists configured models, preserves values, searches, and paginates', async () => {
    installApiFixtures(models, configuredItemsFor(models))
    await renderDialog(25, true)

    assert.equal(visibleModelNames().length, 25)
    // 列表只包含已配置模型，并保持接口返回的稳定顺序。
    assert.deepEqual(visibleModelNames().slice(0, 4), [
      'model-01',
      'model-02',
      'model-03',
      'model-04',
    ])
    assert.equal(visibleModelNames().includes('model-25'), true)
    assert.equal(visibleModelNames().includes('model-26'), false)
    assert.equal(getModelInput('model-01').value, '80')
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
    // 分页留在同一表单内，但不属于模型滚动区；批量操作可增加中间布局层。
    assert.equal(modelPricingScroll.contains(nextPageButton), false)
    assert.equal(modelPricingPagination.contains(nextPageButton), true)
    const modelPricingList = document.querySelector<HTMLElement>(
      '[data-slot="model-pricing-list"]'
    )
    assert.ok(modelPricingList)
    assert.equal(modelPricingList.contains(modelPricingPagination), false)
    assert.equal(
      modelPricingList.closest('form')?.contains(modelPricingPagination),
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
    const sparseModels = models.slice(0, 3)
    installApiFixtures(sparseModels, configuredItemsFor(sparseModels))
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

  // 编辑入口只展示当前用户已配置且仍启用的模型，不再把未改模型混入列表。
  test('shows only configured enabled models in the edit dialog', async () => {
    installApiFixtures()
    await renderDialog(1, true)

    assert.deepEqual(visibleModelNames(), ['model-03'])
    assert.equal(
      document.querySelector<HTMLElement>('[data-slot="select-trigger"]'),
      null
    )
  })

  // 用户管理入口展示接口返回的全部启用模型，未配置项按原价回填。
  test('shows all enabled models in the user model pricing dialog', async () => {
    installApiFixtures()
    await renderDialog()

    assert.equal(visibleModelNames().length, 25)
    assert.equal(visibleModelNames().includes('model-01'), true)
    assert.equal(visibleModelNames().includes('model-25'), true)
    assert.equal(visibleModelNames().includes('model-26'), false)
    assert.equal(getModelInput('model-01').value, '100')
    assert.equal(getModelInput('model-03').value, '80')
  })

  // 提交分页外的空折扣时，页面必须回到错误行并显示字段错误。
  test('reveals an invalid discount after submitting from another page', async () => {
    installApiFixtures(models, configuredItemsFor(models))
    await renderDialog(25)

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
