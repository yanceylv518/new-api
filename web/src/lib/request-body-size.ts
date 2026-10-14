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
const bytesPerMB = 1024 * 1024

// Keep all fractional digits needed to round-trip existing byte policies.
export function formatRequestBodyMB(bytes: number): string {
  return (bytes / bytesPerMB).toFixed(20).replace(/\.?0+$/, '')
}

// Return null for invalid input, including positive values below one byte.
// Blank/inheritance is handled by the caller, separately from explicit zero.
export function parseRequestBodyMB(value: string): number | null {
  if (!/^(?:\d+(?:\.\d*)?|\.\d+)$/.test(value)) return null
  const bytes = Number(value) * bytesPerMB
  if (!Number.isFinite(bytes) || bytes > Number.MAX_SAFE_INTEGER) return null
  if (/[1-9]/.test(value) && bytes < 1) return null
  return Math.round(bytes)
}
