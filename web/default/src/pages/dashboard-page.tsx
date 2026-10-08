import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { getJSON } from '../lib/api'
import { isAPITokenRole } from '../lib/auth'
import { cacheHitRate, formatCompact, formatCostPair, formatDateTime, formatInt, formatPercent } from '../lib/format'
import { DateRangeFilter, normalizeRange, rangeParams, type DateRangeValue } from '../components/date-range'
import { useAutoRefresh } from '../hooks/use-auto-refresh'

type Bucket = { total_requests?: number; success_requests?: number; error_requests?: number; total_input_tokens?: number; total_output_tokens?: number; total_cache_read_tokens?: number; total_cache_creation_tokens?: number; total_cost?: number; effective_cost?: number }
type Summary = { total_requests?: number; success_requests?: number; error_requests?: number; is_today?: boolean; by_client_protocol?: Record<string, Bucket>; by_auth_type?: Record<string, Bucket>; rpm_stats?: { peak_rpm?: number; avg_rpm?: number; recent_rpm?: number } }
type MetricPoint = { ts?: string | number; success?: number; error?: number }

const PROTOCOLS = [{ key: 'anthropic', label: 'Claude Code' }, { key: 'codex', label: 'Codex' }, { key: 'openai', label: 'OpenAI' }, { key: 'gemini', label: 'Gemini' }]
// 与旧版 index.js:AUTH_TYPE_CARD_CONFIG 的顺序一致。
const AUTH_TYPES = [['api_key', 'API Key'], ['codex_oauth', 'Codex OAuth'], ['anthropic_oauth', 'Anthropic OAuth'], ['antigravity_oauth', 'Antigravity'], ['xai_oauth', 'xAI'], ['codebuddy_oauth', 'CodeBuddy'], ['zai_oauth', 'Z.ai'], ['cursor_oauth', 'Cursor'], ['zed_oauth', 'Zed']] as const
const RANGE_KEY = 'dashboard.range'

// 服务健康分桶：与旧版 service-health.js 一致，最多 672 个点。
const BUCKET_CHOICES = [15, 30, 60, 120, 240, 480, 720, 1440]
function bucketMinutesFor(value: DateRangeValue): number {
  const hours = value.range === 'custom' && value.start && value.end ? (value.end - value.start) / 3_600_000
    : value.range === 'this_week' || value.range === 'last_week' ? 168
    : value.range === 'this_month' || value.range === 'last_month' ? 744 : 24
  const required = Math.max(15, Math.ceil(hours * 60 / 672))
  return BUCKET_CHOICES.find((item) => item >= required) ?? 1440
}

type HealthPoint = { ts: number; success: number; error: number; rate: number | null }
function buildHealth(points: MetricPoint[], bucketMinutes: number): HealthPoint[] {
  const bucketMs = bucketMinutes * 60_000
  const map = new Map<number, { success: number; error: number }>()
  for (const point of points) {
    const raw = typeof point.ts === 'number' ? (point.ts < 1e12 ? point.ts * 1000 : point.ts) : Date.parse(String(point.ts))
    if (!Number.isFinite(raw)) continue
    const bucket = Math.floor(raw / bucketMs) * bucketMs
    const current = map.get(bucket) ?? { success: 0, error: 0 }
    current.success += Math.max(0, Number(point.success ?? 0)); current.error += Math.max(0, Number(point.error ?? 0))
    map.set(bucket, current)
  }
  if (!map.size) return []
  const keys = [...map.keys()]; const first = Math.min(...keys); const last = Math.max(...keys)
  const result: HealthPoint[] = []
  for (let ts = first; ts <= last; ts += bucketMs) {
    const counts = map.get(ts) ?? { success: 0, error: 0 }
    const total = counts.success + counts.error
    result.push({ ts, ...counts, rate: total ? counts.success / total : null })
  }
  return result
}
const healthClass = (rate: number | null) => rate == null ? 'health-unknown' : rate >= 0.95 ? 'health-good' : rate >= 0.8 ? 'health-warn' : 'health-bad'

function BucketDetail({ bucket }: { bucket: Bucket }) {
  return <div className="cell-stack">
    <span className="muted">成功 {formatInt(bucket.success_requests)} · 失败 {formatInt(bucket.error_requests)} · 成功率 {formatPercent(Number(bucket.success_requests ?? 0), Number(bucket.total_requests ?? 0))}</span>
    <span className="table-note">输入 {formatCompact(bucket.total_input_tokens)} · 输出 {formatCompact(bucket.total_output_tokens)} · 缓读 {formatCompact(bucket.total_cache_read_tokens)} · 缓建 {formatCompact(bucket.total_cache_creation_tokens)} · 命中 {cacheHitRate(bucket.total_input_tokens, bucket.total_cache_read_tokens, bucket.total_cache_creation_tokens)}</span>
    <span className="table-note">成本 {formatCostPair(bucket.total_cost, bucket.effective_cost)}</span>
  </div>
}

export function DashboardPage() {
  const readOnly = useMemo(() => isAPITokenRole(), [])
  const [range, setRange] = useState<DateRangeValue>(() => { try { return normalizeRange(JSON.parse(localStorage.getItem(RANGE_KEY) ?? 'null') ?? undefined) } catch { return { range: 'today' } } })
  const [summary, setSummary] = useState<Summary>({})
  const [health, setHealth] = useState<{ points: HealthPoint[]; bucket: number; error?: string } | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const generation = useRef(0)

  const load = useCallback(async () => {
    const current = ++generation.current
    setLoading(true)
    try {
      const data = await getJSON<Summary>('/dashboard/summary', { ...rangeParams(range), include_rpm: 1 })
      if (current !== generation.current) return // 丢弃过期响应
      setSummary(data); setError(null)
    } catch (cause) { if (current === generation.current) setError(cause instanceof Error ? cause.message : '概览加载失败') }
    finally { if (current === generation.current) setLoading(false) }
    // 健康时间线在摘要之后后台加载，失败只影响该区块。
    const bucket = bucketMinutesFor(range)
    try {
      const points = await getJSON<MetricPoint[]>('/dashboard/metrics', { ...rangeParams(range), bucket_min: bucket })
      if (current === generation.current) setHealth({ points: buildHealth(Array.isArray(points) ? points : [], bucket), bucket })
    } catch (cause) { if (current === generation.current) setHealth({ points: [], bucket, error: cause instanceof Error ? cause.message : '健康数据加载失败' }) }
  }, [range])

  useEffect(() => { localStorage.setItem(RANGE_KEY, JSON.stringify(range)); void load() }, [range, load])
  useAutoRefresh(() => void load())

  const authTypes = AUTH_TYPES.filter(([key]) => Number(summary.by_auth_type?.[key]?.total_requests ?? 0) > 0)
  const healthTotals = (health?.points ?? []).reduce((acc, point) => ({ success: acc.success + point.success, error: acc.error + point.error }), { success: 0, error: 0 })
  const total = Number(summary.total_requests ?? 0)

  return <>
    <header className="page-header">
      <div><h1>概览</h1><p className="muted">请求量、成功率、成本和服务健康</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}><DateRangeFilter value={range} onChange={setRange} /><button className="btn" disabled={loading} onClick={() => void load()}>刷新</button></div>
    </header>
    {error && <div className="card error-text" role="alert">{error}</div>}

    <section className="grid grid-4">
      <div className="card"><div className="muted">总请求</div><div className="metric">{formatInt(total)}</div></div>
      <div className="card"><div className="muted">成功</div><div className="metric success-text">{formatInt(summary.success_requests)}</div></div>
      <div className="card"><div className="muted">失败</div><div className="metric error-text">{formatInt(summary.error_requests)}</div></div>
      <div className="card"><div className="muted">成功率</div><div className="metric">{formatPercent(Number(summary.success_requests ?? 0), total)}</div>
        {summary.rpm_stats && <div className="table-note">RPM 峰 {Number(summary.rpm_stats.peak_rpm ?? 0).toFixed(1)} · 均 {Number(summary.rpm_stats.avg_rpm ?? 0).toFixed(1)}{summary.is_today ? ` · 近 ${Number(summary.rpm_stats.recent_rpm ?? 0).toFixed(1)}` : ''}</div>}</div>
    </section>

    <section className="card" style={{ marginTop: 16 }}>
      <div className="job-summary" style={{ marginTop: 0 }}><strong>服务健康</strong>{health && !health.error && <span className="muted">每格 {health.bucket >= 60 ? `${health.bucket / 60} 小时` : `${health.bucket} 分钟`} · 成功 {formatInt(healthTotals.success)} · 失败 {formatInt(healthTotals.error)} · 成功率 {formatPercent(healthTotals.success, healthTotals.success + healthTotals.error)}</span>}</div>
      {!health ? <p className="muted">加载中…</p> : health.error ? <p className="error-text">{health.error}</p> : !health.points.length ? <p className="muted">该时间范围内暂无请求</p> : <>
        <div className="health-strip" role="img" aria-label="服务健康时间线">{health.points.map((point) => (
          <i key={point.ts} className={healthClass(point.rate)} title={`${formatDateTime(point.ts)} 起\n成功 ${point.success} · 失败 ${point.error}${point.rate == null ? '\n无请求' : `\n成功率 ${(point.rate * 100).toFixed(1)}%`}`} />
        ))}</div>
        <div className="job-summary"><span className="muted">{formatDateTime(health.points[0].ts)}</span><span className="muted" style={{ marginLeft: 'auto' }}>{formatDateTime(health.points[health.points.length - 1].ts)}</span></div>
        <div className="job-summary"><span><i className="health-legend health-good" /> ≥95%</span><span><i className="health-legend health-warn" /> 80–95%</span><span><i className="health-legend health-bad" /> &lt;80%</span><span><i className="health-legend health-unknown" /> 无请求</span></div>
      </>}
    </section>

    <h2 style={{ marginTop: 24 }}>按客户端协议</h2>
    <section className="grid grid-4">{PROTOCOLS.map(({ key, label }) => {
      const bucket = summary.by_client_protocol?.[key] ?? {}
      return <div className="card" key={key}><div className="muted">{label}</div><div className="metric">{formatInt(bucket.total_requests)}</div><BucketDetail bucket={bucket} /></div>
    })}</section>

    {authTypes.length > 0 && <>
      <h2 style={{ marginTop: 24 }}>按认证类型</h2>
      <section className="grid grid-4">{authTypes.map(([key, label]) => {
        const bucket = summary.by_auth_type?.[key] ?? {}
        return <div className="card" key={key}><div className="muted">{label}</div><div className="metric">{formatInt(bucket.total_requests)}</div><BucketDetail bucket={bucket} /></div>
      })}</section>
    </>}

    <section className="card" style={{ marginTop: 16 }}><h2>快速入口</h2><div className="toolbar">
      {!readOnly && <Link className="btn btn-primary" to="/channels">渠道管理</Link>}
      {!readOnly && <Link className="btn" to="/monitor">渠道监控</Link>}
      <Link className="btn" to="/stats">统计分析</Link><Link className="btn" to="/trend">趋势</Link><Link className="btn" to="/logs">请求日志</Link>
    </div></section>
  </>
}
