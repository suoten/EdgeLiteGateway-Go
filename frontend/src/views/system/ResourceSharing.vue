<template>
  <div class="page-container">
    <n-card :title="t('router.resourceSharing')" size="small">
      <n-space vertical>
        <n-space>
          <n-button type="primary" @click="showCreate = true">{{ t('resourceShare.createShare') }}</n-button>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <n-data-table
          :columns="shareColumns"
          :data="shares"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 15 }"
        />
      </n-space>
    </n-card>

    <n-modal v-model:show="showCreate" preset="card" style="width: 500px;" :title="t('resourceShare.createShare')">
      <n-form label-placement="top">
        <n-form-item :label="t('resourceShare.resourceType')">
          <n-select v-model:value="newShare.resource_type" :options="resourceTypeOptions" />
        </n-form-item>
        <n-form-item :label="t('resourceShare.resourceId')">
          <n-input v-model:value="newShare.resource_id" />
        </n-form-item>
        <n-form-item :label="t('resourceShare.targetGateway')">
          <n-input v-model:value="newShare.target_gateway_id" :placeholder="t('resourceShare.targetGatewayPlaceholder')" />
        </n-form-item>
        <n-form-item :label="t('resourceShare.expiresIn')">
          <n-input-number v-model:value="newShare.expires_hours" :min="1" :max="720" />
          <span style="margin-left: 8px; color: #999;">h</span>
        </n-form-item>
        <n-form-item :label="t('resourceShare.permissions')">
          <n-select v-model:value="newShare.permissions" multiple :options="permissionOptions" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showCreate = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" @click="createShare" :loading="creating">{{ t('common.save') }}</n-button>
        </n-space>
      </template>
    </n-modal>
  </div>
</template>

<script setup lang="ts">
import { ref, h, onMounted, computed } from 'vue'
import http from '@/api/http'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { message, dialog } from '@/utils/discreteApi'
import { NButton, NTag } from 'naive-ui'
import { formatDateTime } from '@/utils/datetime'

const shares = ref<any[]>([])
const showCreate = ref(false)
const creating = ref(false)
const newShare = ref({ resource_type: 'device', resource_id: '', target_gateway_id: '', expires_hours: 24, permissions: ['read'] })

const resourceTypeOptions = computed(() => [
  { label: t('resourceShare.device'), value: 'device' },
  { label: t('resourceShare.rule'), value: 'rule' },
  { label: t('resourceShare.alarm'), value: 'alarm' },
  { label: t('resourceShare.data'), value: 'data' },
])
const permissionOptions = computed(() => [
  { label: t('resourceShare.permRead'), value: 'read' },
  { label: t('resourceShare.permWrite'), value: 'write' },
  { label: t('resourceShare.permExecute'), value: 'execute' },
  { label: t('resourceShare.permAdmin'), value: 'admin' },
])

const statusLabel = (s: string) => {
  switch (s) {
    case 'active': return t('resourceShare.statusActive')
    case 'expired': return t('resourceShare.statusExpired')
    case 'revoked': return t('resourceShare.statusRevoked')
    default: return s
  }
}

const resourceTypeLabel = (v: string) => {
  const opt = resourceTypeOptions.value.find(o => o.value === v)
  return opt ? opt.label : v
}

const shareColumns = computed(() => [
  { title: t('resourceShare.resourceType'), key: 'resource_type', render: (r: any) => resourceTypeLabel(r.resource_type) },
  { title: t('resourceShare.resourceId'), key: 'resource_id' },
  { title: t('resourceShare.targetGateway'), key: 'target_gateway_id' },
  { title: t('resourceShare.permissions'), key: 'permissions', render: (r: any) => (r.permissions || []).map((p: string) => { const opt = permissionOptions.value.find(o => o.value === p); return h(NTag, { size: 'small', style: 'margin-right: 4px;' }, () => opt ? opt.label : p) }) },
  { title: t('resourceShare.colExpires'), key: 'expires_at', render: (r: any) => (r.expires_at ? formatDateTime(r.expires_at) : '-') },
  { title: t('resourceShare.colStatus'), key: 'status', render: (r: any) => h(NTag, { type: r.status === 'active' ? 'success' : 'default', size: 'small' }, () => statusLabel(r.status)) },
  {
    title: t('common.actions'),
    key: 'actions',
    render: (row: any) => h(NButton, { size: 'small', type: 'error', text: true, onClick: () => revokeShare(row) }, () => t('resourceShare.revoke')),
  },
])

async function refresh() {
  try {
    const res = await http.get('/resource-shares')
    shares.value = res.data?.data ?? []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function createShare() {
  creating.value = true
  try {
    await http.post('/resource-shares', newShare.value)
    message.success(t('common.createSuccess'))
    showCreate.value = false
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('common.createFailed')))
  } finally {
    creating.value = false
  }
}

function revokeShare(row: any) {
  dialog.warning({
    title: t('common.confirmOperation'),
    content: t('common.confirmDeleteDesc'),
    positiveText: t('resourceShare.revoke'),
    negativeText: t('common.cancel'),
    onPositiveClick: () => { void doRevokeShare(row) },
  })
}

async function doRevokeShare(row: any) {
  try {
    await http.delete(`/resource-shares/${row.id}`)
    message.success(t('resourceShare.revoked'))
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('resourceShare.revokeFailed')))
  }
}

onMounted(() => { refresh() })
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
