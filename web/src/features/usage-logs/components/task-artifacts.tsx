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
import {
  Alert02Icon,
  Download01Icon,
  File01Icon,
  Image01Icon,
  MusicNote01Icon,
  RefreshIcon,
  Video01Icon,
} from '@hugeicons/core-free-icons'
import { HugeiconsIcon } from '@hugeicons/react'
import { useQuery } from '@tanstack/react-query'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import {
  Alert,
  AlertAction,
  AlertDescription,
  AlertTitle,
} from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardFooter,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { cn } from '@/lib/utils'

import { getTaskArtifacts } from '../api'
import {
  resolveTaskPreviewMode,
  shouldLoadTaskArtifacts,
} from '../lib/task-artifacts'
import type { TaskArtifact, TaskArtifactType, TaskLog } from '../types'
import {
  AudioPreviewDialog,
  type AudioClip,
} from './dialogs/audio-preview-dialog'

function artifactIcon(type: TaskArtifactType) {
  switch (type) {
    case 'image':
      return Image01Icon
    case 'video':
      return Video01Icon
    case 'audio':
      return MusicNote01Icon
    case 'file':
      return File01Icon
  }
}

function artifactTypeLabel(type: TaskArtifactType): string {
  switch (type) {
    case 'image':
      return 'Image'
    case 'video':
      return 'Video'
    case 'audio':
      return 'Audio'
    case 'file':
      return 'File'
  }
}

function parseLegacyAudioClips(data: unknown): AudioClip[] {
  let values: unknown[] = []
  if (Array.isArray(data)) {
    values = data
  } else if (typeof data === 'string') {
    try {
      const parsed = JSON.parse(data)
      values = Array.isArray(parsed) ? parsed : []
    } catch {
      return []
    }
  }

  return values.filter(
    (value): value is AudioClip =>
      value != null &&
      typeof value === 'object' &&
      typeof (value as Record<string, unknown>).audio_url === 'string'
  )
}

function LegacyAudioPreview(props: { data: unknown }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const clips = useMemo(() => parseLegacyAudioClips(props.data), [props.data])

  if (clips.length === 0) return null

  return (
    <>
      <button
        type='button'
        className='group flex items-center gap-1 text-left text-xs'
        onClick={() => setOpen(true)}
      >
        <HugeiconsIcon
          icon={MusicNote01Icon}
          className='text-muted-foreground size-3'
          strokeWidth={2}
          aria-hidden='true'
        />
        <span className='text-foreground leading-snug group-hover:underline'>
          {t('Click to preview audio')}
        </span>
      </button>
      <AudioPreviewDialog open={open} onOpenChange={setOpen} clips={clips} />
    </>
  )
}

function ArtifactMedia(props: {
  artifact: TaskArtifact
  mediaUrl: string
  onError: () => void
}) {
  if (props.artifact.type === 'image') {
    return (
      <img
        src={props.mediaUrl}
        alt={props.artifact.key}
        loading='lazy'
        className='max-h-[60vh] w-full rounded-md object-contain'
        onError={props.onError}
      />
    )
  }
  if (props.artifact.type === 'video') {
    return (
      <video
        src={props.mediaUrl}
        controls
        preload='metadata'
        className='max-h-[60vh] w-full rounded-md bg-black'
        onError={props.onError}
      />
    )
  }
  if (props.artifact.type === 'audio') {
    return (
      <audio
        src={props.mediaUrl}
        controls
        preload='none'
        className='w-full'
        onError={props.onError}
      />
    )
  }
  return null
}

function MediaFailure(props: { onRetry: () => void }) {
  const { t } = useTranslation()
  return (
    <Alert variant='destructive'>
      <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} aria-hidden='true' />
      <AlertTitle>{t('Media preview failed. Please try again.')}</AlertTitle>
      <AlertDescription>{t('Preview unavailable')}</AlertDescription>
      <AlertAction>
        <Button
          type='button'
          variant='outline'
          size='xs'
          onClick={props.onRetry}
        >
          <HugeiconsIcon
            icon={RefreshIcon}
            strokeWidth={2}
            data-icon='inline-start'
          />
          {t('Retry')}
        </Button>
      </AlertAction>
    </Alert>
  )
}

function TaskArtifactCard(props: { artifact: TaskArtifact }) {
  const { t } = useTranslation()
  const [mediaFailed, setMediaFailed] = useState(false)
  const [mediaRevision, setMediaRevision] = useState(0)
  const icon = artifactIcon(props.artifact.type)
  const isVisualArtifact =
    props.artifact.type === 'image' || props.artifact.type === 'video'

  let cardContent = (
    <div
      className={cn(
        'bg-muted/40 text-muted-foreground flex items-center justify-center rounded-md',
        isVisualArtifact ? 'aspect-video min-h-48' : 'min-h-20'
      )}
    >
      <HugeiconsIcon icon={icon} className='size-6' strokeWidth={1.5} />
    </div>
  )
  if (mediaFailed) {
    cardContent = (
      <MediaFailure
        onRetry={() => {
          setMediaFailed(false)
          setMediaRevision((revision) => revision + 1)
        }}
      />
    )
  } else if (props.artifact.type !== 'file') {
    cardContent = (
      <ArtifactMedia
        key={mediaRevision}
        artifact={props.artifact}
        mediaUrl={props.artifact.content_url}
        onError={() => setMediaFailed(true)}
      />
    )
  }

  return (
    <Card size='sm'>
      <CardHeader>
        <CardTitle className='flex min-w-0 items-center gap-1.5'>
          <HugeiconsIcon
            icon={icon}
            className='size-4 shrink-0'
            strokeWidth={2}
            aria-hidden='true'
          />
          <span className='truncate'>
            {t(artifactTypeLabel(props.artifact.type))}
          </span>
        </CardTitle>
        <CardDescription className='min-w-0'>
          <span className='block truncate font-mono text-xs'>
            {props.artifact.key}
          </span>
          {props.artifact.mime_type ? (
            <span className='block truncate font-mono text-[11px]'>
              {props.artifact.mime_type}
            </span>
          ) : null}
        </CardDescription>
      </CardHeader>
      <CardContent>{cardContent}</CardContent>
      <CardFooter className='flex flex-wrap gap-2'>
        {/* 复制服务端投影地址，保留插件鉴权与统一 artifacts 路径。 */}
        <CopyButton
          value={props.artifact.content_url}
          variant='outline'
          size='sm'
          tooltip={t('Copy link')}
        >
          {t('Copy link')}
        </CopyButton>
        <Button
          variant='outline'
          size='sm'
          nativeButton={false}
          render={
            <a
              href={props.artifact.content_url}
              download={props.artifact.key}
              target='_blank'
              rel='noopener noreferrer'
            />
          }
        >
          <HugeiconsIcon
            icon={Download01Icon}
            strokeWidth={2}
            data-icon='inline-start'
          />
          {t('Download')}
        </Button>
      </CardFooter>
    </Card>
  )
}

interface TaskArtifactsProps {
  taskId: string
  enabled: boolean
  emptyContent?: (legacyContentUrl?: string) => React.ReactNode
}

function TaskArtifacts(props: TaskArtifactsProps) {
  const { t } = useTranslation()
  const artifactsQuery = useQuery({
    queryKey: ['usage-logs', 'task-artifacts', props.taskId],
    queryFn: ({ signal }) => getTaskArtifacts(props.taskId, signal),
    enabled: props.enabled,
    retry: false,
    staleTime: 30_000,
  })

  if (!props.enabled) return null

  if (artifactsQuery.isPending) {
    return (
      <div aria-label={t('Loading...')}>
        <Skeleton className='aspect-video min-h-48 w-full rounded-xl' />
      </div>
    )
  }

  if (artifactsQuery.isError) {
    return (
      <Alert variant='destructive'>
        <HugeiconsIcon icon={Alert02Icon} strokeWidth={2} aria-hidden='true' />
        <AlertTitle>{t('Failed to load artifacts')}</AlertTitle>
        <AlertDescription>{t('Preview unavailable')}</AlertDescription>
        <AlertAction>
          <Button
            type='button'
            variant='outline'
            size='xs'
            disabled={artifactsQuery.isFetching}
            onClick={() => void artifactsQuery.refetch()}
          >
            {artifactsQuery.isFetching ? (
              <Spinner data-icon='inline-start' />
            ) : (
              <HugeiconsIcon
                icon={RefreshIcon}
                strokeWidth={2}
                data-icon='inline-start'
              />
            )}
            {t('Retry')}
          </Button>
        </AlertAction>
      </Alert>
    )
  }

  if (artifactsQuery.data.artifacts.length === 0) {
    return (
      props.emptyContent?.(artifactsQuery.data.legacyContentUrl) ?? (
        <EmptyTaskArtifacts />
      )
    )
  }

  return (
    <div
      className={cn(
        'grid gap-3',
        artifactsQuery.data.artifacts.length > 1 && 'lg:grid-cols-2'
      )}
    >
      {artifactsQuery.data.artifacts.map((artifact) => (
        <TaskArtifactCard key={artifact.key} artifact={artifact} />
      ))}
    </div>
  )
}

function EmptyTaskArtifacts() {
  const { t } = useTranslation()
  return (
    <Empty>
      <EmptyHeader>
        <EmptyMedia variant='icon'>
          <HugeiconsIcon icon={File01Icon} strokeWidth={2} aria-hidden='true' />
        </EmptyMedia>
        <EmptyTitle>{t('Artifacts')}</EmptyTitle>
        <EmptyDescription>{t('None')}</EmptyDescription>
      </EmptyHeader>
    </Empty>
  )
}

function LegacyTaskArtifacts(props: { legacyContentUrl?: string }) {
  if (props.legacyContentUrl) {
    return <LegacyVideoMedia contentUrl={props.legacyContentUrl} />
  }
  return <EmptyTaskArtifacts />
}

export function TaskArtifactsCell(props: { log: TaskLog }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const previewMode = resolveTaskPreviewMode(props.log)
  const { copyToClipboard } = useCopyToClipboard()
  const mountedRef = useRef(true)
  const copyingRef = useRef(false)
  const copyQuery = useQuery({
    queryKey: ['usage-logs', 'task-artifacts', props.log.task_id],
    queryFn: ({ signal }) => getTaskArtifacts(props.log.task_id, signal),
    enabled: false,
    retry: false,
    staleTime: 30_000,
  })

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  // 列表只在点击复制时获取授权链接；多视频不任意选取，交由现有 artifacts 弹窗选择。
  const copyVideoLink = async () => {
    if (copyingRef.current) return
    copyingRef.current = true
    try {
      const result = await copyQuery.refetch({ throwOnError: true })
      if (!mountedRef.current || !result.data) return
      const videos = result.data.artifacts.filter(
        (artifact) => artifact.type === 'video'
      )
      let url: string | undefined
      if (videos.length === 1) url = videos[0].content_url
      else if (result.data.artifacts.length === 0) {
        url = result.data.legacyContentUrl
      }
      if (url) await copyToClipboard(url)
      else setOpen(true)
    } catch {
      if (mountedRef.current) toast.error(t('Failed to load artifacts'))
    } finally {
      copyingRef.current = false
    }
  }

  if (!shouldLoadTaskArtifacts(props.log, true)) {
    return <span className='text-muted-foreground/60 text-xs'>-</span>
  }
  if (previewMode === 'legacy-suno') {
    return <LegacyAudioPreview data={props.log.data} />
  }

  return (
    <>
      <div className='flex flex-wrap items-center gap-2'>
        {previewMode === 'legacy-video' ? (
          <Button
            type='button'
            variant='ghost'
            size='xs'
            className='text-foreground text-xs hover:underline'
            onClick={() => setOpen(true)}
          >
            {t('Preview video')}
          </Button>
        ) : (
          <Button
            type='button'
            variant='outline'
            size='xs'
            onClick={() => setOpen(true)}
          >
            <HugeiconsIcon
              icon={File01Icon}
              strokeWidth={2}
              data-icon='inline-start'
            />
            {t('Artifacts')}
          </Button>
        )}
        <Button
          type='button'
          variant='ghost'
          size='xs'
          disabled={copyQuery.isFetching}
          onClick={copyVideoLink}
        >
          {copyQuery.isFetching ? <Spinner /> : null}
          {t('Copy link')}
        </Button>
      </div>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={
          previewMode === 'legacy-video' ? (
            t('Preview')
          ) : (
            <span className='flex items-center gap-2'>
              <HugeiconsIcon
                icon={File01Icon}
                className='text-muted-foreground size-4'
                strokeWidth={2}
                aria-hidden='true'
              />
              {t('Artifacts')}
            </span>
          )
        }
        contentClassName={
          previewMode === 'legacy-video' ? 'sm:max-w-xl' : 'sm:max-w-4xl'
        }
        contentHeight='auto'
        bodyClassName='pr-2 sm:pr-4'
      >
        <TaskArtifacts
          taskId={props.log.task_id}
          enabled={shouldLoadTaskArtifacts(props.log, open)}
          emptyContent={(legacyContentUrl) => (
            <LegacyTaskArtifacts legacyContentUrl={legacyContentUrl} />
          )}
        />
      </Dialog>
    </>
  )
}

interface LegacyVideoMediaProps {
  contentUrl: string
}

function LegacyVideoMedia(props: LegacyVideoMediaProps) {
  const { t } = useTranslation()
  const [mediaFailed, setMediaFailed] = useState(false)
  const [mediaRevision, setMediaRevision] = useState(0)

  // 旧视频也只复制服务端提供的投影链接，不从任务 ID 拼接旧接口。
  return (
    <div className='space-y-3'>
      <CopyButton
        value={props.contentUrl}
        variant='outline'
        size='sm'
        tooltip={t('Copy link')}
      >
        {t('Copy link')}
      </CopyButton>
      {mediaFailed ? (
        <MediaFailure
          onRetry={() => {
            setMediaFailed(false)
            setMediaRevision((revision) => revision + 1)
          }}
        />
      ) : (
        <video
          key={mediaRevision}
          src={props.contentUrl}
          controls
          preload='metadata'
          className='max-h-[60vh] w-full rounded-md bg-black'
          onError={() => setMediaFailed(true)}
        />
      )}
    </div>
  )
}
