import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Dialog } from '../components/dialog'
import { SearchableSelect } from '../components/searchable-select'
import { getJSON, postJSON } from '../lib/api'
import { formatDateTime } from '../lib/format'
import { CooldownRulesPanel, advancedFromChannel, advancedToPayload, emptyAdvanced, validateAdvanced, type CooldownRuleDraft } from './channels/advanced-settings-dialog'

type Setting = { key: string; value: string; value_type?: string; default_value?: string; description?: string; editable?: boolean; disabled_reason?: string; configured?: boolean }

const COOLDOWN_RULES_KEY = 'global_cooldown_detection_rules'
const FALLBACK_KEY = 'model_multimodal_fallback'
const PRICING_KEY = 'model_custom_pricing'
const TYPESAFE_KEY = 'TypeSafe_api_key'
const MODAL_KEYS = new Set([FALLBACK_KEY, PRICING_KEY])
const BYTE_KEYS = new Set(['max_body_bytes', 'max_image_body_bytes', 'responses_ws_max_transcript_bytes'])
const OAUTH_URL_KEYS = new Set(['codex_base_url', 'xai_base_url', 'antigravity_url', 'anthropic_base_url'])
const OAUTH_PLACEHOLDERS: Record<string, string> = { codex_base_url: 'https://chatgpt.com/backend-api/codex/responses', xai_base_url: 'https://cli-chat-proxy.grok.com/v1', antigravity_url: 'https://daily-cloudcode-pa.googleapis.com', anthropic_base_url: 'https://api.anthropic.com' }
const MIB = 1024 * 1024
const MAX_SECONDS = 9223372036; const MAX_MINUTES = 153722867; const MAX_HOURS = 2562047

// 下拉类设置与取值（旧版 settings.js:selectSettingOptions）。
const SELECT_OPTIONS: Record<string, Array<{ value: string; label: string }>> = {
  auto_update_channel: [{ value: 'stable', label: '稳定版' }, { value: 'preview', label: '预览版' }],
  channel_stats_range: [{ value: 'today', label: '今天' }, { value: 'yesterday', label: '昨天' }, { value: 'day_before_yesterday', label: '前天' }, { value: 'this_week', label: '本周' }, { value: 'last_week', label: '上周' }, { value: 'this_month', label: '本月' }, { value: 'last_month', label: '上月' }],
  log_channel_click_action: [{ value: 'edit', label: '打开编辑器' }, { value: 'navigate', label: '跳转渠道页' }],
}
// 数值约束（旧版 settings.js:numericSettingConstraints）。
const CONSTRAINTS: Record<string, { min?: number; max?: number }> = {
  max_key_retries: { min: 1 }, max_concurrency: { min: 1 }, max_body_bytes: { min: 1 / MIB }, max_image_body_bytes: { min: 1 / MIB },
  http_read_timeout_seconds: { min: 0, max: MAX_SECONDS }, log_retention_days: { min: -1, max: 365 },
  cooldown_auth_seconds: { min: 1, max: MAX_SECONDS }, cooldown_server_seconds: { min: 1, max: MAX_SECONDS }, cooldown_timeout_seconds: { min: 1, max: MAX_SECONDS },
  cooldown_rate_limit_seconds: { min: 1, max: MAX_SECONDS }, cooldown_min_seconds: { min: 1, max: MAX_SECONDS }, cooldown_max_seconds: { min: 1, max: MAX_SECONDS },
  model_catalog_sync_interval_hours: { min: 0, max: MAX_HOURS }, auto_update_interval_hours: { min: 0, max: MAX_HOURS }, success_rate_penalty_weight: { min: 0 },
  health_score_window_minutes: { min: 1, max: MAX_MINUTES }, health_score_update_interval: { min: 1, max: MAX_SECONDS }, health_min_confident_sample: { min: 1 },
  ttfb_penalty_weight: { min: 0 }, ttfb_max_slow_ratio: { min: 0 }, ttfb_min_confident_sample: { min: 1 }, debug_log_retention_minutes: { min: 1, max: 1440 },
  auto_refresh_interval_seconds: { min: 0, max: MAX_SECONDS }, responses_ws_max_sessions: { min: 0 }, responses_ws_session_ttl_minutes: { min: 0, max: MAX_MINUTES },
  responses_ws_max_transcript_bytes: { min: 0 }, responses_ws_max_connections: { min: 0 }, responses_ws_max_connections_per_token: { min: 0 },
}
const ADVANCED_KEYS = new Set(['typesafe_enabled', 'typesafe_api_key', 'api_token_login_enabled', 'api_token_show_channels', 'auto_update_interval_hours', 'auto_update_channel', 'auto_refresh_interval_seconds', 'codex_map_429_to_503', 'model_catalog_sync_interval_hours', 'model_fuzzy_match'])
const GROUPS: Array<{ id: string; name: string; order: number; match: (key: string) => boolean }> = [
  { id: 'advanced', name: '高级', order: 70, match: (k) => ADVANCED_KEYS.has(k) },
  { id: 'channel', name: '渠道', order: 10, match: (k) => k.startsWith('channel_') || k === 'max_key_retries' },
  { id: 'upstream-connection', name: '上游连接', order: 19, match: (k) => k === 'upstream_connection_reuse_limit_seconds' || OAUTH_URL_KEYS.has(k) },
  { id: 'websocket', name: 'WebSocket', order: 25, match: (k) => k.startsWith('responses_ws_') },
  { id: 'stream-timeout', name: '流式超时', order: 20, match: (k) => k === 'stream_timeout' || k.endsWith('stream_idle_timeout') || k.endsWith('_first_byte_timeout') },
  { id: 'non-stream-timeout', name: '非流式超时', order: 21, match: (k) => k === 'non_stream_timeout' || k.endsWith('_non_stream_timeout') },
  { id: 'limits', name: '请求限制', order: 26, match: (k) => k === 'max_concurrency' || k.endsWith('_body_bytes') || k === 'http_read_timeout_seconds' },
  { id: 'health', name: '健康度', order: 30, match: (k) => k.includes('health_score') || k.includes('success_rate') || k.includes('penalty_weight') || k.includes('ttfb') || k === 'enable_health_score' || k === 'health_min_confident_sample' },
  { id: 'billing', name: '计费', order: 35, match: (k) => k === PRICING_KEY },
  { id: 'cooldown', name: '冷却', order: 40, match: (k) => k.startsWith('cooldown_') || k === COOLDOWN_RULES_KEY },
  { id: 'log', name: '日志', order: 50, match: (k) => k.startsWith('log_') || k.startsWith('debug_') },
  { id: 'access', name: '访问控制', order: 60, match: (k) => k.includes('auth_') },
]
const ORDER: Record<string, number> = {
  upstream_connection_reuse_limit_seconds: 90, antigravity_url: 100, anthropic_base_url: 110, codex_base_url: 120, xai_base_url: 130,
  upstream_first_byte_timeout: 100, stream_timeout: 101, stream_idle_timeout: 102, non_stream_timeout: 102, anthropic_first_byte_timeout: 110, anthropic_non_stream_timeout: 111, anthropic_stream_idle_timeout: 112,
  codex_first_byte_timeout: 120, codex_non_stream_timeout: 121, openai_first_byte_timeout: 130, openai_non_stream_timeout: 131, gemini_first_byte_timeout: 140, gemini_non_stream_timeout: 141,
  max_concurrency: 200, max_body_bytes: 201, max_image_body_bytes: 202, http_read_timeout_seconds: 203,
  cooldown_fallback_enabled: 300, cooldown_auth_seconds: 301, cooldown_server_seconds: 302, cooldown_timeout_seconds: 303, cooldown_rate_limit_seconds: 304, cooldown_min_seconds: 305, cooldown_max_seconds: 306, global_cooldown_detection_rules: 307,
  auto_update_interval_hours: 700, auto_update_channel: 701, api_token_login_enabled: 702, api_token_show_channels: 703, typesafe_enabled: 705, typesafe_api_key: 706, model_custom_pricing: 710,
}
const groupOf = (key: string) => GROUPS.find((group) => group.match(key.toLowerCase())) ?? GROUPS[0]
const isNumeric = (setting: Setting) => setting.value_type === 'int' || setting.value_type === 'float' || setting.value_type === 'duration' || BYTE_KEYS.has(setting.key)
const toDisplay = (setting: Setting, value: string) => BYTE_KEYS.has(setting.key) && Number.isFinite(Number(value)) ? String(Number(value) / MIB) : value
const toStorage = (setting: Setting, value: string) => BYTE_KEYS.has(setting.key) && Number.isFinite(Number(value)) ? String(Math.round(Number(value) * MIB)) : value
const constraintOf = (setting: Setting) => CONSTRAINTS[setting.key] ?? (setting.value_type === 'duration' ? { min: 0, max: MAX_SECONDS } : undefined)

/** 前端校验（旧版 settings.js:validateSettingInput）；返回空串表示通过。值为展示值（字节类为 MiB）。 */
function validateSetting(setting: Setting, value: string): string {
  if (SELECT_OPTIONS[setting.key]) return SELECT_OPTIONS[setting.key].some((option) => option.value === value) ? '' : '请从列表中选择'
  if (setting.key === 'channel_test_content' && !value.trim()) return '测试内容不能为空'
  if (OAUTH_URL_KEYS.has(setting.key.toLowerCase())) {
    const trimmed = value.trim()
    if (!trimmed) return ''
    try { const url = new URL(trimmed); if (url.protocol !== 'http:' && url.protocol !== 'https:') return '仅支持 http/https'; if (url.username || url.password) return '不能包含用户名或密码'; if (url.search || url.hash) return '不能包含查询参数或锚点' } catch { return 'URL 格式无效' }
    return ''
  }
  if (!isNumeric(setting)) return ''
  if (!value.trim()) return '请输入数值'
  const number = Number(value)
  if (!Number.isFinite(number)) return '请输入有效数值'
  if (!BYTE_KEYS.has(setting.key) && setting.value_type !== 'float' && !Number.isSafeInteger(number)) return '请输入整数'
  const constraint = constraintOf(setting)
  if (constraint?.min !== undefined && number < constraint.min) return `不能小于 ${constraint.min}`
  if (constraint?.max !== undefined && number > constraint.max) return `不能大于 ${constraint.max}`
  if (setting.key === 'log_retention_days' && number !== -1 && (number < 1 || number > 365)) return '取 -1（永久）或 1–365'
  if (BYTE_KEYS.has(setting.key)) {
    const bytes = Math.round(number * MIB)
    if (number !== 0 && bytes === 0) return '数值过小'
    if (setting.key !== 'responses_ws_max_transcript_bytes' && bytes < 1) return '至少 1 字节'
  }
  return ''
}

export function SettingsPage() {
  const [settings, setSettings] = useState<Setting[]>([])
  const [drafts, setDrafts] = useState<Record<string, string>>({})
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [loading, setLoading] = useState(false)
  const [notice, setNotice] = useState<{ kind: 'success' | 'error' | 'info'; text: string } | null>(null)
  const [dialog, setDialog] = useState<null | 'runtime' | 'cooldown' | 'fallback' | 'pricing'>(null)
  const [saving, setSaving] = useState(false)
  const [busyAction, setBusyAction] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    try { const data = await getJSON<Setting[]>('/admin/settings'); setSettings(Array.isArray(data) ? data : []); setDrafts({}); setErrors({}) }
    catch (cause) { setNotice({ kind: 'error', text: cause instanceof Error ? cause.message : '设置加载失败' }) } finally { setLoading(false) }
  }, [])
  useEffect(() => { void load() }, [load])

  const byKey = useMemo(() => new Map(settings.map((setting) => [setting.key, setting])), [settings])
  const groups = useMemo(() => {
    const map = new Map<string, { id: string; name: string; order: number; items: Setting[] }>()
    for (const setting of settings) {
      if (MODAL_KEYS.has(setting.key)) continue
      const group = groupOf(setting.key)
      if (!map.has(group.id)) map.set(group.id, { id: group.id, name: group.name, order: group.order, items: [] })
      map.get(group.id)!.items.push(setting)
    }
    const list = [...map.values()].sort((a, b) => a.order - b.order)
    list.forEach((group) => group.items.sort((a, b) => (ORDER[a.key.toLowerCase()] ?? 1000) - (ORDER[b.key.toLowerCase()] ?? 1000) || a.key.localeCompare(b.key)))
    return list
  }, [settings])

  // TypeSafe Key 后端不回显明文（configured 表示已配置），空输入表示保留。
  const displayValue = (setting: Setting) => drafts[setting.key] ?? (setting.key === TYPESAFE_KEY ? '' : toDisplay(setting, setting.value))
  const dirtyKeys = Object.keys(drafts).filter((key) => { const setting = byKey.get(key); return setting && (key === TYPESAFE_KEY ? drafts[key] !== '' || drafts[`${key}__reset`] !== undefined : toStorage(setting, drafts[key]) !== setting.value) }).filter((key) => !key.endsWith('__reset'))
  const update = (setting: Setting, value: string) => { setDrafts((current) => ({ ...current, [setting.key]: value })); setErrors((current) => { const next = { ...current }; delete next[setting.key]; return next }) }

  // 重置只在本地填入默认值并标记为待保存，与旧版一致；直接调用 /reset 会立即持久化并重启服务。
  const resetToDefault = (setting: Setting) => {
    if (setting.key === TYPESAFE_KEY) {
      setDrafts((current) => ({ ...current, [TYPESAFE_KEY]: '', [`${TYPESAFE_KEY}__reset`]: '1', ...(byKey.has('TypeSafe_enabled') ? { TypeSafe_enabled: 'false' } : {}) }))
      return
    }
    update(setting, toDisplay(setting, String(setting.default_value ?? '')))
  }

  const saveAll = async () => {
    const problems: Record<string, string> = {}
    const updates: Record<string, string> = {}
    for (const key of dirtyKeys) {
      const setting = byKey.get(key)!
      const value = drafts[key]
      const problem = validateSetting(setting, value)
      if (problem) problems[key] = problem
      else updates[key] = key === TYPESAFE_KEY ? value.trim() : toStorage(setting, value)
    }
    const min = Number(updates.cooldown_min_seconds ?? byKey.get('cooldown_min_seconds')?.value); const max = Number(updates.cooldown_max_seconds ?? byKey.get('cooldown_max_seconds')?.value)
    if (Number.isFinite(min) && Number.isFinite(max) && min > max) problems.cooldown_min_seconds = '最小冷却不能大于最大冷却'
    setErrors(problems)
    if (Object.keys(problems).length) { setNotice({ kind: 'error', text: '部分设置未通过校验，请检查标红的项' }); document.getElementById(`setting-${Object.keys(problems)[0]}`)?.focus(); return }
    if (!Object.keys(updates).length) { setNotice({ kind: 'info', text: '没有需要保存的改动' }); return }
    if (!window.confirm(`确定保存 ${Object.keys(updates).length} 项设置吗？多数设置保存后服务会自动重启。`)) return
    setSaving(true)
    try { const result = await postJSON<{ message?: string }>('/admin/settings/batch', updates); setNotice({ kind: 'success', text: result?.message ?? '设置已保存' }); window.setTimeout(() => void load(), 2500) }
    catch (cause) { setNotice({ kind: 'error', text: cause instanceof Error ? cause.message : '保存失败' }) } finally { setSaving(false) }
  }

  const checkUpdate = async () => {
    setBusyAction('update')
    try {
      const result = await postJSON<{ has_update?: boolean; latest_version?: string; current_version?: string; pending_restart?: boolean; message?: string }>('/admin/update/check', {})
      setNotice({ kind: 'info', text: result.pending_restart ? '新版本已下载，等待重启生效' : result.has_update ? `发现新版本 ${result.latest_version ?? ''}` : `已是最新版本${result.current_version ? `（${result.current_version}）` : ''}` })
    } catch (cause) { setNotice({ kind: 'error', text: cause instanceof Error ? cause.message : '检查更新失败' }) } finally { setBusyAction('') }
  }
  const testTypeSafe = async () => {
    setBusyAction('typesafe')
    // 输入为空时发 {}，测试已保存的 Key（旧版行为）。
    const key = (drafts[TYPESAFE_KEY] ?? '').trim()
    try { const result = await postJSON<{ valid?: boolean; message?: string }>('/admin/typesafe/test', key ? { api_key: key } : {}); setNotice({ kind: result.valid === false ? 'error' : 'success', text: result.message ?? (result.valid === false ? 'TypeSafe Key 无效' : 'TypeSafe Key 可用') }) }
    catch (cause) { setNotice({ kind: 'error', text: cause instanceof Error ? cause.message : '测试失败' }) } finally { setBusyAction('') }
  }

  const renderControl = (setting: Setting) => {
    const disabled = setting.editable === false
    const value = displayValue(setting)
    const invalid = Boolean(errors[setting.key])
    const id = `setting-${setting.key}`
    if (setting.key === COOLDOWN_RULES_KEY) {
      const count = (() => { try { return (JSON.parse(drafts[setting.key] ?? setting.value ?? '{}') as { rules?: unknown[] }).rules?.length ?? 0 } catch { return 0 } })()
      return <div className="toolbar" style={{ marginBottom: 0 }}><button className="btn" disabled={disabled} onClick={() => setDialog('cooldown')}>编辑规则</button><span className="muted">{count} 条规则</span></div>
    }
    if (setting.key === TYPESAFE_KEY) return <div className="toolbar" style={{ marginBottom: 0 }}>
      <input id={id} className="input" autoComplete="off" spellCheck={false} disabled={disabled} value={value} placeholder={drafts[`${TYPESAFE_KEY}__reset`] ? '将清除已保存的 Key' : setting.configured ? '已配置，留空保留' : '未配置'} onChange={(event) => update(setting, event.target.value)} aria-invalid={invalid} />
      <button className="btn" disabled={disabled || busyAction === 'typesafe'} onClick={() => void testTypeSafe()}>{busyAction === 'typesafe' ? '测试中…' : '测试'}</button>
    </div>
    if (SELECT_OPTIONS[setting.key]) return <div className="toolbar" style={{ marginBottom: 0 }}>
      <SearchableSelect id={id} ariaLabel={setting.key} className="combobox-inline" disabled={disabled} value={value} options={SELECT_OPTIONS[setting.key]} onChange={(next) => update(setting, next)} />
      {setting.key === 'auto_update_channel' && !disabled && <button className="btn" disabled={busyAction === 'update'} onClick={() => void checkUpdate()}>{busyAction === 'update' ? '检查中…' : '检查更新'}</button>}
    </div>
    if (setting.value_type === 'bool') {
      const on = value === 'true' || value === '1'
      return <div className="toolbar" style={{ marginBottom: 0 }} role="radiogroup" aria-label={setting.key}>
        <label><input type="radio" name={id} disabled={disabled} checked={on} onChange={() => update(setting, 'true')} /> 启用</label>
        <label><input type="radio" name={id} disabled={disabled} checked={!on} onChange={() => update(setting, 'false')} /> 禁用</label>
      </div>
    }
    if (isNumeric(setting)) {
      const constraint = constraintOf(setting)
      return <span className="toolbar" style={{ marginBottom: 0 }}><input id={id} className="input" type="number" required disabled={disabled} min={constraint?.min} max={constraint?.max} step={setting.value_type === 'float' || BYTE_KEYS.has(setting.key) ? 'any' : 1} value={value} onChange={(event) => update(setting, event.target.value)} aria-invalid={invalid} />{BYTE_KEYS.has(setting.key) && <span className="muted">MiB</span>}</span>
    }
    return <input id={id} className={`input${setting.key === 'channel_test_content' || OAUTH_URL_KEYS.has(setting.key.toLowerCase()) ? ' wide' : ''}`} disabled={disabled} value={value} placeholder={OAUTH_PLACEHOLDERS[setting.key.toLowerCase()]} onChange={(event) => update(setting, event.target.value)} aria-invalid={invalid} />
  }

  return <>
    <header className="page-header">
      <div><h1>系统设置</h1><p className="muted">多数设置保存后服务会自动重启；模型价格与多模态回退为热更新</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <button className="btn" onClick={() => setDialog('runtime')}>运行状态</button>
        <button className="btn" onClick={() => setDialog('pricing')}>自定义模型价格</button>
        <button className="btn" onClick={() => setDialog('fallback')}>多模态回退</button>
        <button className="btn" disabled={loading} onClick={() => void load()}>刷新</button>
        <button className="btn btn-primary" disabled={saving || !dirtyKeys.length} onClick={() => void saveAll()}>{saving ? '保存中…' : `保存${dirtyKeys.length ? `（${dirtyKeys.length}）` : ''}`}</button>
      </div>
    </header>
    {notice && <div className={`card ${notice.kind === 'error' ? 'error-text' : notice.kind === 'success' ? 'success-text' : ''}`} role={notice.kind === 'error' ? 'alert' : 'status'}><div className="job-summary" style={{ margin: 0 }}><span>{notice.text}</span><button className="link-button" onClick={() => setNotice(null)}>关闭</button></div></div>}

    {groups.length > 1 && <nav className="toolbar" aria-label="设置分组">{groups.map((group) => <button key={group.id} className="btn" onClick={() => document.getElementById(`settings-group-${group.id}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' })}>{group.name}</button>)}</nav>}

    {groups.map((group) => <section key={group.id} id={`settings-group-${group.id}`} className="card" style={{ marginBottom: 16 }}>
      <h2>{group.name}</h2>
      <div className="table-wrap"><table><thead><tr><th>配置项</th><th>值</th><th>说明</th><th>操作</th></tr></thead><tbody>
        {group.items.map((setting) => {
          const dirty = dirtyKeys.includes(setting.key)
          return <tr key={setting.key} className={dirty ? 'setting-dirty' : undefined}>
            <td><code>{setting.key}</code></td>
            <td className="cell-stack">{renderControl(setting)}{errors[setting.key] && <span className="error-text">{errors[setting.key]}</span>}
              {setting.disabled_reason === 'container_image_managed' && <span className="table-note">容器部署由镜像 tag 管理版本，请通过更新镜像升级。</span>}</td>
            <td className="setting-desc">{setting.description}{setting.default_value !== undefined && setting.default_value !== '' && setting.key !== TYPESAFE_KEY && <span className="table-note">默认：{toDisplay(setting, setting.default_value)}{BYTE_KEYS.has(setting.key) ? ' MiB' : ''}</span>}</td>
            <td><button className="btn btn-sm" disabled={setting.editable === false} onClick={() => resetToDefault(setting)}>恢复默认</button></td>
          </tr>
        })}
      </tbody></table></div>
    </section>)}

    <RuntimeMetricsDialog open={dialog === 'runtime'} onClose={() => setDialog(null)} />
    <GlobalCooldownDialog open={dialog === 'cooldown'} value={drafts[COOLDOWN_RULES_KEY] ?? byKey.get(COOLDOWN_RULES_KEY)?.value ?? '{}'} onClose={() => setDialog(null)} onApply={(value) => { const setting = byKey.get(COOLDOWN_RULES_KEY); if (setting) update(setting, value) }} />
    <FallbackDialog open={dialog === 'fallback'} value={byKey.get(FALLBACK_KEY)?.value ?? '{}'} onClose={() => setDialog(null)} onSaved={(text) => { setNotice({ kind: 'success', text }); void load() }} />
    <PricingDialog open={dialog === 'pricing'} value={byKey.get(PRICING_KEY)?.value ?? '{}'} onClose={() => setDialog(null)} onSaved={(text) => { setNotice({ kind: 'success', text }); void load() }} />
  </>
}

// ---------------------------------------------------------------- 运行状态

type MetricDef = { key: string; label: string; format?: 'duration' | 'seconds' | 'percent' | 'bytes' | 'durationNs' | 'boolean' | 'unixMs'; zeroUnavailable?: boolean }
const RUNTIME_DOMAINS: Array<{ source: string; title: string; optional?: boolean; metrics: MetricDef[] }> = [
  { source: 'process', title: '进程概览', metrics: [{ key: 'uptime_seconds', label: '运行时长', format: 'duration' }, { key: 'concurrency_slots_in_use', label: '已占用并发槽位' }, { key: 'max_concurrency', label: '全局并发上限' }, { key: 'goroutines', label: 'Goroutine 数' }] },
  { source: 'process', title: '资源占用', metrics: [{ key: 'cpu_usage_percent', label: 'CPU 占用率', format: 'percent' }, { key: 'cpu_user_seconds', label: '用户态 CPU 时间', format: 'seconds' }, { key: 'cpu_system_seconds', label: '内核态 CPU 时间', format: 'seconds' }, { key: 'rss_bytes', label: '当前物理内存 (RSS)', format: 'bytes', zeroUnavailable: true }, { key: 'max_rss_bytes', label: '峰值物理内存', format: 'bytes', zeroUnavailable: true }, { key: 'heap_alloc_bytes', label: 'Go 堆已分配', format: 'bytes' }, { key: 'heap_sys_bytes', label: 'Go 堆保留空间', format: 'bytes' }, { key: 'gc_count', label: 'GC 次数' }, { key: 'gc_pause_total_ns', label: 'GC 暂停累计', format: 'durationNs' }, { key: 'gc_cpu_percent', label: 'GC CPU 占比', format: 'percent' }, { key: 'sse_framing_repairs', label: 'SSE 分帧修复次数' }] },
  { source: 'http_proxy', title: 'HTTP 代理', metrics: [{ key: 'active_requests', label: '当前客户端 HTTP 请求' }, { key: 'completed_requests', label: '已完成 HTTP 请求' }, { key: 'non_error_responses', label: '非错误响应' }, { key: 'client_error_responses', label: '4xx 响应' }, { key: 'server_error_responses', label: '5xx 响应' }, { key: 'streaming_requests', label: '流式请求' }, { key: 'non_streaming_requests', label: '非流式请求' }, { key: 'request_body_bytes', label: '已解析请求体', format: 'bytes' }, { key: 'response_body_bytes', label: '已写响应体', format: 'bytes' }] },
  { source: 'logs', title: '异步日志', metrics: [{ key: 'backlog_entries', label: '日志队列积压' }, { key: 'queue_capacity_entries', label: '日志队列容量' }, { key: 'dropped_entries', label: '已丢弃日志' }, { key: 'persistence_failed_entries', label: '持久化失败日志' }] },
  { source: 'storage', title: '混合存储', optional: true, metrics: [{ key: 'primary_sync_pending', label: '主库同步积压' }, { key: 'primary_sync_failures', label: '主库同步失败' }, { key: 'primary_sync_dropped', label: '主库同步丢弃' }, { key: 'sqlite_read_failures', label: 'SQLite 分析读取失败' }, { key: 'analytics_reads_primary', label: '分析查询使用主库', format: 'boolean' }, { key: 'primary_sync_last_success_unix_ms', label: '最近同步成功', format: 'unixMs' }] },
]
const RESPONSES_GROUPS: Array<{ title: string; metrics: MetricDef[] }> = [
  { title: '执行会话', metrics: [{ key: 'sessions', label: '当前会话数' }, { key: 'max_sessions', label: '会话数上限' }, { key: 'active_attachments', label: '活跃会话挂载数' }] },
  { title: '下游 WebSocket 连接', metrics: [{ key: 'downstream_connections', label: '当前下游连接数' }, { key: 'max_downstream_connections', label: '下游连接总上限' }, { key: 'max_downstream_connections_per_token', label: '单令牌连接上限' }, { key: 'rejected_downstream_connections', label: '已拒绝下游连接数' }] },
  { title: '上游原生 WebSocket', metrics: [{ key: 'upstream_connections', label: '当前上游物理连接数' }, { key: 'upstream_handshakes', label: '上游握手次数' }, { key: 'upstream_reuses', label: '上游连接复用次数' }, { key: 'reconnects', label: '上游重连次数' }, { key: 'upstream_heartbeat_failures', label: '上游心跳失败次数' }, { key: 'upstream_queued_read_bytes', label: '上游待读队列字节数', format: 'bytes' }, { key: 'oldest_upstream_connection_seconds', label: '最老上游连接存活时间', format: 'duration' }] },
  { title: '会话累计事件', metrics: [{ key: 'ttl_expired', label: 'TTL 过期会话' }, { key: 'capacity_rejected', label: '会话容量拒绝' }, { key: 'budget_rejected', label: 'Transcript 预算拒绝' }, { key: 'previous_response_misses', label: 'Previous response 未命中' }] },
]
const formatBytes = (value: number) => value >= 1024 ** 3 ? `${(value / 1024 ** 3).toFixed(2)} GiB` : value >= MIB ? `${(value / MIB).toFixed(1)} MiB` : value >= 1024 ? `${(value / 1024).toFixed(1)} KiB` : `${value} B`
const formatDuration = (seconds: number) => { const s = Math.floor(seconds); const d = Math.floor(s / 86400); const h = Math.floor((s % 86400) / 3600); const m = Math.floor((s % 3600) / 60); return d ? `${d}天${h}小时` : h ? `${h}小时${m}分` : m ? `${m}分${s % 60}秒` : `${s}秒` }
function formatMetric(raw: unknown, def: MetricDef): string {
  if (def.format === 'boolean') return raw === true ? '是' : raw === false ? '否' : '—'
  const value = Number(raw)
  if (raw == null || !Number.isFinite(value) || (def.zeroUnavailable && value === 0)) return '—'
  switch (def.format) {
    case 'duration': return formatDuration(value)
    case 'seconds': return `${value.toFixed(2)} s`
    case 'percent': return `${value.toFixed(1)}%`
    case 'bytes': return formatBytes(value)
    case 'durationNs': return value >= 1e9 ? `${(value / 1e9).toFixed(2)} s` : `${(value / 1e6).toFixed(1)} ms`
    case 'unixMs': return value > 0 ? formatDateTime(value, true) : '—'
    default: return value.toLocaleString()
  }
}

function MetricGrid({ data, metrics }: { data: Record<string, unknown>; metrics: MetricDef[] }) {
  return <div className="runtime-metrics-cards">{metrics.map((def) => <div key={def.key} className="runtime-metric-card" title={def.key}><span className="muted">{def.label}</span><strong>{formatMetric(data[def.key], def)}</strong></div>)}</div>
}

/** 运行状态：每 3 秒自动刷新（旧版 settings.js RUNTIME_METRICS_REFRESH_MS）。 */
function RuntimeMetricsDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [data, setData] = useState<Record<string, Record<string, unknown>> | null>(null)
  const [updatedAt, setUpdatedAt] = useState(0)
  const [error, setError] = useState('')
  const timer = useRef(0)
  const refresh = useCallback(async () => {
    try { setData(await getJSON<Record<string, Record<string, unknown>>>('/admin/runtime-metrics')); setUpdatedAt(Date.now()); setError('') }
    catch (cause) { setError(cause instanceof Error ? cause.message : '加载失败') }
  }, [])
  useEffect(() => {
    if (!open) return
    void refresh()
    timer.current = window.setInterval(() => { if (!document.hidden) void refresh() }, 3000)
    return () => window.clearInterval(timer.current)
  }, [open, refresh])
  const responses = (data?.responses_websocket ?? {}) as Record<string, unknown>
  const used = Number(responses.transcript_bytes); const budget = Number(responses.max_transcript_bytes)
  const percent = Number.isFinite(used) && Number.isFinite(budget) && budget > 0 ? (used / budget) * 100 : null
  return <Dialog open={open} onClose={onClose} size="xl" title="运行状态" description={updatedAt ? `更新于 ${new Date(updatedAt).toLocaleTimeString()} · 每 3 秒自动刷新` : undefined}
    footer={<><button className="btn" onClick={() => void refresh()}>刷新</button><button className="btn" onClick={onClose}>关闭</button></>}>
    {error && <p className="error-text">{error}</p>}
    {!data ? <p className="muted">加载中…</p> : <div className="runtime-metrics-grid">
      {RUNTIME_DOMAINS.filter((domain) => !domain.optional || data[domain.source]).map((domain) => <section key={domain.title} className="runtime-metrics-section"><h3>{domain.title}</h3><MetricGrid data={(data[domain.source] ?? {}) as Record<string, unknown>} metrics={domain.metrics} /></section>)}
      {data.responses_websocket && <section className="runtime-metrics-section">
        <h3>Responses 执行与 WebSocket</h3>
        <div className="card cell-stack">
          <strong>Transcript 预算：{Number.isFinite(used) ? formatBytes(used) : '—'} / {Number.isFinite(budget) ? formatBytes(budget) : '—'}{percent != null && `（${percent.toFixed(1)}%）`}</strong>
          <progress max={100} value={percent == null ? 0 : Math.min(100, percent)} className={percent != null && percent > 100 ? 'progress-bad' : percent != null && percent >= 80 ? 'progress-warn' : undefined} style={{ width: '100%' }} />
          <span className={percent != null && percent > 100 ? 'error-text' : percent != null && percent >= 80 ? 'warn-text' : 'muted'}>{percent == null ? '不可用' : percent > 100 ? '已超出预算' : percent >= 80 ? '接近预算上限' : '正常'}</span>
        </div>
        {RESPONSES_GROUPS.map((group) => <div key={group.title}><h4>{group.title}</h4><MetricGrid data={responses} metrics={group.metrics} /></div>)}
      </section>}
    </div>}
  </Dialog>
}

// ---------------------------------------------------------------- 全局冷却规则（草稿，随批量保存提交）

function GlobalCooldownDialog({ open, value, onClose, onApply }: { open: boolean; value: string; onClose: () => void; onApply: (value: string) => void }) {
  const [rules, setRules] = useState<CooldownRuleDraft[]>([])
  const [error, setError] = useState('')
  useEffect(() => {
    if (!open) return
    let parsed: unknown = {}
    try { parsed = JSON.parse(value || '{}') } catch { parsed = {} }
    setRules(advancedFromChannel({ cooldown_detection_rules: parsed }, undefined).cooldown); setError('')
  }, [open, value])
  const apply = () => {
    const draft = { ...emptyAdvanced(), cooldown: rules }
    const problem = validateAdvanced(draft)
    if (problem) { setError(problem.message); return }
    onApply(JSON.stringify(advancedToPayload(draft, true).cooldown_detection_rules ?? { rules: [] })); onClose()
  }
  return <Dialog open={open} onClose={onClose} size="lg" closeOnBackdrop={false} title="全局冷却检测规则" description="渠道未配置自己的规则时使用。应用后需点击页面上的「保存」才会生效。"
    footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={apply}>应用</button></>}>
    {error && <p className="error-text" role="alert">{error}</p>}
    <CooldownRulesPanel rules={rules} onChange={setRules} rulesSource="global" />
  </Dialog>
}

// ---------------------------------------------------------------- 多模态回退（热更新，立即保存）

const normalizeModel = (value: string) => value.trim().toLowerCase().replace(/\s*\([^()]*\)\s*$/, '')

function FallbackDialog({ open, value, onClose, onSaved }: { open: boolean; value: string; onClose: () => void; onSaved: (message: string) => void }) {
  const [rows, setRows] = useState<Array<{ from: string; to: string }>>([])
  const [models, setModels] = useState<string[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!open) return
    try { setRows(Object.entries(JSON.parse(value || '{}') as Record<string, unknown>).map(([from, to]) => ({ from, to: String(to ?? '') }))) } catch { setRows([]) }
    setError('')
    void getJSON<{ models?: string[] }>('/admin/channels/filter-options', { status: 'enabled' }).then((data) => setModels(data.models ?? [])).catch(() => setModels([]))
  }, [open, value])
  const options = useMemo(() => [...new Set([...models, ...rows.flatMap((row) => [row.from, row.to]).filter(Boolean)])].sort().map((name) => ({ value: name, label: name })), [models, rows])
  const save = async () => {
    if (rows.length > 64) { setError('最多 64 条映射'); return }
    const seen = new Set<string>()
    for (const [index, row] of rows.entries()) {
      if (!row.from.trim() || !row.to.trim()) { setError(`第 ${index + 1} 行需同时选择源模型与回退模型`); return }
      if (normalizeModel(row.from) === normalizeModel(row.to)) { setError(`第 ${index + 1} 行不能映射到自身`); return }
      const key = normalizeModel(row.from)
      if (seen.has(key)) { setError(`源模型 ${row.from} 重复`); return }
      seen.add(key)
    }
    const next = JSON.stringify(Object.fromEntries(rows.map((row) => [row.from.trim(), row.to.trim()])))
    if (next === JSON.stringify(JSON.parse(value || '{}'))) { onClose(); return }
    setBusy(true)
    try { await postJSON('/admin/settings/batch', { [FALLBACK_KEY]: next }); onSaved('多模态回退已保存（热更新，无需重启）'); onClose() }
    catch (cause) { setError(cause instanceof Error ? cause.message : '保存失败') } finally { setBusy(false) }
  }
  return <Dialog open={open} onClose={onClose} size="md" closeOnBackdrop={false} title="多模态回退" description="当请求含图片而源模型不支持时，改用回退模型。保存后立即生效。"
    footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={busy} onClick={() => void save()}>保存</button></>}>
    {error && <p className="error-text" role="alert">{error}</p>}
    {rows.map((row, index) => <div className="toolbar" key={index}>
      <SearchableSelect ariaLabel="源模型" className="combobox-inline" allowCustomInput value={row.from} options={options} onChange={(from) => setRows(rows.map((item, itemIndex) => itemIndex === index ? { ...item, from } : item))} placeholder="源模型" />
      <span className="muted">→</span>
      <SearchableSelect ariaLabel="回退模型" className="combobox-inline" allowCustomInput value={row.to} options={options} onChange={(to) => setRows(rows.map((item, itemIndex) => itemIndex === index ? { ...item, to } : item))} placeholder="回退模型" />
      <button className="btn" onClick={() => setRows(rows.filter((_, itemIndex) => itemIndex !== index))}>删除</button>
    </div>)}
    <button className="btn" disabled={rows.length >= 64} onClick={() => setRows([...rows, { from: '', to: '' }])}>添加映射</button>
  </Dialog>
}

// ---------------------------------------------------------------- 自定义模型价格（热更新，立即保存）

const BASIC = ['input_price', 'output_price', 'cache_read_price', 'cache_write_price'] as const
const HIGH = ['input_price_high', 'output_price_high', 'cache_read_price_high', 'cache_write_price_high'] as const
const PRICE_LABELS: Record<string, string> = { input_price: '输入', output_price: '输出', cache_read_price: '缓存读', cache_write_price: '缓存写', input_price_high: '输入(高)', output_price_high: '输出(高)', cache_read_price_high: '缓存读(高)', cache_write_price_high: '缓存写(高)' }
const MODEL_ID = /^[a-z0-9][a-z0-9._:/-]*$/
type PriceRow = { id: string; values: Record<string, string>; tiered?: boolean }

function PricingDialog({ open, value, onClose, onSaved }: { open: boolean; value: string; onClose: () => void; onSaved: (message: string) => void }) {
  const [rows, setRows] = useState<PriceRow[]>([])
  const [filter, setFilter] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!open) return
    try { setRows(Object.entries(JSON.parse(value || '{}') as Record<string, Record<string, number>>).map(([id, prices]) => ({ id, values: Object.fromEntries(Object.entries(prices).map(([key, price]) => [key, String(price)])) }))) } catch { setRows([]) }
    setError(''); setFilter('')
  }, [open, value])
  const set = (index: number, patch: Partial<PriceRow>) => setRows((current) => current.map((row, itemIndex) => itemIndex === index ? { ...row, ...patch } : row))
  // 模型 ID 确认后预填系统默认价，并剔除契约外字段；分层定价模型不能覆盖。
  const prefill = async (index: number) => {
    const id = rows[index]?.id.trim().toLowerCase()
    if (!id || Object.values(rows[index].values).some((item) => item !== '')) return
    try {
      const result = await getJSON<{ found?: boolean; pricing?: Record<string, unknown> }>('/admin/model-pricing', { model: id })
      if (Array.isArray(result.pricing?.token_pricing_tiers) && result.pricing.token_pricing_tiers.length) { set(index, { tiered: true }); return }
      if (!result.found || !result.pricing) return
      set(index, { tiered: false, values: Object.fromEntries([...BASIC, ...HIGH].filter((key) => result.pricing?.[key] != null).map((key) => [key, String(result.pricing?.[key])])) })
    } catch { /* 预填失败不影响手动输入 */ }
  }
  const save = async () => {
    const output: Record<string, Record<string, number>> = {}
    for (const row of rows) {
      const id = row.id.trim().toLowerCase()
      if (!MODEL_ID.test(id)) { setError(`模型 ID「${row.id}」格式无效（小写字母数字开头，仅含 . _ : / -）`); return }
      if (output[id]) { setError(`模型 ${id} 重复`); return }
      if (row.tiered) { setError(`模型 ${id} 使用分层定价，不支持自定义覆盖`); return }
      const prices: Record<string, number> = {}
      for (const [key, raw] of Object.entries(row.values)) {
        if (raw === '') continue
        const price = Number(raw)
        if (!Number.isFinite(price) || price < 0) { setError(`${id} 的${PRICE_LABELS[key]}价格无效`); return }
        prices[key] = price
      }
      if (!Object.keys(prices).length) { setError(`模型 ${id} 至少需要一个价格`); return }
      output[id] = prices
    }
    if (Object.keys(output).length > 512) { setError('最多 512 个模型'); return }
    const next = JSON.stringify(output)
    if (new Blob([next]).size > 1024 * 1024) { setError('价格配置超过 1MiB'); return }
    let previous = '{}'; try { previous = JSON.stringify(JSON.parse(value || '{}')) } catch { /* 旧值无效时按变更处理 */ }
    if (next === previous) { onClose(); return }
    setBusy(true)
    try { await postJSON('/admin/settings/batch', { [PRICING_KEY]: next }); onSaved('自定义模型价格已保存（热更新，无需重启）'); onClose() }
    catch (cause) { setError(cause instanceof Error ? cause.message : '保存失败') } finally { setBusy(false) }
  }
  const visible = rows.map((row, index) => ({ row, index })).filter(({ row }) => !filter.trim() || row.id.toLowerCase().includes(filter.trim().toLowerCase()))
  return <Dialog open={open} onClose={onClose} size="xl" closeOnBackdrop={false} title="自定义模型价格" description="单位为美元 / 百万 token，覆盖系统内置价格。输入模型 ID 后回车或失焦可预填系统默认价。保存后立即生效。"
    footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={busy} onClick={() => void save()}>保存</button></>}>
    {error && <p className="error-text" role="alert">{error}</p>}
    <div className="toolbar"><input className="input" value={filter} onChange={(event) => setFilter(event.target.value)} placeholder="搜索模型" aria-label="搜索模型" /><button className="btn" onClick={() => setRows([...rows, { id: '', values: {} }])}>添加模型</button><span className="muted">{rows.length} / 512</span></div>
    <div className="table-wrap"><table><thead><tr><th>模型 ID</th>{[...BASIC, ...HIGH].map((key) => <th key={key}>{PRICE_LABELS[key]}</th>)}<th /></tr></thead><tbody>
      {visible.map(({ row, index }) => <tr key={index}>
        <td className="cell-stack"><input className="input" value={row.id} onChange={(event) => set(index, { id: event.target.value, tiered: false })} onBlur={() => void prefill(index)} onKeyDown={(event) => { if (event.key === 'Enter') void prefill(index) }} placeholder="claude-sonnet-5" />{row.tiered && <span className="error-text table-note">分层定价，不可覆盖</span>}</td>
        {[...BASIC, ...HIGH].map((key) => <td key={key}><input className="input compact" type="number" min={0} step="any" value={row.values[key] ?? ''} onChange={(event) => set(index, { values: { ...row.values, [key]: event.target.value } })} /></td>)}
        <td><button className="btn btn-sm" onClick={() => setRows(rows.filter((_, itemIndex) => itemIndex !== index))}>删除</button></td>
      </tr>)}
      {!visible.length && <tr><td colSpan={10} className="muted">暂无自定义价格</td></tr>}
    </tbody></table></div>
  </Dialog>
}
