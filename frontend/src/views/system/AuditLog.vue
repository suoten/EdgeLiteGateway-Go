<template>
  <n-spin :show="pageLoading" :description="t('auditLog.loading')">
  <div class="audit-page">
    <n-card :title="t('auditLog.title')">
      <template #header-extra>
        <n-space vertical :size="8" style="width: 100%">
          <n-space>
            <n-select v-model:value="filterAction" :options="filteredActionOptions" clearable :placeholder="t('auditLog.operationType')" style="width: 160px" />
            <n-input v-model:value="filterUsername" clearable :placeholder="t('auditLog.username') || 'Username'" style="width: 140px" />
            <n-date-picker v-model:value="timeRange" type="datetimerange" clearable :shortcuts="timeShortcuts" />
            <n-button type="primary" @click="loadLogs">{{ t('auditLog.query') }}</n-button>
            <n-button @click="exportCSV">{{ t('auditLog.exportCsv') }}</n-button>
            <n-button @click="verifyIntegrity">{{ t('auditLog.integrity') }}</n-button>
            <n-button type="warning" :loading="cleanupLoading" @click="showCleanupModal = true">{{ t('auditLog.cleanup') }}</n-button>
          </n-space>
          <!-- 修复13: 操作类型快捷分组——按类别快速筛选，再在下拉框中精确定位 -->
          <n-button-group size="small">
            <n-button
              v-for="cat in actionCategories"
              :key="cat.value"
              :type="filterCategory === cat.value ? 'primary' : 'default'"
              @click="onCategoryChange(cat.value)"
            >{{ cat.label }}</n-button>
          </n-button-group>
        </n-space>
      </template>
      <n-data-table remote :columns="columns" :data="logs" :pagination="pagination" :loading="loading" :scroll-x="900" :row-props="logRowProps">
        <template #empty>
          <LoadEmptyState :description="t('common.noData')" :failed="loadFailed" :retrying="loading" @retry="loadLogs" />
        </template>
      </n-data-table>
    </n-card>

    <n-modal v-model:show="detailVisible" preset="card" :title="t('auditLog.detail')" style="width: 800px" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-descriptions v-if="selectedLog" :column="1" bordered>
        <n-descriptions-item :label="t('auditLog.time')">{{ formatDateTime(selectedLog.created_at) }}</n-descriptions-item>
        <n-descriptions-item :label="t('auditLog.user')">{{ selectedLog.username || selectedLog.user_id }}</n-descriptions-item>
        <n-descriptions-item :label="t('auditLog.operation')">{{ auditActionLabel[selectedLog.action] || selectedLog.action }}</n-descriptions-item>
        <n-descriptions-item :label="t('auditLog.resourceType')">{{ resourceTypeLabel[selectedLog.resource_type] || selectedLog.resource_type }}</n-descriptions-item>
        <n-descriptions-item :label="t('auditLog.resourceId')">{{ selectedLog.resource_id }}</n-descriptions-item>
        <n-descriptions-item :label="t('auditLog.ipAddress')">{{ selectedLog.ip_address }}</n-descriptions-item>
        <n-descriptions-item :label="t('auditLog.statusCol')">{{ auditStatusLabel[selectedLog.status] || selectedLog.status }}</n-descriptions-item>
        <n-descriptions-item :label="t('auditLog.detail')"><pre style="white-space: pre-wrap; word-break: break-all">{{ maskSensitiveDetails(selectedLog.details) }}</pre></n-descriptions-item>
      </n-descriptions>
    </n-modal>
    <n-modal v-model:show="showCleanupModal" preset="dialog" :title="t('auditLog.cleanupTitle')" :positive-text="t('auditLog.confirmCleanup')" :negative-text="t('common.cancel')" @positive-click="doCleanup" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-form-item :label="t('auditLog.retainDays')">
        <n-input-number v-model:value="retentionDays" :min="30" :max="3650" />
      </n-form-item>
      <p style="color: #999; font-size: 13px">{{ t('auditLog.cleanupHint') }}</p>
    </n-modal>
  </div>
  </n-spin>
</template>

<script setup lang="ts">
import { ref, reactive, onMounted, h, computed } from 'vue'
import { NCard, NButton, NSpace, NSelect, NDatePicker, NDataTable, NTag, NModal, NFormItem, NInputNumber, NSpin, NInput, NDescriptions, NDescriptionsItem } from 'naive-ui'
import { auditApi } from '@/api'
import { auditStatusLabel, auditActionLabel, resourceTypeLabel } from '@/utils/enumLabels'
// FIXED: 原问题-添加i18n支持
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import LoadEmptyState from '@/components/LoadEmptyState.vue'
import { message, dialog } from '@/utils/discreteApi'
import { formatDateTime } from '@/utils/datetime'  // [AUDIT-FIX] 严重级-模板使用 formatDateTime 但未导入，导致运行时 TypeError
// [AUDIT-FIX] 致命级-高危操作无权限校验，引入 auth store 进行角色判断
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()

const logs = ref<any[]>([])
const loading = ref(false)
const loadFailed = ref(false)
const pageLoading = ref(true)
const filterAction = ref(null)
const filterUsername = ref('')
const timeRange = ref<[number, number] | null>(null)
// FIXED: 审计日志页无快捷时间选项，添加近1小时/6小时/24小时/7天/30天快捷选项
// Naive UI n-date-picker shortcuts 为 Record<label, value|getter> 格式
// [AUDIT-FIX] 一般级-timeShortcuts 键名硬编码中文，改用 i18n key
const timeShortcuts = computed<Record<string, [number, number] | (() => [number, number])>>(() => ({
  [t('auditLog.last1Hour')]: () => { const end = Date.now(); return [end - 3600000, end] },
  [t('auditLog.last6Hours')]: () => { const end = Date.now(); return [end - 6 * 3600000, end] },
  [t('auditLog.last24Hours')]: () => { const end = Date.now(); return [end - 24 * 3600000, end] },
  [t('auditLog.last7Days')]: () => { const end = Date.now(); return [end - 7 * 24 * 3600000, end] },
  [t('auditLog.last30Days')]: () => { const end = Date.now(); return [end - 30 * 24 * 3600000, end] },
}))
const pagination = reactive({
  page: 1,
  pageSize: 20,
  itemCount: 0,
  pageSizes: [10, 20, 50, 100],
  showSizePicker: true,
  onChange: (page: number) => { pagination.page = page; loadLogs() },
  onUpdatePageSize: (s: number) => { pagination.pageSize = s; pagination.page = 1; loadLogs() },
})
const showCleanupModal = ref(false)
const retentionDays = ref(90)
// [AUDIT-FIX] 一般级-doCleanup 无独立 loading 状态，添加 cleanupLoading 防止重复提交
const cleanupLoading = ref(false)
// FIX-27: 单条日志详情
const detailVisible = ref(false)
const selectedLog = ref<any>(null)
function showDetail(row: any) {
  selectedLog.value = row
  detailVisible.value = true
}

// 行可点击查看详情；补 tabindex/Enter 让键盘用户也能打开
function logRowProps(row: any) {
  return {
    style: 'cursor: pointer',
    tabindex: '0',
    'aria-label': `${row.action} ${row.username || row.user_id || ''}`,
    onClick: () => showDetail(row),
    onKeydown: (e: KeyboardEvent) => {
      if (e.target !== e.currentTarget) return
      if (e.key !== 'Enter' && e.key !== ' ') return
      e.preventDefault()
      showDetail(row)
    },
  }
}

// [AUDIT-FIX] 一般级-actionOptions 改为 computed，语言切换时选项标签响应式更新
// 选项直接取自 auditActionLabel（表格渲染操作名用的同一张映射表）：之前是手抄一份清单，
// 新增审计动作时表格里能显示中文、下拉框里却筛不到，两处会长期漂移。
const actionOptions = computed(() =>
  Object.keys(auditActionLabel.value).map((k) => ({ label: auditActionLabel.value[k], value: k })),
)

// 修复13: 操作类型快捷分组——按类别快速筛选下拉框选项
const filterCategory = ref<string>('')
const actionCategories = computed(() => [
  { label: t('auditLog.catAll'), value: '' },
  { label: t('auditLog.catAuth'), value: 'auth' },
  { label: t('auditLog.catDevice'), value: 'device' },
  { label: t('auditLog.catRule'), value: 'rule' },
  { label: t('auditLog.catUser'), value: 'user' },
  { label: t('auditLog.catBackup'), value: 'backup' },
  { label: t('auditLog.catOther'), value: 'other' },
])

// 归组按动作前缀推断（device_create -> device）。前缀与语义不一致的在这里显式指定，
// 否则登录类、口令类和会话吊销在前面几个分类下永远筛不到。
const CATEGORY_PREFIXES = ['auth', 'device', 'rule', 'user', 'backup']
const CATEGORY_OVERRIDES: Record<string, string> = {
  login: 'auth',
  logout: 'auth',
  login_failed: 'auth',
  password_change: 'auth',
  forgot_password: 'auth',
  password_reset: 'auth',
  token_revoke: 'auth',
  session_revoke: 'user',
}
function categoryOf(action: string): string {
  const prefix = action.split('_')[0]
  const cat = CATEGORY_OVERRIDES[action] || (CATEGORY_PREFIXES.includes(prefix) ? prefix : 'other')
  return cat
}

const filteredActionOptions = computed(() => {
  if (!filterCategory.value) return actionOptions.value
  return actionOptions.value.filter(opt => categoryOf(opt.value) === filterCategory.value)
})

function onCategoryChange(cat: string) {
  filterCategory.value = cat
  // 切换分类时，若当前选中的 action 不属于该分类则清空
  if (filterAction.value && cat) {
    const stillValid = filteredActionOptions.value.some(opt => opt.value === filterAction.value)
    if (!stillValid) filterAction.value = null
  }
}

// [FIX-13] 严重级-columns 改为 computed，使 i18n 切换时列标题响应更新
const columns = computed(() => [
  { title: t('auditLog.time'), key: 'created_at', width: 180, render: (row: any) => formatDateTime(row.created_at) },
  { title: t('auditLog.user'), key: 'username', width: 100 },
  { title: t('auditLog.operation'), key: 'action', width: 120, render: (row: any) => h(NTag, { size: 'small', type: row.status === 'success' ? 'success' : 'error' }, () => auditActionLabel.value[row.action] || row.action) },
  { title: t('auditLog.resourceType'), key: 'resource_type', width: 100, render: (row: any) => resourceTypeLabel.value[row.resource_type] || row.resource_type },
  { title: t('auditLog.resourceId'), key: 'resource_id', width: 120 },
  { title: t('auditLog.ipAddress'), key: 'ip_address', width: 130 },
  { title: t('auditLog.statusCol'), key: 'status', width: 80, render: (row: any) => h(NTag, { size: 'small', type: row.status === 'success' ? 'success' : 'error' }, () => auditStatusLabel.value[row.status] || row.status) },
  // FIXED-Critical: details 字段可能含密码/Token 等敏感信息，需脱敏后展示
  {
    title: t('auditLog.detail'), key: 'details', ellipsis: { tooltip: true },
    render: (row: any) => maskSensitiveDetails(row.details),
  },
])
// FIXED-Critical: 审计日志 details 字段脱敏，避免泄露密码/Token/API Key 等敏感凭证
// FIX 5: 改进脱敏，优先尝试 JSON 结构化解析，递归脱敏对象字段
function maskSensitiveDetails(details: any): string {
  if (!details) return ''
  const text = typeof details === 'string' ? details : JSON.stringify(details)
  const sensitiveKeys = 'password|passwd|secret|token|api_key|apikey|access_token|refresh_token|private_key|client_secret|smtp_password|auth_password|credential|session_id|cookie|authorization|cvv|card_number'
  const sensitiveRegex = new RegExp(`(${sensitiveKeys})`, 'gi')

  let result = text

  // 尝试 JSON 结构化脱敏
  try {
    const parsed = JSON.parse(text)
    const masked = maskObjectFields(parsed, sensitiveRegex)
    result = JSON.stringify(masked)
  } catch {
    // 非 JSON，使用正则脱敏（仅匹配字段名:值模式，不匹配字符串内容）
    result = result.replace(
      new RegExp(`(^|[\\s,{])(?:"?(${sensitiveKeys})"?\\s*[:=]\\s*)"[^"]*"`, 'gmi'),
      '$1$2"******"'
    )
    result = result.replace(
      new RegExp(`(^|[\\s,{])(${sensitiveKeys})\\s*:\\s*[^,}\\s"']+`, 'gmi'),
      '$1$2: ******'
    )
  }

  return result
}

function maskObjectFields(obj: any, regex: RegExp): any {
  if (obj === null || typeof obj !== 'object') return obj
  if (Array.isArray(obj)) return obj.map(item => maskObjectFields(item, regex))
  const result: any = {}
  for (const key in obj) {
    regex.lastIndex = 0
    if (regex.test(key)) {
      result[key] = '******'
    } else {
      result[key] = maskObjectFields(obj[key], regex)
    }
  }
  return result
}

// [FIX-3] 导出的 CSV 已由后端按 user_id/username 统一过滤，无需客户端二次处理

async function loadLogs() {
  loading.value = true
  try {
    const params: any = { page: pagination.page, size: pagination.pageSize }
    if (filterAction.value) params.action = filterAction.value
    if (filterUsername.value) params.user_id = filterUsername.value
    if (timeRange.value) {
      params.start_time = new Date(timeRange.value[0]).toISOString()
      params.end_time = new Date(timeRange.value[1]).toISOString()
    }
    const data = await auditApi.list(params)
    logs.value = data?.logs ?? []
    pagination.itemCount = data?.total ?? 0
    loadFailed.value = false
  } catch (e: any) {
    loadFailed.value = true
    message.error(extractError(e, t('auditLog.loadFailed')))
  } finally { loading.value = false; pageLoading.value = false }
}

async function exportCSV() {
  // [AUDIT-FIX] 致命级-审计日志导出含敏感数据，需操作员及以上权限，并二次确认
  if (!auth.isOperator) {
    message.warning(t('auditLog.exportDenied'))
    return
  }
  // FIX-16: 导出限量检查
  // [AUDIT-FIX] 严重级-原代码检查 logs.value.length（当前页数据，最大 pageSize=100），
  // 永远不会超过 50000，限量保护形同虚设。应检查 pagination.itemCount（总条数）。
  // 且原代码仅 warning 不阻断，超限时仍继续导出。现改为 return 阻断。
  const MAX_EXPORT = 50000
  if ((pagination.itemCount ?? 0) > MAX_EXPORT) {
    message.warning(t('auditLog.exportLimit', { max: MAX_EXPORT }))
    return
  }
  dialog.warning({
    title: t('auditLog.exportConfirmTitle'),
    content: t('auditLog.exportConfirmContent'),
    positiveText: t('common.confirm'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        const params: any = {}
        if (filterAction.value) params.action = filterAction.value
        // [FIX-3] 致命级-参数名与 loadLogs 不一致（原为 username），统一为 user_id
        if (filterUsername.value) params.user_id = filterUsername.value
        if (timeRange.value) {
          params.start_time = new Date(timeRange.value[0]).toISOString()
          params.end_time = new Date(timeRange.value[1]).toISOString()
        }
        const data = await auditApi.exportCsv(params)
        if (data?.content) {
          // FIXED-Critical: 导出 CSV 也需对敏感字段脱敏
          // 后端 /audit/export/csv 现已支持 action/user_id/时间过滤，无需客户端二次过滤
          const safeContent = maskSensitiveDetails(data.content)
          const blob = new Blob([safeContent], { type: 'text/csv;charset=utf-8' })
          const url = URL.createObjectURL(blob)
          const a = document.createElement('a')
          a.href = url; a.download = 'audit_logs.csv'; a.click()
          // FIX-15: 延迟 revoke，避免 Firefox 等浏览器下载失败
          setTimeout(() => URL.revokeObjectURL(url), 1000)
        }
      } catch (e: any) { message.error(extractError(e, t('auditLog.exportFailed'))) }
    },
  })
}

async function verifyIntegrity() {
  // [AUDIT-FIX] 致命级-完整性校验属高危操作，需操作员及以上权限
  if (!auth.isOperator) {
    message.warning(t('auditLog.integrityDenied'))
    return
  }
  try {
    const data = await auditApi.integrity()
    if (data) {
      const valid = data.valid ? t('auditLog.passMark') : t('auditLog.failMark')
      const broken = data.broken_at?.length ? data.broken_at.join(', ') : t('auditLog.none')
      dialog.info({
        title: t('auditLog.integrityTitle'),
        content: `${valid}\n${t('auditLog.detail')}: ${data.total}\n${broken}`,
        positiveText: t('common.confirm'),
      })
    }
  } catch (e: any) { message.error(extractError(e, t('auditLog.integrityFailed'))) }
}

async function doCleanup() {
  // [AUDIT-FIX] 致命级-清理审计日志属不可逆高危操作，仅 admin 可执行
  if (!auth.isAdmin) {
    message.warning(t('auditLog.cleanupDenied'))
    return false
  }
  // [AUDIT-FIX] 一般级-添加独立 loading 状态，防止重复提交
  cleanupLoading.value = true
  try {
    const data = await auditApi.cleanup(retentionDays.value)
    message.success(t('auditLog.cleanupResult', { count: data?.deleted ?? 0 }))
    await loadLogs()
  } catch (e: any) {
    message.error(extractError(e, t('auditLog.cleanupFailed')))
    return false
  } finally {
    cleanupLoading.value = false
  }
}

onMounted(() => { loadLogs() })
</script>

<style scoped>
.audit-page { padding: 16px; }
</style>
