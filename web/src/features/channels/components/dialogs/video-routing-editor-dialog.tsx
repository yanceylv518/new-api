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
import { Plus, Trash2 } from 'lucide-react'
import { useId, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Combobox } from '@/components/ui/combobox'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import type { VideoRoutingRule } from '../../types'

type Props = {
  models: string[]
  value: VideoRoutingRule[]
  onChange: (rules: VideoRoutingRule[]) => void
  onOpenChange: (open: boolean) => void
}

const RESOLUTIONS: VideoRoutingRule['resolutions'] = [
  '480p',
  '720p',
  '1080p',
  '4k',
]

export function VideoRoutingEditorDialog(props: Props) {
  const { t } = useTranslation()
  const id = useId()
  const [rules, setRules] = useState(() =>
    props.value.map((rule, rowId) => ({
      ...rule,
      rowId,
      resolutions: [...rule.resolutions],
    }))
  )
  const nextRowId = useRef(props.value.length)
  const [error, setError] = useState(false)
  const options = [
    ...new Set([...props.models, ...props.value.map((rule) => rule.model)]),
  ].map((value) => ({ value, label: value }))

  const update = (index: number, patch: Partial<VideoRoutingRule>) => {
    setRules((previous) =>
      previous.map((rule, position) =>
        position === index ? { ...rule, ...patch } : rule
      )
    )
    setError(false)
  }
  const apply = () => {
    const names = rules.map((rule) => rule.model.trim().toLowerCase())
    if (
      rules.some(
        (rule) => !rule.model.trim() || rule.resolutions.length === 0
      ) ||
      new Set(names).size !== rules.length
    ) {
      setError(true)
      return
    }
    props.onChange(
      rules.map(({ model, mode, resolutions }) => ({
        model,
        mode,
        resolutions,
      }))
    )
    props.onOpenChange(false)
  }

  return (
    <Dialog
      open
      onOpenChange={props.onOpenChange}
      title={t('Video routing rules')}
      description={t(
        'Filter this channel by video resolution. Model rules also apply to its aliases.'
      )}
      footer={
        <>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button type='button' onClick={apply}>
            {t('Apply')}
          </Button>
        </>
      }
    >
      <div className='space-y-4'>
        {rules.length === 0 && (
          <p className='text-muted-foreground text-sm'>
            {t('No video routing rules configured.')}
          </p>
        )}
        {rules.map((rule, index) => (
          <div key={rule.rowId} className='space-y-4 rounded-lg border p-4'>
            <div className='space-y-2'>
              <Label htmlFor={`${id}-${index}-model`}>{t('Model')}</Label>
              <div className='flex flex-col gap-3 sm:flex-row sm:items-center'>
                <div className='min-w-0 flex-1'>
                  <Combobox
                    id={`${id}-${index}-model`}
                    value={rule.model}
                    options={options}
                    onValueChange={(value) =>
                      update(index, { model: value ?? '' })
                    }
                    placeholder={t('Select model')}
                  />
                </div>
                <Button
                  type='button'
                  variant='outline'
                  onClick={() => {
                    setRules((previous) =>
                      previous.filter((_, position) => position !== index)
                    )
                    setError(false)
                  }}
                >
                  <Trash2 className='size-4' />
                  {t('Remove')}
                </Button>
              </div>
            </div>
            <div className='space-y-2'>
              <Label htmlFor={`${id}-${index}-mode`}>
                {t('Routing condition')}
              </Label>
              <Select
                items={[
                  {
                    value: 'allow',
                    label: t('Only use this channel for selected resolutions'),
                  },
                  {
                    value: 'deny',
                    label: t('Skip this channel for selected resolutions'),
                  },
                ]}
                value={rule.mode}
                onValueChange={(value) => {
                  if (value === 'allow' || value === 'deny') {
                    update(index, { mode: value })
                  }
                }}
              >
                <SelectTrigger id={`${id}-${index}-mode`} className='w-full'>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent align='start' alignItemWithTrigger={false}>
                  <SelectItem value='allow'>
                    {t('Only use this channel for selected resolutions')}
                  </SelectItem>
                  <SelectItem value='deny'>
                    {t('Skip this channel for selected resolutions')}
                  </SelectItem>
                </SelectContent>
              </Select>
            </div>
            <fieldset className='space-y-2'>
              <legend className='text-sm font-medium'>{t('Resolution')}</legend>
              <div className='flex flex-wrap gap-x-6 gap-y-3'>
                {RESOLUTIONS.map((resolution) => (
                  <Label
                    key={resolution}
                    htmlFor={`${id}-${index}-${resolution}`}
                    className='flex min-h-9 cursor-pointer items-center gap-2'
                  >
                    <Checkbox
                      id={`${id}-${index}-${resolution}`}
                      checked={rule.resolutions.includes(resolution)}
                      onCheckedChange={(checked) =>
                        update(index, {
                          resolutions: checked
                            ? [...rule.resolutions, resolution]
                            : rule.resolutions.filter(
                                (entry) => entry !== resolution
                              ),
                        })
                      }
                    />
                    {resolution === '4k' ? '4K' : resolution}
                  </Label>
                ))}
              </div>
            </fieldset>
            {rule.mode === 'allow' && (
              <p className='text-muted-foreground text-xs'>
                {t('Requests without a resolution do not use this channel.')}
              </p>
            )}
          </div>
        ))}
        <Button
          type='button'
          variant='outline'
          disabled={rules.length >= 32 || options.length === 0}
          onClick={() => {
            const rowId = nextRowId.current++
            setRules((previous) => [
              ...previous,
              {
                rowId,
                model: options[0]?.value || '',
                mode: 'deny',
                resolutions: ['4k'],
              },
            ])
          }}
        >
          <Plus className='size-4' />
          {t('Add rule')}
        </Button>
        {error && (
          <p role='alert' className='text-destructive text-sm'>
            {t(
              'Choose a model and resolution for each rule. Models must be unique.'
            )}
          </p>
        )}
      </div>
    </Dialog>
  )
}
