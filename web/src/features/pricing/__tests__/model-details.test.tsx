/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'

import { ModelDetailsContent } from '../components/model-details'
import type { PricingModel } from '../types'

vi.mock('@/features/performance-metrics/api', () => ({
  getPerfMetrics: vi.fn().mockResolvedValue({ data: { groups: [] } }),
}))
vi.mock('@/lib/lobe-icon', () => ({
  getLobeIcon: vi.fn(() => null),
}))

describe('model details overview', () => {
  it('shows the overview without performance and API tabs', () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const model: PricingModel = {
      id: 1,
      model_name: 'test-model',
      quota_type: 0,
      model_ratio: 1,
      completion_ratio: 1,
      enable_groups: ['default'],
    }

    render(
      <QueryClientProvider client={queryClient}>
        <ModelDetailsContent
          model={model}
          groupRatio={{ default: 1 }}
          usableGroup={{ default: { desc: 'Default group', ratio: 1 } }}
          autoGroups={[]}
          priceRate={1}
          usdExchangeRate={1}
          tokenUnit='M'
        />
      </QueryClientProvider>
    )

    expect(screen.queryByRole('tab')).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'test-model' })).toBeVisible()
    expect(screen.getByRole('heading', { name: 'Pricing' })).toBeVisible()
  })
})
