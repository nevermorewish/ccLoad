import { redirectToLogin } from './api'
import { getToken } from './auth'

/**
 * 读取 SSE 响应并逐条回调。axios 的 responseType:'text' 会等整个响应结束才返回，
 * 批量进度因此只能在最后一次性出现；这里改用 fetch 流式读取，逐事件更新界面。
 */
export async function streamSSE<T>(url: string, body: unknown, onEvent: (event: T) => void, signal?: AbortSignal, method: 'GET' | 'POST' = 'POST'): Promise<void> {
  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  if (method === 'POST') headers['Content-Type'] = 'application/json'
  const token = getToken()
  if (token) headers.Authorization = `Bearer ${token}`
  const response = await fetch(url, { method, headers, credentials: 'include', body: method === 'POST' ? JSON.stringify(body) : undefined, signal })
  if (response.status === 401) { redirectToLogin(); throw new Error('登录已过期') }
  if (!response.ok || !response.body) {
    let message = `请求失败 (${response.status})`
    try { const payload = await response.json() as { error?: string }; if (payload?.error) message = payload.error } catch { /* 非 JSON 错误体沿用状态码文案 */ }
    throw new Error(message)
  }
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    const chunks = buffer.split(/\r?\n\r?\n/)
    buffer = chunks.pop() ?? ''
    for (const chunk of chunks) {
      const data = chunk.split(/\r?\n/).filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trim()).join('\n')
      if (!data || data === '[DONE]') continue
      try { onEvent(JSON.parse(data) as T) } catch { /* 跳过无法解析的事件，继续读取后续进度 */ }
    }
  }
}
