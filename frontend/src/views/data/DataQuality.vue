<template>
  <div class="page-container">
    <n-card :title="t('router.dataQuality')" size="small">
      <n-space vertical>
        <!-- Quality Score Overview -->
        <n-grid :cols="4" :x-gap="12">
          <n-gi>
            <n-card size="small">
              <n-statistic :label="t('dataQuality.completeness')" :value="quality.completeness_pct ?? 0" suffix="%" />
            </n-card>
          </n-gi>
          <n-gi>
            <n-card size="small">
              <n-statistic :label="t('dataQuality.accuracy')" :value="quality.accuracy_pct ?? 0" suffix="%" />
            </n-card>
          </n-gi>
          <n-gi>
            <n-card size="small">
              <n-statistic :label="t('dataQuality.timeliness')" :value="quality.timeliness_pct ?? 0" suffix="%" />
            </n-card>
          </n-gi>
          <n-gi>
            <n-card size="small">
              <n-statistic :label="t('dataQuality.overallScore')" :value="quality.overall_score ?? 0" suffix="/100" />
            </n-card>
          </n-gi>
        </n-grid>

        <n-divider />

        <!-- Device Quality Table -->
        <n-h3>{{ t('dataQuality.deviceDetails') }}</n-H3>
        <n-data-table
          :columns="deviceColumns"
          :data="deviceQuality"
          :loading="loading"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 20 }"
        />

        <n-divider />

        <!-- Quality Rules -->
        <n-h3>{{ t('dataQuality.rules') }}</n-H3>
        <n-data-table
          :columns="ruleColumns"
          :data="qualityRules"
          :loading="loading"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 10 }"
        />
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
import { extractError } from '@/utils/errorCodes'

const quality = ref<any>({})
const deviceQuality = ref<any[]>([])
const qualityRules = ref<any[]>([])
const loading = ref(false)

const deviceColumns = computed(() => [
  { title: t('dataQuality.colDevice'), key: 'device_name', render: (r: any) => r.device_name || r.device_id },
  { title: t('dataQuality.colPoints'), key: 'point_count' },
  { title: t('dataQuality.colValid'), key: 'valid_count' },
  { title: t('dataQuality.colInvalid'), key: 'invalid_count' },
  { title: t('dataQuality.colMissing'), key: 'missing_count' },
  { title: t('dataQuality.colQuality'), key: 'quality_pct', render: (r: any) => (r.quality_pct == null ? '—' : `${Number(r.quality_pct).toFixed(1)}%`) },
  { title: t('dataQuality.colLastCheck'), key: 'last_check', render: (r: any) => (r.last_check ? formatDateTime(r.last_check) : '—') },
])

const ruleColumns = computed(() => [
  { title: t('dataQuality.colRule'), key: 'name' },
  { title: t('dataQuality.colType'), key: 'rule_type' },
  { title: t('dataQuality.colCondition'), key: 'condition' },
  { title: t('common.enabled'), key: 'enabled', render: (r: any) => (r.enabled ? t('common.yes') : t('common.no')) },
])

async function refresh() {
  loading.value = true
  try {
    const [summary, devices, rules] = await Promise.all([
      qualityMonitorApi.getSummary(),
      qualityMonitorApi.getDevices(),
      qualityMonitorApi.getRules(),
    ])
    quality.value = summary || {}
    deviceQuality.value = devices || []
    qualityRules.value = rules || []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  } finally {
    loading.value = false
  }
}

onMounted(() => { refresh() })
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
