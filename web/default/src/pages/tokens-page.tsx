import { useCallback, useEffect, useMemo, useState } from 'react'
import { Dialog } from '../components/dialog'
import { SearchableSelect } from '../components/searchable-select'
import { SearchableMultiSelect } from '../components/searchable-multi-select'
import { DateRangeFilter, normalizeRange, rangeParams, type DateRangeValue } from '../components/date-range'
import { deleteJSON, getJSON, postJSON, putJSON } from '../lib/api'
import { copyToClipboard } from '../lib/clipboard'
import { formatCompact, formatCostPair, formatDateTime, formatInt, formatPercent, formatSeconds } from '../lib/format'
import { useAutoRefresh } from '../hooks/use-auto-refresh'

type TokenRow = {
  id: number; token?: string; description?: string; created_at?: string; expires_at?: number | null; last_used_at?: number | null; is_active?: boolean
  success_count?: number; failure_count?: number; stream_avg_ttfb?: number; non_stream_avg_rt?: number; stream_count?: number; non_stream_count?: number
  prompt_tokens_total?: number; completion_tokens_total?: number; cache_read_tokens_total?: number; cache_creation_tokens_total?: number
  total_cost_usd?: number; effective_cost_usd?: number; cost_used_usd?: number; cost_limit_usd?: number; cost_daily_used_usd?: number; cost_daily_limit_usd?: number; cost_monthly_used_usd?: number; cost_monthly_limit_usd?: number
  peak_rpm?: number; avg_rpm?: number; recent_rpm?: number; allowed_models?: string[]; allowed_channel_ids?: number[]; channel_restriction_mode?: string; max_concurrency?: number
}
type ChannelOption = { id: number; name: string; auth_type?: string; urls?: Array<{ protocols?: string[] }>; models?: Array<string | { model?: string }> }

const RANGE_KEY = 'tokens.range'
const EXPIRY_PRESETS = [
  { value: 'never', label: '永不过期' }, { value: '30', label: '30 天' }, { value: '90', label: '90 天' },
  { value: '180', label: '180 天' }, { value: '365', label: '1 年' }, { value: 'custom', label: '自定义' },
]
const MODES = [{ value: 'allow', label: '白名单（仅允许所选渠道）' }, { value: 'deny', label: '黑名单（排除所选渠道）' }]

type Draft = {
  description: string; expiry: string; customExpiry: string; isActive: boolean; daily: string; monthly: string; total: string; concurrency: string
  models: string[]; channels: string[]; mode: string
}
const emptyDraft = (): Draft => ({ description: '', expiry: 'never', customExpiry: '', isActive: true, daily: '0', monthly: '0', total: '0', concurrency: '0', models: [], channels: [], mode: 'allow' })

const toLocalInput = (ms?: number | null) => { if (!ms) return ''; const d = new Date(ms); const pad = (n: number) => String(n).padStart(2, '0'); return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}` }
const modelOf = (entry: string | { model?: string }) => typeof entry === 'string' ? entry : String(entry.model ?? '')
const maskToken = (value?: string) => !value ? '-' : value.length <= 12 ? value : `${value.slice(0, 6)}…${value.slice(-4)}`

/** 把草稿转为请求体；返回错误文案时阻止提交。expiryTouched=false 时不发 expires_at，避免编辑时改写原过期时间。 */
function buildPayload(draft: Draft, expiryTouched: boolean): { body?: Record<string, unknown>; error?: string } {
  if (!draft.description.trim()) return { error: '请填写描述' }
  const limits = { cost_daily_limit_usd: Number(draft.daily || 0), cost_monthly_limit_usd: Number(draft.monthly || 0), cost_limit_usd: Number(draft.total || 0) }
  if (Object.values(limits).some((value) => !Number.isFinite(value) || value < 0)) return { error: '成本限额必须是非负数' }
  const concurrency = Number(draft.concurrency || 0)
  // 后端字段是 int，小数会导致 JSON 绑定失败。
  if (!Number.isInteger(concurrency) || concurrency < 0) return { error: '最大并发必须是非负整数' }
  // 设置了成本限额却不限并发，会被后端拒绝（auth_token.go:325）。
  if (Object.values(limits).some((value) => value > 0) && concurrency <= 0) return { error: '设置成本限额时必须同时设置最大并发（大于 0）' }
  const body: Record<string, unknown> = {
    description: draft.description.trim(), is_active: draft.isActive, ...limits, max_concurrency: concurrency,
    allowed_models: draft.models, allowed_channel_ids: draft.channels.map(Number), channel_restriction_mode: draft.mode,
  }
  if (expiryTouched) {
    if (draft.expiry === 'never') body.expires_at = null
    else if (draft.expiry === 'custom') {
      const ms = Date.parse(draft.customExpiry)
      if (!Number.isFinite(ms)) return { error: '请填写有效的过期时间' }
      body.expires_at = ms
    } else body.expires_at = Date.now() + Number(draft.expiry) * 86_400_000
  }
  return { body }
}

export function TokensPage() {
  const [range, setRange] = useState<DateRangeValue>(() => { try { return normalizeRange(JSON.parse(localStorage.getItem(RANGE_KEY) ?? 'null') ?? { range: 'all' }, true) } catch { return { range: 'all' } } })
  const [rows, setRows] = useState<TokenRow[]>([])
  const [isToday, setIsToday] = useState(false)
  const [keyword, setKeyword] = useState('')
  const [channels, setChannels] = useState<ChannelOption[]>([])
  const [loading, setLoading] = useState(false)
  const [notice, setNotice] = useState<{ kind: 'success' | 'error'; text: string } | null>(null)
  const [editor, setEditor] = useState<{ open: boolean; row: TokenRow | null }>({ open: false, row: null })
  const [created, setCreated] = useState('')
  const [deleteTarget, setDeleteTarget] = useState<TokenRow | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const data = await getJSON<{ tokens?: TokenRow[]; is_today?: boolean }>('/admin/auth-tokens', range.range === 'all' ? { range: 'all' } : rangeParams(range))
      setRows(data.tokens ?? []); setIsToday(Boolean(data.is_today))
    } catch (cause) { setNotice({ kind: 'error', text: cause instanceof Error ? cause.message : '令牌加载失败' }) } finally { setLoading(false) }
  }, [range])

  useEffect(() => { localStorage.setItem(RANGE_KEY, JSON.stringify(range)); void load() }, [range, load])
  // 渠道全集用于渠道/模型限制选择；不带分页参数以取全量。
  useEffect(() => { void getJSON<ChannelOption[]>('/admin/channels').then((data) => setChannels(Array.isArray(data) ? data : [])).catch(() => setChannels([])) }, [])
  useAutoRefresh(() => void load())

  const visible = useMemo(() => {
    const query = keyword.trim().toLowerCase()
    return query ? rows.filter((row) => String(row.description ?? '').toLowerCase().includes(query) || String(row.token ?? '').toLowerCase().includes(query)) : rows
  }, [rows, keyword])

  const remove = async () => {
    const target = deleteTarget; setDeleteTarget(null)
    if (!target) return
    try { await deleteJSON(`/admin/auth-tokens/${target.id}`); setNotice({ kind: 'success', text: `已删除令牌「${target.description ?? target.id}」` }); await load() }
    catch (cause) { setNotice({ kind: 'error', text: cause instanceof Error ? cause.message : '删除失败' }) }
  }
  const copy = async (text: string, label: string) => {
    try { await copyToClipboard(text); setNotice({ kind: 'success', text: `${label}已复制` }) } catch { setNotice({ kind: 'error', text: '复制失败，请手动复制' }) }
  }

  return <>
    <header className="page-header">
      <div><h1>API 令牌</h1><p className="muted">管理访问令牌、成本限额以及模型和渠道限制</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <DateRangeFilter includeAll value={range} onChange={setRange} />
        <input className="input" value={keyword} onChange={(event) => setKeyword(event.target.value)} placeholder="按描述或令牌筛选" aria-label="筛选令牌" />
        <button className="btn" disabled={loading} onClick={() => void load()}>刷新</button>
        <button className="btn btn-primary" onClick={() => setEditor({ open: true, row: null })}>创建令牌</button>
      </div>
    </header>
    {notice && <div className={`card ${notice.kind === 'error' ? 'error-text' : 'success-text'}`} role={notice.kind === 'error' ? 'alert' : 'status'}><div className="job-summary" style={{ margin: 0 }}><span>{notice.text}</span><button className="link-button" onClick={() => setNotice(null)}>关闭</button></div></div>}

    <div className="table-wrap"><table><thead><tr>
      <th>令牌</th><th>状态</th><th>调用</th><th>成功率</th><th>RPM 峰/均{isToday ? '/近' : ''}</th><th>Token 用量</th><th>成本</th><th>并发</th><th>流式首字</th><th>非流式耗时</th><th>限制</th><th>最后使用</th><th>操作</th>
    </tr></thead><tbody>
      {!visible.length && <tr><td colSpan={13} className="muted">{loading ? '加载中…' : '暂无令牌'}</td></tr>}
      {visible.map((row) => {
        const success = Number(row.success_count ?? 0); const failure = Number(row.failure_count ?? 0)
        const expired = row.expires_at != null && row.expires_at > 0 && row.expires_at < Date.now()
        const channelNames = (row.allowed_channel_ids ?? []).map((id) => channels.find((item) => item.id === id)?.name ?? `#${id}`)
        return <tr key={row.id}>
          <td className="cell-stack"><strong>{row.description || `令牌 #${row.id}`}</strong><code className="table-note">{maskToken(row.token)}</code><span className="table-note">创建 {formatDateTime(row.created_at, true)} · {row.expires_at ? `过期 ${formatDateTime(row.expires_at, true)}` : '永不过期'}</span></td>
          <td>{!row.is_active ? <span className="badge badge-muted">已停用</span> : expired ? <span className="badge badge-bad">已过期</span> : <span className="badge badge-good">启用</span>}</td>
          <td className="cell-stack"><span className="success-text">成功 {formatInt(success)}</span><span className="error-text">失败 {formatInt(failure)}</span></td>
          <td>{formatPercent(success, success + failure)}</td>
          <td>{[row.peak_rpm, row.avg_rpm, ...(isToday ? [row.recent_rpm] : [])].map((value) => Number(value ?? 0).toFixed(1)).join(' / ')}</td>
          <td className="cell-stack"><span>入 {formatCompact(row.prompt_tokens_total)} · 出 {formatCompact(row.completion_tokens_total)}</span><span className="table-note">缓读 {formatCompact(row.cache_read_tokens_total)} · 缓写 {formatCompact(row.cache_creation_tokens_total)}</span></td>
          <td>{formatCostPair(row.total_cost_usd, row.effective_cost_usd)}</td>
          <td>{Number(row.max_concurrency ?? 0) || '∞'}</td>
          <td>{Number(row.stream_count ?? 0) ? formatSeconds(row.stream_avg_ttfb) : '-'}</td>
          <td>{Number(row.non_stream_count ?? 0) ? formatSeconds(row.non_stream_avg_rt) : '-'}</td>
          <td className="cell-stack">
            <span className="table-note" title={(row.allowed_models ?? []).join('\n')}>模型：{row.allowed_models?.length ? `${row.allowed_models.length} 个` : '不限'}</span>
            <span className="table-note" title={channelNames.join('\n')}>渠道：{row.allowed_channel_ids?.length ? `${row.channel_restriction_mode === 'deny' ? '排除' : '仅限'} ${row.allowed_channel_ids.length} 个` : '不限'}</span>
            {(row.cost_daily_limit_usd || row.cost_monthly_limit_usd || row.cost_limit_usd) ? <span className="table-note">限额 日 ${Number(row.cost_daily_limit_usd ?? 0)} / 月 ${Number(row.cost_monthly_limit_usd ?? 0)} / 总 ${Number(row.cost_limit_usd ?? 0)}</span> : null}
          </td>
          <td>{row.last_used_at ? formatDateTime(row.last_used_at, true) : '从未使用'}</td>
          <td><div className="toolbar" style={{ marginBottom: 0, flexWrap: 'nowrap' }}>
            <button className="btn" disabled={!row.token} onClick={() => void copy(String(row.token), '令牌哈希')}>复制</button>
            <button className="btn" onClick={() => setEditor({ open: true, row })}>编辑</button>
            <button className="btn" onClick={() => setDeleteTarget(row)}>删除</button>
          </div></td>
        </tr>
      })}
    </tbody></table></div>

    <TokenEditor open={editor.open} row={editor.row} channels={channels} onClose={() => setEditor({ open: false, row: null })}
      onSaved={(token) => { setEditor({ open: false, row: null }); if (token) setCreated(token); else setNotice({ kind: 'success', text: '令牌已保存' }); void load() }} />

    <Dialog open={Boolean(created)} onClose={() => setCreated('')} size="md" title="令牌已创建" description="明文令牌只显示这一次，请立即复制并妥善保存。"
      footer={<><button className="btn" onClick={() => void copy(created, '令牌')}>复制令牌</button><button className="btn btn-primary" onClick={() => setCreated('')}>我已保存</button></>}>
      <textarea className="input wide" readOnly rows={3} value={created} onFocus={(event) => event.currentTarget.select()} />
    </Dialog>

    <Dialog open={deleteTarget !== null} onClose={() => setDeleteTarget(null)} size="sm" title="删除令牌"
      footer={<><button className="btn" onClick={() => setDeleteTarget(null)}>取消</button><button className="btn btn-danger" onClick={() => void remove()}>删除</button></>}>
      <p>确定删除令牌「{deleteTarget?.description ?? deleteTarget?.id}」吗？使用该令牌的客户端将立即无法访问。</p>
    </Dialog>
  </>
}

function TokenEditor({ open, row, channels, onClose, onSaved }: { open: boolean; row: TokenRow | null; channels: ChannelOption[]; onClose: () => void; onSaved: (createdToken?: string) => void }) {
  const [draft, setDraft] = useState<Draft>(emptyDraft)
  const [expiryTouched, setExpiryTouched] = useState(false)
  const [importText, setImportText] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setError(''); setImportText(''); setExpiryTouched(!row)
    setDraft(row ? {
      description: row.description ?? '', expiry: row.expires_at ? 'custom' : 'never', customExpiry: toLocalInput(row.expires_at), isActive: row.is_active !== false,
      daily: String(row.cost_daily_limit_usd ?? 0), monthly: String(row.cost_monthly_limit_usd ?? 0), total: String(row.cost_limit_usd ?? 0), concurrency: String(row.max_concurrency ?? 0),
      models: row.allowed_models ?? [], channels: (row.allowed_channel_ids ?? []).map(String), mode: row.channel_restriction_mode === 'deny' ? 'deny' : 'allow',
    } : emptyDraft())
  }, [open, row])

  const patch = (value: Partial<Draft>) => setDraft((current) => ({ ...current, ...value }))
  const channelOptions = useMemo(() => channels.map((channel) => {
    const protocols = [...new Set((channel.urls ?? []).flatMap((url) => url.protocols ?? []))]
    return { value: String(channel.id), label: `${channel.name} · #${channel.id}${protocols.length ? ` · ${protocols.join('/')}` : ''}` }
  }), [channels])
  // 候选模型按当前渠道限制过滤（旧版 tokens.js:946-967）：白名单只取所选渠道，黑名单排除所选渠道。
  const modelOptions = useMemo(() => {
    const chosen = new Set(draft.channels.map(Number))
    const scope = !chosen.size ? channels : channels.filter((channel) => draft.mode === 'deny' ? !chosen.has(channel.id) : chosen.has(channel.id))
    const names = new Set(scope.flatMap((channel) => (channel.models ?? []).map(modelOf)).filter(Boolean))
    for (const model of draft.models) names.add(model)
    return [...names].sort().map((name) => ({ value: name, label: name }))
  }, [channels, draft.channels, draft.mode, draft.models])

  const importModels = () => {
    const incoming = importText.split(/[\n,]/).map((item) => item.trim()).filter(Boolean)
    const existing = new Set(draft.models.map((item) => item.toLowerCase()))
    const fresh = incoming.filter((item, index) => !existing.has(item.toLowerCase()) && incoming.findIndex((other) => other.toLowerCase() === item.toLowerCase()) === index)
    patch({ models: [...draft.models, ...fresh] }); setImportText('')
    setError(fresh.length === incoming.length ? '' : `已忽略 ${incoming.length - fresh.length} 个重复模型`)
  }

  const save = async () => {
    const { body, error: problem } = buildPayload(draft, expiryTouched)
    if (problem || !body) { setError(problem ?? '参数无效'); return }
    setBusy(true); setError('')
    try {
      if (row) { await putJSON(`/admin/auth-tokens/${row.id}`, body); onSaved() }
      else { const result = await postJSON<{ token?: string }>('/admin/auth-tokens', body); onSaved(result.token ?? '') }
    } catch (cause) { setError(cause instanceof Error ? cause.message : '保存失败') } finally { setBusy(false) }
  }

  const usage = (used?: number) => row && used != null ? <span className="table-note">已用 ${Number(used).toFixed(4)}</span> : null

  return <Dialog open={open} onClose={onClose} size="lg" closeOnBackdrop={false} title={row ? '编辑令牌' : '创建令牌'}
    footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={busy} onClick={() => void save()}>{busy ? '保存中…' : '保存'}</button></>}>
    {error && <p className="error-text" role="alert">{error}</p>}
    {row && <label className="cell-stack">令牌哈希<input className="input wide" readOnly value={row.token ?? ''} /></label>}
    <label className="cell-stack">描述 *<input className="input wide" value={draft.description} onChange={(event) => patch({ description: event.target.value })} placeholder="用途说明" /></label>
    <div className="toolbar">
      <label className="cell-stack">过期时间<SearchableSelect ariaLabel="过期时间" className="combobox-inline" value={draft.expiry} options={EXPIRY_PRESETS} onChange={(expiry) => { patch({ expiry }); setExpiryTouched(true) }} /></label>
      {draft.expiry === 'custom' && <label className="cell-stack">自定义<input className="input" type="datetime-local" value={draft.customExpiry} onChange={(event) => { patch({ customExpiry: event.target.value }); setExpiryTouched(true) }} /></label>}
      <label className="switch" style={{ alignSelf: 'end' }}><input type="checkbox" checked={draft.isActive} onChange={(event) => patch({ isActive: event.target.checked })} /> 启用</label>
    </div>
    <div className="toolbar">
      <label className="cell-stack">日限额 ($)<input className="input compact" type="number" min={0} step="0.01" value={draft.daily} onChange={(event) => patch({ daily: event.target.value })} />{usage(row?.cost_daily_used_usd)}</label>
      <label className="cell-stack">月限额 ($)<input className="input compact" type="number" min={0} step="0.01" value={draft.monthly} onChange={(event) => patch({ monthly: event.target.value })} />{usage(row?.cost_monthly_used_usd)}</label>
      <label className="cell-stack">总限额 ($)<input className="input compact" type="number" min={0} step="0.01" value={draft.total} onChange={(event) => patch({ total: event.target.value })} />{usage(row?.cost_used_usd)}</label>
      <label className="cell-stack">最大并发<input className="input compact" type="number" min={0} step={1} value={draft.concurrency} onChange={(event) => patch({ concurrency: event.target.value })} /><span className="table-note">0 表示不限</span></label>
    </div>
    <p className="muted">成本限额为 0 表示不限；设置任一成本限额时必须同时设置最大并发。</p>

    <fieldset className="card">
      <legend>渠道限制</legend>
      <div className="toolbar">
        <SearchableSelect ariaLabel="渠道限制模式" className="combobox-inline" value={draft.mode} options={MODES} onChange={(mode) => patch({ mode })} />
        <SearchableMultiSelect ariaLabel="渠道" className="combobox-wide" values={draft.channels} options={channelOptions} onChange={(values) => patch({ channels: values })} placeholder="不限渠道" />
      </div>
      <span className="table-note">{draft.channels.length ? `${draft.mode === 'deny' ? '排除' : '仅允许'} ${draft.channels.length} 个渠道` : '未选择渠道时不限制'}</span>
    </fieldset>

    <fieldset className="card">
      <legend>模型限制</legend>
      <SearchableMultiSelect ariaLabel="模型" className="combobox-wide" values={draft.models} options={modelOptions} onChange={(values) => patch({ models: values })} placeholder="不限模型" />
      <div className="toolbar">
        <input className="input wide" value={importText} onChange={(event) => setImportText(event.target.value)} placeholder="批量导入：逗号或换行分隔" aria-label="批量导入模型" />
        <button className="btn" disabled={!importText.trim()} onClick={importModels}>导入</button>
      </div>
      <span className="table-note">{draft.models.length ? `仅允许 ${draft.models.length} 个模型` : '未选择模型时不限制'}；候选模型已按渠道限制过滤</span>
    </fieldset>
  </Dialog>
}
