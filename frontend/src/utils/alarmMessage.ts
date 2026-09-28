import { t, useCurrentLocale } from '@/i18n'
import { RULE_TEMPLATES } from '@/constants/ruleTemplates'

const _locale = useCurrentLocale()

/**
 * 后端规则引擎生成的告警消息为英文固定句式（如 Rule 'temp-high' triggered），
 * 与站点中文界面不一致。这里按已知句式解析并本地化展示：
 * - 规则名若与内置规则模板 id 一致，替换为当前语言的模板名（如"温度过高"）；
 * - 未匹配的文案原样返回，避免误伤自定义消息。
 * 注意：i18n 响应式依赖 useCurrentLocale，组件内渲染时会随语言切换自动更新。
 */
export function formatAlarmMessage(raw?: string | null): string {
  void _locale.value
  if (!raw) return ''
  const m = /^Rule '(.+?)' triggered$/.exec(raw)
  if (m) {
    const tpl = RULE_TEMPLATES.value.find((tpl) => tpl.id === m[1])
    return t('alarm.ruleTriggered', { name: tpl ? tpl.name : m[1] })
  }
  return raw
}
