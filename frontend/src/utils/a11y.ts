/**
 * a11y helpers for clickable non-button elements (div/span/icon used as controls).
 *
 * `v-bind="kbdClick(label)"` adds button semantics plus Enter/Space activation: the keydown
 * handler fires a native click on the same element, so the existing `@click` handler is reused
 * instead of duplicated at every call site.
 */
export function kbdClick(label?: string) {
  return {
    role: 'button',
    tabindex: '0',
    ...(label ? { 'aria-label': label } : {}),
    onKeydown: (e: KeyboardEvent) => {
      if (e.target !== e.currentTarget) return
      if (e.key !== 'Enter' && e.key !== ' ' && e.key !== 'Spacebar') return
      e.preventDefault()
      ;(e.currentTarget as HTMLElement).click()
    },
  }
}
