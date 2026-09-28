<template>
  <ServiceManager
    :key="reloadSeq"
    service-name="serial_bridge"
    :display-name="t('system.serialBridge.title')"
    :running-fields="runningFields"
    @status-loaded="onStatusLoaded"
  ><!-- FIXED: 原问题-中文硬编码，改用i18n -->
    <template #extra>
      <n-card v-if="running" :title="t('system.serialBridge.transferStats')" :bordered="false" style="margin-top: 12px"><!-- FIXED: 原问题-中文硬编码，改用i18n -->
        <n-descriptions label-placement="left" :column="2" bordered>
          <n-descriptions-item :label="t('system.serialBridge.fromSerial')">{{ bridgeStats.bytes_from_serial }} {{ t('system.serialBridge.bytes') }}</n-descriptions-item><!-- FIXED: 原问题-绑定了后端从不返回的 running_info 键，六个数字恒为 0 -->
          <n-descriptions-item :label="t('system.serialBridge.toSerial')">{{ bridgeStats.bytes_to_serial }} {{ t('system.serialBridge.bytes') }}</n-descriptions-item><!-- FIXED: 原问题-中文硬编码，改用i18n -->
          <n-descriptions-item :label="t('system.serialBridge.clientCount')">{{ bridgeStats.client_count }}</n-descriptions-item><!-- FIXED: 原问题-中文硬编码，改用i18n -->
          <n-descriptions-item :label="t('system.serialBridge.totalConnections')">{{ bridgeStats.total_connections }}</n-descriptions-item><!-- FIXED: 原问题-中文硬编码，改用i18n -->
          <n-descriptions-item :label="t('system.serialBridge.rejected')">{{ bridgeStats.rejected }}</n-descriptions-item><!-- FIXED: 原问题-中文硬编码，改用i18n -->
          <n-descriptions-item :label="t('system.serialBridge.lastClientAt')">{{ bridgeStats.last_client_at || '-' }}</n-descriptions-item><!-- FIXED: 原问题-中文硬编码，改用i18n -->
          <n-descriptions-item :label="t('system.serialBridge.lineMode')">{{ lineMode }}</n-descriptions-item><!-- FIXED: 原问题-中文硬编码，改用i18n -->
          <n-descriptions-item :label="t('system.listenAddress')">{{ bridgeStats.listen_addr || '-' }}</n-descriptions-item><!-- FIXED: 原问题-中文硬编码，改用i18n -->
        </n-descriptions>
        <n-alert v-if="bridgeStats.last_error" type="warning" :show-icon="true" style="margin-top: 8px">
          {{ bridgeStats.last_error }}
        </n-alert>
      </n-card>

      <n-card size="small" :title="t('serialBridge.configTitle')" style="margin-top: 12px">
        <n-spin :show="configLoading">
          <n-form ref="formRef" :model="form" :rules="formRules" label-placement="left" label-width="auto">
            <n-grid :cols="2" :x-gap="16">
              <n-gi>
                <n-form-item :label="t('serialBridge.serialDevice')" path="serial_port">
                  <n-select
                    v-model:value="form.serial_port"
                    :options="portOptions"
                    filterable
                    tag
                    :placeholder="t('serialBridge.devicePlaceholder')"
                  />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item :label="t('serialBridge.baudRate')" path="baud_rate">
                  <n-select v-model:value="form.baud_rate" :options="baudOptions" filterable tag />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item :label="t('serialBridge.dataBits')" path="data_bits">
                  <n-select v-model:value="form.data_bits" :options="dataBitsOptions" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item :label="t('serialBridge.parity')" path="parity">
                  <n-select v-model:value="form.parity" :options="parityOptions" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item :label="t('serialBridge.stopBits')" path="stop_bits">
                  <n-select v-model:value="form.stop_bits" :options="stopBitsOptions" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item :label="t('serialBridge.tcpPort')" path="tcp_port">
                  <n-input-number v-model:value="form.tcp_port" :min="1" :max="65535" style="width: 100%" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item :label="t('serialBridge.maxClients')" path="max_clients">
                  <n-input-number v-model:value="form.max_clients" :min="0" :max="10000" style="width: 100%" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item :label="t('serialBridge.ipWhitelist')" path="ip_whitelist">
                  <n-dynamic-tags v-model:value="form.ip_whitelist" />
                </n-form-item>
              </n-gi>
            </n-grid>
            <n-space vertical size="small">
              <n-text depth="3">{{ t('serialBridge.maxClientsHint') }} · {{ t('serialBridge.whitelistHint') }}</n-text>
              <n-text v-if="running && liveDiff.length" type="warning">
                {{ t('serialBridge.liveDiff', { items: liveDiff.join(', ') }) }}
              </n-text>
              <n-space>
                <n-button type="primary" size="small" :loading="saving" @click="saveConfig">
                  {{ running ? t('serialBridge.saveAndApply') : t('common.save') }}
                </n-button>
                <n-button size="small" :disabled="saving" @click="loadConfig">{{ t('serialBridge.discard') }}</n-button>
              </n-space>
            </n-space>
          </n-form>
        </n-spin>
      </n-card>

      <n-card size="small" :title="t('serialBridge.detectedPorts')" style="margin-top: 12px">
        <n-space vertical size="small">
          <template v-if="portsError">
            <n-text type="error">{{ portsError }}</n-text>
          </template>
          <template v-else-if="portInfo">
            <n-space v-if="portInfo.ports.length" size="small">
              <n-tag v-for="p in portInfo.ports" :key="p" size="small" :type="p === portInfo.configured ? 'success' : 'default'">{{ p }}</n-tag>
            </n-space>
            <n-text v-else type="warning">{{ t('serialBridge.noPortsFound') }}</n-text>
            <n-text v-if="portInfo.configured && !portInfo.configured_present" type="error">
              {{ t('serialBridge.configuredAbsent', { port: portInfo.configured }) }}
            </n-text>
          </template>
          <n-text v-else depth="3">-</n-text>
          <div>
            <n-button size="small" :loading="portsLoading" @click="refresh">{{ t('common.refresh') }}</n-button>
          </div>
        </n-space>
      </n-card>
    </template>
  </ServiceManager>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted } from 'vue'
import { t } from '@/i18n'  // FIXED: 原问题-中文硬编码，改用i18n
import { message as msg } from '@/utils/discreteApi'
import ServiceManager from '@/components/ServiceManager.vue'
import { serialBridgeApi } from '@/api'
import { extractError } from '@/utils/errorCodes'

interface PortInfo {
  ports: string[]
  configured: string
  configured_present: boolean
}

const statusData = ref<any>({})
const running = ref(false)
const reloadSeq = ref(0)
const portInfo = ref<PortInfo | null>(null)
const portsError = ref('')
const portsLoading = ref(false)
// FIXED: 原问题-字段名照抄了后端从未返回的 running_info 结构，整卡恒为 0
const bridgeStats = reactive({
  bytes_from_serial: 0,
  bytes_to_serial: 0,
  client_count: 0,
  total_connections: 0,
  rejected: 0,
  last_client_at: '',
  last_error: '',
  serial_port: '',
  baud_rate: 0,
  data_bits: 0,
  parity: '',
  stop_bits: 0,
  listen_addr: '',
})

// The line settings the bridge actually applied to the device, so a config that
// never reached the port cannot pass for a working link.
const lineMode = computed(() => {
  if (!bridgeStats.baud_rate) return '-'
  const parity = bridgeStats.parity || 'N'
  return `${bridgeStats.baud_rate} ${bridgeStats.data_bits || 8}${parity}${bridgeStats.stop_bits || 1}`
})

const runningFields = computed(() => [
  // FIXED: 原问题-未配置时显示编造的 /dev/ttyUSB0，与真实配置无法区分
  { label: t('serialBridge.serialDevice'), value: statusData.value.current_config?.serial_port || t('serialBridge.notSet') },
  { label: t('serialBridge.baudRate'), value: statusData.value.current_config?.baud_rate || 9600 },  // FIXED: 原问题-硬编码中文label
  { label: t('serialBridge.tcpPort'), value: statusData.value.current_config?.tcp_port || 9000 },  // FIXED: 原问题-硬编码中文label
])

// ─── 透传参数表单 ───
// FIXED: 原问题-后端 GET/PUT /serial-bridge/config 一直存在且会写盘，但控制台里
// 没有任何地方能编辑 serial_bridge：运维看到"配置的串口不在本机列表里"也无法改正
const formRef = ref<any>(null)
const configLoading = ref(false)
const saving = ref(false)
const form = reactive({
  serial_port: '',
  baud_rate: 9600,
  data_bits: 8,
  parity: 'N',
  stop_bits: 1,
  tcp_port: 9000,
  max_clients: 5,
  ip_whitelist: [] as string[],
})

const portOptions = computed(() => (portInfo.value?.ports ?? []).map((p) => ({ label: p, value: p })))
const baudOptions = [1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200, 230400, 460800, 921600]
  .map((b) => ({ label: String(b), value: b }))
const dataBitsOptions = [5, 6, 7, 8].map((b) => ({ label: String(b), value: b }))
const stopBitsOptions = [1, 2].map((b) => ({ label: String(b), value: b }))
const parityOptions = computed(() => [
  { label: t('serialBridge.parityNone'), value: 'N' },
  { label: t('serialBridge.parityEven'), value: 'E' },
  { label: t('serialBridge.parityOdd'), value: 'O' },
  { label: t('serialBridge.parityMark'), value: 'M' },
  { label: t('serialBridge.paritySpace'), value: 'S' },
])

const formRules = computed(() => ({
  serial_port: { required: true, message: t('serialBridge.deviceRequired'), trigger: ['input', 'blur'] },
  baud_rate: { type: 'number', required: true, min: 1, message: t('serialBridge.baudRequired'), trigger: ['input', 'change'] },
  tcp_port: { type: 'number', required: true, min: 1, max: 65535, message: t('serialBridge.tcpPortRange'), trigger: ['input', 'blur'] },
}))

// What the running bridge is actually serving, versus what the file now says.
// Saving restarts the bridge on the stored section, so a leftover difference
// means the live service is not the one this form describes.
const liveDiff = computed(() => {
  if (!running.value || !bridgeStats.listen_addr) return []
  const out: string[] = []
  const livePort = Number(String(bridgeStats.listen_addr).split(':').pop())
  if (livePort && livePort !== form.tcp_port) out.push(t('serialBridge.tcpPort'))
  if (bridgeStats.serial_port && bridgeStats.serial_port !== form.serial_port) out.push(t('serialBridge.serialDevice'))
  if (bridgeStats.baud_rate && bridgeStats.baud_rate !== form.baud_rate) out.push(t('serialBridge.baudRate'))
  return out
})

async function loadConfig() {
  configLoading.value = true
  try {
    const data = await serialBridgeApi.getConfig()
    form.serial_port = data?.serial_port ?? ''
    form.baud_rate = Number(data?.baud_rate) || 9600
    form.data_bits = Number(data?.data_bits) || 8
    form.parity = (data?.parity || 'N').toString().toUpperCase()
    form.stop_bits = Number(data?.stop_bits) || 1
    form.tcp_port = Number(data?.tcp_port) || 9000
    form.max_clients = Number(data?.max_clients ?? 5)
    form.ip_whitelist = Array.isArray(data?.ip_whitelist) ? data.ip_whitelist.map(String) : []
  } catch (e: any) {
    msg.error(extractError(e))
  } finally {
    configLoading.value = false
  }
}

async function saveConfig() {
  // n-form resolves with an (empty) errors object when everything passes, so the
  // result must not be treated as a truthy failure - only a rejection is one.
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  // A tagged n-select hands back what the user typed, so the numbers are
  // normalised here: the backend binds this body into typed fields.
  const baud = Number(form.baud_rate)
  const tcpPort = Number(form.tcp_port)
  if (!Number.isInteger(baud) || baud <= 0) {
    msg.error(t('serialBridge.baudRequired'))
    return
  }
  if (!Number.isInteger(tcpPort) || tcpPort < 1 || tcpPort > 65535) {
    msg.error(t('serialBridge.tcpPortRange'))
    return
  }
  saving.value = true
  try {
    await serialBridgeApi.updateConfig({
      ...form,
      serial_port: form.serial_port.trim(),
      baud_rate: baud,
      tcp_port: tcpPort,
      max_clients: Number(form.max_clients) || 0,
      ip_whitelist: form.ip_whitelist.map((s) => s.trim()).filter(Boolean),
    })
    msg.success(running.value ? t('serialBridge.savedApplied') : t('serialBridge.savedNotRunning'))
    // Re-read both truths the page reports: the stored section, and what the
    // host actually has plugged in now.
    await loadConfig()
    reloadSeq.value += 1
    fetchPorts()
  } catch (e: any) {
    msg.error(extractError(e))
  } finally {
    saving.value = false
  }
}

// FIXED: 原问题-后端 /serial-bridge/ports 已改为真实枚举，前端此前无消费者，页面只能猜设备名
async function fetchPorts() {
  portsLoading.value = true
  portsError.value = ''
  try {
    const data = await serialBridgeApi.getPorts()
    portInfo.value = {
      ports: Array.isArray(data?.ports) ? data.ports : [],
      configured: typeof data?.configured === 'string' ? data.configured : '',
      configured_present: data?.configured_present === true,
    }
  } catch (e: any) {
    portInfo.value = null
    portsError.value = extractError(e)
  } finally {
    portsLoading.value = false
  }
}

function onStatusLoaded(data: any) {
  statusData.value = data
  running.value = data.state === 'running'
  if (data.stats) {
    Object.assign(bridgeStats, data.stats)
  }
}

// One click refreshes both truths this page reports: what the host has, and
// what the running bridge has moved since it started.
function refresh() {
  reloadSeq.value += 1
  fetchPorts()
}

onMounted(() => {
  loadConfig()
  fetchPorts()
})
</script>
