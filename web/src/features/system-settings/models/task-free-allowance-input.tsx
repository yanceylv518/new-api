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
import { useTranslation } from 'react-i18next'

import { Input } from '@/components/ui/input'

// 免费数量独立于货币单价，拒绝负数、小数和不安全整数，零值表示不减免。
export function TaskFreeAllowanceInput(props: {
  label: string
  value: number
  onChange: (value: number) => void
}) {
  const { t } = useTranslation()
  return (
    <label className='text-muted-foreground mt-2 flex flex-col gap-1 text-xs'>
      {t('Free quantity per task')}
      <Input
        type='number'
        min={0}
        max={Number.MAX_SAFE_INTEGER}
        step={1}
        aria-label={`${t('Free quantity per task')}: ${props.label}`}
        value={props.value}
        onChange={(event) => {
          const value = Number(event.target.value)
          if (Number.isSafeInteger(value) && value >= 0) props.onChange(value)
        }}
        className='min-w-28 font-mono'
      />
    </label>
  )
}
