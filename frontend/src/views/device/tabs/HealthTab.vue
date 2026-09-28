<template>
  <n-space vertical :size="12">
    <n-alert v-if="healthError" type="warning" size="small" :show-icon="true">{{ healthError }}</n-alert>

    <template v-if="!healthError && !healthData && !driverHealth">
      <n-empty :description="t('healthDetail.noHealthData')" size="small" />
    </template>

    <template v-else>
      <!-- The scheduler sees collection cycles; the driver sees protocol operations.
           They disagree on purpose, so both are labelled instead of merged. -->
      <n-divider class="health-divider" title-placement="left">{{ t('healthDetail.collectorSection') }}</n-divider>
      <n-descriptions v-if="healthData" :column="2" bordered size="small">
        <n-descriptions-item :label="t('common.status')">{{ healthData.status || '-' }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.lastCheck')">{{ formatDateTime(healthData.last_check || healthData.timestamp) }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.successCount')">{{ healthData.success_count ?? 0 }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.failCount')">{{ healthData.fail_count ?? 0 }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.latency')">{{ healthData.latency_ms ?? '-' }} ms</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.consecutiveFailures')">{{ healthData.consecutive_failures ?? 0 }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.lastError')" :span="2">{{ healthData.message || healthData.error || '-' }}</n-descriptions-item>
      </n-descriptions>
      <n-empty v-else :description="t('healthDetail.noCollectorData')" size="small" />

      <n-divider class="health-divider" title-placement="left">{{ t('healthDetail.driverSection') }}</n-divider>
      <!-- has_samples is the honest switch: every counter is 0 on a device the
           driver never polled, and 0 errors there is not a healthy link. -->
      <n-alert v-if="driverHealth && !driverHealth.has_samples" type="info" size="small" :show-icon="true">
        {{ t('healthDetail.noSamples') }}
      </n-alert>
      <n-descriptions v-if="driverHealth" :column="2" bordered size="small">
        <n-descriptions-item :label="t('healthDetail.qualityScore')">
          {{ fmt1(driverHealth.connection_quality_score) }}
        </n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.onlineRate')">
          {{ fmt1(driverHealth.online_rate, '%') }}
        </n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.totalReads')">{{ driverHealth.total_reads }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.failedReads')">{{ driverHealth.failed_reads }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.totalWrites')">{{ driverHealth.total_writes }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.failedWrites')">{{ driverHealth.failed_writes }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.avgLatency')">{{ fmt1(driverHealth.avg_latency_ms, ' ms') }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.p95Latency')">{{ fmt1(driverHealth.p95_latency_ms, ' ms') }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.reconnectCount')">{{ driverHealth.total_reconnects }}</n-descriptions-item>
        <n-descriptions-item :label="t('healthDetail.consecutiveFailures')">{{ driverHealth.consecutive_failures }}</n-descriptions-item>
        <n-descriptions-item v-if="driverHealth.last_error" :label="t('healthDetail.lastError')" :span="2">
          {{ driverHealth.last_error }}
        </n-descriptions-item>
        <n-descriptions-item v-if="driverHealth.degradation_reason" :label="t('healthDetail.degradationReason')" :span="2">
          {{ driverHealth.degradation_reason }}
        </n-descriptions-item>
      </n-descriptions>
      <n-empty v-else-if="!healthError" :description="t('healthDetail.noDriverData')" size="small" />

      <n-divider class="health-divider" title-placement="left">{{ t('healthDetail.pointQuality') }}</n-divider>
      <n-data-table
        :columns="pointColumns"
        :data="pointHealth"
        :loading="pointLoading"
        size="small"
        :max-height="320"
        :pagination="{ pageSize: 10 }"
      >
        <template #empty>
          <n-empty :description="pointError || t('healthDetail.noPointQualityData')" size="small" />
        </template>
      </n-data-table>
    </template>
  </n-space>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useDeviceDetailConsumer } from '../composables/useDeviceDetail'
import { deviceApi } from '@/api'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { qualityLabel, qualityColor } from '@/utils/enumLabels'
import { formatDateTime } from '@/utils/datetime'
import { NAlert, NDataTable, NDescriptions, NDescriptionsItem, NDivider, NEmpty, NSpace, NTag } from 'naive-ui'

const { device, healthData, driverHealth, healthError } = useDeviceDetailConsumer()

const pointHealth = ref<any[]>([])
const pointLoading = ref(false)
const pointError = ref('')

// Null means the gateway measured nothing, which is not the same answer as 0: a
// 0% success rate, a 0 latency and a 0 score are all real measurements to show.
function fmt1(value: number | null | undefined, suffix = ''): string {
  return value === null || value === undefined ? '-' : `${value.toFixed(1)}${suffix}`
}

const pointColumns = computed(() => [
  { title: t('healthDetail.pointName'), key: 'point_name', width: 160, ellipsis: { tooltip: true } },
  {
    title: t('healthDetail.currentQuality'),
    key: 'current_quality',
    width: 100,
    render: (r: any) => {
      const q = r.current_quality
      const label = q === 'never_collected' ? t('healthDetail.neverCollected') : (qualityLabel.value[q] || q || '-')
      return h(NTag, { type: (qualityColor[q] || 'default') as any, size: 'small', bordered: false }, () => label)
    },
  },
  { title: t('common.value'), key: 'last_value', width: 120, render: (r: any) => (r.last_value === null || r.last_value === undefined ? '-' : String(r.last_value)) },
  { title: t('healthDetail.lastSeenAt'), key: 'last_seen_at', render: (r: any) => (r.last_seen_at ? formatDateTime(r.last_seen_at) : '-') },
])

async function loadPointHealth() {
  if (!device.value) return
  pointLoading.value = true
  try {
    const rows = await deviceApi.getPointHealth(device.value.device_id)
    pointHealth.value = Array.isArray(rows) ? rows : []
    pointError.value = ''
  } catch (e: any) {
    pointHealth.value = []
    pointError.value = extractError(e, t('common.loadFailed'))
  } finally {
    pointLoading.value = false
  }
}

onMounted(() => {
  loadPointHealth()
})
</script>

<style scoped>
.health-divider { margin: 4px 0; font-size: 13px; }
</style>
