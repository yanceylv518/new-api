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
import { CircleAlert, CircleX, Info, type LucideIcon } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { cn } from '@/lib/utils'

import type { RequestTimelineResult } from '../api'
import { formatTraceDuration } from '../lib/timeline-scale'
import { getFailureText, getMissingText } from '../lib/timeline-text'

type IssueTone = 'failed' | 'warning' | 'info'

interface TraceIssue {
  key: string
  tone: IssueTone
  text: string
  code?: string
}

const TONE_ICON: Record<IssueTone, { icon: LucideIcon; className: string }> = {
  failed: { icon: CircleX, className: 'text-destructive' },
  warning: { icon: CircleAlert, className: 'text-warning' },
  info: { icon: Info, className: 'text-muted-foreground' },
}

/** 把 errors、取消、写出失败和缺失原因集中列出；普通用户只看到概括说明。 */
export function TimelineIssues(props: {
  result: RequestTimelineResult
  admin: boolean
}) {
  const { t } = useTranslation()
  const summary = props.result.summary
  const data = props.result.detail?.timeline
  const issues: TraceIssue[] = []
  if (props.admin && data) {
    if ((data.measurement_version ?? 1) < 2) {
      issues.push({
        key: 'legacy-measurement',
        tone: 'info',
        text: t(
          'This historical trace uses earlier measurement boundaries. Attempt ends or cancellation times may have been filled at handler return; the original record is preserved.'
        ),
      })
    }
    data.errors.forEach((failure, index) => {
      issues.push({
        key: `error-${index}`,
        tone: 'failed',
        text: getFailureText(t, failure.stage, failure.code),
        code: `${failure.code} · +${formatTraceDuration(failure.offset_us)}`,
      })
    })
    if (data.response.cancel_observed_us !== null) {
      issues.push({
        key: 'cancel',
        tone: 'warning',
        text: t(
          'Request context cancellation was observed at +{{time}}. This is when the gateway noticed cancellation.',
          { time: formatTraceDuration(data.response.cancel_observed_us) }
        ),
      })
    }
    if (data.response.write_failed) {
      issues.push({
        key: 'write',
        tone: 'failed',
        text: t('Writing to the client failed.'),
      })
    }
    for (const reason of data.capture.missing) {
      issues.push({
        key: `missing-${reason}`,
        tone: 'info',
        text: getMissingText(t, reason),
        code: reason,
      })
    }
    if (data.capture.truncated) {
      issues.push({
        key: 'truncated',
        tone: 'info',
        text: t(
          'Stage or event limits were reached; later records were dropped.'
        ),
      })
    }
  } else if (summary) {
    if (summary.outcome === 'canceled') {
      issues.push({
        key: 'cancel',
        tone: 'warning',
        text: t('The client canceled the request before it finished.'),
      })
    }
    if (summary.capture_status !== 'complete') {
      issues.push({
        key: 'partial',
        tone: 'info',
        text: t(
          'Some trace details were not observed or were truncated. Missing times are not zero.'
        ),
      })
    }
  }
  if (issues.length === 0) return null
  const failed = issues.some((issue) => issue.tone === 'failed')

  return (
    <Alert variant={failed ? 'destructive' : 'default'}>
      <AlertDescription>
        <ul className='space-y-1.5' aria-label={t('Trace issues')}>
          {issues.map((issue) => {
            const tone = TONE_ICON[issue.tone]
            return (
              <li key={issue.key} className='flex items-start gap-2'>
                <tone.icon
                  className={cn('mt-0.5 size-4 shrink-0', tone.className)}
                  aria-hidden='true'
                />
                <span className='text-foreground min-w-0 text-sm'>
                  {issue.text}
                  {issue.code && (
                    <code className='text-muted-foreground ml-2 font-mono text-xs'>
                      {issue.code}
                    </code>
                  )}
                </span>
              </li>
            )
          })}
        </ul>
      </AlertDescription>
    </Alert>
  )
}
