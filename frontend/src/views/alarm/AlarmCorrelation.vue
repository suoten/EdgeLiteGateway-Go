<template>
  <div class="page-container">
    <n-card :title="t('router.alarmCorrelation')" size="small">
      <n-space vertical>
        <!-- Correlation Stats -->
        <n-grid :cols="4" :x-gap="12">
          <n-gi>
            <n-statistic :label="t('alarmCorrelation.totalAlarms')" :value="stats.total_alarms || 0" />
          </n-gi>
          <n-gi>
            <n-statistic :label="t('alarmCorrelation.correlatedGroups')" :value="stats.correlation_groups || 0" />
          </n-gi>
          <n-gi>
            <n-statistic :label="t('alarmCorrelation.rootCauseCount')" :value="stats.root_causes || 0" />
          </n-gi>
          <n-gi>
            <n-statistic :label="t('alarmCorrelation.suppressedCount')" :value="stats.suppressed || 0" />
          </n-gi>
        </n-grid>

        <n-divider />

        <!-- Time Range Selector -->
        <n-space>
          <n-date-picker v-model:value="timeRange" type="datetimerange" clearable />
          <n-button type="primary" @click="loadCorrelations">
            {{ t('common.query') }}
          </n-button>
          <n-button @click="refresh">
            {{ t('common.refresh') }}
          </n-button>
        </n-space>

        <!-- Correlation Groups -->
        <n-divider />
        <n-h3>{{ t('alarmCorrelation.correlationGroups') }}</n-h3>
        <n-data-table
          :columns="groupColumns"
          :data="correlationGroups"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 10 }"
          :row-key="(row: any) => row.id"
          :expanded-row-keys="expandedKeys"
          @update:expanded-row-keys="(keys: any) => expandedKeys = keys"
        >
          <template #expand="props">
            <n-data-table
              :columns="alarmColumns"
              :data="props.record.alarms || []"
              :bordered="false"
              size="small"
            />
          </template>
        </n-data-table>

        <!-- Suppression Rules -->
        <n-divider />
        <n-h3>{{ t('alarmCorrelation.suppressionRules') }}</n-h3>
        <n-data-table
          :columns="suppressionColumns"
          :data="suppressionRules"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 10 }"
        />
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { message } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { formatDateTime } from '@/utils/datetime'
import { severityLabel, alarmStatusLabel } from '@/utils/enumLabels'

const stats = ref<any>({})
const correlationGroups = ref<any[]>([])
const suppressionRules = ref<any[]>([])
const expandedKeys = ref<string[]>([])
const timeRange = ref<[number, number] | null>(null)

const groupColumns = computed(() => [
  { title: t('alarmCorrelation.groupId'), key: 'id', width: 120 },
  { title: t('alarmCorrelation.alarmCount'), key: 'alarm_count', width: 100 },
  { title: t('alarmCorrelation.rootCause'), key: 'root_cause' },
  { title: t('alarmCorrelation.correlationType'), key: 'correlation_type' },
  { title: t('alarmCorrelation.firstAlarm'), key: 'first_alarm_time', width: 170, render: (r: any) => (r.first_alarm_time ? formatDateTime(r.first_alarm_time) : '-') },
  { title: t('alarmCorrelation.lastAlarm'), key: 'last_alarm_time', width: 170, render: (r: any) => (r.last_alarm_time ? formatDateTime(r.last_alarm_time) : '-') },
])

const alarmColumns = computed(() => [
  { title: t('alarmList.device'), key: 'device_name' },
  { title: t('alarmList.title'), key: 'title' },
  { title: t('alarmList.severity'), key: 'severity', render: (r: any) => severityLabel.value?.[r.severity] || r.severity },
  { title: t('alarmList.timestamp'), key: 'timestamp', render: (r: any) => formatDateTime(r.timestamp) },
  { title: t('alarmList.status'), key: 'status', render: (r: any) => alarmStatusLabel.value?.[r.status] || r.status },
])

const suppressionColumns = computed(() => [
  { title: t('alarmCorrelation.ruleName'), key: 'name' },
  { title: t('alarmCorrelation.pattern'), key: 'pattern' },
  { title: t('alarmCorrelation.duration'), key: 'duration_sec', render: (r: any) => (r.duration_sec != null ? `${r.duration_sec}s` : '-') },
  {
    title: t('common.status'),
    key: 'enabled',
    render: (r: any) => r.enabled ? t('common.enabled') : t('common.disabled'),
  },
])

async function loadCorrelations() {
  try {
    const params: any = {}
    if (timeRange.value) {
      params.start_time = new Date(timeRange.value[0]).toISOString()
      params.end_time = new Date(timeRange.value[1]).toISOString()
    }
    const [statsRes, groupsRes] = await Promise.all([
      http.get('/alarms/correlation/stats', { params }),
      http.get('/alarms/correlation/groups', { params }),
    ])
    stats.value = statsRes.data?.data || {}
    correlationGroups.value = groupsRes.data?.data?.items || []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function loadSuppressionRules() {
  try {
    const res = await http.get('/alarms/correlation/suppression')
    suppressionRules.value = res.data?.data?.items || []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function refresh() {
  await Promise.all([loadCorrelations(), loadSuppressionRules()])
}

onMounted(() => {
  refresh()
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
