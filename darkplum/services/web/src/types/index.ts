export interface User {
  id: string
  email: string
  name: string
  role: 'admin' | 'reader'
  created_at: string
  auth_source: 'sso' | 'manual'
}

export interface ApiKey {
  id: string
  user_id: string
  user_name?: string
  name: string
  prefix: string
  key?: string
  created_at: string
  revoked_at: string | null
}

export interface ChatMessage {
  role: 'user' | 'assistant'
  content: string
}

export interface Model {
  id: string
  object: string
  created: number
  owned_by: string
}

export interface AdminModel {
  id: string
  model_id: string
  provider: string
  display_name: string | null
  enabled: boolean
  discovered_at: string
  updated_at: string
}

export interface AuthConfig {
  local: boolean
  oidc?: { enabled: boolean }
  setup_required?: boolean
}

export interface ActivityEvent {
  id: string
  event_type: string
  actor_id: string | null
  actor_name: string | null
  actor_email: string | null
  target_type: string | null
  target_id: string | null
  detail: Record<string, string | number> | null
  ip_address: string | null
  request_id: string | null
  created_at: string
}

export interface InferenceEvent {
  id: string
  request_id: string
  api_key_id: string | null
  user_id: string | null
  user_name: string | null
  user_email: string | null
  model: string
  status_code: number
  latency_ms: number
  prompt_tokens: number | null
  completion_tokens: number | null
  stream: boolean
  ip_address: string | null
  created_at: string
}

export interface UsageBucket {
  bucket_time: string
  request_count: number
  success_count: number
  error_count: number
  total_prompt_tokens: number
  total_completion_tokens: number
  avg_latency_ms: number
  p95_latency_ms: number
}

export interface UsageSummary {
  total_requests: number
  total_requests_previous: number
  active_users: number
  total_users: number
  active_api_keys: number
  total_api_keys: number
  avg_latency_ms: number
  p95_latency_ms: number
}

export type TimeRange = '24h' | '7d' | '30d'

export interface UserUsageSummary {
  user_id: string
  user_name: string | null
  user_email: string | null
  request_count: number
  avg_latency_ms: number
  last_active: string
}

export interface KeyUsageSummary {
  api_key_id: string
  key_name: string | null
  key_prefix: string | null
  owner_name: string | null
  request_count: number
  avg_latency_ms: number
  last_active: string
}
