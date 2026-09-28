<template>
  <div class="page-container">
    <n-card :title="t('router.backupSchedule')" size="small">
      <n-space vertical>
        <!-- Schedule Status -->
        <n-descriptions :title="t('backupSchedule.title')" bordered :column="3" size="small">
          <n-descriptions-item :label="t('backupSchedule.enabled')">
            <n-tag :type="schedule.enabled ? 'success' : 'default'" size="small">
              {{ schedule.enabled ? t('common.enabled') : t('common.disabled') }}
            </n-tag>
          </n-descriptions-item>
          <n-descriptions-item :label="t('backupSchedule.interval')">
            {{ schedule.interval_hours }}h
          </n-descriptions-item>
          <n-descriptions-item :label="t('backupSchedule.retention')">
            {{ schedule.retain_days }}d
          </n-descriptions-item>
          <n-descriptions-item :label="t('backupSchedule.backupDir')">
            {{ schedule.backup_dir }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('backupSchedule.lastBackup')">
            {{ lastBackupTime }}
          </n-descriptions-item>
          <n-descriptions-item :label="t('backupSchedule.nextBackup')">
            {{ nextBackupTime }}
          </n-descriptions-item>
        </n-descriptions>

        <!-- 调度器真实错误：以前页面只显示配置，失败完全不可见 -->
        <n-alert v-if="schedule.last_error" type="error" :title="t('backupSchedule.lastError')">
          {{ schedule.last_error }}
        </n-alert>

        <!-- 已暂存的恢复：重启后才生效，必须让操作员在重启前看得见、能取消 -->
        <n-alert v-if="schedule.pending_restore" type="warning" :title="t('backupSchedule.pendingRestoreTitle')">
          <n-space align="center" :wrap="false">
            <span>{{ t('backupSchedule.pendingRestore', { name: schedule.pending_restore.filename, at: fmt(schedule.pending_restore.staged_at) }) }}</span>
            <n-button size="tiny" :disabled="!auth.isOperator" :loading="cancelling" @click="cancelPendingRestore">
              {{ t('backupSchedule.cancelRestore') }}
            </n-button>
          </n-space>
        </n-alert>

        <!-- 调度设置 -->
        <n-collapse :default-expanded-names="auth.isOperator ? ['schedule'] : []">
          <n-collapse-item :title="t('backupSchedule.editTitle')" name="schedule">
            <n-form label-placement="top" :disabled="!auth.isOperator">
              <n-space align="end" :wrap="true">
                <n-form-item :label="t('backupSchedule.enabled')" :show-feedback="false" style="width:120px">
                  <n-switch v-model:value="form.enabled" />
                </n-form-item>
                <n-form-item :label="t('backupSchedule.intervalHoursLabel')" :show-feedback="false" style="width:150px">
                  <n-input-number v-model:value="form.interval_hours" :min="1" :max="8760" size="small" style="width:100%" />
                </n-form-item>
                <n-form-item :label="t('backupSchedule.retention')" :show-feedback="false" style="width:130px">
                  <n-input-number v-model:value="form.retain_days" :min="1" :max="3650" size="small" style="width:100%" />
                </n-form-item>
                <n-form-item :label="t('backupSchedule.minFreeMb')" :show-feedback="false" style="width:160px">
                  <n-input-number v-model:value="form.min_free_mb" :min="0" :step="100" size="small" style="width:100%" />
                </n-form-item>
                <n-form-item :label="t('backupSchedule.backupDir')" :show-feedback="false" style="width:260px">
                  <n-input v-model:value="form.backup_dir" size="small" />
                </n-form-item>
                <n-form-item label=" " :show-feedback="false">
                  <n-button type="primary" size="small" :loading="saving" @click="saveSchedule">
                    {{ t('common.save') }}
                  </n-button>
                </n-form-item>
              </n-space>
            </n-form>
            <div v-if="!auth.isOperator" class="hint">{{ t('backupSchedule.adminOnly') }}</div>
          </n-collapse-item>
        </n-collapse>

        <!-- Actions -->
        <n-space>
          <n-button type="info" :loading="creating" @click="createBackupNow">
            {{ t('backupSchedule.backupNow') }}
          </n-button>
          <n-button :loading="loading" @click="refresh">
            {{ t('common.refresh') }}
          </n-button>
        </n-space>

        <!-- Backup History -->
        <n-divider />
        <n-h3>{{ t('backupSchedule.history') }}</n-h3>
        <n-data-table
          :columns="historyColumns"
          :data="backups"
          :loading="loading"
          :bordered="false"
          size="small"
          :pagination="{ pageSize: 15 }"
        />
      </n-space>
    </n-card>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted, onUnmounted, nextTick, watch, h } from 'vue'
import { NSpace } from 'naive-ui'
import { systemApi } from '@/api'
import { t } from '@/i18n'
import { message, dialog } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { formatDateTime } from '@/utils/datetime'
import { useAuthStore } from '@/stores/auth'
import { NButton, NTag } from 'naive-ui'

const auth = useAuthStore()

const schedule = ref<any>({
  enabled: false,
  interval_hours: 24,
  retain_days: 7,
  backup_dir: 'data/backups',
  min_free_mb: 100,
  last_run: '',
  next_run: '',
  last_error: '',
})
const form = ref({ enabled: false, interval_hours: 24, retain_days: 7, min_free_mb: 100, backup_dir: 'data/backups' })
const formDirty = ref(false)
const backups = ref<any[]>([])
const loading = ref(false)
const creating = ref(false)
const saving = ref(false)
const cancelling = ref(false)

// 操作员正在编辑时，刷新不得覆盖其未保存的输入
watch(form, () => { formDirty.value = true }, { deep: true })

// API 返回升序列表，最新备份在末尾；按时间取最大值避免顺序假设
// pre_restore_* 是恢复前的安全副本，不是调度器产出的备份，别让它们冒充"上次备份"
const lastBackup = computed(() => {
  const scheduled = backups.value.filter((b: any) => typeof b?.filename === 'string' && b.filename.startsWith('edgelite_backup_'))
  if (!scheduled.length) return null
  return scheduled.reduce((a: any, b: any) => (new Date(a.created_at) > new Date(b.created_at) ? a : b))
})

function fmt(value: string): string {
  const d = new Date(value)
  return isNaN(d.getTime()) ? value : formatDateTime(d)
}

// 上次/下次时间取自调度器自身；从备份文件名推算下次时间会显示一个没人调度的时刻
const lastBackupTime = computed(() => {
  if (schedule.value.last_run) return fmt(schedule.value.last_run)
  return lastBackup.value?.created_at ? formatDateTime(lastBackup.value.created_at) : '-'
})

const nextBackupTime = computed(() => {
  if (!schedule.value.next_run) return '-'
  return fmt(schedule.value.next_run)
})

const historyColumns = computed(() => [
  { title: t('backupSchedule.backupId'), key: 'backup_id', width: 220, ellipsis: { tooltip: true } },
  { title: t('backupSchedule.fileName'), key: 'filename', ellipsis: { tooltip: true } },
  { title: t('backupSchedule.size'), key: 'size', width: 100, render: (r: any) => formatSize(r.size) },
  { title: t('backupSchedule.createdAt'), key: 'created_at', width: 200, render: (r: any) => formatDateTime(r.created_at) },
  {
    title: t('backupSchedule.actions'),
    key: 'actions',
    width: 180,
    render: (row: any) => h(NSpace, { size: 8, wrap: false }, () => [
      h(NButton, { size: 'small', text: true, type: 'primary', onClick: () => handleRestore(row) }, () => t('backupSchedule.restore')),
      h(NButton, { size: 'small', text: true, type: 'info', onClick: () => downloadBackup(row) }, () => t('backupSchedule.download')),
      h(NButton, { size: 'small', text: true, type: 'error', onClick: () => handleDelete(row) }, () => t('backupSchedule.delete')),
    ]),
  },
])

function formatSize(bytes: number): string {
  if (!bytes) return '-'
  const units = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let v = bytes
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++ }
  return `${v.toFixed(1)} ${units[i]}`
}

async function refresh() {
  loading.value = true
  try {
    const [scheduleData, historyData] = await Promise.all([
      systemApi.getBackupSchedule(),
      systemApi.listBackups(),
    ])
    // Replace, do not merge: the backend omits `pending_restore` and clears
    // `last_error` once they are gone, so merging over the previous snapshot
    // resurrected them and the page kept claiming a restore was still staged
    // after a successful cancel.
    if (scheduleData) schedule.value = scheduleData
    if (!formDirty.value) syncForm()
    backups.value = Array.isArray(historyData) ? historyData : []
    watchPendingRestore()
  } catch (e) {
    message.error(extractError(e, t('backupSchedule.loadFailed')))
  } finally {
    loading.value = false
  }
}

let pendingTimer: number | undefined

function stopPendingWatch() {
  if (pendingTimer !== undefined) {
    window.clearInterval(pendingTimer)
    pendingTimer = undefined
  }
}

// 暂存的恢复只在下一次启动生效，页面无法感知操作员是否重启了网关。不轮询的话，
// 重启后这张页仍挂着"存在待生效的恢复"，操作员要么以为恢复没发生，要么去重复暂存。
// 判据用快照文件名集合而不是时间：文件名带秒级时间戳且唯一，比时间戳可靠——按时间比较
// 必须留时钟余量，而"暂存后 1 分钟内就重启"完全正常，余量会把上一次恢复的快照算成这次的。
function snapshotNames(list: any[]): Set<string> {
  return new Set((Array.isArray(list) ? list : [])
    .map((b: any) => String(b?.filename || ''))
    .filter(n => n.startsWith('pre_restore_')))
}

function watchPendingRestore() {
  const staged = schedule.value.pending_restore
  if (!staged?.filename) {
    stopPendingWatch()
    return
  }
  if (pendingTimer !== undefined) return
  const baseline = snapshotNames(backups.value)
  pendingTimer = window.setInterval(async () => {
    try {
      const [s, list] = await Promise.all([
        systemApi.getBackupSchedule(),
        systemApi.listBackups(),
      ])
      if (s?.pending_restore) return
      stopPendingWatch()
      // The alert itself renders from schedule.pending_restore, so it has to be
      // replaced here too: updating only the backup list showed "restore applied"
      // while the page went on claiming a restore was still waiting for a restart.
      if (s && typeof s === 'object') schedule.value = s
      backups.value = Array.isArray(list) ? list : backups.value
      const applied = snapshotNames(list).size > baseline.size
      if (applied) message.info(t('backupSchedule.restoreApplied'))
      else message.warning(t('backupSchedule.restorePendingGone'))
    } catch { /* 网关重启期间请求必然失败，下一轮再看 */ }
  }, 5000)
}

function syncForm() {
  form.value = {
    enabled: !!schedule.value.enabled,
    interval_hours: Number(schedule.value.interval_hours) || 24,
    retain_days: Number(schedule.value.retain_days) || 7,
    min_free_mb: Number(schedule.value.min_free_mb) || 0,
    backup_dir: schedule.value.backup_dir || 'data/backups',
  }
  // 赋值本身会触发 deep watcher，需要在下一轮再解锁
  nextTick(() => { formDirty.value = false })
}

function saveSchedule() {
  if (!auth.isOperator) { message.warning(t('common.permissionDenied')); return }
  dialog.warning({
    title: t('backupSchedule.editTitle'),
    content: t('backupSchedule.confirmChange'),
    positiveText: t('common.save'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      saving.value = true
      try {
        const res: any = await systemApi.updateBackupSchedule({ ...form.value })
        const ignored: string[] = Array.isArray(res?.ignored_keys) ? res.ignored_keys : []
        formDirty.value = false
        await refresh()
        // 后端会静默丢弃结构里没有的键，这里必须让操作员看到
        if (ignored.length) {
          message.warning(t('backupSchedule.keysIgnored', { keys: ignored.join(', ') }))
        } else {
          message.success(t('backupSchedule.scheduleSaved'))
        }
      } catch (e) {
        message.error(extractError(e, t('backupSchedule.scheduleSaveFailed')))
      } finally {
        saving.value = false
      }
    },
  })
}

async function createBackupNow() {
  creating.value = true
  const before = backups.value.length
  try {
    const res: any = await systemApi.createBackup()
    await refresh()
    const name = res?.filename
    // 接口报成功不算成功：确认文件真的出现在备份列表里
    const listed = name
      ? backups.value.some((b: any) => b.filename === name)
      : backups.value.length > before
    if (!listed) {
      message.warning(t('backupSchedule.backupNotListed', { name: name || '-' }))
    } else {
      message.success(t('backupSchedule.backupCreated'))
    }
  } catch (e) {
    message.error(extractError(e, t('backupSchedule.backupFailed')))
  } finally {
    creating.value = false
  }
}

function handleRestore(row: any) {
  dialog.warning({
    title: t('backupSchedule.restore'),
    content: t('backupSchedule.restoreConfirm', { id: row.backup_id || row.filename }),
    positiveText: t('backupSchedule.restore'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        const res: any = await systemApi.restore(row.filename)
        await refresh()
        // 后端只暂存恢复请求，不能说成"恢复成功"
        if (res?.staged) {
          message.success(t('backupSchedule.restoreStaged', { name: res.filename || row.filename }))
        } else if (res?.applied) {
          message.success(t('backupSchedule.restoreSuccess'))
        } else {
          message.warning(t('backupSchedule.restoreNotApplied', { msg: res?.message || '-' }))
        }
      } catch (e) {
        message.error(extractError(e, t('backupSchedule.restoreFailed')))
      }
    },
  })
}

async function cancelPendingRestore() {
  cancelling.value = true
  try {
    const res: any = await systemApi.cancelRestore()
    await refresh()
    if (res?.cancelled && !schedule.value.pending_restore) {
      message.success(t('backupSchedule.restoreCancelled'))
    } else {
      message.warning(t('backupSchedule.restoreCancelFailed'))
    }
  } catch (e) {
    message.error(extractError(e, t('backupSchedule.restoreCancelFailed')))
  } finally {
    cancelling.value = false
  }
}

function handleDelete(row: any) {
  dialog.warning({
    title: t('backupSchedule.delete'),
    content: t('backupSchedule.deleteConfirm', { id: row.backup_id || row.filename }),
    positiveText: t('backupSchedule.delete'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await systemApi.deleteBackup(row.filename)
        message.success(t('backupSchedule.deleteSuccess'))
        await refresh()
      } catch (e) {
        message.error(extractError(e, t('backupSchedule.deleteFailed')))
      }
    },
  })
}

async function downloadBackup(row: any) {
  try {
    const blob = await systemApi.downloadBackup(row.filename)
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = row.filename
    a.click()
    URL.revokeObjectURL(url)
  } catch (e) {
    message.error(extractError(e, t('backupSchedule.loadFailed')))
  }
}

onMounted(() => {
  refresh()
})

onUnmounted(stopPendingWatch)
</script>

<style scoped>
.page-container { padding: 16px; }
.hint { font-size: 12px; opacity: .65; }
</style>
