<template>
  <n-spin :show="pageLoading" :description="t('common.loading')">
  <n-space vertical :size="16">
    <n-card :title="t('ota.title')" :bordered="false">
      <template #header-extra>
        <n-space>
          <n-button @click="checkUpdate" :loading="checking">{{ t('ota.checkUpdate') }}</n-button>
        </n-space>
      </template>

      <n-descriptions label-placement="left" :column="2" bordered v-if="updateInfo">
        <n-descriptions-item :label="t('ota.currentVersion')">{{ updateInfo.current_version || '-' }}</n-descriptions-item>
        <n-descriptions-item :label="t('ota.latestVersion')">
          <!-- 没配置更新源时后端明确回答 checked:false，这里不能替它绿成“已是最新版本”。 -->
          <n-tag v-if="!updateInfo.checked" type="default" size="small">{{ t('ota.notChecked') }}</n-tag>
          <n-tag v-else-if="updateInfo.has_update" type="warning" size="small">{{ updateInfo.latest_version }}</n-tag>
          <n-tag v-else type="success" size="small">{{ t('otaState.up_to_date') }}</n-tag>
        </n-descriptions-item>
        <n-descriptions-item :label="t('ota.releaseNotes')" :span="2">{{ updateInfo.release_notes || updateInfo.reason || '-' }}</n-descriptions-item>
      </n-descriptions>
      <n-empty v-else :description="t('ota.checkHint')" />

      <n-alert v-if="updateInfo && updateInfo.self_update === false" type="info" :bordered="false" style="margin-top: 16px">
        {{ t('ota.selfUpdateUnsupported') }}
      </n-alert>

      <n-space style="margin-top: 16px" v-if="updateInfo?.has_update">
        <n-popconfirm @positive-click="applyUpdate">
          <template #trigger>
            <n-button type="error" :loading="applying">{{ t('ota.applyUpdate') }}</n-button>
          </template>
          <div style="max-width: 320px">
            <n-text strong>{{ t('ota.confirmApply') }}</n-text>
            <n-p>{{ t('ota.applyWarning') }}</n-p>
            <n-p v-if="updateInfo?.latest_version">{{ t('ota.targetVersion') }}：{{ updateInfo.latest_version }}</n-p>
          </div>
        </n-popconfirm>
      </n-space>
    </n-card>

    <n-card :title="t('ota.backupVersions')" :bordered="false">
      <template #header-extra>
        <n-button @click="fetchBackups" :loading="fetchingBackups">{{ t('common.refresh') || 'Refresh' }}</n-button>
      </template>
      <!-- 后端如实列出备份目录里的文件，但它们是数据库备份，不是可回滚的程序版本，
           所以这里不提供回滚入口：/ota/rollback 只会拒绝，点了也不会退回任何版本。 -->
      <n-text depth="3" style="display:block;margin-bottom:8px">{{ t('ota.backupKindNote') }}</n-text>
      <n-data-table :columns="backupColumns" :data="backups" :bordered="false" size="small">
        <template #empty>
          <n-empty :description="t('ota.noBackup')" />
        </template>
      </n-data-table>
    </n-card>
  </n-space>
  </n-spin>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { NButton, NPopconfirm, NSpin, NAlert, NText, useMessage } from 'naive-ui'
import { appUpdateApi } from '@/api'
// FIXED: 原问题-添加i18n支持
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'

const message = useMessage()
const checking = ref(false)
const applying = ref(false)
const fetchingBackups = ref(false)
const updateInfo = ref<any>(null)
const backups = ref<any[]>([])
const pageLoading = ref(true)

const backupColumns = computed(() => [
  { title: t('ota.version'), key: 'version', ellipsis: { tooltip: true } },
  { title: t('ota.backupTime'), key: 'created_at', width: 220 },
  { title: t('ota.size'), key: 'size', width: 120 },
])

async function checkUpdate() {
  checking.value = true
  try {
    const data = await appUpdateApi.check()
    updateInfo.value = data
  } catch (e: any) {
    message.error(extractError(e, t('ota.checkFailed')))
  } finally { checking.value = false }
}

// FIXED: 原问题-固定10秒reload，实际重启时间不确定，改为轮询/health端点
const HEALTH_POLL_INTERVAL_MS = 2000
const HEALTH_POLL_MAX_ATTEMPTS = 30

function pollHealthAndReload() {
  let attempts = 0
  const apiBase = import.meta.env.VITE_API_BASE_URL || '/api/v1'
  const timer = setInterval(async () => {
    attempts++
    if (attempts > HEALTH_POLL_MAX_ATTEMPTS) {
      clearInterval(timer)
      console.warn('[OTA] Health check timed out after 60s, forcing reload')
      window.location.reload()
      return
    }
    try {
      const resp = await fetch(`${apiBase}/system/status`, { method: 'GET' })
      if (resp.ok) {
        clearInterval(timer)
        window.location.reload()
      }
    } catch {
      // Server not ready yet, continue polling
    }
  }, HEALTH_POLL_INTERVAL_MS)
}

async function applyUpdate() {
  applying.value = true
  try {
    await appUpdateApi.apply()
    // FIXED: 原问题-OTA升级成功后无系统重启提示
    message.success(t('ota.applySuccess') + t('ota.restartSoon'))  // FIXED: 原问题-硬编码中文，改用i18n
    pollHealthAndReload()
  } catch (e: any) {
    message.error(extractError(e, t('ota.applyFailed')))
  } finally { applying.value = false }
}

async function fetchBackups() {
  fetchingBackups.value = true
  try {
    const data = await appUpdateApi.backups()
    backups.value = data?.backups || []
  } catch (e: any) {
    message.error(extractError(e, t('otaUpdate.fetchBackupFailed')))
  } finally {
    pageLoading.value = false
    fetchingBackups.value = false
  }
}

onMounted(fetchBackups)
</script>
