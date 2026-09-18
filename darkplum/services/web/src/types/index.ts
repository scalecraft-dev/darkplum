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

/** Where a model actually runs. Mirrors models.backend_class in the API. */
export type BackendClass = 'self_hosted' | 'cloud_managed'

/** A route the administrator wrote, as the chat-facing model list exposes it:
 *  the name and what it is for. Targets are never named to end users. */
export interface ModelRouteSummary {
  route_name: string
  description: string
  /** Optional: the API is only required to send names and descriptions. When it
   *  does send a class, the picker can say that routes run in different places. */
  target_backend_class?: BackendClass
}

export interface Model {
  id: string
  object: string
  created: number
  owned_by: string
  /** Absent on older gateways/APIs — treat a missing value as 'self_hosted'. */
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
  provider: string
  display_name: string | null
  enabled: boolean
  discovered_at: string
  updated_at: string
  /** Absent on older APIs — treat a missing value as 'self_hosted'. */
  backend_class?: BackendClass
  /** False when the gateway has not seen this model for 3+ minutes. The row
   *  stays configured either way. Absent on older APIs — treat as reachable. */
  available?: boolean
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

/** How a routed alias decides which route answers. */
export type AliasClassifierKind = 'rules' | 'model' | 'semantic_router'

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
      classifier: AliasClassifier
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

/** Routing policy residency. 'self_hosted_only' refuses cloud-managed models. */
export type Residency = 'any' | 'self_hosted_only'

export interface RoutingPolicy {
  residency: Residency
  updated_at: string
  /** Returned by PUT: routed aliases whose routes will now be refused. One
   *  readable sentence each, written by the API. */
  warnings?: string[]
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
  /** Absent on older APIs — treat a missing value as 'inference'. */
  purpose?: InferencePurpose
  /** Routed aliases: the route that answered and why, as recorded. */
  route_name?: string | null
  router_reason?: string | null
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
