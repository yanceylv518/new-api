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
*/
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { PricingTable } from '../components/pricing-table'
import type { PricingModel } from '../types'

describe('pricing table', () => {
  it('does not render the model access group column', () => {
    const model: PricingModel = {
      id: 1,
      model_name: 'example-model',
      quota_type: 0,
      model_ratio: 1,
      completion_ratio: 1,
      enable_groups: ['default', 'premium'],
      group_ratio: { default: 1, premium: 3 },
    }

    render(<PricingTable models={[model]} />)

    expect(screen.getByText('example-model')).toBeVisible()
    expect(screen.queryByText('Groups')).not.toBeInTheDocument()
    expect(screen.queryByText('default')).not.toBeInTheDocument()
    expect(screen.queryByText('premium')).not.toBeInTheDocument()
  })
})
