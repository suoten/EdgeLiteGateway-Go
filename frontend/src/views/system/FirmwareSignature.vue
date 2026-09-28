<template>
  <div class="page-container">
    <n-card :title="t('router.firmwareSignature')" size="small">
      <n-space vertical>
        <n-space>
          <n-button type="primary" @click="showSign = true">{{ t('firmware.signNew') }}</n-button>
          <n-button @click="refresh">{{ t('common.refresh') }}</n-button>
        </n-space>

        <n-data-table
          :columns="firmwareColumns"
          :data="firmwareList"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 15 }"
        />
      </n-space>
    </n-card>

    <n-modal v-model:show="showSign" preset="card" style="width: 500px;" :title="t('firmware.signNew')">
      <n-form label-placement="top">
        <n-form-item :label="t('firmware.file')">
          <n-upload v-model:file-list="fileList" :max="1" accept=".bin,.hex,.zip" :custom-request="customUpload">
            <n-button>{{ t('firmware.selectFile') }}</n-button>
          </n-upload>
        </n-form-item>
        <n-form-item :label="t('firmware.version')">
          <n-input v-model:value="signForm.version" />
        </n-form-item>
        <n-form-item :label="t('firmware.signingKey')">
          <n-input v-model:value="signForm.signing_key" type="password" show-password-on="click" />
        </n-form-item>
        <n-form-item :label="t('firmware.algorithm')">
          <n-select v-model:value="signForm.algorithm" :options="algorithmOptions" />
        </n-form-item>
      </n-form>
      <template #footer>
        <n-space justify="end">
          <n-button @click="showSign = false">{{ t('common.cancel') }}</n-button>
          <n-button type="primary" @click="signFirmware" :loading="signing">{{ t('firmware.sign') }}</n-button>
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
import { message } from '@/utils/discreteApi'
import { NButton, NTag } from 'naive-ui'
import { validateFileUpload, sanitizeFilename } from '@/utils/uploadSecurity'
import { formatNum } from '@/utils/format'
import { formatDateTime } from '@/utils/datetime'

const firmwareList = ref<any[]>([])
const showSign = ref(false)
const signing = ref(false)
const fileList = ref<any[]>([])
const signForm = ref({ version: '', signing_key: '', algorithm: 'ed25519', file_path: '', file_content: '' })

const algorithmOptions = [
  { label: 'Ed25519', value: 'ed25519' },
  { label: 'RSA-2048', value: 'rsa2048' },
  { label: 'ECDSA-P256', value: 'ecdsa256' },
  { label: 'HMAC-SHA256', value: 'hmac256' },
]

const firmwareColumns = computed(() => [
  { title: t('firmware.version'), key: 'version' },
  { title: t('firmware.file'), key: 'filename' },
  { title: t('firmware.size'), key: 'size', render: (r: any) => (r.size < 1024 ? `${r.size}B` : `${formatNum(r.size / 1024, 2)}KB`) },
  { title: t('firmware.algorithm'), key: 'algorithm' },
  { title: t('firmware.signature'), key: 'signature', ellipsis: { tooltip: true } },
  { title: t('firmware.signedAt'), key: 'signed_at', render: (r: any) => (r.signed_at ? formatDateTime(r.signed_at) : '-') },
  { title: t('firmware.verified'), key: 'verified', render: (r: any) => h(NTag, { type: r.verified ? 'success' : 'error', size: 'small' }, () => r.verified ? t('firmware.yes') : t('firmware.no')) },
  {
    title: t('common.actions'),
    key: 'actions',
    render: (row: any) => h(NButton, { size: 'small', type: 'primary', text: true, onClick: () => verifyFirmware(row) }, () => t('firmware.verify')),
  },
])

function customUpload({ file }: { file: any }) {
  const fileObj = file.file || file
  const result = validateFileUpload(fileObj, ['.bin', '.hex', '.zip'])
  if (!result.valid) {
    message.error(result.error || t('firmware.selectFile'))
    return
  }
  signForm.value.file_path = sanitizeFilename(fileObj.name)
  const reader = new FileReader()
  reader.onload = () => {
    const dataUrl = String(reader.result || '')
    signForm.value.file_content = dataUrl.includes(',') ? dataUrl.slice(dataUrl.indexOf(',') + 1) : dataUrl
  }
  reader.onerror = () => {
    message.error(t('common.loadFailed'))
    signForm.value.file_content = ''
  }
  reader.readAsDataURL(fileObj)
}

async function refresh() {
  try {
    const res = await http.get('/firmware')
    firmwareList.value = res.data?.data?.items || []
  } catch (e: any) {
    message.error(extractError(e, t('common.loadFailed')))
  }
}

async function signFirmware() {
  if (!signForm.value.file_path || !signForm.value.file_content || !signForm.value.version || !signForm.value.signing_key) {
    message.warning(t('firmware.fillRequired'))
    return
  }
  signing.value = true
  try {
    await http.post('/firmware/sign', signForm.value)
    message.success(t('firmware.signSuccess'))
    showSign.value = false
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('firmware.signFailed')))
  } finally {
    signing.value = false
  }
}

async function verifyFirmware(row: any) {
  try {
    const res = await http.post(`/firmware/${row.id}/verify`)
    if (res.data?.data?.verified) {
      message.success(t('firmware.verifySuccess'))
    } else {
      message.error(t('firmware.verifyFailed'))
    }
    await refresh()
  } catch (e: any) {
    message.error(extractError(e, t('firmware.verifyFailed')))
  }
}

onMounted(() => { refresh() })
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
