/**
 * ResizeObserver fallback for hidden/collapsed viewports — must be imported
 * FIRST in main.ts, before any naive-ui/vueuc module evaluates (vueuc's RO
 * delegate captures window.ResizeObserver once at module load).
 *
 * Why: in hidden pages (document.hidden, e.g. an embedded preview with a
 * collapsed viewport) the browser runs no rendering frames, so native
 * ResizeObserver callbacks never fire and rAF-based transitions stall.
 * naive-ui virtual lists (every n-select dropdown menu, virtual-scroll
 * tables, n-virtual-list) then render ZERO visible items because their
 * ResizeObserver never reports the container size.
 *
 * This shim is installed ONLY when the page starts out hidden, replacing
 * window.ResizeObserver with a spec-compatible polling implementation that
 * fires the initial observation (per spec) and then polls observed elements'
 * sizes on a timer (timers still run while hidden). Visible browsers keep the
 * native ResizeObserver untouched and pay no polling cost.
 */
class PollingResizeObserver {
  private cb: ResizeObserverCallback
  private sizes = new Map<Element, { width: number; height: number }>()
  private timer: ReturnType<typeof setInterval> | null = null

  constructor(cb: ResizeObserverCallback) {
    this.cb = cb
  }

  observe(target: Element): void {
    this.sizes.set(target, this.measure(target))
    this.start()
    queueMicrotask(() => this.check())
  }

  unobserve(target: Element): void {
    this.sizes.delete(target)
    if (this.sizes.size === 0) this.stop()
  }

  disconnect(): void {
    this.sizes.clear()
    this.stop()
  }

  private start(): void {
    if (this.timer === null) {
      this.timer = setInterval(() => this.check(), 150)
    }
  }

  private stop(): void {
    if (this.timer !== null) {
      clearInterval(this.timer)
      this.timer = null
    }
  }

  private measure(el: Element): { width: number; height: number } {
    const rect = el.getBoundingClientRect()
    return { width: rect.width, height: rect.height }
  }

  private check(): void {
    for (const [el, prev] of this.sizes) {
      const cur = this.measure(el)
      if (cur.width !== prev.width || cur.height !== prev.height) {
        this.sizes.set(el, cur)
        this.fire(el)
      }
    }
  }

  private fire(target: Element): void {
    const rect = target.getBoundingClientRect()
    const entry = {
      target,
      contentRect: rect,
      borderBoxSize: [],
      contentBoxSize: [],
      devicePixelContentBoxSize: [],
    } as unknown as ResizeObserverEntry
    try {
      this.cb([entry], this as unknown as ResizeObserver)
    } catch (e) {
      console.error('[ResizeObserverShim] callback error', e)
    }
  }
}

if (
  typeof window !== 'undefined' &&
  typeof window.ResizeObserver !== 'undefined' &&
  document.hidden
) {
  ;(window as any).ResizeObserver = PollingResizeObserver
}
