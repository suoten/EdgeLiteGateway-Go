<template>
  <div class="les">
    <n-empty :description="failed ? failedDescription : description" class="les-empty" />
    <n-button v-if="failed" size="small" :loading="retrying" @click="emit('retry')">
      {{ t('common.retry') }}
    </n-button>
    <slot v-else name="extra" />
  </div>
</template>

<script setup lang="ts">
import { computed, watch } from 'vue'
import { t } from '@/i18n'
import { useApiHealth } from '@/composables/useApiHealth'

// 请求失败时，列表页原本只会退回普通空状态，用户会把"没加载出来"读成"没有数据"。
// 该组件把失败态与空态区分开，并提供重试入口。
const props = withDefaults(defineProps<{
  description: string
  failed?: boolean
  failedDescription?: string
  retrying?: boolean
}>(), {
  failed: false,
  failedDescription: '',
  retrying: false,
})

const emit = defineEmits<{ retry: [] }>()

const failedDescription = computed(() => props.failedDescription || t('common.loadFailedDesc'))

// 后端恢复后自动重取：否则用户仍停留在失败空态，需要再点一次重试
const apiHealth = useApiHealth()
watch(() => apiHealth.recoveredAt.value, (ts) => {
  if (ts && props.failed && !props.retrying) emit('retry')
})
</script>

<style scoped>
.les {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  padding: 8px 0 24px;
}
.les-empty {
  padding: 24px 0 8px;
}
</style>
