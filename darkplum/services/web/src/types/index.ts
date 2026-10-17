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
  /** Assistant turns read back from history: the model the server recorded as
   *  having produced the reply. Display only — stripped before the turn is sent
   *  back to the gateway. */
  model?: string | null
  /** Routed aliases: the route the administrator wrote that this reply came
   *  through. Known for the turn that just arrived (the gateway names it in a
   *  response header); history stores the model only, so this is absent after a
   *  reload. Display only. */
  route?: string | null
  /** Routed aliases: the gateway's own word for why that route was picked, in
   *  the closed vocabulary (rules:… / fallback:… / model:…). Display only. */
  routerReason?: string | null
  /** Where this reply actually ran, from the gateway's response header. Known
   *  for the turn that just arrived; history stores the model only, so it is
   *  absent after a reload. Display only. */
  backendClass?: BackendClass | null
  /** The vendor that served it, from the gateway's response header — the
   *  provider kind (anthropic, openai, bedrock, vllm…). Display only. */
  provider?: string | null
  /** The model this conversation was pinned to had gone, so the alias chose
   *  again for this turn. Display only. */
  rerouted?: boolean
}

/** A conversation as the list endpoint returns it: no message bodies. */
export interface ConversationSummary {
  id: string
  title: string
  /** The model the conversation was last used with. '' when none was chosen. */
  model: string
  /** When the picked entry is a routed alias, the concrete model its first
   *  reply came from. Later turns in the same conversation go straight to that
   *  model, so a thread does not change model mid-answer. Absent on older APIs. */
  pinned_model?: string | null
  message_count: number
  created_at: string
  updated_at: string
}

export interface ConversationMessage {
  /** 1-based position in the conversation, assigned by the server. */
  seq: number
  role: 'user' | 'assistant'
  content: string
  /** Assistant turns: the model that produced the reply, as the server recorded it. */
  model: string | null
  /** Assistant turns: the gateway request this reply came from. */
  request_id: string | null
  created_at: string
}

export interface ConversationFull extends ConversationSummary {
  messages: ConversationMessage[]
}

/** A turn on its way to the server. The server fills in model and request_id
 *  from its own audit trail; these are sent for older servers that do not. */
export interface ConversationMessageInput {
  role: 'user' | 'assistant'
  content: string
  model?: string
  request_id?: string
}

/**
 * Where a model actually runs. Mirrors models.backend_class in the API, and is
 * exactly the closed wire vocabulary — nothing else may be sent or stored.
 *
 *   self_hosted   in your own cluster (vLLM, LM Studio)
 *   cloud_managed your cloud account (AWS Bedrock)
 *   external_api  the vendor's own API, called with your organisation's key
 */
export type BackendClass = 'self_hosted' | 'cloud_managed' | 'external_api'

/**
 * What the UI is willing to say about where a model runs. 'unknown' is a
 * display state only — a value no backend has reported, or one this bundle
 * does not recognise because the server is newer than it is. It is never sent
 * anywhere, and it is refused under every residency, so an unrecognised class
 * can never be mistaken for a safe one.
 */
export type DisplayClass = BackendClass | 'unknown'

/** A vendor Darkplum can hold an organisation-wide API key for. */
export type VendorName = 'anthropic' | 'openai'

/**
 * What the gateway last managed to do with one vendor's key, in the closed
 * vocabulary the wire contract defines. A vendor that is not working never
 * stops the gateway; it is reported here and in /health instead.
 */
export type ProviderState =
  | 'not_configured'
  | 'ok'
  | 'filter_missing'
  | 'filter_matched_nothing'
  | 'auth_failed'
  | 'unreachable'

/**
 * Where the model filter in force for a vendor was written.
 *
 *   db    the UI, or anything else that wrote through the api
 *   env   the command line (`make set-*-key MODELS=…`), mirrored into the
 *         gateway's environment
 *   none  no filter, which for a vendor means no models are offered
 *
 * Last writer wins in both directions, so this is a fact about the most recent
 * write and not a permanent mode. A value this bundle does not recognise reads
 * as 'none': claiming a filter came from somewhere is worse than saying the
 * filter's origin is unknown.
 */
export type FilterSource = 'db' | 'env' | 'none'

/** One vendor's status, as the admin models response passes it through from
 *  the gateway's last sync. Both vendors are always listed. */
export interface ProviderStatus {
  name: VendorName
  configured: boolean
  state: ProviderState
  model_count: number
  last_sync_at: string | null
  /** Absent on an older api — read as 'none'. */
  filter_source: FilterSource
  /** The patterns in force, as the source above wrote them. Empty when there
   *  is no filter. Shown, never guessed at: a family glob written from the
   *  command line is the operator's own words and is repeated verbatim. */
  filter: string[]
  /** RFC3339, set by an install or a filter change: until this moment the
   *  gateway may still be answering with the previous key or filter, so the
   *  card shows the change landing rather than the stale state underneath it.
   *  Null when nothing is in flight. */
  installing_until: string | null
  /** RFC3339, set when the key is deleted and cleared when one is installed.
   *  Until this moment the gateway may still be answering with the key that
   *  has just been removed, so the vendor reports itself configured and `ok`
   *  while nothing of the sort is true any more — the card reads this first.
   *  Null when no removal is in flight, and on an api that predates it. */
  removing_until: string | null
}

/** A route the administrator wrote, as the chat-facing model list exposes it:
 *  the name and what it is for. Targets are never named to end users. */
export interface ModelRouteSummary {
  route_name: string
  description: string
  /** Where this route's target runs, as the chat-facing GET /v1/models sends it
   *  (api routeSummary.backend_class). Not `target_backend_class` — that name
   *  belongs to the admin alias endpoints (ModelAliasRoute). A route without a
   *  class is treated as unknown, never as self-hosted. */
  backend_class?: BackendClass
}

export interface Model {
  id: string
  object: string
  created: number
  /** The provider kind that serves it: vllm | lmstudio | bedrock | anthropic |
   *  openai | mock. On an external_api model this is the vendor, and it is what
   *  the picker names when it says where a prompt goes. */
  owned_by: string
  /** Absent on an older gateway/API, and on anything no backend has reported.
   *  Read it through displayClassOf() — a missing or unrecognised value is
   *  'unknown', never a class that happens to sound safe. */
  backend_class?: BackendClass
  /** Present only on alias entries: the model id this alias resolves to. For a
   *  routed alias this is the default route's target. */
  alias_of?: string
  /** Present only on alias entries. Absent on older APIs — treat as 'static'. */
  alias_kind?: AliasKind
  /** Routed aliases only: the routes an administrator wrote for this alias. */
  routes?: ModelRouteSummary[]
}

export interface AdminModel {
  id: string
  model_id: string
  /** vllm | lmstudio | bedrock | anthropic | openai | mock. */
  provider: string
  display_name: string | null
  enabled: boolean
  discovered_at: string
  updated_at: string
  /** Absent on an older API — read it through displayClassOf(), which calls an
   *  absent or unrecognised value 'unknown' rather than guessing. */
  backend_class?: BackendClass
  /** False when the gateway has not seen this model for 3+ minutes. The row
   *  stays configured either way. Absent on older APIs — treat as reachable. */
  available?: boolean
}

/**
 * The admin catalogue, as the models endpoint answers it. The response used to
 * be a bare array and still is on an older API; a newer one wraps it so it can
 * carry the vendor status block alongside. Both shapes are read — see
 * listAdminModels() — and this is the normalised result.
 */
export interface AdminModelsResult {
  models: AdminModel[]
  /** Empty when the gateway never reported one (older gateway, or a sync that
   *  has not happened yet). Never partially trusted: an empty array means "not
   *  reported", which the Models tab says in those words. */
  providers: ProviderStatus[]
  /** What the gateway behind this api can do, passed through from its own
   *  providers block. Absent on an older gateway, which is exactly the case
   *  the Model vendors card has to recognise: without "ui_key_install" the
   *  install routes answer 409, so the card offers the command line instead of
   *  a form that would fail on submit. */
  providerFeatures: string[]
  /** Whether the api would accept a key over the transport this page is served
   *  on. False means it answers 403 `insecure_transport` to an install or a
   *  verify carrying a key, so the card shows the command line rather than a
   *  form. Null on an api that predates the field — read it through
   *  canInstallKeyOverTransport(), which falls back to the browser's own
   *  isSecureContext rather than assuming either answer. */
  keyInstallTransportOk: boolean | null
}

/** 'static' points at one model. 'routed' holds the routes an administrator
 *  wrote; a classifier picks among those routes and nothing else. */
export type AliasKind = 'static' | 'routed'

/** The health of one target, as the API reports it for aliases and routes. */
export interface AliasTargetHealth {
  target_exists: boolean
  target_enabled: boolean
  target_backend_class: BackendClass | null
  /** False when the target model is configured but currently unreachable.
   *  Absent on older APIs — treat as reachable. */
  target_available?: boolean
}

/** One route of a routed alias, as the API returns it. */
export interface ModelAliasRoute extends AliasTargetHealth {
  route_name: string
  description: string
  target_model_id: string
  /** Case-folded literal substrings, plus the reserved builtin: signals. */
  keywords: string[]
}

/** How a routed alias decides which route answers. Two can be written:
 *  'semantic_router' (the primary path) and 'rules'. 'model' is removed — only
 *  a pre-migration server still sends it, and the UI reads it as rules. */
export type AliasClassifierKind = 'rules' | 'semantic_router' | 'model'

/** The two kinds an administrator can choose. Read-only 'model' is not one. */
export type AliasClassifierChoice = 'rules' | 'semantic_router'

export type AliasClassifier =
  | { kind: 'rules'; model_id?: null }
  | { kind: 'semantic_router'; model_id?: null }
  | { kind: 'model'; model_id: string }

export interface ModelAlias extends AliasTargetHealth {
  alias: string
  /** Absent on older APIs — treat a missing value as 'static'. */
  kind?: AliasKind
  /** null on routed aliases: the target is whichever route is picked. */
  target_model_id: string | null
  /** Routed aliases only. */
  default_route?: string | null
  classifier?: AliasClassifier | null
  routes?: ModelAliasRoute[]
  /** Configure-time notes from the API, one readable sentence each — e.g. the
   *  Semantic Router is not installed on the gateway. Nothing is refused for
   *  them: the alias saves and answers on its default route. */
  warnings?: string[]
  created_at: string
  updated_at: string
}

/** What a route editor sends back. Order in the array is the route's position. */
export interface ModelAliasRouteInput {
  route_name: string
  description: string
  target_model_id: string
  keywords: string[]
}

/** The PUT body for an alias. A static alias is just its target. */
export type ModelAliasInput =
  | { target_model_id: string }
  | {
      kind: 'routed'
      default_route: string
      classifier: { kind: AliasClassifierChoice }
      routes: ModelAliasRouteInput[]
    }

/** The result of testing a prompt against a routed alias. No target model is
 *  called: this is the routing decision on its own. */
export interface AliasRouteTest {
  route_name: string
  target_model_id: string
  /** Closed vocabulary — run it through routerReasonText() before showing it. */
  reason: string
  latency_ms: number
  /** Set when the picked route would be refused as things stand. */
  would_be_refused: { code: string; message: string } | null
}

/**
 * Routing policy residency — a ladder, loosest first. Each rung refuses
 * everything the rung below it refuses, and more:
 *
 *   any                every known class
 *   cloud_account_only self-hosted and cloud-managed; refuses external_api
 *   self_hosted_only   self-hosted only
 *
 * A value this bundle does not recognise is read as the strictest rung, so a
 * newer server can never widen an older UI's idea of what is permitted.
 */
export type Residency = 'any' | 'cloud_account_only' | 'self_hosted_only'

/** The rungs a user or key may set on itself. A principal only ever narrows,
 *  so the loosest rung is not one of them; null inherits. */
export type PrincipalResidency = 'cloud_account_only' | 'self_hosted_only'

export interface RoutingPolicy {
  residency: Residency
  /** null means every enabled model. A list means only those model ids.
   *  Absent on older APIs — treat as null. */
  allowed_models?: string[] | null
  /** Used when a request names no model, and preselected in the chat picker.
   *  May be an alias. Absent on older APIs — treat as null. */
  default_model?: string | null
  updated_at: string
  /** Returned by PUT: routed aliases whose routes will now be refused. One
   *  readable sentence each, written by the API. */
  warnings?: string[]
  /** Returned by PUT when the new policy is tighter: how many users and keys
   *  WITH A POLICY OF THEIR OWN actually reach fewer models because of it —
   *  not merely how many have a row. Absent when the change did not tighten
   *  anything, or the gateway could not be asked. */
  affected_principals?: number
  /** Returned alongside `affected_principals`: whether everyone who simply
   *  inherits the organisation's policy — no row of their own — now reaches
   *  fewer models too. `affected_principals` never counts these people. */
  org_narrowed?: boolean
  /** Enabled, allowed models no backend has reported yet when the new
   *  residency narrows: their class is unknown, so the API neither counts them
   *  as lost nor as kept. Present only when non-empty. */
  unresolved_models?: string[]
}

// ---------------------------------------------------------------------------
// Per-principal routing policy
//
// The same three fields exist at the organisation, user and API-key level. A
// null field inherits from the level above; a set field only ever narrows.
// A key can never reach more than its owner. A user never more than the
// organisation.
// ---------------------------------------------------------------------------

export type PrincipalType = 'org' | 'user' | 'api_key'

/** One principal's own row, exactly as stored. null everywhere = inherits all. */
export interface PrincipalPolicy {
  /** null inherits. Only a rung tighter than what is inherited can be set. */
  residency: PrincipalResidency | null
  /** null inherits. Concrete model ids only — never aliases. */
  allowed_models: string[] | null
  /** null inherits. May be an alias. */
  default_model: string | null
  updated_at?: string
  updated_by?: string | null
}

/** Which level decided a field. 'none' means nothing anywhere set it. */
export type PolicySource = PrincipalType | 'none'

/** What the merge of the whole chain comes to, as the gateway computes it —
 *  the one implementation everything reads from. */
export interface EffectivePolicy {
  /** null means every model; a list means exactly those ids. */
  allowed_models: string[] | null
  residency: Residency
  default_model: string | null
  sources: {
    allowed_models: PolicySource
    residency: PolicySource
    default_model: PolicySource
  }
  /** Allowed ∧ routable ∧ enabled ∧ residency-ok, as things stand right now. */
  reachable_models: string[]
  /** Admin views only: the principal's own row, or null when it inherits. */
  stored?: PrincipalPolicy | null
}

/** One of the edited user's keys: what it is allowed before and after this
 *  change. Both are allowlists, computed from the policy merge — not the
 *  live reachable set, so a down backend never shows up as a key "losing"
 *  something here. */
export interface PolicyKeyImpact {
  id: string
  name: string
  before: string[]
  after: string[]
}

/**
 * The `effective` block of a preview response. The API returns the same shape
 * as the effective endpoint, but a preview is computed against a policy that
 * was never written and the parts drawn from live backend state can be absent
 * from an older or degraded API. They are optional here, and only here, so a
 * partial answer renders instead of throwing on the page that opened it.
 */
export interface PreviewEffectivePolicy
  extends Omit<EffectivePolicy, 'sources' | 'reachable_models'> {
  sources?: Partial<EffectivePolicy['sources']>
  reachable_models?: string[]
}

/** What a policy would do, computed without writing it. */
export interface PolicyPreview {
  effective: PreviewEffectivePolicy
  keys_affected: PolicyKeyImpact[]
  /** Principals left with nothing they can reach. Non-empty means the save is
   *  refused by the API (409), so the UI refuses it first. */
  would_lock_out: string[]
}

/** A row of the "Access overview" table: one explicit user or key policy. */
export interface PolicyPrincipal {
  principal_type: 'user' | 'api_key'
  user_id?: string | null
  api_key_id?: string | null
  /** Email for a user; key name for a key. Written by the API. */
  display: string
  /** Key rows: who owns the key. */
  owner?: string | null
  policy: PrincipalPolicy
  reachable_models: string[]
}

export interface PolicyPrincipalsPage {
  principals: PolicyPrincipal[]
  total: number
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

/** Why a recorded call was made. 'inference' answered someone; 'routing' is a
 *  classifier call that only chose a route; 'routing_dryrun' is an admin test. */
export type InferencePurpose = 'inference' | 'routing' | 'routing_dryrun'

export interface InferenceEvent {
  id: string
  request_id: string
  api_key_id: string | null
  user_id: string | null
  user_name: string | null
  user_email: string | null
  model: string
  /**
   * Where the request actually ran, as the gateway recorded it. null has two
   * causes and the UI must not imply only one of them: the request was refused
   * before a route resolved (so there was no class to record), or the row
   * predates the column. The usage filter calls both 'unrecorded'.
   */
  backend_class?: BackendClass | null
  /** Which provider served it: vllm | lmstudio | bedrock | anthropic | openai
   *  | mock. Never null from a current api; absent on an older one. On an
   *  external_api row this is the vendor, and it is the only thing that turns
   *  "left your account" into "left your account, to Anthropic". */
  provider?: string | null
  /** Absent on older APIs — treat a missing value as 'inference'. */
  purpose?: InferencePurpose
  /** Routed aliases: the route that answered and why, as recorded. */
  route_name?: string | null
  router_reason?: string | null
  status_code: number
  /** Set on a refused request — the gateway's OpenAI-style error code, e.g.
   *  model_not_allowed. null on a 2xx row, and on older APIs that predate this
   *  field. Not a closed enum on this side: an unrecognised code still renders,
   *  it just falls back to its own words. */
  denial_code?: string | null
  /** What the caller asked for when it differs from `model` (an alias, or a
   *  request refused before any model resolved). */
  requested_model?: string | null
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
