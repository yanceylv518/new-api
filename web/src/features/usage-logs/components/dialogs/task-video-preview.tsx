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
import { Download, Loader2, Play } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { api } from '@/lib/api'

// 媒体元素不能附加后台 JWT，先通过统一鉴权客户端获取 Blob，再在弹窗中播放。
export function TaskVideoPreview(props: { taskId: string; videoUrl: string }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [source, setSource] = useState('')
  const [loading, setLoading] = useState(false)
  const [failed, setFailed] = useState(false)
  const request = useRef<AbortController | null>(null)

  // 关闭或卸载时取消下载，并回收大视频占用的 Blob 内存。
  useEffect(() => () => request.current?.abort(), [])
  useEffect(
    () => () => {
      if (source) URL.revokeObjectURL(source)
    },
    [source]
  )

  const load = async () => {
    request.current?.abort()
    const controller = new AbortController()
    request.current = controller
    setOpen(true)
    setLoading(true)
    setFailed(false)
    setSource('')
    try {
      const response = await api.get<Blob>(
        `/v1/videos/${encodeURIComponent(props.taskId)}/content`,
        {
          responseType: 'blob',
          signal: controller.signal,
          disableDuplicate: true,
          timeout: 65000,
        }
      )
      if (controller.signal.aborted) return
      if (response.data.type.includes('json') || !response.data.size) {
        throw new Error('Invalid video response')
      }
      setSource(URL.createObjectURL(response.data))
    } catch {
      if (!controller.signal.aborted) setFailed(true)
    } finally {
      if (!controller.signal.aborted) setLoading(false)
    }
  }

  return (
    <>
      <div className='inline-flex items-center gap-2'>
        <button
          type='button'
          className='text-foreground inline-flex items-center gap-1 text-xs hover:underline'
          onClick={() => void load()}
        >
          <Play className='size-3' aria-hidden='true' />
          {t('Preview video')}
        </button>
        <CopyButton
          value={props.videoUrl}
          size='sm'
          className='h-7 px-2 text-xs'
          iconClassName='size-3.5'
          tooltip={t('Copy Link')}
          aria-label={t('Copy Link')}
        >
          {t('Copy')}
        </CopyButton>
      </div>
      <Dialog
        open={open}
        title={t('Preview')}
        contentClassName='sm:max-w-3xl'
        onOpenChange={(value) => {
          setOpen(value)
          if (!value) {
            request.current?.abort()
            setSource('')
            setLoading(false)
          }
        }}
      >
        {loading ? (
          <div
            role='status'
            className='flex min-h-48 items-center justify-center gap-2'
          >
            <Loader2 className='size-5 animate-spin' />
            {t('Loading')}
          </div>
        ) : null}
        {failed ? (
          <div
            role='alert'
            className='flex min-h-48 items-center justify-center gap-3'
          >
            {t('Request failed')}
            <Button onClick={() => void load()}>{t('Retry')}</Button>
          </div>
        ) : null}
        {source ? (
          <>
            <video
              src={source}
              controls
              playsInline
              className='max-h-[65vh] w-full'
            />
            <a
              href={source}
              download={`${props.taskId}.mp4`}
              className='mt-3 inline-flex items-center gap-2 text-sm'
            >
              <Download className='size-4' />
              {t('Download')}
            </a>
          </>
        ) : null}
      </Dialog>
    </>
  )
}
