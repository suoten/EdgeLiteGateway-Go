<template>
  <n-space vertical>
    <n-form inline v-if="points.length">
      <n-form-item :label="t('deviceDetail.selectPoint')">
        <n-select v-model:value="selectedPoint" :options="pointOptions" style="width: 200px" />
      </n-form-item>
      <n-form-item :label="t('common.value')">
        <n-input v-model:value="writeValue" placeholder="0" style="width: 150px" />
      </n-form-item>
      <n-button type="primary" :loading="writing" :disabled="!selectedPoint || !auth.hasPerm('device:write')" @click="handleWrite">
        {{ t('deviceDetail.write') }}
      </n-button>
    </n-form>
    <n-empty v-else :description="t('deviceDetail.noWritablePoints')" size="small" />
    <n-alert v-if="!auth.hasPerm('device:write')" type="warning" size="small">
      {{ t('common.permissionDenied') }}
    </n-alert>

    <n-divider style="margin: 8px 0 4px" />
    <n-space align="center" justify="space-between" style="width: 100%">
      <n-text strong>{{ t('deviceDetail.writeAuditLog') }}</n-text>
      <n-space align="center">
        <n-input
          v-model:value="auditFilterText"
          size="small"
          clearable
          :placeholder="t('deviceDetail.auditFilterPlaceholder')"
          style="width: 180px"
        />
        <n-button size="small" :loading="auditLoading" @click="loadAudit">{{ t('common.refresh') }}</n-button>
      </n-space>
    </n-space>
    <n-alert v-if="auditError" type="error" size="small">{{ auditError }}</n-alert>
    <n-data-table
      size="small"
      :columns="auditColumns"
      :data="filteredAuditRows"
      :max-height="260"
      :pagination="{ pageSize: 10, size: 'small' }"
    >
      <template #empty>
        <n-empty :description="t('common.noData')" size="small" />
      </template>
    </n-data-table>
  </n-space>
</template>

<script setup lang="ts">
import { ref, computed, h, onMounted, watch } from 'vue'
import { useDeviceDetailConsumer } from '../composables/useDeviceDetail'
import { deviceApi } from '@/api'
import { t } from '@/i18n'
import { formatDateTime } from '@/utils/datetime'
import { message } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { isReadOnlyPoint } from '@/utils/pointAccess'
import { NSpace, NForm, NFormItem, NSelect, NInput, NButton, NEmpty, NAlert, NDataTable, NDivider, NText, NTag } from 'naive-ui'
import { useAuthStore } from '@/stores/auth'

const { device } = useDeviceDetailConsumer()
const auth = useAuthStore()
const selectedPoint = ref<string | null>(null)
const writeValue = ref('')
const writing = ref(false)

const points = computed(() => (device.value?.points ?? []).filter(p => !isReadOnlyPoint(p)))

const pointOptions = computed(() => points.value.map(p => ({ label: p.name, value: p.name })))

// ─── 写操作审计：后端 audit_logs 中本设备的 device_write 记录 ───
type WriteAuditRow = Record<string, any>

const auditRows = ref<WriteAuditRow[]>([])
const auditLoading = ref(false)
const auditError = ref('')
const auditFilterText = ref('')

async function loadAudit() {
  const id = device.value?.device_id
  if (!id) {
    auditRows.value = []
    return
  }
  auditLoading.value = true
  auditError.value = ''
  try {
    const data = await deviceApi.getWriteAudit(id, 100)
    auditRows.value = Array.isArray(data) ? data : []
  } catch (e: any) {
    auditRows.value = []
    auditError.value = extractError(e) || t('http.requestFailed')
  } finally {
    auditLoading.value = false
  }
}

onMounted(loadAudit)
watch(() => device.value?.device_id, loadAudit)

const filteredAuditRows = computed(() => {
  const q = auditFilterText.value.trim().toLowerCase()
  if (!q) return auditRows.value
  return auditRows.value.filter(r =>
    [r.point, r.result, r.error, r.username].some(v => String(v ?? '').toLowerCase().includes(q))
  )
})

function fmtVal(v: any): string {
  if (v === null || v === undefined || v === '') return '-'
  return typeof v === 'object' ? JSON.stringify(v) : String(v)
}

const auditColumns = computed(() => [
  { title: t('deviceDetail.auditTime'), key: 'timestamp', width: 160, render: (r: WriteAuditRow) => (r.timestamp ? formatDateTime(r.timestamp) : '-') },
  { title: t('deviceDetail.auditUser'), key: 'username', width: 100, render: (r: WriteAuditRow) => r.username || '-' },
  { title: t('deviceDetail.auditPoint'), key: 'point', width: 120 },
  { title: t('deviceDetail.auditOldValue'), key: 'old_value', width: 90, render: (r: WriteAuditRow) => fmtVal(r.old_value) },
  { title: t('deviceDetail.auditNewValue'), key: 'value', width: 90, render: (r: WriteAuditRow) => fmtVal(r.value) },
  {
    title: t('deviceDetail.auditResult'), key: 'result', width: 90,
    render: (r: WriteAuditRow) => h(NTag, { size: 'small', type: r.result === 'success' ? 'success' : 'error' }, { default: () => r.result || '-' }),
  },
  { title: t('deviceDetail.auditErrorMsg'), key: 'error', minWidth: 140, ellipsis: { tooltip: true }, render: (r: WriteAuditRow) => r.error || '-' },
])

async function handleWrite() {
  if (!device.value?.device_id || !selectedPoint.value) return
  if (!auth.hasPerm('device:write')) { message.warning(t('common.permissionDenied')); return }
  const num = parseFloat(writeValue.value)
  if (Number.isNaN(num)) {
    message.error(t('common.invalidNumber'))
    return
  }
  writing.value = true
  try {
    await deviceApi.writePoint(device.value.device_id, selectedPoint.value, num)
    message.success(t('deviceDetail.writeSuccess', { point: selectedPoint.value, value: num }))
  } catch (e) {
    message.error(extractError(e) || t('deviceDetail.writeFailed'))
  } finally {
    writing.value = false
    // The trail records rejected writes too, so refresh either way.
    loadAudit()
  }
}
</script>
