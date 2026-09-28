import { ref } from 'vue'
import { deviceApi, ruleApi } from '@/api'

// 全局共享的 id→名称映射：告警/规则相关 UI（通知中心、告警列表等）只需加载一次，
// 避免每个组件各自全量拉取设备/规则列表。
const deviceNameMap = ref<Record<string, string>>({})
const ruleNameMap = ref<Record<string, string>>({})
let loaded = false
let inflight: Promise<void> | null = null

async function loadEntityNames(force = false): Promise<void> {
  if (loaded && !force) return
  if (inflight) return inflight
  inflight = (async () => {
    try {
      const [devices, rules] = await Promise.all([
        deviceApi.list({ page: 1, size: 200 }),
        ruleApi.list({ page: 1, size: 200 }),
      ])
      const dm: Record<string, string> = {}
      for (const d of devices?.data ?? []) dm[d.device_id] = d.name || d.device_id
      deviceNameMap.value = dm
      const rm: Record<string, string> = {}
      for (const r of rules?.data ?? []) rm[r.rule_id] = r.name || r.rule_id
      ruleNameMap.value = rm
      loaded = true
    } catch { /* ignore: 名称解析失败时调用方回退到原始 ID */ } finally {
      inflight = null
    }
  })()
  return inflight
}

export function useEntityNames() {
  return { deviceNameMap, ruleNameMap, loadEntityNames }
}
