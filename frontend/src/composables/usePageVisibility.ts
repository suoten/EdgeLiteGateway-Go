/**
 * usePageVisibility - tracks whether the page is currently visible to the user.
 *
 * Uses the Page Visibility API to detect tab switching / minimization.
 * Components use this to pause polling, reduce WebSocket traffic, etc.
 */
import { ref, onMounted, onUnmounted, type Ref } from 'vue'

// 恢复可见需持续 300ms 才确认：可见性在极短时间内反复翻动时（窗口管理器异常、
// 远程桌面/自动化环境脉冲），避免每次翻回可见都触发"全量刷新 + 定时器重启"的请求风暴
const RESTORE_CONFIRM_MS = 300

export function usePageVisibility(): { isVisible: Ref<boolean> } {
  const isVisible = ref(!document.hidden)
  let restoreTimer: ReturnType<typeof setTimeout> | null = null

  const handler = () => {
    if (document.hidden) {
      // 隐藏立即生效：尽快暂停轮询
      if (restoreTimer) { clearTimeout(restoreTimer); restoreTimer = null }
      isVisible.value = false
    } else if (!restoreTimer) {
      restoreTimer = setTimeout(() => {
        restoreTimer = null
        if (!document.hidden) isVisible.value = true
      }, RESTORE_CONFIRM_MS)
    }
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', handler)
  })

  onUnmounted(() => {
    document.removeEventListener('visibilitychange', handler)
    if (restoreTimer) { clearTimeout(restoreTimer); restoreTimer = null }
  })

  return { isVisible }
}
