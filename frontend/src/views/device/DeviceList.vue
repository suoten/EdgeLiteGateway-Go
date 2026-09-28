<template>
  <n-space vertical :size="16">
    <!-- Row 1: Search & Filters -->
    <div class="device-toolbar-row">
      <n-space>
        <!-- FIXED-UEDebounce: 搜索无防抖，每次按键触发 fetchDevices()，造成大量无效请求与列表闪烁。
             现引入 300ms 防抖：输入停顿后才请求；回车立即触发。 -->
        <n-input
          v-model:value="searchText"
          :placeholder="t('deviceList.searchPlaceholder')"
          clearable
          style="width:200px"
          @update:value="onSearchInput"
          @keyup.enter="onSearchEnter"
        />
        <n-select v-model:value="filterStatus" :options="statusOptions" :placeholder="t('deviceList.filterStatus')" clearable style="width:120px" @update:value="()=>{collectFilter='all';pagination.page=1;fetchDevices()}" />
        <n-select v-model:value="filterProtocol" :options="protocolOptions" :placeholder="t('deviceList.filterProtocol')" clearable style="width:140px" @update:value="()=>{pagination.page=1;fetchDevices()}" />
        <!-- 修复3: 标签筛选器（多选，options 从所有设备 tags 去重生成） -->
        <n-select
          v-model:value="selectedTags"
          :options="tagOptions"
          multiple
          :placeholder="t('deviceList.filterTag')"
          clearable
          size="small"
          style="width:200px"
          tag
        />
        <n-button-group size="small">
          <n-button :type="collectFilter==='all'?'primary':'default'" @click="collectFilter='all';filterStatus=null;pagination.page=1;fetchDevices()">{{ t('deviceList.all') }}</n-button>
          <n-button :type="collectFilter==='collecting'?'primary':'default'" @click="collectFilter='collecting';filterStatus=null;pagination.page=1;fetchDevices()">{{ t('deviceList.collecting') }}</n-button>
          <n-button :type="collectFilter==='stopped'?'primary':'default'" @click="collectFilter='stopped';filterStatus=null;pagination.page=1;fetchDevices()">{{ t('deviceList.stopped') }}</n-button>
        </n-button-group>
        <!-- OPT-UX: 仅在存在生效筛选时显示「重置筛选」+ 徽标，避免常驻噪音 -->
        <n-button v-if="activeFilterCount" size="small" quaternary type="error" @click="resetFilters">
          {{ t('deviceList.resetFilter') }}
          <n-badge :value="activeFilterCount" :max="9" style="margin-left:4px" />
        </n-button>
        <n-tag :type="wsConnected?'success':wsReconnecting?'warning':'error'" size="small" round>{{ wsConnected?t('deviceList.wsConnected'):wsReconnecting?t('deviceList.wsReconnecting'):t('deviceList.wsDisconnected') }}</n-tag>
      </n-space>
      <n-space>
        <n-button-group size="small">
          <n-button :type="viewMode==='table'?'primary':'default'" @click="viewMode='table'">{{ t('deviceList.tableView') }}</n-button>
          <n-button :type="viewMode==='card'?'primary':'default'" @click="viewMode='card'">{{ t('deviceList.cardView') }}</n-button>
        </n-button-group>
      </n-space>
    </div>

    <!-- Row 2: Action Buttons (创建类操作；批量操作改为浮动栏，选中时才出现) -->
    <div class="device-toolbar-row">
      <n-space>
        <n-button type="primary" size="small" :disabled="!auth.hasPerm('device:create')" @click="openCreateWithDraft">{{ t('deviceList.create') }}</n-button>
        <n-button size="small" :disabled="!auth.hasPerm('device:create')" @click="showSimModal=true">{{ t('deviceList.createSim') }}</n-button>
        <n-button size="small" :disabled="!auth.hasPerm('device:create')" @click="showImportModal=true">{{ t('deviceList.import') }}</n-button>
        <n-button size="small" :disabled="!auth.hasPerm('device:create')" @click="showDiscoveryInput=true">{{ t('deviceList.discoverDevice') }}</n-button>
        <!-- 修复4: 配置对比 -->
        <n-button size="small" @click="openCompareModal">{{ t('deviceList.configCompare') }}</n-button>
      </n-space>
    </div>

    <!-- Device Discovery (collapsible, hidden by default) -->
    <n-collapse-transition :show="showDiscoveryInput">
      <n-card size="small" style="margin-bottom: 12px">
        <n-space align="center">
          <n-text strong>{{ t('deviceList.discoverDevice') }}</n-text>
          <n-input v-model:value="discoverHost" size="small" style="width:150px" :placeholder="t('deviceList.discoverHost')" />
          <n-input-number v-model:value="discoverPort" size="small" style="width:110px" :min="1" :max="65535" :placeholder="t('deviceList.port')" />
          <n-select v-model:value="discoverProtocol" :options="discoverProtocolOptions" size="small" style="width:150px" @update:value="handleDiscoverProtocolChange" />
          <!-- Modbus TCP only: one port can front a whole serial bus, and asking all
               247 addresses is the operator's call, not a default. -->
          <template v-if="supportsUnitSweep(discoverProtocol)">
            <n-input-number v-model:value="discoverScanUnits" size="small" style="width:130px" :min="0" :max="247" :placeholder="t('deviceList.discoverScanUnits')" />
            <n-text depth="3" style="font-size:12px">{{ t('deviceList.discoverScanUnitsHint') }}</n-text>
          </template>
          <n-button size="small" type="primary" :loading="discovering" @click="handleDiscover">{{ t('deviceList.discover') }}</n-button>
          <n-button size="small" @click="showDiscoveryInput=false">{{ t('common.cancel') }}</n-button>
        </n-space>
      </n-card>
    </n-collapse-transition>

    <!-- 修复25: 骨架屏加载 -->
    <template v-if="loading && !filteredDevicesByTag.length">
      <n-card v-for="i in 5" :key="i" size="small" style="margin-bottom: 8px">
        <n-skeleton text :repeat="2" />
      </n-card>
    </template>
    <n-data-table v-else-if="viewMode === 'table'" :columns="columns" :data="filteredDevicesByTag" :loading="loading" :pagination="pagination" :row-key="(r:any)=>r.device_id" :row-props="rowProps" v-model:checked-row-keys="checkedKeys" remote :max-height="600" :scroll-x="860">
      <template #empty>
        <!-- FIXED-UEEmpty: 空状态仅显示"暂无数据"，无引导动作，新用户不知如何开始。
             现提供分场景空状态：加载失败 vs 搜索无结果 vs 首次进入无设备。 -->
        <LoadEmptyState v-if="loadFailed" :description="t('deviceList.emptyDesc')" failed @retry="fetchDevices" />
        <div v-else class="dl-empty-state">
          <n-icon :component="HardwareChipOutline" :size="48" depth="3" />
          <p class="dl-empty-title">
            {{ hasListFilters ? t('deviceList.emptySearchTitle')
                : auth.hasPerm('device:create') ? t('deviceList.emptyTitle')
                : t('deviceList.emptyOwnedTitle') }}
          </p>
          <p class="dl-empty-desc">
            {{ hasListFilters ? t('deviceList.emptySearchDesc')
                : auth.hasPerm('device:create') ? t('deviceList.emptyDesc')
                : t('deviceList.emptyOwnedDesc') }}
          </p>
          <n-space v-if="!hasListFilters && auth.hasPerm('device:create')" justify="center">
            <n-button type="primary" size="small" @click="openCreateWithDraft">{{ t('deviceList.create') }}</n-button>
            <n-button size="small" @click="showImportModal=true">{{ t('deviceList.import') }}</n-button>
          </n-space>
        </div>
      </template>
    </n-data-table>

    <!-- Card View -->
    <div v-if="viewMode === 'card' && filteredDevicesByTag.length" class="device-card-grid">
      <n-card v-for="device in filteredDevicesByTag" :key="device.device_id" size="small" hoverable class="device-card" @click="openDevice(device.device_id)">
        <template #header>
          <n-space align="center" :size="8">
            <span class="dl-status-dot" :class="device.status === 'online' ? 'dl-dot-online' : device.status === 'error' ? 'dl-dot-error' : 'dl-dot-offline'" />
            <span>{{ device.name }}</span>
          </n-space>
        </template>
        <template #header-extra>
          <n-tag :type="deviceStatusColor[device.status] || 'default'" size="small">{{ deviceStatusLabel[device.status] || device.status }}</n-tag>
        </template>
        <n-descriptions label-placement="left" :column="1" size="small">
          <n-descriptions-item :label="t('deviceList.deviceId')">{{ device.device_id }}</n-descriptions-item>
          <n-descriptions-item :label="t('deviceList.protocol')">{{ protocolLabel[device.protocol] || device.protocol }}</n-descriptions-item>
          <n-descriptions-item :label="t('deviceList.collectInterval')">{{ device.collect_interval }}s</n-descriptions-item>
        </n-descriptions>
      </n-card>
    </div>
    <!-- FIXED-UEEmpty: 卡片视图同样需要空状态（含加载失败态） -->
    <LoadEmptyState
      v-if="viewMode === 'card' && !filteredDevicesByTag.length && !loading && loadFailed"
      :description="t('deviceList.emptyDesc')"
      failed
      @retry="fetchDevices"
    />
    <div v-else-if="viewMode === 'card' && !filteredDevicesByTag.length && !loading" class="dl-empty-state">
      <n-icon :component="HardwareChipOutline" :size="48" depth="3" />
      <p class="dl-empty-title">
        {{ hasListFilters ? t('deviceList.emptySearchTitle')
            : auth.hasPerm('device:create') ? t('deviceList.emptyTitle')
            : t('deviceList.emptyOwnedTitle') }}
      </p>
      <p class="dl-empty-desc">
        {{ hasListFilters ? t('deviceList.emptySearchDesc')
            : auth.hasPerm('device:create') ? t('deviceList.emptyDesc')
            : t('deviceList.emptyOwnedDesc') }}
      </p>
    </div>
    <!-- 卡片视图底部添加分页：与表格视图共用同一 pagination 状态（远程分页） -->
    <div v-if="viewMode === 'card'" style="margin-top: 16px; display: flex; justify-content: center;">
      <n-pagination
        v-model:page="pagination.page"
        :item-count="pagination.itemCount"
        :page-size="pagination.pageSize"
        :page-sizes="pagination.pageSizes"
        :show-size-picker="true"
        @update:page="pagination.onChange"
        @update:page-size="pagination.onUpdatePageSize"
      />
    </div>
    <n-modal v-model:show="showCreateModal" preset="card" :title="t('deviceList.createDevice')" style="width:720px;max-width:95vw;max-height:85vh;overflow-y:auto" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-steps :current="createStep + 1" size="small" style="margin-bottom:16px">
        <n-step :title="t('deviceList.stepBasicInfo')" />
        <n-step :title="t('deviceList.connectionConfig')" />
        <n-step :title="t('deviceList.pointDefinition')" />
      </n-steps>
      <n-form ref="createFormRef" :model="createForm" :rules="createRules" label-placement="left" label-width="auto">
        <div v-show="createStep === 0">
          <n-grid :cols="2" :x-gap="16">
            <n-gi><n-form-item :label="t('deviceList.deviceId')" path="device_id"><n-input v-model:value="createForm.device_id" /></n-form-item></n-gi>
            <n-gi><n-form-item :label="t('deviceList.name')" path="name"><n-input v-model:value="createForm.name" /></n-form-item></n-gi>
            <n-gi><n-form-item :label="t('deviceList.protocol')" path="protocol"><n-select :value="createForm.protocol" :options="protocolOptions" @update:value="onProtocolChange" /></n-form-item></n-gi>
            <n-gi><n-form-item :label="t('deviceList.collectInterval')" path="collect_interval"><n-input-number v-model:value="createForm.collect_interval" :min="1" :max="3600" style="width:100%" /></n-form-item></n-gi>
            <n-gi span="2"><n-form-item :label="t('deviceList.tags')"><n-dynamic-tags v-model:value="createForm.tags" :max="10" /></n-form-item></n-gi>
          </n-grid>
        </div>
        <div v-show="createStep === 1" ref="protocolPanelWrap">
          <n-alert v-if="createProtocolExperimental" type="warning" :bordered="false" style="margin-bottom:12px">
            {{ t('deviceList.experimentalProtocolNote') }}
          </n-alert>
          <component v-if="createForm.protocol" :is="protocolCreateComponent" :protocol="createForm.protocol" :config="createForm.config" :points="createForm.points" mode="create" :driver-schemas="driverSchemas" ref="protocolFormRef" />
          <!-- 修复1: 连通性测试按钮（仅第2步显示） -->
          <div v-if="createStep === 1" style="margin-top:12px;padding-top:12px;border-top:1px dashed var(--n-border-color,#eee)">
            <n-space align="center" :size="8">
              <n-button size="small" :loading="testingConnection" @click="handleTestConnection()">
                {{ t('deviceList.testConnection') }}
              </n-button>
              <n-text depth="3" style="font-size:12px">{{ t('deviceList.testConnectionHint') }}</n-text>
            </n-space>
            <n-alert
              v-if="connectionTestResult"
              :type="connectionTestResult.success ? 'success' : connectionTestResult.supported ? 'error' : 'warning'"
              :show-icon="true"
              style="margin-top:8px"
            >
              {{ connectionTestResult.message }}
              <n-text v-if="connectionTestResult.address" depth="3" style="display:block;font-size:12px;margin-top:2px">
                {{ connectionTestResult.address }}
              </n-text>
            </n-alert>
          </div>
        </div>
        <div v-show="createStep === 2">
          <n-divider style="margin:4px 0 8px;font-size:13px">{{ t('deviceList.pointDefinition') }}</n-divider>
          <n-space v-if="createForm.protocol === 'opc_ua'" align="center" size="small" style="margin-bottom:8px">
            <n-button size="small" secondary type="primary" @click="showNodePicker = true">{{ t('deviceDetail.opcuaBrowseBtn') }}</n-button>
            <n-text depth="3" style="font-size:12px">{{ t('deviceDetail.opcuaBrowseHint') }}</n-text>
          </n-space>
          <n-space v-if="!createForm.points.length" vertical size="small" align="center" style="width:100%">
            <n-text depth="3" style="font-size:12px">{{ t('deviceList.pointPlaceholder') }}</n-text>
            <n-button v-if="createPointTemplates.length" size="small" tertiary type="primary" @click="applyPointTemplates">{{ t('deviceList.applyPointTemplates') }}</n-button>
          </n-space>
          <n-space vertical size="small">
            <n-space v-for="(pt,idx) in createForm.points" :key="idx" align="center">
              <n-input v-model:value="pt.name" :placeholder="t('deviceList.pointName')" size="small" style="width:120px" />
              <n-select v-model:value="pt.data_type" :options="dataTypeOptions" size="small" style="width:110px" />
              <n-input v-model:value="pt.address" :placeholder="t('deviceList.address')" size="small" style="width:120px" />
              <n-input v-model:value="pt.unit" :placeholder="t('deviceList.unit')" size="small" style="width:60px" />
              <n-select v-model:value="pt.access_mode" :options="accessModeOptions" size="small" style="width:90px" />
              <n-button text type="error" @click="createForm.points.splice(idx,1)">{{ t('common.delete') }}</n-button>
            </n-space>
            <n-button dashed block @click="createForm.points.push({name:'',data_type:'float32',unit:'',address:'',access_mode:'r'})">{{ t('deviceList.addPoint') }}</n-button>
          </n-space>
        </div>
      </n-form>
      <template #action>
        <n-button @click="showCreateModal=false">{{ t('common.cancel') }}</n-button>
        <n-button v-if="createStep > 0" @click="createStep--">{{ t('deviceList.stepPrev') }}</n-button>
        <n-button v-if="createStep < 2" type="primary" @click="nextCreateStep">{{ t('deviceList.stepNext') }}</n-button>
        <n-button v-if="createStep === 2" type="primary" :loading="creating" @click="onCreateClick">{{ t('deviceList.create') }}</n-button>
      </template>
    </n-modal>
    <opcua-node-picker v-model:show="showNodePicker" :config="browseConfig" @pick="addPointFromNode" />
    <n-modal v-model:show="showEditModal" preset="card" :title="t('deviceList.editDevice')" style="width:720px;max-width:95vw;max-height:85vh;overflow-y:auto" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-form ref="editFormRef" :model="editForm" :rules="editRules" label-placement="left" label-width="auto">
        <n-form-item :label="t('deviceList.deviceId')"><n-input :value="editForm.device_id" disabled /></n-form-item>
        <n-form-item :label="t('deviceList.name')" path="name"><n-input v-model:value="editForm.name" /></n-form-item>
        <n-form-item :label="t('deviceList.collectInterval')" path="collect_interval"><n-input-number v-model:value="editForm.collect_interval" :min="1" :max="3600" style="width:100%" /></n-form-item>
        <n-form-item :label="t('deviceList.tags')"><n-dynamic-tags v-model:value="editForm.tags" :max="10" /></n-form-item>
        <component v-if="editForm.protocol" :is="protocolEditComponent" :protocol="editForm.protocol" :config="editForm.config" :points="editForm.points" mode="edit" :driver-schemas="driverSchemas" :key="editForm.device_id" ref="protocolEditRef" />
      </n-form>
      <template #action><n-button @click="showEditModal=false">{{ t('common.cancel') }}</n-button><n-button type="primary" :loading="editSaving" @click="onEditClick">{{ t('common.save') }}</n-button></template>
    </n-modal>
    <n-modal v-model:show="showSimModal" preset="card" :title="t('deviceList.createSim')" style="width:640px;max-width:95vw;max-height:85vh;overflow-y:auto" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-form ref="simFormRef" :model="simForm" :rules="simFormRules" label-placement="left" label-width="auto">
        <n-form-item :label="t('deviceList.deviceId')" path="device_id"><n-input v-model:value="simForm.device_id" /></n-form-item>
        <n-form-item :label="t('deviceList.name')" path="name"><n-input v-model:value="simForm.name" /></n-form-item>
        <n-form-item :label="t('deviceList.collectInterval')"><n-input-number v-model:value="simForm.collect_interval" :min="1" style="width:100%" /></n-form-item>
        <n-divider style="margin:4px 0 8px;font-size:13px">{{ t('deviceList.pointDefinition') }}</n-divider>
        <n-space vertical size="small">
          <n-space v-for="(pt,idx) in simForm.points" :key="idx" align="center"><n-input v-model:value="pt.name" :placeholder="t('deviceList.pointName')" size="small" style="width:100px" /><n-select v-model:value="pt.data_type" :options="dataTypeOptions" size="small" style="width:100px" /><n-input v-model:value="pt.unit" :placeholder="t('deviceList.unit')" size="small" style="width:50px" /><n-input-number v-model:value="pt.min" size="small" style="width:70px" :placeholder="t('deviceList.min')" /><n-input-number v-model:value="pt.max" size="small" style="width:70px" :placeholder="t('deviceList.max')" /><n-select v-model:value="pt.mode" :options="simModeOptions" size="small" style="width:100px" /><n-button text type="error" @click="simForm.points.splice(idx,1)">{{ t('common.delete') }}</n-button></n-space>
          <n-button dashed block @click="simForm.points.push({name:'',data_type:'float32',unit:'',address:'0',access_mode:'r',min:0,max:100,mode:'sine'})">{{ t('deviceList.addPoint') }}</n-button>
        </n-space>
      </n-form>
      <template #action><n-button @click="showSimModal=false">{{ t('common.cancel') }}</n-button><n-button type="primary" :loading="creating" @click="handleCreateSim">{{ t('deviceList.create') }}</n-button></template>
    </n-modal>
    <n-modal v-model:show="showDiscoverModal" preset="card" :title="t('deviceList.discoverResult')" style="width:700px;max-width:95vw" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-data-table :columns="discoverColumns" :data="discoverResults" :loading="discovering" :row-key="discoverKey" v-model:checked-row-keys="selectedDiscoverKeys" :scroll-x="700" />
      <template #action><n-button @click="showDiscoverModal=false">{{ t('common.cancel') }}</n-button><n-button type="primary" :loading="addingDevices" :disabled="!selectedDiscoverKeys.length" @click="handleAddDiscovered">{{ t('deviceList.addSelected') }}</n-button></template>
    </n-modal>
    <n-modal v-model:show="showImportModal" preset="card" :title="t('deviceList.importDevices')" style="width:700px;max-width:95vw" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-alert type="info" :bordered="false" style="margin-bottom:12px">{{ t('deviceList.importTemplateHint') }}</n-alert>
      <n-space style="margin-bottom:12px">
        <n-button size="small" @click="downloadImportTemplate">{{ t('deviceList.downloadTemplate') }}</n-button>
        <n-upload :max="1" accept=".json,.xlsx,.xls" :show-file-list="false" @change="handleImportFileChange"><n-button>{{ t('deviceList.selectFile') }}</n-button></n-upload>
      </n-space>
      <n-alert v-if="importErrors.length" type="error" :show-icon="false" style="margin-top:8px"><ul style="margin:0;padding-left:16px"><li v-for="(e, i) in importErrors" :key="i + '_' + (e.error || e)">{{ e.error || e }}</li></ul></n-alert>
      <n-data-table v-if="importPreview.length" :columns="importPreviewColumns" :data="importPreview" :row-key="(r:any)=>r.device_id" style="margin-top:8px" />
      <n-progress v-if="importing && !importAtomicMode && importProgress > 0" type="line" :percentage="importProgress" indicator-placement="inside" style="margin-top:12px" />
      <!-- FIXED-ATOMIC-IMPORT: 事务模式选项 -->
      <n-checkbox v-model:checked="importAtomicMode" style="margin-top:12px">
        {{ t('deviceList.atomicMode') }}
      </n-checkbox>
      <n-text depth="3" style="font-size:12px;margin-left:20px">
        {{ importAtomicMode ? t('deviceList.atomicModeHint') : t('deviceList.partialModeHint') }}
      </n-text>
      <template #action><n-button @click="showImportModal=false">{{ t('common.cancel') }}</n-button><n-button type="primary" :loading="importing" :disabled="!importPreview.length" @click="handleImportConfirm">{{ t('deviceList.importConfirm') }}</n-button></template>
    </n-modal>
    <n-modal v-model:show="showDeployModal" preset="card" :title="t('deviceList.batchDeploy')" style="width:500px;max-width:95vw" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-form :model="deployForm" ref="deployFormRef" :rules="deployRules" label-placement="left" label-width="auto"><n-form-item :label="t('deviceList.deployTemplate')" path="templateId"><n-select v-model:value="deployForm.templateId" :options="deployTemplateOptions" :placeholder="t('deviceList.selectTemplate')" /></n-form-item></n-form>
      <template #action><n-button @click="showDeployModal=false">{{ t('common.cancel') }}</n-button><n-button type="primary" :loading="deploying" :disabled="!deployForm.templateId" @click="handleBatchDeployWithValidation">{{ t('deviceList.deploy') }}</n-button></template>
    </n-modal>
    <!-- 修复4: 设备配置对比弹窗 -->
    <n-modal v-model:show="showCompareModal" preset="card" :title="t('deviceList.configCompareTitle')" style="width:860px;max-width:95vw;max-height:85vh;overflow-y:auto" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-space align="center" style="margin-bottom:12px">
        <n-select v-model:value="compareDeviceAId" :options="compareDeviceOptions" :placeholder="t('deviceList.selectDeviceA')" filterable style="width:280px" />
        <span style="font-weight:600">VS</span>
        <n-select v-model:value="compareDeviceBId" :options="compareDeviceOptions" :placeholder="t('deviceList.selectDeviceB')" filterable style="width:280px" />
        <n-button type="primary" :loading="compareLoading" @click="handleCompare">{{ t('deviceList.compare') }}</n-button>
      </n-space>
      <n-alert v-if="compareDeviceA && compareDeviceB" :type="compareDiffCount === 0 ? 'success' : 'warning'" :show-icon="true" style="margin-bottom:8px">
        {{ compareDiffCount === 0 ? t('deviceList.compareIdentical') : t('deviceList.compareDiffCount', { count: compareDiffCount }) }}
      </n-alert>
      <n-data-table
        v-if="compareRows.length"
        :columns="[
          { title: t('deviceList.compareField'), key: 'key', width: 200 },
          { title: t('deviceList.compareValueA'), key: 'valueA', render: (r:any) => r.diff ? h('span', { style: 'color:#f56c6c;font-weight:600' }, JSON.stringify(r.valueA)) : JSON.stringify(r.valueA) },
          { title: t('deviceList.compareValueB'), key: 'valueB', render: (r:any) => r.diff ? h('span', { style: 'color:#f56c6c;font-weight:600' }, JSON.stringify(r.valueB)) : JSON.stringify(r.valueB) },
        ]"
        :data="compareRows"
        :row-key="(r:any)=>r.key"
        size="small"
        :max-height="500"
      />
      <n-empty v-else style="padding:32px 0" :description="t('deviceList.compareSelectBoth')" />
      <template #action><n-button @click="showCompareModal=false">{{ t('common.cancel') }}</n-button></template>
    </n-modal>
    <!-- Share Modal -->
    <n-modal v-model:show="showShareModal" preset="card" :title="t('resourceShare.shareResource')" style="width:480px;max-width:95vw" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-alert type="info" :bordered="false" style="margin-bottom:16px">{{ t('resourceShare.shareDeviceHint') }}</n-alert>
      <n-form ref="shareFormRef" :model="shareForm" :rules="shareRules" label-placement="left" label-width="100">
        <n-form-item :label="t('resourceShare.targetGateway')" path="target_gateway_id">
          <n-input v-model:value="shareForm.target_gateway_id" :placeholder="t('resourceShare.targetGatewayPlaceholder')" />
        </n-form-item>
        <n-form-item :label="t('resourceShare.permission')">
          <n-select v-model:value="shareForm.permissions" :options="permissionOptions" multiple />
        </n-form-item>
        <n-form-item :label="t('resourceShare.expiresHours')">
          <n-input-number v-model:value="shareForm.expires_hours" :min="1" :max="720" />
        </n-form-item>
      </n-form>
      <template #action>
        <n-button @click="showShareModal=false">{{ t('common.cancel') }}</n-button>
        <n-button type="primary" :loading="sharing" @click="handleShare">{{ t('resourceShare.share') }}</n-button>
      </template>
    </n-modal>
    <!-- Transfer Modal -->
    <n-modal v-model:show="showTransferModal" preset="card" :title="t('resourceShare.transferOwnership')" style="width:480px;max-width:95vw" :close-on-esc="true" :auto-focus="true" :close-on-esc-aria-label="t('common.closeDialog')">
      <n-alert type="warning" :bordered="false" style="margin-bottom:16px">{{ t('resourceShare.transferWarning') }}</n-alert>
      <n-form ref="transferFormRef" :model="transferForm" :rules="transferRules" label-placement="left" label-width="100">
        <n-form-item :label="t('resourceShare.resource')">{{ transferForm.device_name }}</n-form-item>
        <n-form-item :label="t('resourceShare.newOwner')" path="new_owner_id">
          <n-select v-model:value="transferForm.new_owner_id" :options="userOptions" :placeholder="t('resourceShare.selectNewOwner')" filterable />
        </n-form-item>
      </n-form>
      <template #action>
        <n-button @click="showTransferModal=false">{{ t('common.cancel') }}</n-button>
        <n-button type="warning" :loading="transferring" @click="handleTransfer">{{ t('resourceShare.transfer') }}</n-button>
      </template>
    </n-modal>
    <!-- OPT-UX: 浮动批量操作栏，仅在选中行时滑出，替代常驻 disabled 按钮 -->
    <transition name="dl-slide-up">
      <div v-if="checkedKeys.length" class="dl-batch-bar">
        <n-space align="center" :size="8">
          <n-tag type="info" round :bordered="false">{{ t('deviceList.batchBarSelected', { count: checkedKeys.length }) }}</n-tag>
          <n-button size="small" :disabled="!auth.hasPerm('system:config')" @click="showDeployModal=true">{{ t('deviceList.batchDeploy') }}</n-button>
          <n-button type="success" size="small" :disabled="!auth.hasPerm('device:update')" :loading="batchCollectLoading" @click="handleBatchStartCollect">{{ t('deviceList.batchStart') }}</n-button>
          <n-button type="warning" size="small" :disabled="!auth.hasPerm('device:update')" :loading="batchCollectLoading" @click="handleBatchStopCollect">{{ t('deviceList.batchStop') }}</n-button>
          <n-button size="small" :disabled="!auth.hasPerm('device:read')" @click="handleExport">{{ t('deviceList.export') }}</n-button>
          <n-button type="error" size="small" :disabled="!auth.hasPerm('device:delete')" :loading="batchDeleteLoading" @click="handleBatchDelete">{{ t('deviceList.batchDelete') }}</n-button>
        </n-space>
        <n-button text type="primary" @click="checkedKeys=[]">{{ t('deviceList.clearSelection') }}</n-button>
      </div>
    </transition>
  </n-space>
</template>

<script setup lang="ts">
import { computed, ref, reactive, onUnmounted, nextTick, watch, h } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { HardwareChipOutline } from '@vicons/ionicons5'
import { t } from '@/i18n'
import { dialog, message } from '@/utils/discreteApi'
import { extractError } from '@/utils/errorCodes'
import { formatDateTime } from '@/utils/datetime'
import { deviceStatusLabel, deviceStatusColor, protocolLabel } from '@/utils/enumLabels'
import { useAuthStore } from '@/stores/auth'
import { useDeviceList } from './composables/useDeviceList'
import { getProtocolFormComponent } from './protocols/index'
import LoadEmptyState from '@/components/LoadEmptyState.vue'
import OpcuaNodePicker from '@/components/OpcuaNodePicker.vue'
const {
  devices,loading,loadFailed,searchText,filterStatus,filterProtocol,collectFilter,wsConnected,wsReconnecting,checkedKeys,pagination,columns,
  activeFilterCount,
  showCreateModal,showEditModal,showSimModal,showDiscoverModal,showImportModal,showDeployModal,
  showShareModal,showTransferModal,
  createForm,editForm,simForm,shareForm,transferForm,
  creating,editSaving,discovering,addingDevices,importing,importProgress,deploying,batchCollectLoading,batchDeleteLoading,sharing,transferring,
  // 修复1: 连通性测试
  testingConnection, connectionTestResult,
  discoverHost,discoverPort,discoverProtocol,discoverScanUnits,discoverResults,selectedDiscoverKeys,discoverKey,supportsUnitSweep,handleDiscoverProtocolChange,importPreview,importErrors,importAtomicMode,importPreviewColumns,
  deployTemplateId,deployTemplateOptions,protocolOptions,statusOptions,discoverProtocolOptions,dataTypeOptions,accessModeOptions,simModeOptions,userOptions,
  createFormRef,editFormRef,simFormRef,protocolFormRef,protocolEditRef,shareFormRef,transferFormRef,
  createProtocolExperimental,
  driverSchemas,
  createRules,editRules,simFormRules,shareRules,transferRules,discoverColumns,
  fetchDevices,onProtocolChange,handleCreate,handleEditSubmit,handleEdit,applyPointTemplates,createPointTemplates,
  handleShare,openShare,handleTransfer,openTransfer,
  handleBatchDelete,handleBatchStartCollect,handleBatchStopCollect,handleBatchDeploy,
  handleDiscover,handleAddDiscovered,handleExport,handleImportFileChange,handleImportConfirm,handleCreateSim,
  downloadImportTemplate,
  resetFilters,
  // 修复1: 连通性测试
  handleTestConnection,
  // UX-09: 草稿恢复
  loadCreateDraft, clearCreateDraft, scheduleDraftSave, resetCreateForm,
  // 修复3: 设备标签管理
  selectedTags, tagOptions, filteredDevicesByTag, getDeviceTags,
  // 修复3: 设备克隆
  handleCloneDevice,
  // 修复4: 设备配置对比
  showCompareModal, compareDeviceAId, compareDeviceBId, compareLoading,
  compareDeviceA, compareDeviceB, compareDeviceOptions, compareRows, compareDiffCount,
  openCompareModal, handleCompare,
} = useDeviceList()
const router = useRouter()
const route = useRoute()
const auth = useAuthStore()
// Devices are owner-scoped for non-admin roles, so an empty list is not always
// "no devices exist" — the copy has to say which case the user is in.
const hasListFilters = computed(() => !!(searchText.value || filterStatus.value || filterProtocol.value || collectFilter.value !== 'all'))
const viewMode = ref<'table' | 'card'>('table')
const showDiscoveryInput = ref(false)

// OPC UA's Browse service lists the server's real nodes, which is the only way an
// operator can fill an address without being handed the NodeID list by the plant.
// It browses the endpoint as typed in step 1 — the device does not exist yet, and
// needing a save first is what made this feature unusable exactly when it helps.
const showNodePicker = ref(false)
const browseConfig = computed<Record<string, unknown>>(() => {
  // The protocol form holds its edits privately until submit, so createForm.config
  // is still the empty defaults here; ask the form for what the user sees.
  const assembled = protocolFormRef.value?.getAssembledConfig?.()
  return (assembled && Object.keys(assembled).length ? assembled : createForm.config) as Record<string, unknown>
})
function addPointFromNode(p: { name: string; address: string; dataType: string; writable: boolean }) {
  const used = new Set(createForm.points.map((x) => x.name))
  let name = p.name || p.address
  for (let i = 2; used.has(name); i++) name = `${p.name}_${i}`
  const access = p.writable ? 'rw' : 'r'
  // A blank row is the operator having already clicked "add point"; filling it
  // beats appending and leaving an invalid empty row behind.
  const blank: any = createForm.points.find((x) => !x.name && !x.address)
  if (blank) {
    blank.name = name
    blank.address = p.address
    blank.access_mode = access
    if (p.dataType) blank.data_type = p.dataType
    return
  }
  createForm.points.push({ name, data_type: p.dataType || 'float32', unit: '', address: p.address, access_mode: access } as any)
}

// [AUDIT-FIX] 部署模板表单添加 templateId 必填校验
// deployTemplateId 是 composable 返回的独立 ref，此处用本地 reactive 包装以便绑定 :model 与触发校验
const deployFormRef = ref<any>(null)
const deployForm = reactive({ templateId: null as string | null })
const deployRules = computed(() => ({
  templateId: { required: true, type: 'string' as const, message: t('deviceList.selectTemplate'), trigger: ['change', 'blur'] },
}))
watch(showDeployModal, (show) => { if (show) deployForm.templateId = deployTemplateId.value })
async function handleBatchDeployWithValidation() {
  try { await deployFormRef.value?.validate() } catch { return }
  deployTemplateId.value = deployForm.templateId
  handleBatchDeploy()
}

// 创建向导分步
const createStep = ref(0)
const protocolPanelWrap = ref<HTMLElement | null>(null)
watch(showCreateModal, (v) => { if (!v) createStep.value = 0 })

function nextCreateStep() {
  if (createStep.value === 0) {
    if (!createForm.device_id || !createForm.name || !createForm.protocol) {
      message.error(t('deviceList.stepFillRequired'))
      return
    }
  }
  createStep.value = Math.min(2, createStep.value + 1)
  nextTick(() => {
    const wrap = protocolPanelWrap.value
    if (!wrap) return
    const dividers = wrap.querySelectorAll('.n-divider')
    if (createStep.value === 2 && dividers.length >= 2) {
      dividers[dividers.length - 1].scrollIntoView({ behavior: 'smooth', block: 'start' })
    } else if (dividers.length >= 1) {
      dividers[0].scrollIntoView({ behavior: 'smooth', block: 'start' })
    }
  })
}

// UX-09: 打开创建弹窗时检查草稿，提示用户恢复
// 注意：dialog.warning() 返回 DialogReactive 而非 Promise，不能用 await 判断点击结果，
// 必须通过 onPositiveClick/onNegativeClick 回调区分恢复/放弃。
function openCreateWithDraft() {
  const draft = loadCreateDraft()
  if (draft && (draft.device_id || draft.name)) {
    const openModal = () => { showCreateModal.value = true }
    dialog.warning({
      title: t('deviceList.draftFound'),
      content: t('deviceList.draftRestorePrompt', { time: formatDateTime(draft.savedAt) }),
      positiveText: t('deviceList.restoreDraft'),
      negativeText: t('deviceList.discardDraft'),
      onPositiveClick: () => {
        // 恢复草稿的全部字段：config 与 points 直接回填，
        // 否则用户已填写的连接配置/测点会丢失（点测点编辑器已绑定 createForm.points）。
        createForm.device_id = draft.device_id
        createForm.name = draft.name
        createForm.protocol = draft.protocol
        createForm.collect_interval = draft.collect_interval
        createForm.config = draft.config || {}
        createForm.points = Array.isArray(draft.points) ? draft.points : []
        createForm.tags = Array.isArray(draft.tags) ? draft.tags : []
        openModal()
      },
      onNegativeClick: () => {
        clearCreateDraft()
        resetCreateForm()
        openModal()
      },
      onClose: openModal,
      onMaskClick: openModal,
    })
  } else {
    showCreateModal.value = true
  }
}
// OPT-UX: 表格行可点击进入详情；点击按钮/下拉/复选框等可交互元素时不跳转
// 进入详情时带上当前 query，浏览器"后退"即可还原筛选与分页
function openDevice(id: string) {
  router.push({ path: `/devices/${id}`, query: route.query })
}
function rowProps(row: any) {
  const open = () => openDevice(row.device_id)
  return {
    style: 'cursor: pointer',
    tabindex: '0',
    'aria-label': `${row.name || row.device_id}`,
    onClick: (e: MouseEvent) => {
      const target = e.target as HTMLElement
      if (target.closest('.n-button,.n-dropdown,.n-checkbox,.n-input,.n-base-selection')) return
      open()
    },
    onKeydown: (e: KeyboardEvent) => {
      // 只响应行本身的键盘事件，避免内部按钮的 Enter 被重复触发
      if (e.target !== e.currentTarget) return
      if (e.key !== 'Enter' && e.key !== ' ') return
      e.preventDefault()
      open()
    },
  }
}
const protocolCreateComponent = computed(() => getProtocolFormComponent(createForm.protocol))
const protocolEditComponent = computed(() => getProtocolFormComponent(editForm.protocol))
const permissionOptions = computed(() => [
  { label: t('resourceShare.readOnly'), value: 'read' },
  { label: t('resourceShare.readWrite'), value: 'write' },
])
async function onCreateClick() { try { await createFormRef.value?.validate() } catch { return }; try { const d = await protocolFormRef.value?.validate(); if (!d) return; if (!createForm.points.length) { message.error(t('deviceList.pointsRequired')); return } if (createForm.points.some((p: any) => !p.name || !p.address)) { message.error(t('deviceList.pointNameAddrRequired')); return } await handleCreate(d) } catch (e: any) { console.error('[DeviceList] Create validation failed:', e?.message || e); message.error(extractError(e, t('common.operationFailed'))) } }
async function onEditClick() { try { await editFormRef.value?.validate() } catch { return }; try { await protocolEditRef.value?.validate(); const c = protocolEditRef.value?.getAssembledConfig(); const p = protocolEditRef.value?.getAssembledPoints(); if (!c) return; await handleEditSubmit(c, p) } catch (e: any) { console.error('[DeviceList] Edit validation failed:', e?.message || e); message.error(extractError(e, t('common.operationFailed'))) } }

// FIXED-UEDebounce: 搜索防抖逻辑。
// 原实现 @update:value 直接调用 fetchDevices()，输入 "temperature" 会触发 9 次 API 请求，
// 造成列表闪烁、后端压力与带宽浪费。现采用 300ms 防抖：停顿后请求；回车立即请求。
let _searchTimer: ReturnType<typeof setTimeout> | null = null
function _triggerSearch() {
  pagination.page = 1
  fetchDevices()
}
function onSearchInput() {
  if (_searchTimer) clearTimeout(_searchTimer)
  _searchTimer = setTimeout(() => {
    _searchTimer = null
    _triggerSearch()
  }, 300)
}
function onSearchEnter() {
  if (_searchTimer) { clearTimeout(_searchTimer); _searchTimer = null }
  _triggerSearch()
}
onUnmounted(() => { if (_searchTimer) clearTimeout(_searchTimer) })
</script>

<style scoped>
.dl-status-dot{width:8px;height:8px;border-radius:50%;display:inline-block}
.dl-dot-online{background:#52c41a;box-shadow:0 0 6px #52c41a}
.dl-dot-offline{background:#c0c0c0}
.dl-dot-error{background:#ff4d4f;box-shadow:0 0 6px #ff4d4f}
.device-toolbar-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}
.device-card-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 12px;
}
.device-card {
  cursor: pointer;
  transition: transform 0.2s, box-shadow 0.2s;
}
.device-card:hover {
  transform: translateY(-2px);
  box-shadow: 0 4px 12px rgba(0,0,0,0.1);
}
/* FIXED-UEEmpty: 空状态样式，引导新用户开始操作 */
.dl-empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 48px 16px;
  gap: 8px;
  color: var(--n-text-color-3, #999);
}
.dl-empty-title {
  margin: 8px 0 0 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--n-text-color-2, #666);
}
.dl-empty-desc {
  margin: 0 0 12px 0;
  font-size: 13px;
}
/* OPT-UX: 浮动批量操作栏 */
.dl-batch-bar {
  position: fixed;
  left: 50%;
  bottom: 24px;
  transform: translateX(-50%);
  z-index: 1000;
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 10px 16px;
  background: var(--n-card-color, #fff);
  border-radius: 10px;
  box-shadow: 0 6px 24px rgba(0, 0, 0, 0.16);
  border: 1px solid var(--n-border-color, #eee);
  max-width: calc(100vw - 32px);
  flex-wrap: wrap;
  justify-content: center;
}
.dl-slide-up-enter-active,
.dl-slide-up-leave-active {
  transition: transform 0.25s ease, opacity 0.25s ease;
}
.dl-slide-up-enter-from,
.dl-slide-up-leave-to {
  transform: translate(-50%, 16px);
  opacity: 0;
}
@media (max-width: 768px) {
  .device-toolbar-row {
    flex-direction: column;
    align-items: stretch;
  }
  .device-card-grid {
    grid-template-columns: 1fr;
  }
  .dl-batch-bar {
    bottom: 12px;
    padding: 8px 12px;
  }
}
</style>
