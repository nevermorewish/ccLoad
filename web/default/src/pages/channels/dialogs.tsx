import { useEffect, useMemo, useState, type DragEvent } from 'react'
import { Dialog } from '../../components/dialog'
import { getJSON } from '../../lib/api'
import type { Channel } from '../../types'
import { batchPriority, batchSortOverride, fetchModelsPreview, loadAllChannels, type ChannelKeyRow, type ModelEntry } from './api'

// ---------------------------------------------------------------- 确认

export function ConfirmDialog({ open, title, message, confirmLabel = '确定', danger = false, onConfirm, onClose }: { open: boolean; title: string; message: string; confirmLabel?: string; danger?: boolean; onConfirm: () => void | Promise<void>; onClose: () => void }) {
  const [busy, setBusy] = useState(false)
  return (
    <Dialog open={open} onClose={onClose} size="sm" title={title}
      footer={<>
        <button className="btn" onClick={onClose}>取消</button>
        <button className={`btn ${danger ? 'btn-danger' : 'btn-primary'}`} disabled={busy} onClick={() => { setBusy(true); void Promise.resolve(onConfirm()).finally(() => setBusy(false)) }}>{confirmLabel}</button>
      </>}>
      <p>{message}</p>
    </Dialog>
  )
}

// ---------------------------------------------------------------- 拖拽排序（渠道 / Key 共用）

type SortItem = { id: string; label: string; hint?: string }

function SortableList({ items, onChange }: { items: SortItem[]; onChange: (items: SortItem[]) => void }) {
  const [dragIndex, setDragIndex] = useState<number | null>(null)
  const move = (from: number, to: number) => {
    if (to < 0 || to >= items.length || from === to) return
    const next = [...items]
    const [moved] = next.splice(from, 1)
    next.splice(to, 0, moved)
    onChange(next)
  }
  const onDragOver = (event: DragEvent<HTMLLIElement>, index: number) => {
    event.preventDefault()
    if (dragIndex == null || dragIndex === index) return
    move(dragIndex, index)
    setDragIndex(index)
  }
  return (
    <ol className="sort-list">
      {items.map((item, index) => (
        <li key={item.id} className={`sort-item${dragIndex === index ? ' dragging' : ''}`} draggable onDragStart={() => setDragIndex(index)} onDragOver={(event) => onDragOver(event, index)} onDragEnd={() => setDragIndex(null)}>
          <span className="sort-handle" aria-hidden="true">⋮⋮</span>
          <span className="sort-label">{item.label}{item.hint && <span className="table-note">{item.hint}</span>}</span>
          <button type="button" className="btn btn-sm" aria-label="上移" disabled={index === 0} onClick={() => move(index, index - 1)}>↑</button>
          <button type="button" className="btn btn-sm" aria-label="下移" disabled={index === items.length - 1} onClick={() => move(index, index + 1)}>↓</button>
        </li>
      ))}
    </ol>
  )
}

/** 渠道排序：保存后相邻优先级相差 10（(N-index)*10），走 batch-priority。 */
export function ChannelSortDialog({ open, channels, onClose, onSaved }: { open: boolean; channels: Channel[]; onClose: () => void; onSaved: () => void }) {
  const [items, setItems] = useState<SortItem[]>([])
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!open) return
    setItems([...channels].sort((a, b) => Number(b.priority ?? 0) - Number(a.priority ?? 0) || a.name.localeCompare(b.name)).map((channel) => ({ id: String(channel.id), label: channel.name, hint: `当前优先级 ${String(channel.priority ?? 0)}` })))
  }, [open, channels])
  const save = async () => {
    setBusy(true)
    try { await batchPriority(items.map((item, index) => ({ id: Number(item.id), priority: (items.length - index) * 10 }))); onSaved(); onClose() }
    finally { setBusy(false) }
  }
  return (
    <Dialog open={open} onClose={onClose} size="md" title="渠道排序" description="拖拽或使用上下按钮调整顺序；保存后自上而下优先级依次递减 10。"
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={busy || !items.length} onClick={() => void save()}>保存排序</button></>}>
      {items.length ? <SortableList items={items} onChange={setItems} /> : <p className="muted">当前筛选结果为空。</p>}
    </Dialog>
  )
}

/** 惩罚排序覆盖：按当前有效优先级排初值，保存后写 sort_override=(N-index)*10。
 *  设为覆盖的渠道在选路时直接使用该值，不再叠加失败/首字惩罚。
 *  作用于全部渠道（不是当前页），避免与未显示的渠道错位。 */
export function SortOverrideDialog({ open, onClose, onSaved }: { open: boolean; onClose: () => void; onSaved: () => void }) {
  const [items, setItems] = useState<SortItem[]>([])
  const [busy, setBusy] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!open) return
    let cancelled = false
    setLoading(true); setError(''); setItems([])
    void loadAllChannels()
      .then((channels) => {
        if (cancelled) return
        // effective_priority 已包含覆盖值（覆盖时它等于 sort_override）；健康度关闭时回退到优先级。
        const key = (channel: Channel) => Number(channel.effective_priority ?? channel.sort_override ?? channel.priority ?? 0)
        const sorted = [...channels].sort((a, b) => key(b) - key(a) || a.name.localeCompare(b.name))
        setItems(sorted.map((channel) => ({
          id: String(channel.id),
          label: channel.name,
          hint: Number(channel.sort_override ?? 0) !== 0
            ? `已覆盖 ${Number(channel.sort_override)}`
            : `当前排序值 ${key(channel).toFixed(1)}`,
        })))
      })
      .catch((cause: unknown) => { if (!cancelled) setError(cause instanceof Error ? cause.message : '渠道加载失败') })
      .finally(() => { if (!cancelled) setLoading(false) })
    return () => { cancelled = true }
  }, [open])

  const save = async () => {
    setBusy(true); setError('')
    try {
      await batchSortOverride(items.map((item, index) => ({ id: Number(item.id), sort_override: (items.length - index) * 10 })))
      onSaved(); onClose()
    } catch (cause) { setError(cause instanceof Error ? cause.message : '保存失败') }
    finally { setBusy(false) }
  }
  const clearAll = async () => {
    setBusy(true); setError('')
    try {
      await batchSortOverride(items.map((item) => ({ id: Number(item.id), sort_override: 0 })))
      onSaved(); onClose()
    } catch (cause) { setError(cause instanceof Error ? cause.message : '清除失败') }
    finally { setBusy(false) }
  }

  return (
    <Dialog open={open} onClose={onClose} size="md" title="惩罚排序覆盖"
      description="拖拽调整顺序，保存后自上而下排序值依次递减 10。被覆盖的渠道直接按该值选路，不再叠加失败率与首字惩罚；真正冷却/故障的渠道仍会被排除在候选之外。"
      footer={<>
        <button className="btn" onClick={onClose}>取消</button>
        <button className="btn" disabled={busy || loading || !items.length} onClick={() => void clearAll()}>清除全部覆盖</button>
        <button className="btn btn-primary" disabled={busy || loading || !items.length} onClick={() => void save()}>保存覆盖</button>
      </>}>
      {error && <p className="error-text">{error}</p>}
      {loading ? <p className="muted">正在加载全部渠道…</p> : items.length ? <SortableList items={items} onChange={setItems} /> : <p className="muted">没有可排序的渠道。</p>}
    </Dialog>
  )
}

/** Key 排序：纯本地重排优先级，渠道保存后才生效（与旧页一致，不发请求）。 */
export function KeySortDialog({ open, keys, onClose, onApply }: { open: boolean; keys: ChannelKeyRow[]; onClose: () => void; onApply: (keys: ChannelKeyRow[]) => void }) {
  const [items, setItems] = useState<SortItem[]>([])
  useEffect(() => {
    if (!open) return
    setItems(keys.map((key, index) => ({ id: String(index), label: maskKey(key.api_key), hint: key.note || `优先级 ${Number(key.priority ?? 0)}` })))
  }, [open, keys])
  const apply = () => {
    const total = items.length
    onApply(items.map((item, position) => ({ ...keys[Number(item.id)], priority: (total - position) * 10 })))
    onClose()
  }
  return (
    <Dialog open={open} onClose={onClose} size="md" title="Key 排序" description="调整后保存渠道才会生效。"
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={apply}>确认排序</button></>}>
      <SortableList items={items} onChange={setItems} />
    </Dialog>
  )
}

export const maskKey = (value: string) => !value ? '' : value.length <= 10 ? '****' : `${value.slice(0, 6)}…${value.slice(-4)}`

// ---------------------------------------------------------------- 模型导入

/** 解析模型导入：文本（逗号/换行分隔）或 JSON（字符串/对象数组，或含 data 数组的 /v1/models 响应）。 */
export function parseModelImport(raw: string, format: 'text' | 'json'): ModelEntry[] {
  if (format === 'text') return raw.split(/[\n,]/).map((item) => item.trim()).filter(Boolean).map((model) => ({ model }))
  const parsed = JSON.parse(raw) as unknown
  const list = Array.isArray(parsed) ? parsed : Array.isArray((parsed as { data?: unknown })?.data) ? (parsed as { data: unknown[] }).data : null
  if (!list) throw new Error('JSON 需要是模型数组，或包含 data 数组的模型列表响应')
  return list.map((item) => {
    if (typeof item === 'string') return { model: item.trim() }
    const record = item as Record<string, unknown>
    const model = String(record.model ?? record.id ?? record.name ?? '').trim()
    return { model, ...(record.redirect_model ? { redirect_model: String(record.redirect_model) } : {}) }
  }).filter((entry) => entry.model)
}

export function ModelImportDialog({ open, batch = false, onClose, onImport }: { open: boolean; batch?: boolean; onClose: () => void; onImport: (models: ModelEntry[], mode: 'append' | 'replace') => void | Promise<void> }) {
  const [format, setFormat] = useState<'text' | 'json'>('text')
  const [mode, setMode] = useState<'append' | 'replace'>('append')
  const [raw, setRaw] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => { if (open) { setRaw(''); setError(''); setMode('append') } }, [open])
  const preview = useMemo(() => { try { return parseModelImport(raw, format) } catch { return [] } }, [raw, format])
  const submit = async () => {
    setError('')
    let models: ModelEntry[]
    try { models = parseModelImport(raw, format) } catch (cause) { setError(cause instanceof Error ? cause.message : '解析失败'); return }
    if (!models.length) { setError('没有可导入的模型'); return }
    if (batch && mode === 'replace' && !window.confirm(`覆盖模式会用这 ${models.length} 个模型替换所选渠道的全部模型，确定继续吗？`)) return
    setBusy(true)
    try { await onImport(models, mode); onClose() } finally { setBusy(false) }
  }
  return (
    <Dialog open={open} onClose={onClose} size="md" title={batch ? '批量导入模型' : '导入模型'}
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={busy || !raw.trim()} onClick={() => void submit()}>导入 {preview.length ? `(${preview.length})` : ''}</button></>}>
      <div className="toolbar">
        <label><input type="radio" name="model-import-format" checked={format === 'text'} onChange={() => setFormat('text')} /> 文本</label>
        <label><input type="radio" name="model-import-format" checked={format === 'json'} onChange={() => setFormat('json')} /> JSON</label>
        {batch && <>
          <span className="muted">｜</span>
          <label><input type="radio" name="model-import-mode" checked={mode === 'append'} onChange={() => setMode('append')} /> 追加</label>
          <label><input type="radio" name="model-import-mode" checked={mode === 'replace'} onChange={() => setMode('replace')} /> 覆盖</label>
        </>}
      </div>
      <textarea className="input wide advanced-textarea" value={raw} onChange={(event) => setRaw(event.target.value)} placeholder={format === 'text' ? '每行一个或逗号分隔' : '["model-a", {"model":"b","redirect_model":"c"}] 或 {"data":[{"id":"..."}]}'} />
      {error && <p className="error-text">{error}</p>}
    </Dialog>
  )
}

// ---------------------------------------------------------------- 常用模型

/** 与旧页 channels-modals.js:COMMON_MODELS 保持一致。 */
export const COMMON_MODELS: Record<string, string[]> = {
  anthropic: ['claude-haiku-4-5-20251001', 'claude-opus-4-8', 'claude-opus-5', 'claude-opus-5-5', 'claude-fable-5', 'claude-sonnet-5', 'claude-sonnet-4-6'],
  codex: ['gpt-5.5', 'gpt-5.6-sol', 'gpt-5.6-luna', 'gpt-5.6-terra', 'gpt-5.3-codex-spark', 'codex-auto-review'],
  gemini: ['gemini-3.6-flash', 'gemini-3.5-flash', 'gemini-2.5-pro', 'gemini-3.1-flash-lite', 'gemini-3.1-pro'],
}

export function CommonModelsDialog({ open, onClose, onAdd }: { open: boolean; onClose: () => void; onAdd: (models: string[]) => void }) {
  const [picked, setPicked] = useState<string[]>([])
  useEffect(() => { if (open) setPicked([]) }, [open])
  return (
    <Dialog open={open} onClose={onClose} size="sm" title="添加常用模型"
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={!picked.length} onClick={() => { onAdd(picked.flatMap((type) => COMMON_MODELS[type] ?? [])); onClose() }}>添加</button></>}>
      {Object.entries(COMMON_MODELS).map(([type, models]) => (
        <label key={type} className="cell-stack">
          <span><input type="checkbox" checked={picked.includes(type)} onChange={(event) => setPicked((current) => event.target.checked ? [...current, type] : current.filter((item) => item !== type))} /> {type === 'anthropic' ? 'Anthropic' : type === 'codex' ? 'Codex' : 'Gemini'}</span>
          <span className="table-note">{models.join(', ')}</span>
        </label>
      ))}
    </Dialog>
  )
}

// ---------------------------------------------------------------- Key 导入 / 导出

export function KeyImportDialog({ open, onClose, onImport }: { open: boolean; onClose: () => void; onImport: (keys: string[]) => void }) {
  const [raw, setRaw] = useState('')
  useEffect(() => { if (open) setRaw('') }, [open])
  const keys = useMemo(() => [...new Set(raw.split(/[\n,]/).map((item) => item.trim()).filter(Boolean))], [raw])
  return (
    <Dialog open={open} onClose={onClose} size="md" title="导入 API Key" description="支持换行或逗号分隔，自动去重。"
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={!keys.length} onClick={() => { onImport(keys); onClose() }}>导入 {keys.length ? `(${keys.length})` : ''}</button></>}>
      <textarea className="input wide advanced-textarea" value={raw} onChange={(event) => setRaw(event.target.value)} placeholder="sk-...&#10;sk-..." />
    </Dialog>
  )
}

export function KeyExportDialog({ open, keys, onClose }: { open: boolean; keys: string[]; onClose: () => void }) {
  const [separator, setSeparator] = useState<'newline' | 'comma'>('newline')
  const text = keys.join(separator === 'newline' ? '\n' : ',')
  const download = () => {
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
    const link = document.createElement('a'); link.href = url; link.download = 'api-keys.txt'; link.click()
    window.setTimeout(() => URL.revokeObjectURL(url), 1000)
  }
  return (
    <Dialog open={open} onClose={onClose} size="md" title={`导出 API Key（${keys.length}）`}
      footer={<><button className="btn" onClick={onClose}>关闭</button><button className="btn" onClick={() => void navigator.clipboard?.writeText(text)}>复制</button><button className="btn btn-primary" onClick={download}>下载 api-keys.txt</button></>}>
      <div className="toolbar">
        <label><input type="radio" name="key-export-sep" checked={separator === 'newline'} onChange={() => setSeparator('newline')} /> 换行分隔</label>
        <label><input type="radio" name="key-export-sep" checked={separator === 'comma'} onChange={() => setSeparator('comma')} /> 逗号分隔</label>
      </div>
      <textarea className="input wide advanced-textarea" readOnly value={text} />
    </Dialog>
  )
}

// ---------------------------------------------------------------- 渠道模型价格

const PRICE_FIELDS: Array<{ key: string; label: string; required?: boolean }> = [
  { key: 'input_price', label: '输入', required: true },
  { key: 'output_price', label: '输出', required: true },
  { key: 'cache_read_price', label: '缓存读取' },
  { key: 'cache_write_price', label: '缓存写入' },
  { key: 'input_price_high', label: '输入（高上下文）' },
  { key: 'output_price_high', label: '输出（高上下文）' },
  { key: 'cache_read_price_high', label: '缓存读取（高上下文）' },
  { key: 'cache_write_price_high', label: '缓存写入（高上下文）' },
]

/** 渠道级模型价格：整份替换该模型全局价格；全部留空=恢复全局价格。单位：美元 / 百万 token。 */
export function ModelPricingDialog({ open, entry, onClose, onApply }: { open: boolean; entry: ModelEntry | null; onClose: () => void; onApply: (pricing: Record<string, number> | undefined) => void }) {
  const [values, setValues] = useState<Record<string, string>>({})
  const [status, setStatus] = useState('')
  useEffect(() => {
    if (!open || !entry) return
    setStatus('')
    setValues(Object.fromEntries(PRICE_FIELDS.map((field) => [field.key, entry.pricing?.[field.key] == null ? '' : String(entry.pricing[field.key])])))
  }, [open, entry])
  const lookupModel = entry?.redirect_model || entry?.model || ''
  const loadDefaults = async () => {
    setStatus('加载中…')
    try {
      const result = await getJSON<{ found?: boolean; pricing?: Record<string, unknown> }>('/admin/model-pricing', { model: lookupModel, effective: 1 })
      // 分段计价模型无法用 8 项价目表达，拒绝加载默认值。
      if (Array.isArray(result.pricing?.token_pricing_tiers) && result.pricing.token_pricing_tiers.length) { setStatus('该模型使用分段计价，无法加载默认值'); return }
      if (!result.found) { setStatus('没有找到该模型的全局价格'); return }
      setValues(Object.fromEntries(PRICE_FIELDS.map((field) => [field.key, result.pricing?.[field.key] == null ? '' : String(result.pricing[field.key])])))
      setStatus('已加载当前价格')
    } catch (cause) { setStatus(cause instanceof Error ? cause.message : '加载失败') }
  }
  const apply = () => {
    const filled = Object.entries(values).filter(([, value]) => value.trim() !== '')
    if (!filled.length) {
      if (!window.confirm('全部留空将恢复使用全局价格，确定吗？')) return
      onApply(undefined); onClose(); return
    }
    const hasHigh = filled.some(([key]) => key.endsWith('_high'))
    const required = PRICE_FIELDS.filter((field) => field.required).map((field) => field.key).concat(hasHigh ? ['input_price_high', 'output_price_high'] : [])
    const missing = required.filter((key) => !values[key]?.trim())
    if (missing.length) { setStatus(`请填写：${missing.map((key) => PRICE_FIELDS.find((field) => field.key === key)?.label).join('、')}`); return }
    const pricing: Record<string, number> = {}
    for (const [key, value] of filled) {
      const parsed = Number(value)
      if (!Number.isFinite(parsed) || parsed < 0) { setStatus(`${PRICE_FIELDS.find((field) => field.key === key)?.label} 不是有效的非负数`); return }
      pricing[key] = parsed
    }
    onApply(pricing); onClose()
  }
  return (
    <Dialog open={open} onClose={onClose} size="md" title="渠道模型价格" description={<>模型 <code>{lookupModel}</code>；单位为美元 / 百万 token，整份替换该模型的全局价格。</>}
      footer={<>
        <button className="btn" onClick={() => void loadDefaults()}>加载当前价格</button>
        <button className="btn" onClick={() => setValues({})}>清空</button>
        <button className="btn" onClick={onClose}>取消</button>
        <button className="btn btn-primary" onClick={apply}>应用</button>
      </>}>
      <div className="advanced-grid">
        {PRICE_FIELDS.map((field) => (
          <label key={field.key} className="cell-stack">
            <span>{field.label}{field.required && ' *'}</span>
            <input className="input" type="number" min={0} step="0.0001" value={values[field.key] ?? ''} onChange={(event) => setValues((current) => ({ ...current, [field.key]: event.target.value }))} />
          </label>
        ))}
      </div>
      {status && <p className="muted" role="status">{status}</p>}
    </Dialog>
  )
}

// ---------------------------------------------------------------- 快捷添加

export type QuickAddResult = { name: string; url: string; key: string; models: ModelEntry[] }

const URL_FIELD = /(url|endpoint|api[_-]?base|base)/i
const KEY_FIELD = /(api[_-]?key|token|secret|密钥|key)/i

/** 解析 JSON / 环境变量赋值行 / URL+Key 标签文本，规则与旧页 parseQuickAddChannelInfo 一致。 */
export function parseQuickAdd(raw: string): { url: string; key: string } {
  const text = raw.trim()
  let url = ''; let key = ''
  const visit = (value: unknown, name = '') => {
    if (value == null) return
    if (typeof value === 'string') {
      if (!url && URL_FIELD.test(name) && /^https?:\/\//i.test(value.trim())) url = value.trim()
      else if (!key && KEY_FIELD.test(name) && !URL_FIELD.test(name)) key = value.trim()
      return
    }
    if (Array.isArray(value)) { value.forEach((item) => visit(item, name)); return }
    if (typeof value === 'object') Object.entries(value as Record<string, unknown>).forEach(([childName, child]) => visit(child, childName))
  }
  try { visit(JSON.parse(text)) } catch { /* 非 JSON，继续按行解析 */ }
  if (!url || !key) {
    for (const rawLine of text.split(/\r?\n/)) {
      const line = rawLine.trim().replace(/^(export|set)\s+/i, '')
      const bearer = line.match(/Bearer\s+(\S+)/i)
      if (!key && bearer) { key = bearer[1]; continue }
      const match = line.match(/^([^=:：]+?)\s*[=:：]\s*["']?(.+?)["']?$/)
      if (match) {
        const [, name, value] = match
        if (!url && URL_FIELD.test(name) && /^https?:\/\//i.test(value)) { url = value; continue }
        if (!key && KEY_FIELD.test(name)) { key = value; continue }
      }
      if (!url && /^https?:\/\/\S+$/i.test(line)) { url = line; continue }
    }
    if (!key) {
      const lines = text.split(/\r?\n/).map((item) => item.trim()).filter((item) => item && !/^https?:\/\//i.test(item))
      if (lines.length === 1 && !/\s/.test(lines[0])) key = lines[0]
    }
  }
  return { url: normalizeQuickAddURL(url), key }
}

/** 只接受 http/https，去掉 userinfo/query/hash，并剥离 /v1 及之后的路径。 */
export function normalizeQuickAddURL(value: string): string {
  if (!value) return ''
  try {
    const parsed = new URL(value)
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return ''
    parsed.username = ''; parsed.password = ''; parsed.search = ''; parsed.hash = ''
    parsed.pathname = parsed.pathname.replace(/\/v1(\/.*)?$/i, '').replace(/\/+$/, '')
    return parsed.toString().replace(/\/+$/, '')
  } catch { return '' }
}

export function QuickAddDialog({ open, onClose, onApply }: { open: boolean; onClose: () => void; onApply: (result: QuickAddResult) => void }) {
  const [raw, setRaw] = useState('')
  const [lowercase, setLowercase] = useState(false)
  const [stripPrefix, setStripPrefix] = useState(false)
  const [status, setStatus] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => { if (open) { setRaw(''); setStatus(''); setError('') } }, [open])
  const submit = async () => {
    setError(''); setStatus('')
    if (!raw.trim()) { setError('请粘贴渠道信息'); return }
    const { url, key } = parseQuickAdd(raw)
    if (!url) { setError('未识别到有效的 http/https URL'); return }
    if (!key) { setError('未识别到 API Key'); return }
    setBusy(true)
    try {
      // 依次以 openai、anthropic 协议探测模型，两者都失败才报错。
      let models: ModelEntry[] = []
      for (const protocol of ['openai', 'anthropic']) {
        setStatus(`正在以 ${protocol} 协议探测模型…`)
        try {
          const result = await fetchModelsPreview({ urls: [{ url, exact: false, protocols: [] }], protocol, api_keys: [key], lowercase_models: lowercase, strip_model_source_prefix: stripPrefix })
          models = (result.models ?? []).map((item) => typeof item === 'string' ? { model: item } : item).filter((entry) => entry.model)
          if (models.length) break
        } catch { /* 换下一个协议重试 */ }
      }
      if (!models.length) { setError('两种协议都未能获取到模型，请检查 URL 与 Key'); return }
      onApply({ name: new URL(url).hostname, url, key, models })
      onClose()
    } finally { setBusy(false); setStatus('') }
  }
  return (
    <Dialog open={open} onClose={onClose} size="md" title="一键填充渠道配置" description="支持 JSON、环境变量、URL/Key 标签文本。密钥仅用于本次模型探测，确认保存前不会落库。"
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" disabled={busy} onClick={() => void submit()}>检查并填充</button></>}>
      <textarea className="input wide advanced-textarea" value={raw} onChange={(event) => setRaw(event.target.value)} onKeyDown={(event) => { if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') void submit() }} placeholder={'OPENAI_BASE_URL=https://api.example.com/v1\nOPENAI_API_KEY=sk-...'} spellCheck={false} />
      <div className="toolbar">
        <label><input type="checkbox" checked={lowercase} onChange={(event) => setLowercase(event.target.checked)} /> 模型转小写</label>
        <label><input type="checkbox" checked={stripPrefix} onChange={(event) => setStripPrefix(event.target.checked)} /> 去来源前缀</label>
      </div>
      {error && <p className="error-text" role="alert">{error}</p>}
      {status && <p className="muted" role="status">{status}</p>}
    </Dialog>
  )
}
