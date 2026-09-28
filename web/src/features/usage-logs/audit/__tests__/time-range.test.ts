/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { afterEach, describe, expect, test, vi } from 'vitest'

import { getDefaultAuditTimeRange } from '../api'

describe('audit log default time range', () => {
  afterEach(() => {
    vi.useRealTimers()
  })

  test('covers the current local calendar day through now', () => {
    const now = new Date(2026, 8, 28, 15, 42, 30, 123)
    vi.useFakeTimers()
    vi.setSystemTime(now)

    const range = getDefaultAuditTimeRange()
    const start = new Date(2026, 8, 28, 0, 0, 0, 0)

    expect(range.start_timestamp).toBe(Math.floor(start.getTime() / 1000))
    expect(range.end_timestamp).toBe(Math.floor(now.getTime() / 1000))
  })
})
