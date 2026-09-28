<template>
  <div class="page-container">
    <n-card :title="t('router.dataDownsample')" size="small">
      <n-space vertical>
        <!-- Downsample Config -->
        <n-descriptions :title="t('dataDownsample.config')" bordered :column="2" size="small">
          <n-descriptions-item :label="t('dataDownsample.enabled')">
            <n-tag :type="config.enabled ? 'success' : 'default'">{{ config.enabled ? 'ON' : 'OFF' }}</n-tag>
          </n-descriptions-item>
          <n-descriptions-item :label="t('dataDownsample.autoRun')">
            <n-tag :type="config.auto_run ? 'success' : 'default'">{{ config.auto_run ? 'ON' : 'OFF' }}</n-tag>
          </n-descriptions-item>
          <n-descriptions-item :label="t('dataDownsample.tier1Age')">{{ config.tier1_age_days }}d</n-descriptions-item>
          <n-descriptions-item :label="t('dataDownsample.tier2Age')">{{ config.tier2_age_days }}d</n-descriptions-item>
          <n-descriptions-item :label="t('dataDownsample.tier3Age')">{{ config.tier3_age_days }}d</n-descriptions-item>
          <n-descriptions-item :label="t('dataDownsample.runInterval')">{{ config.run_interval_hours }}h</n-descriptions-item>
        </n-descriptions>

        <!-- Actions -->
        <n-space>
          <n-button type="primary" :loading="executing" @click="executeDownsample">
            {{ t('dataDownsample.executeNow') }}
          </n-button>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <!-- Execution History -->
        <n-divider />
        <n-h3>{{ t('dataDownsample.history') }}</n-H3>
        <n-data-table
          :columns="historyColumns"
          :data="history"
          :loading="loading"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 15 }"
        />

        <!-- Storage Stats -->
        <n-divider />
        <n-h3>{{ t('dataDownsample.storageStats') }}</n-H3>
        <n-grid :cols="3" :x-gap="12">
          <n-gi><n-card size="small"><n-statistic :label="t('dataDownsample.rawSize')" :value="formatNum(storageStats.raw_size_mb ?? 0)" suffix="MB" /></n-card></n-gi>
          <n-gi><n-card size="small"><n-statistic :label="t('dataDownsample.downsampledSize')" :value="formatNum(storageStats.downsampled_size_mb ?? 0)" suffix="MB" /></n-card></n-gi>
          <n-gi><n-card size="small"><n-statistic :label="t('dataDownsample.spaceSaved')" :value="formatNum(storageStats.space_saved_pct ?? 0)" suffix="%" /></n-card></n-gi>
        </n-grid>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { t } from '@/i18n'
import { message } from '@/utils/discreteApi'
import { qualityMonitorApi } from '@/api'
import { formatDateTime } from '@/utils/datetime'
import { formatNum } from '@/utils/format'
import { extractError } from '@/utils/errorCodes'

const config = ref<any>({})
const history = ref<any[]>([])
const storageStats = ref<any>({})
const executing = ref(false)
const loading = ref(false)

const fmtTime = (v: any) => (v ? formatDateTime(v) : '—')

const historyColumns = computed(() => [
  { title: t('dataDownsample.colStart'), key: 'started_at', render: (r: any) => fmtTime(r.started_at) },
  { title: t('dataDownsample.colEnd'), key: 'completed_at', render: (r: any) => fmtTime(r.completed_at) },
  { title: t('dataDownsample.tier'), key: 'tier' },
  { title: t('dataDownsample.colProcessed'), key: 'rows_processed' },
  { title: t('dataDownsample.colArchived'), key: 'rows_archived' },
  { title: t('dataDownsample.colStatus'), key: 'status', render: (r: any) => (r.status === 'success' ? t('common.success') : (r.error || r.status)) },
])

async function refresh() {
  loading.value = true
  try {
    const [cfg, hist, stor] = await Promise.all([
      qualityMonitorApi.getDownsampleConfig(),
      qualityMonitorApi.getDownsampleHistory(),
      qualityMonitorApi.getDownsampleStorage(),
    ])
    config.value = cfg || {}
    history.value = hist || []
    storageStats.value = stor || {}
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  } finally {
    loading.value = false
  }
}

async function executeDownsample() {
  executing.value = true
  try {
    await qualityMonitorApi.executeDownsample()
    message.success(t('dataDownsample.executeSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('dataDownsample.executeFailed')))
  } finally {
    executing.value = false
  }
}

onMounted(() => { refresh() })
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
