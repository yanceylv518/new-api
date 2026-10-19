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
import { useCallback, useEffect, useMemo, useState } from 'react'
import { useForm, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import * as z from 'zod'

import { DateTimePicker } from '@/components/datetime-picker'
import { Accordion } from '@/components/ui/accordion'
import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
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
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import { api } from '@/lib/api'
import dayjs from '@/lib/dayjs'
import { formatTimestampToDate } from '@/lib/format'

import {
  getCurrentLogCleanupTask,
  getSystemTask,
  startLogCleanupTask,
} from '../api'
import { SettingsAccordion } from '../components/settings-accordion'
import {
  SettingsControlGroup,
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'
import type { LogCleanupTask } from '../types'
import { RequestTracePolicyControls } from './request-trace-policy'
import {
  defaultRequestTraceConfig,
  logSettingsSchema,
  requestTraceConfigSchema,
  type LogSettingsFormValues,
} from './request-trace-policy-schema'
import { RequestTraceStatus } from './request-trace-status'

const MAX_REQUEST_ID_HEADERS = 16
const MAX_REQUEST_ID_HEADER_LENGTH = 128

const upstreamRequestIDHeadersSchema = z
  .array(
    z
      .string()
      .max(MAX_REQUEST_ID_HEADER_LENGTH)
      .regex(/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/)
  )
  .max(MAX_REQUEST_ID_HEADERS)

type LogSettingsSectionProps = {
  defaultEnabled: boolean
  defaultTraceConfig?: string
}

type ServerLogInfo = {
  enabled: boolean
  log_dir: string
  file_count: number
  total_size: number
  oldest_time?: string
  newest_time?: string
}

const HOURS_IN_DAY = 24

function formatBytes(bytes: number, decimals = 2): string {
  if (!bytes || Number.isNaN(bytes)) return '0 Bytes'
  if (bytes === 0) return '0 Bytes'
  if (bytes < 0) return `-${formatBytes(-bytes, decimals)}`
  const k = 1024
  const sizes = ['Bytes', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(Math.abs(bytes)) / Math.log(k))
  if (i < 0 || i >= sizes.length) return `${bytes} Bytes`
  return `${Number.parseFloat((bytes / Math.pow(k, i)).toFixed(decimals))} ${
    sizes[i]
  }`
}

const getDateHoursAgo = (hours: number) => {
  const date = new Date()
  date.setHours(date.getHours() - hours)
  return date
}

const getDateDaysAgo = (days: number) => getDateHoursAgo(days * HOURS_IN_DAY)

const quickSelectOptions = [
  {
    label: '24 hours ago',
    getValue: () => getDateHoursAgo(24),
  },
  {
    label: '7 days ago',
    getValue: () => getDateDaysAgo(7),
  },
  {
    label: '30 days ago',
    getValue: () => getDateDaysAgo(30),
  },
]

function isActiveLogCleanupTask(task: LogCleanupTask | null) {
  return task?.status === 'pending' || task?.status === 'running'
}

export function LogSettingsSection({
  defaultEnabled,
  defaultTraceConfig: encodedTraceConfig,
}: LogSettingsSectionProps) {
  const { t } = useTranslation()
  const defaultTraceConfig = useMemo(() => {
    try {
      const result = requestTraceConfigSchema.safeParse(
        JSON.parse(encodedTraceConfig ?? '')
      )
      return result.success ? result.data : defaultRequestTraceConfig
    } catch {
      return defaultRequestTraceConfig
    }
  }, [encodedTraceConfig])
  const updateOption = useUpdateOption()
  const form = useForm<LogSettingsFormValues>({
    resolver: zodResolver(logSettingsSchema),
    defaultValues: {
      LogConsumeEnabled: defaultEnabled,
      RequestTraceEnabled: defaultTraceConfig.level !== 'off',
      RequestTraceLevel: defaultTraceConfig.level === 'all' ? 'all' : 'errors',
      RequestTracePreChannelErrorsEnabled:
        defaultTraceConfig?.pre_channel_errors_enabled ?? true,
      RequestTraceAnonymousErrorsEnabled:
        defaultTraceConfig?.anonymous_errors_enabled ?? false,
      RequestTraceUpstreamRequestIdHeaders:
        defaultTraceConfig.upstream_request_id_headers.join('\n'),
      RequestTraceCompressionLevel: defaultTraceConfig.compression_level,
      RequestTraceSuccessSampleRate: defaultTraceConfig.success_sample_rate,
      RequestTraceRetentionDays: defaultTraceConfig.retention_days,
      RequestTraceDiagnosticChannels:
        defaultTraceConfig.diagnostic_channel_ids.join(', '),
      RequestTraceDiagnosticExpiresAt: defaultTraceConfig.diagnostic_expires_at,
    },
  })

  const traceEnabled = useWatch({
    control: form.control,
    name: 'RequestTraceEnabled',
  })
  const [traceSettingsOpen, setTraceSettingsOpen] = useState(false)

  const [purgeDate, setPurgeDate] = useState<Date | undefined>(() =>
    getDateDaysAgo(30)
  )
  const [isStartingLogCleanup, setIsStartingLogCleanup] = useState(false)
  const [logCleanupTask, setLogCleanupTask] = useState<LogCleanupTask | null>(
    null
  )
  const [showConfirmDialog, setShowConfirmDialog] = useState(false)
  const [serverLogInfo, setServerLogInfo] = useState<ServerLogInfo | null>(null)
  const [serverLogCleanupMode, setServerLogCleanupMode] = useState('by_count')
  const [serverLogCleanupValue, setServerLogCleanupValue] = useState(10)
  const [serverLogCleanupLoading, setServerLogCleanupLoading] = useState(false)

  const fetchServerLogInfo = useCallback(async () => {
    try {
      const res = await api.get('/api/performance/logs')
      if (res.data.success) setServerLogInfo(res.data.data)
    } catch {
      /* ignore */
    }
  }, [])

  useEffect(() => {
    form.reset({
      LogConsumeEnabled: defaultEnabled,
      RequestTraceEnabled: defaultTraceConfig.level !== 'off',
      RequestTraceLevel:
        defaultTraceConfig.level === 'off'
          ? form.getValues('RequestTraceLevel')
          : defaultTraceConfig.level,
      RequestTracePreChannelErrorsEnabled:
        defaultTraceConfig?.pre_channel_errors_enabled ?? true,
      RequestTraceAnonymousErrorsEnabled:
        defaultTraceConfig?.anonymous_errors_enabled ?? false,
      RequestTraceUpstreamRequestIdHeaders:
        defaultTraceConfig.upstream_request_id_headers.join('\n'),
      RequestTraceCompressionLevel: defaultTraceConfig.compression_level,
      RequestTraceSuccessSampleRate: defaultTraceConfig.success_sample_rate,
      RequestTraceRetentionDays: defaultTraceConfig.retention_days,
      RequestTraceDiagnosticChannels:
        defaultTraceConfig.diagnostic_channel_ids.join(', '),
      RequestTraceDiagnosticExpiresAt: defaultTraceConfig.diagnostic_expires_at,
    })
  }, [defaultEnabled, defaultTraceConfig, form])

  useEffect(() => {
    fetchServerLogInfo()
  }, [fetchServerLogInfo])

  useEffect(() => {
    let cancelled = false

    async function fetchCurrentLogCleanupTask() {
      try {
        const res = await getCurrentLogCleanupTask()
        if (!cancelled && res.success && res.data) {
          setLogCleanupTask(res.data)
        }
      } catch {
        /* ignore */
      }
    }

    fetchCurrentLogCleanupTask()

    return () => {
      cancelled = true
    }
  }, [])

  const purgeTimestamp = useMemo(() => {
    if (!purgeDate) return null
    return Math.floor(purgeDate.getTime() / 1000)
  }, [purgeDate])

  const formattedPurgeDate = useMemo(() => {
    if (!purgeDate) return ''
    return formatTimestampToDate(purgeDate.getTime(), 'milliseconds')
  }, [purgeDate])

  const logCleanupActive = isActiveLogCleanupTask(logCleanupTask)
  const logCleanupState = logCleanupTask?.state
  const logCleanupProgress = Math.min(
    100,
    Math.max(0, logCleanupState?.progress ?? 0)
  )
  const logCleanupProcessed = logCleanupState?.processed ?? 0
  const logCleanupTotal = logCleanupState?.total ?? 0
  const logCleanupTaskId = logCleanupTask?.task_id

  useEffect(() => {
    if (!logCleanupTaskId || !logCleanupActive) return

    let cancelled = false
    const interval = window.setInterval(async () => {
      try {
        const res = await getSystemTask(logCleanupTaskId)
        if (cancelled || !res.success || !res.data) return

        setLogCleanupTask(res.data)
        if (!isActiveLogCleanupTask(res.data)) {
          if (res.data.status === 'succeeded') {
            const count =
              res.data.result?.deleted_count ?? res.data.state?.processed ?? 0
            toast.success(
              count > 0
                ? t('{{count}} log entries removed.', { count })
                : t('No log entries matched the selected time.')
            )
          } else if (res.data.status === 'failed') {
            toast.error(res.data.error || t('Failed to clean logs'))
          }
        }
      } catch {
        /* keep polling */
      }
    }, 1000)

    return () => {
      cancelled = true
      window.clearInterval(interval)
    }
  }, [logCleanupActive, logCleanupTaskId, t])

  const onSubmit = async (values: LogSettingsFormValues) => {
    const headerNames = values.RequestTraceUpstreamRequestIdHeaders.split(
      /\r?\n/
    )
      .map((name) => name.trim())
      .filter(Boolean)
    const headers = upstreamRequestIDHeadersSchema.safeParse(headerNames)
    if (!headers.success) {
      setTraceSettingsOpen(true)
      form.setError(
        'RequestTraceUpstreamRequestIdHeaders',
        {
          type: 'validate',
          message: t(
            'Enter up to 16 valid response header names, each at most 128 characters.'
          ),
        },
        { shouldFocus: true }
      )
      return
    }
    const channelIDs = [
      ...new Set(
        values.RequestTraceDiagnosticChannels.split(/[,，\s]+/)
          .filter(Boolean)
          .map(Number)
      ),
    ]
    if (
      !z.array(z.number().int().positive()).max(16).safeParse(channelIDs)
        .success
    ) {
      setTraceSettingsOpen(true)
      form.setError(
        'RequestTraceDiagnosticChannels',
        {
          type: 'validate',
          message: t('Enter up to 16 positive integer channel IDs.'),
        },
        { shouldFocus: true }
      )
      return
    }
    const diagnosticChanged =
      values.RequestTraceDiagnosticExpiresAt !==
        defaultTraceConfig.diagnostic_expires_at ||
      JSON.stringify(channelIDs) !==
        JSON.stringify(defaultTraceConfig.diagnostic_channel_ids)
    const now = Math.floor(Date.now() / 1000)
    if (
      channelIDs.length > 0 &&
      (!values.RequestTraceDiagnosticExpiresAt ||
        values.RequestTraceDiagnosticExpiresAt > now + 86400 ||
        (diagnosticChanged && values.RequestTraceDiagnosticExpiresAt <= now))
    ) {
      setTraceSettingsOpen(true)
      form.setError(
        'RequestTraceDiagnosticExpiresAt',
        {
          type: 'validate',
          message: t('Choose a future diagnostic expiry within 24 hours.'),
        },
        { shouldFocus: true }
      )
      return
    }
    try {
      if (values.LogConsumeEnabled !== defaultEnabled) {
        const response = await updateOption.mutateAsync({
          key: 'LogConsumeEnabled',
          value: values.LogConsumeEnabled,
        })
        if (!response.success) return
      }
      const config = {
        level: values.RequestTraceEnabled ? values.RequestTraceLevel : 'off',
        pre_channel_errors_enabled: values.RequestTracePreChannelErrorsEnabled,
        anonymous_errors_enabled: values.RequestTraceAnonymousErrorsEnabled,
        upstream_request_id_headers: [
          ...new Map(
            headers.data.map((name) => [name.toLowerCase(), name])
          ).values(),
        ],
        compression_level: values.RequestTraceCompressionLevel,
        success_sample_rate: values.RequestTraceSuccessSampleRate,
        retention_days: values.RequestTraceRetentionDays,
        diagnostic_channel_ids: channelIDs,
        diagnostic_expires_at: channelIDs.length
          ? values.RequestTraceDiagnosticExpiresAt
          : 0,
      }
      if (
        config.level !== (defaultTraceConfig?.level ?? 'errors') ||
        config.success_sample_rate !== defaultTraceConfig.success_sample_rate ||
        config.retention_days !== defaultTraceConfig.retention_days ||
        config.diagnostic_expires_at !==
          defaultTraceConfig.diagnostic_expires_at ||
        JSON.stringify(config.diagnostic_channel_ids) !==
          JSON.stringify(defaultTraceConfig.diagnostic_channel_ids) ||
        config.compression_level !== defaultTraceConfig.compression_level ||
        config.pre_channel_errors_enabled !==
          (defaultTraceConfig?.pre_channel_errors_enabled ?? true) ||
        config.anonymous_errors_enabled !==
          (defaultTraceConfig?.anonymous_errors_enabled ?? false) ||
        JSON.stringify(config.upstream_request_id_headers) !==
          JSON.stringify(defaultTraceConfig.upstream_request_id_headers)
      ) {
        await updateOption.mutateAsync({
          key: 'RequestTraceConfig',
          value: JSON.stringify(config),
        })
      }
    } catch {
      // useUpdateOption 统一展示保存错误，保留输入以便重试。
    }
  }

  const handleSave = form.handleSubmit(onSubmit, () => {
    setTraceSettingsOpen(true)
  })

  const handleRequestCleanLogs = () => {
    if (!purgeTimestamp) {
      toast.error(t('Select a timestamp before clearing logs.'))
      return
    }

    setShowConfirmDialog(true)
  }

  const handleCleanLogs = async () => {
    if (!purgeTimestamp) {
      toast.error(t('Select a timestamp before clearing logs.'))
      return
    }

    setIsStartingLogCleanup(true)
    try {
      const res = await startLogCleanupTask(purgeTimestamp)
      if (!res.success) {
        throw new Error(res.message || t('Failed to clean logs'))
      }
      if (!res.data) {
        throw new Error(t('Failed to clean logs'))
      }
      setLogCleanupTask(res.data)
      setShowConfirmDialog(false)
      toast.success(t('Log cleanup task started.'))
    } catch (error) {
      const message =
        error instanceof Error ? error.message : t('Failed to clean logs')
      toast.error(message)
    } finally {
      setIsStartingLogCleanup(false)
    }
  }

  const cleanupServerLogFiles = async () => {
    if (
      !serverLogCleanupValue ||
      Number.isNaN(serverLogCleanupValue) ||
      serverLogCleanupValue < 1
    ) {
      toast.error(t('Please enter a valid number'))
      return
    }

    setServerLogCleanupLoading(true)
    try {
      const res = await api.delete(
        `/api/performance/logs?mode=${serverLogCleanupMode}&value=${serverLogCleanupValue}`
      )
      if (res.data.success) {
        const { deleted_count, freed_bytes } = res.data.data
        toast.success(
          t('Cleaned up {{count}} log files, freed {{size}}', {
            count: deleted_count,
            size: formatBytes(freed_bytes),
          })
        )
      } else {
        toast.error(res.data.message || t('Cleanup failed'))
      }
      fetchServerLogInfo()
    } catch {
      toast.error(t('Cleanup failed'))
    } finally {
      setServerLogCleanupLoading(false)
    }
  }

  return (
    <SettingsSection title={t('Log Maintenance')}>
      <Form {...form}>
        <SettingsForm onSubmit={handleSave}>
          <SettingsPageFormActions
            onSave={handleSave}
            isSaving={updateOption.isPending}
            saveLabel='Save log settings'
          />
          <FormField
            control={form.control}
            name='LogConsumeEnabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>{t('Record quota usage')}</FormLabel>
                  <FormDescription>
                    {t(
                      'Track per-request consumption to power usage analytics. Keeping this on increases database writes.'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                  />
                </FormControl>
                <FormMessage />
              </SettingsSwitchItem>
            )}
          />

          <SettingsControlGroup className='space-y-3'>
            <FormField
              control={form.control}
              name='RequestTraceEnabled'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Enable request tracing')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Disabling this and saving stops new trace metadata. Existing trace access, usage logs and billing are unaffected.'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                      disabled={updateOption.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                </SettingsSwitchItem>
              )}
            />
            <Accordion
              value={traceSettingsOpen ? ['request-trace'] : []}
              onValueChange={(value) =>
                setTraceSettingsOpen(value.includes('request-trace'))
              }
            >
              <SettingsAccordion
                value='request-trace'
                title={t('Request trace settings')}
              >
                <div className='space-y-3'>
                  <SettingsControlGroup className='space-y-3'>
                    <FormField
                      control={form.control}
                      name='RequestTraceLevel'
                      render={({ field }) => (
                        <FormItem className='space-y-2'>
                          <FormLabel>{t('Request trace level')}</FormLabel>
                          <FormDescription>
                            {t(
                              'Store request and response metadata separately. Message bodies and response text are never stored.'
                            )}
                          </FormDescription>
                          <Select
                            items={[
                              {
                                value: 'errors',
                                label: t('Detailed failures only'),
                              },
                              {
                                value: 'all',
                                label: t('All requests in detail'),
                              },
                            ]}
                            value={field.value}
                            onValueChange={field.onChange}
                            disabled={!traceEnabled || updateOption.isPending}
                          >
                            <FormControl>
                              <SelectTrigger className='w-full sm:w-56'>
                                <SelectValue />
                              </SelectTrigger>
                            </FormControl>
                            <SelectContent>
                              <SelectGroup>
                                <SelectItem value='errors'>
                                  {t('Detailed failures only')}
                                </SelectItem>
                                <SelectItem value='all'>
                                  {t('All requests in detail')}
                                </SelectItem>
                              </SelectGroup>
                            </SelectContent>
                          </Select>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name='RequestTraceCompressionLevel'
                      render={({ field }) => (
                        <FormItem className='space-y-2'>
                          <FormLabel>{t('Compression level')}</FormLabel>
                          <FormDescription>
                            {t(
                              'Applies to new request trace metadata in relational databases. Higher compression uses more CPU.'
                            )}
                          </FormDescription>
                          <Select
                            items={[
                              { value: 'fast', label: t('Fast (recommended)') },
                              {
                                value: 'standard',
                                label: t('Standard compression'),
                              },
                              { value: 'high', label: t('Higher compression') },
                              {
                                value: 'best',
                                label: t('Maximum compression'),
                              },
                            ]}
                            value={field.value}
                            onValueChange={field.onChange}
                            disabled={updateOption.isPending}
                          >
                            <FormControl>
                              <SelectTrigger className='w-full sm:w-56'>
                                <SelectValue />
                              </SelectTrigger>
                            </FormControl>
                            <SelectContent>
                              <SelectGroup>
                                <SelectItem value='fast'>
                                  {t('Fast (recommended)')}
                                </SelectItem>
                                <SelectItem value='standard'>
                                  {t('Standard compression')}
                                </SelectItem>
                                <SelectItem value='high'>
                                  {t('Higher compression')}
                                </SelectItem>
                                <SelectItem value='best'>
                                  {t('Maximum compression')}
                                </SelectItem>
                              </SelectGroup>
                            </SelectContent>
                          </Select>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name='RequestTracePreChannelErrorsEnabled'
                      render={({ field }) => (
                        <SettingsSwitchItem>
                          <SettingsSwitchContent>
                            <FormLabel>
                              {t('Record failures before upstream dispatch')}
                            </FormLabel>
                            <FormDescription>
                              {t(
                                'Record validation, rate limit and channel selection failures attributed to an authenticated user.'
                              )}
                            </FormDescription>
                          </SettingsSwitchContent>
                          <FormControl>
                            <Switch
                              checked={field.value}
                              onCheckedChange={field.onChange}
                              disabled={!traceEnabled || updateOption.isPending}
                            />
                          </FormControl>
                        </SettingsSwitchItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name='RequestTraceAnonymousErrorsEnabled'
                      render={({ field }) => (
                        <SettingsSwitchItem>
                          <SettingsSwitchContent>
                            <FormLabel>
                              {t('Record unauthenticated failures')}
                            </FormLabel>
                            <FormDescription>
                              {t(
                                'These records have no user owner and are visible only to administrators.'
                              )}
                            </FormDescription>
                          </SettingsSwitchContent>
                          <FormControl>
                            <Switch
                              checked={field.value}
                              onCheckedChange={field.onChange}
                              disabled={!traceEnabled || updateOption.isPending}
                            />
                          </FormControl>
                        </SettingsSwitchItem>
                      )}
                    />
                  </SettingsControlGroup>
                  <RequestTracePolicyControls
                    form={form}
                    disabled={updateOption.isPending}
                  />
                  <SettingsControlGroup>
                    <FormField
                      control={form.control}
                      name='RequestTraceUpstreamRequestIdHeaders'
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>
                            {t('Upstream request ID response headers')}
                          </FormLabel>
                          <FormDescription>
                            {t(
                              'One header name per line, in priority order. Custom headers take precedence over defaults. Leave blank to use defaults. Applies even when request tracing is off.'
                            )}
                            <span className='mt-1 block'>
                              {t('Default response headers: {{headers}}', {
                                headers:
                                  'X-Oneapi-Request-Id, X-Request-Id, Request-Id, X-Amzn-RequestId, X-Goog-Request-Id',
                              })}
                            </span>
                          </FormDescription>
                          <FormControl>
                            <Textarea
                              {...field}
                              placeholder={'X-Custom-Request-Id\nX-Trace-Id'}
                              className='font-mono text-xs'
                              maxLength={
                                MAX_REQUEST_ID_HEADERS *
                                (MAX_REQUEST_ID_HEADER_LENGTH + 1)
                              }
                              rows={3}
                              disabled={updateOption.isPending}
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  </SettingsControlGroup>
                  {traceSettingsOpen && <RequestTraceStatus />}
                </div>
              </SettingsAccordion>
            </Accordion>
          </SettingsControlGroup>
          <SettingsControlGroup className='space-y-3'>
            <div>
              <h4 className='text-sm font-medium'>{t('Clean history logs')}</h4>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Remove all log entries created before the selected timestamp.'
                )}
              </p>
            </div>
            <DateTimePicker value={purgeDate} onChange={setPurgeDate} />
            <div className='flex flex-wrap gap-3'>
              {quickSelectOptions.map((option) => (
                <Button
                  key={option.label}
                  type='button'
                  variant='outline'
                  onClick={() => setPurgeDate(option.getValue())}
                >
                  {t(option.label)}
                </Button>
              ))}
              <Button
                type='button'
                variant='destructive'
                onClick={handleRequestCleanLogs}
                disabled={isStartingLogCleanup || logCleanupActive}
              >
                {isStartingLogCleanup || logCleanupActive
                  ? t('Cleaning...')
                  : t('Clean logs')}
              </Button>
            </div>
            {logCleanupTask && (
              <div className='rounded-md border p-3'>
                <div className='mb-2 flex items-center justify-between gap-3 text-sm'>
                  <span className='font-medium'>
                    {t('Log cleanup progress')}
                  </span>
                  <span className='text-muted-foreground tabular-nums'>
                    {logCleanupProgress}%
                  </span>
                </div>
                <Progress value={logCleanupProgress} />
                <div className='text-muted-foreground mt-2 text-xs'>
                  {logCleanupState?.phase === 'traces'
                    ? t(
                        'Trace cleanup: {{processed}} of {{total}} records processed.',
                        {
                          processed: logCleanupState.trace_processed ?? 0,
                          total: logCleanupState.trace_total ?? 0,
                        }
                      )
                    : t('{{processed}} of {{total}} log entries processed.', {
                        processed: logCleanupProcessed,
                        total: logCleanupTotal,
                      })}
                </div>
                {!!logCleanupState?.trace_processed &&
                  logCleanupState.phase !== 'traces' && (
                    <p className='text-muted-foreground mt-1 text-xs'>
                      {t('{{count}} trace records removed.', {
                        count: logCleanupState.trace_processed,
                      })}
                    </p>
                  )}
                {logCleanupTask.status === 'failed' && logCleanupTask.error && (
                  <div className='text-destructive mt-2 text-xs'>
                    {logCleanupTask.error}
                  </div>
                )}
              </div>
            )}
          </SettingsControlGroup>
        </SettingsForm>
      </Form>

      <Separator />

      <div className='space-y-4'>
        <div>
          <h4 className='font-medium'>{t('Server Log Management')}</h4>
          <p className='text-muted-foreground mt-1 text-xs'>
            {t(
              'Manage server log files. Log files accumulate over time; regular cleanup is recommended to free disk space.'
            )}
          </p>
        </div>

        {serverLogInfo !== null &&
          (serverLogInfo.enabled ? (
            <div className='space-y-4'>
              <div className='rounded-lg border p-4'>
                <div className='grid grid-cols-2 gap-2 text-sm md:grid-cols-4'>
                  <div>
                    <span className='text-muted-foreground'>
                      {t('Log Directory')}:
                    </span>{' '}
                    <span className='font-mono text-xs'>
                      {serverLogInfo.log_dir}
                    </span>
                  </div>
                  <div>
                    <span className='text-muted-foreground'>
                      {t('Log File Count')}:
                    </span>{' '}
                    {serverLogInfo.file_count}
                  </div>
                  <div>
                    <span className='text-muted-foreground'>
                      {t('Total Log Size')}:
                    </span>{' '}
                    {formatBytes(serverLogInfo.total_size)}
                  </div>
                  {serverLogInfo.oldest_time && serverLogInfo.newest_time && (
                    <div>
                      <span className='text-muted-foreground'>
                        {t('Date Range')}:
                      </span>{' '}
                      {dayjs(serverLogInfo.oldest_time).format('YYYY-MM-DD')} ~{' '}
                      {dayjs(serverLogInfo.newest_time).format('YYYY-MM-DD')}
                    </div>
                  )}
                </div>
              </div>

              <div className='flex flex-wrap items-end gap-3'>
                <div className='grid gap-1.5'>
                  <Label className='text-xs'>{t('Cleanup Mode')}</Label>
                  <Select
                    items={[
                      { value: 'by_count', label: t('Retain last N files') },
                      { value: 'by_days', label: t('Retain last N days') },
                    ]}
                    value={serverLogCleanupMode}
                    onValueChange={(value) =>
                      value !== null && setServerLogCleanupMode(value)
                    }
                  >
                    <SelectTrigger className='w-[160px]'>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent alignItemWithTrigger={false}>
                      <SelectGroup>
                        <SelectItem value='by_count'>
                          {t('Retain last N files')}
                        </SelectItem>
                        <SelectItem value='by_days'>
                          {t('Retain last N days')}
                        </SelectItem>
                      </SelectGroup>
                    </SelectContent>
                  </Select>
                </div>
                <div className='grid gap-1.5'>
                  <Label className='text-xs'>
                    {serverLogCleanupMode === 'by_count'
                      ? t('Files to Retain')
                      : t('Days to Retain')}
                  </Label>
                  <Input
                    type='number'
                    min={1}
                    max={serverLogCleanupMode === 'by_count' ? 1000 : 3650}
                    value={serverLogCleanupValue}
                    onChange={(event) =>
                      setServerLogCleanupValue(Number(event.target.value))
                    }
                    className='w-[120px]'
                  />
                </div>
                <AlertDialog>
                  <AlertDialogTrigger
                    render={
                      <Button
                        type='button'
                        variant='destructive'
                        size='sm'
                        disabled={serverLogCleanupLoading}
                      />
                    }
                  >
                    {serverLogCleanupLoading
                      ? t('Cleaning...')
                      : t('Clean Up Log Files')}
                  </AlertDialogTrigger>
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>
                        {t('Confirm log file cleanup?')}
                      </AlertDialogTitle>
                      <AlertDialogDescription>
                        {serverLogCleanupMode === 'by_count'
                          ? t(
                              'Only the last {{value}} log files will be retained; the rest will be deleted.',
                              {
                                value: serverLogCleanupValue,
                              }
                            )
                          : t(
                              'Log files older than {{value}} days will be deleted.',
                              {
                                value: serverLogCleanupValue,
                              }
                            )}
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>{t('Cancel')}</AlertDialogCancel>
                      <AlertDialogAction
                        variant='destructive'
                        onClick={cleanupServerLogFiles}
                      >
                        {t('Confirm Cleanup')}
                      </AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
              </div>
            </div>
          ) : (
            <Alert>
              <AlertDescription>
                {t(
                  'Server logging is not enabled (log directory not configured)'
                )}
              </AlertDescription>
            </Alert>
          ))}
      </div>

      <AlertDialog open={showConfirmDialog} onOpenChange={setShowConfirmDialog}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t('Confirm log cleanup')}</AlertDialogTitle>
            <AlertDialogDescription>
              {formattedPurgeDate
                ? t(
                    'This will permanently remove all log entries created before {{date}}.',
                    { date: formattedPurgeDate }
                  )
                : t(
                    'This will permanently remove log entries before the selected timestamp.'
                  )}{' '}
              {t('This action cannot be undone.')}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={isStartingLogCleanup}>
              {t('Cancel')}
            </AlertDialogCancel>
            <AlertDialogAction
              variant='destructive'
              onClick={handleCleanLogs}
              disabled={isStartingLogCleanup}
            >
              {isStartingLogCleanup ? t('Cleaning...') : t('Delete logs')}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsSection>
  )
}
