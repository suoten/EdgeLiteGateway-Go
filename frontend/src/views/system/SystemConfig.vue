<template>
  <div class="page-container">
    <n-card :title="t('router.systemConfig')" size="small">
      <n-tabs type="line" animated>
        <n-tab-pane name="general" :tab="t('systemConfig.general')">
          <n-form label-placement="left" :label-width="180">
            <n-form-item :label="t('systemConfig.serverHost')">
              <n-input v-model:value="config.server.host" disabled />
            </n-form-item>
            <n-form-item :label="t('systemConfig.serverPort')">
              <n-input-number v-model:value="config.server.port" :min="1" :max="65535" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.debugApi')">
              <n-switch v-model:value="config.server.debug_api_enabled" />
            </n-form-item>
          </n-form>
        </n-tab-pane>

        <n-tab-pane name="database" :tab="t('systemConfig.database')">
          <n-form label-placement="left" :label-width="180">
            <n-form-item :label="t('systemConfig.dbBackend')">
              <n-select v-model:value="config.database.backend" :options="dbBackendOptions" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.sqlitePath')" v-if="config.database.backend === 'sqlite'">
              <n-input v-model:value="config.database.sqlite_path" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.dbHost')" v-if="config.database.backend !== 'sqlite'">
              <n-input v-model:value="config.database.host" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.dbPort')" v-if="config.database.backend !== 'sqlite'">
              <n-input-number v-model:value="config.database.port" :min="1" :max="65535" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.dbPoolSize')">
              <n-input-number v-model:value="config.database.pool_size" :min="1" :max="100" />
            </n-form-item>
          </n-form>
        </n-tab-pane>

        <n-tab-pane name="mqtt" :tab="t('systemConfig.mqtt')">
          <n-form label-placement="left" :label-width="180">
            <n-form-item :label="t('systemConfig.mqttBroker')">
              <n-input v-model:value="config.mqtt.broker" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.mqttPort')">
              <n-input-number v-model:value="config.mqtt.port" :min="1" :max="65535" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.mqttUsername')">
              <n-input v-model:value="config.mqtt.username" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.mqttPassword')">
              <n-input v-model:value="config.mqtt.password" type="password" show-password-on="click" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.mqttTopicPrefix')">
              <n-input v-model:value="config.mqtt.topic_prefix" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.mqttOfflineCache')">
              <n-switch v-model:value="config.mqtt.offline_cache_enabled" />
            </n-form-item>
          </n-form>
        </n-tab-pane>

        <n-tab-pane name="security" :tab="t('systemConfig.security')">
          <n-form label-placement="left" :label-width="180">
            <n-form-item :label="t('systemConfig.rateLimit')">
              <n-input-number v-model:value="config.security.rate_limit_requests_per_minute" :min="1" :max="10000" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.loginLockoutThreshold')">
              <n-input-number v-model:value="config.security.login_lockout_threshold" :min="1" :max="100" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.loginLockoutMinutes')">
              <n-input-number v-model:value="config.security.login_lockout_minutes" :min="1" :max="1440" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.cookieSecure')">
              <n-switch v-model:value="config.security.cookie_secure" />
            </n-form-item>
          </n-form>
        </n-tab-pane>

        <n-tab-pane name="ai" :tab="t('systemConfig.aiInference')">
          <n-form label-placement="left" :label-width="180">
            <n-form-item :label="t('systemConfig.aiEnabled')">
              <n-switch v-model:value="config.ai_inference.enabled" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.aiSidecarUrl')">
              <n-input v-model:value="config.ai_inference.sidecar_url" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.aiMaxConcurrent')">
              <n-input-number v-model:value="config.ai_inference.max_concurrent_inferences" :min="1" :max="32" />
            </n-form-item>
            <n-form-item :label="t('systemConfig.aiTimeout')">
              <n-input-number v-model:value="config.ai_inference.inference_timeout" :min="1" :max="300" />
            </n-form-item>
          </n-form>
        </n-tab-pane>
      </n-tabs>

      <div style="margin-top: 16px; text-align: right;">
        <n-button type="primary" :loading="saving" @click="saveConfig">
          {{ t('common.save') }}
        </n-button>
        <n-button style="margin-left: 8px;" @click="loadConfig">
          {{ t('common.refresh') }}
        </n-button>
      </div>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, onMounted } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message } from '@/utils/discreteApi'

// Sections must exist before the first render — the template binds config.server.* directly.
const config = ref<any>({ server: {}, database: {}, mqtt: {}, security: {}, ai_inference: {} })
const saving = ref(false)

const SECTIONS = ['server', 'database', 'mqtt', 'security', 'ai_inference'] as const

const dbBackendOptions = [
  { label: 'SQLite', value: 'sqlite' },
  { label: 'MySQL', value: 'mysql' },
  { label: 'PostgreSQL', value: 'postgresql' },
]

// Masked secrets ("a***z") are blanked before display; the server restores the
// real values on save when a sensitive field comes back empty.
function blankMasked(obj: any) {
  for (const k of Object.keys(obj)) {
    if (typeof obj[k] === 'string' && obj[k].includes('***')) {
      obj[k] = ''
    }
  }
}

async function loadConfig() {
  try {
    const res = await http.get('/system/config')
    const cfg: any = { ...(res.data?.data || {}) }
    for (const s of SECTIONS) {
      if (!cfg[s] || typeof cfg[s] !== 'object') cfg[s] = {}
      blankMasked(cfg[s])
    }
    config.value = cfg
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function saveConfig() {
  saving.value = true
  try {
    await http.put('/system/config', config.value)
    message.success(t('common.saveSuccess'))
  } catch (e: any) {
    message.error(extractError(e, t('common.saveFailed')))
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  loadConfig()
})
</script>

<style scoped>
.page-container {
  padding: 16px;
}
</style>
