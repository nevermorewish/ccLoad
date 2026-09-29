import axios from 'axios'

export const api = axios.create({
  baseURL: '/',
  withCredentials: true,
  headers: { Accept: 'application/json' },
})

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
  const response = await api.get<T>(url, { params })
  return unwrap<T>(response.data)
}

export async function postJSON<T>(url: string, body?: unknown): Promise<T> {
  const response = await api.post<T>(url, body)
  return unwrap<T>(response.data)
}

export async function putJSON<T>(url: string, body?: unknown): Promise<T> {
  const response = await api.put<T>(url, body)
  return unwrap<T>(response.data)
}

export async function deleteJSON<T>(url: string): Promise<T> {
  const response = await api.delete<T>(url)
  return unwrap<T>(response.data)
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
  const response = await api.get<Paginated<T>>(url, { params })
  const value = response.data as unknown as { data?: T[]; count?: number }
  return { data: value.data ?? [], count: Number(value.count ?? 0) }
}
