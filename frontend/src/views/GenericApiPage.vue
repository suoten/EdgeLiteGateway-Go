<template>
  <div class="generic-api-page">
    <n-card :title="pageTitle" size="small">
      <n-space vertical>
        <!-- The router declares which endpoint a page reads. Without it no request is
             issued: guessing from the URL produced a permanent 404 per page visit. -->
        <n-alert v-if="!apiPath" type="warning" :title="t('genericApi.noEndpointTitle')">
          {{ t('genericApi.noEndpointBody', { path: route.path }) }}
        </n-alert>

        <template v-else>
          <n-space align="center">
            <n-button @click="fetchData" :loading="loading">{{ t('common.refresh') }}</n-button>
            <n-tag :bordered="false" type="default" size="small">GET {{ apiPath }}</n-tag>
            <n-tag v-if="lastError" type="error">{{ lastError }}</n-tag>
          </n-space>

          <n-alert v-if="notImplemented" type="info" :title="t('genericApi.notSupportedTitle')">
            {{ notImplementedMessage || t('genericApi.notSupportedBody') }}
          </n-alert>

          <n-divider v-if="data" />

          <!-- Render data as JSON viewer if no specific layout -->
          <div v-if="data && !lastError" class="data-view">
            <n-collapse v-if="isObject(data)">
              <n-collapse-item
                v-for="(value, key) in data"
                :key="key"
                :title="String(key)"
                :name="String(key)"
              >
                <pre v-if="typeof value === 'object'" class="json-display">{{ JSON.stringify(value, null, 2) }}</pre>
                <n-text v-else>{{ value }}</n-text>
              </n-collapse-item>
            </n-collapse>
            <pre v-else class="json-display">{{ JSON.stringify(data, null, 2) }}</pre>
          </div>

          <n-empty v-if="!hasData && !loading && !lastError && !notImplemented" :description="t('common.noData')" />
        </template>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useRoute } from 'vue-router'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'

const route = useRoute()
const data = ref<any>(null)
const loading = ref(false)
const lastError = ref('')

const pageTitle = computed(() => (route.meta?.title as string) || t('common.comingSoon'))
const apiPath = computed<string | null>(() => (route.meta?.apiPath as string | undefined) ?? null)

// Endpoints answer with a banner field once the request itself succeeded but the
// feature is not backed by an implementation in this build.
const notImplemented = computed<boolean>(() => {
  const d = data.value
  return !!d && typeof d === 'object' && !Array.isArray(d) && (d.supported === false || d.implemented === false)
})
const notImplementedMessage = computed<string>(() => {
  const d = data.value as any
  return typeof d?.message === 'string' ? d.message : ''
})

function isObject(val: any): boolean {
  return val !== null && typeof val === 'object' && !Array.isArray(val)
}

const hasData = computed(() => {
  if (data.value === null || data.value === undefined) return false
  if (Array.isArray(data.value)) return data.value.length > 0
  return true
})

async function fetchData() {
  const endpoint = apiPath.value
  if (!endpoint) return
  loading.value = true
  lastError.value = ''
  data.value = null
  try {
    const res = await http.get(endpoint)
    data.value = res.data?.data ?? res.data
  } catch (e: any) {
    lastError.value = extractError(e) || e.message
  } finally {
    loading.value = false
  }
}

watch(() => route.path, () => {
  fetchData()
}, { immediate: true })
</script>

<style scoped>
.generic-api-page { padding: 16px; }
.data-view { margin-top: 12px; }
.json-display {
  background: #f5f5f5; padding: 12px; border-radius: 4px;
  font-size: 13px; overflow: auto; max-height: 500px;
}
</style>
