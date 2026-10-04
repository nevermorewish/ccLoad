import { useEffect, useState } from 'react'
import { Dialog } from '../../components/dialog'
import { SearchableSelect } from '../../components/searchable-select'
import { cooldownDetectionTest, refreshCredential, sub2apiLogin, CREDENTIAL_RESOURCE } from './api'

// ---------------------------------------------------------------- 草稿类型

export type HeaderRuleDraft = { action: string; name: string; value: string }
export type BodyRuleDraft = { action: string; path: string; value: string }
export type CooldownRuleDraft = {
  enabled: boolean; name: string; status_codes: string; message_pattern: string; scope: string; mode: string
  cooldown_seconds: string; time_capture: string; time_format: string; time_layout: string; timezone: string
}
export type ManagementDraft = {
  profile: string; base_url: string; access_token: string; refresh_token: string; user_id: string
  email: string; password: string; totp_code: string; daily_checkin_enabled: boolean; daily_checkin_time: string
  credential_configured?: boolean
}
export type AdvancedDraft = {
  headers: HeaderRuleDraft[]; body: BodyRuleDraft[]; cooldown: CooldownRuleDraft[]
  proxy_url: string; available_time_start: string; available_time_end: string; retry_other_keys_on_failure: boolean
  scheduled_check_enabled: boolean; scheduled_check_model: string; scheduled_check_interval_minutes: number; scheduled_check_start_time: string
  management: ManagementDraft
}

const MAX_RULES = 32
const MAX_VALUE_BYTES = 8 * 1024
const BODY_PATH = /^[A-Za-z0-9_.-]+$/
const TIME_HHMM = /^([01]\d|2[0-3]):[0-5]\d$/
// 这些请求头由 ccLoad 根据凭证注入，自定义规则不得覆盖（与旧页 channels-custom-rules.js 一致）。
const PROTECTED_HEADERS = new Set(['authorization', 'x-api-key', 'x-goog-api-key', 'x-refresh-token'])

const HEADER_ACTIONS = [{ value: 'override', label: '覆盖' }, { value: 'append', label: '追加' }, { value: 'remove', label: '删除' }]
const BODY_ACTIONS = [{ value: 'override', label: '覆盖' }, { value: 'remove', label: '删除' }]
const SCOPES = [{ value: 'key', label: 'Key' }, { value: 'model', label: '模型' }, { value: 'channel', label: '渠道' }]
const MODES = [{ value: 'fixed', label: '固定时长' }, { value: 'reset_time', label: '按上游重置时间' }]
const TIME_FORMATS = ['datetime', 'time_of_day', 'unix', 'unix_ms', 'duration_seconds'].map((value) => ({ value, label: value }))
const PROFILES = [{ value: '', label: '不启用' }, { value: 'new_api', label: 'New API' }, { value: 'sub2api', label: 'Sub2API' }, { value: 'sub2api_pro', label: 'Sub2API Pro' }]

// ---------------------------------------------------------------- 与后端结构互转

export function advancedFromChannel(channel: Record<string, unknown>, management: Record<string, unknown> | undefined): AdvancedDraft {
  const custom = (channel.custom_request_rules ?? {}) as { headers?: Array<Record<string, unknown>>; body?: Array<Record<string, unknown>> }
  const cooldown = (channel.cooldown_detection_rules ?? {}) as { rules?: Array<Record<string, unknown>> }
  const rules = [...(cooldown.rules ?? [])].sort((a, b) => Number(a.priority ?? 0) - Number(b.priority ?? 0))
  const account = management ?? {}
  return {
    headers: (custom.headers ?? []).map((rule) => ({ action: String(rule.action ?? 'override'), name: String(rule.name ?? ''), value: String(rule.value ?? '') })),
    body: (custom.body ?? []).map((rule) => ({ action: String(rule.action ?? 'override'), path: String(rule.path ?? ''), value: rule.value === undefined ? '' : JSON.stringify(rule.value) })),
    cooldown: rules.map((rule) => ({
      enabled: rule.enabled !== false, name: String(rule.name ?? ''), status_codes: Array.isArray(rule.status_codes) ? rule.status_codes.join(', ') : '',
      message_pattern: String(rule.message_pattern ?? ''), scope: String(rule.scope ?? 'key'), mode: String(rule.mode ?? 'fixed'),
      cooldown_seconds: rule.cooldown_seconds == null ? '' : String(rule.cooldown_seconds), time_capture: String(rule.time_capture ?? ''),
      time_format: String(rule.time_format ?? 'datetime'), time_layout: String(rule.time_layout ?? ''), timezone: String(rule.timezone ?? ''),
    })),
    proxy_url: String(channel.proxy_url ?? ''),
    available_time_start: String(channel.available_time_start ?? ''),
    available_time_end: String(channel.available_time_end ?? ''),
    retry_other_keys_on_failure: Boolean(channel.retry_other_keys_on_failure),
    scheduled_check_enabled: Boolean(channel.scheduled_check_enabled),
    scheduled_check_model: String(channel.scheduled_check_model ?? ''),
    scheduled_check_interval_minutes: Number(channel.scheduled_check_interval_minutes || 300),
    scheduled_check_start_time: String(channel.scheduled_check_start_time || '00:00'),
    management: {
      profile: String(account.profile ?? ''), base_url: String(account.base_url ?? ''), access_token: String(account.access_token ?? ''),
      refresh_token: String(account.refresh_token ?? ''), user_id: account.user_id == null ? '' : String(account.user_id),
      email: String(account.email ?? ''), password: String(account.password ?? ''), totp_code: '',
      daily_checkin_enabled: Boolean(account.daily_checkin_enabled), daily_checkin_time: String(account.daily_checkin_time ?? ''),
      credential_configured: Boolean(account.credential_configured),
    },
  }
}

export const emptyAdvanced = (): AdvancedDraft => advancedFromChannel({}, undefined)

/** 本地校验，规则与后端 validateCustomRequestRules / NormalizeCooldownDetectionRules 对齐；返回首个错误与所在页签。 */
export function validateAdvanced(draft: AdvancedDraft): { tab: Tab; message: string } | null {
  if (draft.headers.length > MAX_RULES || draft.body.length > MAX_RULES) return { tab: 'custom', message: `每类规则最多 ${MAX_RULES} 条` }
  for (const [index, rule] of draft.headers.entries()) {
    const name = rule.name.trim()
    if (!name) return { tab: 'custom', message: `请求头第 ${index + 1} 条缺少名称` }
    if (name.length > 256) return { tab: 'custom', message: `请求头第 ${index + 1} 条名称过长` }
    if (PROTECTED_HEADERS.has(name.toLowerCase())) return { tab: 'custom', message: `请求头 ${name} 受保护，不能自定义` }
    if (rule.action !== 'remove' && new Blob([rule.value]).size > MAX_VALUE_BYTES) return { tab: 'custom', message: `请求头 ${name} 的值超过 8KB` }
  }
  for (const [index, rule] of draft.body.entries()) {
    if (!BODY_PATH.test(rule.path.trim())) return { tab: 'custom', message: `请求参数第 ${index + 1} 条路径只能包含字母、数字、_ . -` }
    if (rule.action === 'remove') continue
    if (new Blob([rule.value]).size > MAX_VALUE_BYTES) return { tab: 'custom', message: `请求参数 ${rule.path} 的值超过 8KB` }
    try { JSON.parse(rule.value) } catch { return { tab: 'custom', message: `请求参数 ${rule.path} 的值必须是 JSON 字面量（字符串需加引号）` } }
  }
  if (draft.cooldown.length > MAX_RULES) return { tab: 'cooldown', message: `冷却规则最多 ${MAX_RULES} 条` }
  for (const [index, rule] of draft.cooldown.entries()) {
    const codes = parseCodes(rule.status_codes)
    if (codes === null) return { tab: 'cooldown', message: `冷却规则第 ${index + 1} 条状态码无效（100–599）` }
    if (!codes.length && !rule.message_pattern.trim()) return { tab: 'cooldown', message: `冷却规则第 ${index + 1} 条至少需要状态码或消息匹配之一` }
    if (rule.message_pattern.trim()) { try { new RegExp(rule.message_pattern) } catch { return { tab: 'cooldown', message: `冷却规则第 ${index + 1} 条消息匹配不是有效正则` } } }
    if (rule.mode === 'fixed' && !(Number(rule.cooldown_seconds) > 0)) return { tab: 'cooldown', message: `冷却规则第 ${index + 1} 条需要正数冷却秒数` }
    if (rule.mode === 'reset_time' && !rule.time_capture.trim()) return { tab: 'cooldown', message: `冷却规则第 ${index + 1} 条需要时间捕获表达式` }
  }
  if ((draft.available_time_start || draft.available_time_end) && !(TIME_HHMM.test(draft.available_time_start) && TIME_HHMM.test(draft.available_time_end))) return { tab: 'other', message: '可用时段需同时填写 HH:MM 格式的开始与结束时间' }
  if (draft.scheduled_check_enabled) {
    const interval = draft.scheduled_check_interval_minutes
    if (!Number.isInteger(interval) || interval < 1 || interval > 1440) return { tab: 'other', message: '定时检测间隔需为 1–1440 的整数分钟' }
    if (!TIME_HHMM.test(draft.scheduled_check_start_time)) return { tab: 'other', message: '定时检测开始时间需为 HH:MM' }
  }
  const account = draft.management
  if (account.profile) {
    if (!isPanelBaseURL(account.base_url)) return { tab: 'other', message: '面板地址需为 http/https 根地址，不含路径、查询参数或用户信息' }
    if (account.daily_checkin_enabled && account.daily_checkin_time && !TIME_HHMM.test(account.daily_checkin_time)) return { tab: 'other', message: '签到时间需为 HH:MM' }
    if (account.user_id && !(Number.isInteger(Number(account.user_id)) && Number(account.user_id) > 0)) return { tab: 'other', message: '用户 ID 需为正整数' }
  }
  return null
}

function parseCodes(value: string): number[] | null {
  const parts = value.split(/[\s,]+/).filter(Boolean)
  const codes = parts.map(Number)
  return codes.every((code) => Number.isInteger(code) && code >= 100 && code <= 599) ? codes : null
}

function isPanelBaseURL(value: string): boolean {
  try {
    const parsed = new URL(value)
    return (parsed.protocol === 'http:' || parsed.protocol === 'https:') && !parsed.username && !parsed.password && !parsed.search && !parsed.hash && (parsed.pathname === '/' || parsed.pathname === '')
  } catch { return false }
}

/** 草稿转后端结构。调用前必须先通过 validateAdvanced。 */
export function advancedToPayload(draft: AdvancedDraft, isOAuth: boolean): Record<string, unknown> {
  const headers = draft.headers.map((rule) => ({ action: rule.action, name: rule.name.trim(), ...(rule.action === 'remove' ? {} : { value: rule.value }) }))
  const body = draft.body.map((rule) => ({ action: rule.action, path: rule.path.trim(), ...(rule.action === 'remove' ? {} : { value: JSON.parse(rule.value) as unknown }) }))
  const rules = draft.cooldown.map((rule, index) => ({
    enabled: rule.enabled, name: rule.name.trim() || undefined, priority: index + 1,
    status_codes: parseCodes(rule.status_codes) ?? [], message_pattern: rule.message_pattern.trim() || undefined, scope: rule.scope, mode: rule.mode,
    ...(rule.mode === 'fixed' ? { cooldown_seconds: Number(rule.cooldown_seconds) } : { time_capture: rule.time_capture, time_format: rule.time_format, time_layout: rule.time_layout || undefined, timezone: rule.timezone || undefined }),
  }))
  const payload: Record<string, unknown> = {
    custom_request_rules: headers.length || body.length ? { headers, body } : null,
    cooldown_detection_rules: rules.length ? { rules } : null,
    proxy_url: draft.proxy_url.trim(),
    available_time_start: draft.available_time_start,
    available_time_end: draft.available_time_end,
    retry_other_keys_on_failure: draft.retry_other_keys_on_failure,
    scheduled_check_enabled: draft.scheduled_check_enabled,
    scheduled_check_model: draft.scheduled_check_model,
    scheduled_check_interval_minutes: draft.scheduled_check_interval_minutes,
    scheduled_check_start_time: draft.scheduled_check_start_time,
  }
  // OAuth 渠道提交 management_account 会 409；只有 API Key 渠道才带。
  if (!isOAuth) {
    const account = draft.management
    payload.management_account = account.profile ? {
      profile: account.profile, base_url: account.base_url.trim(),
      ...(account.access_token ? { access_token: account.access_token } : {}),
      ...(account.refresh_token ? { refresh_token: account.refresh_token } : {}),
      ...(account.user_id ? { user_id: Number(account.user_id) } : {}),
      ...(account.email ? { email: account.email } : {}),
      ...(account.password ? { password: account.password } : {}),
      ...(account.totp_code ? { totp_code: account.totp_code } : {}),
      daily_checkin_enabled: account.daily_checkin_enabled,
      ...(account.daily_checkin_time ? { daily_checkin_time: account.daily_checkin_time } : {}),
    } : { profile: '' }
  }
  return payload
}

// ---------------------------------------------------------------- 弹窗

type Tab = 'custom' | 'cooldown' | 'credential' | 'other'

type Props = {
  open: boolean
  draft: AdvancedDraft
  channelId: number | null
  authType: string
  urls: string[]
  models: string[]
  oauthCredential?: unknown
  oauthCredentialInfo?: unknown
  onClose: () => void
  onApply: (draft: AdvancedDraft) => void
}

export function AdvancedSettingsDialog({ open, draft: initial, channelId, authType, urls, models, oauthCredential, oauthCredentialInfo, onClose, onApply }: Props) {
  const isOAuth = authType !== 'api_key'
  const [draft, setDraft] = useState<AdvancedDraft>(initial)
  const [tab, setTab] = useState<Tab>('custom')
  const [customTab, setCustomTab] = useState<'headers' | 'body'>('headers')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  // 打开时取一份副本：确认才写回编辑器，取消即丢弃。
  useEffect(() => { if (open) { setDraft(structuredClone(initial)); setTab('custom'); setError(''); setNotice('') } }, [open, initial])

  const patch = (value: Partial<AdvancedDraft>) => setDraft((current) => ({ ...current, ...value }))
  const patchAccount = (value: Partial<ManagementDraft>) => setDraft((current) => ({ ...current, management: { ...current.management, ...value } }))

  const apply = () => {
    const problem = validateAdvanced(draft)
    if (problem) { setTab(problem.tab); setError(problem.message); return }
    onApply(draft); onClose()
  }

  const tabs: Array<{ id: Tab; label: string; hidden?: boolean }> = [
    { id: 'custom', label: `自定义请求规则 (${draft.headers.length + draft.body.length})` },
    { id: 'cooldown', label: `冷却检测规则 (${draft.cooldown.length})` },
    { id: 'credential', label: 'OAuth 凭证', hidden: !isOAuth },
    { id: 'other', label: '其他' },
  ]

  return (
    <Dialog open={open} onClose={onClose} size="lg" closeOnBackdrop={false} title="高级设置"
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={apply}>确定</button></>}>
      <div className="dialog-tabs" role="tablist">
        {tabs.filter((item) => !item.hidden).map((item) => <button key={item.id} role="tab" aria-selected={tab === item.id} className={`dialog-tab${tab === item.id ? ' active' : ''}`} onClick={() => { setTab(item.id); setError('') }}>{item.label}</button>)}
      </div>
      {error && <p className="error-text" role="alert">{error}</p>}
      {notice && <p className="muted" role="status">{notice}</p>}

      {tab === 'custom' && <>
        {urls.some((url) => /anyrouter/i.test(url)) && <p className="muted">检测到 anyrouter 地址：ccLoad 会自动注入 <code>anthropic-beta: context-1m-2025-08-07</code>，并在 /v1/messages 缺少 thinking 时补 <code>thinking.type=adaptive</code>。</p>}
        <div className="dialog-tabs" role="tablist">
          <button role="tab" aria-selected={customTab === 'headers'} className={`dialog-tab${customTab === 'headers' ? ' active' : ''}`} onClick={() => setCustomTab('headers')}>请求头 ({draft.headers.length})</button>
          <button role="tab" aria-selected={customTab === 'body'} className={`dialog-tab${customTab === 'body' ? ' active' : ''}`} onClick={() => setCustomTab('body')}>请求参数 ({draft.body.length})</button>
        </div>
        {customTab === 'headers' ? <>
          <p className="muted">覆盖/追加/删除上游请求头。受保护的认证头（Authorization、x-api-key 等）不可修改。</p>
          {draft.headers.map((rule, index) => (
            <div className="toolbar" key={index}>
              <SearchableSelect ariaLabel="动作" className="combobox-inline" value={rule.action} options={HEADER_ACTIONS} onChange={(action) => patch({ headers: draft.headers.map((item, itemIndex) => itemIndex === index ? { ...item, action } : item) })} />
              <input className="input" value={rule.name} onChange={(event) => patch({ headers: draft.headers.map((item, itemIndex) => itemIndex === index ? { ...item, name: event.target.value } : item) })} placeholder="Header 名称" />
              {rule.action !== 'remove' && <input className="input wide" value={rule.value} onChange={(event) => patch({ headers: draft.headers.map((item, itemIndex) => itemIndex === index ? { ...item, value: event.target.value } : item) })} placeholder="值" />}
              <button className="btn" onClick={() => patch({ headers: draft.headers.filter((_, itemIndex) => itemIndex !== index) })}>删除</button>
            </div>
          ))}
          <button className="btn" disabled={draft.headers.length >= MAX_RULES} onClick={() => patch({ headers: [...draft.headers, { action: 'override', name: '', value: '' }] })}>添加请求头规则</button>
        </> : <>
          <p className="muted">按点分路径改写 JSON 请求体（支持数组下标，如 <code>messages.0.content</code>）；值需是 JSON 字面量，字符串要加引号。</p>
          {draft.body.map((rule, index) => (
            <div className="toolbar" key={index}>
              <SearchableSelect ariaLabel="动作" className="combobox-inline" value={rule.action} options={BODY_ACTIONS} onChange={(action) => patch({ body: draft.body.map((item, itemIndex) => itemIndex === index ? { ...item, action } : item) })} />
              <input className="input" value={rule.path} onChange={(event) => patch({ body: draft.body.map((item, itemIndex) => itemIndex === index ? { ...item, path: event.target.value } : item) })} placeholder="路径，如 temperature" />
              {rule.action !== 'remove' && <input className="input wide" value={rule.value} onChange={(event) => patch({ body: draft.body.map((item, itemIndex) => itemIndex === index ? { ...item, value: event.target.value } : item) })} placeholder='值，如 0.7 或 "text"' />}
              <button className="btn" onClick={() => patch({ body: draft.body.filter((_, itemIndex) => itemIndex !== index) })}>删除</button>
            </div>
          ))}
          <button className="btn" disabled={draft.body.length >= MAX_RULES} onClick={() => patch({ body: [...draft.body, { action: 'override', path: '', value: '' }] })}>添加请求参数规则</button>
        </>}
      </>}

      {tab === 'cooldown' && <CooldownRulesPanel rules={draft.cooldown} onChange={(cooldown) => patch({ cooldown })} />}

      {tab === 'credential' && <CredentialPanel channelId={channelId} authType={authType} credential={oauthCredential} info={oauthCredentialInfo} onNotice={setNotice} />}

      {tab === 'other' && <>
        <label className="cell-stack">代理 URL<input className="input wide" value={draft.proxy_url} onChange={(event) => patch({ proxy_url: event.target.value })} placeholder="http/https/socks5/socks5h，留空直连" /></label>
        <div className="toolbar">
          <label className="cell-stack">可用时段开始<input className="input compact" type="time" value={draft.available_time_start} onChange={(event) => patch({ available_time_start: event.target.value })} /></label>
          <label className="cell-stack">结束<input className="input compact" type="time" value={draft.available_time_end} onChange={(event) => patch({ available_time_end: event.target.value })} /></label>
        </div>
        <p className="muted">HH:MM；留空表示全天可用，支持跨午夜时段，例如 22:00–08:00。</p>
        <label><input type="checkbox" checked={draft.retry_other_keys_on_failure} onChange={(event) => patch({ retry_other_keys_on_failure: event.target.checked })} /> 渠道故障时优先换 Key 重试</label>
        <label><input type="checkbox" checked={draft.scheduled_check_enabled} onChange={(event) => patch({ scheduled_check_enabled: event.target.checked })} /> 每日定时检测</label>
        {draft.scheduled_check_enabled && <div className="toolbar">
          <SearchableSelect ariaLabel="检测模型" className="combobox-inline" allowCustomInput value={draft.scheduled_check_model} options={models.map((model) => ({ value: model, label: model }))} onChange={(value) => patch({ scheduled_check_model: value })} placeholder="检测模型（留空自动）" />
          <label className="cell-stack">间隔（分钟）<input className="input compact" type="number" min={1} max={1440} step={1} value={draft.scheduled_check_interval_minutes} onChange={(event) => patch({ scheduled_check_interval_minutes: Number(event.target.value) })} /></label>
          <label className="cell-stack">开始时间<input className="input compact" type="time" step={60} value={draft.scheduled_check_start_time} onChange={(event) => patch({ scheduled_check_start_time: event.target.value })} /></label>
        </div>}
        {draft.scheduled_check_enabled && <p className="muted">按服务端时间，每天从开始时间按间隔执行至当天结束；错过的检测不补跑。</p>}

        {!isOAuth && <fieldset className="card">
          <legend>管理账户</legend>
          <SearchableSelect ariaLabel="面板类型" className="combobox-inline" value={draft.management.profile} options={PROFILES} onChange={(profile) => patchAccount({ profile })} />
          <p className="muted">选择上游面板类型后可刷新额度，New API 与 Sub2API Pro 还支持签到。</p>
          {draft.management.profile && <>
            <label className="cell-stack">面板地址<input className="input wide" type="url" value={draft.management.base_url} onChange={(event) => patchAccount({ base_url: event.target.value })} placeholder="https://panel.example.com" /></label>
            {draft.management.profile === 'new_api' && <>
              <label className="cell-stack">管理凭据（Access Token）<input className="input wide" value={draft.management.access_token} onChange={(event) => patchAccount({ access_token: event.target.value })} placeholder={draft.management.credential_configured ? '已配置，留空保留现有凭据' : ''} /></label>
              <label className="cell-stack">用户 ID（可选，作为 New-API-User 头）<input className="input compact" inputMode="numeric" value={draft.management.user_id} onChange={(event) => patchAccount({ user_id: event.target.value })} /></label>
            </>}
            {draft.management.profile.startsWith('sub2api') && <>
              <label className="cell-stack">邮箱<input className="input" type="email" value={draft.management.email} onChange={(event) => patchAccount({ email: event.target.value })} /></label>
              <label className="cell-stack">密码<input className="input" type="password" autoComplete="off" value={draft.management.password} onChange={(event) => patchAccount({ password: event.target.value })} /></label>
              <label className="cell-stack">TOTP（未开启两步验证可留空）<input className="input compact" inputMode="numeric" autoComplete="one-time-code" value={draft.management.totp_code} onChange={(event) => patchAccount({ totp_code: event.target.value })} /></label>
              <button className="btn" onClick={() => void login()}>登录</button>
              {draft.management.credential_configured && <p className="muted">已保存登录会话，留空表示保留。</p>}
            </>}
            {draft.management.profile !== 'sub2api' && <div className="toolbar">
              <label><input type="checkbox" checked={draft.management.daily_checkin_enabled} onChange={(event) => patchAccount({ daily_checkin_enabled: event.target.checked })} /> 每日签到</label>
              {draft.management.daily_checkin_enabled && <input className="input compact" type="time" value={draft.management.daily_checkin_time} onChange={(event) => patchAccount({ daily_checkin_time: event.target.value })} title="服务器本地时间" />}
            </div>}
          </>}
        </fieldset>}
      </>}
    </Dialog>
  )

  // Sub2API 登录只把会话写入草稿；渠道保存后才落库。
  async function login() {
    const account = draft.management
    if (!isPanelBaseURL(account.base_url)) { setTab('other'); setError('请先填写有效的面板地址'); return }
    if (!account.email || !account.password) { setError('请填写邮箱与密码'); return }
    setError(''); setNotice('登录中…')
    try {
      const result = await sub2apiLogin({ profile: account.profile, base_url: account.base_url.trim(), email: account.email, password: account.password, totp_code: account.totp_code || undefined, proxy_url: draft.proxy_url.trim() })
      patchAccount({ access_token: String(result.access_token ?? ''), refresh_token: String(result.refresh_token ?? ''), credential_configured: true, totp_code: '' })
      setNotice('登录成功，保存渠道后生效')
    } catch (cause) {
      setNotice('')
      // 开启两步验证但未填 TOTP 时，后端返回 409 totp_required（admin_channel_management.go:61）。
      const message = cause instanceof Error ? cause.message : '登录失败'
      setError(message.includes('totp_required') ? '该账户开启了两步验证，请填写 TOTP 后重试' : message)
    }
  }
}

// ---------------------------------------------------------------- 冷却规则

function CooldownRulesPanel({ rules, onChange }: { rules: CooldownRuleDraft[]; onChange: (rules: CooldownRuleDraft[]) => void }) {
  const [testStatus, setTestStatus] = useState(429)
  const [testBody, setTestBody] = useState('upstream status 429: {"error":{"message":"retry later"}}')
  const [testResult, setTestResult] = useState<unknown>(null)
  const set = (index: number, value: Partial<CooldownRuleDraft>) => onChange(rules.map((rule, itemIndex) => itemIndex === index ? { ...rule, ...value } : rule))
  const move = (index: number, delta: number) => {
    const target = index + delta
    if (target < 0 || target >= rules.length) return
    const next = [...rules]; [next[index], next[target]] = [next[target], next[index]]; onChange(next)
  }
  const runTest = async () => {
    const problem = validateAdvanced({ ...emptyAdvanced(), cooldown: rules })
    if (problem) { setTestResult({ error: problem.message }); return }
    const payload = advancedToPayload({ ...emptyAdvanced(), cooldown: rules }, true)
    try { setTestResult(await cooldownDetectionTest({ rules_source: 'channel', cooldown_detection_rules: payload.cooldown_detection_rules ?? { rules: [] }, status_code: testStatus, error_body: testBody })) }
    catch (cause) { setTestResult({ error: cause instanceof Error ? cause.message : '测试失败' }) }
  }
  return <>
    <p className="muted">规则按顺序匹配，首条命中即止。可按状态码或消息正则识别上游限流，并按固定时长或上游返回的重置时间冷却。</p>
    {rules.map((rule, index) => (
      <div className="card cell-stack" key={index}>
        <div className="toolbar">
          <strong>#{index + 1}</strong>
          <label><input type="checkbox" checked={rule.enabled} onChange={(event) => set(index, { enabled: event.target.checked })} /> 启用</label>
          <input className="input" value={rule.name} onChange={(event) => set(index, { name: event.target.value })} placeholder="规则名称（可选）" />
          <button className="btn" aria-label="上移" disabled={index === 0} onClick={() => move(index, -1)}>↑</button>
          <button className="btn" aria-label="下移" disabled={index === rules.length - 1} onClick={() => move(index, 1)}>↓</button>
          <button className="btn" onClick={() => onChange(rules.filter((_, itemIndex) => itemIndex !== index))}>删除</button>
        </div>
        <div className="toolbar">
          <input className="input" value={rule.status_codes} onChange={(event) => set(index, { status_codes: event.target.value })} placeholder="状态码，如 429, 503" />
          <input className="input wide" value={rule.message_pattern} onChange={(event) => set(index, { message_pattern: event.target.value })} placeholder="消息匹配正则（可选）" />
        </div>
        <div className="toolbar">
          <SearchableSelect ariaLabel="冷却范围" className="combobox-inline" value={rule.scope} options={SCOPES} onChange={(scope) => set(index, { scope })} />
          <SearchableSelect ariaLabel="冷却方式" className="combobox-inline" value={rule.mode} options={MODES} onChange={(mode) => set(index, { mode })} />
          {rule.mode === 'fixed'
            ? <input className="input compact" type="number" min={1} value={rule.cooldown_seconds} onChange={(event) => set(index, { cooldown_seconds: event.target.value })} placeholder="秒" />
            : <>
              <input className="input" value={rule.time_capture} onChange={(event) => set(index, { time_capture: event.target.value })} placeholder="时间捕获正则（含一个分组）" />
              <SearchableSelect ariaLabel="时间格式" className="combobox-inline" value={rule.time_format} options={TIME_FORMATS} onChange={(time_format) => set(index, { time_format })} />
              <input className="input compact" value={rule.time_layout} onChange={(event) => set(index, { time_layout: event.target.value })} placeholder="Go 时间布局" />
              <input className="input compact" value={rule.timezone} onChange={(event) => set(index, { timezone: event.target.value })} placeholder="时区" />
            </>}
        </div>
      </div>
    ))}
    <button className="btn" disabled={rules.length >= MAX_RULES} onClick={() => onChange([...rules, { enabled: true, name: '', status_codes: '429', message_pattern: '', scope: 'key', mode: 'fixed', cooldown_seconds: '60', time_capture: '', time_format: 'datetime', time_layout: '', timezone: '' }])}>添加冷却规则</button>
    <div className="card cell-stack">
      <strong>草稿测试</strong>
      <div className="toolbar">
        <label className="cell-stack">状态码<input className="input compact" type="number" min={100} max={599} value={testStatus} onChange={(event) => setTestStatus(Number(event.target.value))} /></label>
        <button className="btn" onClick={() => void runTest()}>测试</button>
      </div>
      <textarea className="input wide" rows={4} spellCheck={false} value={testBody} onChange={(event) => setTestBody(event.target.value)} placeholder="上游响应 JSON，或 upstream status N: {json} 格式的标准日志" />
      <p className="muted">测试只演算命中结果，不会写入任何冷却状态。</p>
      {testResult !== null && <pre className="result-pre">{JSON.stringify(testResult, null, 2)}</pre>}
    </div>
  </>
}

// ---------------------------------------------------------------- OAuth 凭证

function CredentialPanel({ channelId, authType, credential, info, onNotice }: { channelId: number | null; authType: string; credential?: unknown; info?: unknown; onNotice: (message: string) => void }) {
  const [view, setView] = useState<'decoded' | 'raw'>('decoded')
  const text = JSON.stringify(view === 'decoded' ? (info ?? credential ?? {}) : (credential ?? {}), null, 2)
  const refresh = async () => {
    const provider = CREDENTIAL_RESOURCE[authType]
    if (!channelId || !provider) return
    try { await refreshCredential(channelId, provider); onNotice('凭证已刷新，重新打开编辑器可查看最新内容') }
    catch (cause) { onNotice(cause instanceof Error ? cause.message : '刷新失败') }
  }
  return <>
    <p className="muted">默认显示解码视图；如需备份或复制用于重新导入，请切换到原始视图。AT 只读显示，完整凭证由 ccLoad 管理并自动刷新。</p>
    <div className="toolbar">
      <label><input type="radio" name="credential-view" checked={view === 'decoded'} onChange={() => setView('decoded')} /> 解码</label>
      <label><input type="radio" name="credential-view" checked={view === 'raw'} onChange={() => setView('raw')} /> 原始</label>
      <button className="btn" disabled={!channelId} onClick={() => void refresh()}>刷新凭证</button>
      <button className="btn" onClick={() => void navigator.clipboard?.writeText(text)}>复制</button>
    </div>
    <pre className="result-pre">{text}</pre>
  </>
}
