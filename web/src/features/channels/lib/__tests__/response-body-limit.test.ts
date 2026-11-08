/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { describe, expect, test } from 'vitest'

import { CHANNEL_TYPE_ZHIPU_WEB_SEARCH } from '../../constants'
import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  MAX_RESPONSE_BODY_MIB,
  buildSettingJSON,
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from '../channel-form'
import { hasAdvancedSettingsErrors } from '../channel-form-errors'

function searchForm(limit: number) {
  return {
    ...CHANNEL_FORM_DEFAULT_VALUES,
    name: 'Response limit test',
    type: CHANNEL_TYPE_ZHIPU_WEB_SEARCH,
    key: 'test-key',
    models: 'search_std',
    max_response_body_mib: limit,
  }
}

describe('search channel response body limit', () => {
  test('creates, restores, updates and clears the saved limit', () => {
    const created = transformFormDataToCreatePayload(searchForm(8)).channel
    expect(JSON.parse(created.setting || '{}').max_response_body_mib).toBe(8)

    const saved = channelSchema.parse({
      ...created,
      id: 1,
      status: 1,
      created_time: 0,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      used_quota: 0,
      other: '',
      remark: '',
    })
    const restored = transformChannelToFormDefaults(saved)
    expect(restored.max_response_body_mib).toBe(8)
    const updated = transformFormDataToUpdatePayload(
      { ...restored, max_response_body_mib: 16 },
      1
    )
    expect(JSON.parse(updated.setting || '{}').max_response_body_mib).toBe(16)
    const cleared = transformFormDataToUpdatePayload(
      { ...restored, max_response_body_mib: 0 },
      1
    )
    expect(JSON.parse(cleared.setting || '{}')).not.toHaveProperty(
      'max_response_body_mib'
    )
    expect(
      transformChannelToFormDefaults({ ...saved, setting: '{}' })
        .max_response_body_mib
    ).toBe(0)
  })

  test.each([-1, 1.5, Infinity, MAX_RESPONSE_BODY_MIB + 1])(
    'rejects invalid limit %s without changing the form value',
    (limit) => {
      const form = searchForm(limit)
      const result = channelFormSchema.safeParse(form)
      expect(result.success).toBe(false)
      if (!result.success) {
        expect(
          result.error.issues.some(
            (issue) => issue.path[0] === 'max_response_body_mib'
          )
        ).toBe(true)
      }
      expect(form.max_response_body_mib).toBe(limit)
    }
  )

  test('accepts zero and configurable positive integer limits', () => {
    for (const limit of [0, 1, 8, MAX_RESPONSE_BODY_MIB]) {
      expect(channelFormSchema.safeParse(searchForm(limit)).success).toBe(true)
    }
  })

  test('keeps the limit out of other channel settings', () => {
    expect(
      JSON.parse(buildSettingJSON({ ...searchForm(8), type: 60 }))
    ).not.toHaveProperty('max_response_body_mib')
  })

  test('exposes invalid limit errors through the advanced settings flow', () => {
    expect(
      hasAdvancedSettingsErrors({
        max_response_body_mib: { message: 'Invalid limit' },
      })
    ).toBe(true)
  })
})
