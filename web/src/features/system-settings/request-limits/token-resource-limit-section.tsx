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
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useEffect } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

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
import { Switch } from '@/components/ui/switch'
import { handleServerError } from '@/lib/handle-server-error'
import {
  formatRequestBodyMB,
  parseRequestBodyMB,
} from '@/lib/request-body-size'

import { updateSystemOption } from '../api'
import { SettingsForm } from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'

type Options = {
  TokenResourceLimitsEnabled: boolean
  TokenResourceDefaultMaxRequestBodyBytes: number
  TokenResourceDefaultMaxConcurrentRequests: number
}

export function TokenResourceLimitSection(props: { defaultValues: Options }) {
  const { t } = useTranslation()
  const schema = z.object({
    TokenResourceLimitsEnabled: z.boolean(),
    TokenResourceDefaultMaxRequestBodyBytes: z
      .string()
      .refine(
        (value) => parseRequestBodyMB(value) !== null,
        t(
          'Enter a valid non-negative size in MB (at least 1 byte when positive).'
        )
      ),
    TokenResourceDefaultMaxConcurrentRequests: z
      .number()
      .int()
      .min(0)
      .max(2147483647),
  })
  type Values = z.infer<typeof schema>
  const client = useQueryClient()
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: {
      ...props.defaultValues,
      TokenResourceDefaultMaxRequestBodyBytes: formatRequestBodyMB(
        props.defaultValues.TokenResourceDefaultMaxRequestBodyBytes
      ),
    },
  })
  useEffect(() => {
    form.reset({
      ...props.defaultValues,
      TokenResourceDefaultMaxRequestBodyBytes: formatRequestBodyMB(
        props.defaultValues.TokenResourceDefaultMaxRequestBodyBytes
      ),
    })
  }, [props.defaultValues, form])
  const save = useMutation({
    mutationFn: async (values: Values) => {
      const bodyBytes = parseRequestBodyMB(
        values.TokenResourceDefaultMaxRequestBodyBytes
      )
      if (bodyBytes === null) return
      const options: Options = {
        ...values,
        TokenResourceDefaultMaxRequestBodyBytes: bodyBytes,
      }
      // Disable first, or enable last, so failed default writes cannot enable
      // the feature with only part of the requested configuration.
      const keys: (keyof Values)[] = [
        'TokenResourceDefaultMaxRequestBodyBytes',
        'TokenResourceDefaultMaxConcurrentRequests',
      ]
      if (values.TokenResourceLimitsEnabled) {
        keys.push('TokenResourceLimitsEnabled')
      } else {
        keys.unshift('TokenResourceLimitsEnabled')
      }
      for (const key of keys) {
        if (options[key] === props.defaultValues[key]) continue
        const result = await updateSystemOption({ key, value: options[key] })
        if (!result.success) {
          throw new Error(result.message || t('Failed to update setting'))
        }
      }
      form.reset(values)
      toast.success(t('Setting updated successfully'))
    },
    onError: handleServerError,
    onSettled: () => client.invalidateQueries({ queryKey: ['system-options'] }),
  })
  return (
    <SettingsSection title={t('Key resource limits')}>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Applies to API requests. Zero means unlimited. Per-key overrides are managed in Users. Concurrency limits require shared Redis.'
        )}
      </p>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Enable only after restricted traffic is routed to upgraded instances. Older versions do not enforce these limits.'
        )}
      </p>
      <Form {...form}>
        <SettingsForm
          onSubmit={form.handleSubmit((values) => save.mutate(values))}
        >
          <SettingsPageFormActions
            onSave={form.handleSubmit((values) => save.mutate(values))}
            isSaving={save.isPending}
            saveLabel='Save key resource limits'
          />
          <FieldGroup>
            <FormField
              control={form.control}
              name='TokenResourceLimitsEnabled'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Enable key resource limits')}</FormLabel>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                      disabled={save.isPending}
                    />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='TokenResourceDefaultMaxRequestBodyBytes'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Default request body limit (MB)')}</FormLabel>
                  <FormControl>
                    <Input
                      inputMode='decimal'
                      {...field}
                      disabled={save.isPending}
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
              name='TokenResourceDefaultMaxConcurrentRequests'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>
                    {t('Default concurrent requests per key')}
                  </FormLabel>
                  <FormControl>
                    <Input
                      type='number'
                      min={0}
                      max={2147483647}
                      step={1}
                      {...field}
                      value={
                        field.value === 0 || !Number.isFinite(field.value)
                          ? ''
                          : field.value
                      }
                      placeholder={t('Unlimited')}
                      onChange={(e) =>
                        field.onChange(
                          e.target.value === '' ? 0 : Number(e.target.value)
                        )
                      }
                      disabled={save.isPending}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'Leave blank or enter 0 for no default concurrency limit.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
          </FieldGroup>
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
