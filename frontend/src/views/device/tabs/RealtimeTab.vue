<template>
  <n-data-table
    :columns="columns"
    :data="realtimeData"
    size="small"
    :pagination="{ pageSize: 50 }"
  >
    <template #empty>
      <n-empty :description="t('deviceDetail.noRealtimeData')" size="small" />
    </template>
  </n-data-table>
</template>

<script setup lang="ts">
import { ref, computed, watch, onMounted, onScopeDispose } from 'vue'
import { useDeviceDetailConsumer } from '../composables/useDeviceDetail'
import { deviceApi } from '@/api'
import { t } from '@/i18n'
import { NDataTable, NEmpty } from 'naive-ui'
import { connect, disconnect } from '@/api/websocket'
import { formatDateTime } from '@/utils/datetime'

const { device } = useDeviceDetailConsumer()
const realtimeData = ref<any[]>([])

const columns = computed(() => [
  { title: t('deviceDetail.pointName'), key: 'name', width: 150 },
  { title: t('common.value'), key: 'value', width: 120, render: (row: any) => {
    const v = row.value
    if (v === undefined || v === null || v === '-') return '-'
    if (typeof v === 'boolean') return v ? 'true' : 'false'
    if (typeof v === 'number') return Number.isInteger(v) ? String(v) : String(Math.round(v * 10000) / 10000)
    return String(v)
  } },
  { title: t('common.time'), key: 'timestamp', width: 180, render: (row: any) => formatDateTime(row.timestamp) },
  { title: t('deviceDetail.unit'), key: 'unit', width: 80 },
])

async function fetchRealtime() {
  if (!device.value?.device_id) return
  try {
    // 后端返回 {点名: 数值} 简单映射（旧版本可能为 {点名: {value, timestamp, unit}} 对象），两种都兼容
    const points = await deviceApi.getPoints(device.value.device_id)
    realtimeData.value = Object.entries(points || {}).map(([name, val]: [string, any]) => {
      const isObj = val !== null && typeof val === 'object'
      return {
        name,
        value: isObj ? (val?.value ?? '-') : (val ?? '-'),
        timestamp: (isObj ? (val?.timestamp || val?.time) : null) || new Date().toISOString(),
        unit: isObj ? (val?.unit || '') : '',
      }
    })
  } catch {
    realtimeData.value = []
  }
}

function upsertPoint(name: string, value: any, timestamp: string) {
  if (!name) return
  const row = { name, value: value ?? '-', timestamp: timestamp || new Date().toISOString(), unit: '' }
  const existing = realtimeData.value.findIndex(d => d.name === name)
  if (existing >= 0) realtimeData.value[existing] = row
  else realtimeData.value.push(row)
}

function onWsMessage(data: any) {
  // 后端 realtime 通道广播 collect_scheduler 的 data_collected 事件：
  // {type:'data_collected', data:{device_id, points:[{point_name, value, quality, timestamp}]}}
  if (data?.type === 'data_collected' && data?.data?.device_id === device.value?.device_id) {
    const points = Array.isArray(data.data.points) ? data.data.points : []
    points.forEach((p: any) => upsertPoint(p.point_name || p.name, p.value, p.timestamp))
  }
}

// 详情页各 tab pane 在 device 加载完成前就已挂载（display-directive="show"），
// 必须等 device 就绪后再拉取，否则早退后该 tab 永远为空
watch(() => device.value?.device_id, (id) => { if (id) fetchRealtime() }, { immediate: true })

onMounted(() => {
  connect('realtime', onWsMessage)
})

onScopeDispose(() => {
  disconnect('realtime', onWsMessage)
})
</script>
