/**
 * useUrlState - keeps a set of reactive filter/pagination values in sync with the URL query.
 *
 * Hydrates from `route.query` synchronously (so the first data fetch already sees the
 * shared state), then writes changes back with `router.replace` — the back button and
 * page reload keep working, and only non-default values appear in the address bar.
 */
import { nextTick, onMounted, onScopeDispose, watch, type Ref, type WatchStopHandle } from 'vue'
import { useRoute, useRouter } from 'vue-router'

function serialize(v: any): string {
  if (v === null || v === undefined || v === '') return ''
  if (Array.isArray(v)) return v.filter(x => x !== null && x !== undefined && x !== '').join(',')
  if (typeof v === 'boolean') return v ? '1' : ''
  return String(v)
}

function deserialize(raw: string, current: any): any {
  if (Array.isArray(current)) return raw ? raw.split(',') : []
  if (typeof current === 'number') {
    const n = Number(raw)
    return Number.isFinite(n) ? n : current
  }
  if (typeof current === 'boolean') return raw === '1' || raw === 'true'
  return raw
}

export function useUrlState(sources: Record<string, Ref<any>>, opts?: { validate?: (name: string, value: any) => boolean }) {
  const route = useRoute()
  const router = useRouter()
  const names = Object.keys(sources)
  const defaults = new Map<string, string>()

  for (const name of names) {
    defaults.set(name, serialize(sources[name].value))
    const raw = route.query[name]
    if (typeof raw !== 'string' || raw === '') continue
    const parsed = deserialize(raw, sources[name].value)
    // 非法值（如 ?page=abc&size=99999）保持默认，不污染页面状态也不加重后端查询
    if (opts?.validate && !opts.validate(name, parsed)) continue
    if (parsed !== sources[name].value && !(Array.isArray(parsed) && !parsed.length)) {
      sources[name].value = parsed
    }
  }

  function currentQuery() {
    const query: Record<string, string> = {}
    for (const [k, v] of Object.entries(route.query)) {
      if (!names.includes(k) && typeof v === 'string') query[k] = v
    }
    for (const name of names) {
      const s = serialize(sources[name].value)
      if (s !== '' && s !== defaults.get(name)) query[name] = s
    }
    return query
  }

  let stop: WatchStopHandle | null = null
  let writing = false
  // keep-alive 缓存的页面在别的路由上不应改写地址栏，否则会污染当前页面的 query
  const ownerPath = route.path
  onMounted(() => {
    nextTick(() => {
      if (stop) return
      stop = watch(names.map(n => sources[n]), () => {
        if (writing || route.path !== ownerPath) return
        const query = currentQuery()
        if (serializeQuery(query) === serializeQuery(route.query as Record<string, any>)) return
        writing = true
        Promise.resolve(router.replace({ path: route.path, query })).finally(() => { writing = false })
      })
    })
  })

  onScopeDispose(() => stop?.())
}

function serializeQuery(q: Record<string, any>): string {
  return Object.keys(q).sort().map(k => `${k}=${Array.isArray(q[k]) ? q[k].join(',') : q[k] ?? ''}`).join('&')
}
