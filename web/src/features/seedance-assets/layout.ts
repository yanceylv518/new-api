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
// 素材库让移动端上下两个工作区拥有确定高度，避免侧栏内容撑开后留下空白或挤掉素材区。
export const seedanceAssetLayoutClasses = {
  page: 'flex min-h-0 w-full flex-1 flex-col overflow-hidden',
  content:
    'grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(0,1fr)_minmax(0,1fr)] overflow-hidden md:grid-cols-[250px_minmax(0,1fr)] md:grid-rows-1',
  workspace: 'flex min-h-0 min-w-0 flex-col overflow-hidden',
  uploadPanel: 'relative flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden',
  library: 'flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden',
  groupSidebar:
    'bg-muted/20 flex min-h-0 min-w-0 flex-col gap-4 overflow-hidden border-b p-4 md:border-r md:border-b-0',
  groupSidebarBody: 'flex min-h-0 flex-1 flex-col',
  groupSidebarScroll:
    'flex min-h-0 flex-1 flex-col gap-1 overflow-y-auto overscroll-contain',
  header: 'shrink-0 border-b px-4 py-4 sm:px-6',
  assetScroll:
    'min-h-0 flex-1 overflow-x-hidden overflow-y-auto overscroll-contain px-4 pb-5 sm:px-6',
} as const
