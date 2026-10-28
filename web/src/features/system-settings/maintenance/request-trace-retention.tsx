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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import type { UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Progress } from '@/components/ui/progress'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import { getTraceCleanupStatus, startTraceCleanupTask } from '../api'
import { SettingsControlGroup } from '../components/settings-form-layout'
import type { LogSettingsFormValues } from './log-settings-section'

export function RequestTraceRetentionControls(props: {
  form: UseFormReturn<LogSettingsFormValues>
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const userID = useAuthStore((state) => state.auth.user?.id)
  const [confirmOpen, setConfirmOpen] = useState(false)
  const fieldName = 'RequestTraceRetentionDays'
  const queryKey = ['trace-cleanup', userID, 'timeline']
  const query = useQuery({
    queryKey,
    enabled: !!userID,
    queryFn: ({ signal }) => getTraceCleanupStatus(signal),
    refetchInterval: (current) => (current.state.error ? false : 5000),
    refetchOnWindowFocus: false,
  })
  const cleanup = useMutation({
    mutationFn: () => startTraceCleanupTask(),
    onSuccess: (status) => {
      queryClient.setQueryData(queryKey, status)
      setConfirmOpen(false)
    },
    onError: (error) => {
      handleServerError(error)
    },
  })
  const task = query.data?.task
  const active = task?.status === 'pending' || task?.status === 'running'
  const dirty = !!props.form.formState.dirtyFields[fieldName]
  const disabled =
    dirty ||
    props.form.formState.isSubmitting ||
    cleanup.isPending ||
    active ||
    query.isError ||
    !query.data ||
    query.data.retention_days <= 0
  const labels = {
    pending: t('Pending'),
    running: t('Running'),
    succeeded: t('Succeeded'),
    failed: t('Failed'),
  }

  return (
    <SettingsControlGroup>
      <FormField
        control={props.form.control}
        name={fieldName}
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Request timeline retention (days)')}</FormLabel>
            <FormDescription>
              {t(
                'Default: 30 days. Expired records are cleaned automatically. 0 keeps records indefinitely. Usage logs are kept.'
              )}
            </FormDescription>
            <FormControl>
              <Input
                {...field}
                type='number'
                min={0}
                max={3650}
                step={1}
                value={Number.isFinite(field.value) ? field.value : ''}
                onChange={(event) => field.onChange(event.target.valueAsNumber)}
                disabled={props.form.formState.isSubmitting}
              />
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
      <div className='flex flex-col gap-2'>
        <Button
          type='button'
          variant='outline'
          className='self-start'
          disabled={disabled}
          onClick={() => setConfirmOpen(true)}
        >
          {active || cleanup.isPending
            ? t('Cleaning...')
            : t('Clean expired records')}
        </Button>
        {dirty && (
          <p className='text-muted-foreground text-xs'>
            {t('Save the retention period before cleaning.')}
          </p>
        )}
        {query.isPending && <LoadingState className='min-h-0 py-3' />}
        {query.isError && (
          <ErrorState
            title={t('Unable to load trace cleanup status')}
            className='min-h-0 py-3'
            onRetry={() => void query.refetch()}
          />
        )}
        {cleanup.isError && (
          <p role='alert' className='text-destructive text-xs'>
            {t(cleanup.error.message)}
          </p>
        )}
        {task && (
          <div role='status' className='space-y-2 text-xs'>
            <p className='text-muted-foreground'>
              {labels[task.status]} ·{' '}
              {t('{{count}} trace records removed.', {
                count:
                  task.state?.trace_processed ??
                  task.result?.trace_deleted_count ??
                  0,
              })}
            </p>
            {active && (
              <Progress
                aria-label={t('Request trace cleanup')}
                value={Math.min(100, Math.max(0, task.state?.progress ?? 0))}
              />
            )}
            {task.error && <p className='text-destructive'>{t(task.error)}</p>}
          </div>
        )}
      </div>
      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={t('Clean expired request timelines?')}
        desc={t(
          'Records older than the saved retention period of {{days}} days will be permanently deleted. Usage logs are kept.',
          { days: query.data?.retention_days }
        )}
        confirmText={t('Confirm Cleanup')}
        destructive
        disabled={disabled}
        isLoading={cleanup.isPending}
        handleConfirm={() => {
          if (disabled) {
            toast.error(t('Save the retention period before cleaning.'))
            return
          }
          cleanup.mutate()
        }}
      />
    </SettingsControlGroup>
  )
}
