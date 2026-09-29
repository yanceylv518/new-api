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
import { ChevronDown } from 'lucide-react'
import { lazy, Suspense, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { getTaskRequestSnapshot } from '../../api'

const CodeBlock = lazy(() =>
  import('@/components/ai-elements/code-block').then((module) => ({
    default: module.CodeBlock,
  }))
)

function RequestBodyContent(props: { taskId: string }) {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey: ['task-request-snapshot', props.taskId],
    queryFn: ({ signal }) => getTaskRequestSnapshot(props.taskId, signal),
    retry: false,
    staleTime: Infinity,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    meta: { errorToast: false },
  })
  const body = useMemo(
    () => (query.data ? JSON.stringify(query.data.body, null, 2) : ''),
    [query.data]
  )

  if (query.isPending) {
    return (
      <div role='status'>
        <LoadingState className='min-h-24' size='sm' />
      </div>
    )
  }
  if (query.isError) {
    const title = t('Failed to load request body')
    const message = getServerErrorMessage(query.error, title)
    return (
      <ErrorState
        className='min-h-24 p-3'
        title={title}
        description={message === title ? undefined : message}
        onRetry={() => {
          void query.refetch()
        }}
      />
    )
  }
  if (!query.data) {
    return (
      <EmptyState
        className='min-h-24 p-3'
        title={t('No request body was saved for this task')}
      />
    )
  }
  return (
    <div className='min-w-0 space-y-2 pt-2'>
      <div className='flex flex-wrap items-center gap-2'>
        {query.data.base64_omitted ? (
          <Badge variant='secondary'>{t('Base64 content omitted')}</Badge>
        ) : null}
        {query.data.truncated ? (
          <Badge variant='secondary'>{t('Request body truncated')}</Badge>
        ) : null}
        <CopyButton
          value={body}
          className='ml-auto'
          tooltip={t('Copy request body')}
        />
      </div>
      <Suspense fallback={<LoadingState className='min-h-24' size='sm' />}>
        <CodeBlock
          code={body}
          language='json'
          title={t('Request body')}
          maxExpandedLines={18}
          showLineNumbers
          enableCollapse={false}
          className='my-0'
        />
      </Suspense>
    </div>
  )
}

export function TaskRequestBody(props: { taskId: string }) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState(false)
  return (
    <Collapsible open={expanded} onOpenChange={setExpanded} className='min-w-0'>
      <CollapsibleTrigger className='hover:bg-muted/50 focus-visible:ring-ring flex w-full items-center justify-between gap-2 rounded-md border p-2.5 text-left text-xs font-semibold outline-none focus-visible:ring-2'>
        {t('Request body')}
        <ChevronDown
          aria-hidden='true'
          className={expanded ? 'size-4 rotate-180' : 'size-4'}
        />
      </CollapsibleTrigger>
      <CollapsibleContent>
        {expanded ? <RequestBodyContent taskId={props.taskId} /> : null}
      </CollapsibleContent>
    </Collapsible>
  )
}
