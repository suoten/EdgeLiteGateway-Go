<template>
  <div class="page-container">
    <n-card :title="t('router.selfTest')" size="small">
      <n-space vertical>
        <n-space>
          <n-button type="primary" :loading="running" @click="runSelfTest">
            {{ t('selfTest.runNow') }}
          </n-button>
          <n-tag v-if="lastResult" :type="lastResult.passed ? 'success' : 'error'">
            {{ lastResult.passed ? t('selfTest.allPassed') : t('selfTest.hasFailures') }}
          </n-tag>
        </n-space>

        <n-divider v-if="lastResult" />

        <div v-if="lastResult">
          <n-h3>{{ t('selfTest.results') }}</n-H3>
          <n-data-table
            :columns="testColumns"
            :data="lastResult.tests"
            :bordered="false"
            size="small"
            :pagination="{ pageSize: 50 }"
          />

          <n-divider />

          <n-h3>{{ t('selfTest.summary') }}</n-H3>
          <n-descriptions bordered :column="3" size="small">
            <n-descriptions-item :label="t('selfTest.total')">{{ lastResult.total }}</n-descriptions-item>
            <n-descriptions-item :label="t('selfTest.passed')">{{ lastResult.passed_count }}</n-descriptions-item>
            <n-descriptions-item :label="t('selfTest.failed')">{{ lastResult.failed_count }}</n-descriptions-item>
            <n-descriptions-item :label="t('selfTest.duration')">{{ lastResult.duration }}ms</n-descriptions-item>
            <n-descriptions-item :label="t('selfTest.timestamp')">{{ formatDateTime(lastResult.timestamp) }}</n-descriptions-item>
            <n-descriptions-item :label="t('selfTest.version')">{{ lastResult.version }}</n-descriptions-item>
          </n-descriptions>
        </div>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { computed, h, ref } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { message } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { formatDateTime } from '@/utils/datetime'
import { NTag } from 'naive-ui'

const running = ref(false)
const lastResult = ref<any>(null)

const testColumns = computed(() => [
  { title: t('selfTestSection.environment'), key: 'category', width: 120, render: (r: any) => categoryLabel(r.category) },
  { title: t('selfTest.checkItem'), key: 'name', render: (r: any) => checkNameLabel(r.name) },
  {
    title: t('common.result'),
    key: 'passed',
    width: 80,
    render: (r: any) => h(NTag, { type: r.passed ? 'success' : 'error', size: 'small' }, () => r.passed ? t('selfTest.passed') : t('selfTest.failed')),
  },
  { title: t('selfTest.duration'), key: 'duration_ms', width: 100, render: (r: any) => `${r.duration_ms}ms` },
  { title: t('selfTest.message'), key: 'message', ellipsis: { tooltip: true } },
])

const categoryKeys: Record<string, string> = {
  environment: 'selfTestSection.environment',
  connection: 'selfTestSection.connection',
  readTest: 'selfTestSection.read_test',
  read_test: 'selfTestSection.read_test',
  writeTest: 'selfTestSection.write_test',
  write_test: 'selfTestSection.write_test',
  observability: 'selfTestSection.observability',
}

const checkNameKeys: Record<string, string> = {
  database: 'selfTest.checkDatabase',
  ts_storage: 'selfTest.checkTsStorage',
  scheduler: 'selfTest.checkScheduler',
  event_bus: 'selfTest.checkEventBus',
  mqtt_forwarder: 'selfTest.checkMqttForwarder',
  websocket: 'selfTest.checkWebsocket',
  rule_evaluator: 'selfTest.checkRuleEvaluator',
}

function categoryLabel(category: string) {
  const key = categoryKeys[category]
  return key ? t(key) : category
}

function checkNameLabel(name: string) {
  const key = checkNameKeys[name]
  return key ? t(key) : name
}

async function runSelfTest() {
  running.value = true
  try {
    const res = await http.post('/system/self-test')
    lastResult.value = res.data?.data ?? res.data
    if (lastResult.value?.passed) {
      message.success(t('selfTest.allPassed'))
    } else {
      message.warning(t('selfTest.hasFailures'))
    }
  } catch (e: any) {
    message.error(extractError(e, t('selfTest.runFailed')))
  } finally {
    running.value = false
  }
}
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
