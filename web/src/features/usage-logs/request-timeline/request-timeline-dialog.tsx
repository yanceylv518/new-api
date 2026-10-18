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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

import { isRequestId } from './api'
import { RequestTimeline } from './timeline'

/** 按请求 ID 查询；没有消费日志的请求（如鉴权失败）也能查到。 */
function RequestIdSearch(props: { onSearch: (requestId: string) => void }) {
  const { t } = useTranslation()
  const [input, setInput] = useState('')
  const [invalid, setInvalid] = useState(false)
  return (
    <form
      className='mb-4 space-y-2'
      onSubmit={(event) => {
        event.preventDefault()
        const value = input.trim()
        const valid = isRequestId(value)
        setInvalid(!valid)
        if (valid) props.onSearch(value)
      }}
    >
      <Label htmlFor='trace-request-id'>{t('Request ID')}</Label>
      <div className='flex gap-2'>
        <Input
          id='trace-request-id'
          value={input}
          maxLength={64}
          onChange={(event) => {
            setInput(event.target.value)
            setInvalid(false)
          }}
          aria-invalid={invalid}
          aria-describedby={invalid ? 'trace-request-id-error' : undefined}
          autoComplete='off'
        />
        <Button type='submit'>{t('Search')}</Button>
      </div>
      {invalid && (
        <p
          id='trace-request-id-error'
          className='text-destructive text-sm'
          role='alert'
        >
          {t('Enter a valid request ID')}
        </p>
      )}
    </form>
  )
}

/**
 * 独立的请求时间线弹窗。传入 requestId 时直接展示；为 null 时先输入请求 ID。
 */
export function RequestTimelineDialog(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  requestId: string | null
  admin: boolean
}) {
  const { t } = useTranslation()
  const [searchedId, setSearchedId] = useState<string | null>(null)
  const searchable = props.requestId === null
  const requestId = props.requestId ?? searchedId
  return (
    <Dialog
      open={props.open}
      onOpenChange={(open) => {
        if (!open) setSearchedId(null)
        props.onOpenChange(open)
      }}
      title={searchable ? t('Find request timeline') : t('Request timeline')}
      description={
        searchable
          ? t('Look up a request even when it has no usage log.')
          : t('Time spent in each stage, measured from gateway entry.')
      }
      contentClassName='min-w-0 overflow-hidden max-sm:max-h-[calc(100dvh-1.5rem)] max-sm:w-[calc(100vw-1.5rem)] max-sm:max-w-[calc(100vw-1.5rem)] max-sm:p-4 sm:max-w-4xl lg:max-w-5xl'
      contentHeight='min(80dvh, 860px)'
      bodyClassName='pr-2 sm:pr-4'
    >
      {searchable && <RequestIdSearch onSearch={setSearchedId} />}
      {props.open && requestId && (
        <RequestTimeline
          key={requestId}
          requestId={requestId}
          admin={props.admin}
          enabled={props.open}
        />
      )}
    </Dialog>
  )
}
