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
import { Loader2 } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'

import type { SeedanceAssetGroup } from '../api'

// 分组修改使用独立弹窗，关闭时清空草稿，避免下次打开带入未提交内容。
export function EditAssetGroupDialog(props: {
  group: SeedanceAssetGroup | null
  open: boolean
  isPending: boolean
  onOpenChange: (open: boolean) => void
  onSubmit: (details: {
    name: string
    description: string
    tags: string
  }) => void
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<{
    groupId: number | null
    name: string
    description: string
    tags: string
  }>({ groupId: null, name: '', description: '', tags: '' })
  const groupId = props.group?.id ?? null
  const currentDraft =
    draft.groupId === groupId
      ? draft
      : {
          groupId,
          name: props.group?.name ?? '',
          description: props.group?.description ?? '',
          tags: props.group?.tags ?? '',
        }
  const name = currentDraft.name
  const trimmedName = name.trim()
  const submit = () =>
    props.onSubmit({
      name: trimmedName,
      description: currentDraft.description.trim(),
      tags: currentDraft.tags.trim(),
    })

  const handleOpenChange = (open: boolean) => {
    if (!open) setDraft({ groupId: null, name: '', description: '', tags: '' })
    props.onOpenChange(open)
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={handleOpenChange}
      title={t('Rename asset group')}
      description={t('Update the name used to organize your Seedance media.')}
      contentClassName='sm:max-w-md'
      bodyClassName='space-y-4'
      footer={
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
            onClick={submit}
            disabled={!trimmedName || props.isPending}
          >
            {props.isPending ? <Loader2 className='animate-spin' /> : null}
            {t('Save')}
          </Button>
        </>
      }
    >
      <label className='flex flex-col gap-2 text-sm font-medium'>
        {t('Group name')}
        <Input
          autoFocus
          maxLength={64}
          value={name}
          onChange={(event) =>
            setDraft({ ...currentDraft, name: event.target.value })
          }
          onKeyDown={(event) => {
            if (event.key === 'Enter' && trimmedName && !props.isPending) {
              event.preventDefault()
              submit()
            }
          }}
        />
      </label>
      <label className='flex flex-col gap-2 text-sm font-medium'>
        {t('Description')}
        <Textarea
          maxLength={1024}
          value={currentDraft.description}
          onChange={(event) =>
            setDraft({ ...currentDraft, description: event.target.value })
          }
        />
      </label>
      <label className='flex flex-col gap-2 text-sm font-medium'>
        {t('Tags')}
        <Input
          maxLength={512}
          placeholder={t('Use commas to separate tags')}
          value={currentDraft.tags}
          onChange={(event) =>
            setDraft({ ...currentDraft, tags: event.target.value })
          }
        />
      </label>
    </Dialog>
  )
}

// 删除分组会连同上游和本地素材映射一起删除，因此必须二次确认。
export function DeleteAssetGroupDialog(props: {
  group: SeedanceAssetGroup | null
  open: boolean
  isPending: boolean
  onOpenChange: (open: boolean) => void
  onConfirm: () => void
}) {
  const { t } = useTranslation()
  const groupName = props.group?.name ?? ''
  return (
    <ConfirmDialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Delete asset group')}
      desc={t(
        'Delete "{{name}}" and all of its stored assets? This action cannot be undone.',
        { name: groupName }
      )}
      destructive
      isLoading={props.isPending}
      confirmText={t('Delete')}
      handleConfirm={props.onConfirm}
    />
  )
}
