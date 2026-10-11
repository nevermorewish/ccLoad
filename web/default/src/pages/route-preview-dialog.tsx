import { useEffect, useMemo, useRef, useState } from 'react'
import { Dialog } from '../components/dialog'
import { SearchableSelect } from '../components/searchable-select'
import { getJSON, postJSON } from '../lib/api'
import type { Channel } from '../types'
import { channelModelNames } from './reasoning-settings-dialog'

const protocols = [{ value: 'anthropic', label: 'Anthropic Messages' }, { value: 'codex', label: 'Codex Responses' }, { value: 'openai', label: 'OpenAI Chat' }, { value: 'gemini', label: 'Gemini' }]
type Token = { id: number; description?: string; is_active: boolean; expires_at?: number }
type Candidate = { id: number; name: string; priority: number; effective_priority: number; available_key_count: number; priority_group: number; cooldown_fallback: boolean; same_tier_rotation: boolean }
type Excluded = { id: number; name: string; priority: number; reason: string; until?: string }
type Preview = { model: string; client_protocol: string; model_denied: boolean; candidates: Candidate[]; excluded: Excluded[]; limitations: string[] }
const reasons: Record<string, string> = {
  token_channel_scope: '令牌不允许此渠道', cost_limit: '渠道成本限额已达上限', time_window: '不在渠道可用时段', key_load_failed: 'Key 加载失败', no_model_row: '无可用模型行', model_disabled: '模型已停用', credits_not_applicable: 'Credits 渠道不承接此模型', keys_disabled: 'Key 全部停用', key_model_scope: 'Key 不允许此模型', channel_cooldown: '渠道冷却中', model_cooldown: '模型冷却中', all_keys_cooling: 'Key 全部冷却中',
}
const limitations: Record<string, string> = {
  session_affinity: '会话粘性', execution_session_pinning: '执行会话绑定', channel_header: '请求头指定渠道', antigravity_credits: 'Credits 追加候选', key_rotation: 'Key 轮换', model_row_rotation: '模型行轮换', live_rpm_concurrency: '实时 RPM 与并发占用', protocol_capabilities: '协议能力与请求转换', token_budget: '令牌费用限额',
}

export function buildRoutePreviewPayload(model: string, protocol: string, token: string, multimodal: boolean) {
  if (!model.trim()) throw new Error('请选择模型')
  if (!protocols.some((entry) => entry.value === protocol)) throw new Error('请选择有效协议')
  if (token && (!Number.isSafeInteger(Number(token)) || Number(token) <= 0)) throw new Error('请选择有效令牌')
  return { model: model.trim(), client_protocol: protocol, multimodal, ...(token ? { auth_token_id: Number(token) } : {}) }
}

export function RoutePreviewDialog({ open, channels, onClose, onEditChannel }: { open: boolean; channels: Channel[]; onClose: () => void; onEditChannel: (channel: { id: number; name: string }) => void }) {
  const [model, setModel] = useState('')
  const [protocol, setProtocol] = useState('anthropic')
  const [token, setToken] = useState('')
  const [multimodal, setMultimodal] = useState(false)
  const [tokens, setTokens] = useState<Token[]>([])
  const [loading, setLoading] = useState(false)
  const [loadError, setLoadError] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<Preview | null>(null)
  const sequence = useRef(0)
  const models = useMemo(() => [...new Set(channels.filter((channel) => channel.auth_type !== 'system_one').flatMap(channelModelNames))].sort(), [channels])
  useEffect(() => {
    if (!open) return
    let cancelled = false
    setLoading(true); setLoadError('')
    void getJSON<{ tokens: Token[] }>('/admin/auth-tokens', { range: 'all' }).then((data) => { if (!cancelled) setTokens(data.tokens.filter((entry) => entry.is_active && (!entry.expires_at || entry.expires_at > Date.now()))) })
      .catch((cause) => { if (!cancelled) setLoadError(cause instanceof Error ? cause.message : '令牌加载失败') })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [open])
  useEffect(() => { sequence.current++; setResult(null); setError(''); setBusy(false) }, [open, model, protocol, token, multimodal])
  const run = async () => {
    const seq = ++sequence.current
    setBusy(true); setError(''); setResult(null)
    try {
      const payload = buildRoutePreviewPayload(model, protocol, token, multimodal)
      const data = await postJSON<Preview>('/admin/route-preview', payload)
      if (seq === sequence.current) setResult(data)
    } catch (cause) { if (seq === sequence.current) setError(cause instanceof Error ? cause.message : '预演失败') }
    finally { if (seq === sequence.current) setBusy(false) }
  }
  return <Dialog open={open} onClose={onClose} size="xl" title="普通生成路由预演" footer={<><button className="btn" onClick={onClose}>关闭</button><button className="btn btn-primary" disabled={busy || !model || loading || Boolean(loadError)} onClick={() => void run()}>{busy ? '预演中…' : '预演路由'}</button></>}>
    <p className="muted">检查普通生成请求的候选渠道与排除原因。预演不会调用上游或消耗请求额度。</p>
    <div className="toolbar">
      <SearchableSelect ariaLabel="预演模型" value={model} options={models.map((name) => ({ value: name, label: name }))} onChange={setModel} placeholder="选择模型" />
      <SearchableSelect ariaLabel="预演协议" value={protocol} options={protocols} onChange={setProtocol} />
      <SearchableSelect ariaLabel="预演令牌" value={token} options={[{ value: '', label: '不限制令牌范围' }, ...tokens.map((entry) => ({ value: String(entry.id), label: entry.description || `令牌 #${entry.id}` }))]} onChange={setToken} disabled={loading || Boolean(loadError)} />
      <label><input type="checkbox" checked={multimodal} onChange={(event) => setMultimodal(event.target.checked)} /> 包含图片等非文本内容</label>
    </div>
    {(error || loadError) && <p className="error-text" role="alert">{error || loadError}</p>}
    {result && <>
      <p>实际路由模型：<strong>{result.model}</strong>{result.model_denied && <span className="error-text"> · 该令牌不允许使用此模型</span>}</p>
      <div className="table-wrap"><table><thead><tr><th>渠道</th><th>优先级（有效 / 配置）</th><th>可用 Key</th><th>状态 / 原因</th><th>冷却截止</th></tr></thead><tbody>
        {result.candidates.map((entry) => <tr key={entry.id}><td><button className="link-button" onClick={() => onEditChannel(entry)}>{entry.name}</button></td><td>{entry.effective_priority} / {entry.priority}<span className="table-note">同档分组 {entry.priority_group}</span></td><td>{entry.available_key_count}</td><td>{entry.cooldown_fallback ? '全冷却兜底候选' : '候选'}{entry.same_tier_rotation ? ' · 同档轮换' : ''}</td><td>—</td></tr>)}
        {result.excluded.map((entry) => <tr key={entry.id}><td><button className="link-button" onClick={() => onEditChannel(entry)}>{entry.name}</button></td><td>— / {entry.priority}</td><td>—</td><td>已排除 · {reasons[entry.reason] ?? entry.reason}</td><td>{entry.until ? new Date(entry.until).toLocaleString() : '—'}</td></tr>)}
        {!result.candidates.length && !result.excluded.length && <tr><td colSpan={5} className="muted">{result.model_denied ? '模型授权拒绝，未选择渠道' : '没有匹配的渠道'}</td></tr>}
      </tbody></table></div>
      <details><summary>预演范围与限制</summary><p>同档渠道顺序不代表实际请求首选。以下因素未纳入本次预演：{result.limitations.map((item) => limitations[item] ?? item).join('、')}。</p></details>
    </>}
  </Dialog>
}
