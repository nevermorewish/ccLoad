// The unavailable endpoint returns SystemSetting objects, whose value is a string.
const settingValue = (setting: unknown): unknown => setting && typeof setting === 'object'
  ? (setting as { value?: unknown }).value
  : setting

export function debugLogUnavailableHint(detail?: Record<string, unknown>): string {
  const enabledValue = settingValue(detail?.debug_log_enabled)
  const enabled = typeof enabledValue === 'boolean' ? enabledValue
    : ['true', '1'].includes(String(enabledValue).trim().toLowerCase()) ? true
      : ['false', '0'].includes(String(enabledValue).trim().toLowerCase()) ? false : undefined
  const retentionValue = settingValue(detail?.debug_log_retention_minutes)
  const retention = typeof retentionValue === 'string' || typeof retentionValue === 'number' ? Number(retentionValue) : NaN
  const retentionText = Number.isInteger(retention) && retention >= 1 && retention <= 1440 ? `，保留 ${retention} 分钟` : ''
  const state = enabled === undefined ? '无法读取当前调试日志开关' : `当前调试日志${enabled ? '已开启' : '未开启'}`
  const reason = enabled === false
    ? '开启后只会记录新的请求，无法补回此前未保存的原始请求和响应。'
    : '该请求可能未记录调试日志，或原始请求和响应已过期清理；当前设置不代表请求发生时的设置。'
  return `${state}${retentionText}。${reason}可在设置页调整调试日志开关与保留时长。`
}
