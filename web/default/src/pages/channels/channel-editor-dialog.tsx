import { useEffect, useMemo, useRef, useState } from 'react'
import { Dialog } from '../../components/dialog'
import { SearchableSelect } from '../../components/searchable-select'
import { SearchableMultiSelect } from '../../components/searchable-multi-select'
import type { Channel, ChannelURLStat } from '../../types'
import {
  AUTH_TYPES, URL_PROTOCOLS, checkDuplicate, createChannel, fetchChannelModels, fetchKeyRate, fetchModelsPreview,
  formFromEditor, formToPayload, loadEditor, loadKeys, mergeModelEntries, normalizeModelEntries,
  runtimeURL, testChannelURL, toggleKey, toggleURL, updateChannel, websocketProbe,
  type ChannelEditorData, type ChannelForm, type ChannelKeyRow, type ModelEntry,
} from './api'
import { AdvancedSettingsDialog, advancedFromChannel, advancedToPayload, emptyAdvanced, validateAdvanced, type AdvancedDraft } from './advanced-settings-dialog'
import { CommonModelsDialog, ConfirmDialog, KeyExportDialog, KeyImportDialog, KeySortDialog, ModelImportDialog, ModelPricingDialog, QuickAddDialog, maskKey } from './dialogs'

type Props = {
  open: boolean
  editing: Channel | null
  /** 复制：载入源渠道内容，以新建方式保存，不复制管理账户凭据 */
  duplicateOf?: Channel | null
  onClose: () => void
  onSaved: () => void
  onNotice: (value: unknown) => void
  onTestModel: (channel: Channel, model: string) => void
}

type KeyStatusFilter = 'all' | 'normal' | 'cooldown' | 'disabled'
const VIRTUAL_THRESHOLD = 50
const VIRTUAL_ROW_HEIGHT = 52
const VIRTUAL_BUFFER = 5

const blankForm = (): ChannelForm => ({ name: '', auth_type: 'api_key', urls: [{ url: '' }], priority: 0, enabled: true, models: [], api_keys: [{ api_key: '', cost_multiplier: 1, priority: 0 }], protocol_transform_mode: 'auto', key_strategy: 'sequential' })
const keyCooling = (row: ChannelKeyRow) => Number(row.cooldown_until ?? 0) * 1000 > Date.now()

export function ChannelEditorDialog({ open, editing, duplicateOf, onClose, onSaved, onNotice, onTestModel }: Props) {
  const [form, setForm] = useState<ChannelForm>(blankForm)
  const [advanced, setAdvanced] = useState<AdvancedDraft>(emptyAdvanced)
  const [editorData, setEditorData] = useState<ChannelEditorData>({})
  const [urlStats, setUrlStats] = useState<ChannelURLStat[]>([])
  const [busy, setBusy] = useState(false)
  const [dirty, setDirty] = useState(false)
  const [error, setError] = useState('')
  const [duplicateHint, setDuplicateHint] = useState<Array<{ id: number; name: string }>>([])
  // Key 表
  const [showKeys, setShowKeys] = useState(true)
  const [keyFilter, setKeyFilter] = useState<KeyStatusFilter>('all')
  const [selectedKeys, setSelectedKeys] = useState<number[]>([])
  const [keyScroll, setKeyScroll] = useState(0)
  // 模型表
  const [modelKeyword, setModelKeyword] = useState('')
  const [selectedModels, setSelectedModels] = useState<number[]>([])
  // URL 表
  const [selectedURLs, setSelectedURLs] = useState<number[]>([])
  // 子弹窗
  const [dialog, setDialog] = useState<null | 'advanced' | 'quickAdd' | 'commonModels' | 'modelImport' | 'keyImport' | 'keyExport' | 'keySort' | 'deleteKeys' | 'deleteModels'>(null)
  const [scopeKeyIndex, setScopeKeyIndex] = useState<number | null>(null)
  const [pricingIndex, setPricingIndex] = useState<number | null>(null)
  const [wsProbing, setWsProbing] = useState(false)

  const source = duplicateOf ?? editing
  const isCreate = !editing || Boolean(duplicateOf)
  const isOAuth = form.auth_type !== 'api_key'
  const keyRows = form.api_keys ?? []

  const update = <K extends keyof ChannelForm>(key: K, value: ChannelForm[K]) => { setForm((current) => ({ ...current, [key]: value })); setDirty(true) }
  const setKey = (index: number, patch: Partial<ChannelKeyRow>) => update('api_keys', keyRows.map((item, itemIndex) => itemIndex === index ? { ...item, ...patch } : item))
  const setURL = (index: number, patch: Partial<ChannelForm['urls'][number]>) => update('urls', form.urls.map((item, itemIndex) => itemIndex === index ? { ...item, ...patch } : item))
  const setModel = (index: number, patch: Partial<ModelEntry>) => update('models', form.models.map((item, itemIndex) => itemIndex === index ? { ...item, ...patch } : item))

  // ---- 加载 ----
  useEffect(() => {
    if (!open) return
    setError(''); setDirty(false); setSelectedKeys([]); setSelectedModels([]); setSelectedURLs([]); setModelKeyword(''); setKeyFilter('all'); setDuplicateHint([]); setDialog(null)
    if (!source) {
      setForm(blankForm()); setAdvanced(emptyAdvanced()); setEditorData({}); setUrlStats([])
      return
    }
    let cancelled = false
    setBusy(true)
    loadEditor(source.id).then((data) => {
      if (cancelled) return
      const next = formFromEditor(data, source)
      const channel = (data.channel ?? source) as Record<string, unknown>
      if (duplicateOf) {
        next.name = `${next.name} (副本)`
        // 管理账户凭据不随副本复制。
        setAdvanced({ ...advancedFromChannel(channel, undefined) })
      } else {
        setAdvanced(advancedFromChannel(channel, data.management_account))
      }
      setForm(next); setEditorData(data); setUrlStats(data.url_stats?.items ?? [])
    }).catch((cause: unknown) => { if (!cancelled) setError(cause instanceof Error ? cause.message : '编辑器加载失败') })
      .finally(() => { if (!cancelled) setBusy(false) })
    return () => { cancelled = true }
  }, [open, source, duplicateOf])

  // 新建时 URL 输入 350ms 防抖预览重复渠道。
  useEffect(() => {
    if (!open || !isCreate) return
    const urls = form.urls.filter((entry) => entry.url.trim()).map((entry) => ({ url: entry.url.trim(), exact: Boolean(entry.exact), protocols: entry.protocols ?? [] }))
    if (!urls.length) { setDuplicateHint([]); return }
    const timer = window.setTimeout(() => { checkDuplicate(urls).then((result) => setDuplicateHint(result.duplicates ?? [])).catch(() => setDuplicateHint([])) }, 350)
    return () => window.clearTimeout(timer)
  }, [open, isCreate, form.urls])

  const modelNames = useMemo(() => form.models.map((entry) => entry.model).filter(Boolean), [form.models])
  const modelOptions = useMemo(() => [...new Set(modelNames)].map((name) => ({ value: name, label: name })), [modelNames])

  // ---- 保存 ----
  const save = async () => {
    setError('')
    if (!form.name.trim()) { setError('请填写渠道名称'); return }
    if (!form.urls.some((item) => item.url.trim())) { setError('请至少填写一个上游 URL'); return }
    if (!isOAuth && !keyRows.some((row) => row.api_key.trim())) { setError('请至少填写一个 API Key'); return }
    if (!form.models.length) { setError('请至少配置一个模型'); return }
    const problem = validateAdvanced(advanced)
    if (problem) { setError(`高级设置：${problem.message}`); setDialog('advanced'); return }
    if (isCreate) {
      const urls = form.urls.filter((entry) => entry.url.trim()).map((entry) => ({ url: entry.url.trim(), exact: Boolean(entry.exact), protocols: entry.protocols ?? [] }))
      const duplicates = (await checkDuplicate(urls).catch(() => ({ duplicates: [] }))).duplicates ?? []
      if (duplicates.length && !window.confirm(`以下渠道已使用相同 URL：${duplicates.map((item) => item.name).join('、')}。仍要保存吗？`)) return
    }
    const extra = advancedToPayload(advanced, isOAuth)
    const payload = { ...formToPayload(form, isOAuth, { customRules: extra.custom_request_rules, cooldownRules: extra.cooldown_detection_rules, management: extra.management_account }), ...extra }
    setBusy(true)
    try {
      if (editing && !duplicateOf) await updateChannel(editing.id, payload)
      else await createChannel(payload)
      setDirty(false); onSaved(); onClose()
    } catch (cause) { setError(cause instanceof Error ? cause.message : '保存失败') } finally { setBusy(false) }
  }

  // ---- 模型 ----
  const fetchModels = async () => {
    setError('')
    try {
      if (isOAuth) {
        if (!editing) { setError('OAuth 渠道需先保存后才能获取模型'); return }
        const result = await fetchChannelModels(editing.id)
        update('models', mergeModelEntries(form.models, toEntries(result.models)))
        return
      }
      // API Key 渠道：用当前草稿（含未保存的 URL/Key）逐 Key 探测，只用未停用、未冷却的 Key。
      const urls = form.urls.filter((entry) => entry.url.trim()).map((entry) => ({ url: entry.url.trim(), exact: Boolean(entry.exact), protocols: entry.protocols ?? [] }))
      const usable = keyRows.map((row, index) => ({ row, index })).filter(({ row }) => row.api_key.trim() && !row.disabled && !keyCooling(row))
      if (!urls.length || !usable.length) { setError('需要至少一个 URL 与一个可用 Key'); return }
      const apiKeys = keyRows.map((row, index) => usable.some((item) => item.index === index) ? row.api_key.trim() : '')
      const result = await fetchModelsPreview({ urls, api_keys: apiKeys, per_key: true, proxy_url: advanced.proxy_url.trim() || undefined })
      const merged = mergeModelEntries(form.models, toEntries(result.models))
      const perKey = (result.key_models ?? []).filter((item) => !item.error && item.models?.length)
      // 多 Key 时询问是否把探测结果写回各 Key 的模型范围；单 Key 不写。
      if (usable.length > 1 && perKey.length && window.confirm('是否把各 Key 的探测结果写回其模型范围？')) {
        const nextKeys = keyRows.map((row, index) => {
          const detected = perKey.find((item) => item.key_index === index)
          if (!detected) return row
          const names = (detected.models ?? []).map((entry) => entry.model)
          return { ...row, detected_models: names, allowed_models: names, model_scope_empty: false }
        })
        setForm((current) => ({ ...current, models: merged, api_keys: nextKeys })); setDirty(true)
      } else update('models', merged)
      const failed = (result.key_models ?? []).filter((item) => item.error)
      if (failed.length) onNotice({ warning: `${failed.length} 个 Key 探测失败`, failed })
    } catch (cause) { setError(cause instanceof Error ? cause.message : '获取模型失败') }
  }

  const visibleModels = useMemo(() => {
    const keyword = modelKeyword.trim().toLowerCase()
    return form.models.map((entry, index) => ({ entry, index })).filter(({ entry }) => !keyword || entry.model.toLowerCase().includes(keyword) || String(entry.redirect_model ?? '').toLowerCase().includes(keyword))
  }, [form.models, modelKeyword])

  // 旧页"全选"复选框的语义是反选当前可见模型。
  const invertModelSelection = () => {
    const visible = new Set(visibleModels.map((item) => item.index))
    setSelectedModels((current) => [...current.filter((index) => !visible.has(index)), ...[...visible].filter((index) => !current.includes(index))])
  }
  const normalizeSelected = (options: { lowercase?: boolean; stripPrefix?: boolean }) => {
    const chosen = new Set(selectedModels)
    const normalized = form.models.map((entry, index) => chosen.has(index) ? normalizeModelEntries([entry], options)[0] ?? entry : entry)
    update('models', normalizeModelEntries(normalized, {})); setSelectedModels([])
  }
  const exportModels = () => {
    const chosen = selectedModels.length ? selectedModels.map((index) => form.models[index]) : form.models
    const text = chosen.map((entry) => entry.redirect_model ? `${entry.model}|${entry.redirect_model}` : entry.model).join('\n')
    const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
    const link = document.createElement('a'); link.href = url; link.download = 'channel-models.txt'; link.click()
    window.setTimeout(() => URL.revokeObjectURL(url), 1000)
  }

  // ---- Key ----
  const visibleKeys = useMemo(() => keyRows.map((row, index) => ({ row, index })).filter(({ row }) => {
    if (keyFilter === 'all') return true
    if (keyFilter === 'disabled') return Boolean(row.disabled)
    if (keyFilter === 'cooldown') return !row.disabled && keyCooling(row)
    return !row.disabled && !keyCooling(row)
  }), [keyRows, keyFilter])

  // 删除只改草稿，保存时整体提交；至少保留 1 个 Key。
  const removeKeys = (indexes: number[]) => {
    const drop = new Set(indexes)
    const next = keyRows.filter((_, index) => !drop.has(index))
    if (!next.length) { setError('至少需要保留 1 个 API Key'); return }
    update('api_keys', next); setSelectedKeys([])
  }
  // 启停直接调用接口，按服务端 key_index 定位；有未保存改动时索引可能已漂移，先要求保存。
  const flipKey = async (index: number, disabled: boolean) => {
    if (!editing || duplicateOf) return
    if (dirty) { setError('请先保存其他改动再启停 Key'); return }
    try { await toggleKey(editing.id, index, disabled); update('api_keys', await loadKeys(editing.id)); setDirty(false) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '操作失败') }
  }
  const fetchRate = async (index: number) => {
    const row = keyRows[index]
    const account = advanced.management
    if (!account.profile || !account.base_url) { setError('获取倍率需要先在高级设置中配置管理账户'); return }
    try {
      const result = await fetchKeyRate({ profile: account.profile, base_url: account.base_url, api_key: row.api_key, access_token: account.access_token || undefined, user_id: account.user_id ? Number(account.user_id) : undefined })
      if (result.effective_rate_multiplier != null) setKey(index, { cost_multiplier: result.effective_rate_multiplier })
    } catch (cause) { setError(cause instanceof Error ? cause.message : '获取倍率失败') }
  }

  // ---- URL ----
  const flipURL = async (index: number, disabled: boolean) => {
    if (!editing || duplicateOf) return
    try { await toggleURL(editing.id, runtimeURL(form.urls[index]), disabled); setUrlStats((await loadEditor(editing.id)).url_stats?.items ?? []) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '操作失败') }
  }
  const testURL = async (index: number) => {
    if (!editing || duplicateOf) return
    const model = form.models[0]?.model
    if (!model) { setError('请先配置模型'); return }
    try { onNotice(await testChannelURL(editing.id, { model, stream: true, content: 'test', client_protocol: 'anthropic', key_index: 0, base_url: runtimeURL(form.urls[index]) })) }
    catch (cause) { setError(cause instanceof Error ? cause.message : '测试失败') }
  }
  const probeWebsocket = async () => {
    const targets = form.urls.filter((entry) => entry.url.trim())
    const key = keyRows.find((row) => row.api_key.trim())?.api_key
    if (!targets.length || !key) { setError('需要先填写 URL 与 API Key'); return }
    setWsProbing(true); setError('')
    try {
      // 逐 URL 探测，任一不支持则不勾选。
      const results = await Promise.all(targets.map((entry) => websocketProbe({ url: entry.url.trim(), api_key: key, proxy_url: advanced.proxy_url.trim() || undefined, custom_request_rules: advancedToPayload(advanced, true).custom_request_rules ?? undefined }).then((value) => value as Record<string, unknown>).catch((cause: unknown) => ({ supported: false, error: cause instanceof Error ? cause.message : '探测失败' }))))
      const supported = results.every((item) => item.supported === true)
      update('websockets', supported)
      onNotice({ websocket_probe: results })
    } finally { setWsProbing(false) }
  }

  const statFor = (entry: ChannelForm['urls'][number]) => urlStats.find((item) => item.url === runtimeURL(entry))
  const keyTableRef = useRef<HTMLDivElement>(null)
  const virtual = visibleKeys.length >= VIRTUAL_THRESHOLD
  const viewport = 480
  const startIndex = virtual ? Math.max(0, Math.floor(keyScroll / VIRTUAL_ROW_HEIGHT) - VIRTUAL_BUFFER) : 0
  const endIndex = virtual ? Math.min(visibleKeys.length, Math.ceil((keyScroll + viewport) / VIRTUAL_ROW_HEIGHT) + VIRTUAL_BUFFER) : visibleKeys.length
  const renderedKeys = visibleKeys.slice(startIndex, endIndex)

  return (
    <Dialog open={open} onClose={onClose} size="xl" closeOnBackdrop={false} confirmOnClose={dirty}
      title={duplicateOf ? '复制渠道' : editing ? `编辑渠道：${editing.name}` : '添加渠道'}
      description={isOAuth ? 'OAuth 渠道的认证方式与凭证由登录/导入流程管理，此处只调整路由、限制与倍率' : undefined}
      footer={<>
        <label><input type="checkbox" checked={Boolean(form.websockets)} onChange={(event) => update('websockets', event.target.checked)} /> 支持 WS</label>
        <button className="btn" disabled={wsProbing} onClick={() => void probeWebsocket()}>{wsProbing ? '检测中…' : '检测'}</button>
        <label className="muted">优先级<input className="input compact" type="number" min={-99999} max={9999999} value={form.priority} onChange={(event) => update('priority', Number(event.target.value))} /></label>
        <label className="muted">RPM<input className="input compact" type="number" min={0} value={Number(form.rpm_limit ?? 0)} onChange={(event) => update('rpm_limit', Number(event.target.value))} /></label>
        <label className="muted">并发<input className="input compact" type="number" min={0} value={Number(form.max_concurrency ?? 0)} onChange={(event) => update('max_concurrency', Number(event.target.value))} /></label>
        <label className="muted">日限额 $<input className="input compact" type="number" min={0} step="0.01" value={Number(form.daily_cost_limit ?? 0)} onChange={(event) => update('daily_cost_limit', Number(event.target.value))} /></label>
        <label><input type="checkbox" checked={form.enabled} onChange={(event) => update('enabled', event.target.checked)} /> 启用</label>
        <button className="btn" onClick={() => setDialog('advanced')}>高级</button>
        <button className="btn" onClick={onClose}>取消</button>
        <button className="btn btn-primary" disabled={busy} onClick={() => void save()}>{busy ? '保存中…' : dirty ? '保存 *' : '保存'}</button>
      </>}>
      {error && <p className="error-text" role="alert">{error}</p>}

      <div className="toolbar">
        <input className="input wide" value={form.name} onChange={(event) => update('name', event.target.value)} placeholder="渠道名称" aria-label="渠道名称" />
        {isCreate && <button className="btn" title="从 JSON、环境变量或 URL/Key 文本一键填充" onClick={() => setDialog('quickAdd')}>✨ 一键填充</button>}
        {/* 新建只允许 API Key：后端拒绝以 OAuth 类型创建渠道（admin_channels.go:handleCreateChannel）。 */}
        <SearchableSelect ariaLabel="认证类型" className="combobox-inline" value={form.auth_type} disabled options={AUTH_TYPES} onChange={() => undefined} />
        <SearchableSelect ariaLabel="协议处理" className="combobox-inline" value={String(form.protocol_transform_mode ?? 'auto')} options={[{ value: 'auto', label: '协议处理：自动（原生优先）' }, { value: 'upstream', label: '协议处理：上游直通' }, { value: 'local', label: '协议处理：ccLoad 转换' }]} onChange={(value) => update('protocol_transform_mode', value)} />
      </div>

      {/* ---------------- URL ---------------- */}
      <section className="cell-stack">
        <div className="toolbar">
          <strong>API URL（{form.urls.length}）</strong>
          <button className="btn" onClick={() => update('urls', [...form.urls, { url: '' }])}>添加</button>
          <button className="btn" disabled={!selectedURLs.length || selectedURLs.length >= form.urls.length} onClick={() => { const drop = new Set(selectedURLs); update('urls', form.urls.filter((_, index) => !drop.has(index))); setSelectedURLs([]) }}>删除选中</button>
        </div>
        {duplicateHint.length > 0 && <p className="error-text">以下渠道已使用相同 URL：{duplicateHint.map((item) => item.name).join('、')}</p>}
        <div className="table-wrap"><table><thead><tr>
          <th><input type="checkbox" aria-label="全选 URL" checked={form.urls.length > 0 && selectedURLs.length === form.urls.length} onChange={(event) => setSelectedURLs(event.target.checked ? form.urls.map((_, index) => index) : [])} /></th>
          <th>URL</th><th>支持协议</th><th>完整 URL</th><th>状态</th><th>延迟</th><th>请求/失败</th><th>操作</th>
        </tr></thead><tbody>
          {form.urls.map((entry, index) => {
            const stat = statFor(entry)
            const latency = stat?.latency_ms == null ? null : Number(stat.latency_ms)
            return <tr key={index}>
              <td><input type="checkbox" checked={selectedURLs.includes(index)} onChange={(event) => setSelectedURLs((current) => event.target.checked ? [...current, index] : current.filter((item) => item !== index))} /></td>
              <td><input className="input wide" value={entry.url} onChange={(event) => setURL(index, { url: event.target.value })} placeholder="https://api.example.com" /></td>
              <td><SearchableSelect ariaLabel="URL 协议" className="combobox-inline" value={entry.protocols?.[0] ?? ''} options={URL_PROTOCOLS} onChange={(value) => setURL(index, { protocols: value ? [value] : [] })} /></td>
              <td><input type="checkbox" title="使用原样 URL，不追加路径" checked={Boolean(entry.exact)} onChange={(event) => setURL(index, { exact: event.target.checked })} /></td>
              <td>{!stat ? <span className="badge badge-muted">未知</span> : stat.disabled ? <span className="badge badge-bad">已禁用</span> : stat.cooled_down ? <span className="badge badge-warn">冷却 {Math.ceil(Number(stat.cooldown_remain_ms ?? 0) / 1000)}s</span> : <span className="badge badge-good">正常</span>}</td>
              <td>{latency == null || latency < 0 ? '-' : `${Math.round(latency)}ms`}</td>
              <td>{stat ? `${Number(stat.requests ?? 0)}/${Number(stat.failures ?? 0)}` : '-'}</td>
              <td>
                <button className="btn" disabled={isCreate || !entry.url.trim()} onClick={() => void testURL(index)}>测试</button>
                <button className="btn" disabled={isCreate || !entry.url.trim()} onClick={() => void flipURL(index, !stat?.disabled)}>{stat?.disabled ? '启用' : '禁用'}</button>
                <button className="btn" disabled={form.urls.length <= 1} onClick={() => { update('urls', form.urls.filter((_, itemIndex) => itemIndex !== index)); setSelectedURLs([]) }}>删除</button>
              </td>
            </tr>
          })}
        </tbody></table></div>
        {form.urls.length > 1 && <p className="muted">多 URL 按延迟加权随机调度，失败 URL 指数退避冷却（2 分钟起，最长 30 分钟）。</p>}
      </section>

      {/* ---------------- API Key ---------------- */}
      <section className="cell-stack">
        <div className="toolbar">
          <strong>API Key（{keyRows.length}）</strong>
          {isOAuth ? <span className="muted">AT 只读显示，完整凭证由 ccLoad 管理并自动刷新；倍率可编辑。</span> : <>
            <button className="btn" onClick={() => update('api_keys', [...keyRows, { api_key: '', cost_multiplier: 1, priority: 0 }])}>添加</button>
            <button className="btn" onClick={() => setDialog('keyImport')}>导入</button>
            <button className="btn" disabled={!selectedKeys.length} onClick={() => setDialog('keyExport')}>导出选中</button>
            <button className="btn" disabled={keyRows.filter((row) => row.api_key.trim()).length < 2} onClick={() => setDialog('keySort')}>排序</button>
            <button className="btn" disabled={!selectedKeys.length} onClick={() => setDialog('deleteKeys')}>删除选中</button>
          </>}
          <button className="btn" onClick={() => setShowKeys(!showKeys)}>{showKeys ? '隐藏密钥' : '显示密钥'}</button>
          <SearchableSelect ariaLabel="Key 状态筛选" className="combobox-inline" value={keyFilter} options={[{ value: 'all', label: '全部状态' }, { value: 'normal', label: '正常' }, { value: 'cooldown', label: '冷却中' }, { value: 'disabled', label: '已禁用' }]} onChange={(value) => setKeyFilter(value as KeyStatusFilter)} />
        </div>
        {virtual && <p className="muted">Key 数量较多，已启用虚拟滚动。</p>}
        <div className="table-wrap" ref={keyTableRef} style={virtual ? { maxHeight: viewport, overflow: 'auto' } : undefined} onScroll={virtual ? (event) => setKeyScroll(event.currentTarget.scrollTop) : undefined}>
          <table><thead><tr>
            <th><input type="checkbox" aria-label="全选 Key" checked={visibleKeys.length > 0 && visibleKeys.every(({ index }) => selectedKeys.includes(index))} onChange={(event) => setSelectedKeys(event.target.checked ? visibleKeys.map(({ index }) => index) : [])} /></th>
            <th>#</th><th>API Key</th><th>备注 / 模型范围</th><th>倍率</th><th>优先级</th><th>状态</th><th>操作</th>
          </tr></thead><tbody>
            {virtual && startIndex > 0 && <tr aria-hidden="true" style={{ height: startIndex * VIRTUAL_ROW_HEIGHT }}><td colSpan={8} /></tr>}
            {renderedKeys.map(({ row, index }) => {
              const cooling = keyCooling(row)
              return <tr key={index} style={virtual ? { height: VIRTUAL_ROW_HEIGHT } : undefined}>
                <td><input type="checkbox" checked={selectedKeys.includes(index)} onChange={(event) => setSelectedKeys((current) => event.target.checked ? [...current, index] : current.filter((item) => item !== index))} /></td>
                <td>{index + 1}</td>
                <td><input className="input" type={showKeys ? 'text' : 'password'} value={row.api_key} readOnly={isOAuth} onChange={(event) => setKey(index, { api_key: event.target.value })} autoComplete="off" spellCheck={false} /></td>
                <td><div className="toolbar" style={{ marginBottom: 0, flexWrap: 'nowrap' }}>
                  <input className="input" value={row.note ?? ''} onChange={(event) => setKey(index, { note: event.target.value })} placeholder="备注" disabled={isOAuth} />
                  {!isOAuth && <button className="btn" title="模型范围" onClick={() => setScopeKeyIndex(index)}>{row.model_scope_empty ? '无模型' : row.allowed_models?.length ? `${row.allowed_models.length} 个模型` : '全部模型'}</button>}
                </div></td>
                <td><input className="input compact" type="number" min="0" step="0.01" value={Number(row.cost_multiplier ?? 1)} onChange={(event) => setKey(index, { cost_multiplier: Number(event.target.value) })} title="0 表示免费" /></td>
                <td><input className="input compact" type="number" value={Number(row.priority ?? 0)} disabled={isOAuth} onChange={(event) => setKey(index, { priority: Number(event.target.value) })} /></td>
                <td>{row.disabled ? <span className="badge badge-bad">已禁用</span> : cooling ? <span className="badge badge-warn">冷却中</span> : row.model_scope_empty ? <span className="badge badge-warn">无可用模型</span> : <span className="badge badge-good">正常</span>}</td>
                <td>
                  <button className="btn" onClick={() => void navigator.clipboard?.writeText(row.api_key)}>复制</button>
                  {!isOAuth && !isCreate && <button className="btn" onClick={() => void flipKey(index, !row.disabled)}>{row.disabled ? '启用' : '禁用'}</button>}
                  {!isOAuth && advanced.management.profile && <button className="btn" onClick={() => void fetchRate(index)}>获取倍率</button>}
                  {!isOAuth && <button className="btn" disabled={keyRows.length <= 1} onClick={() => { if (window.confirm('确定删除这个 API Key？保存后生效。')) removeKeys([index]) }}>删除</button>}
                </td>
              </tr>
            })}
            {virtual && endIndex < visibleKeys.length && <tr aria-hidden="true" style={{ height: (visibleKeys.length - endIndex) * VIRTUAL_ROW_HEIGHT }}><td colSpan={8} /></tr>}
            {!visibleKeys.length && <tr><td colSpan={8} className="muted">{keyFilter === 'all' ? '暂无 Key' : '没有符合该状态的 Key'}</td></tr>}
          </tbody></table>
        </div>
      </section>

      {/* ---------------- 模型 ---------------- */}
      <section className="cell-stack">
        <div className="toolbar">
          <strong>模型配置（{form.models.length}）</strong>
          <button className="btn" onClick={() => setDialog('commonModels')}>常用模型</button>
          <button className="btn" onClick={() => void fetchModels()}>获取模型</button>
          <button className="btn" onClick={() => setDialog('modelImport')}>添加模型</button>
          <button className="btn" disabled={!form.models.length} onClick={exportModels}>导出模型</button>
          <button className="btn" disabled={!selectedModels.length} onClick={() => normalizeSelected({ lowercase: true })}>转小写</button>
          <button className="btn" disabled={!selectedModels.length} onClick={() => normalizeSelected({ stripPrefix: true })}>去来源前缀</button>
          <button className="btn" disabled={!selectedModels.length} onClick={() => setDialog('deleteModels')}>删除选中</button>
          <input className="input" value={modelKeyword} onChange={(event) => setModelKeyword(event.target.value)} placeholder="筛选模型" aria-label="筛选模型" />
        </div>
        <div className="table-wrap" style={{ maxHeight: 420, overflow: 'auto' }}><table><thead><tr>
          <th><input type="checkbox" aria-label="反选可见模型" checked={visibleModels.length > 0 && visibleModels.every(({ index }) => selectedModels.includes(index))} onChange={invertModelSelection} /></th>
          <th>模型</th><th>上游模型（重定向）</th><th>状态</th><th>操作</th>
        </tr></thead><tbody>
          {visibleModels.map(({ entry, index }) => (
            <tr key={`${entry.model}-${index}`}>
              <td><input type="checkbox" checked={selectedModels.includes(index)} onChange={(event) => setSelectedModels((current) => event.target.checked ? [...current, index] : current.filter((item) => item !== index))} /></td>
              <td><input className="input" value={entry.model} onChange={(event) => setModel(index, { model: event.target.value })} /></td>
              <td><input className="input" value={entry.redirect_model ?? ''} onChange={(event) => setModel(index, { redirect_model: event.target.value || undefined })} placeholder="同模型名" /></td>
              <td className="cell-stack">
                {entry.disabled ? <span className="badge badge-bad">已停用</span> : <span className="badge badge-good">启用</span>}
                {entry.pricing && <span className="table-note">渠道价：入 {String(entry.pricing.input_price ?? '-')} / 出 {String(entry.pricing.output_price ?? '-')}</span>}
              </td>
              <td>
                <button className="btn" onClick={() => setModel(index, { model: entry.model.toLowerCase() })}>转小写</button>
                <button className="btn" onClick={() => setModel(index, { disabled: !entry.disabled })}>{entry.disabled ? '启用' : '停用'}</button>
                <button className="btn" onClick={() => setPricingIndex(index)}>价格</button>
                <button className="btn" disabled={isCreate || dirty || !editing} title={dirty ? '请先保存改动' : undefined} onClick={() => editing && onTestModel(editing, entry.model)}>测试</button>
                <button className="btn" onClick={() => { update('models', form.models.filter((_, itemIndex) => itemIndex !== index)); setSelectedModels([]) }}>删除</button>
              </td>
            </tr>
          ))}
          {!visibleModels.length && <tr><td colSpan={5} className="muted">{form.models.length ? '没有匹配的模型' : '暂无模型，可从常用模型、获取模型或添加模型开始'}</td></tr>}
        </tbody></table></div>
      </section>

      {/* ---------------- 子弹窗 ---------------- */}
      <AdvancedSettingsDialog open={dialog === 'advanced'} draft={advanced} channelId={editing?.id ?? null} authType={form.auth_type} urls={form.urls.map((entry) => entry.url)} models={modelNames} oauthCredential={editorData.oauth_credential} oauthCredentialInfo={editorData.oauth_credential_info} onClose={() => setDialog(null)} onApply={(next) => { setAdvanced(next); setDirty(true); setError('') }} />
      <QuickAddDialog open={dialog === 'quickAdd'} onClose={() => setDialog(null)} onApply={(result) => {
        setForm((current) => ({ ...current, name: current.name.trim() || result.name, urls: [{ url: result.url }], api_keys: [{ api_key: result.key, cost_multiplier: 1, priority: 0 }], models: mergeModelEntries(current.models, result.models) }))
        setDirty(true)
      }} />
      <CommonModelsDialog open={dialog === 'commonModels'} onClose={() => setDialog(null)} onAdd={(models) => update('models', mergeModelEntries(form.models, models.map((model) => ({ model }))))} />
      <ModelImportDialog open={dialog === 'modelImport'} onClose={() => setDialog(null)} onImport={(models) => update('models', mergeModelEntries(form.models, models))} />
      <KeyImportDialog open={dialog === 'keyImport'} onClose={() => setDialog(null)} onImport={(keys) => {
        const existing = new Set(keyRows.map((row) => row.api_key.trim()).filter(Boolean))
        const kept = keyRows.filter((row) => row.api_key.trim())
        update('api_keys', [...kept, ...keys.filter((key) => !existing.has(key)).map((api_key) => ({ api_key, cost_multiplier: 1, priority: 0 }))])
      }} />
      <KeyExportDialog open={dialog === 'keyExport'} keys={selectedKeys.map((index) => keyRows[index]?.api_key).filter(Boolean)} onClose={() => setDialog(null)} />
      <KeySortDialog open={dialog === 'keySort'} keys={keyRows.filter((row) => row.api_key.trim())} onClose={() => setDialog(null)} onApply={(next) => { update('api_keys', next); setSelectedKeys([]) }} />
      <ConfirmDialog open={dialog === 'deleteKeys'} title="删除 API Key" danger message={`确定删除选中的 ${selectedKeys.length} 个 Key 吗？保存渠道后生效。`} onClose={() => setDialog(null)} onConfirm={() => { removeKeys(selectedKeys); setDialog(null) }} />
      <ConfirmDialog open={dialog === 'deleteModels'} title="删除模型" danger message={`确定删除选中的 ${selectedModels.length} 个模型吗？保存渠道后生效。`} onClose={() => setDialog(null)} onConfirm={() => { const drop = new Set(selectedModels); update('models', form.models.filter((_, index) => !drop.has(index))); setSelectedModels([]); setDialog(null) }} />
      <ModelPricingDialog open={pricingIndex !== null} entry={pricingIndex === null ? null : form.models[pricingIndex] ?? null} onClose={() => setPricingIndex(null)} onApply={(pricing) => { if (pricingIndex !== null) setModel(pricingIndex, { pricing }) }} />
      <KeyScopeDialog open={scopeKeyIndex !== null} row={scopeKeyIndex === null ? null : keyRows[scopeKeyIndex] ?? null} options={modelOptions} urls={form.urls} proxyURL={advanced.proxy_url} onClose={() => setScopeKeyIndex(null)} onApply={(patch) => { if (scopeKeyIndex !== null) setKey(scopeKeyIndex, patch) }} />
    </Dialog>
  )
}

function toEntries(models: Array<string | ModelEntry> | undefined): ModelEntry[] {
  return (models ?? []).map((item) => typeof item === 'string' ? { model: item } : item).filter((entry) => entry.model)
}

// ---------------------------------------------------------------- Key 模型范围

/** Key 模型范围：允许全部 / 指定模型（可搜索多选）；探测会用该 Key 实际拉取模型并勾选命中项。 */
function KeyScopeDialog({ open, row, options, urls, proxyURL, onClose, onApply }: { open: boolean; row: ChannelKeyRow | null; options: Array<{ value: string; label: string }>; urls: ChannelForm['urls']; proxyURL: string; onClose: () => void; onApply: (patch: Partial<ChannelKeyRow>) => void }) {
  const [allowAll, setAllowAll] = useState(true)
  const [values, setValues] = useState<string[]>([])
  const [detected, setDetected] = useState<string[] | undefined>(undefined)
  const [status, setStatus] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    if (!open || !row) return
    const allowed = row.allowed_models ?? []
    setAllowAll(!allowed.length && !row.model_scope_empty); setValues(allowed); setDetected(row.detected_models); setStatus('')
  }, [open, row])
  // 已有范围里不在当前模型表中的条目也保留为可选项，避免打开即丢失。
  const merged = useMemo(() => { const set = new Map(options.map((item) => [item.value, item])); for (const value of values) if (!set.has(value)) set.set(value, { value, label: value }); return [...set.values()] }, [options, values])
  const detect = async () => {
    if (!row?.api_key.trim()) return
    const targets = urls.filter((entry) => entry.url.trim()).map((entry) => ({ url: entry.url.trim(), exact: Boolean(entry.exact), protocols: entry.protocols ?? [] }))
    if (!targets.length) { setStatus('请先填写 URL'); return }
    setBusy(true); setStatus('探测中…')
    try {
      const result = await fetchModelsPreview({ urls: targets, api_keys: [row.api_key.trim()], per_key: true, proxy_url: proxyURL.trim() || undefined })
      const upstream = new Set((result.key_models?.[0]?.models ?? toEntries(result.models)).flatMap((entry) => [entry.model, entry.redirect_model].filter(Boolean).map((name) => String(name).toLowerCase())))
      const hits = options.filter((item) => upstream.has(item.value.toLowerCase())).map((item) => item.value)
      if (!hits.length) { setStatus('该 Key 未探测到任何已配置模型'); return }
      setValues(hits); setDetected([...upstream]); setAllowAll(false); setStatus(`命中 ${hits.length} 个模型`)
    } catch (cause) { setStatus(cause instanceof Error ? cause.message : '探测失败') } finally { setBusy(false) }
  }
  const apply = () => {
    if (!allowAll && JSON.stringify(values).length > 8000) { setStatus('模型范围过长，请减少选择'); return }
    onApply(allowAll ? { allowed_models: [], model_scope_empty: false, detected_models: detected } : { allowed_models: values, model_scope_empty: values.length === 0, detected_models: detected })
    onClose()
  }
  return (
    <Dialog open={open} onClose={onClose} size="md" title={`Key 模型范围：${row ? maskKey(row.api_key) : ''}`}
      footer={<><button className="btn" onClick={onClose}>取消</button><button className="btn btn-primary" onClick={apply}>确定</button></>}>
      <label><input type="checkbox" checked={allowAll} onChange={(event) => setAllowAll(event.target.checked)} /> 允许全部渠道模型</label>
      <div className="toolbar">
        <SearchableMultiSelect ariaLabel="允许的模型" className="combobox-wide" disabled={allowAll} values={values} options={merged} onChange={setValues} placeholder="选择允许的模型" />
        <button className="btn" disabled={busy} onClick={() => void detect()}>探测</button>
      </div>
      {status && <p className="muted" role="status">{status}</p>}
    </Dialog>
  )
}
