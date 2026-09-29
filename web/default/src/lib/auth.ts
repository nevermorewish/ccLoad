import { getJSON, postJSON } from './api'
import type { Session } from '@/types'

const TOKEN_KEY = 'ccload_token'
const ROLE_KEY = 'ccload_web_role'

export async function getSession(): Promise<Session | null> {
  try {
    const session = await getJSON<Session>('/dashboard/session')
    if (session?.role) localStorage.setItem(ROLE_KEY, session.role)
    return session
  } catch {
    return null
  }
}

export async function login(mode: 'admin' | 'api_token', credential: string): Promise<Session> {
  const result = await postJSON<{ token: string; expiresIn?: number; expires_in?: number; role?: string }>('/login',
    mode === 'api_token' ? { mode, token: credential } : { mode, password: credential })
  if (!result?.token) throw new Error('登录响应缺少会话令牌')
  localStorage.setItem(TOKEN_KEY, result.token)
  localStorage.setItem('ccload_token_expiry', String(Date.now() + Number(result.expiresIn ?? result.expires_in ?? 0) * 1000))
  localStorage.setItem(ROLE_KEY, result.role ?? mode)
  return { role: result.role ?? mode, authenticated: true }
}

export function logout() {
  localStorage.removeItem(TOKEN_KEY)
  localStorage.removeItem('ccload_token_expiry')
  localStorage.removeItem(ROLE_KEY)
}

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function isReadOnlySession(session: Session | null): boolean {
  return session?.role === 'api_token' && session.show_channels !== true
}
