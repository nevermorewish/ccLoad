import { useEffect, useState } from 'react'
import { SearchableSelect } from './searchable-select'

/**
 * 时间范围筛选。预设值与后端 internal/app/handlers.go:GetTimeRangeAt 一一对应；
 * 其他值（例如 all）会被后端静默当作 today，因此不提供。
 * 自定义区间点「应用」后才回调，避免起止为空时发出被降级为今天的请求。
 */
export const TIME_RANGES = [
  { value: 'today', label: '今天' },
  { value: 'yesterday', label: '昨天' },
  { value: 'day_before_yesterday', label: '前天' },
  { value: 'this_week', label: '本周' },
  { value: 'last_week', label: '上周' },
  { value: 'this_month', label: '本月' },
  { value: 'last_month', label: '上月' },
  { value: 'custom', label: '自定义' },
]

// 令牌列表接口把 all/空 当作累计统计（admin_auth_tokens.go），只有该页允许选择。
export const ALL_RANGE = { value: 'all', label: '全部（累计）' }
const VALID = new Set(TIME_RANGES.map((item) => item.value))

export type DateRangeValue = { range: string; start?: number; end?: number }

/** 统一归一化外部来源（URL、localStorage）的范围值，不认识的回落到 today。 */
export function normalizeRange(value: DateRangeValue | undefined, allowAll = false): DateRangeValue {
  const range = value?.range && (VALID.has(value.range) || (allowAll && value.range === ALL_RANGE.value)) ? value.range : 'today'
  if (range !== 'custom') return { range }
  const start = Number(value?.start); const end = Number(value?.end)
  return Number.isFinite(start) && Number.isFinite(end) && start > 0 && end > start ? { range, start, end } : { range: 'today' }
}

/** 转为后端查询参数：custom 时附带毫秒级 start_time / end_time。 */
export function rangeParams(value: DateRangeValue): Record<string, string | number> {
  return value.range === 'custom' && value.start && value.end ? { range: 'custom', start_time: value.start, end_time: value.end } : { range: value.range }
}

const toLocalInput = (ms?: number) => {
  if (!ms) return ''
  const date = new Date(ms)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`
}

export function DateRangeFilter({ value, onChange, includeAll = false }: { value: DateRangeValue; onChange: (value: DateRangeValue) => void; includeAll?: boolean }) {
  const [preset, setPreset] = useState(value.range)
  const [start, setStart] = useState(toLocalInput(value.start))
  const [end, setEnd] = useState(toLocalInput(value.end))
  const [error, setError] = useState('')
  useEffect(() => { setPreset(value.range); setStart(toLocalInput(value.start)); setEnd(toLocalInput(value.end)) }, [value.range, value.start, value.end])

  const choose = (next: string) => {
    setPreset(next); setError('')
    if (next !== 'custom') onChange({ range: next })
  }
  const apply = () => {
    const startMs = Date.parse(start)
    // 结束时间不超过当前时刻（与旧版日历一致）。
    const endMs = Math.min(Date.parse(end), Date.now())
    if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) { setError('请填写有效的起止时间，且结束晚于开始'); return }
    setError(''); onChange({ range: 'custom', start: startMs, end: endMs })
  }

  return <>
    <SearchableSelect ariaLabel="时间范围" className="combobox-inline" value={preset} options={includeAll ? [ALL_RANGE, ...TIME_RANGES] : TIME_RANGES} onChange={choose} />
    {preset === 'custom' && <>
      <input className="input" type="datetime-local" step={1} aria-label="开始时间" value={start} max={toLocalInput(Date.now())} onChange={(event) => setStart(event.target.value)} />
      <input className="input" type="datetime-local" step={1} aria-label="结束时间" value={end} max={toLocalInput(Date.now())} onChange={(event) => setEnd(event.target.value)} />
      <button className="btn" onClick={apply}>应用</button>
      {error && <span className="error-text">{error}</span>}
    </>}
  </>
}
