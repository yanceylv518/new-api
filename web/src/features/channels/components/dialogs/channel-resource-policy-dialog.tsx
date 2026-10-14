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
import { useEffect } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
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
import { useAuthStore } from '@/stores/auth-store'

import {
  getChannelResourcePolicy,
  updateChannelResourcePolicy,
} from '../../api'
import {
  canManageChannelResourcePolicy,
  channelResourcePolicySchema,
} from '../../lib/channel-resource-policy'
import type { Channel } from '../../types'

type Props = {
  open: boolean
  onOpenChange: (open: boolean) => void
  channel: Pick<Channel, 'id' | 'name'>
}

export function ChannelResourcePolicyDialog(props: Props) {
  const user = useAuthStore((s) => s.auth.user)
  if (!props.open || !canManageChannelResourcePolicy(user)) return null
  return <ChannelResourcePolicyEditor key={props.channel.id} {...props} />
}

function ChannelResourcePolicyEditor(props: Props) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const queryKey = ['channel-resource-policy', props.channel.id]
  const query = useQuery({
    queryKey,
    queryFn: ({ signal }) => getChannelResourcePolicy(props.channel.id, signal),
    retry: false,
    staleTime: 0,
    refetchOnMount: 'always',
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })
  const form = useForm<{ limitMB: string }>({
    resolver: zodResolver(channelResourcePolicySchema(t)),
    defaultValues: { limitMB: '' },
  })
  useEffect(() => {
    if (query.data) {
      form.reset({
        limitMB: formatRequestBodyMB(query.data.max_request_body_bytes),
      })
    }
  }, [query.data, form])
  const save = useMutation({
    mutationFn: (bytes: number) =>
      updateChannelResourcePolicy(props.channel.id, bytes),
    onSuccess: (policy) => {
      client.setQueryData(queryKey, policy)
      toast.success(t('Setting updated successfully'))
      props.onOpenChange(false)
    },
    onError: handleServerError,
  })
  return (
    <Dialog
      open={props.open}
      onOpenChange={(open) => {
        if (!save.isPending) props.onOpenChange(open)
      }}
      showCloseButton={!save.isPending}
      title={t('Channel request body limit')}
      description={t(
        'Request body limit for channel {{name}} (#{{id}}).',
        props.channel
      )}
    >
      {(query.isFetching || query.isPending) && <LoadingState />}
      {!query.isFetching && query.isError && (
        <ErrorState
          title={t('Unable to load channel request body limit.')}
          onRetry={() => {
            void query.refetch()
          }}
        />
      )}
      {!query.isFetching && query.isSuccess && (
        <Form {...form}>
          <form
            className='space-y-4'
            onSubmit={form.handleSubmit((values) => {
              const bytes = parseRequestBodyMB(values.limitMB)
              if (bytes !== null && !save.isPending) save.mutate(bytes)
            })}
          >
            <FormField
              control={form.control}
              name='limitMB'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Request body limit (MB)')}</FormLabel>
                  <FormControl>
                    <Input
                      {...field}
                      inputMode='decimal'
                      disabled={save.isPending}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      '0 means no channel limit. System and key limits still apply.'
                    )}{' '}
                    {t(
                      '1 MB = 1,048,576 bytes. Decimals are supported; rounded to the nearest byte.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <div className='flex justify-end gap-2'>
              <Button
                type='button'
                variant='outline'
                disabled={save.isPending}
                onClick={() => props.onOpenChange(false)}
              >
                {t('Cancel')}
              </Button>
              <Button
                type='submit'
                disabled={save.isPending || !form.formState.isDirty}
              >
                {save.isPending ? t('Saving...') : t('Save')}
              </Button>
            </div>
          </form>
        </Form>
      )}
    </Dialog>
  )
}
