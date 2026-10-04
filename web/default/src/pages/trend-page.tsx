import { useCallback, useEffect, useMemo, useRef, useState, type MouseEvent } from 'react'
import { DateRangeFilter, normalizeRange, rangeParams, type DateRangeValue } from '../components/date-range'
import { SearchableSelect } from '../components/searchable-select'
import { SearchableMultiSelect } from '../components/searchable-multi-select'
import { api, getJSON } from '../lib/api'
import { shouldHideChannels } from '../lib/auth'
import { formatCompact, formatInt, formatUSD } from '../lib/format'
import { useSession } from '../lib/session-context'
import { readQuery, readStored, writeQuery } from '../lib/url-state'
import { useAutoRefresh } from '../hooks/use-auto-refresh'
import { useTokenOptions } from '../hooks/use-token-options'

type ChannelMetric = { success?: number; error?: number; avg_first_byte_time_seconds?: number; avg_duration_seconds?: number; total_cost?: number; effective_cost?: number; input_tokens?: number; output_tokens?: number; cache_read_tokens?: number; cache_creation_tokens?: number }
type Point = ChannelMetric & { ts: string; channels?: Record<string, ChannelMetric> }
type TrendType = 'count' | 'rpm' | 'first_byte' | 'duration' | 'tokens' | 'cost'
// 字段名与旧版 trend.js:TREND_FILTER_FIELDS 一致，可直接读取旧存储。
type Stored = { range: string; customStartTime: string; customEndTime: string; trendType: TrendType; clientProtocol: string; model: string; authToken: string; channelName: string }

const STORAGE_KEY = 'trend.filters'
const CHANNELS_KEY = 'trend.visibleChannels'
const TYPES: Array<{ value: TrendType; label: string }> = [
  { value: 'count', label: '请求数' }, { value: 'rpm', label: 'RPM' }, { value: 'first_byte', label: '首字节' },
  { value: 'duration', label: '总耗时' }, { value: 'tokens', label: 'Token' }, { value: 'cost', label: '成本' },
]
const VALID_TYPES = new Set(TYPES.map((item) => item.value))
const PROTOCOLS = [{ value: '', label: '全部协议' }, { value: 'anthropic', label: 'Claude Code' }, { value: 'codex', label: 'Codex' }, { value: 'openai', label: 'OpenAI' }, { value: 'gemini', label: 'Gemini' }]
const CHANNEL_COLORS = ['#3b82f6', '#a855f7', '#f59e0b', '#06b6d4', '#ec4899', '#84cc16', '#f97316', '#14b8a6', '#6366f1', '#e11d48', '#0ea5e9', '#65a30d']
const num = (value: unknown) => { const parsed = Number(value); return Number.isFinite(parsed) ? parsed : 0 }
const latency = (value: unknown) => { const parsed = Number(value); return Number.isFinite(parsed) && parsed > 0 ? parsed : null }

/** 自动分桶（分钟）：≤1h→1，≤6h→2，≤24h→5，≤72h→15，其余 60（旧版 trend.js:309-315）。 */
function bucketFor(range: DateRangeValue): number {
  const now = Date.now()
  const startOfDay = new Date(); startOfDay.setHours(0, 0, 0, 0)
  const hours = range.range === 'custom' && range.start && range.end ? (range.end - range.start) / 3_600_000
    : range.range === 'today' ? (now - startOfDay.getTime()) / 3_600_000
    : range.range === 'yesterday' || range.range === 'day_before_yesterday' ? 24
    : range.range === 'this_week' || range.range === 'last_week' ? 168 : 744
  return hours <= 1 ? 1 : hours <= 6 ? 2 : hours <= 24 ? 5 : hours <= 72 ? 15 : 60
}

function initialState(): Stored {
  const stored = readStored<Stored>(STORAGE_KEY, { range: 'today', customStartTime: '', customEndTime: '', trendType: 'first_byte', clientProtocol: '', model: '', authToken: '', channelName: '' })
  const query = readQuery()
  const next = { ...stored }
  if (query.has('range')) { next.range = query.get('range') ?? 'today'; next.customStartTime = query.get('start_time') ?? ''; next.customEndTime = query.get('end_time') ?? '' }
  if (query.has('type')) next.trendType = query.get('type') as TrendType
  if (query.has('client_protocol')) next.clientProtocol = query.get('client_protocol') ?? ''
  if (query.has('model')) next.model = query.get('model') ?? ''
  if (query.has('token')) next.authToken = query.get('token') ?? ''
  if (query.has('channel_name_like')) next.channelName = query.get('channel_name_like') ?? ''
  if (!VALID_TYPES.has(next.trendType)) next.trendType = 'first_byte'
  return next
}

type Series = { name: string; color: string; values: Array<number | null>; dashed?: boolean; total?: boolean }

export function TrendPage() {
  const session = useSession()
  const hideChannels = shouldHideChannels(session)
  const tokenOptions = useTokenOptions()
  const [state, setState] = useState<Stored>(initialState)
  const range = useMemo(() => normalizeRange({ range: state.range, start: Number(state.customStartTime), end: Number(state.customEndTime) }), [state.range, state.customStartTime, state.customEndTime])
  const [points, setPoints] = useState<Point[]>([])
  const [debugTotal, setDebugTotal] = useState<string | null>(null)
  const [options, setOptions] = useState<{ models: string[]; channels: string[] }>({ models: [], channels: [] })
  const [visibleChannels, setVisibleChannels] = useState<string[]>(() => { try { return JSON.parse(localStorage.getItem(CHANNELS_KEY) ?? '[]') as string[] } catch { return [] } })
  const [chart, setChart] = useState<'line' | 'bar'>('line')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const generation = useRef(0)
  const bucket = bucketFor(range)

  const set = (patch: Partial<Stored>) => setState((current) => ({ ...current, ...patch }))
  const setRange = (value: DateRangeValue) => set({ range: value.range, customStartTime: value.start ? String(value.start) : '', customEndTime: value.end ? String(value.end) : '' })

  const load = useCallback(async () => {
    const current = ++generation.current
    setLoading(true)
    try {
      const params: Record<string, string | number> = { ...rangeParams(range), bucket_min: bucket }
      if (state.clientProtocol) params.client_protocol = state.clientProtocol
      if (state.model) params.model = state.model
      if (state.authToken && tokenOptions) params.auth_token_id = state.authToken
      if (state.channelName && !hideChannels) params.channel_name_like = state.channelName
      // 直接用 axios 读取响应头 X-Debug-Total（getJSON 只返回数据）。
      const response = await api.get('/dashboard/metrics', { params })
      if (current !== generation.current) return
      const payload = response.data as { data?: Point[] } | Point[]
      setPoints(Array.isArray(payload) ? payload : payload.data ?? [])
      setDebugTotal(response.headers['x-debug-total'] ?? null)
      setError('')
    } catch (cause) { if (current === generation.current) setError(cause instanceof Error ? cause.message : '趋势加载失败') }
    finally { if (current === generation.current) setLoading(false) }
  }, [range, bucket, state.clientProtocol, state.model, state.authToken, state.channelName, tokenOptions, hideChannels])

  // 模型与渠道选项随时间范围变化，custom 时必须带起止时间（handlers.go:140-144）。
  useEffect(() => {
    void getJSON<{ models?: string[]; channels?: Array<{ name?: string }> }>('/dashboard/models', rangeParams(range))
      .then((data) => setOptions({ models: data.models ?? [], channels: (data.channels ?? []).map((item) => String(item.name ?? '')).filter(Boolean) })).catch(() => undefined)
  }, [range])

  useEffect(() => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(state))
    // URL 键与旧版一致：today / first_byte 为默认值时不写。
    writeQuery({ range: state.range !== 'today' ? state.range : undefined, start_time: state.range === 'custom' ? state.customStartTime : undefined, end_time: state.range === 'custom' ? state.customEndTime : undefined, type: state.trendType !== 'first_byte' ? state.trendType : undefined, client_protocol: state.clientProtocol, model: state.model, token: state.authToken })
  }, [state])

  useEffect(() => { void load() }, [load])
  useAutoRefresh(() => void load(), { fixedSeconds: 300 })

  // 数据中出现过的渠道；已无数据的渠道从可见集合中剔除。
  const channelNames = useMemo(() => [...new Set(points.flatMap((point) => Object.keys(point.channels ?? {})))].sort(), [points])
  useEffect(() => {
    if (!points.length) return
    setVisibleChannels((current) => { const next = current.filter((name) => channelNames.includes(name)); return next.length === current.length ? current : next })
  }, [channelNames, points.length])
  useEffect(() => { localStorage.setItem(CHANNELS_KEY, JSON.stringify(visibleChannels)) }, [visibleChannels])

  const series = useMemo<Series[]>(() => {
    const type = state.trendType
    const pick = (metric: ChannelMetric | undefined, field: 'success' | 'error' | 'rpm' | 'first_byte' | 'duration' | 'io' | 'cost'): number | null => {
      if (!metric) return type === 'first_byte' || type === 'duration' ? null : 0
      switch (field) {
        case 'success': return num(metric.success)
        case 'error': return num(metric.error)
        case 'rpm': return (num(metric.success) + num(metric.error)) / bucket
        case 'first_byte': return latency(metric.avg_first_byte_time_seconds)
        case 'duration': return latency(metric.avg_duration_seconds)
        case 'io': return num(metric.input_tokens) + num(metric.output_tokens)
        case 'cost': return num(metric.total_cost)
      }
    }
    const result: Series[] = []
    const total = (name: string, color: string, values: Array<number | null>, dashed = false) => result.push({ name, color, values, dashed, total: true })
    if (type === 'count') { total('总成功', '#22c55e', points.map((point) => num(point.success))); total('总失败', '#ef4444', points.map((point) => num(point.error)), true) }
    else if (type === 'rpm') total('总 RPM', '#22c55e', points.map((point) => pick(point, 'rpm')))
    else if (type === 'first_byte') total('平均首字节', '#22c55e', points.map((point) => pick(point, 'first_byte')))
    else if (type === 'duration') total('平均耗时', '#22c55e', points.map((point) => pick(point, 'duration')))
    else if (type === 'tokens') {
      total('输入', '#3b82f6', points.map((point) => num(point.input_tokens))); total('输出', '#22c55e', points.map((point) => num(point.output_tokens)))
      total('缓存读', '#f59e0b', points.map((point) => num(point.cache_read_tokens)), true); total('缓存建', '#a855f7', points.map((point) => num(point.cache_creation_tokens)), true)
    } else { total('标准成本', '#22c55e', points.map((point) => num(point.total_cost))); total('倍率后成本', '#f59e0b', points.map((point) => num(point.effective_cost ?? point.total_cost)), true) }
    // 渠道线叠加在总计线之上，总计线始终保留（旧版行为）。
    if (!hideChannels) visibleChannels.forEach((name) => {
      const color = CHANNEL_COLORS[channelNames.indexOf(name) % CHANNEL_COLORS.length]
      const label = name === 'Unknown Channel' ? `⚠️ ${name}` : name
      const get = (field: Parameters<typeof pick>[1]) => points.map((point) => pick(point.channels?.[name], field))
      if (type === 'count') { result.push({ name: `${label} 成功`, color, values: get('success') }); result.push({ name: `${label} 失败`, color, values: get('error'), dashed: true }) }
      else result.push({ name: label, color, values: get(type === 'rpm' ? 'rpm' : type === 'first_byte' ? 'first_byte' : type === 'duration' ? 'duration' : type === 'tokens' ? 'io' : 'cost') })
    })
    return result
  }, [points, state.trendType, visibleChannels, channelNames, bucket, hideChannels])

  const format = state.trendType === 'cost' ? formatUSD : state.trendType === 'tokens' ? formatCompact
    : state.trendType === 'first_byte' || state.trendType === 'duration' ? (value: number) => `${value.toFixed(2)}s`
    : state.trendType === 'rpm' ? (value: number) => `${value.toFixed(1)}/min` : (value: number) => formatInt(Math.round(value))
  const requests = points.map((point) => num(point.success) + num(point.error))

  return <>
    <header className="page-header">
      <div><h1>趋势</h1><p className="muted">按时间查看请求量、延迟、Token 与成本走势</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}><button className="btn" disabled={loading} onClick={() => void load()}>刷新</button></div>
    </header>
    <div className="toolbar">
      <DateRangeFilter value={range} onChange={setRange} />
      <SearchableSelect ariaLabel="模型" className="combobox-inline" value={state.model} options={[{ value: '', label: '所有模型' }, ...options.models.map((name) => ({ value: name, label: name }))]} onChange={(model) => set({ model })} />
      {!hideChannels && <SearchableSelect ariaLabel="渠道" className="combobox-inline" value={state.channelName} options={[{ value: '', label: '所有渠道' }, ...options.channels.map((name) => ({ value: name, label: name }))]} onChange={(channelName) => set({ channelName })} />}
      <SearchableSelect ariaLabel="请求协议" className="combobox-inline" value={state.clientProtocol} options={PROTOCOLS} onChange={(clientProtocol) => set({ clientProtocol })} />
      {tokenOptions && <SearchableSelect ariaLabel="令牌" className="combobox-inline" value={state.authToken} options={[{ value: '', label: '全部令牌' }, ...tokenOptions]} onChange={(authToken) => set({ authToken })} />}
      <button className="btn" onClick={() => setState({ range: 'today', customStartTime: '', customEndTime: '', trendType: state.trendType, clientProtocol: '', model: '', authToken: '', channelName: '' })}>清空</button>
    </div>
    <div className="toolbar">
      <div className="mode-tabs" role="tablist">{TYPES.map((item) => <button key={item.value} role="tab" aria-selected={state.trendType === item.value} className={`mode-tab${state.trendType === item.value ? ' active' : ''}`} onClick={() => set({ trendType: item.value })}>{item.label}</button>)}</div>
      <button className={`btn${chart === 'line' ? ' btn-primary' : ''}`} onClick={() => setChart('line')}>折线</button>
      <button className={`btn${chart === 'bar' ? ' btn-primary' : ''}`} onClick={() => setChart('bar')}>柱状</button>
      {!hideChannels && channelNames.length > 0 && <SearchableMultiSelect ariaLabel="叠加渠道" className="combobox-inline" values={visibleChannels} options={channelNames.map((name) => ({ value: name, label: name === 'Unknown Channel' ? `⚠️ ${name}` : name }))} onChange={setVisibleChannels} placeholder="叠加渠道" />}
    </div>
    {error && <div className="card error-text" role="alert">{error}</div>}
    <section className="card">
      <div className="job-summary" style={{ marginTop: 0 }}>
        <span className="muted">间隔 {bucket >= 60 ? `${bucket / 60} 小时` : `${bucket} 分钟`} · 点数 {points.length} · 请求 {formatInt(requests.reduce((acc, value) => acc + value, 0))}{debugTotal ? ` · 日志总数 ${debugTotal}` : ''}</span>
      </div>
      {!points.length ? <p className="muted">{loading ? '加载中…' : '该时间范围内暂无数据'}</p>
        : <TrendChart points={points} series={series} requests={requests} chart={chart} format={format} bucket={bucket} latencyType={state.trendType === 'first_byte' || state.trendType === 'duration'} />}
    </section>
  </>
}

const W = 1000; const H = 340; const PAD = { left: 64, right: 16, top: 16, bottom: 44 }

/** 自绘 SVG 趋势图：坐标轴、时间标签、悬停提示、空值断线、无请求区间、P50/P90/MAX 参考线。 */
function TrendChart({ points, series, requests, chart, format, bucket, latencyType }: { points: Point[]; series: Series[]; requests: number[]; chart: 'line' | 'bar'; format: (value: number) => string; bucket: number; latencyType: boolean }) {
  const [hover, setHover] = useState<number | null>(null)
  const svgRef = useRef<SVGSVGElement>(null)
  const count = points.length
  const values = series.flatMap((item) => item.values).filter((value): value is number => value != null)
  const max = Math.max(...values, 0)
  // 延迟类留出 10% 余量，其余从 0 起。
  const top = max <= 0 ? 1 : max * (latencyType ? 1.1 : 1.05)
  const plotW = W - PAD.left - PAD.right; const plotH = H - PAD.top - PAD.bottom
  const x = (index: number) => PAD.left + (count <= 1 ? plotW / 2 : (index / (count - 1)) * plotW)
  const y = (value: number) => PAD.top + plotH - (value / top) * plotH
  const times = points.map((point) => Date.parse(point.ts))
  const spanHours = count > 1 ? (times[count - 1] - times[0]) / 3_600_000 : 0
  const label = (index: number) => { const d = new Date(times[index]); const pad = (n: number) => String(n).padStart(2, '0'); return spanHours <= 24 ? `${pad(d.getHours())}:${pad(d.getMinutes())}` : `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:00` }
  const step = Math.max(1, Math.ceil(count / 10))

  // 连续 ≥3 个桶无请求时标注灰色区间。
  const gaps: Array<[number, number]> = []
  for (let index = 0; index < count;) {
    if (requests[index] > 0) { index++; continue }
    let end = index
    while (end + 1 < count && requests[end + 1] === 0) end++
    if (end - index + 1 >= 3) gaps.push([index, end])
    index = end + 1
  }
  // 延迟类参考线基于总计线。
  const reference = latencyType ? (() => {
    const sorted = (series.find((item) => item.total)?.values ?? []).filter((value): value is number => value != null).sort((a, b) => a - b)
    if (!sorted.length) return null
    const quantile = (q: number) => sorted[Math.min(sorted.length - 1, Math.floor(q * (sorted.length - 1)))]
    return { p50: quantile(0.5), p90: quantile(0.9), max: sorted[sorted.length - 1] }
  })() : null

  const path = (vals: Array<number | null>) => vals.reduce((acc, value, index) => value == null ? acc : `${acc}${index === 0 || vals[index - 1] == null ? 'M' : 'L'}${x(index).toFixed(1)},${y(value).toFixed(1)} `, '')
  const onMove = (event: MouseEvent<SVGSVGElement>) => {
    const rect = svgRef.current?.getBoundingClientRect()
    if (!rect || count === 0) return
    const ratio = ((event.clientX - rect.left) / rect.width * W - PAD.left) / plotW
    setHover(Math.max(0, Math.min(count - 1, Math.round(ratio * (count - 1)))))
  }
  const barGroup = plotW / Math.max(count, 1)
  const barWidth = Math.max(1, (barGroup * 0.8) / Math.max(series.length, 1))

  return <div className="trend-chart">
    <svg ref={svgRef} viewBox={`0 0 ${W} ${H}`} className="trend-chart-svg" onMouseMove={onMove} onMouseLeave={() => setHover(null)} role="img" aria-label="趋势图">
      {gaps.map(([from, to]) => <rect key={from} x={x(from) - 2} y={PAD.top} width={Math.max(4, x(to) - x(from) + 4)} height={plotH} className="trend-gap"><title>无请求</title></rect>)}
      {[0, 0.25, 0.5, 0.75, 1].map((ratio) => <g key={ratio}>
        <line x1={PAD.left} x2={W - PAD.right} y1={y(top * ratio)} y2={y(top * ratio)} className="trend-grid" />
        <text x={PAD.left - 6} y={y(top * ratio) + 4} textAnchor="end" className="trend-axis">{format(top * ratio)}</text>
      </g>)}
      {points.map((_, index) => index % step === 0 && <text key={index} x={x(index)} y={H - PAD.bottom + 18} textAnchor="middle" className="trend-axis">{label(index)}</text>)}
      {reference && <>
        <line x1={PAD.left} x2={W - PAD.right} y1={y(reference.p50)} y2={y(reference.p50)} className="trend-ref" /><text x={W - PAD.right} y={y(reference.p50) - 4} textAnchor="end" className="trend-axis">P50 {format(reference.p50)}</text>
        <line x1={PAD.left} x2={W - PAD.right} y1={y(reference.p90)} y2={y(reference.p90)} className="trend-ref trend-ref-strong" /><text x={W - PAD.right} y={y(reference.p90) - 4} textAnchor="end" className="trend-axis">P90 {format(reference.p90)}</text>
      </>}
      {chart === 'line' ? series.map((item) => <path key={item.name} d={path(item.values)} fill="none" stroke={item.color} strokeWidth={item.total ? 2.2 : 1.5} strokeDasharray={item.dashed ? '5 4' : undefined} vectorEffect="non-scaling-stroke" />)
        : series.map((item, seriesIndex) => item.values.map((value, index) => value == null || value <= 0 ? null : <rect key={`${item.name}-${index}`} x={PAD.left + index * barGroup + barGroup * 0.1 + seriesIndex * barWidth} y={y(value)} width={barWidth} height={PAD.top + plotH - y(value)} fill={item.color} opacity={item.dashed ? 0.55 : 0.9} />))}
      {reference && (() => { const totalSeries = series.find((s) => s.total); const index = totalSeries?.values.findIndex((value) => value === reference.max) ?? -1; return index >= 0 ? <g><circle cx={x(index)} cy={y(reference.max)} r={4} fill="#ef4444" /><text x={x(index)} y={y(reference.max) - 8} textAnchor="middle" className="trend-axis">MAX {format(reference.max)}</text></g> : null })()}
      {hover != null && <line x1={x(hover)} x2={x(hover)} y1={PAD.top} y2={PAD.top + plotH} className="trend-cursor" />}
    </svg>
    {hover != null && <div className="trend-tooltip" style={{ left: `${Math.min(80, (x(hover) / W) * 100)}%` }}>
      <strong>{new Date(times[hover]).toLocaleString()}（{bucket} 分钟）</strong>
      <span className="muted">请求 {formatInt(requests[hover])}{requests[hover] === 0 ? '（无请求）' : ''}</span>
      {series.map((item) => <span key={item.name}><i className="trend-legend-swatch" style={{ background: item.color }} />{item.name}：{item.values[hover] == null ? '-' : format(item.values[hover] as number)}</span>)}
    </div>}
    <div className="job-summary">{series.map((item) => <span key={item.name}><i className="trend-legend-swatch" style={{ background: item.color, opacity: item.dashed ? 0.6 : 1 }} />{item.name}</span>)}</div>
  </div>
}
