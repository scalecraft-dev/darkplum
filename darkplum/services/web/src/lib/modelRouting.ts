import type { AliasKind, BackendClass, DisplayClass, ModelAlias, VendorName } from '../types'

/** Alias format enforced by the API and the database check constraint. */
export const ALIAS_PATTERN = '^[a-z0-9][a-z0-9._-]{0,62}$'
const ALIAS_RE = new RegExp(ALIAS_PATTERN)

export const ALIAS_HINT =
  'Lowercase letters, numbers, dots, dashes and underscores. Starts with a letter or number, up to 63 characters.'

/**
 * Returns a message explaining why the alias is unusable, or null when valid.
 * The messages name the specific problem rather than repeating the pattern.
 */
export function validateAlias(alias: string): string | null {
  if (!alias) return 'Enter a name for the alias.'
  if (alias.length > 63) return 'Use 63 characters or fewer.'
  if (/[A-Z]/.test(alias)) return 'Use lowercase letters only.'
  if (!/^[a-z0-9]/.test(alias)) return 'Start with a letter or a number.'
  if (!ALIAS_RE.test(alias)) return 'Use only letters, numbers, dots, dashes and underscores.'
  return null
}

const KNOWN_CLASSES: readonly string[] = ['self_hosted', 'cloud_managed', 'external_api']

/**
 * Where a model runs, as the UI is willing to say it.
 *
 * A value this bundle does not recognise — or none at all, from an older API or
 * a model no backend has reported — is 'unknown', never the class that happens
 * to read as safest. Reading a missing value as self-hosted is how a prompt
 * bound for a vendor's API ends up labelled "Runs in your infrastructure", so
 * the unknown state is carried all the way to the screen instead.
 */
export function displayClassOf(m: {
  backend_class?: BackendClass | string | null
}): DisplayClass {
  const raw = m.backend_class
  return typeof raw === 'string' && KNOWN_CLASSES.includes(raw) ? (raw as BackendClass) : 'unknown'
}

/** The same, for the class field an alias target carries. */
export function targetClassOf(t: {
  target_backend_class?: BackendClass | string | null
}): DisplayClass {
  return displayClassOf({ backend_class: t.target_backend_class })
}

/** Older APIs omit `available`; only an explicit false means unreachable. */
export function isReachable(m: { available?: boolean | null }): boolean {
  return m.available !== false
}

/** Same rule for the target of an alias. */
export function isTargetReachable(a: { target_available?: boolean | null }): boolean {
  return a.target_available !== false
}

/** One sentence, used wherever an unreachable model needs explaining. */
export const UNREACHABLE_HINT =
  'Not reachable right now — it stays configured and returns when its backend is back.'

// ---------------------------------------------------------------------------
// Vendors
// ---------------------------------------------------------------------------

const VENDOR_LABELS: Record<string, string> = {
  anthropic: 'Anthropic',
  openai: 'OpenAI',
  amazon: 'Amazon',
  meta: 'Meta',
  mistral: 'Mistral',
  cohere: 'Cohere',
  ai21: 'AI21',
  deepseek: 'DeepSeek',
  qwen: 'Qwen',
}

/** A vendor or provider kind in the capitalisation the vendor uses. An
 *  unrecognised one is title-cased rather than dropped: a name we have not met
 *  is still the honest answer to "who gets this prompt?". */
export function vendorLabel(name: string | null | undefined): string {
  if (!name) return 'the vendor'
  return VENDOR_LABELS[name.toLowerCase()] ?? name.charAt(0).toUpperCase() + name.slice(1)
}

/** The two vendors Darkplum can hold an organisation-wide key for, in the
 *  order the Models tab lists them. Always both, configured or not. */
export const VENDORS: VendorName[] = ['anthropic', 'openai']

/** The command that installs one vendor's key. Shown verbatim, to be copied:
 *  it must work as pasted, so it reads the key without echoing it and names the
 *  catalogue filter, which the target requires the first time. */
export const VENDOR_KEY_COMMAND: Record<VendorName, string> = {
  anthropic: "read -s ANTHROPIC_API_KEY && export ANTHROPIC_API_KEY\nmake set-anthropic-key MODELS='claude-*'",
  openai: "read -s OPENAI_API_KEY && export OPENAI_API_KEY\nmake set-openai-key MODELS='gpt-*'",
}

/** Re-running the install target with MODELS is how the catalogue filter is
 *  changed; the key it already holds can be re-entered or rotated at the same time. */
export const VENDOR_FILTER_COMMAND: Record<VendorName, string> = {
  anthropic: "make set-anthropic-key MODELS='claude-*'",
  openai: "make set-openai-key MODELS='gpt-*'",
}

/** How a provider kind is named wherever models are grouped by it. */
const PROVIDER_LABELS: Record<string, string> = {
  vllm: 'Local',
  lmstudio: 'LM Studio',
  bedrock: 'AWS Bedrock',
  anthropic: 'Anthropic',
  openai: 'OpenAI',
  mock: 'Mock',
}

export function providerLabel(provider: string): string {
  return PROVIDER_LABELS[provider] ?? provider
}

/** Reading order for provider groups: what runs closest to the organisation
 *  first, what leaves it last. Anything unlisted sorts alphabetically after. */
const PROVIDER_ORDER = ['vllm', 'lmstudio', 'bedrock', 'anthropic', 'openai']

export function compareProviders(a: string, b: string): number {
  const ai = PROVIDER_ORDER.indexOf(a)
  const bi = PROVIDER_ORDER.indexOf(b)
  if (ai === -1 && bi === -1) return a.localeCompare(b)
  if (ai === -1) return 1
  if (bi === -1) return -1
  return ai - bi
}

/** True for a provider kind that is a vendor API rather than infrastructure
 *  the organisation runs or owns. */
export function isVendorProvider(provider: string): provider is VendorName {
  return provider === 'anthropic' || provider === 'openai'
}

// ---------------------------------------------------------------------------
// Class vocabulary
// ---------------------------------------------------------------------------

/** Admin vocabulary — matches the wording on the routing policy ladder. */
export function backendClassLabel(c: DisplayClass): string {
  switch (c) {
    case 'cloud_managed':
      return 'Cloud-managed'
    case 'external_api':
      return 'External API'
    case 'unknown':
      return 'Unknown'
    default:
      return 'Self-hosted'
  }
}

/** The one sentence that explains the External API class wherever its badge
 *  appears. Said the same way every time, because an administrator deciding
 *  whether to allow it is deciding on exactly this. */
export const EXTERNAL_API_TOOLTIP =
  "Called at the vendor's own API with your organisation's key. Prompts leave your cloud account."

/** What a class badge's tooltip says, or undefined when the label says it all. */
export function backendClassTooltip(c: DisplayClass): string | undefined {
  if (c === 'external_api') return EXTERNAL_API_TOOLTIP
  if (c === 'unknown') return UNKNOWN_CLASS_HINT
  return undefined
}

/** No backend has reported where this model runs. Said wherever the unknown
 *  class shows, because "unknown" alone reads as a UI failure rather than as
 *  the refusal it actually causes. */
export const UNKNOWN_CLASS_HINT =
  'No backend has reported where this model runs, so the routing policy refuses it until one does.'

/**
 * End-user vocabulary — answers "where do my words go?". The vendor is named
 * when there is one: "somewhere outside your account" is not an answer a person
 * can act on, and the vendor's name is the whole point of the warning.
 */
export function backendClassChatLabel(c: DisplayClass, vendor?: string | null): string {
  switch (c) {
    case 'cloud_managed':
      return 'Runs in your cloud account'
    case 'external_api':
      return `Sent to ${vendorLabel(vendor)} — leaves your cloud account`
    case 'unknown':
      return 'Where this model runs is not known yet'
    default:
      return 'Runs in your infrastructure'
  }
}

/** True when a prompt on this class leaves the organisation's cloud account.
 *  The one test behind every egress marker in the product. */
export function isEgress(c: DisplayClass): boolean {
  return c === 'external_api'
}

/**
 * Where one recorded request ran, as a parenthetical after the model id.
 *
 * Both facts or neither. The class alone leaves a compliance reviewer knowing
 * a prompt left the account without knowing whose hands it left into, and the
 * provider alone does not say whether it crossed the boundary at all.
 *
 * A row with no class gets nothing rather than "(unknown, Anthropic)": when
 * the request was refused before a route resolved, the provider on the row is
 * not a place anything actually ran, and printing it would invent a fact.
 * Returns '' in that case, and the filter is where those rows are found.
 */
export function ranOnSuffix(
  backendClass: BackendClass | string | null | undefined,
  provider: string | null | undefined,
): string {
  if (!backendClass) return ''
  const cls = displayClassOf({ backend_class: backendClass })
  if (cls === 'unknown') return ''
  return provider
    ? ` (${backendClassLabel(cls)}, ${providerLabel(provider)})`
    : ` (${backendClassLabel(cls)})`
}

/**
 * The badge colour a class wears, decided once so the catalogue, the allowlist
 * picker and the alias table cannot each pick their own.
 *
 * Self-hosted is the unremarkable case and is neutral. Cloud-managed is
 * informational: it is somewhere else, but somewhere the organisation owns.
 * External API is amber — not because it is broken, but because it is the one
 * class where a prompt crosses the account boundary, and that is exactly what
 * amber means everywhere else in this product. Unknown is neutral and wears a
 * dashed border instead: a gap in what we know is not a severity.
 */
export function backendClassVariant(
  c: DisplayClass,
): 'neutral' | 'info' | 'warning' {
  switch (c) {
    case 'cloud_managed':
      return 'info'
    case 'external_api':
      return 'warning'
    default:
      return 'neutral'
  }
}

// ---------------------------------------------------------------------------
// Routed aliases
// ---------------------------------------------------------------------------

/** Older APIs send no kind at all; everything they know about is static. */
export function aliasKindOf(a: { kind?: AliasKind | null }): AliasKind {
  return a.kind === 'routed' ? 'routed' : 'static'
}

export function isRoutedAlias(a: { kind?: AliasKind | null }): boolean {
  return aliasKindOf(a) === 'routed'
}

/** Route name format enforced by the API and the database check constraint. */
export const ROUTE_NAME_PATTERN = '^[a-z0-9][a-z0-9_-]{0,31}$'
const ROUTE_NAME_RE = new RegExp(ROUTE_NAME_PATTERN)

export const ROUTE_NAME_HINT =
  'Lowercase letters, numbers, dashes and underscores. Starts with a letter or number, up to 32 characters.'

/** Returns why this route name cannot be used, or null when it is fine. */
export function validateRouteName(name: string): string | null {
  if (!name) return 'Give the route a name.'
  if (name.length > 32) return 'Use 32 characters or fewer.'
  if (/[A-Z]/.test(name)) return 'Use lowercase letters only.'
  if (!/^[a-z0-9]/.test(name)) return 'Start with a letter or a number.'
  if (!ROUTE_NAME_RE.test(name)) return 'Use only letters, numbers, dashes and underscores.'
  return null
}

/** Routing is decided per message, not per conversation. Administrators write
 *  routes without seeing the chat, so this fact is repeated wherever routes are
 *  written or tested rather than left for them to discover from a transcript. */
export const PER_MESSAGE_ROUTING_HINT =
  'Every message is classified on its own, so a thread may be answered by different models as its questions change; the reply says which one answered.'

export const MAX_KEYWORDS_PER_ROUTE = 32
export const MAX_KEYWORD_LENGTH = 64
export const MAX_ROUTE_DESCRIPTION = 500

/**
 * What no request may carry, wherever it was going. The gateway refuses tools
 * and any non-text content block with a 400 before a route is resolved, on
 * every backend — so this is a fact about the product, not about one class of
 * target. Said where an administrator configures models or writes routes;
 * webchat has no image upload, so it is never said to an end user, who could
 * do nothing with it.
 */
export const UNSUPPORTED_CONTENT_NOTE =
  'Images and tool calls are not supported on any backend yet. A request that carries them is refused by name rather than quietly stripped.'

/**
 * Why builtin:multimodal can no longer fire. The refusal above happens before
 * route resolution, so a rule bound to this signal is unreachable whatever it
 * points at — there is no target class that rescues it, and no advice to give
 * beyond knowing it is inert.
 */
export const MULTIMODAL_NEVER_MATCHES =
  'Never matches today: a message carrying an image or any other non-text content is refused before a route is chosen, on every backend.'

/** The three signals the gateway can detect for itself. An administrator binds
 *  one to a route by using it as a keyword; everything else is literal text.
 *  `inert` is set on a signal no request can reach any more: it stays in the
 *  vocabulary so a stored route that already binds it still renders and can be
 *  named, but it cannot be added to a new one. */
export const BUILTIN_KEYWORDS: { token: string; explanation: string; inert?: string }[] = [
  { token: 'builtin:code', explanation: 'The message contains a code block or a diff.' },
  { token: 'builtin:long', explanation: 'The message is longer than 8,000 characters.' },
  {
    token: 'builtin:multimodal',
    explanation: 'The message contains an image or another attachment.',
    inert: MULTIMODAL_NEVER_MATCHES,
  },
]

/** The tokens BUILTIN_KEYWORDS marks inert, for a stored route that holds one. */
export const INERT_BUILTINS = new Set(
  BUILTIN_KEYWORDS.filter((b) => b.inert).map((b) => b.token),
)

/** The flag on one route that binds the multimodal signal. Said whatever the
 *  route points at: where it sends them is beside the point when nothing can
 *  match it. */
export function multimodalRouteNote(routeName: string): string {
  return `${routeName} binds builtin:multimodal. ${MULTIMODAL_NEVER_MATCHES}`
}

/** How a builtin match reads in a sentence about one decision. */
const BUILTIN_MATCH_TEXT: Record<string, string> = {
  'builtin:code': 'Matched a code block or diff in the message',
  'builtin:long': 'Matched a message longer than 8,000 characters',
  'builtin:multimodal': 'Matched an image or other attachment',
}

/**
 * The gateway records why a route was chosen in a closed vocabulary. This turns
 * one of those strings into the sentence an administrator can act on. Anything
 * unrecognised is returned as it came, so a new reason is never swallowed.
 */
export function routerReasonText(reason: string | null | undefined): string {
  if (!reason) return ''
  if (reason === 'rules:default') return 'No rule matched — default route'
  if (reason.startsWith('rules:')) {
    // rules:<route_name>:<keyword>. A keyword may itself contain a colon
    // (builtin:code), so only the first two segments are structure.
    const parts = reason.split(':')
    const keyword = parts.slice(2).join(':')
    if (!keyword) return reason
    return BUILTIN_MATCH_TEXT[keyword] ?? `Matched keyword '${keyword}'`
  }
  switch (reason) {
    case 'fallback:timeout':
      return 'Classifier timed out — default route'
    case 'fallback:unparseable':
      return 'Classifier did not answer with a route name — default route'
    case 'fallback:circuit_open':
      return 'Classifier skipped after repeated failures — default route'
    case 'fallback:classifier_invalid':
      return 'Classifier cannot be used — default route'
    case 'fallback:low_confidence':
      return 'Classifier was not confident — default route'
    case 'fallback:unknown_category':
      return 'Classifier returned a category no route is named after — default route'
  }
  const fallback = routeFallbackOf(reason)
  if (fallback) {
    return `Fell back to the default route — the model on route '${fallback.route}' ${
      fallback.state === 'disabled' ? 'is disabled' : 'is not reachable'
    }`
  }
  if (reason.startsWith('semantic:')) return 'Classified by the semantic router'
  return reason
}

const ROUTE_FALLBACK_RE = /^fallback:route_target_(disabled|unreachable):(.+)$/

/**
 * A routed alias whose picked route cannot answer — its target is disabled, or
 * the gateway has no backend for it — is answered on the alias's own default
 * route instead, and the gateway records which route was skipped. Policy
 * refusals never land here: those still refuse with the route named.
 */
export function routeFallbackOf(
  reason: string | null | undefined,
): { route: string; state: 'disabled' | 'unreachable' } | null {
  const m = reason ? ROUTE_FALLBACK_RE.exec(reason) : null
  return m ? { state: m[1] as 'disabled' | 'unreachable', route: m[2] } : null
}

/** Why a non-default route is not answering itself, as Admin > Routing sees it. */
export type RouteFallbackState = 'missing' | 'disabled' | 'unreachable'

/** The one sentence under a non-default route whose target cannot answer while
 *  the default route can. Said in the editor and on the alias row alike. */
export function routeFallbackHint(state: RouteFallbackState, defaultRoute: string): string {
  const lead =
    state === 'missing'
      ? 'No model has this ID'
      : state === 'disabled'
        ? 'This model is turned off'
        : 'This model is not reachable right now'
  return `${lead}, so messages that take this route fall back to the default route ${defaultRoute} until it is fixed.`
}

/**
 * The gateway's sentence for a routed pick that cannot answer and has nowhere
 * to fall back to: the picked route is the default, or the default cannot
 * answer either. It names both models and is relayed verbatim; this only
 * recognises it so chat can offer the right next step.
 */
export function isRouteTargetRefusal(code: string | null | undefined, message: string): boolean {
  return (
    (code === 'model_not_enabled' || code === 'model_not_found') &&
    message.includes("the alias's default route")
  )
}

/** One place a model is used by an alias, and what disabling it does there. */
export interface AliasReference {
  alias: string
  /** null for a static alias. */
  route: string | null
  isDefault: boolean
  /** The alias's default route and its target, for a routed alias. */
  defaultRoute: string | null
  defaultTarget: string | null
  /** 'fallback': messages on this route move to the default route.
   *  'refused': messages that land here are refused. */
  outcome: 'fallback' | 'refused'
}

/**
 * Every alias and route that targets `modelId`, with what turning the model off
 * would do to each. A non-default route falls back to its alias's default
 * route — unless that default also targets this model, or cannot answer as
 * things stand, in which case it is refused too.
 */
export function aliasReferencesTo(modelId: string, aliases: ModelAlias[]): AliasReference[] {
  const refs: AliasReference[] = []
  for (const a of aliases) {
    if (!isRoutedAlias(a)) {
      if (a.target_model_id === modelId) {
        refs.push({
          alias: a.alias,
          route: null,
          isDefault: false,
          defaultRoute: null,
          defaultTarget: null,
          outcome: 'refused',
        })
      }
      continue
    }
    const routes = a.routes ?? []
    const def = routes.find((r) => r.route_name === a.default_route) ?? null
    const defaultUsable =
      def !== null &&
      def.target_model_id !== modelId &&
      def.target_exists !== false &&
      def.target_enabled !== false &&
      def.target_available !== false
    for (const r of routes) {
      if (r.target_model_id !== modelId) continue
      const isDefault = def !== null && r.route_name === def.route_name
      refs.push({
        alias: a.alias,
        route: r.route_name,
        isDefault,
        defaultRoute: def?.route_name ?? null,
        defaultTarget: def?.target_model_id ?? null,
        outcome: !isDefault && defaultUsable ? 'fallback' : 'refused',
      })
    }
  }
  return refs
}

/** What follows the alias and route in the disable confirmation. */
export function aliasReferenceConsequence(r: AliasReference): string {
  if (r.route === null) return 'refused until it is retargeted or the model is re-enabled'
  if (r.isDefault) {
    return 'messages that land here will be refused until it is retargeted or re-enabled'
  }
  if (r.outcome === 'fallback') {
    return `messages will use its default route ${r.defaultRoute} (${r.defaultTarget}) instead`
  }
  return r.defaultRoute
    ? `messages will be refused — its default route ${r.defaultRoute} cannot answer either`
    : 'messages will be refused — the alias has no default route to fall back to'
}

/** Every target a routed alias can reach, or the single target of a static one. */
export function aliasTargets(a: ModelAlias): string[] {
  if (isRoutedAlias(a)) return (a.routes ?? []).map((r) => r.target_model_id)
  return a.target_model_id ? [a.target_model_id] : []
}

// ---------------------------------------------------------------------------
// Denial codes
// ---------------------------------------------------------------------------

/** Every code the usage log's denial filter offers — the same closed
 *  vocabulary the API's `?denial_code=` query param and a refused request's
 *  `denial_code` field both accept (activity.go's validDenialCodeFilter). A
 *  code the API sends that isn't here still renders on a row (see
 *  denialLabel); it just isn't one of the filter's options. */
export const DENIAL_CODES = [
  'model_not_found',
  'model_not_enabled',
  'model_residency_violation',
  'model_not_allowed',
  'invalid_request',
  'policy_unavailable',
  'request_canceled',
  'model_requires_data_retention',
] as const

const DENIAL_LABELS: Record<string, string> = {
  model_not_found: 'Model not found',
  model_not_enabled: 'Model disabled',
  model_residency_violation: 'Blocked by where-it-runs policy',
  model_not_allowed: 'Not allowed by policy',
  invalid_request: 'Invalid request',
  policy_unavailable: 'Policy unavailable',
  request_canceled: 'Request cancelled',
  // Same phrase as the Models badge (lib/dataRetention AWS_OPT_IN_LABEL);
  // written out here because check-policy pins the two to each other.
  model_requires_data_retention: 'Needs AWS opt-in',
  // Recorded on a refused request (gateway chat.go sets it before route
  // resolution) but deliberately NOT in DENIAL_CODES above: the api's
  // validDenialCodeFilter does not accept it, so offering it as a filter
  // option would send a query the api answers with a 400. A row carrying it
  // still needs a written label rather than the underscore fallback.
  unsupported_parameter: 'Unsupported request option',
}

// The vendor failures the gateway relays — provider_rate_limited,
// provider_auth_failed, upstream_error, upstream_timeout — are absent from
// both lists on purpose. They are not denials: nothing sets denial_code for
// them, so no usage row can ever carry one and a filter option would return
// nothing. They are explained where they are seen, in chat.

/** Short, admin-facing label for a denial code, e.g. in the activity log. A
 *  code outside the closed vocabulary above still gets a readable label
 *  rather than disappearing — it just isn't hand-written. */
export function denialLabel(code: string): string {
  return DENIAL_LABELS[code] ?? code.replace(/_/g, ' ')
}

// ---------------------------------------------------------------------------
// Vendor failures
// ---------------------------------------------------------------------------

/**
 * Who to name in a vendor failure, ready to open a sentence.
 *
 * Only the two vendors Darkplum holds an organisation-wide key for are named:
 * those messages are about that key, and nothing else has one. A provider kind
 * that is infrastructure the organisation runs, a routed alias's own owner
 * ('darkplum'), or nothing at all, all read as the unnamed vendor rather than
 * as a title-cased guess — "Vllm rejected your organisation's key" is worse
 * than saying less.
 */
export function failedVendorLabel(name: string | null | undefined): string {
  const key = name?.toLowerCase() ?? ''
  return isVendorProvider(key) ? vendorLabel(key) : 'The model vendor'
}

/** How long chat waits before offering a rate-limited send again when the
 *  vendor named no interval of its own — a mid-stream 429 carries no header,
 *  and not every vendor sets one. */
export const RETRY_AFTER_DEFAULT_SECONDS = 10

/** The longest wait the button will sit out. A vendor asking for an hour is
 *  answered with a button that comes back and a message that still says the
 *  key is shared; a countdown nobody watches to the end is not a UI. */
export const RETRY_AFTER_MAX_SECONDS = 120

/**
 * Seconds to wait, from a 429's Retry-After.
 *
 * The header is the vendor's own number, relayed verbatim by the gateway and
 * the api. Only the delta-seconds form is read: the HTTP-date form is legal
 * but no vendor here sends it, and a half-parsed date is worse than the
 * default. Anything absent, unreadable, zero or negative is the default;
 * anything absurd is clamped.
 */
export function parseRetryAfter(raw: string | null | undefined): number {
  if (raw === null || raw === undefined) return RETRY_AFTER_DEFAULT_SECONDS
  const seconds = Number(raw.trim())
  if (!Number.isFinite(seconds) || seconds <= 0) return RETRY_AFTER_DEFAULT_SECONDS
  return Math.min(Math.ceil(seconds), RETRY_AFTER_MAX_SECONDS)
}

/** The remaining wait, as it reads on the button. */
export function formatRetryCountdown(seconds: number): string {
  const whole = Math.max(0, Math.ceil(seconds))
  if (whole < 60) return `${whole}s`
  return `${Math.floor(whole / 60)}m ${String(whole % 60).padStart(2, '0')}s`
}
