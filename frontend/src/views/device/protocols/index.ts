/**
 * Protocol form/detail component registry.
 *
 * Provides dynamic component resolution based on device protocol type.
 * Each protocol can have a custom form component for create/edit and
 * a custom detail component for the device detail page.
 *
 * If no custom component exists for a protocol, null is returned and
 * the caller falls back to the generic protocol config form built from
 * PROTOCOL_CONFIGS.
 */
import { defineComponent, h, reactive, watch, type Component } from 'vue'
import { NForm, NFormItem, NInput, NInputNumber, NSelect, NSwitch, NDivider, NDynamicTags, NText, type FormInst } from 'naive-ui'
import { PROTOCOL_CONFIGS, normalizeProtocolName } from '@/constants/protocolConfig'
import type { ProtocolFieldDef } from '@/constants/protocolConfig'
import { t } from '@/i18n'
import { useAuthStore } from '@/stores/auth'

// DeviceService.WritePoint enforces these config entries, and the device update
// endpoint refuses changes to them from a role without the write-policy
// permission. They are listed here so the form locks exactly the controls that
// would fail on save, and nothing else.
const WRITE_POLICY_KEYS = new Set(['write_verify', 'write_rate_limit', 'write_audit', 'write_whitelist'])

/**
 * Generic protocol form component that renders fields based on PROTOCOL_CONFIGS.
 * Used as fallback when no protocol-specific component exists.
 *
 * 与 DeviceList 的契约：props 用 config（非 v-model），并通过 defineExpose 提供
 * validate()（返回配置对象）/ getAssembledConfig() / getAssembledPoints()，
 * 供创建/克隆/编辑弹窗在提交时收集表单数据。此前缺少这些方法导致
 * "protocolFormRef.value?.validate is not a function"，创建/克隆按钮必然失败。
 */
const GenericProtocolForm = defineComponent({
  name: 'GenericProtocolForm',
  props: {
    protocol: { type: String, required: true },
    config: { type: Object, default: () => ({}) },
    points: { type: Array, default: () => [] },
    mode: { type: String, default: 'create' },
    disabled: { type: Boolean, default: false },
  },
  setup(props, { expose }) {
    const configs = PROTOCOL_CONFIGS
    const auth = useAuthStore()
    const state = reactive<Record<string, any>>({})
    const currentFields = () => (configs.value[normalizeProtocolName(props.protocol)]?.configFields || []) as ProtocolFieldDef[]

    // Creating a device is its own permission; only an existing device's policy
    // is ADMIN-gated, so the lock applies to the edit form.
    const policyLocked = (field: ProtocolFieldDef) =>
      WRITE_POLICY_KEYS.has(field.key) && props.mode !== 'create' && !auth.hasPerm('device:write_policy_edit')

    const initFromProps = () => {
      Object.keys(state).forEach((k) => delete state[k])
      // Start from the device's stored config, not from the field list: the backend
      // replaces the whole config map on update, so a key the form does not render
      // (write_whitelist, hand-authored driver options) used to be deleted from the
      // device the moment anyone saved an unrelated change.
      Object.assign(state, props.config ?? {})
      for (const field of currentFields()) {
        const v = props.config?.[field.key]
        state[field.key] = v !== undefined && v !== null
          ? v
          : field.default !== undefined && field.default !== null
            ? field.default
            : field.type === 'boolean' ? false : field.type === 'number' ? (field.min ?? 0) : ''
      }
    }
    watch(() => [props.protocol, props.config], initFromProps, { immediate: true })

    const validate = async (): Promise<Record<string, any>> => {
      const missing = currentFields()
        .filter((f) => !f.notImplemented && f.required)
        .filter((f) => state[f.key] === undefined || state[f.key] === null || state[f.key] === '')
        .map((f) => f.label)
      if (missing.length) throw new Error(missing.join(', '))
      // Enforce the HOST_PATTERN / URL_PATTERN / endpoint regexes declared on the
      // fields; without this a malformed host/URL saves cleanly and only fails at
      // the driver. An empty value is already covered by the required check above.
      const badPattern = currentFields()
        .filter((f) => !f.notImplemented && f.pattern)
        .filter((f) => {
          const v = state[f.key]
          if (v === undefined || v === null || v === '') return false
          try { return !new RegExp(f.pattern as string).test(String(v)) } catch { return false }
        })
        .map((f) => `${f.label} ${t('common.formatInvalid')}`)
      if (badPattern.length) throw new Error(badPattern.join(', '))
      return { ...state }
    }
    const getAssembledConfig = () => ({ ...state })
    const getAssembledPoints = () => props.points
    expose({ validate, getAssembledConfig, getAssembledPoints })

    return () => {
      const cfg = configs.value[normalizeProtocolName(props.protocol)]
      if (!cfg) return null
      const renderControl = (field: ProtocolFieldDef) => {
        const value = state[field.key]
        const disabled = props.disabled || policyLocked(field)
        if (field.type === 'number') {
          return h(NInputNumber, {
            value,
            disabled,
            placeholder: field.placeholder,
            min: field.min,
            max: field.max,
            'onUpdate:value': (v: number | null) => { state[field.key] = v },
          })
        }
        if (field.type === 'boolean') {
          return h(NSwitch, {
            value,
            disabled,
            'onUpdate:value': (v: boolean) => { state[field.key] = v },
          })
        }
        if (field.type === 'select') {
          return h(NSelect, {
            value,
            disabled,
            options: field.options || [],
            'onUpdate:value': (v: any) => { state[field.key] = v },
          })
        }
        if (field.type === 'password') {
          return h(NInput, {
            value,
            type: 'password',
            showPasswordOn: 'click',
            disabled,
            placeholder: field.placeholder,
            'onUpdate:value': (v: string) => { state[field.key] = v },
          })
        }
        return h(NInput, {
          value,
          disabled,
          placeholder: field.placeholder,
          'onUpdate:value': (v: string) => { state[field.key] = v },
        })
      }
      const fields = currentFields()
      // Settings the driver never reads are kept (a device config saved by hand
      // or by another tool round-trips through this form only while the field is
      // declared) but pushed below a divider, so the operator is not asked to
      // tune something that cannot take effect.
      const live = fields.filter((f) => !f.notImplemented)
      const dead = fields.filter((f) => f.notImplemented)
      const rows = live.map((field) => {
        const hints: string[] = []
        if (field.tooltip) hints.push(field.tooltip)
        if (policyLocked(field)) hints.push(t('protocolConfig.shared.writePolicyLocked'))
        return h(NFormItem, { label: field.label, required: field.required }, {
          default: () => hints.length
            ? [renderControl(field), h(NText, { depth: 3, style: 'font-size:12px' }, { default: () => hints.join(' · ') })]
            : renderControl(field),
        })
      })
      // write_whitelist holds user names/ids, which none of the generated controls
      // can render, so it gets its own editor. Without it an operator rejected with
      // ERR_WRITE_NOT_WHITELISTED could not see that a whitelist exists at all.
      if (props.mode === 'edit' && cfg.capabilities?.write) {
        const raw = state.write_whitelist
        const wl = Array.isArray(raw)
          ? raw.map(String)
          : typeof raw === 'string' && raw.trim()
            ? raw.split(',').map((s) => s.trim()).filter(Boolean)
            : []
        const locked = !auth.hasPerm('device:write_policy_edit')
        rows.push(h(NFormItem, { label: t('protocolConfig.shared.writeWhitelist') }, {
          default: () => [
            h(NDynamicTags, {
              value: wl,
              disabled: props.disabled || locked,
              max: 50,
              'onUpdate:value': (v: string[]) => { state.write_whitelist = v },
            }),
            h(NText, { depth: 3, style: 'font-size:12px' }, {
              default: () => [
                wl.length ? t('protocolConfig.shared.writeWhitelistTip') : t('protocolConfig.shared.writeWhitelistEmpty'),
                locked ? t('protocolConfig.shared.writePolicyLocked') : '',
              ].filter(Boolean).join(' · '),
            }),
          ],
        }))
      }
      if (dead.length) {
        rows.push(h(NDivider, { titlePlacement: 'left', style: 'margin:12px 0' }, { default: () => t('protocolConfig.shared.notImplementedGroup') }))
        for (const field of dead) {
          rows.push(h(NFormItem, { label: field.label }, {
            default: () => h(NText, { depth: 3, italic: true, style: 'font-size:12px' }, { default: () => field.tooltip || t('protocolConfig.shared.notImplementedHint') })
          }))
        }
      }
      return h('div', { class: 'protocol-form-generic' }, [
        h(NDivider, { titlePlacement: 'left' }, { default: () => cfg.label }),
        ...rows,
      ])
    }
  },
})

/**
 * Generic protocol detail component for the device detail page.
 */
const GenericProtocolDetail = defineComponent({
  name: 'GenericProtocolDetail',
  props: {
    protocol: { type: String, required: true },
    config: { type: Object, default: () => ({}) },
    device: { type: Object, default: () => ({}) },
  },
  setup(props) {
    const configs = PROTOCOL_CONFIGS
    return () => {
      const cfg = configs.value[normalizeProtocolName(props.protocol)]
      if (!cfg) return null
      return h('div', { class: 'protocol-detail-generic' }, [
        h(NDivider, { titlePlacement: 'left' }, { default: () => cfg.label }),
        // Not-implemented fields are left out rather than listed with their
        // declared default: "Deadband: 0" on a driver that never reads deadband
        // reads as a configured, working setting.
        ...cfg.configFields.filter((field: ProtocolFieldDef) => !field.notImplemented).map((field: ProtocolFieldDef) => {
          const value = props.config?.[field.key] ?? field.default
          return h('div', { key: field.key, style: 'display:flex;justify-content:space-between;padding:4px 0;' }, [
            h('span', { style: 'color:var(--text-color-3);' }, field.label + ':'),
            h('span', null, String(value ?? '-')),
          ])
        }),
      ])
    }
  },
})

// Registry of protocol-specific components (can be extended)
const _formRegistry: Record<string, Component> = {}
const _detailRegistry: Record<string, Component> = {}

/**
 * Get the protocol-specific form component for create/edit dialogs.
 * Returns null if no specific component exists (caller should use generic form).
 */
export function getProtocolFormComponent(protocol: string): Component | null {
  return _formRegistry[protocol] || GenericProtocolForm
}

/**
 * Get the protocol-specific detail component for the device detail page.
 * Returns null if no specific component exists (caller should use generic detail).
 */
export function getProtocolDetailComponent(protocol: string): Component | null {
  return _detailRegistry[protocol] || GenericProtocolDetail
}

/**
 * Register a custom protocol form component.
 */
export function registerProtocolFormComponent(protocol: string, component: Component): void {
  _formRegistry[protocol] = component
}

/**
 * Register a custom protocol detail component.
 */
export function registerProtocolDetailComponent(protocol: string, component: Component): void {
  _detailRegistry[protocol] = component
}
