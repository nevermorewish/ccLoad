import { useEffect, useRef, useState } from 'react'
import { CheckCircle2, XCircle, LoaderCircle, Circle, Download, Play, Square } from 'lucide-react'
import { getJSON } from '../lib/api'
import { getToken } from '../lib/auth'
import type { Channel } from '../types'
import { finishKimiResults, readKimiEvents, type KimiCase, type KimiEvent, type KimiMeta, type KimiResult } from './kimi-test-state'

function Status({ value }: { value: string }) {
  const Icon = ['passed', 'completed'].includes(value) ? CheckCircle2 : ['failed', 'interrupted'].includes(value) ? XCircle : value === 'running' ? LoaderCircle : Circle
  return <span className={`kimi-status kimi-${value}`}><Icon size={13} aria-hidden="true" />{value}</span>
}

export function KimiTestPanel({ channel, model, keyIndex, onBusy }: { channel: Channel | null; model: string; keyIndex: string; onBusy: (busy: boolean) => void }) {
  const [catalog, setCatalog] = useState<KimiCase[]>([])
  const [version, setVersion] = useState('')
  const [selected, setSelected] = useState<string[]>([])
  const [results, setResults] = useState<Record<string, KimiResult>>({})
  const [events, setEvents] = useState<KimiEvent[]>([])
  const [meta, setMeta] = useState<KimiMeta | null>(null)
  const [state, setState] = useState('idle')
  const [error, setError] = useState('')
  const [expanded, setExpanded] = useState<string | null>(null)
  const [options, setOptions] = useState({ concurrency: 6, timeout_seconds: 180, upload_mib: 20, upload_limit_seconds: 60, image_url: `${window.location.origin}/public/model-test/kimi/blue.png` })
  const controller = useRef<AbortController | null>(null)
  const eventEnd = useRef<HTMLDivElement | null>(null)
  const busy = state === 'running'
  useEffect(() => {
    let active = true
    void getJSON<{ version: string; cases: KimiCase[] }>('/admin/model-test/kimi/cases').then((data) => {
      if (active) { setCatalog(data.cases); setSelected(data.cases.map((item) => item.id)); setVersion(data.version) }
    }).catch((cause: unknown) => { if (active) setError(cause instanceof Error ? cause.message : '加载用例失败') })
    return () => { active = false; controller.current?.abort() }
  }, [])
  useEffect(() => { const list = eventEnd.current?.parentElement; if (list) list.scrollTop = list.scrollHeight }, [events])

  const run = async (ids: string[]) => {
    if (!channel || !model || !ids.length || controller.current) return
    const abort = new AbortController(); controller.current = abort
    setError(''); setEvents([]); setMeta({ channel_id: channel.id, channel: channel.name, model, version, total: ids.length, ...options }); setExpanded(null)
    setResults(Object.fromEntries(ids.map((id) => [id, { id, status: 'queued' }])))
    setState('running'); onBusy(true)
    try {
      const token = getToken()
      const response = await fetch(`/admin/channels/${channel.id}/kimi-test`, { method: 'POST', credentials: 'include', signal: abort.signal, headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream', ...(token ? { Authorization: `Bearer ${token}` } : {}) }, body: JSON.stringify({ ...options, model, case_ids: ids, ...(keyIndex === '' ? {} : { key_index: Number(keyIndex) }) }) })
      if (!response.ok || !response.body) {
        const detail = await response.json().catch(() => ({})) as { error?: string; message?: string }
        throw new Error(detail.error || detail.message || `请求失败：HTTP ${response.status}`)
      }
      await readKimiEvents(response.body, (event) => {
        setEvents((items) => [...items, event])
        if (event.type === 'queued' && event.meta) setMeta(event.meta)
        if (event.type === 'case_started' && event.id) setResults((items) => ({ ...items, [event.id!]: { id: event.id!, status: 'running' } }))
        if (event.result) setResults((items) => ({ ...items, [event.result!.id]: event.result! }))
        if (event.type === 'completed') setMeta((value) => ({ ...value, ...event.meta }))
      })
      setState('completed')
    } catch (cause) {
      const status = abort.signal.aborted ? 'cancelled' : 'interrupted'
      const message = abort.signal.aborted ? '测试已取消' : cause instanceof Error ? cause.message : '测试失败'
      setState(status); setError(abort.signal.aborted ? '' : message)
      setResults((items) => finishKimiResults(items, status))
      setEvents((items) => [...items, { type: status, time: new Date().toISOString(), message }])
    } finally { controller.current = null; onBusy(false) }
  }
  const values = Object.values(results)
  const passed = values.filter((result) => result.status === 'passed').length
  const failed = values.filter((result) => result.status === 'failed').map((result) => result.id)
  const verdict = state === 'completed' ? meta?.verdict ?? 'unknown' : state === 'idle' ? 'pending' : state === 'running' ? 'running' : 'incomplete'
  const sameTarget = meta?.channel_id === channel?.id && meta?.model === model
  const exportReport = () => {
    const blob = new Blob([JSON.stringify({ suite: version, exported_at: new Date().toISOString(), state, verdict, meta, cases: catalog.filter((item) => results[item.id]), results, events }, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob); const a = document.createElement('a'); a.href = url; a.download = `kimi-k3-${new Date().toISOString().replace(/[:.]/g, '-')}.json`; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  return <section className="kimi-panel" aria-label="Kimi K3 全功能性测试">
    <div className="kimi-intro"><div><h2>Kimi K3 全功能性测试</h2><p>API 兼容性测试 · 已确认的 12 项 · OpenAI Chat 原生请求</p></div><div className="toolbar">
      {busy ? <button className="btn" onClick={() => controller.current?.abort()}><Square size={14} /> 停止测试</button> : <button className="btn btn-primary" disabled={!channel || !model || !selected.length || (!!channel.auth_type && channel.auth_type !== 'api_key')} onClick={() => void run(selected)}><Play size={14} /> 开始测试（{selected.length}）</button>}
      <button className="btn" disabled={busy || !failed.length || !sameTarget} onClick={() => void run(failed)}>重测失败项</button>
      <button className="btn" disabled={busy || !values.length} onClick={exportReport}><Download size={14} /> 导出报告</button>
    </div></div>
    {channel?.auth_type && channel.auth_type !== 'api_key' && <p role="alert">请选择使用 API Key 的 Kimi 渠道。</p>}
    <details className="kimi-options"><summary>测试设置与素材说明</summary><fieldset disabled={busy}>
      <div className="kimi-option-grid">{([
        ['concurrency', '并发数', 1, 6], ['timeout_seconds', '单项超时（秒）', 10, 600], ['upload_mib', '上传 Base64 大小（MiB）', 1, 32], ['upload_limit_seconds', '上传阈值（秒）', 1, 600],
      ] as const).map(([key, label, min, max]) => <label key={key}>{label}<input className="input" type="number" min={min} max={max} value={options[key]} onChange={(e) => setOptions((value) => ({ ...value, [key]: Number(e.target.value) }))} /></label>)}</div>
      <label>蓝色测试图片 URL<input className="input" type="url" value={options.image_url} onChange={(e) => setOptions((value) => ({ ...value, image_url: e.target.value }))} /></label>
      <p>图片 URL 必须可被上游公开访问；本机地址通常不可访问，可替换为公网蓝色图片地址。Base64 图片和视频使用随程序附带的固定素材。上传用例通过 MP4 free 区块扩充大小，单独测量连接建立后至请求发送完成的时间。</p>
      <p>按所附报告重建测试名称与已知断言，原报告未提供完整请求和素材。套件版本：{version || '加载中'}。使用所选渠道的首个 OpenAI URL、模型映射和 Key；不应用请求体改写，结果反映上游原生兼容性。每项发起一次真实请求，会产生正常 API 用量。运行绑定当前连接，刷新页面会中止。</p>
    </fieldset></details>
    {error && <div className="kimi-error" role="alert">{error}</div>}
    <div className="kimi-summary" aria-live="polite">
      <div><span>任务状态</span><Status value={state} /></div><div><span>兼容结论</span><Status value={verdict} /></div>
      <div><span>用例</span><strong>{meta?.total ?? catalog.length}</strong></div><div><span>通过</span><strong className="kimi-pass-number">{passed}</strong></div><div><span>失败</span><strong className="kimi-fail-number">{failed.length}</strong></div>
    </div>
    <div className="kimi-metadata"><div><span>运行类型</span><strong>测试套件运行</strong></div><div><span>渠道 / 模型</span><strong>{meta?.channel ?? channel?.name ?? '未选择渠道'} · {meta?.model ?? (model || '未选择模型')}</strong>{meta?.upstream_model && meta.upstream_model !== meta.model && <small>上游模型：{meta.upstream_model}</small>}</div><div><span>完整 API URL</span><strong>{meta?.url ?? '开始测试后显示实际请求地址'}</strong></div></div>
    <div className="kimi-columns">
      <section className="kimi-cases"><div className="kimi-section-title"><div><h3>用例与步骤</h3><p>断言失败会停止当前用例剩余步骤，不影响其他用例。</p></div><label><input type="checkbox" disabled={busy || !catalog.length} checked={catalog.length > 0 && selected.length === catalog.length} onChange={(e) => setSelected(e.target.checked ? catalog.map((item) => item.id) : [])} /> 全选</label></div>
        {!catalog.length && <p>正在加载测试用例…</p>}
        <div className="kimi-case-list">{catalog.map((item) => {
          const result = results[item.id]; const open = expanded === item.id
          return <article className="kimi-case" key={item.id}>
            <div className="kimi-case-heading"><input type="checkbox" aria-label={`选择 ${item.name}`} disabled={busy} checked={selected.includes(item.id)} onChange={(e) => setSelected((ids) => e.target.checked ? [...ids, item.id] : ids.filter((id) => id !== item.id))} />
              <button className="kimi-case-toggle" aria-expanded={open} aria-controls={`detail-${item.id}`} onClick={() => setExpanded(open ? null : item.id)}><strong>{item.name}</strong><small>{version} · {item.id}</small></button><Status value={result?.status ?? 'pending'} /></div>
            {open && <div id={`detail-${item.id}`} className="kimi-case-detail"><p><b>预期：</b>{item.expected}</p><p><b>实际：</b>{result?.actual ?? '尚无结果'}</p>{result?.duration_ms != null && <p>总耗时 {(result.duration_ms / 1000).toFixed(2)}s · HTTP {result.http_status || '—'} · 请求 {((result.request_bytes ?? 0) / 1048576).toFixed(3)} MiB · 上传 {result.upload_ms != null && result.upload_ms >= 0 ? `${result.upload_ms.toFixed(2)}ms` : '未完成'}</p>}{result?.request != null && <><h4>请求体（Base64 已折叠）</h4><pre>{JSON.stringify(result.request, null, 2)}</pre></>}{result?.response && <><h4>实际响应（最多 32 KiB）</h4><pre>{result.response}</pre></>}</div>}
          </article>
        })}</div>
      </section>
      <section className="kimi-events"><h3>运行事件</h3><p>队列、用例与终态进度。</p><div className="kimi-event-list" role="log" aria-label="运行事件">{!events.length && <div className="kimi-empty">选择渠道与模型后开始测试<br /><small>事件将在运行时实时显示</small></div>}{events.map((event, index) => <div className="kimi-event" key={index}><time dateTime={event.time}>{new Date(event.time).toLocaleString('zh-CN', { hour12: false })}</time><span>{event.type}</span><div>{event.message}</div></div>)}<div ref={eventEnd} /></div></section>
    </div>
  </section>
}
