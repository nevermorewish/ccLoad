import { useEffect, useState } from 'react'
import { api, getJSON, getPaginated, postJSON } from '../lib/api'
import type { LogEntry } from '../types'

type DebugLog = Record<string, unknown>
const text = (value: unknown) => String(value ?? '')

export function LogsPage() {
  const initialQuery = new URLSearchParams(window.location.search)
  const [range, setRange] = useState('today')
  const [start, setStart] = useState('')
  const [end, setEnd] = useState('')
  const [channel, setChannel] = useState(initialQuery.get('channel_name') ?? '')
  const [model, setModel] = useState(initialQuery.get('model') ?? '')
  const [status, setStatus] = useState('')
  const [protocol, setProtocol] = useState('')
  const [token, setToken] = useState('')
  const [source, setSource] = useState('proxy')
  const [page, setPage] = useState(1)
  const [rows, setRows] = useState<{ data: LogEntry[]; count: number }>({ data: [], count: 0 })
  const [active, setActive] = useState<Array<Record<string, unknown>>>([])
  const [selected, setSelected] = useState<LogEntry | null>(null)
  const [debug, setDebug] = useState<DebugLog | null>(null)
  const [debugTab, setDebugTab] = useState<'request' | 'translated_request' | 'response' | 'translated_response' | 'merged'>('request')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = async () => {
    setLoading(true); setError(null)
    try { setRows(await getPaginated<LogEntry>('/dashboard/logs', { range, start_time: range === 'custom' ? start : undefined, end_time: range === 'custom' ? end : undefined, limit: 50, offset: (page - 1) * 50, channel_name_like: channel, model_like: model, status_code: status, client_protocol: protocol, auth_token_id: token, log_source: source })) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '加载日志失败') }
    finally { setLoading(false) }
  }
  useEffect(() => { void load() }, [range, page])
  useEffect(() => { let cancelled = false; const refresh = async () => { try { const result = await getJSON<Array<Record<string, unknown>>>('/admin/active-requests'); if (!cancelled) setActive(result) } catch { if (!cancelled) setActive([]) } }; void refresh(); const timer = window.setInterval(refresh, 5000); return () => { cancelled = true; window.clearInterval(timer) } }, [])
  const showDebug = async (id: unknown, activeRequest = false) => { try { setDebug(await getJSON<DebugLog>(activeRequest ? `/admin/active-requests/${id}/debug-log` : `/admin/debug-logs/${id}`)); setDebugTab('request') } catch (cause) { setError(cause instanceof Error ? cause.message : '调试日志加载失败') } }
  const abort = async (id: unknown) => { await postJSON(`/admin/active-requests/${encodeURIComponent(text(id))}/abort`); setActive((items) => items.filter((item) => text(item.id) !== text(id))) }
  const merge = async () => { if (!debug) return; const body = text(debug.resp_body ?? debug.response_body ?? ''); const result = await postJSON('/admin/debug-logs/merged-response', { resp_body: body }); setDebug({ ...debug, merged_response: result }); setDebugTab('merged') }
  const copy = async () => { if (!debug) return; await navigator.clipboard?.writeText(text(debug[debugTab === 'merged' ? 'merged_response' : debugTab === 'request' ? 'req_body' : debugTab === 'response' ? 'resp_body' : debugTab])); }
  const columns = ['时间', '渠道', '模型', '状态', '耗时', 'Token', '成本', '信息']
  return <><header className="page-header"><div><h1>请求日志</h1><p className="muted">共 {rows.count} 条记录，支持请求与响应调试</p></div><button className="btn" onClick={() => void load()}>刷新</button></header>
    <div className="toolbar"><select className="select" value={range} onChange={(event) => { setRange(event.target.value); setPage(1) }}><option value="today">今天</option><option value="yesterday">昨天</option><option value="this_week">本周</option><option value="this_month">本月</option><option value="all">全部</option><option value="custom">自定义</option></select>{range === 'custom' && <><input className="input" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /><input className="input" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></>}<input className="input" value={channel} onChange={(event) => setChannel(event.target.value)} placeholder="渠道" /><input className="input" value={model} onChange={(event) => setModel(event.target.value)} placeholder="模型" /><input className="input" value={status} onChange={(event) => setStatus(event.target.value)} placeholder="状态码" /><select className="select" value={protocol} onChange={(event) => setProtocol(event.target.value)}><option value="">全部协议</option><option value="openai">OpenAI</option><option value="anthropic">Anthropic</option><option value="gemini">Gemini</option><option value="codex">Codex</option></select><input className="input" value={token} onChange={(event) => setToken(event.target.value)} placeholder="Token ID" /><select className="select" value={source} onChange={(event) => setSource(event.target.value)}><option value="proxy">代理请求</option><option value="jev">JEV</option><option value="scheduled_check">定时检测</option><option value="manual_test">手动测试</option><option value="manual_chat">手动聊天</option></select><button className="btn btn-primary" onClick={() => void load()}>查询</button><button className="btn" disabled={page <= 1} onClick={() => setPage((current) => Math.max(1, current - 1))}>上一页</button><span className="muted">第 {page} 页</span><button className="btn" disabled={page * 50 >= rows.count} onClick={() => setPage((current) => current + 1)}>下一页</button></div>
    {error && <div className="card error-text">{error}</div>}
    {active.length > 0 && <div className="card"><h2>进行中的请求</h2><div className="table-wrap"><table><thead><tr><th>ID</th><th>渠道</th><th>模型</th><th>状态</th><th>操作</th></tr></thead><tbody>{active.map((item) => <tr key={text(item.id)}><td>{text(item.id)}</td><td>{text(item.channel_name || '-')}</td><td>{text(item.model || '-')}</td><td>{text(item.status || '运行中')}</td><td><button className="btn" onClick={() => void showDebug(item.id, true)}>调试</button><button className="btn" onClick={() => void abort(item.id)}>中断</button></td></tr>)}</tbody></table></div></div>}
    <div className="table-wrap"><table><thead><tr>{columns.map((column) => <th key={column}>{column}</th>)}</tr></thead><tbody>{loading ? <tr><td colSpan={8}>加载中...</td></tr> : rows.data.map((row, index) => <tr key={text(row.id ?? index)} onClick={() => setSelected(row)}><td>{text(row.created_at ?? row.time ?? '-')}</td><td>{text(row.channel_name || '-')}</td><td>{text(row.model || '-')}</td><td>{text(row.status_code ?? row.status ?? '-')}</td><td>{text(row.duration ?? row.duration_ms ?? '-')}</td><td>{Number(row.input_tokens ?? 0) + Number(row.output_tokens ?? 0)}</td><td>{Number(row.effective_cost ?? row.cost ?? 0).toFixed(6)}</td><td>{text(row.error_message ?? row.message ?? '')}{row.id && <button className="btn" onClick={(event) => { event.stopPropagation(); void showDebug(row.id) }}>调试</button>}</td></tr>)}</tbody></table></div>
    {selected && <div className="card"><div className="toolbar"><h2>日志详情</h2><button className="btn" onClick={() => setSelected(null)}>关闭</button></div><pre className="result-pre">{JSON.stringify(selected, null, 2)}</pre></div>}
    {debug && <div className="card"><div className="toolbar"><h2>调试日志</h2><button className="btn" onClick={() => setDebug(null)}>关闭</button><button className="btn" onClick={() => void copy()}>复制当前内容</button><button className="btn" onClick={() => void merge()}>合并响应</button></div><div className="toolbar">{(['request', 'translated_request', 'response', 'translated_response', 'merged'] as const).map((tab) => <button className={debugTab === tab ? 'btn btn-primary' : 'btn'} key={tab} onClick={() => setDebugTab(tab)}>{tab === 'request' ? '请求' : tab === 'translated_request' ? '转换请求' : tab === 'response' ? '响应' : tab === 'translated_response' ? '转换响应' : '合并响应'}</button>)}</div><pre className="result-pre">{JSON.stringify(debug[debugTab === 'merged' ? 'merged_response' : debugTab === 'request' ? 'req_body' : debugTab === 'response' ? 'resp_body' : debugTab], null, 2)}</pre></div>}
  </>
}
