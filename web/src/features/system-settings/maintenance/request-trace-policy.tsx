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
import type { UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { DateTimePicker } from '@/components/datetime-picker'
import {
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'

import { SettingsControlGroup } from '../components/settings-form-layout'
import type { LogSettingsFormValues } from './request-trace-policy-schema'

export function RequestTracePolicyControls(props: {
  form: UseFormReturn<LogSettingsFormValues>
  disabled: boolean
}) {
  const { t } = useTranslation()
  return (
    <SettingsControlGroup>
      <FormField
        control={props.form.control}
        name='RequestTraceSuccessSampleRate'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Successful request sample rate (%)')}</FormLabel>
            <FormDescription>
              {t(
                '0 saves failures only; 100 saves every successful request. Failed retries and temporary diagnostics are always kept. Unsampled requests use lightweight error detection.'
              )}
            </FormDescription>
            <FormControl>
              <Input
                {...field}
                type='number'
                min={0}
                max={100}
                step={1}
                value={Number.isFinite(field.value) ? field.value : ''}
                onChange={(event) => field.onChange(event.target.valueAsNumber)}
                disabled={props.disabled}
              />
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={props.form.control}
        name='RequestTraceDiagnosticChannels'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Temporary diagnostic channel IDs')}</FormLabel>
            <FormDescription>
              {t(
                'Enter up to 16 channel IDs separated by commas. These channels keep detailed metadata until expiry. Tracing must be enabled.'
              )}
            </FormDescription>
            <FormControl>
              <Input
                {...field}
                placeholder='1, 2, 3'
                maxLength={300}
                disabled={props.disabled}
              />
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
      <FormField
        control={props.form.control}
        name='RequestTraceDiagnosticExpiresAt'
        render={({ field }) => (
          <FormItem>
            <FormLabel>{t('Diagnostic expiry')}</FormLabel>
            <FormDescription>
              {t(
                'Choose a future time within 24 hours. Expired diagnostics stop automatically.'
              )}
            </FormDescription>
            <FormControl>
              <DateTimePicker
                value={field.value ? new Date(field.value * 1000) : undefined}
                onChange={(date) =>
                  field.onChange(date ? Math.floor(date.getTime() / 1000) : 0)
                }
                disabled={props.disabled}
              />
            </FormControl>
            <FormMessage />
          </FormItem>
        )}
      />
    </SettingsControlGroup>
  )
}
