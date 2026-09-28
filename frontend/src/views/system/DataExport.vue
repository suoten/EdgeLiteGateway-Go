<template>
  <div class="page-container">
    <n-card :title="t('router.dataExport')" size="small">
      <n-form label-placement="left" :label-width="120" inline>
        <n-form-item :label="t('dataExport.deviceId')">
          <n-select
            v-model:value="exportForm.device_id"
            :options="deviceOptions"
            filterable
            clearable
            style="width: 200px;"
            @update:value="onDeviceChange"
          />
        </n-form-item>
        <n-form-item :label="t('dataExport.point')">
          <n-select
            v-model:value="exportForm.point_name"
            :options="pointOptions"
            clearable
            :disabled="!exportForm.device_id"
            style="width: 180px;"
          />
        </n-form-item>
        <n-form-item :label="t('dataExport.startTime')">
          <n-date-picker v-model:value="exportForm.start_time" type="datetime" />
        </n-form-item>
        <n-form-item :label="t('dataExport.endTime')">
          <n-date-picker v-model:value="exportForm.end_time" type="datetime" />
        </n-form-item>
        <n-form-item :label="t('dataExport.format')">
          <n-select v-model:value="exportForm.format" :options="formatOptions" style="width: 120px;" />
        </n-form-item>
        <n-form-item>
          <n-button type="primary" :loading="exporting" @click="doExport">
            {{ t('dataExport.export') }}
          </n-button>
        </n-form-item>
      </n-form>

      <n-divider />

      <n-data-table
        :columns="historyColumns"
        :data="exportHistory"
        :bordered="false"
        size="small"
        :loading="historyLoading"
      />
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, h, computed, onMounted } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { message } from '@/utils/discreteApi'
import { formatDateTime } from '@/utils/datetime'
import { NButton } from 'naive-ui'

const exporting = ref(false)
const historyLoading = ref(false)
const devices = ref<any[]>([])
const exportHistory = ref<any[]>([])

const exportForm = ref({
  device_id: null as string | null,
  point_name: null as string | null,
  start_time: Date.now() - 86400000,
  end_time: Date.now(),
  format: 'csv',
})

const formatOptions = [
  { label: 'CSV', value: 'csv' },
  { label: 'JSON', value: 'json' },
]

const deviceOptions = computed(() =>
  devices.value.map(d => ({ label: d.name, value: d.device_id }))
)

const pointOptions = computed(() => {
  const d = devices.value.find(x => x.device_id === exportForm.value.device_id)
  return (d?.points || []).map((p: any) => ({ label: p.name, value: p.name }))
})

function onDeviceChange() {
  exportForm.value.point_name = null
}

function formatSize(n: any): string {
  const v = Number(n)
  if (!Number.isFinite(v) || v < 0) return '—'
  if (v < 1024) return `${v} B`
  if (v < 1048576) return `${(v / 1024).toFixed(1)} KB`
  return `${(v / 1048576).toFixed(2)} MB`
}

const historyColumns = computed(() => [
  { title: t('dataExport.fileName'), key: 'filename' },
  { title: t('dataExport.device'), key: 'device_id' },
  {
    title: t('dataExport.timeRange'),
    key: 'time_range',
    render: (row: any) => `${formatDateTime(row.start_time)} ~ ${formatDateTime(row.end_time)}`,
  },
  {
    title: t('dataExport.size'),
    key: 'size_bytes',
    render: (row: any) => formatSize(row.size_bytes),
  },
  {
    title: t('dataExport.status'),
    key: 'status',
    render: (row: any) => (row.status === 'success' ? t('common.success') : row.status),
  },
  { title: t('dataExport.createdAt'), key: 'created_at', render: (row: any) => formatDateTime(row.created_at) },
  {
    title: t('common.actions'),
    key: 'actions',
    render: (row: any) =>
      h(NButton, { size: 'small', type: 'primary', text: true, onClick: () => downloadFile(row) }, () => t('dataExport.download')),
  },
])

async function loadDevices() {
  try {
    const res = await http.get('/devices', { params: { page: 1, size: 1000 } })
    devices.value = res.data.data || []
  } catch {}
}

async function doExport() {
  if (!exportForm.value.device_id) {
    message.warning(t('dataExport.selectDevice'))
    return
  }
  exporting.value = true
  try {
    const params: any = {
      device_id: exportForm.value.device_id,
      start_time: new Date(exportForm.value.start_time).toISOString(),
      end_time: new Date(exportForm.value.end_time).toISOString(),
      format: exportForm.value.format,
    }
    if (exportForm.value.point_name) {
      params.point_name = exportForm.value.point_name
    }
    const res = await http.get('/data/export', { params, responseType: 'blob' })
    const url = URL.createObjectURL(res.data)
    const a = document.createElement('a')
    a.href = url
    a.download = `export_${exportForm.value.device_id}_${Date.now()}.${exportForm.value.format}`
    a.click()
    URL.revokeObjectURL(url)
    message.success(t('dataExport.exportSuccess'))
    loadHistory()
  } catch {
    message.error(t('dataExport.exportFailed'))
  } finally {
    exporting.value = false
  }
}

async function loadHistory() {
  historyLoading.value = true
  try {
    const res = await http.get('/data/export/history')
    exportHistory.value = res.data.data || []
  } catch {
  } finally {
    historyLoading.value = false
  }
}

async function downloadFile(row: any) {
  try {
    const res = await http.get(`/data/export/${row.id}/download`, { responseType: 'blob' })
    const url = URL.createObjectURL(res.data)
    const a = document.createElement('a')
    a.href = url
    a.download = row.filename || 'export'
    a.click()
    URL.revokeObjectURL(url)
  } catch {
    message.error(t('dataExport.downloadFailed'))
  }
}

onMounted(() => {
  loadDevices()
  loadHistory()
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
