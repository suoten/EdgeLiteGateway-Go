<template>
  <div class="page-container">
    <n-card :title="t('router.dbMonitor')" size="small">
      <n-space vertical>
        <!-- A failed load is shown where the numbers would have been: a blank
             table and zeros are not the same answer as "the service refused". -->
        <n-alert v-if="statsError" type="error" size="small" :show-icon="true">
          {{ t('dbMonitor.loadFailed') }} · {{ statsError }}
        </n-alert>
        <!-- The service reports measured:false when it could not open the file;
             without this the zeros below read as an empty database. -->
        <n-alert v-else-if="dbStats.measured === false" type="warning" size="small" :show-icon="true">
          {{ t('dbMonitor.notMeasured') }}<template v-if="dbStats.measure_error"> · {{ dbStats.measure_error }}</template>
        </n-alert>

        <!-- DB Stats -->
        <n-descriptions :title="t('dbMonitor.stats')" bordered :column="3" size="small">
          <n-descriptions-item :label="t('dbMonitor.dbSize')">
            {{ formatSize(dbStats.db_size) }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('dbMonitor.tableCount')">
            {{ formatNumber(dbStats.table_count) }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('dbMonitor.totalRows')">
            {{ formatNumber(dbStats.total_rows) }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('dbMonitor.walSize')">
            {{ formatSize(dbStats.wal_size) }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('dbMonitor.pageSize')">
            {{ formatSize(dbStats.page_size) }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('dbMonitor.freeNodePages')">
            {{ formatNumber(dbStats.free_node_pages) }}
          </n-descriptions-item>
        </n-descriptions>

        <!-- Table Details -->
        <n-divider />
        <n-h3>{{ t('dbMonitor.tableDetails') }}</n-h3>
        <n-alert v-if="tablesError" type="error" size="small" :show-icon="true" style="margin-bottom: 8px">
          {{ t('dbMonitor.loadFailed') }} · {{ tablesError }}
        </n-alert>
        <n-data-table
          :columns="tableColumns"
          :data="tableData"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 20 }"
        />

        <!-- Slow Queries -->
        <n-divider />
        <n-h3>{{ t('dbMonitor.slowQueries') }}</n-h3>
        <n-alert type="info" size="small" :show-icon="true" style="margin-bottom: 8px">
          {{ t('dbMonitor.slowQueriesUnsupported') }}
        </n-alert>
        <n-data-table
          :columns="slowQueryColumns"
          :data="slowQueries"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 10 }"
        />

        <!-- Actions -->
        <n-divider />
        <n-space>
          <n-button type="warning" :loading="optimizing" @click="optimizeDb">
            {{ t('dbMonitor.vacuum') }}
          </n-button>
          <n-button type="error" :loading="reindexing" @click="reindexDb">
            {{ t('dbMonitor.reindex') }}
          </n-button>
          <n-button @click="refresh">
            {{ t('common.refresh') }}
          </n-button>
        </n-space>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { message } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'

const dbStats = ref<any>({})
const tableData = ref<any[]>([])
const slowQueries = ref<any[]>([])
const statsError = ref('')
const tablesError = ref('')
const optimizing = ref(false)
const reindexing = ref(false)

const tableColumns = computed(() => [
  { title: t('dbMonitor.tableName'), key: 'name', minWidth: 180, ellipsis: { tooltip: true } },
  { title: t('dbMonitor.rowCount'), key: 'row_count', width: 110, render: (r: any) => formatNumber(r.row_count) },
  // Sizes come from SQLite's dbstat module, which is not in every build. When
  // the backend cannot measure, the keys are absent and "-" is honest while
  // "0 B" would claim the table is empty.
  { title: t('dbMonitor.dataSize'), key: 'data_size', width: 110, render: (r: any) => formatSize(r.data_size) },
  { title: t('dbMonitor.indexSize'), key: 'index_size', width: 110, render: (r: any) => formatSize(r.index_size) },
  { title: t('dbMonitor.totalSize'), key: 'total_size', width: 110, render: (r: any) => formatSize(r.total_size) },
])

const slowQueryColumns = computed(() => [
  { title: t('dbMonitor.query'), key: 'query', ellipsis: { tooltip: true } },
  { title: t('dbMonitor.duration'), key: 'duration', render: (r: any) => `${r.duration}ms` },
  { title: t('dbMonitor.timestamp'), key: 'timestamp' },
])

// A null means the backend could not measure this one, which is not the same
// answer as a measured 0 -- so it renders as a dash instead of "0 B".
function formatSize(bytes: number | null | undefined): string {
  if (bytes == null) return '-'
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  while (bytes >= 1024 && i < units.length - 1) {
    bytes /= 1024
    i++
  }
  return `${bytes.toFixed(2)} ${units[i]}`
}

function formatNumber(n: number | null | undefined): string {
  if (n == null) return '-'
  if (!n) return '0'
  return n.toLocaleString()
}

async function refresh() {
  // Each section loads on its own footing. Awaiting everything together let one
  // honest 503 (the monitor refusing to list tables it cannot read) blank the
  // statistics that had loaded fine, so the page lost more than it reported.
  const [statsRes, tableRes, slowRes] = await Promise.allSettled([
    http.get('/db-monitor/stats'),
    http.get('/db-monitor/tables'),
    // /slow-queries answers 501 in a build that times no statements; the
    // standing notice below already says so.
    http.get('/db-monitor/slow-queries'),
  ])
  statsError.value = ''
  tablesError.value = ''
  if (statsRes.status === 'fulfilled') {
    // The axios payload is the {code, message, data} envelope; reading
    // res.data gave the envelope, so every stat rendered blank.
    dbStats.value = statsRes.value.data?.data ?? {}
  } else {
    dbStats.value = {}
    statsError.value = extractError(statsRes.reason, t('dbMonitor.loadFailed'))
  }
  if (tableRes.status === 'fulfilled') {
    const tables = tableRes.value.data?.data
    tableData.value = Array.isArray(tables) ? tables : []
  } else {
    tableData.value = []
    tablesError.value = extractError(tableRes.reason, t('dbMonitor.loadFailed'))
  }
  const slow = slowRes.status === 'fulfilled' ? slowRes.value.data?.data : null
  slowQueries.value = Array.isArray(slow) ? slow : (Array.isArray(slow?.queries) ? slow.queries : [])
}

async function optimizeDb() {
  optimizing.value = true
  try {
    const res = await http.post('/db-monitor/vacuum').then((r) => r.data.data)
    // The endpoint answers 200 both when VACUUM ran and when it did not; the
    // reason it names is SQLite's own error.
    if (res?.status === 'vacuum_skipped') message.warning(withReason(t('dbMonitor.vacuumSkipped'), res.reason))
    else message.success(t('dbMonitor.vacuumSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('dbMonitor.vacuumFailed')))
  } finally {
    optimizing.value = false
  }
}

async function reindexDb() {
  reindexing.value = true
  try {
    const res = await http.post('/db-monitor/reindex').then((r) => r.data.data)
    if (res?.status === 'reindex_skipped') message.warning(withReason(t('dbMonitor.reindexSkipped'), res.reason))
    else message.success(t('dbMonitor.reindexSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('dbMonitor.reindexFailed')))
  } finally {
    reindexing.value = false
  }
}

function withReason(label: string, reason?: string): string {
  return reason ? `${label}: ${reason}` : label
}

onMounted(() => {
  refresh()
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
