import { ref, type Ref } from 'vue'

interface LocaleMessages {
  [key: string]: string | LocaleMessages
}

const localeMessages: Record<string, LocaleMessages> = {}
const currentLocale: Ref<string> = ref('zh-CN')

// Keep the document lang attribute in sync so screen readers and browser
// translation prompts follow the in-app language switch.
function applyHtmlLang(locale: string) {
  if (typeof document !== 'undefined') {
    document.documentElement.lang = locale === 'en-US' ? 'en-US' : 'zh-CN'
  }
}

export function setupLocale(locale: string, msgs: LocaleMessages) {
  localeMessages[locale] = msgs
  if (!localeMessages[currentLocale.value]) {
    currentLocale.value = locale
  }
}

export function setLocale(locale: string) {
  if (locale !== 'zh-CN' && locale !== 'en-US') return
  if (localeMessages[locale]) {
    currentLocale.value = locale
    localStorage.setItem('edgelite_locale', locale)
    applyHtmlLang(locale)
  }
}

export function getLocale(): string {
  return currentLocale.value
}

export function useCurrentLocale(): Ref<string> {
  return currentLocale
}

export function getAvailableLocales(): string[] {
  return Object.keys(localeMessages)
}

export function initLocale() {
  const saved = localStorage.getItem('edgelite_locale')
  if (saved && localeMessages[saved]) {
    currentLocale.value = saved
  }
  applyHtmlLang(currentLocale.value)
}

export function t(key: string, params?: Record<string, string | number> | string): string {
  const fallbackStr = typeof params === 'string' ? params : undefined
  const actualParams = typeof params === 'object' ? params : undefined
  const parts = key.split('.')
  const result = _resolveNested(parts, currentLocale.value)
    ?? _resolveNested(parts, 'en-US')
    ?? _resolveJoined(parts, currentLocale.value)
    ?? _resolveJoined(parts, 'en-US')
  if (result === undefined) return fallbackStr !== undefined ? fallbackStr : key
  if (!actualParams) return result
  return Object.entries(actualParams).reduce(
    (s, [k, v]) => s.replace(new RegExp(`\\{${k}\\}`, 'g'), String(v)),
    result,
  )
}

function _activeMessages(locale: string): LocaleMessages {
  const msgs = localeMessages[locale]
  if (typeof msgs !== 'object' || msgs === null || Object.keys(msgs).length === 0) {
    return localeMessages['zh-CN'] || {}
  }
  return msgs
}

function _resolveNested(parts: string[], locale: string): string | undefined {
  let result: string | LocaleMessages = _activeMessages(locale)
  for (const part of parts) {
    if (typeof result === 'object' && result !== null && part in result) {
      result = result[part]
    } else {
      return undefined
    }
  }
  return typeof result === 'string' ? result : undefined
}

// Locale files store some keys with literal dots inside a single level
// (e.g. driverField: { 'modbus-tcp.ipAddr': ... }), which a plain nested
// walk cannot reach; try longest joined-key matches as a fallback.
function _resolveJoined(parts: string[], locale: string): string | undefined {
  let result: string | LocaleMessages = _activeMessages(locale)
  let i = 0
  while (i < parts.length) {
    if (typeof result !== 'object' || result === null) return undefined
    let matched = false
    for (let j = parts.length; j > i; j--) {
      const joined = parts.slice(i, j).join('.')
      if (joined in result) {
        const v: string | LocaleMessages = result[joined]
        if (typeof v === 'string') return v
        result = v
        i = j
        matched = true
        break
      }
    }
    if (!matched) return undefined
  }
  return typeof result === 'string' ? result : undefined
}

export { type LocaleMessages }
