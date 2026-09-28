/**
 * Number formatting utilities — 全局规范：小数最多保留两位。
 */

/**
 * Format a number with at most `digits` decimal places (default 2).
 * Trailing zeros are stripped: 42.718 → "42.72", 72 → "72", 3.10 → "3.1".
 * Returns "-" for null/undefined/NaN.
 */
export function formatNum(v: number | string | null | undefined, digits = 2): string {
  if (v === null || v === undefined || v === '') return '-'
  const n = typeof v === 'string' ? Number(v) : v
  if (typeof n !== 'number' || isNaN(n)) return '-'
  const rounded = n.toFixed(digits)
  return rounded.replace(/\.?0+$/, '') || '0'
}

/**
 * Format a percentage value (value already in percent units) with ≤2 decimals.
 */
export function formatPct(v: number | string | null | undefined, digits = 2): string {
  const s = formatNum(v, digits)
  return s === '-' ? '-' : `${s}%`
}
