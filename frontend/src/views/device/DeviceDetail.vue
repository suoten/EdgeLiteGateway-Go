<template>
  <n-spin :show="pageLoading" :description="t('deviceDetail.loadingDevice')">
  <n-space vertical :size="16">
    <template v-if="notFound">
      <n-result status="404" :title="t('deviceDetail.deviceNotExist')" :description="t('deviceDetail.deviceNotExistDesc')">
        <template #footer><n-button type="primary" @click="goBack">{{ t('deviceDetail.backToList') }}</n-button></template>
      </n-result>
    </template>
    <!-- 加载失败时不能继续渲染空壳页面：那会让"拿不到数据"看起来像"设备没有数据" -->
    <template v-else-if="loadFailed">
      <n-result status="500" :title="t('deviceDetail.loadFailedTitle')" :description="t('deviceDetail.loadFailedDesc')">
        <template #footer>
          <n-space justify="center">
            <n-button @click="goBack">{{ t('deviceDetail.backToList') }}</n-button>
            <n-button type="primary" :loading="pageLoading" @click="ctx.refresh">{{ t('common.retry') }}</n-button>
          </n-space>
        </template>
      </n-result>
    </template>
    <template v-else>
    <n-page-header @back="goBack" :title="device?.name ?? ''" :subtitle="device?.device_id ?? ''">
      <template #extra>
        <n-space>
          <n-tag :type="deviceStatusColor[device?.status ?? ''] || 'default'">{{ deviceStatusLabel[device?.status ?? ''] || device?.status }}</n-tag>
          <n-tag type="info" :bordered="false">{{ protocolLabel[device?.protocol ?? ''] || device?.protocol }}</n-tag>
          <n-tag v-if="protocolMeta?.experimental" type="warning" size="small">{{ t('capabilities.experimental') }}</n-tag>
          <template v-if="protocolMeta?.capabilities">
            <n-tag v-if="protocolMeta.capabilities.discover" size="small" :bordered="false">{{ t('capabilities.discover') }}</n-tag>
            <n-tag v-if="protocolMeta.capabilities.write" size="small" :bordered="false">{{ t('capabilities.write') }}</n-tag>
            <n-tag v-if="protocolMeta.capabilities.subscribe" size="small" :bordered="false">{{ t('capabilities.subscribe') }}</n-tag>
          </template>
        </n-space>
      </template>
    </n-page-header>
    <!-- 修复2: 快捷操作工具栏 -->
    <n-space :size="8">
      <n-button size="small" @click="ctx.fetchDevice" :loading="ctx.pageLoading.value">{{ t('common.refresh') }}</n-button>
      <n-button size="small" type="primary" @click="ctx.startEdit">{{ t('common.edit') }}</n-button>
      <n-button size="small" @click="ctx.handleResetHealthConfirm" :loading="ctx.resettingHealth.value">{{ t('healthDetail.resetHealth') }}</n-button>
      <n-button size="small" @click="ctx.runSelfTest" :loading="ctx.selfTestRunning.value">{{ t('selfTest.title') }}</n-button>
      <n-button size="small" @click="ctx.exportPointsToCsv">{{ t('deviceDetail.exportPoints') }}</n-button>
    </n-space>
    <n-tabs v-model:value="activeTab" type="line" animated display-directive="show" @update:value="onTabChange">
      <n-tab-pane name="overview" :tab="t('deviceDetail.overview')"><OverviewTab /></n-tab-pane>
      <n-tab-pane name="points" :tab="t('deviceDetail.pointDefinition')"><PointsTab /></n-tab-pane>
      <n-tab-pane name="realtime" :tab="t('deviceDetail.realtimeData')"><RealtimeTab /></n-tab-pane>
      <n-tab-pane name="write" :tab="t('deviceDetail.dataWrite')"><WriteTab /></n-tab-pane>
      <n-tab-pane name="chart" :tab="t('deviceDetail.timeSeriesChart')"><ChartTab /></n-tab-pane>
      <n-tab-pane name="health" :tab="t('healthDetail.title')"><HealthTab /></n-tab-pane>
      <n-tab-pane name="selftest" :tab="t('selfTest.title')"><SelfTestTab /></n-tab-pane>
      <!-- 修复10: 错误日志 Tab -->
      <n-tab-pane name="errorlogs" :tab="t('deviceDetail.errorLogs')">
        <n-data-table :columns="errorLogColumns" :data="errorLogs" :loading="errorLogsLoading" size="small"
          :pagination="{ pageSize: 20, pageSizes: [10, 20, 50, 100], showSizePicker: true }">
          <template #empty>
            <n-empty :description="errorLogsError || t('common.noData')" size="small" />
          </template>
        </n-data-table>
      </n-tab-pane>
      <!-- 修复11: 通信报文 Tab -->
      <n-tab-pane name="packets" :tab="t('deviceDetail.commPackets')">
        <n-data-table :columns="packetColumns" :data="commPackets" :loading="packetsLoading" size="small"
          :pagination="{ pageSize: 20, pageSizes: [10, 20, 50, 100], showSizePicker: true }">
          <template #empty>
            <n-empty :description="packetsError || packetsNote || t('common.noData')" size="small" />
          </template>
        </n-data-table>
      </n-tab-pane>
      <!-- The detail component reads protocol/config/device; passing it device-id and
           device-base meant props.protocol was never supplied, so the section silently
           rendered nothing (and Vue warned about the missing required prop). -->
      <component v-if="protocolDetailComponent && device" :is="protocolDetailComponent"
        :protocol="device.protocol" :config="device.config ?? {}" :device="device" />
    </n-tabs>
    </template>
  </n-space>
  </n-spin>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useDeviceDetailProvider } from './composables/useDeviceDetail'
import { deviceStatusLabel, deviceStatusColor, protocolLabel } from '@/utils/enumLabels'
import { t } from '@/i18n'
import { getProtocolDetailComponent } from './protocols/index'
import { debugApi, logApi } from '@/api'
import { extractError } from '@/utils/errorCodes'
import { formatDateTime } from '@/utils/datetime'
import OverviewTab from './tabs/OverviewTab.vue'
import PointsTab from './tabs/PointsTab.vue'
import RealtimeTab from './tabs/RealtimeTab.vue'
import WriteTab from './tabs/WriteTab.vue'
import ChartTab from './tabs/ChartTab.vue'
import HealthTab from './tabs/HealthTab.vue'
import SelfTestTab from './tabs/SelfTestTab.vue'

const router = useRouter()
const route = useRoute()
const ctx = useDeviceDetailProvider()
const { device, notFound, loadFailed, pageLoading, activeTab, protocolMeta } = ctx

function onTabChange(tab: string) {
  router.replace({ query: { ...route.query, tab } })
}

// 列表进入详情时 URL 已带上筛选/分页参数，后退可原样还原列表；
// 直接打开详情（无站内历史）才回列表首页
function goBack() {
  if (window.history.state?.back) router.back()
  else router.push('/devices')
}

const protocolDetailComponent = computed(() => getProtocolDetailComponent(device.value?.protocol ?? ''))

// 修复10: 错误日志 Tab
const errorLogs = ref<any[]>([])
const errorLogsLoading = ref(false)
// Why the table is empty: "no errors for this device" and "the log service is
// not reachable" are different statements to an operator.
const errorLogsError = ref('')
const errorLogColumns = computed(() => [
  { title: t('common.time'), key: 'timestamp', width: 180, render: (r: any) => (r.timestamp ? formatDateTime(r.timestamp) : '-') },
  { title: t('common.status'), key: 'level', width: 100 },
  { title: t('common.message'), key: 'message', ellipsis: { tooltip: true } },
])
async function loadErrorLogs() {
  if (!device.value) return
  errorLogsLoading.value = true
  try {
    // The aggregator only keeps entries whose logger attached a device_id field,
    // which every driver does for read/write/connection errors.
    const res = await logApi.list({ device_id: device.value.device_id, level: 'ERROR', page: 1, size: 200 })
    errorLogs.value = (res?.items ?? []).map((e: any) => ({
      timestamp: e.timestamp || '',
      level: e.level || 'ERROR',
      message: e.message || '',
    }))
    errorLogsError.value = ''
  } catch (e: any) {
    errorLogs.value = []
    errorLogsError.value = extractError(e, t('common.loadFailed'))
  } finally {
    errorLogsLoading.value = false
  }
}

// 修复11: 通信报文 Tab
const commPackets = ref<any[]>([])
const packetsLoading = ref(false)
const packetsError = ref('')
// 后端明确回答 capture_enabled=false：没有任何驱动记录报文，空表是能力缺失而不是设备静默
const packetsNote = ref('')
const packetColumns = computed(() => [
  { title: t('common.time'), key: 'timestamp', width: 180, render: (r: any) => (r.timestamp ? formatDateTime(r.timestamp) : '-') },
  { title: t('deviceDetail.packetDirection'), key: 'direction', width: 80 },
  { title: t('deviceDetail.packetContent'), key: 'content', ellipsis: { tooltip: true } },
])
async function loadCommPackets() {
  if (!device.value) return
  packetsLoading.value = true
  try {
    const res = await debugApi.getPackets({ device_id: device.value.device_id, limit: 200 })
    commPackets.value = (res?.packets ?? []).map((p: any) => ({
      timestamp: new Date(p.timestamp * 1000).toISOString(),
      direction: p.direction,
      content: p.content,
    }))
    packetsError.value = ''
    packetsNote.value = res?.capture_enabled === false ? t('deviceDetail.noPacketCapture') : ''
  } catch (e: any) {
    commPackets.value = []
    packetsNote.value = ''
    packetsError.value = extractError(e, t('common.loadFailed'))
  } finally {
    packetsLoading.value = false
  }
}

watch(activeTab, (tab) => {
  if (tab === 'errorlogs') loadErrorLogs()
  else if (tab === 'packets') loadCommPackets()
})
</script>
