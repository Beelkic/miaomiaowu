// V2 (ACL4SSR) 模板 → V3 模板转换
// 供「V3 模板创建对话框 - 从 V2 导入」和「聚合订阅 - 绑定 V2 模板」共用,
// 两处转换结果保持一致(GitHub issue #125)。
import { RULE_TEMPLATES } from '@/config/custom-rules-templates'
import { api } from '@/lib/api'
import { ALL_TEMPLATE_PRESETS } from '@/lib/template-presets'

export interface V2UserTemplate {
  id: number
  name: string
  rule_source: string
}

const DEFAULT_V3_DNS_PRESET = 'fake_ip_no_dnsleak'

// 下拉选中值约定: user:<id> 为用户 V2 模板, preset:<name> 为内置预设
export function resolveV2TemplateSelection(
  value: string,
  userTemplates: V2UserTemplate[]
): { ruleSourceUrl: string; baseName: string } | null {
  if (value.startsWith('user:')) {
    const templateId = value.slice('user:'.length)
    const template = userTemplates.find((t) => t.id.toString() === templateId)
    return template
      ? { ruleSourceUrl: template.rule_source, baseName: template.name }
      : null
  }
  if (value.startsWith('preset:')) {
    const presetName = value.slice('preset:'.length)
    const preset = ALL_TEMPLATE_PRESETS.find((p) => p.name === presetName)
    return preset ? { ruleSourceUrl: preset.url, baseName: preset.label } : null
  }
  return null
}

// 把转换结果拼成 V3 模板 YAML
function generateV3TemplateFromConversion(
  proxyGroups: any[],
  rules: string[],
  ruleProviders: Record<string, any>,
  dnsPresetKey: string = DEFAULT_V3_DNS_PRESET
): string {
  const lines: string[] = []

  // Basic config
  lines.push('mode: rule')

  // DNS config from preset
  const dnsPreset =
    RULE_TEMPLATES.dns[dnsPresetKey as keyof typeof RULE_TEMPLATES.dns]
  if (dnsPreset) {
    lines.push('dns:')
    // Indent the DNS content
    const dnsLines = dnsPreset.content.split('\n')
    for (const line of dnsLines) {
      lines.push('  ' + line)
    }
  } else {
    // Fallback to basic DNS config
    lines.push('dns:')
    lines.push('  enable: true')
    lines.push('  enhanced-mode: fake-ip')
    lines.push('  nameserver:')
    lines.push('    - https://doh.pub/dns-query')
    lines.push('  ipv6: false')
  }

  lines.push('proxies: null')

  // Proxy groups
  lines.push('proxy-groups:')
  for (const group of proxyGroups) {
    lines.push(`  - name: ${group.name}`)
    lines.push(`    type: ${group.type}`)
    if (group['include-all']) {
      lines.push('    include-all: true')
    }
    if (group['include-all-proxies']) {
      lines.push('    include-all-proxies: true')
    }
    if (group.filter) {
      lines.push(`    filter: ${group.filter}`)
    }
    if (group['exclude-filter']) {
      lines.push(`    exclude-filter: ${group['exclude-filter']}`)
    }
    if (group.proxies && group.proxies.length > 0) {
      lines.push('    proxies:')
      for (const proxy of group.proxies) {
        lines.push(`      - ${proxy}`)
      }
    }
    if (group.url) {
      lines.push(`    url: ${group.url}`)
    }
    if (group.interval) {
      lines.push(`    interval: ${group.interval}`)
    }
    if (group.tolerance) {
      lines.push(`    tolerance: ${group.tolerance}`)
    }
  }

  // Rules
  if (rules.length > 0) {
    lines.push('rules:')
    for (const rule of rules) {
      lines.push(`  - ${rule}`)
    }
  }

  // Rule providers
  if (Object.keys(ruleProviders).length > 0) {
    lines.push('rule-providers:')
    for (const [name, provider] of Object.entries(ruleProviders)) {
      lines.push(`  ${name}:`)
      lines.push(`    type: ${(provider as any).type}`)
      lines.push(`    behavior: ${(provider as any).behavior}`)
      lines.push(`    url: ${(provider as any).url}`)
      lines.push(`    path: ${(provider as any).path}`)
      lines.push(`    interval: ${(provider as any).interval}`)
    }
  }

  return lines.join('\n')
}

// 拉取 ACL 规则源并转换成 V3 模板内容
export async function convertV2RuleSourceToV3(
  ruleSourceUrl: string,
  dnsPresetKey: string = DEFAULT_V3_DNS_PRESET
): Promise<string> {
  const fetchResponse = await api.post('/api/admin/templates/fetch-source', {
    url: ruleSourceUrl,
    use_proxy: false,
  })
  const response = await api.post('/api/admin/template-v3/convert-v2', {
    content: fetchResponse.data.content,
  })
  const { proxy_groups, rules, rule_providers } = response.data
  return generateV3TemplateFromConversion(
    proxy_groups,
    rules,
    rule_providers,
    dnsPresetKey
  )
}

// V2 模板名 → 转换后 V3 模板文件名,例如 "标准版 - 典型分组" → "标准版_典型分组_v3.yaml"
export function v2TemplateToV3Filename(baseName: string): string {
  const sanitized = baseName
    .trim()
    .replace(/\s*-\s*/g, '_')
    .replace(/[\\/:*?"<>|()（）\s]+/g, '_')
    .replace(/^_+|_+$/g, '')
  return `${sanitized || 'v2_template'}_v3.yaml`
}

// 保存 V3 模板文件,返回服务端确认的文件名
export async function uploadV3Template(
  filename: string,
  content: string
): Promise<string> {
  const formData = new FormData()
  formData.append(
    'template',
    new Blob([content], { type: 'text/yaml' }),
    filename
  )
  const response = await api.post(
    '/api/admin/rule-templates/upload',
    formData,
    {
      headers: { 'Content-Type': 'multipart/form-data' },
    }
  )
  return response.data.filename as string
}
