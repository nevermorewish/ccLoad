import { useEffect, useMemo, useState } from 'react'
import { Dialog } from '../../components/dialog'
import { SearchableSelect } from '../../components/searchable-select'
import { getJSON, postJSON } from '../../lib/api'
import type { Channel } from '../../types'
import { loadKeys, testChannel, type ChannelKeyRow } from './api'

type TestResult = Record<string, unknown>
type BatchRow = { keyIndex: number; masked: string; success: boolean; message: string }

const CLIENT_PROTOCOLS = [
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'openai', label: 'OpenAI Chat' },
  { value: 'codex', label: 'Codex Responses' },
  { value: 'gemini', label: 'Gemini' },
]

const modelNames = (channel: Channel | null) => (Array.isArray(channel?.models) ? channel.models : [])
  .map((entry) => typeof entry === 'string' ? entry : String((entry as { model?: unknown }).model ?? ''))
  .filter(Boolean)

const mask = (value: string) => value.length <= 10 ? '****' : `${value.slice(0, 6)}…${value.slice(-4)}`

/** 渠道测试：单测、按 Key 批量测试（前端分批并发）、上游详情与合并视图。 */
export function TestDialog({ channel, initialModel, onClose }: { channel: Channel | null; initialModel?: string; onClose: () => void }) {
  const models = useMemo(() => modelNames(channel), [channel])
  const [model, setModel] = useState('')
  const [protocol, setProtocol] = useState('anthropic')
  const [keyIndex, setKeyIndex] = useState('')
  const [content, setContent] = useState('test')
  const [stream, setStream] = useState(true)
  const [concurrency, setConcurrency] = useState(10)
  const [keys, setKeys] = useState<ChannelKeyRow[]>([])
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<TestResult | null>(null)
  const [batch, setBatch] = useState<{ done: number; total: number; rows: BatchRow[] } | null>(null)
  const [detailOpen, setDetailOpen] = useState(false)

  useEffect(() => {
    if (!channel) return
    setModel(initialModel || models[0] || ''); setKeyIndex(''); setResult(null); setBatch(null); setDetailOpen(false)
    void loadKeys(channel.id).then(setKeys).catch(() => setKeys([]))
    // 默认测试内容取设置的第一段（以 | 分隔），与旧页一致。
    void getJSON<{ value?: string }>('/admin/settings/channel_test_content').then((setting) => {
      const first = String(setting?.value ?? '').split('|').map((item) => item.trim()).find(Boolean)
      if (first) setContent(first)
    }).catch(() => undefined)
  }, [channel, initialModel, models])

  const body = (index?: number) => ({ model, content, stream, client_protocol: protocol, ...(index == null ? {} : { key_index: index }) })

  const runSingle = async () => {
    if (!channel || !model) return
    setBusy(true); setResult(null)
    try { setResult(await testChannel(channel.id, body(keyIndex === '' ? undefined : Number(keyIndex))) as TestResult) }
    catch (cause) { setResult({ success: false, error: cause instanceof Error ? cause.message : '测试失败' }) }
    finally { setBusy(false) }
  }

  // 前端按并发数分批调用同一 /test 端点，失败 Key 由服务端自动冷却。
  const runBatch = async () => {
    if (!channel || !model || !keys.length) return
    setBusy(true); setResult(null)
    const rows: BatchRow[] = []
    setBatch({ done: 0, total: keys.length, rows })
    try {
      const limit = Math.min(50, Math.max(1, concurrency))
      for (let start = 0; start < keys.length; start += limit) {
        const slice = keys.slice(start, start + limit).map((key, offset) => ({ key, index: start + offset }))
        const settled = await Promise.all(slice.map(async ({ key, index }) => {
          try {
            const response = await testChannel(channel.id, body(index)) as TestResult
            return { keyIndex: index, masked: mask(key.api_key), success: response.success !== false, message: String(response.message ?? response.error ?? '') }
          } catch (cause) {
            return { keyIndex: index, masked: mask(key.api_key), success: false, message: cause instanceof Error ? cause.message : '测试失败' }
          }
        }))
        rows.push(...settled)
        setBatch({ done: rows.length, total: keys.length, rows: [...rows] })
      }
    } finally { setBusy(false) }
  }

  const failed = batch?.rows.filter((row) => !row.success) ?? []
  const batchVerdict = !batch || batch.done < batch.total ? '' : failed.length === 0 ? '全部成功' : failed.length === batch.total ? '全部失败' : `部分成功（失败 ${failed.length}）`
  const hasUpstream = Boolean(result?.upstream_request_url || result?.upstream_response_body)

  return (
    <Dialog open={Boolean(channel)} onClose={onClose} size="lg" title={`测试渠道：${channel?.name ?? ''}`}
      footer={<>
        <button className="btn" onClick={onClose}>关闭</button>
        {keys.length > 1 && <button className="btn" disabled={busy || !model} onClick={() => void runBatch()}>批量测试全部 Key</button>}
        <button className="btn btn-primary" disabled={busy || !model} onClick={() => void runSingle()}>{busy ? '测试中…' : '开始测试'}</button>
      </>}>
      <div className="toolbar">
        <SearchableSelect ariaLabel="测试模型" className="combobox-inline" allowCustomInput value={model} options={models.map((item) => ({ value: item, label: item }))} onChange={setModel} placeholder="模型" />
        <SearchableSelect ariaLabel="客户端协议" className="combobox-inline" value={protocol} options={CLIENT_PROTOCOLS} onChange={setProtocol} />
        {/* 单测 Key 只列前 10 个，与旧页一致；更多 Key 用批量测试。 */}
        <SearchableSelect ariaLabel="测试 Key" className="combobox-inline" value={keyIndex} options={[{ value: '', label: '自动选择 Key' }, ...keys.slice(0, 10).map((key, index) => ({ value: String(index), label: `#${index + 1} ${mask(key.api_key)}` }))]} onChange={setKeyIndex} />
        <label><input type="checkbox" checked={stream} onChange={(event) => setStream(event.target.checked)} /> 流式</label>
        {keys.length > 1 && <label className="muted">并发<input className="input compact" type="number" min={1} max={50} value={concurrency} onChange={(event) => setConcurrency(Number(event.target.value))} /></label>}
      </div>
      {keys.length > 10 && <p className="muted">还有 {keys.length - 10} 个 Key 未在单测列表中显示，可使用批量测试。</p>}
      <textarea className="input wide test-content" value={content} onChange={(event) => setContent(event.target.value)} placeholder="测试内容" />

      {batch && <div className="card">
        <div className="job-summary"><span>进度 {batch.done}/{batch.total}</span>{batchVerdict && <strong>{batchVerdict}</strong>}</div>
        <progress max={batch.total} value={batch.done} style={{ width: '100%' }} />
        {failed.length > 0 && <div className="job-events">{failed.map((row) => <div key={row.keyIndex}>#{row.keyIndex + 1} {row.masked} — {row.message}</div>)}</div>}
        {failed.length > 0 && <p className="muted">失败的 Key 会被服务端自动冷却（指数退避 2→4→8→30 分钟）。</p>}
      </div>}

      {result && <div className="card">
        <div className="job-summary">
          <strong className={result.success === false ? 'error-text' : 'success-text'}>{result.success === false ? '失败' : '成功'}</strong>
          {result.status_code != null && <span>状态码 {String(result.status_code)}</span>}
          {result.duration_ms != null && <span>耗时 {String(result.duration_ms)}ms</span>}
          {result.first_byte_duration_ms != null && <span>首字节 {String(result.first_byte_duration_ms)}ms</span>}
          {result.actual_model != null && <span>实际模型 {String(result.actual_model)}</span>}
        </div>
        {Boolean(result.message || result.error) && <p className={result.success === false ? 'error-text' : 'muted'}>{String(result.message ?? result.error)}</p>}
        {result.response_text != null && <details open><summary>响应文本</summary><pre className="result-pre">{String(result.response_text)}</pre></details>}
        {result.api_error != null && <details open><summary>API 错误</summary><pre className="result-pre">{stringify(result.api_error)}</pre></details>}
        {result.api_response != null && <details><summary>API 响应</summary><pre className="result-pre">{stringify(result.api_response)}</pre></details>}
        {result.raw_response != null && <details><summary>原始响应</summary><pre className="result-pre">{stringify(result.raw_response)}</pre></details>}
        {result.response_headers != null && <details><summary>响应头</summary><pre className="result-pre">{stringify(result.response_headers)}</pre></details>}
        {hasUpstream && <button className="btn" onClick={() => setDetailOpen(true)}>查看上游详情</button>}
      </div>}

      <UpstreamDetailDialog open={detailOpen} result={result} onClose={() => setDetailOpen(false)} />
    </Dialog>
  )
}

function stringify(value: unknown): string {
  if (typeof value === 'string') { try { return JSON.stringify(JSON.parse(value), null, 2) } catch { return value } }
  return JSON.stringify(value, null, 2)
}

/** 上游详情：Request/Response 页签、换行开关、复制、合并视图（POST /admin/debug-logs/merged-response）。 */
function UpstreamDetailDialog({ open, result, onClose }: { open: boolean; result: TestResult | null; onClose: () => void }) {
  const [tab, setTab] = useState<'request' | 'response' | 'merged'>('request')
  const [wrap, setWrap] = useState(true)
  const [merged, setMerged] = useState<{ reasoning?: string; content?: string; tools?: string } | null>(null)
  const [mergeError, setMergeError] = useState('')

  useEffect(() => { if (open) { setTab('request'); setMerged(null); setMergeError('') } }, [open])

  const requestText = [
    result?.upstream_request_url ? `URL: ${String(result.upstream_request_url)}` : '',
    result?.upstream_request_headers ? `\nHeaders:\n${stringify(result.upstream_request_headers)}` : '',
    result?.upstream_request_body ? `\nBody:\n${stringify(result.upstream_request_body)}` : '',
  ].join('')
  const responseText = [
    result?.upstream_status_code != null ? `Status: ${String(result.upstream_status_code)}\n` : '',
    result?.upstream_response_body ? stringify(result.upstream_response_body) : '',
  ].join('')

  const loadMerged = async () => {
    setTab('merged'); setMergeError('')
    try { setMerged(await postJSON('/admin/debug-logs/merged-response', { resp_body: String(result?.upstream_response_body ?? '') })) }
    catch (cause) { setMergeError(cause instanceof Error ? cause.message : '合并失败') }
  }
  const current = tab === 'request' ? requestText : tab === 'response' ? responseText : [merged?.reasoning && `【思考】\n${merged.reasoning}`, merged?.content && `【内容】\n${merged.content}`, merged?.tools && `【工具调用】\n${merged.tools}`].filter(Boolean).join('\n\n')

  return (
    <Dialog open={open} onClose={onClose} size="lg" title="上游详情"
      footer={<>
        <label className="muted"><input type="checkbox" checked={wrap} onChange={(event) => setWrap(event.target.checked)} /> 自动换行</label>
        <button className="btn" onClick={() => void navigator.clipboard?.writeText(current)}>复制</button>
        <button className="btn" onClick={onClose}>关闭</button>
      </>}>
      <div className="dialog-tabs" role="tablist">
        <button role="tab" aria-selected={tab === 'request'} className={`dialog-tab${tab === 'request' ? ' active' : ''}`} onClick={() => setTab('request')}>Request</button>
        <button role="tab" aria-selected={tab === 'response'} className={`dialog-tab${tab === 'response' ? ' active' : ''}`} onClick={() => setTab('response')}>Response</button>
        <button role="tab" aria-selected={tab === 'merged'} className={`dialog-tab${tab === 'merged' ? ' active' : ''}`} onClick={() => void loadMerged()}>合并视图</button>
      </div>
      {mergeError && <p className="error-text">{mergeError}</p>}
      <pre className="result-pre" style={{ whiteSpace: wrap ? 'pre-wrap' : 'pre' }}>{current || '（空）'}</pre>
    </Dialog>
  )
}
