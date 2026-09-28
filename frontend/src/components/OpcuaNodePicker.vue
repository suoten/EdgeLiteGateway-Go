<template>
  <n-modal
    :show="show"
    preset="card"
    :title="t('deviceDetail.opcuaBrowseNodes')"
    style="width: 780px; max-width: 95vw"
    :close-on-esc="true"
    :auto-focus="true"
    @update:show="emit('update:show', $event)"
  >
    <n-space align="center" justify="space-between" style="margin-bottom: 8px">
      <n-breadcrumb>
        <n-breadcrumb-item v-for="(p, i) in path" :key="p.node_id || 'root'" @click="goTo(i)">
          {{ p.name || t('deviceDetail.opcuaBrowseRoot') }}
        </n-breadcrumb-item>
      </n-breadcrumb>
      <n-button size="small" tertiary :loading="loading" @click="load">
        {{ t('common.refresh') }}
      </n-button>
    </n-space>

    <n-data-table
      :columns="columns"
      :data="sorted"
      :loading="loading"
      :bordered="false"
      size="small"
      :max-height="380"
      :pagination="false"
    >
      <template #empty>
        <n-empty :description="error || t('deviceDetail.opcuaNoNodes')" size="small" />
      </template>
    </n-data-table>

    <n-text v-if="!error" depth="3" style="font-size: 12px">
      {{ t('deviceDetail.opcuaBrowseHint') }}
    </n-text>
  </n-modal>
</template>

<script setup lang="ts">
import { ref, computed, watch, h } from 'vue'
import { NButton, NTag } from 'naive-ui'
import { driverApi } from '@/api'
import { t } from '@/i18n'

// A node in a server's address space, as the browse service reports it.
interface BrowseEntry {
  node_id: string
  browse_name?: string
  display_name?: string
  node_class?: string
  data_type?: string
  writable?: boolean
  is_container?: boolean
}

const props = withDefaults(defineProps<{
  show: boolean
  config: Record<string, unknown>
  deviceId?: string
}>(), { deviceId: '' })

const emit = defineEmits<{
  (e: 'update:show', v: boolean): void
  // The picker owns what a browsed node means as a point, because that is where
  // the server's data type and AccessLevel are known; the caller only inserts it.
  (e: 'pick', p: { name: string; address: string; dataType: string; writable: boolean }): void
}>()

const entries = ref<BrowseEntry[]>([])
const loading = ref(false)
const error = ref('')
// The walk is a stack so "back" is one click at any depth; the root entry is the
// empty NodeID, which the driver resolves to the Objects folder.
const path = ref<{ node_id: string; name: string }[]>([{ node_id: '', name: '' }])

// OPC UA type names the platform can store as a point. A node whose type is not
// here (Int64, DateTime, structures, arrays) is listed but not offered: adding it
// with a guessed type would silently reinterpret every value it reads.
const POINT_DATA_TYPES: Record<string, string> = {
  Boolean: 'bool',
  Byte: 'uint16',
  SByte: 'int16',
  Int16: 'int16',
  UInt16: 'uint16',
  Int32: 'int32',
  UInt32: 'uint32',
  Float: 'float32',
  Double: 'float64',
  String: 'string',
}

function pointDataType(entry: BrowseEntry): string {
  return POINT_DATA_TYPES[entry.data_type ?? ''] ?? ''
}

// A BrowseName is "namespace:name"; the bare name is what reads well as a point
// name, and the NodeID is kept separately as the address.
function pickedPoint(e: BrowseEntry) {
  const raw = (e.browse_name ?? '').replace(/^[0-9]+:/, '') || e.display_name || e.node_id
  return {
    name: raw.replace(/[^A-Za-z0-9_.-]/g, '_'),
    address: e.node_id,
    dataType: pointDataType(e),
    writable: !!e.writable,
  }
}

function nodeLabel(e: BrowseEntry): string {
  return e.display_name || e.browse_name || e.node_id
}

// Containers sort first: an address space is navigated before it is read, and a
// folder buried under fifty tags is easy to miss.
const sorted = computed(() =>
  [...entries.value].sort((a, b) => Number(!!b.is_container) - Number(!!a.is_container)),
)

const columns = computed(() => [
  {
    title: t('common.name'),
    key: 'name',
    ellipsis: { tooltip: true },
    render: (r: BrowseEntry) => nodeLabel(r),
  },
  { title: t('deviceDetail.address'), key: 'node_id', ellipsis: { tooltip: true } },
  {
    title: t('deviceDetail.dataType'),
    key: 'data_type',
    width: 90,
    render: (r: BrowseEntry) => (r.node_class === 'variable' ? (r.data_type || '-') : '-'),
  },
  {
    title: t('deviceDetail.accessMode'),
    key: 'writable',
    width: 80,
    render: (r: BrowseEntry) => {
      if (r.node_class !== 'variable') return '-'
      return h(
        NTag,
        { size: 'tiny', type: r.writable ? 'success' : 'default', bordered: false },
        { default: () => (r.writable ? t('deviceDetail.opcuaBrowseWritable') : t('deviceDetail.opcuaBrowseReadOnly')) },
      )
    },
  },
  {
    title: t('common.actions'),
    key: 'action',
    width: 96,
    render: (r: BrowseEntry) => {
      if (r.is_container) {
        return h(
          NButton,
          { size: 'tiny', tertiary: true, onClick: () => descend(r) },
          { default: () => t('deviceDetail.opcuaBrowseEnter') },
        )
      }
      // An unsupported type still gets a row: the operator can read the NodeID,
      // they just cannot hand it to the collector as a point of the wrong type.
      if (!pointDataType(r)) return h('span', { style: 'font-size:12px' }, '-')
      return h(
        NButton,
        { size: 'tiny', type: 'primary', tertiary: true, onClick: () => emit('pick', pickedPoint(r)) },
        { default: () => t('deviceDetail.opcuaBrowseAdd') },
      )
    },
  },
])

async function load() {
  loading.value = true
  error.value = ''
  const current = path.value[path.value.length - 1]
  try {
    const params: { device_id?: string; node_id?: string; config?: Record<string, unknown> } = {
      node_id: current.node_id,
    }
    if (props.deviceId) params.device_id = props.deviceId
    else params.config = props.config
    const data = await driverApi.opcuaBrowse(params)
    entries.value = Array.isArray(data) ? data : []
  } catch (e: any) {
    entries.value = []
    // The gateway answers with the driver's own reason ( refused connection, bad
    // credentials, unsupported security mode); showing it is the whole point of
    // browsing before saving, because that error is the form's feedback.
    error.value = e?.response?.data?.message || e?.message || t('deviceDetail.opcuaBrowseFailed')
  } finally {
    loading.value = false
  }
}

function descend(e: BrowseEntry) {
  path.value.push({ node_id: e.node_id, name: e.display_name || e.browse_name || e.node_id })
  load()
}

function goTo(i: number) {
  if (i === path.value.length - 1) return
  path.value = path.value.slice(0, i + 1)
  load()
}

watch(
  () => props.show,
  (v) => {
    if (!v) return
    path.value = [{ node_id: '', name: '' }]
    load()
  },
)
</script>
