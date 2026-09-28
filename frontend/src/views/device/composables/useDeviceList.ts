/**
 * useDeviceList - composable that manages device list state and operations.
 *
 * Provides reactive state for devices, filters, modals, forms, batch operations,
 * discovery, import/export, and configuration comparison.
 */
import { ref, reactive, computed, onMounted, onUnmounted, watch, h, type Ref, type ComputedRef } from 'vue'
import { useRoute } from 'vue-router'
import { NButton, NTag } from 'naive-ui'
import http from '@/api/http'
import { deviceApi, userApi, type Device, type DeviceCreateParams, type PointDef, type AccessMode } from '@/api'
import { dialog, message } from '@/utils/discreteApi'
import { t } from '@/i18n'
import { extractError } from '@/utils/errorCodes'
import { deviceStatusLabel, deviceStatusColor, protocolLabel } from '@/utils/enumLabels'
import { connect, disconnect, onStatus, offStatus } from '@/api/websocket'
import { PROTOCOL_CONFIGS, normalizeProtocolName } from '@/constants/protocolConfig'
import { useUrlState } from '@/composables/useUrlState'
import { useAuthStore } from '@/stores/auth'

// Type definitions
interface PaginationState { page: number; pageSize: number; itemCount: number; pageSizes?: number[]; onChange?: (page: number) => void; onUpdatePageSize?: (size: number) => void; showSizePicker?: boolean }

export function useDeviceList() {
  // Row actions mirror the per-route permissions the backend enforces, so a
  // role cannot click its way into a guaranteed 403.
  const auth = useAuthStore()

  // ─── Core state ───
  const devices = ref<Device[]>([])
  const loading = ref(false)
  const loadFailed = ref(false)
  const searchText = ref('')
  const filterStatus = ref<string | null>(null)
  const filterProtocol = ref<string | null>(null)
  const collectFilter = ref<string>('all')
  const checkedKeys = ref<string[]>([])
  const pagination = reactive<PaginationState>({
    page: 1, pageSize: 20, itemCount: 0, pageSizes: [10, 20, 50, 100], showSizePicker: true,
    onChange: (page: number) => { pagination.page = page; fetchDevices() },
    onUpdatePageSize: (size: number) => { pagination.pageSize = size; pagination.page = 1; fetchDevices() },
  })

  // ─── WebSocket status ───
  const wsConnected = ref(false)
  const wsReconnecting = ref(false)
  let _wsStatusHandler: ((status: string, reason?: string) => void) | null = null

  // ─── Modal visibility ───
  const showCreateModal = ref(false)
  const showEditModal = ref(false)
  const showSimModal = ref(false)
  const showDiscoverModal = ref(false)
  const showImportModal = ref(false)
  const showDeployModal = ref(false)
  const showShareModal = ref(false)
  const showTransferModal = ref(false)

  // ─── Forms ───
  const createForm = reactive<DeviceCreateParams & { points: PointDef[]; tags: string[] }>({
    device_id: '', name: '', protocol: '', config: {}, collect_interval: 5, points: [], tags: [] as string[],
  })
  const editForm = reactive<Device & { points: PointDef[]; tags: string[] }>({
    device_id: '', name: '', protocol: '', status: 'offline', config: {}, collect_interval: 5,
    points: [], created_by: null, created_at: '', updated_at: '', version: 0, tags: [] as string[],
  })
  const simForm = reactive({
    device_id: '', name: '', protocol: 'simulator', config: { interval: 1 }, collect_interval: 1,
    points: [] as PointDef[],
  })
  const shareForm = reactive({ device_ids: [] as string[], target_gateway_id: '', permissions: ['read'] as string[], expires_hours: 24 })
  const transferForm = reactive({ device_ids: [] as string[], target_user_id: '', device_name: '', new_owner_id: '' })

  // ─── Loading flags ───
  const creating = ref(false)
  const editSaving = ref(false)
  const discovering = ref(false)
  const addingDevices = ref(false)
  const importing = ref(false)
  const importProgress = ref(0)
  const deploying = ref(false)
  const batchCollectLoading = ref(false)
  const collectActionLoading = ref<Record<string, boolean>>({})
  const batchDeleteLoading = ref(false)
  const sharing = ref(false)
  const transferring = ref(false)
  const testingConnection = ref(false)
  const connectionTestResult = ref<{ ok: boolean; success?: boolean; supported?: boolean; message: string; address?: string } | null>(null)

  // ─── Discovery ───
  const discoverHost = ref('192.168.1.1')
  const discoverPort = ref(502)
  const discoverProtocol = ref('modbus_tcp')
  // How many Modbus unit IDs to ask behind one host:port. 0 keeps the scan to the
  // device's own slave_id; sweeping the whole 1..247 range is the operator's call
  // because it opens that many connections at a single serial port.
  const discoverScanUnits = ref(0)
  const discoverResults = ref<any[]>([])
  const selectedDiscoverKeys = ref<string[]>([])

  // A Modbus scan can find several slaves on the same host:port, so the address
  // alone does not identify a row.
  function discoverKey(r: any): string {
    return r?.unit_id != null ? `${r.ip}:${r.port}:${r.unit_id}` : `${r.ip}:${r.port}`
  }
  const importPreview = ref<any[]>([])
  const importErrors = ref<any[]>([])
  const importAtomicMode = ref(true)

  // ─── Deploy ───
  const deployTemplateId = ref<string | null>('')
  const deployTemplateOptions = computed(() => devices.value.map(d => ({ label: `${d.name} (${d.device_id})`, value: d.device_id })))

  // ─── Selects ───
  const protocolOptions = computed(() => {
    const configs = PROTOCOL_CONFIGS.value
    return Object.keys(configs).map(k => ({
      // The driver table showed an "experimental" badge the device form never
      // repeated, so a protocol whose write path does not exist could be picked
      // without any hint that it is partial.
      label: configs[k].experimental ? `${configs[k].label} (${t('capabilities.experimental')})` : configs[k].label,
      value: k,
    }))
  })
  const statusOptions = computed(() => [
    { label: t('deviceList.statusOnline'), value: 'online' },
    { label: t('deviceList.statusOffline'), value: 'offline' },
    { label: t('deviceList.statusError'), value: 'error' },
  ])
  const discoverProtocolOptions = computed(() =>
    Object.keys(PROTOCOL_CONFIGS.value).filter(k => PROTOCOL_CONFIGS.value[k].capabilities?.discover).map(k => ({
      label: PROTOCOL_CONFIGS.value[k].label, value: k,
    }))
  )
  const dataTypeOptions = [
    { label: 'BOOL', value: 'bool' },
    { label: 'INT16', value: 'int16' },
    { label: 'INT32', value: 'int32' },
    { label: 'UINT16', value: 'uint16' },
    { label: 'UINT32', value: 'uint32' },
    { label: 'FLOAT32', value: 'float32' },
    { label: 'FLOAT64', value: 'float64' },
    { label: 'STRING', value: 'string' },
  ]
  const accessModeOptions = [
    { label: t('deviceList.accessReadOnly'), value: 'r' },
    { label: t('deviceList.accessReadWrite'), value: 'rw' },
    { label: t('deviceList.accessWriteOnly'), value: 'w' },
  ]
  const simModeOptions = [
    { label: t('deviceList.simSine'), value: 'sine' },
    { label: t('deviceList.simRandom'), value: 'random' },
    { label: t('deviceList.simStatic'), value: 'static' },
    { label: t('deviceList.simRamp'), value: 'ramp' },
  ]
  const userOptions = ref<{ label: string; value: string }[]>([])

  // ─── Form refs ───
  const createFormRef = ref<any>(null)
  const editFormRef = ref<any>(null)
  const simFormRef = ref<any>(null)
  const protocolFormRef = ref<any>(null)
  const protocolEditRef = ref<any>(null)
  const shareFormRef = ref<any>(null)
  const transferFormRef = ref<any>(null)

  // ─── Driver schemas ───
  const driverSchemas = ref<Record<string, any>>({})

  // ─── Tags ───
  const selectedTags = ref<string[]>([])
  const tagOptions = computed(() => {
    const tags = new Set<string>()
    devices.value.forEach(d => {
      const dtags = (d as any).tags
      if (Array.isArray(dtags)) dtags.forEach((t: string) => tags.add(t))
    })
    return Array.from(tags).map(t => ({ label: t, value: t }))
  })
  const filteredDevicesByTag = computed(() => {
    if (!selectedTags.value.length) return devices.value
    return devices.value.filter(d => {
      const dtags = (d as any).tags || []
      return selectedTags.value.some(t => dtags.includes(t))
    })
  })
  function getDeviceTags(device: Device): string[] {
    return (device as any).tags || []
  }

  // ─── Compare ───
  const showCompareModal = ref(false)
  const compareDeviceAId = ref<string>('')
  const compareDeviceBId = ref<string>('')
  const compareLoading = ref(false)
  const compareDeviceA = ref<Device | null>(null)
  const compareDeviceB = ref<Device | null>(null)
  const compareDeviceOptions = computed(() => devices.value.map(d => ({ label: d.name, value: d.device_id })))
  const compareRows = ref<any[]>([])
  const compareDiffCount = ref(0)
  function openCompareModal() { showCompareModal.value = true; compareRows.value = []; compareDiffCount.value = 0 }
  async function handleCompare() {
    if (!compareDeviceAId.value || !compareDeviceBId.value) return
    compareLoading.value = true
    try {
      const [a, b] = await Promise.all([
        deviceApi.get(compareDeviceAId.value),
        deviceApi.get(compareDeviceBId.value),
      ])
      compareDeviceA.value = a
      compareDeviceB.value = b
      const rows: any[] = []
      let diffCount = 0
      const allKeys = new Set([...Object.keys(a || {}), ...Object.keys(b || {})])
      allKeys.forEach(k => {
        const va = (a as any)?.[k]
        const vb = (b as any)?.[k]
        const isDiff = JSON.stringify(va) !== JSON.stringify(vb)
        if (isDiff) diffCount++
        rows.push({ key: k, valueA: va, valueB: vb, diff: isDiff })
      })
      compareRows.value = rows
      compareDiffCount.value = diffCount
    } catch (e) {
      message.error(extractError(e))
    } finally {
      compareLoading.value = false
    }
  }

  // ─── Active filter count ───
  const activeFilterCount = computed(() => {
    let n = 0
    if (searchText.value) n++
    if (filterStatus.value) n++
    if (filterProtocol.value) n++
    if (collectFilter.value !== 'all') n++
    if (selectedTags.value.length) n++
    return n
  })

  function resetFilters() {
    searchText.value = ''
    filterStatus.value = null
    filterProtocol.value = null
    collectFilter.value = 'all'
    selectedTags.value = []
    pagination.page = 1
    fetchDevices()
  }

  // ─── Discover columns ───
  const discoverColumns = computed(() => [
    { type: 'selection' as const },
    { title: t('deviceList.ip'), key: 'ip' },
    { title: t('deviceList.port'), key: 'port' },
    // A Modbus scan reports one row per unit ID, so the address alone would show
    // the same row many times over.
    { title: t('deviceList.discoverUnit'), key: 'unit_id', render: (row: any) => (row.unit_id != null ? row.unit_id : '-') },
    { title: t('deviceList.protocol'), key: 'protocol' },
  ])

  // ─── Import preview columns ───
  const importPreviewColumns = computed(() => [
    { title: t('deviceList.deviceId'), key: 'device_id' },
    { title: t('deviceList.name'), key: 'name' },
    { title: t('deviceList.protocol'), key: 'protocol' },
    { title: t('common.status'), key: 'status', render: (row: any) => row.error ? t('common.error') : t('common.ok') },
  ])

  // ─── Validation rules ───
  const createRules = computed(() => ({
    device_id: { required: true, message: t('deviceList.deviceIdRequired'), trigger: 'blur' },
    name: { required: true, message: t('deviceList.nameRequired'), trigger: 'blur' },
    protocol: { required: true, message: t('deviceList.protocolRequired'), trigger: 'change' },
  }))
  const editRules = computed(() => ({
    name: { required: true, message: t('deviceList.nameRequired'), trigger: 'blur' },
  }))
  const simFormRules = computed(() => ({
    device_id: { required: true, message: t('deviceList.deviceIdRequired'), trigger: 'blur' },
    name: { required: true, message: t('deviceList.nameRequired'), trigger: 'blur' },
  }))
  const shareRules = computed(() => ({
    target_gateway_id: { required: true, message: t('resourceShare.targetGatewayRequired'), trigger: 'change' },
  }))
  const transferRules = computed(() => ({
    target_user_id: { required: true, message: t('deviceList.selectUser'), trigger: 'change' },
  }))

  // ─── Individual device actions ───
  // The action button keys off row.collecting (live scheduler state), so the
  // label flips 停止采集/开始采集 immediately after the refresh below.
  async function toggleCollect(deviceId: string, stop: boolean) {
    if (collectActionLoading.value[deviceId]) return
    collectActionLoading.value = { ...collectActionLoading.value, [deviceId]: true }
    try {
      const resp = stop
        ? await deviceApi.batchStopCollect([deviceId])
        : await deviceApi.batchStartCollect([deviceId])
      if (!resp || resp.success_count < 1) {
        const reason = resp?.failed?.[deviceId]
        const failMsg = t(stop ? 'deviceList.batchStopCollectFailed' : 'deviceList.batchStartCollectFailed')
        message.error(reason ? `${failMsg}: ${reason}` : failMsg)
        return
      }
      message.success(t(stop ? 'deviceList.batchStopSuccess' : 'deviceList.batchStartSuccess'))
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      const next = { ...collectActionLoading.value }
      delete next[deviceId]
      collectActionLoading.value = next
    }
  }
  const handleStartCollect = (deviceId: string) => toggleCollect(deviceId, false)
  const handleStopCollect = (deviceId: string) => toggleCollect(deviceId, true)
  async function handleDeleteDevice(deviceId: string, deviceName?: string) {
    dialog.warning({
      title: t('common.confirm'),
      content: t('deviceList.deleteConfirm', { name: deviceName || deviceId }),
      positiveText: t('common.delete'),
      negativeText: t('common.cancel'),
      onPositiveClick: async () => {
        try {
          await deviceApi.delete(deviceId)
          message.success(t('common.deleteSuccess'))
          await fetchDevices()
        } catch (e) {
          message.error(extractError(e))
        }
      },
    })
  }

  // ─── Table columns ───
  const columns = computed(() => [
    { type: 'selection' as const },
    { title: t('deviceList.deviceId'), key: 'device_id', width: 150, ellipsis: { tooltip: true }, sorter: true },
    { title: t('deviceList.name'), key: 'name', width: 150, ellipsis: { tooltip: true } },
    { title: t('deviceList.protocol'), key: 'protocol', width: 110, render: (row: any) => protocolLabel.value[row.protocol] || row.protocol },
    { title: t('deviceList.status'), key: 'status', width: 90, render: (row: any) => h(NTag, { size: 'small', type: deviceStatusColor[row.status] || 'default', bordered: false }, { default: () => deviceStatusLabel.value[row.status] || row.status }) },
    { title: t('deviceList.collectInterval'), key: 'collect_interval', width: 90, render: (row: any) => (row.collect_interval != null ? `${row.collect_interval}s` : '-') },
    {
      title: t('common.actions'),
      key: 'actions',
      width: 230,
      fixed: 'right' as const,
      render: (row: any) => h('div', { style: 'display:flex;gap:4px;flex-wrap:wrap' }, [
        h(NButton, { size: 'small', text: true, type: 'primary', disabled: !auth.hasPerm('device:update'), onClick: () => handleEdit(row) }, { default: () => t('common.edit') }),
        row.collecting
          ? h(NButton, { size: 'small', text: true, type: 'warning', disabled: !auth.hasPerm('device:update'), loading: collectActionLoading.value[row.device_id] === true, onClick: () => handleStopCollect(row.device_id) }, { default: () => t('deviceList.stopCollect') })
          : h(NButton, { size: 'small', text: true, type: 'success', disabled: !auth.hasPerm('device:update'), loading: collectActionLoading.value[row.device_id] === true, onClick: () => handleStartCollect(row.device_id) }, { default: () => t('deviceList.startCollect') }),
        h(NButton, { size: 'small', text: true, type: 'info', disabled: !auth.hasPerm('device:create'), onClick: () => handleCloneDevice(row) }, { default: () => t('deviceList.clone') }),
        h(NButton, { size: 'small', text: true, type: 'error', disabled: !auth.hasPerm('device:delete'), onClick: () => handleDeleteDevice(row.device_id, row.name) }, { default: () => t('common.delete') }),
      ]),
    },
  ])

  // ─── Fetch devices ───
  async function fetchDevices() {
    loading.value = true
    try {
      const params: any = { page: pagination.page, size: pagination.pageSize }
      if (searchText.value) params.search = searchText.value
      if (filterStatus.value) params.status = filterStatus.value
      if (filterProtocol.value) params.protocol = filterProtocol.value
      if (collectFilter.value !== 'all') params.collecting = collectFilter.value === 'collecting'
      const resp = await deviceApi.list(params)
      devices.value = resp.data || []
      pagination.itemCount = resp.total || 0
      loadFailed.value = false
    } catch (e) {
      // 保留已加载数据并标记失败：清空列表会让"服务异常"看起来像"没有设备"
      loadFailed.value = true
      message.error(extractError(e))
    } finally {
      loading.value = false
    }
  }

  // ─── Protocol change handler ───
  function onProtocolChange(protocol: string) {
    createForm.protocol = protocol
    // Drop the previous protocol's connection-test verdict so a stale "connected"
    // is not shown against the newly-selected protocol's (empty) config.
    connectionTestResult.value = null
    const cfg = PROTOCOL_CONFIGS.value[normalizeProtocolName(protocol)]
    if (cfg) {
      const defaults: Record<string, any> = {}
      cfg.configFields.forEach(f => {
        if (f.default !== undefined) defaults[f.key] = f.default
      })
      createForm.config = { ...defaults }
    }
  }

  // Build sample points from the protocol's declared pointTemplates so the
  // point-definition step is not empty-by-default. access_mode in templates is
  // non-canonical ('read'/'write'); the rest of the app keys write-gating off
  // 'r'/'w'/'rw', so normalize here at the single consumption point.
  function normalizeAccessMode(raw?: string): AccessMode {
    const s = (raw || '').toLowerCase()
    if (s === 'write' || s === 'w') return 'w'
    if (s === 'readwrite' || s === 'rw') return 'rw'
    return 'r'
  }
  const createPointTemplates = computed<PointDef[]>(() => {
    const cfg = PROTOCOL_CONFIGS.value[normalizeProtocolName(createForm.protocol)]
    return (cfg?.pointTemplates ?? []).map(pt => ({
      name: pt.name,
      data_type: pt.data_type as PointDef['data_type'],
      unit: pt.unit ?? '',
      address: pt.address ?? '',
      access_mode: normalizeAccessMode(pt.access_mode ?? pt.access),
    }))
  })
  function applyPointTemplates() {
    createForm.points = createPointTemplates.value.map(p => ({ ...p }))
  }

  // The create dialog used to have an "experimental protocol" confirmation
  // wired only in its i18n file, so nothing ever warned that ONVIF cannot be
  // written to or that OPC DA cannot run at all.
  const createProtocolExperimental = computed(() => {
    const cfg = PROTOCOL_CONFIGS.value[normalizeProtocolName(createForm.protocol)]
    return !!cfg?.experimental
  })

  // ─── Create ───
  async function handleCreate(protocolConfig?: any) {
    if (creating.value) return
    try {
      await createFormRef.value?.validate()
    } catch { return }
    creating.value = true
    try {
      if (protocolConfig && typeof protocolConfig === 'object') {
        createForm.config = { ...createForm.config, ...protocolConfig }
      }
      await deviceApi.create({ ...createForm })
      message.success(t('common.createSuccess'))
      showCreateModal.value = false
      clearCreateDraft()
      resetCreateForm()
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      creating.value = false
    }
  }

  // ─── Edit ───
  function handleEdit(row: Device) {
    Object.assign(editForm, JSON.parse(JSON.stringify(row)))
    showEditModal.value = true
  }
  async function handleEditSubmit(protocolConfig?: any, protocolPoints?: any) {
    if (editSaving.value) return
    try {
      await editFormRef.value?.validate()
    } catch { return }
    editSaving.value = true
    try {
      if (protocolConfig && typeof protocolConfig === 'object') {
        editForm.config = { ...editForm.config, ...protocolConfig }
      }
      if (protocolPoints && Array.isArray(protocolPoints)) {
        editForm.points = protocolPoints
      }
      await deviceApi.update(editForm.device_id, { ...editForm })
      message.success(t('common.saveSuccess'))
      showEditModal.value = false
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      editSaving.value = false
    }
  }

  // ─── Batch operations ───
  async function handleBatchDelete() {
    if (!checkedKeys.value.length) return
    dialog.warning({
      title: t('common.confirm'),
      content: t('deviceList.batchDeleteConfirm', { count: checkedKeys.value.length }),
      positiveText: t('common.delete'),
      negativeText: t('common.cancel'),
      onPositiveClick: async () => {
        batchDeleteLoading.value = true
        try {
          await Promise.all(checkedKeys.value.map(id => deviceApi.delete(id)))
          message.success(t('common.deleteSuccess'))
          checkedKeys.value = []
          await fetchDevices()
        } catch (e) {
          message.error(extractError(e))
        } finally {
          batchDeleteLoading.value = false
        }
      },
    })
  }

  async function handleBatchStartCollect() {
    if (!checkedKeys.value.length) return
    batchCollectLoading.value = true
    try {
      await deviceApi.batchStartCollect(checkedKeys.value)
      message.success(t('deviceList.batchStartSuccess'))
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      batchCollectLoading.value = false
    }
  }

  async function handleBatchStopCollect() {
    if (!checkedKeys.value.length) return
    batchCollectLoading.value = true
    try {
      await deviceApi.batchStopCollect(checkedKeys.value)
      message.success(t('deviceList.batchStopSuccess'))
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      batchCollectLoading.value = false
    }
  }

  async function handleBatchDeploy() {
    if (!checkedKeys.value.length || !deployTemplateId.value) return
    deploying.value = true
    try {
      await deviceApi.batchDeploy(deployTemplateId.value, checkedKeys.value)
      message.success(t('deviceList.batchDeploySuccess'))
      showDeployModal.value = false
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      deploying.value = false
    }
  }

  // ─── Discovery ───
  async function handleDiscover() {
    discovering.value = true
    discoverResults.value = []
    selectedDiscoverKeys.value = []
    try {
      const data = await deviceApi.discover({
        protocol: discoverProtocol.value,
        host: discoverHost.value,
        port: discoverPort.value,
        config: supportsUnitSweep(discoverProtocol.value) ? { scan_units: discoverScanUnits.value } : undefined,
      })
      discoverResults.value = data || []
      // The scan used to fill a modal that nothing ever opened, so clicking
      // Discover appeared to do nothing at all.
      showDiscoverModal.value = true
      if (!discoverResults.value.length) {
        // "0 found" and "nothing answered" look the same to the user unless the
        // scan says so, so an empty result gets an explicit notice.
        message.info(t('deviceList.discoverEmpty'))
      }
    } catch (e) {
      message.error(extractError(e))
    } finally {
      discovering.value = false
    }
  }

  // Only the Modbus TCP driver sweeps unit IDs; asking another protocol for that
  // number would be a control that changes nothing.
  function supportsUnitSweep(protocol: string) {
    return protocol === 'modbus_tcp'
  }

  // The dialog keeps one port box for every protocol, so switching protocol has to
  // move the number: scanning an OPC UA server on 502 finds nothing at all.
  const discoverDefaultPorts: Record<string, number> = {
    modbus_tcp: 502,
    opc_ua: 4840,
    onvif: 80,
  }
  function handleDiscoverProtocolChange(protocol: string) {
    const fallback = discoverDefaultPorts[protocol]
    if (fallback) discoverPort.value = fallback
    discoverResults.value = []
    selectedDiscoverKeys.value = []
  }

  async function handleAddDiscovered() {
    if (!selectedDiscoverKeys.value.length) return
    addingDevices.value = true
    try {
      const selected = discoverResults.value.filter(r => selectedDiscoverKeys.value.includes(discoverKey(r)))
      await Promise.all(selected.map(s => deviceApi.create({
        device_id: s.device_id || `discovered-${s.ip}-${s.port}${s.unit_id != null ? `-${s.unit_id}` : ''}`,
        name: s.name || `Device ${s.ip}:${s.port}`,
        protocol: s.protocol || 'modbus_tcp',
        // slave_id and endpoint come from the scan result: without them every
        // discovered Modbus unit became one device reading unit 1, and an OPC UA
        // device was created with no URL at all.
        config: {
          host: s.ip,
          port: s.port,
          ...(s.unit_id != null ? { slave_id: s.unit_id } : {}),
          ...(s.endpoint ? { endpoint: s.endpoint } : {}),
        },
        collect_interval: 5,
        points: [],
      })))
      message.success(t('deviceList.addDiscoveredSuccess'))
      showDiscoverModal.value = false
      selectedDiscoverKeys.value = []
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      addingDevices.value = false
    }
  }

  // ─── Connection test ───
  async function handleTestConnection(deviceId?: string) {
    const protocol = createForm.protocol
    // The form's live values live inside GenericProtocolForm's private state and
    // only merge into createForm.config at submit time. Test the typed config.
    const live = protocolFormRef.value?.getAssembledConfig?.()
    const config = live && Object.keys(live).length ? { ...(createForm.config || {}), ...live } : (createForm.config || {})
    if (!protocol) return
    testingConnection.value = true
    connectionTestResult.value = null
    try {
      const result = await deviceApi.testConnection(protocol, config)
      // The backend used to answer success:true for every request, so `ok` cannot
      // trust its message text: build the message from the flags, in the UI's own
      // language. An unreachable port now reads as a failure here.
      const supported = result?.supported !== false
      const ok = supported && result?.success === true
      // The gateway answers failures with an ERR_* code in `message`. "No address
      // could be resolved" is the form not being filled in, which is a different
      // instruction to an operator than "the device did not answer".
      const code = typeof result?.message === 'string' && result.message.startsWith('ERR_') ? result.message : ''
      connectionTestResult.value = {
        ok,
        success: ok,
        supported,
        // Which socket was dialled, so a wrong port never reads as a tested device.
        address: typeof result?.host === 'string' && result.host ? `${result.host}:${result.port}` : undefined,
        message: !supported
          ? t('deviceList.testConnectionUnsupported')
          : ok
            ? t('deviceList.testConnectionSuccess')
            : code === 'ERR_DEVICE_TEST_NO_ADDRESS'
              ? t('deviceList.testConnectionNoAddress')
              : t('deviceList.testConnectionFailed'),
      }
    } catch (e) {
      connectionTestResult.value = { ok: false, message: extractError(e) }
    } finally {
      testingConnection.value = false
    }
  }

  // ─── Import/Export ───
  async function handleExport() {
    try {
      const resp = await http.post('/devices/export', {}, { responseType: 'blob' })
      const text = await (resp.data as Blob).text()
      let exportData = text
      try {
        const parsed = JSON.parse(text)
        if (parsed && Array.isArray(parsed.data)) exportData = JSON.stringify(parsed.data, null, 2)
      } catch { /* not JSON — use raw blob text */ }
      const blob = new Blob([exportData], { type: 'application/json' })
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = 'devices_export.json'
      a.click()
      URL.revokeObjectURL(url)
    } catch (e) {
      message.error(extractError(e))
    }
  }

  async function handleImportFileChange({ file }: { file: any }) {
    importing.value = true
    importProgress.value = 0
    importPreview.value = []
    importErrors.value = []
    try {
      const nativeFile: File | undefined = file.file
      if (!nativeFile) throw new Error('No file selected')
      const text = await nativeFile.text()
      const data = JSON.parse(text)
      if (!Array.isArray(data)) throw new Error('Invalid import format')
      importPreview.value = data.map((d: any) => ({ ...d, status: 'pending' }))
      importProgress.value = 50
    } catch (e: any) {
      importErrors.value.push({ error: e.message || String(e) })
      message.error(t('deviceList.importParseError'))
    } finally {
      importing.value = false
      importProgress.value = 100
    }
  }

  async function handleImportConfirm() {
    if (!importPreview.value.length) return
    importing.value = true
    importProgress.value = 0
    const total = importPreview.value.length
    try {
      const result = await deviceApi.batchImport(importPreview.value, false, importAtomicMode.value)
      const success = result?.imported ?? result?.success ?? 0
      const failed = result?.failed || 0
      importProgress.value = 100
      importing.value = false
      if (failed === 0) {
        message.success(t('deviceList.importSuccess', { count: success }))
        showImportModal.value = false
        await fetchDevices()
      } else {
        message.warning(t('deviceList.importPartial', { success, failed }))
        if (success > 0) await fetchDevices()
      }
    } catch (e) {
      importing.value = false
      message.error(extractError(e))
    }
  }

  async function downloadImportTemplate() {
    const template = [{ device_id: 'demo-001', name: 'Demo Device', protocol: 'modbus_tcp', config: { host: '192.168.1.1', port: 502, slave_id: 1 }, collect_interval: 5, points: [{ name: 'temperature', data_type: 'float32', unit: 'C', address: '40001', access_mode: 'rw' }] }]
    const blob = new Blob([JSON.stringify(template, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'device_import_template.json'
    a.click()
    URL.revokeObjectURL(url)
  }

  // ─── Simulator creation ───
  async function handleCreateSim() {
    try {
      await simFormRef.value?.validate()
    } catch { return }
    creating.value = true
    try {
      await deviceApi.create({ ...simForm } as DeviceCreateParams)
      message.success(t('common.createSuccess'))
      showSimModal.value = false
      simForm.device_id = ''
      simForm.name = ''
      simForm.points = []
      await fetchDevices()
    } catch (e) {
      message.error(extractError(e))
    } finally {
      creating.value = false
    }
  }

  // ─── Clone device ───
  function handleCloneDevice(row: Device) {
    createForm.device_id = row.device_id + '_clone'
    createForm.name = row.name + '_copy'
    createForm.protocol = row.protocol
    createForm.config = { ...row.config }
    createForm.collect_interval = row.collect_interval
    createForm.points = (row.points || []).map(p => ({ ...p }))
    showCreateModal.value = true
  }

  // ─── Share / Transfer ───
  function openShare() { shareForm.device_ids = [...checkedKeys.value]; showShareModal.value = true }
  async function handleShare() {
    try { await shareFormRef.value?.validate() } catch { return }
    sharing.value = true
    try {
      await Promise.all(shareForm.device_ids.map(id =>
        http.post('/resource-shares', {
          resource_type: 'device',
          resource_id: id,
          target_gateway_id: shareForm.target_gateway_id,
          permissions: shareForm.permissions,
          expires_hours: shareForm.expires_hours,
        })
      ))
      message.success(t('deviceList.shareSuccess'))
      showShareModal.value = false
      checkedKeys.value = []
    } catch (e) { message.error(extractError(e)) }
    finally { sharing.value = false }
  }

  async function openTransfer() {
    transferForm.device_ids = [...checkedKeys.value]
    showTransferModal.value = true
    try {
      const resp = await userApi.list({ size: 200 })
      const users = resp?.data ?? []
      userOptions.value = users.map((u: any) => ({ label: u.username, value: u.user_id }))
    } catch { /* ignore — dropdown stays empty */ }
  }
  async function handleTransfer() {
    try { await transferFormRef.value?.validate() } catch { return }
    transferring.value = true
    try {
      const res = await http.post('/resource-shares/transfer', { resource_type: 'device', resource_ids: transferForm.device_ids, target_user_id: transferForm.target_user_id })
      // The endpoint now reports per-resource truth: a device can be missing or
      // of an untransferable kind, and "转移成功" for that would be a lie.
      const result: any = (res as any)?.data?.data
      const moved = Array.isArray(result?.transferred) ? result.transferred.length : 0
      const failedCount = Array.isArray(result?.failed) ? result.failed.length : 0
      if (failedCount > 0) {
        message.warning(t('deviceList.transferPartial', { success: moved, failed: failedCount }))
      } else {
        message.success(t('deviceList.transferSuccess'))
      }
      showTransferModal.value = false
      checkedKeys.value = []
      await fetchDevices()
    } catch (e) { message.error(extractError(e)) }
    finally { transferring.value = false }
  }

  // ─── Draft management ───
  const DRAFT_KEY = 'edgelite_device_create_draft'
  function scheduleDraftSave() {
    const draft = { ...createForm, savedAt: Date.now() }
    try { sessionStorage.setItem(DRAFT_KEY, JSON.stringify(draft)) } catch { /* ignore */ }
  }
  function loadCreateDraft(): any | null {
    try {
      const raw = sessionStorage.getItem(DRAFT_KEY)
      return raw ? JSON.parse(raw) : null
    } catch { return null }
  }
  function clearCreateDraft() {
    try { sessionStorage.removeItem(DRAFT_KEY) } catch { /* ignore */ }
  }
  function resetCreateForm() {
    createForm.device_id = ''
    createForm.name = ''
    createForm.protocol = ''
    createForm.config = {}
    createForm.collect_interval = 5
    createForm.points = []
    createForm.tags = []
  }

  // Auto-save draft when creating
  watch(showCreateModal, (v) => { if (v) scheduleDraftSave() })
  watch(createForm, () => { if (showCreateModal.value) scheduleDraftSave() }, { deep: true })

  // ─── WebSocket ───
  const DEVICE_WS_TYPES = new Set(['device_online', 'device_offline', 'device_error', 'device_created', 'device_updated', 'device_deleted'])
  function onWsMessage(data: any) {
    if (DEVICE_WS_TYPES.has(data?.type)) {
      fetchDevices()
    }
  }

  _wsStatusHandler = (status, reason) => {
    wsConnected.value = status === 'connected'
    wsReconnecting.value = status === 'reconnecting'
  }

  // 深度UX: 筛选条件与分页同步到 URL，从详情页返回/刷新/分享链接都能还原列表状态
  const pageNum = computed({ get: () => pagination.page, set: (v: number) => { pagination.page = v } })
  const pageSizeNum = computed({ get: () => pagination.pageSize, set: (v: number) => { pagination.pageSize = v } })
  useUrlState(
    { search: searchText, status: filterStatus, protocol: filterProtocol, collect: collectFilter, tags: selectedTags, page: pageNum, size: pageSizeNum },
    { validate: (name, value) => {
      if (name === 'size') return (pagination.pageSizes || []).includes(value)
      if (name === 'page') return Number.isInteger(value) && value >= 1
      return true
    } },
  )

  const route = useRoute()
  onMounted(async () => {
    await fetchDevices()
    connect('device', onWsMessage)
    if (_wsStatusHandler) onStatus('device', _wsStatusHandler)
    // Opened from the device detail page's "Edit" button via ?edit=<id>.
    const editId = typeof route.query.edit === 'string' ? route.query.edit : ''
    if (editId) {
      const row = devices.value.find(d => d.device_id === editId)
      if (row) handleEdit(row)
    }
    // Opened from the driver page via ?create=1&driver=<protocol>, optionally
    // with host/port/unit/endpoint carried over from a discovery result. The
    // query used to be pushed by DriverConfig but nothing read it, so the
    // "create device" button landed on this list with the dialog still closed.
    if (route.query.create === '1' && typeof route.query.driver === 'string' && route.query.driver) {
      const protocol = route.query.driver
      resetCreateForm()
      onProtocolChange(protocol)
      applyPointTemplates()
      // Write the address under the key this protocol's form actually binds
      // (host / ip / endpoint), otherwise the field renders empty and the
      // operator cannot tell what was prefilled.
      const cfg = PROTOCOL_CONFIGS.value[normalizeProtocolName(protocol)]
      const addrField = cfg?.configFields.find(f => f.key === 'host' || f.key === 'ip' || f.key === 'endpoint')
      const address = (typeof route.query.endpoint === 'string' && route.query.endpoint)
        || (typeof route.query.host === 'string' ? route.query.host : '')
      const port = Number(route.query.port)
      const unit = Number(route.query.unit)
      const config = { ...createForm.config }
      if (addrField && address) config[addrField.key] = address
      if (Number.isFinite(port) && port > 0) config.port = port
      if (Number.isFinite(unit) && unit > 0) config.slave_id = unit
      createForm.config = config
      showCreateModal.value = true
    }
  })

  onUnmounted(() => {
    disconnect('device', onWsMessage)
    if (_wsStatusHandler) offStatus('device', _wsStatusHandler)
  })

  return {
    // Core state
    devices, loading, loadFailed, searchText, filterStatus, filterProtocol, collectFilter,
    wsConnected, wsReconnecting, checkedKeys, pagination, columns, activeFilterCount,
    // Modals
    showCreateModal, showEditModal, showSimModal, showDiscoverModal, showImportModal,
    showDeployModal, showShareModal, showTransferModal,
    // Forms
    createForm, editForm, simForm, shareForm, transferForm,
    // Loading flags
    creating, editSaving, discovering, addingDevices, importing, importProgress,
    deploying, batchCollectLoading, batchDeleteLoading, sharing, transferring,
    collectActionLoading,
    // Connection
    testingConnection, connectionTestResult,
    // Discovery
    discoverHost, discoverPort, discoverProtocol, discoverScanUnits, discoverResults, selectedDiscoverKeys,
    discoverKey, supportsUnitSweep, handleDiscoverProtocolChange,
    importPreview, importErrors, importAtomicMode, importPreviewColumns,
    // Selects
    deployTemplateId, deployTemplateOptions, protocolOptions, statusOptions,
    discoverProtocolOptions, dataTypeOptions, accessModeOptions, simModeOptions, userOptions,
    // Refs
    createFormRef, editFormRef, simFormRef, protocolFormRef, protocolEditRef,
    createProtocolExperimental,
    shareFormRef, transferFormRef,
    // Schemas
    driverSchemas,
    // Rules
    createRules, editRules, simFormRules, shareRules, transferRules, discoverColumns,
    // Methods
    fetchDevices, onProtocolChange, handleCreate, handleEditSubmit, handleEdit, applyPointTemplates, createPointTemplates,
    handleStartCollect, handleStopCollect, handleDeleteDevice,
    handleShare, openShare, handleTransfer, openTransfer,
    handleBatchDelete, handleBatchStartCollect, handleBatchStopCollect, handleBatchDeploy,
    handleDiscover, handleAddDiscovered, handleExport, handleImportFileChange, handleImportConfirm, handleCreateSim,
    downloadImportTemplate, resetFilters, handleTestConnection,
    // Draft
    loadCreateDraft, clearCreateDraft, scheduleDraftSave, resetCreateForm,
    // Tags
    selectedTags, tagOptions, filteredDevicesByTag, getDeviceTags,
    // Clone
    handleCloneDevice,
    // Compare
    showCompareModal, compareDeviceAId, compareDeviceBId, compareLoading,
    compareDeviceA, compareDeviceB, compareDeviceOptions, compareRows, compareDiffCount,
    openCompareModal, handleCompare,
  }
}
