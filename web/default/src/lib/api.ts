import axios from 'axios'

export const api = axios.create({
  baseURL: '/',
  withCredentials: true,
  headers: { Accept: 'application/json' },
  timeout: 30000,
})

type GetCacheEntry = { expiresAt: number; value: unknown }
const getCache = new Map<string, GetCacheEntry>()
const getInflight = new Map<string, Promise<unknown>>()
const GET_CACHE_TTL_MS = 750

function queryKey(url: string, params?: Record<string, unknown>): string {
  if (!params) return url
  const search = Object.entries(params)
    .filter(([, value]) => value !== undefined && value !== null)
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([key, value]) => `${encodeURIComponent(key)}=${encodeURIComponent(typeof value === 'string' ? value : JSON.stringify(value))}`)
    .join('&')
  return search ? `${url}?${search}` : url
}

export function invalidateGetCache(): void {
  getCache.clear()
}

/** 后端错误：保留 HTTP 状态和响应里的 data（如调试日志 404 时的不可用原因）。 */
export class ApiError extends Error {
  status: number
  data: unknown
  constructor(message: string, status = 0, data: unknown = undefined) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.data = data
  }
}

const SESSION_KEYS = ['ccload_token', 'ccload_token_expiry', 'ccload_web_role', 'ccload_api_token']

/** 会话失效：清本地会话并带回跳地址进入登录页（与旧版 ui.js:fetchWithAuth 一致）。 */
export function redirectToLogin(): void {
  SESSION_KEYS.forEach((key) => localStorage.removeItem(key))
  const path = window.location.pathname
  if (path === '/web/login' || path.startsWith('/web/login')) return
  const redirect = `${path}${window.location.search}${window.location.hash}`
  window.location.href = `/web/login?redirect=${encodeURIComponent(redirect)}`
}

// 登录与公开接口不需要会话，失效时也不应跳转。
const isPublicURL = (url = '') => /^\/*(login|logout|public\/|health)/.test(url)

api.interceptors.response.use((response) => {
  const payload = response.data
  if (payload && payload.success === false) {
    return Promise.reject(new ApiError(payload.error || '请求失败', response.status, payload.data))
  }
  return response
}, (error: unknown) => {
  if (axios.isCancel(error)) return Promise.reject(error)
  const response = axios.isAxiosError(error) ? error.response : undefined
  if (!response) return Promise.reject(new ApiError(error instanceof Error && error.message ? `网络错误：${error.message}` : '网络错误', 0))
  if (response.status === 401 && !isPublicURL(response.config?.url)) {
    redirectToLogin()
  }
  const payload = response.data as { error?: unknown; data?: unknown } | undefined
  // 后端统一返回 {success:false,error,data}；非 JSON 时退回状态文本。
  const message = payload && typeof payload === 'object' && typeof payload.error === 'string' && payload.error
    ? payload.error
    : `请求失败（HTTP ${response.status}）`
  return Promise.reject(new ApiError(message, response.status, payload && typeof payload === 'object' ? payload.data : undefined))
})

api.interceptors.request.use((config) => {
  const token = localStorage.getItem('ccload_token')
  const expiry = Number(localStorage.getItem('ccload_token_expiry') ?? 0)
  // 客户端先判断过期，避免带着失效 token 发出一串注定 401 的请求。
  if (token && expiry > 0 && Date.now() > expiry && !isPublicURL(config.url)) {
    redirectToLogin()
    return Promise.reject(new ApiError('登录已过期', 401))
  }
  if (token) config.headers.set('Authorization', `Bearer ${token}`)
  return config
})

export async function getJSON<T>(url: string, params?: Record<string, unknown>): Promise<T> {
  const key = queryKey(url, params)
  const cached = getCache.get(key)
  if (cached && cached.expiresAt > Date.now()) return cached.value as T
  const existing = getInflight.get(key)
  if (existing) return existing as Promise<T>
  const request = api.get<T>(url, { params }).then((response) => {
    const value = unwrap<T>(response.data)
    getCache.set(key, { expiresAt: Date.now() + GET_CACHE_TTL_MS, value })
    return value
  }).finally(() => getInflight.delete(key))
  getInflight.set(key, request)
  return request
}

export async function postJSON<T>(url: string, body?: unknown): Promise<T> {
  invalidateGetCache()
  try {
    const response = await api.post<T>(url, body)
    return unwrap<T>(response.data)
  } finally {
    invalidateGetCache()
  }
}

export async function putJSON<T>(url: string, body?: unknown): Promise<T> {
  invalidateGetCache()
  try {
    const response = await api.put<T>(url, body)
    return unwrap<T>(response.data)
  } finally {
    invalidateGetCache()
  }
}

export async function deleteJSON<T>(url: string): Promise<T> {
  invalidateGetCache()
  try {
    const response = await api.delete<T>(url)
    return unwrap<T>(response.data)
  } finally {
    invalidateGetCache()
  }
}

function unwrap<T>(value: unknown): T {
  if (value && typeof value === 'object' && 'data' in value && (value as { data?: unknown }).data !== undefined) {
    return (value as { data: T }).data
  }
  return value as T
}

export interface Paginated<T> {
  data: T[]
  count: number
}

export async function getPaginated<T>(url: string, params?: Record<string, unknown>): Promise<Paginated<T>> {
  const key = queryKey(url, params)
  const cached = getCache.get(key)
  if (cached && cached.expiresAt > Date.now()) return cached.value as Paginated<T>
  const existing = getInflight.get(key)
  if (existing) return existing as Promise<Paginated<T>>
  const request = api.get<Paginated<T>>(url, { params }).then((response) => {
    const value = response.data as unknown as { data?: T[]; count?: number }
    const result = { data: value.data ?? [], count: Number(value.count ?? 0) }
    getCache.set(key, { expiresAt: Date.now() + GET_CACHE_TTL_MS, value: result })
    return result
  }).finally(() => getInflight.delete(key))
  getInflight.set(key, request)
  return request
}
