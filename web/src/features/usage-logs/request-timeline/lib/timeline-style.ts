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
import type { TimelineKind } from './timeline-model'

export const TIMELINE_KIND_BG: Record<TimelineKind, string> = {
  gateway: 'bg-info',
  upstream: 'bg-warning',
  response: 'bg-success',
  log: 'bg-muted-foreground/40',
  failed: 'bg-destructive',
}

export const TIMELINE_KIND_TEXT: Record<TimelineKind, string> = {
  gateway: 'text-info',
  upstream: 'text-warning',
  response: 'text-success',
  log: 'text-muted-foreground',
  failed: 'text-destructive',
}

export const HATCH =
  'bg-[image:repeating-linear-gradient(135deg,transparent_0_3px,rgb(255_255_255/0.45)_3px_5px)]'
