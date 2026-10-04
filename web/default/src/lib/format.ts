/** 后端时间可能是 Unix 秒（model.JSONTime）、毫秒或 RFC3339 字符串，统一转毫秒。 */
export function toMillis(value: unknown): number | null {
  if (value == null || value === '') return null
  if (typeof value === 'number') return Number.isFinite(value) && value > 0 ? (value < 1e12 ? value * 1000 : value) : null
  const numeric = Number(value)
  if (Number.isFinite(numeric) && /^\d+(\.\d+)?$/.test(String(value))) return toMillis(numeric)
  const parsed = Date.parse(String(value))
  return Number.isFinite(parsed) ? parsed : null
}

const pad = (n: number) => String(n).padStart(2, '0')

/** MM-DD HH:mm:ss，与旧版日志列一致。 */
export function formatDateTime(value: unknown, withYear = false): string {
  const ms = toMillis(value)
  if (ms == null) return '-'
  const d = new Date(ms)
  const date = `${withYear ? `${d.getFullYear()}-` : ''}${pad(d.getMonth() + 1)}-${pad(d.getDate())}`
  return `${date} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

export const formatInt = (value: unknown) => Number(value ?? 0).toLocaleString()

/** Token 等大数紧凑显示：1.2K / 3.4M / 5.6B。 */
export function formatCompact(value: unknown): string {
  const n = Number(value ?? 0)
  if (!Number.isFinite(n) || n === 0) return '0'
  const abs = Math.abs(n)
  if (abs >= 1e9) return `${(n / 1e9).toFixed(2)}B`
  if (abs >= 1e6) return `${(n / 1e6).toFixed(2)}M`
  if (abs >= 1e3) return `${(n / 1e3).toFixed(1)}K`
  return String(Math.round(n))
}

export function formatUSD(value: unknown): string {
  const n = Number(value ?? 0)
  if (!Number.isFinite(n) || n === 0) return '$0'
  return Math.abs(n) < 0.01 ? `$${n.toFixed(4)}` : `$${n.toFixed(2)}`
}

/** 标准成本与倍率后成本：两者相同只显示一个，不同时显示「倍率后（标准）」。 */
export function formatCostPair(standard: unknown, effective: unknown): string {
  const std = Number(standard ?? 0); const eff = effective == null ? std : Number(effective)
  return Math.abs(std - eff) < 1e-9 ? formatUSD(eff) : `${formatUSD(eff)}（标准 ${formatUSD(std)}）`
}

export const formatPercent = (part: number, total: number, digits = 1) => total > 0 ? `${((part / total) * 100).toFixed(digits)}%` : '-'

/** 缓存命中率：read / (input + read + creation)，与旧版 stats.js / logs.js 一致。 */
export function cacheHitRate(input: unknown, read: unknown, creation: unknown): string {
  const r = Number(read ?? 0); const denominator = Number(input ?? 0) + r + Number(creation ?? 0)
  return denominator > 0 ? `${((r / denominator) * 100).toFixed(1)}%` : '-'
}

export const formatSeconds = (value: unknown) => value == null || !Number.isFinite(Number(value)) || Number(value) <= 0 ? '-' : Number(value) >= 10 ? `${Number(value).toFixed(1)}s` : `${Number(value).toFixed(2)}s`
