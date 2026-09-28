<template>
  <div class="page-container">
    <n-card :title="t('router.linkage')" size="small">
      <n-space vertical>
        <n-alert v-if="statusWarning" type="warning" :show-icon="false" size="small">
          {{ statusWarning }}
        </n-alert>
        <n-space>
          <n-button type="primary" @click="openCreate">{{ t('linkage.createRule') }}</n-button>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <n-data-table
          :columns="ruleColumns"
          :data="linkageRules"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 15 }"
        />
      </n-space>
    </n-card>

    <n-modal v-model:show="showCreate" preset="card" style="width: 600px;" :title="t('linkage.createRule')">
      <n-form label-placement="top">
        <n-form-item :label="t('linkage.ruleName')">
          <n-input v-model:value="newRule.name" />
        </n-form-item>
        <n-form-item :label="t('linkage.sourceDevice')">
          <n-select v-model:value="newRule.source_device_id" :options="deviceOptions" filterable @update:value="onSourceChange" />
        </n-form-item>
        <n-form-item :label="t('linkage.sourcePoint')">
          <n-select v-model:value="newRule.source_point" :options="sourcePointOptions" filterable tag
            :placeholder="t('linkage.pickSourceDeviceFirst')" />
        </n-form-item>
        <n-form-item :label="t('linkage.condition')">
          <n-select v-model:value="newRule.condition_op" :options="conditionOptions" />
        </n-form-item>
        <n-form-item :label="t('linkage.threshold')">
          <n-input-number v-model:value="newRule.threshold" />
        </n-form-item>
        <n-form-item :label="t('linkage.targetDevice')">
          <n-select v-model:value="newRule.target_device_id" :options="deviceOptions" filterable @update:value="onTargetChange" />
        </n-form-item>
        <n-form-item :label="t('linkage.targetPoint')">
          <n-select v-model:value="newRule.target_point" :options="targetPointOptions" filterable tag
            :placeholder="t('linkage.pickTargetDeviceFirst')" />
        </n-form-item>
        <n-form-item :label="t('linkage.targetValue')">
          <n-input v-model:value="newRule.target_value" :placeholder="t('linkage.targetValueTip')" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showCreate = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" @click="createRule" :loading="creating">{{ t('common.save') }}</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onBeforeUnmount, onMounted, ref } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message, dialog } from '@/utils/discreteApi'
import { NButton, NTag, NSwitch, NTooltip } from 'naive-ui'
import { linkageApi, type LinkageRule, type LinkageStatus } from '@/api/index'

const linkageRules = ref<LinkageRule[]>([])
const status = ref<LinkageStatus | null>(null)
const deviceOptions = ref<any[]>([])
const sourcePointOptions = ref<any[]>([])
const targetPointOptions = ref<any[]>([])
const showCreate = ref(false)
const creating = ref(false)
const newRule = ref({ name: '', source_device_id: null as string | null, source_point: null as string | null, condition_op: '>', threshold: 0, target_device_id: null as string | null, target_point: null as string | null, target_value: '' })

// The evaluator stores rules but only fires them while it is fed by the collector
// and allowed to write. A page that hides that state reads as "my rule is broken"
// when the runtime never started.
const statusWarning = computed(() => {
  const s = status.value
  if (!s) return ''
  if (!s.running || !s.feed_configured) return t('linkage.notRunningTip')
  if (!s.sink_wired) return t('linkage.sinkNotWired')
  if (!s.persistence) return t('linkage.noPersistence')
  return ''
})

const conditionOptions = [
  { label: '>', value: '>' },
  { label: '<', value: '<' },
  { label: '>=', value: '>=' },
  { label: '<=', value: '<=' },
  { label: '==', value: '==' },
  { label: '!=', value: '!=' },
]

function runtimeCell(row: LinkageRule) {
  const rt = row.runtime
  if (!rt) {
    return h(NTag, { size: 'small', type: 'default' }, () => t('linkage.notLoaded'))
  }
  if (rt.last_error) {
    return h(NTooltip, null, {
      trigger: () => h(NTag, { size: 'small', type: 'error' }, () => t('linkage.errorCount', { n: rt.live_errors })),
      default: () => rt.last_error,
    })
  }
  if (!rt.watching) {
    return h(NTag, { size: 'small', type: 'warning' }, () => t('linkage.noSample'))
  }
  const label = rt.satisfied ? t('linkage.armed') : t('linkage.watching')
  return h(NTag, { size: 'small', type: rt.satisfied ? 'info' : 'default' }, () => `${label}: ${rt.last_value}`)
}

const ruleColumns = computed(() => [
  { title: t('linkage.ruleName'), key: 'name', ellipsis: { tooltip: true } },
  { title: t('linkage.source'), key: 'source', render: (r: LinkageRule) => `${r.source_device_name}/${r.source_point}` },
  { title: t('linkage.condition'), key: 'condition', render: (r: LinkageRule) => `${r.condition_op} ${r.threshold}` },
  { title: t('linkage.target'), key: 'target', render: (r: LinkageRule) => `${r.target_device_name}/${r.target_point}=${r.target_value}` },
  { title: t('linkage.runtime'), key: 'runtime', render: runtimeCell, width: 150 },
  { title: t('linkage.triggered'), key: 'trigger_count', width: 90 },
  { title: t('linkage.lastTriggered'), key: 'last_triggered_at', width: 160, render: (r: LinkageRule) => r.last_triggered_at || '-' },
  {
    title: t('common.status'),
    key: 'enabled',
    width: 80,
    render: (r: LinkageRule) => h(NSwitch, { value: r.enabled, 'onUpdateValue': (val: boolean) => toggleRule(r, val) }),
  },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 80,
    render: (row: LinkageRule) => h(NButton, { size: 'small', type: 'error', text: true, onClick: () => deleteRule(row) }, () => t('common.delete')),
  },
])

async function loadDevicePoints(deviceID: string | null, writableOnly = false) {
  if (!deviceID) return []
  try {
    const res = await http.get(`/devices/${encodeURIComponent(deviceID)}`)
    const points: any[] = res.data?.data?.points || []
    const mode = (p: any) => String(p.access_mode || '').toLowerCase()
    const kept = writableOnly
      ? points.filter((p: any) => !['r', 'read', 'readonly', 'read_only'].includes(mode(p)))
      : points
    return kept.map((p: any) => ({
      label: `${p.name}${p.data_type ? ` (${p.data_type})` : ''}`,
      value: p.name,
    }))
  } catch {
    return []
  }
}

async function onSourceChange(deviceID: string | null) {
  newRule.value.source_point = null
  sourcePointOptions.value = await loadDevicePoints(deviceID)
}

async function onTargetChange(deviceID: string | null) {
  newRule.value.target_point = null
  // The backend rejects a read-only target, so do not offer one.
  targetPointOptions.value = await loadDevicePoints(deviceID, true)
}

function openCreate() {
  // 取消不会清空上一次的填写，所以每次打开都从空表单开始，避免把半个规则提交上去。
  newRule.value = { name: '', source_device_id: null, source_point: null, condition_op: '>', threshold: 0, target_device_id: null, target_point: null, target_value: '' }
  showCreate.value = true
  sourcePointOptions.value = []
  targetPointOptions.value = []
}

async function refresh() {
  try {
    const [rulesRes, statusRes, devRes] = await Promise.all([
      linkageApi.listRules(),
      linkageApi.status().catch(() => null),
      http.get('/devices', { params: { page: 1, size: 1000 } }),
    ])
    linkageRules.value = rulesRes?.items || []
    status.value = statusRes
    deviceOptions.value = (devRes.data?.data || []).map((d: any) => ({ label: d.name || d.device_id, value: d.device_id }))
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

let timer: ReturnType<typeof setInterval> | null = null

async function createRule() {
  creating.value = true
  try {
    await linkageApi.createRule(newRule.value as any)
    message.success(t('common.createSuccess'))
    showCreate.value = false
    newRule.value = { name: '', source_device_id: null, source_point: null, condition_op: '>', threshold: 0, target_device_id: null, target_point: null, target_value: '' }
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.createFailed')))
  } finally {
    creating.value = false
  }
}

async function toggleRule(row: LinkageRule, val: boolean) {
  try {
    await linkageApi.setRuleEnabled(row.id, val)
    row.enabled = val
    message.success(t('common.saveSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.saveFailed')))
  }
}

function deleteRule(row: LinkageRule) {
  dialog.warning({
    title: t('common.confirmDeleteName', { name: row.name ?? row.id }),
    content: t('common.confirmDeleteDesc'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => { void doDeleteRule(row) },
  })
}

async function doDeleteRule(row: LinkageRule) {
  try {
    await linkageApi.deleteRule(row.id)
    message.success(t('common.deleteSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.deleteFailed')))
  }
}

onMounted(() => {
  refresh()
  // Trigger counts only change at runtime, so poll quietly instead of asking the
  // operator to hit refresh while watching a device react.
  timer = setInterval(() => {
    if (document.visibilityState === 'visible') {
      linkageApi.listRules().then(r => { linkageRules.value = r?.items || [] }).catch(() => {})
      linkageApi.status().then(s => { status.value = s }).catch(() => {})
    }
  }, 5000)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
  timer = null
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
