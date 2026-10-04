import { useEffect, useState } from 'react'
import { getJSON } from '../lib/api'
import { isAPITokenRole } from '../lib/auth'

/**
 * 令牌筛选选项（/admin/auth-tokens）。API Token 角色无权访问且后端会强制绑定自身令牌，
 * 与旧版 ui.js:initAuthTokenFilter 一致：此时不加载、调用方隐藏该筛选。
 */
export function useTokenOptions(): Array<{ value: string; label: string }> | null {
  const [options, setOptions] = useState<Array<{ value: string; label: string }> | null>(null)
  useEffect(() => {
    if (isAPITokenRole()) return
    void getJSON<{ tokens?: Array<{ id: number; description?: string }> }>('/admin/auth-tokens')
      .then((data) => setOptions((data.tokens ?? []).map((token) => ({ value: String(token.id), label: token.description || `令牌 #${token.id}` }))))
      .catch(() => setOptions([]))
  }, [])
  return isAPITokenRole() ? null : options ?? []
}
