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

import {
  CHANNEL_TYPE_OPTIONS,
  CHANNEL_TYPE_TASK_PLUGIN,
  CHANNEL_TYPE_ZHIPU_WEB_SEARCH,
  channelTypeOptionsForTaskPluginBind,
} from '../../constants'
import { getChannelTypeConfig } from '../channel-type-config'

describe('channel type options for task plugin bind', () => {
  test('offers a separate web search channel with search engines when task plugin binding is unavailable', () => {
    const options = channelTypeOptionsForTaskPluginBind(false)

    expect(
      options.find((option) => option.value === CHANNEL_TYPE_ZHIPU_WEB_SEARCH)
    ).toEqual({
      value: CHANNEL_TYPE_ZHIPU_WEB_SEARCH,
      label: 'Zhipu Web Search',
    })
    expect(
      getChannelTypeConfig(CHANNEL_TYPE_ZHIPU_WEB_SEARCH).supportedModels
    ).toEqual([
      'search_std',
      'search_pro',
      'search_pro_sogou',
      'search_pro_quark',
    ])
  })

  test('hides the task plugin type when the caller cannot bind', () => {
    const options = channelTypeOptionsForTaskPluginBind(false)

    expect(
      options.some((option) => option.value === CHANNEL_TYPE_TASK_PLUGIN)
    ).toBe(false)
  })

  test('shows the task plugin type when the caller can bind', () => {
    const options = channelTypeOptionsForTaskPluginBind(true)

    expect(options).toEqual(CHANNEL_TYPE_OPTIONS)
    expect(
      options.some((option) => option.value === CHANNEL_TYPE_TASK_PLUGIN)
    ).toBe(true)
  })
})
