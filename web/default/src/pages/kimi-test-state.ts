export type KimiCase = { id: string; name: string; expected: string }
export type KimiResult = { id: string; status: string; expected?: string; actual?: string; duration_ms?: number; upload_ms?: number; request_bytes?: number; http_status?: number; request?: unknown; response?: string }
export type KimiMeta = { version?: string; channel_id?: number; channel?: string; key_index?: number; model?: string; upstream_model?: string; url?: string; total?: number; verdict?: string; concurrency?: number; timeout_seconds?: number; upload_mib?: number; upload_limit_seconds?: number }
export type KimiEvent = { type: string; time: string; message: string; id?: string; result?: KimiResult; meta?: KimiMeta }

// Retain incomplete UTF-8 and SSE frames across network chunks. A closed stream
// without a completed event is an interrupted run, never a successful one.
export async function readKimiEvents(body: ReadableStream<Uint8Array>, onEvent: (event: KimiEvent) => void) {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''; let completed = false
  const consume = () => {
    let match: RegExpExecArray | null
    while ((match = /\r?\n\r?\n/.exec(buffer))) {
      const frame = buffer.slice(0, match.index); buffer = buffer.slice(match.index + match[0].length)
      const data = frame.split(/\r?\n/).filter((line) => line.startsWith('data:')).map((line) => line.slice(5).trimStart()).join('\n')
      if (!data) continue
      const event = JSON.parse(data) as KimiEvent
      if (!event.type || !event.time) throw new Error('测试事件格式无效')
      if (event.type === 'completed') completed = true
      onEvent(event)
    }
  }
  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      buffer += decoder.decode(value, { stream: true }); consume()
    }
    buffer += decoder.decode(); consume()
    if (!completed) throw new Error('测试连接中断，尚未收到完成事件')
  } finally { await reader.cancel().catch(() => {}); reader.releaseLock() }
}

export function finishKimiResults(results: Record<string, KimiResult>, status: 'cancelled' | 'interrupted') {
  return Object.fromEntries(Object.entries(results).map(([id, result]) => [id, ['queued', 'running'].includes(result.status) ? { ...result, status, actual: status === 'cancelled' ? '用户取消测试' : '测试连接中断，结果未知' } : result]))
}
