import { useEffect, useMemo, useRef, useState } from 'react'
import { Dialog } from '../components/dialog'
import { SearchableSelect } from '../components/searchable-select'
import { getJSON, postJSON } from '../lib/api'
import { getSession, getToken } from '../lib/auth'
import { formatCompact, formatSeconds, formatUSD } from '../lib/format'
import type { Channel, Session } from '../types'
import { ChannelEditorDialog } from './channels/channel-editor-dialog'

type Entry = { model: string; redirect_model?: string; disabled?: boolean }
type TestMode = 'channel' | 'model' | 'chat' | 'image'
type Key = { key_index?: number; api_key?: string; disabled?: boolean; note?: string; cost_multiplier?: number }
type ChatBlock = { type: 'text'; text: string } | { type: 'image_url'; image_url: { url: string } }
type ChatMessage = { role: string; content: string | ChatBlock[]; thinking?: string; summary?: Record<string, unknown> }
type PendingImage = { id: string; name: string; dataUrl: string }
type TestRow = { id: string; channelId: number; channel: string; model: string; status: 'pending' | 'running' | 'done' | 'failed'; result?: Record<string, unknown>; error?: string; note?: string }

const CHAT_STORAGE_KEY = 'ccload_model_test_chat_messages'
const CHAT_OPTIONS_KEY = 'ccload_model_test_chat_advanced_options'
const STATE_KEY = 'ccload_model_test_state'
const CLIENT_PROTOCOLS = [{ value: 'anthropic', label: 'Anthropic' }, { value: 'codex', label: 'Codex Responses' }, { value: 'openai', label: 'OpenAI Chat' }, { value: 'gemini', label: 'Gemini' }]
const THINKING_EFFORTS = [{ value: '', label: '默认' }, ...['none', 'minimal', 'low', 'medium', 'high', 'xhigh'].map((value) => ({ value, label: value }))]
const IMAGE_APIS = [{ value: 'images', label: 'Images API' }, { value: 'chat_completions', label: 'Chat Completions' }]
// 像素尺寸用于 Images API；比例@尺寸用于 Chat Completions（后端 admin_testing_image.go 校验）。
const PIXEL_SIZES = ['auto', '1024x1024', '1536x1024', '1024x1536', '2048x2048', '512x512', '768x768', '1280x720']
const RATIO_SIZES = ['auto', '1:1@1k', '1:1@2k', '16:9@1k', '16:9@2k', '9:16@1k', '9:16@2k', '3:2@1k', '2:3@1k']
const IMAGE_QUALITIES = ['auto', 'standard', 'hd', 'low', 'medium', 'high']
const BACKGROUNDS = ['auto', 'transparent', 'opaque']
const FORMATS = ['auto', 'png', 'jpeg', 'webp']

type LooseEntry = string | { model?: unknown; redirect_model?: unknown; disabled?: unknown }
const modelName = (entry: LooseEntry | undefined) => !entry ? '' : typeof entry === 'string' ? entry : String(entry.model ?? '')
const entryDisabled = (entry: LooseEntry | undefined) => typeof entry === 'object' && entry != null && entry.disabled === true
const entryRedirect = (entry: LooseEntry | undefined) => typeof entry === 'object' && entry != null && entry.redirect_model ? String(entry.redirect_model) : undefined
const maskKey = (key?: string) => !key ? '' : key.length <= 10 ? '****' : `${key.slice(0, 6)}…${key.slice(-4)}`

const readStored = <T,>(key: string, fallback: T): T => { try { const raw = localStorage.getItem(key); return raw ? JSON.parse(raw) as T : fallback } catch { return fallback } }
const readStoredFlag = (key: string, fallback: boolean) => { try { const value = localStorage.getItem(key); return value == null ? fallback : value === '1' } catch { return fallback } }
const readStoredText = (key: string) => { try { return localStorage.getItem(key) ?? '' } catch { return '' } }
const readSavedChat = (): { sessionId: string; messages: ChatMessage[] } => {
  const value = readStored<{ session_id?: string; messages?: ChatMessage[] }>(CHAT_STORAGE_KEY, {})
  return { sessionId: value.session_id || crypto.randomUUID(), messages: Array.isArray(value.messages) ? value.messages : [] }
}

function extractSSEText(value: unknown): string {
  if (!value || typeof value !== 'object') return typeof value === 'string' ? value : ''
  const data = value as Record<string, unknown>
  const choices = Array.isArray(data.choices) ? data.choices : []
  const choice = (choices[0] ?? {}) as Record<string, unknown>
  const delta = (choice.delta ?? {}) as Record<string, unknown>
  const message = (choice.message ?? {}) as Record<string, unknown>
  return String(delta.content ?? message.content ?? choice.text ?? data.content ?? data.text ?? '')
}

const chatMessageText = (message: ChatMessage) => typeof message.content === 'string' ? message.content : message.content.filter((part): part is Extract<ChatBlock, { type: 'text' }> => part.type === 'text').map((part) => part.text).join('\n')

const readPendingImages = (files: File[]) => Promise.all(files.map((file) => new Promise<PendingImage>((resolve, reject) => {
  const reader = new FileReader()
  reader.onload = () => resolve({ id: crypto.randomUUID(), name: file.name || 'clipboard-image', dataUrl: String(reader.result ?? '') })
  reader.onerror = () => reject(reader.error)
  reader.readAsDataURL(file)
})))

function findGeneratedImages(value: unknown): Array<{ src: string; label: string }> {
  if (!value || typeof value !== 'object') return []
  const data = value as Record<string, unknown>
  const list = Array.isArray(data.data) ? data.data : Array.isArray(data.images) ? data.images : []
  return list.flatMap((item, index) => {
    if (!item || typeof item !== 'object') return []
    const image = item as Record<string, unknown>
    const src = typeof image.url === 'string' ? image.url : typeof image.b64_json === 'string' ? `data:image/png;base64,${image.b64_json}` : typeof image.base64 === 'string' ? `data:${String(image.mime_type ?? 'image/png')};base64,${image.base64}` : ''
    return src ? [{ src, label: String(image.revised_prompt ?? `生成图片 ${index + 1}`) }] : []
  })
}

export function ModelTestPage() {
  const [savedChat] = useState(readSavedChat)
  const stored = useMemo(() => readStored<{ mode?: TestMode; channelId?: number; model?: string; protocol?: string }>(STATE_KEY, {}), [])
  const [session, setSession] = useState<Session | null>(null)
  const [mode, setMode] = useState<TestMode>(stored.mode ?? 'channel')
  const [channelRows, setChannelRows] = useState<Channel[]>([])
  const [channelId, setChannelId] = useState<number | ''>(stored.channelId ?? '')
  const [model, setModel] = useState(stored.model ?? '')
  const [protocol, setProtocol] = useState(stored.protocol ?? 'anthropic')
  const [content, setContent] = useState('hi')
  const [stream, setStream] = useState(true)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<unknown>(null)
  const [error, setError] = useState('')
  const [keys, setKeys] = useState<Key[]>([])
  const [keyIndex, setKeyIndex] = useState('')
  const [rows, setRows] = useState<TestRow[]>([])
  const [concurrency, setConcurrency] = useState(5)
  const [filter, setFilter] = useState('')
  const [selectedRows, setSelectedRows] = useState<string[]>([])
  const [modelFilter, setModelFilter] = useState('')
  const [modelSelected, setModelSelected] = useState<string[]>([])
  const [editor, setEditor] = useState<{ id: number; name: string } | null>(null)
  const [upstream, setUpstream] = useState<Record<string, unknown> | null>(null)
  // 聊天
  const [chat, setChat] = useState<ChatMessage[]>(savedChat.messages)
  const [sessionId, setSessionId] = useState(savedChat.sessionId)
  const [chatInput, setChatInput] = useState('')
  const [chatStream, setChatStream] = useState(() => readStoredFlag('ccload_model_test_chat_stream_enabled', true))
  const [thinkingEffort, setThinkingEffort] = useState(() => readStoredText('ccload_model_test_chat_thinking_effort'))
  const [builtinSearch, setBuiltinSearch] = useState(() => readStoredFlag('ccload_model_test_chat_builtin_search', false))
  const [pendingImages, setPendingImages] = useState<PendingImage[]>([])
  const [advanced, setAdvanced] = useState(() => { const saved = readStored<Record<string, unknown>>(CHAT_OPTIONS_KEY, {}); return { systemPrompt: String(saved.systemPrompt ?? ''), temperature: saved.temperature == null ? '' : String(saved.temperature), topP: saved.topP == null ? '' : String(saved.topP), maxTokens: saved.maxTokens == null ? '' : String(saved.maxTokens), contextMessages: saved.contextMessages == null ? '0' : String(saved.contextMessages) } })
  const [advancedOpen, setAdvancedOpen] = useState(false)
  // 生图
  const [image, setImage] = useState({ generation_api: 'images', size: 'auto', quality: 'auto', background: 'auto', output_format: 'auto' })
  const [prompt, setPrompt] = useState('')
  // Token 角色
  const [tokenModels, setTokenModels] = useState<string[]>([])
  const [tokenModel, setTokenModel] = useState('')
  const [tokenContent, setTokenContent] = useState('Hello')
  const [tokenResult, setTokenResult] = useState('')
  const abort = useRef<AbortController | null>(null)

  const readOnly = session?.role === 'api_token'
  const current = channelRows.find((row) => row.id === channelId) ?? null
  const configuredModels = useMemo(() => (current?.models ?? []).map(modelName).filter(Boolean), [current])
  // 渠道的 models 是对象数组（model/config.go:ModelEntry），必须取 .model 而不是整体字符串化。
  const allModels = useMemo(() => [...new Set(channelRows.flatMap((row) => (row.models ?? []).map(modelName).filter(Boolean)))].sort(), [channelRows])
  const channelsForModel = useMemo(() => {
    if (!model) return channelRows
    const exact = channelRows.filter((row) => (row.models ?? []).some((entry) => modelName(entry) === model))
    return exact.length ? exact : channelRows
  }, [channelRows, model])

  useEffect(() => {
    void getSession().then(async (value) => {
      setSession(value)
      if (value?.role === 'api_token') {
        const allowed = Array.isArray(value.allowed_models) ? value.allowed_models.map(String) : []
        const choices = allowed.length ? allowed : ((await getJSON<{ models?: string[] }>('/dashboard/models', { range: 'this_month' }).catch(() => ({ models: [] }))).models ?? [])
        setTokenModels(choices); setTokenModel(choices[0] ?? '')
        setTokenContent('')
        return
      }
      const settings = await getJSON<Array<{ key: string; value?: string }>>('/admin/settings').catch(() => [])
      const configured = settings.find((item) => item.key === 'channel_test_content')?.value?.split('|').map((item) => item.trim()).find(Boolean)
      if (configured) setContent(configured)
      // 不带分页参数以取全集（旧版行为）。
      const channels = await getJSON<Channel[]>('/admin/channels').catch(() => [])
      setChannelRows(Array.isArray(channels) ? channels : [])
    })
  }, [])

  useEffect(() => { try { localStorage.setItem(CHAT_STORAGE_KEY, JSON.stringify({ session_id: sessionId, messages: chat })) } catch { /* 存储不可用 */ } }, [chat, sessionId])
  useEffect(() => { try { localStorage.setItem(CHAT_OPTIONS_KEY, JSON.stringify({ systemPrompt: advanced.systemPrompt, temperature: advanced.temperature === '' ? null : Number(advanced.temperature), topP: advanced.topP === '' ? null : Number(advanced.topP), contextMessages: advanced.contextMessages === '' ? null : Number(advanced.contextMessages), maxTokens: advanced.maxTokens === '' ? null : Number(advanced.maxTokens) })) } catch { /* 存储不可用 */ } }, [advanced])
  useEffect(() => { try { localStorage.setItem('ccload_model_test_chat_stream_enabled', chatStream ? '1' : '0'); localStorage.setItem('ccload_model_test_chat_thinking_effort', thinkingEffort); localStorage.setItem('ccload_model_test_chat_builtin_search', builtinSearch ? '1' : '0') } catch { /* 存储不可用 */ } }, [chatStream, thinkingEffort, builtinSearch])
  useEffect(() => { try { localStorage.setItem(STATE_KEY, JSON.stringify({ mode, channelId: channelId === '' ? undefined : channelId, model, protocol })) } catch { /* 存储不可用 */ } }, [mode, channelId, model, protocol])

  useEffect(() => {
    if (!channelId) { setKeys([]); setKeyIndex(''); return }
    void getJSON<Key[]>(`/admin/channels/${channelId}/keys`).then((value) => { setKeys(Array.isArray(value) ? value : []); setKeyIndex(String(value.find((item) => !item.disabled)?.key_index ?? '')) }).catch(() => setKeys([]))
  }, [channelId])

  // 切换模式时重置不适用的选择，避免把生图/聊天参数带到其他模式。
  const switchMode = (next: TestMode) => { setMode(next); setResult(null); setError('') }
  // 生图选项按接口与渠道类型联动：Chat Completions 与 xAI OAuth 只支持比例尺寸，且不支持质量/背景/格式。
  const ratioSizes = current?.auth_type === 'xai_oauth' || image.generation_api === 'chat_completions'
  const sizeOptions = (ratioSizes ? RATIO_SIZES : PIXEL_SIZES).map((value) => ({ value, label: value }))
  const optionDisabled = ratioSizes

  const body = (targetModel: string, withKey = true): Record<string, unknown> => ({
    model: targetModel, content, stream, client_protocol: protocol,
    ...(withKey && keyIndex !== '' ? { key_index: Number(keyIndex) } : {}),
    system_prompt: advanced.systemPrompt.trim() || undefined,
    temperature: advanced.temperature === '' ? undefined : Number(advanced.temperature),
    top_p: advanced.topP === '' ? undefined : Number(advanced.topP),
    max_tokens: advanced.maxTokens === '' ? undefined : Number(advanced.maxTokens),
  })

  const runSingle = async () => {
    if (!channelId || !model) { setError('请选择渠道与模型'); return }
    setBusy(true); setError(''); setResult(null)
    try { setResult(await postJSON(`/admin/channels/${channelId}/test`, body(model))) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '测试失败') }
    finally { setBusy(false) }
  }
  const runChat = async () => {
    if (!channelId || !model) { setError('请选择渠道与模型'); return }
    const text = chatInput.trim()
    if (!text && !pendingImages.length) return
    const userContent: string | ChatBlock[] = pendingImages.length ? [...(text ? [{ type: 'text' as const, text }] : []), ...pendingImages.map((item) => ({ type: 'image_url' as const, image_url: { url: item.dataUrl } }))] : text
    const messages = [...chat, { role: 'user', content: userContent }]
    const contextCount = Math.max(0, Number(advanced.contextMessages) || 0)
    const payload = {
      model, messages: (contextCount > 0 ? messages.slice(-contextCount) : messages).map(({ role, content: value }) => ({ role, content: value })),
      stream: chatStream, client_protocol: protocol,
      ...(keyIndex === '' ? {} : { key_index: Number(keyIndex) }),
      session_id: sessionId, thinking_effort: thinkingEffort || undefined, builtin_search: builtinSearch,
      system_prompt: advanced.systemPrompt.trim() || undefined,
      temperature: advanced.temperature === '' ? undefined : Number(advanced.temperature),
      top_p: advanced.topP === '' ? undefined : Number(advanced.topP),
      max_tokens: advanced.maxTokens === '' ? undefined : Number(advanced.maxTokens),
    }
    setChat(messages); setChatInput(''); setPendingImages([]); setBusy(true); setError('')
    const controller = new AbortController(); abort.current = controller
    let assistant = ''; let thinking = ''; let summary: Record<string, unknown> | undefined
    try {
      const token = getToken()
      const response = await fetch(`/admin/channels/${channelId}/chat`, { method: 'POST', credentials: 'include', signal: controller.signal, headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream', ...(token ? { Authorization: `Bearer ${token}` } : {}) }, body: JSON.stringify(payload) })
      if (!response.ok || !response.body) throw new Error(`聊天请求失败（HTTP ${response.status}）`)
      const reader = response.body.getReader(); const decoder = new TextDecoder(); let buffer = ''
      const consume = (chunk: string, flush = false) => {
        buffer += chunk
        const events = buffer.split(/\r?\n\r?\n/)
        buffer = flush ? '' : (events.pop() ?? '')
        for (const event of events) {
          const line = event.split(/\r?\n/).find((item) => item.startsWith('data:'))
          if (!line) continue
          const raw = line.slice(5).trim()
          if (!raw || raw === '[DONE]') continue
          let parsed: Record<string, unknown>
          try { parsed = JSON.parse(raw) as Record<string, unknown> } catch { assistant += raw; continue }
          if (parsed.error) throw new Error(String(parsed.error))
          // 统计事件（admin_testing_stream.go:chatSummaryEventChunk）单独解析，避免被当成正文。
          if (parsed.summary && typeof parsed.summary === 'object') { summary = parsed.summary as Record<string, unknown>; continue }
          if (typeof parsed.delta === 'string') assistant += parsed.delta
          if (typeof parsed.thinking_delta === 'string') thinking += parsed.thinking_delta
          if (typeof parsed.delta !== 'string' && typeof parsed.thinking_delta !== 'string') assistant += extractSSEText(parsed)
        }
      }
      while (true) { const part = await reader.read(); if (part.done) break; consume(decoder.decode(part.value, { stream: true })) }
      consume(decoder.decode(), true)
      // 失败或空回复时撤回刚发的用户消息，不写入历史。
      if (!assistant.trim()) { setChat(chat); setError('上游返回了空回复，已撤回本次消息'); return }
      setChat((currentChat) => [...currentChat, { role: 'assistant', content: assistant, thinking: thinking || undefined, summary }])
    } catch (cause) {
      if (!controller.signal.aborted) { setChat(chat); setError(cause instanceof Error ? cause.message : '聊天请求失败') }
    } finally { abort.current = null; setBusy(false) }
  }
  const runImage = async () => {
    if (!channelId || !model) { setError('请选择渠道与模型'); return }
    if (!prompt.trim()) { setError('请填写提示词'); return }
    setBusy(true); setError(''); setResult(null)
    try {
      setResult(await postJSON(`/admin/channels/${channelId}/images/generations`, {
        generation_api: image.generation_api, model, prompt: prompt.trim(),
        ...(keyIndex === '' ? {} : { key_index: Number(keyIndex) }),
        size: image.size, ...(optionDisabled ? {} : { quality: image.quality, background: image.background, output_format: image.output_format }),
      }))
    } catch (cause) { setError(cause instanceof Error ? cause.message : '生图失败') } finally { setBusy(false) }
  }

  // ---- 表格数据 ----
  const channelModelRows = configuredModels.map((name) => ({ id: `c:${channelId}:${name}`, channelId: Number(channelId), channel: current?.name ?? '', model: name }))
  const modelRows = useMemo(() => channelRows.flatMap((row) => (row.models ?? []).map((entry) => ({ channelId: row.id, channel: row.name, model: modelName(entry), disabled: entryDisabled(entry), redirect: entryRedirect(entry) })))
    .filter((item) => item.model && (!modelFilter.trim() || item.model.toLowerCase().includes(modelFilter.trim().toLowerCase()) || item.channel.toLowerCase().includes(modelFilter.trim().toLowerCase()))), [channelRows, modelFilter])
  const tableRows: Array<{ id: string; channelId: number; channel: string; model: string; priority?: number; disabled?: boolean; redirect?: string; effective?: number | null }> =
    mode === 'model' ? modelRows.map((item) => ({ id: `m:${item.channelId}:${item.model}`, ...item })) : channelModelRows
  const visibleRows = tableRows.filter((row) => !filter.trim() || row.model.toLowerCase().includes(filter.trim().toLowerCase()) || row.channel.toLowerCase().includes(filter.trim().toLowerCase()))
  const resultById = useMemo(() => new Map(rows.map((row) => [row.id, row])), [rows])

  const runBatch = async () => {
    const selectedTargets = mode === 'model' ? tableRows.filter((row) => modelSelected.includes(row.id)) : channelModelRows.filter((row) => selectedRows.includes(row.id))
    if (!selectedTargets.length) { setError('请先勾选要测试的行'); return }
    const initial: TestRow[] = selectedTargets.map((row) => ({ id: row.id, channelId: row.channelId, channel: row.channel, model: row.model, status: 'pending' }))
    setRows(initial); setBusy(true); setError('')
    let cursor = 0
    // Key 变化时按渠道重新定位 Key，避免跨渠道沿用错误的 key_index。
    const workers = Array.from({ length: Math.min(Math.max(1, concurrency), initial.length) }, async () => {
      while (cursor < initial.length) {
        const target = initial[cursor++]
        const update = (patch: Partial<TestRow>) => setRows((currentRows) => currentRows.map((row) => row.id === target.id ? { ...row, ...patch } : row))
        update({ status: 'running' })
        try {
          const targetKeys = await getJSON<Key[]>(`/admin/channels/${target.channelId}/keys`).catch(() => [])
          const usable = targetKeys.find((item) => !item.disabled)?.key_index
          const payload = { ...body(target.model, false), ...(usable == null ? {} : { key_index: usable }) }
          const value = await postJSON<Record<string, unknown>>(`/admin/channels/${target.channelId}/test`, payload)
          update({ status: value?.success === false ? 'failed' : 'done', result: value, error: value?.success === false ? String(value?.error ?? value?.message ?? '') : undefined })
        } catch (cause) { update({ status: 'failed', error: cause instanceof Error ? cause.message : '测试失败' }) }
      }
    })
    await Promise.all(workers)
    setBusy(false)
    // 测完只保留失败行选中，便于重测。
    const failed = (await new Promise<TestRow[]>((resolve) => setRows((currentRows) => { resolve(currentRows.filter((row) => row.status === 'failed')); return currentRows }))).map((row) => row.id)
    if (mode === 'model') setModelSelected(failed); else setSelectedRows(failed)
  }

  const savePriority = async (channelIdTarget: number, priority: number) => {
    try { await postJSON('/admin/channels/batch-priority', { updates: [{ id: channelIdTarget, priority }] }); setChannelRows((currentRows) => currentRows.map((row) => row.id === channelIdTarget ? { ...row, priority } : row)) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '优先级保存失败') }
  }
  const toggleModelDisabled = async (channelIdTarget: number, targetModel: string, disabled: boolean) => {
    setChannelRows((currentRows) => currentRows.map((row) => row.id === channelIdTarget ? { ...row, models: (row.models ?? []).map((entry) => modelName(entry) === targetModel && typeof entry === 'object' ? { ...entry, disabled } : entry) } : row))
    try { await postJSON(`/admin/channels/${channelIdTarget}`, { model: targetModel, disabled }) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '切换失败'); void getJSON<Channel[]>('/admin/channels').then((value) => setChannelRows(Array.isArray(value) ? value : [])) }
  }

  const sendTokenTest = async () => {
    if (!tokenModel) return
    setBusy(true); setTokenResult('')
    const endpoints: Record<string, string> = { anthropic: '/dashboard/v1/messages', openai: '/dashboard/v1/chat/completions', codex: '/dashboard/v1/responses', gemini: `/dashboard/v1beta/models/${encodeURIComponent(tokenModel)}:generateContent` }
    try {
      const token = getToken()
      const response = await fetch(endpoints[protocol] ?? endpoints.openai, { method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) }, body: JSON.stringify({ model: tokenModel, stream: false, messages: [{ role: 'user', content: tokenContent }] }) })
      setTokenResult(JSON.stringify(await response.json(), null, 2))
    } catch (cause) { setTokenResult(cause instanceof Error ? cause.message : '请求失败') } finally { setBusy(false) }
  }

  // ---- Token 角色：仅显示模型测试卡片 ----
  if (readOnly) return <>
    <header className="page-header"><div><h1>模型测试</h1><p className="muted">以当前 API Token 的身份调用网关，验证模型可用性</p></div></header>
    <section className="card">
      <div className="toolbar">
        <SearchableSelect ariaLabel="模型" className="combobox-inline" allowCustomInput value={tokenModel} options={tokenModels.map((name) => ({ value: name, label: name }))} onChange={setTokenModel} placeholder="模型" />
        <SearchableSelect ariaLabel="协议" className="combobox-inline" value={protocol} options={CLIENT_PROTOCOLS} onChange={setProtocol} />
      </div>
      <textarea className="input wide test-content" value={tokenContent} onChange={(event) => setTokenContent(event.target.value)} placeholder="内容" />
      <button className="btn btn-primary" disabled={busy || !tokenModel} onClick={() => void sendTokenTest()}>{busy ? '请求中…' : '发送请求'}</button>
      {tokenResult && <pre className="result-pre">{tokenResult}</pre>}
    </section>
  </>

  const selectedCount = mode === 'model' ? modelSelected.length : selectedRows.length
  const images = findGeneratedImages(result)

  return <>
    <header className="page-header">
      <div><h1>模型测试</h1><p className="muted">按渠道或按模型批量测试连通性、对话与生图</p></div>
      <div className="toolbar" style={{ marginBottom: 0 }}><button className="btn" onClick={() => setAdvancedOpen(true)}>高级参数</button></div>
    </header>

    <div className="toolbar" role="tablist">
      {([['channel', '按渠道'], ['model', '按模型'], ['chat', '对话'], ['image', '生图']] as Array<[TestMode, string]>).map(([key, label]) => (
        <button key={key} role="tab" aria-selected={mode === key} className={`btn${mode === key ? ' btn-primary' : ''}`} onClick={() => switchMode(key)}>{label}</button>
      ))}
    </div>

    <div className="toolbar">
      <SearchableSelect ariaLabel="渠道" className="combobox-inline" value={channelId === '' ? '' : String(channelId)} options={[{ value: '', label: '选择渠道' }, ...channelRows.map((row) => ({ value: String(row.id), label: `${row.name}${row.enabled === false ? '（已停用）' : ''}` }))]} onChange={(value) => { setChannelId(value === '' ? '' : Number(value)); setModel('') }} />
      {mode !== 'model' && <SearchableSelect ariaLabel="模型" className="combobox-inline" options={configuredModels.map((name) => ({ value: name, label: name }))} value={model} onChange={setModel} placeholder={configuredModels.length ? '选择模型' : '该渠道暂无模型'} />}
      <SearchableSelect ariaLabel="请求协议" className="combobox-inline" value={protocol} options={CLIENT_PROTOCOLS} onChange={setProtocol} />
      <SearchableSelect ariaLabel="API Key" className="combobox-inline" value={keyIndex} options={[{ value: '', label: '自动选择 Key' }, ...keys.slice(0, 10).map((key) => ({ value: String(key.key_index), label: `#${Number(key.key_index) + 1} ${maskKey(key.api_key)}${key.note ? `（${key.note}）` : ''}×${key.cost_multiplier ?? 1}` }))]} onChange={setKeyIndex} />
      <label><input type="checkbox" checked={stream} onChange={(event) => setStream(event.target.checked)} /> 流式</label>
      {mode === 'image' && <button className="btn btn-primary" disabled={busy || !channelId || !model} onClick={() => void runImage()}>{busy ? '生成中…' : '生成图片'}</button>}
    </div>
    {error && <div className="card error-text" role="alert">{error}</div>}

    {/* ---------------- 按渠道 / 按模型 ---------------- */}
    {(mode === 'channel' || mode === 'model') && <>
      <div className="toolbar">
        <input className="input" value={filter} onChange={(event) => setFilter(event.target.value)} placeholder="筛选模型或渠道" aria-label="筛选" />
        {mode === 'model' && <input className="input" value={modelFilter} onChange={(event) => setModelFilter(event.target.value)} placeholder="筛选模型" aria-label="筛选模型" />}
        <label className="muted"><input type="checkbox" checked={visibleRows.length > 0 && visibleRows.every((row) => (mode === 'model' ? modelSelected : selectedRows).includes(row.id))} onChange={(event) => { const ids = visibleRows.map((row) => row.id); if (mode === 'model') setModelSelected(event.target.checked ? [...new Set([...modelSelected, ...ids])] : modelSelected.filter((id) => !ids.includes(id))); else setSelectedRows(event.target.checked ? [...new Set([...selectedRows, ...ids])] : selectedRows.filter((id) => !ids.includes(id))) }} /> 全选可见</label>
        <label className="muted">并发<input className="input compact" type="number" min={1} max={20} value={concurrency} onChange={(event) => setConcurrency(Number(event.target.value) || 5)} /></label>
        <button className="btn btn-primary" disabled={busy || !selectedCount} onClick={() => void runBatch()}>{busy ? '测试中…' : `批量测试（${selectedCount}）`}</button>
        {mode === 'channel' && <button className="btn" disabled={!channelId} onClick={() => setEditor({ id: Number(channelId), name: current?.name ?? '' })}>编辑渠道</button>}
        {rows.length > 0 && <span className="muted">完成 {rows.filter((row) => row.status === 'done' || row.status === 'failed').length} / {rows.length} · 失败 {rows.filter((row) => row.status === 'failed').length}</span>}
      </div>
      <div className="table-wrap"><table><thead><tr>
        <th><input type="checkbox" aria-label="全选" checked={visibleRows.length > 0 && visibleRows.every((row) => (mode === 'model' ? modelSelected : selectedRows).includes(row.id))} onChange={(event) => { const ids = visibleRows.map((row) => row.id); if (mode === 'model') setModelSelected(event.target.checked ? [...new Set([...modelSelected, ...ids])] : []); else setSelectedRows(event.target.checked ? [...new Set([...selectedRows, ...ids])] : []) }} /></th>
        {mode === 'model' && <th>渠道</th>}<th>模型</th>{mode === 'model' && <th>优先级</th>}
        <th>首字</th><th>耗时</th><th>输入</th><th>输出</th><th>速度</th><th>缓读</th><th>缓建</th><th>费用</th><th>结果</th>
      </tr></thead><tbody>
        {!visibleRows.length && <tr><td colSpan={mode === 'model' ? 13 : 11} className="muted">{channelId || mode === 'model' ? '没有匹配的模型' : '请选择渠道'}</td></tr>}
        {visibleRows.map((row) => {
          const outcome = resultById.get(row.id)
          const data = outcome?.result ?? {}
          const input = Number(data.usage ? (data.usage as Record<string, unknown>).input_tokens ?? 0 : 0)
          const output = Number((data.usage as Record<string, unknown> | undefined)?.output_tokens ?? 0)
          const firstByte = Number(data.first_byte_duration_ms ?? 0)
          const duration = Number(data.duration_ms ?? 0)
          const speed = duration > 0 && output > 0 ? output / (duration / 1000) : null
          const channelRow = channelRows.find((item) => item.id === row.channelId)
          const channelMultiplier = Number((channelRow as Record<string, unknown> | undefined)?.cost_multiplier ?? 1)
          const selected = (mode === 'model' ? modelSelected : selectedRows).includes(row.id)
          return <tr key={row.id} className={outcome?.status === 'failed' ? 'error-row' : undefined}>
            <td><input type="checkbox" checked={selected} onChange={(event) => mode === 'model' ? setModelSelected((list) => event.target.checked ? [...list, row.id] : list.filter((id) => id !== row.id)) : setSelectedRows((list) => event.target.checked ? [...list, row.id] : list.filter((id) => id !== row.id))} /></td>
            {mode === 'model' && <td>{row.channel}{channelRow?.enabled === false && <span className="badge badge-muted">已停用</span>}</td>}
            <td className="cell-stack"><span>{row.model}{row.disabled && <span className="badge badge-muted">已停用</span>}</span>{row.redirect && <span className="table-note">↪ {row.redirect}</span>}</td>
            {mode === 'model' && <td><PriorityInput value={Number(channelRow?.priority ?? 0)} onSave={(value) => void savePriority(row.channelId, value)} /><label className="table-note"><input type="checkbox" checked={!row.disabled} onChange={(event) => void toggleModelDisabled(row.channelId, row.model, !event.target.checked)} /> 启用</label></td>}
            <td>{outcome?.status === 'pending' ? '-' : firstByte ? `${firstByte}ms` : '-'}</td>
            <td>{outcome?.status === 'pending' ? '-' : duration ? `${duration}ms` : '-'}</td>
            <td>{input ? formatCompact(input) : '-'}</td><td>{output ? formatCompact(output) : '-'}</td>
            <td>{speed == null ? '-' : `${speed.toFixed(1)} t/s`}</td>
            <td>{formatCompact((data.usage as Record<string, unknown> | undefined)?.cache_read_tokens ?? 0)}</td>
            <td>{formatCompact((data.usage as Record<string, unknown> | undefined)?.cache_creation_tokens ?? 0)}</td>
            <td title={`渠道倍率 ×${channelMultiplier}`}>{data.cost_usd == null ? '-' : `${formatUSD(Number(data.cost_usd) * channelMultiplier)}`}</td>
            <td className="log-message">
              {outcome?.status === 'running' || outcome?.status === 'pending' ? <span className="badge badge-warn">{outcome.status === 'running' ? '测试中' : '排队中'}</span>
                : outcome?.status === 'failed' ? <span className="error-text" title={outcome.error}>{outcome.error || '失败'}</span>
                : data.success === false ? <span className="error-text">{String(data.error ?? data.message ?? '失败')}</span>
                : <><span className="success-text">{String(data.response_text ?? '成功').slice(0, 120)}</span>{Boolean(data.upstream_request_url) && <button className="link-button" onClick={() => setUpstream(data)}>上游详情</button>}</>}
            </td>
          </tr>
        })}
      </tbody></table></div>
      {rows.length > 0 && rows.some((row) => row.status === 'failed') && <p className="muted">失败的模型已自动勾选，便于直接重测。</p>}
    </>}

    {/* ---------------- 对话 ---------------- */}
    {mode === 'chat' && <section className="card">
      <div className="toolbar">
        <SearchableSelect ariaLabel="思考等级" className="combobox-inline" value={thinkingEffort} options={THINKING_EFFORTS} onChange={setThinkingEffort} />
        <label><input type="checkbox" checked={chatStream} onChange={(event) => setChatStream(event.target.checked)} /> 流式</label>
        <label><input type="checkbox" checked={builtinSearch} onChange={(event) => setBuiltinSearch(event.target.checked)} /> 内置搜索</label>
        <button className="btn" disabled={!chat.length} onClick={() => { setChat([]); setSessionId(crypto.randomUUID()); setResult(null) }}>清空对话</button>
        <button className="btn" disabled={chat.length < 2} onClick={() => exportChat(chat)}>导出 Markdown</button>
      </div>
      <div className="chat-transcript">
        {!chat.length && <p className="muted">发送第一条消息开始对话。</p>}
        {chat.map((message, index) => <div key={index} className={`chat-message chat-${message.role === 'user' ? 'user' : 'assistant'}`}>
          <strong>{message.role === 'user' ? '我' : '助手'}</strong>
          {message.thinking && <details><summary>思考过程</summary><pre className="result-pre">{message.thinking}</pre></details>}
          <span style={{ whiteSpace: 'pre-wrap' }}>{chatMessageText(message)}</span>
          {message.summary && <span className="table-note">首字 {Number(message.summary.first_byte_ms ?? 0)}ms · 耗时 {Number(message.summary.duration_ms ?? 0)}ms · 输入 {formatCompact(message.summary.input_tokens)} · 输出 {formatCompact(message.summary.output_tokens)} · 缓读 {formatCompact(message.summary.cache_read)} · 缓建 {formatCompact(message.summary.cache_create)}{message.summary.speed != null ? ` · ${Number(message.summary.speed).toFixed(1)} t/s` : ''}{message.summary.cost_usd != null ? ` · ${formatUSD(message.summary.cost_usd)}` : ''}</span>}
          {message.role === 'user' && <span className="toolbar" style={{ marginBottom: 0 }}><button className="link-button" onClick={() => void navigator.clipboard?.writeText(chatMessageText(message))}>复制</button><button className="link-button" onClick={() => { setChat(chat.slice(0, index)); setChatInput(chatMessageText(message)) }}>修改重发</button></span>}
        </div>)}
      </div>
      {pendingImages.length > 0 && <div className="toolbar">{pendingImages.map((item) => <span key={item.id} className="badge">{item.name} <button className="link-button" aria-label="移除图片" onClick={() => setPendingImages((list) => list.filter((image) => image.id !== item.id))}>×</button></span>)}</div>}
      <textarea className="input wide test-content" value={chatInput} onChange={(event) => setChatInput(event.target.value)}
        onPaste={(event) => { const files = Array.from(event.clipboardData.files); if (files.length) void readPendingImages(files).then((images) => setPendingImages((list) => [...list, ...images])) }}
        onKeyDown={(event) => { if (event.key === 'Enter' && !event.shiftKey) { event.preventDefault(); void runChat() } }} placeholder="输入消息，Enter 发送，Shift+Enter 换行" />
      <div className="toolbar">
        <label className="btn">添加图片<input hidden type="file" accept="image/*" multiple onChange={(event) => { void readPendingImages(Array.from(event.target.files ?? [])).then((images) => setPendingImages((list) => [...list, ...images])); event.target.value = '' }} /></label>
        {busy && abort.current && <button className="btn" onClick={() => abort.current?.abort()}>停止</button>}
        <button className="btn btn-primary" disabled={busy || !channelId || !model} onClick={() => void runChat()}>{busy ? '生成中…' : '发送'}</button>
      </div>
    </section>}

    {/* ---------------- 生图 ---------------- */}
    {mode === 'image' && <section className="card">
      <div className="toolbar">
        <SearchableSelect ariaLabel="生图接口" className="combobox-inline" value={image.generation_api} options={IMAGE_APIS} onChange={(generation_api) => setImage((currentImage) => ({ ...currentImage, generation_api, size: 'auto' }))} />
        <SearchableSelect ariaLabel="尺寸" className="combobox-inline" value={image.size} options={sizeOptions} onChange={(size) => setImage((currentImage) => ({ ...currentImage, size }))} />
        <SearchableSelect ariaLabel="质量" className="combobox-inline" disabled={optionDisabled} value={image.quality} options={IMAGE_QUALITIES.map((value) => ({ value, label: value }))} onChange={(quality) => setImage((currentImage) => ({ ...currentImage, quality }))} />
        <SearchableSelect ariaLabel="背景" className="combobox-inline" disabled={optionDisabled} value={image.background} options={BACKGROUNDS.map((value) => ({ value, label: value }))} onChange={(background) => setImage((currentImage) => ({ ...currentImage, background }))} />
        <SearchableSelect ariaLabel="格式" className="combobox-inline" disabled={optionDisabled} value={image.output_format} options={FORMATS.map((value) => ({ value, label: value }))} onChange={(output_format) => setImage((currentImage) => ({ ...currentImage, output_format }))} />
      </div>
      {optionDisabled && <p className="muted">Chat Completions 与 xAI OAuth 只支持「比例@尺寸」，且不支持质量、背景与格式。</p>}
      <textarea className="input wide test-content" value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder="提示词，Ctrl/⌘ + Enter 提交" onKeyDown={(event) => { if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') void runImage() }} />
      {images.length > 0 && <div className="image-results">{images.map((item) => <figure key={item.src.slice(0, 64)}><img src={item.src} alt={item.label} /><figcaption>{item.label} <a className="link-button" href={item.src} target="_blank" rel="noopener noreferrer">打开原图</a></figcaption></figure>)}</div>}
    </section>}

    {result != null && mode !== 'image' && <section className="card"><div className="job-summary" style={{ marginTop: 0 }}><strong>最近结果</strong><button className="link-button" onClick={() => setResult(null)}>关闭</button></div><pre className="result-pre">{JSON.stringify(result, null, 2)}</pre></section>}

    <AdvancedDialog open={advancedOpen} value={advanced} onClose={() => setAdvancedOpen(false)} onApply={(next) => { setAdvanced(next); setAdvancedOpen(false) }} />
    <ChannelEditorDialog open={editor !== null} editing={editor} onClose={() => setEditor(null)} onSaved={() => { void getJSON<Channel[]>('/admin/channels').then((value) => setChannelRows(Array.isArray(value) ? value : [])) }} onNotice={(value) => setError(typeof value === 'object' && value && 'error' in value ? String((value as { error: unknown }).error) : '')} onTestModel={() => undefined} />
    <UpstreamDialog open={upstream !== null} data={upstream} onClose={() => setUpstream(null)} />
  </>
}

/** 导出对话为 Markdown，含思考过程与统计信息。 */
function exportChat(messages: ChatMessage[]) {
  const lines = messages.map((message) => {
    const role = message.role === 'user' ? '我' : '助手'
    const parts = [`## ${role}`, '']
    if (message.thinking) parts.push('<details><summary>思考过程</summary>', '', '```', message.thinking, '```', '', '</details>', '')
    parts.push(chatMessageText(message), '')
    if (message.summary) parts.push(`> 首字 ${Number(message.summary.first_byte_ms ?? 0)}ms · 耗时 ${Number(message.summary.duration_ms ?? 0)}ms · 输入 ${Number(message.summary.input_tokens ?? 0)} · 输出 ${Number(message.summary.output_tokens ?? 0)}`, '')
    return parts.join('\n')
  }).join('\n')
  const body = ['# 模型测试对话', '', `导出时间：${new Date().toLocaleString()}`, '', lines].join('\n')
  const url = URL.createObjectURL(new Blob([body], { type: 'text/markdown' }))
  const link = document.createElement('a')
  link.href = url
  link.download = `model-test-chat-${Date.now()}.md`
  link.click()
  window.setTimeout(() => URL.revokeObjectURL(url), 1000)
}

/** 行内优先级：1 秒防抖保存，Enter 立即保存，Esc 还原（旧版 model-test.js:684-742）。 */
function PriorityInput({ value, onSave }: { value: number; onSave: (value: number) => void }) {
  const [draft, setDraft] = useState(String(value))
  const timer = useRef(0)
  useEffect(() => { setDraft(String(value)) }, [value])
  const flush = (next: string) => { const parsed = Math.trunc(Number(next)); if (Number.isFinite(parsed) && parsed !== value) onSave(parsed); else setDraft(String(value)) }
  return <input className="input priority-input" type="number" value={draft} aria-label="优先级"
    onChange={(event) => { setDraft(event.target.value); window.clearTimeout(timer.current); timer.current = window.setTimeout(() => flush(event.target.value), 1000) }}
    onBlur={(event) => { window.clearTimeout(timer.current); flush(event.target.value) }}
    onKeyDown={(event) => { if (event.key === 'Enter') { event.preventDefault(); window.clearTimeout(timer.current); flush(event.currentTarget.value) } if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); window.clearTimeout(timer.current); setDraft(String(value)) } }} />
}

type AdvancedValue = { systemPrompt: string; temperature: string; topP: string; maxTokens: string; contextMessages: string }
function AdvancedDialog({ open, value, onClose, onApply }: { open: boolean; value: AdvancedValue; onClose: () => void; onApply: (value: AdvancedValue) => void }) {
  const [draft, setDraft] = useState(value)
  const [error, setError] = useState('')
  useEffect(() => { if (open) { setDraft(value); setError('') } }, [open, value])
  const apply = () => {
    const temperature = draft.temperature === '' ? null : Number(draft.temperature)
    const topP = draft.topP === '' ? null : Number(draft.topP)
    const maxTokens = draft.maxTokens === '' ? null : Number(draft.maxTokens)
    if (temperature != null && (!Number.isFinite(temperature) || temperature < 0 || temperature > 2)) { setError('温度需在 0–2 之间'); return }
    if (topP != null && (!Number.isFinite(topP) || topP < 0 || topP > 1)) { setError('Top-P 需在 0–1 之间'); return }
    // max_tokens 后端是 int，小数会导致 JSON 绑定失败。
    if (maxTokens != null && (!Number.isInteger(maxTokens) || maxTokens <= 0)) { setError('最大 Token 需为正整数'); return }
    onApply(draft)
  }
  return <Dialog open={open} onClose={onClose} size="md" closeOnBackdrop={false} title="高级参数" description="仅作用于对话模式"
    footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={apply}>应用</button></>}>
    {error && <p className="error-text" role="alert">{error}</p>}
    <label className="cell-stack">系统提示词<textarea className="input wide" value={draft.systemPrompt} onChange={(event) => setDraft({ ...draft, systemPrompt: event.target.value })} /></label>
    <div className="toolbar">
      <label className="cell-stack">温度（0–2）<input className="input compact" type="number" min={0} max={2} step="0.1" value={draft.temperature} onChange={(event) => setDraft({ ...draft, temperature: event.target.value })} /></label>
      <label className="cell-stack">Top-P（0–1）<input className="input compact" type="number" min={0} max={1} step="0.05" value={draft.topP} onChange={(event) => setDraft({ ...draft, topP: event.target.value })} /></label>
      <label className="cell-stack">最大 Token<input className="input compact" type="number" min={1} step={1} value={draft.maxTokens} onChange={(event) => setDraft({ ...draft, maxTokens: event.target.value })} /></label>
      <label className="cell-stack">上下文条数<input className="input compact" type="number" min={0} step={1} value={draft.contextMessages} onChange={(event) => setDraft({ ...draft, contextMessages: event.target.value })} /></label>
    </div>
  </Dialog>
}

/** 上游详情：Request / Response 两个页签 + 合并视图（纯文本，不引入 Markdown 依赖）。 */
function UpstreamDialog({ open, data, onClose }: { open: boolean; data: Record<string, unknown> | null; onClose: () => void }) {
  const [tab, setTab] = useState<'request' | 'response' | 'merged'>('request')
  const [merged, setMerged] = useState<Record<string, unknown> | null>(null)
  const [error, setError] = useState('')
  useEffect(() => { if (open) { setTab('request'); setMerged(null); setError('') } }, [open])
  const pretty = (value: unknown) => { if (value == null) return ''; if (typeof value === 'string') { try { return JSON.stringify(JSON.parse(value), null, 2) } catch { return value } } return JSON.stringify(value, null, 2) }
  const request = [data?.upstream_request_url ? String(data.upstream_request_url) : '', pretty(data?.upstream_request_headers), pretty(data?.upstream_request_body)].filter(Boolean).join('\n\n')
  const response = [data?.status_code != null ? `HTTP ${String(data.status_code)}` : '', pretty(data?.response_headers), pretty(data?.upstream_response_body ?? data?.raw_response)].filter(Boolean).join('\n\n')
  const loadMerged = async () => {
    setTab('merged'); setError('')
    try { setMerged(await postJSON('/admin/debug-logs/merged-response', { resp_body: String(data?.upstream_response_body ?? data?.raw_response ?? '') })) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '合并失败') }
  }
  const mergedText = merged ? [merged.reasoning ? `【思考】\n${String(merged.reasoning)}` : '', merged.content ? `【内容】\n${String(merged.content)}` : '', merged.tools ? `【工具调用】\n${String(merged.tools)}` : ''].filter(Boolean).join('\n\n') || '（无可合并内容）' : ''
  const current = tab === 'request' ? request : tab === 'response' ? response : mergedText
  return <Dialog open={open} onClose={onClose} size="lg" title="上游详情"
    footer={<><button className="btn" onClick={() => void navigator.clipboard?.writeText(current)}>复制</button><button className="btn" onClick={onClose}>关闭</button></>}>
    <div className="dialog-tabs" role="tablist">
      <button role="tab" aria-selected={tab === 'request'} className={`dialog-tab${tab === 'request' ? ' active' : ''}`} onClick={() => setTab('request')}>Request</button>
      <button role="tab" aria-selected={tab === 'response'} className={`dialog-tab${tab === 'response' ? ' active' : ''}`} onClick={() => setTab('response')}>Response</button>
      <button role="tab" aria-selected={tab === 'merged'} className={`dialog-tab${tab === 'merged' ? ' active' : ''}`} onClick={() => void loadMerged()}>合并视图</button>
    </div>
    {error && <p className="error-text">{error}</p>}
    <pre className="result-pre debug-pre">{current || '（空）'}</pre>
  </Dialog>
}
