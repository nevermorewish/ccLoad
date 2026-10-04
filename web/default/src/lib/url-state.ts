/** 读取当前 URL 查询参数（旧版各页筛选都以 URL 为准，其次才是 localStorage）。 */
export function readQuery(): URLSearchParams {
  return new URLSearchParams(window.location.search)
}

/** 将筛选写回 URL。空值不写；push=false 时 replaceState，不产生历史记录。 */
export function writeQuery(params: Record<string, string | number | undefined | null>, push = false): void {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === null || value === '') continue
    query.set(key, String(value))
  }
  const search = query.toString()
  const next = `${window.location.pathname}${search ? `?${search}` : ''}${window.location.hash}`
  if (next === `${window.location.pathname}${window.location.search}${window.location.hash}`) return
  window.history[push ? 'pushState' : 'replaceState'](window.history.state, '', next)
}

export function readStored<T extends object>(key: string, fallback: T): T {
  try { const raw = localStorage.getItem(key); return raw ? { ...fallback, ...(JSON.parse(raw) as Partial<T>) } : fallback } catch { return fallback }
}
