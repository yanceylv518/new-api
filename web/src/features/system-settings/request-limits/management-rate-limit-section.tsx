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
import axios from 'axios'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import * as z from 'zod'

import { ErrorState } from '@/components/error-state'
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
import { Switch } from '@/components/ui/switch'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

const maxRateLimitValue = 2_147_483_647
const limitSchema = z.object({
  enabled: z.boolean(),
  requests: z.number().int().min(1).max(maxRateLimitValue),
  window_seconds: z.number().int().min(1).max(maxRateLimitValue),
})

const managementRateLimitsSchema = z.object({
  global_api: limitSchema,
  login_ip: limitSchema,
  login_account: limitSchema,
  session_refresh: limitSchema,
  token_query: limitSchema,
  sensitive: limitSchema,
})

type ManagementRateLimits = z.infer<typeof managementRateLimitsSchema>

function parseManagementRateLimits(value: string): ManagementRateLimits | null {
  try {
    const result = managementRateLimitsSchema.safeParse(JSON.parse(value))
    return result.success ? result.data : null
  } catch {
    return null
  }
}

const policies = [
  { key: 'global_api', label: 'Global API requests (per IP)' },
  { key: 'login_ip', label: 'Login attempts (per IP)' },
  { key: 'login_account', label: 'Password login attempts (per account)' },
  { key: 'session_refresh', label: 'Session refresh (per account)' },
  { key: 'token_query', label: 'Token usage and logs (per account)' },
  { key: 'sensitive', label: 'Other sensitive actions' },
] as const

export function ManagementRateLimitSection({ value }: { value: string }) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const baselineValue = useRef(value)
  const lastServerValue = useRef(value)
  const [conflict, setConflict] = useState<{
    value: string
    limits: ManagementRateLimits
  } | null>(null)
  const defaults = useMemo(() => parseManagementRateLimits(value), [value])
  const latestSaved = useRef({ value, limits: defaults })
  const form = useForm<ManagementRateLimits>({
    resolver: zodResolver(managementRateLimitsSchema, {
      error: (issue) => {
        const property = issue.path?.at(-1)
        if (property !== 'requests' && property !== 'window_seconds') return
        return t('Enter a whole number between 1 and {{max}}', {
          max: maxRateLimitValue,
        })
      },
    }),
    defaultValues: defaults ?? undefined,
    mode: 'onChange',
  })
  const isDirty = form.formState.isDirty

  useEffect(() => {
    if (!defaults || lastServerValue.current === value) return
    lastServerValue.current = value
    latestSaved.current = { value, limits: defaults }
    const baseline = parseManagementRateLimits(baselineValue.current)
    const hasEdits =
      baseline !== null &&
      JSON.stringify(form.getValues()) !== JSON.stringify(baseline)
    if (!hasEdits) {
      baselineValue.current = value
      form.reset(defaults)
    }
  }, [defaults, form, isDirty, value])

  if (!defaults) {
    return <ErrorState title={t('Unable to load management rate limits')} />
  }

  const onSubmit = (values: ManagementRateLimits) => {
    updateOption.mutate(
      {
        key: 'ManagementRateLimits',
        value: JSON.stringify(values),
        expected_value: baselineValue.current,
      },
      {
        onSuccess: (response) => {
          const data = response.data
          baselineValue.current =
            data &&
            typeof data === 'object' &&
            'value' in data &&
            typeof data.value === 'string'
              ? data.value
              : JSON.stringify(values)
          latestSaved.current = { value: baselineValue.current, limits: values }
          setConflict(null)
          form.reset(values)
        },
        onError: (error) => {
          if (!axios.isAxiosError(error) || error.response?.status !== 409)
            return
          const data: unknown = error.response.data
          if (
            !data ||
            typeof data !== 'object' ||
            !('code' in data) ||
            data.code !== 'MANAGEMENT_RATE_LIMITS_CONFLICT' ||
            !('data' in data)
          )
            return
          const current = data.data
          if (
            !current ||
            typeof current !== 'object' ||
            !('value' in current) ||
            typeof current.value !== 'string'
          )
            return
          const limits = parseManagementRateLimits(current.value)
          if (limits) {
            latestSaved.current = { value: current.value, limits }
            setConflict({ value: current.value, limits })
          }
        },
      }
    )
  }

  return (
    <SettingsSection title={t('Management API rate limits')}>
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)}>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={updateOption.isPending}
            isSaveDisabled={!isDirty || conflict !== null}
            resetLabel='Reload saved limits'
            onReset={() => {
              const saved = conflict ?? latestSaved.current
              baselineValue.current = saved.value
              form.reset(saved.limits ?? defaults)
              setConflict(null)
            }}
          />
          {conflict !== null && (
            <p role='alert' className='text-destructive text-sm'>
              {t(
                'Rate limits changed elsewhere. Reload saved limits before saving again.'
              )}
            </p>
          )}
          <p className='text-muted-foreground text-sm'>
            {t(
              'Saved limits take effect without restarting. Requests also remain subject to the global IP limit.'
            )}
          </p>
          {policies.map(({ key, label }) => (
            <fieldset
              key={key}
              aria-label={t(label)}
              className='space-y-3 border-b pb-5 last:border-b-0'
            >
              <FormField
                control={form.control}
                name={`${key}.enabled`}
                render={({ field }) => (
                  <SettingsSwitchItem>
                    <SettingsSwitchContent>
                      <FormLabel>{t(label)}</FormLabel>
                      {key === 'token_query' && (
                        <FormDescription>
                          {t(
                            'All API keys belonging to the same account share the query limit.'
                          )}
                        </FormDescription>
                      )}
                    </SettingsSwitchContent>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                        disabled={updateOption.isPending}
                      />
                    </FormControl>
                  </SettingsSwitchItem>
                )}
              />
              <div className='grid gap-4 sm:grid-cols-2'>
                {(['requests', 'window_seconds'] as const).map((property) => (
                  <FormField
                    key={property}
                    control={form.control}
                    name={`${key}.${property}`}
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>
                          {property === 'requests'
                            ? t('Max requests per period')
                            : t('Window (seconds)')}
                        </FormLabel>
                        <FormControl>
                          <Input
                            {...field}
                            value={
                              Number.isFinite(field.value) ? field.value : ''
                            }
                            type='number'
                            min={1}
                            max={maxRateLimitValue}
                            step={1}
                            disabled={
                              !form.watch(`${key}.enabled`) ||
                              updateOption.isPending
                            }
                            onChange={(event) =>
                              field.onChange(event.target.valueAsNumber)
                            }
                          />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                ))}
              </div>
            </fieldset>
          ))}
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
