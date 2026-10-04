import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Dialog } from '../components/dialog'
import { DateRangeFilter, normalizeRange, rangeParams, type DateRangeValue } from '../components/date-range'
import { SearchableSelect } from '../components/searchable-select'
import { ApiError, deleteJSON, getJSON, getPaginated, postJSON } from '../lib/api'
import { isAPITokenRole, shouldHideChannels } from '../lib/auth'
import { cacheHitRate, formatCompact, formatDateTime, formatSeconds, formatUSD } from '../lib/format'
import { useSession } from '../lib/session-context'
import { readQuery, readStored, writeQuery } from '../lib/url-state'
import { ChannelEditorDialog } from './channels/channel-editor-dialog'

type LogRow = {
  id: number; time?: number; model?: string; actual_model?: string; response_model?: string; log_source?: string; channel_id?: number; channel_name?: string
  status_code?: number; message?: string; duration?: number; is_streaming?: boolean; upstream_websocket?: boolean; first_byte_time?: number
  api_key_used?: string; api_key_hash?: string; auth_token_id?: number; auth_token_description?: string; client_protocol?: string; upstream_protocol?: string
  client_ip?: string; base_url?: string; service_tier?: string; thinking_effort?: string; input_tokens?: number; output_tokens?: number; reasoning_tokens?: number
  cache_read_input_tokens?: number; cache_creation_input_tokens?: number; cache_5m_input_tokens?: number; cache_1h_input_tokens?: number
  cost?: number; cost_multiplier?: number; effective_cost?: number
  cost_breakdown?: { input?: { tokens?: number; price?: number; cost?: number }; output?: { tokens?: number; price?: number; cost?: number }; cache_read?: { tokens?: number; price?: number; cost?: number }; cache_write?: { tokens?: number; price?: number; cost?: number }; total?: number; service_tier_multiplier?: number }
}
type ActiveRow = {
  id: number; model?: string; client_ip?: string; start_time?: number; is_streaming?: boolean; channel_id?: number; channel_name?: string; client_protocol?: string
  api_key_used?: string; token_id?: number; base_url?: string; bytes_received?: number; client_first_byte_time?: number; cost_multiplier?: number
  upstream_websocket?: boolean; debug_log_available?: boolean; thinking_effort?: string; upstream_status?: string; abortable?: boolean
}
type Bootstrap = { channel_test_content?: string; log_channel_click_action?: string; auth_tokens?: Array<{ id: number; description?: string }>; models?: string[]; channels?: Array<{ id: number; name: string }>; status_codes?: number[] }
type Filters = { range: DateRangeValue; channel: string; model: string; status: string; protocol: string; source: string; token: string }

const PAGE_SIZE = 15
const FILTER_KEY = 'logs.filters'
const COLUMN_KEY = 'ccload_logs_columns'
const PROTOCOLS = [{ value: '', label: '全部协议' }, { value: 'anthropic', label: 'Claude Code' }, { value: 'codex', label: 'Codex' }, { value: 'openai', label: 'OpenAI' }, { value: 'gemini', label: 'Gemini' }]
// 与旧版 page-filters.js 的日志来源一致。
const SOURCES = [{ value: 'proxy', label: '请求日志' }, { value: 'detection', label: '检测日志' }, { value: 'checkin', label: '签到' }, { value: 'jev', label: 'Jev' }, { value: 'all', label: '全部日志' }]
const SOURCE_LABEL: Record<string, string> = { scheduled_check: '定时检测', manual_test: '手动测试', manual_chat: '手动对话', checkin: '签到', jev: 'Jev' }
const UPSTREAM_STATUS: Record<string, string> = { requesting: '请求上游中', receiving: '接收响应中', retrying: '重试中' }
const COLUMNS = [
  ['time', '时间'], ['ip', 'IP'], ['token', '令牌'], ['key', 'Key'], ['channel', '渠道'], ['model', '模型'], ['status', '状态'],
  ['timing', '首字/耗时'], ['speed', '速度'], ['input', '输入'], ['output', '输出'], ['cache_read', '缓读'], ['cache_write', '缓建'], ['hit', '命中'], ['cost', '成本'], ['info', '信息'],
] as const
type ColumnKey = typeof COLUMNS[number][0]
// 只读角色看不到 IP、令牌、Key 列（旧版 styles.css:6348-6356）。
const ADMIN_ONLY: ColumnKey[] = ['ip', 'token', 'key']
const num = (value: unknown) => { const parsed = Number(value); return Number.isFinite(parsed) ? parsed : 0 }
// IP 掩码隐藏后两段，与旧版 logs.js:maskIP 一致。
const maskIP = (ip?: string) => {
  if (!ip) return '-'
  if (ip.length <= 3) return ip
  const v4 = ip.split('.')
  if (v4.length === 4) return `${v4[0]}.${v4[1]}.*.*`
  const v6 = ip.split(':')
  return v6.length >= 2 ? `${v6[0]}:${v6[1]}::*` : ip
}

function initialFilters(): Filters {
  const query = readQuery()
  const stored = readStored<Partial<Filters>>(FILTER_KEY, {})
  const keys = ['range', 'channel_name', 'channel_name_like', 'model', 'model_like', 'status_code', 'client_protocol', 'log_source', 'auth_token_id']
  if (!keys.some((key) => query.has(key))) return { range: normalizeRange(stored.range), channel: stored.channel ?? '', model: stored.model ?? '', status: stored.status ?? '', protocol: stored.protocol ?? '', source: stored.source ?? 'proxy', token: stored.token ?? '' }
  return {
    range: normalizeRange({ range: query.get('range') ?? 'today', start: Number(query.get('start_time')), end: Number(query.get('end_time')) }),
    channel: query.get('channel_name') ?? query.get('channel_name_like') ?? '', model: query.get('model') ?? query.get('model_like') ?? '',
    status: query.get('status_code') ?? '', protocol: query.get('client_protocol') ?? '', source: query.get('log_source') ?? 'proxy', token: query.get('auth_token_id') ?? '',
  }
}

/** 管理员响应只有 cost 与 cost_multiplier（Token 投影才有 effective_cost），倍率后成本需前端计算。 */
const effectiveCost = (row: { cost?: number; cost_multiplier?: number; effective_cost?: number }) => row.effective_cost != null ? num(row.effective_cost) : num(row.cost) * (row.cost_multiplier && row.cost_multiplier > 0 ? row.cost_multiplier : 1)

export function LogsPage() {
  const session = useSession()
  const readOnly = useMemo(() => isAPITokenRole(), [])
  const hideChannels = shouldHideChannels(session)
  const [filters, setFilters] = useState<Filters>(initialFilters)
  const [page, setPage] = useState(1)
  const [jump, setJump] = useState('')
  const [rows, setRows] = useState<LogRow[]>([])
  const [count, setCount] = useState(0)
  const [active, setActive] = useState<ActiveRow[]>([])
  const [boot, setBoot] = useState<Bootstrap>({})
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [hidden, setHidden] = useState<string[]>(() => { try { const raw = JSON.parse(localStorage.getItem(COLUMN_KEY) ?? '{}') as Record<string, boolean>; return Object.entries(raw).filter(([, visible]) => visible === false).map(([key]) => key) } catch { return [] } })
  const [columnMenu, setColumnMenu] = useState(false)
  const [debug, setDebug] = useState<{ logId?: number; activeId?: number } | null>(null)
  const [keyTest, setKeyTest] = useState<LogRow | null>(null)
  const [editChannel, setEditChannel] = useState<{ id: number; name: string } | null>(null)
  const [aborting, setAborting] = useState<Record<number, number>>({})
  const generation = useRef(0)
  const activeFingerprint = useRef('')
  const lastFilters = useRef<Filters | null>(null)

  const channelNames = useMemo(() => [...new Set([...(boot.channels ?? []).map((item) => item.name), ...rows.map((row) => row.channel_name ?? '').filter(Boolean)])].sort(), [boot.channels, rows])
  const modelNames = useMemo(() => [...new Set([...(boot.models ?? []), ...rows.map((row) => row.model ?? '').filter(Boolean)])].sort(), [boot.models, rows])
  const statusCodes = useMemo(() => [...new Set([...(boot.status_codes ?? []), ...rows.map((row) => num(row.status_code)).filter(Boolean)])].sort((a, b) => a - b), [boot.status_codes, rows])
  const tokenOptions = readOnly ? null : (boot.auth_tokens ?? []).map((token) => ({ value: String(token.id), label: token.description || `令牌 #${token.id}` }))

  const params = useCallback((value: Filters): Record<string, string | number> => {
    const result: Record<string, string | number> = { ...rangeParams(value.range) }
    // 命中已知选项按精确匹配，否则模糊匹配（旧版 logs.js:178-197）。
    if (value.channel && !hideChannels) result[channelNames.includes(value.channel) ? 'channel_name' : 'channel_name_like'] = value.channel
    if (value.model) result[modelNames.includes(value.model) ? 'model' : 'model_like'] = value.model
    if (value.status) result.status_code = value.status
    if (value.protocol) result.client_protocol = value.protocol
    if (!readOnly && value.source) result.log_source = value.source
    if (tokenOptions && value.token) result.auth_token_id = value.token
    return result
  }, [hideChannels, channelNames, modelNames, readOnly, tokenOptions])

  const load = useCallback(async (silent = false) => {
    const current = ++generation.current
    if (!silent) setLoading(true)
    try {
      const result = await getPaginated<LogRow>('/dashboard/logs', { ...params(filters), limit: PAGE_SIZE, offset: (page - 1) * PAGE_SIZE })
      if (current !== generation.current) return
      setRows(result.data); setCount(result.count); setError('')
    } catch (cause) { if (current === generation.current) setError(cause instanceof Error ? cause.message : '日志加载失败') }
    finally { if (current === generation.current && !silent) setLoading(false) }
  }, [filters, page, params])

  useEffect(() => {
    void getJSON<Bootstrap>('/dashboard/logs/bootstrap', rangeParams(filters.range)).then(setBoot).catch(() => undefined)
  }, [filters.range])

  useEffect(() => {
    localStorage.setItem(FILTER_KEY, JSON.stringify(filters))
    writeQuery(params(filters), lastFilters.current !== null && lastFilters.current !== filters)
    lastFilters.current = filters
  }, [filters, params])
  useEffect(() => { void load() }, [load])

  // 进行中请求：仅管理员，每 2 秒轮询，页面隐藏时暂停；请求结束或换渠道/Key/URL 时静默刷新日志。
  const showActive = !readOnly && page === 1 && filters.range.range === 'today' && !filters.status && (filters.source === 'proxy' || filters.source === 'all')
  useEffect(() => {
    if (!showActive) { setActive([]); return }
    let stopped = false
    let timer = 0
    const poll = async () => {
      if (!document.hidden) {
        try {
          const list = await getJSON<ActiveRow[]>('/admin/active-requests')
          if (stopped) return
          const items = Array.isArray(list) ? list : []
          const fingerprint = items.map((item) => `${item.id}:${item.channel_id}:${item.base_url}:${item.api_key_used}`).sort().join('|')
          if (activeFingerprint.current && fingerprint !== activeFingerprint.current) window.setTimeout(() => void load(true), 2000)
          activeFingerprint.current = fingerprint
          setActive(items)
        } catch { /* 下次轮询重试 */ }
      }
      if (!stopped) timer = window.setTimeout(() => void poll(), 2000)
    }
    void poll()
    return () => { stopped = true; window.clearTimeout(timer); activeFingerprint.current = '' }
  }, [showActive, load])

  const visibleActive = useMemo(() => active.filter((item) => {
    if (filters.channel && !hideChannels && !(channelNames.includes(filters.channel) ? item.channel_name === filters.channel : String(item.channel_name ?? '').toLowerCase().includes(filters.channel.toLowerCase()))) return false
    if (filters.model && !(modelNames.includes(filters.model) ? item.model === filters.model : String(item.model ?? '').toLowerCase().includes(filters.model.toLowerCase()))) return false
    if (filters.protocol && item.client_protocol !== filters.protocol) return false
    if (filters.token && String(item.token_id ?? '') !== filters.token) return false
    return true
  }), [active, filters, hideChannels, channelNames, modelNames])

  const set = (patch: Partial<Filters>) => { setFilters((current) => ({ ...current, ...patch })); setPage(1) }
  const clear = () => { setFilters({ range: { range: 'today' }, channel: '', model: '', status: '', protocol: '', source: 'proxy', token: '' }); setPage(1) }
  const totalPages = Math.max(1, Math.ceil(count / PAGE_SIZE))
  const visibleColumns = COLUMNS.filter(([key]) => !hidden.includes(key) && !(readOnly && ADMIN_ONLY.includes(key)) && !(hideChannels && key === 'channel'))
  const show = (key: ColumnKey) => visibleColumns.some(([column]) => column === key)
  const toggleColumn = (key: string, visible: boolean) => {
    const next = visible ? hidden.filter((item) => item !== key) : [...hidden, key]
    setHidden(next)
    localStorage.setItem(COLUMN_KEY, JSON.stringify(Object.fromEntries(next.map((item) => [item, false]))))
  }

  // 中断只是让当前请求放弃这一轮上游、改走下一个渠道，请求本身可能继续。
  const abort = async (item: ActiveRow) => {
    if (!window.confirm(`中断请求 #${item.id} 当前的上游尝试？请求会改用下一个渠道重试（若下游已开始响应则终止）。`)) return
    setAborting((current) => ({ ...current, [item.id]: Number(item.start_time ?? 0) }))
    try { await postJSON(`/admin/active-requests/${item.id}/abort`) }
    catch (cause) { setNotice(cause instanceof Error ? cause.message : '中断失败'); setAborting((current) => { const next = { ...current }; delete next[item.id]; return next }) }
  }
  useEffect(() => {
    // start_time 变化说明已切到下一轮上游，恢复按钮；请求消失也清理。
    setAborting((current) => { const next = { ...current }; for (const [id, start] of Object.entries(current)) { const item = active.find((row) => row.id === Number(id)); if (!item || Number(item.start_time ?? 0) !== start) delete next[Number(id)] } return Object.keys(next).length === Object.keys(current).length ? current : next })
  }, [active])

  const clickChannel = (row: { channel_id?: number; channel_name?: string }) => {
    if (readOnly || !row.channel_id) return
    if (boot.log_channel_click_action === 'navigate') window.location.href = `/web/channels?id=${row.channel_id}#channel-${row.channel_id}`
    else setEditChannel({ id: row.channel_id, name: row.channel_name ?? '' })
  }
  const canDebug = (row: LogRow) => !readOnly && (num(row.channel_id) > 0 || row.log_source === 'jev')

  return <>
    <header className="page-header">
      <div><h1>请求日志</h1><p className="muted">查看请求记录、进行中请求与上游调试详情</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <div style={{ position: 'relative' }}>
          <button className="btn" aria-expanded={columnMenu} onClick={() => setColumnMenu(!columnMenu)}>列设置</button>
          {columnMenu && <div className="column-menu" style={{ position: 'absolute', right: 0, top: 'calc(100% + 6px)', zIndex: 30 }} onKeyDown={(event) => { if (event.key === 'Escape') setColumnMenu(false) }}>
            {COLUMNS.filter(([key]) => !(readOnly && ADMIN_ONLY.includes(key)) && !(hideChannels && key === 'channel')).map(([key, label]) => <label key={key}><input type="checkbox" checked={!hidden.includes(key)} onChange={(event) => toggleColumn(key, event.target.checked)} />{label}</label>)}
          </div>}
        </div>
        <button className="btn" disabled={loading} onClick={() => void load()}>刷新</button>
      </div>
    </header>

    <div className="toolbar">
      <DateRangeFilter value={filters.range} onChange={(range) => set({ range })} />
      {!hideChannels && <SearchableSelect ariaLabel="渠道" className="combobox-inline" allowCustomInput value={filters.channel} options={[{ value: '', label: '所有渠道' }, ...channelNames.map((name) => ({ value: name, label: name }))]} onChange={(channel) => set({ channel })} placeholder="渠道" />}
      <SearchableSelect ariaLabel="模型" className="combobox-inline" allowCustomInput value={filters.model} options={[{ value: '', label: '所有模型' }, ...modelNames.map((name) => ({ value: name, label: name }))]} onChange={(model) => set({ model })} placeholder="模型" />
      <SearchableSelect ariaLabel="状态码" className="combobox-inline" value={filters.status} options={[{ value: '', label: '所有状态码' }, ...statusCodes.map((code) => ({ value: String(code), label: String(code) }))]} onChange={(status) => set({ status })} />
      <SearchableSelect ariaLabel="请求协议" className="combobox-inline" value={filters.protocol} options={PROTOCOLS} onChange={(protocol) => set({ protocol })} />
      {!readOnly && <SearchableSelect ariaLabel="日志来源" className="combobox-inline" value={filters.source} options={SOURCES} onChange={(source) => set({ source })} />}
      {tokenOptions && <SearchableSelect ariaLabel="令牌" className="combobox-inline" value={filters.token} options={[{ value: '', label: '全部令牌' }, ...tokenOptions]} onChange={(token) => set({ token })} />}
      <button className="btn" onClick={clear}>清空</button>
    </div>
    {error && <div className="card error-text" role="alert">{error}</div>}
    {notice && <div className="card" role="status"><div className="job-summary" style={{ margin: 0 }}><span>{notice}</span><button className="link-button" onClick={() => setNotice('')}>关闭</button></div></div>}

    <div className="table-wrap"><table><thead><tr>{visibleColumns.map(([key, label]) => <th key={key}>{label}</th>)}</tr></thead><tbody>
      {visibleActive.map((item) => {
        const elapsed = Math.max(0, (Date.now() - num(item.start_time)) / 1000)
        return <tr key={`active-${item.id}`} className="active-row">
          {show('time') && <td className="cell-stack"><span>{formatDateTime(item.start_time)}</span><span className="badge badge-warn">进行中 {elapsed.toFixed(1)}s</span></td>}
          {show('ip') && <td title={item.client_ip}>{maskIP(item.client_ip)}</td>}
          {show('token') && <td>{item.token_id ? (tokenOptions?.find((option) => option.value === String(item.token_id))?.label ?? `#${item.token_id}`) : '-'}</td>}
          {show('key') && <td><code>{item.api_key_used || '-'}</code></td>}
          {show('channel') && <td>{item.channel_id ? <button className="link-button" title={item.base_url} onClick={() => clickChannel(item)}>{item.channel_name || `#${item.channel_id}`}</button> : <span className="muted">选择渠道中…</span>}{item.upstream_websocket && <span className="badge">ws</span>}{item.cost_multiplier && item.cost_multiplier !== 1 ? <span className="badge">×{item.cost_multiplier}</span> : null}</td>}
          {show('model') && <td>{item.model}{item.thinking_effort && <span className="badge">{item.thinking_effort}</span>}</td>}
          {show('status') && <td>{UPSTREAM_STATUS[item.upstream_status ?? ''] ?? item.upstream_status ?? '-'}</td>}
          {show('timing') && <td className="cell-stack"><span>{item.client_first_byte_time ? formatSeconds(item.client_first_byte_time) : '-'}</span><span className="table-note">{item.is_streaming ? '流' : ''}</span></td>}
          {show('speed') && <td>-</td>}
          {show('input') && <td>-</td>}
          {show('output') && <td>{item.bytes_received ? `${formatCompact(item.bytes_received)}B` : '-'}</td>}
          {show('cache_read') && <td>-</td>}
          {show('cache_write') && <td>-</td>}
          {show('hit') && <td>-</td>}
          {show('cost') && <td>-</td>}
          {show('info') && <td><div className="toolbar" style={{ marginBottom: 0, flexWrap: 'nowrap' }}>
            {item.debug_log_available && <button className="btn btn-sm" onClick={() => setDebug({ activeId: item.id })}>调试</button>}
            {item.abortable && <button className="btn btn-sm" disabled={aborting[item.id] != null} onClick={() => void abort(item)}>{aborting[item.id] != null ? '中断中…' : '中断'}</button>}
          </div></td>}
        </tr>
      })}
      {!rows.length && !visibleActive.length && <tr><td colSpan={visibleColumns.length} className="muted">{loading ? '加载中…' : '暂无日志'}</td></tr>}
      {rows.map((row) => <LogTableRow key={row.id} row={row} show={show} tokenLabel={tokenOptions?.find((option) => option.value === String(row.auth_token_id))?.label} readOnly={readOnly}
        onChannel={() => clickChannel(row)} onDebug={canDebug(row) ? () => setDebug({ logId: row.id }) : undefined}
        onTestKey={!readOnly && num(row.status_code) !== 200 && row.api_key_used && row.channel_id && row.model ? () => setKeyTest(row) : undefined}
        onDeleteKey={!readOnly && (row.status_code === 401 || row.status_code === 403) && row.api_key_used && row.channel_id ? () => void deleteKey(row) : undefined} />)}
    </tbody></table></div>

    <div className="toolbar" style={{ marginTop: 16 }}>
      <button className="btn" disabled={page <= 1} onClick={() => setPage(1)}>首页</button>
      <button className="btn" disabled={page <= 1} onClick={() => setPage(page - 1)}>上一页</button>
      <span className="muted">第 {page} / {totalPages} 页，共 {count} 条</span>
      <button className="btn" disabled={page >= totalPages} onClick={() => setPage(page + 1)}>下一页</button>
      <button className="btn" disabled={page >= totalPages} onClick={() => setPage(totalPages)}>尾页</button>
      <input className="input compact" type="number" min={1} max={totalPages} value={jump} placeholder="跳页" aria-label="跳转页码" onChange={(event) => setJump(event.target.value)}
        onKeyDown={(event) => { if (event.key !== 'Enter') return; const target = Math.trunc(Number(jump)); if (target >= 1 && target <= totalPages) setPage(target); else setNotice(`页码需在 1–${totalPages} 之间`); setJump('') }} />
    </div>

    <DebugLogDialog target={debug} onClose={() => setDebug(null)} />
    <KeyTestDialog row={keyTest} defaultContent={String(boot.channel_test_content ?? '').split('|').map((item) => item.trim()).find(Boolean) ?? 'test'} onClose={() => setKeyTest(null)} />
    <ChannelEditorDialog open={editChannel !== null} editing={editChannel ? { id: editChannel.id, name: editChannel.name } : null}
      onClose={() => setEditChannel(null)} onSaved={() => { setNotice('渠道已保存'); void load(true) }} onNotice={(value) => setNotice(typeof value === 'object' && value && 'error' in value ? String((value as { error: unknown }).error) : '操作完成')}
      onTestModel={() => undefined} />
  </>

  async function deleteKey(row: LogRow) {
    if (!row.channel_id) return
    try {
      const keys = await getJSON<Array<{ key_index: number; api_key: string }>>(`/admin/channels/${row.channel_id}/keys`)
      const matches = await matchKey(keys, row)
      if (matches.length !== 1) { setNotice(matches.length ? '匹配到多个 Key，无法确定要删除哪一个，请到渠道页处理' : '没有找到对应的 Key，可能已被删除'); return }
      if (!window.confirm(`确定从渠道「${row.channel_name}」删除 Key ${row.api_key_used}？`)) return
      await deleteJSON(`/admin/channels/${row.channel_id}/keys/${matches[0].key_index}`)
      if (keys.length === 1 && window.confirm('该渠道已无可用 Key，是否删除整个渠道？')) { await deleteJSON(`/admin/channels/${row.channel_id}`); setNotice('Key 与渠道已删除') }
      else setNotice('Key 已删除')
      void load(true)
    } catch (cause) { setNotice(cause instanceof Error ? cause.message : '删除失败') }
  }
}

const sha256Hex = async (value: string) => {
  if (!value || !crypto?.subtle) return ''
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value))
  return Array.from(new Uint8Array(digest)).map((byte) => byte.toString(16).padStart(2, '0')).join('')
}
// 日志里的 Key 写入时脱敏为前 3 位 + "." + 后 3 位（model/log.go），与旧版 logs.js:maskKeyForCompare 一致。
const maskForCompare = (key: string) => !key ? '' : key.length <= 6 ? '****' : `${key.slice(0, 3)}.${key.slice(-3)}`

/** 定位日志所用 Key：优先按 SHA-256 哈希精确匹配，再回退掩码匹配（旧版 resolveKeyIndexForLogEntry）。 */
async function matchKey(keys: Array<{ key_index: number; api_key: string }>, row: LogRow) {
  if (row.api_key_hash) {
    const target = row.api_key_hash.trim().toLowerCase()
    const byHash: typeof keys = []
    for (const key of keys) if (await sha256Hex(key.api_key) === target) byHash.push(key)
    if (byHash.length) return byHash
  }
  const masked = String(row.api_key_used ?? '').trim()
  return keys.filter((key) => maskForCompare(key.api_key) === masked)
}

function LogTableRow({ row, show, tokenLabel, readOnly, onChannel, onDebug, onTestKey, onDeleteKey }: { row: LogRow; show: (key: ColumnKey) => boolean; tokenLabel?: string; readOnly: boolean; onChannel: () => void; onDebug?: () => void; onTestKey?: () => void; onDeleteKey?: () => void }) {
  const status = num(row.status_code)
  const success = status >= 200 && status < 300
  const duration = num(row.duration); const ttft = num(row.first_byte_time); const output = num(row.output_tokens)
  const generation = row.is_streaming && duration - ttft >= 1 ? duration - ttft : duration
  const speed = success && output > 0 && generation > 0 ? output / generation : null
  const effective = effectiveCost(row)
  const breakdown = row.cost_breakdown
  const costTip = breakdown ? [['输入', breakdown.input], ['输出', breakdown.output], ['缓存读', breakdown.cache_read], ['缓存写', breakdown.cache_write]].filter(([, part]) => part && num((part as { tokens?: number }).tokens) > 0).map(([label, part]) => { const p = part as { tokens?: number; price?: number; cost?: number }; return `${label}: ${formatCompact(p.tokens)} × $${num(p.price)}/M = ${formatUSD(p.cost)}` }).join('\n') + (breakdown.service_tier_multiplier && breakdown.service_tier_multiplier !== 1 ? `\n服务等级 ×${breakdown.service_tier_multiplier}` : '') : ''
  const timingClass = (seconds: number, warn: number, bad: number) => seconds >= bad ? 'error-text' : seconds >= warn ? 'warn-text' : undefined
  return <tr>
    {show('time') && <td>{formatDateTime(row.time)}</td>}
    {show('ip') && <td title={row.client_ip}>{maskIP(row.client_ip)}</td>}
    {show('token') && <td title={row.auth_token_description}>{row.auth_token_id ? (tokenLabel ?? row.auth_token_description ?? `#${row.auth_token_id}`) : '-'}</td>}
    {show('key') && <td className="cell-stack"><code>{row.api_key_used || '-'}</code>{(onTestKey || onDeleteKey) && <span className="toolbar" style={{ marginBottom: 0 }}>{onTestKey && <button className="btn btn-sm" title="测试此 Key" onClick={onTestKey}>⚡ 测试</button>}{onDeleteKey && <button className="btn btn-sm" title="删除此 Key" onClick={onDeleteKey}>删除 Key</button>}</span>}</td>}
    {show('channel') && <td>{row.channel_id ? (readOnly ? <span>{row.channel_name || `#${row.channel_id}`}</span> : <button className="link-button" title={row.base_url} onClick={onChannel}>{row.channel_name || `#${row.channel_id}`}</button>) : <span className="muted">-</span>}{row.upstream_websocket && <span className="badge">ws</span>}{row.cost_multiplier && row.cost_multiplier !== 1 ? <span className="badge">×{row.cost_multiplier}</span> : null}</td>}
    {show('model') && <td className="cell-stack"><span>{row.model}{row.thinking_effort && <span className="badge">{row.thinking_effort}</span>}</span>{row.actual_model && row.actual_model !== row.model && <span className="table-note">↪ {row.actual_model}</span>}{row.response_model && row.response_model !== (row.actual_model || row.model) && <span className="table-note">响应：{row.response_model}</span>}{num(row.reasoning_tokens) > 0 && <span className="table-note">推理 {formatCompact(row.reasoning_tokens)}</span>}</td>}
    {show('status') && <td><span className={`badge ${success ? 'badge-good' : status >= 500 || status === 0 ? 'badge-bad' : 'badge-warn'}`}>{status || '-'}</span></td>}
    {show('timing') && <td className="cell-stack"><span className={timingClass(ttft, 5, 15)}>{row.is_streaming && ttft > 0 ? formatSeconds(ttft) : '-'}</span><span className={`table-note ${timingClass(duration, 30, 120) ?? ''}`}>{formatSeconds(duration)}{row.is_streaming ? ' · 流' : ''}</span></td>}
    {show('speed') && <td>{speed == null ? '-' : `${speed.toFixed(1)} t/s`}</td>}
    {show('input') && <td>{num(row.input_tokens) ? formatCompact(row.input_tokens) : ''}</td>}
    {show('output') && <td>{output ? formatCompact(output) : ''}</td>}
    {show('cache_read') && <td>{num(row.cache_read_input_tokens) ? formatCompact(row.cache_read_input_tokens) : ''}</td>}
    {show('cache_write') && <td className="cell-stack">{num(row.cache_creation_input_tokens) ? <><span>{formatCompact(row.cache_creation_input_tokens)}</span><span className="table-note">{num(row.cache_5m_input_tokens) ? `5m ${formatCompact(row.cache_5m_input_tokens)} ` : ''}{num(row.cache_1h_input_tokens) ? `1h ${formatCompact(row.cache_1h_input_tokens)}` : ''}</span></> : ''}</td>}
    {show('hit') && <td>{cacheHitRate(row.input_tokens, row.cache_read_input_tokens, row.cache_creation_input_tokens)}</td>}
    {show('cost') && <td title={costTip || undefined} className="cell-stack">{num(row.cost) || effective ? <><span>{formatUSD(effective)}</span>{Math.abs(effective - num(row.cost)) > 1e-9 && <span className="table-note">标准 {formatUSD(row.cost)}</span>}{row.service_tier && <span className="badge">{row.service_tier}</span>}</> : ''}</td>}
    {show('info') && <td className="log-message">
      {row.log_source && row.log_source !== 'proxy' && <span className="badge badge-muted">{SOURCE_LABEL[row.log_source] ?? row.log_source}</span>}
      {onDebug ? <button className="link-button" title="查看调试日志" onClick={onDebug}>{summarizeMessage(row) || '查看详情'}</button> : <span title={row.message}>{summarizeMessage(row) || '-'}</span>}
    </td>}
  </tr>
}

/** jev 审计 JSON 摘要为一行；其他信息截断显示。 */
function summarizeMessage(row: LogRow): string {
  const message = String(row.message ?? '').trim()
  if (row.log_source === 'jev' && message.startsWith('{')) {
    try { const parsed = JSON.parse(message) as Record<string, unknown>; return Object.entries(parsed).slice(0, 4).map(([key, value]) => `${key}=${typeof value === 'object' ? JSON.stringify(value) : String(value)}`).join(' ') } catch { /* 非 JSON 原样显示 */ }
  }
  return message.length > 160 ? `${message.slice(0, 160)}…` : message
}

// ---------------------------------------------------------------- 调试日志

type DebugData = Record<string, unknown>
const decodeBody = (data: DebugData, key: string) => {
  const value = data[key]
  if (value == null || value === '') return ''
  if (data[`${key}_encoding`] === 'base64') { try { return new TextDecoder().decode(Uint8Array.from(atob(String(value)), (c) => c.charCodeAt(0))) } catch { return String(value) } }
  return String(value)
}
const pretty = (raw: string) => { const trimmed = raw.trim(); if (!trimmed) return ''; try { return JSON.stringify(JSON.parse(trimmed), null, 2) } catch { return raw } }
// 头字段是 JSON 字符串（admin_debug_log.go:maskSensitiveHeaderJSON），解析后逐行展示，不要再 stringify 一层。
const formatHeaders = (raw: unknown) => {
  if (raw == null || raw === '') return ''
  try { const parsed = typeof raw === 'string' ? JSON.parse(raw) as Record<string, unknown> : raw as Record<string, unknown>; return Object.entries(parsed).map(([key, value]) => `${key}: ${Array.isArray(value) ? value.join(', ') : String(value)}`).join('\n') } catch { return String(raw) }
}

type Tab = 'original_request' | 'request' | 'response' | 'translated_response'

function DebugLogDialog({ target, onClose }: { target: { logId?: number; activeId?: number } | null; onClose: () => void }) {
  const [data, setData] = useState<DebugData | null>(null)
  const [unavailable, setUnavailable] = useState<{ message: string; detail?: Record<string, unknown> } | null>(null)
  const [ended, setEnded] = useState(false)
  const [tab, setTab] = useState<Tab>('request')
  const [wrap, setWrap] = useState(true)
  const [mergedOn, setMergedOn] = useState<Record<string, boolean>>({})
  const [merged, setMerged] = useState<Record<string, { reasoning?: string; content?: string; tools?: string } | string>>({})
  const preRef = useRef<HTMLPreElement>(null)

  useEffect(() => {
    if (!target) return
    setData(null); setUnavailable(null); setEnded(false); setMergedOn({}); setMerged({})
    const url = target.activeId != null ? `/admin/active-requests/${target.activeId}/debug-log` : `/admin/debug-logs/${target.logId}`
    let stopped = false; let timer = 0
    const fetchOnce = async () => {
      try {
        const next = await getJSON<DebugData>(url)
        if (stopped) return
        // 在底部时跟随新内容滚动，否则保持位置。
        const pre = preRef.current
        const atBottom = !pre || pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24
        setData(next)
        setTab((current) => current === 'original_request' && !next.protocol_transformed ? 'request' : current)
        if (atBottom && pre) window.requestAnimationFrame(() => { pre.scrollTop = pre.scrollHeight })
      } catch (cause) {
        if (stopped) return
        if (target.activeId != null && cause instanceof ApiError && cause.status === 404) { setEnded(true); return }
        // 404 时 data 里带不可用原因与 debug_log_enabled / retention（admin_debug_log.go:78-97）。
        setUnavailable({ message: cause instanceof Error ? cause.message : '加载失败', detail: cause instanceof ApiError && cause.data && typeof cause.data === 'object' ? cause.data as Record<string, unknown> : undefined })
        return
      }
      if (target.activeId != null && !stopped) timer = window.setTimeout(() => void fetchOnce(), 1500)
    }
    void fetchOnce()
    return () => { stopped = true; window.clearTimeout(timer) }
  }, [target])

  const transformed = Boolean(data?.protocol_transformed)
  const sections: Record<Tab, string> = data ? {
    // req_* 是发往上游的（转换后）请求；original_req_* 是客户端原始请求，仅协议转换时存在。
    original_request: [`${String(data.original_req_url ?? '')}`, formatHeaders(data.original_req_headers), pretty(decodeBody(data, 'original_req_body'))].filter(Boolean).join('\n\n'),
    request: [`${String(data.req_method ?? '')} ${String(data.req_url ?? '')}`.trim(), formatHeaders(data.req_headers), pretty(decodeBody(data, 'req_body'))].filter(Boolean).join('\n\n'),
    response: [`HTTP ${String(data.resp_status ?? '')}`, data.upstream_error ? `上游错误：${String(data.upstream_error)}` : '', formatHeaders(data.resp_headers), pretty(decodeBody(data, 'resp_body'))].filter(Boolean).join('\n\n'),
    translated_response: [`HTTP ${String(data.translated_resp_status ?? '')}`, formatHeaders(data.translated_resp_headers), pretty(decodeBody(data, 'translated_resp_body'))].filter(Boolean).join('\n\n'),
  } : { original_request: '', request: '', response: '', translated_response: '' }

  const isResponse = tab === 'response' || tab === 'translated_response'
  const bodyKey = tab === 'translated_response' ? 'translated_resp_body' : 'resp_body'
  const loadMerged = async () => {
    if (!data) return
    const on = !mergedOn[tab]
    setMergedOn((current) => ({ ...current, [tab]: on }))
    if (!on) return
    try { setMerged((current) => ({ ...current, [tab]: '合并中…' })); const result = await postJSON<{ reasoning?: string; content?: string; tools?: string }>('/admin/debug-logs/merged-response', { resp_body: decodeBody(data, bodyKey) }); setMerged((current) => ({ ...current, [tab]: result })) }
    catch (cause) { setMerged((current) => ({ ...current, [tab]: cause instanceof Error ? cause.message : '合并失败' })) }
  }
  // 进行中请求随轮询更新合并视图。
  useEffect(() => { if (target?.activeId != null && isResponse && mergedOn[tab] && data) void postJSON<{ reasoning?: string; content?: string; tools?: string }>('/admin/debug-logs/merged-response', { resp_body: decodeBody(data, bodyKey) }).then((result) => setMerged((current) => ({ ...current, [tab]: result }))).catch(() => undefined) }, [data])

  const mergedValue = merged[tab]
  const mergedText = typeof mergedValue === 'string' ? mergedValue : mergedValue ? [mergedValue.reasoning && `【思考】\n${mergedValue.reasoning}`, mergedValue.content && `【内容】\n${mergedValue.content}`, mergedValue.tools && `【工具调用】\n${mergedValue.tools}`].filter(Boolean).join('\n\n') || '（无可合并内容）' : ''
  const current = isResponse && mergedOn[tab] ? mergedText : sections[tab]
  const tabs: Array<[Tab, string, boolean]> = [['original_request', '原始请求', transformed], ['request', transformed ? '转换后请求' : '请求', true], ['response', '原始响应', true], ['translated_response', '转换后响应', transformed]]

  return <Dialog open={target !== null} onClose={onClose} size="xl" title={target?.activeId != null ? `调试日志 · 进行中请求 #${target.activeId}` : `调试日志 · #${target?.logId ?? ''}`}
    description={target?.activeId != null ? (ended ? '请求已结束，显示最后一次快照' : '正在实时更新…') : undefined}
    footer={<>
      <label className="muted"><input type="checkbox" checked={wrap} onChange={(event) => setWrap(event.target.checked)} /> 自动换行</label>
      {isResponse && data && <button className="btn" onClick={() => void loadMerged()}>{mergedOn[tab] ? '查看原始' : '合并视图'}</button>}
      <button className="btn" disabled={!current} onClick={() => void navigator.clipboard?.writeText(current)}>复制</button>
      <button className="btn" onClick={onClose}>关闭</button>
    </>}>
    {unavailable ? <div className="cell-stack">
      <p className="error-text">调试日志不可用：{unavailable.message}</p>
      {unavailable.detail && <p className="muted">{unavailable.detail.reason ? `原因：${String(unavailable.detail.reason)}；` : ''}调试日志{unavailable.detail.debug_log_enabled ? '已开启' : '未开启'}{unavailable.detail.debug_log_retention_minutes != null ? `，保留 ${String(unavailable.detail.debug_log_retention_minutes)} 分钟` : ''}。可在设置页调整 debug_log_enabled 与保留时长。</p>}
    </div> : !data ? <p className="muted">加载中…</p> : <>
      <div className="dialog-tabs" role="tablist">{tabs.filter(([, , visible]) => visible).map(([key, label]) => <button key={key} role="tab" aria-selected={tab === key} className={`dialog-tab${tab === key ? ' active' : ''}`} onClick={() => setTab(key)}>{label}</button>)}</div>
      <pre ref={preRef} className="result-pre debug-pre" style={{ whiteSpace: wrap ? 'pre-wrap' : 'pre' }}>{current || '（空）'}</pre>
    </>}
  </Dialog>
}

// ---------------------------------------------------------------- 测试此 Key

function KeyTestDialog({ row, defaultContent, onClose }: { row: LogRow | null; defaultContent: string; onClose: () => void }) {
  const [content, setContent] = useState(defaultContent)
  const [stream, setStream] = useState(true)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<Record<string, unknown> | null>(null)
  const [hint, setHint] = useState('')
  useEffect(() => { if (row) { setContent(defaultContent); setResult(null); setHint(''); setStream(Boolean(row.is_streaming ?? true)) } }, [row, defaultContent])
  const run = async () => {
    if (!row?.channel_id) return
    setBusy(true); setResult(null); setHint('')
    try {
      let keyIndex: number | undefined
      try {
        const keys = await getJSON<Array<{ key_index: number; api_key: string }>>(`/admin/channels/${row.channel_id}/keys`)
        const matches = await matchKey(keys, row)
        if (matches.length === 1) keyIndex = matches[0].key_index
        else setHint('未能唯一定位该 Key，已按渠道默认顺序选择 Key 测试')
      } catch { setHint('读取 Key 列表失败，已按渠道默认顺序选择 Key 测试') }
      // key_index 可省略，后端会自动选择（testutil/types.go:52）。
      const response = await postJSON<Record<string, unknown>>(`/admin/channels/${row.channel_id}/test`, { model: row.model, stream, content, client_protocol: row.client_protocol || 'anthropic', ...(keyIndex == null ? {} : { key_index: keyIndex }) })
      setResult(response)
    } catch (cause) { setResult({ success: false, error: cause instanceof Error ? cause.message : '测试失败' }) } finally { setBusy(false) }
  }
  return <Dialog open={row !== null} onClose={onClose} size="lg" title={`测试 Key：${row?.api_key_used ?? ''}`} description={row ? `渠道 ${row.channel_name ?? row.channel_id} · 模型 ${row.model}` : undefined}
    footer={<><button className="btn" onClick={onClose}>关闭</button><button className="btn btn-primary" disabled={busy} onClick={() => void run()}>{busy ? '测试中…' : '开始测试'}</button></>}>
    <label><input type="checkbox" checked={stream} onChange={(event) => setStream(event.target.checked)} /> 流式</label>
    <textarea className="input wide test-content" value={content} onChange={(event) => setContent(event.target.value)} />
    {hint && <p className="muted">{hint}</p>}
    {result && <div className="card cell-stack">
      <strong className={result.success === false ? 'error-text' : 'success-text'}>{result.success === false ? '失败' : '成功'}{result.status_code != null ? ` · HTTP ${String(result.status_code)}` : ''}{result.duration_ms != null ? ` · ${String(result.duration_ms)}ms` : ''}</strong>
      {Boolean(result.error || result.message) && <span className={result.success === false ? 'error-text' : 'muted'}>{String(result.error ?? result.message)}</span>}
      {result.response_text != null && <pre className="result-pre">{String(result.response_text)}</pre>}
      {result.api_response != null && <details><summary>完整响应</summary><pre className="result-pre">{typeof result.api_response === 'string' ? result.api_response : JSON.stringify(result.api_response, null, 2)}</pre></details>}
      {result.raw_response != null && <details><summary>原始响应</summary><pre className="result-pre">{String(result.raw_response)}</pre></details>}
      {result.success === false && <p className="muted">失败的 Key 会被自动冷却。</p>}
    </div>}
  </Dialog>
}

