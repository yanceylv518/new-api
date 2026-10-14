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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { Dialog } from '@/components/dialog'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { FieldGroup } from '@/components/ui/field'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { handleServerError } from '@/lib/handle-server-error'
import {
  formatRequestBodyMB,
  parseRequestBodyMB,
} from '@/lib/request-body-size'

import {
  getUserTokenResourcePolicies,
  updateUserTokenResourcePolicy,
  type TokenResourcePolicy,
  type TokenResourcePolicyPage,
} from '../../api'

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
  user: { id: number; username: string }
}

export function UserTokenResourceDialog(props: Props) {
  const { t } = useTranslation()
  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Key resource limits')}
      description={t(
        'Resource limits for {{username}}. Leave blank to inherit defaults; enter 0 for unlimited. The system request body ceiling still applies.',
        { username: props.user.username }
      )}
    >
      {props.open && (
        <TokenPolicyList key={props.user.id} userId={props.user.id} />
      )}
    </Dialog>
  )
}

function TokenPolicyList(props: { userId: number }) {
  const { t } = useTranslation()
  const [page, setPage] = useState(1)
  const query = useQuery({
    queryKey: ['token-resource-policies', props.userId, page],
    queryFn: () => getUserTokenResourcePolicies(props.userId, page),
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    refetchOnMount: 'always',
  })
  // Mount editors only after the fresh read, so cached defaults cannot remain
  // in react-hook-form and overwrite a newer policy saved by another admin.
  if (query.isPending || query.isFetching) return <LoadingState />
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return (
    <div className='flex flex-col gap-4'>
      {query.data?.defaults?.enabled === false && (
        <Alert>
          <AlertDescription>
            {t(
              'Key resource limits are disabled. The saved limits below are not enforced.'
            )}
          </AlertDescription>
        </Alert>
      )}
      {query.data?.items.length === 0 && (
        <EmptyState title={t('No API keys')} />
      )}
      {query.data?.items.map((policy) => (
        <TokenPolicyEditor
          key={policy.token_id}
          userId={props.userId}
          policy={policy}
          defaults={query.data?.defaults}
        />
      ))}
      <div className='flex items-center justify-between gap-2'>
        <Button
          variant='outline'
          disabled={page <= 1}
          onClick={() => setPage(page - 1)}
        >
          {t('Previous')}
        </Button>
        <span>{t('Page {{page}}', { page })}</span>
        <Button
          variant='outline'
          disabled={page * 10 >= (query.data?.total ?? 0)}
          onClick={() => setPage(page + 1)}
        >
          {t('Next')}
        </Button>
      </div>
    </div>
  )
}

function TokenPolicyEditor(props: {
  userId: number
  policy: TokenResourcePolicy
  defaults?: TokenResourcePolicyPage['defaults']
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  let bodyPlaceholder = t('Inherit default (current value unavailable)')
  let concurrencyPlaceholder = bodyPlaceholder
  if (props.defaults) {
    const body =
      props.defaults.max_request_body_bytes === 0
        ? t('Unlimited')
        : `${formatRequestBodyMB(props.defaults.max_request_body_bytes)} MB`
    const concurrency =
      props.defaults.max_concurrent_requests === 0
        ? t('Unlimited')
        : String(props.defaults.max_concurrent_requests)
    bodyPlaceholder = t('Inherit default ({{value}})', { value: body })
    concurrencyPlaceholder = t('Inherit default ({{value}})', {
      value: concurrency,
    })
  }
  const limit = (max: number) =>
    z
      .string()
      .refine(
        (value) =>
          value === '' ||
          (/^\d+$/.test(value) &&
            Number.isSafeInteger(Number(value)) &&
            Number(value) <= max),
        t('Enter a non-negative whole number within the allowed range.')
      )
  const schema = z.object({
    max_request_body_bytes: z
      .string()
      .refine(
        (value) => value === '' || parseRequestBodyMB(value) !== null,
        t(
          'Enter a valid non-negative size in MB (at least 1 byte when positive).'
        )
      ),
    max_concurrent_requests: limit(2147483647),
  })
  const form = useForm<z.infer<typeof schema>>({
    resolver: zodResolver(schema),
    defaultValues: {
      max_request_body_bytes:
        props.policy.max_request_body_bytes == null
          ? ''
          : formatRequestBodyMB(props.policy.max_request_body_bytes),
      max_concurrent_requests:
        props.policy.max_concurrent_requests?.toString() ?? '',
    },
  })
  const save = useMutation({
    mutationFn: async (values: z.infer<typeof schema>) => {
      const limits = {
        max_request_body_bytes:
          values.max_request_body_bytes === ''
            ? null
            : parseRequestBodyMB(values.max_request_body_bytes),
        max_concurrent_requests:
          values.max_concurrent_requests === ''
            ? null
            : Number(values.max_concurrent_requests),
      }
      await updateUserTokenResourcePolicy(
        props.userId,
        props.policy.token_id,
        limits
      )
      client.setQueriesData<TokenResourcePolicyPage>(
        { queryKey: ['token-resource-policies', props.userId] },
        (page) =>
          page && {
            ...page,
            items: page.items.map((policy) =>
              policy.token_id === props.policy.token_id
                ? { ...policy, ...limits }
                : policy
            ),
          }
      )
      form.reset(values)
      toast.success(t('Setting updated successfully'))
      // Mark cached pages stale without refetching/resetting other edited rows.
      await client.invalidateQueries({
        queryKey: ['token-resource-policies', props.userId],
        refetchType: 'none',
      })
    },
    onError: handleServerError,
  })
  return (
    <Form {...form}>
      <form
        aria-label={t('Resource limits for key {{id}}', {
          id: props.policy.token_id,
        })}
        onSubmit={form.handleSubmit((values) => save.mutate(values))}
        className='rounded-md border p-3'
      >
        <FieldGroup>
          <p className='font-medium break-all'>
            {props.policy.name || t('Unnamed key')} · #{props.policy.token_id}
          </p>
          <FormField
            control={form.control}
            name='max_request_body_bytes'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Request body limit (MB)')}</FormLabel>
                <FormControl>
                  <Input
                    inputMode='decimal'
                    {...field}
                    disabled={save.isPending}
                    placeholder={bodyPlaceholder}
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    '1 MB = 1,048,576 bytes. Decimals are supported; rounded to the nearest byte.'
                  )}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='max_concurrent_requests'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Concurrent requests')}</FormLabel>
                <FormControl>
                  <Input
                    inputMode='numeric'
                    {...field}
                    disabled={save.isPending}
                    placeholder={concurrencyPlaceholder}
                  />
                </FormControl>
                <FormMessage />
              </FormItem>
            )}
          />
          <Button
            type='submit'
            disabled={save.isPending || !form.formState.isDirty}
          >
            {t('Save')}
          </Button>
        </FieldGroup>
      </form>
    </Form>
  )
}
