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

api.interceptors.response.use((response) => {
  const payload = response.data
  if (payload && payload.success === false) {
    return Promise.reject(new Error(payload.error || '请求失败'))
  }
  return response
})

api.interceptors.request.use((config) => {
  const token = localStorage.getItem('ccload_token')
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
