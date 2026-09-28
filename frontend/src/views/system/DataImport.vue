<template>
  <div class="page-container">
    <n-card :title="t('router.dataImport')" size="small">
      <n-upload
        :max="1"
        accept=".csv,.json"
        :default-upload="false"
        v-model:file-list="fileList"
        :on-before-upload="onBeforeUpload"
      >
        <n-upload-dragger>
          <div style="margin-bottom: 12px;">
            <n-icon size="48" :depth="3">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M17 8l-5-5-5 5M12 3v12"/></svg>
            </n-icon>
          </div>
          <n-text style="font-size: 16px;">{{ t('dataImport.dragHere') }}</n-text>
          <n-p depth="3" style="margin: 8px 0 0 0;">
            {{ t('dataImport.fileTypes') }}
          </n-p>
        </n-upload-dragger>
      </n-upload>

      <!-- 选中文件不直接上传：导入会立即写入运行数据，需要一次显式确认 -->
      <n-space align="center" style="margin-top: 12px;">
        <n-button
          type="primary"
          :disabled="!fileList.length"
          :loading="importing"
          @click="startImport"
        >{{ t('dataImport.startImport') }}</n-button>
        <n-text v-if="!fileList.length" depth="3">{{ t('dataImport.pickFileFirst') }}</n-text>
      </n-space>

      <n-divider />

      <div v-if="importResult">
        <n-descriptions :title="t('dataImport.result')" bordered :column="2">
          <n-descriptions-item :label="t('dataImport.totalRows')">
            {{ importResult.total }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('dataImport.successRows')">
            <n-tag type="success">{{ importResult.success }}</n-tag>
          </n-descriptions-item>
          <n-descriptions-item :label="t('dataImport.failedRows')">
            <n-tag type="error" v-if="importResult.failed > 0">{{ importResult.failed }}</n-tag>
            <span v-else>0</span>
          </n-descriptions-item>
          <n-descriptions-item :label="t('dataImport.deviceId')">
            {{ importResult.device_id }}
          </n-descriptions-item>
        </n-descriptions>

        <div v-if="importResult.errors && importResult.errors.length > 0" style="margin-top: 12px;">
          <n-collapse>
            <n-collapse-item :title="t('dataImport.errorDetails')" name="errors">
              <n-data-table
                :columns="errorColumns"
                :data="importResult.errors"
                :max-height="300"
                size="small"
              />
            </n-collapse-item>
          </n-collapse>
        </div>
      </div>

      <n-divider />

      <n-space>
        <n-button @click="downloadTemplate('csv')">{{ t('dataImport.downloadCsvTemplate') }}</n-button>
        <n-button @click="downloadTemplate('json')">{{ t('dataImport.downloadJsonTemplate') }}</n-button>
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed } from 'vue'
import type { UploadFileInfo } from 'naive-ui'
import http from '@/api/http'
import { t } from '@/i18n'
import { message } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { validateFileUpload, MAX_FILE_SIZE } from '@/utils/uploadSecurity'

const importResult = ref<any>(null)
const fileList = ref<UploadFileInfo[]>([])
const importing = ref(false)

const errorColumns = computed(() => [
  { title: t('dataImport.rowNumber'), key: 'row', width: 100 },
  { title: t('dataImport.error'), key: 'message' },
])

function onBeforeUpload({ file }: { file: UploadFileInfo }) {
  if (!file.file) return true
  const result = validateFileUpload(file.file, ['.csv', '.json'], MAX_FILE_SIZE)
  if (!result.valid) {
    message.error(result.error || t('dataImport.invalidFileType'))
    return false
  }
  return true
}

async function startImport() {
  const raw = fileList.value[0]?.file
  if (!raw || importing.value) return
  importing.value = true
  try {
    const fd = new FormData()
    fd.append('file', raw)
    const res = await http.post('/data/import', fd, {
      headers: { 'Content-Type': 'multipart/form-data' },
      timeout: 300000,
    })
    importResult.value = res.data?.data ?? null
    message.success(t('dataImport.importSuccess'))
    fileList.value = []
  } catch (e: any) {
    message.error(extractError(e, t('dataImport.importFailed')))
  } finally {
    importing.value = false
  }
}async function downloadTemplate(format: string) {
  try {
    const res = await http.get('/data/import/template', {
      params: { format },
      responseType: 'blob',
    })
    const url = URL.createObjectURL(res.data)
    const a = document.createElement('a')
    a.href = url
    a.download = `import_template.${format}`
    a.click()
    URL.revokeObjectURL(url)
  } catch {
    message.error(t('dataImport.templateDownloadFailed'))
  }
}
</script>

<style scoped>
.page-container { padding: 16px; }
</style>
