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
import { after, describe, test } from 'node:test'

import { Window } from 'happy-dom'

import type { TaskLog } from '../../../types'

const domWindow = new Window()
const domGlobals = [
  'window',
  'document',
  'navigator',
  'HTMLElement',
  'HTMLAnchorElement',
  'SVGElement',
  'Node',
  'Element',
  'Event',
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
const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { TaskLogDetailsCell } = await import('../task-logs-columns')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        Copy: 'Copy',
        'Copy Link': 'Copy Link',
        'Preview video': 'Preview video',
      },
    },
  },
})

const reactTestGlobals = globalThis as typeof globalThis & {
  IS_REACT_ACT_ENVIRONMENT?: boolean
}
reactTestGlobals.IS_REACT_ACT_ENVIRONMENT = true

const taskLog: TaskLog = {
  id: 3,
  user_id: 1,
  platform: '54',
  task_id: 'task_video_123',
  action: 'generate',
  channel_id: 4,
  submit_time: 1788741907,
  finish_time: 1788742030,
  progress: '100%',
  status: 'SUCCESS',
  result_url: 'https://example.com/video.mp4',
}

type RenderedDetails = {
  container: HTMLDivElement
  root: ReturnType<typeof createRoot>
}

async function renderDetails(log: TaskLog): Promise<RenderedDetails> {
  const container = document.createElement('div')
  document.body.append(container)
  const root = createRoot(container)

  await act(async () => {
    root.render(
      <I18nextProvider i18n={i18n}>
        <TaskLogDetailsCell log={log} />
      </I18nextProvider>
    )
  })

  return { container, root }
}

after(() => {
  domWindow.close()
})

describe('task log details', () => {
  // 点击预览必须通过统一客户端请求 Blob，并在弹窗关闭时释放对象地址。
  test('loads video through the API client and renders the returned blob', async () => {
    const { api } = await import('@/lib/api')
    const originalAdapter = api.defaults.adapter
    const originalCreate = URL.createObjectURL
    const originalRevoke = URL.revokeObjectURL
    let requested = false
    let revoked = false
    api.defaults.adapter = async (config) => {
      assert.equal(config.url, '/v1/videos/task_video_123/content')
      assert.equal(config.responseType, 'blob')
      requested = true
      return {
        data: new Blob(['video'], { type: 'video/mp4' }),
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      }
    }
    URL.createObjectURL = () => 'blob:test-video'
    URL.revokeObjectURL = () => {
      revoked = true
    }
    const rendered = await renderDetails(taskLog)
    try {
      await act(async () =>
        rendered.container.querySelector<HTMLButtonElement>('button')?.click()
      )
      assert.equal(requested, true)
      assert.equal(
        document.querySelector('video')?.getAttribute('src'),
        'blob:test-video'
      )
      await act(async () => rendered.root.unmount())
      assert.equal(revoked, true)
    } finally {
      await act(async () => rendered.root.unmount())
      rendered.container.remove()
      api.defaults.adapter = originalAdapter
      URL.createObjectURL = originalCreate
      URL.revokeObjectURL = originalRevoke
    }
  })
  // 成功任务存在 result_url 时，详情列必须同时提供预览和复制入口。
  test('shows preview and copy actions for a successful task result', async () => {
    const rendered = await renderDetails(taskLog)

    const buttons =
      rendered.container.querySelectorAll<HTMLButtonElement>('button')
    assert.equal(buttons.length, 2)
    assert.equal(buttons[0].textContent?.includes('Preview video'), true)
    assert.equal(buttons[1].textContent?.includes('Copy'), true)
    assert.equal(buttons[1].getAttribute('aria-label'), 'Copy Link')

    await act(async () => rendered.root.unmount())
    rendered.container.remove()
  })

  // 复制入口必须复制日志中的实际视频地址，而不是弹窗播放用的 Blob 地址。
  test('copies the stored video URL', async () => {
    const originalClipboard = Object.getOwnPropertyDescriptor(
      navigator,
      'clipboard'
    )
    let copiedText = ''
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: async (text: string) => {
          copiedText = text
        },
      },
    })
    const rendered = await renderDetails(taskLog)
    try {
      const buttons =
        rendered.container.querySelectorAll<HTMLButtonElement>('button')
      await act(async () => {
        buttons[1]?.click()
        await Promise.resolve()
      })
      assert.equal(copiedText, taskLog.result_url)
    } finally {
      await act(async () => rendered.root.unmount())
      rendered.container.remove()
      if (originalClipboard) {
        Object.defineProperty(navigator, 'clipboard', originalClipboard)
      } else {
        Reflect.deleteProperty(navigator, 'clipboard')
      }
    }
  })

  // 未迁移的历史任务仍通过 fail_reason 中的地址保持视频入口可用。
  test('keeps the video link for a legacy task result', async () => {
    const rendered = await renderDetails({
      ...taskLog,
      result_url: undefined,
      fail_reason: 'https://example.com/legacy-video.mp4',
    })

    const link = rendered.container.querySelector<HTMLButtonElement>('button')
    assert.ok(link)
    assert.equal(link.getAttribute('href'), null)

    await act(async () => rendered.root.unmount())
    rendered.container.remove()
  })
})
