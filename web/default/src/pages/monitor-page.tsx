import { useEffect, useMemo, useState } from 'react'
import { Activity, CheckCircle2, Clock3, Gauge, RefreshCw, RotateCw, XCircle } from 'lucide-react'
import type { ReactNode } from 'react'
import { getJSON, postJSON, putJSON } from '../lib/api'
import { toMillis } from '../lib/format'

interface Probe { time: number | string; status_code: number; duration: number; message?: string }
interface Item {
  id: number; name: string; enabled?: boolean; status?: string; running?: boolean
  /** 是否参与统一监控（间隔全局统一，模型由后端自动选择） */
  monitored?: boolean; effective_model?: string; next_check?: string | null
  stats?: { samples?: number; successes?: number; average_latency?: number; last_error?: string; recent?: Probe[] }
}
interface MonitorData { items?: Item[]; server_time?: string; timezone?: string; interval_minutes?: number; interval_min?: number; interval_max?: number }

const statusText: Record<string, string> = { online: '稳定', degraded: '降级', offline: '离线', disabled: '已停用', unknown: '待检测' }
const statusTone = (status?: string) => status === 'online' ? 'monitor-good' : status === 'offline' ? 'monitor-bad' : status === 'disabled' ? 'monitor-muted' : 'monitor-warn'
// recent[].time 为 Unix 秒（model.JSONTime），server_time/next_check 为 RFC3339，统一转毫秒再格式化。
const formatTime = (value?: string | number | null) => { const ms = toMillis(value); return ms == null ? '尚未检测' : new Date(ms).toLocaleString() }
const formatLatency = (seconds?: number) => { const value = Number(seconds); return !Number.isFinite(value) || value <= 0 ? '—' : `${value.toFixed(2)}秒` }
const latencyTone = (seconds?: number) => { const value = Number(seconds); return !Number.isFinite(value) || value <= 0 ? 'monitor-muted' : value < 1 ? 'monitor-good' : value < 3 ? 'monitor-warn' : 'monitor-bad' }
const formatCountdown = (ms: number) => { const seconds = Math.max(0, Math.floor(ms / 1000)); const minutes = Math.floor(seconds / 60); const hours = Math.floor(minutes / 60); return hours ? `${hours}小时${minutes % 60}分` : minutes ? `${minutes}分${seconds % 60}秒` : `${seconds}秒` }

export function MonitorPage() {
  const [items, setItems] = useState<Item[]>([])
  const [interval, setIntervalMinutes] = useState(300)
  const [bounds, setBounds] = useState({ min: 1, max: 600 })
  const [intervalDraft, setIntervalDraft] = useState('300')
  const [serverTime, setServerTime] = useState('')
  const [timezone, setTimezone] = useState('')
  const [busy, setBusy] = useState<number | null>(null)
  const [savingInterval, setSavingInterval] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState('')
  const [clock, setClock] = useState(() => Date.now())

  const load = async () => {
    try {
      const value = await getJSON<MonitorData>('/admin/channel-monitor')
      setItems(value.items ?? [])
      setServerTime(value.server_time ?? '')
      setTimezone(value.timezone ?? '')
      if (value.interval_minutes != null) { setIntervalMinutes(value.interval_minutes); setIntervalDraft(String(value.interval_minutes)) }
      setBounds({ min: Number(value.interval_min ?? 1), max: Number(value.interval_max ?? 600) })
      setError(null)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '加载监控失败') }
  }
  useEffect(() => {
    void load()
    const refresh = () => { if (document.visibilityState === 'visible') void load() }
    const timer = window.setInterval(refresh, 10_000)
    window.addEventListener('focus', refresh)
    document.addEventListener('visibilitychange', refresh)
    return () => { window.clearInterval(timer); window.removeEventListener('focus', refresh); document.removeEventListener('visibilitychange', refresh) }
  }, [])
  useEffect(() => { const timer = window.setInterval(() => setClock(Date.now()), 1000); return () => window.clearInterval(timer) }, [])

  // 检测间隔是全局设置，改一次对所有渠道生效。
  const saveInterval = async () => {
    const value = Math.trunc(Number(intervalDraft))
    if (!Number.isFinite(value) || value < bounds.min || value > bounds.max) { setError(`检测间隔需在 ${bounds.min}–${bounds.max} 分钟之间`); return }
    setSavingInterval(true); setError(''); setNotice('')
    try {
      await postJSON('/admin/settings/batch', { channel_monitor_interval_minutes: String(value) })
      // 该项不是热更新设置，后端会重启进程；提示后轮询等待服务恢复。
      setNotice('间隔已保存，服务正在重启，稍后自动刷新')
      setIntervalMinutes(value)
      window.setTimeout(() => void load(), 4000)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '保存间隔失败') } finally { setSavingInterval(false) }
  }
  const toggleMonitored = async (item: Item, monitored: boolean) => {
    setBusy(item.id); setError(''); setNotice('')
    try { await putJSON(`/admin/channels/${item.id}/monitor-participation`, { enabled: monitored }); await load() }
    catch (cause) { setError(cause instanceof Error ? cause.message : '保存监控开关失败') } finally { setBusy(null) }
  }
  const run = async (item: Item) => {
    setBusy(item.id); setError(''); setNotice('')
    try { await postJSON(`/admin/channels/${item.id}/monitor-run`); await load() }
    catch (cause) { setError(cause instanceof Error ? cause.message : '检测失败') } finally { setBusy(null) }
  }

  const summary = useMemo(() => {
    const enabled = items.filter((item) => item.enabled !== false)
    const samples = enabled.reduce((sum, item) => sum + Number(item.stats?.samples ?? 0), 0)
    const successes = enabled.reduce((sum, item) => sum + Number(item.stats?.successes ?? 0), 0)
    const latencies = enabled.map((item) => Number(item.stats?.average_latency ?? 0)).filter((value) => value > 0)
    return {
      total: items.length,
      monitored: items.filter((item) => item.monitored).length,
      online: enabled.filter((item) => item.status === 'online').length,
      degraded: enabled.filter((item) => item.status === 'degraded').length,
      offline: enabled.filter((item) => item.status === 'offline').length,
      running: items.filter((item) => item.running).length,
      samples,
      rate: samples ? successors(successes, samples) : 0,
      latency: latencies.length ? latencies.reduce((a, b) => a + b, 0) / latencies.length : 0,
    }
  }, [items])

  const intervalSaved = Number(intervalDraft) === interval

  return <>
    <header className="page-header">
      <div><h1>渠道监控</h1><p className="muted">统一的自动检测间隔，逐渠道查看稳定状态与响应延迟</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <span className="muted">{serverTime ? `服务端 ${formatTime(serverTime)} · ${timezone}` : ''}</span>
        <button className="btn" onClick={() => void load()}><RefreshCw size={15} /> 刷新</button>
      </div>
    </header>
    {error && <div className="card error-text" role="alert">{error}</div>}
    {notice && <div className="card" role="status"><div className="job-summary" style={{ margin: 0 }}><span>{notice}</span><button className="link-button" onClick={() => setNotice('')}>关闭</button></div></div>}

    <section className="card monitor-interval-bar">
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <strong>检测间隔</strong>
        <label className="muted">每<input className="input compact" type="number" min={bounds.min} max={bounds.max} step={1} value={intervalDraft} disabled={savingInterval} aria-label="检测间隔分钟" onChange={(event) => setIntervalDraft(event.target.value)} />分钟检测一次</label>
        <button className="btn btn-primary" disabled={savingInterval || intervalSaved} onClick={() => void saveInterval()}>{savingInterval ? '保存中…' : '保存'}</button>
        {!intervalSaved && <span className="muted">未保存</span>}
        <span className="muted">全部渠道共用该间隔（{bounds.min}–{bounds.max} 分钟），检测模型由后端自动选择；保存后服务会重启。</span>
      </div>
    </section>

    <section className="monitor-summary-grid">
      <SummaryCard icon={<Activity size={18} />} label="监测渠道" value={`${summary.monitored} / ${summary.total}`} detail={`${summary.online} 稳定 · ${summary.degraded} 降级 · ${summary.offline} 离线`} />
      <SummaryCard icon={<CheckCircle2 size={18} />} label="24 小时成功率" value={summary.samples ? `${summary.rate.toFixed(1)}%` : '-'} detail={`${summary.samples} 次有效探测`} tone={summary.rate >= 95 ? 'good' : summary.rate >= 80 ? 'warn' : 'bad'} />
      <SummaryCard icon={<Gauge size={18} />} label="平均响应延迟" value={summary.latency ? formatLatency(summary.latency) : '—'} detail="最近 24 小时成功探测" tone={summary.latency > 0 && summary.latency < 1 ? 'good' : summary.latency < 3 ? 'warn' : 'bad'} />
      <SummaryCard icon={<RotateCw size={18} />} label="正在检测" value={String(summary.running)} detail={summary.running ? '有渠道正在检测' : '当前空闲'} tone={summary.running ? 'warn' : 'good'} />
    </section>

    <section className="card">
      <div className="table-wrap">
        <table className="monitor-table">
          <thead><tr>
            <th>渠道</th><th>状态</th><th>平均延迟</th><th>24 小时成功率</th><th>探测</th><th>最近检测</th><th>下次检测</th><th>监控</th><th>操作</th>
          </tr></thead>
          <tbody>
            {!items.length && <tr><td colSpan={9} className="muted">暂无渠道，请先在渠道页面添加并启用渠道。</td></tr>}
            {items.map((item) => {
              const samples = Number(item.stats?.samples ?? 0)
              const successes = Number(item.stats?.successes ?? 0)
              const rate = samples ? successors(successes, samples) : null
              const probes = (item.stats?.recent ?? []).filter((probe) => probe.status_code !== 0 || probe.duration > 0)
              const latest = probes[0]
              const nextMs = item.next_check ? new Date(item.next_check).getTime() - clock : 0
              const tone = statusTone(item.status)
              return <tr key={item.id}>
                <td className="cell-stack">
                  <span className="monitor-title"><span className={`monitor-status-dot ${tone}`} /><strong>{item.name}</strong></span>
                  <span className="table-note">#{item.id} · 检测模型 {item.effective_model || '自动'} · {item.enabled === false ? '渠道已停用' : `自动监测 ${item.monitored ? '已开启' : '已关闭'}`}</span>
                </td>
                <td><span className={`monitor-status ${tone}`}>{item.running ? '检测中' : statusText[item.status ?? 'unknown']}</span>
                  {(item.status === 'offline' || item.status === 'degraded') && item.stats?.last_error && <span className="table-note monitor-error" title={item.stats.last_error}><XCircle size={13} /> {item.stats.last_error}</span>}
                </td>
                <td><strong className={latencyTone(item.stats?.average_latency)}>{formatLatency(item.stats?.average_latency)}</strong></td>
                <td>{rate == null ? '—' : <><strong className={rate >= 95 ? 'monitor-good' : rate >= 80 ? 'monitor-warn' : 'monitor-bad'}>{rate.toFixed(1)}%</strong><span className="table-note"> {successes}/{samples}</span></>}</td>
                <td><div className="monitor-sparkline" aria-label="最近探测稳定状态">{probes.length ? probes.slice(0, 12).reverse().map((probe, index) => <i key={`${probe.time}-${index}`} className={probe.status_code >= 200 && probe.status_code < 300 ? 'probe-good' : 'probe-bad'} title={`${formatTime(probe.time)} · ${probe.status_code || '失败'} · ${formatLatency(probe.duration)}`} />) : <span className="muted">无记录</span>}</div></td>
                <td className="cell-stack"><span>{latest ? formatTime(latest.time) : '尚未检测'}</span>{latest && <span className="table-note">{formatLatency(latest.duration)}{latest.message ? ` · ${latest.message}` : ''}</span>}</td>
                <td>{item.next_check ? <span className="cell-stack"><span>{nextMs > 0 ? `${formatCountdown(nextMs)}后` : '即将检测'}</span><span className="table-note">{formatTime(item.next_check)}</span></span> : <span className="muted">未参与</span>}</td>
                <td><label className="switch"><input type="checkbox" aria-label={`${item.name} 参与监控`} checked={Boolean(item.monitored)} disabled={busy === item.id || item.enabled === false} onChange={(event) => void toggleMonitored(item, event.target.checked)} /> {item.monitored ? '开启' : '关闭'}</label></td>
                <td><button className="btn" disabled={busy === item.id || item.running || item.enabled === false} onClick={() => void run(item)}>{item.running ? '检测中…' : '立即检测'}</button></td>
              </tr>
            })}
          </tbody>
        </table>
      </div>
      {summary.total > 0 && <div className="job-summary" style={{ marginBottom: 0 }}><Clock3 size={14} /> 服务端时间 {serverTime ? formatTime(serverTime) : '—'} · 时区 {timezone || '—'}</div>}
    </section>
  </>
}

function successors(successes: number, samples: number) { return samples ? (successes / samples) * 100 : 0 }

function SummaryCard({ icon, label, value, detail, tone }: { icon: ReactNode; label: string; value: string; detail: string; tone?: string }) {
  return <div className={`card monitor-summary-card ${tone ? `monitor-${tone}` : ''}`}>
    <div className="monitor-summary-label">{icon}<span>{label}</span></div>
    <div className="monitor-summary-value">{value}</div>
    <div className="muted">{detail}</div>
  </div>
}
