import { useEffect, useMemo, useRef, useState } from 'react'
import { getJSON } from '../lib/api'
import type { StatsEntry } from '../types'
import { ChartPie, Table2 } from 'lucide-react'

type SortKey = 'channel_name' | 'model' | 'success' | 'error' | 'total' | 'rpm' | 'success_rate' | 'duration' | 'speed' | 'input' | 'output' | 'cost' | 'cache_read' | 'cache_creation'
type FilterOptions = { channel_names?: string[]; models?: string[] }
type StatsView = 'table' | 'chart'
const FILTER_STORAGE_KEY = 'ccload_stats_filters'
const VIEW_STORAGE_KEY = 'stats.view'

function readStatsFilters() {
  try {
    const value = JSON.parse(localStorage.getItem(FILTER_STORAGE_KEY) ?? '{}') as Record<string, unknown>
    return {
      range: String(value.range ?? 'today'), channel: String(value.channel ?? ''), model: String(value.model ?? ''),
      protocol: String(value.protocol ?? ''), token: String(value.token ?? ''), start: String(value.start ?? ''), end: String(value.end ?? ''),
      exact: value.exact === true, hideZero: value.hideZero !== false,
    }
  } catch { return { range: 'today', channel: '', model: '', protocol: '', token: '', start: '', end: '', exact: false, hideZero: true } }
}
function readStatsView(): StatsView {
  try { return localStorage.getItem(VIEW_STORAGE_KEY) === 'chart' ? 'chart' : 'table' } catch { return 'table' }
}
function numericRangeValue(value: string) {
  const parsed = Date.parse(value)
  return Number.isFinite(parsed) ? parsed : undefined
}

export function StatsPage() {
  const [savedFilters] = useState(readStatsFilters)
  const [range, setRange] = useState(savedFilters.range); const [channel, setChannel] = useState(savedFilters.channel); const [model, setModel] = useState(savedFilters.model); const [protocol, setProtocol] = useState(savedFilters.protocol); const [token, setToken] = useState(savedFilters.token); const [start, setStart] = useState(savedFilters.start); const [end, setEnd] = useState(savedFilters.end); const [exact, setExact] = useState(savedFilters.exact); const [hideZero, setHideZero] = useState(savedFilters.hideZero)
  const [view, setView] = useState<StatsView>(readStatsView)
  const [sort, setSort] = useState<SortKey>('total'); const [direction, setDirection] = useState<'asc' | 'desc' | 'none'>('none'); const [rows, setRows] = useState<StatsEntry[]>([]); const [rpm, setRpm] = useState<Record<string, number>>({}); const [options, setOptions] = useState<FilterOptions>({}); const [loading, setLoading] = useState(false); const [error, setError] = useState<string | null>(null)
  const [isToday, setIsToday] = useState(true)
  const requestVersion = useRef(0)
  const queryParams = (includeHealth = false) => ({ range, channel_name: exact && channel ? channel : undefined, channel_name_like: !exact && channel ? channel : undefined, model: exact && model ? model : undefined, model_like: !exact && model ? model : undefined, client_protocol: protocol || undefined, auth_token_id: token || undefined, start_time: range === 'custom' ? numericRangeValue(start) : undefined, end_time: range === 'custom' ? numericRangeValue(end) : undefined, include_health: includeHealth })
  const refresh = async () => {
    const version = ++requestVersion.current
    setLoading(true); setError(null)
    try {
      // Render the aggregate table immediately. Health buckets are hydrated in
      // the background because they require a separate grouped log query.
      const result = await getJSON<{ stats?: StatsEntry[]; rpm_stats?: Record<string, number>; is_today?: boolean }>('/dashboard/stats', queryParams(false))
      if (version !== requestVersion.current) return
      setRows(result.stats ?? []); setRpm(result.rpm_stats ?? {}); setIsToday(result.is_today !== false); setLoading(false)
      void getJSON<{ stats?: StatsEntry[] }>('/dashboard/stats', queryParams(true)).then((health) => {
        if (version === requestVersion.current && health.stats) setRows(health.stats)
      }).catch(() => { /* The aggregate table is still useful without the timeline. */ })
    } catch (cause) { if (version === requestVersion.current) { setError(cause instanceof Error ? cause.message : '加载统计失败'); setLoading(false) } }
  }
  const loadOptions = async () => { try { setOptions(await getJSON<FilterOptions>('/dashboard/stats/filter-options', queryParams())) } catch { setOptions({}) } }
  useEffect(() => { void refresh(); void loadOptions() }, [range])
  useEffect(() => {
    try { localStorage.setItem(FILTER_STORAGE_KEY, JSON.stringify({ range, channel, model, protocol, token, start, end, exact, hideZero })) } catch { /* Storage may be unavailable. */ }
  }, [range, channel, model, protocol, token, start, end, exact, hideZero])
  useEffect(() => { try { localStorage.setItem(VIEW_STORAGE_KEY, view) } catch { /* Storage may be unavailable. */ } }, [view])
  const filtered = useMemo(() => {
    const result = rows.filter((row) => !hideZero || Number(row.success ?? 0) > 0)
    const value = (row: StatsEntry): string | number => { if (sort === 'channel_name') return String(row.channel_name ?? ''); if (sort === 'model') return String(row.model ?? ''); if (sort === 'success_rate') return Number(row.total ?? 0) ? Number(row.success ?? 0) / Number(row.total) : 0; if (sort === 'cost') return Number(row.effective_cost ?? row.total_cost ?? 0); if (sort === 'rpm') return Number(row.peak_rpm ?? 0); if (sort === 'duration') return Number(row.avg_duration_seconds ?? 0); if (sort === 'speed') return Number(row.speed_output_tokens ?? 0) / Math.max(Number(row.speed_duration_seconds ?? 0), 0.001); if (sort === 'input') return Number(row.total_input_tokens ?? 0); if (sort === 'output') return Number(row.total_output_tokens ?? 0); if (sort === 'cache_read') return Number(row.total_cache_read_input_tokens ?? 0); if (sort === 'cache_creation') return Number(row.total_cache_creation_input_tokens ?? 0); return Number(row[sort] ?? 0) }
    if (direction === 'none') return result
    return [...result].sort((a, b) => { const left = value(a); const right = value(b); const comparison = typeof left === 'string' && typeof right === 'string' ? left.localeCompare(right) : Number(left) - Number(right); return direction === 'asc' ? comparison : -comparison })
  }, [rows, hideZero, sort, direction])
  const cycleSort = (key: SortKey) => { if (sort !== key) { setSort(key); setDirection('desc'); return }; setDirection((current) => current === 'none' ? 'desc' : current === 'desc' ? 'asc' : 'none') }
  const heading = (key: SortKey, label: string) => <th className="sortable" onClick={() => cycleSort(key)}>{label}{sort === key && direction !== 'none' ? direction === 'desc' ? ' ↓' : ' ↑' : ''}</th>
  const applyFilters = () => { void refresh(); void loadOptions() }
  const chartGroups = useMemo(() => {
    const grouped = (key: 'channel_name' | 'model', value: (row: StatsEntry) => number) => {
      const totals = new Map<string, number>()
      for (const row of filtered) {
        const label = String(row[key] ?? '未知')
        totals.set(label, (totals.get(label) ?? 0) + Math.max(0, value(row)))
      }
      return [...totals].sort((a, b) => b[1] - a[1]).slice(0, 10)
    }
    const calls = (row: StatsEntry) => Number(row.total ?? 0)
    const cost = (row: StatsEntry) => Number(row.effective_cost ?? row.total_cost ?? 0)
    const tokens = (row: StatsEntry) => Number(row.total_input_tokens ?? 0) + Number(row.total_output_tokens ?? 0)
    return [
      { key: 'channel_calls', title: '渠道调用次数', data: grouped('channel_name', calls) },
      { key: 'model_calls', title: '模型调用次数', data: grouped('model', calls) },
      { key: 'channel_cost', title: '渠道成本', data: grouped('channel_name', cost) },
      { key: 'model_cost', title: '模型成本', data: grouped('model', cost) },
      { key: 'channel_tokens', title: '渠道 Token 用量', data: grouped('channel_name', tokens) },
      { key: 'model_tokens', title: '模型 Token 用量', data: grouped('model', tokens) },
    ]
  }, [filtered])
  return <><header className="page-header"><div><h1>统计分析</h1><p className="muted">按渠道和模型查看调用、Token 与成本</p></div><button className="btn" onClick={() => void refresh()} disabled={loading}>刷新</button></header>
    <div className="toolbar"><select className="select" value={range} onChange={(event) => setRange(event.target.value)}><option value="today">今天</option><option value="yesterday">昨天</option><option value="this_week">本周</option><option value="this_month">本月</option><option value="all">全部</option><option value="custom">自定义</option></select><input className="input" list="stats-channels" value={channel} onChange={(event) => setChannel(event.target.value)} placeholder="渠道筛选" /><datalist id="stats-channels">{(options.channel_names ?? []).map((item) => <option key={item} value={item} />)}</datalist><input className="input" list="stats-models" value={model} onChange={(event) => setModel(event.target.value)} placeholder="模型筛选" /><datalist id="stats-models">{(options.models ?? []).map((item) => <option key={item} value={item} />)}</datalist><label className="muted"><input type="checkbox" checked={exact} onChange={(event) => setExact(event.target.checked)} /> 精确</label><label className="muted"><input type="checkbox" checked={hideZero} onChange={(event) => setHideZero(event.target.checked)} /> 隐藏零成功</label><select className="select" value={protocol} onChange={(event) => setProtocol(event.target.value)}><option value="">全部入口协议</option><option value="openai">OpenAI</option><option value="anthropic">Anthropic</option><option value="gemini">Gemini</option><option value="codex">Codex</option></select><input className="input" value={token} onChange={(event) => setToken(event.target.value)} placeholder="Token ID" />{range === 'custom' && <><input className="input" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /><input className="input" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></>}<button className="btn btn-primary" onClick={applyFilters}>查询</button><span className="muted">峰值 RPM {Number(rpm.peak_rpm ?? 0).toFixed(1)}</span></div>
    {error && <div className="card error-text">{error}</div>}
    <div className="toolbar" role="group" aria-label="统计视图"><button className={view === 'table' ? 'btn btn-primary' : 'btn'} onClick={() => setView('table')} aria-pressed={view === 'table'}><Table2 size={16} /> 表格</button><button className={view === 'chart' ? 'btn btn-primary' : 'btn'} onClick={() => setView('chart')} aria-pressed={view === 'chart'}><ChartPie size={16} /> 图表</button></div>
    {view === 'table' ? <div className="table-wrap"><table><thead><tr>{heading('channel_name', '渠道')}{heading('model', '模型')}{heading('success', '成功')}{heading('error', '失败')}{heading('total', '总调用')}{heading('rpm', 'RPM 峰/均/近')}{heading('success_rate', '成功率')}{heading('duration', '首字/耗时 (s)')}{heading('speed', '速度 tok/s')}{heading('input', '输入 Token')}{heading('output', '输出 Token')}{heading('cache_read', '缓存读取')}{heading('cache_creation', '缓存创建')}<th>缓存命中</th>{heading('cost', '成本 (USD)')}</tr></thead><tbody>{loading ? <tr><td colSpan={15}>加载中...</td></tr> : filtered.length === 0 ? <tr><td colSpan={15}>暂无数据</td></tr> : <>{filtered.map((row, index) => { const total = Number(row.total ?? 0); const rate = total ? Number(row.success ?? 0) / total : 0; const speed = Number(row.speed_output_tokens ?? 0) / Math.max(Number(row.speed_duration_seconds ?? 0), 0.001); const cacheRead = Number(row.total_cache_read_input_tokens ?? 0); const cacheCreation = Number(row.total_cache_creation_input_tokens ?? 0); const input = Number(row.total_input_tokens ?? 0); const rpmText = `${Number(row.peak_rpm ?? 0).toFixed(1)} / ${Number(row.avg_rpm ?? 0).toFixed(1)} / ${isToday ? Number(row.recent_rpm ?? 0).toFixed(1) : '-'}`; return <tr key={`${row.channel_id}-${row.model}-${index}`}><td><button className="link-button" onClick={() => { window.location.href = `/web/logs?channel_name=${encodeURIComponent(String(row.channel_name ?? ''))}` }}>{String(row.channel_name ?? '-')}</button></td><td><button className="link-button" onClick={() => { window.location.href = `/web/logs?model=${encodeURIComponent(String(row.model ?? ''))}` }}>{String(row.model ?? '-')}</button></td><td>{Number(row.success ?? 0)}</td><td>{Number(row.error ?? 0)}</td><td>{total}</td><td>{rpmText}</td><td>{total ? `${(rate * 100).toFixed(1)}%` : '-'}</td><td>{Number(row.avg_first_byte_time_seconds ?? 0).toFixed(2)} / {Number(row.avg_duration_seconds ?? 0).toFixed(2)}</td><td>{Number.isFinite(speed) && Number(row.speed_output_tokens ?? 0) > 0 ? speed.toFixed(2) : '-'}</td><td>{input.toLocaleString()}</td><td>{Number(row.total_output_tokens ?? 0).toLocaleString()}</td><td>{cacheRead.toLocaleString()}</td><td>{cacheCreation.toLocaleString()}</td><td>{input ? `${((cacheRead + cacheCreation) / input * 100).toFixed(1)}%` : '-'}</td><td>{Number(row.effective_cost ?? row.total_cost ?? 0).toFixed(4)}</td></tr> })}<tr><th colSpan={2}>合计</th><th>{filtered.reduce((sum, row) => sum + Number(row.success ?? 0), 0)}</th><th>{filtered.reduce((sum, row) => sum + Number(row.error ?? 0), 0)}</th><th>{filtered.reduce((sum, row) => sum + Number(row.total ?? 0), 0)}</th><th>{Number(rpm.peak_rpm ?? 0).toFixed(1)} / {Number(rpm.avg_rpm ?? 0).toFixed(1)} / {isToday ? Number(rpm.recent_rpm ?? 0).toFixed(1) : '-'}</th><td colSpan={3}></td><th>{filtered.reduce((sum, row) => sum + Number(row.total_input_tokens ?? 0), 0).toLocaleString()}</th><th>{filtered.reduce((sum, row) => sum + Number(row.total_output_tokens ?? 0), 0).toLocaleString()}</th><th>{filtered.reduce((sum, row) => sum + Number(row.total_cache_read_input_tokens ?? 0), 0).toLocaleString()}</th><th>{filtered.reduce((sum, row) => sum + Number(row.total_cache_creation_input_tokens ?? 0), 0).toLocaleString()}</th><td>-</td><th>{filtered.reduce((sum, row) => sum + Number(row.effective_cost ?? row.total_cost ?? 0), 0).toFixed(4)}</th></tr></>}</tbody></table></div> : <div className="charts-grid">{chartGroups.map((group) => <section className="chart-card" key={group.key}><h2>{group.title}</h2><PieBreakdown data={group.data} /></section>)}</div>}
    <div className="card" style={{ marginTop: 16 }}><h2>渠道健康时间线</h2><div className="health-grid">{filtered.slice(0, 20).map((row, index) => { const timeline = Array.isArray(row.health_timeline) ? row.health_timeline as Array<{ rate?: number }> : []; return <div className="health-line" key={`${row.channel_id}-${row.model}-${index}`}><span>{row.channel_name} / {row.model}</span><div>{timeline.map((point, pointIndex) => { const rate = Number(point.rate ?? -1); return <i key={pointIndex} title={rate < 0 ? '无请求' : `${(rate * 100).toFixed(1)}%`} style={{ background: rate < 0 ? '#64748b' : rate >= 0.95 ? '#22c55e' : rate >= 0.7 ? '#eab308' : '#ef4444' }} /> })}</div></div> })}</div></div>
  </>
}

function PieBreakdown({ data }: { data: Array<[string, number]> }) {
  const total = data.reduce((sum, [, value]) => sum + value, 0)
  const colors = ['#2878c8', '#1b8f78', '#d97706', '#c2415d', '#6656b3', '#4f7d32', '#0f9bb5', '#a14e82', '#697586', '#9a7023']
  let offset = 0
  const stops = data.map(([, value], index) => {
    const start = offset
    offset += total > 0 ? value / total * 100 : 0
    return `${colors[index % colors.length]} ${start}% ${offset}%`
  })
  return <div className="pie-breakdown">{data.length && total > 0 ? <div aria-label={`分布总计 ${total.toLocaleString()}`} className="pie-breakdown-chart" style={{ background: `conic-gradient(${stops.join(', ')})` }}><div>{total.toLocaleString()}</div></div> : <div className="pie-breakdown-empty">暂无数据</div>}<div className="pie-breakdown-legend">{data.map(([label, value], index) => <div key={label}><i style={{ backgroundColor: colors[index % colors.length] }} /><span title={label}>{label}</span><strong>{total ? `${(value / total * 100).toFixed(1)}%` : value}</strong></div>)}</div></div>
}
