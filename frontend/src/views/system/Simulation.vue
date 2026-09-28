<template>
  <div class="page-container">
    <n-card :title="t('router.simulation')" size="small">
      <n-space vertical>
        <n-space>
          <n-switch v-model:value="autoCreate" @update:value="updateAutoCreate" />
          <span>{{ t('simulation.autoCreate') }}</span>
          <n-button type="primary" @click="showCreate = true">{{ t('simulation.addDevice') }}</n-button>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <n-data-table
          :columns="deviceColumns"
          :data="devices"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 15 }"
        />
      </n-space>
    </n-card>

    <!-- Create Simulator Device Modal -->
    <n-modal v-model:show="showCreate" preset="card" style="width: 600px;" :title="t('simulation.addDevice')">
      <n-form label-placement="top">
        <n-form-item :label="t('simulation.deviceId')">
          <n-input v-model:value="newDevice.device_id" />
        </n-form-item>
        <n-form-item :label="t('simulation.deviceName')">
          <n-input v-model:value="newDevice.name" />
        </n-form-item>
        <n-form-item :label="t('simulation.collectInterval')">
          <n-input-number v-model:value="newDevice.collect_interval" :min="1" :max="3600" />
          <span style="margin-left: 8px; color: #999;">s</span>
        </n-form-item>
        <n-form-item :label="t('simulation.points')">
          <n-input
            v-model:value="pointsJson"
            type="textarea"
            :rows="8"
            placeholder='[{"name":"temp","data_type":"float","min":20,"max":80,"mode":"sine"}]'
          />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showCreate = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" @click="createDevice" :loading="creating">{{ t('common.save') }}</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message, dialog } from '@/utils/discreteApi'
import { NButton } from 'naive-ui'

const autoCreate = ref(false)
const devices = ref<any[]>([])
const showCreate = ref(false)
const creating = ref(false)
const pointsJson = ref('[{"name":"temp","data_type":"float","min":20,"max":80,"mode":"sine"},{"name":"pressure","data_type":"float","min":0.1,"max":1.5,"mode":"random"},{"name":"flow","data_type":"float","min":50,"max":200,"mode":"ramp"}]')
const newDevice = ref({ device_id: '', name: '', collect_interval: 5 })

const deviceColumns = computed(() => [
  { title: t('simulation.deviceId'), key: 'device_id' },
  { title: t('simulation.deviceName'), key: 'name' },
  { title: t('simulation.pointCount'), key: 'point_count' },
  { title: t('simulation.collectInterval'), key: 'collect_interval', render: (r: any) => `${r.collect_interval}s` },
  {
    title: t('common.actions'),
    key: 'actions',
    render: (row: any) => h(NButton, { size: 'small', type: 'error', text: true, onClick: () => deleteDevice(row) }, () => t('common.delete')),
  },
])

async function refresh() {
  try {
    const res = await http.get('/simulation/devices')
    devices.value = res.data?.data?.items || []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function loadConfig() {
  try {
    const res = await http.get('/simulation/config')
    autoCreate.value = !!res.data?.data?.auto_create
  } catch {}
}

async function updateAutoCreate(val: boolean) {
  try {
    await http.put('/simulation/auto-create', { auto_create: val })
    message.success(t('common.saveSuccess'))
  } catch (e: any) {
    message.error(extractError(e, t('common.saveFailed')))
    autoCreate.value = !val
  }
}

async function createDevice() {
  let points: any[]
  try {
    points = JSON.parse(pointsJson.value)
  } catch {
    message.error(t('simulation.invalidJson'))
    return
  }
  creating.value = true
  try {
    await http.post('/simulation/devices', { ...newDevice.value, points })
    message.success(t('common.createSuccess'))
    showCreate.value = false
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.createFailed')))
  } finally {
    creating.value = false
  }
}

function deleteDevice(row: any) {
  dialog.warning({
    title: t('common.confirmDeleteName', { name: row.name ?? row.device_id }),
    content: t('common.confirmDeleteDesc'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => { void doDeleteDevice(row) },
  })
}

async function doDeleteDevice(row: any) {
  try {
    await http.delete(`/simulation/devices/${row.device_id}`)
    message.success(t('common.deleteSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.deleteFailed')))
  }
}

onMounted(() => {
  loadConfig()
  refresh()
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
