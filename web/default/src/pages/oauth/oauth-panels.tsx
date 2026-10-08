import { useEffect, useRef, useState } from 'react'
import { SearchableSelect } from '../../components/searchable-select'
import { api, getJSON, postJSON } from '../../lib/api'
import { streamSSE } from '../../lib/sse'

/**
 * OAuth 登录 / 凭证导入 / 凭证清理的共享面板。
 * 渠道页以弹窗打开，/oauth 与 /oauth-jobs 独立页直接渲染，两处共用同一份实现。
 */

type FlowState = { url?: string; state?: string; status?: string; error?: string; channel_id?: number }
const TERMINAL = new Set(['complete', 'completed', 'cancelled', 'error', 'failed'])

const PROVIDERS = [
  { value: 'codex', label: 'Codex' },
  { value: 'antigravity', label: 'Antigravity' },
  { value: 'xai', label: 'xAI' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'zai', label: 'Z.ai' },
  { value: 'cursor', label: 'Cursor' },
  { value: 'zed', label: 'Zed' },
  { value: 'codebuddy', label: 'CodeBuddy' },
]

const METHODS: Record<string, Array<{ value: string; label: string }>> = {
  codex: [{ value: 'oauth', label: '浏览器 OAuth' }, { value: 'pat', label: 'Personal Access Token' }],
  xai: [{ value: 'manual', label: '手动授权' }, { value: 'refresh_token', label: '刷新令牌' }, { value: 'sso', label: 'SSO Cookie' }],
  anthropic: [{ value: 'code', label: '授权码' }],
  zai: [{ value: 'oauth', label: 'ZCode 浏览器登录' }, { value: 'api_key', label: 'Coding Plan API Key' }],
  codebuddy: [{ value: 'oauth', label: '浏览器 OAuth' }, { value: 'file', label: '认证文件' }],
}

// 支持回调提交的提供商；Z.ai 与 CodeBuddy 只轮询状态。
const CALLBACK_PROVIDERS = new Set(['codex', 'antigravity', 'xai', 'zed', 'anthropic'])
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

export function OAuthLoginPanel({ onChanged }: { onChanged?: () => void }) {
  const [provider, setProvider] = useState('codex')
  const [method, setMethod] = useState('oauth')
  const [edition, setEdition] = useState<'domestic' | 'international'>('domestic')
  const [flow, setFlow] = useState<FlowState | null>(null)
  const [callback, setCallback] = useState('')
  const [secret, setSecret] = useState('')
  const [systemID, setSystemID] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [progress, setProgress] = useState<{ processed: number; total: number; created: number; skipped: number; failed: number; errors: string[] } | null>(null)
  const changedRef = useRef(onChanged)
  useEffect(() => { changedRef.current = onChanged }, [onChanged])

  const methods = METHODS[provider]
  const pathProvider = provider === 'codebuddy' && edition === 'international' ? 'codebuddy-international' : provider
  const usesBrowser = !methods || method === 'oauth' || method === 'manual' || method === 'code'

  const reset = () => { setFlow(null); setCallback(''); setSecret(''); setError(''); setNotice(''); setProgress(null) }
  const switchProvider = (value: string) => { setProvider(value); setMethod(METHODS[value]?.[0]?.value ?? 'oauth'); reset() }

  // 轮询 OAuth 状态：网络错误容忍并继续，直到终态。
  useEffect(() => {
    const state = flow?.state
    if (!state || TERMINAL.has(String(flow?.status ?? ''))) return
    let stopped = false
    let timer = 0
    const poll = async () => {
      try {
        const next = await getJSON<FlowState>(`/admin/${pathProvider}/oauth/status`, { state })
        if (stopped) return
        setFlow((current) => ({ ...current, ...next }))
        if (next.status === 'complete' || next.status === 'completed') { setNotice('授权完成，已创建/更新渠道'); changedRef.current?.() }
        if (TERMINAL.has(String(next.status ?? ''))) return
      } catch { /* 网络抖动：继续轮询 */ }
      if (!stopped) timer = window.setTimeout(() => void poll(), 2000)
    }
    timer = window.setTimeout(() => void poll(), 1200)
    return () => { stopped = true; window.clearTimeout(timer) }
  }, [flow?.state, flow?.status, pathProvider])

  const start = async () => {
    setError(''); setNotice(''); setBusy(true)
    try {
      let body: Record<string, unknown> = {}
      if (provider === 'zed') {
        const value = systemID.trim()
        if (value && (!UUID.test(value) || /^0{8}-0{4}-0{4}-0{4}-0{12}$/.test(value))) { setError('System ID 需为有效且非全零的 UUID'); return }
        body = value ? { system_id: value } : {}
      }
      const result = await postJSON<FlowState>(`/admin/${pathProvider}/oauth/start`, body)
      setFlow(result)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '启动授权失败') } finally { setBusy(false) }
  }

  const submitCallback = async () => {
    const value = callback.trim()
    if (!value) return
    setError(''); setBusy(true)
    try {
      if (provider === 'anthropic') {
        // 授权页给出的是 code#state；兼容只粘贴 code。
        const [code, state] = value.split('#')
        await postJSON('/admin/anthropic/oauth/callback', { code: code.trim(), state: (state || flow?.state || '').trim() })
      } else await postJSON(`/admin/${pathProvider}/oauth/callback`, { callback_url: value })
      setCallback('')
    } catch (cause) { setError(cause instanceof Error ? cause.message : '提交回调失败') } finally { setBusy(false) }
  }

  const cancel = async () => {
    if (!flow?.state) return
    try { setFlow(await postJSON<FlowState>(`/admin/${pathProvider}/oauth/cancel`, { state: flow.state })) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '取消失败') }
  }

  // 非浏览器方式：凭证直接导入。输入在提交后立即清空，浏览器不保留。
  const importSecret = async () => {
    const value = secret.trim()
    if (!value) return
    setError(''); setNotice(''); setBusy(true); setSecret('')
    try {
      if (provider === 'codex') {
        if (!value.startsWith('at-')) { setError('Personal Access Token 需以 at- 开头'); return }
        await postJSON('/admin/codex/personal-access-token', { access_token: value })
        setNotice('已导入 Personal Access Token')
      } else if (provider === 'xai') {
        const tally = { processed: 0, total: 0, created: 0, skipped: 0, failed: 0, errors: [] as string[] }
        setProgress({ ...tally })
        await streamSSE<{ event?: string; processed?: number; total?: number; created?: number; skipped?: number; failed?: number; result?: { file_name?: string; status?: string; error?: string } }>('/admin/xai/credentials/import/stream', { method, values: value, priority_increment: 10 }, (event) => {
          Object.assign(tally, { processed: event.processed ?? tally.processed, total: event.total ?? tally.total, created: event.created ?? tally.created, skipped: event.skipped ?? tally.skipped, failed: event.failed ?? tally.failed })
          if (event.result?.error) tally.errors.push(`${event.result.file_name ?? ''} ${event.result.error}`.trim())
          setProgress({ ...tally, errors: [...tally.errors] })
        })
        setNotice(`导入完成：新增 ${tally.created}，跳过 ${tally.skipped}，失败 ${tally.failed}`)
      } else if (provider === 'zai' || provider === 'cursor') {
        await postJSON(`/admin/${provider}/credentials/import`, { api_key: value })
        setNotice('已导入凭证')
      } else if (provider === 'codebuddy') {
        let content: unknown
        try { content = JSON.parse(value) } catch { setError('认证文件内容不是有效 JSON'); return }
        await postJSON(`/admin/${pathProvider}/credentials/import`, content)
        setNotice('已导入 CodeBuddy 凭证')
      }
      changedRef.current?.()
    } catch (cause) { setError(cause instanceof Error ? cause.message : '导入失败') } finally { setBusy(false) }
  }

  const readFile = async (file: File | undefined) => { if (file) setSecret(await file.text()) }

  const secretPlaceholder = provider === 'codex' ? 'at-...' : provider === 'xai' ? '每行一个凭证' : provider === 'codebuddy' ? '认证文件 JSON 内容' : 'API Key'
  const pending = Boolean(flow?.state) && !TERMINAL.has(String(flow?.status ?? ''))

  return <div className="cell-stack">
    <div className="toolbar">
      <SearchableSelect ariaLabel="认证类型" className="combobox-inline" value={provider} options={PROVIDERS} disabled={pending} onChange={switchProvider} />
      {methods && <SearchableSelect ariaLabel="授权方式" className="combobox-inline" value={method} options={methods} disabled={pending} onChange={(value) => { setMethod(value); reset() }} />}
      {provider === 'codebuddy' && <SearchableSelect ariaLabel="CodeBuddy 版本" className="combobox-inline" value={edition} options={[{ value: 'domestic', label: '国内版' }, { value: 'international', label: '国际版' }]} disabled={pending} onChange={(value) => { setEdition(value as typeof edition); reset() }} />}
    </div>

    {provider === 'zed' && usesBrowser && <label className="cell-stack">System ID（可选，试用账户需要）<input className="input" value={systemID} onChange={(event) => setSystemID(event.target.value)} placeholder="00000000-0000-0000-0000-000000000000" /></label>}
    {provider === 'cursor' && <p className="muted">在 Cursor Dashboard 生成 User API Key 后粘贴到下方。</p>}

    {usesBrowser && provider !== 'cursor' ? <>
      <div className="toolbar">
        <button className="btn btn-primary" disabled={busy || pending} onClick={() => void start()}>生成授权链接</button>
        {pending && <button className="btn" onClick={() => void cancel()}>取消</button>}
        {flow?.state && !pending && <button className="btn" onClick={reset}>重新授权</button>}
      </div>
      {flow?.url && <>
        <textarea className="input wide" readOnly rows={3} value={flow.url} />
        <div className="toolbar">
          <button className="btn" onClick={() => void navigator.clipboard?.writeText(String(flow.url))}>复制链接</button>
          <a className="btn" href={flow.url} target="_blank" rel="noopener noreferrer">打开链接</a>
          <span className="muted">状态：{String(flow.status ?? 'pending')}</span>
        </div>
      </>}
      {pending && CALLBACK_PROVIDERS.has(provider) && <div className="toolbar">
        <input className="input wide" value={callback} onChange={(event) => setCallback(event.target.value)} placeholder={provider === 'anthropic' ? '授权码（code#state）' : '浏览器地址栏中的回调 URL（本机无法接收 localhost 回调时使用）'} />
        <button className="btn" disabled={busy || !callback.trim()} onClick={() => void submitCallback()}>提交</button>
      </div>}
    </> : <>
      {provider === 'codebuddy' && <input className="input" type="file" accept=".info,.json,application/json" onChange={(event) => void readFile(event.target.files?.[0])} />}
      <textarea className="input wide advanced-textarea" rows={provider === 'codebuddy' ? 8 : 5} value={secret} onChange={(event) => setSecret(event.target.value)} placeholder={secretPlaceholder} autoComplete="off" spellCheck={false} />
      <p className="muted">提交后输入会立即清除，不会保存在浏览器中。</p>
      <button className="btn btn-primary" disabled={busy || !secret.trim()} onClick={() => void importSecret()}>导入</button>
    </>}

    {progress && <div className="card">
      <div className="job-summary"><span>进度 {progress.processed}/{progress.total || '-'}</span><span className="success-text">新增 {progress.created}</span><span>跳过 {progress.skipped}</span><span className="error-text">失败 {progress.failed}</span></div>
      <progress max={Math.max(progress.total, 1)} value={progress.processed} style={{ width: '100%' }} />
      {progress.errors.length > 0 && <div className="job-events">{progress.errors.map((item, index) => <div key={index}>{item}</div>)}</div>}
    </div>}
    {error && <p className="error-text" role="alert">{error}</p>}
    {notice && <p className="success-text" role="status">{notice}</p>}
    {flow?.error && <p className="error-text">{flow.error}</p>}
  </div>
}

// ---------------------------------------------------------------- 凭证导入

type ImportView = { job_id?: string; status?: string; processed?: number; total?: number; created?: number; skipped?: number; failed?: number; results?: Array<{ file_name?: string; channel_name?: string; status?: string; error?: string }>; next?: number; error?: string }

export function OAuthImportPanel({ onChanged }: { onChanged?: () => void }) {
  const [files, setFiles] = useState<File[]>([])
  const [provider, setProvider] = useState('auto')
  const [increment, setIncrement] = useState('10')
  const [job, setJob] = useState<ImportView | null>(null)
  const [results, setResults] = useState<NonNullable<ImportView['results']>>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const cursor = useRef(0)
  const changedRef = useRef(onChanged)
  useEffect(() => { changedRef.current = onChanged }, [onChanged])

  const start = async () => {
    if (!files.length) return
    setBusy(true); setError(''); setResults([]); cursor.current = 0
    try {
      // provider / priority_increment 必须先于文件写入 multipart。
      const body = new FormData()
      body.append('provider', provider)
      body.append('priority_increment', increment)
      files.forEach((file) => body.append('files', file, file.name))
      const response = await api.post('/admin/oauth/credentials/import/jobs', body)
      setJob((response.data?.data ?? response.data) as ImportView)
    } catch (cause) { setError(cause instanceof Error ? cause.message : '启动导入任务失败') } finally { setBusy(false) }
  }

  // 后端返回 results + next 游标（admin_oauth_import_jobs.go:oauthCredentialImportJobView）。
  useEffect(() => {
    const id = job?.job_id
    if (!id || (job?.status && job.status !== 'running')) return
    let stopped = false
    let timer = 0
    let delay = 1200
    const poll = async () => {
      try {
        const next = await getJSON<ImportView>(`/admin/oauth/credentials/import/jobs/${encodeURIComponent(id)}`, { after: cursor.current })
        if (stopped) return
        const batch = next.results ?? []
        if (batch.length) setResults((current) => [...current, ...batch])
        cursor.current = Number(next.next ?? cursor.current + batch.length)
        setJob(next)
        delay = 1200
        if (next.status && next.status !== 'running') { changedRef.current?.(); return }
      } catch { delay = Math.min(delay * 2, 10000) }
      if (!stopped) timer = window.setTimeout(() => void poll(), delay)
    }
    timer = window.setTimeout(() => void poll(), 300)
    return () => { stopped = true; window.clearTimeout(timer) }
  }, [job?.job_id, job?.status])

  const failures = results.filter((item) => item.error)
  return <div className="cell-stack">
    <p className="muted">接受 JSON / TXT / ZIP / tar.gz，可多选；支持 CLIProxyAPI 与 Sub2API 聚合凭证。</p>
    <div className="toolbar">
      <SearchableSelect ariaLabel="凭证类型" className="combobox-inline" value={provider} options={[{ value: 'auto', label: '自动识别' }, { value: 'codebuddy', label: 'CodeBuddy' }, { value: 'codex', label: 'Codex' }, { value: 'antigravity', label: 'Antigravity' }, { value: 'xai', label: 'xAI' }, { value: 'anthropic', label: 'Anthropic' }]} onChange={setProvider} />
      <SearchableSelect ariaLabel="优先级递增" className="combobox-inline" value={increment} options={['0', '10', '20', '50'].map((value) => ({ value, label: `优先级递增 ${value}` }))} onChange={setIncrement} />
    </div>
    <input className="input" type="file" multiple accept=".json,.txt,.zip,.tar.gz,.tgz,application/json" onChange={(event) => setFiles(Array.from(event.target.files ?? []))} />
    <button className="btn btn-primary" disabled={busy || !files.length || (Boolean(job?.job_id) && job?.status === 'running')} onClick={() => void start()}>开始导入 {files.length ? `(${files.length} 个文件)` : ''}</button>
    {job && <div className="card">
      <div className="job-summary"><span>状态：{String(job.status ?? 'running')}</span><span>进度 {Number(job.processed ?? 0)}/{Number(job.total ?? 0) || '-'}</span><span className="success-text">新增 {Number(job.created ?? 0)}</span><span>跳过 {Number(job.skipped ?? 0)}</span><span className="error-text">失败 {Number(job.failed ?? 0)}</span></div>
      <progress max={Math.max(Number(job.total ?? 0), 1)} value={Number(job.processed ?? 0)} style={{ width: '100%' }} />
      {failures.length > 0 && <div className="job-events">{failures.map((item, index) => <div key={index}>{item.file_name} — {item.error}</div>)}</div>}
      {job.error && <p className="error-text">{job.error}</p>}
    </div>}
    {error && <p className="error-text" role="alert">{error}</p>}
  </div>
}

// ---------------------------------------------------------------- 凭证清理

type CleanupEvent = { event?: string; processed?: number; total?: number; healthy?: number; refreshed?: number; disabled?: number; deleted?: number; failed?: number; skipped?: number; channel_name?: string; status?: string; error?: string; message?: string }

const CLEANUP_TYPES = [
  { value: 'codex_oauth', label: 'Codex' }, { value: 'antigravity_oauth', label: 'Antigravity' }, { value: 'xai_oauth', label: 'xAI' },
  { value: 'anthropic_oauth', label: 'Anthropic' }, { value: 'zai_oauth', label: 'Z.ai' }, { value: 'cursor_oauth', label: 'Cursor' },
  { value: 'zed_oauth', label: 'Zed' }, { value: 'codebuddy_oauth', label: 'CodeBuddy' },
]

export function OAuthCleanupPanel({ onChanged }: { onChanged?: () => void }) {
  const [authType, setAuthType] = useState('codex_oauth')
  const [models, setModels] = useState<string[]>([])
  const [channelCount, setChannelCount] = useState(0)
  const [model, setModel] = useState('')
  const [action, setAction] = useState<'disable' | 'delete'>('disable')
  const [jobID, setJobID] = useState('')
  const [phase, setPhase] = useState<'idle' | 'running' | 'stopping'>('idle')
  const [summary, setSummary] = useState<CleanupEvent | null>(null)
  const [rows, setRows] = useState<CleanupEvent[]>([])
  const [error, setError] = useState('')
  const abortRef = useRef<AbortController | null>(null)

  // 切换类型自动重载可选模型。
  useEffect(() => {
    let cancelled = false
    setModels([]); setModel('')
    getJSON<{ models?: string[]; channel_count?: number }>('/admin/oauth/credentials/cleanup/options', { auth_type: authType })
      .then((data) => { if (cancelled) return; setModels(data.models ?? []); setChannelCount(Number(data.channel_count ?? 0)); setModel(data.models?.[0] ?? '') })
      .catch((cause: unknown) => { if (!cancelled) setError(cause instanceof Error ? cause.message : '加载选项失败') })
    return () => { cancelled = true }
  }, [authType])

  // 关闭面板只断开进度订阅，不中断服务端任务。
  useEffect(() => () => abortRef.current?.abort(), [])

  const start = async () => {
    if (!model) return
    const message = action === 'delete' ? `将检测全部 ${channelCount} 个凭证，并删除失效渠道，此操作不可撤销。确定继续吗？` : `将检测全部 ${channelCount} 个凭证，并禁用失效渠道。确定继续吗？`
    if (!window.confirm(message)) return
    setError(''); setRows([]); setSummary(null); setPhase('running')
    try {
      const response = await api.post('/admin/oauth/credentials/cleanup/jobs', { auth_type: authType, model, action }, { headers: { 'Idempotency-Key': crypto.randomUUID() } })
      const started = (response.data?.data ?? response.data) as { job_id?: string }
      if (!started.job_id) throw new Error('未返回任务 ID')
      setJobID(started.job_id)
      const controller = new AbortController(); abortRef.current = controller
      await streamSSE<CleanupEvent>(`/admin/oauth/credentials/cleanup/jobs/${encodeURIComponent(started.job_id)}/stream?after=0`, undefined, (event) => {
        setSummary((current) => ({ ...current, ...event }))
        if (event.channel_name || event.error) setRows((current) => [...current.slice(-199), event])
      }, controller.signal, 'GET')
      onChanged?.()
    } catch (cause) {
      if (!(cause instanceof DOMException && cause.name === 'AbortError')) setError(cause instanceof Error ? cause.message : '清理任务失败')
    } finally { setPhase('idle') }
  }

  const cancel = async () => {
    if (!jobID) return
    setPhase('stopping')
    try { await postJSON(`/admin/oauth/credentials/cleanup/jobs/${encodeURIComponent(jobID)}/cancel`) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '停止失败'); setPhase('running') }
  }

  return <div className="cell-stack">
    <div className="toolbar">
      <SearchableSelect ariaLabel="凭证类型" className="combobox-inline" value={authType} disabled={phase !== 'idle'} options={CLEANUP_TYPES} onChange={setAuthType} />
      <SearchableSelect ariaLabel="检测模型" className="combobox-inline" value={model} disabled={phase !== 'idle' || !models.length} options={models.map((item) => ({ value: item, label: item }))} onChange={setModel} placeholder="检测模型" />
      <SearchableSelect ariaLabel="失效处理" className="combobox-inline" value={action} disabled={phase !== 'idle'} options={[{ value: 'disable', label: '禁用失效渠道' }, { value: 'delete', label: '删除失效渠道' }]} onChange={(value) => setAction(value as typeof action)} />
    </div>
    <p className="muted">共 {channelCount} 个渠道。任务在服务端运行，关闭窗口不会中断。</p>
    <div className="toolbar">
      <button className="btn btn-primary" disabled={phase !== 'idle' || !model} onClick={() => void start()}>开始清理</button>
      <button className="btn" disabled={phase !== 'running'} onClick={() => void cancel()}>{phase === 'stopping' ? '停止中…' : '停止'}</button>
    </div>
    {summary && <div className="card">
      <div className="job-summary">
        <span>进度 {Number(summary.processed ?? 0)}/{Number(summary.total ?? 0) || '-'}</span>
        <span className="success-text">健康 {Number(summary.healthy ?? 0)}</span><span>已刷新 {Number(summary.refreshed ?? 0)}</span>
        <span>已禁用 {Number(summary.disabled ?? 0)}</span><span>已删除 {Number(summary.deleted ?? 0)}</span>
        <span className="error-text">失败 {Number(summary.failed ?? 0)}</span><span>跳过 {Number(summary.skipped ?? 0)}</span>
      </div>
      <progress max={Math.max(Number(summary.total ?? 0), 1)} value={Number(summary.processed ?? 0)} style={{ width: '100%' }} />
      {rows.length > 0 && <div className="job-events">{rows.map((row, index) => <div key={index}>{row.channel_name ?? ''} {row.status ?? row.event ?? ''} {row.error ?? row.message ?? ''}</div>)}</div>}
    </div>}
    {error && <p className="error-text" role="alert">{error}</p>}
  </div>
}
