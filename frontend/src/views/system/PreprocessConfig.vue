<template>
  <n-spin :show="pageLoading" :description="t('preprocess.loading')">
  <n-space vertical :size="16">
    <n-card :title="t('preprocess.globalConfig')" :bordered="false">
      <n-form :model="globalForm" ref="globalFormRef" :rules="globalRules" label-placement="left" label-width="140">
        <n-form-item :label="t('preprocess.enablePreprocess')">
          <n-switch v-model:value="globalForm.enabled" />
        </n-form-item>
        <n-form-item :label="t('preprocess.defaultDeadband')" path="default_deadband">
          <n-input-number v-model:value="globalForm.default_deadband" :min="0" :step="0.1" style="width: 200px" />
        </n-form-item>
        <n-form-item :label="t('preprocess.defaultFilterWindow')" path="default_filter_window">
          <n-input-number v-model:value="globalForm.default_filter_window" :min="1" :max="21" style="width: 200px" />
        </n-form-item>
        <n-form-item :label="t('preprocess.defaultAggWindow')" path="default_aggregate_window_sec">
          <n-input-number v-model:value="globalForm.default_aggregate_window_sec" :min="0" style="width: 200px" />
        </n-form-item>
      </n-form>
    </n-card>

    <n-card :title="t('preprocess.pointConfig')" :bordered="false">
      <template #header-extra>
        <n-button type="primary" size="small" @click="showAddModal = true">
          <template #icon><n-icon :component="AddOutline" /></template>
          {{ t('preprocess.addPoint') }}
        </n-button>
      </template>
      <n-data-table
        :columns="columns"
        :data="pointList"
        :loading="pageLoading"
        :bordered="false"
        size="small"
        :scroll-x="970"
      />
    </n-card>

    <n-space>
      <n-button type="primary" :loading="saving" @click="handleSave">{{ t('preprocess.saveConfig') }}</n-button>
      <n-button @click="fetchConfig">{{ t('preprocess.refresh') }}</n-button>
    </n-space>

    <n-modal v-model:show="showAddModal" :title="t('preprocess.addPointTitle')" preset="card" style="width: 500px; max-width: 95vw" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-form :model="addForm" :rules="addRules" ref="addFormRef" label-placement="left" label-width="120">
        <n-form-item :label="t('preprocess.pointId')" path="point_key">
          <n-input v-model:value="addForm.point_key" maxlength="100" :placeholder="t('preprocess.pointIdPlaceholder')" />
        </n-form-item>
        <n-form-item :label="t('preprocess.deadbandValue')">
          <n-input-number v-model:value="addForm.deadband" :min="0" :step="0.1" style="width: 200px" />
        </n-form-item>
        <n-form-item :label="t('preprocess.deadbandPercent')">
          <n-input-number v-model:value="addForm.deadband_percent" :min="0" :max="100" :step="0.1" style="width: 200px" />
        </n-form-item>
        <n-form-item :label="t('preprocess.filterType')" path="filter">
          <n-select v-model:value="addForm.filter" :options="filterOptions" clearable style="width: 200px" @update:value="syncFilterWindow" />
        </n-form-item>
        <n-form-item v-if="addForm.filter" :label="t('preprocess.filterWindow')">
          <n-input-number v-model:value="addForm.filter_window" :min="1" :max="21" :disabled="!filterUsesWindow" style="width: 200px" />
          <n-text v-if="!filterUsesWindow" depth="3" style="margin-left: 8px; font-size: 12px">
            {{ windowHint }}
          </n-text>
        </n-form-item>
        <n-form-item v-if="addForm.filter === 'ema'" :label="t('preprocess.emaAlpha')">
          <n-input-number v-model:value="addForm.ema_alpha" :min="0.01" :max="1" :step="0.05" style="width: 200px" />
          <n-text depth="3" style="margin-left: 8px; font-size: 12px">{{ t('preprocess.emaAlphaHint') }}</n-text>
        </n-form-item>
        <template v-if="addForm.filter === 'kalman'">
          <n-form-item :label="t('preprocess.kalmanProcessNoise')">
            <n-input-number v-model:value="addForm.kalman_process_noise" :min="0.000001" :step="0.001" style="width: 200px" />
          </n-form-item>
          <n-form-item :label="t('preprocess.kalmanMeasNoise')">
            <n-input-number v-model:value="addForm.kalman_measurement_noise" :min="0.000001" :step="0.001" style="width: 200px" />
          </n-form-item>
        </template>
        <n-form-item :label="t('preprocess.aggType')">
          <n-select v-model:value="addForm.aggregate" :options="aggregateOptions" clearable style="width: 200px" />
        </n-form-item>
        <n-form-item v-if="addForm.aggregate" :label="t('preprocess.aggWindow')">
          <n-input-number v-model:value="addForm.aggregate_window_sec" :min="1" style="width: 200px" />
        </n-form-item>
      </n-form>
      <template #action>
        <n-button @click="showAddModal = false">{{ t('common.cancel') }}</n-button>
        <n-button type="primary" :loading="adding" @click="handleAdd">{{ t('common.confirm') }}</n-button>
      </template>
    </n-modal>
  </n-space>
  </n-spin>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted, h, watch, nextTick } from 'vue'
import { NButton, NSpace, NTag, NSpin, NText } from 'naive-ui'
import { AddOutline, TrashOutline } from '@vicons/ionicons5'
import { preprocessApi } from '@/api'
// FIXED: 原问题-添加i18n支持
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message, dialog } from '@/utils/discreteApi'
import { useDirtyFormGuard } from '@/composables/useDirtyFormGuard'
// [AUDIT-FIX] 严重级-预处理配置的保存与新增点位属敏感写操作，需函数级权限校验
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()

const pageLoading = ref(true)

const globalForm = reactive({
  enabled: false,
  default_deadband: 0,
  default_filter_window: 3,
  default_aggregate_window_sec: 0,
})
// [AUDIT-FIX] 全局配置表单添加数值范围校验
const globalFormRef = ref<any>(null)
const globalRules = computed(() => ({
  default_deadband: [{ type: 'number' as const, min: 0, message: t('preprocess.valueMustBeNonNegative'), trigger: ['input', 'blur'] }],
  default_filter_window: [{ type: 'number' as const, min: 1, max: 21, message: t('preprocess.filterWindowRange'), trigger: ['input', 'blur'] }],
  default_aggregate_window_sec: [{ type: 'number' as const, required: true, min: 0, message: t('preprocess.valueMustBeNonNegative'), trigger: ['input', 'blur'] }],
}))

const pointConfigs = ref<Record<string, any>>({})
const pointList = ref<any[]>([])
const saving = ref(false)
const dirty = ref(false)
// True only while fetchConfig writes the loaded configuration into the reactive
// objects; the dirty watcher skips those mutations.
let loadingConfig = false
const showAddModal = ref(false)
const addFormRef = ref<any>(null)
const adding = ref(false)

const addForm = reactive({
  point_key: '',
  deadband: 0,
  deadband_percent: 0,
  filter: null as string | null,
  filter_window: 3,
  ema_alpha: 0.3,
  kalman_process_noise: 0.001,
  kalman_measurement_noise: 0.01,
  aggregate: null as string | null,
  aggregate_window_sec: 60,
})

const addRules = computed(() => ({
  point_key: { required: true, message: t('preprocess.pointIdRequired'), trigger: ['input', 'blur'] },
}))

const filterOptions = [
  { label: t('preprocess.filterMedian3'), value: 'median_3' },
  { label: t('preprocess.filterMedian5'), value: 'median_5' },
  { label: t('preprocess.filterMedian7'), value: 'median_7' },
  { label: t('preprocess.filterMovingAvg'), value: 'moving_avg' },
  { label: t('preprocess.filterEma'), value: 'ema' },
  { label: t('preprocess.filterKalman'), value: 'kalman' },
]

// Only median_<N> and moving_avg consume a sample count: a median carries its own
// width in its name, and ema/kalman are tuned by their own parameters. Showing an
// editable window for those would offer a number that filters nothing.
const filterUsesWindow = computed(() => {
  const filter = addForm.filter || ''
  return filter === 'moving_avg' || filter.startsWith('median_')
})

const windowHint = computed(() =>
  (addForm.filter || '').startsWith('median_')
    ? t('preprocess.medianWindowHint')
    : t('preprocess.windowUnusedHint'),
)

function syncFilterWindow(value: string | null) {
  if (value && value.startsWith('median_')) {
    const width = Number(value.slice('median_'.length))
    if (!Number.isNaN(width) && width > 0) addForm.filter_window = width
  }
}

const aggregateOptions = [
  { label: t('preprocess.aggAvg'), value: 'avg' },
  { label: t('preprocess.aggMax'), value: 'max' },
  { label: t('preprocess.aggMin'), value: 'min' },
  { label: t('preprocess.aggSum'), value: 'sum' },
  { label: t('preprocess.aggLast'), value: 'last' },
]

// The table has to show the parameters the filter actually runs with, because
// ema and kalman ignore a sample window and a median takes its width from its own
// name. Without this a row could read "ema, window 3" while the 3 meant nothing.
function filterUsesWindowFor(filter: string | undefined): boolean {
  const value = filter || ''
  return value === 'moving_avg' || value.startsWith('median_')
}

function filterLabel(row: any): string {
  if (!row.filter) return ''
  if (row.filter === 'ema') return `ema (α=${row.ema_alpha ?? 0.3})`
  if (row.filter === 'kalman') return `kalman (q=${row.kalman_process_noise ?? 0.001}, r=${row.kalman_measurement_noise ?? 0.01})`
  return String(row.filter)
}

// FIXED: 原问题-表格列标题中文硬编码，改为i18n
const columns = computed(() => [
  { title: t('preprocess.pointId'), key: 'point_key', width: 200 },
  { title: t('preprocess.deadbandValue'), key: 'deadband', width: 100 },
  { title: t('preprocess.deadbandPercent'), key: 'deadband_percent', width: 100 },
  { title: t('preprocess.filterType'), key: 'filter', width: 190, render: (row: any) => filterLabel(row) },
  {
    title: t('preprocess.filterWindow'),
    key: 'filter_window',
    width: 100,
    render: (row: any) => (!row.filter ? '' : filterUsesWindowFor(row.filter) ? row.filter_window : t('preprocess.windowNotApplicable')),
  },
  { title: t('preprocess.aggType'), key: 'aggregate', width: 100 },
  { title: t('preprocess.aggWindow'), key: 'aggregate_window_sec', width: 120 },
  {
    title: t('common.actions'), key: 'actions', width: 80,  // FIXED: 原问题-跨域误用alarmList.actions
    render: (row: any) =>
      h(NButton, { text: true, type: 'error', onClick: () => handleDelete(row.point_key) }, {
        default: () => t('common.delete'),
      }),
  },
])

function updatePointList() {
  pointList.value = Object.entries(pointConfigs.value).map(([key, config]) => ({
    point_key: key,
    ...config,
  }))
}

async function fetchConfig() {
  loadingConfig = true
  try {
    const data = await preprocessApi.getConfig()
    globalForm.enabled = data.enabled ?? false
    globalForm.default_deadband = data.default_deadband ?? 0
    globalForm.default_filter_window = data.default_filter_window ?? 3
    globalForm.default_aggregate_window_sec = data.default_aggregate_window_sec ?? 0
    pointConfigs.value = data.point_configs ?? {}
    updatePointList()
  } catch (e: any) {
    message.error(extractError(e, t('http.requestFailed')))
  } finally {
    pageLoading.value = false
    await nextTick()
    loadingConfig = false
    dirty.value = false
  }
}

async function handleSave() {
  if (!auth.isOperator) { message.warning(t('common.permissionDenied')); return }
  try {
    await globalFormRef.value?.validate()
  } catch {
    return
  }
  saving.value = true
  try {
    await preprocessApi.updateConfig({
      global: { ...globalForm },
      points: pointConfigs.value,
    })
    message.success(t('common.success'))
    dirty.value = false
  } catch (e: any) {
    message.error(extractError(e, t('common.failed')))
  } finally {
    saving.value = false
  }
}

async function handleAdd() {
  if (!auth.isOperator) { message.warning(t('common.permissionDenied')); return }
  try {
    await addFormRef.value?.validate()
  } catch { return }

  if (addForm.deadband > 0 && addForm.deadband_percent > 0) {
    message.warning(t('preprocess.deadbandConflict'))
    return
  }

  // The backend stores one rule per setting and refuses a row that configures
  // nothing; saying so here keeps the error next to the field instead of in a
  // toast after the request.
  if (!addForm.filter && !addForm.aggregate && addForm.deadband <= 0 && addForm.deadband_percent <= 0) {
    message.warning(t('preprocess.rowNeedsSetting'))
    return
  }

  const config: any = {}
  if (addForm.deadband > 0) config.deadband = addForm.deadband
  if (addForm.deadband_percent > 0) config.deadband_percent = addForm.deadband_percent
  if (addForm.filter) config.filter = addForm.filter
  if (addForm.filter && filterUsesWindow.value) config.filter_window = addForm.filter_window
  if (addForm.filter === 'ema') config.ema_alpha = addForm.ema_alpha
  if (addForm.filter === 'kalman') {
    config.kalman_process_noise = addForm.kalman_process_noise
    config.kalman_measurement_noise = addForm.kalman_measurement_noise
  }
  if (addForm.aggregate) config.aggregate = addForm.aggregate
  if (addForm.aggregate && addForm.aggregate_window_sec) config.aggregate_window_sec = addForm.aggregate_window_sec

  pointConfigs.value[addForm.point_key] = config
  updatePointList()

  adding.value = true
  try {
    // The save replaces the whole row set, so a body carrying only the new row
    // would have deleted every point that was already configured.
    await preprocessApi.updateConfig({
      global: { ...globalForm },
      points: { ...pointConfigs.value },
    })
    dirty.value = false
    message.success(t('common.success'))
  } catch (e: any) {
    message.error(extractError(e, t('common.failed')))
  } finally {
    adding.value = false
  }

  addForm.point_key = ''
  addForm.deadband = 0
  addForm.deadband_percent = 0
  addForm.filter = null
  addForm.filter_window = 3
  addForm.ema_alpha = 0.3
  addForm.kalman_process_noise = 0.001
  addForm.kalman_measurement_noise = 0.01
  addForm.aggregate = null
  addForm.aggregate_window_sec = 60
  showAddModal.value = false
}

function handleDelete(pointKey: string) {
  dialog.warning({
    title: t('common.confirm'),
    content: t('deviceList.deleteConfirm', { name: pointKey }),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      delete pointConfigs.value[pointKey]
      updatePointList()
      try {
        const remaining = { ...pointConfigs.value }
        delete remaining[pointKey]
        await preprocessApi.updateConfig({
          global: { ...globalForm },
          points: pointConfigs.value,
        })
        dirty.value = false
        message.success(t('common.success'))
      } catch (e: any) {
        message.error(extractError(e, t('common.failed')))
      }
    },
  })
}

onMounted(fetchConfig)

// fetchConfig assigns the loaded values through the same reactive objects the
// watcher below tracks, so a page nobody edited came back dirty and every
// navigation away asked about unsaved changes.
watch([() => ({ ...globalForm }), pointConfigs], () => { if (!loadingConfig) dirty.value = true }, { deep: true })

// The shared guard supplies the unsaved-changes wording and the beforeunload
// handler; the local dialog here rendered t('common.required') ("此项为必填") as
// the leave prompt, and its 确认/取消 buttons left the operator guessing.
useDirtyFormGuard({ isDirty: () => dirty.value })
</script>
