import { CHANNEL_POOLS, channelPoolLabel } from './channels/api'

/** 与 internal/app/channel_pool_routing.go 的 ChannelRouteConfig 一一对应。 */
export type TokenTier = { min_tokens: number; max_tokens: number; pool: number }
export type QueueOverflow = { pool: number; max_queue: number; target_pool: number }
export type StickyMode = 'off' | 'tier' | 'session'
export type RouteRule = {
  id: string
  name: string
  enabled: boolean
  models: string[]
  default_pool: number
  token_tiers: TokenTier[]
  overflow: QueueOverflow[]
  sticky: StickyMode
  strict: boolean
  /** 首字超时秒数，0 表示沿用系统设置。 */
  first_byte_timeout_seconds: number
  first_byte_timeout_channel_id?: number
  non_stream_channel_id?: number
}
export type RouteConfig = { rules: RouteRule[] }
export type PoolView = { pool: number; name: string; channels: number; enabled: number; inflight: number; models: string[] }
export type InflightView = { rule_id: string; pool: number; inflight: number }
export type RouteTarget = { id: number; name: string; enabled: boolean }
export type RoutesView = { channels?: RouteTarget[]; config: RouteConfig; pools: PoolView[]; inflight: InflightView[] }
export type Simulation = {
  model: string
  matched: boolean
  rule_id: string
  rule_name: string
  tier: number
  input_tokens: number
  reason: string
  pools: Array<{ pool: number; name: string; inflight: number }>
  first_byte_timeout_seconds: number
  first_byte_timeout_channel_id?: number
  non_stream_channel_id?: number
}

/** 编辑草稿：Token 与队列保持输入原文（支持 8k / 50k 写法），保存时统一换算校验。 */
export type TierDraft = { min: string; max: string; pool: number }
export type OverflowDraft = { pool: number; maxQueue: string; target: number }
export type RuleDraft = {
  key: string
  id: string
  name: string
  enabled: boolean
  models: string[]
  defaultPool: number
  tiers: TierDraft[]
  overflow: OverflowDraft[]
  sticky: StickyMode
  strict: boolean
  firstByteTimeout: string
  firstByteTimeoutChannel: string
  nonStreamChannel: string
}

/** 与后端 maxChannelRouteFirstByteTimeoutSeconds 一致。 */
export const MAX_FIRST_BYTE_TIMEOUT_SECONDS = 3600

export const STICKY_MODES: Array<{ value: StickyMode; label: string; hint: string }> = [
  { value: 'off', label: '会话粘性：关闭', hint: '每个请求独立按 Token 档位与队列分流' },
  { value: 'tier', label: '会话粘性：同档位', hint: '同一会话在同一 Token 档位内固定渠道组（不受队列溢出影响），档位变化时重新分流' },
  { value: 'session', label: '会话粘性：整会话', hint: '同一会话固定在首次成功的渠道组，直到该组不可用或 1 小时无请求' },
]

export const ROUTE_REASONS: Record<string, string> = {
  default: '默认渠道组', token_tier: '输入 Token 档位', overflow: '队列溢出', sticky: '会话粘性',
}

export const POOL_OPTIONS = CHANNEL_POOLS.map((pool) => ({ value: String(pool.value), label: pool.label }))

let draftSequence = 0
const nextKey = () => `rule-${Date.now().toString(36)}-${++draftSequence}`

/** 解析 Token/队列输入：支持千分位、k（千）与 m（百万）后缀；空串返回 0，非法返回 null。 */
export function parseTokenInput(raw: string): number | null {
  const text = raw.trim().replace(/[,_\s]/g, '').toLowerCase()
  if (!text) return 0
  const match = /^(\d+(?:\.\d+)?)([km]?)$/.exec(text)
  if (!match) return null
  const scale = match[2] === 'k' ? 1000 : match[2] === 'm' ? 1_000_000 : 1
  const value = Math.round(Number(match[1]) * scale)
  return Number.isSafeInteger(value) ? value : null
}

/** 解析首字超时秒数：只接受整数，空串返回 0（沿用系统设置），非法返回 null。 */
export function parseSecondsInput(raw: string): number | null {
  const text = raw.trim()
  if (!text) return 0
  if (!/^\d+$/.test(text)) return null
  const value = Number(text)
  return Number.isSafeInteger(value) ? value : null
}

/** 8000 → 8k，50000 → 50k，1500000 → 1.5m；不能整除时原样显示。 */
export function formatTokens(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return ''
  if (value % 1_000_000 === 0) return `${value / 1_000_000}m`
  if (value >= 1_000_000 && value % 100_000 === 0) return `${value / 1_000_000}m`
  if (value % 1000 === 0) return `${value / 1000}k`
  if (value >= 1000 && value % 100 === 0) return `${value / 1000}k`
  return String(value)
}

export function describeTier(tier: TokenTier): string {
  const min = tier.min_tokens > 0 ? `≥ ${formatTokens(tier.min_tokens)}` : ''
  const max = tier.max_tokens > 0 ? `< ${formatTokens(tier.max_tokens)}` : ''
  return [min, max].filter(Boolean).join(' 且 ') || '任意长度'
}

export function emptyRuleDraft(): RuleDraft {
  return { key: nextKey(), id: '', name: '', enabled: true, models: [], defaultPool: 0, tiers: [], overflow: [], sticky: 'tier', strict: false, firstByteTimeout: '', firstByteTimeoutChannel: '', nonStreamChannel: '' }
}

/** 示例：kimi-k3 输入 < 8k 走渠道1、≥ 50k 走渠道2，渠道1 进行中达到 100 时溢出到渠道2，同档位会话粘性。 */
export function exampleRuleDraft(): RuleDraft {
  return {
    ...emptyRuleDraft(), name: 'kimi-k3 分流', models: ['kimi-k3'],
    tiers: [{ min: '', max: '8k', pool: 1 }, { min: '50k', max: '', pool: 2 }],
    overflow: [{ pool: 1, maxQueue: '100', target: 2 }],
  }
}

export function ruleToDraft(rule: RouteRule): RuleDraft {
  const sticky = STICKY_MODES.some((mode) => mode.value === rule.sticky) ? rule.sticky : 'off'
  return {
    key: nextKey(), id: rule.id ?? '', name: rule.name ?? '', enabled: rule.enabled !== false,
    models: [...(rule.models ?? [])], defaultPool: Number(rule.default_pool ?? 0),
    tiers: (rule.token_tiers ?? []).map((tier) => ({ min: formatTokens(tier.min_tokens), max: formatTokens(tier.max_tokens), pool: Number(tier.pool ?? 0) })),
    overflow: (rule.overflow ?? []).map((item) => ({ pool: Number(item.pool ?? 0), maxQueue: String(item.max_queue ?? ''), target: Number(item.target_pool ?? 0) })),
    sticky, strict: Boolean(rule.strict),
    nonStreamChannel: rule.non_stream_channel_id ? String(rule.non_stream_channel_id) : '',
    firstByteTimeoutChannel: rule.first_byte_timeout_channel_id ? String(rule.first_byte_timeout_channel_id) : '',
    firstByteTimeout: Number(rule.first_byte_timeout_seconds) > 0 ? String(rule.first_byte_timeout_seconds) : '',
  }
}

/** 把模型输入（逗号/空白/换行分隔）合并进已有列表，大小写无关去重。 */
export function addModels(current: string[], raw: string): string[] {
  const result = [...current]
  const seen = new Set(current.map((item) => item.toLowerCase()))
  for (const part of raw.split(/[\s,，;；]+/)) {
    const name = part.trim()
    if (!name || seen.has(name.toLowerCase())) continue
    seen.add(name.toLowerCase())
    result.push(name)
  }
  return result
}

/**
 * 草稿转提交载荷并做前端校验（与后端 normalizeChannelRouteRule 一致，后端仍为准）。
 * 返回第一条错误，前缀标明规则序号，便于定位。
 */
export function draftsToConfig(drafts: RuleDraft[]): { config?: RouteConfig; error?: string } {
  const rules: RouteRule[] = []
  for (const [index, draft] of drafts.entries()) {
    const label = `规则 ${index + 1}${draft.name.trim() ? `「${draft.name.trim()}」` : ''}`
    const models = addModels([], draft.models.join(','))
    if (!models.length) return { error: `${label}：至少填写一个模型` }
    const tiers: TokenTier[] = []
    for (const [tierIndex, tier] of draft.tiers.entries()) {
      const min = parseTokenInput(tier.min)
      const max = parseTokenInput(tier.max)
      if (min === null || max === null) return { error: `${label}：第 ${tierIndex + 1} 个 Token 档位格式无效（示例：8000、8k、50k）` }
      if (min === 0 && max === 0) return { error: `${label}：第 ${tierIndex + 1} 个 Token 档位至少填写下限或上限` }
      if (max > 0 && max <= min) return { error: `${label}：第 ${tierIndex + 1} 个 Token 档位的上限必须大于下限` }
      tiers.push({ min_tokens: min, max_tokens: max, pool: tier.pool })
    }
    const overflow: QueueOverflow[] = []
    const sources = new Set<number>()
    for (const [itemIndex, item] of draft.overflow.entries()) {
      const maxQueue = parseTokenInput(item.maxQueue)
      if (maxQueue === null || maxQueue < 1) return { error: `${label}：第 ${itemIndex + 1} 条队列溢出的队列上限必须为正整数` }
      if (item.pool === item.target) return { error: `${label}：第 ${itemIndex + 1} 条队列溢出的目标渠道组不能与来源相同` }
      if (sources.has(item.pool)) return { error: `${label}：${channelPoolLabel(item.pool)} 配置了多条队列溢出` }
      sources.add(item.pool)
      overflow.push({ pool: item.pool, max_queue: maxQueue, target_pool: item.target })
    }
    const firstByteTimeout = parseSecondsInput(draft.firstByteTimeout)
    if (firstByteTimeout === null || firstByteTimeout > MAX_FIRST_BYTE_TIMEOUT_SECONDS) {
      return { error: `${label}：首字超时必须是 0–${MAX_FIRST_BYTE_TIMEOUT_SECONDS} 之间的整数秒（留空沿用系统设置）` }
    }
    const target = parseSecondsInput(draft.firstByteTimeoutChannel ?? '')
    if (target === null || (target > 0 && firstByteTimeout === 0)) return { error: `${label}：指定超时渠道时必须填写正整数秒数和有效渠道` }
    const nonStreamTarget = parseSecondsInput(draft.nonStreamChannel ?? '')
    if (nonStreamTarget === null) return { error: `${label}：非流式请求必须指定有效渠道` }
    rules.push({
      ...(nonStreamTarget > 0 ? { non_stream_channel_id: nonStreamTarget } : {}),
      ...(target > 0 ? { first_byte_timeout_channel_id: target } : {}),
      id: draft.id, name: draft.name.trim(), enabled: draft.enabled, models, default_pool: draft.defaultPool,
      token_tiers: tiers, overflow, sticky: draft.sticky, strict: draft.strict, first_byte_timeout_seconds: firstByteTimeout,
    })
  }
  return { config: { rules } }
}

/** 规则摘要：一行说明分流效果，列表折叠时也能看出规则意图。 */
export function summarizeRule(rule: RouteRule): string {
  const parts = rule.token_tiers.map((tier) => `${describeTier(tier)} → ${channelPoolLabel(tier.pool)}`)
  parts.push(`${rule.token_tiers.length ? '其余' : '全部'} → ${channelPoolLabel(rule.default_pool)}`)
  for (const item of rule.overflow) parts.push(`${channelPoolLabel(item.pool)} 进行中 ≥ ${item.max_queue} 时优先 ${channelPoolLabel(item.target_pool)}`)
  if (rule.first_byte_timeout_seconds > 0) parts.push(rule.first_byte_timeout_channel_id ? `首字超过 ${rule.first_byte_timeout_seconds} 秒转到渠道 #${rule.first_byte_timeout_channel_id}，返回最终结果` : `首字超过 ${rule.first_byte_timeout_seconds} 秒切换渠道`)
  if (rule.non_stream_channel_id) parts.push(`非流式请求直接使用渠道 #${rule.non_stream_channel_id}，返回最终结果`)
  return parts.join('；')
}

export function inflightFor(inflight: InflightView[], ruleID: string, pool: number): number {
  return inflight.find((item) => item.rule_id === ruleID && item.pool === pool)?.inflight ?? 0
}

/** 用实时队列重算各渠道组的进行中总数（含未命中规则的请求）。 */
export function withPoolInflight(pools: PoolView[], inflight: InflightView[]): PoolView[] {
  return pools.map((pool) => ({ ...pool, inflight: inflight.filter((item) => item.pool === pool.pool).reduce((sum, item) => sum + item.inflight, 0) }))
}
