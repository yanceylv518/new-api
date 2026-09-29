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
  ExternalLink,
  ImagePlus,
  Loader2,
  RefreshCw,
  ShieldCheck,
  Upload,
} from 'lucide-react'
import { QRCodeSVG } from 'qrcode.react'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Progress } from '@/components/ui/progress'
import { Textarea } from '@/components/ui/textarea'

import {
  uploadSeedanceAsset,
  type SeedanceAsset,
  type SeedanceAssetValidationSession,
} from '../api'
import {
  SEEDANCE_PORTRAIT_ACCEPT,
  formatSeedanceFileSize,
  validateSeedancePortraitFile,
} from '../lib/upload'

const validationStatusCopy: Record<string, string> = {
  Creating: 'Starting verification...',
  Pending: 'Verification pending',
  Succeeded: 'Verification completed',
  Failed: 'Verification failed',
  Expired: 'Verification expired',
}

export type HumanAssetGroupDetails = {
  name: string
  description: string
  tags: string
}

export function CreateHumanValidationDialog(props: {
  open: boolean
  isPending: boolean
  isChecking: boolean
  session: SeedanceAssetValidationSession | null
  onOpenChange: (open: boolean) => void
  onSubmit: (details: HumanAssetGroupDetails) => void
  onCheckStatus: () => void
  onStartOver: () => void
  onPortraitUploaded: (asset: SeedanceAsset) => void
}) {
  const { t } = useTranslation()
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [tags, setTags] = useState('')
  const [portrait, setPortrait] = useState<File | null>(null)
  const [fileError, setFileError] = useState('')
  const [uploadProgress, setUploadProgress] = useState(0)
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000))
  const session = props.session
  const trimmedName = name.trim()
  const isSessionFailed =
    session?.status === 'Failed' || session?.status === 'Expired'
  const isVerified = session?.status === 'Succeeded'
  const status = session
    ? t(validationStatusCopy[session.status] ?? session.status)
    : ''

  const uploadMutation = useMutation({
    mutationFn: async (file: File) => {
      if (!session?.group_id) {
        throw new Error(t('The verified group is not ready yet'))
      }
      return uploadSeedanceAsset({
        groupId: session.group_id,
        file,
        name: file.name,
        onUploadProgress: setUploadProgress,
      })
    },
    onSuccess: async (response) => {
      await props.onPortraitUploaded(response.data)
      setName('')
      setDescription('')
      setTags('')
      setPortrait(null)
      setFileError('')
      setUploadProgress(0)
    },
  })

  useEffect(() => {
    if (!props.open || session?.status !== 'Pending') return
    setNow(Math.floor(Date.now() / 1000))
    const timer = window.setInterval(() => {
      setNow(Math.floor(Date.now() / 1000))
    }, 1000)
    return () => window.clearInterval(timer)
  }, [props.open, session?.status])

  const handleOpenChange = (open: boolean) => {
    if (!open && uploadMutation.isPending) return
    if (!open && !session) {
      setName('')
      setDescription('')
      setTags('')
    }
    props.onOpenChange(open)
  }

  const handleStartOver = () => {
    setName('')
    setDescription('')
    setTags('')
    setPortrait(null)
    setFileError('')
    setUploadProgress(0)
    uploadMutation.reset()
    props.onStartOver()
  }

  const handleStartVerification = () => {
    if (!trimmedName || props.isPending) return
    props.onSubmit({
      name: trimmedName,
      description: description.trim(),
      tags: tags.trim(),
    })
  }

  const handlePortraitChange = (file: File | undefined) => {
    if (!file) return
    uploadMutation.reset()
    setUploadProgress(0)
    const validation = validateSeedancePortraitFile(file)
    if (!validation.valid) {
      setPortrait(null)
      setFileError(
        validation.limit
          ? t(validation.errorKey, { limit: validation.limit })
          : t(validation.errorKey)
      )
      return
    }
    setFileError('')
    setPortrait(file)
  }

  const remainingSeconds = session ? Math.max(0, session.expires_at - now) : 0
  const sessionDuration = session
    ? Math.max(1, session.expires_at - session.created_at)
    : 1
  const verificationProgress = session
    ? Math.min(100, Math.max(0, (remainingSeconds / sessionDuration) * 100))
    : 0
  const minutes = Math.floor(remainingSeconds / 60)
  const seconds = String(remainingSeconds % 60).padStart(2, '0')
  const statusFailedOrExpired = Boolean(isSessionFailed)
  let title: string
  let dialogDescription: string
  if (!session) {
    title = t('Create a verified human asset group')
    dialogDescription = t(
      'Start a 30-minute H5 identity check. After it succeeds, upload a portrait for review.'
    )
  } else if (isVerified) {
    title = t('Step 2 of 2: Upload portrait')
    dialogDescription = t(
      'Identity verification is complete. Upload a matching portrait to finish setup.'
    )
  } else if (statusFailedOrExpired) {
    title = t('Human verification')
    dialogDescription = t(
      'This verification session can no longer be continued.'
    )
  } else {
    title = t('Step 1 of 2: Identity verification')
    dialogDescription = t(
      'Scan the QR code or open the H5 page to complete identity verification.'
    )
  }

  let footer: ReactNode
  if (!session) {
    footer = (
      <>
        <Button
          type='button'
          variant='outline'
          onClick={() => handleOpenChange(false)}
          disabled={props.isPending}
        >
          {t('Cancel')}
        </Button>
        <Button
          type='button'
          onClick={handleStartVerification}
          disabled={!trimmedName || props.isPending}
        >
          {props.isPending ? (
            <Loader2 className='animate-spin' />
          ) : (
            <ShieldCheck />
          )}
          {t('Start verification')}
        </Button>
      </>
    )
  } else if (isVerified) {
    footer = (
      <>
        <Button
          type='button'
          variant='outline'
          onClick={() => handleOpenChange(false)}
          disabled={uploadMutation.isPending}
        >
          {t('Close')}
        </Button>
        <Button
          type='button'
          onClick={() => portrait && uploadMutation.mutate(portrait)}
          disabled={!portrait || !session.group_id || uploadMutation.isPending}
        >
          {uploadMutation.isPending ? (
            <Loader2 className='animate-spin' />
          ) : (
            <Upload />
          )}
          {t('Upload and start review')}
        </Button>
      </>
    )
  } else if (statusFailedOrExpired) {
    footer = (
      <>
        <Button
          type='button'
          variant='outline'
          onClick={() => handleOpenChange(false)}
        >
          {t('Close')}
        </Button>
        <Button type='button' onClick={handleStartOver}>
          <RefreshCw />
          {t('Start over')}
        </Button>
      </>
    )
  } else {
    footer = (
      <Button
        type='button'
        variant='outline'
        onClick={() => handleOpenChange(false)}
      >
        {t('Close')}
      </Button>
    )
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={handleOpenChange}
      title={title}
      description={dialogDescription}
      contentClassName='sm:max-w-lg'
      bodyClassName='space-y-4'
      footer={footer}
      showCloseButton={!uploadMutation.isPending}
    >
      {!session ? (
        <div className='space-y-4'>
          <div className='border-border bg-muted/30 flex items-start gap-3 rounded-lg border p-3'>
            <ShieldCheck className='text-primary mt-0.5 size-5 shrink-0' />
            <div className='min-w-0 space-y-1'>
              <p className='font-medium'>
                {t('Identity verification protects portrait rights')}
              </p>
              <p className='text-muted-foreground text-sm'>
                {t('Only verified users can use this character group.')}
              </p>
            </div>
          </div>
          <label className='flex flex-col gap-2 text-sm font-medium'>
            {t('Group name')}
            <Input
              autoFocus
              maxLength={64}
              value={name}
              onChange={(event) => setName(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter' && trimmedName && !props.isPending) {
                  event.preventDefault()
                  handleStartVerification()
                }
              }}
            />
          </label>
          <label className='flex flex-col gap-2 text-sm font-medium'>
            {t('Description')}
            <Textarea
              maxLength={1024}
              value={description}
              onChange={(event) => setDescription(event.target.value)}
            />
          </label>
          <label className='flex flex-col gap-2 text-sm font-medium'>
            {t('Tags')}
            <Input
              maxLength={512}
              placeholder={t('Use commas to separate tags')}
              value={tags}
              onChange={(event) => setTags(event.target.value)}
            />
          </label>
        </div>
      ) : null}

      {session && !isVerified && !statusFailedOrExpired ? (
        <div className='space-y-4'>
          <div className='flex flex-col items-center gap-4 rounded-lg border p-4'>
            {session.launch_url ? (
              <div className='rounded-lg bg-white p-3'>
                <QRCodeSVG
                  value={session.launch_url}
                  size={240}
                  level='M'
                  marginSize={2}
                  role='img'
                  aria-label={t('Verification QR code')}
                />
              </div>
            ) : (
              <Loader2 className='text-muted-foreground size-8 animate-spin' />
            )}
            <div className='w-full space-y-2'>
              <div className='flex items-center justify-between gap-3 text-sm'>
                <span className='font-medium'>{status}</span>
                <span className='text-muted-foreground tabular-nums'>
                  {t('Time remaining')}: {minutes}:{seconds}
                </span>
              </div>
              <Progress value={verificationProgress} />
            </div>
            <div className='flex flex-wrap justify-center gap-2'>
              {session.launch_url ? (
                <Button
                  type='button'
                  render={
                    <a
                      href={session.launch_url}
                      target='_blank'
                      rel='noopener noreferrer'
                    />
                  }
                >
                  <ExternalLink />
                  {t('Open verification page')}
                </Button>
              ) : null}
              <Button
                type='button'
                variant='outline'
                onClick={props.onCheckStatus}
                disabled={props.isChecking}
              >
                {props.isChecking ? (
                  <Loader2 className='animate-spin' />
                ) : (
                  <RefreshCw />
                )}
                {t('Check status')}
              </Button>
            </div>
          </div>
          <p className='text-muted-foreground text-sm'>{session.name}</p>
          {session.last_error ? (
            <p className='text-destructive text-sm break-words'>
              {session.last_error}
            </p>
          ) : null}
        </div>
      ) : null}

      {session && isVerified ? (
        <div className='space-y-4'>
          <div className='border-border bg-muted/30 flex items-start gap-3 rounded-lg border p-3'>
            <ShieldCheck className='text-primary mt-0.5 size-5 shrink-0' />
            <div className='min-w-0 space-y-1'>
              <p className='font-medium'>
                {t('Portrait must match the verified person')}
              </p>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'After upload, the upstream service reviews the image. The group is ready when the asset status becomes Active.'
                )}
              </p>
            </div>
          </div>
          <div className='space-y-2'>
            <p className='text-sm font-medium'>{t('Portrait image')}</p>
            <input
              ref={fileInputRef}
              aria-label={t('Portrait image')}
              type='file'
              accept={SEEDANCE_PORTRAIT_ACCEPT}
              className='sr-only'
              disabled={uploadMutation.isPending}
              onChange={(event) => {
                handlePortraitChange(event.currentTarget.files?.[0])
                event.currentTarget.value = ''
              }}
            />
            <div className='flex min-w-0 items-center gap-2'>
              <Button
                type='button'
                variant='outline'
                onClick={() => fileInputRef.current?.click()}
                disabled={uploadMutation.isPending}
              >
                <ImagePlus />
                {t('Choose file')}
              </Button>
              <span className='text-muted-foreground min-w-0 truncate text-sm'>
                {portrait?.name ?? t('No file selected')}
              </span>
            </div>
            <p className='text-muted-foreground text-sm'>
              {t('Supports JPG, PNG, WebP, GIF, or HEIC, up to 30 MB.')}
            </p>
            {portrait ? (
              <p className='text-muted-foreground text-xs'>
                {formatSeedanceFileSize(portrait.size)}
              </p>
            ) : null}
            {fileError ? (
              <p role='alert' className='text-destructive text-sm'>
                {fileError}
              </p>
            ) : null}
            {uploadMutation.error ? (
              <p role='alert' className='text-destructive text-sm break-words'>
                {uploadMutation.error.message || t('Upload failed')}
              </p>
            ) : null}
            {uploadMutation.isPending ? (
              <div className='space-y-2' role='status'>
                <Progress value={uploadProgress} />
                <p className='text-muted-foreground text-xs'>
                  {t('Uploading portrait for review')} {uploadProgress}%
                </p>
              </div>
            ) : null}
            {!session.group_id ? (
              <p role='alert' className='text-destructive text-sm'>
                {t('The verified group is not ready yet')}
              </p>
            ) : null}
          </div>
        </div>
      ) : null}

      {session && statusFailedOrExpired ? (
        <div className='space-y-3'>
          <div className='border-destructive/30 bg-destructive/5 flex items-start gap-3 rounded-lg border p-3'>
            <ShieldCheck className='text-destructive mt-0.5 size-5 shrink-0' />
            <div className='min-w-0 space-y-1'>
              <p className='font-medium'>{status}</p>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Start a new verification to continue creating this character group.'
                )}
              </p>
            </div>
          </div>
          {session.last_error ? (
            <p className='text-destructive text-sm break-words'>
              {session.last_error}
            </p>
          ) : null}
        </div>
      ) : null}
    </Dialog>
  )
}
