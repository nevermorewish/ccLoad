import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react'
import { SearchableSelect } from '../../components/searchable-select'
import { Dialog } from '../../components/dialog'
import { getJSON, getPaginated, invalidateGetCache, putJSON } from '../../lib/api'
import { isAPITokenRole } from '../../lib/auth'
import { streamSSE } from '../../lib/sse'
import type { Channel } from '../../types'
import { OAuthCleanupPanel, OAuthImportPanel, OAuthLoginPanel } from '../oauth/oauth-panels'
import {
  AUTH_TYPES, PROTOCOL_MODES, batchClearCooldowns, batchDelete, batchEnabled, batchPatch, batchPriority, batchSortOverride,
  codebuddyCheckin, codexQuotaReset, download, managementBalance, managementCheckin, oauthUsage,
  refreshModelsBatch, removeChannel, uploadJSON, type ModelEntry,
} from './api'
import { ChannelEditorDialog } from './channel-editor-dialog'
import { ChannelSortDialog, ConfirmDialog, ModelImportDialog, SortOverrideDialog } from './dialogs'
import { aggregateChannelStats, formatCost, formatDuration, formatRemaining, type ChannelStats } from './stats'
import { TestDialog } from './test-dialog'

// localStorage 键与旧页保持一致，新旧页面之间切换时偏好不丢。
const FILTERS_KEY = 'channels.filters'
const PAGE_SIZE_KEY = 'channels.pageSize'
const BATCH_OPTIONS_KEY = 'channels.batchRefreshOptions'

type Filters = { status: string; authType: string; model: string; search: string }
type Row = Channel & Record<string, unknown>
type UsageEvent = { event?: string; processed?: number; total?: number; succeeded?: number; failed?: number; result?: { channel_id?: number; status?: string; error?: string } }
type BatchRefreshState = { done: number; total: number; current: string; results: Record<number, { status: string; error?: string }> }

const readJSON = <T,>(key: string, fallback: T): T => { try { const raw = localStorage.getItem(key); return raw ? { ...fallback, ...JSON.parse(raw) as T } : fallback } catch { return fallback } }
const clampPageSize = (value: unknown) => { const parsed = Math.trunc(Number(value)); return Number.isFinite(parsed) && parsed >= 1 ? Math.min(parsed, 1000) : 20 }
const authLabel = (value: unknown) => AUTH_TYPES.find((item) => item.value === String(value || 'api_key'))?.label ?? String(value)
const isOAuthRow = (row: Row) => String(row.auth_type || 'api_key') !== 'api_key'
const modelNames = (row: Row) => (Array.isArray(row.models) ? row.models : []).map((entry) => typeof entry === 'string' ? entry : String((entry as ModelEntry).model ?? '')).filter(Boolean)
const errorMessage = (cause: unknown, fallback: string) => cause instanceof Error ? cause.message : fallback
// 指标列高亮：阈值与旧版一致（首字 5s/15s，耗时 15s/30s，成功率 95%/80%）。
const timingTone = (seconds?: number, warn = 5, bad = 15) => { const value = Number(seconds); return !Number.isFinite(value) || value <= 0 ? '' : value >= bad ? 'metric-bad' : value >= warn ? 'metric-warn' : 'metric-good' }
const rateTone = (ratio: number | null | undefined) => !Number.isFinite(Number(ratio)) ? '' : Number(ratio) >= 0.95 ? 'metric-good' : Number(ratio) >= 0.8 ? 'metric-warn' : 'metric-bad'
// 有效优先级按偏离基准优先级着色：扣分越多越红，无偏离为中性。
const effectiveTone = (effective: number, base: number) => {
  const delta = base - effective
  return !Number.isFinite(delta) || delta < 0.1 ? '' : delta >= 10 ? 'metric-bad' : 'metric-warn'
}
const formatPriorityScore = (value: number) => Number.isFinite(value) ? value.toFixed(1) : '-'
const effectiveDeltaLabel = (effective: number, base: number) => {
  const delta = base - effective
  if (!Number.isFinite(delta) || Math.abs(delta) < 0.1) return '无惩罚'
  return `${delta > 0 ? '−' : '+'}${Math.abs(delta).toFixed(1)}`
}
// 健康度列取 healthCache 窗口口径（enable_health_score 开启时后端下发），
// 与右侧 24h 统计列不是同一份数据，两者可能不一致。
const healthFromRow = (row: Row) => {
  const rate = row.success_rate == null ? null : Number(row.success_rate)
  const firstByte = row.health_avg_first_byte_seconds == null ? null : Number(row.health_avg_first_byte_seconds)
  const samples = row.health_sample_count == null ? null : Number(row.health_sample_count)
  if (rate == null && firstByte == null) return null
  return {
    rate: Number.isFinite(rate as number) ? rate : null,
    firstByte: Number.isFinite(firstByte as number) && (firstByte as number) > 0 ? firstByte : null,
    samples: Number.isFinite(samples as number) ? samples : null,
  }
}

export function ChannelsPage() {
  const readOnly = useMemo(() => isAPITokenRole(), [])
  const [filters, setFilters] = useState<Filters>(() => readJSON<Filters>(FILTERS_KEY, { status: 'all', authType: 'all', model: '', search: '' }))
  const [page, setPage] = useState(() => Math.max(1, Number(readJSON<{ page?: number }>(FILTERS_KEY, {}).page ?? 1)))
  const [pageSize, setPageSize] = useState(() => clampPageSize(localStorage.getItem(PAGE_SIZE_KEY) ?? 20))
  const [jumpPage, setJumpPage] = useState('')
  const [rows, setRows] = useState<Row[]>([])
  const [count, setCount] = useState(0)
  const [loading, setLoading] = useState(false)
  const [stats, setStats] = useState<Record<number, ChannelStats>>({})
  const [statsRange, setStatsRange] = useState('today')
  const [options, setOptions] = useState<{ channel_names: string[]; models: string[] }>({ channel_names: [], models: [] })
  const [selected, setSelected] = useState<number[]>([])
  const [notice, setNotice] = useState<{ kind: 'success' | 'error' | 'info'; text: string; detail?: unknown } | null>(null)
  const [rowBusy, setRowBusy] = useState<Record<number, string>>({})
  const [priorityDrafts, setPriorityDrafts] = useState<Record<number, string>>({})
  const [overrideDrafts, setOverrideDrafts] = useState<Record<number, string>>({})
  // 弹窗
  const [editor, setEditor] = useState<{ open: boolean; channel: Channel | null; duplicate: boolean }>({ open: false, channel: null, duplicate: false })
  const [testTarget, setTestTarget] = useState<{ channel: Channel; model?: string } | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Row | 'batch' | null>(null)
  const [sortOpen, setSortOpen] = useState(false)
  const [sortOverrideOpen, setSortOverrideOpen] = useState(false)
  const [batchImportOpen, setBatchImportOpen] = useState(false)
  const [oauthDialog, setOAuthDialog] = useState<null | 'login' | 'import' | 'cleanup'>(null)
  // 批量
  const [advancedOpen, setAdvancedOpen] = useState(false)
  const [batchValues, setBatchValues] = useState({ priority: '0', cost_multiplier: '1', rpm_limit: '0', max_concurrency: '0', daily_cost_limit: '0', protocol_transform_mode: 'auto' })
  const [batchOptions, setBatchOptions] = useState(() => readJSON(BATCH_OPTIONS_KEY, { lowercase_models: false, strip_model_source_prefix: false }))
  const [batchRefresh, setBatchRefresh] = useState<BatchRefreshState | null>(null)
  const [usageProgress, setUsageProgress] = useState<{ processed: number; total: number; succeeded: number; failed: number; done: boolean } | null>(null)
  const [csvProgress, setCsvProgress] = useState<{ state: 'pending' | 'success' | 'error'; text: string } | null>(null)
  const usageAbort = useRef<AbortController | null>(null)
  const loadSeq = useRef(0)
  const priorityTimers = useRef<Record<number, number>>({})
  const overrideTimers = useRef<Record<number, number>>({})
  const busyRef = useRef(false)

  const readBase = readOnly ? '/dashboard' : '/admin'
  const totalPages = Math.max(1, Math.ceil(count / pageSize))

  // ---- 持久化 ----
  useEffect(() => { localStorage.setItem(FILTERS_KEY, JSON.stringify({ ...filters, page })) }, [filters, page])
  useEffect(() => { localStorage.setItem(PAGE_SIZE_KEY, String(pageSize)) }, [pageSize])
  useEffect(() => { localStorage.setItem(BATCH_OPTIONS_KEY, JSON.stringify(batchOptions)) }, [batchOptions])

  // ---- 加载 ----
  const load = useCallback(async () => {
    const seq = ++loadSeq.current
    setLoading(true)
    try {
      const params: Record<string, unknown> = { limit: pageSize, offset: (page - 1) * pageSize, status: filters.status, auth_type: filters.authType, range: statsRange }
      // 命中筛选全集时用精确参数（channel_name / model），否则模糊（search / model_like）。
      if (filters.search.trim()) params[options.channel_names.includes(filters.search.trim()) ? 'channel_name' : 'search'] = filters.search.trim()
      if (filters.model.trim()) params[options.models.includes(filters.model.trim()) ? 'model' : 'model_like'] = filters.model.trim()
      const result = await getPaginated<Row>(`${readBase}/channels`, params)
      if (seq !== loadSeq.current) return // 丢弃过期响应
      setRows(result.data); setCount(result.count)
      // 翻页/筛选后清理不可见的选中项。
      setSelected((current) => current.filter((id) => result.data.some((row) => row.id === id)))
    } catch (cause) {
      if (seq === loadSeq.current) setNotice({ kind: 'error', text: errorMessage(cause, '渠道加载失败') })
    } finally { if (seq === loadSeq.current) setLoading(false) }
  }, [pageSize, page, filters, statsRange, options, readBase])

  const loadStats = useCallback(async () => {
    try {
      const data = await getJSON<{ stats?: Array<Record<string, unknown>> }>(`${readBase}/stats`, { range: statsRange, limit: 500, offset: 0 })
      setStats(aggregateChannelStats(data.stats ?? []))
    } catch { /* 统计列缺失不影响列表 */ }
  }, [readBase, statsRange])

  const refresh = useCallback(async () => { invalidateGetCache(); await Promise.all([load(), loadStats()]) }, [load, loadStats])

  useEffect(() => {
    if (!readOnly) void getJSON<{ value?: string }>('/admin/settings/channel_stats_range').then((setting) => { if (setting?.value) setStatsRange(setting.value) }).catch(() => undefined)
    void getJSON<{ channel_names?: string[]; models?: string[] }>(`${readBase}/channels/filter-options`).then((data) => setOptions({ channel_names: data.channel_names ?? [], models: data.models ?? [] })).catch(() => undefined)
  }, [readOnly, readBase])

  // 深链：旧页链接 channels.html?id=N 被服务端 302 到 /web/channels?id=N（internal/app/static.go），
  // 与旧页一致，按该渠道名预填精确筛选。
  useEffect(() => {
    const id = Number(new URLSearchParams(window.location.search).get('id'))
    if (readOnly || !Number.isInteger(id) || id <= 0) return
    void getJSON<Channel>(`/admin/channels/${id}`).then((channel) => { if (channel?.name) setFilter({ search: channel.name }) }).catch(() => undefined)
  }, [readOnly])

  useEffect(() => { void load() }, [load])
  useEffect(() => { void loadStats() }, [loadStats])

  // 自动刷新：间隔来自设置；有对话框打开、页面不可见或批量任务进行中时跳过本轮。
  useEffect(() => {
    if (readOnly) return
    let timer = 0
    void getJSON<{ value?: string }>('/admin/settings/auto_refresh_interval_seconds').then((setting) => {
      const seconds = Number(setting?.value ?? 0)
      if (!(seconds > 0)) return
      timer = window.setInterval(() => {
        if (document.hidden || document.querySelector('dialog[open]') || busyRef.current || Object.keys(priorityTimers.current).length) return
        void refresh()
      }, Math.max(seconds, 5) * 1000)
    }).catch(() => undefined)
    return () => window.clearInterval(timer)
  }, [readOnly, refresh])

  useEffect(() => { busyRef.current = Boolean(batchRefresh && batchRefresh.done < batchRefresh.total) || Boolean(usageProgress && !usageProgress.done) }, [batchRefresh, usageProgress])

  const setFilter = (patch: Partial<Filters>) => { setFilters((current) => ({ ...current, ...patch })); setPage(1) }
  const clearFilters = () => { setFilters({ status: 'all', authType: 'all', model: '', search: '' }); setPage(1) }
  const notify = (kind: 'success' | 'error' | 'info', text: string, detail?: unknown) => setNotice({ kind, text, detail })

  // ---- 行内优先级（1s 防抖 → batch-priority；Enter/失焦立即保存；Esc 还原） ----
  const flushPriority = async (row: Row) => {
    window.clearTimeout(priorityTimers.current[row.id]); delete priorityTimers.current[row.id]
    const raw = priorityDrafts[row.id]
    if (raw === undefined) return
    const value = Math.trunc(Number(raw))
    const original = Number(row.priority ?? 0)
    setPriorityDrafts((current) => { const next = { ...current }; delete next[row.id]; return next })
    if (!Number.isFinite(value) || value < -99999 || value > 9999999 || value === original) return
    setRows((current) => current.map((item) => item.id === row.id ? { ...item, priority: value } : item))
    try { await batchPriority([{ id: row.id, priority: value }]); notify('success', '优先级已更新') }
    catch (cause) {
      setRows((current) => current.map((item) => item.id === row.id ? { ...item, priority: original } : item))
      notify('error', errorMessage(cause, '优先级更新失败'))
    }
  }
  const editPriority = (row: Row, value: string) => {
    setPriorityDrafts((current) => ({ ...current, [row.id]: value }))
    window.clearTimeout(priorityTimers.current[row.id])
    priorityTimers.current[row.id] = window.setTimeout(() => void flushPriority({ ...row }), 1000)
  }
  const priorityKey = (event: KeyboardEvent<HTMLInputElement>, row: Row) => {
    if (event.key === 'Enter') { event.preventDefault(); void flushPriority(row) }
    if (event.key === 'Escape') {
      event.preventDefault(); event.stopPropagation()
      window.clearTimeout(priorityTimers.current[row.id]); delete priorityTimers.current[row.id]
      setPriorityDrafts((current) => { const next = { ...current }; delete next[row.id]; return next })
    }
  }

  // ---- 行内排序覆盖（同 1s 防抖 → batch-sort-override） ----
  // 留空或填 0 表示取消覆盖，恢复自动健康度排序。
  const flushOverride = async (row: Row) => {
    window.clearTimeout(overrideTimers.current[row.id]); delete overrideTimers.current[row.id]
    const raw = overrideDrafts[row.id]
    if (raw === undefined) return
    const trimmed = raw.trim()
    const value = trimmed === '' ? 0 : Math.trunc(Number(trimmed))
    const original = Number(row.sort_override ?? 0)
    setOverrideDrafts((current) => { const next = { ...current }; delete next[row.id]; return next })
    if (!Number.isFinite(value) || value < -99999 || value > 9999999 || value === original) return
    setRows((current) => current.map((item) => item.id === row.id ? { ...item, sort_override: value } : item))
    try { await batchSortOverride([{ id: row.id, sort_override: value }]); notify('success', value === 0 ? '已取消排序覆盖' : `排序覆盖已设为 ${value}`); await refresh() }
    catch (cause) {
      setRows((current) => current.map((item) => item.id === row.id ? { ...item, sort_override: original } : item))
      notify('error', errorMessage(cause, '排序覆盖更新失败'))
    }
  }
  const editOverride = (row: Row, value: string) => {
    setOverrideDrafts((current) => ({ ...current, [row.id]: value }))
    window.clearTimeout(overrideTimers.current[row.id])
    overrideTimers.current[row.id] = window.setTimeout(() => void flushOverride({ ...row }), 1000)
  }
  const overrideKey = (event: KeyboardEvent<HTMLInputElement>, row: Row) => {
    if (event.key === 'Enter') { event.preventDefault(); void flushOverride(row) }
    if (event.key === 'Escape') {
      event.preventDefault(); event.stopPropagation()
      window.clearTimeout(overrideTimers.current[row.id]); delete overrideTimers.current[row.id]
      setOverrideDrafts((current) => { const next = { ...current }; delete next[row.id]; return next })
    }
  }

  // ---- 行内动作 ----
  const runRow = async (row: Row, label: string, action: () => Promise<unknown>, success: string) => {
    setRowBusy((current) => ({ ...current, [row.id]: label }))
    try { const result = await action(); notify('success', success, result); await refresh() }
    catch (cause) { notify('error', errorMessage(cause, `${label}失败`)) }
    finally { setRowBusy((current) => { const next = { ...current }; delete next[row.id]; return next }) }
  }
  // 启用开关：乐观更新，失败回滚。只提交 enabled 字段，后端走快捷路径。
  const toggleEnabled = async (row: Row) => {
    const next = row.enabled === false
    setRows((current) => current.map((item) => item.id === row.id ? { ...item, enabled: next } : item))
    try { await putJSON(`/admin/channels/${row.id}`, { enabled: next }) }
    catch (cause) {
      setRows((current) => current.map((item) => item.id === row.id ? { ...item, enabled: !next } : item))
      notify('error', errorMessage(cause, '切换失败'))
    }
  }
  const resetCodexQuota = (row: Row) => { if (window.confirm(`确定重置渠道「${row.name}」的 Codex 配额吗？会消耗一次重置机会。`)) void runRow(row, '重置配额', () => codexQuotaReset(row.id), '配额已重置') }

  // ---- 批量 ----
  const selectedRows = rows.filter((row) => selected.includes(row.id))
  const runBatch = async (label: string, action: () => Promise<unknown>, keepSelection = false) => {
    if (!selected.length) return
    try {
      const result = await action() as Record<string, unknown>
      notify('success', `${label}完成`, result)
      if (!keepSelection) setSelected([])
      await refresh()
    } catch (cause) { notify('error', errorMessage(cause, `${label}失败`)) }
  }
  const applyBatchField = (field: keyof typeof batchValues) => {
    const raw = batchValues[field]
    if (field === 'protocol_transform_mode') { void runBatch('设置协议处理', () => batchPatch(selected, { protocol_transform_mode: raw }), true); return }
    const value = Number(raw)
    const integer = field === 'priority' || field === 'rpm_limit' || field === 'max_concurrency'
    if (!Number.isFinite(value) || (integer && !Number.isInteger(value)) || (field !== 'priority' && value < 0) || (field === 'priority' && (value < -99999 || value > 9999999))) { notify('error', '请输入有效的数值'); return }
    // batch-advanced 只读顶层字段，不能包一层 patch。
    void runBatch('批量设置', () => batchPatch(selected, { [field]: value }), true)
  }
  // 模型刷新逐渠道串行，便于显示当前渠道与逐行结果；覆盖模式需确认。
  const refreshModels = async (mode: 'merge' | 'replace') => {
    if (!selected.length) return
    if (mode === 'replace' && !window.confirm(`覆盖模式会用上游返回的模型替换 ${selected.length} 个渠道的现有模型，确定继续吗？`)) return
    const targets = [...selectedRows]
    const state: BatchRefreshState = { done: 0, total: targets.length, current: '', results: {} }
    setBatchRefresh({ ...state })
    for (const row of targets) {
      state.current = row.name; setBatchRefresh({ ...state, results: { ...state.results } })
      try {
        const result = await refreshModelsBatch([row.id], mode, batchOptions) as { results?: Array<{ status?: string; error?: string; warning?: string }> }
        const item = result.results?.[0]
        state.results[row.id] = { status: item?.status ?? 'updated', error: item?.error ?? item?.warning }
      } catch (cause) { state.results[row.id] = { status: 'failed', error: errorMessage(cause, '刷新失败') } }
      state.done++
      setBatchRefresh({ ...state, results: { ...state.results } })
    }
    await refresh()
  }
  // 批量刷新额度走 SSE，逐条更新进度。
  const refreshUsage = async () => {
    if (!selected.length) return
    usageAbort.current?.abort()
    const controller = new AbortController(); usageAbort.current = controller
    setUsageProgress({ processed: 0, total: selected.length, succeeded: 0, failed: 0, done: false })
    try {
      await streamSSE<UsageEvent>('/admin/channels/oauth-usage/batch/stream', { channel_ids: selected }, (event) => {
        setUsageProgress((current) => ({ processed: Number(event.processed ?? current?.processed ?? 0), total: Number(event.total ?? current?.total ?? selected.length), succeeded: Number(event.succeeded ?? current?.succeeded ?? 0), failed: Number(event.failed ?? current?.failed ?? 0), done: event.event === 'complete' }))
      }, controller.signal)
      setUsageProgress((current) => current ? { ...current, done: true } : current)
      await refresh()
    } catch (cause) {
      if (!controller.signal.aborted) { notify('error', errorMessage(cause, '批量刷新额度失败')); setUsageProgress((current) => current ? { ...current, done: true } : current) }
    }
  }
  const confirmDelete = async () => {
    const target = deleteTarget
    setDeleteTarget(null)
    if (target === 'batch') await runBatch('批量删除', () => batchDelete(selected))
    else if (target) { try { await removeChannel(target.id); notify('success', `已删除渠道「${target.name}」`); await refresh() } catch (cause) { notify('error', errorMessage(cause, '删除失败')) } }
  }

  // ---- 导入导出 ----
  const stamp = () => new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '').replace('T', '-')
  const importCSV = async (file: File | undefined) => {
    if (!file) return
    setCsvProgress({ state: 'pending', text: `正在导入 ${file.name}…` })
    try {
      const result = await uploadJSON('/admin/channels/import', file) as { data?: Record<string, unknown> } & Record<string, unknown>
      const summary = (result.data ?? result) as { created?: number; updated?: number; skipped?: number; errors?: string[] }
      const errors = summary.errors ?? []
      setCsvProgress({ state: errors.length ? 'error' : 'success', text: `导入完成：新增 ${Number(summary.created ?? 0)}，更新 ${Number(summary.updated ?? 0)}，跳过 ${Number(summary.skipped ?? 0)}${errors.length ? `；错误：${errors.slice(0, 3).join('；')}${errors.length > 3 ? ` 等 ${errors.length} 条` : ''}` : ''}` })
      await refresh()
    } catch (cause) { setCsvProgress({ state: 'error', text: errorMessage(cause, 'CSV 导入失败') }) }
  }
  const importJSONFile = async (file: File | undefined) => {
    if (!file) return
    try {
      const result = await uploadJSON('/admin/channels/import.json', file) as { data?: Record<string, unknown> } & Record<string, unknown>
      const summary = (result.data ?? result) as { created?: number; updated?: number }
      notify('success', `JSON 导入完成：新增 ${Number(summary.created ?? 0)}，更新 ${Number(summary.updated ?? 0)}`)
      await refresh()
    } catch (cause) { notify('error', errorMessage(cause, 'JSON 导入失败')) }
  }
  const exportFile = async (kind: 'csv' | 'json', ids?: number[]) => {
    try { await download(kind === 'csv' ? '/admin/channels/export' : '/admin/channels/export.json', `channels-${stamp()}.${kind}`, ids?.length ? { ids: ids.join(',') } : undefined) }
    catch (cause) { notify('error', errorMessage(cause, '导出失败')) }
  }

  // ---- 渲染辅助 ----
  const allVisibleSelected = rows.length > 0 && rows.every((row) => selected.includes(row.id))
  const statusCell = (row: Row) => {
    const parts: ReactNode[] = []
    const channelCooldown = Number(row.cooldown_remaining_ms ?? 0)
    if (row.enabled === false) parts.push(<span key="off" className="badge badge-muted">已停用</span>)
    else if (channelCooldown > 0) parts.push(<span key="cd" className="badge badge-warn">冷却 {formatRemaining(channelCooldown)}</span>)
    else parts.push(<span key="ok" className="badge badge-good">正常</span>)
    const keyCooldowns = (Array.isArray(row.key_cooldowns) ? row.key_cooldowns as Array<{ cooldown_remaining_ms?: number }> : []).filter((item) => Number(item.cooldown_remaining_ms ?? 0) > 0)
    if (keyCooldowns.length) parts.push(<span key="kc" className="table-note">{keyCooldowns.length} 个 Key 冷却中</span>)
    const modelCooldowns = Array.isArray(row.model_cooldowns) ? row.model_cooldowns as Array<{ model?: string; cooldown_remaining_ms?: number }> : []
    if (modelCooldowns.length) parts.push(<span key="mc" className="table-note" title={modelCooldowns.map((item) => `${item.model} ${formatRemaining(item.cooldown_remaining_ms)}`).join('\n')}>{modelCooldowns.length} 个模型冷却中</span>)
    if (Number(row.protocol_probe_retry_count ?? 0) > 0) parts.push(<span key="pp" className="table-note">协议探测重试 {formatRemaining(Number(row.protocol_probe_retry_remaining_ms ?? 0))}</span>)
    const usage = row.oauth_usage as { windows?: Array<{ limit_name?: string; kind?: string; used_percent?: number; reset_at?: number }>; display_message?: string } | undefined
    for (const window of usage?.windows ?? []) parts.push(<span key={`w-${window.limit_name}-${window.kind}`} className="table-note">{window.limit_name || window.kind}：已用 {Math.round(Number(window.used_percent ?? 0))}%{window.reset_at ? `，${new Date(window.reset_at * 1000).toLocaleString()} 重置` : ''}</span>)
    if (usage?.display_message) parts.push(<span key="dm" className="table-note">{usage.display_message}</span>)
    const balance = (row.management_account as { balance?: { remaining?: number; unit?: string } } | undefined)?.balance
    if (balance) parts.push(<span key="bal" className="table-note">余额 {Number(balance.remaining ?? 0).toFixed(2)} {balance.unit ?? ''}</span>)
    const stat = stats[row.id]
    if (stat?.lastRequestStatus && stat.lastRequestStatus >= 400) parts.push(<span key="lf" className="table-note error-text" title={stat.lastRequestMessage}>最近失败：{stat.lastRequestStatus}</span>)
    const refreshResult = batchRefresh?.results[row.id]
    if (refreshResult) parts.push(<span key="br" className={`badge ${refreshResult.status === 'failed' ? 'badge-bad' : refreshResult.status === 'updated' ? 'badge-good' : 'badge-muted'}`} title={refreshResult.error}>模型刷新：{refreshResult.status}</span>)
    return <div className="cell-stack">{parts}</div>
  }

  return <>
    <header className="page-header">
      <div><h1>渠道管理</h1><p className="muted">配置 API 渠道、优先级和模型支持（优先请求高优先级渠道，相同优先级按 Key 数量加权随机）</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <button className="btn" disabled={loading} onClick={() => void refresh()}>刷新</button>
        {!readOnly && <>
          <button className="btn" onClick={() => setOAuthDialog('login')}>OAuth 登录</button>
          <button className="btn" onClick={() => setOAuthDialog('import')}>导入凭证</button>
          <button className="btn" onClick={() => setOAuthDialog('cleanup')}>清理凭证</button>
          <label className="btn">导入 CSV<input hidden type="file" accept=".csv,text/csv" onChange={(event) => { void importCSV(event.target.files?.[0]); event.target.value = '' }} /></label>
          <label className="btn">导入 JSON<input hidden type="file" accept=".json,application/json" onChange={(event) => { void importJSONFile(event.target.files?.[0]); event.target.value = '' }} /></label>
          <button className="btn" onClick={() => void exportFile('json')}>导出 JSON</button>
          <button className="btn" onClick={() => setSortOpen(true)}>排序</button>
          <button className="btn" onClick={() => setSortOverrideOpen(true)}>惩罚排序覆盖</button>
          <button className="btn btn-primary" onClick={() => setEditor({ open: true, channel: null, duplicate: false })}>+ 添加渠道</button>
        </>}
      </div>
    </header>

    {csvProgress && <div className="card" role="status" aria-live="polite">
      <div className="job-summary"><strong>CSV 导入</strong><span className={csvProgress.state === 'error' ? 'error-text' : csvProgress.state === 'success' ? 'success-text' : 'muted'}>{csvProgress.text}</span><button className="link-button" onClick={() => setCsvProgress(null)}>关闭</button></div>
      {csvProgress.state === 'pending' && <progress style={{ width: '100%' }} />}
    </div>}

    {notice && <div className={`card ${notice.kind === 'error' ? 'error-text' : ''}`} role={notice.kind === 'error' ? 'alert' : 'status'}>
      <div className="job-summary"><strong>{notice.text}</strong><button className="link-button" onClick={() => setNotice(null)}>关闭</button></div>
      {notice.detail !== undefined && notice.detail !== null && typeof notice.detail === 'object' && <details><summary>详情</summary><pre className="result-pre">{JSON.stringify(notice.detail, null, 2)}</pre></details>}
    </div>}

    {/* ---------------- 筛选 ---------------- */}
    <div className="toolbar">
      <SearchableSelect ariaLabel="渠道名筛选" className="combobox-inline" allowCustomInput value={filters.search} options={[{ value: '', label: '所有渠道' }, ...options.channel_names.map((name) => ({ value: name, label: name }))]} onChange={(value) => setFilter({ search: value })} placeholder="搜索渠道" />
      <SearchableSelect ariaLabel="状态筛选" className="combobox-inline" value={filters.status} options={[{ value: 'all', label: '全部状态' }, { value: 'enabled', label: '已启用' }, { value: 'disabled', label: '已停用' }, { value: 'cooldown', label: '冷却中' }]} onChange={(value) => setFilter({ status: value })} />
      <SearchableSelect ariaLabel="认证类型筛选" className="combobox-inline" value={filters.authType} options={[{ value: 'all', label: '全部认证' }, ...AUTH_TYPES]} onChange={(value) => setFilter({ authType: value })} />
      <SearchableSelect ariaLabel="模型筛选" className="combobox-inline" allowCustomInput value={filters.model} options={[{ value: '', label: '所有模型' }, ...options.models.map((name) => ({ value: name, label: name }))]} onChange={(value) => setFilter({ model: value })} placeholder="模型" />
      <button className="btn" onClick={clearFilters}>清空</button>
      <span className="muted">{rows.length} / {count} 个渠道</span>
    </div>

    {/* ---------------- 批量浮动条 ---------------- */}
    {!readOnly && selected.length > 0 && <div className="card batch-bar">
      <div className="toolbar" style={{ marginBottom: 0 }}>
        <span className="badge">{selected.length}</span><span className="muted">个渠道已选择</span>
        <button className="btn" onClick={() => void runBatch('批量启用', () => batchEnabled(selected, true), true)}>启用</button>
        <button className="btn" onClick={() => void runBatch('批量禁用', () => batchEnabled(selected, false), true)}>禁用</button>
        <button className="btn" disabled={Boolean(usageProgress && !usageProgress.done)} onClick={() => void refreshUsage()}>刷新额度</button>
        <button className="btn" aria-expanded={advancedOpen} onClick={() => setAdvancedOpen(!advancedOpen)}>高级 ▾</button>
        <button className="btn" onClick={() => void exportFile('csv', selected)}>导出 CSV</button>
        <button className="btn" onClick={() => void exportFile('json', selected)}>导出 JSON</button>
        <button className="btn btn-danger" onClick={() => setDeleteTarget('batch')}>删除</button>
        <button className="btn" onClick={() => setSelected([])}>清空选择</button>
      </div>
      {advancedOpen && <div className="advanced-grid" style={{ marginTop: 12 }} onKeyDown={(event) => { if (event.key === 'Escape') { event.stopPropagation(); setAdvancedOpen(false) } }}>
        {([['priority', '优先级', '数值越大优先级越高'], ['cost_multiplier', 'Key 倍率', '0 表示免费渠道'], ['rpm_limit', 'RPM', '0 表示不限制'], ['max_concurrency', '并发', '0 表示不限制'], ['daily_cost_limit', '日限额（美元）', '0 表示不限制']] as const).map(([field, label, hint]) => (
          <label key={field} className="cell-stack">{label}
            <span className="toolbar" style={{ marginBottom: 0 }}>
              <input className="input compact" type="number" step={field === 'cost_multiplier' || field === 'daily_cost_limit' ? '0.01' : '1'} value={batchValues[field]} onChange={(event) => setBatchValues((current) => ({ ...current, [field]: event.target.value }))} />
              <button className="btn" onClick={() => applyBatchField(field)}>设置</button>
            </span>
            <span className="table-note">{hint}</span>
          </label>
        ))}
        <label className="cell-stack">协议处理
          <span className="toolbar" style={{ marginBottom: 0 }}>
            <SearchableSelect ariaLabel="协议处理" className="combobox-inline" value={batchValues.protocol_transform_mode} options={PROTOCOL_MODES} onChange={(value) => setBatchValues((current) => ({ ...current, protocol_transform_mode: value }))} />
            <button className="btn" onClick={() => applyBatchField('protocol_transform_mode')}>设置</button>
          </span>
        </label>
        <div className="cell-stack">模型刷新
          <span className="toolbar" style={{ marginBottom: 0 }}>
            <label><input type="checkbox" checked={batchOptions.lowercase_models} onChange={(event) => setBatchOptions((current) => ({ ...current, lowercase_models: event.target.checked }))} /> 转小写</label>
            <label><input type="checkbox" checked={batchOptions.strip_model_source_prefix} onChange={(event) => setBatchOptions((current) => ({ ...current, strip_model_source_prefix: event.target.checked }))} /> 去来源前缀</label>
            <button className="btn" disabled={Boolean(batchRefresh && batchRefresh.done < batchRefresh.total)} onClick={() => void refreshModels('merge')}>增量</button>
            <button className="btn" disabled={Boolean(batchRefresh && batchRefresh.done < batchRefresh.total)} onClick={() => void refreshModels('replace')}>覆盖</button>
          </span>
        </div>
        <div className="toolbar" style={{ alignItems: 'end' }}>
          <button className="btn" onClick={() => setBatchImportOpen(true)}>导入模型</button>
          <button className="btn" onClick={() => void runBatch('清除冷却', () => batchClearCooldowns(selected), true)}>清除冷却</button>
        </div>
      </div>}
      {batchRefresh && <div className="job-summary">
        <span>模型刷新 {batchRefresh.done}/{batchRefresh.total}</span>
        {batchRefresh.done < batchRefresh.total && <span className="muted">当前：{batchRefresh.current}</span>}
        <progress max={batchRefresh.total} value={batchRefresh.done} style={{ flex: 1 }} />
        {batchRefresh.done >= batchRefresh.total && <button className="link-button" onClick={() => setBatchRefresh(null)}>关闭</button>}
      </div>}
      {usageProgress && <div className="job-summary">
        <span>额度刷新 {usageProgress.processed}/{usageProgress.total}</span><span className="success-text">成功 {usageProgress.succeeded}</span><span className="error-text">失败 {usageProgress.failed}</span>
        <progress max={Math.max(usageProgress.total, 1)} value={usageProgress.processed} style={{ flex: 1 }} />
        {usageProgress.done ? <button className="link-button" onClick={() => setUsageProgress(null)}>关闭</button> : <button className="link-button" onClick={() => { usageAbort.current?.abort(); setUsageProgress((current) => current ? { ...current, done: true } : current) }}>停止</button>}
      </div>}
    </div>}

    {/* ---------------- 表格 ---------------- */}
    <div className="table-wrap"><table><thead><tr>
      {!readOnly && <th><input type="checkbox" aria-label="全选当前页" checked={allVisibleSelected} onChange={(event) => setSelected((current) => event.target.checked ? [...new Set([...current, ...rows.map((row) => row.id)])] : current.filter((id) => !rows.some((row) => row.id === id)))} /></th>}
      <th>渠道</th><th>模型</th><th>优先级</th><th className="metric-col">健康度</th><th className="metric-col">惩罚排序优先级</th><th className="metric-col">首字</th><th className="metric-col">耗时</th><th className="metric-col">请求数</th><th className="metric-col">成功率</th><th className="metric-col">成本</th><th>状态</th>{!readOnly && <th>启用</th>}<th>操作</th>
    </tr></thead><tbody>
      {loading && !rows.length && <tr><td colSpan={readOnly ? 12 : 14} className="muted">加载中…</td></tr>}
      {!loading && !rows.length && <tr><td colSpan={readOnly ? 12 : 14} className="muted">没有符合条件的渠道</td></tr>}
      {rows.map((row) => {
        const urls = Array.isArray(row.urls) ? row.urls as Array<{ url?: string }> : []
        const models = modelNames(row)
        const stat = stats[row.id]
        const oauth = isOAuthRow(row)
        const busy = rowBusy[row.id]
        const management = row.management_account as { profile?: string } | undefined
        const multiplierMin = row.cost_multiplier_min as number | undefined
        const multiplierMax = row.cost_multiplier_max as number | undefined
        const effective = row.effective_priority as number | undefined
        const override = Number(row.sort_override ?? 0)
        // 实际参与排序的值：有覆盖用覆盖，否则健康度开启时用 P_eff、关闭时用基础优先级。
        const autoSortValue = override !== 0 ? override : (effective ?? Number(row.priority ?? 0))
        // 后端在 healthEnabled 时对每个渠道都会下发 effective_priority（含无样本渠道），
        // 因此它等价于「健康度模式已开启」；健康度列另据 success_rate 判断有无样本。
        const healthMode = effective != null
        const health = healthFromRow(row)
        return <tr key={row.id} id={`channel-${row.id}`}>
          {!readOnly && <td><input type="checkbox" aria-label={`选择 ${row.name}`} checked={selected.includes(row.id)} onChange={(event) => setSelected((current) => event.target.checked ? [...current, row.id] : current.filter((id) => id !== row.id))} /></td>}
          <td className="cell-stack">
            <span><strong>{row.name}</strong> <span className="badge badge-muted">{authLabel(row.auth_type)}</span>{multiplierMin != null && <span className="badge" title="成本倍率">×{multiplierMin === multiplierMax ? multiplierMin : `${multiplierMin}–${multiplierMax}`}</span>}</span>
            {urls[0]?.url && <span className="table-note" title={urls.map((item) => item.url).join('\n')}>{urls[0].url}{urls.length > 1 ? ` 等 ${urls.length} 个` : ''}</span>}
          </td>
          <td><span title={models.join('\n')}>{models.length ? `${models.slice(0, 3).join(', ')}${models.length > 3 ? ` 等 ${models.length} 个` : ''}` : '-'}</span></td>
          <td>
            {readOnly ? String(row.priority ?? 0) : <input className="input priority-input" type="number" min={-99999} max={9999999} aria-label={`${row.name} 优先级`} value={priorityDrafts[row.id] ?? String(row.priority ?? 0)} onChange={(event) => editPriority(row, event.target.value)} onBlur={() => void flushPriority(row)} onKeyDown={(event) => priorityKey(event, row)} />}
          </td>
          <td className="metric-cell metric-health" title={health ? `健康度统计窗口内：${health.samples?.toLocaleString() ?? '?'} 个样本（与右侧 24h 统计列口径不同）` : undefined}>{healthMode ? (health ? <>
            <strong className={rateTone(health.rate)}>{health.rate == null ? '-' : `${(health.rate * 100).toFixed(1)}%`}</strong>
            <span className="table-note">{health.firstByte == null ? '首字 -' : `首字 ${formatDuration(health.firstByte)}`}</span>
          </> : <span className="muted" title="该渠道在健康度统计窗口内没有样本">无样本</span>) : <span className="muted" title={readOnly ? '只读视图不提供健康度数据' : '未开启健康度排序（enable_health_score）'}>-</span>}</td>
          <td className="metric-cell metric-effective" title={override !== 0
            ? `已手动覆盖为 ${override}，直接决定选路顺序（不叠加健康度惩罚）。清空输入框可恢复自动。`
            : `留空 = 自动（当前 ${formatPriorityScore(autoSortValue)}）。填入数值即固定该渠道的排序位置，不再叠加失败/首字惩罚。`}>
            {readOnly ? <><strong className={override !== 0 ? 'metric-good' : effectiveTone(autoSortValue, Number(row.priority ?? 0))}>{formatPriorityScore(autoSortValue)}</strong>
              <span className="table-note">{override !== 0 ? '已覆盖' : '自动'}</span></> : <>
              <input className={`input override-input${override !== 0 ? ' is-overridden' : ''}`} type="number" min={-99999} max={9999999}
                aria-label={`${row.name} 排序覆盖`}
                placeholder={formatPriorityScore(autoSortValue)}
                value={overrideDrafts[row.id] ?? (override !== 0 ? String(override) : '')}
                onChange={(event) => editOverride(row, event.target.value)}
                onBlur={() => void flushOverride(row)}
                onKeyDown={(event) => overrideKey(event, row)} />
              <span className="table-note">{override !== 0 ? '已覆盖' : effective != null ? effectiveDeltaLabel(effective, Number(row.priority ?? 0)) : '自动'}</span>
            </>}
          </td>
          <td className="metric-cell metric-ttft">{stat ? <><strong className={timingTone(stat.avgFirstByteTimeSeconds)}>{formatDuration(stat.avgFirstByteTimeSeconds)}</strong><span className="table-note">首字</span></> : <span className="muted">-</span>}</td>
          <td className="metric-cell metric-duration">{stat ? <><strong className={timingTone(stat.avgDurationSeconds, 15, 30)}>{formatDuration(stat.avgDurationSeconds)}</strong><span className="table-note">耗时</span></> : <span className="muted">-</span>}</td>
          <td className="metric-cell metric-requests">{stat ? <><strong>{stat.total.toLocaleString()}</strong><span className="table-note">成功 {stat.success.toLocaleString()}</span></> : <span className="muted">-</span>}</td>
          <td className="metric-cell metric-rate">{stat && stat.total ? <><strong className={rateTone(stat.success / stat.total)}>{((stat.success / stat.total) * 100).toFixed(1)}%</strong><span className="table-note">{stat.total.toLocaleString()} 次</span></> : <span className="muted">-</span>}</td>
          <td className="metric-cell metric-cost">{stat ? <><strong>{formatCost(stat.effectiveCost)}</strong><span className="table-note">24h</span></> : <span className="muted">-</span>}</td>
          <td>{statusCell(row)}</td>
          {!readOnly && <td><label className="switch"><input type="checkbox" aria-label={`${row.name} 启用`} checked={row.enabled !== false} onChange={() => void toggleEnabled(row)} /> {row.enabled === false ? '停用' : '启用'}</label></td>}
          <td><div className="toolbar" style={{ marginBottom: 0, flexWrap: 'nowrap' }}>
            {!readOnly && <button className="btn" onClick={() => setEditor({ open: true, channel: row, duplicate: false })}>编辑</button>}
            {!readOnly && <button className="btn" onClick={() => setTestTarget({ channel: row })}>测试</button>}
            {!readOnly && !oauth && <button className="btn" onClick={() => setEditor({ open: true, channel: row, duplicate: true })}>复制</button>}
            {!readOnly && oauth && <button className="btn" disabled={Boolean(busy)} onClick={() => void runRow(row, '刷新额度', () => oauthUsage(row.id), '额度已刷新')}>刷新额度</button>}
            {!readOnly && row.auth_type === 'codebuddy_oauth' && !row.codebuddy_enterprise && !row.codebuddy_international && <button className="btn" disabled={Boolean(busy)} onClick={() => void runRow(row, '签到', () => codebuddyCheckin(row.id), '签到完成')}>签到</button>}
            {!readOnly && row.auth_type === 'codex_oauth' && <button className="btn" disabled={Boolean(busy)} onClick={() => resetCodexQuota(row)}>重置配额</button>}
            {!readOnly && management?.profile && <button className="btn" disabled={Boolean(busy)} onClick={() => void runRow(row, '刷新余额', () => managementBalance(row.id), '余额已刷新')}>余额</button>}
            {!readOnly && (management?.profile === 'new_api' || management?.profile === 'sub2api_pro') && <button className="btn" disabled={Boolean(busy)} onClick={() => void runRow(row, '签到', () => managementCheckin(row.id), '签到完成')}>面板签到</button>}
            {!readOnly && <button className="btn" onClick={() => setDeleteTarget(row)}>删除</button>}
            {busy && <span className="muted">{busy}中…</span>}
          </div></td>
        </tr>
      })}
    </tbody></table></div>

    {/* ---------------- 分页 ---------------- */}
    <div className="toolbar" style={{ marginTop: 16 }}>
      <button className="btn" disabled={page <= 1} onClick={() => setPage(1)}>首页</button>
      <button className="btn" disabled={page <= 1} onClick={() => setPage((current) => current - 1)}>上一页</button>
      <span className="muted">第 {page} / {totalPages} 页，共 {count} 个渠道</span>
      <button className="btn" disabled={page >= totalPages} onClick={() => setPage((current) => current + 1)}>下一页</button>
      <button className="btn" disabled={page >= totalPages} onClick={() => setPage(totalPages)}>尾页</button>
      <input className="input compact" type="number" min={1} max={totalPages} value={jumpPage} placeholder="跳页" aria-label="跳转页码" onChange={(event) => setJumpPage(event.target.value)} onKeyDown={(event) => { if (event.key !== 'Enter') return; const target = Math.trunc(Number(jumpPage)); if (target >= 1 && target <= totalPages) setPage(target); setJumpPage('') }} />
      <label className="muted">每页<input className="input compact" type="number" min={1} max={1000} defaultValue={pageSize} aria-label="每页条数" onKeyDown={(event) => { if (event.key === 'Enter') { setPageSize(clampPageSize(event.currentTarget.value)); setPage(1) } }} onBlur={(event) => { const next = clampPageSize(event.currentTarget.value); if (next !== pageSize) { setPageSize(next); setPage(1) } }} /></label>
    </div>

    {/* ---------------- 弹窗 ---------------- */}
    <ChannelEditorDialog open={editor.open} editing={editor.duplicate ? null : editor.channel} duplicateOf={editor.duplicate ? editor.channel : null}
      onClose={() => setEditor({ open: false, channel: null, duplicate: false })}
      onSaved={() => { notify('success', '渠道已保存'); void refresh() }}
      onNotice={(value) => notify('info', '操作结果', value)}
      onTestModel={(channel, model) => setTestTarget({ channel, model })} />
    <TestDialog channel={testTarget?.channel ?? null} initialModel={testTarget?.model} onClose={() => { setTestTarget(null); void loadStats() }} />
    <ConfirmDialog open={deleteTarget !== null} danger title={deleteTarget === 'batch' ? '批量删除渠道' : '删除渠道'}
      message={deleteTarget === 'batch' ? `确定删除选中的 ${selected.length} 个渠道吗？此操作不可恢复。` : `确定删除渠道「${deleteTarget?.name ?? ''}」吗？此操作不可恢复。`}
      confirmLabel="删除" onClose={() => setDeleteTarget(null)} onConfirm={confirmDelete} />
    <ChannelSortDialog open={sortOpen} channels={rows} onClose={() => setSortOpen(false)} onSaved={() => { notify('success', '排序已保存'); void refresh() }} />
    <SortOverrideDialog open={sortOverrideOpen} onClose={() => setSortOverrideOpen(false)} onSaved={() => { notify('success', '惩罚排序覆盖已保存'); void refresh() }} />
    <ModelImportDialog open={batchImportOpen} batch onClose={() => setBatchImportOpen(false)} onImport={(models, mode) => runBatch('导入模型', () => batchPatch(selected, { models, model_import_mode: mode }), true)} />
    <Dialog open={oauthDialog === 'login'} onClose={() => setOAuthDialog(null)} size="md" title="OAuth 登录" description="选择认证类型，然后生成授权链接或导入凭证。"><OAuthLoginPanel onChanged={() => void refresh()} /></Dialog>
    <Dialog open={oauthDialog === 'import'} onClose={() => setOAuthDialog(null)} size="md" title="导入 OAuth 凭证"><OAuthImportPanel onChanged={() => void refresh()} /></Dialog>
    <Dialog open={oauthDialog === 'cleanup'} onClose={() => setOAuthDialog(null)} size="md" title="清理 OAuth 凭证"><OAuthCleanupPanel onChanged={() => void refresh()} /></Dialog>
  </>
}
