<template>
  <div class="page-container">
    <n-card :title="t('router.observabilityOverview')" size="small">
      <n-tabs type="line" animated>
        <n-tab-pane name="overview" :tab="t('observability.overview')">
          <n-grid :cols="3" :x-gap="12" :y-gap="12">
            <n-gi v-for="card in metricCards" :key="card.label">
              <n-card size="small">
                <n-statistic :label="card.label" :value="card.value" />
              </n-card>
            </n-gi>
          </n-grid>

          <n-divider />

          <n-h3>{{ t('observability.systemHealth') }}</n-h3>
          <n-grid :cols="2" :x-gap="12">
            <n-gi>
              <n-card :title="t('observability.cpuUsage')" size="small">
                <n-progress
                  v-if="health.cpu_percent != null"
                  type="line"
                  :percentage="health.cpu_percent"
                  :status="health.cpu_percent > 80 ? 'error' : health.cpu_percent > 60 ? 'warning' : 'success'"
                />
                <span v-else class="unmeasured">{{ t('observability.notCollected') }}</span>
              </n-card>
            </n-gi>
            <n-gi>
              <n-card :title="t('observability.memoryUsage')" size="small">
                <n-progress
                  v-if="health.memory_percent != null"
                  type="line"
                  :percentage="health.memory_percent"
                  :status="health.memory_percent > 80 ? 'error' : health.memory_percent > 60 ? 'warning' : 'success'"
                />
                <span v-else class="unmeasured">{{ t('observability.notCollected') }}</span>
              </n-card>
            </n-gi>
            <n-gi>
              <n-card :title="t('observability.diskUsage')" size="small">
                <n-progress
                  v-if="health.disk_percent != null"
                  type="line"
                  :percentage="health.disk_percent"
                  :status="health.disk_percent > 80 ? 'error' : health.disk_percent > 60 ? 'warning' : 'success'"
                />
                <span v-else class="unmeasured">{{ t('observability.notCollected') }}</span>
              </n-card>
            </n-gi>
            <n-gi>
              <n-card :title="t('observability.goroutines')" size="small">
                <n-statistic :value="health.goroutines" />
              </n-card>
            </n-gi>
          </n-grid>
        </n-tab-pane>

        <n-tab-pane name="events" :tab="t('observability.events')">
          <n-alert v-if="!eventsSupported" type="info" size="small" :show-icon="true" style="margin-bottom: 8px">
            {{ t('observability.notCollected') }}
          </n-alert>
          <n-data-table
            :columns="eventColumns"
            :data="events"
            :bordered="false"
            size="small"
            :pagination="{ pageSize: 20 }"
          />
        </n-tab-pane>

        <n-tab-pane name="traces" :tab="t('observability.traces')">
          <n-alert v-if="!tracesSupported" type="info" size="small" :show-icon="true" style="margin-bottom: 8px">
            {{ t('observability.notCollected') }}
          </n-alert>
          <n-data-table
            :columns="traceColumns"
            :data="traces"
            :bordered="false"
            size="small"
            :pagination="{ pageSize: 20 }"
          />
        </n-tab-pane>

        <n-tab-pane name="rules" :tab="t('observability.rules')">
          <n-alert v-if="!rulesSupported" type="info" size="small" :show-icon="true" style="margin-bottom: 8px">
            {{ t('observability.notCollected') }}
          </n-alert>
          <n-data-table
            :columns="ruleColumns"
            :data="observabilityRules"
            :bordered="false"
            size="small"
            :pagination="{ pageSize: 20 }"
          />
        </n-tab-pane>
      </n-tabs>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { message } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'

const health = ref<any>({})
const events = ref<any[]>([])
const traces = ref<any[]>([])
const observabilityRules = ref<any[]>([])
// Each panel is only empty-by-data if the backend says it collects the data.
const eventsSupported = ref(true)
const tracesSupported = ref(true)
const rulesSupported = ref(true)

// A null means "this build does not collect it"; showing 0 there would be a
// number the operator cannot tell apart from a measured one.
function fmtCount(v: unknown): string | number {
  return v == null ? '-' : (v as number).toLocaleString()
}

const metricCards = computed(() => [
  { label: t('observability.totalRequests'), value: fmtCount(health.value.total_requests) },
  { label: t('observability.errorRate'), value: health.value.error_count == null || health.value.total_requests == null ? '-' : `${(health.value.error_count / Math.max(health.value.total_requests, 1) * 100).toFixed(2)}%` },
  { label: t('observability.avgResponseTime'), value: health.value.avg_response_ms == null ? '-' : `${health.value.avg_response_ms}ms` },
  { label: t('observability.activeConnections'), value: fmtCount(health.value.active_connections) },
  { label: t('observability.eventsDelivered'), value: fmtCount(health.value.events_delivered) },
  { label: t('observability.eventsDropped'), value: fmtCount(health.value.events_dropped) },
])

const eventColumns = computed(() => [
  { title: t('common.time'), key: 'timestamp', width: 180 },
  { title: t('observability.type'), key: 'type', width: 100 },
  { title: t('observability.source'), key: 'source', width: 120 },
  { title: t('common.message'), key: 'message' },
  { title: t('observability.severity'), key: 'severity', width: 80 },
])

const traceColumns = computed(() => [
  { title: t('observability.traceId'), key: 'trace_id', width: 120 },
  { title: t('observability.method'), key: 'method', width: 80 },
  { title: t('observability.path'), key: 'path' },
  { title: t('observability.duration'), key: 'duration_ms', render: (r: any) => (r.duration_ms == null ? '-' : `${r.duration_ms}ms`) },
  { title: t('observability.status'), key: 'status_code', width: 80 },
])

const ruleColumns = computed(() => [
  { title: t('observability.ruleName'), key: 'name' },
  { title: t('observability.condition'), key: 'condition' },
  { title: t('common.actions'), key: 'action' },
  { title: t('observability.enabled'), key: 'enabled', render: (r: any) => (r.enabled ? t('common.yes') : t('common.no')) },
])

async function refresh() {
  try {
    const [healthRes, eventsRes, tracesRes] = await Promise.all([
      http.get('/observability/health'),
      http.get('/observability/events'),
      http.get('/observability/traces'),
    ])
    // http.get resolves the {code, message, data} envelope, so the previous
    // .data reads handed the envelope to the page and every field was blank.
    health.value = healthRes.data?.data ?? {}
    const eventsPayload = eventsRes.data?.data
    events.value = Array.isArray(eventsPayload) ? eventsPayload : (eventsPayload?.items ?? [])
    eventsSupported.value = !(eventsPayload && eventsPayload.supported === false)
    const tracesPayload = tracesRes.data?.data
    traces.value = Array.isArray(tracesPayload) ? tracesPayload : (tracesPayload?.items ?? [])
    tracesSupported.value = !(tracesPayload && tracesPayload.supported === false)
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function loadRules() {
  try {
    const res = await http.get('/observability/rules')
    const payload = res.data?.data
    observabilityRules.value = Array.isArray(payload) ? payload : (payload?.items ?? [])
    rulesSupported.value = !(payload && payload.supported === false)
  } catch (e: any) {
    // A rules panel that fails silently looks like a panel with no rules.
    rulesSupported.value = false
    message.error(extractError(e, t('common.loadFailed')))
  }
}

onMounted(() => {
  refresh()
  loadRules()
})
</script>

<style scoped>
.page-container { padding: 16px; }
.unmeasured { color: var(--n-text-color-3, #999); font-size: 12px; }
</style>
