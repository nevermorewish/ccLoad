import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api, getJSON, postJSON } from '../lib/api'

type Job = { job_id?: string; status?: string; total?: number; processed?: number; succeeded?: number; failed?: number; skipped?: number; cancelled?: boolean; error?: string; [key: string]: unknown }
type EventRow = { event?: string; status?: string; processed?: number; total?: number; succeeded?: number; failed?: number; skipped?: number; message?: string; error?: string; model?: string; channel_id?: number; [key: string]: unknown }

const terminal = new Set(['completed', 'complete', 'failed', 'cancelled', 'error'])
const parseEvent = (value: string): EventRow | null => {
  const data = value.split(/\r?\n/).find((line) => line.startsWith('data:'))?.slice(5).trim()
  if (!data || data === '[DONE]') return null
  try { return JSON.parse(data) as EventRow } catch { return { event: 'message', message: data } }
}

export function OAuthJobsPage() {
  const [authType, setAuthType] = useState('codex_oauth')
  const [files, setFiles] = useState<FileList | null>(null)
  const [importJob, setImportJob] = useState<Job | null>(null)
  const [importEvents, setImportEvents] = useState<EventRow[]>([])
  const [importBusy, setImportBusy] = useState(false)
  const [model, setModel] = useState('')
  const [action, setAction] = useState<'disable' | 'delete'>('disable')
  const [cleanupJob, setCleanupJob] = useState<Job | null>(null)
  const [cleanupEvents, setCleanupEvents] = useState<EventRow[]>([])
  const [cleanupOptions, setCleanupOptions] = useState<string[]>([])
  const [cleanupBusy, setCleanupBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const importCursor = useRef(0)
  const cleanupCursor = useRef(0)

  const loadOptions = useCallback(async () => {
    try {
      const data = await getJSON<{ models?: string[] }>('/admin/oauth/credentials/cleanup/options', { auth_type: authType })
      const models = (data.models ?? []).map(String).filter(Boolean)
      setCleanupOptions(models)
      setModel((current) => models.includes(current) ? current : models[0] ?? '')
    } catch (cause) { setError(cause instanceof Error ? cause.message : '加载可清理模型失败') }
  }, [authType])

  const startImport = async () => {
    if (!files?.length || importBusy) return
    setImportBusy(true); setError(null); setImportEvents([]); importCursor.current = 0
    try {
      const body = new FormData(); Array.from(files).forEach((file) => body.append('files', file))
      const response = await api.post('/admin/oauth/credentials/import/jobs', body)
      setImportJob((response.data?.data ?? response.data) as Job)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '启动导入任务失败') }
    finally { setImportBusy(false) }
  }

  useEffect(() => {
    const id = importJob?.job_id
    if (!id || terminal.has(String(importJob?.status ?? '').toLowerCase())) return
    let stopped = false
    const poll = async () => {
      try {
        const next = await getJSON<Job>(`/admin/oauth/credentials/import/jobs/${encodeURIComponent(id)}`, { after: importCursor.current })
        if (stopped) return
        const events = Array.isArray(next.events) ? next.events as EventRow[] : []
        importCursor.current += events.length
        if (events.length) setImportEvents((current) => [...current, ...events])
        setImportJob(next)
        if (!terminal.has(String(next.status ?? '').toLowerCase())) window.setTimeout(poll, 1200)
      } catch (cause) {
        if (!stopped) { setError(cause instanceof Error ? cause.message : '导入任务连接失败'); window.setTimeout(poll, 2500) }
      }
    }
    void poll()
    return () => { stopped = true }
  }, [importJob?.job_id, importJob?.status])

  const startCleanup = async () => {
    if (!model || cleanupBusy) return
    if (action === 'delete' && !window.confirm(`确定删除 ${authType} / ${model} 的凭证吗？此操作不可撤销。`)) return
    setCleanupBusy(true); setError(null); setCleanupEvents([]); cleanupCursor.current = 0
    try { setCleanupJob(await postJSON<Job>('/admin/oauth/credentials/cleanup/jobs', { auth_type: authType, model, action })) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '启动清理任务失败') }
    finally { setCleanupBusy(false) }
  }
  const cancelCleanup = async () => {
    const id = cleanupJob?.job_id
    if (!id) return
    try { setCleanupJob(await postJSON<Job>(`/admin/oauth/credentials/cleanup/jobs/${encodeURIComponent(id)}/cancel`)) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '取消清理失败') }
  }

  useEffect(() => {
    const id = cleanupJob?.job_id
    if (!id || terminal.has(String(cleanupJob?.status ?? '').toLowerCase())) return
    let stopped = false
    const poll = async () => {
      try {
        const response = await api.get(`/admin/oauth/credentials/cleanup/jobs/${encodeURIComponent(id)}/stream`, { params: { after: cleanupCursor.current }, headers: { Accept: 'text/event-stream' }, responseType: 'text' })
        if (stopped) return
        const chunks = String(response.data ?? '').split(/\r?\n\r?\n+/).filter(Boolean)
        const parsed = chunks.map(parseEvent).filter((item): item is EventRow => Boolean(item))
        cleanupCursor.current += parsed.length
        if (parsed.length) {
          setCleanupEvents((current) => [...current, ...parsed])
          const last = parsed[parsed.length - 1]
          if (last.event && terminal.has(last.event.toLowerCase())) setCleanupJob((current) => current ? { ...current, status: last.event, ...last } : current)
        }
        if (!parsed.some((item) => item.event && terminal.has(item.event.toLowerCase()))) window.setTimeout(poll, 1200)
      } catch (cause) {
        if (!stopped) { setError(cause instanceof Error ? cause.message : '清理任务连接失败'); window.setTimeout(poll, 2500) }
      }
    }
    void poll()
    return () => { stopped = true }
  }, [cleanupJob?.job_id, cleanupJob?.status])

  const importSummary = useMemo(() => summarize(importJob, importEvents), [importJob, importEvents])
  const cleanupSummary = useMemo(() => summarize(cleanupJob, cleanupEvents), [cleanupJob, cleanupEvents])
  return <>
    <header className="page-header"><div><h1>OAuth 批量任务</h1><p className="muted">批量导入凭证，或按认证类型和模型清理旧凭证</p></div></header>
    {error && <div className="card error-text">{error}</div>}
    <section className="card"><h2>批量导入凭证</h2><div className="toolbar"><input className="input" type="file" multiple accept=".json,.txt,.zip" onChange={(event) => setFiles(event.target.files)} /><button className="btn btn-primary" disabled={!files?.length || importBusy} onClick={() => void startImport()}>{importBusy ? '启动中...' : '开始导入'}</button></div>{importJob && <JobSummary job={importJob} summary={importSummary} />}{importEvents.length > 0 && <EventList events={importEvents} />}</section>
    <section className="card"><h2>凭证清理</h2><div className="toolbar"><select className="select" value={authType} disabled={cleanupBusy} onChange={(event) => { setAuthType(event.target.value); setCleanupJob(null); setCleanupEvents([]) }}><option value="codex_oauth">Codex</option><option value="anthropic_oauth">Anthropic</option><option value="antigravity_oauth">Antigravity</option><option value="xai_oauth">xAI</option><option value="codebuddy_oauth">CodeBuddy</option><option value="zai_oauth">Z.ai</option><option value="zed_oauth">Zed</option></select><button className="btn" disabled={cleanupBusy} onClick={() => void loadOptions()}>加载模型</button><select className="select" value={model} disabled={cleanupBusy || cleanupOptions.length === 0} onChange={(event) => setModel(event.target.value)}><option value="">选择模型</option>{cleanupOptions.map((value) => <option key={value} value={value}>{value}</option>)}</select><select className="select" value={action} disabled={cleanupBusy} onChange={(event) => setAction(event.target.value as typeof action)}><option value="disable">停用</option><option value="delete">删除</option></select><button className="btn btn-primary" disabled={!model || cleanupBusy} onClick={() => void startCleanup()}>{cleanupBusy ? '启动中...' : action === 'delete' ? '删除凭证' : '停用凭证'}</button><button className="btn" disabled={!cleanupJob?.job_id || terminal.has(String(cleanupJob.status ?? '').toLowerCase())} onClick={() => void cancelCleanup()}>取消</button></div>{cleanupJob && <JobSummary job={cleanupJob} summary={cleanupSummary} />}{cleanupEvents.length > 0 && <EventList events={cleanupEvents} />}</section>
  </>
}

function summarize(job: Job | null, events: EventRow[]) { const last = events[events.length - 1]; return { processed: Number(last?.processed ?? job?.processed ?? 0), total: Number(last?.total ?? job?.total ?? 0), succeeded: Number(last?.succeeded ?? job?.succeeded ?? 0), failed: Number(last?.failed ?? job?.failed ?? 0), skipped: Number(last?.skipped ?? job?.skipped ?? 0) } }
function JobSummary({ job, summary }: { job: Job; summary: ReturnType<typeof summarize> }) { return <div className="job-summary"><span>状态：{String(job.status ?? '运行中')}</span><span>进度：{summary.processed}/{summary.total || '-'}</span><span className="success-text">成功：{summary.succeeded}</span><span className="error-text">失败：{summary.failed}</span><span>跳过：{summary.skipped}</span></div> }
function EventList({ events }: { events: EventRow[] }) { return <div className="job-events">{events.slice(-100).map((event, index) => <div key={`${String(event.event)}-${index}`}><strong>{String(event.event ?? 'progress')}</strong> {event.model ? `${event.model} ` : ''}{event.message || event.error || `进度 ${event.processed ?? 0}/${event.total ?? '-'}`}</div>)}</div> }
