import type { User, ApiKey, ChatMessage, ActivityEvent, InferenceEvent, UsageBucket, UsageSummary, UserUsageSummary, KeyUsageSummary, Model, AdminModel, ModelAlias, Residency, RoutingPolicy, ConversationSummary, ConversationFull, ConversationMessage, ConversationMessageInput } from '../types'

const BASE = '/api/v1'

/**
 * An error carrying the HTTP status so callers can branch on 400 / 404 / 409
 * without string-matching the message.
 */
export class ApiError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

/**
 * Pull a human-readable message out of an error body. Two shapes exist:
 *   {"error":"..."}                          — the api service
 *   {"error":{"message":"...","code":"..."}} — the gateway (OpenAI envelope)
 * Anything else (HTML from a proxy, empty body) yields undefined so the caller
 * can fall back to a status-based message instead of dumping markup on screen.
 */
function extractErrorMessage(raw: string): string | undefined {
  const trimmed = raw.trim()
  if (!trimmed || !(trimmed.startsWith('{') || trimmed.startsWith('['))) return undefined
  try {
    const body = JSON.parse(trimmed) as { error?: { message?: string } | string; message?: string }
    if (body.error && typeof body.error === 'object' && body.error.message) return body.error.message
    if (typeof body.error === 'string' && body.error) return body.error
    if (typeof body.message === 'string' && body.message) return body.message
  } catch {
    // Not JSON after all.
  }
  return undefined
}

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
    const body = await res.text().catch(() => '')
    const plain = body.trim()
    // Plain-text bodies stay useful; HTML from a proxy never reaches the UI.
    const fallback = plain && !plain.startsWith('<') && plain.length <= 200 ? plain : ''
    throw new ApiError(
      extractErrorMessage(body) || fallback || res.statusText || `HTTP ${res.status}`,
      res.status,
    )
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

// Model aliases (admin)

export function listModelAliases(): Promise<ModelAlias[]> {
  return fetchJSON<ModelAlias[]>('/model-aliases')
}

/** Creates the alias, or points an existing one at a new target. */
export function putModelAlias(alias: string, targetModelId: string): Promise<ModelAlias> {
  return fetchJSON<ModelAlias>(`/model-aliases/${encodeURIComponent(alias)}`, {
    method: 'PUT',
    body: JSON.stringify({ target_model_id: targetModelId }),
  })
}

export function deleteModelAlias(alias: string): Promise<void> {
  return fetchJSON<void>(`/model-aliases/${encodeURIComponent(alias)}`, { method: 'DELETE' })
}

// Routing policy (admin)

export function getRoutingPolicy(): Promise<RoutingPolicy> {
  return fetchJSON<RoutingPolicy>('/routing-policy')
}

export function updateRoutingPolicy(residency: Residency): Promise<RoutingPolicy> {
  return fetchJSON<RoutingPolicy>('/routing-policy', {
    method: 'PUT',
    body: JSON.stringify({ residency }),
  })
}

// Conversations
//
// Session-scoped: every one of these reads and writes the signed-in user's own
// conversations. Another user's id comes back as a 404, never a 403.

export function listConversations(limit?: number): Promise<ConversationSummary[]> {
  const query = limit ? `?limit=${limit}` : ''
  return fetchJSON<ConversationSummary[]>(`/conversations${query}`)
}

export function createConversation(body: {
  title?: string
  model?: string
  import_key?: string
  messages?: ConversationMessageInput[]
}): Promise<ConversationFull> {
  return fetchJSON<ConversationFull>('/conversations', {
    method: 'POST',
    body: JSON.stringify(body),
  })
}

export function getConversation(id: string): Promise<ConversationFull> {
  return fetchJSON<ConversationFull>(`/conversations/${id}`)
}

export function updateConversation(
  id: string,
  patch: { title?: string; model?: string },
): Promise<ConversationSummary> {
  return fetchJSON<ConversationSummary>(`/conversations/${id}`, {
    method: 'PATCH',
    body: JSON.stringify(patch),
  })
}

/** Appends a user turn, or a user turn and the assistant turn that answered it.
 *  The server assigns seq; the response is the authority on where they landed. */
export function appendConversationMessages(
  id: string,
  messages: ConversationMessageInput[],
): Promise<{ messages: ConversationMessage[] }> {
  return fetchJSON<{ messages: ConversationMessage[] }>(`/conversations/${id}/messages`, {
    method: 'POST',
    body: JSON.stringify({ messages }),
  })
}

export function deleteConversation(id: string): Promise<void> {
  return fetchJSON<void>(`/conversations/${id}`, { method: 'DELETE' })
}

/** Removes the turn at fromSeq and everything after it. */
export function truncateConversationMessages(id: string, fromSeq: number): Promise<void> {
  return fetchJSON<void>(`/conversations/${id}/messages?from_seq=${fromSeq}`, {
    method: 'DELETE',
  })
}

/** The person's own copy of one conversation, as a JSON attachment. Read as a
 *  blob rather than linked to directly so a failure lands in the UI instead of
 *  replacing the page with an error body. */
export async function exportConversation(id: string): Promise<{ blob: Blob; filename: string }> {
  const res = await fetch(`${BASE}/conversations/${id}?format=export`, {
    headers: { 'X-Requested-With': 'XMLHttpRequest' },
    credentials: 'same-origin',
  })
  if (res.status === 401) {
    window.location.href = '/login'
    return { blob: new Blob(), filename: '' }
  }
  if (!res.ok) {
    const body = await res.text().catch(() => '')
    throw new ApiError(extractErrorMessage(body) || res.statusText || `HTTP ${res.status}`, res.status)
  }
  const disposition = res.headers.get('Content-Disposition') ?? ''
  const match = /filename="?([^";]+)"?/.exec(disposition)
  return { blob: await res.blob(), filename: match?.[1] || `conversation-${id}.json` }
}

// Chat streaming

/**
 * A chat failure carrying the gateway's OpenAI-style error code, so the
 * conversation UI can explain routing refusals (model_not_found,
 * model_not_enabled, model_residency_violation, policy_unavailable) instead of
 * showing a generic failure.
 */
export class ChatError extends Error {
  readonly code?: string
  readonly type?: string
  readonly status?: number

  constructor(message: string, opts: { code?: string; type?: string; status?: number } = {}) {
    super(message)
    this.name = 'ChatError'
    this.code = opts.code
    this.type = opts.type
    this.status = opts.status
  }
}

/** What the gateway said about the request itself, as soon as the response
 *  headers land. The request id ties a stored assistant turn to the audit
 *  record of the call that produced it. */
export interface ChatResponseMeta {
  requestId: string | null
}

export async function streamChat(
  messages: ChatMessage[],
  model: string,
  onToken: (token: string) => void,
  onDone: () => void,
  onError: (err: Error) => void,
  signal: AbortSignal,
  onMeta?: (meta: ChatResponseMeta) => void,
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

  onMeta?.({ requestId: res.headers.get('X-Request-Id') })

  if (!res.ok) {
    // Routing refusals (403/404/503) arrive as a JSON body even when the
    // request asked for stream=true — they are not delivered as SSE events.
    // Two shapes exist:
    //   {"error":{"message":"...","type":"...","code":"..."}}  (gateway)
    //   {"error":"..."}                                        (api / proxy)
    // For non-JSON responses (e.g. nginx HTML 502) we fall back to a generic
    // message — we never dump raw markup into the UI.
    const raw = await res.text().catch(() => '')
    let message: string | undefined
    let code: string | undefined
    let type: string | undefined
    const trimmed = raw.trim()
    if (trimmed.startsWith('{')) {
      try {
        const body = JSON.parse(trimmed) as {
          error?: { message?: string; code?: string; type?: string } | string
        }
        if (body.error && typeof body.error === 'object') {
          message = body.error.message
          code = body.error.code
          type = body.error.type
        } else if (typeof body.error === 'string' && body.error) {
          message = body.error
        }
      } catch {
        // Not JSON after all — fall through to the status message.
      }
    }
    onError(
      new ChatError(message || `Request failed (HTTP ${res.status})`, {
        code,
        type,
        status: res.status,
      }),
    )
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
            error?: { message: string; code?: string; type?: string }
          }
          if (parsed.error) {
            onError(new ChatError(parsed.error.message, { code: parsed.error.code, type: parsed.error.type }))
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
