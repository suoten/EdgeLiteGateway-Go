<template>
  <div class="page-container">
    <n-card :title="t('router.scripts')" size="small">
      <n-space vertical>
        <n-space>
          <n-button type="primary" @click="showEditor = true">{{ t('scripts.newScript') }}</n-button>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <n-data-table
          :columns="scriptColumns"
          :data="scripts"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 15 }"
        />
      </n-space>
    </n-card>

    <!-- Script Editor Modal -->
    <n-modal v-model:show="showEditor" preset="card" style="width: 800px;" :title="editingScript?.id ? t('scripts.editScript') : t('scripts.newScript')">
      <n-form label-placement="top">
        <n-form-item :label="t('scripts.name')">
          <n-input v-model:value="editingScript.name" :placeholder="t('scripts.namePlaceholder')" />
        </n-form-item>
        <n-form-item :label="t('scripts.language')">
          <n-select v-model:value="editingScript.language" :options="languageOptions" />
        </n-form-item>
        <n-form-item :label="t('common.status')">
          <n-switch v-model:value="editingScript.enabled" />
          <span style="margin-left: 8px; color: #999;">{{ editingScript.enabled ? t('common.enabled') : t('common.disabled') }}</span>
        </n-form-item>
        <n-form-item :label="t('scripts.code')">
          <n-input
            v-model:value="editingScript.code"
            type="textarea"
            :rows="15"
            :placeholder="t('scripts.codePlaceholder')"
            style="font-family: monospace;"
          />
        </n-form-item>
        <n-form-item :label="t('scripts.timeout')">
          <n-input-number v-model:value="editingScript.timeout_ms" :min="100" :max="60000" />
          <span style="margin-left: 8px; color: #999;">ms</span>
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="testScript" :loading="testing">{{ t('scripts.test') }}</n-button>
          <n-button @click="showEditor = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" @click="saveScript" :loading="saving">{{ t('common.save') }}</n-button>
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
import { formatDateTime } from '@/utils/datetime'
import { NButton, NTag } from 'naive-ui'

const scripts = ref<any[]>([])
const showEditor = ref(false)
const editingScript = ref<any>({ name: '', language: 'javascript', code: '', timeout_ms: 5000, enabled: true })
const saving = ref(false)
const testing = ref(false)
const togglingIds = ref<Set<string>>(new Set())

// Only javascript runs in this gateway: runScriptCode rejects every other
// language, so the alternatives stay visible but cannot be picked.
const languageOptions = computed(() => [
  { label: 'JavaScript', value: 'javascript' },
  { label: `Python - ${t('scripts.languageUnsupported')}`, value: 'python', disabled: true },
  { label: `Lua - ${t('scripts.languageUnsupported')}`, value: 'lua', disabled: true },
])

const scriptColumns = computed(() => [
  { title: t('scripts.name'), key: 'name' },
  { title: t('scripts.language'), key: 'language', render: (r: any) => h(NTag, { size: 'small' }, () => r.language) },
  {
    title: t('common.status'),
    key: 'enabled',
    render: (r: any) => h(NTag, { size: 'small', type: r.enabled ? 'success' : 'default' },
      () => (r.enabled ? t('common.enabled') : t('common.disabled'))),
  },
  { title: t('scripts.timeout'), key: 'timeout_ms', render: (r: any) => `${r.timeout_ms}ms` },
  { title: t('scripts.updatedAt'), key: 'updated_at', render: (r: any) => (r.updated_at ? formatDateTime(r.updated_at) : '-') },
  {
    title: t('common.actions'),
    key: 'actions',
    render: (row: any) => h('div', [h(NButton, { size: 'small', type: 'primary', text: true, onClick: () => editScript(row) }, () => t('common.edit')),
      h(NButton, {
        size: 'small',
        text: true,
        style: 'margin-left: 8px;',
        loading: togglingIds.value.has(row.id),
        onClick: () => toggleScript(row),
      }, () => (row.enabled ? t('common.disable') : t('common.enable'))),
      h(NButton, { size: 'small', type: 'error', text: true, style: 'margin-left: 8px;', onClick: () => deleteScript(row) }, () => t('common.delete'))]),
  },
])

async function refresh() {
  try {
    const res = await http.get('/scripts')
    scripts.value = res.data?.data || []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

function editScript(row: any) {
  editingScript.value = { ...row }
  showEditor.value = true
}

async function saveScript() {
  saving.value = true
  try {
    if (editingScript.value.id) {
      await http.put(`/scripts/${editingScript.value.id}`, editingScript.value)
    } else {
      await http.post('/scripts', editingScript.value)
    }
    message.success(t('common.saveSuccess'))
    showEditor.value = false
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.saveFailed')))
  } finally {
    saving.value = false
  }
}

function deleteScript(row: any) {
  dialog.warning({
    title: t('common.confirmDeleteName', { name: row.name ?? row.id }),
    content: t('common.confirmDeleteDesc'),
    positiveText: t('common.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => { void doDeleteScript(row) },
  })
}

async function doDeleteScript(row: any) {
  try {
    await http.delete(`/scripts/${row.id}`)
    message.success(t('common.deleteSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.deleteFailed')))
  }
}

async function toggleScript(row: any) {
  const action = row.enabled ? 'disable' : 'enable'
  togglingIds.value = new Set(togglingIds.value).add(row.id)
  try {
    await http.post(`/scripts/${row.id}/${action}`)
    message.success(t('common.saveSuccess'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.operationFailed')))
  } finally {
    const next = new Set(togglingIds.value)
    next.delete(row.id)
    togglingIds.value = next
  }
}

async function testScript() {
  testing.value = true
  try {
    const res = await http.post('/scripts/test', editingScript.value)
    const out = res.data?.data || {}
    if (out.error) {
      message.error(t('scripts.testFailed') + ': ' + out.error)
    } else {
      message.success(t('scripts.testResult') + ': ' + (out.output || t('scripts.noOutput')))
    }
  } catch (e: any) {
    message.error(extractError(e, t('scripts.testFailed')))
  } finally {
    testing.value = false
  }
}

onMounted(() => {
  refresh()
})
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
