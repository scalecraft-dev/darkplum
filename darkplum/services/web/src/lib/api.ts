import type { User, ApiKey, ChatMessage, ActivityEvent, AliasRouteTest, InferenceEvent, InferencePurpose, UsageBucket, UsageSummary, UserUsageSummary, KeyUsageSummary, Model, AdminModel, ModelAlias, ModelAliasInput, Residency, RoutingPolicy, PrincipalPolicy, EffectivePolicy, PolicyPreview, PolicyPrincipalsPage, ConversationSummary, ConversationFull, ConversationMessage, ConversationMessageInput } from '../types'
import { ROUTES, buildPath, type QueryValue, type Route } from './routes'

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

async function fetchJSON<T>(path: string, options?: RequestInit): Promise<T> {
  const fetchOptions = options ?? {}
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest', ...fetchOptions.headers },
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

/** The one way a request URL is built: from a ROUTES entry, never a literal. */
function pathOf(
  route: Route,
  params?: Record<string, string>,
  query?: Record<string, QueryValue>,
): string {
  return buildPath(route, params, query)
}

/** JSON request against a route. The method comes from the table, so a call
 *  can never drift from the verb the parity check compares. */
function request<T>(
  route: Route,
  opts: {
    params?: Record<string, string>
    query?: Record<string, QueryValue>
    body?: unknown
  } = {},
): Promise<T> {
  const init: RequestInit = { method: route.method }
  if (opts.body !== undefined) init.body = JSON.stringify(opts.body)
  return fetchJSON<T>(pathOf(route, opts.params, opts.query), init)
}

/**
 * Downloads a route's response as a file. Read as a blob rather than linked to
 * directly so a failure lands in the UI instead of replacing the page with an
 * error body.
 */
async function requestBlob(
  route: Route,
  opts: { params?: Record<string, string>; query?: Record<string, QueryValue>; fallbackName: string },
): Promise<{ blob: Blob; filename: string }> {
  const res = await fetch(pathOf(route, opts.params, opts.query), {
    method: route.method,
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
  return { blob: await res.blob(), filename: match?.[1] || opts.fallbackName }
}

// Users

export function listUsers(): Promise<User[]> {
  return request<User[]>(ROUTES.listUsers)
}

export function createUser(data: {
  name: string
  email: string
  password?: string
  /** Omitted means the API's default, which is reader. */
  role?: 'admin' | 'reader'
}): Promise<User> {
  return request<User>(ROUTES.createUser, { body: data })
}

export function deleteUser(id: string): Promise<void> {
  return request<void>(ROUTES.deleteUser, { params: { id } })
}

export function resetUserPassword(id: string, password: string): Promise<{ message: string }> {
  return request<{ message: string }>(ROUTES.resetUserPassword, {
    params: { id },
    body: { password },
  })
}

export function changePassword(currentPassword: string, newPassword: string): Promise<{ message: string }> {
  return request<{ message: string }>(ROUTES.authChangePassword, {
    body: { current_password: currentPassword, new_password: newPassword },
  })
}

export function updateUserRole(id: string, role: 'admin' | 'reader'): Promise<{ role: string }> {
  return request<{ role: string }>(ROUTES.updateUserRole, { params: { id }, body: { role } })
}

// API Keys

export function listApiKeys(): Promise<ApiKey[]> {
  return request<ApiKey[]>(ROUTES.listApiKeys)
}

export function createApiKey(data: { name: string; user_id: string }): Promise<ApiKey> {
  return request<ApiKey>(ROUTES.createApiKey, { body: data })
}

export function revokeApiKey(id: string): Promise<void> {
  return request<void>(ROUTES.revokeApiKey, { params: { id } })
}

// Activity & Usage

/** Filters the activity log accepts. The export below takes the same shape, so
 *  what is on screen and what is downloaded can never disagree. */
export type ActivityFilters = {
  actor_id?: string
  event_type?: string
  since?: string
  until?: string
  limit?: number
  offset?: number
}

export function listActivity(
  params: ActivityFilters,
): Promise<{ events: ActivityEvent[]; total: number }> {
  return request<{ events: ActivityEvent[]; total: number }>(ROUTES.listActivity, { query: params })
}

export function exportActivity(
  params: Omit<ActivityFilters, 'limit' | 'offset'>,
): Promise<{ blob: Blob; filename: string }> {
  return requestBlob(ROUTES.exportActivity, { query: params, fallbackName: 'activity_events.csv' })
}

/** Filters the usage log accepts. `purpose` defaults to 'inference' on the
 *  server, so a caller that wants the classifier's own calls must ask for
 *  them: 'all' removes the filter. */
export type UsageFilters = {
  user_id?: string
  api_key_id?: string
  model?: string
  purpose?: InferencePurpose | 'all'
  since?: string
  until?: string
  limit?: number
  offset?: number
}

export function listUsage(
  params: UsageFilters,
): Promise<{ events: InferenceEvent[]; total: number }> {
  return request<{ events: InferenceEvent[]; total: number }>(ROUTES.listUsage, { query: params })
}

export function exportUsage(
  params: Omit<UsageFilters, 'limit' | 'offset'>,
): Promise<{ blob: Blob; filename: string }> {
  return requestBlob(ROUTES.exportUsage, { query: params, fallbackName: 'usage_events.csv' })
}

export function getUsageStats(params: {
  since: string
  until: string
  interval: 'hour' | 'day'
  user_id?: string
  api_key_id?: string
}): Promise<UsageBucket[]> {
  return request<UsageBucket[]>(ROUTES.usageStats, { query: params })
}

export function getUsageSummary(params: { since: string; until: string }): Promise<UsageSummary> {
  return request<UsageSummary>(ROUTES.usageSummary, { query: params })
}

export function getUserSummary(params: { since: string; until: string }): Promise<UserUsageSummary[]> {
  return request<UserUsageSummary[]>(ROUTES.usageUserSummary, { query: params })
}

export function getKeySummary(params: { since: string; until: string }): Promise<KeyUsageSummary[]> {
  return request<KeyUsageSummary[]>(ROUTES.usageKeySummary, { query: params })
}

// Models

/** The chat-facing list, already filtered to what this person's policy allows.
 *  `default_model` is what the picker preselects. Absent on older APIs. */
export function listModels(): Promise<{ data: Model[]; default_model?: string | null }> {
  return request<{ data: Model[]; default_model?: string | null }>(ROUTES.listChatModels)
}

// Admin Models

export function listAdminModels(): Promise<AdminModel[]> {
  return request<AdminModel[]>(ROUTES.listAdminModels)
}

export function toggleModel(id: string, enabled: boolean): Promise<AdminModel> {
  return request<AdminModel>(ROUTES.updateModel, { params: { id }, body: { enabled } })
}

/** Renames a model for everyone who picks one. null clears the name and the
 *  discovered model id is formatted for display again. */
export function setModelDisplayName(id: string, displayName: string | null): Promise<AdminModel> {
  return request<AdminModel>(ROUTES.updateModel, {
    params: { id },
    body: { display_name: displayName },
  })
}

// Model aliases (admin)

export function listModelAliases(): Promise<ModelAlias[]> {
  return request<ModelAlias[]>(ROUTES.listModelAliases)
}

/** Creates the alias, or replaces what it points at. A routed body replaces the
 *  whole set of routes in one write — there is no per-route endpoint. */
export function putModelAlias(alias: string, body: ModelAliasInput): Promise<ModelAlias> {
  return request<ModelAlias>(ROUTES.putModelAlias, { params: { alias }, body })
}

/** Runs one prompt through a routed alias's classifier and reports the route it
 *  would take. The target model is never called. */
export function testModelAlias(alias: string, text: string): Promise<AliasRouteTest> {
  return request<AliasRouteTest>(ROUTES.testModelAlias, { params: { alias }, body: { text } })
}

export function deleteModelAlias(alias: string): Promise<void> {
  return request<void>(ROUTES.deleteModelAlias, { params: { alias } })
}

// Routing policy — organisation (admin)

export function getRoutingPolicy(): Promise<RoutingPolicy> {
  return request<RoutingPolicy>(ROUTES.getRoutingPolicy)
}

/** The response carries `warnings` when the new policy leaves routed aliases
 *  with routes that will be refused, and `affected_principals` when it narrows
 *  what users and keys can reach. Still a success. */
export function updateRoutingPolicy(body: {
  residency: Residency
  allowed_models: string[] | null
  default_model: string | null
}): Promise<RoutingPolicy> {
  return request<RoutingPolicy>(ROUTES.updateRoutingPolicy, { body })
}

/** Every user and key with a policy row of its own, with the set each one can
 *  actually reach. Principals that inherit are not listed — they are the org. */
export function listPolicyPrincipals(params: {
  limit?: number
  offset?: number
} = {}): Promise<PolicyPrincipalsPage> {
  return request<PolicyPrincipalsPage>(ROUTES.listPolicyPrincipals, { query: params })
}

// Routing policy — per principal (admin)
//
// Four operations per principal, identical for users and keys: read the merged
// result (with the principal's own row alongside it), try a change without
// writing it, write it, or drop the row so the principal inherits again.

export function getUserPolicy(id: string): Promise<PrincipalPolicy> {
  return request<PrincipalPolicy>(ROUTES.getUserPolicy, { params: { id } })
}

export function putUserPolicy(id: string, body: PrincipalPolicy): Promise<PrincipalPolicy> {
  return request<PrincipalPolicy>(ROUTES.putUserPolicy, { params: { id }, body })
}

export function deleteUserPolicy(id: string): Promise<void> {
  return request<void>(ROUTES.deleteUserPolicy, { params: { id } })
}

export function getUserPolicyEffective(id: string): Promise<EffectivePolicy> {
  return request<EffectivePolicy>(ROUTES.userPolicyEffective, { params: { id } })
}

export function previewUserPolicy(id: string, body: PrincipalPolicy): Promise<PolicyPreview> {
  return request<PolicyPreview>(ROUTES.userPolicyPreview, { params: { id }, body })
}

export function getKeyPolicy(id: string): Promise<PrincipalPolicy> {
  return request<PrincipalPolicy>(ROUTES.getKeyPolicy, { params: { id } })
}

export function putKeyPolicy(id: string, body: PrincipalPolicy): Promise<PrincipalPolicy> {
  return request<PrincipalPolicy>(ROUTES.putKeyPolicy, { params: { id }, body })
}

export function deleteKeyPolicy(id: string): Promise<void> {
  return request<void>(ROUTES.deleteKeyPolicy, { params: { id } })
}

export function getKeyPolicyEffective(id: string): Promise<EffectivePolicy> {
  return request<EffectivePolicy>(ROUTES.keyPolicyEffective, { params: { id } })
}

export function previewKeyPolicy(id: string, body: PrincipalPolicy): Promise<PolicyPreview> {
  return request<PolicyPreview>(ROUTES.keyPolicyPreview, { params: { id }, body })
}

/** The signed-in person's own merged policy. Any session user may read it —
 *  it is what chat shows when someone asks why a model is missing. */
export function getMyPolicyEffective(): Promise<EffectivePolicy> {
  return request<EffectivePolicy>(ROUTES.myPolicyEffective)
}

// Conversations
//
// Session-scoped: every one of these reads and writes the signed-in user's own
// conversations. Another user's id comes back as a 404, never a 403.

export function listConversations(params: { limit?: number; offset?: number } = {}): Promise<ConversationSummary[]> {
  return request<ConversationSummary[]>(ROUTES.listConversations, { query: params })
}

export function createConversation(body: {
  title?: string
  model?: string
  import_key?: string
  messages?: ConversationMessageInput[]
}): Promise<ConversationFull> {
  return request<ConversationFull>(ROUTES.createConversation, { body })
}

export function getConversation(id: string): Promise<ConversationFull> {
  return request<ConversationFull>(ROUTES.getConversation, { params: { id } })
}

/** `pinned_model: null` clears the pin — the alias classifies again next turn. */
export function updateConversation(
  id: string,
  patch: { title?: string; model?: string; pinned_model?: string | null },
): Promise<ConversationSummary> {
  return request<ConversationSummary>(ROUTES.updateConversation, { params: { id }, body: patch })
}

/** Appends a user turn, or a user turn and the assistant turn that answered it.
 *  The server assigns seq; the response is the authority on where they landed. */
export function appendConversationMessages(
  id: string,
  messages: ConversationMessageInput[],
): Promise<{ messages: ConversationMessage[] }> {
  return request<{ messages: ConversationMessage[] }>(ROUTES.appendMessages, {
    params: { id },
    body: { messages },
  })
}

export function deleteConversation(id: string): Promise<void> {
  return request<void>(ROUTES.deleteConversation, { params: { id } })
}

/** Removes the turn at fromSeq and everything after it. */
export function truncateConversationMessages(id: string, fromSeq: number): Promise<void> {
  return request<void>(ROUTES.truncateMessages, {
    params: { id },
    query: { from_seq: fromSeq },
  })
}

/** The person's own copy of one conversation, as a JSON attachment. */
export function exportConversation(id: string): Promise<{ blob: Blob; filename: string }> {
  return requestBlob(ROUTES.getConversation, {
    params: { id },
    query: { format: 'export' },
    fallbackName: `conversation-${id}.json`,
  })
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
 *  record of the call that produced it; the model is the one that actually
 *  answered, which is the only way to know what a routed alias resolved to. */
export interface ChatResponseMeta {
  requestId: string | null
  /** X-Darkplum-Model — the resolved model id. */
  model: string | null
  /** X-Darkplum-Route — routed aliases only. */
  route: string | null
  /** X-Darkplum-Router-Reason — closed vocabulary, see routerReasonText(). */
  routerReason: string | null
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
    res = await fetch(pathOf(ROUTES.chatCompletions), {
      method: ROUTES.chatCompletions.method,
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

  onMeta?.({
    requestId: res.headers.get('X-Request-Id'),
    model: res.headers.get('X-Darkplum-Model'),
    route: res.headers.get('X-Darkplum-Route'),
    routerReason: res.headers.get('X-Darkplum-Router-Reason'),
  })

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
