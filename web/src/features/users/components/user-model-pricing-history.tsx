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
import { useQuery } from '@tanstack/react-query'
import { History } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import dayjs from '@/lib/dayjs'

import { getUserModelPricingHistory } from '../api'
import type { UserModelPricingHistoryItem } from '../types'

function HistoryConfiguration(props: {
  value: UserModelPricingHistoryItem['before']
  label: string
}) {
  const { t } = useTranslation()
  return (
    <div className='min-w-0 flex-1 rounded-md border p-2 text-xs'>
      <p className='mb-1 font-medium'>{props.label}</p>
      {!props.value ? (
        <span className='text-muted-foreground'>{t('No discount')}</span>
      ) : (
        <>
          <p>
            {props.value.mode === 'scheduled'
              ? t('Multiple periods')
              : t('Single period')}
          </p>
          {props.value.periods.map((period) => (
            <p
              key={period.start_time ?? 'initial'}
              className='mt-1 font-mono break-words'
            >
              {period.discount_bps / 100}% ·{' '}
              {period.start_time
                ? dayjs.unix(period.start_time).format('YYYY-MM-DD HH:mm')
                : t('Existing rule')}
              {' → '}
              {period.end_time == null
                ? t('Permanent')
                : dayjs.unix(period.end_time).format('YYYY-MM-DD HH:mm')}
            </p>
          ))}
        </>
      )}
    </div>
  )
}

function HistoryContent(props: { userId?: number }) {
  const { t } = useTranslation()
  const [input, setInput] = useState(props.userId?.toString() ?? '')
  const [userId, setUserId] = useState(props.userId)
  const [page, setPage] = useState(1)
  const [invalidId, setInvalidId] = useState(false)
  const query = useQuery({
    queryKey: ['user-model-pricing-history', userId, page],
    queryFn: async ({ signal }) => {
      const response = await getUserModelPricingHistory(userId, page, signal)
      if (!response.success || !response.data) {
        throw new Error(
          response.message || t('Failed to load discount history')
        )
      }
      return response.data
    },
    staleTime: 0,
  })
  const items = query.data?.items ?? []
  const pageCount = Math.max(1, Math.ceil((query.data?.total ?? 0) / 20))
  const actionLabels = {
    create: t('Create'),
    update: t('Modified'),
    remove: t('Removed'),
    user_deleted: t('User deleted'),
  }
  return (
    <div className='flex flex-col gap-3'>
      <form
        className='flex gap-2'
        onSubmit={(event) => {
          event.preventDefault()
          const parsed = input.trim() === '' ? undefined : Number(input)
          if (
            parsed !== undefined &&
            (!Number.isSafeInteger(parsed) || parsed <= 0)
          ) {
            setInvalidId(true)
            return
          }
          setInvalidId(false)
          if (parsed === userId && page === 1) void query.refetch()
          setUserId(parsed)
          setPage(1)
        }}
      >
        <Input
          aria-label={t('User ID')}
          placeholder={t('User ID (including deleted users)')}
          value={input}
          aria-invalid={invalidId}
          onChange={(event) => setInput(event.target.value)}
        />
        <Button type='submit' variant='outline'>
          {t('Search')}
        </Button>
      </form>
      {invalidId && (
        <p role='alert' className='text-destructive text-sm'>
          {t('Enter a valid user ID')}
        </p>
      )}
      <p className='text-muted-foreground text-xs'>
        {t('Blank end time means permanent.')}
      </p>
      {query.isPending && <LoadingState />}
      {!query.isPending && query.isError && (
        <ErrorState
          title={t('Failed to load discount history')}
          onRetry={() => {
            void query.refetch()
          }}
        />
      )}
      {!query.isPending && !query.isError && items.length === 0 && (
        <EmptyState title={t('No discount history')} />
      )}
      {!query.isPending &&
        !query.isError &&
        items.map((item) => (
          <article
            key={item.id}
            className='flex flex-col gap-2 rounded-lg border p-3'
          >
            <div className='flex flex-wrap items-center gap-2 text-xs'>
              <span className='font-mono font-medium break-all'>
                {item.model_name}
              </span>
              <Badge variant='outline'>{actionLabels[item.action]}</Badge>
              {item.user_deleted && (
                <Badge variant='secondary'>{t('Deleted account')}</Badge>
              )}
            </div>
            <p className='text-muted-foreground text-xs'>
              {t('User ID')}: {item.user_id} · {t('Operator ID')}:{' '}
              {item.actor_id || t('System')} · {t('Revision')}: {item.revision}{' '}
              · {dayjs.unix(item.created_at).format('YYYY-MM-DD HH:mm:ss')}
            </p>
            <div className='flex flex-col gap-2 sm:flex-row'>
              <HistoryConfiguration label={t('Before')} value={item.before} />
              <HistoryConfiguration label={t('After')} value={item.after} />
            </div>
          </article>
        ))}
      <div className='flex items-center justify-between gap-2'>
        <Button
          variant='outline'
          disabled={page <= 1 || query.isFetching}
          onClick={() => setPage(page - 1)}
        >
          {t('Previous page')}
        </Button>
        <span className='text-muted-foreground text-xs'>
          {t('Page {{current}} of {{total}}', {
            current: page,
            total: pageCount,
          })}
        </span>
        <Button
          variant='outline'
          disabled={page >= pageCount || query.isFetching}
          onClick={() => setPage(page + 1)}
        >
          {t('Next page')}
        </Button>
      </div>
    </div>
  )
}

export function UserModelPricingHistoryButton(props: { userId?: number }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button
        type='button'
        variant='outline'
        size='sm'
        onClick={() => setOpen(true)}
      >
        <History aria-hidden='true' />
        {t('Discount history')}
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title={t('Discount history')}
        contentClassName='sm:max-w-3xl'
      >
        {open && <HistoryContent userId={props.userId} />}
      </Dialog>
    </>
  )
}
