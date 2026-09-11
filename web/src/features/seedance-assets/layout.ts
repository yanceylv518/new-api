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
// 移动端分组区有明确上限，把主要高度留给素材和分页；两侧列表分别局部滚动。
export const seedanceAssetLayoutClasses = {
  page: 'flex min-h-0 w-full flex-1 flex-col overflow-hidden',
  content:
    'grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(0,11rem)_minmax(0,1fr)] overflow-hidden md:grid-cols-[250px_minmax(0,1fr)] md:grid-rows-1',
  workspace: 'flex min-h-0 min-w-0 flex-col overflow-hidden',
  uploadPanel: 'relative flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden',
  uploadQueue:
    'absolute right-4 bottom-4 z-30 flex max-w-[calc(100%-2rem)] flex-col items-end gap-2',
  uploadQueuePanel:
    'bg-background/95 w-80 max-w-full overflow-hidden rounded-lg border shadow-xl backdrop-blur-sm',
  library: 'flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden',
  groupSidebar:
    'bg-muted/20 flex min-h-0 min-w-0 flex-col gap-2 overflow-hidden border-b p-3 md:gap-4 md:border-r md:border-b-0 md:p-4',
  groupSidebarBody: 'flex min-h-0 flex-1 flex-col',
  groupSidebarScroll:
    'flex min-h-0 flex-1 flex-col gap-1 overflow-y-auto overscroll-contain',
  header: 'shrink-0 border-b px-4 py-4 sm:px-6',
  assetScroll:
    'min-h-0 flex-1 overflow-x-hidden overflow-y-auto overscroll-contain px-4 pb-5 sm:px-6',
} as const
