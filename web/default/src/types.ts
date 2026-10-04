export interface Session {
  role?: string
  show_channels?: boolean
  authenticated?: boolean
  allowed_models?: string[]
  description?: string
  default_test_content?: string
  [key: string]: unknown
}

export interface Channel {
  id: number
  name: string
  /** api_key 或各 OAuth 类型，见 internal/model/config.go:NormalizeAuthType */
  auth_type?: string
  status?: string
  enabled?: boolean
  priority?: number
  models?: Array<string | { model?: unknown; redirect_model?: unknown; name?: unknown }>
  [key: string]: unknown
}

export interface ChannelURLStat {
  url: string
  latency_ms?: number
  cooled_down?: boolean
  cooldown_remain_ms?: number
  requests?: number
  failures?: number
  disabled?: boolean
}

export interface StatsEntry {
  channel_id?: number
  channel_name?: string
  model?: string
  success?: number
  error?: number
  total?: number
  total_cost?: number
  [key: string]: unknown
}

export interface LogEntry {
  id?: number
  created_at?: string
  channel_name?: string
  model?: string
  status_code?: number
  duration?: number
  message?: string
  [key: string]: unknown
}

export interface DashboardSummary {
  by_client_protocol?: Record<string, Record<string, number>>
  by_auth_type?: Record<string, Record<string, number>>
  rpm_stats?: Record<string, number>
  [key: string]: unknown
}
