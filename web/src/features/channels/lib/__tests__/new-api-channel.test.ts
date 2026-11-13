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
import { describe, expect, test } from 'vitest'

import type { TaskPluginOption } from '../../api'
import {
  CHANNEL_TYPE_NEW_API,
  CHANNEL_TYPE_TASK_PLUGIN,
  CHANNEL_TYPE_VLLM,
  CHANNEL_TYPE_SGLANG,
  CHANNEL_TYPE_OPTIONS,
  MODEL_FETCHABLE_TYPES,
} from '../../constants'
import { channelSchema } from '../../types'
import {
  CHANNEL_FORM_DEFAULT_VALUES,
  channelFormSchema,
  transformChannelToFormDefaults,
  transformFormDataToCreatePayload,
  transformFormDataToUpdatePayload,
} from '../channel-form'
import {
  getChannelPluginExtensions,
  readTaskExtendPluginKeys,
  supportsNewAPIUpstream,
} from '../channel-plugin-extensions'
import { getChannelTypeConfig } from '../channel-type-config'
import { getChannelTypeIcon, getKeyPromptForType } from '../channel-utils'
import { videoRoutingModels } from '../video-routing'

function newAPIForm(baseUrl: string) {
  return {
    ...CHANNEL_FORM_DEFAULT_VALUES,
    name: 'New API upstream',
    type: CHANNEL_TYPE_NEW_API,
    base_url: baseUrl,
    key: 'test-key',
    models: 'gpt-5',
  }
}

describe('Channel settings patches and video routing', () => {
  const original = channelSchema.parse({
    id: 90,
    name: 'Video',
    type: CHANNEL_TYPE_TASK_PLUGIN,
    key: '',
    status: 1,
    created_time: 1,
    test_time: 0,
    response_time: 0,
    balance_updated_time: 0,
    group: 'vip',
    models: 'video-alias',
    base_url: 'https://provider.example/video',
    model_mapping: '{"video-alias":"doubao-seedance-2-0-260128"}',
    setting:
      '{"task_plugin_key":"doubao","proxy":"https://proxy.example","future":{"large":9007199254740993},"seedance_asset_url":"https://provider.example/assets","seedance_asset_share_group":"shared"}',
    settings: '{"future":{"enabled":true},"allow_service_tier":true}',
  })
  test('legacy null settings remain untouched until a setting is edited', () => {
    const legacy = {
      ...original,
      type: CHANNEL_TYPE_NEW_API,
      setting: 'null',
      settings: 'null',
    }
    const form = transformChannelToFormDefaults(legacy)
    const unchanged = transformFormDataToUpdatePayload(form, legacy.id, legacy)
    expect(unchanged).not.toHaveProperty('setting_patch')
    expect(unchanged).not.toHaveProperty('settings_patch')
    form.proxy = 'https://proxy.example'
    const changed = transformFormDataToUpdatePayload(form, legacy.id, legacy)
    expect(changed.setting_patch).toEqual({ proxy: 'https://proxy.example' })
    expect(changed.setting_base).toBe('null')
  })
  test('ordinary edits omit settings instead of rebuilding them', () => {
    const form = transformChannelToFormDefaults(original)
    form.name = 'Renamed'
    const update = transformFormDataToUpdatePayload(form, original.id, original)
    expect(update).not.toHaveProperty('setting')
    expect(update).not.toHaveProperty('settings')
    expect(update).not.toHaveProperty('setting_patch')
    expect(update).not.toHaveProperty('settings_patch')
  })
  test('routing edits patch only the rule, with the exact configuration baseline', () => {
    const form = transformChannelToFormDefaults(original)
    form.video_routing_rules = [
      {
        model: 'doubao-seedance-2-0-260128',
        mode: 'deny',
        resolutions: ['4k'],
      },
    ]
    const update = transformFormDataToUpdatePayload(form, original.id, original)
    expect(update.setting_patch).toEqual({
      video_routing_rules: form.video_routing_rules,
    })
    expect(update.setting_base).toBe(original.setting)
    expect(update.setting_base).toContain('9007199254740993')
    expect(update).not.toHaveProperty('settings_patch')
    form.proxy = ''
    expect(
      transformFormDataToUpdatePayload(form, original.id, original)
        .setting_patch
    ).toMatchObject({ proxy: '' })
  })
  test('clearing rules sends an explicit deletion while preserving other settings', () => {
    const configured = {
      ...original,
      setting: JSON.stringify({
        ...JSON.parse(original.setting ?? '{}'),
        video_routing_rules: [
          { model: 'video-alias', mode: 'allow', resolutions: ['4k'] },
        ],
      }),
    }
    const form = transformChannelToFormDefaults(configured)
    form.video_routing_rules = []
    expect(
      transformFormDataToUpdatePayload(form, configured.id, configured)
        .setting_patch
    ).toEqual({ video_routing_rules: null })
  })
  test('offers alias and canonical video models while excluding images', () => {
    expect(
      videoRoutingModels(
        ['video-alias', 'doubao-seedream-5-0-pro-260628'],
        original.model_mapping ?? undefined
      )
    ).toEqual(['video-alias', 'doubao-seedance-2-0-260128'])
  })
  test('rejects duplicate rules and invalid JSON without replacing existing configuration', () => {
    const form = transformChannelToFormDefaults(original)
    const rule = {
      model: 'video-alias',
      mode: 'deny' as const,
      resolutions: ['4k' as const],
    }
    expect(
      channelFormSchema.safeParse({
        ...form,
        video_routing_rules: [rule, rule],
      }).success
    ).toBe(false)
    expect(() =>
      transformFormDataToUpdatePayload(
        { ...form, setting: '{"broken":' },
        original.id,
        original
      )
    ).toThrow()
    expect(original.setting).toContain('9007199254740993')
  })
})

describe('New API channel', () => {
  test('saves a complete asset endpoint without changing the video base URL', () => {
    const form = channelFormSchema.parse({
      ...newAPIForm('https://provider.example/doubao'),
      type: CHANNEL_TYPE_TASK_PLUGIN,
      task_plugin_key: 'doubao',
      seedance_asset_url: 'https://provider.example/api/ark/assets',
      seedance_asset_share_group: 'shared-account',
    })
    const payload = transformFormDataToCreatePayload(form)
    expect(payload.channel.base_url).toBe('https://provider.example/doubao')
    expect(JSON.parse(payload.channel.setting ?? '{}').seedance_asset_url).toBe(
      'https://provider.example/api/ark/assets'
    )
    expect(
      JSON.parse(payload.channel.setting ?? '{}').seedance_asset_share_group
    ).toBe('shared-account')
    const saved = channelSchema.parse({
      id: 8,
      name: 'Video',
      type: CHANNEL_TYPE_TASK_PLUGIN,
      key: '',
      status: 1,
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      models: 'seedance',
      group: 'default',
      base_url: 'https://provider.example/doubao',
      setting: payload.channel.setting,
    })
    expect(
      transformChannelToFormDefaults(saved).seedance_asset_share_group
    ).toBe('shared-account')
    const seedance = channelFormSchema.parse({
      ...form,
      task_plugin_key: 'anyllm',
      base_url: 'https://provider.example/api/v1',
      seedance_asset_url: 'https://provider.example/api/material',
    })
    const seedancePayload = transformFormDataToCreatePayload(seedance)
    expect(seedancePayload.channel.base_url).toBe(
      'https://provider.example/api/v1'
    )
    expect(
      JSON.parse(seedancePayload.channel.setting ?? '{}').seedance_asset_url
    ).toBe('https://provider.example/api/material')
    const otherPlugin = channelFormSchema.parse({
      ...form,
      task_plugin_key: 'vidu',
      seedance_asset_url: '/not-a-material-endpoint',
    })
    expect(
      JSON.parse(
        transformFormDataToCreatePayload(otherPlugin).channel.setting ?? '{}'
      )
    ).not.toHaveProperty('seedance_asset_url')
    expect(
      JSON.parse(
        transformFormDataToCreatePayload(otherPlugin).channel.setting ?? '{}'
      )
    ).not.toHaveProperty('seedance_asset_share_group')
    for (const invalid of [
      '/v2/assets',
      'https://user:secret@example.com/',
      'https://example.com/?Action=x',
    ]) {
      expect(
        channelFormSchema.safeParse({ ...form, seedance_asset_url: invalid })
          .success
      ).toBe(false)
    }
    for (const invalid of ['a'.repeat(65), 'account\nother']) {
      expect(
        channelFormSchema.safeParse({
          ...form,
          seedance_asset_share_group: invalid,
        }).success
      ).toBe(false)
    }
    expect(
      channelFormSchema.safeParse({ ...form, seedance_asset_url: '' }).success
    ).toBe(false)
  })
  test('registers selection, ordering, model discovery, and icon metadata', () => {
    const option = CHANNEL_TYPE_OPTIONS.find(
      (item) => item.value === CHANNEL_TYPE_NEW_API
    )

    expect(option).toEqual({
      value: CHANNEL_TYPE_NEW_API,
      label: 'New API',
    })
    expect(
      CHANNEL_TYPE_OPTIONS.findIndex(
        (item) => item.value === CHANNEL_TYPE_NEW_API
      ) + 1
    ).toBe(CHANNEL_TYPE_OPTIONS.findIndex((item) => item.value === 58))
    expect(MODEL_FETCHABLE_TYPES.has(CHANNEL_TYPE_NEW_API)).toBe(true)
    expect(getChannelTypeIcon(CHANNEL_TYPE_NEW_API)).toBe('NewAPI')
    expect(getKeyPromptForType(CHANNEL_TYPE_NEW_API)).toBe(
      'Enter API key for this channel'
    )
    expect(getChannelTypeConfig(CHANNEL_TYPE_NEW_API).icon).toBe('NewAPI')
  })

  test('requires a non-blank Base URL', () => {
    const blankResult = channelFormSchema.safeParse(newAPIForm('  '))

    expect(blankResult.success).toBe(false)
    if (!blankResult.success) {
      expect(
        blankResult.error.issues.some(
          (issue) =>
            issue.path[0] === 'base_url' &&
            issue.message === 'Base URL is required for this channel type'
        )
      ).toBe(true)
    }

    expect(
      channelFormSchema.safeParse(newAPIForm('https://new-api.example')).success
    ).toBe(true)
  })

  test('keeps Sub2API Base URL validation unchanged', () => {
    const result = channelFormSchema.safeParse({
      ...newAPIForm(''),
      type: 59,
    })

    expect(result.success).toBe(true)
  })
})

describe.each([
  { type: CHANNEL_TYPE_VLLM, name: 'vLLM', icon: 'Vllm' },
  { type: CHANNEL_TYPE_SGLANG, name: 'SGLang', icon: 'SGLang' },
])('$name channel', ({ type, name, icon }) => {
  test('can be selected and discover served models', () => {
    expect(CHANNEL_TYPE_OPTIONS).toContainEqual({
      value: type,
      label: name,
    })
    expect(MODEL_FETCHABLE_TYPES.has(type)).toBe(true)
    expect(getChannelTypeIcon(type)).toBe(icon)
    expect(getChannelTypeConfig(type).icon).toBe(icon)
    expect(getKeyPromptForType(type)).toBe(
      `${name} API key, or EMPTY if authentication is disabled`
    )
  })

  test('requires an upstream address and submits the served model name', () => {
    const form = {
      ...newAPIForm(''),
      type,
      models: 'deepseek-v4-flash-vision-exp',
      key: 'EMPTY',
    }
    const blank = channelFormSchema.safeParse(form)
    expect(blank.success).toBe(false)
    if (!blank.success) {
      expect(blank.error.issues).toEqual(
        expect.arrayContaining([
          expect.objectContaining({
            path: ['base_url'],
            message: 'Base URL is required for this channel type',
          }),
        ])
      )
    }
    const parsed = channelFormSchema.parse({
      ...form,
      base_url: 'http://vllm:8000/',
    })
    const payload = transformFormDataToCreatePayload(parsed)
    expect(payload.channel).toMatchObject({
      type,
      base_url: 'http://vllm:8000',
      models: 'deepseek-v4-flash-vision-exp',
      key: 'EMPTY',
    })
  })
})

describe('New API channel task plugin extensions', () => {
  const plugins: TaskPluginOption[] = [
    {
      key: 'doubao',
      name: 'Doubao',
      models: ['seedance'],
      channelTypes: [45],
      upstreams: ['vendor', 'new_api'],
    },
    {
      key: 'kling',
      name: 'Kling',
      models: ['kling-v2'],
      upstreams: ['new_api'],
    },
    {
      key: 'hailuo',
      name: 'Hailuo',
      models: ['minimax'],
      upstreams: ['vendor'],
    },
  ]

  test('only plugins declaring a New API upstream can be bound to a New API channel', () => {
    expect(plugins.filter(supportsNewAPIUpstream).map((p) => p.key)).toEqual([
      'doubao',
      'kling',
    ])
    expect(supportsNewAPIUpstream({ upstreams: null })).toBe(false)
    expect(supportsNewAPIUpstream({})).toBe(false)
  })

  test('reads the extension list together with a single key on New API channels only', () => {
    expect(
      readTaskExtendPluginKeys(CHANNEL_TYPE_NEW_API, {
        task_plugin_key: 'kling',
        task_extend_plugin_keys: ['doubao', 'kling', '', 'doubao'],
      })
    ).toEqual(['kling', 'doubao'])
    expect(readTaskExtendPluginKeys(CHANNEL_TYPE_NEW_API, {})).toEqual([])
    expect(
      readTaskExtendPluginKeys(1, { task_extend_plugin_keys: ['doubao'] })
    ).toEqual([])
  })

  test('lists only bound plugins as extensions while other types keep declared channel types', () => {
    expect(
      getChannelPluginExtensions(CHANNEL_TYPE_NEW_API, plugins, ['kling'])
    ).toEqual([plugins[1]])
    expect(getChannelPluginExtensions(CHANNEL_TYPE_NEW_API, plugins)).toEqual(
      []
    )
    expect(getChannelPluginExtensions(45, plugins, ['kling'])).toEqual([
      plugins[0],
    ])
  })

  test('round-trips bound plugins through the channel setting for New API channels only', () => {
    const channel = channelSchema.parse({
      id: 7,
      name: 'Gateway',
      type: CHANNEL_TYPE_NEW_API,
      key: '',
      status: 1,
      created_time: 1,
      test_time: 0,
      response_time: 0,
      balance_updated_time: 0,
      models: 'seedance',
      group: 'default',
      base_url: 'https://gateway.example',
      setting: JSON.stringify({
        task_plugin_key: 'kling',
        task_extend_plugin_keys: ['doubao'],
      }),
    })
    expect(
      transformChannelToFormDefaults(channel).task_extend_plugin_keys
    ).toEqual(['kling', 'doubao'])

    const gateway = transformFormDataToCreatePayload(
      channelFormSchema.parse({
        ...newAPIForm('https://gateway.example'),
        task_extend_plugin_keys: ['doubao', 'kling'],
      })
    )
    const gatewaySetting = JSON.parse(gateway.channel.setting ?? '{}')
    expect(gatewaySetting).toMatchObject({
      task_extend_plugin_keys: ['doubao', 'kling'],
    })
    expect(gatewaySetting).not.toHaveProperty('task_plugin_key')

    const openai = transformFormDataToCreatePayload(
      channelFormSchema.parse({
        ...newAPIForm('https://gateway.example'),
        type: 1,
        task_extend_plugin_keys: ['doubao'],
      })
    )
    expect(JSON.parse(openai.channel.setting ?? '{}')).not.toHaveProperty(
      'task_extend_plugin_keys'
    )
  })
})
