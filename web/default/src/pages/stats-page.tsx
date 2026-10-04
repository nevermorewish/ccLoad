import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { DateRangeFilter, normalizeRange, rangeParams, type DateRangeValue } from '../components/date-range'
import { SearchableSelect } from '../components/searchable-select'
import { getJSON } from '../lib/api'
import { shouldHideChannels } from '../lib/auth'
import { cacheHitRate, formatCompact, formatCostPair, formatDateTime, formatInt, formatPercent, formatSeconds, formatUSD } from '../lib/format'
import { useSession } from '../lib/session-context'
import { readQuery, readStored, writeQuery } from '../lib/url-state'
import { useAutoRefresh } from '../hooks/use-auto-refresh'
import { useTokenOptions } from '../hooks/use-token-options'

type HealthPoint = { ts?: string; rate?: number; success?: number; error?: number; rate_limited?: number; avg_first_byte_time?: number; avg_duration?: number; input_tokens?: number; output_tokens?: number; cache_read_tokens?: number; cache_creation_tokens?: number; cost?: number; effective_cost?: number }
type Entry = {
  channel_id?: number; channel_name?: string; channel_priority?: number; cost_multiplier_min?: number; cost_multiplier_max?: number; model?: string
  success?: number; error?: number; total?: number; avg_first_byte_time_seconds?: number; avg_duration_seconds?: number
  peak_rpm?: number; avg_rpm?: number; recent_rpm?: number; total_input_tokens?: number; total_output_tokens?: number; speed_output_tokens?: number; speed_duration_seconds?: number
  total_cache_read_input_tokens?: number; total_cache_creation_input_tokens?: number; total_cost?: number; effective_cost?: number; health_timeline?: HealthPoint[]
}
type StatsResponse = { stats?: Entry[]; is_today?: boolean; rpm_stats?: { peak_rpm?: number; avg_rpm?: number; recent_rpm?: number } }
type Filters = { range: DateRangeValue; channel: string; model: string; protocol: string; token: string; channelId: string; hideZero: boolean }
type SortKey = 'channel_name' | 'model' | 'success' | 'error' | 'duration' | 'speed' | 'rpm' | 'input' | 'output' | 'cache_read' | 'cache_creation' | 'cost'

const STORAGE_KEY = 'stats.filters'
const VIEW_KEY = 'stats.view'
const PROTOCOLS = [{ value: '', label: '全部协议' }, { value: 'anthropic', label: 'Claude Code' }, { value: 'codex', label: 'Codex' }, { value: 'openai', label: 'OpenAI' }, { value: 'gemini', label: 'Gemini' }]
const COLORS = ['#3b82f6', '#22c55e', '#f59e0b', '#ef4444', '#8b5cf6', '#06b6d4', '#ec4899', '#84cc16', '#f97316', '#14b8a6', '#6366f1', '#a855f7']
const num = (value: unknown) => { const parsed = Number(value); return Number.isFinite(parsed) ? parsed : 0 }

/** 单行速度：每次成功的平均输出 ÷ 生成耗时（总耗时减首字，生成耗时 ≥1s 才扣），与旧版 stats.js 一致。 */
function rowSpeed(entry: Entry): number | null {
  const success = num(entry.success); const output = num(entry.total_output_tokens); const duration = num(entry.avg_duration_seconds); const ttft = num(entry.avg_first_byte_time_seconds)
  if (!success || !output || duration <= 0) return null
  const generation = duration - ttft >= 1 ? duration - ttft : duration
  return output / success / generation
}

function initialFilters(): Filters {
  const query = readQuery()
  const stored = readStored<Partial<Filters>>(STORAGE_KEY, {})
  const fromURL = ['range', 'channel_name', 'channel_name_like', 'model', 'model_like', 'client_protocol', 'auth_token_id', 'channel_id'].some((key) => query.has(key))
  if (!fromURL) return { range: normalizeRange(stored.range), channel: stored.channel ?? '', model: stored.model ?? '', protocol: stored.protocol ?? '', token: stored.token ?? '', channelId: '', hideZero: stored.hideZero ?? true }
  return {
    range: normalizeRange({ range: query.get('range') ?? 'today', start: Number(query.get('start_time')), end: Number(query.get('end_time')) }),
    channel: query.get('channel_name') ?? query.get('channel_name_like') ?? '', model: query.get('model') ?? query.get('model_like') ?? '',
    protocol: query.get('client_protocol') ?? '', token: query.get('auth_token_id') ?? '', channelId: query.get('channel_id') ?? '', hideZero: stored.hideZero ?? true,
  }
}

export function StatsPage() {
  const session = useSession()
  const hideChannels = shouldHideChannels(session)
  const tokenOptions = useTokenOptions()
  const [filters, setFilters] = useState<Filters>(initialFilters)
  const [options, setOptions] = useState<{ channel_names: string[]; models: string[] }>({ channel_names: [], models: [] })
  const [data, setData] = useState<StatsResponse>({})
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [sort, setSort] = useState<{ key: SortKey; dir: 'asc' | 'desc' } | null>(null)
  const [view, setView] = useState<'table' | 'chart'>(() => localStorage.getItem(VIEW_KEY) === 'chart' ? 'chart' : 'table')
  const generation = useRef(0)
  const lastFilters = useRef<Filters | null>(null)

  // 精确或模糊：值命中筛选项时按精确匹配（channel_name/model），否则模糊（*_like），与旧版一致。
  const queryParams = useCallback((value: Filters): Record<string, string | number> => {
    const params: Record<string, string | number> = { ...rangeParams(value.range) }
    if (!hideChannels) {
      if (value.channelId) params.channel_id = value.channelId
      if (value.channel) params[options.channel_names.includes(value.channel) ? 'channel_name' : 'channel_name_like'] = value.channel
    }
    if (value.model) params[options.models.includes(value.model) ? 'model' : 'model_like'] = value.model
    if (value.protocol) params.client_protocol = value.protocol
    if (value.token && tokenOptions) params.auth_token_id = value.token
    return params
  }, [hideChannels, options, tokenOptions])

  const load = useCallback(async () => {
    const current = ++generation.current
    setLoading(true)
    try {
      const response = await getJSON<StatsResponse>('/dashboard/stats', queryParams(filters))
      if (current === generation.current) { setData(response); setError('') }
    } catch (cause) { if (current === generation.current) setError(cause instanceof Error ? cause.message : '统计加载失败') }
    finally { if (current === generation.current) setLoading(false) }
  }, [filters, queryParams])

  // 筛选项只带时间参数，避免当前筛选把候选缩窄（旧版 stats.js:709-711）。
  useEffect(() => {
    void getJSON<{ channel_names?: string[]; models?: string[] }>('/dashboard/stats/filter-options', rangeParams(filters.range))
      .then((value) => setOptions({ channel_names: value.channel_names ?? [], models: value.models ?? [] })).catch(() => undefined)
  }, [filters.range])

  useEffect(() => {
    const { hideZero, ...rest } = filters
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ ...rest, channelId: undefined, hideZero }))
    const params = queryParams(filters)
    // 首次加载与筛选项异步就绪只替换 URL；用户改动筛选才写入历史，便于前进后退与分享。
    writeQuery(params, lastFilters.current !== null && lastFilters.current !== filters)
    lastFilters.current = filters
    void load()
  }, [filters, load, queryParams])

  useAutoRefresh(() => void load())
  useEffect(() => { localStorage.setItem(VIEW_KEY, view) }, [view])

  const set = (patch: Partial<Filters>) => setFilters((current) => ({ ...current, ...patch }))
  const clear = () => setFilters({ range: { range: 'today' }, channel: '', model: '', protocol: '', token: '', channelId: '', hideZero: true })

  const all = data.stats ?? []
  const rows = useMemo(() => {
    const filtered = filters.hideZero ? all.filter((entry) => num(entry.success) > 0) : [...all]
    const value = (entry: Entry, key: SortKey): number | string => {
      switch (key) {
        case 'channel_name': return String(entry.channel_name ?? '')
        case 'model': return String(entry.model ?? '')
        case 'duration': return num(entry.avg_duration_seconds ?? entry.avg_first_byte_time_seconds)
        case 'speed': return rowSpeed(entry) ?? 0
        case 'rpm': return num(entry.peak_rpm)
        case 'input': return num(entry.total_input_tokens)
        case 'output': return num(entry.total_output_tokens)
        case 'cache_read': return num(entry.total_cache_read_input_tokens)
        case 'cache_creation': return num(entry.total_cache_creation_input_tokens)
        case 'cost': return num(entry.total_cost)
        default: return num(entry[key])
      }
    }
    if (!sort) {
      // 默认：渠道优先级降序 → 渠道名 → 模型（旧版 stats.js:812-845）。
      return filtered.sort((a, b) => num(b.channel_priority) - num(a.channel_priority) || String(a.channel_name ?? '').localeCompare(String(b.channel_name ?? '')) || String(a.model ?? '').localeCompare(String(b.model ?? '')))
    }
    return filtered.sort((a, b) => {
      const left = value(a, sort.key); const right = value(b, sort.key)
      const result = typeof left === 'string' && typeof right === 'string' ? left.localeCompare(right) : Number(left) - Number(right)
      return sort.dir === 'asc' ? result : -result
    })
  }, [all, filters.hideZero, sort])

  const totals = useMemo(() => {
    const sum = (pick: (entry: Entry) => number) => rows.reduce((acc, entry) => acc + pick(entry), 0)
    const success = sum((entry) => num(entry.success))
    const weighted = (field: 'avg_first_byte_time_seconds' | 'avg_duration_seconds') => {
      let total = 0; let weight = 0
      for (const entry of rows) { const value = num(entry[field]); const w = num(entry.success); if (value > 0 && w > 0) { total += value * w; weight += w } }
      return weight ? total / weight : undefined
    }
    const speedOut = sum((entry) => num(entry.speed_output_tokens)); const speedSeconds = sum((entry) => num(entry.speed_duration_seconds))
    return {
      success, error: sum((entry) => num(entry.error)), ttft: weighted('avg_first_byte_time_seconds'), duration: weighted('avg_duration_seconds'),
      speed: speedSeconds > 0 ? speedOut / speedSeconds : null,
      input: sum((entry) => num(entry.total_input_tokens)), output: sum((entry) => num(entry.total_output_tokens)),
      read: sum((entry) => num(entry.total_cache_read_input_tokens)), creation: sum((entry) => num(entry.total_cache_creation_input_tokens)),
      cost: sum((entry) => num(entry.total_cost)), effective: sum((entry) => num(entry.effective_cost ?? entry.total_cost)),
    }
  }, [rows])

  const toggleSort = (key: SortKey) => setSort((current) => !current || current.key !== key ? { key, dir: 'desc' } : current.dir === 'desc' ? { key, dir: 'asc' } : null)
  const header = (key: SortKey, label: string) => <th><button className="link-button" onClick={() => toggleSort(key)}>{label}{sort?.key === key ? (sort.dir === 'desc' ? ' ↓' : ' ↑') : ''}</button></th>
  // 跳转日志时带上当前时间范围；点模型时同时带上渠道（旧版 stats.js:102-110）。
  const logsLink = (entry: Entry, withModel: boolean) => {
    const params = new URLSearchParams(Object.entries(rangeParams(filters.range)).map(([key, value]) => [key, String(value)]))
    if (!hideChannels && entry.channel_name) params.set('channel_name', entry.channel_name)
    if (withModel && entry.model) params.set('model', entry.model)
    return `/web/logs?${params.toString()}`
  }
  const today = Boolean(data.is_today)
  const columns = hideChannels ? 13 : 14

  return <>
    <header className="page-header">
      <div><h1>统计分析</h1><p className="muted">按渠道与模型查看请求、延迟、Token 与成本</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <button className={`btn${view === 'table' ? ' btn-primary' : ''}`} onClick={() => setView('table')}>表格</button>
        <button className={`btn${view === 'chart' ? ' btn-primary' : ''}`} onClick={() => setView('chart')}>图表</button>
        <button className="btn" disabled={loading} onClick={() => void load()}>刷新</button>
      </div>
    </header>

    <div className="toolbar">
      <DateRangeFilter value={filters.range} onChange={(range) => set({ range })} />
      {!hideChannels && <SearchableSelect ariaLabel="渠道" className="combobox-inline" allowCustomInput value={filters.channel} options={[{ value: '', label: '所有渠道' }, ...options.channel_names.map((name) => ({ value: name, label: name }))]} onChange={(channel) => set({ channel })} placeholder="渠道" />}
      <SearchableSelect ariaLabel="模型" className="combobox-inline" allowCustomInput value={filters.model} options={[{ value: '', label: '所有模型' }, ...options.models.map((name) => ({ value: name, label: name }))]} onChange={(model) => set({ model })} placeholder="模型" />
      <SearchableSelect ariaLabel="请求协议" className="combobox-inline" value={filters.protocol} options={PROTOCOLS} onChange={(protocol) => set({ protocol })} />
      {tokenOptions && <SearchableSelect ariaLabel="令牌" className="combobox-inline" value={filters.token} options={[{ value: '', label: '全部令牌' }, ...tokenOptions]} onChange={(token) => set({ token })} />}
      {filters.channelId && !hideChannels && <span className="badge">渠道 ID {filters.channelId} <button className="link-button" aria-label="移除渠道 ID 筛选" onClick={() => set({ channelId: '' })}>×</button></span>}
      <label className="switch"><input type="checkbox" checked={filters.hideZero} onChange={(event) => set({ hideZero: event.target.checked })} /> 隐藏 0 成功</label>
      <button className="btn" onClick={clear}>清空</button>
    </div>
    {error && <div className="card error-text" role="alert">{error}</div>}

    {view === 'table' ? <div className="table-wrap"><table><thead><tr>
      {!hideChannels && header('channel_name', '渠道')}{header('model', '模型')}{header('success', '成功 / 成功率')}{header('error', '失败')}
      {header('duration', '首字 / 耗时')}{header('speed', 'Tok/s')}{header('rpm', today ? 'RPM 峰/均/近' : 'RPM 峰/均')}
      {header('input', '输入')}{header('output', '输出')}{header('cache_read', '缓存读')}{header('cache_creation', '缓存建')}<th>缓存命中</th>{header('cost', '成本')}
      {!hideChannels && <th>健康</th>}
    </tr></thead><tbody>
      {!rows.length && <tr><td colSpan={columns} className="muted">{loading ? '加载中…' : '暂无数据'}</td></tr>}
      {rows.map((entry, index) => {
        const success = num(entry.success); const total = num(entry.total) || success + num(entry.error)
        const multiplier = entry.cost_multiplier_min != null ? (entry.cost_multiplier_min === entry.cost_multiplier_max ? `×${entry.cost_multiplier_min}` : `×${entry.cost_multiplier_min}–${entry.cost_multiplier_max}`) : ''
        const speed = rowSpeed(entry)
        return <tr key={`${entry.channel_id ?? entry.channel_name}-${entry.model}-${index}`}>
          {!hideChannels && <td><a className="link-button" href={logsLink(entry, false)}>{entry.channel_name || '-'}</a>{entry.channel_id != null && <span className="table-note">ID: {entry.channel_id}</span>}</td>}
          <td><a className="link-button" href={logsLink(entry, true)}>{entry.model || '-'}</a>{multiplier && <span className="badge" title="成本倍率">{multiplier}</span>}</td>
          <td className="cell-stack"><span className="success-text">{formatInt(success)}</span><span className="table-note">{formatPercent(success, total)}</span></td>
          <td className={num(entry.error) ? 'error-text' : 'muted'}>{formatInt(entry.error)}</td>
          <td className="cell-stack"><span>{formatSeconds(entry.avg_first_byte_time_seconds)}</span><span className="table-note">{formatSeconds(entry.avg_duration_seconds)}</span></td>
          <td>{speed == null ? '-' : speed.toFixed(1)}</td>
          <td>{[entry.peak_rpm, entry.avg_rpm, ...(today ? [entry.recent_rpm] : [])].map((value) => num(value).toFixed(1)).join(' / ')}</td>
          <td>{formatCompact(entry.total_input_tokens)}</td><td>{formatCompact(entry.total_output_tokens)}</td>
          <td>{formatCompact(entry.total_cache_read_input_tokens)}</td><td>{formatCompact(entry.total_cache_creation_input_tokens)}</td>
          <td>{cacheHitRate(entry.total_input_tokens, entry.total_cache_read_input_tokens, entry.total_cache_creation_input_tokens)}</td>
          <td>{formatCostPair(entry.total_cost, entry.effective_cost)}</td>
          {!hideChannels && <td><HealthBar points={entry.health_timeline ?? []} /></td>}
        </tr>
      })}
      {rows.length > 0 && <tr className="totals-row">
        {!hideChannels && <td><strong>合计</strong></td>}<td>{hideChannels ? <strong>合计</strong> : `${rows.length} 行`}</td>
        <td className="cell-stack"><span className="success-text">{formatInt(totals.success)}</span><span className="table-note">{formatPercent(totals.success, totals.success + totals.error)}</span></td>
        <td>{formatInt(totals.error)}</td>
        <td className="cell-stack"><span>{formatSeconds(totals.ttft)}</span><span className="table-note">{formatSeconds(totals.duration)}</span></td>
        <td>{totals.speed == null ? '-' : totals.speed.toFixed(1)}</td>
        <td>{data.rpm_stats ? [data.rpm_stats.peak_rpm, data.rpm_stats.avg_rpm, ...(today ? [data.rpm_stats.recent_rpm] : [])].map((value) => num(value).toFixed(1)).join(' / ') : '-'}</td>
        <td>{formatCompact(totals.input)}</td><td>{formatCompact(totals.output)}</td><td>{formatCompact(totals.read)}</td><td>{formatCompact(totals.creation)}</td>
        <td>{cacheHitRate(totals.input, totals.read, totals.creation)}</td><td>{formatCostPair(totals.cost, totals.effective)}</td>
        {!hideChannels && <td />}
      </tr>}
    </tbody></table></div> : <Charts entries={all} hideChannels={hideChannels} />}
  </>
}

/** 每行健康条：阈值 95%/80%，全部失败均为 429 时显示限流色（旧版 stats.js:950-1009）。 */
function HealthBar({ points }: { points: HealthPoint[] }) {
  if (!points.length) return <span className="muted">-</span>
  return <div className="health-strip health-strip-sm" role="img" aria-label="健康时间线">{points.map((point, index) => {
    const success = num(point.success); const error = num(point.error); const limited = num(point.rate_limited)
    const rate = point.rate == null || point.rate < 0 || success + error === 0 ? null : point.rate
    const cls = rate == null ? 'health-unknown' : error > 0 && limited === error && success === 0 ? 'health-limited' : rate >= 0.95 ? 'health-good' : rate >= 0.8 ? 'health-warn' : 'health-bad'
    const tip = `${formatDateTime(point.ts)}\n成功 ${success} · 失败 ${error}${limited ? `（限流 ${limited}）` : ''}${rate == null ? '\n无请求' : `\n成功率 ${(rate * 100).toFixed(1)}%`}\n首字 ${formatSeconds(point.avg_first_byte_time)} · 耗时 ${formatSeconds(point.avg_duration)}\nToken 入 ${formatCompact(point.input_tokens)} 出 ${formatCompact(point.output_tokens)} 缓读 ${formatCompact(point.cache_read_tokens)} 缓建 ${formatCompact(point.cache_creation_tokens)}\n成本 ${formatCostPair(point.cost, point.effective_cost)}`
    return <i key={index} className={cls} title={tip} />
  })}</div>
}

type Slice = { label: string; value: number; detail?: string }

/** 6 个环形图：渠道/模型 × 调用次数（仅成功）/ 成本（倍率后）/ Token（含缓存），不受「隐藏 0 成功」影响。 */
function Charts({ entries, hideChannels }: { entries: Entry[]; hideChannels: boolean }) {
  const group = (key: 'channel_name' | 'model', pick: (entry: Entry) => number, detail?: (entries: Entry[]) => string): Slice[] => {
    const map = new Map<string, Entry[]>()
    for (const entry of entries) { if (num(entry.success) <= 0) continue; const label = String(entry[key] || '未知'); map.set(label, [...(map.get(label) ?? []), entry]) }
    return [...map.entries()].map(([label, items]) => ({ label, value: items.reduce((acc, item) => acc + pick(item), 0), detail: detail?.(items) })).filter((slice) => slice.value > 0).sort((a, b) => b.value - a.value)
  }
  const tokens = (entry: Entry) => num(entry.total_input_tokens) + num(entry.total_output_tokens) + num(entry.total_cache_read_input_tokens) + num(entry.total_cache_creation_input_tokens)
  const cost = (entry: Entry) => num(entry.effective_cost ?? entry.total_cost)
  const costDetail = (items: Entry[]) => `标准 ${formatUSD(items.reduce((acc, item) => acc + num(item.total_cost), 0))}`
  const charts: Array<{ title: string; slices: Slice[]; format: (value: number) => string }> = [
    ...(hideChannels ? [] : [{ title: '渠道调用次数（成功）', slices: group('channel_name', (entry) => num(entry.success)), format: formatInt }]),
    { title: '模型调用次数（成功）', slices: group('model', (entry) => num(entry.success)), format: formatInt },
    ...(hideChannels ? [] : [{ title: '渠道成本', slices: group('channel_name', cost, costDetail), format: formatUSD }]),
    { title: '模型成本', slices: group('model', cost, costDetail), format: formatUSD },
    ...(hideChannels ? [] : [{ title: '渠道 Token', slices: group('channel_name', tokens), format: formatCompact }]),
    { title: '模型 Token', slices: group('model', tokens), format: formatCompact },
  ]
  return <section className="charts-grid">{charts.map((chart) => <Donut key={chart.title} {...chart} />)}</section>
}

function Donut({ title, slices, format }: { title: string; slices: Slice[]; format: (value: number) => string }) {
  const [active, setActive] = useState<number | null>(null)
  const total = slices.reduce((acc, slice) => acc + slice.value, 0)
  let cursor = 0
  const stops = slices.map((slice, index) => { const from = cursor; cursor += (slice.value / total) * 360; return `${COLORS[index % COLORS.length]} ${from}deg ${cursor}deg` }).join(', ')
  const focus = active == null ? null : slices[active]
  return <div className="chart-card">
    <h2>{title}</h2>
    {!total ? <div className="pie-breakdown"><div className="pie-breakdown-empty">暂无数据</div></div> : <div className="pie-breakdown">
      <div className="pie-breakdown-chart" style={{ background: `conic-gradient(${stops})` }}><div title={focus?.label}>{focus ? `${focus.label}\n${format(focus.value)}` : format(total)}</div></div>
      <div className="pie-breakdown-legend pie-legend-scroll">{slices.map((slice, index) => (
        <div key={slice.label} className={active === index ? 'active' : undefined} onMouseEnter={() => setActive(index)} onMouseLeave={() => setActive(null)} title={`${slice.label}\n${format(slice.value)}（${((slice.value / total) * 100).toFixed(1)}%）${slice.detail ? `\n${slice.detail}` : ''}`}>
          <i style={{ background: COLORS[index % COLORS.length] }} /><span>{slice.label}</span><strong>{((slice.value / total) * 100).toFixed(1)}%</strong>
        </div>
      ))}</div>
    </div>}
  </div>
}
