<template>
  <div class="page-container">
    <n-card :title="t('router.configVersion')" size="small">
      <n-space vertical>
        <!-- Current Version Info -->
        <n-descriptions :title="t('configVersion.current')" bordered :column="2" size="small">
          <n-descriptions-item :label="t('configVersion.version')">
            {{ currentVersion.version }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('configVersion.updatedAt')">
            {{ currentVersion.updated_at ? formatDateTime(currentVersion.updated_at) : '-' }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('configVersion.updatedBy')">
            {{ currentVersion.updated_by }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('configVersion.changeCount')">
            {{ currentVersion.changed_keys?.length || 0 }}
          </n-descriptions-item>
        </n-descriptions>

        <!-- Actions -->
        <n-space>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <!-- Version History -->
        <n-divider />
        <n-h3>{{ t('configVersion.history') }}</n-h3>
        <n-data-table
          :columns="historyColumns"
          :data="versionHistory"
          :bordered="false"
          :loading="loading"
          size="small"
          :pagination="{ pageSize: 15 }"
        >
          <template #empty>
            <LoadEmptyState :description="t('configVersion.emptyDesc')" :failed="loadFailed" :retrying="loading" @retry="refresh" />
          </template>
        </n-data-table>

        <!-- Diff Viewer -->
        <n-divider />
        <n-h3>{{ t('configVersion.diff') }}</n-h3>
        <n-select
          v-model:value="diffVersion"
          :options="versionOptions"
          style="width: 200px; margin-bottom: 12px;"
          @update:value="loadDiff"
        />
        <div v-if="diffData" class="diff-viewer">
          <div class="diff-old">
            <div class="diff-header">{{ t('configVersion.previous') }}</div>
            <pre>{{ diffData.old_config }}</pre>
          </div>
          <div class="diff-new">
            <div class="diff-header">{{ t('configVersion.current') }}</div>
            <pre>{{ diffData.new_config }}</pre>
          </div>
        </div>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, h } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { message, dialog } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { formatDateTime } from '@/utils/datetime'
import { NButton } from 'naive-ui'
import LoadEmptyState from '@/components/LoadEmptyState.vue'

const currentVersion = ref<any>({})
const versionHistory = ref<any[]>([])
const diffVersion = ref<number | null>(null)
const diffData = ref<any>(null)
const loading = ref(false)
const loadFailed = ref(false)

const versionOptions = computed(() =>
  versionHistory.value.map((v) => ({ label: `v${v.version}`, value: v.version }))
)

const historyColumns = computed(() => [
  { title: t('configVersion.version'), key: 'version', width: 100 },
  { title: t('configVersion.updatedAt'), key: 'updated_at', width: 180, render: (r: any) => formatDateTime(r.updated_at) },
  { title: t('configVersion.updatedBy'), key: 'updated_by', width: 120 },
  { title: t('configVersion.changeSummary'), key: 'change_summary' },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 150,
    render: (row: any) =>
      h(NButton, { size: 'small', type: 'primary', text: true, onClick: () => rollback(row) }, () => t('configVersion.rollback')),
  },
])

async function refresh() {
  loading.value = true
  try {
    const res = await http.get('/config/versions')
    currentVersion.value = res.data?.data?.current || {}
    versionHistory.value = res.data?.data?.history || []
    loadFailed.value = false
  } catch (e: any) {
    loadFailed.value = true
    message.error(extractError(e, t('common.loadFailed')))
  } finally {
    loading.value = false
  }
}

async function loadDiff(version: number) {
  if (!version) {
    diffData.value = null
    return
  }
  try {
    const res = await http.get(`/config/versions/${version}/diff`)
    diffData.value = res.data?.data
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

// 回滚会用历史版本覆盖整个运行配置，属于影响面最大的操作，必须先确认
function rollback(row: any) {
  dialog.warning({
    title: t('configVersion.rollbackConfirmTitle'),
    content: t('configVersion.rollbackConfirm', { version: row.version }),
    positiveText: t('common.confirm'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => doRollback(row),
  })
}

async function doRollback(row: any) {
  try {
    const res = await http.post(`/config/versions/${row.version}/rollback`)
    if (res.data?.data?.rolled_back) {
      message.success(t('configVersion.rollbackSuccess'))
    } else {
      message.error(t('configVersion.rollbackFailed'))
    }
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('configVersion.rollbackFailed')))
  }
}

onMounted(() => {
  refresh()
})
</script>

<style scoped>
.page-container { padding: 16px; }
.diff-viewer { display: flex; gap: 12px; }
.diff-old, .diff-new { flex: 1; }
.diff-header { font-weight: bold; margin-bottom: 8px; }
.diff-old .diff-header { color: #ff6600; }
.diff-new .diff-header { color: #00aa00; }
pre { background: #f5f5f5; padding: 12px; border-radius: 4px; font-size: 13px; overflow: auto; max-height: 400px; }
</style>
