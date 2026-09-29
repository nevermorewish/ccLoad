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
  type?: string
  status?: string
  enabled?: boolean
  priority?: number
  models?: string[]
  [key: string]: unknown
}

export interface ChannelForm {
  name: string
  urls: Array<{ url: string; protocol?: string; protocols?: string[]; exact?: boolean }>
  priority: number
  enabled: boolean
  models: Array<string | { model: string; redirect_model?: string; disabled?: boolean }>
  api_keys?: Array<string | { api_key: string; note?: string; allowed_models?: string[]; detected_models?: string[]; model_scope_empty?: boolean; cost_multiplier?: number; priority?: number; disabled?: boolean; cooldown_until?: number }>
  auth_type?: string
  key_strategy?: string
  rpm_limit?: number
  max_concurrency?: number
  proxy_url?: string
  websockets?: boolean
  protocol_transform_mode?: string
  retry_other_keys_on_failure?: boolean
  scheduled_check_enabled?: boolean
  scheduled_check_model?: string
  daily_cost_limit?: number
  custom_request_rules?: { headers?: Array<{ action: string; name: string; value?: string }>; body?: Array<{ action: string; path: string; value?: unknown }> }
  cooldown_detection_rules?: { rules?: Array<Record<string, unknown>> }
  management_account?: Record<string, unknown>
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
