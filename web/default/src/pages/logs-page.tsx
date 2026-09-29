import { useEffect, useMemo, useState } from 'react'
import { getJSON, getPaginated, postJSON } from '../lib/api'
import type { LogEntry } from '../types'

type DebugLog = Record<string, unknown>
type LogChannel = { id: number; name: string }
type LogOptions = { models: string[]; channels: LogChannel[]; status_codes: number[]; auth_tokens: Array<{ id: number; description?: string; name?: string }> }
type LogColumn = { key: string; label: string; value: (row: LogEntry) => string }
const PAGE_SIZE = 15
const COLUMN_STORAGE_KEY = 'ccload_logs_columns'
const text = (value: unknown) => value == null || value === '' ? '-' : String(value)
const number = (value: unknown) => Number.isFinite(Number(value)) ? Number(value) : 0
const formatCost = (row: LogEntry) => number(row.effective_cost ?? row.cost).toFixed(6)
const columns: LogColumn[] = [
  { key: 'time', label: '时间', value: (row) => text(row.created_at ?? row.time) },
  { key: 'ip', label: 'IP', value: (row) => text(row.ip ?? row.client_ip) },
  { key: 'tokenDesc', label: '令牌', value: (row) => text(row.auth_token_description ?? row.token_description) },
  { key: 'apiKey', label: '渠道 Key', value: (row) => text(row.api_key_used) },
  { key: 'channel', label: '渠道', value: (row) => text(row.channel_name) },
  { key: 'model', label: '模型', value: (row) => text(row.actual_model ?? row.response_model ?? row.model) },
  { key: 'status', label: '状态码', value: (row) => text(row.status_code ?? row.status) },
  { key: 'timing', label: '首字 / 耗时', value: (row) => `${row.is_streaming ? `${number(row.first_byte_time).toFixed(2)}s / ` : ''}${row.duration == null ? '-' : `${number(row.duration).toFixed(2)}s`}` },
  { key: 'speed', label: '速度 tok/s', value: (row) => { const duration = number(row.duration) - (row.is_streaming ? number(row.first_byte_time) : 0); return duration > 0 ? (number(row.output_tokens) / duration).toFixed(1) : '-' } },
  { key: 'input', label: '输入 Token', value: (row) => number(row.input_tokens).toLocaleString() },
  { key: 'output', label: '输出 Token', value: (row) => number(row.output_tokens).toLocaleString() },
  { key: 'cacheRead', label: '缓存读', value: (row) => number(row.cache_read_input_tokens).toLocaleString() },
  { key: 'cacheWrite', label: '缓存建', value: (row) => number(row.cache_creation_input_tokens).toLocaleString() },
  { key: 'cacheUtil', label: '缓存命中%', value: (row) => { const input = number(row.input_tokens); const read = number(row.cache_read_input_tokens); return input > 0 ? `${((read / input) * 100).toFixed(1)}%` : '-' } },
  { key: 'cost', label: '成本', value: formatCost },
  { key: 'message', label: '信息', value: (row) => text(row.error_message ?? row.message) },
]

function readHiddenColumns(): string[] {
  try {
    const stored = JSON.parse(localStorage.getItem(COLUMN_STORAGE_KEY) ?? '{}') as Record<string, boolean>
    return columns.filter((column) => stored[column.key] === false).map((column) => column.key)
  } catch { return [] }
}

function dateRangeParams(range: string, start: string, end: string) {
  if (range !== 'custom') return { range }
  const startTime = Date.parse(start)
  const endTime = Date.parse(end)
  return { range, start_time: Number.isFinite(startTime) ? startTime : undefined, end_time: Number.isFinite(endTime) ? endTime : undefined }
}

export function LogsPage() {
  const initialQuery = useMemo(() => new URLSearchParams(window.location.search), [])
  const [range, setRange] = useState('today')
  const [start, setStart] = useState('')
  const [end, setEnd] = useState('')
  const [channel, setChannel] = useState(initialQuery.get('channel_name') ?? '')
  const [model, setModel] = useState(initialQuery.get('model') ?? '')
  const [status, setStatus] = useState('')
  const [protocol, setProtocol] = useState('')
  const [token, setToken] = useState('')
  const [source, setSource] = useState('proxy')
  const [exact, setExact] = useState(Boolean(initialQuery.get('channel_name') || initialQuery.get('model')))
  const [options, setOptions] = useState<LogOptions>({ models: [], channels: [], status_codes: [], auth_tokens: [] })
  const [page, setPage] = useState(1)
  const [jumpPage, setJumpPage] = useState('')
  const [rows, setRows] = useState<{ data: LogEntry[]; count: number }>({ data: [], count: 0 })
  const [active, setActive] = useState<Array<Record<string, unknown>>>([])
  const [selected, setSelected] = useState<LogEntry | null>(null)
  const [debug, setDebug] = useState<DebugLog | null>(null)
  const [debugTab, setDebugTab] = useState<'request' | 'translated_request' | 'response' | 'translated_response' | 'merged'>('request')
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [hiddenColumns, setHiddenColumns] = useState<string[]>(readHiddenColumns)
  const totalPages = Math.max(1, Math.ceil(rows.count / PAGE_SIZE))
  const visibleColumns = columns.filter((column) => !hiddenColumns.includes(column.key))

  const load = async (targetPage = page) => {
    setLoading(true); setError(null)
    try {
      const data = await getPaginated<LogEntry>('/dashboard/logs', {
        ...dateRangeParams(range, start, end), limit: PAGE_SIZE, offset: (targetPage - 1) * PAGE_SIZE,
        channel_name: exact && channel ? channel : undefined, channel_name_like: !exact && channel ? channel : undefined,
        model: exact && model ? model : undefined, model_like: !exact && model ? model : undefined,
        status_code: status || undefined, client_protocol: protocol || undefined, auth_token_id: token || undefined, log_source: source || 'all',
      })
      setRows(data)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '加载日志失败') }
    finally { setLoading(false) }
  }
  const loadBootstrap = async () => {
    try {
      const value = await getJSON<{ models?: string[]; channels?: LogChannel[]; status_codes?: number[]; auth_tokens?: LogOptions['auth_tokens'] }>('/dashboard/logs/bootstrap', dateRangeParams(range, start, end))
      setOptions({ models: value.models ?? [], channels: value.channels ?? [], status_codes: value.status_codes ?? [], auth_tokens: value.auth_tokens ?? [] })
    } catch { setOptions({ models: [], channels: [], status_codes: [], auth_tokens: [] }) }
  }
  useEffect(() => { void load(page); void loadBootstrap() }, [range, page])
  useEffect(() => { let cancelled = false; const refresh = async () => { try { const result = await getJSON<Array<Record<string, unknown>>>('/admin/active-requests'); if (!cancelled) setActive(result) } catch { if (!cancelled) setActive([]) } }; void refresh(); const timer = window.setInterval(refresh, 5000); return () => { cancelled = true; window.clearInterval(timer) } }, [])
  useEffect(() => {
    try {
      const visibility = Object.fromEntries(columns.map(({ key }) => [key, !hiddenColumns.includes(key)]))
      localStorage.setItem(COLUMN_STORAGE_KEY, JSON.stringify(visibility))
    } catch { /* Storage may be unavailable. */ }
  }, [hiddenColumns])

  const showDebug = async (id: unknown, activeRequest = false) => { try { setDebug(await getJSON<DebugLog>(activeRequest ? `/admin/active-requests/${id}/debug-log` : `/admin/debug-logs/${id}`)); setDebugTab('request') } catch (cause) { setError(cause instanceof Error ? cause.message : '调试日志加载失败') } }
  const abort = async (id: unknown) => { try { await postJSON(`/admin/active-requests/${encodeURIComponent(text(id))}/abort`); setActive((items) => items.filter((item) => text(item.id) !== text(id))) } catch (cause) { setError(cause instanceof Error ? cause.message : '中断请求失败') } }
  const merge = async () => { if (!debug) return; try { const body = text(debug.resp_body ?? debug.translated_resp_body ?? ''); const result = await postJSON('/admin/debug-logs/merged-response', { resp_body: body }); setDebug({ ...debug, merged_response: result }); setDebugTab('merged') } catch (cause) { setError(cause instanceof Error ? cause.message : '合并响应失败') } }
  const copy = async () => { if (!debug) return; const key = debugTab === 'merged' ? 'merged_response' : debugTab === 'request' ? 'req_body' : debugTab === 'translated_request' ? 'original_req_body' : debugTab === 'response' ? 'resp_body' : 'translated_resp_body'; await navigator.clipboard?.writeText(text(debug[key])) }
  const changeHiddenColumn = (key: string) => setHiddenColumns((current) => current.includes(key) ? current.filter((item) => item !== key) : [...current, key])
  const applyFilters = () => { setPage(1); void load(1); void loadBootstrap() }
  const jump = () => { const target = Math.min(totalPages, Math.max(1, Number(jumpPage) || 1)); setPage(target); setJumpPage('') }
  const debugValue = debug && (debugTab === 'request' ? debug.req_body : debugTab === 'translated_request' ? debug.original_req_body : debugTab === 'response' ? debug.resp_body : debugTab === 'translated_response' ? debug.translated_resp_body : debug.merged_response)

  return <><header className="page-header"><div><h1>请求日志</h1><p className="muted">共 {rows.count} 条记录</p></div><button className="btn" disabled={loading} onClick={() => void load()}>刷新</button></header>
    <div className="toolbar"><select className="select" value={range} onChange={(event) => { setRange(event.target.value); setPage(1) }}><option value="today">今天</option><option value="yesterday">昨天</option><option value="this_week">本周</option><option value="this_month">本月</option><option value="all">全部</option><option value="custom">自定义</option></select>{range === 'custom' && <><input className="input" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /><input className="input" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></>}<input className="input" list="log-channels" value={channel} onChange={(event) => setChannel(event.target.value)} placeholder="渠道" /><datalist id="log-channels">{options.channels.map((item) => <option key={item.id} value={item.name} />)}</datalist><input className="input" list="log-models" value={model} onChange={(event) => setModel(event.target.value)} placeholder="模型" /><datalist id="log-models">{options.models.map((item) => <option key={item} value={item} />)}</datalist><label className="muted"><input type="checkbox" checked={exact} onChange={(event) => setExact(event.target.checked)} /> 精确匹配</label><input className="input" list="log-status-codes" value={status} onChange={(event) => setStatus(event.target.value)} placeholder="状态码" /><datalist id="log-status-codes">{options.status_codes.map((item) => <option key={item} value={item} />)}</datalist><select className="select" value={protocol} onChange={(event) => setProtocol(event.target.value)}><option value="">全部协议</option><option value="openai">OpenAI</option><option value="anthropic">Anthropic</option><option value="gemini">Gemini</option><option value="codex">Codex</option></select><select className="select" value={token} onChange={(event) => setToken(event.target.value)}><option value="">全部令牌</option>{options.auth_tokens.map((item) => <option key={item.id} value={item.id}>{item.description || item.name || `#${item.id}`}</option>)}</select><select className="select" value={source} onChange={(event) => setSource(event.target.value)}><option value="all">全部来源</option><option value="proxy">代理请求</option><option value="jev">JEV</option><option value="scheduled_check">定时检测</option><option value="manual_test">手动测试</option><option value="manual_chat">手动聊天</option><option value="checkin">签到</option><option value="detection">检测</option></select><button className="btn btn-primary" onClick={applyFilters}>查询</button></div>
    {error && <div className="card error-text">{error}</div>}
    {active.length > 0 && <div className="card"><h2>进行中的请求</h2><div className="table-wrap"><table><thead><tr><th>ID</th><th>渠道</th><th>模型</th><th>状态</th><th>操作</th></tr></thead><tbody>{active.map((item) => <tr key={text(item.id)}><td>{text(item.id)}</td><td>{text(item.channel_name)}</td><td>{text(item.model)}</td><td>{text(item.status || '运行中')}</td><td><button className="btn" onClick={() => void showDebug(item.id, true)}>调试</button><button className="btn" onClick={() => void abort(item.id)}>中断</button></td></tr>)}</tbody></table></div></div>}
    <div className="toolbar"><details><summary className="btn">显示列</summary><div className="column-menu">{columns.map((column) => <label key={column.key}><input type="checkbox" checked={!hiddenColumns.includes(column.key)} onChange={() => changeHiddenColumn(column.key)} /> {column.label}</label>)}</div></details></div>
    <div className="table-wrap"><table><thead><tr>{visibleColumns.map((column) => <th key={column.key}>{column.label}</th>)}<th>操作</th></tr></thead><tbody>{loading ? <tr><td colSpan={visibleColumns.length + 1}>加载中...</td></tr> : rows.data.length === 0 ? <tr><td colSpan={visibleColumns.length + 1}>暂无日志数据</td></tr> : rows.data.map((row, index) => <tr key={text(row.id ?? index)} onClick={() => setSelected(row)}>{visibleColumns.map((column) => <td key={column.key}>{column.key === 'status' ? <span className={number(row.status_code) >= 200 && number(row.status_code) < 300 ? 'success-text' : 'error-text'}>{column.value(row)}</span> : column.value(row)}</td>)}<td>{row.id && <button className="btn" onClick={(event) => { event.stopPropagation(); void showDebug(row.id) }}>调试</button>}</td></tr>)}</tbody></table></div>
    <div className="toolbar"><button className="btn" disabled={page <= 1} onClick={() => setPage(1)}>首页</button><button className="btn" disabled={page <= 1} onClick={() => setPage((current) => Math.max(1, current - 1))}>上一页</button><span className="muted">第 {page} 页，共 {totalPages} 页</span><input className="input compact" type="number" min={1} max={totalPages} value={jumpPage} onChange={(event) => setJumpPage(event.target.value)} onKeyDown={(event) => { if (event.key === 'Enter') jump() }} placeholder="页码" /><button className="btn" onClick={jump}>跳转</button><button className="btn" disabled={page >= totalPages} onClick={() => setPage((current) => Math.min(totalPages, current + 1))}>下一页</button><button className="btn" disabled={page >= totalPages} onClick={() => setPage(totalPages)}>尾页</button></div>
    {selected && <div className="card"><div className="toolbar"><h2>日志详情</h2><button className="btn" onClick={() => setSelected(null)}>关闭</button></div><pre className="result-pre">{JSON.stringify(selected, null, 2)}</pre></div>}
    {debug && <div className="card"><div className="toolbar"><h2>调试日志</h2><button className="btn" onClick={() => setDebug(null)}>关闭</button><button className="btn" onClick={() => void copy()}>复制当前内容</button><button className="btn" onClick={() => void merge()}>合并响应</button></div><div className="toolbar">{(['request', 'translated_request', 'response', 'translated_response', 'merged'] as const).map((tab) => <button className={debugTab === tab ? 'btn btn-primary' : 'btn'} key={tab} onClick={() => setDebugTab(tab)}>{tab === 'request' ? '请求' : tab === 'translated_request' ? '转换请求' : tab === 'response' ? '响应' : tab === 'translated_response' ? '转换响应' : '合并响应'}</button>)}</div><div className="toolbar muted">{text(debug.req_method)} {text(debug.req_url)} · HTTP {text(debug.resp_status)}</div><pre className="result-pre">{typeof debugValue === 'string' ? debugValue : JSON.stringify(debugValue, null, 2)}</pre></div>}
  </>
}
