import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import { t } from '@/i18n'
import { message as discreteMessage } from '@/utils/discreteApi'

// Lazy-load real components for previously placeholder routes
const SystemConfig = () => import('@/views/system/SystemConfig.vue')
const DataExport = () => import('@/views/system/DataExport.vue')
const DataImport = () => import('@/views/system/DataImport.vue')
const DbMonitor = () => import('@/views/system/DbMonitor.vue')
const BackupSchedule = () => import('@/views/system/BackupSchedule.vue')
const LogAggregator = () => import('@/views/system/LogAggregator.vue')
const ConfigVersion = () => import('@/views/system/ConfigVersion.vue')
const ScriptEngine = () => import('@/views/system/ScriptEngine.vue')
const Simulation = () => import('@/views/system/Simulation.vue')
const AlarmCorrelation = () => import('@/views/alarm/AlarmCorrelation.vue')
const LargeScreen = () => import('@/views/LargeScreen.vue')
const ProtocolDebug = () => import('@/views/system/ProtocolDebug.vue')
const Metrics = () => import('@/views/system/Metrics.vue')
const SelfTest = () => import('@/views/system/SelfTest.vue')
const ResourceSharing = () => import('@/views/system/ResourceSharing.vue')
const FirmwareSignature = () => import('@/views/system/FirmwareSignature.vue')
const DeviceLinkage = () => import('@/views/system/DeviceLinkage.vue')
const ProfilerView = () => import('@/views/system/ProfilerView.vue')
const DataQuality = () => import('@/views/data/DataQuality.vue')
const DataDownsample = () => import('@/views/data/DataDownsample.vue')
const GenericApiPage = () => import('@/views/GenericApiPage.vue')
const ObservabilityOverview = () => import('@/views/observability/ObservabilityOverview.vue')
const PlatformDashboard = () => import('@/views/system/PlatformDashboard.vue')

function _showPermissionDenied(msg: string) {
  discreteMessage.warning(msg)
}

export function setMessageInstance(_instance: { warning: (msg: string) => void }) {
  // kept for backward compat, no-op
}

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'Login',
      component: () => import('@/views/Login.vue'),
      meta: { requiresAuth: false },
    },
    {
      path: '/',
      component: () => import('@/layouts/MainLayout.vue'),
      meta: { requiresAuth: true },
      children: [
        { path: '', name: 'Dashboard', component: () => import('@/views/Dashboard.vue'), meta: { title: t('router.dashboard') } },
        { path: 'devices', name: 'Devices', component: () => import('@/views/device/DeviceList.vue'), meta: { title: t('router.devices') } },
        { path: 'devices/templates', name: 'DeviceTemplates', component: GenericApiPage, meta: { title: t('router.deviceTemplates'), requiredRole: ['admin', 'operator'], hidden: true, apiPath: '/devices/templates' } },
        { path: 'devices/:id', name: 'DeviceDetail', component: () => import('@/views/device/DeviceDetail.vue'), meta: { title: t('router.deviceDetail') } },
        { path: 'devices/shadow', name: 'DeviceShadow', component: () => import('@/views/device/DeviceShadow.vue'), meta: { title: t('router.deviceShadow'), requiredRole: ['admin', 'operator'], hidden: true } },
        { path: 'rules', name: 'Rules', component: () => import('@/views/rule/RuleList.vue'), meta: { title: t('router.rules') } },
        { path: 'alarms', name: 'Alarms', component: () => import('@/views/alarm/AlarmList.vue'), meta: { title: t('router.alarms') } },
        { path: 'data', name: 'DataQuery', component: () => import('@/views/data/DataQuery.vue'), meta: { title: t('router.dataQuery') } },
        { path: 'report', name: 'Report', component: GenericApiPage, meta: { title: t('router.report'), hidden: true } },
        { path: 'data/quality', name: 'DataQuality', component: DataQuality, meta: { title: t('router.dataQuality') } },
        { path: 'data/quality-monitor', name: 'DataQualityMonitor', component: GenericApiPage, meta: { title: t('router.qualityMonitor'), hidden: true, apiPath: '/data-quality/summary' } },
        { path: 'system', name: 'System', component: () => import('@/views/system/SystemStatus.vue'), meta: { title: t('router.system') } },
        { path: 'system/services', name: 'ServiceOverview', component: () => import('@/views/system/ServiceOverview.vue'), meta: { title: t('router.services'), requiredRole: 'admin' } },
        { path: 'system/drivers', name: 'DriverConfig', component: () => import('@/views/system/DriverConfig.vue'), meta: { title: t('router.drivers'), requiredRole: 'admin' } },
        { path: 'system/platforms', name: 'PlatformConfig', component: () => import('@/views/system/PlatformConfig.vue'), meta: { title: t('router.platforms'), requiredRole: 'admin' } },
        { path: 'system/platforms/dashboard', name: 'PlatformDashboard', component: PlatformDashboard, meta: { title: t('router.platformDashboard'), requiredRole: 'admin', hidden: true } },
        { path: 'system/platforms/tb-monitor', name: 'TbMonitor', component: GenericApiPage, meta: { title: t('router.tbMonitor'), requiredRole: 'admin', hidden: true } },
        { path: 'system/platforms/custom-mqtt/:name', name: 'CustomMqttConfig', component: GenericApiPage, meta: { title: t('router.customMqttConfig'), requiredRole: 'admin', hidden: true } },
        { path: 'system/expressions', name: 'ExpressionConfig', component: () => import('@/views/system/ExpressionConfig.vue'), meta: { title: t('router.expressions'), requiredRole: 'admin' } },
        { path: 'system/preprocess', name: 'PreprocessConfig', component: () => import('@/views/system/PreprocessConfig.vue'), meta: { title: t('router.preprocess'), requiredRole: 'admin' } },
        { path: 'system/audit', name: 'AuditLog', component: () => import('@/views/system/AuditLog.vue'), meta: { title: t('router.audit'), requiredRole: 'admin' } },
        { path: 'system/serial-bridge', name: 'SerialBridge', component: () => import('@/views/system/SerialBridge.vue'), meta: { title: t('router.serialBridge'), requiredRole: 'admin' } },
        { path: 'system/bridge', name: 'BridgeConfig', component: () => import('@/views/system/ProtocolBridge.vue'), meta: { title: t('router.bridgeConfig'), requiredRole: 'admin' } },
        { path: 'system/mqtt-server', name: 'MqttServer', component: () => import('@/views/system/MqttServer.vue'), meta: { title: t('router.mqttServer'), requiredRole: 'admin' } },
        { path: 'system/modbus-slave', name: 'ModbusSlave', component: () => import('@/views/system/ModbusSlave.vue'), meta: { title: t('router.modbusSlave'), requiredRole: 'admin' } },
        { path: 'system/app-update', name: 'AppUpdate', component: () => import('@/views/system/OtaUpdate.vue'), meta: { title: t('router.appUpdate'), requiredRole: 'admin' } },
        { path: 'system/grafana', name: 'GrafanaDashboard', component: () => import('@/views/system/GrafanaDashboard.vue'), meta: { title: t('router.grafana'), requiredRole: 'admin' } },
        { path: 'system/mcp', name: 'McpServer', component: () => import('@/views/system/McpServer.vue'), meta: { title: t('router.mcp'), requiredRole: 'admin' } },
        { path: 'system/ai-model', name: 'AiModel', component: () => import('@/views/system/AiModel.vue'), meta: { title: t('router.aiModel'), requiredRole: 'admin' } },
        { path: 'system/ai-monitor', name: 'AiMonitor', component: GenericApiPage, meta: { title: t('router.aiMonitor'), requiredRole: 'admin', hidden: true, apiPath: '/system/ai-monitor' } },
        { path: 'system/ai-ab-test', name: 'AiAbTest', component: GenericApiPage, meta: { title: t('router.aiAbTest'), requiredRole: 'admin', hidden: true, apiPath: '/ai/ab-test' } },
        { path: 'system/linkage', name: 'DeviceLinkage', component: DeviceLinkage, meta: { title: t('router.linkage'), requiredRole: 'admin' } },
        { path: 'system/profiler', name: 'ProfilerView', component: ProfilerView, meta: { title: t('router.profiler'), requiredRole: 'admin' } },
        { path: 'system/log-aggregator', name: 'LogAggregator', component: LogAggregator, meta: { title: t('router.logAggregator'), requiredRole: 'admin' } },
        { path: 'system/firmware-signature', name: 'FirmwareSignature', component: FirmwareSignature, meta: { title: t('router.firmwareSignature'), requiredRole: 'admin' } },
        { path: 'system/notify', name: 'NotifyConfig', component: () => import('@/views/system/NotifyConfig.vue'), meta: { title: t('router.notify'), requiredRole: 'admin' } },
        { path: 'system/integration', name: 'Integration', component: () => import('@/views/system/Integration.vue'), meta: { title: t('router.integration'), requiredRole: 'admin' } },
        { path: 'system/debug', name: 'ProtocolDebug', component: ProtocolDebug, meta: { title: t('router.debug'), requiredRole: 'admin' } },
        { path: 'system/metrics', name: 'Metrics', component: Metrics, meta: { title: t('metrics.title'), requiredRole: 'admin' } },
        { path: 'system/config-version', name: 'ConfigVersion', component: ConfigVersion, meta: { title: t('router.configVersion'), requiredRole: 'admin' } },
        { path: 'system/self-test', name: 'SelfTest', component: SelfTest, meta: { title: t('router.selfTest'), requiredRole: 'admin' } },
        { path: 'system/data-export', name: 'DataExport', component: DataExport, meta: { title: t('router.dataExport'), requiredRole: 'admin' } },
        { path: 'system/data-import', name: 'DataImport', component: DataImport, meta: { title: t('router.dataImport'), requiredRole: 'admin' } },
        { path: 'system/resource-sharing', name: 'ResourceSharing', component: ResourceSharing, meta: { title: t('router.resourceSharing'), requiredRole: 'admin' } },
        { path: 'data/downsample', name: 'DataDownsample', component: DataDownsample, meta: { title: t('router.dataDownsample'), requiredRole: 'admin' } },
        { path: 'system/db-monitor', name: 'DbMonitor', component: DbMonitor, meta: { title: t('router.dbMonitor'), requiredRole: 'admin' } },
        { path: 'alarms/trend', name: 'AlarmTrend', component: GenericApiPage, meta: { title: t('router.alarmTrend'), requiredRole: ['admin', 'operator'], hidden: true, apiPath: '/alarms/trend' } },
        { path: 'alarms/correlation', name: 'AlarmCorrelation', component: AlarmCorrelation, meta: { title: t('router.alarmCorrelation') || 'Alarm Correlation', requiredRole: ['admin', 'operator'] } },
        { path: 'system/backup-schedule', name: 'BackupSchedule', component: BackupSchedule, meta: { title: t('router.backupSchedule'), requiredRole: 'admin' } },
        { path: 'system/config', name: 'SystemConfig', component: SystemConfig, meta: { title: t('router.systemConfig'), requiredRole: 'admin' } },
        { path: 'observability', name: 'Observability', redirect: '/observability/overview' },
        { path: 'observability/overview', name: 'ObservabilityOverview', component: ObservabilityOverview, meta: { title: t('router.observabilityOverview'), requiredRole: 'admin' } },
        { path: 'observability/rules', name: 'ObservabilityRulesPage', component: GenericApiPage, meta: { title: t('router.observabilityRules'), requiredRole: 'admin', hidden: true, apiPath: '/observability/rules' } },
        { path: 'observability/events', name: 'ObservabilityEventsPage', component: GenericApiPage, meta: { title: t('router.observabilityEvents'), requiredRole: 'admin', hidden: true, apiPath: '/observability/events' } },
        { path: 'observability/traces', name: 'ObservabilityTraces', component: GenericApiPage, meta: { title: t('router.observabilityTraces'), requiredRole: 'admin', hidden: true, apiPath: '/observability/traces' } },
        { path: 'observability/metrics', name: 'ObservabilityMetrics', component: GenericApiPage, meta: { title: t('router.observabilityMetrics'), requiredRole: 'admin', hidden: true, apiPath: '/observability/metrics' } },
        { path: 'system/scripts', name: 'ScriptEngine', component: ScriptEngine, meta: { title: t('router.scripts'), requiredRole: 'admin' } },
        { path: 'system/simulation', name: 'Simulation', component: Simulation, meta: { title: t('router.simulation'), requiredRole: 'admin' } },
        { path: 'system/anomaly-learner', name: 'AnomalyLearner', component: GenericApiPage, meta: { title: t('router.anomalyLearner'), requiredRole: 'admin', hidden: true, apiPath: '/anomaly-learner/stats' } },
        { path: 'system/trend-learner', name: 'TrendLearner', component: GenericApiPage, meta: { title: t('router.trendLearner'), requiredRole: 'admin', hidden: true, apiPath: '/trend-learner/stats' } },
        { path: 'system/threshold-learner', name: 'ThresholdLearner', component: GenericApiPage, meta: { title: t('router.thresholdLearner'), requiredRole: 'admin', hidden: true, apiPath: '/threshold-learner/stats' } },
        { path: 'system/ai-center', name: 'AiCenter', component: GenericApiPage, meta: { title: t('router.aiCenter'), requiredRole: 'admin', hidden: true } },
        { path: 'system/ai-test', name: 'AiTest', component: GenericApiPage, meta: { title: t('router.aiTest'), requiredRole: 'admin', hidden: true } },
        { path: 'system/calibration', name: 'CalibrationData', component: GenericApiPage, meta: { title: t('router.calibration'), requiredRole: 'admin', hidden: true } },
        { path: 'system/physics-calibrator', name: 'PhysicsCalibrator', component: GenericApiPage, meta: { title: t('router.physCalib'), requiredRole: 'admin', hidden: true } },
        { path: 'system/physics-param-db', name: 'PhysicsParamDb', component: GenericApiPage, meta: { title: t('router.paramDb'), requiredRole: 'admin', hidden: true } },
        { path: 'system/precision-test', name: 'PrecisionTest', component: GenericApiPage, meta: { title: t('router.precTest'), requiredRole: 'admin', hidden: true } },
        { path: 'system/evolution-verify', name: 'EvolutionVerify', component: GenericApiPage, meta: { title: t('router.evoVerify'), requiredRole: 'admin', hidden: true } },
        { path: 'system/boundary-test', name: 'AiBoundaryTest', component: GenericApiPage, meta: { title: t('router.bndTest'), requiredRole: 'admin', hidden: true } },
        { path: 'system/stress-test', name: 'AiStressTest', component: GenericApiPage, meta: { title: t('router.stressTest'), requiredRole: 'admin', hidden: true } },
        { path: 'system/ai-report', name: 'AiReportCenter', component: GenericApiPage, meta: { title: t('router.aiRpt'), requiredRole: 'admin', hidden: true } },
        { path: 'modbus-ops', name: 'ModbusOps', component: GenericApiPage, meta: { title: t('router.modbusOps'), requiredRole: ['admin', 'operator'], hidden: true } },
        { path: 'users', name: 'Users', component: () => import('@/views/system/UserManage.vue'), meta: { title: t('router.users'), requiredRole: 'admin' } },
        { path: 'digital-twin', name: 'DigitalTwin', component: () => import('@/views/digital-twin/DigitalTwin.vue'), meta: { title: t('router.digitalTwin') } },
        { path: 'scada', name: 'ScadaEditor', component: () => import('@/views/scada/ScadaEditor.vue'), meta: { title: t('router.scada'), requiredRole: ['admin', 'operator'] } },
      ],
    },
    { path: '/dashboard/large-screen', name: 'LargeScreen', component: LargeScreen, meta: { requiresAuth: true } },
    { path: '/:pathMatch(.*)*', name: 'NotFound', component: () => import('@/views/NotFound.vue'), meta: { requiresAuth: false } },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()

  // 未认证用户：跳转登录页
  if (!auth.isAuthenticated) {
    if (to.meta.requiresAuth !== false && to.name !== 'Login') {
      return { name: 'Login', query: { redirect: to.fullPath } }
    }
    return
  }

  // 强制改密检查：mustChangePassword 时只允许访问登录页
  if (auth.mustChangePassword && to.name !== 'Login') {
    return { name: 'Login' }
  }

  // 已认证但缺少角色信息：尝试获取
  if (!auth.role) {
    try {
      await auth.fetchUserInfo()
    } catch (e: any) {
      // 只在 401（认证失败）时登出，其他错误（503/网络不可达）允许停留在当前页
      if (e?.response?.status === 401 && !auth.role) {
        auth.logout()
        return { name: 'Login', query: { redirect: to.fullPath } }
      }
      // 非 401 错误：如果仍无 role，允许用户继续（可能后端暂时不可达）
      // 不强制 logout，避免后端短暂抖动导致用户被踢
    }
  }

  // 需要认证但未认证（防御性检查）
  if (to.meta.requiresAuth !== false && !auth.isAuthenticated) {
    return { name: 'Login', query: { redirect: to.fullPath } }
  }

  // FIXED-P2: 支持requiredRole为数组，精确控制operator/viewer可访问的路由
  // 之前：仅检查requiredRole==='admin'，operator和viewer无法区分
  // 之后：requiredRole支持string|string[]，admin始终放行，其他角色需在数组中
  if (to.meta.requiredRole) {
    const required = to.meta.requiredRole as string | string[]
    if (auth.role === 'admin') { /* admin拥有所有权限 */ }
    else {
      const allowedRoles = Array.isArray(required) ? required : [required]
      if (!allowedRoles.includes(auth.role || '')) {
        _showPermissionDenied(t('common.permissionDenied'))
        return { name: 'Dashboard' }
      }
    }
  }
})

// [AUDIT-FIX] 致命级-路由错误处理：动态 import 失败（chunk 加载失败）时自动整页刷新
// 场景：发版后旧 hash chunk 失效、CDN 异常、断网恢复时首次切换路由
// 不刷新会停留在白屏，用户无法恢复
const CHUNK_RELOAD_MARK = 'edgelite_chunk_reload'

router.onError((error, to) => {
  // 仅处理 chunk 动态加载失败
  const msg = (error?.message || '') + ' ' + (error?.name || '')
  const isChunkError = /Failed to fetch dynamically imported module|Importing a module script failed|Loading chunk \S+ failed|Loading CSS chunk \S+ failed|chunk \S+ not found/i.test(msg)
  if (!isChunkError) {
    console.error('[Router Error]', error)
    return
  }
  console.error('[Router Chunk Load Error]', error)
  const target = to?.fullPath || window.location.pathname
  // 刷新只对"客户端还拿着旧 hash"这一种情况有效。若服务端持续返回失败
  // （dev server 编译报错、部署不完整），整页刷新会重新导入同一个坏 chunk
  // 并无限循环，用户看到的是不断闪屏而不是错误信息。
  let mark: { target: string; at: number } | null = null
  try {
    const raw = sessionStorage.getItem(CHUNK_RELOAD_MARK)
    mark = raw ? JSON.parse(raw) : null
  } catch { mark = null }
  const now = Date.now()
  if (mark && mark.target === target && now - mark.at < 15000) {
    console.error('[Router Chunk Load Error] 已为该路由刷新过一次，仍是失败：停止重载以避免死循环', target)
    discreteMessage.warning(t('router.chunkLoadFailed'))
    return
  }
  try { sessionStorage.setItem(CHUNK_RELOAD_MARK, JSON.stringify({ target, at: now })) } catch { /* 隐私模式下写入失败仍继续刷新 */ }
  // 使用 location.href 触发整页加载，避免 SPA 路由再次复用失败的 chunk
  window.location.href = target
})

export default router
