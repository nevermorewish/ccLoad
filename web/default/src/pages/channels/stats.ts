/** 列表统计列：按渠道聚合 /admin/stats 的逐模型行，权重与旧页 channels-data.js:aggregateChannelStats 一致。 */
export type ChannelStats = {
  success: number
  error: number
  total: number
  totalCost: number
  effectiveCost: number
  totalInputTokens: number
  totalOutputTokens: number
  avgFirstByteTimeSeconds?: number
  avgDurationSeconds?: number
  lastSuccessAt: number
  lastRequestAt: number
  lastRequestStatus: number | null
  lastRequestMessage: string
}

const num = (value: unknown) => { const parsed = Number(value); return Number.isFinite(parsed) ? parsed : 0 }
const ts = (value: unknown) => { if (value == null || value === '') return 0; const parsed = typeof value === 'number' ? value : Date.parse(String(value)); return Number.isFinite(parsed) ? (parsed < 1e12 ? parsed * 1000 : parsed) : 0 }

export function aggregateChannelStats(entries: Array<Record<string, unknown>>): Record<number, ChannelStats> {
  const result: Record<number, ChannelStats & { fbSum: number; fbWeight: number; durSum: number; durWeight: number }> = {}
  for (const entry of entries) {
    const id = Number(entry.channel_id)
    if (!Number.isFinite(id) || id <= 0) continue
    const stats = result[id] ??= { success: 0, error: 0, total: 0, totalCost: 0, effectiveCost: 0, totalInputTokens: 0, totalOutputTokens: 0, lastSuccessAt: 0, lastRequestAt: 0, lastRequestStatus: null, lastRequestMessage: '', fbSum: 0, fbWeight: 0, durSum: 0, durWeight: 0 }
    const success = num(entry.success); const total = num(entry.total)
    stats.success += success; stats.error += num(entry.error); stats.total += total
    const weight = success || total
    const firstByte = Number(entry.avg_first_byte_time_seconds)
    if (Number.isFinite(firstByte) && firstByte > 0 && weight > 0) { stats.fbSum += firstByte * weight; stats.fbWeight += weight }
    const duration = Number(entry.avg_duration_seconds)
    if (Number.isFinite(duration) && duration > 0 && weight > 0) { stats.durSum += duration * weight; stats.durWeight += weight }
    stats.totalInputTokens += num(entry.total_input_tokens)
    stats.totalOutputTokens += num(entry.total_output_tokens)
    stats.totalCost += num(entry.total_cost)
    stats.effectiveCost += entry.effective_cost != null ? num(entry.effective_cost) : num(entry.total_cost)
    stats.lastSuccessAt = Math.max(stats.lastSuccessAt, ts(entry.last_success_at))
    const lastRequestAt = ts(entry.last_request_at)
    if (lastRequestAt > stats.lastRequestAt) {
      stats.lastRequestAt = lastRequestAt
      stats.lastRequestStatus = entry.last_request_status == null ? null : num(entry.last_request_status)
      stats.lastRequestMessage = String(entry.last_request_message ?? '')
    }
  }
  const output: Record<number, ChannelStats> = {}
  for (const [id, { fbSum, fbWeight, durSum, durWeight, ...stats }] of Object.entries(result)) {
    output[Number(id)] = { ...stats, avgFirstByteTimeSeconds: fbWeight ? fbSum / fbWeight : undefined, avgDurationSeconds: durWeight ? durSum / durWeight : undefined }
  }
  return output
}

export const formatDuration = (seconds?: number) => seconds == null ? '-' : seconds >= 10 ? `${seconds.toFixed(1)}s` : `${seconds.toFixed(2)}s`
export const formatCost = (value: number) => value === 0 ? '$0' : value < 0.01 ? `$${value.toFixed(4)}` : `$${value.toFixed(2)}`
export const formatRemaining = (ms?: number) => {
  if (!ms || ms <= 0) return ''
  const seconds = Math.ceil(ms / 1000)
  if (seconds < 60) return `${seconds}s`
  const minutes = Math.ceil(seconds / 60)
  return minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h${minutes % 60 ? `${minutes % 60}m` : ''}`
}
