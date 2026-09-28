<template>
  <n-space vertical :size="16">
    <n-card :bordered="false">
      <template #header>
        <n-space align="center" :size="12">
          <span style="font-size: 16px; font-weight: 600">{{ t('mcpServer.title') }}</span>
          <n-tag :type="stateTagType" size="small">{{ stateLabel }}</n-tag>
        </n-space>
      </template>
      <template #header-extra>
        <n-space :size="8">
          <n-switch
            :value="enabled"
            :loading="toggleLoading"
            @update:value="handleToggle"
          >
            <template #checked>{{ t('mcpServer.enabled') }}</template>
            <template #unchecked>{{ t('mcpServer.disabled') }}</template>
          </n-switch>
          <n-button size="small" @click="fetchStatus" :loading="loading">{{ t('mcpServer.refresh') }}</n-button>
        </n-space>
      </template>

      <n-alert
        v-if="missingDeps.length > 0"
        type="warning"
        :bordered="false"
        style="margin-bottom: 12px"
      >
        <template #header>{{ t('mcpServer.missingDepsTitle') }}</template>
        {{ t('mcpServer.missingDepsDesc') }}{{ missingDeps.map(d => d.package).join(', ') }}
        <n-button
          type="primary"
          size="small"
          style="margin-left: 12px"
          @click="handleInstallDeps"
          :loading="installing"
        >{{ t('mcpServer.oneClickInstall') }}</n-button>
      </n-alert>

      <template v-if="enabled">
        <n-descriptions label-placement="left" :column="2" bordered>
          <n-descriptions-item :label="t('mcpServer.transport')">{{ t('mcpServer.transportDesc') }}</n-descriptions-item>
          <n-descriptions-item :label="t('mcpServer.toolsTitle')">{{ tools.length }}</n-descriptions-item>
          <n-descriptions-item :label="t('mcpServer.apiKeyAuth')">
            <n-space align="center" :size="8">
              <n-tag :type="authEnabled ? 'success' : 'default'" size="small">
                {{ authEnabled ? t('mcpServer.authEnabled') : t('mcpServer.authDisabled') }}
              </n-tag>
              <n-text depth="3" style="font-size: 12px">
                {{ authEnabled ? t('mcpServer.authEnabledDesc') : t('mcpServer.authDisabledDesc') }}
              </n-text>
            </n-space>
          </n-descriptions-item>
          <n-descriptions-item :label="t('mcpServer.apiKeyCount')">{{ apiKeys.length }}</n-descriptions-item>
        </n-descriptions>
      </template>

      <n-empty v-else :description="t('mcpServer.notEnabledDesc')" />
    </n-card>

    <template v-if="enabled">
      <n-card :title="t('mcpServer.toolsTitle')" :bordered="false">
        <n-data-table
          :columns="toolColumns"
          :data="tools"
          :loading="loadingTools"
          :bordered="false"
          size="small"
        />
      </n-card>

      <n-card :title="t('mcpServer.resourcesTitle')" :bordered="false">
        <n-data-table
          :columns="resourceColumns"
          :data="resources"
          :loading="loadingResources"
          :bordered="false"
          size="small"
        />
      </n-card>

      <n-card :title="t('mcpServer.promptsTitle')" :bordered="false">
        <n-data-table
          :columns="promptColumns"
          :data="prompts"
          :loading="loadingPrompts"
          :bordered="false"
          size="small"
        />
      </n-card>

      <n-card :title="t('mcpServer.apiKeyTitle')" :bordered="false">
        <!-- The create/delete UI is gone rather than disabled: the backend refuses
             both because there is no key store behind them, so a button here would
             only ever produce an error message. -->
        <n-alert type="info" :bordered="false" style="margin-bottom: 12px">
          {{ t('mcpServer.createKeyUnsupported') }}
        </n-alert>
        <n-data-table
          :columns="keyColumns"
          :data="apiKeys"
          :loading="loadingKeys"
          :bordered="false"
          size="small"
        />
      </n-card>
    </template>

    <n-modal v-model:show="showInstallProgress" :title="t('mcpServer.installTitle')" preset="card" style="width: 480px; max-width: 95vw" :closable="false" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-spin :description="installProgress">
        <n-space vertical>
          <p>{{ t('mcpServer.installDesc') }}</p>
          <p v-if="installResult">{{ installResult }}</p>
        </n-space>
      </n-spin>
      <template #action>
        <n-button @click="showInstallProgress = false" :disabled="installing">{{ t('deviceList.close') }}</n-button>
      </template>
    </n-modal>

    <n-modal v-model:show="showToolCallModal" :title="t('mcpServer.callTool') + ' ' + toolCallName" preset="card" style="width: 600px; max-width: 95vw" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-space vertical :size="12">
        <n-form-item :label="t('mcpServer.paramJson')">
          <n-input v-model:value="toolCallArgs" type="textarea" :rows="6" placeholder='{"key": "value"}' />
        </n-form-item>
        <n-form-item v-if="toolCallResult" :label="t('mcpServer.result')">
          <n-input :value="toolCallResult" type="textarea" :rows="8" readonly />
        </n-form-item>
      </n-space>
      <template #action>
        <n-button @click="showToolCallModal = false">{{ t('deviceList.close') }}</n-button>
        <n-button type="primary" :loading="callingTool" @click="handleToolCall">{{ t('common.confirm') }}</n-button>
      </template>
    </n-modal>
  </n-space>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, h } from 'vue'
import { NTag, NButton } from 'naive-ui'
import { serviceApi, mcpApi } from '@/api'
import type { ServiceDependency } from '@/api'
// FIXED: 原问题-添加i18n支持
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { useAuthStore } from '@/stores/auth'
import { message, dialog } from '@/utils/discreteApi'

const loading = ref(false)
const toggleLoading = ref(false)
const installing = ref(false)
const auth = useAuthStore()
const showInstallProgress = ref(false)
const installProgress = ref('')
const installResult = ref('')

const enabled = ref(false)
const state = ref<string>('disabled')
const dependencies = ref<ServiceDependency[]>([])
const authEnabled = ref(false)

const tools = ref<any[]>([])
const resources = ref<any[]>([])
const prompts = ref<any[]>([])
const apiKeys = ref<any[]>([])

const loadingTools = ref(false)
const loadingResources = ref(false)
const loadingPrompts = ref(false)
const loadingKeys = ref(false)
const showToolCallModal = ref(false)
const callingTool = ref(false)
const toolCallName = ref('')
const toolCallArgs = ref('{}')
const toolCallResult = ref('')
const missingDeps = computed(() => dependencies.value.filter(d => !d.installed))

const stateTagType = computed(() => {
  switch (state.value) {
    case 'running': return 'success'
    case 'enabled': return 'info'
    case 'error': return 'error'
    default: return 'default'
  }
})

// FIXED: 原问题-stateLabel中文硬编码，改为i18n
const stateLabel = computed(() => {
  switch (state.value) {
    case 'running': return t('serviceState.running')
    case 'enabled': return t('serviceState.enabled')
    case 'error': return t('serviceState.error')
    case 'disabled': return t('serviceState.disabled')
    default: return state.value
  }
})

// FIXED: 原问题-表格列标题中文硬编码，改为i18n
const mcpToolDescMap: Record<string, string> = {
  list_devices: 'mcpServer.toolListDevicesDesc',
  get_device_status: 'mcpServer.toolGetDeviceStatusDesc',
  read_device_points: 'mcpServer.toolReadDevicePointsDesc',
  list_alarms: 'mcpServer.toolListAlarmsDesc',
  get_system_status: 'mcpServer.toolGetSystemStatusDesc',
  list_rules: 'mcpServer.toolListRulesDesc',
}

const mcpResourceNameMap: Record<string, string> = {
  devices: 'mcpServer.resourceDevicesName',
  'alarms/active': 'mcpServer.resourceAlarmsName',
  'system/status': 'mcpServer.resourceSystemName',
}
const mcpResourceDescMap: Record<string, string> = {
  devices: 'mcpServer.resourceDevicesDesc',
  'alarms/active': 'mcpServer.resourceAlarmsDesc',
  'system/status': 'mcpServer.resourceSystemDesc',
}

const mcpTemplateNameMap: Record<string, string> = {
  analyze_device: 'mcpServer.templateAnalyzeName',
  alarm_summary: 'mcpServer.templateAlarmSummaryName',
}
const mcpTemplateDescMap: Record<string, string> = {
  analyze_device: 'mcpServer.templateAnalyzeDesc',
  alarm_summary: 'mcpServer.templateAlarmSummaryDesc',
}

const toolColumns = computed(() => [
  { title: t('ruleList.name'), key: 'name', width: 200 },
  {
    title: t('auditLog.detail'), key: 'description', ellipsis: { tooltip: true },
    render: (row: any) => {
      const i18nKey = mcpToolDescMap[row.name]
      return i18nKey ? t(i18nKey) : (row.description || '-')
    },
  },
  {
    title: t('common.actions'), key: 'action', width: 100,
    render: (row: any) => h(NButton, { text: true, type: 'primary', size: 'small', onClick: () => openToolCall(row) }, { default: () => t('common.confirm') }),
  },
])

const resourceColumns = computed(() => [
  { title: t('mcpServer.colUri'), key: 'uri', width: 250 },
  {
    title: t('ruleList.name'), key: 'name', width: 200,
    render: (row: any) => {
      const i18nKey = mcpResourceNameMap[row.name] || mcpResourceNameMap[row.uri?.replace('edgelite://', '')]
      return i18nKey ? t(i18nKey) : (row.name || '-')
    },
  },
  {
    title: t('auditLog.detail'), key: 'description', ellipsis: { tooltip: true },
    render: (row: any) => {
      const i18nKey = mcpResourceDescMap[row.name] || mcpResourceDescMap[row.uri?.replace('edgelite://', '')]
      return i18nKey ? t(i18nKey) : (row.description || '-')
    },
  },
])

const promptColumns = computed(() => [
  {
    title: t('ruleList.name'), key: 'name', width: 200,
    render: (row: any) => {
      const i18nKey = mcpTemplateNameMap[row.name]
      return i18nKey ? t(i18nKey) : (row.name || '-')
    },
  },
  {
    title: t('auditLog.detail'), key: 'description', ellipsis: { tooltip: true },
    render: (row: any) => {
      const i18nKey = mcpTemplateDescMap[row.name]
      return i18nKey ? t(i18nKey) : (row.description || '-')
    },
  },
])

const keyColumns = computed(() => [
  { title: t('mcpServer.keyName'), key: 'name', width: 150 },
  // FIXED-Severe: API Key 明文显示存在泄露风险，改为掩码显示（前4+****+后4）
  {
    title: t('mcpServer.colKey'), key: 'key', width: 200, ellipsis: { tooltip: true },
    render: (row: any) => {
      const k = String(row.key || '')
      if (!k) return '-'
      if (k.length <= 8) return '****'
      return `${k.slice(0, 4)}****${k.slice(-4)}`
    },
  },
  {
    title: t('mcpServer.permission'), key: 'scopes', width: 150,
    render: (row: any) => h(NTag, { size: 'small', type: 'info' }, { default: () => (row.scopes || []).join(', ') }),
  },
  { title: t('auditLog.time'), key: 'created_at', width: 180 },
])

async function fetchStatus() {
  loading.value = true
  try {
    const data = await serviceApi.status('mcp_server')
    enabled.value = data?.state === 'running'
    state.value = data.state
    dependencies.value = data.dependencies || []

    if (enabled.value) {
      await Promise.all([fetchTools(), fetchResources(), fetchPrompts(), fetchApiKeys()])
    }
  } catch (e: any) {
    if (e?.response?.status !== 404) message.error(extractError(e, t('http.requestFailed')))
  } finally {
    loading.value = false
  }
}

async function fetchTools() {
  loadingTools.value = true
  try {
    const data = await mcpApi.tools()
    tools.value = data?.tools || []
  } catch (e: any) {
    message.error(extractError(e, t('http.requestFailed')))
  } finally {
    loadingTools.value = false
  }
}

async function fetchResources() {
  loadingResources.value = true
  try {
    const data = await mcpApi.resources()
    resources.value = data?.resources || []
  } catch (e: any) {
    message.error(extractError(e, t('http.requestFailed')))
  } finally {
    loadingResources.value = false
  }
}

async function fetchPrompts() {
  loadingPrompts.value = true
  try {
    const data = await mcpApi.prompts()
    prompts.value = data?.prompts || []
  } catch (e: any) {
    message.error(extractError(e, t('http.requestFailed')))
  } finally {
    loadingPrompts.value = false
  }
}

async function fetchApiKeys(): Promise<number> {
  loadingKeys.value = true
  try {
    const data = await mcpApi.authKeys()
    apiKeys.value = data?.keys || []
    authEnabled.value = data?.enabled ?? false
  } catch (e: any) {
    message.error(extractError(e, t('http.requestFailed')))
  } finally {
    loadingKeys.value = false
  }
  return apiKeys.value.length
}

async function handleToggle(val: boolean) {
  if (!val) {
    dialog.warning({
      title: t('serviceOverview.disableTitle'),
      content: t('serviceOverview.disableContent', { name: t('mcpServer.title') }),
      positiveText: t('serviceOverview.confirmDisable'),
      negativeText: t('common.cancel'),
      onPositiveClick: () => doToggleMcp(false),
    })
    return
  }
  await doToggleMcp(true)
}

async function doToggleMcp(val: boolean) {
  toggleLoading.value = true
  try {
    if (val) {
      await serviceApi.enable('mcp_server')
      message.success(t('serviceOverview.enableSuccess'))
    } else {
      await serviceApi.disable('mcp_server')
      message.success(t('serviceOverview.disableSuccess'))
    }
    await fetchStatus()
  } catch (e: any) {
    message.error(extractError(e, t('serviceOverview.operationFailed')))
  } finally {
    toggleLoading.value = false
  }
}

async function handleInstallDeps() {
  if (!auth.isAdmin) { message.warning(t('common.permissionDenied')); return }
  installing.value = true
  showInstallProgress.value = true
  installProgress.value = t('mcpServer.installDesc')
  installResult.value = ''
  try {
    await serviceApi.installDeps('mcp_server')
    installResult.value = t('serviceOverview.installSuccess')
    message.success(t('serviceOverview.installSuccess'))
    await fetchStatus()
  } catch (e: any) {
    installResult.value = `${t('serviceOverview.installFailed')}: ${extractError(e, '')}`
    message.error(t('serviceOverview.installFailed'))
  } finally {
    installing.value = false
    installProgress.value = ''
  }
}

function openToolCall(tool: any) {
  toolCallName.value = tool.name
  toolCallArgs.value = '{}'
  toolCallResult.value = ''
  showToolCallModal.value = true
}

async function handleToolCall() {
  if (!auth.isOperator) { message.warning(t('common.permissionDenied')); return }
  // FIXED-Severe: 写操作类工具（如 write_device_point）可能修改工业设备状态，需二次确认
  const writeToolPatterns = ['write', 'set', 'update', 'delete', 'remove', 'create', 'reset', 'send', 'control']
  const toolNameLower = toolCallName.value.toLowerCase()
  const isWriteTool = writeToolPatterns.some(p => toolNameLower.includes(p))
  const doCall = async () => {
    callingTool.value = true
    toolCallResult.value = ''
    try {
      const args = JSON.parse(toolCallArgs.value)
      const result = await mcpApi.callTool(toolCallName.value, args)
      toolCallResult.value = JSON.stringify(result, null, 2)
      message.success(t('common.success'))
    } catch (e: any) {
      toolCallResult.value = extractError(e, t('common.failed'))
      message.error(toolCallResult.value)
    } finally {
      callingTool.value = false
    }
  }
  if (isWriteTool) {
    dialog.warning({
      title: t('common.confirm'),
      content: `${t('mcpServer.callConfirmContent') || t('common.confirm')} (${toolCallName.value})`,
      positiveText: t('common.confirm'),
      negativeText: t('common.cancel'),
      onPositiveClick: doCall,
    })
  } else {
    await doCall()
  }
}

onMounted(fetchStatus)
</script>
