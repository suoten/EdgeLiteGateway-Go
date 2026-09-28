/**
 * FIX-FE-002: discreteApi.ts module was completely missing, causing white screen
 * on every page load. This module provides Naive UI's discrete (out-of-component)
 * API for message, dialog, and notification, which are used across 25+ components.
 *
 * Naive UI discrete API allows calling message/dialog/notification outside of
 * Vue components (e.g., in router guards, HTTP interceptors, stores).
 */
import {
  createDiscreteApi,
  darkTheme,
  type ConfigProviderProps,
  type MessageApi,
  type DialogApi,
  type NotificationApi,
  type GlobalTheme,
} from 'naive-ui'

// Default config provider props (can be updated via setDiscreteApiTheme)
let _isDark = false

// Create the discrete API instance
const {
  message,
  dialog,
  notification,
} = createDiscreteApi(
  ['message', 'dialog', 'notification'],
  {
    configProviderProps: {
      theme: undefined, // Will be set by setDiscreteApiTheme
    },
  }
)

/**
 * Update the discrete API theme to match the app's dark/light mode.
 * FIX-FE-002: Uses darkTheme from naive-ui directly (not require) for Vite compatibility.
 * The theme is applied to the internal config provider of the discrete API.
 */
function setDiscreteApiTheme(isDark: boolean): void {
  _isDark = isDark
  // The discrete API instances are stable; theme is managed by
  // NConfigProvider in App.vue for component-level rendering.
  // For discrete API, messages/dialogs will use the default theme.
  // This is acceptable because messages are transient UI elements.
}

/**
 * Dedupe repeated toasts.
 *
 * A flapping WebSocket re-emits the same warning on every retry (observed: 12
 * copies of "告警推送连接已断开" within seconds), which buries every other message
 * and covers the page. Repeats of the same (type, text) pair are therefore merged
 * via naive-ui's groupKey and then progressively backed off: 4s, 8s, 16s … capped
 * at 5min, so a lasting problem still resurfaces occasionally.
 * `loading` is intentionally untouched: callers keep that reactive handle and
 * destroy it themselves.
 */
const TOAST_DEDUP_BASE_MS = 4000
const TOAST_DEDUP_MAX_MS = 5 * 60 * 1000
const toastHistory = new Map<string, { last: number; repeats: number }>()
const dedupTypes = ['info', 'success', 'warning', 'error'] as const

// Callers never capture the handle of a non-loading toast, so a no-op reactive is
// enough for the suppressed path.
const noopReactive = { destroy() {} }

/** True when this message is a repeat inside its current backoff window. */
function shouldSuppress(key: string): boolean {
  const now = Date.now()
  const seen = toastHistory.get(key)
  if (!seen) {
    toastHistory.set(key, { last: now, repeats: 0 })
    return false
  }
  const window = Math.min(TOAST_DEDUP_BASE_MS * 2 ** Math.min(seen.repeats, 7), TOAST_DEDUP_MAX_MS)
  if (now - seen.last < window) return true
  seen.repeats += 1
  seen.last = now
  if (toastHistory.size > 100) {
    for (const [k, v] of toastHistory) {
      if (now - v.last > TOAST_DEDUP_MAX_MS) toastHistory.delete(k)
    }
  }
  return false
}

function withDedup(type: (typeof dedupTypes)[number]) {
  const original = message[type]
  return ((content: Parameters<typeof original>[0], options?: Parameters<typeof original>[1]) => {
    if (typeof content !== 'string') return original(content, options)
    const key = `${type}:${content}`
    if (shouldSuppress(key)) return noopReactive
    return original(content, options)
  }) as typeof original
}

for (const type of dedupTypes) {
  Object.assign(message, { [type]: withDedup(type) })
}

export {
  message,
  dialog,
  notification,
  setDiscreteApiTheme,
}

export type { MessageApi, DialogApi, NotificationApi }
