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
let context: CanvasRenderingContext2D | null | undefined

/** 时间线标签用到的三种 12px 字体，字族取自页面当前的正文和等宽字体。 */
export function getLabelFonts(): {
  strong: string
  text: string
  mono: string
} {
  const sans = getComputedStyle(document.body).fontFamily || 'sans-serif'
  const mono =
    getComputedStyle(document.documentElement)
      .getPropertyValue('--font-mono')
      .trim() || 'monospace'
  return {
    strong: `600 12px ${sans}`,
    text: `400 12px ${sans}`,
    mono: `400 12px ${mono}`,
  }
}

/** 用 canvas 量出文字宽度，判断标签能否放下；拿不到 canvas 时按每字一个字号宽保守估计。 */
export function measureTextWidth(text: string, font: string): number {
  if (context === undefined) {
    context = document.createElement('canvas').getContext('2d')
  }
  if (!context) return text.length * 12
  context.font = font
  return Math.ceil(context.measureText(text).width)
}
