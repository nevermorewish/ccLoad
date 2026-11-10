export const RATE_LIMIT_LABELS: Record<string, string> = {
  ws_transcript: '会话记录容量超限',
  ws_sessions: '会话数超限',
  ws_connections: '连接数超限',
  token_rpm: '令牌 RPM 超限',
  quota: '额度不足',
  other: '其他限流 / 未识别',
}

export function logStatusOptions(codes: number[]) {
  return [
    { value: '', label: '所有状态码' },
    ...[...new Set([...codes, 429])].sort((a, b) => a - b).flatMap((code) => [
      { value: String(code), label: code === 429 ? '429 · 全部原因' : String(code) },
      ...(code === 429 ? Object.entries(RATE_LIMIT_LABELS).map(([category, label]) => ({ value: `429:${category}`, label: `429 · ${label}` })) : []),
    ]),
  ]
}
