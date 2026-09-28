/**
 * DateTime utility functions
 * Provides consistent date/time formatting across the application.
 *
 * All timestamps are rendered in Beijing time (Asia/Shanghai, UTC+8)
 * regardless of the viewer's device timezone — 产品要求全局统一北京时间。
 */

const BJ_DATETIME = new Intl.DateTimeFormat('en-CA', {
  timeZone: 'Asia/Shanghai',
  hourCycle: 'h23',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})

const BJ_DATE = new Intl.DateTimeFormat('en-CA', {
  timeZone: 'Asia/Shanghai',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
})

const BJ_TIME = new Intl.DateTimeFormat('en-GB', {
  timeZone: 'Asia/Shanghai',
  hourCycle: 'h23',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})

function toDate(ts: string | number | Date | null | undefined): Date | null {
  if (!ts) return null
  try {
    const d = ts instanceof Date ? ts : new Date(ts)
    return isNaN(d.getTime()) ? null : d
  } catch {
    return null
  }
}

/**
 * Format a timestamp (ISO string, epoch ms, or Date) into a Beijing-time
 * datetime string like "2026-08-28 11:13:46", or "-" if invalid.
 */
export function formatDateTime(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts)
  if (!d) return '-'
  // en-CA yields "2026-08-28, 11:13:46" — normalize the separator
  return BJ_DATETIME.format(d).replace(', ', ' ')
}

/**
 * Format a timestamp to Beijing-time date only (without time).
 */
export function formatDate(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts)
  if (!d) return '-'
  return BJ_DATE.format(d)
}

/**
 * Format a timestamp to Beijing-time time only (HH:mm:ss).
 */
export function formatTime(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts)
  if (!d) return '-'
  return BJ_TIME.format(d)
}

/**
 * Format a relative time from now (e.g., "3 minutes ago").
 */
export function formatRelativeTime(ts: string | number | Date | null | undefined): string {
  const d = toDate(ts)
  if (!d) return '-'
  const diff = Date.now() - d.getTime()
  const seconds = Math.floor(diff / 1000)
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  if (days < 30) return `${days}d ago`
  return formatDateTime(ts)
}
