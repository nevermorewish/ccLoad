import { getJSON, postJSON } from './api'
import type { Session } from '@/types'

const TOKEN_KEY = 'ccload_token'
const EXPIRY_KEY = 'ccload_token_expiry'
const ROLE_KEY = 'ccload_web_role'
const LEGACY_TOKEN_KEY = 'ccload_api_token'

/**
 * API Token 角色可访问的页面，与旧版 web-auth.js:API_TOKEN_NAV 对齐，另含旧版 token-model-test.js 提供的模型测试。
 * 渠道监控在旧版导航里出现但接口只对管理员开放（/admin 组 RequireAdminAuth），这里不再暴露。
 */
const API_TOKEN_PATHS = new Set(['/', '/stats', '/trend', '/logs', '/model-test'])

export async function getSession(): Promise<Session | null> {
  try {
    const session = await getJSON<Session>('/dashboard/session')
    if (session?.role) localStorage.setItem(ROLE_KEY, session.role)
    return session
  } catch {
    return null
  }
}

export function clearSession() {
  ;[TOKEN_KEY, EXPIRY_KEY, ROLE_KEY, LEGACY_TOKEN_KEY].forEach((key) => localStorage.removeItem(key))
}

export async function login(mode: 'admin' | 'api_token', credential: string): Promise<Session> {
  const result = await postJSON<{ token: string; expiresIn?: number; expires_in?: number; role?: string }>('/login',
    mode === 'api_token' ? { mode, token: credential.trim() } : { mode, password: credential })
  if (!result?.token) throw new Error('登录响应缺少会话令牌')
  // 先清掉旧会话（含旧版遗留键），避免角色残留。
  clearSession()
  localStorage.setItem(TOKEN_KEY, result.token)
  localStorage.setItem(EXPIRY_KEY, String(Date.now() + Number(result.expiresIn ?? result.expires_in ?? 0) * 1000))
  localStorage.setItem(ROLE_KEY, result.role ?? mode)
  return { role: result.role ?? mode, authenticated: true }
}

/** 退出：先清本地会话，再用原 token 通知后端失效（失败不影响退出）。 */
export async function logout(): Promise<void> {
  const token = localStorage.getItem(TOKEN_KEY)
  clearSession()
  if (!token) return
  try { await fetch('/logout', { method: 'POST', headers: { Authorization: `Bearer ${token}` } }) } catch { /* 网络失败时本地会话已清除 */ }
}

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function isReadOnlySession(session: Session | null): boolean {
  return session?.role === 'api_token' && session.show_channels !== true
}

/** API Token 登录的只读角色：渠道页改读 /dashboard/* 并隐藏全部写操作。 */
export function isAPITokenRole(): boolean {
  return localStorage.getItem(ROLE_KEY) === 'api_token'
}

/** API Token 角色且未开启 show_channels 时，各页隐藏渠道维度（旧版 shouldHideChannels）。 */
export function shouldHideChannels(session: Session | null | undefined): boolean {
  return localStorage.getItem(ROLE_KEY) === 'api_token' && session?.show_channels !== true
}

export function canAccessPath(path: string): boolean {
  return !isAPITokenRole() || API_TOKEN_PATHS.has(path)
}

/** 登录后回跳：只接受同源站内路径，否则回到首页（旧版 getSafeRedirectPath）。 */
export function safeRedirectPath(redirect: string | null | undefined): string {
  const fallback = '/web/'
  const candidate = String(redirect ?? '').trim()
  if (!candidate.startsWith('/') || candidate.startsWith('//')) return fallback
  try {
    const url = new URL(candidate, window.location.origin)
    if (url.origin !== window.location.origin || url.pathname.startsWith('/web/login')) return fallback
    return `${url.pathname}${url.search}${url.hash}`
  } catch { return fallback }
}
