<template>
  <n-space vertical>
    <n-space align="center">
      <n-button type="primary" :loading="ctx.selfTestRunning.value" @click="ctx.runSelfTest">
        {{ t('selfTest.runTest') }}
      </n-button>
      <n-tag v-if="result" :type="result.passed ? 'success' : 'error'" size="small">
        {{ result.passed ? t('selfTest.allPassed') : t('selfTest.hasFailures') }}
      </n-tag>
    </n-space>

    <template v-if="result">
      <n-descriptions :column="1" bordered size="small">
        <n-descriptions-item :label="t('common.status')">{{ result.passed ? t('selfTest.allPassed') : t('selfTest.hasFailures') }}</n-descriptions-item>
        <n-descriptions-item :label="t('selfTest.duration')">{{ result.duration ?? result.duration_ms ?? 0 }} ms</n-descriptions-item>
        <n-descriptions-item :label="t('selfTest.lastCollectTime')">{{ result.details?.last_collect_at ? formatDateTime(result.details.last_collect_at) : '-' }}</n-descriptions-item>
        <n-descriptions-item :label="t('selfTest.consecutiveErrors')">{{ result.details?.consecutive_errors ?? 0 }}</n-descriptions-item>
        <n-descriptions-item v-if="result.details?.last_error" :label="t('selfTest.lastError')">{{ result.details.last_error }}</n-descriptions-item>
      </n-descriptions>

      <n-data-table
        :columns="checkColumns"
        :data="result.checks || []"
        :bordered="false"
        size="small"
      />
    </template>
    <n-empty v-else :description="t('selfTest.noResult')" />
  </n-space>
</template>

<script setup lang="ts">
import { h, computed } from 'vue'
import { useDeviceDetailConsumer } from '../composables/useDeviceDetail'
import { t } from '@/i18n'
import { formatDateTime } from '@/utils/datetime'
import { NSpace, NButton, NDescriptions, NDescriptionsItem, NEmpty, NTag, NDataTable } from 'naive-ui'

const ctx = useDeviceDetailConsumer()
const result = computed(() => ctx.selfTestResult.value)

const checkNameKeys: Record<string, string> = {
  device_registered: 'selfTest.checkDeviceRegistered',
  collector_running: 'selfTest.checkCollectorRunning',
  recent_data: 'selfTest.checkRecentData',
  no_errors: 'selfTest.checkNoErrors',
}

const checkColumns = computed(() => [
  {
    title: t('selfTest.checkItem'),
    key: 'name',
    render: (row: any) => {
      const key = checkNameKeys[row.name]
      return key ? t(key) : row.name
    },
  },
  {
    title: t('common.result'),
    key: 'passed',
    width: 90,
    render: (row: any) =>
      h(NTag, { type: row.passed ? 'success' : 'error', size: 'small' }, () =>
        row.passed ? t('selfTest.passed') : t('selfTest.failed'),
      ),
  },
  {
    title: t('selfTest.duration'),
    key: 'duration_ms',
    width: 100,
    render: (row: any) => `${row.duration_ms ?? 0} ms`,
  },
  { title: t('selfTest.checkDetail'), key: 'message', ellipsis: { tooltip: true } },
])
</script>
