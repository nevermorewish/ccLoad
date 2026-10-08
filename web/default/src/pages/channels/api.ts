import { api, getJSON, postJSON, putJSON, deleteJSON } from '../../lib/api'
import type { Channel, ChannelURLStat } from '../../types'

/** 渠道编辑器一次拉取的聚合响应，见 internal/app/admin_channel_editor.go。 */
export type ChannelKeyRow = {
  key_index?: number
  api_key: string
  note?: string
  allowed_models?: string[]
  detected_models?: string[]
  model_scope_empty?: boolean
  cost_multiplier?: number
  priority?: number
  disabled?: boolean
  cooldown_until?: number
  cooldown_remaining_ms?: number
}

export type ModelEntry = { model: string; redirect_model?: string; disabled?: boolean; pricing?: Record<string, number> }

export type ChannelEditorData = {
  channel?: Channel
  keys?: ChannelKeyRow[]
  management_account?: Record<string, unknown>
  oauth_credential?: unknown
  oauth_credential_info?: Record<string, unknown>
  model_stats?: { available?: boolean; items?: Array<Record<string, unknown>> }
  url_stats?: { available?: boolean; items?: ChannelURLStat[] }
}

/** 渠道表单：字段名与后端 ChannelRequest（internal/app/admin_types.go）一一对应。 */
export type ChannelForm = {
  name: string
  auth_type: string
  urls: Array<{ url: string; exact?: boolean; protocols?: string[] }>
  priority: number
  enabled: boolean
  models: ModelEntry[]
  api_keys: ChannelKeyRow[]
  key_strategy?: string
  rpm_limit?: number
  max_concurrency?: number
  proxy_url?: string
  websockets?: boolean
  protocol_transform_mode?: string
  retry_other_keys_on_failure?: boolean
  daily_cost_limit?: number
  scheduled_check_enabled?: boolean
  scheduled_check_model?: string
  scheduled_check_interval_minutes?: number
  scheduled_check_start_time?: string
  available_time_start?: string
  available_time_end?: string
  custom_request_rules?: unknown
  cooldown_detection_rules?: unknown
  management_account?: unknown
}

export const AUTH_TYPES = [
  { value: 'api_key', label: 'API Key' },
  { value: 'codex_oauth', label: 'Codex OAuth' },
  { value: 'anthropic_oauth', label: 'Anthropic OAuth' },
  { value: 'antigravity_oauth', label: 'Antigravity OAuth' },
  { value: 'xai_oauth', label: 'xAI OAuth' },
  { value: 'codebuddy_oauth', label: 'CodeBuddy OAuth' },
  { value: 'zai_oauth', label: 'Z.ai OAuth' },
  { value: 'zed_oauth', label: 'Zed OAuth' },
  { value: 'cursor_oauth', label: 'Cursor OAuth' },
]

/** 协议转换模式只接受 auto/upstream/local（internal/model/config.go:NormalizeProtocolTransformMode）。 */
export const PROTOCOL_MODES = [
  { value: 'auto', label: '自动（原生优先）' },
  { value: 'upstream', label: '上游直通（透传）' },
  { value: 'local', label: 'ccLoad 转换' },
]

/** Key 策略只接受 sequential/round_robin（internal/model/config.go:IsValidKeyStrategy）。 */
export const KEY_STRATEGIES = [
  { value: 'sequential', label: '按索引顺序' },
  { value: 'round_robin', label: '轮询' },
]

export const URL_PROTOCOLS = [
  { value: '', label: '自动检测' },
  { value: 'anthropic', label: 'anthropic' },
  { value: 'codex', label: 'codex' },
  { value: 'openai', label: 'openai' },
  { value: 'gemini', label: 'gemini' },
]

/** 供 OAuth 凭证刷新使用：auth_type -> 后端资源名。 */
export const CREDENTIAL_RESOURCE: Record<string, string> = {
  codex_oauth: 'codex',
  anthropic_oauth: 'anthropic',
  antigravity_oauth: 'antigravity',
  xai_oauth: 'xai',
  codebuddy_oauth: 'codebuddy',
  zai_oauth: 'zai',
  zed_oauth: 'zed',
  cursor_oauth: 'cursor',
}

export function emptyForm(): ChannelForm {
  return {
    name: '', auth_type: 'api_key', urls: [{ url: '' }], priority: 0, enabled: true,
    models: [], api_keys: [], key_strategy: 'sequential', protocol_transform_mode: 'auto',
  }
}

/** 把编辑器聚合响应转成表单模型。 */
export function formFromEditor(data: ChannelEditorData, fallback: Channel): ChannelForm {
  const detail = data.channel ?? fallback
  const raw = detail as Record<string, unknown>
  // 列表/详情会把无重定向的 redirect_model 回填为模型名本身；载入时去掉，编辑器只显示真正的重定向。
  const entries = (Array.isArray(raw.models) ? raw.models as Array<string | ModelEntry> : [])
    .map((item) => typeof item === 'string' ? { model: item } : { ...item })
    .map(({ redirect_model: redirect, ...entry }) => redirect && redirect !== entry.model ? { ...entry, redirect_model: redirect } : entry)
  return {
    name: String(raw.name ?? fallback.name ?? ''),
    auth_type: String(raw.auth_type ?? 'api_key'),
    urls: Array.isArray(raw.urls) && raw.urls.length ? (raw.urls as ChannelForm['urls']) : [{ url: '' }],
    priority: Number(raw.priority ?? 0),
    enabled: raw.enabled !== false,
    models: entries,
    api_keys: (data.keys ?? []).map((key) => ({ ...key })),
    key_strategy: String(raw.key_strategy ?? 'sequential'),
    rpm_limit: Number(raw.rpm_limit ?? 0),
    max_concurrency: Number(raw.max_concurrency ?? 0),
    proxy_url: String(raw.proxy_url ?? ''),
    websockets: Boolean(raw.websockets),
    protocol_transform_mode: String(raw.protocol_transform_mode ?? 'auto'),
    retry_other_keys_on_failure: Boolean(raw.retry_other_keys_on_failure),
    daily_cost_limit: Number(raw.daily_cost_limit ?? 0),
    scheduled_check_enabled: Boolean(raw.scheduled_check_enabled),
    scheduled_check_model: String(raw.scheduled_check_model ?? ''),
    scheduled_check_interval_minutes: Number(raw.scheduled_check_interval_minutes ?? 300),
    scheduled_check_start_time: String(raw.scheduled_check_start_time ?? '00:00'),
    available_time_start: String(raw.available_time_start ?? ''),
    available_time_end: String(raw.available_time_end ?? ''),
    custom_request_rules: raw.custom_request_rules,
    cooldown_detection_rules: raw.cooldown_detection_rules,
  }
}

/**
 * 表单模型转提交载荷，按认证类型分支（internal/app/admin_channels.go:handleUpdateChannel）：
 * - OAuth 渠道：不得携带 key_strategy、management_account（均 409）；api_keys 至多一行掩码合成 Key，
 *   仅用于回传倍率。
 * - API Key 渠道：携带完整 Key 元数据与 management_account。
 */
export function formToPayload(form: ChannelForm, isOAuth: boolean, extras: { customRules: unknown; cooldownRules: unknown; management: unknown }): Record<string, unknown> {
  const models = form.models.map((entry) => ({ ...entry, model: String(entry.model ?? '').trim() })).filter((entry) => entry.model)
  const keys = (form.api_keys ?? []).map((entry) => ({ ...entry, api_key: String(entry.api_key ?? '').trim() })).filter((entry) => entry.api_key)
  const { key_strategy: keyStrategy, management_account: _managementAccount, custom_request_rules: _customRules, cooldown_detection_rules: _cooldownRules, ...rest } = form
  const payload: Record<string, unknown> = {
    ...rest,
    models,
    // 空规则发 null：省略字段会被 ToConfig 写成 nil 并清掉现有规则，显式 null 语义更清楚。
    custom_request_rules: extras.customRules ?? null,
    cooldown_detection_rules: extras.cooldownRules ?? null,
  }
  if (isOAuth) {
    const synthetic = keys.slice(0, 1).map((entry) => ({ api_key: entry.api_key, cost_multiplier: entry.cost_multiplier }))
    payload.api_keys = synthetic
    payload.api_key = synthetic.map((entry) => entry.api_key).join(',')
    return payload
  }
  payload.api_keys = keys.map(({ cooldown_until: _cooldownUntil, cooldown_remaining_ms: _cooldownRemaining, key_index: _keyIndex, disabled: _disabled, ...entry }) => entry)
  payload.api_key = keys.map((entry) => entry.api_key).join(',')
  payload.key_strategy = keyStrategy || 'sequential'
  payload.management_account = extras.management ?? { profile: '' }
  return payload
}

/**
 * URL 启停、test-url、编辑器 URL 统计都按 RuntimeURL 匹配：exact URL 末尾带 '#'
 * （internal/model/config.go:RuntimeURL）。不带 '#' 会报 "url not found in channel"。
 */
export const runtimeURL = (entry: { url: string; exact?: boolean }) => entry.exact ? `${entry.url.trim()}#` : entry.url.trim()

export const loadEditor = (id: number) => getJSON<ChannelEditorData>(`/admin/channels/${id}/editor`)
export const loadKeys = (id: number) => getJSON<ChannelKeyRow[]>(`/admin/channels/${id}/keys`)
export const createChannel = (payload: Record<string, unknown>) => postJSON('/admin/channels', payload)
export const updateChannel = (id: number, payload: Record<string, unknown>) => putJSON(`/admin/channels/${id}`, payload)
export const removeChannel = (id: number) => deleteJSON(`/admin/channels/${id}`)
export const toggleKey = (id: number, index: number, disabled: boolean) => postJSON(`/admin/channels/${id}/${disabled ? 'key-disable' : 'key-enable'}`, { key_index: index })
export const deleteKey = (id: number, index: number) => deleteJSON(`/admin/channels/${id}/keys/${index}`)
export const toggleURL = (id: number, url: string, disabled: boolean) => postJSON(`/admin/channels/${id}/${disabled ? 'url-disable' : 'url-enable'}`, { url })
export const testChannel = (id: number, body: Record<string, unknown>) => postJSON(`/admin/channels/${id}/test`, body)
export const testChannelURL = (id: number, body: Record<string, unknown>) => postJSON(`/admin/channels/${id}/test-url`, body)
export type FetchModelsResult = { models?: Array<string | ModelEntry>; key_models?: Array<{ key_index: number; models?: ModelEntry[]; error?: string }>; protocol?: string }
export const fetchModelsPreview = (body: Record<string, unknown>) => postJSON<FetchModelsResult>('/admin/channels/models/fetch', body)
export const fetchChannelModels = (id: number, params?: Record<string, unknown>) => getJSON<FetchModelsResult>(`/admin/channels/${id}/models/fetch`, params)

/**
 * 模型别名规范化，等价于 web/assets/js/model-entry-parser.js:normalizeModelEntries：
 * 只改对外别名，原名作为上游模型保留在 redirect_model；按（别名, 上游）去重。
 */
export function normalizeModelEntries(entries: ModelEntry[], options: { lowercase?: boolean; stripPrefix?: boolean }): ModelEntry[] {
  const spelling = new Map<string, string>()
  const seen = new Set<string>()
  const result: ModelEntry[] = []
  for (const entry of entries) {
    const model = String(entry.model ?? '').trim()
    if (!model) continue
    const upstream = String(entry.redirect_model ?? '').trim() || model
    let alias = model
    if (options.stripPrefix) { const separator = alias.lastIndexOf('/'); if (separator >= 0 && separator + 1 < alias.length) alias = alias.slice(separator + 1) }
    if (options.lowercase) alias = alias.toLowerCase()
    const group = alias.toLowerCase()
    if (!spelling.has(group)) spelling.set(group, alias)
    alias = spelling.get(group) ?? alias
    const key = `${alias.toLowerCase()}\u0000${upstream.toLowerCase()}`
    if (seen.has(key)) continue
    seen.add(key)
    const { redirect_model: _redirect, ...rest } = entry
    result.push({ ...rest, model: alias, ...(upstream === alias ? {} : { redirect_model: upstream }) })
  }
  return result
}

/** 合并获取到的模型：已存在的（别名, 上游）保持原行，保留价格与停用状态。 */
export function mergeModelEntries(current: ModelEntry[], incoming: ModelEntry[]): ModelEntry[] {
  const identity = (entry: ModelEntry) => `${entry.model.toLowerCase()}\u0000${(entry.redirect_model || entry.model).toLowerCase()}`
  const seen = new Set(current.map(identity))
  const merged = [...current]
  for (const entry of incoming) {
    if (!entry.model || seen.has(identity(entry))) continue
    seen.add(identity(entry))
    merged.push(entry)
  }
  return merged
}
export const addModels = (id: number, models: ModelEntry[]) => postJSON(`/admin/channels/${id}/models`, { models })
export const deleteModels = (id: number, models: string[]) => api.delete(`/admin/channels/${id}/models`, { data: { models } }).then((response) => response.data)
export const checkDuplicate = (urls: unknown[]) => postJSON<{ duplicates?: Array<{ id: number; name: string }> }>('/admin/channels/check-duplicate', { urls })
export const websocketProbe = (body: Record<string, unknown>) => postJSON('/admin/channels/websocket-probe', body)
export const fetchKeyRate = (body: Record<string, unknown>) => postJSON<{ effective_rate_multiplier?: number }>('/admin/channels/billing/fetch', body)
export const refreshCredential = (id: number, provider: string) => postJSON(`/admin/channels/${id}/${provider}-credential/refresh`, {})
export const oauthUsage = (id: number) => postJSON(`/admin/channels/${id}/oauth-usage`, {})
export const codexQuotaReset = (id: number) => postJSON(`/admin/channels/${id}/codex-quota-reset`, {})
export const codebuddyCheckin = (id: number) => postJSON(`/admin/channels/${id}/codebuddy-checkin`, {})
export const managementBalance = (id: number) => postJSON(`/admin/channels/${id}/management-account/balance`, {})
export const managementCheckin = (id: number) => postJSON(`/admin/channels/${id}/management-account/checkin`, {})
export const sub2apiLogin = (body: Record<string, unknown>) => postJSON<Record<string, unknown>>('/admin/channel-management/sub2api-login', body)
export const cooldownDetectionTest = (body: Record<string, unknown>) => postJSON('/admin/channels/cooldown-detection/test', body)

/** 行内优先级编辑：后端只接受 {updates:[{id,priority}]}。 */
export const batchPriority = (updates: Array<{ id: number; priority: number }>) => postJSON('/admin/channels/batch-priority', { updates })

/** 手动排序覆盖：sort_override=0 取消覆盖，恢复自动健康度排序。 */
export const batchSortOverride = (updates: Array<{ id: number; sort_override: number }>) => postJSON('/admin/channels/batch-sort-override', { updates })

/** 拉取全部渠道（不带 limit/offset 时后端返回全量，用于全局排序）。 */
export const loadAllChannels = () => getJSON<Channel[]>('/admin/channels')

/**
 * 批量高级设置。后端 HandleBatchPatchChannels 只读顶层字段，
 * 绝不接受 { patch: {...} } 包装层。
 */
export const batchPatch = (channelIds: number[], patch: Record<string, unknown>) =>
  postJSON('/admin/channels/batch-advanced', { channel_ids: channelIds, ...patch })

export const batchEnabled = (channelIds: number[], enabled: boolean) => postJSON('/admin/channels/batch-enabled', { channel_ids: channelIds, enabled })
export const batchClearCooldowns = (channelIds: number[]) => postJSON('/admin/channels/batch-clear-cooldowns', { channel_ids: channelIds })
export const batchDelete = (channelIds: number[]) => postJSON('/admin/channels/batch-delete', { channel_ids: channelIds })
export const batchDeleteModels = (operations: Array<{ channel_id: number; models: string[] }>) => postJSON('/admin/channels/models/batch-delete', { operations })
export const refreshModelsBatch = (channelIds: number[], mode: 'merge' | 'replace', extra?: Record<string, unknown>) => postJSON('/admin/channels/models/refresh-batch', { channel_ids: channelIds, mode, ...extra })

export const loadFilterOptions = () => getJSON<{ channel_names?: string[]; models?: string[] }>('/admin/channels/filter-options')

/** 触发浏览器下载。 */
export async function download(url: string, filename: string, params?: Record<string, unknown>): Promise<void> {
  const response = await api.get(url, { params, responseType: 'blob' })
  const objectURL = URL.createObjectURL(response.data as Blob)
  const link = document.createElement('a')
  link.href = objectURL
  link.download = filename
  document.body.appendChild(link)
  link.click()
  link.remove()
  window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000)
}

export function uploadJSON(url: string, file: File): Promise<unknown> {
  const body = new FormData()
  body.append('file', file, file.name)
  return api.post(url, body).then((response) => response.data)
}
