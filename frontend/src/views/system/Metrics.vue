<template>
  <div class="page-container">
    <n-card :title="t('router.metrics')" size="small">
      <n-space vertical>
        <!-- Metrics Categories -->
        <n-tabs type="segment" v-model:value="activeCategory">
          <n-tab-pane name="system" :tab="t('metrics.tabSystem')">
            <n-grid :cols="3" :x-gap="12" :y-gap="12">
              <n-gi v-for="metric in systemMetrics" :key="metric.name">
                <n-card size="small">
                  <n-statistic :label="metric.label" :value="metric.value" :suffix="metric.unit" />
                </n-card>
              </n-gi>
            </n-grid>
          </n-tab-pane>
          <n-tab-pane name="engine" :tab="t('metrics.tabEngine')">
            <n-grid :cols="3" :x-gap="12" :y-gap="12">
              <n-gi v-for="metric in engineMetrics" :key="metric.name">
                <n-card size="small">
                  <n-statistic :label="metric.label" :value="metric.value" :suffix="metric.unit" />
                </n-card>
              </n-gi>
            </n-grid>
          </n-tab-pane>
          <n-tab-pane name="drivers" :tab="t('metrics.tabDrivers')">
            <n-data-table :columns="driverColumns" :data="driverMetrics" :bordered="false" size="small" />
          </n-tab-pane>
          <n-tab-pane name="api" :tab="t('metrics.tabApi')">
            <n-space vertical>
              <n-alert v-if="apiNotCollected" type="info" :show-icon="false" size="small">
                {{ t('metrics.apiNotCollected') }}
              </n-alert>
              <n-data-table :columns="apiColumns" :data="apiMetrics" :bordered="false" size="small" />
            </n-space>
          </n-tab-pane>
        </n-tabs>

        <n-divider />
        <n-space>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
          <n-tag :type="liveUpdating ? 'success' : 'default'">
            {{ liveUpdating ? t('metrics.live') : t('metrics.paused') }}
          </n-tag>
          <n-switch v-model:value="liveUpdating" @update:value="toggleLive" />
        </n-space>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message } from '@/utils/discreteApi'
import { formatNum } from '@/utils/format'
import { deviceStatusLabel } from '@/utils/enumLabels'

const activeCategory = ref('system')
const metricsData = ref<any>({})
const liveUpdating = ref(true)
let timer: any

const systemMetrics = computed(() => {
  const sys = metricsData.value.system || {}
  return [
    { name: 'cpu', label: t('metrics.cpuUsage'), value: formatNum(sys.cpu_percent, 1), unit: '%' },
    { name: 'memory', label: t('metrics.memUsage'), value: formatNum(sys.memory_percent, 1), unit: '%' },
    { name: 'disk', label: t('metrics.diskUsage'), value: formatNum(sys.disk_percent, 1), unit: '%' },
    { name: 'goroutines', label: t('metrics.goroutines'), value: sys.goroutines || 0, unit: '' },
    { name: 'uptime', label: t('metrics.uptime'), value: formatNum(sys.uptime_hours, 2), unit: 'h' },
    { name: 'gcPauses', label: t('metrics.gcPauses'), value: formatNum(sys.gc_pauses_ms, 2), unit: 'ms' },
  ]
})

const engineMetrics = computed(() => {
  const eng = metricsData.value.engine || {}
  return [
    { name: 'totalDevices', label: t('metrics.totalDevices'), value: eng.total_devices || 0, unit: '' },
    { name: 'onlineDevices', label: t('metrics.onlineDevices'), value: eng.online_devices || 0, unit: '' },
    { name: 'totalPoints', label: t('metrics.totalPoints'), value: eng.total_points || 0, unit: '' },
    { name: 'collectRate', label: t('metrics.collectRate'), value: formatNum(eng.collect_rate, 1), unit: '/s' },
    { name: 'activeRules', label: t('metrics.activeRules'), value: eng.active_rules || 0, unit: '' },
    { name: 'alarmsToday', label: t('metrics.alarmsToday'), value: eng.alarms_today || 0, unit: '' },
  ]
})

const driverMetrics = computed(() => metricsData.value.drivers || [])
const apiMetrics = computed(() => metricsData.value.api_endpoints || [])
const apiNotCollected = computed(() =>
  apiMetrics.value.length === 0 && (metricsData.value.not_collected || []).includes('api_endpoints')
)

const circuitLabels: Record<string, string> = {
  closed: 'metrics.circuitClosed',
  open: 'metrics.circuitOpen',
  half_open: 'metrics.circuitHalfOpen',
}

const driverColumns = computed(() => [
  { title: t('metrics.device'), key: 'device_name' },
  { title: t('metrics.protocol'), key: 'protocol' },
  { title: t('metrics.status'), key: 'status', render: (r: any) => deviceStatusLabel.value?.[r.status] || r.status },
  { title: t('metrics.reads'), key: 'read_count', render: (r: any) => formatNum(r.read_count, 0) },
  { title: t('metrics.errors'), key: 'error_count', render: (r: any) => formatNum(r.error_count, 0) },
  // error_rate arrives null when the device has attempted nothing; coercing it to
  // 0 drew a green 0.00% for a device that never reported.
  { title: t('metrics.errorRate'), key: 'error_rate', render: (r: any) => (r.error_rate === null || r.error_rate === undefined ? '-' : `${formatNum(r.error_rate * 100, 2)}%`) },
  { title: t('metrics.avgLatency'), key: 'avg_latency_ms', render: (r: any) => (r.avg_latency_ms === null || r.avg_latency_ms === undefined ? '-' : `${formatNum(r.avg_latency_ms, 1)}ms`) },
  // An unknown or absent breaker state must not borrow the "closed" label:
  // that reads as "no circuit protection engaged", which is a different claim.
  { title: t('metrics.circuit'), key: 'circuit_state', render: (r: any) => (r.circuit_state ? (circuitLabels[r.circuit_state] ? t(circuitLabels[r.circuit_state]) : r.circuit_state) : '-') },
])

const apiColumns = computed(() => [
  { title: t('metrics.endpoint'), key: 'path' },
  { title: t('metrics.method'), key: 'method' },
  { title: t('metrics.calls'), key: 'call_count' },
  { title: t('metrics.errors'), key: 'error_count' },
  { title: t('metrics.avgDuration'), key: 'avg_duration_ms', render: (r: any) => `${formatNum(r.avg_duration_ms, 1)}ms` },
  { title: t('metrics.p99'), key: 'p99_ms', render: (r: any) => `${formatNum(r.p99_ms, 1)}ms` },
])

async function refresh() {
  try {
    const res = await http.get('/metrics/summary')
    metricsData.value = res.data?.data || {}
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

function toggleLive(val: boolean) {
  if (val) {
    timer = setInterval(refresh, 5000)
  } else {
    if (timer) clearInterval(timer)
  }
}

onMounted(() => {
  refresh()
  if (liveUpdating.value) {
    timer = setInterval(refresh, 5000)
  }
})

onUnmounted(() => {
  if (timer) clearInterval(timer)
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
