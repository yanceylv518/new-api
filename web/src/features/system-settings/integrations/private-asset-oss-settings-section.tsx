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
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { PasswordInput } from '@/components/password-input'
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

import { updatePrivateAssetOSSSettings } from '../api'
import { FormDirtyIndicator } from '../components/form-dirty-indicator'
import { FormNavigationGuard } from '../components/form-navigation-guard'
import { SettingsForm } from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import {
  createPrivateAssetOSSSchema,
  type PrivateAssetOSSFormValues,
} from './private-asset-oss-validation'

type PrivateAssetOSSSettingsSectionProps = {
  defaultValues: PrivateAssetOSSFormValues
  secretConfigured: boolean
}

export function PrivateAssetOSSSettingsSection(
  props: PrivateAssetOSSSettingsSectionProps
) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const mutation = useMutation({ mutationFn: updatePrivateAssetOSSSettings })
  const form = useForm<PrivateAssetOSSFormValues>({
    resolver: zodResolver(createPrivateAssetOSSSchema(t)),
    defaultValues: props.defaultValues,
  })

  const onSubmit = async (values: PrivateAssetOSSFormValues) => {
    const prefix = values.prefix.trim().replaceAll(/^\/+|\/+$/g, '')
    try {
      const response = await mutation.mutateAsync({
        region: values.region.trim(),
        endpoint: values.endpoint.trim().replace(/\/+$/, ''),
        bucket: values.bucket.trim(),
        prefix: prefix ? `${prefix}/` : 'private-assets/',
        access_key_id: values.accessKeyId.trim(),
        ...(values.accessKeySecret.trim()
          ? { access_key_secret: values.accessKeySecret.trim() }
          : {}),
      })
      if (!response.success) {
        throw new Error(response.message || t('Failed to update setting'))
      }
      await queryClient.invalidateQueries({ queryKey: ['system-options'] })
      toast.success(t('OSS settings saved'))
      form.reset({
        ...values,
        region: values.region.trim(),
        endpoint: values.endpoint.trim().replace(/\/+$/, ''),
        bucket: values.bucket.trim(),
        prefix: prefix ? `${prefix}/` : 'private-assets/',
        accessKeyId: values.accessKeyId.trim(),
        accessKeySecret: '',
      })
    } catch (error) {
      handleServerError(error)
    }
  }

  return (
    <>
      <FormNavigationGuard when={form.formState.isDirty} />
      <SettingsSection title={t('Private Asset OSS')}>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Store private library uploads in Alibaba Cloud OSS with temporary signed URLs.'
          )}
        </p>
        <Form {...form}>
          <SettingsForm
            onSubmit={form.handleSubmit(onSubmit)}
            autoComplete='off'
          >
            <SettingsPageFormActions
              onSave={form.handleSubmit(onSubmit)}
              onReset={() => form.reset(props.defaultValues)}
              isSaving={mutation.isPending}
              isSaveDisabled={!form.formState.isDirty}
              isResetDisabled={!form.formState.isDirty}
              saveLabel='Save OSS settings'
            />
            <FormDirtyIndicator isDirty={form.formState.isDirty} />

            <FormField
              control={form.control}
              name='region'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('OSS Region')}</FormLabel>
                  <FormControl>
                    <Input placeholder='cn-hangzhou' {...field} />
                  </FormControl>
                  <FormDescription>
                    {t('The Region ID where the bucket is located.')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='endpoint'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('OSS Endpoint')}</FormLabel>
                  <FormControl>
                    <Input
                      type='url'
                      inputMode='url'
                      placeholder='https://oss-cn-hangzhou.aliyuncs.com'
                      {...field}
                    />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'Leave blank to use the public endpoint resolved from the Region.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='bucket'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('OSS Bucket')}</FormLabel>
                  <FormControl>
                    <Input placeholder='private-assets' {...field} />
                  </FormControl>
                  <FormDescription>
                    {t('A private bucket used for private asset files.')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='prefix'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Object Prefix')}</FormLabel>
                  <FormControl>
                    <Input placeholder='private-assets/' {...field} />
                  </FormControl>
                  <FormDescription>
                    {t('Objects are stored under this prefix.')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='accessKeyId'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('AccessKey ID')}</FormLabel>
                  <FormControl>
                    <Input autoComplete='off' {...field} />
                  </FormControl>
                  <FormDescription>
                    {t(
                      'Use a RAM user with PutObject, GetObject, and DeleteObject permissions.'
                    )}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <FormField
              control={form.control}
              name='accessKeySecret'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('AccessKey Secret')}</FormLabel>
                  <FormControl>
                    <PasswordInput
                      autoComplete='new-password'
                      placeholder={
                        props.secretConfigured
                          ? t('Enter a new secret to replace it')
                          : t('Enter the AccessKey secret')
                      }
                      {...field}
                    />
                  </FormControl>
                  <FormDescription>
                    {props.secretConfigured
                      ? t('A secret is configured. Leave blank to keep it.')
                      : t('A secret is required before uploads can use OSS.')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />

            <p className='text-muted-foreground text-xs lg:col-span-2'>
              {t(
                'Region, endpoint, and bucket cannot be changed while OSS-backed assets exist.'
              )}
            </p>
          </SettingsForm>
        </Form>
      </SettingsSection>
    </>
  )
}
