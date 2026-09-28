<template>
  <div class="page-container">
    <n-card :title="t('bridge.title')" size="small">
      <n-space vertical>
        <n-space align="center">
          <n-button type="primary" @click="openCreate">{{ t('bridge.create') }}</n-button>
          <n-button @click="refresh" :loading="loading">{{ t('common.refresh') }}</n-button>
          <n-tag v-if="!status.enabled" type="warning" size="small">{{ t('bridge.managerStopped') }}</n-tag>
          <n-tag v-else-if="!status.sink_wired" type="error" size="small">{{ t('bridge.sinkNotWired') }}</n-tag>
          <n-tag v-else type="success" size="small">{{ t('bridge.running') }}</n-tag>
        </n-space>

        <n-alert v-if="!status.enabled || !status.sink_wired" type="warning" :bordered="false">
          {{ t('bridge.notForwardingTip') }}
        </n-alert>

        <n-descriptions label-placement="left" :column="4" bordered size="small">
          <n-descriptions-item :label="t('bridge.configuredBridges')">{{ status.configured_bridges ?? 0 }}</n-descriptions-item>
          <n-descriptions-item :label="t('bridge.activeBridges')">{{ status.active_bridges ?? 0 }}</n-descriptions-item>
          <n-descriptions-item :label="t('bridge.transferred')">{{ status.total_transferred ?? 0 }}</n-descriptions-item>
          <n-descriptions-item :label="t('bridge.transferErrors')">{{ status.total_errors ?? 0 }}</n-descriptions-item>
          <n-descriptions-item :label="t('bridge.droppedSamples')">{{ status.dropped_samples ?? 0 }}</n-descriptions-item>
        </n-descriptions>

        <n-empty v-if="!loading && bridges.length === 0" :description="t('common.noData')" />

        <n-data-table
          v-else
          :columns="bridgeColumns"
          :data="bridges"
          :bordered="false"
          size="small"
          :scroll-x="1180"
          :pagination="{ pageSize: 15 }"
        />
      </n-space>
    </n-card>

    <n-modal v-model:show="showEditor" preset="card" style="width: 720px;" :title="editingId ? t('bridge.editBridge') : t('bridge.createBridge')">
      <n-form label-placement="top">
        <n-grid :cols="2" :x-gap="12">
          <n-form-item-gi :label="t('bridge.name')">
            <n-input v-model:value="form.name" :placeholder="t('bridge.namePlaceholder')" />
          </n-form-item-gi>
          <n-form-item-gi :label="t('common.status')">
            <n-switch v-model:value="form.enabled">
              <template #checked>{{ t('bridge.enabled') }}</template>
              <template #unchecked>{{ t('bridge.disabled') }}</template>
            </n-switch>
          </n-form-item-gi>
          <n-form-item-gi :label="t('bridge.sourceDevice')">
            <n-select v-model:value="form.source_device" :options="deviceOptions" filterable @update:value="onSourceChange" />
          </n-form-item-gi>
          <n-form-item-gi :label="t('bridge.targetDevice')">
            <n-select v-model:value="form.target_device" :options="deviceOptions" filterable @update:value="onTargetChange" />
          </n-form-item-gi>
        </n-grid>

        <n-divider title-placement="left">{{ t('bridge.mappings') }}</n-divider>

        <div v-for="(rule, idx) in form.rules" :key="idx" class="rule-row">
          <n-grid :cols="5" :x-gap="8">
            <n-gi>
              <n-select v-model:value="rule.source_point" :options="sourcePointOptions" filterable tag
                :placeholder="t('bridge.sourcePoint')" />
            </n-gi>
            <n-gi>
              <n-select v-model:value="rule.target_point" :options="targetPointOptions" filterable tag
                :placeholder="t('bridge.targetPoint')" />
            </n-gi>
            <n-gi>
              <n-select v-model:value="rule.conversion_type" :options="conversionOptions" />
            </n-gi>
            <n-gi>
              <n-input-number v-model:value="rule.scale" :show-button="false" size="small"
                :disabled="!needsScale(rule.conversion_type)" :placeholder="t('bridge.scale')" />
            </n-gi>
            <n-gi>
              <n-input-number v-model:value="rule.offset" :show-button="false" size="small"
                :disabled="!needsScale(rule.conversion_type)" :placeholder="t('bridge.offset')" />
            </n-gi>
          </n-grid>
          <n-button text size="tiny" type="error" @click="removeRule(idx)">{{ t('bridge.removeMapping') }}</n-button>
        </div>

        <n-button dashed block size="small" @click="addRule">{{ t('bridge.addMapping') }}</n-button>
        <n-text depth="3" style="font-size: 12px; display: block; margin-top: 8px;">
          {{ t('bridge.rateLimitTip') }}
        </n-text>
      </n-form>

      <template #footer>
        <n-space justify="end">
          <n-button @click="showEditor = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, onUnmounted, ref } from 'vue'
import { NButton, NSwitch, NTag, NText, NSpace } from 'naive-ui'
import http from '@/api/http'
import { bridgeApi, type BridgeConfig, type BridgeRule, type BridgeStatus } from '@/api'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message, dialog } from '@/utils/discreteApi'

const bridges = ref<BridgeConfig[]>([])
const status = ref<Partial<BridgeStatus>>({})
const deviceOptions = ref<{ label: string; value: string }[]>([])
const sourcePointOptions = ref<{ label: string; value: string }[]>([])
const targetPointOptions = ref<{ label: string; value: string }[]>([])
const loading = ref(false)
const saving = ref(false)
const showEditor = ref(false)
const editingId = ref<string | null>(null)
let refreshTimer: number | null = null

const emptyRule = (): BridgeRule => ({
  source_point: '',
  target_point: '',
  conversion_type: 'passthrough',
  scale: 1,
  offset: 0,
  enabled: true,
})

const form = ref<Omit<BridgeConfig, 'id'>>({
  name: '',
  source_device: null,
  target_device: null,
  enabled: true,
  rules: [emptyRule()],
})

const conversionOptions = computed(() => [
  { label: t('bridge.convPassthrough'), value: 'passthrough' },
  { label: t('bridge.convLinear'), value: 'linear' },
  { label: t('bridge.convBoolToInt'), value: 'bool_to_int' },
  { label: t('bridge.convIntToBool'), value: 'int_to_bool' },
])

function needsScale(type: string) {
  return type === 'linear'
}

function stat(row: BridgeConfig, key: string): number {
  const v = (row.stats as any)?.[key]
  return typeof v === 'number' ? v : 0
}

const bridgeColumns = computed(() => [
  { title: t('bridge.name'), key: 'name', width: 160, ellipsis: { tooltip: true } },
  {
    title: t('bridge.sourceDevice'),
    key: 'source_device',
    width: 220,
    render: (row: BridgeConfig) => h('div', { class: 'cell-stack' }, [
      row.source_device,
      h(NText, { depth: 3, style: 'font-size: 12px' }, () => sourceDeviceName(row.source_device)),
    ]),
  },
  {
    title: t('bridge.targetDevice'),
    key: 'target_device',
    width: 220,
    render: (row: BridgeConfig) => h('div', { class: 'cell-stack' }, [
      row.target_device,
      h(NText, { depth: 3, style: 'font-size: 12px' }, () => sourceDeviceName(row.target_device)),
    ]),
  },
  { title: t('bridge.mappingCount'), key: 'rule_count', width: 90 },
  {
    title: t('bridge.transferred'),
    key: 'transferred',
    width: 90,
    render: (row: BridgeConfig) => String(stat(row, 'total_transferred')),
  },
  {
    title: t('bridge.transferErrors'),
    key: 'errors',
    width: 90,
    render: (row: BridgeConfig) => {
      const errors = stat(row, 'total_errors')
      const lastError = (row.stats as any)?.last_error || ''
      return errors > 0
        ? h(NTag, { type: 'error', size: 'small', title: lastError }, () => String(errors))
        : h('span', '0')
    },
  },
  { title: t('bridge.lastTransfer'), key: 'last_transfer', width: 170, render: (row: BridgeConfig) => (row.stats as any)?.last_transfer_at || '-' },
  {
    title: t('common.status'),
    key: 'enabled',
    width: 90,
    render: (row: BridgeConfig) => h(NSwitch, {
      value: row.enabled,
      'onUpdate:value': (val: boolean) => void toggleBridge(row, val),
    }),
  },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 130,
    render: (row: BridgeConfig) => h(NSpace, { size: 8 }, () => [
      h(NButton, { size: 'tiny', text: true, type: 'primary', onClick: () => openEdit(row) }, () => t('bridge.edit')),
      h(NButton, { size: 'tiny', text: true, type: 'error', onClick: () => confirmDelete(row) }, () => t('common.delete')),
    ]),
  },
])

function sourceDeviceName(deviceID: string | null) {
  const hit = deviceOptions.value.find((d) => d.value === deviceID)
  return hit ? hit.label : deviceID || '-'
}

async function loadDevicePoints(deviceID: string | null): Promise<{ label: string; value: string }[]> {
  if (!deviceID) return []
  try {
    const res = await http.get(`/devices/${encodeURIComponent(deviceID)}`)
    const points = res.data?.data?.points || []
    return points.map((p: any) => ({ label: `${p.name}${p.access_mode ? ` (${p.access_mode})` : ''}`, value: p.name }))
  } catch {
    return []
  }
}

async function onSourceChange(deviceID: string | null) {
  sourcePointOptions.value = await loadDevicePoints(deviceID)
}

async function onTargetChange(deviceID: string | null) {
  targetPointOptions.value = await loadDevicePoints(deviceID)
}

function openCreate() {
  editingId.value = null
  form.value = { name: '', source_device: null, target_device: null, enabled: true, rules: [emptyRule()] }
  sourcePointOptions.value = []
  targetPointOptions.value = []
  showEditor.value = true
}

async function openEdit(row: BridgeConfig) {
  editingId.value = row.id || null
  form.value = {
    name: row.name,
    source_device: row.source_device,
    target_device: row.target_device,
    enabled: row.enabled,
    rules: (row.rules || []).map((r) => ({
      rule_id: r.rule_id,
      source_point: r.source_point,
      target_point: r.target_point,
      conversion_type: r.conversion_type || 'passthrough',
      scale: r.scale ?? 1,
      offset: r.offset ?? 0,
      enabled: r.enabled !== false,
    })),
  }
  if (form.value.rules.length === 0) form.value.rules = [emptyRule()]
  sourcePointOptions.value = await loadDevicePoints(row.source_device)
  targetPointOptions.value = await loadDevicePoints(row.target_device)
  showEditor.value = true
}

function addRule() {
  form.value.rules.push(emptyRule())
}

function removeRule(idx: number) {
  if (form.value.rules.length <= 1) {
    message.warning(t('bridge.mappingRequired'))
    return
  }
  form.value.rules.splice(idx, 1)
}

async function save() {
  if (!form.value.name.trim()) {
    message.warning(t('bridge.nameRequired'))
    return
  }
  if (form.value.rules.some((r) => !r.source_point || !r.target_point)) {
    message.warning(t('bridge.mappingFieldsRequired', { index: form.value.rules.findIndex((r) => !r.source_point || !r.target_point) + 1 }))
    return
  }
  saving.value = true
  try {
    if (editingId.value) {
      await bridgeApi.update(editingId.value, { ...form.value, id: editingId.value } as BridgeConfig)
    } else {
      await bridgeApi.create(form.value as BridgeConfig)
    }
    message.success(t('bridge.saveSuccess'))
    showEditor.value = false
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('bridge.saveFailed')))
  } finally {
    saving.value = false
  }
}

async function toggleBridge(row: BridgeConfig, val: boolean) {
  if (!row.id) return
  try {
    if (val) await bridgeApi.enable(row.id)
    else await bridgeApi.disable(row.id)
    row.enabled = val
    message.success(t('common.saveSuccess'))
  } catch (e: any) {
    message.error(extractError(e, t('bridge.toggleFailed')))
  }
}

function confirmDelete(row: BridgeConfig) {
  dialog.warning({
    title: t('common.confirmDeleteName', { name: row.name || row.id || '' }),
    content: t('common.confirmDeleteDesc'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => { void doDelete(row) },
  })
}

async function doDelete(row: BridgeConfig) {
  if (!row.id) return
  try {
    await bridgeApi.remove(row.id)
    message.success(t('bridge.deleteSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('bridge.deleteFailed')))
  }
}

async function refresh() {
  loading.value = true
  try {
    const [listRes, statusRes, devRes] = await Promise.all([
      bridgeApi.list(),
      bridgeApi.status(),
      http.get('/devices', { params: { page: 1, size: 1000 } }),
    ])
    bridges.value = listRes?.bridges || []
    status.value = statusRes || {}
    const devices = devRes.data?.data || []
    deviceOptions.value = devices.map((d: any) => ({ label: d.name || d.device_id, value: d.device_id }))
  } catch (e: any) {
    message.error(extractError(e, t('bridge.loadFailed')))
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  refresh()
  // Transfer counters only move while data flows, so a poll is what turns the
  // table from a config dump into evidence that the bridge is actually working.
  refreshTimer = window.setInterval(() => { void refreshStats() }, 5000)
})

onUnmounted(() => {
  if (refreshTimer !== null) window.clearInterval(refreshTimer)
})

async function refreshStats() {
  try {
    const [listRes, statusRes] = await Promise.all([bridgeApi.list(), bridgeApi.status()])
    bridges.value = listRes?.bridges || []
    status.value = statusRes || {}
  } catch {
    // A failed background poll is not an error toast; the next tick retries.
  }
}
</script>

<style scoped>
.page-container { padding: 16px; }
.rule-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.cell-stack { display: flex; flex-direction: column; line-height: 1.3; }
</style>
