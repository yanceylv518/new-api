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
export function videoRoutingModels(
  models: string[],
  rawMapping: string | undefined
): string[] {
  let mapping: Record<string, unknown> = {}
  if (rawMapping?.trim()) {
    try {
      const parsed: unknown = JSON.parse(rawMapping)
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        mapping = parsed as Record<string, unknown>
      }
    } catch {
      return models.filter((model) =>
        model.toLowerCase().startsWith('doubao-seedance-')
      )
    }
  }
  const result = new Set<string>()
  for (const model of models) {
    let current = model
    const visited = new Set<string>()
    while (!visited.has(current)) {
      visited.add(current)
      if (current.toLowerCase().startsWith('doubao-seedance-')) {
        result.add(model)
        result.add(current)
      }
      const next = mapping[current]
      if (typeof next !== 'string' || !next) break
      current = next
    }
  }
  return [...result]
}
