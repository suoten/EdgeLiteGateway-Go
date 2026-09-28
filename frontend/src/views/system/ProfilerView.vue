<template>
  <div class="page-container">
    <n-card :title="t('router.profiler')" size="small">
      <n-space vertical>
        <n-space>
          <n-select v-model:value="profileType" :options="typeOptions" style="width: 180px;" />
          <n-input-number v-model:value="duration" :min="5" :max="300" />
          <span style="margin-left: 8px; color: #999;">{{ t('profiler.seconds') }}</span>
          <n-button type="primary" :loading="profiling" @click="startProfile">
            {{ t('profiler.start') }}
          </n-button>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <n-divider />

        <div v-if="currentProfile">
          <n-h3>{{ t('profiler.results') }}</n-H3>
          <n-tabs type="line" animated>
            <n-tab-pane name="cpu" :tab="t('profiler.cpu')">
              <n-data-table :columns="cpuColumns" :data="currentProfile.cpu || []" :bordered="false" size="small" :pagination="{ pageSize: 30 }" />
            </n-tab-pane>
            <n-tab-pane name="memory" :tab="t('profiler.memory')">
              <n-data-table :columns="memColumns" :data="currentProfile.memory || []" :bordered="false" size="small" :pagination="{ pageSize: 30 }" />
            </n-tab-pane>
            <n-tab-pane name="goroutines" :tab="t('profiler.goroutines')">
              <n-data-table :columns="goroutineColumns" :data="currentProfile.goroutines || []" :bordered="false" size="small" :pagination="{ pageSize: 30 }" />
            </n-tab-pane>
            <n-tab-pane name="summary" :tab="t('profiler.summary')">
              <n-descriptions bordered :column="2" size="small">
                <n-descriptions-item :label="t('profiler.profileId')">{{ currentProfile.id }}</n-descriptions-item>
                <n-descriptions-item :label="t('profiler.duration')">{{ currentProfile.duration }}s</n-descriptions-item>
                <n-descriptions-item :label="t('profiler.startedAt')">{{ currentProfile.started_at ? formatDateTime(currentProfile.started_at) : '-' }}</n-descriptions-item>
                <n-descriptions-item :label="t('profiler.completedAt')">{{ currentProfile.completed_at ? formatDateTime(currentProfile.completed_at) : '-' }}</n-descriptions-item>
                <n-descriptions-item :label="t('profiler.goroutines')">{{ currentProfile.goroutine_count }}</n-descriptions-item>
                <n-descriptions-item :label="t('profiler.memoryMb')">{{ formatNum(currentProfile.memory_mb, 2) }}</n-descriptions-item>
              </n-descriptions>
            </n-tab-pane>
          </n-tabs>
        </div>

        <n-divider />

        <n-h3>{{ t('profiler.history') }}</n-H3>
        <n-data-table :columns="historyColumns" :data="profileHistory" :bordered="false" size="small" :pagination="{ pageSize: 15 }" />
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message } from '@/utils/discreteApi'
import { formatDateTime } from '@/utils/datetime'
import { formatNum } from '@/utils/format'

const profileType = ref('cpu')
const duration = ref(30)
const profiling = ref(false)
const currentProfile = ref<any>(null)
const profileHistory = ref<any[]>([])

const typeOptions = [
  { label: 'CPU', value: 'cpu' },
  { label: t('profiler.memory'), value: 'memory' },
  { label: 'Goroutines', value: 'goroutines' },
  { label: t('profiler.all'), value: 'all' },
]

const cpuColumns = computed(() => [
  { title: t('profiler.function'), key: 'function', ellipsis: { tooltip: true } },
  { title: t('profiler.samples'), key: 'samples' },
  { title: t('profiler.cpuPct'), key: 'cpu_pct', render: (r: any) => `${formatNum(r.cpu_pct, 2)}%` },
  { title: t('profiler.timeMs'), key: 'duration_ms', render: (r: any) => formatNum(r.duration_ms, 2) },
])

const memColumns = computed(() => [
  { title: t('profiler.function'), key: 'function', ellipsis: { tooltip: true } },
  { title: t('profiler.allocs'), key: 'alloc_count' },
  { title: t('profiler.sizeMb'), key: 'alloc_mb', render: (r: any) => formatNum(r.alloc_mb, 2) },
  { title: t('profiler.liveMb'), key: 'live_mb', render: (r: any) => formatNum(r.live_mb, 2) },
])

const goroutineColumns = computed(() => [
  { title: t('profiler.state'), key: 'state' },
  { title: t('profiler.function'), key: 'function', ellipsis: { tooltip: true } },
  { title: t('profiler.count'), key: 'count' },
  { title: t('profiler.duration'), key: 'duration' },
])

const historyColumns = computed(() => [
  { title: t('profiler.id'), key: 'id' },
  { title: t('profiler.type'), key: 'type' },
  { title: t('profiler.duration'), key: 'duration', render: (r: any) => `${r.duration}s` },
  { title: t('profiler.startedAt'), key: 'started_at', render: (r: any) => (r.started_at ? formatDateTime(r.started_at) : '-') },
  { title: t('profiler.goroutines'), key: 'goroutine_count' },
  { title: t('profiler.memoryMb'), key: 'memory_mb', render: (r: any) => formatNum(r.memory_mb, 2) },
])

async function startProfile() {
  profiling.value = true
  try {
    const res = await http.post('/profiler/start', { type: profileType.value, duration: duration.value })
    message.success(t('profiler.started'))
    const profileId = res.data?.data?.id
    // Wait for profile to complete
    setTimeout(async () => {
      try {
        if (!profileId) return
        const result = await http.get(`/profiler/${profileId}`)
        currentProfile.value = result.data?.data
        message.success(t('profiler.completed'))
      } catch {}
      profiling.value = false
      await refresh()
    }, duration.value * 1000 + 2000)
  } catch (e: any) {
    message.error(extractError(e, t('profiler.startFailed')))
    profiling.value = false
  }
}

async function refresh() {
  try {
    const res = await http.get('/profiler/history')
    profileHistory.value = res.data?.data?.items || []
  } catch {}
}

onMounted(() => { refresh() })
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
