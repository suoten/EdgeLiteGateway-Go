<template>
  <div class="page-container">
    <n-card :title="t('router.platformDashboard')" size="small">
      <n-space vertical>
        <n-space>
          <n-button @click="fetchDashboard" :loading="loading">{{ t('common.refresh') }}</n-button>
        </n-space>

        <n-data-table :columns="columns" :data="platforms" size="small" :bordered="false">
          <template #empty>
            <n-empty :description="t('platformDashboard.noData')" />
          </template>
        </n-data-table>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, h, computed, onMounted } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message } from '@/utils/discreteApi'
import { NTag } from 'naive-ui'
import { formatDateTime } from '@/utils/datetime'

const platforms = ref<any[]>([])
const loading = ref(false)

const stateTagType = (s: string) => {
  switch (s) {
    case 'connected': return 'success'
    case 'connecting': return 'info'
    case 'disconnected': return 'warning'
    case 'error': return 'error'
    default: return 'default'
  }
}

const stateLabel = (s: string) => {
  switch (s) {
    case 'connected': return t('platformDashboard.connected')
    case 'connecting': return t('platformDashboard.connecting')
    case 'disconnected': return t('platformDashboard.disconnected')
    case 'error': return t('platformDashboard.error')
    default: return t('platformDashboard.unknown')
  }
}

const columns = computed(() => [
  { title: t('platformDashboard.platform'), key: 'label', render: (r: any) => r.label || r.platform_name },
  { title: t('platformDashboard.state'), key: 'state', render: (r: any) => h(NTag, { type: stateTagType(r.state), size: 'small' }, () => stateLabel(r.state)) },
  { title: t('platformDashboard.messages'), key: 'messages_today', render: (r: any) => String(r.messages_today ?? 0) },
  { title: t('platformDashboard.errorRate'), key: 'error_rate', render: (r: any) => `${(Number(r.error_rate ?? 0) * 100).toFixed(2)}%` },
  { title: t('platformDashboard.queue'), key: 'queue_backlog', render: (r: any) => String(r.queue_backlog ?? 0) },
  {
    title: t('platformDashboard.heartbeat'),
    key: 'last_heartbeat',
    render: (r: any) => (r.last_heartbeat ? formatDateTime(Number(r.last_heartbeat) * 1000) : '-'),
  },
  {
    title: t('platformDashboard.latencyMs'),
    key: 'latency_ms',
    render: (r: any) => `${Number(r.latency_ms ?? 0).toFixed(2)} ms`,
  },
])

async function fetchDashboard() {
  loading.value = true
  try {
    const res = await http.get('/platforms/dashboard')
    platforms.value = res.data?.data ?? []
  } catch (e: any) {
    message.error(extractError(e, t('platformDashboard.loadFailed')))
  } finally {
    loading.value = false
  }
}

onMounted(fetchDashboard)
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
