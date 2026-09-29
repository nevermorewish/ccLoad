import { useEffect, useMemo, useState } from 'react'
import { getJSON } from '../lib/api'
import type { StatsEntry } from '../types'

type SortKey = 'channel_name' | 'model' | 'success' | 'error' | 'total' | 'rpm' | 'success_rate' | 'duration' | 'speed' | 'input' | 'output' | 'cost'

function Header({ onRefresh }: { onRefresh: () => void }) {
  return <header className="page-header"><div><h1>统计分析</h1><p className="muted">按渠道和模型查看调用、Token 与成本</p></div><button className="btn" onClick={onRefresh}>刷新</button></header>
}

export function StatsPage() {
  const [range, setRange] = useState('today')
  const [channel, setChannel] = useState('')
  const [model, setModel] = useState('')
  const [protocol, setProtocol] = useState('')
  const [token, setToken] = useState('')
  const [start, setStart] = useState('')
  const [end, setEnd] = useState('')
  const [sort, setSort] = useState<SortKey>('total')
  const [direction, setDirection] = useState<'asc' | 'desc' | 'none'>('none')
  const [rows, setRows] = useState<StatsEntry[]>([])
  const [rpm, setRpm] = useState<Record<string, number>>({})
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const refresh = async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await getJSON<{ stats?: StatsEntry[]; rpm_stats?: Record<string, number> }>('/dashboard/stats', {
        range,
        channel_name_like: channel,
        model_like: model,
        client_protocol: protocol,
        auth_token_id: token,
        start_time: range === 'custom' ? start : undefined,
        end_time: range === 'custom' ? end : undefined,
      })
      setRows(result.stats ?? [])
      setRpm(result.rpm_stats ?? {})
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '加载统计失败')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { void refresh() }, [range])

  const filtered = useMemo(() => {
    const result = rows.filter((row) => {
      const channelName = String(row.channel_name ?? '').toLowerCase()
      const modelName = String(row.model ?? '').toLowerCase()
      return (!channel || channelName.includes(channel.toLowerCase())) && (!model || modelName.includes(model.toLowerCase()))
    })
    const value = (row: StatsEntry): string | number => {
      if (sort === 'channel_name') return String(row.channel_name ?? '')
      if (sort === 'model') return String(row.model ?? '')
      if (sort === 'success_rate') return Number(row.total ?? 0) ? Number(row.success ?? 0) / Number(row.total) : 0
      if (sort === 'cost') return Number(row.effective_cost ?? row.total_cost ?? 0)
      if (sort === 'rpm') return Number(row.peak_rpm ?? 0)
      if (sort === 'duration') return Number(row.avg_duration_seconds ?? 0)
      if (sort === 'speed') return Number(row.avg_speed ?? 0)
      if (sort === 'input') return Number(row.total_input_tokens ?? 0)
      if (sort === 'output') return Number(row.total_output_tokens ?? 0)
      return Number(row[sort] ?? 0)
    }
    if (direction === 'none') return result
    return result.sort((a, b) => {
      const left = value(a)
      const right = value(b)
      const comparison = typeof left === 'string' && typeof right === 'string' ? left.localeCompare(right) : Number(left) - Number(right)
      return direction === 'asc' ? comparison : -comparison
    })
  }, [rows, channel, model, sort, direction])

  const cycleSort = (key: SortKey) => {
    if (sort !== key) { setSort(key); setDirection('desc'); return }
    setDirection((current) => current === 'none' ? 'desc' : current === 'desc' ? 'asc' : 'none')
  }
  const heading = (key: SortKey, label: string) => <th className="sortable" onClick={() => cycleSort(key)}>{label}{sort === key && direction !== 'none' ? direction === 'desc' ? ' ↓' : ' ↑' : ''}</th>

  return <><Header onRefresh={() => void refresh()} />
    <div className="toolbar">
      <select className="select" value={range} onChange={(event) => setRange(event.target.value)}><option value="today">今天</option><option value="yesterday">昨天</option><option value="this_week">本周</option><option value="this_month">本月</option><option value="all">全部</option><option value="custom">自定义</option></select>
      <input className="input" value={channel} onChange={(event) => setChannel(event.target.value)} placeholder="渠道筛选" />
      <input className="input" value={model} onChange={(event) => setModel(event.target.value)} placeholder="模型筛选" />
      <select className="select" value={protocol} onChange={(event) => setProtocol(event.target.value)}><option value="">全部入口协议</option><option value="openai">OpenAI</option><option value="anthropic">Anthropic</option><option value="gemini">Gemini</option><option value="codex">Codex</option></select>
      <input className="input" value={token} onChange={(event) => setToken(event.target.value)} placeholder="Token ID" />
      {range === 'custom' && <><input className="input" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /><input className="input" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></>}
      <button className="btn btn-primary" onClick={() => void refresh()}>查询</button><span className="muted">峰值 RPM {Number(rpm.peak_rpm ?? 0).toFixed(1)}</span>
    </div>
    {error && <div className="card error-text">{error}</div>}
    <div className="table-wrap"><table><thead><tr>{heading('channel_name', '渠道')}{heading('model', '模型')}{heading('success', '成功')}{heading('error', '失败')}{heading('total', '总调用')}{heading('rpm', '峰值 RPM')}{heading('success_rate', '成功率')}{heading('duration', '平均耗时')}{heading('speed', '平均速度')}{heading('input', '输入 Token')}{heading('output', '输出 Token')}{heading('cost', '成本')}</tr></thead><tbody>{loading ? <tr><td colSpan={12}>加载中...</td></tr> : filtered.length === 0 ? <tr><td colSpan={12}>暂无数据</td></tr> : filtered.map((row, index) => { const total = Number(row.total ?? 0); return <tr key={`${row.channel_id}-${row.model}-${index}`}><td>{String(row.channel_name ?? '-')}</td><td>{String(row.model ?? '-')}</td><td>{Number(row.success ?? 0)}</td><td>{Number(row.error ?? 0)}</td><td>{total}</td><td>{Number(row.peak_rpm ?? 0).toFixed(1)}</td><td>{total ? `${(Number(row.success ?? 0) / total * 100).toFixed(1)}%` : '-'}</td><td>{Number(row.avg_duration_seconds ?? 0).toFixed(2)}</td><td>{Number(row.avg_speed ?? 0).toFixed(2)}</td><td>{Number(row.total_input_tokens ?? 0).toLocaleString()}</td><td>{Number(row.total_output_tokens ?? 0).toLocaleString()}</td><td>{Number(row.effective_cost ?? row.total_cost ?? 0).toFixed(4)}</td></tr> })}</tbody></table></div>
    <div className="card" style={{ marginTop: 16 }}><h2>渠道健康时间线</h2><div className="health-grid">{filtered.slice(0, 20).map((row, index) => { const timeline = Array.isArray(row.health_timeline) ? row.health_timeline as Array<{ rate?: number }> : []; return <div className="health-line" key={`${row.channel_id}-${row.model}-${index}`}><span>{row.channel_name} / {row.model}</span><div>{timeline.map((point, pointIndex) => { const rate = Number(point.rate ?? -1); return <i key={pointIndex} title={rate < 0 ? '无请求' : `${(rate * 100).toFixed(1)}%`} style={{ background: rate < 0 ? '#64748b' : rate >= 0.95 ? '#22c55e' : rate >= 0.7 ? '#eab308' : '#ef4444' }} /> })}</div></div> })}</div></div>
  </>
}
