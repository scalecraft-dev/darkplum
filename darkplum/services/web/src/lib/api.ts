import type { User, ApiKey, ChatMessage, ActivityEvent, AliasRouteTest, BackendClass, InferenceEvent, InferencePurpose, UsageBucket, UsageSummary, UserUsageSummary, KeyUsageSummary, Model, AdminModel, AdminModelsResult, ModelAlias, ModelAliasInput, ProviderStatus, Residency, RoutingPolicy, PrincipalPolicy, EffectivePolicy, PolicyPreview, PolicyPrincipalsPage, ConversationSummary, ConversationFull, ConversationMessage, ConversationMessageInput, VendorName, SpendGroupBy, SpendReport, SpendTimelineGroupBy, ProviderBilling, ReconciliationGroupBy, ReconciliationReport, ModelPricesResult, ModelPrice, ModelPriceInput , SystemPromptSetting, LibraryItem, LibraryItemInput, LibraryItemUpdate, LibraryImportRequest, LibraryImportPreview, LibraryKind, LibraryVisibility, ToolEvent, ToolHostsSetting } from '../types'
import { parseRetryAfter } from './modelRouting'
import { ROUTES, buildPath, type QueryValue, type Route } from './routes'

/**
 * What this bundle can make sense of, declared on every request.
 *
 * Deploy order is not enforceable here — one repo, one `make deploy` — so the
 * server has to be able to tell an old bundle from a new one rather than assume
 * they moved together. Without this header the api leaves external_api models
 * out of every list it answers, which is what makes a stale cached bundle safe:
 * it never sees a vendor model, so it can never label one "Runs in your
 * infrastructure".
 *
 * It is a capability declaration and nothing more. It is not sent on
 * /v1/chat/completions, and it never affects what the gateway will permit —
 * enforcement is the routing policy's job, on every request, header or not.
 */
const CLASSES_HEADER = { 'X-Darkplum-Classes': 'external_api' } as const

/**
 * An error carrying the HTTP status so callers can branch on 400 / 404 / 409
 * without string-matching the message.
 */
export class ApiError extends Error {
  readonly status: number
  /** The parsed JSON error body, when there was one. Some refusals carry more
   *  than a sentence — a 409 from a routing-policy save lists `locked_out`
   *  principals — and the page showing the refusal wants that, not a toast. */
  readonly details?: unknown
  /** Seconds to wait, from a 429's Retry-After. Undefined on every other
   *  status: no other refusal has an interval to offer, and inventing one on
   *  a button would be worse than leaving it enabled. */
  readonly retryAfterSeconds?: number

  constructor(message: string, status: number, details?: unknown, retryAfterSeconds?: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.details = details
    this.retryAfterSeconds = retryAfterSeconds
  }

  /** `locked_out` from a routing-policy 409, or [] when this isn't one. */
  lockedOut(): string[] {
    const d = this.details as { locked_out?: unknown } | undefined
    return Array.isArray(d?.locked_out) ? d.locked_out.filter((x): x is string => typeof x === 'string') : []
  }

  /**
   * The closed-vocabulary code from `{"error":{"code":…}}`, or ''.
   *
   * Branch on this, never on the message. The message is prose the server may
   * reword; the code is the contract. It is also the only part of an error
   * body this client reads beside the message — nothing echoed back from a
   * request body is ever shown, so a key cannot reach the screen through an
   * error.
   */
  code(): string {
    const d = this.details as { error?: { code?: unknown } } | undefined
    const code = d?.error?.code
    return typeof code === 'string' ? code : ''
  }

  /** The `vendor_error` 422's two closed fields, or null when this isn't one. */
  vendorError(): { status: number | null; type: string | null } | null {
    const d = this.details as
      | { error?: { code?: unknown; vendor_http_status?: unknown; vendor_error_type?: unknown } }
      | undefined
    if (d?.error?.code !== 'vendor_error') return null
    const status = d.error.vendor_http_status
    const type = d.error.vendor_error_type
    return {
      status: typeof status === 'number' ? status : null,
      type: typeof type === 'string' ? type : null,
    }
  }

  /** The model ids a `models_enabled` 409 names, or []. */
  enabledModels(): string[] {
    const d = this.details as { error?: { models?: unknown } } | undefined
    const models = d?.error?.models
    return Array.isArray(models) ? models.filter((x): x is string => typeof x === 'string') : []
  }
}

/** The body as JSON if it is JSON, else undefined. */
function parseJSONBody(raw: string): unknown {
  const trimmed = raw.trim()
  if (!trimmed || !(trimmed.startsWith('{') || trimmed.startsWith('['))) return undefined
  try {
    return JSON.parse(trimmed) as unknown
  } catch {
    return undefined
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
    headers: {
      'Content-Type': 'application/json',
      'X-Requested-With': 'XMLHttpRequest',
      ...CLASSES_HEADER,
      ...fetchOptions.headers,
    },
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
      parseJSONBody(body),
      // Read on the one status that carries it. The vendor's own interval
      // reaches us through the gateway and the api unmodified; a 429 with no
      // header still gets a wait, decided here rather than invented per screen.
      res.status === 429 ? parseRetryAfter(res.headers.get('Retry-After')) : undefined,
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
    headers: { 'X-Requested-With': 'XMLHttpRequest', ...CLASSES_HEADER },
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
  /** The gateway's error code on a refused request, e.g. model_not_allowed. */
  denial_code?: string
  /** Where the request ran, as a comma-separated list of
   *  self_hosted | cloud_managed | external_api | unrecorded. Omitted means
   *  every row, recorded or not. */
  backend_class?: string
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

/** Estimated spend at list price, grouped one way. Every amount in the reply
 *  is an estimate from Darkplum's own records, not a vendor invoice. */
export function getSpend(params: {
  group_by: SpendGroupBy
  since: string
  until: string
  limit?: number
}): Promise<SpendReport> {
  return request<SpendReport>(ROUTES.usageSpend, { query: params })
}

/** Spend per time bucket × backend class in one read — the chart's data and,
 *  in `totals`, the whole period's tiles. Rows are chronological; `key` is
 *  the RFC3339 UTC bucket start, `backend_class` the class. `limit` counts
 *  buckets, not rows. */
export function getSpendTimeline(params: {
  group_by: SpendTimelineGroupBy
  since: string
  until: string
  limit?: number
}): Promise<SpendReport> {
  return request<SpendReport>(ROUTES.usageSpend, { query: params })
}

/** Darkplum's estimate beside each vendor's own usage and cost reports, plus
 *  where each vendor's pull stands. */
export function getReconciliation(params: {
  since: string
  until: string
  group_by: ReconciliationGroupBy
}): Promise<ReconciliationReport> {
  return request<ReconciliationReport>(ROUTES.usageReconciliation, { query: params })
}

/** Asks the api to pull every configured vendor's reports now (202). */
export function refreshReconciliation(): Promise<void> {
  return request<void>(ROUTES.refreshUsageReconciliation, { body: {} })
}

// Model prices

export function listModelPrices(): Promise<ModelPricesResult> {
  return request<ModelPricesResult>(ROUTES.listModelPrices)
}

/** Adds a row (no id) or replaces one going forward (with id). */
export function putModelPrice(row: ModelPriceInput): Promise<ModelPrice> {
  return request<ModelPrice>(ROUTES.putModelPrice, { body: row })
}

/** Retires a row from now on. Past estimates keep the price they were made at. */
export function deleteModelPrice(id: string): Promise<void> {
  return request<void>(ROUTES.deleteModelPrice, { params: { id } })
}

// Models

/** The chat-facing list, already filtered to what this person's policy allows.
 *  `default_model` is what the picker preselects. Absent on older APIs.
 *  `policy_filtered` is false when the per-user policy lookup failed and the
 *  list below is UNFILTERED — it may contain models the gateway will still
 *  refuse. Absent on older APIs, which never failed open like this. */
export function listModels(): Promise<{
  data: Model[]
  default_model?: string | null
  policy_filtered?: boolean
}> {
  return request<{ data: Model[]; default_model?: string | null; policy_filtered?: boolean }>(
    ROUTES.listChatModels,
  )
}

// Admin Models

/** Every vendor the UI always shows a row for, so a state is never absent
 *  merely because the gateway has not mentioned it. */
const VENDOR_NAMES = ['anthropic', 'openai'] as const

/** The vendor status block, read defensively: it comes from the gateway's last
 *  sync through the api, and a field the api could not fill must not throw away
 *  the catalogue it arrived with. An entry that is not an object is dropped;
 *  a vendor that is missing is added as not_configured, which is what "the
 *  gateway has never mentioned it" means. */
function normaliseProviders(raw: unknown): ProviderStatus[] {
  const byName = new Map<string, ProviderStatus>()
  if (Array.isArray(raw)) {
    for (const entry of raw) {
      if (typeof entry !== 'object' || entry === null) continue
      const p = entry as Partial<ProviderStatus>
      if (p.name !== 'anthropic' && p.name !== 'openai') continue
      byName.set(p.name, {
        name: p.name,
        configured: p.configured === true,
        state: typeof p.state === 'string' ? (p.state as ProviderStatus['state']) : 'not_configured',
        model_count: typeof p.model_count === 'number' ? p.model_count : 0,
        last_sync_at: typeof p.last_sync_at === 'string' ? p.last_sync_at : null,
        // A source this bundle does not know reads as 'none'. The card says
        // different things about a filter set from the command line and one
        // set here; guessing which would put the wrong instruction on screen.
        filter_source:
          p.filter_source === 'db' || p.filter_source === 'env' ? p.filter_source : 'none',
        filter: stringList(p.filter),
        installing_until: typeof p.installing_until === 'string' ? p.installing_until : null,
        // Absent on an api that predates the removal stamp, which is the same
        // answer as "nothing is being removed" — never inferred from anything
        // else, because inferring it would hide a card's actions.
        removing_until: typeof p.removing_until === 'string' ? p.removing_until : null,
        billing: normaliseBilling((entry as { billing?: unknown }).billing),
      })
    }
  }
  return VENDOR_NAMES.map(
    (name) =>
      byName.get(name) ?? {
        name,
        configured: false,
        state: 'not_configured' as const,
        model_count: 0,
        last_sync_at: null,
        filter_source: 'none' as const,
        filter: [],
        installing_until: null,
        removing_until: null,
      },
  )
}

/** The billing block, or undefined when the api did not send one (it predates
 *  billing keys). A state this bundle does not know is kept as-is; the card
 *  names it as unrecognised rather than guessing. */
function normaliseBilling(raw: unknown): ProviderBilling | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined
  const b = raw as Partial<ProviderBilling>
  return {
    configured: b.configured === true,
    state: typeof b.state === 'string' ? b.state : 'not_configured',
    last_pulled_at: typeof b.last_pulled_at === 'string' ? b.last_pulled_at : null,
  }
}

/** A list of ids from a response, with anything that is not a string dropped.
 *  These lists are rendered as a checklist and then written back as a filter,
 *  so one bad entry must not become a pattern nobody chose. */
function stringList(raw: unknown): string[] {
  return Array.isArray(raw) ? raw.filter((x): x is string => typeof x === 'string') : []
}

/**
 * The admin catalogue.
 *
 * Two shapes, and which one arrives is decided by the classes header this
 * client always sends:
 *   with it     {"data": [...rows], "providers": [...]}
 *   without it  [...rows]            (an older api, or a bundle that predates
 *                                     the header — not this one)
 *
 * We always send the header, so `data` is the shape we expect. The bare array
 * is still read rather than treated as a failure: a cached bundle talking to
 * an older api should show a catalogue without vendors, not an error page.
 * There is no third key guessed at — `data` is the contract.
 */
export async function listAdminModels(): Promise<AdminModelsResult> {
  const body = await request<
    | AdminModel[]
    | {
        data?: AdminModel[]
        providers?: unknown
        provider_features?: unknown
        key_install_transport_ok?: unknown
      }
  >(ROUTES.listAdminModels)
  if (Array.isArray(body)) {
    return {
      models: body,
      providers: normaliseProviders(undefined),
      providerFeatures: [],
      keyInstallTransportOk: null,
    }
  }
  return {
    models: body?.data ?? [],
    providers: normaliseProviders(body?.providers),
    // Absent on an older gateway or api, which is the same answer as "cannot
    // install a key from here" — and read as such, never assumed.
    providerFeatures: stringList(body?.provider_features),
    // Null, not false: an api that predates the field has not refused
    // anything, and the browser's own isSecureContext answers instead.
    keyInstallTransportOk:
      typeof body?.key_install_transport_ok === 'boolean' ? body.key_install_transport_ok : null,
  }
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

// Vendor provider keys (admin)
//
// The key is an argument to two of these and is held nowhere else: it goes
// into the JSON body of one request and the local goes out of scope when the
// promise settles. request() below sends it once — there is no retry in this
// client, so a key is never re-sent to a server that has already refused it —
// and nothing here logs a request body. On failure the caller sees the
// server's own `error.message` and `error.code`; no part of the request is
// echoed into an error, a toast or the URL.

/** What a vendor's API answers to, and which of those this filter would offer.
 *  `admitted` is `reachable` minus the non-chat models the gateway never
 *  catalogues, narrowed by the filter when one was sent. */
export interface VendorVerification {
  reachable: string[]
  admitted: string[]
}

/**
 * Checks a key against the vendor, through the gateway — the egress path the
 * key will really use, so a proxy or firewall that would break it in
 * production breaks it here instead of after it is stored.
 *
 * With no key, the installed one is used: that is how Change models lists what
 * is on offer without asking for the key again. Nothing is stored either way.
 */
export function verifyVendorKey(
  vendor: VendorName,
  body: { key?: string; model_filter?: string[]; workspace_id?: string } = {},
): Promise<VendorVerification> {
  return request<VendorVerification>(ROUTES.verifyProviderKey, { params: { vendor }, body })
}

/** The result of a successful install: what is now on offer, when the gateway
 *  should have it, and anything that did not go to plan without the install
 *  having failed (the ConfigMap mirror, the gateway refresh). */
export interface VendorKeyInstalled {
  admitted: string[]
  /** RFC3339. The card counts towards this rather than towards a local timer,
   *  so the progress survives a reload. */
  installing_until: string
  warnings?: string[]
}

/** Stores the key and the model filter together. Verified at the vendor before
 *  anything is written, so a mistyped key cannot replace a working one. */
export function installVendorKey(
  vendor: VendorName,
  body: { key: string; model_filter: string[]; workspace_id?: string },
): Promise<VendorKeyInstalled> {
  return request<VendorKeyInstalled>(ROUTES.putProviderKey, { params: { vendor }, body })
}

/** Changes which of the vendor's models are catalogued, using the key already
 *  installed. The key is not involved and is not asked for. `workspaceId` is
 *  Anthropic's: undefined leaves the stored one alone, "" clears it. */
export function setVendorFilter(
  vendor: VendorName,
  modelFilter: string[],
  workspaceId?: string,
): Promise<{ admitted: string[] }> {
  return request<{ admitted: string[] }>(ROUTES.putProviderFilter, {
    params: { vendor },
    body: { model_filter: modelFilter, workspace_id: workspaceId },
  })
}

/**
 * Removes the key. The vendor's name is typed by the administrator and sent as
 * `confirm`, so the api refuses a removal that some other page could have
 * triggered. `disable_models` answers the 409 this returns while any of the
 * vendor's models are still enabled — the models are turned off in the same
 * transaction rather than left pointing at a key that has gone.
 *
 * The model filter is kept: reinstalling a key should not also mean choosing
 * the models again.
 *
 * `warnings` is what happened around the removal without it having failed —
 * models left enabled, a mirror that could not be rewritten. The card shows
 * them verbatim while the removal is reaching the gateway; a 204 with no body
 * is the ordinary case and yields none.
 */
export async function removeVendorKey(
  vendor: VendorName,
  opts: { disableModels?: boolean } = {},
): Promise<{ warnings: string[] }> {
  const body = await request<{ warnings?: unknown } | undefined>(ROUTES.deleteProviderKey, {
    params: { vendor },
    body: { confirm: vendor, ...(opts.disableModels ? { disable_models: true } : {}) },
  })
  return { warnings: stringList(body?.warnings) }
}

// Vendor billing keys (admin session only)
//
// A different credential from the inference key above, with the same rules:
// the key is passed straight through on the body and never kept, echoed or
// logged here. Nothing a 422 says about it ever contains it.

/** One day of the vendor's own report, fetched with the candidate key and
 *  thrown away — proof the key can read what the job needs. */
export interface BillingKeyVerification {
  state: 'ok'
  sample: { day: string; buckets: unknown }
}

export function verifyBillingKey(vendor: VendorName, key: string): Promise<BillingKeyVerification> {
  return request<BillingKeyVerification>(ROUTES.verifyProviderBillingKey, {
    params: { vendor },
    body: { key },
  })
}

export function installBillingKey(vendor: VendorName, key: string): Promise<{ state: string }> {
  return request<{ state: string }>(ROUTES.putProviderBillingKey, { params: { vendor }, body: { key } })
}

export function removeBillingKey(vendor: VendorName): Promise<void> {
  return request<void>(ROUTES.deleteProviderBillingKey, { params: { vendor } })
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

export function getSystemPrompt(): Promise<SystemPromptSetting> {
  return request<SystemPromptSetting>(ROUTES.getSystemPrompt)
}

/** null (or blank) restores the built-in default. */
export function updateSystemPrompt(systemPrompt: string | null): Promise<SystemPromptSetting> {
  return request<SystemPromptSetting>(ROUTES.updateSystemPrompt, { body: { system_prompt: systemPrompt } })
}

/** The response carries `warnings` when the new policy leaves routed aliases
 *  with routes that will be refused, and `affected_principals` when it narrows
 *  what users and keys can reach. Still a success. */
export function updateRoutingPolicy(body: {
  residency: Residency
  allowed_models: string[] | null
  default_model: string | null
  /** Optional: omitted keeps what is stored, so a write that is not about it
   *  (enabling a vendor model) can never flip it. */
  params_mode?: 'lenient' | 'strict'
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
// writing it, write it, or drop the row so the principal inherits again. The
// API's plain GET .../routing-policy (stored row only) is not wrapped here:
// the UI never needs the row without the merged view, so it is a machine
// route -- see docs/api-routes.json.

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

/** `pinned_model` is the API's; webchat never sends it, since a routed alias
 *  classifies every message rather than holding a thread to one model. */
export function updateConversation(
  id: string,
  patch: { title?: string; model?: string; pinned_model?: string | null; library_item_ids?: string[] },
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
 * model_not_enabled, model_residency_violation, policy_unavailable) and vendor
 * failures (provider_rate_limited, provider_auth_failed, upstream_error,
 * upstream_timeout, unsupported_parameter) instead of showing a generic
 * failure.
 */
export class ChatError extends Error {
  readonly code?: string
  readonly type?: string
  readonly status?: number
  /** The request field a 400 unsupported_parameter names, from the error
   *  body's `param`. */
  readonly param?: string
  /** Seconds to wait, parsed from a 429's Retry-After. Undefined unless this
   *  is a rate limit: no other refusal has an interval to offer, and a number
   *  on one would be invented. */
  readonly retryAfterSeconds?: number
  /** model_requires_data_retention only: the concrete model AWS refused and,
   *  for a routed alias, the route that picked it. */
  readonly refusedModel?: string
  readonly route?: string
  /** library_item_not_found: the attached item ids the api could not use. */
  readonly missing?: string[]

  constructor(
    message: string,
    opts: {
      missing?: string[]
      code?: string
      type?: string
      status?: number
      param?: string
      retryAfterSeconds?: number
      refusedModel?: string
      route?: string
    } = {},
  ) {
    super(message)
    this.name = 'ChatError'
    this.code = opts.code
    this.missing = opts.missing
    this.type = opts.type
    this.status = opts.status
    this.param = opts.param
    this.retryAfterSeconds = opts.retryAfterSeconds
    this.refusedModel = opts.refusedModel
    this.route = opts.route
  }
}

/** A non-empty string off an error body, or undefined. */
function stringOrUndefined(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined
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
  /** X-Darkplum-Backend-Class — where the reply actually ran. The gateway is
   *  the only thing that knows: a routed alias resolves after the request has
   *  left, so nothing on this side can work it out from the model picked. */
  backendClass: BackendClass | null
  /** X-Darkplum-Provider — the provider kind that served it, which is the
   *  vendor's name on an external route. */
  provider: string | null
  /** X-Darkplum-Library — the library item ids the api attached to this turn.
   *  Empty when none were. */
  libraryUsed: string[]
}

/** X-Darkplum-Library is `<id>:<sha12>,...`; the ids are what the UI names. */
function readLibraryUsed(raw: string | null): string[] {
  if (!raw) return []
  return raw
    .split(',')
    .map((part) => part.trim().split(':')[0])
    .filter(Boolean)
}

/** Only the three wire values are accepted off a header. Anything else is null
 *  — 'no class reported' — rather than a class the UI would then label. */
function readBackendClass(raw: string | null): BackendClass | null {
  return raw === 'self_hosted' || raw === 'cloud_managed' || raw === 'external_api' ? raw : null
}

/** What a turn may carry beyond its messages. */
export interface StreamChatOptions {
  /** Library items attached to the conversation. The api resolves them
   *  against the signed-in person and refuses the turn (404
   *  library_item_not_found, `missing` ids in the body) if one is gone. */
  libraryItemIds?: string[]
  /** Tool activity the api reports while it runs a tool server on the
   *  model's behalf: the call, then its result. */
  onToolEvent?: (event: ToolEvent) => void
}

export async function streamChat(
  messages: ChatMessage[],
  model: string,
  onToken: (token: string) => void,
  onDone: () => void,
  onError: (err: Error) => void,
  signal: AbortSignal,
  onMeta?: (meta: ChatResponseMeta) => void,
  options: StreamChatOptions = {},
): Promise<void> {
  let res: Response
  const libraryHeader: Record<string, string> =
    options.libraryItemIds && options.libraryItemIds.length > 0
      ? { 'X-Darkplum-Library-Items': options.libraryItemIds.join(',') }
      : {}
  try {
    res = await fetch(pathOf(ROUTES.chatCompletions), {
      method: ROUTES.chatCompletions.method,
      // Sent here too so the header goes out on every request from one place,
      // but it is inert on this route: what may answer is decided by the
      // routing policy, and no header a caller sends can widen it.
      headers: {
        'Content-Type': 'application/json',
        'X-Requested-With': 'XMLHttpRequest',
        ...CLASSES_HEADER,
        ...libraryHeader,
      },
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
    backendClass: readBackendClass(res.headers.get('X-Darkplum-Backend-Class')),
    provider: res.headers.get('X-Darkplum-Provider'),
    libraryUsed: readLibraryUsed(res.headers.get('X-Darkplum-Library')),
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
    let param: string | undefined
    let refusedModel: string | undefined
    let route: string | undefined
    let missing: string[] | undefined
    const trimmed = raw.trim()
    if (trimmed.startsWith('{')) {
      try {
        const body = JSON.parse(trimmed) as {
          error?:
            | {
                message?: string
                code?: string
                type?: string
                param?: string
                model?: unknown
                route?: unknown
                missing?: unknown
              }
            | string
        }
        if (body.error && typeof body.error === 'object') {
          message = body.error.message
          code = body.error.code
          type = body.error.type
          param = typeof body.error.param === 'string' ? body.error.param : undefined
          refusedModel = stringOrUndefined(body.error.model)
          route = stringOrUndefined(body.error.route)
          missing = Array.isArray(body.error.missing)
            ? body.error.missing.filter((x): x is string => typeof x === 'string')
            : undefined
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
        param,
        refusedModel,
        route,
        missing,
        // Read only on the one status that can carry it. The vendor's own
        // interval reaches us through the gateway and the api unmodified; a
        // 429 that arrives without one still gets a wait, decided by the UI.
        retryAfterSeconds:
          res.status === 429 ? parseRetryAfter(res.headers.get('Retry-After')) : undefined,
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
            /** The api's own frames while it runs a tool: no choices, one
             *  event. An OpenAI SDK would ignore them; the chat shows them. */
            darkplum?: ToolEvent
            error?: {
              message: string
              code?: string
              type?: string
              param?: string
              model?: unknown
              route?: unknown
            }
          }
          if (parsed.error) {
            // No status and no Retry-After here: the response committed as a
            // 200 before this frame, so a mid-stream rate limit carries no
            // interval and the UI falls back to its own short wait.
            onError(
              new ChatError(parsed.error.message, {
                code: parsed.error.code,
                type: parsed.error.type,
                param: typeof parsed.error.param === 'string' ? parsed.error.param : undefined,
                refusedModel: stringOrUndefined(parsed.error.model),
                route: stringOrUndefined(parsed.error.route),
              }),
            )
            return
          }
          if (parsed.darkplum && (parsed.darkplum.type === 'tool_call' || parsed.darkplum.type === 'tool_result')) {
            options.onToolEvent?.(parsed.darkplum)
            continue
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

// ---------------------------------------------------------------------------
// The library
//
// Session-scoped like conversations: a private item that is not the signed-in
// person's comes back as a 404, never a 403. Shared items are readable by
// everyone in the organisation and editable by their owner only.
// ---------------------------------------------------------------------------

export function listLibrary(params: { kind?: LibraryKind; scope?: 'mine' | 'shared' | 'all'; q?: string } = {}): Promise<LibraryItem[]> {
  return request<LibraryItem[]>(ROUTES.listLibrary, { query: params })
}

export function createLibraryItem(body: LibraryItemInput): Promise<LibraryItem> {
  return request<LibraryItem>(ROUTES.createLibraryItem, { body })
}

export function getLibraryItem(id: string): Promise<LibraryItem> {
  return request<LibraryItem>(ROUTES.getLibraryItem, { params: { id } })
}

export function updateLibraryItem(id: string, body: LibraryItemUpdate): Promise<LibraryItem> {
  return request<LibraryItem>(ROUTES.updateLibraryItem, { params: { id }, body })
}

export function deleteLibraryItem(id: string): Promise<void> {
  return request<void>(ROUTES.deleteLibraryItem, { params: { id } })
}

/** `org` shares with everyone in the organisation; `private` takes it back. */
export function shareLibraryItem(id: string, visibility: LibraryVisibility): Promise<LibraryItem> {
  return request<LibraryItem>(ROUTES.shareLibraryItem, { params: { id }, body: { visibility } })
}

/** One item as a portable file: markdown with front matter, or JSON for a
 *  tool (never with its credential). */
export function exportLibraryItem(id: string): Promise<{ blob: Blob; filename: string }> {
  return requestBlob(ROUTES.exportLibraryItem, { params: { id }, fallbackName: `library-item-${id}.md` })
}

/** Everything the person can see, as a zip of those files. */
export function exportLibrary(): Promise<{ blob: Blob; filename: string }> {
  return requestBlob(ROUTES.exportLibrary, { fallbackName: 'darkplum-library.zip' })
}

/** One file. With dryRun the server only says what it would create. */
export function importLibraryItem(body: LibraryImportRequest, dryRun = false): Promise<LibraryImportPreview> {
  return request<LibraryImportPreview>(ROUTES.importLibraryItem, {
    body,
    query: dryRun ? { dry_run: 1 } : undefined,
  })
}

/** Connects to a tool item's server and records the tools it offers. The
 *  server's host must be on the administrator's allowed list
 *  (422 tool_host_not_allowed otherwise). */
export function verifyLibraryTool(id: string): Promise<LibraryItem> {
  return request<LibraryItem>(ROUTES.verifyLibraryTool, { params: { id } })
}

/** Stores the bearer token a tool server needs, in the cluster — never in
 *  the database, never shown again. The reply says it is set and its last
 *  four characters, nothing more. */
export function setLibraryToolCredential(id: string, token: string): Promise<LibraryItem> {
  return request<LibraryItem>(ROUTES.putLibraryToolCredential, { params: { id }, body: { token } })
}

export function removeLibraryToolCredential(id: string): Promise<void> {
  return request<void>(ROUTES.deleteLibraryToolCredential, { params: { id } })
}

// Governance (admin): shared items only. Private items are never listed here.

export function listSharedLibrary(): Promise<LibraryItem[]> {
  return request<LibraryItem[]>(ROUTES.listSharedLibrary)
}

export function adminUnshareLibraryItem(id: string): Promise<void> {
  return request<void>(ROUTES.adminUnshareLibraryItem, { params: { id } })
}

export function adminDeleteLibraryItem(id: string): Promise<void> {
  return request<void>(ROUTES.adminDeleteLibraryItem, { params: { id } })
}

export function getToolHosts(): Promise<ToolHostsSetting> {
  return request<ToolHostsSetting>(ROUTES.getToolHosts)
}

export function updateToolHosts(hosts: string[]): Promise<ToolHostsSetting> {
  return request<ToolHostsSetting>(ROUTES.updateToolHosts, { body: { hosts } })
}
