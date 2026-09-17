import type { User, ApiKey, ChatMessage, ActivityEvent, InferenceEvent, UsageBucket, UsageSummary, UserUsageSummary, KeyUsageSummary, Model, AdminModel } from '../types'

const BASE = '/api/v1'

async function fetchJSON<T>(
  path: string,
  options?: RequestInit & { baseUrl?: string },
): Promise<T> {
  const { baseUrl = BASE, ...fetchOptions } = options ?? {}
  const res = await fetch(`${baseUrl}${path}`, {
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest', ...fetchOptions?.headers },
    credentials: 'same-origin',
    ...fetchOptions,
  })
  if (res.status === 401) {
    window.location.href = '/login'
    return undefined as T
  }
  if (!res.ok) {
    const body = await res.text().catch(() => res.statusText)
    throw new Error(body || `HTTP ${res.status}`)
  }
  if (res.status === 204) return undefined as T
  const contentType = res.headers.get('Content-Type') ?? ''
  if (!contentType.includes('application/json')) {
    throw new Error('Unexpected response format')
  }
  return res.json() as Promise<T>
}

// Users

export function listUsers(): Promise<User[]> {
  return fetchJSON<User[]>('/users')
}

export function createUser(data: { name: string; email: string; password?: string }): Promise<User> {
  return fetchJSON<User>('/users', {
    method: 'POST',
    body: JSON.stringify(data),
  })
}

export function deleteUser(id: string): Promise<void> {
  return fetchJSON<void>(`/users/${id}`, { method: 'DELETE' })
}

export function resetUserPassword(id: string, password: string): Promise<{ message: string }> {
  return fetchJSON<{ message: string }>(`/users/${id}/password`, {
    method: 'PATCH',
    body: JSON.stringify({ password }),
  })
}

export function changePassword(currentPassword: string, newPassword: string): Promise<{ message: string }> {
  return fetch('/auth/change-password', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
    credentials: 'same-origin',
    body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
  }).then(async res => {
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: 'Failed' }))
      throw new Error((body as { error?: string }).error || `HTTP ${res.status}`)
    }
    return res.json() as Promise<{ message: string }>
  })
}

export function updateUserRole(id: string, role: 'admin' | 'reader'): Promise<{ role: string }> {
  return fetchJSON<{ role: string }>(`/users/${id}/role`, {
    method: 'PATCH',
    body: JSON.stringify({ role }),
  })
}

// API Keys

export function listApiKeys(): Promise<ApiKey[]> {
  return fetchJSON<ApiKey[]>('/api-keys')
}

export function createApiKey(data: { name: string; user_id: string }): Promise<ApiKey> {
  return fetchJSON<ApiKey>('/api-keys', {
    method: 'POST',
    body: JSON.stringify(data),
  })
}

export function revokeApiKey(id: string): Promise<void> {
  return fetchJSON<void>(`/api-keys/${id}`, { method: 'DELETE' })
}

// Activity & Usage

export function listActivity(params: {
  actor_id?: string
  event_type?: string
  since?: string
  until?: string
  limit?: number
  offset?: number
}): Promise<{ events: ActivityEvent[]; total: number }> {
  const searchParams = new URLSearchParams()
  Object.entries(params).forEach(([k, v]) => {
    if (v !== undefined) searchParams.set(k, String(v))
  })
  return fetchJSON<{ events: ActivityEvent[]; total: number }>(`/activity?${searchParams}`)
}

export function listUsage(params: {
  user_id?: string
  api_key_id?: string
  since?: string
  until?: string
  limit?: number
  offset?: number
}): Promise<{ events: InferenceEvent[]; total: number }> {
  const searchParams = new URLSearchParams()
  Object.entries(params).forEach(([k, v]) => {
    if (v !== undefined) searchParams.set(k, String(v))
  })
  return fetchJSON<{ events: InferenceEvent[]; total: number }>(`/usage?${searchParams}`)
}

export function getUsageStats(params: {
  since: string
  until: string
  interval: 'hour' | 'day'
  user_id?: string
  api_key_id?: string
}): Promise<UsageBucket[]> {
  const searchParams = new URLSearchParams()
  Object.entries(params).forEach(([k, v]) => {
    if (v !== undefined) searchParams.set(k, String(v))
  })
  return fetchJSON<UsageBucket[]>(`/usage/stats?${searchParams}`)
}

export function getUsageSummary(params: {
  since: string
  until: string
}): Promise<UsageSummary> {
  const searchParams = new URLSearchParams({ since: params.since, until: params.until })
  return fetchJSON<UsageSummary>(`/usage/summary?${searchParams}`)
}

export function getUserSummary(params: { since: string; until: string }): Promise<UserUsageSummary[]> {
  const searchParams = new URLSearchParams({ since: params.since, until: params.until })
  return fetchJSON<UserUsageSummary[]>(`/usage/user-summary?${searchParams}`)
}

export function getKeySummary(params: { since: string; until: string }): Promise<KeyUsageSummary[]> {
  const searchParams = new URLSearchParams({ since: params.since, until: params.until })
  return fetchJSON<KeyUsageSummary[]>(`/usage/key-summary?${searchParams}`)
}

// Models

export function listModels(): Promise<{ data: Model[] }> {
  return fetchJSON<{ data: Model[] }>('/models', { baseUrl: '/v1' })
}

// Admin Models

export function listAdminModels(): Promise<AdminModel[]> {
  return fetchJSON<AdminModel[]>('/models')
}

export function toggleModel(id: string, enabled: boolean): Promise<AdminModel> {
  return fetchJSON<AdminModel>(`/models/${id}`, {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  })
}

// Chat streaming

export async function streamChat(
  messages: ChatMessage[],
  model: string,
  onToken: (token: string) => void,
  onDone: () => void,
  onError: (err: Error) => void,
  signal: AbortSignal,
): Promise<void> {
  let res: Response
  try {
    res = await fetch('/v1/chat/completions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
      credentials: 'same-origin',
      body: JSON.stringify({ model, messages, stream: true }),
      signal,
    })
  } catch (err) {
    if ((err as Error).name === 'AbortError') return
    onError(err instanceof Error ? err : new Error(String(err)))
    return
  }

  if (res.status === 401) {
    window.location.href = '/login'
    return
  }

  if (!res.ok) {
    // Surface a clean, human-readable message. The gateway returns structured
    // JSON errors in two shapes:
    //   {"error":{"message":"...","type":"..."}}  (OpenAI-compat shape)
    //   {"error":"..."}                           (gateway unavailable / proxy shape)
    // For non-JSON responses (e.g. nginx HTML 502) we fall back to a generic
    // message — we never dump raw HTML into the toast.
    const contentType = res.headers.get('Content-Type') ?? ''
    if (contentType.includes('application/json')) {
      try {
        const body = await res.json() as { error?: { message?: string } | string }
        let message: string | undefined
        if (body.error && typeof body.error === 'object' && body.error.message) {
          message = body.error.message
        } else if (typeof body.error === 'string' && body.error) {
          message = body.error
        }
        onError(new Error(message || `Request failed (HTTP ${res.status})`))
      } catch {
        onError(new Error(`Request failed (HTTP ${res.status})`))
      }
    } else {
      onError(new Error(`Request failed (HTTP ${res.status})`))
    }
    return
  }

  const reader = res.body?.getReader()
  if (!reader) {
    onError(new Error('No response body'))
    return
  }

  const decoder = new TextDecoder()
  let buffer = ''

  try {
    while (true) {
      const { done, value } = await reader.read()
      if (done) break

      buffer += decoder.decode(value, { stream: true })
      const lines = buffer.split('\n')
      buffer = lines.pop() ?? ''

      for (const line of lines) {
        const trimmed = line.trim()
        if (!trimmed.startsWith('data:')) continue
        const data = trimmed.slice(5).trim()
        if (data === '[DONE]') {
          onDone()
          return
        }
        try {
          const parsed = JSON.parse(data) as {
            choices?: Array<{ delta: { content?: string } }>
            error?: { message: string }
          }
          if (parsed.error) {
            onError(new Error(parsed.error.message))
            return
          }
          const token = parsed.choices?.[0]?.delta?.content
          if (token) onToken(token)
        } catch {
          // Partial JSON or non-data line — skip
        }
      }
    }
  } catch (err) {
    if ((err as Error).name === 'AbortError') return
    onError(err instanceof Error ? err : new Error(String(err)))
    return
  } finally {
    reader.releaseLock()
  }

  onDone()
}
