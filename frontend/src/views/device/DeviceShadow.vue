<template>
  <div class="page-container">
    <n-card :title="t('router.deviceShadow')" size="small">
      <n-space vertical>
        <n-space align="center">
          <n-button size="small" :loading="loading" @click="loadAll">{{ t('common.refresh') }}</n-button>
          <n-select
            v-model:value="selectedDeviceId"
            :options="deviceOptions"
            :placeholder="t('shadow.selectDevice')"
            size="small"
            style="width: 260px"
            filterable
            clearable
            @update:value="viewDeviceShadow"
          />
        </n-space>

        <n-data-table
          :columns="columns"
          :data="shadows"
          :bordered="false"
          size="small"
          :loading="loading"
          :pagination="{ pageSize: 20 }"
        />
      </n-space>
    </n-card>

    <n-modal v-model:show="showDetail" preset="card" style="width: 720px" :title="t('shadow.shadowOf', { id: detail?.device_id || '' })">
      <n-spin :show="detailLoading">
        <template v-if="detail">
          <n-descriptions bordered :column="2" size="small">
            <n-descriptions-item :label="t('shadow.version')">{{ detail.version }}</n-descriptions-item>
            <n-descriptions-item :label="t('shadow.lastUpdated')">{{ detail.last_updated ? formatDateTime(detail.last_updated) : '-' }}</n-descriptions-item>
          </n-descriptions>

          <n-h4>{{ t('shadow.reported') }}</n-h4>
          <n-descriptions v-if="toRows(detail.reported).length" bordered :column="2" size="small" label-placement="left">
            <n-descriptions-item v-for="row in toRows(detail.reported)" :key="row.key" :label="row.key">{{ row.value }}</n-descriptions-item>
          </n-descriptions>
          <n-text v-else depth="3">-</n-text>

          <n-h4>{{ t('shadow.desired') }}</n-h4>
          <n-descriptions v-if="toRows(detail.desired).length" bordered :column="2" size="small" label-placement="left">
            <n-descriptions-item v-for="row in toRows(detail.desired)" :key="row.key" :label="row.key">{{ row.value }}</n-descriptions-item>
          </n-descriptions>
          <n-text v-else depth="3">-</n-text>

          <n-h4>{{ t('shadow.delta') }}</n-h4>
          <n-tag v-if="deltaCount > 0" type="warning" size="small">{{ t('shadow.deltaCount') }}: {{ deltaCount }}</n-tag>
          <n-descriptions v-if="deltaCount > 0" bordered :column="2" size="small" label-placement="left">
            <n-descriptions-item v-for="row in toRows(detail.delta)" :key="row.key" :label="row.key">{{ row.value }}</n-descriptions-item>
          </n-descriptions>
          <n-text v-else depth="3">{{ t('shadow.noDelta') }}</n-text>

          <n-space style="margin-top: 16px">
            <n-button size="small" type="primary" @click="openEditor('desired')">{{ t('shadow.setDesired') }}</n-button>
            <n-button size="small" @click="openEditor('reported')">{{ t('shadow.reportState') }}</n-button>
            <n-button size="small" @click="clearDesired">{{ t('shadow.clearDesired') }}</n-button>
          </n-space>
        </template>
      </n-spin>
    </n-modal>

    <n-modal v-model:show="showEditor" preset="card" style="width: 560px" :title="editorMode === 'desired' ? t('shadow.setDesired') : t('shadow.reportState')">
      <n-space vertical>
        <n-input
          v-model:value="editorJson"
          type="textarea"
          :rows="8"
          :placeholder="t('shadow.jsonPlaceholder')"
        />
        <n-text v-if="jsonError" type="error">{{ jsonError }}</n-text>
        <n-space justify="end">
          <n-button size="small" @click="showEditor = false">{{ t('common.cancel') }}</n-button>
          <n-button size="small" type="primary" :loading="saving" :disabled="saving" @click="saveEditor">{{ t('common.save') }}</n-button>
        </n-space>
      </n-space>
    </n-modal>
  </div>
</template>

<script setup lang="ts">
import { computed, h, ref } from 'vue'
import { NButton, NTag } from 'naive-ui'
import { t } from '@/i18n'
import { message, dialog } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { formatDateTime } from '@/utils/datetime'
import { deviceApi, shadowApi } from '@/api'

const shadows = ref<any[]>([])
const devices = ref<any[]>([])
const loading = ref(false)
const selectedDeviceId = ref<string | null>(null)

const showDetail = ref(false)
const detailLoading = ref(false)
const detail = ref<any>(null)

const showEditor = ref(false)
const editorMode = ref<'desired' | 'reported'>('desired')
const editorJson = ref('')
const jsonError = ref('')

const deviceOptions = computed(() =>
  devices.value.map((d) => ({ label: d.name ? `${d.name} (${d.device_id})` : d.device_id, value: d.device_id })),
)

const deltaCount = computed(() => Object.keys(detail.value?.delta || {}).length)

function toRows(obj: Record<string, any> | undefined): { key: string; value: any }[] {
  if (!obj) return []
  return Object.entries(obj).map(([key, value]) => ({
    key,
    value: typeof value === 'object' && value !== null ? JSON.stringify(value) : String(value),
  }))
}

const columns = computed(() => [
  { title: t('shadow.deviceId'), key: 'device_id' },
  { title: t('shadow.version'), key: 'version', width: 90 },
  { title: t('shadow.reportedCount'), key: 'reported_count', width: 110 },
  { title: t('shadow.desiredCount'), key: 'desired_count', width: 110 },
  {
    title: t('shadow.deltaCount'),
    key: 'delta_count',
    width: 110,
    render: (r: any) => h(NTag, { type: r.delta_count > 0 ? 'warning' : 'default', size: 'small' }, () => r.delta_count),
  },
  { title: t('shadow.lastUpdated'), key: 'last_updated', width: 180, render: (r: any) => (r.last_updated ? formatDateTime(r.last_updated) : '-') },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 260,
    render: (r: any) =>
      h('div', { style: 'display:flex;gap:6px' }, [
        h(NButton, { size: 'small', onClick: () => viewShadow(r.device_id) }, () => t('common.detail')),
        h(NButton, { size: 'small', type: 'error', secondary: true, onClick: () => removeShadow(r.device_id) }, () => t('common.delete')),
      ]),
  },
])

async function loadAll() {
  loading.value = true
  try {
    const [shadowList, devList] = await Promise.all([
      shadowApi.list(),
      deviceApi.list({ page: 1, size: 500 }).then((r) => r.data || []),
    ])
    shadows.value = shadowList || []
    devices.value = devList
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  } finally {
    loading.value = false
  }
}

async function viewShadow(deviceId: string) {
  showDetail.value = true
  detailLoading.value = true
  try {
    detail.value = await shadowApi.get(deviceId)
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
    showDetail.value = false
  } finally {
    detailLoading.value = false
  }
}

function viewDeviceShadow(deviceId: string | null) {
  if (deviceId) viewShadow(deviceId)
}

function removeShadow(deviceId: string) {
  dialog.warning({
    title: t('common.confirmDeleteName', { name: deviceId }),
    content: t('common.confirmDeleteDesc'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => { void doRemoveShadow(deviceId) },
  })
}

async function doRemoveShadow(deviceId: string) {
  try {
    await shadowApi.delete(deviceId)
    message.success(t('common.deleteSuccess'))
    await loadAll()
  } catch (e: any) {
    message.error(extractError(e, t('common.deleteFailed')))
  }
}

// 清除期望状态会一次性丢掉设备上全部待下发值，不可撤销，必须先确认
function clearDesired() {
  if (!detail.value) return
  dialog.warning({
    title: t('shadow.clearDesired'),
    content: t('shadow.clearDesiredConfirm', { id: detail.value.device_id }),
    positiveText: t('common.ok'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => { void doClearDesired() },
  })
}

async function doClearDesired() {
  if (!detail.value) return
  try {
    await shadowApi.updateDesired(detail.value.device_id, {})
    message.success(t('shadow.cleared'))
    detail.value = await shadowApi.get(detail.value.device_id)
    await loadAll()
  } catch (e: any) {
    message.error(extractError(e, t('common.operationFailed')))
  }
}

function openEditor(mode: 'desired' | 'reported') {
  if (!detail.value) return
  editorMode.value = mode
  const source = mode === 'desired' ? detail.value.desired : detail.value.reported
  editorJson.value = JSON.stringify(source || {}, null, 2)
  jsonError.value = ''
  showEditor.value = true
}

const saving = ref(false)

async function saveEditor() {
  if (!detail.value || saving.value) return
  let parsed: Record<string, any>
  try {
    parsed = JSON.parse(editorJson.value || '{}')
  } catch {
    jsonError.value = t('shadow.invalidJson')
    return
  }
  jsonError.value = ''
  saving.value = true
  try {
    if (editorMode.value === 'desired') {
      await shadowApi.updateDesired(detail.value.device_id, parsed)
    } else {
      await shadowApi.updateReported(detail.value.device_id, parsed)
    }
    message.success(t('shadow.saved'))
    showEditor.value = false
    detail.value = await shadowApi.get(detail.value.device_id)
    await loadAll()
  } catch (e: any) {
    message.error(extractError(e, t('common.operationFailed')))
  } finally {
    saving.value = false
  }
}

loadAll()
</script>

<style scoped>
.page-container {
  padding: 12px;
}
</style>
