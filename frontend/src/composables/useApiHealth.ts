/**
 * useApiHealth - tracks whether the backend is currently answering requests.
 *
 * The browser being online (`network.offline`) is not the same as the gateway
 * service being reachable: with the service down or a dependency failing, every
 * list page still renders its "no data" empty state, so an outage is
 * indistinguishable from an idle system.
 *
 * State is a module singleton — http.ts records outcomes, MainLayout renders the
 * banner. 4xx is deliberately ignored: validation/permission failures are normal
 * conversation between a page and the API, and surfacing them here would be noise.
 */
import { computed, readonly, ref } from 'vue'

const degraded = ref(false)
const dismissed = ref(false)
const failureCount = ref(0)
const lastStatus = ref(0)
const lastFailureAt = ref(0)
const recoveredAt = ref(0)
const probing = ref(false)

function markFailure(status: number) {
  failureCount.value += 1
  lastStatus.value = status
  lastFailureAt.value = Date.now()
  degraded.value = true
}

function markSuccess() {
  if (degraded.value) recoveredAt.value = Date.now()
  degraded.value = false
  dismissed.value = false
  failureCount.value = 0
  lastStatus.value = 0
}

/** Snooze the banner until the next failure after a recovery. */
function dismiss() {
  dismissed.value = true
}

const PROBE_URL = '/api/v1/system/health/basic'
const PROBE_TIMEOUT_MS = 8000

/**
 * Ask the service directly whether it is up. Any HTTP answer below 500 counts as
 * reachable — 401/403 prove the process is serving requests even if this role
 * cannot read the health payload.
 */
async function probe(): Promise<boolean> {
  if (probing.value) return false
  probing.value = true
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), PROBE_TIMEOUT_MS)
  try {
    const resp = await fetch(PROBE_URL, {
      credentials: 'include',
      signal: ctrl.signal,
      headers: { Accept: 'application/json' },
    })
    if (resp.status < 500) {
      markSuccess()
      return true
    }
    markFailure(resp.status)
    return false
  } catch {
    return false
  } finally {
    clearTimeout(timer)
    probing.value = false
  }
}

export function useApiHealth() {
  return {
    degraded: readonly(degraded),
    probing: readonly(probing),
    failureCount: readonly(failureCount),
    lastStatus: readonly(lastStatus),
    lastFailureAt: readonly(lastFailureAt),
    recoveredAt: readonly(recoveredAt),
    /** Banner visibility: degraded and not snoozed by the user. */
    visible: computed(() => degraded.value && !dismissed.value),
    markFailure,
    markSuccess,
    dismiss,
    probe,
  }
}
