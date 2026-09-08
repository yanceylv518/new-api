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
  Folder,
  FolderOpen,
  Loader2,
  MoreHorizontal,
  Pencil,
  Plus,
  RefreshCw,
  Trash2,
} from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Input } from '@/components/ui/input'

import type { SeedanceAssetGroup } from '../api'
import { seedanceAssetLayoutClasses } from '../layout'

// 分组侧栏负责选择当前工作区，并把重命名、删除放在每个分组的局部菜单中。
export function AssetGroupSidebar(props: {
  groups: SeedanceAssetGroup[]
  selectedId: string
  isLoading: boolean
  isError: boolean
  isCreating: boolean
  onRetry: () => void
  onSelect: (group: SeedanceAssetGroup) => void
  onCreate: (name: string) => Promise<boolean>
  onEdit: (group: SeedanceAssetGroup) => void
  onDelete: (group: SeedanceAssetGroup) => void
}) {
  const { t } = useTranslation()
  const [newGroupName, setNewGroupName] = useState('')

  const submitNewGroup = async () => {
    const name = newGroupName.trim()
    if (!name || props.isCreating) return
    if (await props.onCreate(name)) setNewGroupName('')
  }

  return (
    <aside
      className={seedanceAssetLayoutClasses.groupSidebar}
      aria-label={t('Asset groups')}
    >
      <div className='flex items-center justify-between gap-3'>
        <div className='min-w-0'>
          <h2 className='text-sm font-semibold'>{t('Asset groups')}</h2>
          <p className='text-muted-foreground mt-1 hidden text-xs md:block'>
            {t('Organize reusable media for video generation')}
          </p>
        </div>
        <span className='text-muted-foreground shrink-0 text-xs tabular-nums'>
          {props.groups.length}
        </span>
      </div>

      <div className='flex items-center gap-2'>
        <Input
          aria-label={t('New group name')}
          placeholder={t('New group name')}
          maxLength={64}
          value={newGroupName}
          onChange={(event) => setNewGroupName(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              event.preventDefault()
              void submitNewGroup()
            }
          }}
          className='min-w-0'
        />
        <Button
          type='button'
          size='icon'
          variant='outline'
          title={t('Create group')}
          aria-label={t('Create group')}
          disabled={!newGroupName.trim() || props.isCreating}
          onClick={submitNewGroup}
        >
          {props.isCreating ? <Loader2 className='animate-spin' /> : <Plus />}
        </Button>
      </div>

      <div className={seedanceAssetLayoutClasses.groupSidebarBody}>
        {props.isLoading ? (
          <div
            role='status'
            className='text-muted-foreground flex flex-1 flex-col items-center justify-center gap-2 text-center text-sm'
          >
            <Loader2 className='size-5 animate-spin' />
            {t('Loading')}
          </div>
        ) : null}
        {props.isError ? (
          <div className='flex flex-1 items-center justify-center'>
            <Button type='button' variant='outline' onClick={props.onRetry}>
              <RefreshCw />
              {t('Retry')}
            </Button>
          </div>
        ) : null}
        {!props.isLoading && !props.isError && props.groups.length === 0 ? (
          <div className='text-muted-foreground flex min-h-0 w-full flex-1 flex-col items-center justify-center gap-3 px-4 text-center'>
            <span className='bg-muted flex size-11 items-center justify-center rounded-full'>
              <FolderOpen className='size-5' aria-hidden='true' />
            </span>
            <p className='max-w-[18rem] text-sm leading-6'>
              {t('No asset groups yet')}
            </p>
          </div>
        ) : null}
        {!props.isLoading && !props.isError && props.groups.length > 0 ? (
          <nav className={seedanceAssetLayoutClasses.groupSidebarScroll}>
            {props.groups.map((group) => {
              const selected = group.group_id === props.selectedId
              return (
                <div
                  key={group.group_id}
                  className='group flex min-w-0 items-center gap-1'
                >
                  <Button
                    type='button'
                    variant={selected ? 'secondary' : 'ghost'}
                    className='min-w-0 flex-1 justify-start'
                    aria-current={selected ? 'page' : undefined}
                    title={group.name}
                    onClick={() => props.onSelect(group)}
                  >
                    {selected ? (
                      <FolderOpen className='text-primary shrink-0' />
                    ) : (
                      <Folder className='shrink-0' />
                    )}
                    <span className='truncate'>{group.name}</span>
                    {group.status === 'Deleting' ? (
                      <span className='text-muted-foreground text-xs'>
                        {t('Deleting...')}
                      </span>
                    ) : null}
                  </Button>
                  <DropdownMenu>
                    <DropdownMenuTrigger
                      render={
                        <Button
                          type='button'
                          size='icon-xs'
                          variant='ghost'
                          className='shrink-0'
                          title={t('Group actions')}
                          aria-label={t('Group actions')}
                        />
                      }
                    >
                      <MoreHorizontal aria-hidden='true' />
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align='end'>
                      <DropdownMenuItem
                        disabled={group.status === 'Deleting'}
                        onClick={() => props.onEdit(group)}
                      >
                        <Pencil />
                        {t('Rename')}
                      </DropdownMenuItem>
                      <DropdownMenuItem
                        variant='destructive'
                        onClick={() => props.onDelete(group)}
                      >
                        <Trash2 />
                        {t('Delete')}
                      </DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </div>
              )
            })}
          </nav>
        ) : null}
      </div>
    </aside>
  )
}
