<template>
  <div class="page-container">
    <n-card :title="t('router.debug')" size="small">
      <n-space vertical>
        <!-- Protocol Selector -->
        <n-form inline>
          <n-form-item :label="t('protocolDebug.protocol')">
            <n-select v-model:value="protocol" :options="protocolOptions" style="width: 200px;" />
          </n-form-item>
          <n-form-item :label="t('protocolDebug.host')">
            <n-input v-model:value="host" placeholder="192.168.1.1" style="width: 160px;" />
          </n-form-item>
          <n-form-item :label="t('protocolDebug.port')">
            <n-input-number v-model:value="port" :min="1" :max="65535" style="width: 100px;" />
          </n-form-item>
          <n-form-item v-if="isModbus" :label="t('protocolDebug.unitId')">
            <n-input-number v-model:value="unitId" :min="1" :max="247" style="width: 80px;" />
          </n-form-item>
        </n-form>
        <n-text depth="3" style="font-size: 12px;">{{ addressHint }}</n-text>

        <!-- Read Operation -->
        <n-divider />
        <n-h3>{{ t('protocolDebug.read') }}</n-h3>
        <n-form inline>
          <n-form-item v-if="isModbus" :label="t('protocolDebug.functionCode')">
            <n-select v-model:value="readFc" :options="fcOptions" style="width: 200px;" />
          </n-form-item>
          <n-form-item :label="t('protocolDebug.startAddress')">
            <n-input v-model:value="readAddr" :placeholder="addressPlaceholder" style="width: 160px;" />
          </n-form-item>
          <n-form-item v-if="isModbus" :label="t('protocolDebug.quantity')">
            <n-input-number v-model:value="readQty" :min="1" :max="125" style="width: 80px;" />
          </n-form-item>
          <n-form-item>
            <n-button type="primary" :loading="reading" @click="doRead">{{ t('protocolDebug.read') }}</n-button>
          </n-form-item>
        </n-form>

        <!-- Write Operation -->
        <n-divider />
        <n-h3>{{ t('protocolDebug.write') }}</n-h3>
        <n-form inline>
          <n-form-item v-if="isModbus" :label="t('protocolDebug.functionCode')">
            <n-select v-model:value="writeFc" :options="writeFcOptions" style="width: 200px;" />
          </n-form-item>
          <n-form-item :label="t('protocolDebug.address')">
            <n-input v-model:value="writeAddr" :placeholder="addressPlaceholder" style="width: 160px;" />
          </n-form-item>
          <n-form-item :label="t('protocolDebug.value')">
            <n-input v-model:value="writeValue" style="width: 120px;" />
          </n-form-item>
          <n-form-item>
            <n-button type="warning" :loading="writing" @click="doWrite">{{ t('protocolDebug.write') }}</n-button>
          </n-form-item>
        </n-form>


        <!-- Response -->
        <n-divider />
        <n-h3>{{ t('protocolDebug.response') }}</n-h3>
        <n-code :code="responseText" language="json" />
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message } from '@/utils/discreteApi'

// Per-protocol metadata for the debug form: default port, whether the driver
// uses Modbus addressing (function code / unit id / quantity), the address
// hint text key and an example address used as the input placeholder.
const PROTOCOL_DEBUG_META: Record<string, { port: number; modbus: boolean; hintKey: string; example: string }> = {
  modbus_tcp: { port: 502, modbus: true, hintKey: 'protocolDebug.modbusAddressHint', example: 'HR200' },
  modbus_rtu: { port: 502, modbus: true, hintKey: 'protocolDebug.modbusAddressHint', example: 'HR200' },
  s7: { port: 102, modbus: false, hintKey: 'protocolDebug.s7AddressHint', example: 'DB1.DBD0' },
  mc: { port: 5000, modbus: false, hintKey: 'protocolDebug.mcAddressHint', example: 'D100' },
  fins: { port: 9600, modbus: false, hintKey: 'protocolDebug.finsAddressHint', example: 'D100' },
  cip: { port: 44818, modbus: false, hintKey: 'protocolDebug.cipAddressHint', example: 'N7:0' },
  opc_ua: { port: 4840, modbus: false, hintKey: 'protocolDebug.opcuaAddressHint', example: 'ns=2;s=Tag1' },
  onvif: { port: 8000, modbus: false, hintKey: 'protocolDebug.onvifAddressHint', example: '' },
}

const protocol = ref('modbus_tcp')
const host = ref('')
const port = ref(502)
const unitId = ref(1)
const readFc = ref('read_holding')
const readAddr = ref('0')
const readQty = ref(10)
const writeFc = ref('write_single_register')
const writeAddr = ref('0')
const writeValue = ref('0')
const reading = ref(false)
const writing = ref(false)
const responseText = ref(`// ${t('protocolDebug.noResponse')}`)

const currentMeta = computed(() => PROTOCOL_DEBUG_META[protocol.value] ?? { port: 502, modbus: false, hintKey: '', example: '' })
const isModbus = computed(() => currentMeta.value.modbus)
const addressHint = computed(() => (currentMeta.value.hintKey ? t(currentMeta.value.hintKey) : ''))
const addressPlaceholder = computed(() => currentMeta.value.example || '0')

// When the protocol changes, reset the port to that protocol's default and
// swap the address field to a representative example for its syntax.
watch(protocol, (p) => {
  const meta = PROTOCOL_DEBUG_META[p]
  if (meta) {
    port.value = meta.port
    readAddr.value = meta.example || '0'
    writeAddr.value = meta.example || '0'
  }
})


const protocolOptions = [
  { label: 'Modbus TCP', value: 'modbus_tcp' },
  { label: 'Modbus RTU', value: 'modbus_rtu' },
  { label: 'S7 (Siemens)', value: 's7' },
  { label: 'MC (Mitsubishi)', value: 'mc' },
  { label: 'FINS (Omron)', value: 'fins' },
  { label: 'CIP (AB)', value: 'cip' },
  { label: 'OPC UA', value: 'opc_ua' },
  { label: 'ONVIF', value: 'onvif' },
]

const fcOptions = [
  { label: 'Read Coils (FC01)', value: 'read_coils' },
  { label: 'Read Discrete Inputs (FC02)', value: 'read_discrete' },
  { label: 'Read Holding Registers (FC03)', value: 'read_holding' },
  { label: 'Read Input Registers (FC04)', value: 'read_input' },
]

const writeFcOptions = [
  { label: 'Write Single Coil (FC05)', value: 'write_single_coil' },
  { label: 'Write Single Register (FC06)', value: 'write_single_register' },
  { label: 'Write Multiple Coils (FC0F)', value: 'write_multiple_coils' },
  { label: 'Write Multiple Registers (FC10)', value: 'write_multiple_registers' },
]

async function doRead() {
  reading.value = true
  responseText.value = `// ${t('protocolDebug.sending')}`
  try {
    const res = await http.post('/debug/protocol-read', {
      protocol: protocol.value,
      host: host.value,
      port: port.value,
      unit_id: unitId.value,
      function_code: readFc.value,
      start_address: readAddr.value,
      quantity: readQty.value,
    })
    responseText.value = JSON.stringify(res.data, null, 2)
    message.success(t('protocolDebug.readSuccess'))
  } catch (e: any) {
    responseText.value = `Error: ${extractError(e)}`
    message.error(extractError(e, t('protocolDebug.readFailed')))
  } finally {
    reading.value = false
  }
}

async function doWrite() {
  writing.value = true
  responseText.value = `// ${t('protocolDebug.sendingWrite')}`
  try {
    const res = await http.post('/debug/protocol-write', {
      protocol: protocol.value,
      host: host.value,
      port: port.value,
      unit_id: unitId.value,
      function_code: writeFc.value,
      address: writeAddr.value,
      value: writeValue.value,
    })
    responseText.value = JSON.stringify(res.data, null, 2)
    message.success(t('protocolDebug.writeSuccess'))
  } catch (e: any) {
    responseText.value = `Error: ${extractError(e)}`
    message.error(extractError(e, t('protocolDebug.writeFailed')))
  } finally {
    writing.value = false
  }
}
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
