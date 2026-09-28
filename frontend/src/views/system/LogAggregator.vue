<template>
  <div class="page-container">
    <n-card :title="t('router.logAggregator')" size="small">
      <n-space vertical>
        <!-- Log Level Filter -->
        <n-space>
          <n-select
            v-model:value="logLevel"
            :options="levelOptions"
            style="width: 120px;"
            @update:value="loadLogs"
          />
          <n-input
            v-model:value="searchText"
            :placeholder="t('logAggregator.searchPlaceholder')"
            clearable
            style="width: 300px;"
            @update:value="loadLogs"
          />
          <n-date-picker
            v-model:value="timeRange"
            type="datetimerange"
            clearable
            @update:value="loadLogs"
          />
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
          <n-button :loading="exporting" @click="exportLogs">{{ t('logAggregator.export') }}</n-button>
        </n-space>

        <!-- Runtime level: what the gateway writes from now on, not a filter over
             what it already wrote. -->
        <n-space align="center" :size="8">
          <n-text depth="3" style="font-size:12px">{{ t('logAggregator.runtimeLevel') }}</n-text>
          <n-select
            v-model:value="runtimeLevel"
            :options="runtimeLevelOptions"
            :loading="runtimeLevelLoading"
            style="width: 120px;"
            @update:value="applyRuntimeLevel"
          />
          <n-text depth="3" style="font-size:12px">{{ t('logAggregator.runtimeLevelHint') }}</n-text>
        </n-space>

        <!-- Stats Cards -->
        <n-space v-if="logStats.length" align="center" :size="8">
          <n-text depth="3" style="font-size:12px">{{ t('logAggregator.statsScope', { count: statsScope }) }}</n-text>
        </n-space>
        <n-grid :cols="5" :x-gap="12">
          <n-gi v-for="stat in logStats" :key="stat.level">
            <n-statistic :label="stat.label" :value="stat.count" />
          </n-gi>
        </n-grid>
        <n-alert v-if="statsError" type="warning" size="small" :show-icon="true">{{ statsError }}</n-alert>

        <!-- Log Table -->
        <n-data-table
          :columns="logColumns"
          :data="logData"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 50 }"
          :max-height="500"
        />
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { logApi, type LogQueryParams } from '@/api'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message } from '@/utils/discreteApi'
import { NTag } from 'naive-ui'
import { formatDateTime } from '@/utils/datetime'

const logLevel = ref('')
const searchText = ref('')
const timeRange = ref<[number, number] | null>(null)
const logData = ref<any[]>([])
const logStats = ref<any[]>([])
// How many buffered entries the counters cover, and why they are missing.
const statsScope = ref(0)
const statsError = ref('')

// Runtime log level, read from the gateway rather than assumed, so the select
// cannot offer a level the backend would reject.
const runtimeLevel = ref('')
const runtimeLevelOptions = ref<{ label: string; value: string }[]>([])
const runtimeLevelLoading = ref(false)
// Last level the gateway confirmed, so a failed change can be rolled back.
let runtimeLevelApplied = ''
const exporting = ref(false)

const levelOptions = [
  { label: t('logAggregator.all'), value: '' },
  { label: 'DEBUG', value: 'DEBUG' },
  { label: 'INFO', value: 'INFO' },
  { label: 'WARN', value: 'WARN' },
  { label: 'ERROR', value: 'ERROR' },
  { label: 'FATAL', value: 'FATAL' },
]

const levelColors: Record<string, string> = {
  DEBUG: 'default', INFO: 'info', WARN: 'warning', ERROR: 'error', FATAL: 'error',
}

const logColumns = computed(() => [
  {
    title: t('logAggregator.level'),
    key: 'level',
    width: 80,
    render: (r: any) => h(NTag, { type: (levelColors[r.level] || 'default') as any, size: 'small' }, () => r.level),
  },
  { title: t('logAggregator.time'), key: 'timestamp', width: 180, render: (r: any) => (r.timestamp ? formatDateTime(r.timestamp) : '-') },
  { title: t('logAggregator.source'), key: 'source', width: 120 },
  { title: t('logAggregator.message'), key: 'message', ellipsis: { tooltip: true } },
  { title: t('logAggregator.traceId'), key: 'trace_id', width: 120, ellipsis: { tooltip: true } },
])

function filterParams() {
  const params: LogQueryParams = { page: 1, size: 200 }
  if (logLevel.value) params.level = logLevel.value
  if (searchText.value) params.search = searchText.value
  if (timeRange.value) {
    params.start_time = new Date(timeRange.value[0]).toISOString()
    params.end_time = new Date(timeRange.value[1]).toISOString()
  }
  return params
}

async function loadLogs() {
  try {
    const res = await logApi.list(filterParams())
    logData.value = res?.items || []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function loadStats() {
  try {
    const data = await logApi.stats()
    statsScope.value = Number(data?.total_logs ?? 0)
    logStats.value = (data?.stats || []).map((s: any) => ({
      level: s.level,
      label: s.level,
      count: s.count,
    }))
    statsError.value = ''
  } catch (e: any) {
    // The panel used to swallow this: an unreachable stats route left five empty
    // counters that read as "no logs at any level".
    logStats.value = []
    statsError.value = extractError(e, t('common.loadFailed'))
  }
}

async function loadRuntimeLevel() {
  runtimeLevelLoading.value = true
  try {
    const data = await logApi.levels()
    runtimeLevel.value = data?.current || ''
    runtimeLevelApplied = runtimeLevel.value
    runtimeLevelOptions.value = (data?.available || []).map((l) => ({ label: l, value: l }))
  } catch (e: any) {
    statsError.value = extractError(e, t('common.loadFailed'))
  } finally {
    runtimeLevelLoading.value = false
  }
}

async function applyRuntimeLevel(level: string) {
  try {
    const data = await logApi.setLevel(level)
    runtimeLevelApplied = data?.level || level
    runtimeLevel.value = runtimeLevelApplied
    message.success(t('logAggregator.levelChanged', { level: runtimeLevelApplied }))
  } catch (e: any) {
    // v-model already moved the select; put it back to what the gateway confirmed.
    runtimeLevel.value = runtimeLevelApplied
    message.error(extractError(e, t('common.operationFailed')))
  }
}

async function exportLogs() {
  exporting.value = true
  try {
    const blob = await logApi.export(filterParams())
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'edgelite-logs.csv'
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
  } catch (e: any) {
    message.error(extractError(e, t('logAggregator.exportFailed')))
  } finally {
    exporting.value = false
  }
}

async function refresh() {
  await Promise.all([loadLogs(), loadStats()])
}

onMounted(() => {
  refresh()
  loadRuntimeLevel()
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
