import { useCallback, useEffect, useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { SearchableSelect } from '../components/searchable-select'
import { getJSON, invalidateGetCache, postJSON, putJSON } from '../lib/api'
import { CHANNEL_POOLS } from './channels/api'
import '../styles/channel-routes.css'
import {
  POOL_OPTIONS, ROUTE_REASONS, STICKY_MODES, addModels, draftsToConfig, emptyRuleDraft, exampleRuleDraft,
  inflightFor, parseTokenInput, ruleToDraft, summarizeRule, withPoolInflight,
  type InflightView, type OverflowDraft, type RoutesView, type RuleDraft, type Simulation, type StickyMode, type TierDraft,
} from './channel-routes'

// 进行中请求数是实时值，定时刷新渠道组概况；规则草稿只在加载、保存或手动刷新时同步。
const REFRESH_MS = 5000
const errorText = (cause: unknown, fallback: string) => cause instanceof Error ? cause.message : fallback

type RuleCardProps = {
  draft: RuleDraft
  index: number
  total: number
  inflight: InflightView[]
  knownModels: string[]
  onChange: (patch: Partial<RuleDraft>) => void
  onMove: (delta: number) => void
  onRemove: () => void
}

function RuleCard({ draft, index, total, inflight, knownModels, onChange, onMove, onRemove }: RuleCardProps) {
  const summary = useMemo(() => {
    const { config } = draftsToConfig([draft])
    return config ? summarizeRule(config.rules[0]) : ''
  }, [draft])
  const modelOptions = useMemo(() => knownModels.filter((name) => !draft.models.some((item) => item.toLowerCase() === name.toLowerCase())).map((name) => ({ value: name, label: name })), [knownModels, draft.models])
  const setTier = (tierIndex: number, patch: Partial<TierDraft>) => onChange({ tiers: draft.tiers.map((tier, itemIndex) => itemIndex === tierIndex ? { ...tier, ...patch } : tier) })
  const setOverflow = (overflowIndex: number, patch: Partial<OverflowDraft>) => onChange({ overflow: draft.overflow.map((item, itemIndex) => itemIndex === overflowIndex ? { ...item, ...patch } : item) })
  const label = `规则 ${index + 1}`

  return <div className={`card route-rule${draft.enabled ? '' : ' route-rule-disabled'}`}>
    <div className="route-rule-head">
      <span className="badge">#{index + 1}</span>
      <label className="switch"><input type="checkbox" aria-label={`${label} 启用`} checked={draft.enabled} onChange={(event) => onChange({ enabled: event.target.checked })} /> 启用</label>
      <input className="input" value={draft.name} placeholder="规则名称（可选）" aria-label={`${label} 名称`} maxLength={64} onChange={(event) => onChange({ name: event.target.value })} />
      <span className="route-rule-actions">
        <button className="btn btn-sm" disabled={index === 0} onClick={() => onMove(-1)}>上移</button>
        <button className="btn btn-sm" disabled={index === total - 1} onClick={() => onMove(1)}>下移</button>
        <button className="btn btn-sm btn-danger" onClick={onRemove}>删除</button>
      </span>
    </div>
    {summary && <p className="muted route-rule-summary">{summary}</p>}
    {draft.id && <p className="table-note">本规则进行中：{CHANNEL_POOLS.map((pool) => `${pool.label} ${inflightFor(inflight, draft.id, pool.value)}`).join(' · ')}</p>}

    <div className="route-field">
      <span className="route-label">模型</span>
      <div className="cell-stack">
        <div className="route-models">
          {draft.models.map((name) => <span key={name} className="badge route-chip">{name}<button type="button" className="link-button" aria-label={`移除模型 ${name}`} onClick={() => onChange({ models: draft.models.filter((item) => item !== name) })}>×</button></span>)}
          <SearchableSelect ariaLabel={`${label} 添加模型`} className="combobox-inline" allowCustomInput value="" options={modelOptions} placeholder="选择或输入模型" onChange={(value) => { if (value.trim()) onChange({ models: addModels(draft.models, value) }) }} />
        </div>
        <span className="table-note">请求模型（去掉思考后缀后）大小写无关匹配；支持 * 通配，如 kimi-*。规则自上而下匹配，第一条启用且命中的规则生效。</span>
      </div>
    </div>

    <div className="route-field">
      <span className="route-label">渠道组</span>
      <div className="cell-stack">
        <div className="toolbar route-row">
          <span className="muted">默认</span>
          <SearchableSelect ariaLabel={`${label} 默认渠道组`} className="combobox-inline" value={String(draft.defaultPool)} options={POOL_OPTIONS} onChange={(value) => onChange({ defaultPool: Number(value) })} />
          <SearchableSelect ariaLabel={`${label} 会话粘性`} className="combobox-inline" value={draft.sticky} options={STICKY_MODES.map(({ value, label: text }) => ({ value, label: text }))} onChange={(value) => onChange({ sticky: value as StickyMode })} />
          <label title="只使用规则选中的渠道组；队列溢出目标与会话粘性组除外"><input type="checkbox" checked={draft.strict} onChange={(event) => onChange({ strict: event.target.checked })} /> 严格模式（不回退其他渠道组）</label>
        </div>
        <span className="table-note">{STICKY_MODES.find((mode) => mode.value === draft.sticky)?.hint}。会话按客户端显式会话 ID 识别（Claude Code 会话、Codex Session-Id、X-Session-Id 等），无会话 ID 的请求不粘性。</span>
      </div>
    </div>

    <div className="route-field">
      <span className="route-label">输入 Token</span>
      <div className="cell-stack">
        {draft.tiers.map((tier, tierIndex) => <div key={tierIndex} className="toolbar route-row">
          <span className="muted">档位 {tierIndex + 1}</span>
          <label className="muted">≥<input className="input compact" value={tier.min} placeholder="不限" aria-label={`${label} 档位 ${tierIndex + 1} 下限`} onChange={(event) => setTier(tierIndex, { min: event.target.value })} /></label>
          <label className="muted">且 &lt;<input className="input compact" value={tier.max} placeholder="不限" aria-label={`${label} 档位 ${tierIndex + 1} 上限`} onChange={(event) => setTier(tierIndex, { max: event.target.value })} /></label>
          <span className="muted">→</span>
          <SearchableSelect ariaLabel={`${label} 档位 ${tierIndex + 1} 渠道组`} className="combobox-inline" value={String(tier.pool)} options={POOL_OPTIONS} onChange={(value) => setTier(tierIndex, { pool: Number(value) })} />
          <button className="btn btn-sm" onClick={() => onChange({ tiers: draft.tiers.filter((_, itemIndex) => itemIndex !== tierIndex) })}>删除</button>
        </div>)}
        <div><button className="btn btn-sm" disabled={draft.tiers.length >= 16} onClick={() => onChange({ tiers: [...draft.tiers, { min: '', max: '', pool: 1 }] })}>+ 添加 Token 档位</button></div>
        <span className="table-note">按顺序取第一个命中的档位，留空表示不限，可写 8000、8k、50k；都不命中时使用默认渠道组。输入 Token 由请求体估算（与本地 count_tokens 同口径，图片按 1500 计）。</span>
      </div>
    </div>

    <div className="route-field">
      <span className="route-label">队列溢出</span>
      <div className="cell-stack">
        {draft.overflow.map((item, overflowIndex) => <div key={overflowIndex} className="toolbar route-row">
          <span className="muted">当</span>
          <SearchableSelect ariaLabel={`${label} 溢出 ${overflowIndex + 1} 来源`} className="combobox-inline" value={String(item.pool)} options={POOL_OPTIONS} onChange={(value) => setOverflow(overflowIndex, { pool: Number(value) })} />
          <label className="muted">本规则进行中请求 ≥<input className="input compact" value={item.maxQueue} aria-label={`${label} 溢出 ${overflowIndex + 1} 队列上限`} onChange={(event) => setOverflow(overflowIndex, { maxQueue: event.target.value })} /></label>
          <span className="muted">时，新请求优先</span>
          <SearchableSelect ariaLabel={`${label} 溢出 ${overflowIndex + 1} 目标`} className="combobox-inline" value={String(item.target)} options={POOL_OPTIONS} onChange={(value) => setOverflow(overflowIndex, { target: Number(value) })} />
          <button className="btn btn-sm" onClick={() => onChange({ overflow: draft.overflow.filter((_, itemIndex) => itemIndex !== overflowIndex) })}>删除</button>
        </div>)}
        <div><button className="btn btn-sm" disabled={draft.overflow.length >= CHANNEL_POOLS.length} onClick={() => onChange({ overflow: [...draft.overflow, { pool: 1, maxQueue: '100', target: 2 }] })}>+ 添加队列溢出</button></div>
        <span className="table-note">进行中请求按本规则落在各渠道组的实际占用计数（流式响应持续到结束）；溢出可以接力，原渠道组保留为回退。</span>
      </div>
    </div>
  </div>
}

export function ChannelRoutesPage() {
  const [view, setView] = useState<RoutesView | null>(null)
  const [drafts, setDrafts] = useState<RuleDraft[]>([])
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; text: string } | null>(null)
  const [simulation, setSimulation] = useState({ model: '', tokens: '' })
  const [simulated, setSimulated] = useState<Simulation | null>(null)
  const [simulateError, setSimulateError] = useState('')

  const load = useCallback(async (syncDrafts: boolean) => {
    try {
      invalidateGetCache()
      const data = await getJSON<RoutesView>('/admin/channel-routes')
      setView(data)
      if (syncDrafts) { setDrafts(data.config.rules.map(ruleToDraft)); setDirty(false) }
    } catch (cause) { setNotice({ kind: 'error', text: errorText(cause, '渠道路由加载失败') }) }
  }, [])

  useEffect(() => { void load(true) }, [load])
  useEffect(() => {
    const timer = window.setInterval(() => {
      if (document.hidden) return
      void getJSON<{ inflight: InflightView[] }>('/admin/channel-routes/inflight').then((data) => {
        setView((current) => current && { ...current, inflight: data.inflight, pools: withPoolInflight(current.pools, data.inflight) })
      }).catch(() => undefined)
    }, REFRESH_MS)
    return () => window.clearInterval(timer)
  }, [])

  const knownModels = useMemo(() => [...new Set((view?.pools ?? []).flatMap((pool) => pool.models))].sort(), [view])
  const change = (key: string, patch: Partial<RuleDraft>) => { setDrafts((current) => current.map((draft) => draft.key === key ? { ...draft, ...patch } : draft)); setDirty(true) }
  const add = (draft: RuleDraft) => { setDrafts((current) => [...current, draft]); setDirty(true) }
  const move = (index: number, delta: number) => {
    setDrafts((current) => {
      const target = index + delta
      if (target < 0 || target >= current.length) return current
      const next = [...current];
      [next[index], next[target]] = [next[target], next[index]]
      return next
    })
    setDirty(true)
  }
  const remove = (draft: RuleDraft, index: number) => {
    if (!window.confirm(`确定删除规则 ${index + 1}${draft.name ? `「${draft.name}」` : ''} 吗？保存后生效。`)) return
    setDrafts((current) => current.filter((item) => item.key !== draft.key)); setDirty(true)
  }
  const refresh = () => {
    if (dirty && !window.confirm('放弃未保存的修改并重新加载吗？')) return
    void load(true)
  }
  const save = async () => {
    const { config, error } = draftsToConfig(drafts)
    if (!config) { setNotice({ kind: 'error', text: error ?? '规则无效' }); return }
    setBusy(true)
    try {
      const data = await putJSON<RoutesView>('/admin/channel-routes', config)
      setView(data); setDrafts(data.config.rules.map(ruleToDraft)); setDirty(false)
      setNotice({ kind: 'success', text: '渠道路由已保存，立即生效' })
    } catch (cause) { setNotice({ kind: 'error', text: errorText(cause, '保存失败') }) } finally { setBusy(false) }
  }
  const simulate = async () => {
    setSimulateError(''); setSimulated(null)
    const tokens = parseTokenInput(simulation.tokens)
    if (!simulation.model.trim()) { setSimulateError('请填写模型'); return }
    if (tokens === null) { setSimulateError('输入 Token 格式无效（示例：9000、60k）'); return }
    try { setSimulated(await postJSON<Simulation>('/admin/channel-routes/simulate', { model: simulation.model.trim(), input_tokens: tokens })) }
    catch (cause) { setSimulateError(errorText(cause, '模拟失败')) }
  }

  return <>
    <header className="page-header">
      <div><h1>渠道路由</h1><p className="muted">按模型把请求分流到主渠道 / 渠道1 / 渠道2：命中规则后依次按会话粘性、输入 Token 档位与队列溢出选出首选渠道组，首选组无可用渠道或全部失败时按顺序回退其他渠道组（严格模式除外）。未命中规则的请求优先使用主渠道。保存后立即生效，无需重启。</p></div>
      <div className="toolbar" style={{ marginBottom: 0, flexWrap: 'nowrap', flexShrink: 0 }}>
        <button className="btn" disabled={busy} onClick={refresh}>刷新</button>
        <button className="btn" onClick={() => add(exampleRuleDraft())}>+ kimi-k3 示例</button>
        <button className="btn" onClick={() => add(emptyRuleDraft())}>+ 添加规则</button>
        <button className="btn btn-primary" disabled={busy || !dirty} onClick={() => void save()}>{busy ? '保存中…' : dirty ? '保存 *' : '已保存'}</button>
      </div>
    </header>

    {notice && <div className={`card${notice.kind === 'error' ? ' error-text' : ''}`} role={notice.kind === 'error' ? 'alert' : 'status'}>
      <div className="job-summary"><strong>{notice.text}</strong><button className="link-button" onClick={() => setNotice(null)}>关闭</button></div>
    </div>}

    <section className="route-pools">
      {CHANNEL_POOLS.map((pool) => {
        const stats = view?.pools.find((item) => item.pool === pool.value)
        return <div key={pool.value} className="card route-pool-card">
          <div className="route-pool-head"><strong>{pool.label}</strong><Link to={pool.path} className="link-button">管理渠道</Link></div>
          <span className="route-pool-value">{stats?.inflight ?? 0}</span>
          <span className="muted">进行中请求</span>
          <span className="table-note">{stats ? `${stats.channels} 个渠道（${stats.enabled} 个启用）· ${stats.models.length} 个模型` : '加载中…'}</span>
        </div>
      })}
    </section>

    {view && !drafts.length && <div className="card muted route-empty">还没有渠道路由规则：所有请求优先使用主渠道，主渠道无可用渠道时依次回退渠道1、渠道2。可点击「+ kimi-k3 示例」创建示例规则：输入 &lt; 8k 走渠道1、≥ 50k 走渠道2，渠道1 进行中达到 100 时溢出到渠道2。</div>}
    {drafts.map((draft, index) => <RuleCard key={draft.key} draft={draft} index={index} total={drafts.length} inflight={view?.inflight ?? []} knownModels={knownModels}
      onChange={(patch) => change(draft.key, patch)} onMove={(delta) => move(index, delta)} onRemove={() => remove(draft, index)} />)}

    <section className="card route-simulate">
      <h2>路由模拟</h2>
      <p className="muted">按已保存的规则与实时队列计算渠道组顺序；不发送请求、不占用队列，也不考虑会话粘性。</p>
      <div className="toolbar">
        <SearchableSelect ariaLabel="模拟模型" allowCustomInput value={simulation.model} options={knownModels.map((name) => ({ value: name, label: name }))} placeholder="模型，如 kimi-k3" onChange={(value) => setSimulation((current) => ({ ...current, model: value }))} />
        <input className="input" style={{ width: 240 }} value={simulation.tokens} placeholder="输入 Token，如 9000 或 60k" aria-label="模拟输入 Token" onChange={(event) => setSimulation((current) => ({ ...current, tokens: event.target.value }))} onKeyDown={(event) => { if (event.key === 'Enter') void simulate() }} />
        <button className="btn btn-primary" onClick={() => void simulate()}>模拟</button>
      </div>
      {simulateError && <p className="error-text" role="alert">{simulateError}</p>}
      {simulated && <div className="cell-stack">
        <span>{simulated.matched ? `命中规则「${simulated.rule_name || simulated.rule_id}」` : '未命中规则（默认顺序）'} · 依据：{ROUTE_REASONS[simulated.reason] ?? simulated.reason}{simulated.tier >= 0 ? `（档位 ${simulated.tier + 1}）` : ''} · 输入 Token {simulated.input_tokens.toLocaleString()}</span>
        <span className="route-order">{simulated.pools.map((pool, order) => <span key={pool.pool} className={`badge ${order === 0 ? 'badge-good' : 'badge-muted'}`}>{order + 1}. {pool.name}（进行中 {pool.inflight}）</span>)}</span>
        {dirty && <span className="warn-text">有未保存的修改，模拟结果基于已保存的规则。</span>}
      </div>}
    </section>
  </>
}
