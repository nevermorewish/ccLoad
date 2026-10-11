import { useEffect, useMemo, useState } from 'react'
import { Dialog } from '../components/dialog'
import { SearchableSelect } from '../components/searchable-select'
import { SearchableMultiSelect } from '../components/searchable-multi-select'
import { getJSON } from '../lib/api'
import type { Channel } from '../types'

export type ReasoningConfig = { enabled: boolean; decision_channel_id: number; decision_model: string; business_models: string[]; timeout_ms: number }
export const reasoningDefaults: ReasoningConfig = { enabled: false, decision_channel_id: 0, decision_model: '', business_models: [], timeout_ms: 2000 }
const thinkingSuffix = /\((?:auto|none|minimal|low|medium|high|xhigh|max|-?\d+)\)$/i

export function parseReasoningConfig(raw: string): ReasoningConfig {
  const value = JSON.parse(raw) as ReasoningConfig
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('动态思考配置必须为 JSON 对象')
  const config = { ...reasoningDefaults, ...value }
  if (typeof config.enabled !== 'boolean' || !Number.isSafeInteger(config.decision_channel_id) || config.decision_channel_id < 0 || typeof config.decision_model !== 'string'
    || !Array.isArray(config.business_models) || config.business_models.some((name) => typeof name !== 'string' || !name.trim() || /[*?]/.test(name) || thinkingSuffix.test(name.trim()))
    || !Number.isSafeInteger(config.timeout_ms) || config.timeout_ms < 100 || config.timeout_ms > 10000) throw new Error('请检查模型名单与超时（100–10000 ms），模型不可包含通配符或思考后缀')
  config.decision_model = config.decision_model.trim()
  config.business_models = config.business_models.map((name) => name.trim())
  if (new Set(config.business_models).size !== config.business_models.length) throw new Error('业务模型不能重复')
  if (config.enabled && (!config.decision_channel_id || !config.decision_model || !config.business_models.length)) throw new Error('启用时请选择决策渠道、决策模型和业务模型')
  return config
}

export function channelModelNames(channel: Channel): string[] {
  return (channel.models ?? []).filter((entry) => typeof entry === 'string' || !(entry as { disabled?: boolean }).disabled)
    .map((entry) => (typeof entry === 'string' ? entry : String(entry.model ?? '')).replace(thinkingSuffix, '')).filter(Boolean)
}

export function ReasoningSettingsDialog({ open, value, onClose, onApply }: { open: boolean; value: string; onClose: () => void; onApply: (value: string) => void }) {
  const [draft, setDraft] = useState<ReasoningConfig | null>(null)
  const [channels, setChannels] = useState<Channel[]>([])
  const [error, setError] = useState('')
  const [loadError, setLoadError] = useState('')
  const [loading, setLoading] = useState(false)
  useEffect(() => {
    if (!open) return
    try { setDraft(parseReasoningConfig(value)); setError('') } catch (cause) { setDraft(null); setError(String(cause)) }
    let cancelled = false
    setLoading(true); setLoadError(''); setChannels([])
    void getJSON<Channel[]>('/admin/channels').then((rows) => { if (!cancelled) setChannels(rows) })
      .catch((cause) => { if (!cancelled) setLoadError(cause instanceof Error ? cause.message : '渠道加载失败') })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [open, value])
  const decisionChannels = channels.filter((channel) => channel.auth_type === 'system_one' && channel.enabled !== false)
  const decisionChannel = decisionChannels.find((channel) => channel.id === draft?.decision_channel_id)
  const decisionModels = decisionChannel ? channelModelNames(decisionChannel) : []
  const businessModels = useMemo(() => [...new Set(channels.filter((channel) => channel.enabled !== false && channel.auth_type !== 'system_one').flatMap(channelModelNames))].sort(), [channels])
  const patch = (values: Partial<ReasoningConfig>) => { setDraft((current) => current && { ...current, ...values }); setError('') }
  const apply = () => {
    if (!draft) return
    try {
      const next = parseReasoningConfig(JSON.stringify(draft))
      if (next.enabled && (!decisionChannel || !decisionModels.includes(next.decision_model) || next.business_models.some((name) => !businessModels.includes(name)))) throw new Error('请选择已启用的决策渠道及模型；业务模型必须有可用的普通渠道')
      onApply(JSON.stringify(next)); onClose()
    } catch (cause) { setError(cause instanceof Error ? cause.message : '配置无效') }
  }
  const optionsWithSaved = (values: string[], saved: string[]) => [...new Set([...values, ...saved])].map((name) => ({ value: name, label: `${name}${values.includes(name) ? '' : '（当前不可用）'}` }))
  return <Dialog open={open} onClose={onClose} size="lg" title="动态思考等级" closeOnBackdrop={false}
    footer={<><button className="btn" onClick={() => { setDraft({ ...reasoningDefaults, business_models: [] }); setError('') }}>恢复默认</button><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={!draft || (draft.enabled && (loading || Boolean(loadError)))} onClick={apply}>应用到待保存设置</button></>}>
    <p className="muted">由决策渠道为选定业务模型选择思考等级。明确的模型等级后缀优先；保存系统设置并重启后生效。</p>
    {error && <p className="error-text" role="alert">{error}</p>}
    {loadError && <p className="error-text" role="alert">{loadError}</p>}
    {loading && <p role="status">正在加载渠道…</p>}
    {draft && <>
      <label><input type="checkbox" checked={draft.enabled} onChange={(event) => patch({ enabled: event.target.checked })} /> 启用动态思考</label>
      <div className="cell-stack">
        <label>决策渠道</label>
        <SearchableSelect ariaLabel="决策渠道" value={draft.decision_channel_id ? String(draft.decision_channel_id) : ''} options={[{ value: '', label: '选择 System One 渠道' }, ...decisionChannels.map((channel) => ({ value: String(channel.id), label: channel.name })), ...(!decisionChannel && draft.decision_channel_id ? [{ value: String(draft.decision_channel_id), label: `#${draft.decision_channel_id}（当前不可用）` }] : [])]} onChange={(id) => patch({ decision_channel_id: Number(id), decision_model: '' })} />
        {!loading && !loadError && !decisionChannels.length && <p className="muted">请先在渠道页添加并启用决策渠道。</p>}
        <label>决策模型</label>
        <SearchableSelect ariaLabel="决策模型" value={draft.decision_model} options={optionsWithSaved(decisionModels, draft.decision_model ? [draft.decision_model] : [])} onChange={(name) => patch({ decision_model: name })} placeholder="选择决策模型" />
        <label>业务模型</label>
        <SearchableMultiSelect ariaLabel="业务模型" values={draft.business_models} options={optionsWithSaved(businessModels, draft.business_models)} onChange={(names) => patch({ business_models: names })} placeholder="选择需要动态思考的模型" />
        <label>决策超时（ms）<input className="input compact" type="number" min={100} max={10000} step={1} value={draft.timeout_ms} onChange={(event) => patch({ timeout_ms: Number(event.target.value) })} /></label>
      </div>
    </>}
  </Dialog>
}
