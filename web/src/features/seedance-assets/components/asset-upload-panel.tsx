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
import { useMutation } from '@tanstack/react-query'
import {
  AlertCircle,
  CheckCircle2,
  File,
  Loader2,
  RotateCcw,
  X,
} from 'lucide-react'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

import { uploadSeedanceAsset, type SeedanceAsset } from '../api'
import { seedanceAssetLayoutClasses } from '../layout'
import {
  SEEDANCE_ASSET_ACCEPT,
  formatSeedanceFileSize,
  validateSeedanceAssetFile,
} from '../lib/upload'
import type { SeedanceUploadItem } from '../types'

// 包含完成条目，限制文件引用和 DOM 的总占用；用户可清除后继续上传。
const MAX_UPLOAD_QUEUE_ITEMS = 100

// 上传队列逐个执行并有界保留；切换分组或离开页面时取消请求并丢弃未开始条目。
export function AssetUploadPanel(props: {
  groupId: string
  onUploaded: (asset: SeedanceAsset) => void
  children: (browse: () => void) => ReactNode
}) {
  const { t } = useTranslation()
  const inputRef = useRef<HTMLInputElement>(null)
  const controllerRef = useRef<AbortController | null>(null)
  const queueSizeRef = useRef(0)
  const pendingRef = useRef<SeedanceUploadItem[]>([])
  // 用令牌记录等待处理的条目，防止重复点击重试造成重复素材。
  const pendingIdsRef = useRef(new Set<string>())
  const workerRunningRef = useRef(false)
  const mountedRef = useRef(true)
  const [dragActive, setDragActive] = useState(false)
  const [queue, setQueue] = useState<SeedanceUploadItem[]>([])

  const uploadMutation = useMutation({ mutationFn: uploadSeedanceAsset })

  useEffect(() => {
    mountedRef.current = true
    const pendingIds = pendingIdsRef.current
    return () => {
      mountedRef.current = false
      pendingRef.current = []
      pendingIds.clear()
      controllerRef.current?.abort()
    }
  }, [])

  const updateQueueItem = (id: string, update: Partial<SeedanceUploadItem>) => {
    if (!mountedRef.current) return
    setQueue((current) =>
      current.map((item) => (item.id === id ? { ...item, ...update } : item))
    )
  }

  const processQueue = async () => {
    if (workerRunningRef.current) return
    workerRunningRef.current = true
    try {
      while (mountedRef.current && pendingRef.current.length > 0) {
        const item = pendingRef.current.shift()
        if (!item) continue
        pendingIdsRef.current.delete(item.id)
        updateQueueItem(item.id, { status: 'uploading', error: undefined })
        try {
          const controller = new AbortController()
          controllerRef.current = controller
          const response = await uploadMutation.mutateAsync({
            groupId: item.groupId,
            file: item.file,
            name: item.name,
            signal: controller.signal,
          })
          updateQueueItem(item.id, { status: 'success' })
          if (mountedRef.current) props.onUploaded(response.data)
        } catch (error: unknown) {
          updateQueueItem(item.id, {
            status: 'error',
            error: error instanceof Error ? error.message : t('Upload failed'),
          })
        }
      }
    } finally {
      controllerRef.current = null
      workerRunningRef.current = false
    }
  }

  const enqueueFiles = (files: File[]) => {
    if (!props.groupId) {
      toast.error(t('Select an asset group first'))
      return
    }
    const validItems: SeedanceUploadItem[] = []
    for (const file of files) {
      if (queueSizeRef.current + validItems.length >= MAX_UPLOAD_QUEUE_ITEMS) {
        toast.error(
          t('Upload queue is full ({{limit}} files)', {
            limit: MAX_UPLOAD_QUEUE_ITEMS,
          })
        )
        break
      }
      const validation = validateSeedanceAssetFile(file)
      if (!validation.valid) {
        toast.error(
          `${file.name}: ${t(validation.errorKey, { limit: validation.limit })}`
        )
        continue
      }
      validItems.push({
        id: crypto.randomUUID(),
        groupId: props.groupId,
        file,
        name: [...file.name].slice(0, 64).join(''),
        status: 'queued',
      })
    }
    if (validItems.length === 0) return
    queueSizeRef.current += validItems.length
    setQueue((current) => [...current, ...validItems])
    pendingRef.current.push(...validItems)
    for (const item of validItems) pendingIdsRef.current.add(item.id)
    void processQueue()
  }

  const retryItem = (item: SeedanceUploadItem) => {
    if (item.status !== 'error' || pendingIdsRef.current.has(item.id)) return
    pendingIdsRef.current.add(item.id)
    pendingRef.current.push(item)
    updateQueueItem(item.id, { status: 'queued', error: undefined })
    void processQueue()
  }

  const clearFinished = () => {
    queueSizeRef.current = queue.filter(
      (item) => item.status === 'queued' || item.status === 'uploading'
    ).length
    setQueue((current) =>
      current.filter(
        (item) => item.status === 'queued' || item.status === 'uploading'
      )
    )
  }

  return (
    <section
      aria-label={t('Upload assets')}
      className={cn(
        seedanceAssetLayoutClasses.uploadPanel,
        dragActive && 'bg-primary/5 ring-primary ring-2 ring-inset'
      )}
      onDragEnter={(event) => {
        if (!event.dataTransfer.types.includes('Files')) return
        event.preventDefault()
        if (props.groupId) setDragActive(true)
      }}
      onDragOver={(event) => {
        if (!event.dataTransfer.types.includes('Files')) return
        event.preventDefault()
        event.dataTransfer.dropEffect = props.groupId ? 'copy' : 'none'
        if (props.groupId) setDragActive(true)
      }}
      onDragLeave={(event) => {
        // 穿过卡片等子元素不清除高亮，仅在离开整个素材区时复位。
        if (!event.currentTarget.contains(event.relatedTarget as Node | null)) {
          setDragActive(false)
        }
      }}
      onDrop={(event) => {
        event.preventDefault()
        setDragActive(false)
        enqueueFiles([...event.dataTransfer.files])
      }}
    >
      {props.children(() => inputRef.current?.click())}
      <input
        ref={inputRef}
        aria-label={t('Upload assets')}
        type='file'
        className='hidden'
        disabled={!props.groupId}
        accept={SEEDANCE_ASSET_ACCEPT}
        multiple
        onChange={(event) => {
          enqueueFiles([...(event.target.files ?? [])])
          event.target.value = ''
        }}
      />

      {queue.length > 0 ? (
        <div className='border-border/70 mx-4 mb-4 shrink-0 rounded-lg border sm:mx-6'>
          <div className='flex items-center justify-between gap-3 border-b px-3 py-2'>
            <div className='text-sm font-medium'>
              {t('Upload queue')}{' '}
              <span className='text-muted-foreground'>({queue.length})</span>
            </div>
            <Button
              type='button'
              size='sm'
              variant='ghost'
              onClick={clearFinished}
              disabled={
                !queue.some(
                  (item) => item.status === 'success' || item.status === 'error'
                )
              }
            >
              <X />
              {t('Clear finished')}
            </Button>
          </div>
          <ul className='max-h-40 divide-y overflow-y-auto overscroll-contain'>
            {queue.map((item) => (
              <li
                key={item.id}
                className='flex min-w-0 items-center gap-3 px-3 py-2.5'
              >
                <span className='bg-muted text-muted-foreground flex size-8 shrink-0 items-center justify-center rounded-md'>
                  <File className='size-4' aria-hidden='true' />
                </span>
                <div className='min-w-0 flex-1'>
                  <p className='truncate text-sm' title={item.name}>
                    {item.name}
                  </p>
                  <p className='text-muted-foreground text-xs'>
                    {formatSeedanceFileSize(item.file.size)}
                  </p>
                  {item.error ? (
                    <p
                      className='text-destructive truncate text-xs'
                      title={item.error}
                    >
                      {item.error}
                    </p>
                  ) : null}
                </div>
                {item.status === 'queued' ? (
                  <span className='text-muted-foreground shrink-0 text-xs'>
                    {t('Queued')}
                  </span>
                ) : null}
                {item.status === 'uploading' ? (
                  <Loader2
                    className='text-muted-foreground size-4 shrink-0 animate-spin'
                    aria-label={t('Uploading')}
                  />
                ) : null}
                {item.status === 'success' ? (
                  <CheckCircle2
                    className='text-success size-4 shrink-0'
                    aria-label={t('Uploaded')}
                  />
                ) : null}
                {item.status === 'error' ? (
                  <Button
                    type='button'
                    size='icon-sm'
                    variant='ghost'
                    title={t('Retry')}
                    aria-label={t('Retry')}
                    onClick={() => retryItem(item)}
                  >
                    <RotateCcw />
                  </Button>
                ) : null}
                {item.status === 'error' ? (
                  <AlertCircle
                    className='text-destructive size-4 shrink-0'
                    aria-hidden='true'
                  />
                ) : null}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </section>
  )
}
