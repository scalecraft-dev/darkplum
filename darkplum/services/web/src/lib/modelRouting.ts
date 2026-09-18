import type { AliasKind, BackendClass, ModelAlias } from '../types'

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

/** Older APIs omit backend_class; everything we ship in-cluster is self-hosted. */
export function backendClassOf(m: { backend_class?: BackendClass | null }): BackendClass {
  return m.backend_class === 'cloud_managed' ? 'cloud_managed' : 'self_hosted'
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

/** Admin vocabulary — matches the wording on the routing policy control. */
export function backendClassLabel(c: BackendClass): string {
  return c === 'cloud_managed' ? 'Cloud-managed' : 'Self-hosted'
}

/** End-user vocabulary — answers "where does my conversation go?". */
export function backendClassChatLabel(c: BackendClass): string {
  return c === 'cloud_managed' ? 'Runs in your cloud provider' : 'Runs in your infrastructure'
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

/** Routing is decided once per conversation. Administrators write routes without
 *  seeing the chat, so this fact is repeated wherever routes are written or
 *  tested rather than left for them to discover from a transcript. */
export const STICKINESS_HINT =
  'Routing happens on the first message of a conversation; later messages in that conversation stay on the model that answered first. Users can start a new chat to be routed again.'

export const MAX_KEYWORDS_PER_ROUTE = 32
export const MAX_KEYWORD_LENGTH = 64
export const MAX_ROUTE_DESCRIPTION = 500

/** The three signals the gateway can detect for itself. An administrator binds
 *  one to a route by using it as a keyword; everything else is literal text. */
export const BUILTIN_KEYWORDS: { token: string; explanation: string }[] = [
  { token: 'builtin:code', explanation: 'The message contains a code block or a diff.' },
  { token: 'builtin:long', explanation: 'The message is longer than 8,000 characters.' },
  { token: 'builtin:multimodal', explanation: 'The message contains an image or another attachment.' },
]

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
  if (reason.startsWith('semantic:')) return 'Classified by the semantic router'
  return reason
}

/** Every target a routed alias can reach, or the single target of a static one. */
export function aliasTargets(a: ModelAlias): string[] {
  if (isRoutedAlias(a)) return (a.routes ?? []).map((r) => r.target_model_id)
  return a.target_model_id ? [a.target_model_id] : []
}
